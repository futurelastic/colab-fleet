package remote

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	fleet "github.com/futurelastic/muster"
	"github.com/futurelastic/muster/internal/drivers/stub"
	"github.com/futurelastic/muster/internal/service"
)

type peerTurnsDriver struct {
	stub.Driver
	q    fleet.TurnsQuery
	req  fleet.Request
	page fleet.TurnsPage
	err  error
}

func (d *peerTurnsDriver) Turns(_ context.Context, req fleet.Request, _ fleet.SessionRef, q fleet.TurnsQuery) (fleet.TurnsPage, error) {
	d.q, d.req = q, req
	return d.page, d.err
}

func entryService(t *testing.T, token string, rd *Driver) *httptest.Server {
	t.Helper()
	svcA := homeService(t, "homebox", "peerbox", rd)
	srv := httptest.NewServer(service.NewMux(svcA, service.Config{Token: token}))
	t.Cleanup(srv.Close)
	return srv
}

func readTurnsVia(t *testing.T, srv *httptest.Server, token, query string) (int, []byte) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/v1/machines/peerbox/sessions/s1/turns"+query, nil)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var b bytes.Buffer
	b.ReadFrom(resp.Body)
	return resp.StatusCode, b.Bytes()
}

// The ruling's "done when": a caller on one machine reads a session's turns on a
// peer — through a real HTTP hop, with the query and the caller's corroboration
// arriving at the machine that owns the session.
func TestFederatedTurnsReadReachesThePeersDriver(t *testing.T) {
	const token = "federation-token"
	at := time.Date(2026, 10, 4, 10, 0, 4, 0, time.UTC)
	pd := &peerTurnsDriver{Driver: stub.Driver{DeadlineMs: 2000},
		page: fleet.TurnsPage{Turns: []fleet.Turn{{At: at, Text: "said on the peer"}}, Next: "peer-cursor"}}
	base := peerService(t, "peerbox", "stub", pd, token, false)
	srv := entryService(t, token, New("peerbox", base, WithDeadline(2*time.Second)))

	status, body := readTurnsVia(t, srv, token, "?since=c1&limit=5&startedAt=2026-10-04T09:00:00Z")
	if status != http.StatusOK {
		t.Fatalf("status = %d: %s", status, body)
	}
	var page fleet.TurnsPage
	if err := json.Unmarshal(body, &page); err != nil {
		t.Fatal(err)
	}
	if len(page.Turns) != 1 || page.Turns[0].Text != "said on the peer" || !page.Turns[0].At.Equal(at) || page.Next != "peer-cursor" {
		t.Fatalf("page = %+v", page)
	}
	if pd.q.Since != "c1" || pd.q.Limit != 5 {
		t.Errorf("peer driver saw %+v", pd.q)
	}
	if want := pd.req.Expect.StartedAt; want == nil || want.UTC().Format(time.RFC3339) != "2026-10-04T09:00:00Z" {
		t.Errorf("startedAt did not reach the machine that owns the session: %v", want)
	}
}

func TestFederatedTurnsKindsSurviveTheHop(t *testing.T) {
	const token = "federation-token"
	for name, tc := range map[string]struct {
		err  error
		kind fleet.ErrorKind
	}{
		"stale corroboration": {fleet.ErrAmbiguousTarget, fleet.ErrorConflict},
		"no such session":     {fleet.ErrNoSuchSession, fleet.ErrorNotFound},
		"no record":           {fleet.ErrNoTurnRecord, fleet.ErrorNotFound},
	} {
		t.Run(name, func(t *testing.T) {
			pd := &peerTurnsDriver{Driver: stub.Driver{DeadlineMs: 2000}, err: tc.err}
			srv := entryService(t, token, New("peerbox", peerService(t, "peerbox", "stub", pd, token, false)))
			_, body := readTurnsVia(t, srv, token, "")
			var env fleet.ErrorEnvelope
			if err := json.Unmarshal(body, &env); err != nil || env.Error.Kind != tc.kind {
				t.Fatalf("kind = %q (%v), want %q: %s", env.Error.Kind, err, tc.kind, body)
			}
		})
	}
}

// A peer on a build that has no turns route, or a runtime that cannot read one,
// is `unsupported` — never an empty conversation.
func TestFederatedTurnsAbsentOnThePeerIsUnsupported(t *testing.T) {
	const token = "federation-token"

	older := httptest.NewServer(http.NotFoundHandler()) // a bare 404, as a build predating the route answers
	t.Cleanup(older.Close)
	srv := entryService(t, token, New("peerbox", older.URL))
	_, body := readTurnsVia(t, srv, token, "")
	if !strings.Contains(string(body), `"unsupported"`) {
		t.Errorf("older peer: %s", body)
	}

	srv = entryService(t, token, New("peerbox", peerService(t, "peerbox", "stub", &stub.Driver{DeadlineMs: 2000}, token, false)))
	_, body = readTurnsVia(t, srv, token, "")
	if !strings.Contains(string(body), `"unsupported"`) {
		t.Errorf("runtime without turns: %s", body)
	}
}
