// Command colab-fleetd runs the machine-local colab-fleet service.
//
// # Configuration
//
// Everything is environment-driven, because the operational facts — which
// address, which port, which peers — are machine-specific and do not belong
// in a public repository.
//
//	FLEET_MACHINE          this machine's id in the fleet. Required in
//	                       practice; defaults to "local", which is only
//	                       useful for a single-machine trial.
//	FLEET_TOKEN            shared bearer token. Required. No default, no
//	                       unauthenticated mode (api-http.md §5).
//	FLEET_ADDR             listen address. Defaults to loopback on an
//	                       ephemeral port (§6.1: exposure beyond loopback is
//	                       explicit configuration, never a side effect).
//	                       Bind a specific interface, never 0.0.0.0.
//	FLEET_RUNTIME          "tmux" (default) or "stub".
//	FLEET_TMUX_BIN         path to the multiplexer binary. Defaults to
//	                       "tmux" on PATH — which a non-interactive ssh
//	                       session may not have.
//	FLEET_PEERS            comma-separated name=url list, e.g.
//	                       "other=https://other.example:PORT". Peers are
//	                       statically configured; there is no discovery
//	                       (§7.2), and the address is one the OPERATOR has
//	                       confirmed reachable from THIS machine — never the
//	                       peer's own idea of its name.
//	FLEET_ALLOW_MUTATIONS  set to 1 to permit create/input/interrupt/close.
//	                       Defaults OFF (§6 requirement 3).
package main

import (
	"context"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	fleet "github.com/godx-jp/colab-fleet"
	"github.com/godx-jp/colab-fleet/internal/driver"
	"github.com/godx-jp/colab-fleet/internal/drivers/remote"
	"github.com/godx-jp/colab-fleet/internal/drivers/stub"
	"github.com/godx-jp/colab-fleet/internal/drivers/tmux"
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

	// --- local runtime -------------------------------------------------
	var (
		localDriver driver.Driver
		runtimeID   fleet.RuntimeId
	)
	switch name := getenv("FLEET_RUNTIME", "tmux"); name {
	case "tmux":
		// FLEET_TMUX_BIN exists because a non-interactive ssh session gets a
		// bare PATH (/usr/bin:/bin:/usr/sbin:/sbin on the machines this was
		// deployed to), and the multiplexer lives in a package manager's
		// prefix that differs by architecture. Relying on PATH resolution
		// here fails only under remote invocation, which is precisely how
		// this service gets deployed.
		var opts []tmux.Option
		if bin := os.Getenv("FLEET_TMUX_BIN"); bin != "" {
			opts = append(opts, tmux.WithBinary(bin))
		}
		d := tmux.New(self, opts...)
		localDriver, runtimeID = d, tmux.DefaultRuntime
	case "stub":
		localDriver, runtimeID = &stub.Driver{DeadlineMs: 5000}, "stub"
	default:
		log.Fatalf("colab-fleetd: unknown FLEET_RUNTIME %q (want tmux or stub)", name)
	}
	if err := svc.RegisterLocalDriver(runtimeID, localDriver); err != nil {
		log.Fatalf("colab-fleetd: registering local runtime: %v", err)
	}

	// --- peers ---------------------------------------------------------
	//
	// A peer that is unreachable right now is still a configured peer:
	// §5.7 requires it to surface as a source reporting unreachable, not to
	// vanish from the fleet because it happened to be down at startup. So
	// registration never probes and never fails on reachability.
	for _, spec := range splitList(os.Getenv("FLEET_PEERS")) {
		name, base, ok := strings.Cut(spec, "=")
		if !ok || name == "" || base == "" {
			log.Fatalf("colab-fleetd: bad FLEET_PEERS entry %q (want name=url)", spec)
		}
		machine := fleet.MachineId(strings.TrimSpace(name))
		if machine == self {
			log.Fatalf("colab-fleetd: peer %q is this machine; fan-out is one hop and peers never recurse (§13.1)", machine)
		}
		peer := remote.New(machine, strings.TrimSpace(base), token,
			remote.WithDeadline(3*time.Second))
		if err := svc.RegisterPeerDriver(machine, peer); err != nil {
			log.Fatalf("colab-fleetd: registering peer %q: %v", machine, err)
		}
		log.Printf("colab-fleetd: peer %s configured", machine)
	}

	allowMutations := os.Getenv("FLEET_ALLOW_MUTATIONS") == "1"
	if !allowMutations {
		log.Print("colab-fleetd: read-only — create/input/interrupt/close are refused (§6; set FLEET_ALLOW_MUTATIONS=1 to permit)")
	}
	mux := service.NewMux(svc, service.Config{Token: token, AllowMutations: allowMutations})

	// --- reconciliation (§12) ------------------------------------------
	//
	// Startup is reconciliation, not initialisation: sessions outlive the
	// service that manages them. Nothing is destroyed here, and nothing
	// can be — this only reports what was found, which is the whole of
	// rule 4 ("a session the service cannot explain is a session for a
	// human to look at, not one to clean up").
	if rec, ok := localDriver.(interface {
		Reconcile(context.Context) (fleet.Collection[fleet.Session], error)
	}); ok {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		found, err := rec.Reconcile(ctx)
		cancel()
		switch {
		case err != nil:
			log.Printf("colab-fleetd: reconciliation failed: %v", err)
		default:
			log.Printf("colab-fleetd: adopted %d existing session(s)", len(found.Items()))
		}
	}

	// Bind narrowly by default (§6.1: "Default to loopback. Exposure
	// beyond it is explicit configuration, never a side effect of
	// enabling federation.") No specific host or port is hardcoded here —
	// the fleet's actual port assignment is an operational fact, not a
	// specification one.
	addr := getenv("FLEET_ADDR", "127.0.0.1:0")
	if strings.HasPrefix(addr, "0.0.0.0:") {
		log.Print("colab-fleetd: WARNING binding 0.0.0.0 — this service can read paths and (when mutations are enabled) start processes; bind a specific interface instead")
	}

	ln, err := net.Listen("tcp", addr)
	if err != nil {
		log.Fatalf("colab-fleetd: listen %s: %v", addr, err)
	}
	log.Printf("colab-fleetd: listening on %s (machine=%s runtime=%s)", ln.Addr(), self, runtimeID)

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

func splitList(raw string) []string {
	var out []string
	for _, p := range strings.Split(raw, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}
