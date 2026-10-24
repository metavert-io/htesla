package store

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/metavert-io/htesla/internal/fakehearth"
	"github.com/metavert-io/htesla/internal/hearth"
	"github.com/metavert-io/htesla/internal/receiver"
)

func TestChunkPathNaming(t *testing.T) {
	// UTC minute buckets, even from another zone
	est := time.FixedZone("EDT", -4*3600)
	got := ChunkPath(time.Date(2026, 10, 23, 23, 5, 59, 0, est))
	if got != "telemetry/2026-10-24/0305.jsonl" {
		t.Fatalf("got %q", got)
	}
	if ChunkPath(time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)) != "telemetry/2026-01-02/0000.jsonl" {
		t.Fatal("midnight")
	}
	if UsageKey("2026-10-23") != "usage/2026-10-23" {
		t.Fatal("usage key")
	}
	taken := map[string]bool{"telemetry/2026-10-23/1405.jsonl": true, "telemetry/2026-10-23/1405_2.jsonl": true}
	n := freeName("telemetry/2026-10-23/1405.jsonl", taken)
	if n != "telemetry/2026-10-23/1405_3.jsonl" {
		t.Fatalf("freeName %q", n)
	}
	// a second chunk for a minute sorts after that minute and before the next
	if !(n > "telemetry/2026-10-23/1405.jsonl" && n < "telemetry/2026-10-23/1406.jsonl") {
		t.Fatalf("%q sorts out of place", n)
	}
}

func ev(at time.Time, data map[string]any) receiver.Event {
	return receiver.Event{ReceivedAt: at, VIN: "VIN1", Topic: "V", Data: data, Signals: len(data)}
}

func TestFlushWritesFinishedMinutesOnceAndNeverRewrites(t *testing.T) {
	d := fakehearth.New()
	defer d.Close()
	api := hearth.NewWithKey(d.URL, d.Key)
	clock := time.Date(2026, 10, 23, 14, 5, 10, 0, time.UTC)
	w := &Writer{API: api, Now: func() time.Time { return clock }}
	ctx := context.Background()

	w.Add(ev(clock, map[string]any{"VehicleSpeed": 10.0}))
	w.Add(ev(clock.Add(20*time.Second), map[string]any{"VehicleSpeed": 12.0, "Gear": "ShiftStateD"}))
	// the minute isn't over: nothing written
	if err := w.Flush(ctx, false, false); err != nil {
		t.Fatal(err)
	}
	if len(d.Puts) != 0 {
		t.Fatalf("wrote an unfinished minute: %v", d.Puts)
	}
	clock = clock.Add(time.Minute) // 14:06:10
	w.Add(ev(clock, map[string]any{"VehicleSpeed": 14.0}))
	if err := w.Flush(ctx, false, true); err != nil {
		t.Fatal(err)
	}
	p := "telemetry/2026-10-23/1405.jsonl"
	if d.Puts[p] != 1 || len(d.Puts) != 1 {
		t.Fatalf("want exactly one PUT of %s, got %v", p, d.Puts)
	}
	lines := strings.Split(strings.TrimSpace(string(d.Files[p])), "\n")
	if len(lines) != 2 {
		t.Fatalf("want 2 lines, got %d", len(lines))
	}
	var first receiver.Event
	if err := json.Unmarshal([]byte(lines[0]), &first); err != nil || first.Data["VehicleSpeed"] != 10.0 {
		t.Fatalf("line 1 %s", lines[0])
	}
	// flushing again doesn't touch the written chunk
	_ = w.Flush(ctx, false, false)
	if d.Puts[p] != 1 {
		t.Fatal("a written chunk was rewritten")
	}
	// final flush (shutdown) writes the partial 14:06 minute
	if err := w.Flush(ctx, true, true); err != nil {
		t.Fatal(err)
	}
	if d.Puts["telemetry/2026-10-23/1406.jsonl"] != 1 {
		t.Fatalf("final flush: %v", d.Puts)
	}

	// a restarted sidecar in the same minute must not overwrite 1406
	w2 := &Writer{API: api, Now: func() time.Time { return clock }}
	w2.Add(ev(clock.Add(5*time.Second), map[string]any{"VehicleSpeed": 15.0}))
	if err := w2.Flush(ctx, true, false); err != nil {
		t.Fatal(err)
	}
	if d.Puts["telemetry/2026-10-23/1406.jsonl"] != 1 || d.Puts["telemetry/2026-10-23/1406_2.jsonl"] != 1 {
		t.Fatalf("restart collision: %v", d.Puts)
	}

	// latest snapshot + usage
	var latest Latest
	if err := json.Unmarshal([]byte(d.KV["latest"]), &latest); err != nil {
		t.Fatal(err)
	}
	if latest.Cars["VIN1"].Fields["VehicleSpeed"].Value != 14.0 || latest.Cars["VIN1"].Fields["Gear"].Value != "ShiftStateD" {
		t.Fatalf("latest %+v", latest.Cars["VIN1"].Fields)
	}
	var u Usage
	if err := json.Unmarshal([]byte(d.KV["usage/2026-10-23"]), &u); err != nil {
		t.Fatalf("usage KV %q: %v", d.KV["usage/2026-10-23"], err)
	}
	if u.Signals != 5 || u.Records != 4 {
		t.Fatalf("usage %+v (want 5 signals over 4 records — the restart folded in the stored count)", u)
	}
	if u.EstCostUSD <= 0 {
		t.Fatal("cost estimate")
	}
}

func TestPruneKeeps14Days(t *testing.T) {
	now := time.Date(2026, 10, 23, 12, 0, 0, 0, time.UTC)
	files := []hearth.FileInfo{
		{Path: "telemetry/2026-10-08/0101.jsonl"}, // 15 days old → gone
		{Path: "telemetry/2026-10-09/2359.jsonl"}, // exactly 14 days → kept
		{Path: "telemetry/2026-10-22/1200.jsonl"},
		{Path: "tesla/public-key.pem"},
		{Path: "tls/ca.pem"},
	}
	got := ExpiredChunks(files, now, KeepDays)
	if len(got) != 1 || got[0] != "telemetry/2026-10-08/0101.jsonl" {
		t.Fatalf("expired %v", got)
	}
	d := fakehearth.New()
	defer d.Close()
	for _, f := range files {
		d.Files[f.Path] = []byte("x")
	}
	w := &Writer{API: hearth.NewWithKey(d.URL, d.Key), Now: func() time.Time { return now }}
	n, err := w.Prune(context.Background())
	if err != nil || n != 1 {
		t.Fatalf("pruned %d, %v", n, err)
	}
	if _, ok := d.Files["telemetry/2026-10-08/0101.jsonl"]; ok {
		t.Fatal("old chunk survived")
	}
	if _, ok := d.Files["tesla/public-key.pem"]; !ok {
		t.Fatal("prune touched a non-telemetry file")
	}
}

func TestOlderResendDoesNotOverwriteLatest(t *testing.T) {
	w := &Writer{API: nil}
	newer := time.Date(2026, 10, 23, 14, 0, 0, 0, time.UTC)
	older := newer.Add(-30 * time.Minute)
	e1 := ev(newer, map[string]any{"BatteryLevel": 80.0})
	e1.CreatedAt = &newer
	e2 := ev(newer.Add(time.Second), map[string]any{"BatteryLevel": 90.0})
	e2.CreatedAt = &older
	e2.Resend = true
	w.Add(e1)
	w.Add(e2)
	if v := w.latest.Cars["VIN1"].Fields["BatteryLevel"].Value; v != 80.0 {
		t.Fatalf("a delayed resend overwrote a newer value: %v", v)
	}
}
