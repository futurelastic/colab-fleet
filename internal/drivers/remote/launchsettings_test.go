package remote

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	fleet "github.com/futurelastic/muster"
)

// A create's `settings` across a federation hop (muster #247): the same
// mixed-version rule conversationId follows — a peer that predates the field
// must never silently drop it.

func launchSettingsPeer(t *testing.T, supports bool) (*httptest.Server, *atomic.Int32, *atomic.Value) {
	t.Helper()
	var creates atomic.Int32
	var last atomic.Value
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/v1/health":
			h := map[string]any{"build": fleet.Build{}, "maxInputBytes": 1024}
			if supports {
				h["supportsLaunchSettings"] = true
			}
			_ = json.NewEncoder(w).Encode(h)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/sessions"):
			creates.Add(1)
			raw, _ := io.ReadAll(r.Body)
			last.Store(string(raw))
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(fleet.Session{
				SessionRef: fleet.SessionRef{Machine: "peerbox", ID: "s1"},
				State:      fleet.ObservedState(fleet.StatusIdle, "fixture", nil),
			})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, &creates, &last
}

func TestCreateForwardsSettingsToAPeerThatCarriesThem(t *testing.T) {
	srv, creates, last := launchSettingsPeer(t, true)
	d := New("peerbox", srv.URL)
	if _, err := d.Create(context.Background(), caller, "k1", fleet.SessionSpec{
		Cwd: "/work", PermissionMode: fleet.PermissionModeBypass,
		Settings: json.RawMessage(`{"crossSessionInbound":"accept"}`),
	}); err != nil {
		t.Fatal(err)
	}
	if creates.Load() != 1 {
		t.Fatalf("creates = %d, want 1", creates.Load())
	}
	body, _ := last.Load().(string)
	if !strings.Contains(body, `"settings":{"crossSessionInbound":"accept"}`) {
		t.Errorf("create body %s does not carry the settings", body)
	}
}

func TestCreateRefusesASettingsCreateToAPeerThatWouldDropIt(t *testing.T) {
	srv, creates, _ := launchSettingsPeer(t, false)
	d := New("peerbox", srv.URL)
	_, err := d.Create(context.Background(), caller, "k1", fleet.SessionSpec{
		Cwd: "/work", PermissionMode: fleet.PermissionModeBypass,
		Settings: json.RawMessage(`{"crossSessionInbound":"accept"}`),
	})
	if kindOf(err) != fleet.ErrorUnsupported {
		t.Fatalf("err = %v, want unsupported", err)
	}
	if creates.Load() != 0 {
		t.Fatalf("the refused create reached the peer %d time(s)", creates.Load())
	}
	// A create with no settings is unaffected.
	if _, err := d.Create(context.Background(), caller, "k2", fleet.SessionSpec{Cwd: "/work"}); err != nil {
		t.Fatalf("plain create: %v", err)
	}
	if creates.Load() != 1 {
		t.Fatal("plain create did not reach the peer")
	}
}
