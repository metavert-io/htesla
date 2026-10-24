// Package certs makes the receiver's TLS identity: a private CA (EC
// P-256, made once) and a server certificate for the node's public
// hostname, issued by it. The car verifies our server against the CA
// we send in the telemetry configuration ("ca"), so no public
// certificate authority is involved. If cars ever refuse private CAs,
// a public certificate can be loaded instead — Source is the seam.
package certs

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net"
	"time"
)

// Material is a ready server identity plus the PEM the car must trust.
type Material struct {
	Cert       tls.Certificate
	TrustPEM   string // what goes in the telemetry config's "ca"
	Host       string
	NotAfter   time.Time
	SourceName string
}

// Source produces the server's TLS identity. The default is a private
// CA (PrivateCA); a public-certificate source can replace it later.
type Source interface {
	Name() string
	Material() (*Material, error)
}

// Static wraps PEMs already in hand (a public certificate chain + key,
// or anything else) as a Source.
type Static struct {
	ChainPEM string // leaf first, then intermediates
	KeyPEM   string
	TrustPEM string // what the car should trust (the chain's root/intermediates)
	Label    string
}

func (s Static) Name() string { return s.Label }

func (s Static) Material() (*Material, error) {
	cert, err := tls.X509KeyPair([]byte(s.ChainPEM), []byte(s.KeyPEM))
	if err != nil {
		return nil, err
	}
	leaf, err := x509.ParseCertificate(cert.Certificate[0])
	if err != nil {
		return nil, err
	}
	host := ""
	if len(leaf.DNSNames) > 0 {
		host = leaf.DNSNames[0]
	}
	return &Material{Cert: cert, TrustPEM: s.TrustPEM, Host: host, NotAfter: leaf.NotAfter, SourceName: s.Label}, nil
}

func serial() (*big.Int, error) {
	return rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 126))
}

func keyPEM(k *ecdsa.PrivateKey) (string, error) {
	der, err := x509.MarshalECPrivateKey(k)
	if err != nil {
		return "", err
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: der})), nil
}

func certPEM(der []byte) string {
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
}

// NewCA makes a private CA (EC P-256, 10 years).
func NewCA(commonName string) (certPEMOut, keyPEMOut string, err error) {
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return "", "", err
	}
	sn, err := serial()
	if err != nil {
		return "", "", err
	}
	now := time.Now().Add(-time.Hour)
	tmpl := &x509.Certificate{
		SerialNumber:          sn,
		Subject:               pkix.Name{CommonName: commonName, Organization: []string{"Hearthloom htesla"}},
		NotBefore:             now,
		NotAfter:              now.AddDate(10, 0, 0),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
		IsCA:                  true,
		MaxPathLenZero:        true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &k.PublicKey, k)
	if err != nil {
		return "", "", err
	}
	kp, err := keyPEM(k)
	return certPEM(der), kp, err
}

func parseCertPEM(p string) (*x509.Certificate, error) {
	block, _ := pem.Decode([]byte(p))
	if block == nil || block.Type != "CERTIFICATE" {
		return nil, errors.New("not a PEM certificate")
	}
	return x509.ParseCertificate(block.Bytes)
}

func parseKeyPEM(p string) (*ecdsa.PrivateKey, error) {
	block, _ := pem.Decode([]byte(p))
	if block == nil {
		return nil, errors.New("not a PEM key")
	}
	return x509.ParseECPrivateKey(block.Bytes)
}

// IssueServer issues a server certificate for host from the CA (EC
// P-256, 2 years). Returns the chain PEM (leaf then CA) and key PEM.
func IssueServer(caCertPEM, caKeyPEM, host string) (chainPEM, keyPEMOut string, err error) {
	ca, err := parseCertPEM(caCertPEM)
	if err != nil {
		return "", "", fmt.Errorf("ca certificate: %w", err)
	}
	caKey, err := parseKeyPEM(caKeyPEM)
	if err != nil {
		return "", "", fmt.Errorf("ca key: %w", err)
	}
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return "", "", err
	}
	sn, err := serial()
	if err != nil {
		return "", "", err
	}
	now := time.Now().Add(-time.Hour)
	tmpl := &x509.Certificate{
		SerialNumber: sn,
		Subject:      pkix.Name{CommonName: host},
		NotBefore:    now,
		NotAfter:     now.AddDate(2, 0, 0),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	if ip := net.ParseIP(host); ip != nil {
		tmpl.IPAddresses = []net.IP{ip}
	} else {
		tmpl.DNSNames = []string{host}
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca, &k.PublicKey, caKey)
	if err != nil {
		return "", "", err
	}
	kp, err := keyPEM(k)
	if err != nil {
		return "", "", err
	}
	return certPEM(der) + caCertPEM, kp, nil
}

// PrivateCA is the default Source: the CA + server cert PEMs as stored
// (certs as files, keys as sealed secrets).
type PrivateCA struct {
	CACertPEM    string
	ServerKeyPEM string
	ServerChain  string
}

func (p PrivateCA) Name() string { return "private-ca" }

func (p PrivateCA) Material() (*Material, error) {
	cert, err := tls.X509KeyPair([]byte(p.ServerChain), []byte(p.ServerKeyPEM))
	if err != nil {
		return nil, err
	}
	leaf, err := x509.ParseCertificate(cert.Certificate[0])
	if err != nil {
		return nil, err
	}
	cert.Leaf = leaf
	host := ""
	if len(leaf.DNSNames) > 0 {
		host = leaf.DNSNames[0]
	} else if len(leaf.IPAddresses) > 0 {
		host = leaf.IPAddresses[0].String()
	}
	return &Material{Cert: cert, TrustPEM: p.CACertPEM, Host: host, NotAfter: leaf.NotAfter, SourceName: p.Name()}, nil
}

// NeedsReissue reports whether the server cert must be re-issued: a
// different host, or less than 30 days left.
func NeedsReissue(chainPEM, host string, now time.Time) bool {
	leaf, err := parseCertPEM(chainPEM)
	if err != nil {
		return true
	}
	if now.Add(30 * 24 * time.Hour).After(leaf.NotAfter) {
		return true
	}
	if ip := net.ParseIP(host); ip != nil {
		for _, a := range leaf.IPAddresses {
			if a.Equal(ip) {
				return false
			}
		}
		return true
	}
	return leaf.VerifyHostname(host) != nil
}

// Fingerprint is a short SHA-256 of a PEM certificate (for status).
func Fingerprint(certPEMIn string) string {
	c, err := parseCertPEM(certPEMIn)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(c.Raw)
	return fmt.Sprintf("%x", sum[:8])
}
