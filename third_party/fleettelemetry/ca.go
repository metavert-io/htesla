// Package fleettelemetry carries the one file this ember copies from
// Tesla's fleet-telemetry repository instead of importing it: the
// production CA bundle the reference server trusts for car client
// certificates. It lives in fleet-telemetry's `config` package, which
// also pulls in Kafka, ZeroMQ and cloud SDKs (some need cgo), so the
// bundle is vendored here verbatim. See NOTICE.
package fleettelemetry

import (
	"crypto/x509"
	_ "embed"
)

// ProdCA is config/files/prod_ca.crt from
// github.com/teslamotors/fleet-telemetry v0.9.4 (unchanged in v0.9.5),
// Copyright Tesla, Inc., Apache License 2.0.
//
//go:embed prod_ca.crt
var ProdCA []byte

// TeslaCAs is the pool the receiver verifies cars' client certs with.
func TeslaCAs() *x509.CertPool {
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(ProdCA) {
		panic("fleet-telemetry CA bundle didn't parse")
	}
	return pool
}
