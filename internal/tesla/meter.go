package tesla

import (
	"net/http"
	"regexp"
	"strings"
	"sync"
)

// Meter counts calls to the Fleet API, sorted into the free endpoints
// this ember uses and everything else (which Tesla bills as data
// requests). The ember makes no billed calls today; the meter is how
// the cost guard would see one if that ever changed.
type Meter struct {
	mu     sync.Mutex
	free   int64
	billed int64
	byPath map[string]int64
}

var vinSeg = regexp.MustCompile(`^/api/1/vehicles/[^/]+(/.*)?$`)

// freeEndpoint says whether a Fleet API path is one Tesla doesn't bill.
func freeEndpoint(method, path string) (string, bool) {
	switch {
	case path == "/api/1/vehicles" || path == "/api/1/vehicles/fleet_status" ||
		path == "/api/1/vehicles/fleet_telemetry_config_jws" || path == "/api/1/users/region" ||
		strings.HasPrefix(path, "/api/1/partner_accounts"):
		return path, true
	case vinSeg.MatchString(path):
		rest := vinSeg.FindStringSubmatch(path)[1]
		name := "/api/1/vehicles/{vin}" + rest
		switch rest {
		case "", "/fleet_telemetry_config", "/fleet_telemetry_errors", "/release_notes", "/service_data":
			return name, true
		}
		return name, false
	}
	return path, false
}

// Count records one call.
func (m *Meter) Count(method, path string) {
	name, free := freeEndpoint(method, path)
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.byPath == nil {
		m.byPath = map[string]int64{}
	}
	m.byPath[method+" "+name]++
	if free {
		m.free++
	} else {
		m.billed++
	}
}

// Take returns the counts since the last Take and resets them.
func (m *Meter) Take() (free, billed int64, byPath map[string]int64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	free, billed, byPath = m.free, m.billed, m.byPath
	m.free, m.billed, m.byPath = 0, 0, nil
	return
}

// Transport wraps an http.RoundTripper, counting Fleet API calls (the
// token endpoint is auth, not billed, and isn't counted).
func (m *Meter) Transport(next http.RoundTripper) http.RoundTripper {
	if next == nil {
		next = http.DefaultTransport
	}
	return roundTripper(func(r *http.Request) (*http.Response, error) {
		if strings.HasPrefix(r.URL.Path, "/api/1/") {
			m.Count(r.Method, r.URL.Path)
		}
		return next.RoundTrip(r)
	})
}

type roundTripper func(*http.Request) (*http.Response, error)

func (f roundTripper) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
