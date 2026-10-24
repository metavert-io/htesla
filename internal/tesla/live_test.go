//go:build live

package tesla

// A credentials check against Tesla's real token endpoint — the ONE
// live call the tests make, and a harmless one: a client_credentials
// (partner) token request. It changes nothing at Tesla. It never prints
// the secret or the token; only whether one came back and its expiry.
//
//	HTESLA_LIVE_ENV=~/.config/htesla/tesla.env go test -tags live -run Live ./internal/tesla/

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"
)

func readEnv(t *testing.T, path string) map[string]string {
	f, err := os.Open(path)
	if err != nil {
		t.Skipf("no env file: %v", err)
	}
	defer f.Close()
	out := map[string]string{}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if ok {
			out[strings.TrimSpace(k)] = strings.Trim(strings.TrimSpace(v), `"'`)
		}
	}
	return out
}

func TestLivePartnerToken(t *testing.T) {
	path := os.Getenv("HTESLA_LIVE_ENV")
	if path == "" {
		t.Skip("set HTESLA_LIVE_ENV to the tesla.env path")
	}
	env := readEnv(t, path)
	tokenURL := env["TESLA_AUTH_TOKEN_URL"]
	if tokenURL == "" {
		tokenURL = TokenURL
	}
	audience := env["TESLA_FLEET_API"]
	if audience == "" {
		audience = BaseURL("na")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	tok, err := PartnerToken(ctx, &http.Client{Timeout: 30 * time.Second}, tokenURL,
		env["TESLA_CLIENT_ID"], env["TESLA_CLIENT_SECRET"], audience)
	if err != nil {
		t.Fatalf("Tesla refused the client credentials: %v", err) // AuthError carries no secret
	}
	exp, ok := JWTExpiry(tok)
	if !ok {
		t.Fatal("token came back but isn't a JWT")
	}
	// report only non-secret claims
	var claims struct {
		Aud    any    `json:"aud"`
		Scopes []any  `json:"scp"`
		Iss    string `json:"iss"`
	}
	if parts := strings.Split(tok, "."); len(parts) >= 2 {
		if b, err := base64.RawURLEncoding.DecodeString(parts[1]); err == nil {
			_ = json.Unmarshal(b, &claims)
		}
	}
	t.Logf("partner token OK: expires in %s; aud=%v; scopes=%v; iss=%s",
		time.Until(exp).Round(time.Minute), claims.Aud, claims.Scopes, claims.Iss)
}
