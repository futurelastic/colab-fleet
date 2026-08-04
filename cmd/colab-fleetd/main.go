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
//	FLEET_STATE_DIR        directory for durable state: idempotency keys
//	                       (§10) and the event sequence (§7.3). Absent means
//	                       in-memory only, which is honest for a throwaway
//	                       instance and is the defect D5 described for a real
//	                       one. Created if missing, mode 0700.
//	FLEET_CONFIG           path to a JSON file carrying the principal table
//	                       and per-peer credentials (§6). When present it is
//	                       authoritative and FLEET_TOKEN / FLEET_ALLOW_* are
//	                       ignored. Absent means single-token mode.
//	FLEET_ALLOW_MUTATIONS  set to 1 to permit create/input/interrupt/close
//	                       against sessions ON THIS MACHINE. Defaults OFF.
//	FLEET_ALLOW_RELAY      set to 1 to permit forwarding a mutation to a
//	                       PEER. Defaults OFF. Separate from the above on
//	                       purpose: a hardened host can still be a
//	                       full-featured client (§6, defect D6).
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
	"github.com/godx-jp/colab-fleet/internal/state"
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

	// A principal table, when configured, decides everything about who may
	// do what (§6). Without one the service runs in single-token mode,
	// which is honest for one machine and unstatable for a fleet.
	var cfgFile *fileConfig
	if path := os.Getenv("FLEET_CONFIG"); path != "" {
		var err error
		cfgFile, err = loadConfig(path)
		if err != nil {
			log.Fatalf("colab-fleetd: %v", err)
		}
	}

	// Durable state (§7.3, §10, §12). Nil is a valid configuration; a
	// configured directory that cannot be opened is not, because starting
	// anyway would silently be the in-memory behaviour this replaces.
	store, err := state.Open(os.Getenv("FLEET_STATE_DIR"))
	if err != nil {
		log.Fatalf("colab-fleetd: %v", err)
	}
	if store != nil {
		log.Printf("colab-fleetd: state directory %s", store.Dir())
	}

	svc, err := service.NewWithState(self, store)
	if err != nil {
		log.Fatalf("colab-fleetd: %v", err)
	}
	// The service's own authority for long-lived peer reads (event
	// subscriptions, §14 D9). Never used for a proxied unary call — those
	// authenticate as this machine and assert the caller (§6, §13).
	svc.SetPeerCredential(token)

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
		opts := []tmux.Option{tmux.WithState(store)}
		if bin := os.Getenv("FLEET_TMUX_BIN"); bin != "" {
			opts = append(opts, tmux.WithBinary(bin))
		}
		d := tmux.New(self, opts...)
		// An unreadable key table is surfaced, never absorbed: continuing
		// with an empty one is exactly the behaviour §10 calls a disaster.
		if err := d.StateError(); err != nil {
			log.Fatalf("colab-fleetd: %v", err)
		}
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
	peerSpecs := splitList(os.Getenv("FLEET_PEERS"))
	if cfgFile != nil && len(peerSpecs) == 0 {
		for _, p := range cfgFile.Peers {
			peerSpecs = append(peerSpecs, p.Machine+"="+p.URL)
		}
	}
	for _, spec := range peerSpecs {
		name, base, ok := strings.Cut(spec, "=")
		if !ok || name == "" || base == "" {
			log.Fatalf("colab-fleetd: bad FLEET_PEERS entry %q (want name=url)", spec)
		}
		machine := fleet.MachineId(strings.TrimSpace(name))
		if machine == self {
			log.Fatalf("colab-fleetd: peer %q is this machine; fan-out is one hop and peers never recurse (§13.1)", machine)
		}
		// No credential is handed to the peer driver, and none exists to
		// hand: it presents the authority of whoever made the request
		// (§13). A proxy holding its own identity is the confused deputy
		// this design forbids.
		opts := []remote.Option{remote.WithDeadline(3 * time.Second)}
		// The credential THIS machine holds on that peer. Distinct from
		// anything a caller presents here, and distinct from the peer's
		// own credential on us — conflating those is how a fleet ends up
		// with one shared secret again by accident.
		if cfgFile != nil {
			if _, tok, ok := cfgFile.peerFor(machine); ok && tok != "" {
				opts = append(opts, remote.WithIdentity(tok))
			}
		}
		peer := remote.New(machine, strings.TrimSpace(base), opts...)
		if err := svc.RegisterPeerDriver(machine, peer); err != nil {
			log.Fatalf("colab-fleetd: registering peer %q: %v", machine, err)
		}
		log.Printf("colab-fleetd: peer %s configured", machine)

		// Learn the peer's declared deadline so this driver does not
		// abandon calls the peer would have completed (§14 D7). Best
		// effort: a peer that is down stays configured (§5.7), and the
		// driver falls back to its floor until the peer answers.
		go func(p *remote.Driver, m fleet.MachineId) {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			if err := p.RefreshCapabilities(ctx, fleet.Request{
				Caller: fleet.Caller{Principal: "system:self", Credential: token},
			}); err != nil {
				log.Printf("colab-fleetd: peer %s capabilities unknown for now: %v", m, err)
				return
			}
			log.Printf("colab-fleetd: peer %s deadline learned: %dms",
				m, p.Capabilities().DeadlineMs)
		}(peer, machine)
	}

	// Two independent grants (§6, and defect D6 for why one was not enough):
	// what this HOST exposes, and what this instance may do as a CLIENT.
	allowLocal := os.Getenv("FLEET_ALLOW_MUTATIONS") == "1"
	allowRelay := os.Getenv("FLEET_ALLOW_RELAY") == "1"
	log.Printf("colab-fleetd: local mutations=%v · relay to peers=%v (§6; both default off)",
		allowLocal, allowRelay)
	svcCfg := service.Config{
		Token:               token,
		AllowLocalMutations: allowLocal,
		AllowPeerRelay:      allowRelay,
	}
	if cfgFile != nil {
		principals, err := cfgFile.principals()
		if err != nil {
			log.Fatalf("colab-fleetd: %v", err)
		}
		svcCfg.Principals = principals
		for _, p := range principals {
			log.Printf("colab-fleetd: principal %q grants=%v", p.Name, p.Grants)
		}
	}
	mux := service.NewMux(svc, svcCfg)

	// --- reconciliation (§12) ------------------------------------------
	//
	// Startup is reconciliation, not initialisation: sessions outlive the
	// service that manages them. Nothing is destroyed here, and nothing
	// can be — this only reports what was found, which is the whole of
	// rule 4 ("a session the service cannot explain is a session for a
	// human to look at, not one to clean up").
	if rec, ok := localDriver.(interface {
		Reconcile(context.Context) (tmux.Reconciliation, error)
	}); ok {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		got, err := rec.Reconcile(ctx)
		cancel()
		if err != nil {
			log.Printf("colab-fleetd: reconciliation failed: %v", err)
		} else {
			log.Printf("colab-fleetd: reconciled — %s", got)
			// Orphans and disappearances are named individually. §12 rule 4
			// forbids acting on them, which makes reporting them the entire
			// value: a session this service cannot explain is one for a
			// human to look at, and a human cannot look at a count.
			for _, s := range got.Orphaned {
				log.Printf("colab-fleetd:   orphaned %q cwd=%s (%s)", s.ID, s.Cwd, s.State.Evidence)
			}
			for _, s := range got.Vanished {
				log.Printf("colab-fleetd:   vanished %q (%s)", s.ID, s.State.Evidence)
			}
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
