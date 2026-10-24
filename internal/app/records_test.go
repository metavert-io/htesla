package app

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/metavert-io/htesla/internal/builder"
	"github.com/metavert-io/htesla/internal/cost"
	"github.com/metavert-io/htesla/internal/fakehearth"
	"github.com/metavert-io/htesla/internal/hearth"
	"github.com/metavert-io/htesla/internal/receiver"
	"github.com/metavert-io/htesla/internal/store"
)

const fvin = "7SAYGDEE0PA000001"

// a day of telemetry: home → work drive, an evening charge, an update
func fixtureChunk(day time.Time) []byte {
	var b strings.Builder
	add := func(min float64, data map[string]any) {
		t := day.Add(time.Duration(min * float64(time.Minute)))
		ev := receiver.Event{ReceivedAt: t.Add(time.Second), CreatedAt: &t, VIN: fvin, Topic: "V", Data: data}
		line, _ := json.Marshal(ev)
		b.Write(line)
		b.WriteByte('\n')
	}
	loc := func(lat, lon float64) map[string]any { return map[string]any{"latitude": lat, "longitude": lon} }
	add(0, map[string]any{"Gear": "ShiftStateP", "Location": loc(42.36, -71.06), "Odometer": 1000.0, "EnergyRemaining": 60.0,
		"LocatedAtHome": true, "VehicleName": "Ember", "CarType": "CarTypeModelY", "Version": "2026.32.1 abc", "BatteryLevel": 70.0,
		"DetailedChargeState": "DetailedChargeStateDisconnected"})
	add(5, map[string]any{"Gear": "ShiftStateD"})
	for i := 1; i <= 10; i++ {
		add(5+float64(i), map[string]any{"Location": loc(42.36+0.003*float64(i), -71.06-0.004*float64(i)), "OutsideTemp": 11.0})
	}
	add(16, map[string]any{"Odometer": 1008.0, "EnergyRemaining": 57.0, "LocatedAtHome": false, "LocatedAtWork": true, "BatteryLevel": 66.0})
	add(17, map[string]any{"Gear": "ShiftStateP"})
	add(21, map[string]any{"Locked": true})
	add(600, map[string]any{"DetailedChargeState": "DetailedChargeStateCharging", "ACChargingEnergyIn": 0.0, "LocatedAtHome": true, "LocatedAtWork": false})
	add(700, map[string]any{"ACChargingEnergyIn": 9.5, "BatteryLevel": 80.0})
	add(701, map[string]any{"DetailedChargeState": "DetailedChargeStateComplete"})
	add(800, map[string]any{"Version": "2026.38.6 def"})
	return []byte(b.String())
}

func newRecordsApp(t *testing.T, d *fakehearth.Daemon) *App {
	d.SetKV(KVConfig, `{"public_origin":"https://brain.example.com","client_id":"c","timezone":"America/New_York"}`)
	a := newApp(t, d, nil)
	a.reloadConfig(context.Background())
	a.loadBuilder(context.Background())
	return a
}

func TestRecordsPostedOnceAcrossRestarts(t *testing.T) {
	d := fakehearth.New()
	defer d.Close()
	day := time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)
	d.Files[store.ChunkPath(day)] = fixtureChunk(day)
	ctx := context.Background()
	a := newRecordsApp(t, d)
	a.builderPass(ctx)

	mv, eps, _ := d.Snapshot()
	if len(mv) != 1 {
		t.Fatalf("want one movement post, got %d", len(mv))
	}
	veh := mv[0]["vehicle"].(map[string]any)
	if veh["id"] != builder.VehicleID(fvin) || veh["name"] != "Ember" || veh["descriptor"].(map[string]any)["model"] != "Model Y" {
		t.Fatalf("vehicle %v", veh)
	}
	body, _ := json.Marshal(mv[0])
	if strings.Contains(string(body), fvin) {
		t.Fatal("the VIN left in a movement post")
	}
	item := mv[0]["items"].([]any)[0].(map[string]any)
	if item["mode"] != "drive" || item["start_place"] != "Home" || item["end_place"] != "Work" ||
		item["tz_offset_minutes"].(float64) != -240 || item["key"] != fmt.Sprintf("drive:%s:%d", builder.VehicleID(fvin), day.Add(5*time.Minute).Unix()) {
		t.Fatalf("item %v", item)
	}
	attrs := item["attrs"].(map[string]any)
	route := attrs["route_file"].(string)
	if _, ok := d.Files[route]; !ok {
		t.Fatalf("route file %s not written", route)
	}
	if len(eps) != 2 {
		t.Fatalf("want charge + update episodes, got %d", len(eps))
	}
	kinds := map[string]map[string]any{}
	for _, e := range eps {
		kinds[e["kind"].(string)] = e
	}
	if kinds["charge"]["title"] != "Charged at Home — 9.5 kWh (66% → 80%)" || kinds["charge"]["source"] != "htesla" {
		t.Fatalf("charge %v", kinds["charge"]["title"])
	}
	if kinds["software_update"]["title"] != "Model Y updated to 2026.38.6" {
		t.Fatalf("update %v", kinds["software_update"]["title"])
	}
	et := kinds["charge"]["event_time"].(map[string]any)
	if et["uncertainty"] != "minute" || et["tz_offset_minutes"].(float64) != -240 {
		t.Fatalf("event time %v", et)
	}
	// index + summary for the UI
	var idx driveIndex
	_ = json.Unmarshal([]byte(d.KVValue(KVDrives)), &idx)
	if len(idx.Drives) != 1 || idx.Drives[0].WhPerKm == nil || idx.Drives[0].RouteFile != route {
		t.Fatalf("index %+v", idx)
	}
	var sum Summary
	_ = json.Unmarshal([]byte(d.KVValue(KVSummary)), &sum)
	if len(sum.Cars) != 1 || sum.Cars[0].Place != "Home" || *sum.Cars[0].Battery != 80 || sum.Cars[0].LastDrive == nil || sum.Cars[0].PluggedIn != true {
		t.Fatalf("summary %+v", sum)
	}

	// a restart: same chunks, nothing posted twice
	b := newRecordsApp(t, d)
	b.builderPass(ctx)
	mv2, eps2, _ := d.Snapshot()
	if len(mv2) != 1 || len(eps2) != 2 {
		t.Fatalf("re-posted after restart: %d movements, %d episodes", len(mv2), len(eps2))
	}
}

func TestClosedRecordSurvivesAFailedPost(t *testing.T) {
	d := fakehearth.New()
	defer d.Close()
	day := time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)
	d.Files[store.ChunkPath(day)] = fixtureChunk(day)
	d.FailIngest = 503 // the daemon is unwell
	ctx := context.Background()
	a := newRecordsApp(t, d)
	a.builderPass(ctx)
	var saved builder.State
	if err := json.Unmarshal([]byte(d.KVValue(KVBuilder)), &saved); err != nil {
		t.Fatal(err)
	}
	if len(saved.Outbox) != 3 || saved.Cursor != store.ChunkPath(day) {
		t.Fatalf("closed records must be saved before posting: outbox %d cursor %q", len(saved.Outbox), saved.Cursor)
	}
	// the process dies; a new one starts with the daemon healthy again
	d.FailIngest = 0
	b := newRecordsApp(t, d)
	b.builderPass(ctx)
	mv, eps, _ := d.Snapshot()
	if len(mv) != 1 || len(eps) != 2 {
		t.Fatalf("after restart: %d movements, %d episodes", len(mv), len(eps))
	}
	_ = json.Unmarshal([]byte(d.KVValue(KVBuilder)), &saved)
	if len(saved.Outbox) != 0 || len(saved.Posted) != 3 {
		t.Fatalf("outbox %d posted %d", len(saved.Outbox), len(saved.Posted))
	}
}

func TestCostAlertsOncePerMonth(t *testing.T) {
	d := fakehearth.New()
	defer d.Close()
	now := time.Now().UTC()
	// enough signals already this month to pass 100% of the credit
	d.SetKV(store.UsageKey(now.Format("2006-01-02")), `{"day":"x","signals":1600000}`)
	ctx := context.Background()
	a := newApp(t, d, nil)
	a.costPass(ctx)
	a.costPass(ctx)
	_, _, notes := d.Snapshot()
	month := cost.MonthKey(now)
	keys := map[string]int{}
	for _, n := range notes {
		keys[n["dedupe_key"].(string)]++
	}
	if keys["htesla-cost:"+month+":80"] != 1 || keys["htesla-cost:"+month+":100"] != 1 || len(notes) != 2 {
		t.Fatalf("want one bell per level: %v", keys)
	}
	var p cost.Projection
	_ = json.Unmarshal([]byte(d.KVValue(KVCost)), &p)
	if p.Signals != 1600000 || p.CostSoFar < 10 {
		t.Fatalf("cost doc %+v", p)
	}
	// a restarted sidecar remembers what it already said
	b := newApp(t, d, nil)
	b.costPass(ctx)
	if _, _, n2 := d.Snapshot(); len(n2) != 2 {
		t.Fatalf("a restart re-sent cost warnings: %d", len(n2))
	}
}

var _ = hearth.ErrNotFound
