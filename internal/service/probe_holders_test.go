package service

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	fleet "github.com/godx-jp/colab-fleet"
	"github.com/godx-jp/colab-fleet/internal/driver"
	"github.com/godx-jp/colab-fleet/internal/drivers/stub"
)

// colab-fleet issue #63: probeHolders folded driver.ErrUnsupported — a
// driver's firm, permanent "I can never hold a session" — into the same
// inconclusive bucket as a driver that is merely unreachable right now.
// One runtime being down then poisoned bare-id resolution for every
// runtime on the machine, including ones that structurally never held
// anything. These tests pin the fix in place, and — the guardrail #60
// shipped and this must never weaken — prove a driver that really is
// unreachable still makes resolution refuse.

// TestProbeHolders_ErrUnsupportedIsNeitherHolderNorInconclusive is a direct
// unit test of the fold itself: a driver answering driver.ErrUnsupported
// must land in neither returned set, the same as one answering
// fleet.ErrNoSuchSession.
func TestProbeHolders_ErrUnsupportedIsNeitherHolderNorInconclusive(t *testing.T) {
	svc := New("test-machine")
	incapable := &stub.Driver{DeadlineMs: 200}
	local := map[fleet.RuntimeId]driver.Driver{"incapable": incapable}

	holders, inconclusive := svc.probeHolders(context.Background(), fleet.Request{}, local, "anything", 0)
	if len(holders) != 0 {
		t.Errorf("holders = %v, want empty", holders)
	}
	if len(inconclusive) != 0 {
		t.Errorf("inconclusive = %v, want empty — driver.ErrUnsupported is a firm \"not mine\", "+
			"not evidence the probe could not reach a verdict", inconclusive)
	}
}

// TestProbeHolders_TransientFailureIsStillInconclusive is the fold's other
// half, proven at the same grain: an error that is not ErrUnsupported and
// not ErrNoSuchSession — the shape a driver that is merely unreachable
// produces — must still land in inconclusive.
func TestProbeHolders_TransientFailureIsStillInconclusive(t *testing.T) {
	svc := New("test-machine")
	down := &idDriver{Driver: stub.Driver{DeadlineMs: 200},
		failErr: map[string]error{"flaky": fmt.Errorf("driver: connection refused")}}
	local := map[fleet.RuntimeId]driver.Driver{"down": down}

	holders, inconclusive := svc.probeHolders(context.Background(), fleet.Request{}, local, "flaky", 0)
	if len(holders) != 0 {
		t.Errorf("holders = %v, want empty", holders)
	}
	if len(inconclusive) != 1 {
		t.Errorf("inconclusive = %v, want exactly one entry — a transient failure is not \"not mine\"", inconclusive)
	}
}

// TestResolve_StructuralIncapacityNoLongerForcesRefusalOnGenuineMiss is the
// regression #63 exists for, at the resolution grain rather than the probe
// grain. Two local drivers: one structurally incapable (stub — the exact
// driver production runs as FLEET_RUNTIME=stub), one that demonstrably CAN
// be genuinely down (proven on a different, previously-seen id below) but
// answers a never-seen id from its own local knowledge, the same shape
// internal/drivers/opencode's wasSeen check produces without touching a
// dead subprocess. The id under test is a genuine miss — nobody holds it —
// and before #63 the stub's ErrUnsupported alone was enough to make this
// refuse with 400 instead of reaching the configured default, with no
// runtime ever having been unreachable at all.
func TestResolve_StructuralIncapacityNoLongerForcesRefusalOnGenuineMiss(t *testing.T) {
	incapable := &stub.Driver{DeadlineMs: 200}
	sometimesDown := &idDriver{Driver: stub.Driver{DeadlineMs: 200},
		failErr: map[string]error{"previously-seen": fmt.Errorf("driver: connection refused")}}

	svc := New("test-machine")
	if err := svc.RegisterLocalDriver("incapable", incapable); err != nil {
		t.Fatal(err)
	}
	if err := svc.RegisterLocalDriver("sometimes-down", sometimesDown); err != nil {
		t.Fatal(err)
	}
	if err := svc.SetDefaultRuntime("sometimes-down"); err != nil {
		t.Fatal(err)
	}

	// Establish that sometimesDown really can be unreachable — the
	// guardrail case, proven on its own id so it is not a "tidier cousin"
	// standing in for a driver that never actually fails.
	if _, err := sometimesDown.State(context.Background(), fleet.Request{}, fleet.SessionRef{ID: "previously-seen"}); err == nil ||
		errors.Is(err, fleet.ErrNoSuchSession) || errors.Is(err, driver.ErrUnsupported) {
		t.Fatalf("test fixture is not actually down for its own id: %v", err)
	}

	mux := NewMux(svc, Config{Token: testToken, AllowLocalMutations: true})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	resp := getSession(t, srv, "genuine-miss")
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNotFound {
		body, _ := readAll(resp)
		t.Fatalf("status = %d, want 404 — a genuine miss reaching the configured default; body: %s",
			resp.StatusCode, body)
	}
	if got := resp.Header.Get("Fleet-Runtime-Resolution"); got != "default" {
		t.Errorf("Fleet-Runtime-Resolution = %q, want %q", got, "default")
	}
}

// TestResolve_GenuinelyDownDriverStillRefusesAlongsideAnIncapableOne is
// guardrail 3 (#60), proven again in the presence of #63's new
// classification: when the driver that IS unreachable is the one being
// asked about THIS id, resolution must still refuse — an incapable
// sibling being folded out of inconclusive must never be read as license
// to guess past a driver that might actually be holding it.
func TestResolve_GenuinelyDownDriverStillRefusesAlongsideAnIncapableOne(t *testing.T) {
	incapable := &stub.Driver{DeadlineMs: 200}
	down := &idDriver{Driver: stub.Driver{DeadlineMs: 200},
		failErr: map[string]error{"flaky": fmt.Errorf("driver: connection refused")}}

	svc := New("test-machine")
	if err := svc.RegisterLocalDriver("incapable", incapable); err != nil {
		t.Fatal(err)
	}
	if err := svc.RegisterLocalDriver("down", down); err != nil {
		t.Fatal(err)
	}
	if err := svc.SetDefaultRuntime("down"); err != nil {
		t.Fatal(err)
	}

	mux := NewMux(svc, Config{Token: testToken, AllowLocalMutations: true})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	resp := getSession(t, srv, "flaky")
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		body, _ := readAll(resp)
		t.Fatalf("status = %d, want 400 — the down driver's own id must still refuse rather than guess; body: %s",
			resp.StatusCode, body)
	}
}

// TestResolve_UnreachableDriverDoesNotPoisonAHealthyHolder is #55's own
// incident, reproduced: with an incapable driver and a genuinely
// unreachable one both registered alongside a driver that is neither, a
// session the healthy driver plainly holds must resolve — existence,
// proven directly, outranks every other driver's trouble.
func TestResolve_UnreachableDriverDoesNotPoisonAHealthyHolder(t *testing.T) {
	incapable := &stub.Driver{DeadlineMs: 200}
	down := &idDriver{Driver: stub.Driver{DeadlineMs: 200},
		failErr: map[string]error{"abc": fmt.Errorf("driver: connection refused")}}
	healthy := &idDriver{Driver: stub.Driver{DeadlineMs: 200}, held: map[string]bool{"abc": true}}

	svc := New("test-machine")
	if err := svc.RegisterLocalDriver("incapable", incapable); err != nil {
		t.Fatal(err)
	}
	if err := svc.RegisterLocalDriver("down", down); err != nil {
		t.Fatal(err)
	}
	if err := svc.RegisterLocalDriver("healthy", healthy); err != nil {
		t.Fatal(err)
	}

	mux := NewMux(svc, Config{Token: testToken, AllowLocalMutations: true})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	resp := getSession(t, srv, "abc")
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := readAll(resp)
		t.Fatalf("status = %d, want 200 — a driver that plainly holds the id must win regardless of "+
			"an incapable or an unreachable sibling; body: %s", resp.StatusCode, body)
	}
	if got := resp.Header.Get("Fleet-Runtime"); got != "healthy" {
		t.Errorf("Fleet-Runtime = %q, want %q", got, "healthy")
	}
}
