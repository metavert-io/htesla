package certs

import (
	"crypto/x509"
	"encoding/pem"
	"strings"
	"testing"
	"time"
)

func TestPrivateCAIssuesVerifiableServerCert(t *testing.T) {
	caPEM, caKey, err := NewCA("htesla receiver CA (test)")
	if err != nil {
		t.Fatal(err)
	}
	chain, key, err := IssueServer(caPEM, caKey, "yourname.hearthlaunch.com")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(chain, caPEM) || strings.Count(chain, "BEGIN CERTIFICATE") != 2 {
		t.Fatal("chain should be leaf then CA")
	}
	m, err := PrivateCA{CACertPEM: caPEM, ServerKeyPEM: key, ServerChain: chain}.Material()
	if err != nil {
		t.Fatal(err)
	}
	if m.Host != "yourname.hearthlaunch.com" || m.TrustPEM != caPEM || m.SourceName != "private-ca" {
		t.Fatalf("%+v", m)
	}
	roots := x509.NewCertPool()
	roots.AppendCertsFromPEM([]byte(caPEM))
	if _, err := m.Cert.Leaf.Verify(x509.VerifyOptions{Roots: roots, DNSName: "yourname.hearthlaunch.com"}); err != nil {
		t.Fatalf("the car's check (server cert against our CA) fails: %v", err)
	}
	if _, err := m.Cert.Leaf.Verify(x509.VerifyOptions{Roots: roots, DNSName: "evil.example.com"}); err == nil {
		t.Fatal("verified for the wrong host")
	}
	block, _ := pem.Decode([]byte(caPEM))
	ca, _ := x509.ParseCertificate(block.Bytes)
	if !ca.IsCA || ca.NotAfter.Before(time.Now().AddDate(9, 0, 0)) {
		t.Fatal("CA should be a long-lived CA")
	}
	if NeedsReissue(chain, "yourname.hearthlaunch.com", time.Now()) {
		t.Fatal("fresh cert shouldn't need reissue")
	}
	if !NeedsReissue(chain, "other.hearthlaunch.com", time.Now()) {
		t.Fatal("a host change needs a new cert")
	}
	if !NeedsReissue(chain, "yourname.hearthlaunch.com", time.Now().AddDate(1, 11, 10)) {
		t.Fatal("a cert with under 30 days left needs reissue")
	}
	if Fingerprint(caPEM) == "" || Fingerprint(caPEM) == Fingerprint(chain) {
		t.Fatal("fingerprints")
	}
}
