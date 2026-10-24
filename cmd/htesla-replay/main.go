// htesla-replay is a DEVELOPMENT tool: it plays recorded or generated
// telemetry into a running htesla receiver over the same mutual-TLS
// WebSocket a car uses, so the whole path — receiver → decode → chunks
// → builder → Hearthloom — can be exercised without a car. It only
// works against a sidecar started with HTESLA_DEV_CLIENT_CA (the dev CA
// it makes) and HTESLA_DEV_VINS; a real node trusts Tesla's CAs only.
//
//	htesla-replay certs -dir DIR -vin VIN          dev CA + a car client cert
//	htesla-replay gen -day 2026-10-22 -tz America/New_York -vin VIN > day.jsonl
//	htesla-replay send -addr 127.0.0.1:18444 -servername HOST -server-ca ca.pem \
//	    -cert DIR/car.pem -key DIR/car.key -vin VIN day.jsonl
//
// Fixture lines: {"t":"<RFC3339>","data":{"Gear":"ShiftStateD","Location":
// {"latitude":…,"longitude":…},"Odometer":1234.5,…}} or {"disconnect":true}.
package main

import (
	"bufio"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"flag"
	"fmt"
	"math"
	"math/big"
	"os"
	"path/filepath"
	"time"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"
	"github.com/teslamotors/fleet-telemetry/messages"
	"github.com/teslamotors/fleet-telemetry/protos"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: htesla-replay certs|gen|send …")
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "certs":
		err = certsCmd(os.Args[2:])
	case "gen":
		err = genCmd(os.Args[2:])
	case "send":
		err = sendCmd(os.Args[2:])
	default:
		err = fmt.Errorf("unknown command %q", os.Args[1])
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "htesla-replay:", err)
		os.Exit(1)
	}
}

/* ———— certs ———— */

func writePEM(path, typ string, der []byte) error {
	return os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: typ, Bytes: der}), 0o600)
}

func certsCmd(args []string) error {
	fs := flag.NewFlagSet("certs", flag.ExitOnError)
	dir := fs.String("dir", ".", "output directory")
	vin := fs.String("vin", "", "the replay car's VIN")
	_ = fs.Parse(args)
	if *vin == "" {
		return fmt.Errorf("-vin is required")
	}
	if err := os.MkdirAll(*dir, 0o700); err != nil {
		return err
	}
	caKey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	// named like Tesla's product CA so fleet-telemetry's identity rules
	// accept its leaves — trusted ONLY where HTESLA_DEV_CLIENT_CA says so
	caT := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()), Subject: pkix.Name{CommonName: "Tesla Motors Products CA", Organization: []string{"htesla replay (development)"}},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(7 * 24 * time.Hour),
		KeyUsage: x509.KeyUsageCertSign, BasicConstraintsValid: true, IsCA: true,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caT, caT, &caKey.PublicKey, caKey)
	if err != nil {
		return err
	}
	ca, _ := x509.ParseCertificate(caDER)
	carKey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	carT := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano() + 1), Subject: pkix.Name{CommonName: *vin},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(7 * 24 * time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}
	carDER, err := x509.CreateCertificate(rand.Reader, carT, ca, &carKey.PublicKey, caKey)
	if err != nil {
		return err
	}
	carKeyDER, _ := x509.MarshalECPrivateKey(carKey)
	for _, f := range []struct {
		name, typ string
		der       []byte
	}{{"devca.pem", "CERTIFICATE", caDER}, {"car.pem", "CERTIFICATE", carDER}, {"car.key", "EC PRIVATE KEY", carKeyDER}} {
		if err := writePEM(filepath.Join(*dir, f.name), f.typ, f.der); err != nil {
			return err
		}
	}
	fmt.Printf("wrote devca.pem, car.pem, car.key to %s\n", *dir)
	return nil
}

/* ———— gen: a plausible day ———— */

type line struct {
	T          string         `json:"t,omitempty"`
	Data       map[string]any `json:"data,omitempty"`
	Disconnect bool           `json:"disconnect,omitempty"`
}

func genCmd(args []string) error {
	fs := flag.NewFlagSet("gen", flag.ExitOnError)
	dayS := fs.String("day", "", "local day, YYYY-MM-DD")
	tzS := fs.String("tz", "America/New_York", "zone the day is in")
	_ = fs.String("vin", "", "unused (the sender sets the VIN)")
	_ = fs.Parse(args)
	tz, err := time.LoadLocation(*tzS)
	if err != nil {
		return err
	}
	day, err := time.ParseInLocation("2006-01-02", *dayS, tz)
	if err != nil {
		return fmt.Errorf("-day: %w", err)
	}
	out := json.NewEncoder(os.Stdout)
	at := func(h, m float64) time.Time { return day.Add(time.Duration((h*60 + m) * float64(time.Minute))) }
	emit := func(t time.Time, d map[string]any) { _ = out.Encode(line{T: t.UTC().Format(time.RFC3339), Data: d}) }
	loc := func(lat, lon float64) map[string]any { return map[string]any{"latitude": lat, "longitude": lon} }
	home, work := [2]float64{42.3736, -71.1097}, [2]float64{42.3601, -71.0589}
	odo, energy, soc := 12345.0, 62.0, 78.0

	emit(at(7, 50), map[string]any{"Gear": "ShiftStateP", "Location": loc(home[0], home[1]), "Odometer": odo,
		"EnergyRemaining": energy, "BatteryLevel": soc, "LocatedAtHome": true, "LocatedAtWork": false,
		"VehicleName": "Ember", "CarType": "CarTypeModelY", "Version": "2026.32.1 a1b2c3d4",
		"DetailedChargeState": "DetailedChargeStateDisconnected", "GuestModeEnabled": false, "OutsideTemp": 9.0})
	drive := func(from, to [2]float64, h, m float64, minutes int, miles float64, kwh float64, songs [][2]string) {
		emit(at(h, m), map[string]any{"Gear": "ShiftStateD"})
		for i := 1; i <= minutes*6; i++ { // a fix every 10 s
			f := float64(i) / float64(minutes*6)
			wig := 0.0015 * math.Sin(f*math.Pi*3)
			d := map[string]any{
				"Location":     loc(from[0]+(to[0]-from[0])*f+wig, from[1]+(to[1]-from[1])*f),
				"VehicleSpeed": 25 + 10*math.Sin(f*7),
			}
			if i%6 == 0 {
				d["Odometer"] = odo + miles*f
				d["EnergyRemaining"] = energy - kwh*f
			}
			if i%18 == 0 {
				d["OutsideTemp"] = 9.0 + 3*f
			}
			if k := (i - 1) * len(songs) / (minutes * 6); k != (i-2)*len(songs)/(minutes*6) || i == 1 {
				d["MediaNowPlayingTitle"], d["MediaNowPlayingArtist"] = songs[k][0], songs[k][1]
			}
			switch i {
			case minutes * 3: // a short stop at a light: Park for 40 s
				d["Gear"] = "ShiftStateP"
				d["VehicleSpeed"] = 0.0
			case minutes*3 + 4:
				d["Gear"] = "ShiftStateD"
			}
			emit(at(h, m).Add(time.Duration(i)*10*time.Second), d)
		}
		odo += miles
		energy -= kwh
		soc -= kwh / 75 * 100
		end := at(h, m).Add(time.Duration(minutes) * time.Minute)
		emit(end.Add(5*time.Second), map[string]any{"Gear": "ShiftStateP", "VehicleSpeed": 0.0, "Odometer": odo,
			"EnergyRemaining": energy, "BatteryLevel": math.Round(soc),
			"LocatedAtHome": to == home, "LocatedAtWork": to == work})
		emit(end.Add(4*time.Minute), map[string]any{"Locked": true})
	}
	drive(home, work, 8, 5, 28, 4.6, 1.4, [][2]string{{"So What", "Miles Davis"}, {"Blue in Green", "Miles Davis"}})
	drive(work, home, 17, 40, 34, 4.9, 1.6, [][2]string{{"Clair de Lune", "Debussy"}})
	// an evening charge at home, with a scheduled pause
	emit(at(19, 0), map[string]any{"DetailedChargeState": "DetailedChargeStateStarting"})
	emit(at(19, 1), map[string]any{"DetailedChargeState": "DetailedChargeStateCharging", "ACChargingEnergyIn": 0.0})
	added := 0.0
	for i := 1; i <= 24; i++ {
		added += 0.7
		emit(at(19, 1+float64(i)*10), map[string]any{"ACChargingEnergyIn": added, "BatteryLevel": math.Min(100, math.Round(soc+added/75*100))})
	}
	emit(at(23, 10), map[string]any{"DetailedChargeState": "DetailedChargeStateComplete"})
	// an over-the-air update overnight
	emit(at(23, 40), map[string]any{"Version": "2026.38.6 e5f6a7b8"})
	_ = out.Encode(line{Disconnect: true})
	return nil
}

/* ———— send ———— */

func value(v any) (*protos.Value, error) {
	switch x := v.(type) {
	case bool:
		return &protos.Value{Value: &protos.Value_BooleanValue{BooleanValue: x}}, nil
	case float64:
		return &protos.Value{Value: &protos.Value_DoubleValue{DoubleValue: x}}, nil
	case map[string]any:
		la, _ := x["latitude"].(float64)
		lo, _ := x["longitude"].(float64)
		return &protos.Value{Value: &protos.Value_LocationValue{LocationValue: &protos.LocationValue{Latitude: la, Longitude: lo}}}, nil
	case string:
		// an enum value name (ShiftStateD, DetailedChargeStateCharging…)?
		val := &protos.Value{}
		m := val.ProtoReflect()
		od := m.Descriptor().Oneofs().ByName("value")
		for i := 0; i < od.Fields().Len(); i++ {
			fd := od.Fields().Get(i)
			if fd.Kind() != protoreflect.EnumKind {
				continue
			}
			if ev := fd.Enum().Values().ByName(protoreflect.Name(x)); ev != nil {
				m.Set(fd, protoreflect.ValueOfEnum(ev.Number()))
				return val, nil
			}
		}
		return &protos.Value{Value: &protos.Value_StringValue{StringValue: x}}, nil
	}
	return nil, fmt.Errorf("can't encode %T", v)
}

func sendCmd(args []string) error {
	fs := flag.NewFlagSet("send", flag.ExitOnError)
	addr := fs.String("addr", "127.0.0.1:8443", "receiver address")
	server := fs.String("servername", "", "the receiver certificate's host")
	serverCA := fs.String("server-ca", "", "the receiver's CA (the ember's tls/ca.pem)")
	certF := fs.String("cert", "car.pem", "car client certificate")
	keyF := fs.String("key", "car.key", "car client key")
	vin := fs.String("vin", "", "the car's VIN (as in its certificate)")
	_ = fs.Parse(args)
	if fs.NArg() != 1 || *vin == "" || *server == "" || *serverCA == "" {
		return fmt.Errorf("send needs -vin -servername -server-ca and a fixture file")
	}
	caPEM, err := os.ReadFile(*serverCA)
	if err != nil {
		return err
	}
	roots := x509.NewCertPool()
	roots.AppendCertsFromPEM(caPEM)
	cert, err := tls.LoadX509KeyPair(*certF, *keyF)
	if err != nil {
		return err
	}
	dial := func() (*websocket.Conn, error) {
		d := websocket.Dialer{HandshakeTimeout: 10 * time.Second, TLSClientConfig: &tls.Config{
			RootCAs: roots, ServerName: *server, Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12}}
		ws, _, err := d.Dial("wss://"+*addr+"/", map[string][]string{"Version": {"replay"}})
		return ws, err
	}
	f, err := os.Open(fs.Arg(0))
	if err != nil {
		return err
	}
	defer f.Close()
	ws, err := dial()
	if err != nil {
		return fmt.Errorf("connecting: %w", err)
	}
	sent, acked := 0, 0
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), 1<<20)
	for sc.Scan() {
		var l line
		if err := json.Unmarshal(sc.Bytes(), &l); err != nil {
			return fmt.Errorf("fixture line %d: %w", sent+1, err)
		}
		if l.Disconnect {
			_ = ws.Close()
			ws = nil
			continue
		}
		if ws == nil {
			if ws, err = dial(); err != nil {
				return fmt.Errorf("reconnecting: %w", err)
			}
		}
		t, err := time.Parse(time.RFC3339, l.T)
		if err != nil {
			return err
		}
		payload := &protos.Payload{CreatedAt: timestamppb.New(t)}
		for k, v := range l.Data {
			key, ok := protos.Field_value[k]
			if !ok {
				return fmt.Errorf("unknown field %q", k)
			}
			val, err := value(v)
			if err != nil {
				return fmt.Errorf("%s: %w", k, err)
			}
			payload.Data = append(payload.Data, &protos.Datum{Key: protos.Field(key), Value: val})
		}
		pb, _ := proto.Marshal(payload)
		sm := messages.StreamMessage{TXID: []byte(uuid.NewString()), SenderID: []byte("vehicle_device." + *vin),
			MessageTopic: []byte("V"), Payload: pb, CreatedAt: uint32(t.Unix()),
			DeviceType: []byte("vehicle_device"), DeviceID: []byte(*vin)}
		msg, _ := sm.ToBytes()
		if err := ws.WriteMessage(websocket.BinaryMessage, msg); err != nil {
			return err
		}
		sent++
		_ = ws.SetReadDeadline(time.Now().Add(10 * time.Second))
		_, reply, err := ws.ReadMessage()
		if err != nil {
			return fmt.Errorf("waiting for the ack of record %d: %w", sent, err)
		}
		if ack, err := messages.StreamAckMessageFromBytes(reply); err == nil && string(ack.TXID) == string(sm.TXID) {
			acked++
		}
	}
	if ws != nil {
		_ = ws.Close()
	}
	fmt.Printf("sent %d records, %d acked\n", sent, acked)
	if acked != sent {
		return fmt.Errorf("%d records weren't acked", sent-acked)
	}
	return sc.Err()
}
