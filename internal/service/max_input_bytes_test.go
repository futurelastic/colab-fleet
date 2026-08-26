package service

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	fleet "github.com/godx-jp/colab-fleet"
	"github.com/godx-jp/colab-fleet/internal/drivers/stub"
)

// --- Service.SetMaxInputBytes / MaxInputBytes -----------------------------

// TestMaxInputBytes_DefaultsWithoutConfiguration is colab-fleet #130's core
// acceptance criterion: an unconfigured deployment behaves exactly as
// before this setting existed.
func TestMaxInputBytes_DefaultsWithoutConfiguration(t *testing.T) {
	svc := New("self")
	if got := svc.MaxInputBytes(); got != defaultMaxInputBytes {
		t.Errorf("MaxInputBytes() = %d, want %d (the shipped default, unconfigured)", got, defaultMaxInputBytes)
	}
}

func TestSetMaxInputBytes_AcceptsAPositiveValueBelowTheCeiling(t *testing.T) {
	svc := New("self")
	if err := svc.SetMaxInputBytes(4096); err != nil {
		t.Fatalf("SetMaxInputBytes(4096) = %v, want nil", err)
	}
	if got := svc.MaxInputBytes(); got != 4096 {
		t.Errorf("MaxInputBytes() = %d, want 4096", got)
	}
}

func TestSetMaxInputBytes_RefusesZero(t *testing.T) {
	svc := New("self")
	err := svc.SetMaxInputBytes(0)
	if err == nil {
		t.Fatal("SetMaxInputBytes(0) accepted a zero limit")
	}
	if got := svc.MaxInputBytes(); got != defaultMaxInputBytes {
		t.Errorf("MaxInputBytes() = %d after a refused Set, want the untouched default %d", got, defaultMaxInputBytes)
	}
}

func TestSetMaxInputBytes_RefusesNegative(t *testing.T) {
	svc := New("self")
	err := svc.SetMaxInputBytes(-1)
	if err == nil {
		t.Fatal("SetMaxInputBytes(-1) accepted a negative limit")
	}
	if got := svc.MaxInputBytes(); got != defaultMaxInputBytes {
		t.Errorf("MaxInputBytes() = %d after a refused Set, want the untouched default %d", got, defaultMaxInputBytes)
	}
}

// TestSetMaxInputBytes_RefusesAtOrAboveTheCeiling is #130's "a value large
// enough to be meaningless should be refused with a clear reason rather
// than silently honoured."
func TestSetMaxInputBytes_RefusesAtOrAboveTheCeiling(t *testing.T) {
	svc := New("self")
	err := svc.SetMaxInputBytes(maxInputBytesCeiling)
	if err == nil {
		t.Fatal("SetMaxInputBytes(ceiling) accepted a value at the ceiling")
	}
	if !strings.Contains(err.Error(), strconv.Itoa(maxInputBytesCeiling)) {
		t.Errorf("error %q does not name the ceiling it refused", err.Error())
	}
	if got := svc.MaxInputBytes(); got != defaultMaxInputBytes {
		t.Errorf("MaxInputBytes() = %d after a refused Set, want the untouched default %d", got, defaultMaxInputBytes)
	}
}

func TestSetMaxInputBytes_AcceptsJustBelowTheCeiling(t *testing.T) {
	svc := New("self")
	if err := svc.SetMaxInputBytes(maxInputBytesCeiling - 1); err != nil {
		t.Fatalf("SetMaxInputBytes(ceiling-1) = %v, want nil", err)
	}
	if got := svc.MaxInputBytes(); got != maxInputBytesCeiling-1 {
		t.Errorf("MaxInputBytes() = %d, want %d", got, maxInputBytesCeiling-1)
	}
}

// --- enforcement: a configured limit is actually honoured over the wire --

// TestCreateSession_HonoursConfiguredLimit proves #130's second acceptance
// criterion: a configured value is honoured. A prompt under the shipped
// default but over a smaller CONFIGURED one must still be rejected.
func TestCreateSession_HonoursConfiguredLimit(t *testing.T) {
	svc := New("test-machine")
	if err := svc.RegisterLocalDriver("stub", &stub.Driver{DeadlineMs: 200}); err != nil {
		t.Fatal(err)
	}
	if err := svc.SetMaxInputBytes(16); err != nil {
		t.Fatal(err)
	}
	mux := NewMux(svc, Config{Token: testToken, AllowLocalMutations: true})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	body, _ := json.Marshal(map[string]string{
		"runtime": "stub", "cwd": "/tmp",
		"prompt": strings.Repeat("x", 17), // one byte over the configured 16
	})
	req := authedRequest(t, http.MethodPost, srv.URL+"/v1/machines/test-machine/sessions", body)
	req.Header.Set("Idempotency-Key", "key-1")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 — the configured 16-byte limit was not honoured", resp.StatusCode)
	}
	env := decodeError(t, resp)
	if !strings.Contains(env.Error.Message, "16") {
		t.Errorf("message = %q, want it to name the CONFIGURED limit (16), not the shipped default", env.Error.Message)
	}
}

// TestSendInput_HonoursConfiguredLimit is the same proof for `text` on
// input, the second call site rejectOverLength guards.
func TestSendInput_HonoursConfiguredLimit(t *testing.T) {
	svc := New("test-machine")
	if err := svc.RegisterLocalDriver("stub", &stub.Driver{DeadlineMs: 200}); err != nil {
		t.Fatal(err)
	}
	if err := svc.SetMaxInputBytes(16); err != nil {
		t.Fatal(err)
	}
	mux := NewMux(svc, Config{Token: testToken, AllowLocalMutations: true})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	body, _ := json.Marshal(map[string]any{"text": strings.Repeat("x", 17), "submit": true})
	req := authedRequest(t, http.MethodPost, srv.URL+"/v1/machines/test-machine/sessions/some-id/input", body)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 — the configured 16-byte limit was not honoured", resp.StatusCode)
	}
	env := decodeError(t, resp)
	if !strings.Contains(env.Error.Message, "16") {
		t.Errorf("message = %q, want it to name the CONFIGURED limit (16), not the shipped default", env.Error.Message)
	}
}

// --- exposure: a caller can read the effective limit without triggering it

// TestHealthReportsEffectiveMaxInputBytes proves #130's third acceptance
// criterion for self: GET /v1/health carries the configured value, so a
// caller can size its input before sending.
func TestHealthReportsEffectiveMaxInputBytes(t *testing.T) {
	svc := New("test-machine")
	if err := svc.SetMaxInputBytes(2048); err != nil {
		t.Fatal(err)
	}
	mux := NewMux(svc, Config{Token: testToken, AllowLocalMutations: true})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	req := authedRequest(t, http.MethodGet, srv.URL+"/v1/health", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close()

	var out struct {
		MaxInputBytes int `json:"maxInputBytes"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.MaxInputBytes != 2048 {
		t.Errorf("health maxInputBytes = %d, want 2048", out.MaxInputBytes)
	}
}

// TestMachinesIncludesSelfMaxInputBytes mirrors
// TestMachinesIncludesSelfBuild (#121) for #130: GET /v1/machines' self
// entry carries this machine's own effective limit.
func TestMachinesIncludesSelfMaxInputBytes(t *testing.T) {
	svc := New("test-machine")
	if err := svc.SetMaxInputBytes(2048); err != nil {
		t.Fatal(err)
	}
	mux := NewMux(svc, Config{Token: testToken, AllowLocalMutations: true})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	req := authedRequest(t, http.MethodGet, srv.URL+"/v1/machines", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close()

	var body struct {
		Items []fleet.MachineInfo `json:"items"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	var sawSelf bool
	for _, it := range body.Items {
		if !it.Self {
			continue
		}
		sawSelf = true
		if it.MaxInputBytes != 2048 {
			t.Errorf("self entry maxInputBytes = %d, want 2048", it.MaxInputBytes)
		}
	}
	if !sawSelf {
		t.Fatal("no self entry in /v1/machines")
	}
}

// peerMaxInputBytesDriver is a peer whose MaxInputBytes() is scripted
// directly — the smallest fake that proves ListMachines reads
// driver.MaxInputBytesReporter without standing up a real remote.Driver and
// HTTP peer, mirroring peerBuildDriver (#121) in http_test.go.
type peerMaxInputBytesDriver struct {
	stub.Driver
	max int
}

func (p *peerMaxInputBytesDriver) MaxInputBytes() int { return p.max }

// TestMachinesIncludesPeerMaxInputBytesWhenDriverReportsIt mirrors
// TestMachinesIncludesPeerBuildWhenDriverReportsIt (#121) for #130: a
// caller talking to two machines cannot assume one number, so a peer's own
// value must be visible in the same listing.
func TestMachinesIncludesPeerMaxInputBytesWhenDriverReportsIt(t *testing.T) {
	svc := New("testbox")
	if err := svc.RegisterPeerDriver("otherbox", &peerMaxInputBytesDriver{
		Driver: stub.Driver{DeadlineMs: 500},
		max:    4096,
	}); err != nil {
		t.Fatal(err)
	}

	col, err := svc.ListMachines(context.Background(), fleet.Request{}, 0)
	if err != nil {
		t.Fatal(err)
	}
	var sawPeer bool
	for _, it := range col.Items() {
		if it.Machine != "otherbox" {
			continue
		}
		sawPeer = true
		if it.MaxInputBytes != 4096 {
			t.Errorf("peer maxInputBytes = %d, want 4096", it.MaxInputBytes)
		}
	}
	if !sawPeer {
		t.Fatal("no otherbox entry in /v1/machines")
	}
}

// TestMachinesReportsUnknownMaxInputBytesForAPeerThatNeverAnswered mirrors
// TestMachinesReportsUnknownBuildForAPeerThatNeverAnswered (#121): a peer
// driver that cannot report a limit (never probed, or a driver type with
// nothing to say) must read as unknown — zero — never a plausible-looking
// default.
func TestMachinesReportsUnknownMaxInputBytesForAPeerThatNeverAnswered(t *testing.T) {
	svc := New("testbox")
	if err := svc.RegisterPeerDriver("otherbox", &stub.Driver{DeadlineMs: 500}); err != nil {
		t.Fatal(err)
	}

	col, err := svc.ListMachines(context.Background(), fleet.Request{}, 0)
	if err != nil {
		t.Fatal(err)
	}
	var sawPeer bool
	for _, it := range col.Items() {
		if it.Machine != "otherbox" {
			continue
		}
		sawPeer = true
		if it.MaxInputBytes != 0 {
			t.Errorf("peer maxInputBytes = %d, want 0 (unknown) with nothing behind it", it.MaxInputBytes)
		}
	}
	if !sawPeer {
		t.Fatal("no otherbox entry in /v1/machines")
	}
}
