// Package keys makes and reads the ember's Tesla application key: an
// EC key on NIST P-256 (prime256v1), the curve Tesla's virtual keys
// use. The private half is sealed by the daemon; the public half is
// published at /.well-known/appspecific/com.tesla.3p.public-key.pem.
//
// The PEM shapes match what Tesla's docs produce with openssl:
//
//	openssl ecparam -name prime256v1 -genkey -noout   → "EC PRIVATE KEY" (SEC 1)
//	openssl ec -pubout                                 → "PUBLIC KEY"    (PKIX)
package keys

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/teslamotors/vehicle-command/pkg/protocol"
)

// Generate makes a new P-256 key pair and returns both PEMs.
func Generate(rng io.Reader) (privatePEM, publicPEM string, err error) {
	if rng == nil {
		rng = rand.Reader
	}
	k, err := ecdsa.GenerateKey(elliptic.P256(), rng)
	if err != nil {
		return "", "", err
	}
	der, err := x509.MarshalECPrivateKey(k)
	if err != nil {
		return "", "", err
	}
	privatePEM = string(pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: der}))
	publicPEM, err = PublicPEM(&k.PublicKey)
	return privatePEM, publicPEM, err
}

// PublicPEM encodes a public key as PKIX "PUBLIC KEY" PEM.
func PublicPEM(pub *ecdsa.PublicKey) (string, error) {
	der, err := x509.MarshalPKIXPublicKey(pub)
	if err != nil {
		return "", err
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der})), nil
}

// ParsePrivate reads an EC P-256 private key from SEC 1 or PKCS #8 PEM.
func ParsePrivate(privatePEM string) (*ecdsa.PrivateKey, error) {
	block, _ := pem.Decode([]byte(privatePEM))
	if block == nil {
		return nil, errors.New("not a PEM private key")
	}
	var k *ecdsa.PrivateKey
	switch block.Type {
	case "EC PRIVATE KEY":
		var err error
		if k, err = x509.ParseECPrivateKey(block.Bytes); err != nil {
			return nil, err
		}
	case "PRIVATE KEY":
		anyKey, err := x509.ParsePKCS8PrivateKey(block.Bytes)
		if err != nil {
			return nil, err
		}
		var ok bool
		if k, ok = anyKey.(*ecdsa.PrivateKey); !ok {
			return nil, errors.New("only EC keys are supported")
		}
	default:
		return nil, fmt.Errorf("unexpected PEM block %q", block.Type)
	}
	if k.Curve != elliptic.P256() {
		return nil, errors.New("the key must be on NIST P-256 (prime256v1)")
	}
	return k, nil
}

// PublicFromPrivate derives the public PEM from a private PEM.
func PublicFromPrivate(privatePEM string) (string, error) {
	k, err := ParsePrivate(privatePEM)
	if err != nil {
		return "", err
	}
	return PublicPEM(&k.PublicKey)
}

// SamePublicKey reports whether two public-key PEMs hold the same key
// (whitespace and line-ending differences ignored).
func SamePublicKey(a, b string) bool {
	pa, errA := parsePublic(a)
	pb, errB := parsePublic(b)
	if errA != nil || errB != nil {
		return false
	}
	return pa.Equal(pb)
}

func parsePublic(p string) (*ecdsa.PublicKey, error) {
	block, _ := pem.Decode([]byte(strings.TrimSpace(p)))
	if block == nil || block.Type != "PUBLIC KEY" {
		return nil, errors.New("not a PEM public key")
	}
	anyKey, err := x509.ParsePKIXPublicKey(block.Bytes)
	if err != nil {
		return nil, err
	}
	k, ok := anyKey.(*ecdsa.PublicKey)
	if !ok {
		return nil, errors.New("not an EC public key")
	}
	return k, nil
}

// CommandKey converts the private key into vehicle-command's key type,
// the one its fleet signer (pkg/sign) takes. Only the 32-byte scalar
// crosses over; the curve is fixed to P-256 on both sides.
func CommandKey(privatePEM string) (protocol.ECDHPrivateKey, error) {
	k, err := ParsePrivate(privatePEM)
	if err != nil {
		return nil, err
	}
	scalar := make([]byte, 32)
	k.D.FillBytes(scalar)
	ck := protocol.UnmarshalECDHPrivateKey(scalar)
	if ck == nil {
		return nil, errors.New("vehicle-command refused the key")
	}
	return ck, nil
}
