package service

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	fleet "github.com/godx-jp/colab-fleet"
	"github.com/godx-jp/colab-fleet/internal/drivers/stub"
)

const testToken = "test-token"

func newTestServer(t *testing.T) (*Service, *httptest.Server) {
	t.Helper()
	svc := New("test-machine")
	if err := svc.RegisterLocalDriver("stub", &stub.Driver{DeadlineMs: 200}); err != nil {
		t.Fatalf("RegisterLocalDriver: %v", err)
	}
	mux := NewMux(svc, Config{Token: testToken})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return svc, srv
}

func authedRequest(t *testing.T, method, url string, body []byte) *http.Request {
	t.Helper()
	var r io.Reader
	if body != nil {
		r = bytes.NewReader(body)
	}
	req, err := http.NewRequest(method, url, r)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+testToken)
	req.Header.Set("Content-Type", "application/json")
	return req
}

func decodeError(t *testing.T, resp *http.Response) fleet.ErrorEnvelope {
	t.Helper()
	var env fleet.ErrorEnvelope
	if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
		t.Fatalf("decode error envelope: %v", err)
	}
	return env
}

func TestHealth_OK(t *testing.T) {
	_, srv := newTestServer(t)

	req := authedRequest(t, http.MethodGet, srv.URL+"/v1/health", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var body map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if _, ok := body["epoch"]; !ok {
		t.Error("health response missing epoch")
	}
}

func TestAuth_MissingTokenIs401(t *testing.T) {
	_, srv := newTestServer(t)

	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/v1/health", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 — api-http.md §5: no unauthenticated mode, ever", resp.StatusCode)
	}
	env := decodeError(t, resp)
	if env.Error.Kind != fleet.ErrorUnauthorized {
		t.Fatalf("error kind = %q, want %q", env.Error.Kind, fleet.ErrorUnauthorized)
	}
}

func TestAuth_WrongTokenIs401(t *testing.T) {
	_, srv := newTestServer(t)

	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/v1/health", nil)
	req.Header.Set("Authorization", "Bearer wrong")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", resp.StatusCode)
	}
}

func TestCreateSession_MissingIdempotencyKeyIs400(t *testing.T) {
	_, srv := newTestServer(t)

	body, _ := json.Marshal(map[string]string{"runtime": "stub", "cwd": "/tmp"})
	req := authedRequest(t, http.MethodPost, srv.URL+"/v1/machines/test-machine/sessions", body)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (§10: Idempotency-Key is required, not optional)", resp.StatusCode)
	}
	env := decodeError(t, resp)
	if env.Error.Kind != fleet.ErrorInvalid {
		t.Fatalf("error kind = %q, want %q", env.Error.Kind, fleet.ErrorInvalid)
	}
}

func TestCreateSession_StubReturnsUnsupported501(t *testing.T) {
	_, srv := newTestServer(t)

	body, _ := json.Marshal(map[string]string{"runtime": "stub", "cwd": "/tmp"})
	req := authedRequest(t, http.MethodPost, srv.URL+"/v1/machines/test-machine/sessions", body)
	req.Header.Set("Idempotency-Key", "key-1")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNotImplemented {
		t.Fatalf("status = %d, want 501", resp.StatusCode)
	}
	env := decodeError(t, resp)
	if env.Error.Kind != fleet.ErrorUnsupported {
		t.Fatalf("error kind = %q, want %q", env.Error.Kind, fleet.ErrorUnsupported)
	}
}

func TestGetSession_UnknownMachineIs404(t *testing.T) {
	_, srv := newTestServer(t)

	req := authedRequest(t, http.MethodGet, srv.URL+"/v1/machines/nowhere/sessions/abc", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
}

func TestGetSession_AmbiguousRuntimeIs400(t *testing.T) {
	svc := New("test-machine")
	if err := svc.RegisterLocalDriver("stub-a", &stub.Driver{DeadlineMs: 200}); err != nil {
		t.Fatalf("RegisterLocalDriver: %v", err)
	}
	if err := svc.RegisterLocalDriver("stub-b", &stub.Driver{DeadlineMs: 200}); err != nil {
		t.Fatalf("RegisterLocalDriver: %v", err)
	}
	mux := NewMux(svc, Config{Token: testToken})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	req := authedRequest(t, http.MethodGet, srv.URL+"/v1/machines/test-machine/sessions/abc", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 — two local runtimes and no ?runtime= hint is genuinely ambiguous (api-http.md §3.3 amendment)", resp.StatusCode)
	}

	// The disambiguated form must succeed in routing (still 501, since the
	// driver itself is unsupported, but no longer ambiguous).
	req2 := authedRequest(t, http.MethodGet, srv.URL+"/v1/machines/test-machine/sessions/abc?runtime=stub-a", nil)
	resp2, err := http.DefaultClient.Do(req2)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusNotImplemented {
		t.Fatalf("status = %d, want 501 once ?runtime= disambiguates", resp2.StatusCode)
	}
}

func TestListSessions_StubReportsDegradedSource(t *testing.T) {
	_, srv := newTestServer(t)

	req := authedRequest(t, http.MethodGet, srv.URL+"/v1/sessions?scope=local", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 — a source that can't answer degrades the envelope, it does not fail the whole call (§3.3)", resp.StatusCode)
	}

	var col fleet.Collection[fleet.Session]
	if err := json.NewDecoder(resp.Body).Decode(&col); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if col.Complete() {
		t.Fatal("Complete() = true, want false — the only driver returned ErrUnsupported")
	}
	if len(col.Sources()) != 1 || col.Sources()[0].Status != fleet.SourceDegraded {
		t.Fatalf("Sources() = %+v, want exactly one degraded source", col.Sources())
	}
	if len(col.Items()) != 0 {
		t.Fatalf("Items() = %v, want empty", col.Items())
	}
}

func TestListSessions_InvalidScopeIs400(t *testing.T) {
	_, srv := newTestServer(t)

	req := authedRequest(t, http.MethodGet, srv.URL+"/v1/sessions?scope=bogus", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
}

func TestEvents_UnimplementedIsUnsupported(t *testing.T) {
	_, srv := newTestServer(t)

	req := authedRequest(t, http.MethodGet, srv.URL+"/v1/events", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNotImplemented {
		t.Fatalf("status = %d, want 501", resp.StatusCode)
	}
}

func TestSendInput_RefusalIsNotAnHTTPError(t *testing.T) {
	// This skeleton's stub driver only ever returns ErrUnsupported (a
	// driver-level failure, correctly 501) — it cannot produce a refused
	// DeliveryReceipt to prove the 200-not-4xx rule end to end. That
	// requires a driver capable of returning DeliveryReceipt{Outcome:
	// refused} at all, which does not exist yet in this repo. Documented
	// here as a known gap, not silently skipped.
	t.Skip("no driver in this skeleton can produce a refused DeliveryReceipt; see comment")
}
