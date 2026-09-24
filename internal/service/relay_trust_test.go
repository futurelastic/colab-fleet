package service

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	fleet "github.com/godx-jp/colab-fleet"
	"github.com/godx-jp/colab-fleet/internal/driver"
	"github.com/godx-jp/colab-fleet/internal/drivers/stub"
)

// sendCapturingDriver is stub.Driver that accepts every send and remembers
// the options it was given.
type sendCapturingDriver struct {
	stub.Driver
	mu   sync.Mutex
	opts []driver.SendOptions
}

func (d *sendCapturingDriver) Send(_ context.Context, _ fleet.Request, _ fleet.SessionRef, _ string, opts driver.SendOptions) (fleet.DeliveryReceipt, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.opts = append(d.opts, opts)
	return fleet.DeliveryReceipt{Outcome: fleet.OutcomeQueued}, nil
}

// relayOwner is an owning machine whose principal table has a peer
// ("entrybox", configured as a peer here) and an ordinary agent.
func relayOwner(t *testing.T) (*sendCapturingDriver, *httptest.Server) {
	t.Helper()
	svc := New("owner")
	d := &sendCapturingDriver{Driver: stub.Driver{DeadlineMs: 500}}
	if err := svc.RegisterLocalDriver("stub", d); err != nil {
		t.Fatal(err)
	}
	if err := svc.RegisterPeerDriver("entrybox", &stub.Driver{}); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(NewMux(svc, Config{AllowLocalMutations: true, Principals: []Principal{
		{Name: "entrybox", Token: "peer-token", Grants: []Grant{GrantSend}},
		{Name: "agent", Token: "agent-token", Grants: []Grant{GrantSend}},
	}}))
	t.Cleanup(srv.Close)
	return d, srv
}

func postInput(t *testing.T, srv *httptest.Server, token string, headers map[string]string, body map[string]any) *http.Response {
	return postInputTo(t, srv, "owner", token, headers, body)
}

func postInputTo(t *testing.T, srv *httptest.Server, machine, token string, headers map[string]string, body map[string]any) *http.Response {
	t.Helper()
	raw, _ := json.Marshal(body)
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/machines/"+machine+"/sessions/s1/input", strings.NewReader(string(raw)))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

// #180 L3: a human relay's route:"terminal" send, entering on another
// machine, arrives here authenticated as the relaying PEER, which holds no
// human-relay grant. The peer's assertion carries the fact, and the owner
// honours it because that principal is one of its configured peers.
func TestHumanRelayCrossesAPeerRelay(t *testing.T) {
	d, srv := relayOwner(t)
	resp := postInput(t, srv, "peer-token",
		map[string]string{onBehalfOfHeader: "human-relay", humanRelayHeader: "1"},
		map[string]any{"text": "a person's message", "submit": true, "route": "terminal"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d: %s", resp.StatusCode, decodeError(t, resp).Error.Message)
	}
	if len(d.opts) != 1 || !d.opts[0].HumanRelay || d.opts[0].Route != fleet.RouteTerminal {
		t.Fatalf("driver got %+v, want a human-relay terminal send", d.opts)
	}
}

// The same headers from a caller that is not a configured peer assert
// authority it was never given: ignored, so the unlabelled terminal send is
// refused, and a label's machine is stamped rather than blanked.
func TestRelayAssertionsFromAnUntrustedCallerAreIgnored(t *testing.T) {
	d, srv := relayOwner(t)
	resp := postInput(t, srv, "agent-token",
		map[string]string{onBehalfOfHeader: "human-relay", humanRelayHeader: "1"},
		map[string]any{"text": "hello", "submit": true, "route": "terminal"})
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}

	resp = postInput(t, srv, "agent-token", map[string]string{onBehalfOfHeader: "someone"},
		map[string]any{"text": "hello", "submit": true, "from": map[string]any{"agent": "a"}})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if got := d.opts[len(d.opts)-1].From; got == nil || got.Machine != "owner" {
		t.Fatalf("from = %+v, want the machine stamped as this one — the relay claim was not trusted", got)
	}
}

// #180 M8: route:"terminal" needs a label that PRINTS. `{}` with a blanked
// machine, or a name label normalisation drops whole, prints nothing.
func TestRouteTerminalRefusesALabelThatPrintsAsNothing(t *testing.T) {
	_, srv := newTestServer(t) // no principal table: relay assertions are honoured
	for _, tc := range []struct {
		name    string
		headers map[string]string
		from    map[string]any
	}{
		{"empty from under a relay claim", map[string]string{onBehalfOfHeader: "x"}, map[string]any{}},
		{"an unassigned character", nil, map[string]any{"agent": "͸"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resp := postInputTo(t, srv, "test-machine", testToken, tc.headers,
				map[string]any{"text": "hello", "route": "terminal", "from": tc.from})
			if resp.StatusCode != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400", resp.StatusCode)
			}
			if msg := decodeError(t, resp).Error.Message; !strings.Contains(msg, "prints") {
				t.Fatalf("message = %q", msg)
			}
		})
	}
}
