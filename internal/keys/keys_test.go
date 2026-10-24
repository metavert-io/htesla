package keys

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/asn1"
	"encoding/pem"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// prime256v1 (a.k.a. secp256r1, NIST P-256)
var oidPrime256v1 = asn1.ObjectIdentifier{1, 2, 840, 10045, 3, 1, 7}

func TestGeneratePEMShapes(t *testing.T) {
	priv, pub, err := Generate(nil)
	if err != nil {
		t.Fatal(err)
	}
	pb, rest := pem.Decode([]byte(priv))
	if pb == nil || pb.Type != "EC PRIVATE KEY" || len(strings.TrimSpace(string(rest))) != 0 {
		t.Fatalf("private PEM should be one SEC 1 'EC PRIVATE KEY' block, got %q", priv[:30])
	}
	ub, _ := pem.Decode([]byte(pub))
	if ub == nil || ub.Type != "PUBLIC KEY" {
		t.Fatalf("public PEM should be a PKIX 'PUBLIC KEY' block")
	}
	// the SubjectPublicKeyInfo names the prime256v1 curve explicitly
	var spki struct {
		Algorithm pkix.AlgorithmIdentifier
		Key       asn1.BitString
	}
	if _, err := asn1.Unmarshal(ub.Bytes, &spki); err != nil {
		t.Fatal(err)
	}
	var curve asn1.ObjectIdentifier
	if _, err := asn1.Unmarshal(spki.Algorithm.Parameters.FullBytes, &curve); err != nil {
		t.Fatal(err)
	}
	if !curve.Equal(oidPrime256v1) {
		t.Fatalf("curve OID %v, want prime256v1 %v", curve, oidPrime256v1)
	}
	if len(spki.Key.Bytes) != 65 || spki.Key.Bytes[0] != 0x04 {
		t.Fatalf("public point should be uncompressed 65 bytes, got %d", len(spki.Key.Bytes))
	}
	k, err := ParsePrivate(priv)
	if err != nil {
		t.Fatal(err)
	}
	if k.Curve != elliptic.P256() {
		t.Fatal("not P-256")
	}
	pk, err := x509.ParsePKIXPublicKey(ub.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	if !pk.(*ecdsa.PublicKey).Equal(&k.PublicKey) {
		t.Fatal("public PEM doesn't match the private key")
	}
	derived, err := PublicFromPrivate(priv)
	if err != nil || !SamePublicKey(derived, pub) {
		t.Fatal("PublicFromPrivate should reproduce the public key")
	}
	_, other, _ := Generate(nil)
	if SamePublicKey(pub, other) {
		t.Fatal("two generated keys compared equal")
	}
	if !SamePublicKey(pub, strings.ReplaceAll(pub, "\n", "\r\n")+"\n\n") {
		t.Fatal("line endings shouldn't matter")
	}
}

func TestCommandKeyMatches(t *testing.T) {
	priv, _, err := Generate(nil)
	if err != nil {
		t.Fatal(err)
	}
	k, _ := ParsePrivate(priv)
	ck, err := CommandKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	want := elliptic.Marshal(elliptic.P256(), k.PublicKey.X, k.PublicKey.Y) //nolint:staticcheck // reference encoding
	if string(ck.PublicBytes()) != string(want) {
		t.Fatal("vehicle-command key has a different public point")
	}
}

func TestRejectsOtherCurves(t *testing.T) {
	k, err := ecdsa.GenerateKey(elliptic.P384(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, _ := x509.MarshalECPrivateKey(k)
	p := string(pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: der}))
	if _, err := ParsePrivate(p); err == nil {
		t.Fatal("a P-384 key must be refused")
	}
}

// The same check Tesla's docs make with openssl, when openssl exists.
func TestOpenSSLAgrees(t *testing.T) {
	bin, err := exec.LookPath("openssl")
	if err != nil {
		t.Skip("openssl not installed")
	}
	priv, pub, _ := Generate(nil)
	dir := t.TempDir()
	pp := filepath.Join(dir, "private-key.pem")
	_ = os.WriteFile(pp, []byte(priv), 0o600)
	out, err := exec.Command(bin, "ec", "-in", pp, "-pubout").CombinedOutput()
	if err != nil {
		t.Fatalf("openssl ec: %v %s", err, out)
	}
	if !SamePublicKey(string(out), pub) {
		t.Fatal("openssl derives a different public key")
	}
	text, _ := exec.Command(bin, "ec", "-in", pp, "-noout", "-text").CombinedOutput()
	if !strings.Contains(string(text), "prime256v1") && !strings.Contains(string(text), "P-256") {
		t.Fatalf("openssl doesn't see prime256v1: %s", text)
	}
}
