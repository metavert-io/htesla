package app

// Item 13: turning the stored stream into records. A loop reads the
// telemetry chunks after its cursor, feeds them to the builder, and
// posts each closed record ONCE: drives to Hearthloom's movement layer
// (Hearthloom decides whose drive it was and mints the episode),
// charging sessions and software updates as episodes. The builder's
// whole state — cursor, open drives, the outbox — is one KV document
// written before anything is posted, so a restart neither loses a
// closed record nor skips a chunk; a re-post is a no-op by key.

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/metavert-io/htesla/internal/builder"
	"github.com/metavert-io/htesla/internal/hearth"
	"github.com/metavert-io/htesla/internal/receiver"
	"github.com/metavert-io/htesla/internal/store"
	"github.com/metavert-io/htesla/internal/tesla"
)

const (
	KVBuilder = "builder"
	KVDrives  = "drives"
	KVSummary = "summary"

	builderKVMax  = 200 * 1024 // stay well under the daemon's 256KB
	maxChunksPass = 400
	maxIndex      = 400
	actor         = "plugin:htesla"
)

// DriveEntry is one line of the KV "drives" index (the Drives page and
// the Desk widget read it — never Tesla).
type DriveEntry struct {
	Key        string   `json:"key"`
	Vehicle    string   `json:"vehicle"` // opaque id
	Car        string   `json:"car"`
	Start      string   `json:"start"`
	End        string   `json:"end"`
	TZOffset   int      `json:"tz_offset_minutes"`
	DistanceM  float64  `json:"distance_m"`
	EnergyKWh  *float64 `json:"energy_kwh,omitempty"`
	WhPerKm    *float64 `json:"wh_per_km,omitempty"`
	OutsideC   *float64 `json:"outside_temp_c,omitempty"`
	StartPlace string   `json:"start_place,omitempty"`
	EndPlace   string   `json:"end_place,omitempty"`
	RouteFile  string   `json:"route_file"`
	Partial    bool     `json:"partial,omitempty"`
	Guest      bool     `json:"guest_mode,omitempty"`
}

type driveIndex struct {
	Drives []DriveEntry `json:"drives"`
}

/* ———— the loop ———— */

func (a *App) builderLoop(ctx context.Context) {
	a.loadBuilder(ctx)
	a.builderPass(ctx)
	tick := time.NewTicker(a.BuilderEvery)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			a.builderPass(ctx)
		}
	}
}

func (a *App) loadBuilder(ctx context.Context) {
	a.bmu.Lock()
	defer a.bmu.Unlock()
	raw, err := a.H.KVGet(ctx, KVBuilder)
	switch {
	case err == nil:
		st := builder.New()
		if jerr := json.Unmarshal([]byte(raw), st); jerr != nil {
			// never silently restart from zero: that would re-read 14 days
			// of chunks and re-post everything (harmless, but noisy)
			a.logf("the builder state didn't parse (%v); starting a fresh one", jerr)
			st = builder.New()
			st.Cursor = a.newestChunk(ctx)
		}
		a.bstate = st
		a.bloaded = true
	case errors.Is(err, hearth.ErrNotFound):
		a.bstate = builder.New()
		a.bloaded = true
	default:
		a.logf("reading the builder state: %v (will retry)", err)
	}
	var idx driveIndex
	if err := a.H.KVGetJSON(ctx, KVDrives, &idx); err == nil {
		a.drives = &idx
	} else {
		a.drives = &driveIndex{}
	}
}

func (a *App) newestChunk(ctx context.Context) string {
	files, err := a.H.FileList(ctx)
	if err != nil {
		return ""
	}
	newest := ""
	for _, f := range files {
		if strings.HasPrefix(f.Path, store.Prefix) && f.Path > newest {
			newest = f.Path
		}
	}
	return newest
}

// builderPass: read new chunks → apply → tick → persist → post.
func (a *App) builderPass(ctx context.Context) {
	a.bmu.Lock()
	defer a.bmu.Unlock()
	if !a.bloaded {
		a.bmu.Unlock()
		a.loadBuilder(ctx)
		a.bmu.Lock()
		if !a.bloaded {
			return
		}
	}
	st := a.bstate
	before, _ := json.Marshal(st)

	files, err := a.H.FileList(ctx)
	if err != nil {
		a.recordsError("listing telemetry: " + err.Error())
		return
	}
	var todo []string
	for _, f := range files {
		if strings.HasPrefix(f.Path, store.Prefix) && strings.HasSuffix(f.Path, ".jsonl") && f.Path > st.Cursor {
			todo = append(todo, f.Path)
		}
	}
	sort.Strings(todo)
	if len(todo) > maxChunksPass {
		todo = todo[:maxChunksPass]
	}
	var evs []receiver.Event
	cursor := st.Cursor
	for _, p := range todo {
		b, err := a.H.FileGet(ctx, p)
		if errors.Is(err, hearth.ErrNotFound) {
			cursor = p // pruned meanwhile
			continue
		}
		if err != nil {
			a.recordsError("reading " + p + ": " + err.Error())
			break // stop here; the cursor only covers what was read
		}
		evs = append(evs, ParseChunk(b)...)
		cursor = p
	}
	builder.SortEvents(evs)
	for _, ev := range evs {
		st.Apply(ev)
	}
	st.Cursor = cursor
	st.Tick(time.Now())

	after, _ := json.Marshal(st)
	if !bytes.Equal(before, after) {
		if err := a.persistBuilder(ctx); err != nil {
			a.recordsError("saving the builder state: " + err.Error())
			return // nothing is posted that isn't safely recorded first
		}
	}
	a.postOutbox(ctx)
	a.writeSummary(ctx)
	a.noteRecords("")
}

// ParseChunk reads one chunk's JSON lines (bad lines are skipped).
func ParseChunk(b []byte) []receiver.Event {
	var out []receiver.Event
	sc := bufio.NewScanner(bytes.NewReader(b))
	sc.Buffer(make([]byte, 64*1024), 4<<20)
	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		var ev receiver.Event
		if json.Unmarshal(line, &ev) == nil && ev.VIN != "" {
			out = append(out, ev)
		}
	}
	return out
}

// persistBuilder writes the whole state as one KV document (thinning
// open routes if it grows too big).
func (a *App) persistBuilder(ctx context.Context) error {
	st := a.bstate
	b, err := json.Marshal(st)
	if err != nil {
		return err
	}
	if len(b) > builderKVMax {
		for _, c := range st.Cars {
			if c.Drive != nil && len(c.Drive.Points) > 300 {
				c.Drive.Points = builder.Thin(c.Drive.Points, 300)
			}
		}
		if len(st.Posted) > 20 {
			st.Posted = st.Posted[len(st.Posted)-20:]
		}
		if b, err = json.Marshal(st); err != nil {
			return err
		}
		if len(b) > builderKVMax {
			return errors.New("the builder state outgrew its KV slot")
		}
	}
	return a.H.KVPut(ctx, KVBuilder, string(b))
}

/* ———— posting ———— */

// refused is a 4xx from the daemon: retrying the same body won't help.
func refused(err error) bool {
	s := err.Error()
	return strings.Contains(s, ": 400 ") || strings.Contains(s, ": 422 ") || strings.Contains(s, ": 413 ")
}

func (a *App) postOutbox(ctx context.Context) {
	st := a.bstate
	for _, c := range append([]builder.Closed(nil), st.Outbox...) {
		err := a.postOne(ctx, c)
		if err == nil {
			st.MarkPosted(c.Key)
			a.update(func(s *Status) {
				s.Records.LastPosted = c.Key
				s.Records.LastAt = now()
			})
			if perr := a.persistBuilder(ctx); perr != nil {
				a.recordsError("saving the builder state: " + perr.Error())
				return
			}
			continue
		}
		a.logf("posting %s: %v", c.Key, err)
		if refused(err) {
			a.attempts[c.Key]++
			if a.attempts[c.Key] >= 3 {
				// the daemon keeps refusing this one: set it aside so it
				// can't block the records behind it
				st.MarkPosted(c.Key)
				a.update(func(s *Status) {
					s.Records.Failed = append(s.Records.Failed, c.Key)
					if len(s.Records.Failed) > 20 {
						s.Records.Failed = s.Records.Failed[len(s.Records.Failed)-20:]
					}
				})
				_ = a.persistBuilder(ctx)
			}
			continue
		}
		a.recordsError("posting " + c.Kind + ": " + err.Error())
		return // the daemon is unwell; try again next pass
	}
}

func (a *App) postOne(ctx context.Context, c builder.Closed) error {
	tz := a.config().Location()
	info := a.bstate.Info(c.VIN)
	switch c.Kind {
	case "drive":
		doc, _ := json.Marshal(builder.RouteDoc(c))
		if err := a.H.FilePut(ctx, builder.RoutePath(c), doc); err != nil {
			return err
		}
		v := hearth.Vehicle{ID: c.VID, Name: info.Name, Descriptor: map[string]any{"make": "Tesla"}}
		if info.Model != "" {
			v.Descriptor["model"] = info.Model
		}
		mv := builder.MovementFor(c, tz)
		if _, err := a.H.IngestMovement(ctx, actor, v, []hearth.Movement{mv}); err != nil {
			return err
		}
		a.indexDrive(ctx, c, info, mv)
		return nil
	case "charge":
		_, err := a.H.Ingest(ctx, actor, []hearth.Episode{builder.ChargeEpisode(c, info, tz)})
		return err
	case "version":
		notes := a.releaseNotes(ctx, c.VIN)
		_, err := a.H.Ingest(ctx, actor, []hearth.Episode{builder.UpdateEpisode(c, info, notes, tz)})
		return err
	}
	return nil
}

// releaseNotes asks Tesla's free release_notes endpoint (never the
// car); any failure just means an update recorded without notes.
func (a *App) releaseNotes(ctx context.Context, vin string) []builder.ReleaseNote {
	a.mu.Lock()
	signed := a.st.SignIn.State == "signed_in"
	a.mu.Unlock()
	if !signed {
		return nil
	}
	var notes []tesla.ReleaseNote
	err := a.withUser(ctx, func(f *tesla.Fleet) error {
		var err error
		notes, err = f.ReleaseNotes(ctx, vin)
		return err
	})
	if err != nil {
		a.logf("release notes unavailable: %v", err)
		return nil
	}
	out := make([]builder.ReleaseNote, 0, len(notes))
	for _, n := range notes {
		out = append(out, builder.ReleaseNote{Title: n.Title, Subtitle: n.Subtitle, Description: n.Description})
	}
	return out
}

func r1(v float64) float64 { return math.Round(v*10) / 10 }

func (a *App) indexDrive(ctx context.Context, c builder.Closed, info builder.CarInfo, mv hearth.Movement) {
	d := c.Drive
	e := DriveEntry{
		Key: c.Key, Vehicle: c.VID, Car: info.Name, Start: mv.Start, End: mv.End, TZOffset: mv.TZOffsetMinutes,
		DistanceM: r1(d.DistanceM), StartPlace: d.StartPlace, EndPlace: d.EndPlace,
		RouteFile: builder.RoutePath(c), Partial: d.Partial, Guest: d.Guest,
	}
	if d.EnergyKWh != nil {
		v := math.Round(*d.EnergyKWh*100) / 100
		e.EnergyKWh = &v
		if d.DistanceM > 100 {
			w := math.Round(*d.EnergyKWh * 1000 / (d.DistanceM / 1000))
			e.WhPerKm = &w
		}
	}
	if d.OutsideC != nil {
		v := r1(*d.OutsideC)
		e.OutsideC = &v
	}
	if a.drives == nil {
		a.drives = &driveIndex{}
	}
	for _, x := range a.drives.Drives {
		if x.Key == e.Key {
			return
		}
	}
	a.drives.Drives = append(a.drives.Drives, e)
	sort.Slice(a.drives.Drives, func(i, j int) bool { return a.drives.Drives[i].Start < a.drives.Drives[j].Start })
	if n := len(a.drives.Drives); n > maxIndex {
		a.drives.Drives = a.drives.Drives[n-maxIndex:]
	}
	if err := a.H.KVPutJSON(ctx, KVDrives, a.drives); err != nil {
		a.logf("saving the drives index: %v", err)
	}
}

/* ———— the Desk summary ———— */

// CarSummary is one car on the Desk widget.
type CarSummary struct {
	Vehicle     string      `json:"vehicle"`
	Name        string      `json:"name"`
	Model       string      `json:"model,omitempty"`
	Battery     *float64    `json:"battery,omitempty"`
	RangeKm     *float64    `json:"range_km,omitempty"`
	ChargeState string      `json:"charge_state,omitempty"`
	PluggedIn   bool        `json:"plugged_in"`
	Charging    bool        `json:"charging"`
	Driving     bool        `json:"driving"`
	Place       string      `json:"place,omitempty"` // Home | Work
	Lat         *float64    `json:"lat,omitempty"`
	Lon         *float64    `json:"lon,omitempty"`
	Online      bool        `json:"online"`
	LastSeen    string      `json:"last_seen,omitempty"`
	LastDrive   *DriveEntry `json:"last_drive,omitempty"`
}

// Summary is the KV "summary" document.
type Summary struct {
	Cars []CarSummary `json:"cars"`
}

func (a *App) buildSummary() Summary {
	st := a.bstate
	vins := make([]string, 0, len(st.Cars))
	for v := range st.Cars {
		vins = append(vins, v)
	}
	sort.Strings(vins)
	a.mu.Lock()
	online := map[string]*CarState{}
	for k, v := range a.st.Cars {
		cp := *v
		online[k] = &cp
	}
	a.mu.Unlock()
	var out Summary
	for _, vin := range vins {
		c := st.Cars[vin]
		info := st.Info(vin)
		cs := CarSummary{Vehicle: builder.VehicleID(vin), Name: info.Name, Model: info.Model, Driving: c.Drive != nil}
		if f, ok := c.Latest["BatteryLevel"]; ok {
			if v, ok := f.V.(float64); ok {
				v = math.Round(v)
				cs.Battery = &v
			}
		}
		if f, ok := c.Latest["RatedRange"]; ok {
			if v, ok := f.V.(float64); ok {
				km := math.Round(v * builder.MilesToMeters / 1000)
				cs.RangeKm = &km
			}
		}
		if f, ok := c.Latest["DetailedChargeState"]; ok {
			s, _ := f.V.(string)
			s = strings.TrimPrefix(s, "DetailedChargeState")
			cs.ChargeState = s
			cs.PluggedIn = s != "" && s != "Disconnected" && s != "Unknown"
			cs.Charging = s == "Charging" || s == "Starting"
		}
		home, _ := c.Latest["LocatedAtHome"].V.(bool)
		work, _ := c.Latest["LocatedAtWork"].V.(bool)
		if !cs.Driving {
			if home {
				cs.Place = "Home"
			} else if work {
				cs.Place = "Work"
			}
			if m, ok := c.Latest["Location"].V.(map[string]any); ok {
				la, ok1 := m["latitude"].(float64)
				lo, ok2 := m["longitude"].(float64)
				if ok1 && ok2 {
					la, lo = math.Round(la*1e4)/1e4, math.Round(lo*1e4)/1e4
					cs.Lat, cs.Lon = &la, &lo
				}
			}
		}
		if o := online[vin]; o != nil {
			cs.Online, cs.LastSeen = o.Online, o.LastSeen
		}
		if a.drives != nil {
			for i := len(a.drives.Drives) - 1; i >= 0; i-- {
				if a.drives.Drives[i].Vehicle == cs.Vehicle {
					d := a.drives.Drives[i]
					cs.LastDrive = &d
					break
				}
			}
		}
		out.Cars = append(out.Cars, cs)
	}
	return out
}

func (a *App) writeSummary(ctx context.Context) {
	b, err := json.Marshal(a.buildSummary())
	if err != nil || string(b) == a.summary {
		return
	}
	if err := a.H.KVPut(ctx, KVSummary, string(b)); err != nil {
		a.logf("saving the summary: %v", err)
		return
	}
	a.summary = string(b)
}

/* ———— status ———— */

func (a *App) recordsError(msg string) {
	a.logf("records: %s", msg)
	a.noteRecords(msg)
}

func (a *App) noteRecords(errMsg string) {
	st := a.bstate
	open, charging := 0, 0
	for _, c := range st.Cars {
		if c.Drive != nil {
			open++
		}
		if c.Charge != nil {
			charging++
		}
	}
	a.mu.Lock()
	r := a.st.Records
	same := r.Cursor == st.Cursor && r.OpenDrives == open && r.OpenCharge == charging &&
		r.Outbox == len(st.Outbox) && r.Posted == len(st.Posted) && r.Dropped == st.Dropped && r.Error == errMsg
	a.mu.Unlock()
	if same {
		return
	}
	a.update(func(s *Status) {
		s.Records.Cursor = st.Cursor
		s.Records.OpenDrives = open
		s.Records.OpenCharge = charging
		s.Records.Outbox = len(st.Outbox)
		s.Records.Posted = len(st.Posted)
		s.Records.Dropped = st.Dropped
		s.Records.Error = errMsg
	})
}
