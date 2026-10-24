package tesla

import (
	"errors"
	"sort"

	"github.com/golang-jwt/jwt/v5"
	"github.com/teslamotors/vehicle-command/pkg/protocol"
	"github.com/teslamotors/vehicle-command/pkg/sign"
)

// FieldConfig is one streamed signal's cadence. A value is sent only
// when it CHANGED and interval_seconds has passed since its last send;
// minimum_delta (location fields only, metres) suppresses GPS jitter.
type FieldConfig struct {
	IntervalSeconds int `json:"interval_seconds"`
	MinimumDelta    int `json:"minimum_delta,omitempty"`
}

// StarterFields is the lean default set (configurable later).
//
// Cost: Tesla bills streamed signals at 150,000 per $1, with a $10
// monthly credit per developer account. Every field below is sent only
// on change, so a PARKED car costs next to nothing. Worst case for an
// hour of DRIVING (every value changing as fast as its interval allows):
//
//	Location 10s (≥25 m moved) ≤360 · VehicleSpeed 10s ≤360 · GpsHeading 30s ≤120
//	Odometer, BatteryLevel, EnergyRemaining 60s ≤60 each (180) · RatedRange 120s ≤30
//	media title+artist 30s ≤240 (typically ~30: one pair per song)
//	OutsideTemp 300s ≤12 · Gear/Locked/Sentry/charge states/LocatedAt*/CarType: a handful per drive
//	≈ 1,100–1,300 signals/hour ≈ $0.008/hour of driving
//
// An hour of AC charging adds ACChargingEnergyIn/BatteryLevel/
// EnergyRemaining at 60s (≤180/hour ≈ $0.0012). Two hours of driving a
// day is roughly 75,000 signals ≈ $0.50 a month — well inside the
// $10 credit. Alerts (alert_types "service") are billed as signals too
// but are rare.
var StarterFields = map[string]FieldConfig{
	// motion — fast while driving, silent while parked (sent on change)
	"Gear":         {IntervalSeconds: 1},
	"VehicleSpeed": {IntervalSeconds: 10},
	"Location":     {IntervalSeconds: 10, MinimumDelta: 25},
	"GpsHeading":   {IntervalSeconds: 30},
	"Odometer":     {IntervalSeconds: 60},
	// energy
	"BatteryLevel":    {IntervalSeconds: 60},
	"EnergyRemaining": {IntervalSeconds: 60},
	"RatedRange":      {IntervalSeconds: 120},
	// charging — the states change rarely, so 1s costs little and
	// catches plug-in/out precisely
	"ChargeState":         {IntervalSeconds: 1},
	"DetailedChargeState": {IntervalSeconds: 1},
	"ACChargingEnergyIn":  {IntervalSeconds: 60},
	"DCChargingEnergyIn":  {IntervalSeconds: 60},
	"FastChargerPresent":  {IntervalSeconds: 1},
	// car + cabin
	"Version":               {IntervalSeconds: 3600},
	"Locked":                {IntervalSeconds: 5},
	"SentryMode":            {IntervalSeconds: 5},
	"OutsideTemp":           {IntervalSeconds: 300},
	"MediaNowPlayingTitle":  {IntervalSeconds: 30},
	"MediaNowPlayingArtist": {IntervalSeconds: 30},
	"VehicleName":           {IntervalSeconds: 3600},
	"GuestModeEnabled":      {IntervalSeconds: 60},
	"LocatedAtHome":         {IntervalSeconds: 60},
	"LocatedAtWork":         {IntervalSeconds: 60},
	"FastChargerType":       {IntervalSeconds: 1}, // which fast charger (Supercharger, CCS…)
	"CarType":               {IntervalSeconds: 3600},
}

// FieldNames lists the starter fields, sorted.
func FieldNames() []string {
	out := make([]string, 0, len(StarterFields))
	for k := range StarterFields {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// TelemetryConfig is the body of a fleet_telemetry_config — the claims
// the signed JWS carries.
type TelemetryConfig struct {
	Hostname       string                 `json:"hostname"`
	Port           int                    `json:"port"`
	CA             string                 `json:"ca"`
	Fields         map[string]FieldConfig `json:"fields"`
	AlertTypes     []string               `json:"alert_types,omitempty"`
	DeliveryPolicy string                 `json:"delivery_policy,omitempty"`
	// exp deliberately omitted: the configuration lasts until replaced
}

// NewTelemetryConfig builds the configuration for our receiver.
func NewTelemetryConfig(hostname string, port int, caPEM string) TelemetryConfig {
	fields := make(map[string]FieldConfig, len(StarterFields))
	for k, v := range StarterFields {
		fields[k] = v
	}
	return TelemetryConfig{
		Hostname:   hostname,
		Port:       port,
		CA:         caPEM,
		Fields:     fields,
		AlertTypes: []string{"service"},
		// "latest": the car resends anything we haven't acked (needs a
		// server with fleet-telemetry ≥0.7.1 ack semantics — the
		// receiver acks every record it has taken).
		DeliveryPolicy: "latest",
	}
}

// Claims renders the configuration as JWT claims.
func (c TelemetryConfig) Claims() jwt.MapClaims {
	fields := make(map[string]any, len(c.Fields))
	for k, v := range c.Fields {
		f := map[string]any{"interval_seconds": v.IntervalSeconds}
		if v.MinimumDelta > 0 {
			f["minimum_delta"] = v.MinimumDelta
		}
		fields[k] = f
	}
	claims := jwt.MapClaims{
		"hostname": c.Hostname,
		"port":     c.Port,
		"ca":       c.CA,
		"fields":   fields,
	}
	if len(c.AlertTypes) > 0 {
		claims["alert_types"] = c.AlertTypes
	}
	if c.DeliveryPolicy != "" {
		claims["delivery_policy"] = c.DeliveryPolicy
	}
	return claims
}

// TelemetryApp is the application name tesla-http-proxy signs fleet
// telemetry configurations for; the JWS audience becomes
// "com.tesla.fleet.TelemetryClient".
const TelemetryApp = "TelemetryClient"

// SignTelemetryConfig signs the configuration exactly the way Tesla's
// tesla-http-proxy does for POST fleet_telemetry_config: vehicle-
// command's pkg/sign.SignMessageForFleet (a JWS, alg "Tesla.SS256" —
// Schnorr over NIST P-256 with SHA-256, keyed by the app's private key;
// iss = the base64 public key, aud = com.tesla.fleet.TelemetryClient).
func SignTelemetryConfig(key protocol.ECDHPrivateKey, cfg TelemetryConfig) (string, error) {
	if key == nil {
		return "", errors.New("no signing key")
	}
	if cfg.Hostname == "" || cfg.Port <= 0 || cfg.CA == "" || len(cfg.Fields) == 0 {
		return "", errors.New("telemetry config needs hostname, port, ca and fields")
	}
	return sign.SignMessageForFleet(key, TelemetryApp, cfg.Claims())
}
