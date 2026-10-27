// htesla — a Hearthloom ember that reads your Tesla's driving,
// location, charging and software data from Tesla's servers and the
// car's own telemetry stream. Read-only: it can't unlock, drive or
// command the car.
//
// The daemon starts this binary with HEARTHLOOM_URL,
// HEARTHLOOM_PLUGIN_KEY_FILE and HEARTHLOOM_PLUGIN_NAME; its output
// lands in the ember's plugin.log.
package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/signal"
	"strings"
	"syscall"
	_ "time/tzdata" // zone names work on minimal hosted images

	"github.com/metavert-io/htesla/internal/app"
	"github.com/metavert-io/htesla/internal/hearth"
	"github.com/metavert-io/htesla/third_party/fleettelemetry"
)

// Version is stamped by build.sh (-ldflags "-X main.Version=…").
var Version = "0.2.2"

func main() {
	if len(os.Args) > 1 && (os.Args[1] == "-version" || os.Args[1] == "--version") {
		fmt.Println("htesla", Version)
		return
	}
	logger := log.New(os.Stdout, "", log.LstdFlags|log.LUTC)
	h, err := hearth.New()
	if err != nil {
		logger.Fatalf("htesla: %v", err)
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	cas := fleettelemetry.TeslaCAs()
	a := &app.App{
		H:        h,
		TeslaCAs: cas,
		Version:  Version,
		Logf:     logger.Printf,
	}
	// Development replays only (cmd/htesla-replay): trust one extra
	// client CA and VIN list, the way fleet-telemetry's own tls.ca_file
	// adds a CA. Only whoever controls the daemon's environment can set
	// these; a real node never does.
	if path := os.Getenv("HTESLA_DEV_CLIENT_CA"); path != "" {
		pem, err := os.ReadFile(path)
		if err != nil || !cas.AppendCertsFromPEM(pem) {
			logger.Fatalf("htesla: HTESLA_DEV_CLIENT_CA %s didn't load", path)
		}
		logger.Printf("DEVELOPMENT: the receiver also trusts client certs from %s", path)
	}
	if vins := os.Getenv("HTESLA_DEV_VINS"); vins != "" {
		for _, v := range strings.Split(vins, ",") {
			if v = strings.TrimSpace(v); v != "" {
				a.ExtraVINs = append(a.ExtraVINs, v)
			}
		}
		logger.Printf("DEVELOPMENT: the receiver also accepts %d replay VIN(s)", len(a.ExtraVINs))
	}
	logger.Printf("htesla %s starting (daemon %s)", Version, h.Base)
	if err := a.Run(ctx); err != nil {
		logger.Fatalf("htesla: %v", err)
	}
	logger.Printf("htesla stopped")
}
