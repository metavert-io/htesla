// Package store keeps what the car streams, through the daemon's file
// and KV API (never the disk directly):
//
//   - telemetry/<YYYY-MM-DD>/<HHMM>.jsonl — one JSON line per record,
//     bucketed by the UTC minute we RECEIVED it (each line also carries
//     the car's own created_at). One PUT per finished minute; a chunk is
//     never rewritten — a second chunk for the same minute (after a
//     restart) gets a "_2", "_3"… suffix (sorting between that minute
//     and the next, so a reader's name cursor sees it).
//   - KV "latest" — last known value + time per field, per car (for a
//     future Desk widget).
//   - KV "usage/<YYYY-MM-DD>" — streamed signals counted per UTC day
//     (what Tesla bills: 150,000 signals per $1).
//
// Chunks older than 14 days are pruned. Turning these into drive and
// charge episodes is later work (it reads the chunks back by day).
package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/metavert-io/htesla/internal/hearth"
	"github.com/metavert-io/htesla/internal/receiver"
)

// API is the slice of the daemon client the store needs.
type API interface {
	FilePut(ctx context.Context, path string, data []byte) error
	FileList(ctx context.Context) ([]hearth.FileInfo, error)
	FileDelete(ctx context.Context, path string) error
	KVGet(ctx context.Context, key string) (string, error)
	KVPut(ctx context.Context, key, value string) error
}

// Prefix is where chunks live in the ember's file store.
const Prefix = "telemetry/"

// KeepDays is how long chunks are kept.
const KeepDays = 14

// SignalsPerDollar is Tesla's streaming price.
const SignalsPerDollar = 150000.0

// maxBuffered caps memory if the daemon is unreachable for a long time.
const maxBuffered = 32 << 20

// ChunkPath names the chunk for the minute containing t (UTC).
func ChunkPath(t time.Time) string {
	t = t.UTC()
	return fmt.Sprintf("%s%s/%s.jsonl", Prefix, t.Format("2006-01-02"), t.Format("1504"))
}

// UsageKey names the per-day usage KV entry.
func UsageKey(day string) string { return "usage/" + day }

// FieldValue is one remembered value.
type FieldValue struct {
	Value any    `json:"v"`
	At    string `json:"t"` // the car's created_at when known, else received_at
}

// CarLatest is everything remembered about one car.
type CarLatest struct {
	Fields       map[string]FieldValue `json:"fields"`
	Alerts       []any                 `json:"alerts,omitempty"` // last 10
	Connectivity *receiver.Event       `json:"connectivity,omitempty"`
	LastSeen     string                `json:"last_seen,omitempty"`
}

// Latest is the KV "latest" snapshot.
type Latest struct {
	UpdatedAt string                `json:"updated_at"`
	Cars      map[string]*CarLatest `json:"cars"`
}

// Usage is one day's KV "usage/<day>" entry.
type Usage struct {
	Day        string  `json:"day"`
	Signals    int64   `json:"signals"`
	Records    int64   `json:"records"`
	EstCostUSD float64 `json:"est_cost_usd"`
	UpdatedAt  string  `json:"updated_at"`
}

// Writer buffers events and writes them out.
type Writer struct {
	API  API
	Now  func() time.Time
	Logf func(format string, args ...any)

	mu         sync.Mutex
	buf        map[string][]byte // minute path → JSON lines
	bufBytes   int
	written    map[string]bool // chunk paths known to exist
	scanned    map[string]bool // days whose existing chunks were listed
	usage      map[string]*Usage
	usageSeq   map[string]int64 // bumped by every Add that counts
	usageSaved map[string]int64 // the seq last written to KV
	folded     map[string]bool  // the stored count was added in
	latest     Latest
	dirty      bool
	loaded     bool
}

func (w *Writer) now() time.Time {
	if w.Now != nil {
		return w.Now()
	}
	return time.Now()
}

func (w *Writer) logf(format string, args ...any) {
	if w.Logf != nil {
		w.Logf(format, args...)
	}
}

func (w *Writer) init() {
	if w.buf == nil {
		w.buf = map[string][]byte{}
		w.written = map[string]bool{}
		w.scanned = map[string]bool{}
		w.usage = map[string]*Usage{}
		w.usageSeq = map[string]int64{}
		w.usageSaved = map[string]int64{}
		w.folded = map[string]bool{}
		w.latest = Latest{Cars: map[string]*CarLatest{}}
	}
}

// Load restores the "latest" snapshot so a restart doesn't forget.
func (w *Writer) Load(ctx context.Context) {
	w.mu.Lock()
	w.init()
	w.mu.Unlock()
	raw, err := w.API.KVGet(ctx, "latest")
	if err != nil {
		w.mu.Lock()
		w.loaded = true
		w.mu.Unlock()
		return
	}
	var l Latest
	if json.Unmarshal([]byte(raw), &l) == nil && l.Cars != nil {
		w.mu.Lock()
		// merge: anything that arrived before Load wins
		for vin, car := range l.Cars {
			if _, ok := w.latest.Cars[vin]; !ok {
				w.latest.Cars[vin] = car
			}
		}
		w.loaded = true
		w.mu.Unlock()
	}
}

// Add takes one event (called on the socket goroutine before the ack —
// memory only, never the network).
func (w *Writer) Add(ev receiver.Event) {
	line, err := json.Marshal(ev)
	if err != nil {
		w.logf("couldn't encode a %s record: %v", ev.Topic, err)
		return
	}
	line = append(line, '\n')
	w.mu.Lock()
	defer w.mu.Unlock()
	w.init()
	if w.bufBytes+len(line) > maxBuffered {
		w.dropOldestLocked()
	}
	path := ChunkPath(ev.ReceivedAt)
	w.buf[path] = append(w.buf[path], line...)
	w.bufBytes += len(line)

	day := ev.ReceivedAt.UTC().Format("2006-01-02")
	if ev.Signals > 0 || ev.Topic == "V" || ev.Topic == "alerts" {
		u := w.usage[day]
		if u == nil {
			u = &Usage{Day: day}
			w.usage[day] = u
		}
		u.Signals += int64(ev.Signals)
		u.Records++
		w.usageSeq[day]++
	}

	car := w.latest.Cars[ev.VIN]
	if car == nil {
		car = &CarLatest{Fields: map[string]FieldValue{}}
		w.latest.Cars[ev.VIN] = car
	}
	if car.Fields == nil {
		car.Fields = map[string]FieldValue{}
	}
	at := ev.ReceivedAt.UTC().Format(time.RFC3339)
	when := at
	if ev.CreatedAt != nil {
		when = ev.CreatedAt.UTC().Format(time.RFC3339)
	}
	switch ev.Topic {
	case "V":
		for k, v := range ev.Data {
			prev, ok := car.Fields[k]
			if ok && prev.At > when { // an older resend never overwrites
				continue
			}
			car.Fields[k] = FieldValue{Value: v, At: when}
		}
	case "alerts":
		car.Alerts = append(car.Alerts, ev.Alerts...)
		if len(car.Alerts) > 10 {
			car.Alerts = car.Alerts[len(car.Alerts)-10:]
		}
	case "connectivity":
		e := ev
		car.Connectivity = &e
	}
	car.LastSeen = at
	w.dirty = true
}

func (w *Writer) dropOldestLocked() {
	keys := make([]string, 0, len(w.buf))
	for k := range w.buf {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if w.bufBytes <= maxBuffered/2 {
			return
		}
		w.bufBytes -= len(w.buf[k])
		delete(w.buf, k)
		w.logf("the daemon has been unreachable too long — dropped buffered chunk %s", k)
	}
}

// existingForDay lists the chunks already in the store for a day, once
// per process (so a chunk written before a restart is never rewritten).
func (w *Writer) existingForDay(ctx context.Context, day string) error {
	w.mu.Lock()
	done := w.scanned[day]
	w.mu.Unlock()
	if done {
		return nil
	}
	files, err := w.API.FileList(ctx)
	if err != nil {
		return err
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	for _, f := range files {
		if strings.HasPrefix(f.Path, Prefix) {
			w.written[f.Path] = true
		}
	}
	w.scanned[day] = true
	return nil
}

// freeName picks the first unused name for a minute's chunk.
func freeName(base string, taken map[string]bool) string {
	if !taken[base] {
		return base
	}
	stem := strings.TrimSuffix(base, ".jsonl")
	for n := 2; ; n++ {
		// "_n" sorts after the minute's first chunk and before the next
		// minute, so a name cursor over chunks never skips one
		p := fmt.Sprintf("%s_%d.jsonl", stem, n)
		if !taken[p] {
			return p
		}
	}
}

// Flush writes every FINISHED minute (all buffered minutes when final),
// the usage counters that changed, and the latest snapshot when
// latestToo. Each chunk is one PUT; a failed PUT stays buffered.
func (w *Writer) Flush(ctx context.Context, final, latestToo bool) error {
	w.mu.Lock()
	w.init()
	current := ChunkPath(w.now())
	var due []string
	for p := range w.buf {
		if final || p < current {
			due = append(due, p)
		}
	}
	w.mu.Unlock()
	sort.Strings(due)
	var firstErr error
	for _, p := range due {
		day := strings.SplitN(strings.TrimPrefix(p, Prefix), "/", 2)[0]
		if err := w.existingForDay(ctx, day); err != nil {
			firstErr = err
			break
		}
		w.mu.Lock()
		data := w.buf[p]
		name := freeName(p, w.written)
		w.mu.Unlock()
		if len(data) == 0 {
			continue
		}
		if err := w.API.FilePut(ctx, name, data); err != nil {
			if firstErr == nil {
				firstErr = err
			}
			break
		}
		w.mu.Lock()
		w.written[name] = true
		w.bufBytes -= len(data)
		delete(w.buf, p)
		w.mu.Unlock()
	}
	if err := w.flushUsage(ctx); err != nil && firstErr == nil {
		firstErr = err
	}
	if latestToo {
		if err := w.flushLatest(ctx); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

func (w *Writer) flushUsage(ctx context.Context) error {
	w.mu.Lock()
	var days []string
	for d, seq := range w.usageSeq {
		if w.usageSaved[d] != seq {
			days = append(days, d)
		}
	}
	w.mu.Unlock()
	sort.Strings(days)
	for _, day := range days {
		w.mu.Lock()
		folded := w.folded[day]
		w.mu.Unlock()
		if !folded {
			// fold in what's stored: a restart starts our counter at zero
			raw, err := w.API.KVGet(ctx, UsageKey(day))
			if err != nil && !errors.Is(err, hearth.ErrNotFound) {
				return err
			}
			var prev Usage
			if err == nil {
				_ = json.Unmarshal([]byte(raw), &prev)
			}
			w.mu.Lock()
			u := w.usage[day]
			u.Signals += prev.Signals
			u.Records += prev.Records
			w.folded[day] = true
			w.mu.Unlock()
		}
		w.mu.Lock()
		u := w.usage[day]
		u.EstCostUSD = float64(u.Signals) / SignalsPerDollar
		u.UpdatedAt = w.now().UTC().Format(time.RFC3339)
		b, _ := json.Marshal(u)
		seq := w.usageSeq[day]
		w.mu.Unlock()
		if err := w.API.KVPut(ctx, UsageKey(day), string(b)); err != nil {
			return err
		}
		w.mu.Lock()
		w.usageSaved[day] = seq
		w.mu.Unlock()
	}
	return nil
}

func (w *Writer) flushLatest(ctx context.Context) error {
	w.mu.Lock()
	if !w.dirty {
		w.mu.Unlock()
		return nil
	}
	w.latest.UpdatedAt = w.now().UTC().Format(time.RFC3339)
	b, err := json.Marshal(w.latest)
	w.dirty = false
	w.mu.Unlock()
	if err != nil {
		return err
	}
	if len(b) > 250*1024 {
		return errors.New("latest snapshot grew past the KV cap")
	}
	if err := w.API.KVPut(ctx, "latest", string(b)); err != nil {
		w.mu.Lock()
		w.dirty = true
		w.mu.Unlock()
		return err
	}
	return nil
}

// Usage returns a copy of today's counter as known in memory.
func (w *Writer) UsageFor(day string) Usage {
	w.mu.Lock()
	defer w.mu.Unlock()
	if u := w.usage[day]; u != nil {
		return *u
	}
	return Usage{Day: day}
}

// ExpiredChunks returns the chunk paths older than keepDays (by their
// day directory) relative to now.
func ExpiredChunks(files []hearth.FileInfo, now time.Time, keepDays int) []string {
	cutoff := now.UTC().AddDate(0, 0, -keepDays).Format("2006-01-02")
	var out []string
	for _, f := range files {
		if !strings.HasPrefix(f.Path, Prefix) {
			continue
		}
		day := strings.SplitN(strings.TrimPrefix(f.Path, Prefix), "/", 2)[0]
		if len(day) == len("2006-01-02") && day < cutoff {
			out = append(out, f.Path)
		}
	}
	sort.Strings(out)
	return out
}

// Prune deletes chunks older than KeepDays.
func (w *Writer) Prune(ctx context.Context) (int, error) {
	files, err := w.API.FileList(ctx)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, p := range ExpiredChunks(files, w.now(), KeepDays) {
		if err := w.API.FileDelete(ctx, p); err != nil {
			return n, err
		}
		w.mu.Lock()
		delete(w.written, p)
		w.mu.Unlock()
		n++
	}
	return n, nil
}
