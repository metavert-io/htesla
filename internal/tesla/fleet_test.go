package tesla

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
)

func TestPartnerRegistrationShape(t *testing.T) {
	var tokenForm url.Values
	tokSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		tokenForm = r.PostForm
		_, _ = w.Write([]byte(`{"access_token":"partner-token","expires_in":28800,"token_type":"Bearer"}`))
	}))
	defer tokSrv.Close()

	var gotMethod, gotPath, gotAuth, gotCT string
	var gotBody map[string]any
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath = r.Method, r.URL.Path
		gotAuth, gotCT = r.Header.Get("Authorization"), r.Header.Get("Content-Type")
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &gotBody)
		_, _ = w.Write([]byte(`{"response":{"domain":"yourname.hearthlaunch.com"}}`))
	}))
	defer api.Close()

	tok, err := PartnerToken(context.Background(), http.DefaultClient, tokSrv.URL, "cid", "secret", api.URL)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"grant_type": "client_credentials", "client_id": "cid", "client_secret": "secret",
		"scope": "openid vehicle_device_data vehicle_location", "audience": api.URL,
	}
	for k, v := range want {
		if tokenForm.Get(k) != v {
			t.Errorf("token %s = %q, want %q", k, tokenForm.Get(k), v)
		}
	}
	f := &Fleet{HTTP: http.DefaultClient, Base: api.URL, Token: func(context.Context) (string, error) { return tok, nil }}
	if _, err := f.RegisterPartner(context.Background(), "yourname.hearthlaunch.com"); err != nil {
		t.Fatal(err)
	}
	if gotMethod != http.MethodPost || gotPath != "/api/1/partner_accounts" {
		t.Fatalf("%s %s", gotMethod, gotPath)
	}
	if gotAuth != "Bearer partner-token" || gotCT != "application/json" {
		t.Fatalf("auth %q ct %q", gotAuth, gotCT)
	}
	if len(gotBody) != 1 || gotBody["domain"] != "yourname.hearthlaunch.com" {
		t.Fatalf("body %v", gotBody)
	}
}

func TestFleetStatusAndVehicles(t *testing.T) {
	var statusBody map[string][]string
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/1/vehicles":
			_, _ = w.Write([]byte(`{"response":[{"id":100021,"vehicle_id":99999,"vin":"5YJ3E1EA0KF000001","display_name":"Ember","state":"asleep"}],"count":1}`))
		case "/api/1/vehicles/fleet_status":
			b, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(b, &statusBody)
			_, _ = w.Write([]byte(`{"response":{"key_paired_vins":["5YJ3E1EA0KF000001"],"unpaired_vins":[],"vehicle_info":{"5YJ3E1EA0KF000001":{"firmware_version":"2026.32.1","vehicle_command_protocol_required":true}}}}`))
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
			w.WriteHeader(404)
		}
	}))
	defer api.Close()
	f := &Fleet{HTTP: http.DefaultClient, Base: api.URL, Token: func(context.Context) (string, error) { return "t", nil }}
	vs, err := f.Vehicles(context.Background())
	if err != nil || len(vs) != 1 || vs[0].VIN != "5YJ3E1EA0KF000001" || vs[0].DisplayName != "Ember" {
		t.Fatalf("%v %+v", err, vs)
	}
	fs, err := f.FleetStatus(context.Background(), []string{vs[0].VIN})
	if err != nil || len(fs.KeyPairedVINs) != 1 {
		t.Fatalf("%v %+v", err, fs)
	}
	if len(statusBody["vins"]) != 1 {
		t.Fatalf("fleet_status body %v", statusBody)
	}
}
