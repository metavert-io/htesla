package builder

import (
	"encoding/json"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/metavert-io/htesla/internal/receiver"
)

const vin = "7SAYGDEE0PA000001"

var t0 = time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)

func at(min float64) time.Time { return t0.Add(time.Duration(min * float64(time.Minute))) }

func rec(min float64, data map[string]any) receiver.Event {
	t := at(min)
	return receiver.Event{ReceivedAt: t.Add(2 * time.Second), CreatedAt: &t, VIN: vin, Topic: "V", Data: data}
}

func conn(status string, wall time.Time) receiver.Event {
	return receiver.Event{ReceivedAt: wall, VIN: vin, Topic: "connectivity", Status: status}
}

func loc(lat, lon float64) map[string]any { return map[string]any{"latitude": lat, "longitude": lon} }

func feed(s *State, evs ...receiver.Event) {
	SortEvents(evs)
	for _, e := range evs {
		s.Apply(e)
	}
}

// a 20-minute drive from home to work with a regen-free energy drop
func normalDrive() []receiver.Event {
	evs := []receiver.Event{
		rec(-5, map[string]any{"Gear": "ShiftStateP", "Location": loc(42.3600, -71.0600), "Odometer": 1000.0,
			"EnergyRemaining": 60.0, "LocatedAtHome": true, "LocatedAtWork": false, "VehicleName": "Ember", "CarType": "CarTypeModelY"}),
		rec(0, map[string]any{"Gear": "ShiftStateD"}),
	}
	for i := 1; i <= 20; i++ {
		d := map[string]any{"Location": loc(42.3600+0.002*float64(i), -71.0600-0.003*float64(i)), "VehicleSpeed": 30.0}
		if i%5 == 0 {
			d["OutsideTemp"] = 10.0 + float64(i)/5
		}
		if i == 3 {
			d["MediaNowPlayingTitle"], d["MediaNowPlayingArtist"] = "So What", "Miles Davis"
		}
		if i == 9 {
			d["MediaNowPlayingTitle"] = "Blue in Green"
		}
		if i == 10 {
			d["Odometer"] = 1005.0
			d["LocatedAtHome"] = false
		}
		evs = append(evs, rec(float64(i), d))
	}
	evs = append(evs,
		rec(20.5, map[string]any{"Odometer": 1010.0, "EnergyRemaining": 57.5, "LocatedAtWork": true}),
		rec(21, map[string]any{"Gear": "ShiftStateP", "VehicleSpeed": 0.0}),
		rec(24, map[string]any{"Locked": true}), // car-time evidence: parked ≥2 min
	)
	return evs
}

func TestNormalDrive(t *testing.T) {
	s := New()
	feed(s, normalDrive()...)
	if len(s.Outbox) != 1 {
		t.Fatalf("want 1 closed drive, got %d: %+v", len(s.Outbox), s.Outbox)
	}
	c := s.Outbox[0]
	d := c.Drive
	if c.Kind != "drive" || c.Key != "drive:"+VehicleID(vin)+":"+itoa(at(0).Unix()) {
		t.Fatalf("key %q", c.Key)
	}
	if !d.Start.Equal(at(0)) || !d.End.Equal(at(21)) {
		t.Fatalf("span %s–%s", d.Start, d.End)
	}
	// 10 miles on the odometer → metres
	if math.Abs(d.DistanceM-10*1609.344) > 0.01 || d.DistanceSrc != "odometer" {
		t.Fatalf("distance %.2f (%s)", d.DistanceM, d.DistanceSrc)
	}
	if d.EnergyKWh == nil || math.Abs(*d.EnergyKWh-2.5) > 1e-9 {
		t.Fatalf("energy %v", d.EnergyKWh)
	}
	if d.StartPlace != "Home" || d.EndPlace != "Work" {
		t.Fatalf("places %q → %q", d.StartPlace, d.EndPlace)
	}
	if *d.StartLat != 42.36 || math.Abs(*d.EndLat-42.40) > 1e-9 {
		t.Fatalf("ends %v %v", *d.StartLat, *d.EndLat)
	}
	if d.OutsideC == nil || math.Abs(*d.OutsideC-12.5) > 1e-9 {
		t.Fatalf("temp %v", d.OutsideC)
	}
	if len(d.Media) != 2 || d.Media[0] != (Media{"So What", "Miles Davis"}) || d.Media[1].Title != "Blue in Green" {
		t.Fatalf("media %+v", d.Media)
	}
	if d.Partial || d.Guest {
		t.Fatal("flags")
	}
	if d.OdoStartKm == nil || math.Abs(*d.OdoStartKm-1609.344) > 1e-6 {
		t.Fatalf("odometer km %v", d.OdoStartKm)
	}
	if len(d.Route) < 2 || d.Route[0][0] != 42.36 {
		t.Fatalf("route %v", d.Route)
	}
	// round-trips through JSON (the state is persisted between passes)
	b, _ := json.Marshal(s)
	var back State
	if err := json.Unmarshal(b, &back); err != nil || len(back.Outbox) != 1 {
		t.Fatal(err)
	}
}

func itoa(n int64) string { b, _ := json.Marshal(n); return string(b) }

func TestShortParkContinuesTheDrive(t *testing.T) {
	s := New()
	feed(s,
		rec(-1, map[string]any{"Gear": "ShiftStateP", "Odometer": 100.0, "Location": loc(40, -74)}),
		rec(0, map[string]any{"Gear": "ShiftStateD"}),
		rec(4, map[string]any{"Location": loc(40.01, -74), "Odometer": 101.0}),
		rec(5, map[string]any{"Gear": "ShiftStateP"}), // a quick stop…
		rec(6, map[string]any{"Gear": "ShiftStateD"}), // …60s later, off again
		rec(10, map[string]any{"Location": loc(40.03, -74), "Odometer": 103.0}),
		rec(12, map[string]any{"Gear": "ShiftStateP"}),
		rec(15, map[string]any{"Locked": true}),
	)
	if len(s.Outbox) != 1 {
		t.Fatalf("a stop under 2 minutes must not split the drive: %d drives", len(s.Outbox))
	}
	d := s.Outbox[0].Drive
	if !d.Start.Equal(at(0)) || !d.End.Equal(at(12)) || math.Abs(d.DistanceM-3*MilesToMeters) > 0.01 {
		t.Fatalf("%s–%s %.0f m", d.Start, d.End, d.DistanceM)
	}
}

func TestLongParkSplitsDrives(t *testing.T) {
	s := New()
	feed(s,
		rec(-1, map[string]any{"Gear": "ShiftStateP", "Odometer": 100.0}),
		rec(0, map[string]any{"Gear": "ShiftStateD"}),
		rec(4, map[string]any{"Odometer": 101.0}),
		rec(5, map[string]any{"Gear": "ShiftStateP"}),
		rec(8, map[string]any{"Gear": "ShiftStateD"}), // 3 minutes parked: a new drive
		rec(12, map[string]any{"Odometer": 102.0}),
		rec(13, map[string]any{"Gear": "ShiftStateP"}),
	)
	if len(s.Outbox) != 1 || !s.Outbox[0].Drive.End.Equal(at(5)) {
		t.Fatalf("first drive should close at the 3-minute park: %+v", s.Outbox)
	}
	if c := s.Cars[vin]; c.Drive == nil || !c.Drive.Start.Equal(at(8)) {
		t.Fatal("second drive should be open")
	}
	// the car goes quiet while parked: the clock closes it
	s.Tick(at(13).Add(5 * time.Minute))
	if len(s.Outbox) != 2 || !s.Outbox[1].Drive.End.Equal(at(13)) {
		t.Fatalf("quiet parked car should close by the clock: %+v", s.Outbox)
	}
}

func TestSocketCloseWhileParkedEndsTheDrive(t *testing.T) {
	s := New()
	feed(s,
		rec(-1, map[string]any{"Gear": "ShiftStateP", "Odometer": 100.0}),
		rec(0, map[string]any{"Gear": "ShiftStateD"}),
		rec(5, map[string]any{"Odometer": 102.0}),
		rec(6, map[string]any{"Gear": "ShiftStateP"}),
	)
	s.Apply(conn("disconnected", at(6.5).Add(5*time.Second)))
	if len(s.Outbox) != 1 || !s.Outbox[0].Drive.End.Equal(at(6)) || s.Outbox[0].Drive.Partial {
		t.Fatalf("socket close in P ends the drive: %+v", s.Outbox)
	}
}

func TestSocketDropMidDriveIsPartial(t *testing.T) {
	s := New()
	feed(s,
		rec(-1, map[string]any{"Gear": "ShiftStateP", "Odometer": 100.0, "EnergyRemaining": 40.0}),
		rec(0, map[string]any{"Gear": "ShiftStateD"}),
		rec(3, map[string]any{"Odometer": 101.5, "Location": loc(40.01, -74), "EnergyRemaining": 39.5}),
		rec(4, map[string]any{"Location": loc(40.02, -74)}),
	)
	drop := at(4).Add(20 * time.Second)
	s.Apply(conn("disconnected", drop))
	s.Tick(drop.Add(5 * time.Minute))
	if len(s.Outbox) != 0 {
		t.Fatal("within the grace period the drive stays open (the car may come back)")
	}
	// it came back and kept driving: still one drive
	s.Apply(conn("connected", drop.Add(6*time.Minute)))
	s.Tick(drop.Add(20 * time.Minute))
	if len(s.Outbox) != 0 && s.Cars[vin].Drive == nil {
		// reconnected but silent for 20 min (< SilentDrive): must still be open
		t.Fatal("reconnected drive closed too early")
	}
	// now it drops for good
	s.Apply(conn("disconnected", drop.Add(21*time.Minute)))
	s.Tick(drop.Add(21*time.Minute + PartialGrace + time.Second))
	if len(s.Outbox) != 1 {
		t.Fatalf("want a partial drive, got %+v", s.Outbox)
	}
	d := s.Outbox[0].Drive
	if !d.Partial || !d.End.Equal(at(4)) || d.EndPlace != "" {
		t.Fatalf("partial drive ends at its last record: %+v", d)
	}
}

func TestRegenNeverMakesEnergyNegative(t *testing.T) {
	s := New()
	feed(s,
		rec(-1, map[string]any{"Gear": "ShiftStateP", "Odometer": 100.0, "EnergyRemaining": 50.0}),
		rec(0, map[string]any{"Gear": "ShiftStateD"}),
		rec(5, map[string]any{"Odometer": 102.0, "EnergyRemaining": 50.6}), // downhill
		rec(6, map[string]any{"Gear": "ShiftStateP"}),
		rec(9, map[string]any{"Locked": true}),
	)
	if len(s.Outbox) != 1 || s.Outbox[0].Drive.EnergyKWh == nil || *s.Outbox[0].Drive.EnergyKWh != 0 {
		t.Fatalf("energy must be clamped at 0: %+v", s.Outbox[0].Drive.EnergyKWh)
	}
}

func TestShiftWithoutMovingIsDropped(t *testing.T) {
	s := New()
	feed(s,
		rec(-1, map[string]any{"Gear": "ShiftStateP", "Odometer": 100.0}),
		rec(0, map[string]any{"Gear": "ShiftStateD"}),
		rec(0.5, map[string]any{"Gear": "ShiftStateP"}),
		rec(4, map[string]any{"Locked": true}),
	)
	if len(s.Outbox) != 0 || s.Dropped != 1 {
		t.Fatalf("a D-and-back with no distance isn't a drive: %+v", s.Outbox)
	}
}

func TestSpeedStartsADriveWithoutGear(t *testing.T) {
	s := New()
	feed(s,
		rec(0, map[string]any{"VehicleSpeed": 12.0, "Location": loc(40, -74), "Odometer": 50.0}),
		rec(5, map[string]any{"Location": loc(40.02, -74), "Odometer": 51.2}),
	)
	if s.Cars[vin].Drive == nil {
		t.Fatal("speed with no Gear yet should open a drive")
	}
}

func TestChargeSessionAC(t *testing.T) {
	s := New()
	feed(s,
		rec(-2, map[string]any{"DetailedChargeState": "DetailedChargeStateDisconnected", "BatteryLevel": 45.0,
			"LocatedAtHome": true, "ACChargingEnergyIn": 23.4}), // last session's counter
		rec(0, map[string]any{"DetailedChargeState": "DetailedChargeStateStarting"}),
		rec(1, map[string]any{"DetailedChargeState": "DetailedChargeStateCharging", "ACChargingEnergyIn": 0.0}),
		rec(60, map[string]any{"ACChargingEnergyIn": 7.0, "BatteryLevel": 55.0}),
		rec(90, map[string]any{"DetailedChargeState": "DetailedChargeStateStopped"}), // scheduled pause
		rec(200, map[string]any{"DetailedChargeState": "DetailedChargeStateCharging"}),
		rec(260, map[string]any{"ACChargingEnergyIn": 15.0}),
		rec(300, map[string]any{"ACChargingEnergyIn": 18.5, "BatteryLevel": 80.0}),
		rec(301, map[string]any{"DetailedChargeState": "DetailedChargeStateComplete"}),
		rec(400, map[string]any{"DetailedChargeState": "DetailedChargeStateDisconnected"}),
	)
	if len(s.Outbox) != 1 {
		t.Fatalf("want 1 session (the pause continues it), got %+v", s.Outbox)
	}
	c := s.Outbox[0]
	ch := c.Charge
	if c.Key != "charge:"+VehicleID(vin)+":"+itoa(at(0).Unix()) || !ch.End.Equal(at(301)) {
		t.Fatalf("key %s end %s", c.Key, ch.End)
	}
	if math.Abs(ch.EnergyIn-18.5) > 1e-9 || ch.DCKWh != 0 || ch.Fast {
		t.Fatalf("energy %.2f dc %.2f fast %v", ch.EnergyIn, ch.DCKWh, ch.Fast)
	}
	if *ch.SocStart != 45 || *ch.SocEnd != 80 || ch.Place != "Home" {
		t.Fatalf("soc %v→%v place %q", *ch.SocStart, *ch.SocEnd, ch.Place)
	}
}

func TestSuperchargeWithCounterRestart(t *testing.T) {
	s := New()
	feed(s,
		rec(-1, map[string]any{"BatteryLevel": 12.0, "LocatedAtHome": false, "Location": loc(41, -73)}),
		rec(0, map[string]any{"DetailedChargeState": "DetailedChargeStateCharging", "FastChargerPresent": true,
			"FastChargerType": "FastChargerSupercharger", "DCChargingEnergyIn": 0.0}),
		rec(10, map[string]any{"DCChargingEnergyIn": 20.0}),
		rec(11, map[string]any{"DCChargingEnergyIn": 0.5}), // the counter restarted
		rec(25, map[string]any{"DCChargingEnergyIn": 11.0, "BatteryLevel": 70.0}),
		rec(26, map[string]any{"DetailedChargeState": "DetailedChargeStateDisconnected"}),
	)
	ch := s.Outbox[0].Charge
	if math.Abs(ch.DCKWh-31.0) > 1e-9 || !ch.Fast || ch.FastType != "Supercharger" || ch.Place != "" {
		t.Fatalf("%+v", ch)
	}
	if ch.Loc == nil || ch.Loc[0] != 41 {
		t.Fatal("location of the session")
	}
}

func TestVersionChangeDetection(t *testing.T) {
	s := New()
	feed(s,
		rec(0, map[string]any{"Version": "2026.32.1 a1b2c3d4"}),
		rec(10, map[string]any{"Version": "2026.32.1 a1b2c3d4"}),
	)
	if len(s.Outbox) != 0 {
		t.Fatal("the first version seen is a baseline, not an update")
	}
	feed(s, rec(600, map[string]any{"Version": "2026.38.6 e5f6a7b8"}))
	feed(s, rec(700, map[string]any{"Version": "2026.38.6 e5f6a7b8"}))
	if len(s.Outbox) != 1 {
		t.Fatalf("want one update, got %+v", s.Outbox)
	}
	u := s.Outbox[0]
	if u.Key != "version:"+VehicleID(vin)+":2026.38.6" || u.Update.From != "2026.32.1" || u.Update.To != "2026.38.6" || !u.Update.At.Equal(at(600)) {
		t.Fatalf("%+v %+v", u, u.Update)
	}
}

func TestLateResendFillsButDoesNotRewind(t *testing.T) {
	s := New()
	feed(s,
		rec(-1, map[string]any{"Gear": "ShiftStateP", "Odometer": 10.0}),
		rec(0, map[string]any{"Gear": "ShiftStateD"}),
		rec(10, map[string]any{"Odometer": 12.0, "Location": loc(40.1, -74)}),
	)
	// a buffered record from minute 5 arrives late
	s.Apply(rec(5, map[string]any{"Location": loc(40.05, -74), "Gear": "ShiftStateP"}))
	d := s.Cars[vin].Drive
	if d == nil || d.ParkedSince != nil || len(d.Points) != 2 {
		t.Fatalf("a late P must not park a drive that has moved on: %+v", d)
	}
	if g := gear(s.Cars[vin].Latest["Gear"].V); g != "D" {
		t.Fatalf("latest gear rewound to %q", g)
	}
}

func TestThinKeepsEndsAndCap(t *testing.T) {
	var p []Point
	for i := 0; i < 5000; i++ {
		f := float64(i)
		p = append(p, Point{40 + f*0.0001, -74 + 0.002*math.Sin(f/50), f})
	}
	out := Thin(p, 500)
	if len(out) > 500 || len(out) < 50 {
		t.Fatalf("thinned to %d", len(out))
	}
	if out[0] != p[0] || out[len(out)-1] != p[len(p)-1] {
		t.Fatal("ends must survive")
	}
	if math.Abs(PathLength(out)-PathLength(p))/PathLength(p) > 0.02 {
		t.Fatalf("shape lost: %.0f vs %.0f m", PathLength(out), PathLength(p))
	}
	short := Thin(p[:10], 500)
	if len(short) != 10 {
		t.Fatal("short routes untouched")
	}
}

func TestVehicleIDIsOpaque(t *testing.T) {
	id := VehicleID(vin)
	if len(id) != 12 || strings.Contains(strings.ToUpper(vin), strings.ToUpper(id)) {
		t.Fatalf("id %q", id)
	}
	if VehicleID(strings.ToLower(vin)) != id {
		t.Fatal("case-insensitive")
	}
	if ModelFromVIN(vin) != "Model Y" || ModelFromVIN("5YJ3E1EA0KF000001") != "Model 3" {
		t.Fatal("model from VIN")
	}
	s := New()
	feed(s, rec(0, map[string]any{"CarType": "CarTypeModelY", "VehicleName": "Ember"}))
	if s.Cars[vin].Model() != "Model Y" || s.Cars[vin].Name() != "Ember" {
		t.Fatal("name/model")
	}
}
