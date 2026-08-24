package service

import (
	"bytes"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// routeOf's whole job is to name the route from something the service
// observed (ServeMux's own match), never from anything a caller supplies.
func TestRouteOfUsesTheMatchedPattern(t *testing.T) {
	r := httptest.NewRequest(http.MethodPost, "/v1/machines/x/sessions/s1/input", nil)
	r.Pattern = "POST /v1/machines/{machine}/sessions/{id}/input"

	if got := routeOf(r); got != r.Pattern {
		t.Errorf("routeOf = %q, want the matched pattern %q", got, r.Pattern)
	}
}

// A request built directly (as this repo's own unit tests do throughout
// auth_test.go, bypassing NewMux) never had a pattern matched against it.
// routeOf must still name something rather than log an empty field.
func TestRouteOfFallsBackWhenUnmatched(t *testing.T) {
	r := httptest.NewRequest(http.MethodPost, "/v1/machines/x/sessions/s1/input", nil)

	got := routeOf(r)
	if got == "" {
		t.Fatal("routeOf returned empty for an unmatched request")
	}
	if !strings.Contains(got, http.MethodPost) || !strings.Contains(got, "/input") {
		t.Errorf("routeOf fallback = %q, want it to still name method and path", got)
	}
}

// colab-fleet#105: input and respond both require GrantSend, so before this
// fix the audit line for each read verb=send and nothing else — identical,
// whatever route was actually hit. This is the fix's own oracle: two
// requests sharing a grant must not share a route in the log.
func TestAuditRouteDistinguishesInputFromRespond(t *testing.T) {
	var buf bytes.Buffer
	restore := log.Writer()
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(restore) })

	srv := principalSrv(t, []Principal{
		{Name: "worker", Token: "tok-send", Grants: []Grant{GrantRead, GrantSend}},
	})

	// Neither call needs to succeed against a real session — logMutation
	// (or logDenied, if the grant check itself refuses) fires either way,
	// and the route it records is what this test checks.
	call(t, srv, http.MethodPost, "/v1/machines/testbox/sessions/s1/input", "tok-send")
	call(t, srv, http.MethodPost, "/v1/machines/testbox/sessions/s1/respond", "tok-send")

	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	var inputRoute, respondRoute string
	for _, line := range lines {
		if !strings.Contains(line, "verb=send") {
			continue
		}
		if strings.Contains(line, "/input") {
			inputRoute = routeField(line)
		}
		if strings.Contains(line, "/respond") {
			respondRoute = routeField(line)
		}
	}

	if inputRoute == "" || respondRoute == "" {
		t.Fatalf("did not find both audit lines; got:\n%s", buf.String())
	}
	if inputRoute == respondRoute {
		t.Errorf("input and respond logged the same route %q; #105 is unfixed", inputRoute)
	}
	if !strings.Contains(inputRoute, "/input") {
		t.Errorf("input route = %q, want it to name the input path", inputRoute)
	}
	if !strings.Contains(respondRoute, "/respond") {
		t.Errorf("respond route = %q, want it to name the respond path", respondRoute)
	}
}

// routeField pulls the space-delimited route=<value> token out of one audit
// log line — audit.go's format uses %s for a value that itself contains a
// space ("POST /v1/…"), so a naive split on the field name's own boundary
// would truncate it at the method/path space. The route is always the last
// field save target and outcome/status, so anchor on "route=" and take
// everything up to " target=".
func routeField(line string) string {
	const marker = "route="
	i := strings.Index(line, marker)
	if i < 0 {
		return ""
	}
	rest := line[i+len(marker):]
	if j := strings.Index(rest, " target="); j >= 0 {
		rest = rest[:j]
	}
	return rest
}
