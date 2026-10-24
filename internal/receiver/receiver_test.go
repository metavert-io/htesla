package receiver

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"math/big"
	"net"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/metavert-io/htesla/internal/certs"
	"github.com/teslamotors/fleet-telemetry/messages"
	"github.com/teslamotors/fleet-telemetry/protos"
	"github.com/teslamotors/fleet-telemetry/telemetry"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

const testVIN = "5YJ3E1EA0KF000001"

// testPKI is a stand-in for Tesla's PKI: a CA named like Tesla's
// product CA (so fleet-telemetry's identity rules accept its leaves)
// that issues car client certificates.
type testPKI struct {
	cert *x509.Certificate
	key  *ecdsa.PrivateKey
}

func newPKI(t *testing.T, cn string) *testPKI {
	t.Helper()
	k, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()), Subject: pkix.Name{CommonName: cn},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(24 * time.Hour),
		KeyUsage: x509.KeyUsageCertSign, BasicConstraintsValid: true, IsCA: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &k.PublicKey, k)
	if err != nil {
		t.Fatal(err)
	}
	c, _ := x509.ParseCertificate(der)
	return &testPKI{cert: c, key: k}
}

func (p *testPKI) pool() *x509.CertPool {
	pool := x509.NewCertPool()
	pool.AddCert(p.cert)
	return pool
}

func (p *testPKI) carCert(t *testing.T, vin string) tls.Certificate {
	t.Helper()
	k, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()), Subject: pkix.Name{CommonName: vin},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(24 * time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, p.cert, &k.PublicKey, p.key)
	if err != nil {
		t.Fatal(err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: k}
}

type sinkLog struct {
	mu sync.Mutex
	ev []Event
}

func (s *sinkLog) add(e Event) { s.mu.Lock(); s.ev = append(s.ev, e); s.mu.Unlock() }
func (s *sinkLog) all() []Event {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Event(nil), s.ev...)
}

// startServer runs the receiver on 127.0.0.1 with a private-CA server
// cert for "localhost" and the given client CA pool.
func startServer(t *testing.T, clientCAs *x509.CertPool, allow func(string) bool) (*Server, *sinkLog, *x509.CertPool) {
	t.Helper()
	caPEM, caKey, err := certs.NewCA("test receiver CA")
	if err != nil {
		t.Fatal(err)
	}
	chain, key, err := certs.IssueServer(caPEM, caKey, "localhost")
	if err != nil {
		t.Fatal(err)
	}
	m, err := certs.PrivateCA{CACertPEM: caPEM, ServerKeyPEM: key, ServerChain: chain}.Material()
	if err != nil {
		t.Fatal(err)
	}
	log := &sinkLog{}
	s := &Server{ClientCAs: clientCAs, AllowVIN: allow, Sink: log.add}
	s.SetCertificate(&m.Cert)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Serve(ln); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	trust := x509.NewCertPool()
	trust.AppendCertsFromPEM([]byte(m.TrustPEM))
	return s, log, trust
}

func dial(s *Server, trust *x509.CertPool, client *tls.Certificate) (*websocket.Conn, *http.Response, error) {
	cfg := &tls.Config{RootCAs: trust, ServerName: "localhost"}
	if client != nil {
		cfg.Certificates = []tls.Certificate{*client}
	}
	d := websocket.Dialer{TLSClientConfig: cfg, HandshakeTimeout: 5 * time.Second}
	return d.Dial("wss://"+s.Addr()+"/", nil)
}

func fixtureV(t *testing.T, vin, txid string) []byte {
	t.Helper()
	created := time.Date(2026, 10, 23, 18, 4, 5, 0, time.UTC)
	payload, err := proto.Marshal(&protos.Payload{
		Vin:       vin,
		CreatedAt: timestamppb.New(created),
		Data: []*protos.Datum{
			{Key: protos.Field_VehicleSpeed, Value: &protos.Value{Value: &protos.Value_DoubleValue{DoubleValue: 42.5}}},
			{Key: protos.Field_Gear, Value: &protos.Value{Value: &protos.Value_ShiftStateValue{ShiftStateValue: protos.ShiftState_ShiftStateD}}},
			{Key: protos.Field_Location, Value: &protos.Value{Value: &protos.Value_LocationValue{LocationValue: &protos.LocationValue{Latitude: 42.36, Longitude: -71.06}}}},
			{Key: protos.Field_ChargeState, Value: &protos.Value{Value: &protos.Value_ChargingValue{ChargingValue: protos.ChargingState_ChargeStateDisconnected}}},
			{Key: protos.Field_MediaNowPlayingTitle, Value: &protos.Value{Value: &protos.Value_StringValue{StringValue: "Ember Song"}}},
			{Key: protos.Field_Locked, Value: &protos.Value{Value: &protos.Value_BooleanValue{BooleanValue: true}}},
			{Key: protos.Field_OutsideTemp, Value: &protos.Value{Value: &protos.Value_Invalid{Invalid: true}}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	sm := messages.StreamMessage{
		TXID: []byte(txid), SenderID: []byte("vehicle_device." + vin), MessageTopic: []byte("V"),
		Payload: payload, CreatedAt: uint32(created.Unix()), DeviceType: []byte("vehicle_device"), DeviceID: []byte(vin),
	}
	b, err := sm.ToBytes()
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestDecodeFixtureRecord(t *testing.T) {
	ser := telemetry.NewBinarySerializer(&telemetry.RequestIdentity{DeviceID: testVIN, SenderID: "vehicle_device." + testVIN},
		dispatchTopics, quietLogger)
	rec, err := telemetry.NewRecord(ser, fixtureV(t, testVIN, "tx-1"), "sock", false)
	if err != nil {
		t.Fatal(err)
	}
	ev, ok := Decode(rec)
	if !ok {
		t.Fatal("V record not decoded")
	}
	if ev.VIN != testVIN || ev.Topic != "V" || ev.Txid != "tx-1" || ev.Signals != 7 {
		t.Fatalf("envelope: %+v", ev)
	}
	if ev.CreatedAt == nil || ev.CreatedAt.Format(time.RFC3339) != "2026-10-23T18:04:05Z" {
		t.Fatalf("created_at %v", ev.CreatedAt)
	}
	checks := map[string]any{
		"VehicleSpeed":         42.5,
		"Gear":                 "ShiftStateD",
		"ChargeState":          "ChargeStateDisconnected",
		"MediaNowPlayingTitle": "Ember Song",
		"Locked":               true,
	}
	for k, v := range checks {
		if ev.Data[k] != v {
			t.Errorf("%s = %#v, want %#v", k, ev.Data[k], v)
		}
	}
	loc, ok := ev.Data["Location"].(map[string]any)
	if !ok || loc["latitude"] != 42.36 || loc["longitude"] != -71.06 {
		t.Errorf("Location %#v", ev.Data["Location"])
	}
	if v, present := ev.Data["OutsideTemp"]; !present || v != nil {
		t.Errorf("an invalid value should be stored as null, got %#v (present %v)", v, present)
	}
}

func TestDecodeAlerts(t *testing.T) {
	payload, _ := proto.Marshal(&protos.VehicleAlerts{Alerts: []*protos.VehicleAlert{
		{Name: "BMS_a066_SW_Over_Current", Audiences: []protos.Audience{protos.Audience_Service}, StartedAt: timestamppb.Now()},
	}})
	sm := messages.StreamMessage{TXID: []byte("tx-a"), SenderID: []byte("vehicle_device." + testVIN), MessageTopic: []byte("alerts"), Payload: payload}
	b, _ := sm.ToBytes()
	ser := telemetry.NewBinarySerializer(&telemetry.RequestIdentity{DeviceID: testVIN, SenderID: "vehicle_device." + testVIN}, dispatchTopics, quietLogger)
	rec, err := telemetry.NewRecord(ser, b, "sock", false)
	if err != nil {
		t.Fatal(err)
	}
	ev, ok := Decode(rec)
	if !ok || ev.Topic != "alerts" || len(ev.Alerts) != 1 || ev.Signals != 1 {
		t.Fatalf("%+v", ev)
	}
	if ev.Alerts[0].(map[string]any)["name"] != "BMS_a066_SW_Over_Current" {
		t.Fatalf("alert %v", ev.Alerts[0])
	}
}

func TestMTLSAcceptsTeslaCAAndAcks(t *testing.T) {
	tesla := newPKI(t, "Tesla Motors Products CA") // the injected "Tesla" CA
	s, log, trust := startServer(t, tesla.pool(), nil)
	car := tesla.carCert(t, testVIN)
	ws, _, err := dial(s, trust, &car)
	if err != nil {
		t.Fatalf("a car cert from the configured CA must be accepted: %v", err)
	}
	if err := ws.WriteMessage(websocket.BinaryMessage, fixtureV(t, testVIN, "tx-42")); err != nil {
		t.Fatal(err)
	}
	_ = ws.SetReadDeadline(time.Now().Add(5 * time.Second))
	mt, reply, err := ws.ReadMessage()
	if err != nil || mt != websocket.BinaryMessage {
		t.Fatalf("no ack: %v", err)
	}
	ack, err := messages.StreamAckMessageFromBytes(reply)
	if err != nil {
		t.Fatalf("reply isn't a stream ack: %v", err)
	}
	if string(ack.TXID) != "tx-42" || ack.Topic() != "V" {
		t.Fatalf("ack txid %q topic %q", ack.TXID, ack.Topic())
	}
	_ = ws.Close()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && len(log.all()) < 3 {
		time.Sleep(20 * time.Millisecond)
	}
	ev := log.all()
	if len(ev) != 3 {
		t.Fatalf("want connected, V, disconnected; got %d events: %+v", len(ev), ev)
	}
	if ev[0].Topic != "connectivity" || ev[0].Status != "connected" || ev[0].VIN != testVIN {
		t.Errorf("first event %+v", ev[0])
	}
	if ev[1].Topic != "V" || ev[1].Data["VehicleSpeed"] != 42.5 {
		t.Errorf("record event %+v", ev[1])
	}
	if ev[2].Topic != "connectivity" || ev[2].Status != "disconnected" {
		t.Errorf("last event %+v", ev[2])
	}
	if st := s.Snapshot(); st.Connections != 1 || st.Records != 1 || st.Rejected != 0 {
		t.Errorf("stats %+v", st)
	}
}

func TestMTLSRejectsForeignCA(t *testing.T) {
	tesla := newPKI(t, "Tesla Motors Products CA")
	// same name, different key: an impostor CA
	rogue := newPKI(t, "Tesla Motors Products CA")
	s, log, trust := startServer(t, tesla.pool(), nil)
	fake := rogue.carCert(t, testVIN)
	if ws, _, err := dial(s, trust, &fake); err == nil {
		// TLS 1.3 can finish the client side before the server's alert;
		// the server must still never accept the socket
		_ = ws.SetReadDeadline(time.Now().Add(2 * time.Second))
		_ = ws.WriteMessage(websocket.BinaryMessage, fixtureV(t, testVIN, "tx-x"))
		if _, _, rerr := ws.ReadMessage(); rerr == nil {
			t.Fatal("a cert from another CA got a reply")
		}
		ws.Close()
	}
	if ws, _, err := dial(s, trust, nil); err == nil {
		ws.Close()
		t.Fatal("a connection with no client cert was accepted")
	}
	time.Sleep(100 * time.Millisecond)
	if n := len(log.all()); n != 0 {
		t.Fatalf("rejected connections produced %d events", n)
	}
	if st := s.Snapshot(); st.TLSRefused < 2 || st.Connections != 0 {
		t.Fatalf("both handshakes should be counted as refused: %+v", st)
	}
}

func TestAllowlistRejectsOtherCars(t *testing.T) {
	tesla := newPKI(t, "Tesla Motors Products CA")
	s, log, trust := startServer(t, tesla.pool(), func(vin string) bool { return vin == testVIN })
	other := tesla.carCert(t, "5YJ3E1EA0KF999999")
	_, resp, err := dial(s, trust, &other)
	if err == nil {
		t.Fatal("a car outside the allowlist was accepted")
	}
	if resp == nil || resp.StatusCode != http.StatusForbidden {
		t.Fatalf("want 403, got %v", resp)
	}
	if len(log.all()) != 0 || s.Snapshot().Rejected != 1 {
		t.Fatalf("events %v stats %+v", log.all(), s.Snapshot())
	}
	mine := tesla.carCert(t, testVIN)
	ws, _, err := dial(s, trust, &mine)
	if err != nil {
		t.Fatalf("own car refused: %v", err)
	}
	ws.Close()
}

func TestUnknownIssuerRefused(t *testing.T) {
	// a CA the pool trusts but whose name fleet-telemetry doesn't know:
	// verified TLS, yet no Tesla identity → refused before upgrade
	odd := newPKI(t, "Some Other CA")
	s, log, trust := startServer(t, odd.pool(), nil)
	c := odd.carCert(t, testVIN)
	_, resp, err := dial(s, trust, &c)
	if err == nil || resp == nil || resp.StatusCode != http.StatusForbidden {
		t.Fatalf("want 403 for an unknown issuer, got err=%v resp=%v", err, resp)
	}
	if len(log.all()) != 0 {
		t.Fatal("no events for refused identities")
	}
}
