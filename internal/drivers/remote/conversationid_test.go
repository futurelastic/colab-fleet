package remote

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	fleet "github.com/futurelastic/muster"
)

// A create's conversationId across a federation hop (muster #224). The
// rule under test is the MIXED-VERSION case, the exact mirror of labels_test.go:
// a peer on a build that predates the field must never silently drop it on a
// create.

func healthJSONWithConversationId(withConversationId bool) map[string]any {
	h := map[string]any{"build": fleet.Build{}, "maxInputBytes": 1024}
	if withConversationId {
		t := true
		h["supportsConversationId"] = &t
	}
	return h
}

// conversationIdPeer serves /v1/health (with or without conversationId
// support) and a create route that echoes the id back, counting creates.
func conversationIdPeer(t *testing.T, supports bool) (*httptest.Server, *atomic.Int32, *string, *sync.Mutex) {
	t.Helper()
	var creates atomic.Int32
	var mu sync.Mutex
	var lastBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/v1/health":
			_ = json.NewEncoder(w).Encode(healthJSONWithConversationId(supports))
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/sessions"):
			creates.Add(1)
			raw, _ := io.ReadAll(r.Body)
			mu.Lock()
			lastBody = string(raw)
			mu.Unlock()
			var body struct {
				ConversationId string `json:"conversationId"`
			}
			_ = json.Unmarshal(raw, &body)
			w.WriteHeader(http.StatusCreated)
			resp := fleet.Session{
				SessionRef: fleet.SessionRef{Machine: "peerbox", ID: "s1"},
				State:      fleet.ObservedState(fleet.StatusIdle, "fixture", nil),
			}
			if body.ConversationId != "" {
				resp.Conversation = fleet.ResolvedConversation(body.ConversationId, fleet.ConversationCaptured, "fixture")
			}
			_ = json.NewEncoder(w).Encode(resp)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, &creates, &lastBody, &mu
}

func TestCreateForwardsConversationIdToAPeerThatCarriesIt(t *testing.T) {
	srv, creates, lastBody, mu := conversationIdPeer(t, true)
	d := New("peerbox", srv.URL)
	got, err := d.Create(context.Background(), caller, "k1", fleet.SessionSpec{
		Cwd: "/work", ConversationId: "7f3a1c22-0b9e-4d51-9f2a-8e6b1d4c5a70",
	})
	if err != nil {
		t.Fatal(err)
	}
	if creates.Load() != 1 {
		t.Fatalf("creates = %d, want 1", creates.Load())
	}
	mu.Lock()
	body := *lastBody
	mu.Unlock()
	if !strings.Contains(body, `"conversationId":"7f3a1c22-0b9e-4d51-9f2a-8e6b1d4c5a70"`) {
		t.Errorf("create body %s does not carry the conversationId", body)
	}
	if got.Conversation == nil || got.Conversation.ID != "7f3a1c22-0b9e-4d51-9f2a-8e6b1d4c5a70" {
		t.Errorf("adopted session Conversation = %+v", got.Conversation)
	}
}

// The mixed-version rule for creates: refuse BEFORE any side effect, because
// an older peer would answer 201 having silently started a conversation under
// an id of its own choosing.
func TestCreateRefusesAConversationIdCreateToAPeerThatWouldDropIt(t *testing.T) {
	srv, creates, _, _ := conversationIdPeer(t, false)
	d := New("peerbox", srv.URL)

	_, err := d.Create(context.Background(), caller, "k1", fleet.SessionSpec{
		Cwd: "/work", ConversationId: "7f3a1c22-0b9e-4d51-9f2a-8e6b1d4c5a70",
	})
	if kindOf(err) != fleet.ErrorUnsupported {
		t.Fatalf("err = %v, want unsupported", err)
	}
	if creates.Load() != 0 {
		t.Fatalf("the refused create reached the peer %d time(s)", creates.Load())
	}

	// A create with no conversationId is unaffected: nothing to drop, nothing
	// to check.
	if _, err := d.Create(context.Background(), caller, "k2", fleet.SessionSpec{Cwd: "/work"}); err != nil {
		t.Fatalf("plain create: %v", err)
	}
	if creates.Load() != 1 {
		t.Fatalf("plain create did not reach the peer")
	}
}

// A cached positive answer is trusted without re-asking; a stale or absent
// one is re-asked before refusing, so a peer that has since upgraded is not
// refused on stale evidence — requireConversationId's own stated contract,
// exercised the same way requireLabels already is implicitly by the two
// tests above (one health call per Create there too).
func TestRequireConversationIdReasksOnAnUnconfirmedAnswer(t *testing.T) {
	var healthCalls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/v1/health":
			healthCalls.Add(1)
			_ = json.NewEncoder(w).Encode(healthJSONWithConversationId(true))
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/sessions"):
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
	d := New("peerbox", srv.URL)

	for i := 0; i < 2; i++ {
		if _, err := d.Create(context.Background(), caller, "k"+string(rune('1'+i)),
			fleet.SessionSpec{Cwd: "/work", ConversationId: "7f3a1c22-0b9e-4d51-9f2a-8e6b1d4c5a70"}); err != nil {
			t.Fatalf("create %d: %v", i, err)
		}
	}
	if healthCalls.Load() != 1 {
		t.Errorf("health calls = %d, want 1 — a positive cached answer must not be re-asked", healthCalls.Load())
	}
}
