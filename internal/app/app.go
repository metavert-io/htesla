// Package app is the sidecar's brain: it keeps the setup state the page
// shows (KV "status"), takes the page's button presses (KV "cmd"), owns
// the Tesla tokens, and runs the telemetry receiver.
//
// The page never talks to Tesla and never reads a secret: it writes a
// command, this process acts, and the page watches "status" change.
package app

import (
	"context"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/metavert-io/htesla/internal/builder"
	"github.com/metavert-io/htesla/internal/certs"
	"github.com/metavert-io/htesla/internal/hearth"
	"github.com/metavert-io/htesla/internal/keys"
	"github.com/metavert-io/htesla/internal/receiver"
	"github.com/metavert-io/htesla/internal/store"
	"github.com/metavert-io/htesla/internal/tesla"
)

// Names in the ember's stores.
const (
	SecretPrivateKey   = "tesla_private_key"
	SecretClientSecret = "client_secret"
	SecretRefreshToken = "tesla_refresh_token"
	SecretCAKey        = "tls_ca_key"
	SecretServerKey    = "tls_server_key"

	FilePublicKey = "tesla/public-key.pem"
	FileCACert    = "tls/ca.pem"
	FileServer    = "tls/server.pem"

	KVStatus = "status"
	KVCmd    = "cmd"
	KVConfig = "config"
)

// App is one running sidecar.
type App struct {
	H        *hearth.Client
	HTTP     *http.Client // Tesla + the public-key self-check
	TokenURL string       // tesla.TokenURL unless testing
	// FleetBaseOverride replaces the regional base (tests).
	FleetBaseOverride string
	// FetchPublicKey fetches a URL's body (tests replace it).
	FetchPublicKey func(ctx context.Context, url string) (string, error)
	TeslaCAs       *x509.CertPool
	ListenHost     string // "0.0.0.0"
	Version        string
	Logf           func(format string, args ...any)
	// ExtraVINs widens the receiver's allowlist (development replays
	// only — HTESLA_DEV_VINS; never set on a real node).
	ExtraVINs []string
	// BuilderEvery is the record builder's cadence (default 30s).
	BuilderEvery time.Duration

	mu        sync.Mutex
	cfg       Config
	cfgRaw    string
	st        Status
	dirty     int // 0 clean · 1 lazy (≤30s) · 2 soon (≤1s)
	lastWrite time.Time
	done      map[string]bool

	tokens   *tesla.Tokens
	tokensID string
	recv     *receiver.Server
	recvAddr string
	material *certs.Material
	writer   *store.Writer

	jobs        chan job
	pending     map[string]bool
	takeNow     chan struct{}
	pageSeen    time.Time
	signinUntil time.Time
	takeAt      time.Time // next oauth/take poll (a sign-in start pulls it forward)
	notified    map[string]bool

	// records (item 13): the builder's state, guarded by bmu
	bmu      sync.Mutex
	bstate   *builder.State
	bloaded  bool
	attempts map[string]int
	drives   *driveIndex
	summary  string // last summary written (skip identical writes)

	// cost guard (item 14)
	meter    *tesla.Meter
	calls    map[string]*callCount // by month
	alerts   map[string]string     // dedupe key → when sent
	alertsOK bool
	// today's signals as last read from KV (the writer's in-memory
	// count starts at zero after a restart)
	todayStored map[string]int64
}

type job struct {
	name string
	fn   func(ctx context.Context)
}

func (a *App) logf(format string, args ...any) {
	if a.Logf != nil {
		a.Logf(format, args...)
	}
}

func now() string { return time.Now().UTC().Format(time.RFC3339) }

func (a *App) init() {
	a.meter = &tesla.Meter{}
	if a.HTTP == nil {
		a.HTTP = &http.Client{Timeout: 30 * time.Second, Transport: a.meter.Transport(nil)}
	} else if a.HTTP.Transport == nil || a.HTTP.Transport == http.DefaultTransport {
		a.HTTP = &http.Client{Timeout: a.HTTP.Timeout, Transport: a.meter.Transport(a.HTTP.Transport)}
	}
	if a.BuilderEvery == 0 {
		a.BuilderEvery = 30 * time.Second
	}
	a.bstate = builder.New()
	a.attempts = map[string]int{}
	a.calls = map[string]*callCount{}
	a.alerts = map[string]string{}
	if a.TokenURL == "" {
		a.TokenURL = tesla.TokenURL
	}
	if a.ListenHost == "" {
		a.ListenHost = "0.0.0.0"
	}
	if a.FetchPublicKey == nil {
		a.FetchPublicKey = a.fetchURL
	}
	a.done = map[string]bool{}
	a.pending = map[string]bool{}
	a.notified = map[string]bool{}
	a.jobs = make(chan job, 64)
	a.takeNow = make(chan struct{}, 1)
	a.writer = &store.Writer{API: a.H, Logf: a.Logf}
	a.st = Status{Cars: map[string]*CarState{}}
	a.recv = &receiver.Server{
		ClientCAs: a.TeslaCAs,
		AllowVIN:  a.allowVIN,
		Sink:      a.sink,
		Logf:      a.Logf,
	}
}

// markDirty schedules a status write (soon=true: within a second).
func (a *App) markDirty(soon bool) {
	level := 1
	if soon {
		level = 2
	}
	if level > a.dirty {
		a.dirty = level
	}
}

// update mutates the status under the lock and schedules a write.
func (a *App) update(fn func(st *Status)) {
	a.mu.Lock()
	fn(&a.st)
	a.markDirty(true)
	a.mu.Unlock()
}

// Run is the sidecar's life: set up, then loop until ctx ends.
func (a *App) Run(ctx context.Context) error {
	a.init()
	a.loadStatus(ctx)
	a.reloadConfig(ctx)
	a.refreshSecrets(ctx)
	a.ensureKeys(ctx)
	a.ensureReceiver(ctx)
	a.writer.Load(ctx)
	a.writeStatus(ctx)
	// give the page an (empty) command document to read, so its first
	// look isn't a 404; the page is its only other writer
	if _, err := a.H.KVGet(ctx, KVCmd); errors.Is(err, hearth.ErrNotFound) {
		_ = a.H.KVPut(ctx, KVCmd, `{"queue":[]}`)
	}

	work, stopWork := context.WithCancel(context.Background())
	defer stopWork()
	go a.worker(work)
	go a.flushLoop(ctx)
	go a.builderLoop(ctx)
	go a.costLoop(ctx)
	go a.H.Subscribe(ctx, func(ev hearth.BusEvent) {
		// the daemon's hint that a sign-in code landed (or a gap)
		if ev.Event == "resync" || ev.Topic == "plugin.oauth" {
			a.kickTake()
		}
	})

	a.enqueue("check_key", a.checkKey)
	a.enqueue("take", a.take)
	a.mu.Lock()
	signedIn := a.st.SignIn.State == "signed_in"
	streaming := a.st.Streaming.State == "configured"
	a.mu.Unlock()
	if signedIn {
		a.enqueue("vehicles", a.vehicles)
	}
	if streaming {
		a.enqueue("check_streaming", a.checkStreaming)
	}

	var (
		nextCmd, nextConfig, nextSecrets time.Time
		nextVehicles                     = time.Now().Add(6 * time.Hour)
		nextStream                       = time.Now().Add(10 * time.Minute)
		nextKeyCheck                     = time.Now().Add(24 * time.Hour)
		nextRetry                        = time.Now().Add(time.Minute)
	)
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			a.shutdown()
			return nil
		case <-a.takeNow:
			a.enqueue("take", a.take)
		case t := <-tick.C:
			a.mu.Lock()
			fastCmd := t.Sub(a.pageSeen) < 2*time.Minute
			signedIn := a.st.SignIn.State == "signed_in"
			streamState := a.st.Streaming.State
			allSynced := len(a.st.Streaming.PerVIN) > 0
			for _, v := range a.st.Streaming.PerVIN {
				allSynced = allSynced && v.Synced
			}
			keysBad := a.st.Keys.State != "ready" && a.st.Keys.State != "lost"
			recvDown := !a.st.Receiver.Listening
			a.mu.Unlock()

			if !t.Before(nextCmd) {
				a.pollCommands(ctx)
				a.mu.Lock()
				fastCmd = t.Sub(a.pageSeen) < 2*time.Minute // the poll may just have seen the page
				a.mu.Unlock()
				if fastCmd {
					nextCmd = t.Add(3 * time.Second)
				} else {
					nextCmd = t.Add(10 * time.Second)
				}
			}
			a.mu.Lock()
			takeDue := !t.Before(a.takeAt)
			if takeDue {
				// the bus hint is the fast path; this poll is the fallback.
				// Re-read the window: a sign-in may have started this tick.
				if t.Before(a.signinUntil) {
					a.takeAt = t.Add(3 * time.Second)
				} else {
					a.takeAt = t.Add(60 * time.Second)
				}
			}
			a.mu.Unlock()
			if takeDue {
				a.enqueue("take", a.take)
			}
			if !t.Before(nextConfig) {
				if a.reloadConfig(ctx) {
					a.ensureReceiver(ctx)
					a.enqueue("check_key", a.checkKey) // the public address may be new
				}
				nextConfig = t.Add(30 * time.Second)
			}
			if !t.Before(nextSecrets) {
				a.enqueue("secrets", a.refreshSecrets)
				nextSecrets = t.Add(60 * time.Second)
			}
			if signedIn && !t.Before(nextVehicles) {
				a.enqueue("vehicles", a.vehicles)
				nextVehicles = t.Add(6 * time.Hour)
			}
			if streamState == "configured" && !t.Before(nextStream) {
				a.enqueue("check_streaming", a.checkStreaming)
				if allSynced {
					nextStream = t.Add(6 * time.Hour)
				} else {
					nextStream = t.Add(10 * time.Minute)
				}
			}
			if !t.Before(nextKeyCheck) {
				a.enqueue("check_key", a.checkKey)
				nextKeyCheck = t.Add(24 * time.Hour)
			}
			if !t.Before(nextRetry) {
				if keysBad {
					a.ensureKeys(ctx)
				}
				if recvDown {
					a.ensureReceiver(ctx)
				}
				nextRetry = t.Add(time.Minute)
			}
			a.maybeWriteStatus(ctx, t)
		}
	}
}

func (a *App) shutdown() {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_ = a.recv.Close()
	if err := a.writer.Flush(ctx, true, true); err != nil {
		a.logf("final flush: %v", err)
	}
	a.mu.Lock()
	for _, c := range a.st.Cars {
		if c.Online {
			c.Online = false
			c.DisconnectedAt = now()
		}
	}
	a.st.Receiver.Listening = false
	a.dirty = 2
	a.mu.Unlock()
	a.writeStatus(ctx)
}

func (a *App) enqueue(name string, fn func(ctx context.Context)) {
	a.mu.Lock()
	if a.pending[name] {
		a.mu.Unlock()
		return
	}
	a.pending[name] = true
	a.mu.Unlock()
	select {
	case a.jobs <- job{name: name, fn: fn}:
	default:
		a.mu.Lock()
		delete(a.pending, name)
		a.mu.Unlock()
	}
}

func (a *App) kickTake() {
	select {
	case a.takeNow <- struct{}{}:
	default:
	}
}

func (a *App) worker(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case j := <-a.jobs:
			a.mu.Lock()
			delete(a.pending, j.name)
			a.mu.Unlock()
			jctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
			j.fn(jctx)
			cancel()
		}
	}
}

func (a *App) flushLoop(ctx context.Context) {
	flush := time.NewTicker(10 * time.Second)
	defer flush.Stop()
	lastLatest := time.Time{}
	nextPrune := time.Now().Add(time.Minute)
	for {
		select {
		case <-ctx.Done():
			return
		case t := <-flush.C:
			latestToo := t.Sub(lastLatest) >= 30*time.Second
			if err := a.writer.Flush(ctx, false, latestToo); err != nil {
				a.logf("saving telemetry: %v", err)
			} else if latestToo {
				lastLatest = t
			}
			if !t.Before(nextPrune) {
				if n, err := a.writer.Prune(ctx); err != nil {
					a.logf("pruning old telemetry: %v", err)
				} else if n > 0 {
					a.logf("pruned %d telemetry chunks older than %d days", n, store.KeepDays)
				}
				nextPrune = t.Add(time.Hour)
			}
		}
	}
}

/* ————— status persistence ————— */

func (a *App) loadStatus(ctx context.Context) {
	var prev Status
	if err := a.H.KVGetJSON(ctx, KVStatus, &prev); err != nil {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.st.Keys.CreatedAt = prev.Keys.CreatedAt
	a.st.Register = prev.Register
	if a.st.Register.State == "working" {
		a.st.Register.State = "idle"
	}
	a.st.SignIn = prev.SignIn
	if a.st.SignIn.State == "waiting" {
		a.st.SignIn.State = "signed_out" // refreshSecrets corrects it
	}
	a.st.Vehicles = prev.Vehicles
	a.st.VehAt = prev.VehAt
	a.st.Streaming = prev.Streaming
	if a.st.Streaming.State == "working" {
		a.st.Streaming.State = "off"
	}
	if prev.Cars != nil {
		a.st.Cars = prev.Cars
		for _, c := range a.st.Cars {
			c.Online = false // no socket survives a restart
		}
	}
	for _, id := range prev.Done {
		a.done[id] = true
	}
	a.st.Done = prev.Done
}

func (a *App) maybeWriteStatus(ctx context.Context, t time.Time) {
	a.mu.Lock()
	due := (a.dirty == 2 && t.Sub(a.lastWrite) >= time.Second) ||
		(a.dirty == 1 && t.Sub(a.lastWrite) >= 30*time.Second)
	a.mu.Unlock()
	if due {
		a.writeStatus(ctx)
	}
}

func (a *App) writeStatus(ctx context.Context) {
	day := time.Now().UTC().Format("2006-01-02")
	u := a.writer.UsageFor(day)
	stats := a.recv.Snapshot()
	a.mu.Lock()
	a.st.Version = a.Version
	a.st.UpdatedAt = now()
	if a.st.StartedAt == "" {
		a.st.StartedAt = a.st.UpdatedAt
	}
	a.st.Receiver.Stats = stats
	sig := u.Signals
	if v := a.todayStored[day]; v > sig {
		sig = v
	}
	a.st.Today = TodayUsage{Day: day, Signals: sig, EstCostUSD: float64(sig) / store.SignalsPerDollar}
	b, err := json.Marshal(a.st)
	a.dirty = 0
	a.lastWrite = time.Now()
	a.mu.Unlock()
	if err != nil {
		a.logf("encoding status: %v", err)
		return
	}
	if err := a.H.KVPut(ctx, KVStatus, string(b)); err != nil {
		a.logf("saving status: %v", err)
		a.mu.Lock()
		a.markDirty(true)
		a.mu.Unlock()
	}
}

/* ————— configuration ————— */

// reloadConfig re-reads KV "config"; true when it changed.
func (a *App) reloadConfig(ctx context.Context) bool {
	raw, err := a.H.KVGet(ctx, KVConfig)
	if err != nil && !errors.Is(err, hearth.ErrNotFound) {
		return false
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if raw == a.cfgRaw && a.st.Config.Scopes != "" {
		return false
	}
	a.cfgRaw = raw
	a.cfg = ParseConfig(raw)
	c := a.cfg
	a.st.Config = ConfigStatus{
		PublicOrigin:  c.PublicOrigin,
		ClientID:      c.ClientID,
		Region:        c.Region,
		TelemetryPort: c.TelemetryPort,
		Domain:        c.Domain(),
		AllowedOrigin: c.PublicOrigin,
		RedirectURI:   c.RedirectURI(),
		PublicKeyURL:  c.PublicKeyURL(),
		FleetBase:     c.FleetBase(),
		Scopes:        tesla.Scopes,
		Timezone:      c.Location().String(),
		Problems:      c.Problems(),
	}
	if a.st.Config.Problems == nil {
		a.st.Config.Problems = []string{}
	}
	a.markDirty(true)
	return true
}

func (a *App) config() Config {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.cfg
}

func (a *App) fleetBase(c Config) string {
	if a.FleetBaseOverride != "" {
		return a.FleetBaseOverride
	}
	return c.FleetBase()
}

/* ————— secrets: sealing, client secret, sign-in presence ————— */

func (a *App) refreshSecrets(ctx context.Context) {
	list, sealing, err := a.H.SecretList(ctx)
	if err != nil {
		a.logf("listing secrets: %v", err)
		return
	}
	has := map[string]string{}
	for _, s := range list {
		has[s.Key] = s.SetAt
	}
	a.update(func(st *Status) {
		st.Sealing = sealing
		at, ok := has[SecretClientSecret]
		st.App.ClientSecretSet = ok
		st.App.ClientSecretAt = at
		_, signed := has[SecretRefreshToken]
		switch {
		case signed && (st.SignIn.State == "" || st.SignIn.State == "signed_out"):
			st.SignIn.State = "signed_in"
		case !signed && st.SignIn.State == "signed_in":
			st.SignIn.State = "signed_out"
		case st.SignIn.State == "":
			st.SignIn.State = "signed_out"
		}
	})
}

/* ————— step 1: the application key ————— */

// ensureKeys makes the key pair ONCE. Losing the private key breaks
// pairing with every car, so it is never regenerated on its own: only
// when no key exists at all, or on the owner's explicit "new key".
func (a *App) ensureKeys(ctx context.Context) {
	priv, err := a.H.SecretGet(ctx, SecretPrivateKey)
	switch {
	case err == nil:
		pub, perr := keys.PublicFromPrivate(priv)
		if perr != nil {
			a.update(func(st *Status) {
				st.Keys.State = "error"
				st.Keys.Error = "the stored private key didn't parse: " + perr.Error()
			})
			return
		}
		cur, ferr := a.H.FileGet(ctx, FilePublicKey)
		if ferr != nil || !keys.SamePublicKey(string(cur), pub) {
			if errors.Is(ferr, hearth.ErrNotFound) || ferr == nil {
				if err := a.H.FilePut(ctx, FilePublicKey, []byte(pub)); err != nil {
					a.update(func(st *Status) { st.Keys.State = "error"; st.Keys.Error = "publishing the public key: " + err.Error() })
					return
				}
			} else {
				a.update(func(st *Status) {
					st.Keys.State = "error"
					st.Keys.Error = "reading the public key file: " + ferr.Error()
				})
				return
			}
		}
		a.update(func(st *Status) {
			st.Keys.State = "ready"
			st.Keys.Error = ""
			st.Keys.PublicPEM = pub
			if st.Keys.CreatedAt == "" {
				st.Keys.CreatedAt = now()
			}
		})
	case errors.Is(err, hearth.ErrNotFound):
		_, ferr := a.H.FileGet(ctx, FilePublicKey)
		if ferr == nil {
			// a public key without its private half: the secret was lost
			// (secrets don't travel in exports). Say so; don't silently
			// replace a key the cars may be paired with.
			a.update(func(st *Status) {
				st.Keys.State = "lost"
				st.Keys.Error = "The private key is missing (secrets aren't carried by exports or to other nodes). Make a new key, then register and pair your car again."
			})
			return
		}
		if !errors.Is(ferr, hearth.ErrNotFound) {
			a.update(func(st *Status) {
				st.Keys.State = "error"
				st.Keys.Error = "reading the public key file: " + ferr.Error()
			})
			return
		}
		a.generateKeys(ctx)
	default:
		// the daemon couldn't answer — try again later, never regenerate
		a.update(func(st *Status) { st.Keys.State = "error"; st.Keys.Error = "reading the private key: " + err.Error() })
	}
}

func (a *App) generateKeys(ctx context.Context) bool {
	priv, pub, err := keys.Generate(nil)
	if err != nil {
		a.update(func(st *Status) { st.Keys.State = "error"; st.Keys.Error = err.Error() })
		return false
	}
	// private half sealed FIRST: a public key with no private key is the
	// state we refuse to create
	if err := a.H.SecretPut(ctx, SecretPrivateKey, priv); err != nil {
		a.update(func(st *Status) { st.Keys.State = "error"; st.Keys.Error = "sealing the private key: " + err.Error() })
		return false
	}
	if err := a.H.FilePut(ctx, FilePublicKey, []byte(pub)); err != nil {
		a.update(func(st *Status) { st.Keys.State = "error"; st.Keys.Error = "publishing the public key: " + err.Error() })
		return false
	}
	a.logf("made a new Tesla application key (P-256)")
	a.update(func(st *Status) {
		st.Keys = KeyStatus{State: "ready", CreatedAt: now(), PublicPEM: pub}
	})
	return true
}

// newKey is the owner's explicit replacement of a LOST key.
func (a *App) newKey(ctx context.Context) {
	a.mu.Lock()
	state := a.st.Keys.State
	a.mu.Unlock()
	if state != "lost" {
		return // a working key is never replaced from the page
	}
	if !a.generateKeys(ctx) {
		return
	}
	a.update(func(st *Status) {
		st.Register = StepStatus{State: "idle"}
		if st.Streaming.State == "configured" {
			st.Streaming.Stale = "The application key changed: register again, re-pair your car, then start streaming again."
		}
	})
	a.checkKey(ctx)
}

func (a *App) fetchURL(ctx context.Context, url string) (string, error) {
	cctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(cctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Cache-Control", "no-cache")
	resp, err := a.HTTP.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("it answered %d", resp.StatusCode)
	}
	return string(body), nil
}

// checkKey asks the public address for our key, the way Tesla will.
func (a *App) checkKey(ctx context.Context) {
	c := a.config()
	a.mu.Lock()
	pub := a.st.Keys.PublicPEM
	a.mu.Unlock()
	if pub == "" || c.PublicKeyURL() == "" {
		return
	}
	body, err := a.FetchPublicKey(ctx, c.PublicKeyURL())
	served := err == nil && keys.SamePublicKey(body, pub)
	note := ""
	switch {
	case err != nil:
		note = "Couldn't fetch it: " + err.Error()
	case !served:
		note = "That address answers, but not with this ember's key."
	}
	a.update(func(st *Status) {
		st.Keys.Served = &served
		st.Keys.CheckedAt = now()
		st.Keys.CheckNote = note
	})
}

/* ————— step 3: register the domain with Tesla ————— */

func (a *App) clientSecret(ctx context.Context) (string, error) {
	s, err := a.H.SecretGet(ctx, SecretClientSecret)
	if errors.Is(err, hearth.ErrNotFound) {
		return "", errors.New("save your Tesla app's client secret first (step 2)")
	}
	return strings.TrimSpace(s), err
}

func (a *App) register(ctx context.Context) {
	c := a.config()
	fail := func(msg string) {
		a.update(func(st *Status) { st.Register.State = "error"; st.Register.Error = msg; st.Register.At = now() })
	}
	if p := c.Problems(); len(p) > 0 {
		fail(p[0])
		return
	}
	a.mu.Lock()
	keyReady := a.st.Keys.State == "ready"
	a.mu.Unlock()
	if !keyReady {
		fail("The application key isn't ready yet (step 1).")
		return
	}
	a.update(func(st *Status) { st.Register.State = "working"; st.Register.Error = "" })
	// Tesla fetches the key from the domain during registration — prove
	// it's there before asking
	a.checkKey(ctx)
	a.mu.Lock()
	served := a.st.Keys.Served != nil && *a.st.Keys.Served
	note := a.st.Keys.CheckNote
	a.mu.Unlock()
	if !served {
		fail(fmt.Sprintf("Tesla must be able to read your public key at %s first. %s", c.PublicKeyURL(), note))
		return
	}
	secret, err := a.clientSecret(ctx)
	if err != nil {
		fail(err.Error())
		return
	}
	base := a.fleetBase(c)
	tok, err := tesla.PartnerToken(ctx, a.HTTP, a.TokenURL, c.ClientID, secret, base)
	if err != nil {
		fail("Tesla refused the app's credentials — check the Client ID and secret. (" + err.Error() + ")")
		return
	}
	f := &tesla.Fleet{HTTP: a.HTTP, Base: base, Token: func(context.Context) (string, error) { return tok, nil }}
	if _, err := f.RegisterPartner(ctx, c.Domain()); err != nil {
		fail("Registration failed: " + err.Error())
		return
	}
	a.logf("registered %s with Tesla (%s)", c.Domain(), c.Region)
	a.update(func(st *Status) {
		st.Register = StepStatus{State: "done", At: now(), Domain: c.Domain()}
	})
}

/* ————— step 4: sign in ————— */

type secretStore struct{ h *hearth.Client }

func (s secretStore) LoadRefresh(ctx context.Context) (string, error) {
	v, err := s.h.SecretGet(ctx, SecretRefreshToken)
	if errors.Is(err, hearth.ErrNotFound) {
		return "", tesla.ErrNoRefreshToken
	}
	return strings.TrimSpace(v), err
}

func (s secretStore) SaveRefresh(ctx context.Context, token string) error {
	return s.h.SecretPut(ctx, SecretRefreshToken, token)
}

func (a *App) tokenSet() *tesla.Tokens {
	c := a.config()
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.tokens == nil || a.tokensID != c.ClientID {
		a.tokens = &tesla.Tokens{HTTP: a.HTTP, TokenURL: a.TokenURL, ClientID: c.ClientID, Store: secretStore{a.H}}
		a.tokensID = c.ClientID
	}
	return a.tokens
}

func (a *App) take(ctx context.Context) {
	code, _, err := a.H.OAuthTake(ctx)
	if errors.Is(err, hearth.ErrNotFound) {
		return
	}
	if err != nil {
		a.logf("collecting the sign-in code: %v", err)
		return
	}
	a.mu.Lock()
	a.signinUntil = time.Time{}
	a.mu.Unlock()
	c := a.config()
	fail := func(msg string) {
		a.update(func(st *Status) { st.SignIn.State = "error"; st.SignIn.Error = msg; st.SignIn.At = now() })
	}
	secret, err := a.clientSecret(ctx)
	if err != nil {
		fail(err.Error())
		return
	}
	tr, err := tesla.ExchangeCode(ctx, a.HTTP, a.TokenURL, c.ClientID, secret, code, a.fleetBase(c), c.RedirectURI())
	if err != nil {
		fail("Tesla didn't accept the sign-in: " + err.Error())
		return
	}
	if err := a.tokenSet().Adopt(ctx, tr); err != nil {
		fail(err.Error())
		return
	}
	a.logf("signed in to Tesla")
	a.update(func(st *Status) { st.SignIn = SignInStatus{State: "signed_in", At: now()} })
	// which region does this account live in?
	_ = a.withUser(ctx, func(f *tesla.Fleet) error {
		r, err := f.UserRegion(ctx)
		if err != nil {
			return err
		}
		a.update(func(st *Status) {
			st.SignIn.AccountRegion = r.Region
			st.SignIn.AccountBase = r.FleetAPIBaseURL
			if r.FleetAPIBaseURL != "" && a.FleetBaseOverride == "" &&
				strings.TrimRight(r.FleetAPIBaseURL, "/") != strings.TrimRight(c.FleetBase(), "/") {
				st.SignIn.Error = fmt.Sprintf("Your Tesla account lives in region %q, but region is set to %q. Change region on the Integrations card, then register again.", r.Region, c.Region)
			}
		})
		return nil
	})
	a.vehicles(ctx)
}

func (a *App) signOut(ctx context.Context) {
	if err := a.H.SecretDelete(ctx, SecretRefreshToken); err != nil && !errors.Is(err, hearth.ErrNotFound) {
		a.logf("forgetting the refresh token: %v", err)
	}
	a.mu.Lock()
	a.tokens = nil
	a.mu.Unlock()
	a.update(func(st *Status) { st.SignIn = SignInStatus{State: "signed_out", At: now()} })
}

// withUser runs fn with a Fleet client on the owner's token; a 401
// drops the access token and retries once; a spent refresh token marks
// "sign in again".
func (a *App) withUser(ctx context.Context, fn func(f *tesla.Fleet) error) error {
	c := a.config()
	tokens := a.tokenSet()
	f := &tesla.Fleet{HTTP: a.HTTP, Base: a.fleetBase(c), Token: tokens.Access}
	err := fn(f)
	if tesla.IsUnauthorized(err) {
		tokens.Forget()
		err = fn(f)
	}
	if err != nil && tesla.NeedsSignIn(err) {
		a.signInAgain(ctx, err)
	}
	return err
}

func (a *App) signInAgain(ctx context.Context, cause error) {
	state := "sign_in_again"
	if errors.Is(cause, tesla.ErrNoRefreshToken) {
		state = "signed_out"
	}
	a.update(func(st *Status) {
		st.SignIn.State = state
		st.SignIn.Error = cause.Error()
		st.SignIn.At = now()
	})
	if state == "sign_in_again" {
		a.notify(ctx, "signin-again:"+time.Now().UTC().Format("2006-01-02"), hearth.Notification{
			Severity: "warn",
			Title:    "Tesla needs you to sign in again",
			Body:     "Your Tesla sign-in ran out (they last three months, or were revoked). Open the Tesla page and sign in again to keep your car's data flowing.",
			Link:     "#/plugin/htesla/index.html", LinkLabel: "Open Tesla setup",
		})
	}
}

// notifyExact posts with the dedupe key as given (once per process).
func (a *App) notifyExact(ctx context.Context, key string, n hearth.Notification) error {
	n.DedupeKey = key
	return a.H.Notify(ctx, n)
}

func (a *App) notify(ctx context.Context, dedupe string, n hearth.Notification) {
	a.mu.Lock()
	seen := a.notified[dedupe]
	a.notified[dedupe] = true
	a.mu.Unlock()
	if seen {
		return
	}
	n.DedupeKey = "htesla:" + dedupe
	if err := a.H.Notify(ctx, n); err != nil {
		a.logf("notification: %v", err)
	}
}

/* ————— step 5: the cars ————— */

func last4(vin string) string {
	if len(vin) <= 4 {
		return vin
	}
	return vin[len(vin)-4:]
}

func (a *App) vehicles(ctx context.Context) {
	c := a.config()
	err := a.withUser(ctx, func(f *tesla.Fleet) error {
		list, err := f.Vehicles(ctx)
		if err != nil {
			return err
		}
		vins := make([]string, 0, len(list))
		for _, v := range list {
			vins = append(vins, v.VIN)
		}
		paired := map[string]bool{}
		firmware := map[string]string{}
		statusErr := ""
		if len(vins) > 0 {
			fs, err := f.FleetStatus(ctx, vins)
			if err != nil {
				statusErr = "Couldn't check pairing: " + err.Error()
			} else {
				for _, v := range fs.KeyPairedVINs {
					paired[v] = true
				}
				for vin, raw := range fs.VehicleInfo {
					var info struct {
						Firmware string `json:"firmware_version"`
					}
					if json.Unmarshal(raw, &info) == nil {
						firmware[vin] = info.Firmware
					}
				}
			}
		}
		out := make([]VehicleState, 0, len(list))
		for _, v := range list {
			vs := VehicleState{
				VIN: v.VIN, Last4: last4(v.VIN), Name: v.DisplayName, State: v.State,
				Firmware: firmware[v.VIN],
				PairURL:  "https://tesla.com/_ak/" + c.Domain() + "?vin=" + v.VIN,
			}
			if statusErr == "" {
				p := paired[v.VIN]
				vs.Paired = &p
			}
			out = append(out, vs)
		}
		sort.Slice(out, func(i, j int) bool { return out[i].VIN < out[j].VIN })
		a.update(func(st *Status) {
			st.Vehicles = out
			st.VehAt = now()
			st.VehError = statusErr
		})
		return nil
	})
	if err != nil {
		a.update(func(st *Status) { st.VehError = "Couldn't list your cars: " + err.Error() })
	}
}

func (a *App) allowVIN(vin string) bool {
	for _, v := range a.ExtraVINs {
		if strings.EqualFold(v, vin) {
			return true
		}
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, v := range a.st.Vehicles {
		if v.VIN == vin {
			return true
		}
	}
	return false
}

/* ————— step 6: streaming ————— */

func (a *App) startStreaming(ctx context.Context) {
	c := a.config()
	fail := func(msg string) {
		a.update(func(st *Status) { st.Streaming.State = "error"; st.Streaming.Error = msg; st.Streaming.At = now() })
	}
	if p := c.Problems(); len(p) > 0 {
		fail(p[0])
		return
	}
	a.mu.Lock()
	material := a.material
	listening := a.st.Receiver.Listening
	var vins []string
	for _, v := range a.st.Vehicles {
		if v.Paired != nil && *v.Paired {
			vins = append(vins, v.VIN)
		}
	}
	signed := a.st.SignIn.State == "signed_in"
	a.mu.Unlock()
	switch {
	case !signed:
		fail("Sign in to Tesla first (step 4).")
		return
	case len(vins) == 0:
		fail("Pair this app's key with your car first (step 5), then check again.")
		return
	case material == nil || !listening:
		fail("The telemetry receiver isn't running yet — see its status below.")
		return
	}
	a.update(func(st *Status) { st.Streaming.State = "working"; st.Streaming.Error = "" })
	priv, err := a.H.SecretGet(ctx, SecretPrivateKey)
	if err != nil {
		fail("Couldn't read the application key: " + err.Error())
		return
	}
	ck, err := keys.CommandKey(priv)
	if err != nil {
		fail(err.Error())
		return
	}
	cfg := tesla.NewTelemetryConfig(c.Domain(), c.TelemetryPort, material.TrustPEM)
	jws, err := tesla.SignTelemetryConfig(ck, cfg)
	if err != nil {
		fail("Signing the configuration failed: " + err.Error())
		return
	}
	var res *tesla.ConfigResult
	err = a.withUser(ctx, func(f *tesla.Fleet) error {
		var err error
		res, err = f.PostSignedTelemetryConfig(ctx, vins, jws)
		return err
	})
	if err != nil {
		fail("Tesla didn't take the streaming configuration: " + err.Error())
		return
	}
	a.logf("telemetry configuration sent: %d updated", res.UpdatedVehicles)
	a.update(func(st *Status) {
		st.Streaming = StreamStatus{
			State: "configured", At: now(), VINs: vins,
			Updated: res.UpdatedVehicles, Skipped: res.SkippedVehicles,
			Hostname: cfg.Hostname, Port: cfg.Port,
			CAFingerprint: certs.Fingerprint(material.TrustPEM),
			Fields:        tesla.FieldNames(),
			PerVIN:        map[string]*VINStream{},
		}
	})
	a.checkStreaming(ctx)
}

func (a *App) checkStreaming(ctx context.Context) {
	a.mu.Lock()
	vins := append([]string(nil), a.st.Streaming.VINs...)
	a.mu.Unlock()
	if len(vins) == 0 {
		return
	}
	per := map[string]*VINStream{}
	_ = a.withUser(ctx, func(f *tesla.Fleet) error {
		for _, vin := range vins {
			vs := &VINStream{}
			cs, err := f.TelemetryConfig(ctx, vin)
			if err != nil {
				vs.Error = err.Error()
				if tesla.IsUnauthorized(err) {
					return err
				}
			} else {
				vs.Synced, vs.LimitReached, vs.KeyPaired = cs.Synced, cs.LimitReached, cs.KeyPaired
			}
			if errs, err := f.TelemetryErrors(ctx, vin); err == nil {
				for i, e := range errs {
					if i >= 5 {
						break
					}
					vs.Errors = append(vs.Errors, strings.TrimSpace(e.Name+": "+e.Error))
				}
			}
			per[vin] = vs
		}
		return nil
	})
	a.update(func(st *Status) {
		st.Streaming.PerVIN = per
		st.Streaming.CheckedAt = now()
	})
}

/* ————— the receiver ————— */

// ensureReceiver makes sure the server identity exists for the current
// host and that we listen on the configured port.
func (a *App) ensureReceiver(ctx context.Context) {
	c := a.config()
	host := c.Domain()
	if host == "" || c.TelemetryPort < 1 || c.TelemetryPort > 65535 {
		a.update(func(st *Status) { st.Receiver.Error = "Waiting for public_origin and telemetry_port." })
		return
	}
	m, err := a.ensureTLS(ctx, host)
	if err != nil {
		a.update(func(st *Status) { st.Receiver.Error = "Server certificate: " + err.Error() })
		return
	}
	a.recv.SetCertificate(&m.Cert)
	addr := fmt.Sprintf("%s:%d", a.ListenHost, c.TelemetryPort)
	a.mu.Lock()
	a.material = m
	cur := a.recvAddr
	a.mu.Unlock()
	if cur != addr || a.recv.Addr() == "" {
		_ = a.recv.Close()
		if err := a.recv.Listen(addr); err != nil {
			a.update(func(st *Status) {
				st.Receiver.Listening = false
				st.Receiver.Error = fmt.Sprintf("Couldn't listen on %s: %v", addr, err)
			})
			return
		}
		a.mu.Lock()
		a.recvAddr = addr
		a.mu.Unlock()
		a.logf("telemetry receiver listening on %s (mTLS, Tesla client certificates)", addr)
	}
	fp := certs.Fingerprint(m.TrustPEM)
	a.update(func(st *Status) {
		st.Receiver.Listening = true
		st.Receiver.Addr = addr
		st.Receiver.Error = ""
		st.Receiver.CertHost = m.Host
		st.Receiver.CertExpires = m.NotAfter.UTC().Format(time.RFC3339)
		st.Receiver.CertSource = m.SourceName
		st.Receiver.CAFingerprint = fp
		if st.Streaming.State == "configured" {
			switch {
			case st.Streaming.CAFingerprint != "" && st.Streaming.CAFingerprint != fp:
				st.Streaming.Stale = "The receiver's certificate authority changed since streaming was set up — start streaming again."
			case st.Streaming.Hostname != host || st.Streaming.Port != c.TelemetryPort:
				st.Streaming.Stale = "public_origin or telemetry_port changed since streaming was set up — start streaming again."
			default:
				st.Streaming.Stale = ""
			}
		}
	})
}

// ensureTLS loads (or makes, once) the private CA and the server cert
// for host. Only a definite "not found" ever leads to generating.
func (a *App) ensureTLS(ctx context.Context, host string) (*certs.Material, error) {
	caCert, errCert := a.H.FileGet(ctx, FileCACert)
	caKey, errKey := a.H.SecretGet(ctx, SecretCAKey)
	for _, e := range []error{errCert, errKey} {
		if e != nil && !errors.Is(e, hearth.ErrNotFound) {
			return nil, e
		}
	}
	if errCert != nil || errKey != nil {
		// one half missing (or both): a fresh CA. The car must then get
		// the new CA — ensureReceiver flags the config as stale.
		cp, kp, err := certs.NewCA("htesla receiver CA (" + host + ")")
		if err != nil {
			return nil, err
		}
		if err := a.H.SecretPut(ctx, SecretCAKey, kp); err != nil {
			return nil, err
		}
		if err := a.H.FilePut(ctx, FileCACert, []byte(cp)); err != nil {
			return nil, err
		}
		caCert, caKey = []byte(cp), kp
		_ = a.H.FileDelete(ctx, FileServer) // re-issue under the new CA
		a.logf("made the receiver's private CA")
	}
	chain, errChain := a.H.FileGet(ctx, FileServer)
	srvKey, errSrv := a.H.SecretGet(ctx, SecretServerKey)
	for _, e := range []error{errChain, errSrv} {
		if e != nil && !errors.Is(e, hearth.ErrNotFound) {
			return nil, e
		}
	}
	reissue := errChain != nil || errSrv != nil || certs.NeedsReissue(string(chain), host, time.Now()) ||
		!strings.Contains(string(chain), strings.TrimSpace(string(caCert)))
	if !reissue {
		m, err := certs.PrivateCA{CACertPEM: string(caCert), ServerKeyPEM: srvKey, ServerChain: string(chain)}.Material()
		if err == nil {
			return m, nil
		}
	}
	newChain, newKey, err := certs.IssueServer(string(caCert), caKey, host)
	if err != nil {
		return nil, err
	}
	if err := a.H.SecretPut(ctx, SecretServerKey, newKey); err != nil {
		return nil, err
	}
	if err := a.H.FilePut(ctx, FileServer, []byte(newChain)); err != nil {
		return nil, err
	}
	a.logf("issued the receiver's server certificate for %s", host)
	return certs.PrivateCA{CACertPEM: string(caCert), ServerKeyPEM: newKey, ServerChain: newChain}.Material()
}

// sink takes each streamed event (socket goroutine — memory only).
func (a *App) sink(ev receiver.Event) {
	a.writer.Add(ev)
	at := ev.ReceivedAt.UTC().Format(time.RFC3339)
	first := false
	a.mu.Lock()
	car := a.st.Cars[ev.VIN]
	if car == nil {
		car = &CarState{}
		a.st.Cars[ev.VIN] = car
		first = true
	}
	car.LastSeen = at
	if ev.Topic == "connectivity" {
		car.Online = ev.Status == "connected"
		if car.Online {
			car.ConnectedAt = at
			car.Network = ev.Network
			car.ClientVersion = ev.ClientVersion
		} else {
			car.DisconnectedAt = at
		}
		a.markDirty(true)
	} else {
		a.markDirty(false)
	}
	a.mu.Unlock()
	if first {
		go a.notify(context.Background(), "first-stream:"+ev.VIN, hearth.Notification{
			Title: "Your Tesla is streaming to Hearthloom",
			Body:  fmt.Sprintf("The car ending %s connected to this node's telemetry receiver.", last4(ev.VIN)),
			Link:  "#/plugin/htesla/index.html", LinkLabel: "Open Tesla",
		})
	}
}

/* ————— commands from the page ————— */

// Command is one button press queued by the page in KV "cmd".
type Command struct {
	ID     string            `json:"id"`
	Action string            `json:"action"`
	At     string            `json:"at"`
	Args   map[string]string `json:"args,omitempty"`
}

// CmdDoc is the page's KV "cmd" document.
type CmdDoc struct {
	Seen  string    `json:"seen"`
	Queue []Command `json:"queue"`
}

func (a *App) pollCommands(ctx context.Context) {
	var doc CmdDoc
	if err := a.H.KVGetJSON(ctx, KVCmd, &doc); err != nil {
		return
	}
	if t, err := time.Parse(time.RFC3339, doc.Seen); err == nil {
		a.mu.Lock()
		if t.After(a.pageSeen) {
			a.pageSeen = t
		}
		a.mu.Unlock()
	}
	for _, c := range doc.Queue {
		a.handle(ctx, c)
	}
}

func (a *App) handle(ctx context.Context, c Command) {
	if c.ID == "" {
		return
	}
	a.mu.Lock()
	if a.done[c.ID] {
		a.mu.Unlock()
		return
	}
	a.done[c.ID] = true
	a.st.Done = append(a.st.Done, c.ID)
	if len(a.st.Done) > 50 {
		a.st.Done = a.st.Done[len(a.st.Done)-50:]
	}
	a.markDirty(true)
	a.mu.Unlock()
	if at, err := time.Parse(time.RFC3339, c.At); err != nil || time.Since(at) > 15*time.Minute {
		return // stale: never replay an old press
	}
	switch c.Action {
	case "check_key":
		a.enqueue("check_key", a.checkKey)
	case "register":
		a.enqueue("register", a.register)
	case "signin_started":
		a.mu.Lock()
		a.signinUntil = time.Now().Add(15 * time.Minute)
		a.takeAt = time.Time{} // poll for the code right away, then every 3s
		a.mu.Unlock()
		a.update(func(st *Status) {
			st.SignIn.State = "waiting"
			st.SignIn.Error = ""
			st.SignIn.WaitingSince = now()
		})
	case "signin_cancel":
		a.mu.Lock()
		a.signinUntil = time.Time{}
		a.mu.Unlock()
		a.refreshSecrets(ctx)
		a.update(func(st *Status) {
			if st.SignIn.State == "waiting" {
				st.SignIn.State = "signed_out"
			}
		})
	case "sign_out":
		a.enqueue("sign_out", a.signOut)
	case "refresh_vehicles":
		a.enqueue("vehicles", a.vehicles)
	case "start_streaming":
		a.enqueue("start_streaming", a.startStreaming)
	case "check_streaming":
		a.enqueue("check_streaming", a.checkStreaming)
	case "new_key":
		a.enqueue("new_key", a.newKey)
	case "pause_streaming":
		a.enqueue("pause_streaming", a.pauseStreaming)
	case "resume_streaming":
		a.enqueue("start_streaming", a.startStreaming)
	case "refresh":
		a.enqueue("secrets", a.refreshSecrets)
	default:
		a.logf("ignored an unknown command %q", c.Action)
	}
}
