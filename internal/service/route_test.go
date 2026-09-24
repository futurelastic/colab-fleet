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

// #184: the service's half of routing — everything that depends on WHO is
// sending. The driver decides what depends on the session; what reaches it is
// pinned here. The recurring subject is authority: the human-relay fact comes
// only from the principal's own grant (or a trusted relay's assertion of it),
// never from anything a caller can set.

// routeDriver remembers what it was asked to send and answers with a receipt
// that names a path, so pass-through can be seen.
type routeDriver struct {
	stub.Driver
	mu      sync.Mutex
	opts    []driver.SendOptions
	texts   []string
	receipt fleet.DeliveryReceipt
}

func (d *routeDriver) Send(_ context.Context, _ fleet.Request, _ fleet.SessionRef, text string, opts driver.SendOptions) (fleet.DeliveryReceipt, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.opts = append(d.opts, opts)
	d.texts = append(d.texts, text)
	r := d.receipt
	if r.Outcome == "" {
		r = fleet.DeliveryReceipt{Outcome: fleet.OutcomeQueued}
	}
	return r, nil
}

func (d *routeDriver) last(t *testing.T) driver.SendOptions {
	t.Helper()
	d.mu.Lock()
	defer d.mu.Unlock()
	if len(d.opts) == 0 {
		t.Fatal("the driver was never asked to send")
	}
	return d.opts[len(d.opts)-1]
}

func (d *routeDriver) sends() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return len(d.opts)
}

// routeOwner is an owning machine with a principal table: a configured peer, an
// ordinary agent, and a human relay.
func routeOwner(t *testing.T) (*routeDriver, *httptest.Server) {
	t.Helper()
	svc := New("owner")
	d := &routeDriver{Driver: stub.Driver{DeadlineMs: 500}}
	if err := svc.RegisterLocalDriver("stub", d); err != nil {
		t.Fatal(err)
	}
	if err := svc.RegisterPeerDriver("entrybox", &stub.Driver{}); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(NewMux(svc, Config{AllowLocalMutations: true, Principals: []Principal{
		{Name: "entrybox", Token: "peer-token", Grants: []Grant{GrantSend}},
		{Name: "agent", Token: "agent-token", Grants: []Grant{GrantSend}},
		{Name: "human-relay", Token: "human-token", Grants: []Grant{GrantSend, GrantHumanRelay}},
	}}))
	t.Cleanup(srv.Close)
	return d, srv
}

func mustOK(t *testing.T, resp *http.Response) {
	t.Helper()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d: %s", resp.StatusCode, decodeError(t, resp).Error.Message)
	}
}

func TestSendInput_RouteValues(t *testing.T) {
	d, srv := routeOwner(t)
	for _, tc := range []struct {
		name    string
		body    map[string]any
		want    int
		route   fleet.Route
		message string
	}{
		{"omitted", map[string]any{"text": "x", "submit": true}, 200, fleet.RouteAuto, ""},
		{"empty", map[string]any{"text": "x", "submit": true, "route": ""}, 200, fleet.RouteAuto, ""},
		{"auto", map[string]any{"text": "x", "submit": true, "route": "auto"}, 200, fleet.RouteAuto, ""},
		{"terminal with a label", map[string]any{"text": "x", "submit": true, "route": "terminal", "from": map[string]any{"agent": "a"}}, 200, fleet.RouteTerminal, ""},
		{"inbox", map[string]any{"text": "x", "submit": true, "route": "inbox"}, 200, fleet.RouteInbox, ""},
		{"a value nobody defined", map[string]any{"text": "x", "submit": true, "route": "carrier-pigeon"}, 400, "", `"auto"`},
		{"the wrong case", map[string]any{"text": "x", "submit": true, "route": "Inbox"}, 400, "", `"inbox"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before := d.sends()
			resp := postInput(t, srv, "agent-token", nil, tc.body)
			if resp.StatusCode != tc.want {
				t.Fatalf("status = %d, want %d", resp.StatusCode, tc.want)
			}
			if tc.want == 400 {
				msg := decodeError(t, resp).Error.Message
				if !strings.Contains(msg, tc.message) || !strings.Contains(msg, `"terminal"`) {
					t.Errorf("message = %q, want it to name every accepted value", msg)
				}
				if d.sends() != before {
					t.Error("a rejected route reached the driver")
				}
				return
			}
			if got := d.last(t).Route; got != tc.route {
				t.Errorf("driver got route %q, want %q", got, tc.route)
			}
		})
	}
}

// The inbox has no composer: a text that is not submitted, or a resume/replace
// of a delivery stranded in one, cannot be an inbox delivery — refused as the
// request's own shape, before any session is looked up.
func TestSendInput_RouteInboxShapeIs400(t *testing.T) {
	d, srv := routeOwner(t)
	for name, body := range map[string]map[string]any{
		"submit false": {"text": "x", "route": "inbox"},
		"resume":       {"text": "x", "submit": true, "route": "inbox", "resumeIfStranded": true},
		"replace":      {"text": "x", "submit": true, "route": "inbox", "replaceIfStranded": true},
	} {
		t.Run(name, func(t *testing.T) {
			resp := postInput(t, srv, "agent-token", nil, body)
			if resp.StatusCode != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400", resp.StatusCode)
			}
			if msg := decodeError(t, resp).Error.Message; !strings.Contains(msg, "inbox") {
				t.Errorf("message = %q, want it to name the inbox", msg)
			}
		})
	}
	if d.sends() != 0 {
		t.Error("a malformed inbox request reached the driver")
	}
}

// The receipt's account of the path comes back untouched, and a receipt that
// names none is not given one.
func TestSendInput_ReceiptDeliveryRoutePassesThrough(t *testing.T) {
	d, srv := routeOwner(t)
	for _, route := range []fleet.Route{fleet.RouteInbox, fleet.RouteTerminal, ""} {
		d.receipt = fleet.DeliveryReceipt{Outcome: fleet.OutcomeQueued}.WithRoute(route)
		resp := postInput(t, srv, "agent-token", nil, map[string]any{"text": "x", "submit": true})
		mustOK(t, resp)
		var got fleet.DeliveryReceipt
		if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
			t.Fatal(err)
		}
		if got.RouteOf() != route {
			t.Errorf("receipt route = %q, want %q", got.RouteOf(), route)
		}
	}
}

// A human relay's message is the user's own words: auto becomes the terminal
// and it arrives unlabelled — nothing is synthesised, `from` stays absent.
func TestSendInput_HumanRelayAutoGoesTerminalUnlabelled(t *testing.T) {
	d, srv := routeOwner(t)
	mustOK(t, postInput(t, srv, "human-token", nil, map[string]any{"text": "a person's message", "submit": true}))
	got := d.last(t)
	if got.Route != fleet.RouteTerminal || !got.HumanRelay {
		t.Fatalf("driver got route %q humanRelay=%v, want terminal and a human relay", got.Route, got.HumanRelay)
	}
	if got.From != nil {
		t.Errorf("driver got from = %+v, want none: a human relay's message is not labelled", got.From)
	}
}

// The human-relay fact also crosses a peer relay, under the same trust bound as
// the on-behalf-of assertion, and lands the same way.
func TestSendInput_HumanRelayCrossesAPeerAndGoesTerminal(t *testing.T) {
	d, srv := routeOwner(t)
	mustOK(t, postInput(t, srv, "peer-token",
		map[string]string{onBehalfOfHeader: "human-relay", humanRelayHeader: "1"},
		map[string]any{"text": "a person's message", "submit": true}))
	got := d.last(t)
	if got.Route != fleet.RouteTerminal || !got.HumanRelay || got.From != nil {
		t.Fatalf("driver got %+v, want an unlabelled human-relay terminal send", got)
	}
}

// A human relay may still ask for the inbox explicitly; the request is honoured
// as asked, not overridden by the relay policy.
func TestSendInput_HumanRelayExplicitInboxIsHonoured(t *testing.T) {
	d, srv := routeOwner(t)
	mustOK(t, postInput(t, srv, "human-token", nil, map[string]any{"text": "x", "submit": true, "route": "inbox"}))
	if got := d.last(t); got.Route != fleet.RouteInbox || !got.HumanRelay {
		t.Fatalf("driver got route %q humanRelay=%v, want inbox from a human relay", got.Route, got.HumanRelay)
	}
}

// Everyone else's message is labelled, whether or not they said who they are.
func TestSendInput_NonHumanGetsAPrintableLabel(t *testing.T) {
	d, srv := routeOwner(t)

	// Said nothing: the one fact the service holds — who authenticated — fills in.
	mustOK(t, postInput(t, srv, "agent-token", nil, map[string]any{"text": "x", "submit": true}))
	got := d.last(t)
	if got.Route != fleet.RouteAuto || got.HumanRelay {
		t.Fatalf("driver got route %q humanRelay=%v, want auto and not a human relay", got.Route, got.HumanRelay)
	}
	if got.From == nil || got.From.Agent != "agent" || got.From.Machine != "owner" {
		t.Fatalf("driver got from = %+v, want the authenticated principal on this machine", got.From)
	}
	if driver.SenderLabel(got.From) != "agent · owner" {
		t.Errorf("label = %q", driver.SenderLabel(got.From))
	}

	// Said who they are: kept exactly, machine stamped by the service.
	mustOK(t, postInput(t, srv, "agent-token", nil, map[string]any{"text": "x", "submit": true,
		"from": map[string]any{"agent": "alex", "session": "s-1"}}))
	got = d.last(t)
	if got.From == nil || got.From.Agent != "alex" || got.From.Session != "s-1" || got.From.Machine != "owner" {
		t.Fatalf("driver got from = %+v, want the caller's own words kept and the machine stamped", got.From)
	}

	// Declared a human relay and nothing else: the declaration is kept, and the
	// label is still filled in — the declaration is a line of text, not an identity.
	mustOK(t, postInput(t, srv, "agent-token", nil, map[string]any{"text": "x", "submit": true,
		"from": map[string]any{"relayOfHuman": true}}))
	got = d.last(t)
	if got.From == nil || !got.From.RelayOfHuman || got.From.Agent != "agent" {
		t.Fatalf("driver got from = %+v, want relayOfHuman kept and the principal named", got.From)
	}
}

// With no principal table there is no authenticated identity to name, so the
// label is the machine alone — never a guess at a person.
func TestSendInput_NoPrincipalTableLabelsTheMachine(t *testing.T) {
	svc := New("test-machine")
	d := &routeDriver{Driver: stub.Driver{DeadlineMs: 500}}
	if err := svc.RegisterLocalDriver("stub", d); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(NewMux(svc, Config{Token: testToken, AllowLocalMutations: true}))
	t.Cleanup(srv.Close)

	mustOK(t, postInputTo(t, srv, "test-machine", testToken, nil, map[string]any{"text": "x", "submit": true}))
	got := d.last(t)
	if got.From == nil || got.From.Agent != "" || got.From.Machine != "test-machine" {
		t.Fatalf("driver got from = %+v, want the machine alone", got.From)
	}
}

// A request that arrives through a trusted relay with no label of its own is
// named for the principal it was made on behalf of, and its machine is left out
// rather than guessed: it entered somewhere this service was not told.
func TestSendInput_RelayedUnlabelledSendIsNamedForTheOriginalCaller(t *testing.T) {
	d, srv := routeOwner(t)
	mustOK(t, postInput(t, srv, "peer-token", map[string]string{onBehalfOfHeader: "agent"},
		map[string]any{"text": "x", "submit": true}))
	got := d.last(t)
	if got.HumanRelay {
		t.Fatal("an on-behalf-of assertion alone made the send a human relay")
	}
	if got.From == nil || got.From.Agent != "agent" || got.From.Machine != "" {
		t.Fatalf("driver got from = %+v, want the original principal and no machine", got.From)
	}
}

// The property the whole design rests on: the human-relay fact is never
// inferred from anything a caller sets. An ordinary agent that sets every header
// and every field there is stays an ordinary agent.
func TestSendInput_CallerSetClaimsNeverMakeAHumanRelay(t *testing.T) {
	d, srv := routeOwner(t)
	for name, tc := range map[string]struct {
		headers map[string]string
		body    map[string]any
	}{
		"both relay headers": {
			map[string]string{onBehalfOfHeader: "human-relay", humanRelayHeader: "1"},
			map[string]any{"text": "x", "submit": true},
		},
		"the human header alone": {
			map[string]string{humanRelayHeader: "1"},
			map[string]any{"text": "x", "submit": true},
		},
		"a from that says it relays a human": {
			nil,
			map[string]any{"text": "x", "submit": true, "from": map[string]any{"agent": "human-relay", "relayOfHuman": true}},
		},
	} {
		t.Run(name, func(t *testing.T) {
			mustOK(t, postInput(t, srv, "agent-token", tc.headers, tc.body))
			got := d.last(t)
			if got.HumanRelay {
				t.Fatal("a caller-set claim made the send a human relay")
			}
			if got.Route != fleet.RouteAuto {
				t.Errorf("route = %q, want auto: a claim must not move a send to the terminal", got.Route)
			}
			if got.From == nil {
				t.Error("a send that claimed to be human arrived unlabelled")
			}
		})
	}

	// From a configured peer, the human header without an on-behalf-of is
	// meaningless: there is no original caller for it to describe.
	mustOK(t, postInput(t, srv, "peer-token", map[string]string{humanRelayHeader: "1"}, map[string]any{"text": "x", "submit": true}))
	if d.last(t).HumanRelay {
		t.Fatal("a peer's human header with no on-behalf-of assertion was honoured")
	}
}

// With NO principal table every caller presents the one shared token and nothing
// tells a relay from anyone else (#180 L3), so the relay assertions are honoured
// as they always were — and that is what lets a single-token machine relay a
// person unlabelled at all. #196 (ruled on #195, option 2) did NOT change this
// request-time behaviour; it removed what made it dangerous. The header can only
// ever pick the terminal path here, because such a machine cannot turn the inbox
// route on: colab-fleetd refuses to start with FLEET_INBOX_INDEX and no table
// (cmd/colab-fleetd/inboxgate.go, pinned by TestRequireTableForInbox), so a
// person's message is never diverted into a peer message, whether it carried the
// header or not. If this test starts failing because the headers stopped being
// honoured, that is a different decision from #196's and it strands unlabelled
// relay on single-token machines: read #195 before flipping it.
func TestSendInput_WithoutAPrincipalTableRelayHeadersAreHonoured(t *testing.T) {
	svc := New("test-machine")
	d := &routeDriver{Driver: stub.Driver{DeadlineMs: 500}}
	if err := svc.RegisterLocalDriver("stub", d); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(NewMux(svc, Config{Token: testToken, AllowLocalMutations: true}))
	t.Cleanup(srv.Close)

	mustOK(t, postInputTo(t, srv, "test-machine", testToken,
		map[string]string{onBehalfOfHeader: "someone", humanRelayHeader: "1"},
		map[string]any{"text": "x", "submit": true}))
	if got := d.last(t); !got.HumanRelay || got.Route != fleet.RouteTerminal {
		t.Fatalf("driver got humanRelay=%v route=%q; a single-token machine's terminal relay (#195) no longer holds", got.HumanRelay, got.Route)
	}
}

// route:"terminal" without a label that prints is still refused for a caller
// that is not a human relay (#180 M8), even now that auto labels for them.
func TestSendInput_ForcedTerminalUnlabelledStillRefused(t *testing.T) {
	d, srv := routeOwner(t)
	resp := postInput(t, srv, "agent-token", nil, map[string]any{"text": "x", "submit": true, "route": "terminal"})
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
	if d.sends() != 0 {
		t.Error("a refused terminal request reached the driver")
	}
}
