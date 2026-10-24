package fleettelemetry

import (
	"bytes"
	"testing"
)

func TestBundleParses(t *testing.T) {
	if n := bytes.Count(ProdCA, []byte("BEGIN CERTIFICATE")); n != 14 {
		t.Fatalf("bundle has %d certificates, want 14 (fleet-telemetry v0.9.4)", n)
	}
	_ = TeslaCAs() // panics if the PEM doesn't parse
}
