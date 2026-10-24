package tesla

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

// APIError is a non-2xx answer from the Fleet API.
type APIError struct {
	Status int
	Body   string
}

func (e *APIError) Error() string {
	b := e.Body
	if len(b) > 300 {
		b = b[:300] + "…"
	}
	return fmt.Sprintf("tesla fleet api %d: %s", e.Status, strings.TrimSpace(b))
}

// IsUnauthorized reports a 401 from the Fleet API.
func IsUnauthorized(err error) bool {
	var ae *APIError
	return errors.As(err, &ae) && ae.Status == http.StatusUnauthorized
}

// TokenSource hands out a bearer token for each call.
type TokenSource func(ctx context.Context) (string, error)

// Fleet is a client for the free, read-only Fleet API endpoints.
type Fleet struct {
	HTTP  *http.Client
	Base  string // regional base URL
	Token TokenSource
}

func (f *Fleet) call(ctx context.Context, method, path string, body any, out any) error {
	tok, err := f.Token(ctx)
	if err != nil {
		return err
	}
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(f.Base, "/")+path, rdr)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := f.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if resp.StatusCode/100 != 2 {
		return &APIError{Status: resp.StatusCode, Body: string(data)}
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(data, out)
}

// Vehicle is one entry of GET /api/1/vehicles.
type Vehicle struct {
	ID          json.Number `json:"id"`
	VehicleID   json.Number `json:"vehicle_id"`
	VIN         string      `json:"vin"`
	DisplayName string      `json:"display_name"`
	State       string      `json:"state"`
}

// Vehicles lists the owner's cars (free; never wakes them).
func (f *Fleet) Vehicles(ctx context.Context) ([]Vehicle, error) {
	var out struct {
		Response []Vehicle `json:"response"`
	}
	err := f.call(ctx, http.MethodGet, "/api/1/vehicles", nil, &out)
	return out.Response, err
}

// Region answers which regional base URL this account lives on.
type Region struct {
	Region          string `json:"region"`
	FleetAPIBaseURL string `json:"fleet_api_base_url"`
}

// UserRegion calls GET /api/1/users/region.
func (f *Fleet) UserRegion(ctx context.Context) (*Region, error) {
	var out struct {
		Response Region `json:"response"`
	}
	if err := f.call(ctx, http.MethodGet, "/api/1/users/region", nil, &out); err != nil {
		return nil, err
	}
	return &out.Response, nil
}

// FleetStatus is the answer of POST /api/1/vehicles/fleet_status.
type FleetStatus struct {
	KeyPairedVINs []string                   `json:"key_paired_vins"`
	UnpairedVINs  []string                   `json:"unpaired_vins"`
	VehicleInfo   map[string]json.RawMessage `json:"vehicle_info"`
}

// FleetStatus checks which cars have this app's virtual key paired.
func (f *Fleet) FleetStatus(ctx context.Context, vins []string) (*FleetStatus, error) {
	var out struct {
		Response FleetStatus `json:"response"`
	}
	if err := f.call(ctx, http.MethodPost, "/api/1/vehicles/fleet_status", map[string]any{"vins": vins}, &out); err != nil {
		return nil, err
	}
	return &out.Response, nil
}

// RegisterPartner registers the app's domain (partner token required).
// Tesla fetches the public key from the domain's .well-known address
// while handling this call.
func (f *Fleet) RegisterPartner(ctx context.Context, domain string) (json.RawMessage, error) {
	var out json.RawMessage
	err := f.call(ctx, http.MethodPost, "/api/1/partner_accounts", map[string]any{"domain": domain}, &out)
	return out, err
}

// TelemetryConfigState is GET /api/1/vehicles/{vin}/fleet_telemetry_config.
type TelemetryConfigState struct {
	Synced       bool            `json:"synced"`
	LimitReached bool            `json:"limit_reached"`
	KeyPaired    *bool           `json:"key_paired,omitempty"`
	Config       json.RawMessage `json:"config"`
}

// TelemetryConfig reads a car's current telemetry configuration.
func (f *Fleet) TelemetryConfig(ctx context.Context, vin string) (*TelemetryConfigState, error) {
	var out struct {
		Response TelemetryConfigState `json:"response"`
	}
	if err := f.call(ctx, http.MethodGet, "/api/1/vehicles/"+url.PathEscape(vin)+"/fleet_telemetry_config", nil, &out); err != nil {
		return nil, err
	}
	return &out.Response, nil
}

// TelemetryError is one entry of fleet_telemetry_errors.
type TelemetryError struct {
	Name      string `json:"name"`
	Error     string `json:"error"`
	VIN       string `json:"vin"`
	CreatedAt string `json:"created_at,omitempty"`
}

// TelemetryErrors lists recent errors a car reported while streaming.
func (f *Fleet) TelemetryErrors(ctx context.Context, vin string) ([]TelemetryError, error) {
	var out struct {
		Response struct {
			Errors []TelemetryError `json:"fleet_telemetry_errors"`
		} `json:"response"`
	}
	if err := f.call(ctx, http.MethodGet, "/api/1/vehicles/"+url.PathEscape(vin)+"/fleet_telemetry_errors", nil, &out); err != nil {
		return nil, err
	}
	return out.Response.Errors, nil
}

// ConfigResult is the answer of fleet_telemetry_config_jws.
type ConfigResult struct {
	UpdatedVehicles int                 `json:"updated_vehicles"`
	SkippedVehicles map[string][]string `json:"skipped_vehicles"`
}

// PostSignedTelemetryConfig sends a signed configuration (see
// SignTelemetryConfig) — what Tesla's tesla-http-proxy does for
// POST fleet_telemetry_config, done here in-process.
func (f *Fleet) PostSignedTelemetryConfig(ctx context.Context, vins []string, token string) (*ConfigResult, error) {
	var out struct {
		Response ConfigResult `json:"response"`
	}
	body := map[string]any{"vins": vins, "token": token}
	if err := f.call(ctx, http.MethodPost, "/api/1/vehicles/fleet_telemetry_config_jws", body, &out); err != nil {
		return nil, err
	}
	return &out.Response, nil
}

// DeleteTelemetryConfig removes the streaming configuration from a car
// (free; "pause streaming").
func (f *Fleet) DeleteTelemetryConfig(ctx context.Context, vin string) error {
	return f.call(ctx, http.MethodDelete, "/api/1/vehicles/"+url.PathEscape(vin)+"/fleet_telemetry_config", nil, nil)
}

// ReleaseNote is one entry of release_notes.
type ReleaseNote struct {
	Title           string `json:"title"`
	Subtitle        string `json:"subtitle"`
	Description     string `json:"description"`
	CustomerVersion string `json:"customer_version"`
}

// ReleaseNotes reads the firmware release notes Tesla holds for a car
// (free; served by Tesla, the car isn't asked).
func (f *Fleet) ReleaseNotes(ctx context.Context, vin string) ([]ReleaseNote, error) {
	var out struct {
		Response struct {
			ReleaseNotes []ReleaseNote `json:"release_notes"`
		} `json:"response"`
	}
	if err := f.call(ctx, http.MethodGet, "/api/1/vehicles/"+url.PathEscape(vin)+"/release_notes", nil, &out); err != nil {
		return nil, err
	}
	return out.Response.ReleaseNotes, nil
}
