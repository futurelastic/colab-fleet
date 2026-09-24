package remote

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	fleet "github.com/godx-jp/colab-fleet"
)

// state.permissionMode crosses a peer relay untouched (#194). A client reaches a
// session on another machine through this service, and the machine that runs the
// session is the one that reads its indicator — so what arrives here is what the
// far end read, and this driver must neither drop it nor re-derive it.
func TestPermissionModeSurvivesARelay(t *testing.T) {
	for _, want := range []fleet.PermissionModeState{
		fleet.PermissionModeDefault, fleet.PermissionModeAcceptEdits, fleet.PermissionModePlan,
		fleet.PermissionModeAuto, fleet.PermissionModeBypass, fleet.PermissionModeUnknown,
	} {
		st := fleet.InferredState(fleet.StatusIdle, "idle", nil)
		st.PermissionMode = want
		srv := peerServing(t, 200, fleet.Session{
			SessionRef: fleet.SessionRef{Machine: "peerbox", ID: "s1"},
			Runtime:    "claude-code-tmux",
			State:      st,
		}, nil)
		got, err := New("peerbox", srv.URL).State(context.Background(), caller, fleet.SessionRef{Machine: "peerbox", ID: "s1"})
		if err != nil {
			t.Fatalf("%q: %v", want, err)
		}
		if got.PermissionMode != want {
			t.Errorf("relayed %q, arrived as %q", want, got.PermissionMode)
		}
	}
}

// A peer on a build that predates the field sends no key. That must read as
// "nothing was read" — the absence a client is told to read again on — and never
// as a mode; a relay that invented one would turn an old machine into a machine
// that appears to be in the default mode.
func TestAPeerWithoutTheFieldRelaysNothingRead(t *testing.T) {
	srv := peerRaw(t, `{"machine":"peerbox","id":"s1","runtime":"claude-code-tmux",`+
		`"state":{"status":"idle","confidence":"inferred","evidence":"e"}}`)
	got, err := New("peerbox", srv.URL).State(context.Background(), caller, fleet.SessionRef{Machine: "peerbox", ID: "s1"})
	if err != nil {
		t.Fatal(err)
	}
	if got.PermissionMode != "" {
		t.Errorf("an absent key arrived as %q", got.PermissionMode)
	}
}

// The set is closed, so a mode this build does not know is refused, not passed on
// as text: a client branching on six names must never meet a seventh by way of a
// relay. This is the documented cost of a strict decoder (ADR 194), pinned so
// nobody weakens it by accident and nobody is surprised by it.
func TestAPeerNamingAnUnknownModeIsRefusedNotRelayedAsText(t *testing.T) {
	srv := peerRaw(t, `{"machine":"peerbox","id":"s1","runtime":"claude-code-tmux",`+
		`"state":{"status":"idle","confidence":"inferred","evidence":"e","permissionMode":"dontAsk"}}`)
	if _, err := New("peerbox", srv.URL).State(context.Background(), caller, fleet.SessionRef{Machine: "peerbox", ID: "s1"}); err == nil {
		t.Error("a mode outside the closed set was relayed")
	}
}

// peerRaw serves one fixed JSON body for every request — for a case where the
// point is a wire shape the typed helpers cannot produce (a missing key, a value
// this build's own encoder would refuse to emit).
func peerRaw(t *testing.T, body string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.HasPrefix(r.URL.Path, "/v1/runtimes") || r.URL.Path == "/v1/health" || r.URL.Path == "/v1/whoami" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv
}
