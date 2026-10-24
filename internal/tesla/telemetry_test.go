package tesla

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/golang-jwt/jwt/v5"
	"github.com/metavert-io/htesla/internal/keys"
)

const testCA = "-----BEGIN CERTIFICATE-----\nMIIB\n-----END CERTIFICATE-----\n"

func TestTelemetryConfigShape(t *testing.T) {
	cfg := NewTelemetryConfig("yourname.hearthlaunch.com", 8443, testCA)
	b, err := json.Marshal(cfg.Claims())
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	_ = json.Unmarshal(b, &m)
	for _, k := range []string{"hostname", "port", "ca", "fields", "alert_types", "delivery_policy"} {
		if _, ok := m[k]; !ok {
			t.Errorf("missing %q", k)
		}
	}
	if _, ok := m["exp"]; ok {
		t.Error("exp must be omitted")
	}
	if m["hostname"] != "yourname.hearthlaunch.com" || m["port"].(float64) != 8443 || m["ca"] != testCA {
		t.Errorf("wrong basics: %v", m)
	}
	if m["delivery_policy"] != "latest" {
		t.Errorf("delivery_policy %v", m["delivery_policy"])
	}
	fields := m["fields"].(map[string]any)
	want := []string{"Gear", "VehicleSpeed", "Odometer", "Location", "GpsHeading", "BatteryLevel", "EnergyRemaining",
		"RatedRange", "ChargeState", "DetailedChargeState", "ACChargingEnergyIn", "DCChargingEnergyIn",
		"FastChargerPresent", "Version", "Locked", "SentryMode", "OutsideTemp", "MediaNowPlayingTitle",
		"MediaNowPlayingArtist", "VehicleName", "GuestModeEnabled", "LocatedAtHome",
		"LocatedAtWork", "FastChargerType", "CarType"}
	if len(fields) != len(want) {
		t.Errorf("%d fields, want %d", len(fields), len(want))
	}
	for _, f := range want {
		fv, ok := fields[f].(map[string]any)
		if !ok {
			t.Errorf("field %s missing", f)
			continue
		}
		if iv, _ := fv["interval_seconds"].(float64); iv < 1 {
			t.Errorf("%s interval %v", f, fv["interval_seconds"])
		}
		_, hasDelta := fv["minimum_delta"]
		if hasDelta != (f == "Location") {
			t.Errorf("%s minimum_delta present=%v (only Location supports it)", f, hasDelta)
		}
	}
	if loc := fields["Location"].(map[string]any); loc["minimum_delta"].(float64) <= 0 {
		t.Error("Location should carry a minimum_delta")
	}
}

func TestSignedConfigVerifies(t *testing.T) {
	priv, _, err := keys.Generate(nil)
	if err != nil {
		t.Fatal(err)
	}
	ck, err := keys.CommandKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	cfg := NewTelemetryConfig("yourname.hearthlaunch.com", 8443, testCA)
	token, err := SignTelemetryConfig(ck, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(token, ".") != 2 {
		t.Fatalf("not a compact JWS: %q", token)
	}
	// header: Tesla's Schnorr algorithm
	hdr, _ := base64.RawURLEncoding.DecodeString(strings.Split(token, ".")[0])
	if !strings.Contains(string(hdr), `"alg":"Tesla.SS256"`) {
		t.Fatalf("header %s", hdr)
	}
	// verify with vehicle-command's own verifier (registered with jwt
	// under "Tesla.SS256"; the key is the uncompressed public point)
	parsed, err := jwt.Parse(token, func(*jwt.Token) (any, error) { return ck.PublicBytes(), nil },
		jwt.WithValidMethods([]string{"Tesla.SS256"}))
	if err != nil || !parsed.Valid {
		t.Fatalf("signature didn't verify: %v", err)
	}
	claims := parsed.Claims.(jwt.MapClaims)
	if claims["aud"] != "com.tesla.fleet.TelemetryClient" {
		t.Errorf("aud %v", claims["aud"])
	}
	if claims["iss"] != base64.StdEncoding.EncodeToString(ck.PublicBytes()) {
		t.Errorf("iss %v", claims["iss"])
	}
	if claims["hostname"] != cfg.Hostname || claims["delivery_policy"] != "latest" {
		t.Errorf("claims %v", claims)
	}
	// a different key must not verify it
	other, _, _ := keys.Generate(nil)
	ok2, _ := keys.CommandKey(other)
	if _, err := jwt.Parse(token, func(*jwt.Token) (any, error) { return ok2.PublicBytes(), nil },
		jwt.WithValidMethods([]string{"Tesla.SS256"})); err == nil {
		t.Fatal("verified with the wrong key")
	}
	// tampering breaks it
	parts := strings.Split(token, ".")
	body, _ := base64.RawURLEncoding.DecodeString(parts[1])
	evil := strings.Replace(string(body), "yourname", "evilbrain", 1)
	tampered := parts[0] + "." + base64.RawURLEncoding.EncodeToString([]byte(evil)) + "." + parts[2]
	if _, err := jwt.Parse(tampered, func(*jwt.Token) (any, error) { return ck.PublicBytes(), nil },
		jwt.WithValidMethods([]string{"Tesla.SS256"})); err == nil {
		t.Fatal("tampered claims verified")
	}
}

func TestPostSignedConfigShape(t *testing.T) {
	var path, auth string
	var body map[string]any
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path, auth = r.URL.Path, r.Header.Get("Authorization")
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &body)
		_, _ = w.Write([]byte(`{"response":{"updated_vehicles":1,"skipped_vehicles":{"missing_key":[],"unsupported_hardware":[],"unsupported_firmware":[],"max_configs":[]}}}`))
	}))
	defer api.Close()
	f := &Fleet{HTTP: http.DefaultClient, Base: api.URL, Token: func(context.Context) (string, error) { return "user-token", nil }}
	res, err := f.PostSignedTelemetryConfig(context.Background(), []string{"5YJ3E1EA0KF000001"}, "a.b.c")
	if err != nil || res.UpdatedVehicles != 1 {
		t.Fatal(err)
	}
	if path != "/api/1/vehicles/fleet_telemetry_config_jws" || auth != "Bearer user-token" {
		t.Fatalf("%s %s", path, auth)
	}
	if body["token"] != "a.b.c" || len(body["vins"].([]any)) != 1 || len(body) != 2 {
		t.Fatalf("body %v", body)
	}
}
