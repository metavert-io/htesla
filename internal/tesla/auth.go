// Package tesla talks to Tesla's official Fleet API — read-only toward
// the car. It requests only the scopes in Scopes, never calls wake_up,
// never sends vehicle commands and never polls the billed vehicle_data
// endpoint. The free endpoints it uses: vehicles, vehicle, fleet_status,
// users/region, partner_accounts (register), and fleet_telemetry_config
// (create via the signed _jws form, get, errors).
package tesla

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// Scopes is everything this ember ever asks the owner for. No command
// scopes (vehicle_cmds, vehicle_charging_cmds) — ever.
const Scopes = "openid offline_access vehicle_device_data vehicle_location"

// PartnerScopes is what the app-level (client_credentials) token asks for.
const PartnerScopes = "openid vehicle_device_data vehicle_location"

const (
	AuthorizeURL = "https://auth.tesla.com/oauth2/v3/authorize"
	TokenURL     = "https://fleet-auth.prd.vn.cloud.tesla.com/oauth2/v3/token"
)

// Regional Fleet API base URLs.
var Regions = map[string]string{
	"na": "https://fleet-api.prd.na.vn.cloud.tesla.com",
	"eu": "https://fleet-api.prd.eu.vn.cloud.tesla.com",
}

// BaseURL returns the Fleet API base for a region (default na).
func BaseURL(region string) string {
	if u, ok := Regions[strings.ToLower(strings.TrimSpace(region))]; ok {
		return u
	}
	return Regions["na"]
}

// CallbackPath is where the daemon's OAuth relay answers for this ember.
const CallbackPath = "/plugins/htesla/oauth/callback"

// RedirectURI is the address Tesla sends the owner back to.
func RedirectURI(publicOrigin string) string {
	return strings.TrimRight(publicOrigin, "/") + CallbackPath
}

// AuthorizeLink builds the sign-in address (the page builds the same
// one in JavaScript; this is the reference the tests pin).
func AuthorizeLink(clientID, redirectURI, state string) string {
	q := url.Values{}
	q.Set("response_type", "code")
	q.Set("client_id", clientID)
	q.Set("redirect_uri", redirectURI)
	q.Set("scope", Scopes)
	q.Set("state", state)
	return AuthorizeURL + "?" + q.Encode()
}

// TokenResponse is Tesla's token endpoint answer.
type TokenResponse struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	IDToken      string `json:"id_token"`
	ExpiresIn    int64  `json:"expires_in"`
	TokenType    string `json:"token_type"`
	Error        string `json:"error"`
	ErrorDesc    string `json:"error_description"`
}

// AuthError is a refusal from the token endpoint.
type AuthError struct {
	Status int
	Code   string // e.g. login_required, invalid_grant
	Desc   string
}

func (e *AuthError) Error() string {
	if e.Desc != "" {
		return fmt.Sprintf("tesla sign-in refused (%d %s): %s", e.Status, e.Code, e.Desc)
	}
	return fmt.Sprintf("tesla sign-in refused (%d %s)", e.Status, e.Code)
}

// NeedsSignIn reports whether err means the owner must sign in again
// (the refresh token is spent, expired or revoked).
func NeedsSignIn(err error) bool {
	var ae *AuthError
	if errors.As(err, &ae) {
		return ae.Code == "login_required" || ae.Code == "invalid_grant" || ae.Status == http.StatusUnauthorized
	}
	return errors.Is(err, ErrNoRefreshToken)
}

// ErrNoRefreshToken means nobody has signed in yet.
var ErrNoRefreshToken = errors.New("not signed in to Tesla")

// postForm sends a form to the token endpoint.
func postForm(ctx context.Context, hc *http.Client, tokenURL string, form url.Values) (*TokenResponse, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, tokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	resp, err := hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	var tr TokenResponse
	_ = json.Unmarshal(data, &tr)
	if resp.StatusCode/100 != 2 || tr.AccessToken == "" {
		code := tr.Error
		if code == "" {
			code = http.StatusText(resp.StatusCode)
		}
		return nil, &AuthError{Status: resp.StatusCode, Code: code, Desc: tr.ErrorDesc}
	}
	return &tr, nil
}

// PartnerToken gets an app-level token (client_credentials) for the
// partner endpoints (domain registration). It is never stored.
func PartnerToken(ctx context.Context, hc *http.Client, tokenURL, clientID, clientSecret, audience string) (string, error) {
	tr, err := postForm(ctx, hc, tokenURL, url.Values{
		"grant_type":    {"client_credentials"},
		"client_id":     {clientID},
		"client_secret": {clientSecret},
		"scope":         {PartnerScopes},
		"audience":      {audience},
	})
	if err != nil {
		return "", err
	}
	return tr.AccessToken, nil
}

// ExchangeCode trades the sign-in code for the owner's tokens.
func ExchangeCode(ctx context.Context, hc *http.Client, tokenURL, clientID, clientSecret, code, audience, redirectURI string) (*TokenResponse, error) {
	return postForm(ctx, hc, tokenURL, url.Values{
		"grant_type":    {"authorization_code"},
		"client_id":     {clientID},
		"client_secret": {clientSecret},
		"code":          {code},
		"audience":      {audience},
		"redirect_uri":  {redirectURI},
	})
}

// JWTExpiry reads the exp claim of a JWT without verifying it (we only
// need to know when to renew our own token).
func JWTExpiry(token string) (time.Time, bool) {
	parts := strings.Split(token, ".")
	if len(parts) < 2 {
		return time.Time{}, false
	}
	payload, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(parts[1], "="))
	if err != nil {
		return time.Time{}, false
	}
	var claims struct {
		Exp json.Number `json:"exp"`
	}
	if json.Unmarshal(payload, &claims) != nil || claims.Exp == "" {
		return time.Time{}, false
	}
	secs, err := claims.Exp.Int64()
	if err != nil {
		f, ferr := claims.Exp.Float64()
		if ferr != nil {
			return time.Time{}, false
		}
		secs = int64(f)
	}
	return time.Unix(secs, 0), true
}

// RefreshStore is where the rotating refresh token lives (the daemon's
// sealed secret store in production).
type RefreshStore interface {
	LoadRefresh(ctx context.Context) (string, error) // ErrNoRefreshToken if none
	SaveRefresh(ctx context.Context, token string) error
}

// Tokens keeps the owner's access token in memory and renews it with
// the single-use, rotating refresh token. THE ORDERING RULE: each new
// refresh token is saved (sealed) BEFORE its access token is used —
// a crash between the two must never leave us holding only a spent
// token. (Tesla keeps the previous refresh token valid for up to 24h,
// which covers a save that failed and is retried.)
type Tokens struct {
	HTTP     *http.Client
	TokenURL string
	ClientID string
	Store    RefreshStore
	Now      func() time.Time

	mu      sync.Mutex
	access  string
	expires time.Time
	pending *TokenResponse // a rotated pair whose refresh token isn't saved yet
}

func (t *Tokens) now() time.Time {
	if t.Now != nil {
		return t.Now()
	}
	return time.Now()
}

// Adopt installs a freshly exchanged token pair: refresh token saved
// first, then the access token becomes usable.
func (t *Tokens) Adopt(ctx context.Context, tr *TokenResponse) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.adoptLocked(ctx, tr)
}

func (t *Tokens) adoptLocked(ctx context.Context, tr *TokenResponse) error {
	if tr.RefreshToken != "" {
		if err := t.Store.SaveRefresh(ctx, tr.RefreshToken); err != nil {
			// keep the pair so the next call retries the save; don't use
			// the access token until the refresh token is safe
			t.pending = tr
			t.access = ""
			return fmt.Errorf("saving the new Tesla refresh token: %w", err)
		}
	}
	t.pending = nil
	t.access = tr.AccessToken
	if exp, ok := JWTExpiry(tr.AccessToken); ok {
		t.expires = exp
	} else if tr.ExpiresIn > 0 {
		t.expires = t.now().Add(time.Duration(tr.ExpiresIn) * time.Second)
	} else {
		t.expires = t.now().Add(5 * time.Minute)
	}
	return nil
}

// Forget drops the in-memory access token (e.g. after a 401).
func (t *Tokens) Forget() {
	t.mu.Lock()
	t.access = ""
	t.mu.Unlock()
}

// Access returns a usable access token, renewing it when it is within
// a minute of expiry.
func (t *Tokens) Access(ctx context.Context) (string, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.access != "" && t.now().Add(time.Minute).Before(t.expires) {
		return t.access, nil
	}
	if t.pending != nil {
		// a rotated pair is waiting: save its refresh token, then use it
		tr := t.pending
		if err := t.adoptLocked(ctx, tr); err != nil {
			return "", err
		}
		if t.access != "" && t.now().Add(time.Minute).Before(t.expires) {
			return t.access, nil
		}
		return t.refreshLocked(ctx, tr.RefreshToken)
	}
	rt, err := t.Store.LoadRefresh(ctx)
	if err != nil {
		return "", err
	}
	return t.refreshLocked(ctx, rt)
}

func (t *Tokens) refreshLocked(ctx context.Context, refreshToken string) (string, error) {
	tr, err := postForm(ctx, t.HTTP, t.TokenURL, url.Values{
		"grant_type":    {"refresh_token"},
		"client_id":     {t.ClientID},
		"refresh_token": {refreshToken},
	})
	if err != nil {
		return "", err
	}
	if err := t.adoptLocked(ctx, tr); err != nil {
		return "", err
	}
	return t.access, nil
}
