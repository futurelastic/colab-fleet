// Command colab-fleetd runs the machine-local colab-fleet service. In this
// phase it wires up only the no-op stub driver (internal/drivers/stub) —
// see that package's doc comment and NOTES.local.md's sequencing for why
// no real driver exists yet.
package main

import (
	"context"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	fleet "github.com/godx-jp/colab-fleet"
	"github.com/godx-jp/colab-fleet/internal/drivers/stub"
	"github.com/godx-jp/colab-fleet/internal/service"
)

func main() {
	self := fleet.MachineId(getenv("FLEET_MACHINE", "local"))

	// There is no unauthenticated mode (api-http.md §5) — not for
	// loopback, not for development. A missing token is a refusal to
	// start, not a fallback to open.
	token := os.Getenv("FLEET_TOKEN")
	if token == "" {
		log.Fatal("colab-fleetd: FLEET_TOKEN must be set — there is no unauthenticated mode (api-http.md §5)")
	}

	svc := service.New(self)
	if err := svc.RegisterLocalDriver("stub", &stub.Driver{DeadlineMs: 5000}); err != nil {
		log.Fatalf("colab-fleetd: %v", err)
	}

	mux := service.NewMux(svc, service.Config{Token: token})

	// Bind narrowly by default (§6.1: "Default to loopback. Exposure
	// beyond it is explicit configuration, never a side effect of
	// enabling federation.") No specific host or port is hardcoded here —
	// the fleet's actual port assignment is an operational fact, not a
	// specification one; see NOTES.local.md.
	addr := getenv("FLEET_ADDR", "127.0.0.1:0")

	ln, err := net.Listen("tcp", addr)
	if err != nil {
		log.Fatalf("colab-fleetd: listen %s: %v", addr, err)
	}
	log.Printf("colab-fleetd: listening on %s (machine=%s)", ln.Addr(), self)

	srv := &http.Server{Handler: mux}

	go func() {
		if err := srv.Serve(ln); err != nil && err != http.ErrServerClosed {
			log.Fatalf("colab-fleetd: serve: %v", err)
		}
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	<-stop

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = srv.Shutdown(ctx)
}

func getenv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
