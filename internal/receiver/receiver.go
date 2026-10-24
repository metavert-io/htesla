// Package receiver is the ember's Fleet Telemetry server: the car opens
// a WebSocket over mutual TLS to this process and pushes protobuf
// records inside Tesla's flatbuffers envelope. mTLS terminates HERE —
// the car's client certificate is verified against Tesla's CA bundle —
// and every record is decoded with Tesla's own fleet-telemetry library
// (github.com/teslamotors/fleet-telemetry: messages, telemetry, protos)
// and acknowledged the way its reference server acks (so
// delivery_policy "latest" resends only what we never took).
//
// Connectivity events (online/offline) are not sent by the car; like
// the reference server, we derive them from the socket opening and
// closing.
package receiver

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"math"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"
	ftlog "github.com/teslamotors/fleet-telemetry/logger"
	"github.com/teslamotors/fleet-telemetry/messages"
	"github.com/teslamotors/fleet-telemetry/protos"
	"github.com/teslamotors/fleet-telemetry/telemetry"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
)

// Event is one thing that happened on the stream, decoded and ready to
// be stored as a JSON line.
type Event struct {
	ReceivedAt time.Time      `json:"received_at"`
	VIN        string         `json:"vin"`
	Topic      string         `json:"topic"` // V | alerts | errors | connectivity
	Txid       string         `json:"txid,omitempty"`
	CreatedAt  *time.Time     `json:"created_at,omitempty"`
	Resend     bool           `json:"resend,omitempty"`
	Data       map[string]any `json:"data,omitempty"`   // V: field → value (nil = invalid)
	Alerts     []any          `json:"alerts,omitempty"` // alerts topic
	Errors     []any          `json:"errors,omitempty"` // errors topic
	// connectivity (derived from the socket)
	Status        string `json:"status,omitempty"` // connected | disconnected
	ConnectionID  string `json:"connection_id,omitempty"`
	Network       string `json:"network,omitempty"`
	ClientVersion string `json:"client_version,omitempty"`
	// Signals is what Tesla bills for this record (data points + alerts).
	Signals int `json:"signals,omitempty"`
}

// Stats are counters for the status page.
type Stats struct {
	Connections int64  `json:"connections"`
	Records     int64  `json:"records"`
	Rejected    int64  `json:"rejected"`    // verified TLS, refused identity / not our car
	TLSRefused  int64  `json:"tls_refused"` // handshakes refused (no or untrusted client cert, scanners)
	BadRecords  int64  `json:"bad_records"`
	LastError   string `json:"last_error,omitempty"`
	LastErrorAt string `json:"last_error_at,omitempty"`
}

// Server is the mTLS WebSocket receiver.
type Server struct {
	// ClientCAs verifies the car's client certificate — Tesla's CA
	// bundle in production (TeslaCAs), a test CA in tests.
	ClientCAs *x509.CertPool
	// AllowVIN, when set, must approve the certificate's VIN (an
	// allowlist of the owner's own cars — Tesla's advice).
	AllowVIN func(vin string) bool
	// Sink receives every event. It is called on the socket's goroutine
	// BEFORE the record is acked, so keep it quick (buffer, don't block
	// on the network).
	Sink func(Event)
	Logf func(format string, args ...any)

	cert atomic.Pointer[tls.Certificate]

	mu         sync.Mutex
	ln         net.Listener
	srv        *http.Server
	stats      Stats
	sockets    map[string]*websocket.Conn
	addr       string
	serveErr   error
	lastTLSLog time.Time
}

func (s *Server) logf(format string, args ...any) {
	if s.Logf != nil {
		s.Logf(format, args...)
	}
}

// SetCertificate swaps the server identity (takes effect on the next
// handshake; open sockets keep theirs).
func (s *Server) SetCertificate(c *tls.Certificate) { s.cert.Store(c) }

// TLSConfig is the server side of the mutual TLS handshake.
func (s *Server) TLSConfig() *tls.Config {
	return &tls.Config{
		MinVersion: tls.VersionTLS12,
		ClientAuth: tls.RequireAndVerifyClientCert,
		ClientCAs:  s.ClientCAs,
		GetCertificate: func(*tls.ClientHelloInfo) (*tls.Certificate, error) {
			c := s.cert.Load()
			if c == nil {
				return nil, errors.New("no server certificate yet")
			}
			return c, nil
		},
		// HTTP/1.1 only: the car speaks WebSocket
		NextProtos: []string{"http/1.1"},
	}
}

// Listen binds addr (e.g. "0.0.0.0:8443") and serves in the background.
func (s *Server) Listen(addr string) error {
	if s.ClientCAs == nil {
		return errors.New("no client CA pool")
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	return s.Serve(ln)
}

// Serve serves on an existing listener (tests pass 127.0.0.1:0).
func (s *Server) Serve(ln net.Listener) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.srv != nil {
		ln.Close()
		return errors.New("already serving")
	}
	s.sockets = map[string]*websocket.Conn{}
	mux := http.NewServeMux()
	mux.HandleFunc("/", s.handle)
	srv := &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 15 * time.Second,
		// handshake refusals (internet scanners, certs we don't trust)
		// are counted, and logged at most once a minute
		ErrorLog: log.New(serverErrors{s}, "", 0),
	}
	tln := tls.NewListener(ln, s.TLSConfig())
	s.ln = tln
	s.srv = srv
	s.addr = ln.Addr().String()
	go func() {
		err := srv.Serve(tln)
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			s.mu.Lock()
			s.serveErr = err
			s.mu.Unlock()
			s.logf("telemetry receiver stopped: %v", err)
		}
	}()
	return nil
}

// Addr is the bound address ("" when not listening).
func (s *Server) Addr() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.srv == nil {
		return ""
	}
	return s.addr
}

// Close stops listening and closes open sockets.
func (s *Server) Close() error {
	s.mu.Lock()
	srv := s.srv
	socks := s.sockets
	s.srv, s.ln, s.sockets = nil, nil, nil
	s.mu.Unlock()
	if srv == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	err := srv.Shutdown(ctx)
	for _, ws := range socks {
		_ = ws.Close()
	}
	return err
}

// Snapshot returns the counters.
func (s *Server) Snapshot() Stats {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.stats
}

func (s *Server) noteError(format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	s.mu.Lock()
	s.stats.LastError = msg
	s.stats.LastErrorAt = time.Now().UTC().Format(time.RFC3339)
	s.mu.Unlock()
	s.logf("%s", msg)
}

func (s *Server) bump(f func(*Stats)) {
	s.mu.Lock()
	f(&s.stats)
	s.mu.Unlock()
}

// serverErrors receives net/http's own error lines.
type serverErrors struct{ s *Server }

func (w serverErrors) Write(p []byte) (int, error) {
	msg := strings.TrimSpace(string(p))
	s := w.s
	s.mu.Lock()
	quiet := false
	if strings.Contains(msg, "TLS handshake error") {
		s.stats.TLSRefused++
		quiet = time.Since(s.lastTLSLog) < time.Minute
		if !quiet {
			s.lastTLSLog = time.Now()
		}
	}
	s.mu.Unlock()
	if !quiet {
		s.logf("receiver: %s", msg)
	}
	return len(p), nil
}

var upgrader = websocket.Upgrader{
	// cars, not browsers — there is no Origin to check
	CheckOrigin:     func(*http.Request) bool { return true },
	ReadBufferSize:  4096,
	WriteBufferSize: 1024,
}

// Identity is who the verified client certificate says connected.
type Identity struct {
	VIN      string
	SenderID string
}

// IdentityFromRequest reads the VIN from the VERIFIED client cert via
// fleet-telemetry's own rules (known Tesla issuers / OIDs).
func IdentityFromRequest(r *http.Request) (*Identity, error) {
	if r.TLS == nil || len(r.TLS.VerifiedChains) == 0 || len(r.TLS.VerifiedChains[0]) == 0 {
		return nil, errors.New("no verified client certificate")
	}
	cert := r.TLS.VerifiedChains[0][0]
	clientType, deviceID, err := messages.CreateIdentityFromCert(cert)
	if err != nil {
		return nil, fmt.Errorf("certificate issuer %q / subject %q: %w", cert.Issuer.CommonName, cert.Subject.CommonName, err)
	}
	return &Identity{VIN: deviceID, SenderID: clientType + "." + deviceID}, nil
}

func (s *Server) handle(w http.ResponseWriter, r *http.Request) {
	id, err := IdentityFromRequest(r)
	if err != nil {
		s.bump(func(st *Stats) { st.Rejected++ })
		s.noteError("refused a connection: %v", err)
		http.Error(w, "unauthorized", http.StatusForbidden)
		return
	}
	if s.AllowVIN != nil && !s.AllowVIN(id.VIN) {
		s.bump(func(st *Stats) { st.Rejected++ })
		s.noteError("refused car …%s: not one of this account's cars", last4(id.VIN))
		http.Error(w, "unknown vehicle", http.StatusForbidden)
		return
	}
	ws, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		return // Upgrade already answered
	}
	connID := uuid.NewString()
	s.mu.Lock()
	if s.sockets != nil {
		s.sockets[connID] = ws
	}
	s.stats.Connections++
	s.mu.Unlock()
	network := r.Header.Get("X-Network-Interface")
	clientVersion := r.Header.Get("Version")
	s.emit(Event{ReceivedAt: time.Now().UTC(), VIN: id.VIN, Topic: "connectivity", Status: "connected",
		ConnectionID: connID, Network: network, ClientVersion: clientVersion})
	defer func() {
		s.mu.Lock()
		if s.sockets != nil {
			delete(s.sockets, connID)
		}
		s.mu.Unlock()
		_ = ws.Close()
		s.emit(Event{ReceivedAt: time.Now().UTC(), VIN: id.VIN, Topic: "connectivity", Status: "disconnected",
			ConnectionID: connID, Network: network, ClientVersion: clientVersion})
	}()
	s.serveSocket(ws, id, connID, clientVersion)
}

// dispatchTopics mirrors the reference server's dispatch rules: topics
// it routes skip the sender-id re-check (the verified certificate
// already named the car). Values are empty — we consume records
// ourselves rather than through fleet-telemetry's producers.
var dispatchTopics = map[string][]telemetry.Producer{"V": nil, "alerts": nil, "errors": nil}

var quietLogger = func() *ftlog.Logger { l, _ := ftlog.NoOpLogger(); return l }()

func (s *Server) serveSocket(ws *websocket.Conn, id *Identity, connID, clientVersion string) {
	serializer := telemetry.NewBinarySerializer(&telemetry.RequestIdentity{
		DeviceID: id.VIN, SenderID: id.SenderID, DeviceClientVersion: clientVersion,
	}, dispatchTopics, quietLogger)
	for {
		msgType, msg, err := ws.ReadMessage()
		if err != nil {
			return
		}
		if msgType != websocket.BinaryMessage {
			return // the reference server hangs up on anything else
		}
		reply := s.process(serializer, msg, connID)
		if reply == nil {
			continue
		}
		_ = ws.SetWriteDeadline(time.Now().Add(10 * time.Second))
		if err := ws.WriteMessage(websocket.BinaryMessage, reply); err != nil {
			return
		}
	}
}

// process decodes one message and returns the reply (ack or error),
// following the reference server's choices.
func (s *Server) process(serializer *telemetry.BinarySerializer, msg []byte, connID string) []byte {
	record, err := telemetry.NewRecord(serializer, msg, connID, false)
	if err != nil {
		s.bump(func(st *Stats) { st.BadRecords++ })
		if errors.Is(err, telemetry.ErrMessageTooBig) {
			return record.Error(errors.New("incorrect message format"))
		}
		var unknown *telemetry.UnknownMessageType
		var sender *telemetry.UnauthorizedSenderIDError
		if errors.As(err, &unknown) || errors.As(err, &sender) {
			// ack so the car stops resending what we'll never accept
			s.noteError("dropped a %s record: %v", record.TxType, err)
			return record.Ack()
		}
		s.noteError("bad %s record: %v", record.TxType, err)
		return record.Error(errors.New("incorrect message format"))
	}
	ev, ok := Decode(record)
	if ok {
		s.emit(ev)
		s.bump(func(st *Stats) { st.Records++ })
	}
	return record.Ack()
}

func (s *Server) emit(ev Event) {
	if s.Sink != nil {
		s.Sink(ev)
	}
}

// Decode turns a fleet-telemetry record into an Event.
func Decode(record *telemetry.Record) (Event, bool) {
	ev := Event{
		ReceivedAt: time.UnixMilli(record.ReceivedTimestamp).UTC(),
		VIN:        record.Vin,
		Topic:      record.TxType,
		Txid:       record.Txid,
	}
	if record.ReceivedTimestamp == 0 {
		ev.ReceivedAt = time.Now().UTC()
	}
	switch m := record.GetProtoMessage().(type) {
	case *protos.Payload:
		ev.Data = make(map[string]any, len(m.GetData()))
		for _, d := range m.GetData() {
			ev.Data[d.GetKey().String()] = ValueOf(d.GetValue())
		}
		if ts := m.GetCreatedAt(); ts != nil {
			t := ts.AsTime().UTC()
			ev.CreatedAt = &t
		}
		ev.Resend = m.GetIsResend()
		ev.Signals = len(m.GetData())
	case *protos.VehicleAlerts:
		for _, a := range m.GetAlerts() {
			ev.Alerts = append(ev.Alerts, messageJSON(a))
		}
		if ts := m.GetCreatedAt(); ts != nil {
			t := ts.AsTime().UTC()
			ev.CreatedAt = &t
		}
		ev.Signals = len(m.GetAlerts())
	case *protos.VehicleErrors:
		for _, e := range m.GetErrors() {
			ev.Errors = append(ev.Errors, messageJSON(e))
		}
		if ts := m.GetCreatedAt(); ts != nil {
			t := ts.AsTime().UTC()
			ev.CreatedAt = &t
		}
	default:
		return ev, false // a topic we don't keep
	}
	return ev, true
}

// ValueOf flattens a telemetry Value's oneof into a plain JSON value:
// numbers stay numbers, enums become their names, structured values
// (location, doors, time…) become objects; "invalid" becomes nil.
func ValueOf(v *protos.Value) any {
	if v == nil {
		return nil
	}
	m := v.ProtoReflect()
	od := m.Descriptor().Oneofs().ByName("value")
	if od == nil {
		return nil
	}
	fd := m.WhichOneof(od)
	if fd == nil || fd.Name() == "invalid" {
		return nil
	}
	val := m.Get(fd)
	switch fd.Kind() {
	case protoreflect.EnumKind:
		if ev := fd.Enum().Values().ByNumber(val.Enum()); ev != nil {
			return string(ev.Name())
		}
		return int32(val.Enum())
	case protoreflect.MessageKind:
		return messageJSON(val.Message().Interface())
	case protoreflect.FloatKind:
		f := val.Float()
		if math.IsNaN(f) || math.IsInf(f, 0) {
			return nil
		}
		return float32(f)
	case protoreflect.DoubleKind:
		f := val.Float()
		if math.IsNaN(f) || math.IsInf(f, 0) {
			return nil
		}
		return f
	default:
		return val.Interface()
	}
}

var jsonOpts = protojson.MarshalOptions{UseProtoNames: true}

func messageJSON(m proto.Message) any {
	b, err := jsonOpts.Marshal(m)
	if err != nil {
		return nil
	}
	var out any
	if json.Unmarshal(b, &out) != nil {
		return nil
	}
	return out
}

func last4(vin string) string {
	if len(vin) <= 4 {
		return vin
	}
	return vin[len(vin)-4:]
}
