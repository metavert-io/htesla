// Package builder turns the stored telemetry stream into records:
// drives (posted to Hearthloom's movement layer), charging sessions and
// software updates (posted as episodes). It is pure — events in, closed
// records out — so the rules are unit-tested without a car; the sidecar
// feeds it the minute chunks in order and persists State (cursor
// included) in one KV document, so a restart neither loses nor repeats
// a closed record.
//
// Rules (all times are the car's own created_at; connectivity events
// carry the time we saw the socket open/close):
//   - a drive starts when Gear leaves P (or, with no Gear yet, when the
//     speed is above zero); it ends once Gear has been P for ≥2 minutes
//     (by the car's later records, or by the clock when the car has gone
//     quiet), or when the socket closes while in P. A socket that drops
//     mid-drive ends the drive at its last record, flagged partial, once
//     the car has stayed away for PartialGrace.
//   - distance from the Odometer delta (Tesla reports miles), energy from
//     the EnergyRemaining delta (start − end, never negative — regen can
//     raise it), outside temperature averaged, distinct media ≤12.
//   - a charging session runs from DetailedChargeState Starting/Charging
//     to Complete or Disconnected (a Stopped/NoPower pause in between
//     continues it); energy in sums the AC/DC energy-in counters'
//     increases (a counter that restarts is handled).
//   - a software update is a change of Version from one already seen.
package builder

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/metavert-io/htesla/internal/receiver"
)

// Tunables.
const (
	ParkClose     = 2 * time.Minute  // Gear P this long ends a drive
	PartialGrace  = 10 * time.Minute // car gone this long mid-drive → partial
	SilentDrive   = 30 * time.Minute // connected but silent mid-drive → partial
	SilentCharge  = 12 * time.Hour   // a charge session with no records this long closes
	MinDriveM     = 50.0             // shorter "drives" are a shift into D and back
	MaxRoutePts   = 500              // a posted route
	MaxOpenPts    = 2000             // an open drive's buffer before it's thinned
	MaxMedia      = 12
	MilesToMeters = 1609.344
)

// VehicleID is the opaque id Hearthloom sees: the first 12 hex
// characters of sha256(VIN) — stable, and not the VIN.
func VehicleID(vin string) string {
	sum := sha256.Sum256([]byte(strings.ToUpper(strings.TrimSpace(vin))))
	return hex.EncodeToString(sum[:])[:12]
}

// ModelFromVIN reads the model line from the VIN's 4th character.
func ModelFromVIN(vin string) string {
	if len(vin) < 4 {
		return ""
	}
	switch strings.ToUpper(vin[3:4]) {
	case "S":
		return "Model S"
	case "X":
		return "Model X"
	case "3":
		return "Model 3"
	case "Y":
		return "Model Y"
	case "C":
		return "Cybertruck"
	case "T":
		return "Semi"
	}
	return ""
}

var carTypes = map[string]string{
	"CarTypeModelS": "Model S", "CarTypeModelX": "Model X", "CarTypeModel3": "Model 3",
	"CarTypeModelY": "Model Y", "CarTypeCybertruck": "Cybertruck", "CarTypeSemiTruck": "Semi",
}

// FieldAt is a remembered value and the car time it was true.
type FieldAt struct {
	V  any       `json:"v"`
	At time.Time `json:"t"`
}

// Point is one route fix: latitude, longitude, unix seconds.
type Point [3]float64

// Media is one thing that played.
type Media struct {
	Title  string `json:"title"`
	Artist string `json:"artist,omitempty"`
}

// OpenDrive is a drive in progress.
type OpenDrive struct {
	Start       time.Time   `json:"start"`
	StartLoc    *[2]float64 `json:"start_loc,omitempty"` // where the car stood when it left
	StartHome   bool        `json:"start_home,omitempty"`
	StartWork   bool        `json:"start_work,omitempty"`
	Points      []Point     `json:"points,omitempty"`
	OdoStart    *float64    `json:"odo_start,omitempty"` // miles
	OdoLast     *float64    `json:"odo_last,omitempty"`
	EnergyStart *float64    `json:"energy_start,omitempty"` // kWh
	EnergyLast  *float64    `json:"energy_last,omitempty"`
	TempSum     float64     `json:"temp_sum,omitempty"`
	TempN       int         `json:"temp_n,omitempty"`
	Media       []Media     `json:"media,omitempty"`
	Guest       bool        `json:"guest,omitempty"`
	ParkedSince *time.Time  `json:"parked_since,omitempty"`
	LastAt      time.Time   `json:"last_at"`
}

// counter sums a cumulative counter's increases (one that restarts at
// zero keeps counting from its new value).
type counter struct {
	Seen  bool    `json:"seen,omitempty"`
	Prev  float64 `json:"prev,omitempty"`
	Total float64 `json:"total,omitempty"`
}

func (c *counter) add(v float64) {
	if !c.Seen {
		c.Seen, c.Prev = true, v
		return
	}
	if v >= c.Prev {
		c.Total += v - c.Prev
	} else {
		c.Total += v // the counter restarted
	}
	c.Prev = v
}

// OpenCharge is a charging session in progress.
type OpenCharge struct {
	Start          time.Time   `json:"start"`
	SocStart       *float64    `json:"soc_start,omitempty"`
	AC             counter     `json:"ac"`
	DC             counter     `json:"dc"`
	Fast           bool        `json:"fast,omitempty"`
	FastType       string      `json:"fast_type,omitempty"`
	Home           bool        `json:"home,omitempty"`
	Work           bool        `json:"work,omitempty"`
	Loc            *[2]float64 `json:"loc,omitempty"`
	StoppedAt      *time.Time  `json:"stopped_at,omitempty"`
	LastChargingAt time.Time   `json:"last_charging_at"`
	LastAt         time.Time   `json:"last_at"`
}

// Car is everything the builder remembers about one car.
type Car struct {
	VIN            string             `json:"vin"`
	Latest         map[string]FieldAt `json:"latest"`
	LastEventAt    time.Time          `json:"last_event_at"`
	LastRecvAt     time.Time          `json:"last_recv_at"`
	Connected      bool               `json:"connected"`
	DisconnectedAt time.Time          `json:"disconnected_at"`
	Drive          *OpenDrive         `json:"drive,omitempty"`
	Charge         *OpenCharge        `json:"charge,omitempty"`
	Version        string             `json:"version,omitempty"`
}

// Closed is a finished record waiting to be posted.
type Closed struct {
	Kind string `json:"kind"` // drive | charge | version
	Key  string `json:"key"`
	VIN  string `json:"vin"`
	VID  string `json:"vid"`

	Drive  *Drive  `json:"drive,omitempty"`
	Charge *Charge `json:"charge,omitempty"`
	Update *Update `json:"update,omitempty"`
}

// Drive is a closed drive.
type Drive struct {
	Start       time.Time `json:"start"`
	End         time.Time `json:"end"`
	StartLat    *float64  `json:"start_lat,omitempty"`
	StartLon    *float64  `json:"start_lon,omitempty"`
	EndLat      *float64  `json:"end_lat,omitempty"`
	EndLon      *float64  `json:"end_lon,omitempty"`
	StartPlace  string    `json:"start_place,omitempty"`
	EndPlace    string    `json:"end_place,omitempty"`
	DistanceM   float64   `json:"distance_m"`
	DistanceSrc string    `json:"distance_source"` // odometer | gps
	EnergyKWh   *float64  `json:"energy_kwh,omitempty"`
	OutsideC    *float64  `json:"outside_temp_c,omitempty"`
	Guest       bool      `json:"guest_mode"`
	Media       []Media   `json:"media,omitempty"`
	OdoStartKm  *float64  `json:"odometer_start_km,omitempty"`
	OdoEndKm    *float64  `json:"odometer_end_km,omitempty"`
	Partial     bool      `json:"partial,omitempty"`
	Route       []Point   `json:"route,omitempty"`
}

// Charge is a closed charging session.
type Charge struct {
	Start    time.Time   `json:"start"`
	End      time.Time   `json:"end"`
	EnergyIn float64     `json:"energy_in_kwh"`
	ACKWh    float64     `json:"ac_kwh"`
	DCKWh    float64     `json:"dc_kwh"`
	SocStart *float64    `json:"soc_start,omitempty"`
	SocEnd   *float64    `json:"soc_end,omitempty"`
	Fast     bool        `json:"fast"`
	FastType string      `json:"fast_type,omitempty"`
	Place    string      `json:"place,omitempty"` // Home | Work | ""
	Loc      *[2]float64 `json:"loc,omitempty"`
	Partial  bool        `json:"partial,omitempty"`
}

// Update is a software version change.
type Update struct {
	At   time.Time `json:"at"`
	From string    `json:"from"`
	To   string    `json:"to"`
}

// State is the builder's whole memory (persisted as one document).
type State struct {
	V      int             `json:"v"`
	Cursor string          `json:"cursor"` // the last chunk path consumed
	Cars   map[string]*Car `json:"cars"`   // by VIN
	Outbox []Closed        `json:"outbox"` // closed, not yet posted
	Posted []string        `json:"posted"` // recent keys (for status)
	// Dropped counts "drives" too short to keep (a shift into D and back).
	Dropped int `json:"dropped,omitempty"`
}

// New returns an empty state.
func New() *State { return &State{V: 1, Cars: map[string]*Car{}} }

func (s *State) car(vin string) *Car {
	if s.Cars == nil {
		s.Cars = map[string]*Car{}
	}
	c := s.Cars[vin]
	if c == nil {
		c = &Car{VIN: vin, Latest: map[string]FieldAt{}, Connected: true}
		s.Cars[vin] = c
	}
	if c.Latest == nil {
		c.Latest = map[string]FieldAt{}
	}
	return c
}

// EventTime is when an event happened: the car's created_at when it has
// one, else when we received it.
func EventTime(ev receiver.Event) time.Time {
	if ev.CreatedAt != nil && !ev.CreatedAt.IsZero() {
		return ev.CreatedAt.UTC()
	}
	return ev.ReceivedAt.UTC()
}

// SortEvents orders a batch by event time (stable, so a record and the
// connectivity event after it keep their order at equal times).
func SortEvents(evs []receiver.Event) {
	sort.SliceStable(evs, func(i, j int) bool { return EventTime(evs[i]).Before(EventTime(evs[j])) })
}

/* ———— value helpers ———— */

func num(v any) (float64, bool) {
	switch x := v.(type) {
	case float64:
		return x, !math.IsNaN(x) && !math.IsInf(x, 0)
	case float32:
		return float64(x), true
	case int:
		return float64(x), true
	case int32:
		return float64(x), true
	case int64:
		return float64(x), true
	case string:
		var f float64
		if _, err := fmt.Sscanf(x, "%g", &f); err == nil {
			return f, true
		}
	}
	return 0, false
}

func boolOf(v any) (bool, bool) {
	switch x := v.(type) {
	case bool:
		return x, true
	case string:
		switch strings.ToLower(x) {
		case "true":
			return true, true
		case "false":
			return false, true
		}
	}
	return false, false
}

func strOf(v any) string {
	s, _ := v.(string)
	return s
}

// gear normalizes Gear to P | D | R | N | "" (unknown/invalid).
func gear(v any) string {
	s := strings.TrimPrefix(strOf(v), "ShiftState")
	switch s {
	case "P", "D", "R", "N":
		return s
	}
	return ""
}

// chargeState normalizes DetailedChargeState (typed or legacy string).
func chargeState(v any) string {
	s := strOf(v)
	s = strings.TrimPrefix(s, "DetailedChargeState")
	s = strings.TrimPrefix(s, "ChargeState")
	return s
}

func location(v any) (lat, lon float64, ok bool) {
	m, isMap := v.(map[string]any)
	if !isMap {
		return 0, 0, false
	}
	la, ok1 := num(m["latitude"])
	lo, ok2 := num(m["longitude"])
	if !ok1 || !ok2 || (la == 0 && lo == 0) || math.Abs(la) > 90 || math.Abs(lo) > 180 {
		return 0, 0, false
	}
	return la, lo, true
}

func (c *Car) latestNum(field string) *float64 {
	if f, ok := c.Latest[field]; ok {
		if v, ok := num(f.V); ok {
			return &v
		}
	}
	return nil
}

func (c *Car) latestBool(field string) bool {
	if f, ok := c.Latest[field]; ok {
		b, _ := boolOf(f.V)
		return b
	}
	return false
}

func (c *Car) latestLoc() *[2]float64 {
	if f, ok := c.Latest["Location"]; ok {
		if la, lo, ok := location(f.V); ok {
			return &[2]float64{la, lo}
		}
	}
	return nil
}

// Model is the car's model line (CarType when streamed, else the VIN).
func (c *Car) Model() string {
	if f, ok := c.Latest["CarType"]; ok {
		if m := carTypes[strOf(f.V)]; m != "" {
			return m
		}
	}
	return ModelFromVIN(c.VIN)
}

// Name is what the owner calls the car (VehicleName), else the model.
func (c *Car) Name() string {
	if f, ok := c.Latest["VehicleName"]; ok {
		if n := strings.TrimSpace(strOf(f.V)); n != "" {
			return n
		}
	}
	if m := c.Model(); m != "" {
		return m
	}
	return "Tesla"
}

/* ———— applying events ———— */

// Apply feeds one event.
func (s *State) Apply(ev receiver.Event) {
	if ev.VIN == "" {
		return
	}
	c := s.car(ev.VIN)
	t := EventTime(ev)
	switch ev.Topic {
	case "connectivity":
		if ev.Status == "connected" {
			c.Connected = true
			c.DisconnectedAt = time.Time{}
		} else if ev.Status == "disconnected" {
			c.Connected = false
			c.DisconnectedAt = ev.ReceivedAt.UTC()
			// the socket closed while parked: the drive is over
			if c.Drive != nil && c.Drive.ParkedSince != nil {
				s.closeDrive(c, *c.Drive.ParkedSince, false)
			}
		}
		return
	case "V":
	default:
		return
	}
	if ev.ReceivedAt.After(c.LastRecvAt) {
		c.LastRecvAt = ev.ReceivedAt.UTC()
	}
	older := t.Before(c.LastEventAt)
	if !older {
		c.LastEventAt = t
	}
	for k, v := range ev.Data {
		if older && k == "Gear" {
			continue // the gear the state machine acted on stays the gear
		}
		if prev, ok := c.Latest[k]; !ok || !t.Before(prev.At) {
			c.Latest[k] = FieldAt{V: v, At: t}
		}
	}
	if older {
		// a late resend: it may fill in an open drive, but never moves
		// the state machine backwards
		if c.Drive != nil && !t.Before(c.Drive.Start) {
			s.accumulate(c, ev.Data, t)
		}
		if c.Charge != nil && !t.Before(c.Charge.Start) {
			accumulateCharge(c, ev.Data)
		}
		return
	}

	// —— drives ——
	// parked long enough by THIS record's time: that drive is over before
	// anything in this record can continue it
	if c.Drive != nil && c.Drive.ParkedSince != nil && t.Sub(*c.Drive.ParkedSince) >= ParkClose {
		s.closeDrive(c, *c.Drive.ParkedSince, false)
	}
	g, hasGear := "", false
	if v, ok := ev.Data["Gear"]; ok {
		g, hasGear = gear(v), true
	}
	if c.Drive == nil {
		curGear := ""
		if f, ok := c.Latest["Gear"]; ok {
			curGear = gear(f.V)
		}
		_, everGear := c.Latest["Gear"]
		sp, hasSpeed := num(ev.Data["VehicleSpeed"])
		moving := hasSpeed && sp > 0
		_, hasLoc := ev.Data["Location"]
		start := false
		switch {
		case curGear == "D" || curGear == "R" || curGear == "N":
			// out of Park — on the Gear change itself, or (after a car
			// came back mid-drive) on the next sign of motion
			start = hasGear || moving || hasLoc
		case !everGear:
			start = moving // no Gear streamed yet: speed is the tell
		}
		if start {
			s.openDrive(c, t)
		}
	} else if hasGear {
		switch g {
		case "D", "R", "N":
			c.Drive.ParkedSince = nil // a stop shorter than ParkClose continues the drive
		case "P":
			if c.Drive.ParkedSince == nil {
				ps := t
				c.Drive.ParkedSince = &ps
			}
		}
	}
	if c.Drive != nil {
		s.accumulate(c, ev.Data, t)
		if ps := c.Drive.ParkedSince; ps != nil && t.Sub(*ps) >= ParkClose {
			s.closeDrive(c, *ps, false)
		}
	}

	// —— charging ——
	if v, ok := ev.Data["DetailedChargeState"]; ok {
		s.chargeTransition(c, chargeState(v), t)
	}
	if c.Charge != nil {
		accumulateCharge(c, ev.Data)
		c.Charge.LastAt = t
	}

	// —— software ——
	if v, ok := ev.Data["Version"]; ok {
		ver := strings.Fields(strOf(v))
		if len(ver) > 0 {
			if c.Version != "" && ver[0] != c.Version {
				s.Outbox = append(s.Outbox, Closed{
					Kind: "version", Key: "version:" + VehicleID(c.VIN) + ":" + ver[0],
					VIN: c.VIN, VID: VehicleID(c.VIN),
					Update: &Update{At: t, From: c.Version, To: ver[0]},
				})
			}
			c.Version = ver[0]
		}
	}
}

func (s *State) openDrive(c *Car, t time.Time) {
	d := &OpenDrive{Start: t, LastAt: t}
	d.StartLoc = c.latestLoc()
	d.StartHome = c.latestBool("LocatedAtHome")
	d.StartWork = c.latestBool("LocatedAtWork")
	d.OdoStart = c.latestNum("Odometer")
	d.EnergyStart = c.latestNum("EnergyRemaining")
	if c.latestBool("GuestModeEnabled") {
		d.Guest = true
	}
	c.Drive = d
}

func (s *State) accumulate(c *Car, data map[string]any, t time.Time) {
	d := c.Drive
	if t.After(d.LastAt) {
		d.LastAt = t
	}
	if la, lo, ok := location(data["Location"]); ok {
		d.Points = append(d.Points, Point{la, lo, float64(t.Unix())})
		if len(d.Points) > MaxOpenPts {
			sortPoints(d.Points)
			d.Points = Thin(d.Points, MaxOpenPts/2)
		}
	}
	if v, ok := num(data["Odometer"]); ok {
		if d.OdoStart == nil {
			d.OdoStart = &v
		}
		if d.OdoLast == nil || v >= *d.OdoLast {
			d.OdoLast = &v
		}
	}
	if v, ok := num(data["EnergyRemaining"]); ok {
		if d.EnergyStart == nil {
			d.EnergyStart = &v
		}
		vv := v
		d.EnergyLast = &vv
	}
	if v, ok := num(data["OutsideTemp"]); ok {
		d.TempSum += v
		d.TempN++
	}
	if b, ok := boolOf(data["GuestModeEnabled"]); ok && b {
		d.Guest = true
	}
	if title := strings.TrimSpace(strOf(data["MediaNowPlayingTitle"])); title != "" {
		artist := strings.TrimSpace(strOf(data["MediaNowPlayingArtist"]))
		if artist == "" {
			if f, ok := c.Latest["MediaNowPlayingArtist"]; ok {
				artist = strings.TrimSpace(strOf(f.V))
			}
		}
		addMedia(d, Media{Title: title, Artist: artist})
	} else if artist := strings.TrimSpace(strOf(data["MediaNowPlayingArtist"])); artist != "" {
		// the artist arrived after its title: complete the last entry
		if n := len(d.Media); n > 0 && d.Media[n-1].Artist == "" {
			d.Media[n-1].Artist = artist
		}
	}
}

func addMedia(d *OpenDrive, m Media) {
	for i, x := range d.Media {
		if strings.EqualFold(x.Title, m.Title) && (x.Artist == "" || m.Artist == "" || strings.EqualFold(x.Artist, m.Artist)) {
			if d.Media[i].Artist == "" {
				d.Media[i].Artist = m.Artist
			}
			return
		}
	}
	if len(d.Media) < MaxMedia {
		d.Media = append(d.Media, m)
	}
}

func sortPoints(p []Point) {
	sort.SliceStable(p, func(i, j int) bool { return p[i][2] < p[j][2] })
}

func (s *State) closeDrive(c *Car, end time.Time, partial bool) {
	d := c.Drive
	c.Drive = nil
	if d == nil {
		return
	}
	if end.Before(d.Start) {
		end = d.Start
	}
	sortPoints(d.Points)
	var pts []Point
	for _, p := range d.Points {
		if p[2] <= float64(end.Unix())+60 { // fixes up to a minute after parking still describe the stop
			pts = append(pts, p)
		}
	}
	out := &Drive{Start: d.Start, End: end, Guest: d.Guest, Media: d.Media, Partial: partial}
	if d.StartLoc != nil {
		out.StartLat, out.StartLon = &d.StartLoc[0], &d.StartLoc[1]
	} else if len(pts) > 0 {
		out.StartLat, out.StartLon = &pts[0][0], &pts[0][1]
	}
	if len(pts) > 0 {
		last := pts[len(pts)-1]
		out.EndLat, out.EndLon = &last[0], &last[1]
	} else if loc := c.latestLoc(); loc != nil {
		out.EndLat, out.EndLon = &loc[0], &loc[1]
	}
	// the route runs from where the car stood to where it stopped
	route := pts
	if d.StartLoc != nil && (len(route) == 0 || route[0][0] != d.StartLoc[0] || route[0][1] != d.StartLoc[1]) {
		route = append([]Point{{d.StartLoc[0], d.StartLoc[1], float64(d.Start.Unix())}}, route...)
	}
	out.Route = Thin(route, MaxRoutePts)
	if d.StartHome {
		out.StartPlace = "Home"
	} else if d.StartWork {
		out.StartPlace = "Work"
	}
	if !partial {
		if c.latestBool("LocatedAtHome") {
			out.EndPlace = "Home"
		} else if c.latestBool("LocatedAtWork") {
			out.EndPlace = "Work"
		}
	}
	if d.OdoStart != nil && d.OdoLast != nil && *d.OdoLast >= *d.OdoStart {
		out.DistanceM = (*d.OdoLast - *d.OdoStart) * MilesToMeters
		out.DistanceSrc = "odometer"
		a, b := *d.OdoStart*MilesToMeters/1000, *d.OdoLast*MilesToMeters/1000
		out.OdoStartKm, out.OdoEndKm = &a, &b
	}
	if out.DistanceSrc == "" || (out.DistanceM == 0 && len(route) > 1) {
		out.DistanceM = PathLength(route)
		out.DistanceSrc = "gps"
	}
	if d.EnergyStart != nil && d.EnergyLast != nil {
		e := math.Max(0, *d.EnergyStart-*d.EnergyLast)
		out.EnergyKWh = &e
	}
	if d.TempN > 0 {
		m := d.TempSum / float64(d.TempN)
		out.OutsideC = &m
	}
	if out.DistanceM < MinDriveM {
		s.Dropped++
		return
	}
	vid := VehicleID(c.VIN)
	s.Outbox = append(s.Outbox, Closed{
		Kind: "drive", Key: fmt.Sprintf("drive:%s:%d", vid, d.Start.Unix()),
		VIN: c.VIN, VID: vid, Drive: out,
	})
}

/* ———— charging ———— */

func isCharging(st string) bool { return st == "Starting" || st == "Charging" }

func (s *State) chargeTransition(c *Car, st string, t time.Time) {
	switch {
	case c.Charge == nil && isCharging(st):
		ch := &OpenCharge{Start: t, LastChargingAt: t, LastAt: t}
		ch.SocStart = c.latestNum("BatteryLevel")
		ch.Home = c.latestBool("LocatedAtHome")
		ch.Work = c.latestBool("LocatedAtWork")
		ch.Loc = c.latestLoc()
		c.Charge = ch
	case c.Charge != nil && isCharging(st):
		c.Charge.StoppedAt = nil
		c.Charge.LastChargingAt = t
	case c.Charge != nil && (st == "Stopped" || st == "NoPower"):
		if c.Charge.StoppedAt == nil {
			ts := t
			c.Charge.StoppedAt = &ts
		}
	case c.Charge != nil && (st == "Complete" || st == "Disconnected"):
		end := t
		if c.Charge.StoppedAt != nil {
			end = *c.Charge.StoppedAt
		}
		accumulateCharge(c, map[string]any{}) // nothing new; keeps the shape
		s.closeCharge(c, end, false)
	}
}

func accumulateCharge(c *Car, data map[string]any) {
	ch := c.Charge
	if v, ok := num(data["ACChargingEnergyIn"]); ok {
		ch.AC.add(v)
	}
	if v, ok := num(data["DCChargingEnergyIn"]); ok {
		ch.DC.add(v)
	}
	if b, ok := boolOf(data["FastChargerPresent"]); ok && b {
		ch.Fast = true
	}
	if ft := strings.TrimPrefix(strOf(data["FastChargerType"]), "FastCharger"); ft != "" && ft != "Unknown" && ft != "SNA" {
		ch.FastType = ft
	}
	if ch.Loc == nil {
		if la, lo, ok := location(data["Location"]); ok {
			ch.Loc = &[2]float64{la, lo}
		}
	}
}

func (s *State) closeCharge(c *Car, end time.Time, partial bool) {
	ch := c.Charge
	c.Charge = nil
	if ch == nil {
		return
	}
	out := &Charge{
		Start: ch.Start, End: end, ACKWh: ch.AC.Total, DCKWh: ch.DC.Total,
		Fast: ch.Fast || ch.DC.Total > 0.05, FastType: ch.FastType,
		SocStart: ch.SocStart, SocEnd: c.latestNum("BatteryLevel"), Loc: ch.Loc, Partial: partial,
	}
	out.EnergyIn = out.ACKWh + out.DCKWh
	if ch.Home {
		out.Place = "Home"
	} else if ch.Work {
		out.Place = "Work"
	}
	if out.EnergyIn < 0.1 && end.Sub(ch.Start) < 2*time.Minute {
		return // a plug-in blip
	}
	vid := VehicleID(c.VIN)
	s.Outbox = append(s.Outbox, Closed{
		Kind: "charge", Key: fmt.Sprintf("charge:%s:%d", vid, ch.Start.Unix()),
		VIN: c.VIN, VID: vid, Charge: out,
	})
}

/* ———— the clock ———— */

// Tick closes what time alone can close: a parked car that went quiet,
// a car that dropped off mid-drive, a charge session that went silent.
func (s *State) Tick(now time.Time) {
	vins := make([]string, 0, len(s.Cars))
	for v := range s.Cars {
		vins = append(vins, v)
	}
	sort.Strings(vins)
	for _, vin := range vins {
		c := s.Cars[vin]
		if d := c.Drive; d != nil {
			quiet := now.Sub(c.LastRecvAt)
			switch {
			case d.ParkedSince != nil && (c.LastEventAt.Sub(*d.ParkedSince) >= ParkClose ||
				(now.Sub(*d.ParkedSince) >= ParkClose && quiet >= ParkClose)):
				s.closeDrive(c, *d.ParkedSince, false)
			case !c.Connected && !c.DisconnectedAt.IsZero() && now.Sub(laterOf(c.DisconnectedAt, c.LastRecvAt)) >= PartialGrace:
				s.closeDrive(c, d.LastAt, true)
			case c.Connected && quiet >= SilentDrive:
				s.closeDrive(c, d.LastAt, true)
			}
		}
		if ch := c.Charge; ch != nil && now.Sub(laterOf(ch.LastAt, c.LastRecvAt)) >= SilentCharge {
			end := ch.LastChargingAt
			if ch.StoppedAt != nil {
				end = *ch.StoppedAt
			}
			s.closeCharge(c, end, true)
		}
	}
}

func laterOf(a, b time.Time) time.Time {
	if a.After(b) {
		return a
	}
	return b
}

// MarkPosted moves a key out of the outbox into the recent list.
func (s *State) MarkPosted(key string) {
	out := s.Outbox[:0]
	for _, c := range s.Outbox {
		if c.Key != key {
			out = append(out, c)
		}
	}
	s.Outbox = out
	s.Posted = append(s.Posted, key)
	if len(s.Posted) > 100 {
		s.Posted = s.Posted[len(s.Posted)-100:]
	}
}

/* ———— geometry ———— */

// Haversine distance in metres.
func Haversine(lat1, lon1, lat2, lon2 float64) float64 {
	const r = 6371008.8
	p1, p2 := lat1*math.Pi/180, lat2*math.Pi/180
	dp, dl := (lat2-lat1)*math.Pi/180, (lon2-lon1)*math.Pi/180
	a := math.Sin(dp/2)*math.Sin(dp/2) + math.Cos(p1)*math.Cos(p2)*math.Sin(dl/2)*math.Sin(dl/2)
	return 2 * r * math.Asin(math.Min(1, math.Sqrt(a)))
}

// PathLength is the length of a route in metres.
func PathLength(p []Point) float64 {
	total := 0.0
	for i := 1; i < len(p); i++ {
		total += Haversine(p[i-1][0], p[i-1][1], p[i][0], p[i][1])
	}
	return total
}

// Thin reduces a route to at most max points with Ramer–Douglas–Peucker
// (the tolerance grows until it fits), always keeping both ends.
func Thin(p []Point, max int) []Point {
	if max < 2 {
		max = 2
	}
	if len(p) <= max {
		return append([]Point(nil), p...)
	}
	eps := 1.0 // metres
	for i := 0; i < 40; i++ {
		out := rdp(p, eps)
		if len(out) <= max {
			return out
		}
		eps *= 1.6
	}
	// pathological input: fall back to even sampling
	out := make([]Point, 0, max)
	step := float64(len(p)-1) / float64(max-1)
	for i := 0; i < max; i++ {
		out = append(out, p[int(math.Round(float64(i)*step))])
	}
	return out
}

func rdp(p []Point, eps float64) []Point {
	keep := make([]bool, len(p))
	keep[0], keep[len(p)-1] = true, true
	type span struct{ a, b int }
	stack := []span{{0, len(p) - 1}}
	for len(stack) > 0 {
		sp := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if sp.b-sp.a < 2 {
			continue
		}
		best, idx := 0.0, -1
		for i := sp.a + 1; i < sp.b; i++ {
			if d := segDist(p[i], p[sp.a], p[sp.b]); d > best {
				best, idx = d, i
			}
		}
		if idx >= 0 && best > eps {
			keep[idx] = true
			stack = append(stack, span{sp.a, idx}, span{idx, sp.b})
		}
	}
	out := make([]Point, 0, len(p))
	for i, k := range keep {
		if k {
			out = append(out, p[i])
		}
	}
	return out
}

// segDist: distance (m) from p to segment a–b on a local flat projection.
func segDist(p, a, b Point) float64 {
	kx := math.Cos(a[0]*math.Pi/180) * 111320
	const ky = 110574.0
	px, py := (p[1]-a[1])*kx, (p[0]-a[0])*ky
	bx, by := (b[1]-a[1])*kx, (b[0]-a[0])*ky
	l2 := bx*bx + by*by
	if l2 == 0 {
		return math.Hypot(px, py)
	}
	t := math.Max(0, math.Min(1, (px*bx+py*by)/l2))
	return math.Hypot(px-t*bx, py-t*by)
}
