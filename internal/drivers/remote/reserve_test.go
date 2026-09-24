package remote

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"testing"
	"time"

	fleet "github.com/godx-jp/colab-fleet"
	"github.com/godx-jp/colab-fleet/internal/driver"
	"github.com/godx-jp/colab-fleet/internal/service"
)

// Tests for colab-fleet #175: the bound announced to a peer in
// Fleet-Deadline-Ms holds a transit reserve back from the bound this driver
// enforces, so a peer that is up but slow gets its own answer home before
// the requester's timer fires.
//
// captureLog swaps the standard logger, so nothing here may call t.Parallel.

// slowPeer is a peer that is up, but slow: it uses every millisecond it was
// announced, then spends `transit` more getting the answer back — the network
// hop, made explicit so the failure this guards against is deterministic
// rather than a race against scheduler jitter.
type slowPeer struct {
	mu        sync.Mutex
	announced []int64
}

func (p *slowPeer) seen() []int64 {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]int64(nil), p.announced...)
}

func slowButUpPeer(t *testing.T, transit time.Duration, src fleet.SourceStatus) (*httptest.Server, *slowPeer) {
	t.Helper()
	p := &slowPeer{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// The capability probe (#67) must never block on this peer's
		// slowness; it is not the call under test.
		switch r.URL.Path {
		case "/v1/runtimes", "/v1/health", "/v1/whoami":
			http.NotFound(w, r)
			return
		}
		_, _ = io.Copy(io.Discard, r.Body)
		raw := r.Header.Get("Fleet-Deadline-Ms")
		if raw == "" {
			t.Errorf("%s %s arrived with no Fleet-Deadline-Ms", r.Method, r.URL.Path)
		} else if ms, err := strconv.ParseInt(raw, 10, 64); err != nil {
			t.Errorf("bad Fleet-Deadline-Ms %q", raw)
		} else {
			p.mu.Lock()
			p.announced = append(p.announced, ms)
			p.mu.Unlock()
			select {
			case <-time.After(time.Duration(ms)*time.Millisecond + transit):
			case <-r.Context().Done():
				return // the requester gave up; nobody is listening
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(collectionJSON([]fleet.SourceStatus{src}, nil))
	}))
	t.Cleanup(srv.Close)
	return srv, p
}

// The core of #175: a peer that answers with its own degraded report just
// before the bound it was told is adopted as that report (§13.2), not
// overwritten with a requester-side "unreachable".
func TestASlowButUpPeerIsAdoptedAsDegradedNotUnreachable(t *testing.T) {
	buf := captureLog(t)
	const why = "slowupbox: capture ran out of time for 2 panes"
	srv, peer := slowButUpPeer(t, 30*time.Millisecond, fleet.SourceStatus{
		Machine: "slowupbox", Status: fleet.SourceDegraded, Error: why, ObservedAt: time.Now(),
	})
	d := New("slowupbox", srv.URL, WithDeadline(5*time.Second))

	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()
	got, err := d.List(ctx, caller, driver.ListFilter{})
	if err != nil {
		t.Fatalf("a slow peer degrades the envelope, it does not fail the call: %v", err)
	}
	srcs := got.Sources()
	if len(srcs) != 1 {
		t.Fatalf("want one source, got %+v", srcs)
	}
	if srcs[0].Status != fleet.SourceDegraded || srcs[0].Error != why {
		t.Errorf("source = %s %q; want the peer's own degraded report %q", srcs[0].Status, srcs[0].Error, why)
	}
	if got.Complete() {
		t.Error("a degraded source must not read as complete")
	}
	if lines := missLines(buf, "slowupbox"); len(lines) != 0 {
		t.Errorf("a peer that answered was logged as a miss:\n%s", buf.String())
	}
	ann := peer.seen()
	if len(ann) != 1 || ann[0] < 700 || ann[0] > 999 {
		t.Errorf("announced %v ms; want one bound held back from the caller's 1s", ann)
	}
}

// The same, through the service's own fan-out — the path a fleet-scoped
// read actually takes (#174's path).
func TestFederatedSlowButUpPeerKeepsItsOwnDegradedStatus(t *testing.T) {
	buf := captureLog(t)
	const why = "slowfedbox: capture ran out of time for 1 pane"
	srv, _ := slowButUpPeer(t, 30*time.Millisecond, fleet.SourceStatus{
		Machine: "slowfedbox", Status: fleet.SourceDegraded, Error: why, ObservedAt: time.Now(),
	})
	rd := New("slowfedbox", srv.URL, WithDeadline(5*time.Second))
	svcA := homeService(t, "homebox", "slowfedbox", rd)

	got, err := svcA.ListSessions(context.Background(), caller, service.ScopeFleet,
		driver.ListFilter{}, 1*time.Second)
	if err != nil {
		t.Fatalf("fleet-scoped list failed outright: %v", err)
	}
	var saw bool
	for _, src := range got.Sources() {
		if src.Machine != "slowfedbox" {
			continue
		}
		saw = true
		if src.Status != fleet.SourceDegraded || src.Error != why {
			t.Errorf("peer source = %s %q; want its own degraded report", src.Status, src.Error)
		}
	}
	if !saw {
		t.Fatalf("the peer contributed no source; sources=%+v", got.Sources())
	}
	if lines := missLines(buf, "slowfedbox"); len(lines) != 0 {
		t.Errorf("a peer that answered was logged as a miss:\n%s", buf.String())
	}
}

// Both construction sites announce through the one rule: do (reads and most
// verbs) and doWithKey (idempotent create). A fix to one alone would leave
// relayed creates announcing the full budget.
func TestBothAnnounceSitesHoldTheTransitReserve(t *testing.T) {
	var rec capture
	srv := peerServing(t, 200, collectionJSON(
		[]fleet.SourceStatus{{Machine: "announcebox", Status: fleet.SourceOK, ObservedAt: time.Now()}},
		nil), &rec)
	d := New("announcebox", srv.URL, WithDeadline(2*time.Second))
	if _, err := d.List(context.Background(), caller, driver.ListFilter{}); err != nil {
		t.Fatal(err)
	}
	if ms := parseMs(t, rec.dline); ms < 1500 || ms > 1750 {
		t.Errorf("List announced %dms against a 2s bound; want 2s less a reserve of at most 250ms", ms)
	}

	var crec capture
	csrv := peerServing(t, 200, map[string]any{}, &crec)
	cd := New("announcebox", csrv.URL, WithDeadline(2*time.Second))
	if _, err := cd.Create(context.Background(), caller, "k-1", fleet.SessionSpec{Cwd: "/w", Name: "n"}); err != nil {
		t.Fatal(err)
	}
	if crec.path == "" {
		t.Fatal("the create never reached the peer")
	}
	if ms := parseMs(t, crec.dline); ms < 1500 || ms > 1750 {
		t.Errorf("Create announced %dms against a 2s bound; want 2s less a reserve of at most 250ms", ms)
	}
}

// The rule itself: a reserve that is capped both ways, and a header that
// never disappears while a deadline exists.
func TestAnnouncedDeadlineHoldsAReserveButNeverVanishes(t *testing.T) {
	for _, tc := range []struct {
		remaining time.Duration
		want      int64
	}{
		{-time.Second, 1},
		{0, 1},
		{500 * time.Microsecond, 1},
		{1 * time.Millisecond, 1},
		{5 * time.Millisecond, 4},
		{200 * time.Millisecond, 160},
		{1 * time.Second, 800},
		{1250 * time.Millisecond, 1000}, // where the share and the cap meet
		{2 * time.Second, 1750},
		{32 * time.Second, 31750},
	} {
		got := announcedDeadlineMs(tc.remaining)
		if got != tc.want {
			t.Errorf("announcedDeadlineMs(%s) = %d, want %d", tc.remaining, got, tc.want)
		}
		if got < 1 {
			t.Errorf("announcedDeadlineMs(%s) = %d; a bounded call must always announce a bound", tc.remaining, got)
		}
		if rem := tc.remaining.Milliseconds(); rem >= 1 && (got > rem || got < rem-250) {
			t.Errorf("announcedDeadlineMs(%s) = %d; want within [%d, %d]", tc.remaining, got, rem-250, rem)
		}
	}
}
