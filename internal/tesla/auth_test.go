package tesla

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestAuthorizeLink(t *testing.T) {
	redirect := RedirectURI("https://yourname.hearthlaunch.com/")
	if redirect != "https://yourname.hearthlaunch.com/plugins/htesla/oauth/callback" {
		t.Fatalf("redirect %q", redirect)
	}
	link := AuthorizeLink("client-123", redirect, "st4te-with+odd&chars=0123456789")
	u, err := url.Parse(link)
	if err != nil {
		t.Fatal(err)
	}
	if u.Scheme != "https" || u.Host != "auth.tesla.com" || u.Path != "/oauth2/v3/authorize" {
		t.Fatalf("wrong endpoint: %s", link)
	}
	q := u.Query()
	want := map[string]string{
		"response_type": "code",
		"client_id":     "client-123",
		"redirect_uri":  redirect,
		"scope":         "openid offline_access vehicle_device_data vehicle_location",
		"state":         "st4te-with+odd&chars=0123456789",
	}
	for k, v := range want {
		if q.Get(k) != v {
			t.Errorf("%s = %q, want %q", k, q.Get(k), v)
		}
	}
	if len(q) != len(want) {
		t.Errorf("unexpected extra parameters: %v", q)
	}
	// read-only: no command scopes, ever
	for _, bad := range []string{"vehicle_cmds", "vehicle_charging_cmds", "energy_cmds"} {
		if strings.Contains(link, bad) || strings.Contains(Scopes, bad) || strings.Contains(PartnerScopes, bad) {
			t.Fatalf("%s must never be requested", bad)
		}
	}
	// the raw query is encoded (spaces, &, + survive the trip)
	if strings.Contains(u.RawQuery, " ") || strings.Count(u.RawQuery, "&") != 4 {
		t.Fatalf("query not encoded: %s", u.RawQuery)
	}
}

func fakeJWT(exp time.Time, tag string) string {
	h := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none"}`))
	p := base64.RawURLEncoding.EncodeToString([]byte(fmt.Sprintf(`{"exp":%d,"tag":%q}`, exp.Unix(), tag)))
	return h + "." + p + ".sig"
}

func TestJWTExpiry(t *testing.T) {
	exp := time.Unix(1893456000, 0)
	got, ok := JWTExpiry(fakeJWT(exp, "a"))
	if !ok || !got.Equal(exp) {
		t.Fatalf("got %v %v", got, ok)
	}
	if _, ok := JWTExpiry("not-a-jwt"); ok {
		t.Fatal("garbage should not parse")
	}
}

// eventLog records what happened, in order, across the fake servers
// and the fake secret store.
type eventLog struct {
	mu sync.Mutex
	ev []string
}

func (l *eventLog) add(s string) { l.mu.Lock(); l.ev = append(l.ev, s); l.mu.Unlock() }
func (l *eventLog) all() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.ev...)
}
func (l *eventLog) index(s string) int {
	for i, e := range l.all() {
		if e == s {
			return i
		}
	}
	return -1
}

type memStore struct {
	log     *eventLog
	token   string
	failing bool
}

func (m *memStore) LoadRefresh(context.Context) (string, error) {
	if m.token == "" {
		return "", ErrNoRefreshToken
	}
	return m.token, nil
}

func (m *memStore) SaveRefresh(_ context.Context, tok string) error {
	if m.failing {
		m.log.add("save-failed:" + tok)
		return errors.New("disk full")
	}
	m.token = tok
	m.log.add("save:" + tok)
	return nil
}

// A fake Tesla token endpoint whose refresh tokens are SINGLE-USE and
// rotate: R1 → (A2, R2) → (A3, R3)…
func rotatingTokenServer(t *testing.T, log *eventLog, now func() time.Time) (*httptest.Server, *int) {
	used := map[string]bool{}
	n := 1
	calls := 0
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		_ = r.ParseForm()
		calls++
		if r.Form.Get("grant_type") != "refresh_token" || r.Form.Get("client_id") != "cid" {
			t.Errorf("unexpected token request %v", r.Form)
		}
		rt := r.Form.Get("refresh_token")
		log.add("refresh-with:" + rt)
		if used[rt] || !strings.HasPrefix(rt, "R") {
			w.WriteHeader(http.StatusUnauthorized)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": "login_required", "error_description": "The refresh_token is invalid"})
			return
		}
		used[rt] = true
		n++
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token":  fakeJWT(now().Add(8*time.Hour), fmt.Sprintf("A%d", n)),
			"refresh_token": fmt.Sprintf("R%d", n),
			"expires_in":    28800,
		})
	}))
	t.Cleanup(srv.Close)
	return srv, &calls
}

func tagOf(tok string) string {
	parts := strings.Split(tok, ".")
	b, _ := base64.RawURLEncoding.DecodeString(parts[1])
	var c struct {
		Tag string `json:"tag"`
	}
	_ = json.Unmarshal(b, &c)
	return c.Tag
}

func TestRefreshRotationSavesBeforeUse(t *testing.T) {
	log := &eventLog{}
	clock := time.Date(2026, 10, 23, 12, 0, 0, 0, time.UTC)
	now := func() time.Time { return clock }
	tokSrv, calls := rotatingTokenServer(t, log, now)
	// a Fleet API that records which access token each call carries
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		log.add("api-call-with:" + tagOf(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")))
		_, _ = w.Write([]byte(`{"response":[]}`))
	}))
	defer api.Close()

	store := &memStore{log: log}
	tokens := &Tokens{HTTP: http.DefaultClient, TokenURL: tokSrv.URL, ClientID: "cid", Store: store, Now: now}
	// the sign-in: an access token 30s from expiry (inside the renewal
	// minute) and refresh token R1
	if err := tokens.Adopt(context.Background(), &TokenResponse{
		AccessToken: fakeJWT(clock.Add(30*time.Second), "A1"), RefreshToken: "R1",
	}); err != nil {
		t.Fatal(err)
	}
	if log.index("save:R1") != 0 {
		t.Fatalf("the first refresh token must be saved on adoption: %v", log.all())
	}
	fleet := &Fleet{HTTP: http.DefaultClient, Base: api.URL, Token: tokens.Access}
	if _, err := fleet.Vehicles(context.Background()); err != nil {
		t.Fatal(err)
	}
	// renewed with R1, R2 saved BEFORE A2 was used
	iRefresh, iSave, iUse := log.index("refresh-with:R1"), log.index("save:R2"), log.index("api-call-with:A2")
	if iRefresh < 0 || iSave < 0 || iUse < 0 || !(iRefresh < iSave && iSave < iUse) {
		t.Fatalf("ordering wrong (want refresh R1 < save R2 < use A2): %v", log.all())
	}
	// a fresh token is reused without another refresh
	if _, err := fleet.Vehicles(context.Background()); err != nil {
		t.Fatal(err)
	}
	if *calls != 1 {
		t.Fatalf("token endpoint called %d times, want 1", *calls)
	}

	// --- a save failure: the new access token must NOT be used until its
	// refresh token is safe, and no second network refresh is spent
	clock = clock.Add(8 * time.Hour) // A2 expired
	store.failing = true
	if _, err := fleet.Vehicles(context.Background()); err == nil {
		t.Fatal("expected an error while the refresh token can't be saved")
	}
	if log.index("api-call-with:A3") >= 0 {
		t.Fatalf("A3 was used before R3 was saved: %v", log.all())
	}
	if log.index("save-failed:R3") < 0 {
		t.Fatalf("expected a failed save of R3: %v", log.all())
	}
	store.failing = false
	if _, err := fleet.Vehicles(context.Background()); err != nil {
		t.Fatal(err)
	}
	iSave3, iUse3 := log.index("save:R3"), log.index("api-call-with:A3")
	if iSave3 < 0 || iUse3 < 0 || iSave3 > iUse3 {
		t.Fatalf("R3 must be saved before A3 is used: %v", log.all())
	}
	if *calls != 2 {
		t.Fatalf("the retried save must not spend another refresh (calls=%d)", *calls)
	}
	if store.token != "R3" {
		t.Fatalf("stored token %q, want R3", store.token)
	}
}

func TestSpentRefreshTokenNeedsSignIn(t *testing.T) {
	log := &eventLog{}
	now := time.Now
	tokSrv, _ := rotatingTokenServer(t, log, now)
	store := &memStore{log: log, token: "spent-token"}
	tokens := &Tokens{HTTP: http.DefaultClient, TokenURL: tokSrv.URL, ClientID: "cid", Store: store}
	_, err := tokens.Access(context.Background())
	if err == nil || !NeedsSignIn(err) {
		t.Fatalf("a login_required refusal should mean sign in again, got %v", err)
	}
	var ae *AuthError
	if !errors.As(err, &ae) || ae.Code != "login_required" || ae.Status != 401 {
		t.Fatalf("got %#v", err)
	}
	empty := &Tokens{HTTP: http.DefaultClient, TokenURL: tokSrv.URL, ClientID: "cid", Store: &memStore{log: log}}
	if _, err := empty.Access(context.Background()); !errors.Is(err, ErrNoRefreshToken) || !NeedsSignIn(err) {
		t.Fatalf("no token at all: %v", err)
	}
}

func TestExchangeCodeRequest(t *testing.T) {
	var form url.Values
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Content-Type") != "application/x-www-form-urlencoded" {
			t.Errorf("content type %q", r.Header.Get("Content-Type"))
		}
		_ = r.ParseForm()
		form = r.PostForm
		_, _ = w.Write([]byte(`{"access_token":"a","refresh_token":"r","expires_in":28800}`))
	}))
	defer srv.Close()
	tr, err := ExchangeCode(context.Background(), http.DefaultClient, srv.URL, "cid", "sec", "the-code",
		"https://fleet-api.prd.na.vn.cloud.tesla.com", "https://x.example.com/plugins/htesla/oauth/callback")
	if err != nil || tr.RefreshToken != "r" {
		t.Fatal(err)
	}
	want := map[string]string{
		"grant_type": "authorization_code", "client_id": "cid", "client_secret": "sec", "code": "the-code",
		"audience":     "https://fleet-api.prd.na.vn.cloud.tesla.com",
		"redirect_uri": "https://x.example.com/plugins/htesla/oauth/callback",
	}
	for k, v := range want {
		if form.Get(k) != v {
			t.Errorf("%s = %q, want %q", k, form.Get(k), v)
		}
	}
}
