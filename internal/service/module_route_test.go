package service

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	fleet "github.com/godx-jp/colab-fleet"
	"github.com/godx-jp/colab-fleet/internal/driver"
	"github.com/godx-jp/colab-fleet/internal/drivers/stub"
)

// #185: the service's half of an optional external delivery module — the route
// vocabulary that names one, and the guards that must hold before any driver is
// consulted. What a driver then DOES with a module route is the driver's own
// test; what is pinned here is that the request's shape is judged by the
// service alone, from this machine's configuration.

// moduleOwner is routeOwner with two enabled modules configured.
func moduleOwner(t *testing.T, modules ...string) (*routeDriver, *httptest.Server) {
	t.Helper()
	svc := New("owner")
	d := &routeDriver{Driver: stub.Driver{DeadlineMs: 500}}
	if err := svc.RegisterLocalDriver("stub", d); err != nil {
		t.Fatal(err)
	}
	if err := svc.SetDeliveryModuleRoutes(modules); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(NewMux(svc, Config{AllowLocalMutations: true, Principals: []Principal{
		{Name: "agent", Token: "agent-token", Grants: []Grant{GrantSend}},
		{Name: "human-relay", Token: "human-token", Grants: []Grant{GrantSend, GrantHumanRelay}},
	}}))
	t.Cleanup(srv.Close)
	return d, srv
}

func TestSendInput_ModuleRouteValues(t *testing.T) {
	d, srv := moduleOwner(t, "relay-a", "relay-b")
	label := map[string]any{"agent": "a"}
	for _, tc := range []struct {
		name  string
		token string
		body  map[string]any
		want  int
		route fleet.Route
	}{
		{"a named module with a label", "agent-token", map[string]any{"text": "x", "submit": true, "route": "relay-a", "from": label}, 200, "relay-a"},
		{"the second module", "agent-token", map[string]any{"text": "x", "submit": true, "route": "relay-b", "from": label}, 200, "relay-b"},
		{"a human relay needs no label", "human-token", map[string]any{"text": "x", "submit": true, "route": "relay-a"}, 200, "relay-a"},
		{"an unlabelled agent is refused like terminal", "agent-token", map[string]any{"text": "x", "submit": true, "route": "relay-a"}, 400, ""},
		{"a module nobody enabled", "agent-token", map[string]any{"text": "x", "submit": true, "route": "relay-c", "from": label}, 400, ""},
		{"the receipt's own word is not a request", "agent-token", map[string]any{"text": "x", "submit": true, "route": "module", "from": label}, 400, ""},
		{"a module name in the wrong case", "agent-token", map[string]any{"text": "x", "submit": true, "route": "Relay-A", "from": label}, 400, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before := d.sends()
			resp := postInput(t, srv, tc.token, nil, tc.body)
			if resp.StatusCode != tc.want {
				t.Fatalf("status = %d, want %d", resp.StatusCode, tc.want)
			}
			if tc.want == 400 {
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

// The 400 for an unrecognised route names every accepted value, module names
// included, in the operator's configured order; with no module enabled the
// message is exactly what it was before #185.
func TestSendInput_UnrecognisedRouteNamesTheEnabledModules(t *testing.T) {
	_, srv := moduleOwner(t, "relay-a", "relay-b")
	resp := postInput(t, srv, "agent-token", nil, map[string]any{"text": "x", "submit": true, "route": "carrier-pigeon"})
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	msg := decodeError(t, resp).Error.Message
	for _, want := range []string{`"carrier-pigeon"`, `"auto"`, `"terminal"`, `"inbox"`, `"relay-a", "relay-b"`} {
		if !strings.Contains(msg, want) {
			t.Errorf("message %q lacks %s", msg, want)
		}
	}

	_, plain := routeOwner(t)
	resp = postInput(t, plain, "agent-token", nil, map[string]any{"text": "x", "submit": true, "route": "carrier-pigeon"})
	msg = decodeError(t, resp).Error.Message
	if !strings.HasSuffix(msg, `the accepted values are "auto" (or omitted), "terminal" and "inbox"`) {
		t.Errorf("with no module enabled the message must be unchanged, got %q", msg)
	}
}

// An unknown route value is refused as the request's own shape: a 400 naming
// the field even when the session, the machine or the runtime does not exist,
// because no driver is resolved before this check (the same order #184 set).
func TestSendInput_UnknownRoute400BeforeDriverResolved(t *testing.T) {
	// A service with NO driver registered at all: had a driver been resolved
	// first, this would be a resolution failure, not the route's own 400.
	svc := New("owner")
	if err := svc.SetDeliveryModuleRoutes([]string{"relay-a"}); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(NewMux(svc, Config{AllowLocalMutations: true, Principals: []Principal{
		{Name: "agent", Token: "agent-token", Grants: []Grant{GrantSend}},
	}}))
	t.Cleanup(srv.Close)
	resp := postInput(t, srv, "agent-token", nil,
		map[string]any{"text": "x", "submit": true, "route": "relay-zzz", "from": map[string]any{"agent": "a"}})
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want the route's own 400 before any driver is resolved", resp.StatusCode)
	}
	if msg := decodeError(t, resp).Error.Message; !strings.Contains(msg, "route") || !strings.Contains(msg, "relay-a") {
		t.Errorf("message = %q, want it to name the field and the accepted values", msg)
	}
}

// A module has no composer: a text that is not submitted, or a resume/replace
// of a delivery stranded in one, cannot be a module delivery.
func TestSendInput_ForcedModuleShapeIs400(t *testing.T) {
	d, srv := moduleOwner(t, "relay-a")
	label := map[string]any{"agent": "a"}
	for name, body := range map[string]map[string]any{
		"submit false":      {"text": "x", "route": "relay-a", "from": label},
		"resume stranded":   {"text": "x", "submit": true, "resumeIfStranded": true, "route": "relay-a", "from": label},
		"replace stranded":  {"text": "x", "submit": true, "replaceIfStranded": true, "route": "relay-a", "from": label},
		"both stranded":     {"text": "x", "submit": true, "resumeIfStranded": true, "replaceIfStranded": true, "route": "relay-a", "from": label},
		"submit omitted":    {"text": "x", "route": "relay-a", "from": label},
		"unsubmitted human": {"text": "x", "submit": false, "route": "relay-a"},
	} {
		t.Run(name, func(t *testing.T) {
			before := d.sends()
			token := "agent-token"
			if name == "unsubmitted human" {
				token = "human-token"
			}
			resp := postInput(t, srv, token, nil, body)
			if resp.StatusCode != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400", resp.StatusCode)
			}
			if d.sends() != before {
				t.Error("a rejected shape reached the driver")
			}
		})
	}
}

// A human relay's auto becomes terminal before a driver sees it (#184). It is
// marked, because a module carries the user's own turn too and that call's
// auto may use a live lane; an explicit terminal never does.
func TestSendInput_HumanRelayAutoMarksTerminalFromAuto(t *testing.T) {
	d, srv := moduleOwner(t, "relay-a")
	for _, body := range []map[string]any{
		{"text": "x", "submit": true},
		{"text": "x", "submit": true, "route": ""},
		{"text": "x", "submit": true, "route": "auto"},
	} {
		mustOK(t, postInput(t, srv, "human-token", nil, body))
		got := d.last(t)
		if got.Route != fleet.RouteTerminal || !got.TerminalFromAuto || !got.HumanRelay {
			t.Errorf("body %v: driver got %+v, want terminal + TerminalFromAuto + HumanRelay", body, got)
		}
	}
}

func TestSendInput_ExplicitTerminalNeverFromAuto(t *testing.T) {
	d, srv := moduleOwner(t, "relay-a")
	mustOK(t, postInput(t, srv, "human-token", nil, map[string]any{"text": "x", "submit": true, "route": "terminal"}))
	if got := d.last(t); got.Route != fleet.RouteTerminal || got.TerminalFromAuto {
		t.Errorf("a human's EXPLICIT terminal was marked from-auto: %+v", got)
	}
	mustOK(t, postInput(t, srv, "agent-token", nil,
		map[string]any{"text": "x", "submit": true, "route": "terminal", "from": map[string]any{"agent": "a"}}))
	if got := d.last(t); got.TerminalFromAuto {
		t.Errorf("an agent's explicit terminal was marked from-auto: %+v", got)
	}
	// An agent's auto is never marked: only the human-relay decision is.
	mustOK(t, postInput(t, srv, "agent-token", nil, map[string]any{"text": "x", "submit": true}))
	if got := d.last(t); got.Route != fleet.RouteAuto || got.TerminalFromAuto {
		t.Errorf("an agent's auto reached the driver as %+v", got)
	}
	// The field is the service's, never a caller's: a body naming it is ignored.
	mustOK(t, postInput(t, srv, "agent-token", nil,
		map[string]any{"text": "x", "submit": true, "route": "inbox", "terminalFromAuto": true}))
	if got := d.last(t); got.TerminalFromAuto {
		t.Errorf("a caller set TerminalFromAuto through the body: %+v", got)
	}
}

func TestSetDeliveryModuleRoutes_RejectsReservedWords(t *testing.T) {
	for _, bad := range []string{"auto", "terminal", "inbox", "module", "", "Relay", "a b", "../x", strings.Repeat("a", 33)} {
		if err := New("m").SetDeliveryModuleRoutes([]string{"ok-name", bad}); err == nil {
			t.Errorf("%q was accepted as a delivery module name", bad)
		}
	}
	svc := New("m")
	if err := svc.SetDeliveryModuleRoutes([]string{"relay-a", "relay-a", "relay-b"}); err != nil {
		t.Fatal(err)
	}
	if got := svc.deliveryModuleRouteNames(); len(got) != 2 || got[0] != "relay-a" || got[1] != "relay-b" {
		t.Errorf("names = %v, want the deduplicated configured order", got)
	}
}

// prefixReservingStubDriver is stub.Driver whose delivery module reserves every
// environment name beginning with a prefix (#185).
type prefixReservingStubDriver struct{ stub.Driver }

func (*prefixReservingStubDriver) ReservedEnvPrefixes() []string { return []string{"MODTEST_"} }

var _ driver.ReservedEnvPrefixReporter = (*prefixReservingStubDriver)(nil)

func TestCreateSession_ReservedPrefixIs400(t *testing.T) {
	svc := New("test-machine")
	if err := svc.RegisterLocalDriver("stub", &prefixReservingStubDriver{stub.Driver{DeadlineMs: 200}}); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(NewMux(svc, Config{Token: testToken, AllowLocalMutations: true}))
	t.Cleanup(srv.Close)

	post := func(key string, env map[string]string) *http.Response {
		body, _ := json.Marshal(map[string]any{"runtime": "stub", "cwd": "/tmp", "env": env})
		req := authedRequest(t, http.MethodPost, srv.URL+"/v1/machines/test-machine/sessions", body)
		req.Header.Set("Idempotency-Key", key)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		return resp
	}

	resp := post("k1", map[string]string{"MODTEST_LANE": "x", "MODTEST_A": "y"})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
	env := decodeError(t, resp)
	if env.Error.Kind != fleet.ErrorInvalid || !strings.Contains(env.Error.Message, "MODTEST_A, MODTEST_LANE") {
		t.Fatalf("error = %+v, want invalid naming every offending variable", env.Error)
	}

	// A name that merely CONTAINS the prefix is not reserved, and reaches the
	// driver (the stub's own 501).
	other := post("k2", map[string]string{"X_MODTEST_": "y"})
	defer other.Body.Close()
	if other.StatusCode != http.StatusNotImplemented {
		t.Fatalf("an unreserved name reached status %d, want the stub's own 501", other.StatusCode)
	}
}
