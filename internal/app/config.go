package app

import (
	"encoding/json"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/metavert-io/htesla/internal/tesla"
)

// Config is what the owner set on the ember's Integrations card (the
// daemon writes it into KV "config"); absent values take the defaults
// from the manifest.
type Config struct {
	PublicOrigin  string `json:"public_origin"`
	ClientID      string `json:"client_id"`
	Region        string `json:"region"`
	TelemetryPort int    `json:"telemetry_port"`
	Timezone      string `json:"timezone"` // IANA name; "" = this node's zone
}

// Defaults mirror hearthember.json's config[] defaults.
const (
	DefaultRegion        = "na"
	DefaultTelemetryPort = 8443
)

// ParseConfig reads the KV "config" JSON (values may arrive as numbers
// or strings — the dashboard form is forgiving, so are we).
func ParseConfig(raw string) Config {
	c := Config{Region: DefaultRegion, TelemetryPort: DefaultTelemetryPort}
	if strings.TrimSpace(raw) == "" {
		return c
	}
	var m map[string]any
	if json.Unmarshal([]byte(raw), &m) != nil {
		return c
	}
	str := func(k string) string {
		switch v := m[k].(type) {
		case string:
			return strings.TrimSpace(v)
		case float64:
			return strconv.FormatFloat(v, 'f', -1, 64)
		}
		return ""
	}
	c.PublicOrigin = normalizeOrigin(str("public_origin"))
	c.ClientID = str("client_id")
	if r := strings.ToLower(str("region")); r != "" {
		c.Region = r
	}
	c.Timezone = str("timezone")
	if p := str("telemetry_port"); p != "" {
		if n, err := strconv.Atoi(p); err == nil {
			c.TelemetryPort = n
		}
	}
	return c
}

// normalizeOrigin lowercases the scheme and host and drops a trailing
// slash, so the redirect address the page sends to Tesla and the one
// the sidecar exchanges the code with are byte-identical.
func normalizeOrigin(o string) string {
	o = strings.TrimRight(strings.TrimSpace(o), "/")
	u, err := url.Parse(o)
	if err != nil || u.Host == "" {
		return o
	}
	u.Scheme = strings.ToLower(u.Scheme)
	u.Host = strings.ToLower(u.Host)
	return u.String()
}

// Location is the zone drives and charges are dated in: the configured
// IANA zone, else this node's own.
func (c Config) Location() *time.Location {
	if c.Timezone != "" {
		if loc, err := time.LoadLocation(c.Timezone); err == nil {
			return loc
		}
	}
	return time.Local
}

// Domain is the lowercase host of public_origin — what Tesla registers
// and what the telemetry config's hostname must be.
func (c Config) Domain() string {
	u, err := url.Parse(c.PublicOrigin)
	if err != nil {
		return ""
	}
	return strings.ToLower(u.Hostname())
}

// RedirectURI is the sign-in return address.
func (c Config) RedirectURI() string {
	if c.PublicOrigin == "" {
		return ""
	}
	return tesla.RedirectURI(c.PublicOrigin)
}

// FleetBase is the regional Fleet API base URL.
func (c Config) FleetBase() string { return tesla.BaseURL(c.Region) }

// PublicKeyURL is where Tesla fetches the app's public key.
func (c Config) PublicKeyURL() string {
	if c.Domain() == "" {
		return ""
	}
	return "https://" + c.Domain() + PublicKeyPath
}

// PublicKeyPath is the address the manifest publishes.
const PublicKeyPath = "/.well-known/appspecific/com.tesla.3p.public-key.pem"

// Problems lists what's wrong with the configuration, in plain words.
func (c Config) Problems() []string {
	var out []string
	if c.PublicOrigin == "" {
		out = append(out, "Set this node's public address (public_origin) on the ember's Integrations card.")
	} else {
		u, err := url.Parse(c.PublicOrigin)
		switch {
		case err != nil || u.Host == "":
			out = append(out, fmt.Sprintf("public_origin %q isn't a web address like https://yourname.hearthlaunch.com.", c.PublicOrigin))
		case u.Scheme != "https":
			out = append(out, "public_origin must start with https:// — Tesla only talks to secure addresses.")
		case u.Port() != "" && u.Port() != "443":
			out = append(out, "public_origin can't carry a port: Tesla looks for the key on the standard https port.")
		case u.Path != "" || u.RawQuery != "":
			out = append(out, "public_origin is just the address — no path after the host.")
		}
	}
	if c.ClientID == "" {
		out = append(out, "Paste your Tesla app's Client ID (client_id) on the Integrations card.")
	}
	if _, ok := tesla.Regions[c.Region]; !ok {
		out = append(out, fmt.Sprintf("region %q isn't one Tesla offers here — use na (North America / Asia-Pacific) or eu (Europe / Middle East / Africa).", c.Region))
	}
	if c.Timezone != "" {
		if _, err := time.LoadLocation(c.Timezone); err != nil {
			out = append(out, fmt.Sprintf("timezone %q isn't a zone name like America/New_York.", c.Timezone))
		}
	}
	if c.TelemetryPort < 1 || c.TelemetryPort > 65535 {
		out = append(out, "telemetry_port must be a port number (1–65535).")
	}
	return out
}
