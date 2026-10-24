// Package hearth is a small client for the Hearthloom daemon's ember
// API (docs/plugin-guide.md in the hearthloom repo). Everything this
// ember keeps goes through it: KV, files, sealed secrets, the OAuth
// relay, notifications and the bus. The ember never writes under
// ~/.hearthloom itself.
package hearth

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"
)

// ErrNotFound is returned when the daemon answers 404 (no such key,
// file, secret or waiting sign-in code).
var ErrNotFound = errors.New("not found")

// Client talks to the daemon on loopback with the ember's own key.
type Client struct {
	Base    string // e.g. http://127.0.0.1:43278
	KeyFile string // HEARTHLOOM_PLUGIN_KEY_FILE; re-read on a 401
	HTTP    *http.Client

	mu  sync.Mutex
	key string
}

// New builds a client from the environment the daemon hands a sidecar.
func New() (*Client, error) {
	base := os.Getenv("HEARTHLOOM_URL")
	if base == "" {
		base = "http://127.0.0.1:43278"
	}
	c := &Client{
		Base:    strings.TrimRight(base, "/"),
		KeyFile: os.Getenv("HEARTHLOOM_PLUGIN_KEY_FILE"),
		HTTP:    &http.Client{Timeout: 30 * time.Second},
	}
	if c.KeyFile == "" {
		return nil, errors.New("HEARTHLOOM_PLUGIN_KEY_FILE is not set — run this from Hearthloom")
	}
	if err := c.reloadKey(); err != nil {
		return nil, err
	}
	return c, nil
}

// NewWithKey is for tests: a fixed key, no key file.
func NewWithKey(base, key string) *Client {
	return &Client{Base: strings.TrimRight(base, "/"), HTTP: &http.Client{Timeout: 30 * time.Second}, key: key}
}

func (c *Client) reloadKey() error {
	if c.KeyFile == "" {
		return nil
	}
	b, err := os.ReadFile(c.KeyFile)
	if err != nil {
		return fmt.Errorf("reading the ember key: %w", err)
	}
	c.mu.Lock()
	c.key = strings.TrimSpace(string(b))
	c.mu.Unlock()
	return nil
}

func (c *Client) bearer() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.key
}

// do sends one request; on a 401 it re-reads the key file once (the
// user may have reinstalled) and retries.
func (c *Client) do(ctx context.Context, method, path string, body []byte, contentType string) (*http.Response, error) {
	for attempt := 0; attempt < 2; attempt++ {
		var rdr io.Reader
		if body != nil {
			rdr = bytes.NewReader(body)
		}
		req, err := http.NewRequestWithContext(ctx, method, c.Base+path, rdr)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Authorization", "Bearer "+c.bearer())
		if contentType != "" {
			req.Header.Set("Content-Type", contentType)
		}
		resp, err := c.HTTP.Do(req)
		if err != nil {
			return nil, err
		}
		if resp.StatusCode == http.StatusUnauthorized && attempt == 0 && c.KeyFile != "" {
			resp.Body.Close()
			if c.reloadKey() == nil {
				continue
			}
		}
		return resp, nil
	}
	return nil, errors.New("unreachable")
}

func (c *Client) call(ctx context.Context, method, path string, body []byte, contentType string, out any) error {
	resp, err := c.do(ctx, method, path, body, contentType)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if resp.StatusCode == http.StatusNotFound {
		return ErrNotFound
	}
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("%s %s: %d %s", method, path, resp.StatusCode, strings.TrimSpace(string(data)))
	}
	if out == nil {
		return nil
	}
	if b, ok := out.(*[]byte); ok {
		*b = data
		return nil
	}
	return json.Unmarshal(data, out)
}

// esc keeps a KV key or secret name in ONE path segment (a "/" in a
// key name travels as %2F).
func esc(s string) string { return url.PathEscape(s) }

// escPath escapes each segment of a file path, keeping the slashes.
func escPath(p string) string {
	parts := strings.Split(strings.TrimLeft(p, "/"), "/")
	for i, s := range parts {
		parts[i] = url.PathEscape(s)
	}
	return strings.Join(parts, "/")
}

// KVGet returns the raw string stored at key (ErrNotFound if none).
func (c *Client) KVGet(ctx context.Context, key string) (string, error) {
	var out struct {
		Value string `json:"value"`
	}
	if err := c.call(ctx, http.MethodGet, "/v1/plugins/self/kv/"+esc(key), nil, "", &out); err != nil {
		return "", err
	}
	return out.Value, nil
}

// KVGetJSON decodes the JSON stored at key into v.
func (c *Client) KVGetJSON(ctx context.Context, key string, v any) error {
	raw, err := c.KVGet(ctx, key)
	if err != nil {
		return err
	}
	return json.Unmarshal([]byte(raw), v)
}

// KVPut stores a raw string (≤256KB).
func (c *Client) KVPut(ctx context.Context, key, value string) error {
	return c.call(ctx, http.MethodPut, "/v1/plugins/self/kv/"+esc(key), []byte(value), "text/plain; charset=utf-8", nil)
}

// KVPutJSON JSON-encodes v and stores it.
func (c *Client) KVPutJSON(ctx context.Context, key string, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return c.KVPut(ctx, key, string(b))
}

// KVDelete removes a key.
func (c *Client) KVDelete(ctx context.Context, key string) error {
	return c.call(ctx, http.MethodDelete, "/v1/plugins/self/kv/"+esc(key), nil, "", nil)
}

// FileGet reads a file from the ember's data store.
func (c *Client) FileGet(ctx context.Context, path string) ([]byte, error) {
	var b []byte
	err := c.call(ctx, http.MethodGet, "/v1/plugins/self/files/"+escPath(path), nil, "", &b)
	return b, err
}

// FilePut writes a whole file (≤32MB).
func (c *Client) FilePut(ctx context.Context, path string, data []byte) error {
	return c.call(ctx, http.MethodPut, "/v1/plugins/self/files/"+escPath(path), data, "application/octet-stream", nil)
}

// FileDelete removes a file.
func (c *Client) FileDelete(ctx context.Context, path string) error {
	return c.call(ctx, http.MethodDelete, "/v1/plugins/self/files/"+escPath(path), nil, "", nil)
}

// FileInfo is one entry of the recursive file listing.
type FileInfo struct {
	Path  string `json:"path"`
	Size  int64  `json:"size"`
	Mtime int64  `json:"mtime"`
}

// FileList lists every file in the ember's data store.
func (c *Client) FileList(ctx context.Context) ([]FileInfo, error) {
	var out struct {
		Files []FileInfo `json:"files"`
	}
	err := c.call(ctx, http.MethodGet, "/v1/plugins/self/files", nil, "", &out)
	return out.Files, err
}

// SecretInfo is a secret's name and when it was set (never its value).
type SecretInfo struct {
	Key   string `json:"key"`
	SetAt string `json:"set_at"`
}

// SecretList returns the secret names and how they are sealed
// ("master-key" or "node-key").
func (c *Client) SecretList(ctx context.Context) ([]SecretInfo, string, error) {
	var out struct {
		Secrets []SecretInfo `json:"secrets"`
		Sealing string       `json:"sealing"`
	}
	err := c.call(ctx, http.MethodGet, "/v1/plugins/self/secrets", nil, "", &out)
	return out.Secrets, out.Sealing, err
}

// SecretGet reads a sealed secret back (sidecar key only).
func (c *Client) SecretGet(ctx context.Context, key string) (string, error) {
	var out struct {
		Value string `json:"value"`
	}
	if err := c.call(ctx, http.MethodGet, "/v1/plugins/self/secrets/"+esc(key), nil, "", &out); err != nil {
		return "", err
	}
	return out.Value, nil
}

// SecretPut seals and stores a secret (≤64KB of text).
func (c *Client) SecretPut(ctx context.Context, key, value string) error {
	return c.call(ctx, http.MethodPut, "/v1/plugins/self/secrets/"+esc(key), []byte(value), "text/plain; charset=utf-8", nil)
}

// SecretDelete removes a secret.
func (c *Client) SecretDelete(ctx context.Context, key string) error {
	return c.call(ctx, http.MethodDelete, "/v1/plugins/self/secrets/"+esc(key), nil, "", nil)
}

// OAuthTake collects a waiting sign-in code exactly once (ErrNotFound
// when none is waiting).
func (c *Client) OAuthTake(ctx context.Context) (code, state string, err error) {
	var out struct {
		Code  string `json:"code"`
		State string `json:"state"`
	}
	if err := c.call(ctx, http.MethodPost, "/v1/plugins/self/oauth/take", []byte("{}"), "application/json", &out); err != nil {
		return "", "", err
	}
	return out.Code, out.State, nil
}

// Notification is a bell entry (needs the notify scope).
type Notification struct {
	Severity  string `json:"severity,omitempty"`
	Title     string `json:"title"`
	Body      string `json:"body,omitempty"`
	Link      string `json:"link,omitempty"`
	LinkLabel string `json:"link_label,omitempty"`
	DedupeKey string `json:"dedupe_key,omitempty"`
}

// Notify posts to the user's notification bell.
func (c *Client) Notify(ctx context.Context, n Notification) error {
	b, _ := json.Marshal(n)
	return c.call(ctx, http.MethodPost, "/v1/notifications", b, "application/json", nil)
}

// BusEvent is one hint from the daemon's bus stream.
type BusEvent struct {
	Event string // "bus" or "resync"
	Topic string
}

// Subscribe follows GET /v1/bus/stream and calls fn for each event
// until ctx ends. It reconnects with backoff; a hint carries no data,
// so a missed one only costs latency (callers keep a poll fallback).
func (c *Client) Subscribe(ctx context.Context, fn func(BusEvent)) {
	backoff := time.Second
	stream := &http.Client{} // no timeout: a long-lived stream
	for ctx.Err() == nil {
		started := time.Now()
		err := func() error {
			req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.Base+"/v1/bus/stream", nil)
			if err != nil {
				return err
			}
			req.Header.Set("Authorization", "Bearer "+c.bearer())
			req.Header.Set("Accept", "text/event-stream")
			resp, err := stream.Do(req)
			if err != nil {
				return err
			}
			defer resp.Body.Close()
			if resp.StatusCode == http.StatusUnauthorized {
				_ = c.reloadKey()
			}
			if resp.StatusCode != http.StatusOK {
				return fmt.Errorf("bus stream: %d", resp.StatusCode)
			}
			sc := bufio.NewScanner(resp.Body)
			sc.Buffer(make([]byte, 64*1024), 1<<20)
			var ev BusEvent
			for sc.Scan() {
				line := sc.Text()
				switch {
				case line == "":
					if ev.Event != "" {
						fn(ev)
					}
					ev = BusEvent{}
				case strings.HasPrefix(line, "event:"):
					ev.Event = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
				case strings.HasPrefix(line, "data:"):
					var d struct {
						Topic string `json:"topic"`
					}
					if json.Unmarshal([]byte(strings.TrimSpace(strings.TrimPrefix(line, "data:"))), &d) == nil {
						ev.Topic = d.Topic
					}
				}
			}
			return sc.Err()
		}()
		_ = err
		if time.Since(started) > time.Minute {
			backoff = time.Second
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		if backoff < time.Minute {
			backoff *= 2
		}
	}
}

/* ———— writing to the record (ingest scope) ———— */

// EventTime is the episode time contract: RFC3339 UTC + the local
// offset + how sure we are.
type EventTime struct {
	UTC             string `json:"utc"`
	TZOffsetMinutes int    `json:"tz_offset_minutes"`
	Uncertainty     string `json:"uncertainty"`
}

// Episode is one memory for POST /v1/ingest.
type Episode struct {
	Source    string    `json:"source"`
	SourceRef string    `json:"source_ref"`
	Kind      string    `json:"kind"`
	Title     string    `json:"title"`
	Body      string    `json:"body"`
	EventTime EventTime `json:"event_time"`
	Trust     string    `json:"trust"`
	Scope     *string   `json:"scope"`
	Entities  []any     `json:"entities"`
	Edges     []any     `json:"edges"`
}

// Ingest writes episodes; (source, source_ref) makes a retry a no-op.
func (c *Client) Ingest(ctx context.Context, actor string, eps []Episode) (json.RawMessage, error) {
	for i := range eps {
		if eps[i].Entities == nil {
			eps[i].Entities = []any{}
		}
		if eps[i].Edges == nil {
			eps[i].Edges = []any{}
		}
	}
	b, err := json.Marshal(map[string]any{"actor": actor, "episodes": eps})
	if err != nil {
		return nil, err
	}
	var out json.RawMessage
	err = c.call(ctx, http.MethodPost, "/v1/ingest", b, "application/json", &out)
	return out, err
}

// Vehicle names the car a movement belongs to (an opaque id — never
// the VIN).
type Vehicle struct {
	ID         string         `json:"id"`
	Name       string         `json:"name,omitempty"`
	Descriptor map[string]any `json:"descriptor,omitempty"`
}

// Movement is one span of travel for POST /v1/ingest/movement.
type Movement struct {
	Key             string         `json:"key"`
	Mode            string         `json:"mode"`
	Start           string         `json:"start"`
	End             string         `json:"end"`
	TZOffsetMinutes int            `json:"tz_offset_minutes"`
	StartLat        *float64       `json:"start_lat,omitempty"`
	StartLon        *float64       `json:"start_lon,omitempty"`
	EndLat          *float64       `json:"end_lat,omitempty"`
	EndLon          *float64       `json:"end_lon,omitempty"`
	StartPlace      *string        `json:"start_place"`
	EndPlace        *string        `json:"end_place"`
	DistanceM       *float64       `json:"distance_m,omitempty"`
	Attrs           map[string]any `json:"attrs,omitempty"`
}

// MovementResult is the daemon's answer.
type MovementResult struct {
	Accepted   int    `json:"accepted"`
	Duplicates int    `json:"duplicates"`
	Subject    string `json:"subject"`
}

// IngestMovement posts a vehicle's movements (idempotent by key).
func (c *Client) IngestMovement(ctx context.Context, actor string, v Vehicle, items []Movement) (*MovementResult, error) {
	b, err := json.Marshal(map[string]any{"actor": actor, "vehicle": v, "items": items})
	if err != nil {
		return nil, err
	}
	var out MovementResult
	if err := c.call(ctx, http.MethodPost, "/v1/ingest/movement", b, "application/json", &out); err != nil {
		return nil, err
	}
	return &out, nil
}
