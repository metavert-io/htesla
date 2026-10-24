package builder

import (
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/metavert-io/htesla/internal/hearth"
)

// CarInfo is how a record names its car.
type CarInfo struct {
	Name  string // what the owner calls it ("Ember"), else the model
	Model string // "Model Y"
}

// Info describes a car from the builder's memory.
func (s *State) Info(vin string) CarInfo {
	c := s.Cars[vin]
	if c == nil {
		return CarInfo{Name: "Tesla", Model: ModelFromVIN(vin)}
	}
	return CarInfo{Name: c.Name(), Model: c.Model()}
}

// Offset is tz's UTC offset in minutes at t.
func Offset(tz *time.Location, t time.Time) int {
	if tz == nil {
		tz = time.UTC
	}
	_, secs := t.In(tz).Zone()
	return secs / 60
}

func rfc(t time.Time) string { return t.UTC().Format(time.RFC3339) }

func ptr[T any](v T) *T { return &v }

func round(v float64, places int) float64 {
	p := math.Pow(10, float64(places))
	return math.Round(v*p) / p
}

// RoutePath is where a drive's route is stored.
func RoutePath(c Closed) string {
	return fmt.Sprintf("drives/%s-%d.json", c.VID, c.Drive.Start.Unix())
}

// RouteDoc is the stored route: [lat, lon, unix seconds] points.
func RouteDoc(c Closed) map[string]any {
	pts := make([][3]float64, 0, len(c.Drive.Route))
	for _, p := range c.Drive.Route {
		pts = append(pts, [3]float64{round(p[0], 6), round(p[1], 6), p[2]})
	}
	return map[string]any{
		"key": c.Key, "vehicle": c.VID, "start": rfc(c.Drive.Start), "end": rfc(c.Drive.End),
		"distance_m": round(c.Drive.DistanceM, 1), "points": pts,
	}
}

// MovementFor renders a closed drive for POST /v1/ingest/movement.
func MovementFor(c Closed, tz *time.Location) hearth.Movement {
	d := c.Drive
	m := hearth.Movement{
		Key: c.Key, Mode: "drive", Start: rfc(d.Start), End: rfc(d.End),
		TZOffsetMinutes: Offset(tz, d.Start),
		StartLat:        d.StartLat, StartLon: d.StartLon, EndLat: d.EndLat, EndLon: d.EndLon,
		DistanceM: ptr(round(d.DistanceM, 1)),
	}
	if d.StartPlace != "" {
		m.StartPlace = ptr(d.StartPlace)
	}
	if d.EndPlace != "" {
		m.EndPlace = ptr(d.EndPlace)
	}
	attrs := map[string]any{
		"guest_mode":      d.Guest,
		"route_file":      RoutePath(c),
		"distance_source": d.DistanceSrc,
	}
	if d.EnergyKWh != nil {
		attrs["energy_kwh"] = round(*d.EnergyKWh, 2)
		if d.DistanceM > 0 {
			attrs["wh_per_km"] = round(*d.EnergyKWh*1000/(d.DistanceM/1000), 0)
		}
	}
	if d.OutsideC != nil {
		attrs["outside_temp_c"] = round(*d.OutsideC, 1)
	}
	if len(d.Media) > 0 {
		media := make([]map[string]string, 0, len(d.Media))
		for _, x := range d.Media {
			e := map[string]string{"title": x.Title}
			if x.Artist != "" {
				e["artist"] = x.Artist
			}
			media = append(media, e)
		}
		attrs["media"] = media
	}
	if d.OdoStartKm != nil && d.OdoEndKm != nil {
		attrs["odometer_start_km"] = round(*d.OdoStartKm, 1)
		attrs["odometer_end_km"] = round(*d.OdoEndKm, 1)
	}
	if d.Partial {
		attrs["partial"] = true
	}
	m.Attrs = attrs
	return m
}

func localTime(tz *time.Location, t time.Time) string {
	if tz == nil {
		tz = time.UTC
	}
	return t.In(tz).Format("Mon 2 Jan 2006, 15:04 MST")
}

func pct(v *float64) string {
	if v == nil {
		return ""
	}
	return fmt.Sprintf("%.0f%%", *v)
}

var chargerNames = map[string]string{
	"Supercharger": "Tesla Supercharger", "Combo": "CCS fast charger", "CHAdeMO": "CHAdeMO fast charger",
	"GB": "GB/T fast charger",
}

// ChargeEpisode renders a closed charging session as a `charge` episode.
func ChargeEpisode(c Closed, info CarInfo, tz *time.Location) hearth.Episode {
	ch := c.Charge
	soc := ""
	if ch.SocStart != nil && ch.SocEnd != nil {
		soc = fmt.Sprintf(" (%s → %s)", pct(ch.SocStart), pct(ch.SocEnd))
	}
	energy := fmt.Sprintf("%.1f kWh", ch.EnergyIn)
	var title string
	switch {
	case ch.Fast && ch.FastType == "Supercharger":
		title = "Supercharged — " + energy + soc
	case ch.Fast:
		title = "Fast charged — " + energy + soc
	case ch.Place != "":
		title = "Charged at " + ch.Place + " — " + energy + soc
	default:
		title = "Charged — " + energy + soc
	}
	var b strings.Builder
	car := info.Name
	if info.Model != "" && info.Model != info.Name {
		car = fmt.Sprintf("%s (%s)", info.Name, info.Model)
	}
	fmt.Fprintf(&b, "%s charged", car)
	if ch.Place != "" {
		fmt.Fprintf(&b, " at %s", ch.Place)
	}
	b.WriteString(".\n\n")
	fmt.Fprintf(&b, "- Started: %s\n", localTime(tz, ch.Start))
	fmt.Fprintf(&b, "- Ended: %s\n", localTime(tz, ch.End))
	fmt.Fprintf(&b, "- Energy added: %.1f kWh", ch.EnergyIn)
	if ch.ACKWh > 0 && ch.DCKWh > 0 {
		fmt.Fprintf(&b, " (AC %.1f, DC %.1f)", ch.ACKWh, ch.DCKWh)
	}
	b.WriteString("\n")
	if ch.SocStart != nil || ch.SocEnd != nil {
		fmt.Fprintf(&b, "- Battery: %s → %s\n", orDash(pct(ch.SocStart)), orDash(pct(ch.SocEnd)))
	}
	if ch.Fast {
		name := chargerNames[ch.FastType]
		if name == "" {
			name = "DC fast charger"
		}
		fmt.Fprintf(&b, "- Charger: %s (fast charging: yes)\n", name)
	} else {
		b.WriteString("- Charger: AC (fast charging: no)\n")
	}
	if ch.Place != "" {
		fmt.Fprintf(&b, "- Place: %s\n", ch.Place)
	}
	if ch.Partial {
		b.WriteString("\nThe end of this session wasn't seen (the car went quiet); the figures run to its last report.\n")
	}
	b.WriteString("\nRecorded from the car's own data stream by the htesla ember.\n")
	return hearth.Episode{
		Source: "htesla", SourceRef: c.Key, Kind: "charge", Title: title, Body: b.String(),
		EventTime: hearth.EventTime{UTC: rfc(ch.Start), TZOffsetMinutes: Offset(tz, ch.Start), Uncertainty: "minute"},
		Trust:     "self",
	}
}

func orDash(s string) string {
	if s == "" {
		return "—"
	}
	return s
}

// ReleaseNote is one entry from Tesla's release_notes endpoint.
type ReleaseNote struct {
	Title       string `json:"title"`
	Subtitle    string `json:"subtitle"`
	Description string `json:"description"`
}

// UpdateEpisode renders a software update as a `software_update` episode.
func UpdateEpisode(c Closed, info CarInfo, notes []ReleaseNote, tz *time.Location) hearth.Episode {
	u := c.Update
	who := info.Model
	if who == "" {
		who = info.Name
	}
	title := fmt.Sprintf("%s updated to %s", who, u.To)
	var b strings.Builder
	car := info.Name
	if info.Model != "" && info.Model != info.Name {
		car = fmt.Sprintf("%s (%s)", info.Name, info.Model)
	}
	fmt.Fprintf(&b, "%s installed software %s (it was on %s). First seen running the new version %s.\n",
		car, u.To, u.From, localTime(tz, u.At))
	if len(notes) > 0 {
		b.WriteString("\n## What's new\n")
		for _, n := range notes {
			if strings.TrimSpace(n.Title) == "" && strings.TrimSpace(n.Description) == "" {
				continue
			}
			fmt.Fprintf(&b, "\n### %s\n", strings.TrimSpace(n.Title))
			if s := strings.TrimSpace(n.Subtitle); s != "" {
				fmt.Fprintf(&b, "*%s*\n\n", s)
			}
			if d := strings.TrimSpace(n.Description); d != "" {
				b.WriteString(d + "\n")
			}
		}
	} else {
		b.WriteString("\nTesla's release notes weren't available when this was recorded.\n")
	}
	return hearth.Episode{
		Source: "htesla", SourceRef: c.Key, Kind: "software_update", Title: title, Body: b.String(),
		EventTime: hearth.EventTime{UTC: rfc(u.At), TZOffsetMinutes: Offset(tz, u.At), Uncertainty: "minute"},
		Trust:     "self",
	}
}
