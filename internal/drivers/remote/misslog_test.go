package remote

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	fleet "github.com/godx-jp/colab-fleet"
	"github.com/godx-jp/colab-fleet/internal/driver"
)

// Tests for colab-fleet #174: every peer call that gets no answer is logged
// by the requesting daemon, with its latency and the budget that fired.
//
// captureLog swaps the standard logger, so nothing in this package may call
// t.Parallel.

// lockedBuffer is a log sink that is safe to read while written. A plain
// bytes.Buffer is not: log.Logger serializes its own writes, but a test
// reading String() races a capability probe still logging in the background.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func captureLog(t *testing.T) *lockedBuffer {
	t.Helper()
	buf := &lockedBuffer{}
	prevOut, prevFlags := log.Writer(), log.Flags()
	log.SetOutput(buf)
	log.SetFlags(0)
	t.Cleanup(func() {
		log.SetOutput(prevOut)
		log.SetFlags(prevFlags)
	})
	return buf
}

// hungPeer accepts every request and never answers it.
//
// It drains the body first: net/http only starts watching for the client
// hanging up once the handler has consumed the request body, so a POST left
// unread keeps the handler — and srv.Close — waiting forever.
func hungPeer(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		<-r.Context().Done()
	}))
	t.Cleanup(srv.Close)
	return srv
}

var missLine = regexp.MustCompile(`remote: peer call failed machine=(\S+) op="([^"]*)" after=(\S+) budget=(\S+) bound=(\S+) on_behalf_of=(\S+) kind=(\S+) err=`)

// missLines returns the miss lines logged for one machine. Each test names
// its peer uniquely because a capability probe started by an EARLIER test
// can still be in flight when its server closes, and would otherwise land in
// this test's buffer.
func missLines(buf *lockedBuffer, machine string) [][]string {
	var out [][]string
	for _, line := range strings.Split(buf.String(), "\n") {
		if m := missLine.FindStringSubmatch(line); m != nil && m[1] == machine {
			out = append(out, m)
		}
	}
	return out
}

func mustDur(t *testing.T, s string) time.Duration {
	t.Helper()
	d, err := time.ParseDuration(s)
	if err != nil {
		t.Fatalf("not a duration: %q", s)
	}
	return d
}

// The core of #174: a caller-shortened deadline that fires below this
// driver's own bound is logged with both, so the cliff is traceable to the
// caller rather than mistaken for a constant here.
func TestAPeerDeadlineMissIsLoggedWithItsLatency(t *testing.T) {
	buf := captureLog(t)
	srv := hungPeer(t)
	d := New("deadlinebox", srv.URL, WithDeadline(5*time.Second))

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	got, err := d.List(ctx, caller, driver.ListFilter{Labels: map[string]string{"secret": "value"}})
	if err != nil {
		t.Fatalf("a miss degrades the envelope, it does not fail the call: %v", err)
	}

	// The envelope is exactly what it was before this change.
	if len(got.Sources()) != 1 || got.Sources()[0].Status != fleet.SourceUnreachable {
		t.Fatalf("want one unreachable source, got %+v", got.Sources())
	}
	if !strings.Contains(got.Sources()[0].Error, "no answer from deadlinebox after") {
		t.Errorf("source error changed shape: %q", got.Sources()[0].Error)
	}

	lines := missLines(buf, "deadlinebox")
	if len(lines) != 1 {
		t.Fatalf("want exactly one miss line, got %d in:\n%s", len(lines), buf.String())
	}
	m := lines[0]
	if m[1] != "deadlinebox" {
		t.Errorf("machine = %q", m[1])
	}
	if m[2] != "GET /v1/sessions" {
		t.Errorf("op = %q; want the route with no query", m[2])
	}
	if strings.Contains(buf.String(), "secret") {
		t.Errorf("the query string leaked into the log:\n%s", buf.String())
	}
	if after := mustDur(t, m[3]); after < 150*time.Millisecond || after > 2*time.Second {
		t.Errorf("after = %s, want roughly the 200ms that elapsed", after)
	}
	if budget := mustDur(t, m[4]); budget < 150*time.Millisecond || budget > 250*time.Millisecond {
		t.Errorf("budget = %s, want the caller's ~200ms", budget)
	}
	if m[5] != "5s" {
		t.Errorf("bound = %q, want this driver's own 5s", m[5])
	}
	if m[6] != "addr:198.51.100.7" {
		t.Errorf("on_behalf_of = %q", m[6])
	}
	if m[7] != "deadline" {
		t.Errorf("kind = %q, want deadline", m[7])
	}
}

// A healthy fleet must log nothing: success never reaches the logger.
func TestASuccessfulReadLogsNothing(t *testing.T) {
	buf := captureLog(t)
	srv := peerServing(t, 200, collectionJSON(
		[]fleet.SourceStatus{{Machine: "healthybox", Status: fleet.SourceOK, ObservedAt: time.Now()}},
		nil), nil)
	d := New("healthybox", srv.URL)

	for i := 0; i < 3; i++ {
		if _, err := d.List(context.Background(), caller, driver.ListFilter{}); err != nil {
			t.Fatal(err)
		}
	}
	if lines := missLines(buf, "healthybox"); len(lines) != 0 {
		t.Errorf("a successful read logged a miss:\n%s", buf.String())
	}
	// Any status is a domain answer, not a miss.
	srv404 := peerServing(t, 404, map[string]any{"error": map[string]any{"kind": "not_found", "message": "x"}}, nil)
	_, _ = New("healthybox", srv404.URL).State(context.Background(), caller, fleet.SessionRef{Machine: "healthybox", ID: "gone"})
	if lines := missLines(buf, "healthybox"); len(lines) != 0 {
		t.Errorf("an HTTP error status was logged as a peer miss:\n%s", buf.String())
	}
}

// A requester that hung up is not the peer's fault.
func TestACallerHangingUpIsNotLoggedAsAPeerMiss(t *testing.T) {
	buf := captureLog(t)
	srv := hungPeer(t)
	d := New("hangupbox", srv.URL, WithDeadline(5*time.Second))

	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(100*time.Millisecond, cancel)
	got, err := d.List(ctx, caller, driver.ListFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if got.Complete() {
		t.Error("an abandoned read must still not report complete")
	}
	if lines := missLines(buf, "hangupbox"); len(lines) != 0 {
		t.Errorf("a caller hang-up was logged as a peer miss:\n%s", buf.String())
	}
}

// A dead port is a transport miss, and it is logged too.
func TestAClosedPortIsLoggedAsATransportMiss(t *testing.T) {
	buf := captureLog(t)
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	url := srv.URL
	srv.Close()

	if _, err := New("closedbox", url).List(context.Background(), caller, driver.ListFilter{}); err != nil {
		t.Fatal(err)
	}
	lines := missLines(buf, "closedbox")
	if len(lines) != 1 {
		t.Fatalf("want one miss line, got:\n%s", buf.String())
	}
	if lines[0][7] != "transport" {
		t.Errorf("kind = %q, want transport", lines[0][7])
	}
	if lines[0][5] != "3s" || lines[0][4] != "3s" {
		t.Errorf("budget/bound = %s/%s, want the 3s floor for both with no caller deadline", lines[0][4], lines[0][5])
	}
}

// Create goes through doWithKey, the second construction site.
func TestACreateMissIsLoggedToo(t *testing.T) {
	buf := captureLog(t)
	srv := hungPeer(t)
	d := New("createbox", srv.URL, WithDeadline(200*time.Millisecond))

	_, err := d.Create(context.Background(), caller, "k-1", fleet.SessionSpec{Cwd: "/w", Name: "n"})
	var ferr *fleet.Error
	if !errors.As(err, &ferr) || ferr.Kind != fleet.ErrorUnreachable {
		t.Fatalf("want an unreachable error, got %v", err)
	}
	lines := missLines(buf, "createbox")
	if len(lines) != 1 {
		t.Fatalf("want one miss line, got:\n%s", buf.String())
	}
	if lines[0][2] != "POST /v1/machines/createbox/sessions" || lines[0][7] != "deadline" {
		t.Errorf("op/kind = %q/%q", lines[0][2], lines[0][7])
	}
}
