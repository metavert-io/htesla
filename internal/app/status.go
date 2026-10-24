package app

import (
	"github.com/metavert-io/htesla/internal/cost"
	"github.com/metavert-io/htesla/internal/receiver"
)

// Status is the KV "status" document the setup page renders. The
// sidecar is its only writer.
type Status struct {
	Version   string `json:"version"`
	UpdatedAt string `json:"updated_at"`
	StartedAt string `json:"started_at"`

	Config ConfigStatus `json:"config"`
	// Sealing: master-key | node-key (node-key = the seal key sits on the
	// same disk — no master key provisioned on this node).
	Sealing string `json:"sealing"`

	Keys      KeyStatus            `json:"keys"`
	App       AppStatus            `json:"app"`
	Register  StepStatus           `json:"register"`
	SignIn    SignInStatus         `json:"signin"`
	Vehicles  []VehicleState       `json:"vehicles"`
	VehAt     string               `json:"vehicles_at,omitempty"`
	VehError  string               `json:"vehicles_error,omitempty"`
	Streaming StreamStatus         `json:"streaming"`
	Receiver  ReceiverStatus       `json:"receiver"`
	Cars      map[string]*CarState `json:"cars"`
	Today     TodayUsage           `json:"today"`
	Records   RecordsStatus        `json:"records"`
	Cost      *cost.Projection     `json:"cost,omitempty"`

	// command ids already handled (so a restart doesn't redo them)
	Done []string `json:"done"`
}

// ConfigStatus echoes the configuration plus the derived values the
// page shows for pasting into developer.tesla.com.
type ConfigStatus struct {
	PublicOrigin  string   `json:"public_origin"`
	ClientID      string   `json:"client_id"`
	Region        string   `json:"region"`
	TelemetryPort int      `json:"telemetry_port"`
	Domain        string   `json:"domain"`
	AllowedOrigin string   `json:"allowed_origin"`
	RedirectURI   string   `json:"redirect_uri"`
	PublicKeyURL  string   `json:"public_key_url"`
	FleetBase     string   `json:"fleet_base"`
	Scopes        string   `json:"scopes"`
	Timezone      string   `json:"timezone"` // the zone drives and charges are dated in
	Problems      []string `json:"problems"`
}

// KeyStatus is step 1.
type KeyStatus struct {
	State     string `json:"state"` // missing | ready | lost | error
	CreatedAt string `json:"created_at,omitempty"`
	PublicPEM string `json:"public_pem,omitempty"`
	Served    *bool  `json:"served,omitempty"` // does the public URL answer with OUR key?
	CheckedAt string `json:"checked_at,omitempty"`
	CheckNote string `json:"check_note,omitempty"`
	Error     string `json:"error,omitempty"`
}

// AppStatus is step 2.
type AppStatus struct {
	ClientSecretSet bool   `json:"client_secret_set"`
	ClientSecretAt  string `json:"client_secret_at,omitempty"`
}

// StepStatus is a generic action step (register).
type StepStatus struct {
	State  string `json:"state"` // idle | working | done | error
	At     string `json:"at,omitempty"`
	Error  string `json:"error,omitempty"`
	Domain string `json:"domain,omitempty"`
}

// SignInStatus is step 4.
type SignInStatus struct {
	State         string `json:"state"` // signed_out | waiting | signed_in | sign_in_again | error
	At            string `json:"at,omitempty"`
	Error         string `json:"error,omitempty"`
	WaitingSince  string `json:"waiting_since,omitempty"`
	AccountRegion string `json:"account_region,omitempty"`
	AccountBase   string `json:"account_base,omitempty"`
}

// VehicleState is step 5, per car.
type VehicleState struct {
	VIN      string `json:"vin"`
	Last4    string `json:"last4"`
	Name     string `json:"name"`
	State    string `json:"state,omitempty"` // online | asleep | offline (as Tesla lists it)
	Paired   *bool  `json:"paired,omitempty"`
	Firmware string `json:"firmware,omitempty"`
	PairURL  string `json:"pair_url"`
}

// StreamStatus is step 6.
type StreamStatus struct {
	State         string                `json:"state"` // off | working | configured | paused | error
	At            string                `json:"at,omitempty"`
	Error         string                `json:"error,omitempty"`
	VINs          []string              `json:"vins,omitempty"`
	Updated       int                   `json:"updated,omitempty"`
	Skipped       map[string][]string   `json:"skipped,omitempty"`
	Hostname      string                `json:"hostname,omitempty"`
	Port          int                   `json:"port,omitempty"`
	CAFingerprint string                `json:"ca_fingerprint,omitempty"`
	Fields        []string              `json:"fields,omitempty"`
	PerVIN        map[string]*VINStream `json:"per_vin,omitempty"`
	CheckedAt     string                `json:"checked_at,omitempty"`
	Stale         string                `json:"stale,omitempty"` // why the sent config no longer matches
	PausedAt      string                `json:"paused_at,omitempty"`
}

// VINStream is Tesla's view of one car's streaming config.
type VINStream struct {
	Synced       bool     `json:"synced"`
	LimitReached bool     `json:"limit_reached"`
	KeyPaired    *bool    `json:"key_paired,omitempty"`
	Errors       []string `json:"errors,omitempty"`
	Error        string   `json:"error,omitempty"`
}

// ReceiverStatus is the mTLS server.
type ReceiverStatus struct {
	Listening     bool           `json:"listening"`
	Addr          string         `json:"addr,omitempty"`
	Error         string         `json:"error,omitempty"`
	CertHost      string         `json:"cert_host,omitempty"`
	CertExpires   string         `json:"cert_expires,omitempty"`
	CertSource    string         `json:"cert_source,omitempty"`
	CAFingerprint string         `json:"ca_fingerprint,omitempty"`
	Stats         receiver.Stats `json:"stats"`
}

// CarState is connectivity per car (from the stream itself).
type CarState struct {
	Online         bool   `json:"online"`
	LastSeen       string `json:"last_seen,omitempty"`
	ConnectedAt    string `json:"connected_at,omitempty"`
	DisconnectedAt string `json:"disconnected_at,omitempty"`
	Network        string `json:"network,omitempty"`
	ClientVersion  string `json:"client_version,omitempty"`
}

// TodayUsage is the running signal count for today (UTC).
type TodayUsage struct {
	Day        string  `json:"day"`
	Signals    int64   `json:"signals"`
	EstCostUSD float64 `json:"est_cost_usd"`
}

// RecordsStatus is the record builder (drives, charges, updates).
type RecordsStatus struct {
	Cursor     string   `json:"cursor,omitempty"` // last telemetry chunk read
	OpenDrives int      `json:"open_drives"`
	OpenCharge int      `json:"open_charges"`
	Outbox     int      `json:"outbox"`
	Posted     int      `json:"posted"`
	Dropped    int      `json:"dropped,omitempty"`
	LastPosted string   `json:"last_posted,omitempty"` // key
	LastAt     string   `json:"last_at,omitempty"`
	Error      string   `json:"error,omitempty"`
	Failed     []string `json:"failed,omitempty"` // keys the daemon refused (kept out of the outbox)
}
