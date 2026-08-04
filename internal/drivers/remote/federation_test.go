package remote

import (
	"context"
	"errors"
	"net/http/httptest"
	"os"
	"os/exec"
	"testing"
	"time"

	fleet "github.com/godx-jp/colab-fleet"
	"github.com/godx-jp/colab-fleet/internal/driver"
	"github.com/godx-jp/colab-fleet/internal/drivers/stub"
	tmuxdrv "github.com/godx-jp/colab-fleet/internal/drivers/tmux"
	"github.com/godx-jp/colab-fleet/internal/service"
)

// These tests assemble the whole federation path in one process:
//
//	caller -> Service A -> remote.Driver -> HTTP -> Service B -> real driver
//
// Service A holds no local driver and one peer. Service B holds a real
// driver and answers for itself. Neither service is modified for the
// occasion, and A never learns that its peer is remote — it registers a
// driver.Driver and calls it.
//
// That is §4.2's claim stated as an executable assertion: "cross-machine
// operation is not a feature layered on top of the abstraction — it is one
// implementation of it." If the interface could not express a session on
// another machine, this file would not compile, or it would need a special
// case somewhere. It needs neither.
//
// One machine is enough to test this. Two machines would additionally test
// the network; they would not test the design any harder.

// peerService builds Service B: a real service, serving a real driver over
// a real HTTP surface.
// allowMutations must be set explicitly by any test that exercises a
// mutating verb: the service refuses them by default (§6 requirement 3).
// Without it, a mutation is refused as "unauthorized" — the same wire kind a
// genuine authority failure produces, which would make a test asserting on
// authority propagation pass or fail for the wrong reason.
func peerService(t *testing.T, self fleet.MachineId, runtime fleet.RuntimeId, d driver.Driver, token string, allowMutations bool) string {
	t.Helper()
	svcB := service.New(self)
	if err := svcB.RegisterLocalDriver(runtime, d); err != nil {
		t.Fatalf("registering driver on the peer: %v", err)
	}
	srv := httptest.NewServer(service.NewMux(svcB, service.Config{
		Token: token, AllowMutations: allowMutations,
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

// homeService builds Service A: no local drivers, one peer reached through
// the remote driver.
func homeService(t *testing.T, self fleet.MachineId, peer fleet.MachineId, rd *Driver) *service.Service {
	t.Helper()
	svcA := service.New(self)
	if err := svcA.RegisterPeerDriver(peer, rd); err != nil {
		t.Fatalf("registering the remote driver as a peer: %v", err)
	}
	return svcA
}

// The structural claim, with a driver that supports nothing: a peer's
// "unsupported" must arrive as unsupported, not as an empty result and not
// as a generic failure. This runs everywhere, including CI.
func TestFederatedUnsupportedSurvivesTheWholePath(t *testing.T) {
	const token = "federation-token"
	base := peerService(t, "peerbox", "stub", &stub.Driver{}, token, false)

	rd := New("peerbox", base, WithDeadline(2*time.Second))
	svcA := homeService(t, "homebox", "peerbox", rd)

	got, err := svcA.ListSessions(context.Background(), fleetCaller(token), service.ScopeFleet,
		driver.ListFilter{}, 0)
	if err != nil {
		t.Fatalf("fleet-scoped list failed outright: %v", err)
	}

	// §5.7 at the top of the whole stack: the peer answered, and said it
	// cannot do this. That is not an empty fleet.
	if got.Complete() {
		t.Error("a peer that cannot answer must not produce a complete envelope")
	}
	var sawPeer bool
	for _, src := range got.Sources() {
		if src.Machine == "peerbox" {
			sawPeer = true
			if src.Status == fleet.SourceOK {
				t.Errorf("peer reported ok despite its driver supporting nothing")
			}
			if src.Error == "" {
				t.Error("peer source carries no explanation")
			}
		}
	}
	if !sawPeer {
		t.Fatalf("the peer contributed no SourceStatus at all; sources=%+v", got.Sources())
	}
}

// The authority rule, end to end and through a real HTTP surface.
func TestFederatedMutationCarriesTheOriginalCallersAuthority(t *testing.T) {
	const token = "federation-token"
	base := peerService(t, "peerbox", "stub", &stub.Driver{}, token, true) // exercises a mutating verb
	rd := New("peerbox", base)

	// No authority: refused before a request is ever made. There is no
	// proxy credential to fall back to — that is the point of the fix.
	_, err := rd.Send(context.Background(), fleet.Request{Caller: fleet.Caller{Principal: "addr:test"}},
		fleet.SessionRef{Machine: "peerbox", ID: "x"}, "hello", driver.SendOptions{})
	if !errors.Is(err, ErrNoCallerAuthority) {
		t.Fatalf("want ErrNoCallerAuthority, got %v", err)
	}

	// With the caller's own credential, the request reaches the peer's
	// driver and gets that driver's honest answer rather than an auth
	// failure.
	_, err = rd.Send(context.Background(), fleetCaller(token),
		fleet.SessionRef{Machine: "peerbox", ID: "x"}, "hello", driver.SendOptions{})
	var fe *fleet.Error
	if !errors.As(err, &fe) {
		t.Fatalf("want a typed wire error, got %v", err)
	}
	if fe.Kind == fleet.ErrorUnauthorized {
		t.Errorf("the caller's credential was rejected; authority did not survive proxying")
	}
	if fe.Kind != fleet.ErrorUnsupported {
		t.Logf("note: peer answered %q (%s)", fe.Kind, fe.Message)
	}
}

// fleetCaller is what a service derives from an inbound request: the
// principal that asked, and the credential they presented.
func fleetCaller(tok string) fleet.Request {
	return fleet.Request{Caller: fleet.Caller{Principal: "addr:test", Credential: tok}}
}

// A peer that is down must degrade the fleet view, never fail it, and never
// vanish from it (§5.7, §4.4).
func TestFederatedUnreachablePeerDegradesRatherThanFails(t *testing.T) {
	const token = "federation-token"
	rd := New("peerbox", "http://127.0.0.1:1", WithDeadline(300*time.Millisecond))
	svcA := homeService(t, "homebox", "peerbox", rd)

	started := time.Now()
	got, err := svcA.ListSessions(context.Background(), fleetCaller(token), service.ScopeFleet, driver.ListFilter{}, 0)
	elapsed := time.Since(started)

	if err != nil {
		t.Fatalf("one dead peer must not fail a fleet-wide query: %v", err)
	}
	if elapsed > 5*time.Second {
		t.Errorf("took %s to give up on an unreachable peer", elapsed)
	}
	if got.Complete() {
		t.Error("envelope reported complete with a dead peer in it")
	}
	var found bool
	for _, src := range got.Sources() {
		if src.Machine == "peerbox" {
			found = true
			if src.Status == fleet.SourceOK {
				t.Error("a dead peer reported ok")
			}
		}
	}
	if !found {
		t.Error("the dead peer vanished from sources instead of appearing as unreachable")
	}
}

// The one that matters: a real driver, over a real HTTP surface, seen
// through the remote driver, carrying real sessions.
//
// Read-only. It lists; it never creates, sends, interrupts or closes.
func TestFederatedListOfRealSessionsThroughARemoteDriver(t *testing.T) {
	if os.Getenv("FLEET_TMUX_INTEGRATION") != "1" {
		t.Skip("set FLEET_TMUX_INTEGRATION=1 to run against the live multiplexer")
	}
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("no multiplexer on PATH")
	}
	if err := exec.Command("tmux", "has-session").Run(); err != nil {
		t.Skip("no multiplexer server running")
	}

	const token = "federation-token"
	local := tmuxdrv.New("peerbox")
	base := peerService(t, "peerbox", tmuxdrv.DefaultRuntime, local, token, false) // read-only

	rd := New("peerbox", base, WithDeadline(10*time.Second))
	svcA := homeService(t, "homebox", "peerbox", rd)

	// What the driver sees directly, on the machine it runs on.
	direct, err := local.List(context.Background(), fleetCaller(token), driver.ListFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(direct.Items()) == 0 {
		t.Skip("no sessions to look at")
	}

	// What a caller sees through service -> remote driver -> HTTP -> service.
	viaFleet, err := svcA.ListSessions(context.Background(), fleetCaller(token), service.ScopeFleet,
		driver.ListFilter{}, 0)
	if err != nil {
		t.Fatalf("federated list failed: %v", err)
	}

	if len(viaFleet.Items()) != len(direct.Items()) {
		t.Errorf("federated view has %d sessions, direct view has %d",
			len(viaFleet.Items()), len(direct.Items()))
	}

	// Statuses must survive the round trip intact — including the
	// confidence, which is the field §5.6 exists to protect. A federation
	// layer that upgraded "inferred" to "observed" in transit would destroy
	// the exact distinction the design is built around.
	byID := map[string]fleet.Session{}
	for _, s := range direct.Items() {
		byID[s.ID] = s
	}
	for _, s := range viaFleet.Items() {
		want, ok := byID[s.ID]
		if !ok {
			t.Errorf("federated view invented a session %q", s.ID)
			continue
		}
		if s.State.Confidence != fleet.ConfidenceInferred {
			t.Errorf("session %q came back as %q through federation; the peer "+
				"reported inferred (§5.6)", s.ID, s.State.Confidence)
		}
		if s.Cwd != want.Cwd {
			t.Errorf("session %q: cwd %q through federation, %q directly", s.ID, s.Cwd, want.Cwd)
		}
		if s.State.Evidence == "" {
			t.Errorf("session %q lost its evidence in transit (§2.3)", s.ID)
		}
		if s.Machine != "peerbox" {
			t.Errorf("session %q reports machine %q, want peerbox", s.ID, s.Machine)
		}
	}

	// And the peer's own SourceStatus was adopted, not manufactured.
	if len(viaFleet.Sources()) == 0 {
		t.Fatal("no sources in the federated envelope")
	}
	var peerSrc *fleet.SourceStatus
	for i := range viaFleet.Sources() {
		if viaFleet.Sources()[i].Machine == "peerbox" {
			peerSrc = &viaFleet.Sources()[i]
		}
	}
	if peerSrc == nil {
		t.Fatalf("peer contributed no source; got %+v", viaFleet.Sources())
	}
	if peerSrc.Count == nil {
		t.Error("the peer's own count was dropped rather than adopted (§13.2)")
	}
	t.Logf("federated fleet view: %d sessions from %q, source=%q",
		len(viaFleet.Items()), peerSrc.Machine, peerSrc.Status)
}
