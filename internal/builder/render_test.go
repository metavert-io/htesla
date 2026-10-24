package builder

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestMovementRendering(t *testing.T) {
	s := New()
	feed(s, normalDrive()...)
	ny, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatal(err)
	}
	m := MovementFor(s.Outbox[0], ny)
	if m.Mode != "drive" || m.Start != "2026-10-22T12:00:00Z" || m.End != "2026-10-22T12:21:00Z" || m.TZOffsetMinutes != -240 {
		t.Fatalf("%+v", m)
	}
	if *m.StartPlace != "Home" || *m.EndPlace != "Work" || *m.DistanceM != 16093.4 {
		t.Fatalf("places/distance %v %v %v", *m.StartPlace, *m.EndPlace, *m.DistanceM)
	}
	b, _ := json.Marshal(m)
	var raw map[string]any
	_ = json.Unmarshal(b, &raw)
	attrs := raw["attrs"].(map[string]any)
	for _, k := range []string{"energy_kwh", "outside_temp_c", "guest_mode", "media", "route_file", "odometer_start_km", "odometer_end_km"} {
		if _, ok := attrs[k]; !ok {
			t.Errorf("attrs missing %s", k)
		}
	}
	if attrs["route_file"] != "drives/"+VehicleID(vin)+"-1792670400.json" {
		t.Errorf("route file %v", attrs["route_file"])
	}
	if strings.Contains(string(b), vin) {
		t.Fatal("the VIN must never leave in a movement")
	}
	// no place known: explicit nulls (core shows coordinates)
	d := *s.Outbox[0].Drive
	d.StartPlace, d.EndPlace = "", ""
	c := s.Outbox[0]
	c.Drive = &d
	b2, _ := json.Marshal(MovementFor(c, ny))
	if !strings.Contains(string(b2), `"start_place":null`) || !strings.Contains(string(b2), `"end_place":null`) {
		t.Fatalf("places should be null: %s", b2)
	}
	doc := RouteDoc(s.Outbox[0])
	if pts := doc["points"].([][3]float64); len(pts) < 2 || len(pts) > MaxRoutePts {
		t.Fatalf("route points %d", len(pts))
	}
}

func TestChargeTitles(t *testing.T) {
	ny, _ := time.LoadLocation("America/New_York")
	soc := func(a, b float64) (*float64, *float64) { return &a, &b }
	mk := func(ch Charge) Closed {
		ch.Start, ch.End = t0, t0.Add(time.Hour)
		return Closed{Kind: "charge", Key: "charge:abc:1", Charge: &ch}
	}
	s1, e1 := soc(45, 80)
	home := ChargeEpisode(mk(Charge{EnergyIn: 23.4, ACKWh: 23.4, SocStart: s1, SocEnd: e1, Place: "Home"}), CarInfo{"Ember", "Model Y"}, ny)
	if home.Title != "Charged at Home — 23.4 kWh (45% → 80%)" || home.Kind != "charge" || home.Source != "htesla" {
		t.Fatalf("%q", home.Title)
	}
	if home.EventTime.Uncertainty != "minute" || home.EventTime.TZOffsetMinutes != -240 || home.EventTime.UTC != "2026-10-22T12:00:00Z" {
		t.Fatalf("%+v", home.EventTime)
	}
	if !strings.Contains(home.Body, "Place: Home") || home.Scope != nil {
		t.Fatalf("body %s", home.Body)
	}
	s2, e2 := soc(12, 70)
	sc := ChargeEpisode(mk(Charge{EnergyIn: 31.0, DCKWh: 31, SocStart: s2, SocEnd: e2, Fast: true, FastType: "Supercharger"}), CarInfo{"Ember", "Model Y"}, ny)
	if sc.Title != "Supercharged — 31.0 kWh (12% → 70%)" || !strings.Contains(sc.Body, "Tesla Supercharger") || strings.Contains(sc.Body, "Place:") {
		t.Fatalf("%q\n%s", sc.Title, sc.Body)
	}
	away := ChargeEpisode(mk(Charge{EnergyIn: 5, ACKWh: 5}), CarInfo{"Model Y", "Model Y"}, ny)
	if away.Title != "Charged — 5.0 kWh" || strings.Contains(away.Body, "Place:") {
		t.Fatalf("%q", away.Title)
	}
}

func TestUpdateEpisode(t *testing.T) {
	c := Closed{Kind: "version", Key: "version:abc:2026.38.6", Update: &Update{At: t0, From: "2026.32.1", To: "2026.38.6"}}
	ep := UpdateEpisode(c, CarInfo{"Ember", "Model Y"}, []ReleaseNote{{Title: "Dashcam Viewer", Subtitle: "Watch clips", Description: "Tap the icon."}}, time.UTC)
	if ep.Title != "Model Y updated to 2026.38.6" || ep.Kind != "software_update" || ep.SourceRef != "version:abc:2026.38.6" {
		t.Fatalf("%+v", ep)
	}
	if !strings.Contains(ep.Body, "### Dashcam Viewer") || !strings.Contains(ep.Body, "2026.32.1") {
		t.Fatal(ep.Body)
	}
	bare := UpdateEpisode(c, CarInfo{"Ember", "Model Y"}, nil, time.UTC)
	if !strings.Contains(bare.Body, "weren't available") {
		t.Fatal(bare.Body)
	}
}
