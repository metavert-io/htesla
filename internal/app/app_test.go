package app

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/metavert-io/htesla/internal/fakehearth"
	"github.com/metavert-io/htesla/internal/hearth"
	"github.com/metavert-io/htesla/internal/keys"
	"github.com/metavert-io/htesla/third_party/fleettelemetry"
)

const vin = "5YJ3E1EA0KF000001"

// fakeTesla is Tesla's token endpoint + Fleet API, recording requests.
type fakeTesla struct {
	srv  *httptest.Server
	mu   sync.Mutex
	reqs []string
	form map[string]url.Values // by grant_type
	body map[string]string     // by path
}

func newFakeTesla(t *testing.T) *fakeTesla {
	f := &fakeTesla{form: map[string]url.Values{}, body: map[string]string{}}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		b, _ := io.ReadAll(r.Body)
		f.reqs = append(f.reqs, r.Method+" "+r.URL.Path)
		f.body[r.URL.Path] = string(b)
		switch r.URL.Path {
		case "/token":
			form, _ := url.ParseQuery(string(b))
			f.form[form.Get("grant_type")] = form
			if form.Get("grant_type") == "client_credentials" {
				_, _ = w.Write([]byte(`{"access_token":"partner-tok","expires_in":28800}`))
				return
			}
			_, _ = w.Write([]byte(`{"access_token":"user-access","refresh_token":"R1","expires_in":28800}`))
		case "/api/1/partner_accounts":
			if r.Header.Get("Authorization") != "Bearer partner-tok" {
				w.WriteHeader(401)
				return
			}
			_, _ = w.Write([]byte(`{"response":{}}`))
		case "/api/1/users/region":
			_, _ = w.Write([]byte(fmt.Sprintf(`{"response":{"region":"na","fleet_api_base_url":%q}}`, f.srv.URL)))
		case "/api/1/vehicles":
			_, _ = w.Write([]byte(`{"response":[{"id":1,"vehicle_id":2,"vin":"` + vin + `","display_name":"Ember","state":"asleep"}]}`))
		case "/api/1/vehicles/fleet_status":
			_, _ = w.Write([]byte(`{"response":{"key_paired_vins":["` + vin + `"],"unpaired_vins":[],"vehicle_info":{"` + vin + `":{"firmware_version":"2026.32.1"}}}}`))
		case "/api/1/vehicles/fleet_telemetry_config_jws":
			_, _ = w.Write([]byte(`{"response":{"updated_vehicles":1,"skipped_vehicles":{"missing_key":[]}}}`))
		case "/api/1/vehicles/" + vin + "/fleet_telemetry_config":
			_, _ = w.Write([]byte(`{"response":{"synced":false,"limit_reached":false,"key_paired":true,"config":{}}}`))
		case "/api/1/vehicles/" + vin + "/fleet_telemetry_errors":
			_, _ = w.Write([]byte(`{"response":{"fleet_telemetry_errors":[]}}`))
		default:
			t.Errorf("unexpected Tesla call %s %s", r.Method, r.URL.Path)
			w.WriteHeader(404)
		}
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeTesla) calls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.reqs...)
}

func freePort(t *testing.T) int {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port
}

func newApp(t *testing.T, d *fakehearth.Daemon, ft *fakeTesla) *App {
	a := &App{
		H:          hearth.NewWithKey(d.URL, d.Key),
		TeslaCAs:   fleettelemetry.TeslaCAs(),
		ListenHost: "127.0.0.1",
		Version:    "test",
	}
	if ft != nil {
		a.TokenURL = ft.srv.URL + "/token"
		a.FleetBaseOverride = ft.srv.URL
	}
	a.init()
	t.Cleanup(func() { _ = a.recv.Close() })
	return a
}

func setConfig(d *fakehearth.Daemon, port int) {
	d.KV["config"] = fmt.Sprintf(`{"public_origin":"https://YourName.hearthlaunch.com/","client_id":"cid-1","region":"na","telemetry_port":%d}`, port)
}

func indexOf(log []string, prefix string) int {
	for i, e := range log {
		if strings.HasPrefix(e, prefix) {
			return i
		}
	}
	return -1
}

func TestKeysMadeOnceSealedFirst(t *testing.T) {
	d := fakehearth.New()
	defer d.Close()
	a := newApp(t, d, nil)
	ctx := context.Background()
	a.ensureKeys(ctx)
	log := d.Events()
	iPriv, iPub := indexOf(log, "secret:put:"+SecretPrivateKey), indexOf(log, "file:put:"+FilePublicKey)
	if iPriv < 0 || iPub < 0 || iPriv > iPub {
		t.Fatalf("private key must be sealed before the public key is published: %v", log)
	}
	if a.st.Keys.State != "ready" || !keys.SamePublicKey(string(d.Files[FilePublicKey]), a.st.Keys.PublicPEM) {
		t.Fatalf("keys status %+v", a.st.Keys)
	}
	priv := d.Secrets[SecretPrivateKey]
	// a second start never regenerates
	b := newApp(t, d, nil)
	b.ensureKeys(ctx)
	if d.Secrets[SecretPrivateKey] != priv {
		t.Fatal("the private key was regenerated")
	}
	// private half lost (e.g. after a restore): say so, don't replace
	delete(d.Secrets, SecretPrivateKey)
	c := newApp(t, d, nil)
	c.ensureKeys(ctx)
	if c.st.Keys.State != "lost" || d.Secrets[SecretPrivateKey] != "" {
		t.Fatalf("lost key handling: %+v", c.st.Keys)
	}
	// …until the owner explicitly asks for a new one
	c.newKey(ctx)
	if c.st.Keys.State != "ready" || d.Secrets[SecretPrivateKey] == "" || d.Secrets[SecretPrivateKey] == priv {
		t.Fatalf("new key: %+v", c.st.Keys)
	}
	// a daemon error never leads to generating
	d2 := fakehearth.New()
	defer d2.Close()
	e := newApp(t, d2, nil)
	e.H = hearth.NewWithKey(d2.URL, "wrong-key") // every call fails 401
	e.ensureKeys(ctx)
	if e.st.Keys.State != "error" || len(d2.Secrets) != 0 {
		t.Fatalf("error path: %+v", e.st.Keys)
	}
}

func TestRegisterChecksKeyThenRegistersDomain(t *testing.T) {
	d := fakehearth.New()
	defer d.Close()
	ft := newFakeTesla(t)
	setConfig(d, freePort(t))
	d.Secrets[SecretClientSecret] = "shh"
	a := newApp(t, d, ft)
	ctx := context.Background()
	a.reloadConfig(ctx)
	a.ensureKeys(ctx)

	// not served yet → refused, Tesla never called
	var fetched string
	a.FetchPublicKey = func(_ context.Context, u string) (string, error) { fetched = u; return "<html>nope</html>", nil }
	a.register(ctx)
	if a.st.Register.State != "error" || len(ft.calls()) != 0 {
		t.Fatalf("register without a served key: %+v calls %v", a.st.Register, ft.calls())
	}
	if fetched != "https://yourname.hearthlaunch.com/.well-known/appspecific/com.tesla.3p.public-key.pem" {
		t.Fatalf("checked %q", fetched)
	}
	// served → partner token, then register the lowercase host
	a.FetchPublicKey = func(context.Context, string) (string, error) { return string(d.Files[FilePublicKey]), nil }
	a.register(ctx)
	if a.st.Register.State != "done" {
		t.Fatalf("register: %+v", a.st.Register)
	}
	cc := ft.form["client_credentials"]
	if cc.Get("client_id") != "cid-1" || cc.Get("client_secret") != "shh" || cc.Get("audience") != ft.srv.URL ||
		cc.Get("scope") != "openid vehicle_device_data vehicle_location" {
		t.Fatalf("partner token form %v", cc)
	}
	var body map[string]string
	_ = json.Unmarshal([]byte(ft.body["/api/1/partner_accounts"]), &body)
	if body["domain"] != "yourname.hearthlaunch.com" {
		t.Fatalf("registered %v", body)
	}
}

func TestSignInThenStreamingConfig(t *testing.T) {
	d := fakehearth.New()
	defer d.Close()
	ft := newFakeTesla(t)
	port := freePort(t)
	setConfig(d, port)
	d.Secrets[SecretClientSecret] = "shh"
	a := newApp(t, d, ft)
	ctx := context.Background()
	a.reloadConfig(ctx)
	a.ensureKeys(ctx)
	a.ensureReceiver(ctx)
	if !a.st.Receiver.Listening || a.st.Receiver.CertHost != "yourname.hearthlaunch.com" {
		t.Fatalf("receiver %+v", a.st.Receiver)
	}
	if _, ok := d.Secrets[SecretCAKey]; !ok {
		t.Fatal("CA key should be sealed")
	}
	if _, ok := d.Files[FileCACert]; !ok {
		t.Fatal("CA cert should be a file")
	}

	// the daemon's relay holds a code
	d.OAuthCode, d.OAuthState = "the-code", "st"
	a.take(ctx)
	if a.st.SignIn.State != "signed_in" {
		t.Fatalf("sign-in %+v", a.st.SignIn)
	}
	ac := ft.form["authorization_code"]
	if ac.Get("code") != "the-code" || ac.Get("redirect_uri") != "https://yourname.hearthlaunch.com/plugins/htesla/oauth/callback" ||
		ac.Get("audience") != ft.srv.URL || ac.Get("client_secret") != "shh" {
		t.Fatalf("code exchange %v", ac)
	}
	if d.Secrets[SecretRefreshToken] != "R1" {
		t.Fatal("refresh token not sealed")
	}
	if len(a.st.Vehicles) != 1 || a.st.Vehicles[0].Paired == nil || !*a.st.Vehicles[0].Paired ||
		a.st.Vehicles[0].PairURL != "https://tesla.com/_ak/yourname.hearthlaunch.com?vin="+vin {
		t.Fatalf("vehicles %+v", a.st.Vehicles)
	}
	if !a.allowVIN(vin) || a.allowVIN("5YJ3E1EA0KF999999") {
		t.Fatal("allowlist should be the account's cars")
	}

	a.startStreaming(ctx)
	if a.st.Streaming.State != "configured" || a.st.Streaming.Updated != 1 {
		t.Fatalf("streaming %+v", a.st.Streaming)
	}
	var sent struct {
		VINs  []string `json:"vins"`
		Token string   `json:"token"`
	}
	_ = json.Unmarshal([]byte(ft.body["/api/1/vehicles/fleet_telemetry_config_jws"]), &sent)
	if len(sent.VINs) != 1 || sent.VINs[0] != vin {
		t.Fatalf("vins %v", sent.VINs)
	}
	ck, _ := keys.CommandKey(d.Secrets[SecretPrivateKey])
	tok, err := jwt.Parse(sent.Token, func(*jwt.Token) (any, error) { return ck.PublicBytes(), nil },
		jwt.WithValidMethods([]string{"Tesla.SS256"}))
	if err != nil {
		t.Fatalf("the JWS sent to Tesla doesn't verify with our key: %v", err)
	}
	cl := tok.Claims.(jwt.MapClaims)
	if cl["hostname"] != "yourname.hearthlaunch.com" || int(cl["port"].(float64)) != port ||
		cl["ca"] != string(d.Files[FileCACert]) {
		t.Fatalf("claims %v", cl)
	}
	if a.st.Streaming.PerVIN[vin] == nil || a.st.Streaming.PerVIN[vin].KeyPaired == nil {
		t.Fatalf("per-vin check %+v", a.st.Streaming.PerVIN)
	}
	// pause: the free DELETE removes the config; resume sends a new one
	a.pauseStreaming(ctx)
	if a.st.Streaming.State != "paused" || a.st.Streaming.PausedAt == "" {
		t.Fatalf("pause: %+v", a.st.Streaming)
	}
	deleted := false
	for _, c := range ft.calls() {
		deleted = deleted || c == "DELETE /api/1/vehicles/"+vin+"/fleet_telemetry_config"
	}
	if !deleted {
		t.Fatalf("pause must DELETE the telemetry config: %v", ft.calls())
	}
	a.startStreaming(ctx)
	if a.st.Streaming.State != "configured" {
		t.Fatalf("resume: %+v", a.st.Streaming)
	}
	// read-only: none of the calls were commands, wake-ups or vehicle_data
	for _, c := range ft.calls() {
		if strings.Contains(c, "/command/") || strings.Contains(c, "wake_up") || strings.Contains(c, "vehicle_data") {
			t.Fatalf("forbidden call %s", c)
		}
	}
}

func TestCommandsRunOnceAndStaleOnesNever(t *testing.T) {
	d := fakehearth.New()
	defer d.Close()
	a := newApp(t, d, nil)
	a.handle(context.Background(), Command{ID: "c1", Action: "signin_started", At: now()})
	if a.st.SignIn.State != "waiting" {
		t.Fatalf("state %q", a.st.SignIn.State)
	}
	a.st.SignIn.State = "signed_out"
	a.handle(context.Background(), Command{ID: "c1", Action: "signin_started", At: now()})
	if a.st.SignIn.State != "signed_out" {
		t.Fatal("a command ran twice")
	}
	a.handle(context.Background(), Command{ID: "c2", Action: "signin_started", At: "2026-01-01T00:00:00Z"})
	if a.st.SignIn.State != "signed_out" {
		t.Fatal("a stale command ran")
	}
}

func TestConfigParsingAndProblems(t *testing.T) {
	c := ParseConfig(`{"public_origin":"https://Brain.Example.com/","client_id":"x","region":"EU","telemetry_port":"9443"}`)
	if c.Domain() != "brain.example.com" || c.Region != "eu" || c.TelemetryPort != 9443 ||
		c.RedirectURI() != "https://brain.example.com/plugins/htesla/oauth/callback" ||
		c.FleetBase() != "https://fleet-api.prd.eu.vn.cloud.tesla.com" {
		t.Fatalf("%+v %s", c, c.Domain())
	}
	if len(c.Problems()) != 0 {
		t.Fatalf("problems %v", c.Problems())
	}
	d := ParseConfig(``)
	if d.Region != "na" || d.TelemetryPort != 8443 || len(d.Problems()) != 2 {
		t.Fatalf("defaults %+v %v", d, d.Problems())
	}
	for _, bad := range []string{"http://x.example.com", "https://x.example.com:8443", "https://x.example.com/path"} {
		if len(ParseConfig(`{"public_origin":"`+bad+`","client_id":"x"}`).Problems()) != 1 {
			t.Errorf("%s should be refused", bad)
		}
	}
}

// The page's "signin_started" must switch the code pickup to the fast
// poll at once (the bus hint doesn't reach ember keys today), so the
// code is collected within seconds of Tesla's redirect.
func TestRunPicksUpSignInCodeQuickly(t *testing.T) {
	d := fakehearth.New()
	defer d.Close()
	a := &App{H: hearth.NewWithKey(d.URL, d.Key), TeslaCAs: fleettelemetry.TeslaCAs(), ListenHost: "127.0.0.1", Version: "test"}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = a.Run(ctx); close(done) }()
	defer func() { cancel(); <-done }()

	status := func() Status {
		var st Status
		_ = json.Unmarshal([]byte(d.KVValue(KVStatus)), &st)
		return st
	}
	waitFor := func(what string, limit time.Duration, ok func(Status) bool) time.Duration {
		start := time.Now()
		for time.Since(start) < limit {
			if ok(status()) {
				return time.Since(start)
			}
			time.Sleep(100 * time.Millisecond)
		}
		t.Fatalf("%s didn't happen within %s: %+v", what, limit, status().SignIn)
		return 0
	}
	waitFor("first status", 10*time.Second, func(s Status) bool { return s.Keys.State == "ready" })
	at := time.Now().UTC().Format(time.RFC3339)
	d.SetKV(KVCmd, `{"seen":"`+at+`","queue":[{"id":"q1","action":"signin_started","at":"`+at+`"}]}`)
	waitFor("waiting state", 15*time.Second, func(s Status) bool { return s.SignIn.State == "waiting" })
	time.Sleep(1500 * time.Millisecond) // the user is at Tesla…
	d.SetOAuth("code-1", "state-1")     // …and comes back
	took := waitFor("code pickup", 10*time.Second, func(s Status) bool { return s.SignIn.State == "error" })
	if took > 6*time.Second {
		t.Fatalf("code picked up after %s; want the 3s fast poll", took)
	}
	if !strings.Contains(status().SignIn.Error, "client secret") {
		t.Fatalf("expected the no-secret refusal (no Tesla call), got %q", status().SignIn.Error)
	}
}
