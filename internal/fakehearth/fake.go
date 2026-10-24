// Package fakehearth is an in-memory stand-in for the Hearthloom
// daemon's ember API, for tests only: KV, files, sealed secrets, the
// OAuth take, notifications and an idle bus stream. Every write is
// appended to Log so tests can check ordering.
package fakehearth

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"
)

// Daemon is the fake.
type Daemon struct {
	*httptest.Server
	Key string

	mu      sync.Mutex
	KV      map[string]string
	Files   map[string][]byte
	Secrets map[string]string
	Log     []string
	Notes   []map[string]any
	// Movements and Episodes record POST /v1/ingest/movement and /v1/ingest
	// bodies; FailIngest makes both answer this status (0 = accept).
	Movements  []map[string]any
	Episodes   []map[string]any
	FailIngest int
	// OAuth is a waiting sign-in code (taken once).
	OAuthCode, OAuthState string
	// FailSecretPut makes secret writes for these names fail (500).
	FailSecretPut map[string]bool
	Puts          map[string]int // file path → PUT count
}

// New starts a fake daemon.
func New() *Daemon {
	d := &Daemon{
		Key: "test-ember-key", KV: map[string]string{}, Files: map[string][]byte{},
		Secrets: map[string]string{}, FailSecretPut: map[string]bool{}, Puts: map[string]int{},
	}
	d.Server = httptest.NewServer(http.HandlerFunc(d.serve))
	return d
}

func (d *Daemon) logf(s string) {
	d.Log = append(d.Log, s)
}

// SetOAuth places a waiting sign-in code (as the daemon's callback does).
func (d *Daemon) SetOAuth(code, state string) {
	d.mu.Lock()
	d.OAuthCode, d.OAuthState = code, state
	d.mu.Unlock()
}

// KVValue reads a KV entry safely while the app runs.
func (d *Daemon) KVValue(key string) string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.KV[key]
}

// SetKV writes a KV entry (as the page would).
func (d *Daemon) SetKV(key, value string) {
	d.mu.Lock()
	d.KV[key] = value
	d.mu.Unlock()
}

// Snapshot copies what the app has posted so far.
func (d *Daemon) Snapshot() (movements, episodes, notes []map[string]any) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]map[string]any(nil), d.Movements...), append([]map[string]any(nil), d.Episodes...),
		append([]map[string]any(nil), d.Notes...)
}

// Events returns a copy of the write log.
func (d *Daemon) Events() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]string(nil), d.Log...)
}

func jsonOut(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func (d *Daemon) serve(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Authorization") != "Bearer "+d.Key {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	// decode like axum: match on the raw path, unescape per segment
	raw := r.URL.EscapedPath()
	body, _ := io.ReadAll(r.Body)
	d.mu.Lock()
	defer d.mu.Unlock()
	switch {
	case raw == "/v1/bus/stream":
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		d.mu.Unlock()
		select {
		case <-r.Context().Done():
		case <-time.After(2 * time.Second):
		}
		d.mu.Lock()
	case raw == "/v1/plugins/self/kv":
		var out []map[string]string
		for k, v := range d.KV {
			out = append(out, map[string]string{"key": k, "value": v})
		}
		jsonOut(w, map[string]any{"kv": out})
	case strings.HasPrefix(raw, "/v1/plugins/self/kv/"):
		seg := strings.TrimPrefix(raw, "/v1/plugins/self/kv/")
		if strings.Contains(seg, "/") {
			http.Error(w, "no route", http.StatusNotFound) // axum: one segment
			return
		}
		key, _ := url.PathUnescape(seg)
		switch r.Method {
		case http.MethodGet:
			v, ok := d.KV[key]
			if !ok {
				http.Error(w, "no value", http.StatusNotFound)
				return
			}
			jsonOut(w, map[string]string{"key": key, "value": v})
		case http.MethodPut:
			d.KV[key] = string(body)
			d.logf("kv:put:" + key)
			jsonOut(w, map[string]any{"key": key})
		case http.MethodDelete:
			delete(d.KV, key)
			jsonOut(w, map[string]any{"deleted": true})
		}
	case raw == "/v1/plugins/self/files":
		var out []map[string]any
		for p, b := range d.Files {
			out = append(out, map[string]any{"path": p, "size": len(b), "mtime": 0})
		}
		sort.Slice(out, func(i, j int) bool { return out[i]["path"].(string) < out[j]["path"].(string) })
		jsonOut(w, map[string]any{"files": out})
	case strings.HasPrefix(raw, "/v1/plugins/self/files/"):
		p, _ := url.PathUnescape(strings.TrimPrefix(raw, "/v1/plugins/self/files/"))
		switch r.Method {
		case http.MethodGet:
			b, ok := d.Files[p]
			if !ok {
				http.Error(w, "no such file", http.StatusNotFound)
				return
			}
			_, _ = w.Write(b)
		case http.MethodPut:
			d.Files[p] = body
			d.Puts[p]++
			d.logf("file:put:" + p)
			jsonOut(w, map[string]any{"path": p})
		case http.MethodDelete:
			_, ok := d.Files[p]
			delete(d.Files, p)
			d.logf("file:delete:" + p)
			jsonOut(w, map[string]any{"deleted": ok})
		}
	case raw == "/v1/plugins/self/secrets":
		var out []map[string]string
		for k := range d.Secrets {
			out = append(out, map[string]string{"key": k, "set_at": "2026-10-23T00:00:00Z"})
		}
		jsonOut(w, map[string]any{"secrets": out, "sealing": "node-key"})
	case strings.HasPrefix(raw, "/v1/plugins/self/secrets/"):
		key, _ := url.PathUnescape(strings.TrimPrefix(raw, "/v1/plugins/self/secrets/"))
		switch r.Method {
		case http.MethodGet:
			v, ok := d.Secrets[key]
			if !ok {
				http.Error(w, "no secret", http.StatusNotFound)
				return
			}
			jsonOut(w, map[string]string{"key": key, "value": v})
		case http.MethodPut:
			if d.FailSecretPut[key] {
				d.logf("secret:put-failed:" + key)
				http.Error(w, "disk full", http.StatusInternalServerError)
				return
			}
			d.Secrets[key] = string(body)
			d.logf("secret:put:" + key + "=" + string(body))
			jsonOut(w, map[string]any{"key": key, "set": true})
		case http.MethodDelete:
			_, ok := d.Secrets[key]
			delete(d.Secrets, key)
			d.logf("secret:delete:" + key)
			jsonOut(w, map[string]any{"deleted": ok})
		}
	case raw == "/v1/plugins/self/oauth/take":
		if d.OAuthCode == "" {
			http.Error(w, "no sign-in code waiting", http.StatusNotFound)
			return
		}
		jsonOut(w, map[string]string{"code": d.OAuthCode, "state": d.OAuthState})
		d.logf("oauth:take")
		d.OAuthCode, d.OAuthState = "", ""
	case raw == "/v1/ingest/movement":
		if d.FailIngest != 0 {
			http.Error(w, "refused", d.FailIngest)
			return
		}
		var m map[string]any
		_ = json.Unmarshal(body, &m)
		d.Movements = append(d.Movements, m)
		d.logf("movement")
		jsonOut(w, map[string]any{"accepted": 1, "duplicates": 0, "subject": "vehicle:htesla:x"})
	case raw == "/v1/ingest":
		if d.FailIngest != 0 {
			http.Error(w, "refused", d.FailIngest)
			return
		}
		var m map[string]any
		_ = json.Unmarshal(body, &m)
		if eps, ok := m["episodes"].([]any); ok {
			for _, e := range eps {
				if em, ok := e.(map[string]any); ok {
					d.Episodes = append(d.Episodes, em)
				}
			}
		}
		d.logf("ingest")
		jsonOut(w, map[string]any{"episodes_new": 1})
	case raw == "/v1/notifications":
		var n map[string]any
		_ = json.Unmarshal(body, &n)
		d.Notes = append(d.Notes, n)
		jsonOut(w, map[string]any{"id": "n1"})
	default:
		http.Error(w, "no route "+raw, http.StatusNotFound)
	}
}
