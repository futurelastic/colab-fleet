package opencode

import (
	"context"
	"errors"
	"strings"
	"testing"

	fleet "github.com/godx-jp/colab-fleet"
	"github.com/godx-jp/colab-fleet/internal/driver"
)

func createOne(t *testing.T, d *Driver, cwd, key string) fleet.SessionRef {
	t.Helper()
	ref, err := d.Create(context.Background(), fleet.RequestFrom(fleet.Caller{Principal: "test"}), key,
		fleet.SessionSpec{Cwd: fleet.AbsolutePath(cwd), Name: "t"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	return ref
}

func TestCreate_IdempotencyKeyReturnsSameSession(t *testing.T) {
	f := newFakeServer(t)
	d := newTestDriver(t, f)

	a := createOne(t, d, "/work/x", "key-1")
	b := createOne(t, d, "/work/x", "key-1")
	if a.ID != b.ID {
		t.Errorf("second create with the same key returned a different session: %q vs %q", a.ID, b.ID)
	}
	if len(f.sessions) != 1 {
		t.Errorf("fake server holds %d sessions, want exactly 1 (the repeat must not have created a second one)", len(f.sessions))
	}
}

func TestCreate_RequiresIdempotencyKey(t *testing.T) {
	f := newFakeServer(t)
	d := newTestDriver(t, f)
	_, err := d.Create(context.Background(), fleet.RequestFrom(fleet.Caller{}), "",
		fleet.SessionSpec{Cwd: "/work/x"})
	if err == nil {
		t.Fatal("Create with no idempotency key succeeded, want a refusal (§10)")
	}
}

// Effort is declared unsupported (SupportsPin.Effort: false) and must be
// refused rather than silently dropped (§2.1).
func TestCreate_RefusesEffortRatherThanDroppingIt(t *testing.T) {
	f := newFakeServer(t)
	d := newTestDriver(t, f)
	_, err := d.Create(context.Background(), fleet.RequestFrom(fleet.Caller{}), "key",
		fleet.SessionSpec{Cwd: "/work/x", Effort: "high"})
	if err == nil {
		t.Fatal("Create with Effort set succeeded, want a refusal — this driver has no honest way to honour it")
	}
	var fe *fleet.Error
	if errors.As(err, &fe) && fe.Kind != fleet.ErrorUnsupported {
		t.Errorf("error kind = %q, want unsupported", fe.Kind)
	}
}

func TestCreate_ModelMustBeProviderSlashModel(t *testing.T) {
	f := newFakeServer(t)
	d := newTestDriver(t, f)
	_, err := d.Create(context.Background(), fleet.RequestFrom(fleet.Caller{}), "key",
		fleet.SessionSpec{Cwd: "/work/x", Model: "not-a-provider-model-pair"})
	if err == nil {
		t.Fatal("Create with a malformed Model succeeded, want a refusal")
	}
}

func TestSend_DeliversAndReportsQueued_NeverSubmittedUnconfirmed(t *testing.T) {
	f := newFakeServer(t)
	d := newTestDriver(t, f)
	ref := createOne(t, d, "/work/x", "key-1")

	receipt, err := d.Send(context.Background(), fleet.RequestFrom(fleet.Caller{}),
		fleet.SessionRef{ID: ref.ID}, "hello", driver.SendOptions{Submit: true})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if receipt.Outcome != fleet.OutcomeQueued {
		t.Errorf("Outcome = %q, want queued — ConfirmsDelivery is false, so this driver must not claim Submitted", receipt.Outcome)
	}
}

func TestSend_SubmitFalseIsUnsupported_NoComposerToStageIn(t *testing.T) {
	f := newFakeServer(t)
	d := newTestDriver(t, f)
	ref := createOne(t, d, "/work/x", "key-1")

	_, err := d.Send(context.Background(), fleet.RequestFrom(fleet.Caller{}),
		fleet.SessionRef{ID: ref.ID}, "hello", driver.SendOptions{Submit: false})
	if !errors.Is(err, driver.ErrUnsupported) {
		t.Errorf("err = %v, want driver.ErrUnsupported", err)
	}
}

func TestSend_UnknownIDIsRefusedWithoutARoundTrip(t *testing.T) {
	f := newFakeServer(t)
	d := newTestDriver(t, f)

	before := len(f.requestsSnapshot())
	_, err := d.Send(context.Background(), fleet.RequestFrom(fleet.Caller{}),
		fleet.SessionRef{ID: "ses_never-created"}, "hello", driver.SendOptions{Submit: true})
	if !errors.Is(err, fleet.ErrNoSuchSession) {
		t.Errorf("err = %v, want fleet.ErrNoSuchSession", err)
	}
	if got := len(f.requestsSnapshot()); got != before {
		t.Errorf("Send on an unseen id made %d HTTP calls, want 0 (scope boundary: never seen, never asked)", got-before)
	}
}

// --- the core of #55: present/absent/failed discrimination -----------------

func TestState_BusySessionReportsWorkingObserved(t *testing.T) {
	f := newFakeServer(t)
	d := newTestDriver(t, f)
	ref := createOne(t, d, "/work/x", "key-1")
	f.setBusy(ref.ID)

	st, err := d.State(context.Background(), fleet.RequestFrom(fleet.Caller{}), fleet.SessionRef{ID: ref.ID})
	if err != nil {
		t.Fatalf("State: %v", err)
	}
	if st.Status != fleet.StatusWorking || st.Confidence != fleet.ConfidenceObserved {
		t.Errorf("got %+v, want working/observed", st)
	}
}

func TestState_RetrySessionReportsWorkingWithRetryableLastTurn(t *testing.T) {
	f := newFakeServer(t)
	d := newTestDriver(t, f)
	ref := createOne(t, d, "/work/x", "key-1")
	f.setRetry(ref.ID, 2, "temporary provider error")

	st, err := d.State(context.Background(), fleet.RequestFrom(fleet.Caller{}), fleet.SessionRef{ID: ref.ID})
	if err != nil {
		t.Fatalf("State: %v", err)
	}
	if st.Status != fleet.StatusWorking {
		t.Errorf("Status = %q, want working", st.Status)
	}
	if st.LastTurn == nil || !st.LastTurn.Retryable {
		t.Errorf("LastTurn = %+v, want Retryable: true", st.LastTurn)
	}
}

// A session absent from the status map (never prompted, or finished) and
// still confirmed to exist is genuinely idle — the "everyone quiet" case,
// distinguished from a failed read by the second GET succeeding.
func TestState_IdleSessionThatStillExistsReportsIdleObserved(t *testing.T) {
	f := newFakeServer(t)
	d := newTestDriver(t, f)
	ref := createOne(t, d, "/work/x", "key-1")
	// Never set busy/retry, and never cleared — it was simply never there.

	st, err := d.State(context.Background(), fleet.RequestFrom(fleet.Caller{}), fleet.SessionRef{ID: ref.ID})
	if err != nil {
		t.Fatalf("State: %v", err)
	}
	if st.Status != fleet.StatusIdle || st.Confidence != fleet.ConfidenceObserved {
		t.Errorf("got %+v, want idle/observed", st)
	}
}

// The trap #55 exists to close: a status-map read that FAILS must never
// render as idle, even though an empty/unreachable body looks identical
// to "nobody is busy" at the wire. This is the single most load-bearing
// assertion in this package.
func TestState_FailedStatusReadIsNeverIdle_UnreachablePropagates(t *testing.T) {
	f := newFakeServer(t)
	d := newTestDriver(t, f)
	ref := createOne(t, d, "/work/x", "key-1")

	f.statusDown = true
	st, err := d.State(context.Background(), fleet.RequestFrom(fleet.Caller{}), fleet.SessionRef{ID: ref.ID})
	if err == nil {
		t.Fatalf("State returned no error while the status read was down; got state %+v — this is exactly the idle-on-failure trap #55 exists to close", st)
	}
	if st.Status == fleet.StatusIdle {
		t.Fatalf("State returned StatusIdle on a failed read: %+v", st)
	}
	var fe *fleet.Error
	if !errors.As(err, &fe) {
		t.Fatalf("err = %v (%T), want a *fleet.Error", err, err)
	}
	if fe.Kind != fleet.ErrorUnreachable {
		t.Errorf("error kind = %q, want unreachable", fe.Kind)
	}
}

// The other half of the same trap: a credential the runtime rejects must
// surface as unauthorized, never as idle.
func TestState_UnauthorizedReadIsNeverIdle(t *testing.T) {
	f := newFakeServer(t)
	d := newTestDriver(t, f)
	ref := createOne(t, d, "/work/x", "key-1")

	f.unauthorized = true
	st, err := d.State(context.Background(), fleet.RequestFrom(fleet.Caller{}), fleet.SessionRef{ID: ref.ID})
	if err == nil {
		t.Fatalf("State returned no error while unauthorized; got %+v", st)
	}
	var fe *fleet.Error
	if !errors.As(err, &fe) || fe.Kind != fleet.ErrorUnauthorized {
		t.Errorf("err = %v, want *fleet.Error{Kind: unauthorized}", err)
	}
}

// An id this driver never created or listed is refused immediately,
// without ever asking the runtime — the scope boundary in the package
// doc, verified structurally rather than by comment.
func TestState_NeverSeenIDIsRefusedWithoutARoundTrip(t *testing.T) {
	f := newFakeServer(t)
	d := newTestDriver(t, f)

	before := len(f.requestsSnapshot())
	_, err := d.State(context.Background(), fleet.RequestFrom(fleet.Caller{}), fleet.SessionRef{ID: "ses_never-created"})
	if !errors.Is(err, fleet.ErrNoSuchSession) {
		t.Errorf("err = %v, want fleet.ErrNoSuchSession", err)
	}
	if got := len(f.requestsSnapshot()); got != before {
		t.Errorf("made %d HTTP calls for a never-seen id, want 0", got-before)
	}
}

// A session this driver previously saw, now gone (deleted by another
// client), is `dead` — a claim about history — never "no such session",
// which would tell the caller its mistyped id was simply wrong.
func TestState_PreviouslySeenNowGoneIsDead_NeverNoSuchSession(t *testing.T) {
	f := newFakeServer(t)
	d := newTestDriver(t, f)
	ref := createOne(t, d, "/work/x", "key-1")

	f.mu.Lock()
	delete(f.sessions, ref.ID)
	f.mu.Unlock()

	st, err := d.State(context.Background(), fleet.RequestFrom(fleet.Caller{}), fleet.SessionRef{ID: ref.ID})
	if err != nil {
		t.Fatalf("State: %v, want a successful dead reading, not an error", err)
	}
	if st.Status != fleet.StatusDead {
		t.Errorf("Status = %q, want dead", st.Status)
	}
}

// --- List: session identity known, status unknown ---------------------------

func TestList_ScopedToSessionsThisDriverHasSeen(t *testing.T) {
	f := newFakeServer(t)
	d := newTestDriver(t, f)
	ref := createOne(t, d, "/work/x", "key-1")

	// A session that exists on the runtime but was never created or
	// listed through this driver — simulating another client's history
	// sharing the same opencode server (package doc's scope boundary).
	f.mu.Lock()
	f.sessions["ses_outside-fleet"] = wireSession{ID: "ses_outside-fleet", Directory: "/elsewhere", Title: "not ours"}
	f.mu.Unlock()

	col, err := d.List(context.Background(), fleet.RequestFrom(fleet.Caller{}), driver.ListFilter{})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	ids := map[string]bool{}
	for _, s := range col.Items() {
		ids[s.ID] = true
	}
	if !ids[ref.ID] {
		t.Errorf("List did not report the session this driver created: %v", ids)
	}
	if ids["ses_outside-fleet"] {
		t.Error("List reported a session this driver never saw — scope boundary violated")
	}
}

// List against a driver that has cached nothing (a fresh process, or one
// that has never created or read a session) succeeds and reports nothing —
// an empty, complete answer, never an error.
func TestList_NothingKnownYetIsEmptyAndComplete(t *testing.T) {
	f := newFakeServer(t)
	d := newTestDriver(t, f)

	col, err := d.List(context.Background(), fleet.RequestFrom(fleet.Caller{}), driver.ListFilter{})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(col.Items()) != 0 {
		t.Errorf("Items = %v, want empty", col.Items())
	}
	if !col.Complete() {
		t.Error("Complete() = false, want true — the status read succeeded, it just found nothing to report")
	}
}

// List succeeding on the identity call but failing on the status call must
// report each known session as unknown, never silently idle — the same
// §5.7 discrimination as State, at the collection grain.
func TestList_StatusReadFailureReportsUnknown_NeverIdle(t *testing.T) {
	f := newFakeServer(t)
	d := newTestDriver(t, f)
	ref := createOne(t, d, "/work/x", "key-1")

	f.statusDown = true
	col, err := d.List(context.Background(), fleet.RequestFrom(fleet.Caller{}), driver.ListFilter{})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(col.Items()) != 1 {
		t.Fatalf("got %d items, want 1 (identity is still known)", len(col.Items()))
	}
	got := col.Items()[0]
	if got.ID != ref.ID {
		t.Fatalf("got session %q, want %q", got.ID, ref.ID)
	}
	if got.State.Status == fleet.StatusIdle {
		t.Errorf("Status = idle while the status read had failed — must be unknown")
	}
	if got.State.Status != fleet.StatusUnknown {
		t.Errorf("Status = %q, want unknown", got.State.Status)
	}
	if col.Complete() {
		t.Error("Complete() = true, want false — the status source did not fully answer")
	}
	srcs := col.Sources()
	if len(srcs) != 1 || srcs[0].Status != fleet.SourceDegraded {
		t.Errorf("Sources = %+v, want exactly one SourceDegraded", srcs)
	}
}

// --- Close: corroboration -----------------------------------------------

func TestClose_SucceedsAndDeletesTheSession(t *testing.T) {
	f := newFakeServer(t)
	d := newTestDriver(t, f)
	ref := createOne(t, d, "/work/x", "key-1")

	ack, err := d.Close(context.Background(), fleet.RequestFrom(fleet.Caller{}), fleet.SessionRef{ID: ref.ID})
	if err != nil {
		t.Fatalf("Close: %v", err)
	}
	if !ack.Accepted {
		t.Error("Accepted = false")
	}
	if _, ok := f.sessions[ref.ID]; ok {
		t.Error("session still present on the fake server after Close")
	}
}

func TestClose_UnseenIDIsRefused(t *testing.T) {
	f := newFakeServer(t)
	d := newTestDriver(t, f)
	_, err := d.Close(context.Background(), fleet.RequestFrom(fleet.Caller{}), fleet.SessionRef{ID: "ses_never-created"})
	if !errors.Is(err, fleet.ErrNoSuchSession) {
		t.Errorf("err = %v, want fleet.ErrNoSuchSession", err)
	}
}

// --- unsupported operations, declared not emulated --------------------------

func TestUnimplementedOperations_ReturnErrUnsupported_NeverEmulate(t *testing.T) {
	f := newFakeServer(t)
	d := newTestDriver(t, f)
	ref := createOne(t, d, "/work/x", "key-1")
	sref := fleet.SessionRef{ID: ref.ID}
	ctx := context.Background()
	req := fleet.RequestFrom(fleet.Caller{})

	if _, err := d.Respond(ctx, req, sref, fleet.Response{Choice: 1}); !errors.Is(err, driver.ErrUnsupported) {
		t.Errorf("Respond: err = %v, want ErrUnsupported", err)
	}
	if _, err := d.Discard(ctx, req, sref, ""); !errors.Is(err, driver.ErrUnsupported) {
		t.Errorf("Discard: err = %v, want ErrUnsupported", err)
	}
	if _, err := d.Rename(ctx, req, sref, "new-name"); !errors.Is(err, driver.ErrUnsupported) {
		t.Errorf("Rename: err = %v, want ErrUnsupported", err)
	}
	if _, err := d.Subscribe(ctx, req, driver.SubscribeFilter{}); !errors.Is(err, driver.ErrUnsupported) {
		t.Errorf("Subscribe: err = %v, want ErrUnsupported", err)
	}
	if ks, ok := driver.Driver(d).(driver.KeySender); ok {
		t.Errorf("driver unexpectedly implements KeySender: %v — DeliversRawKeys must stay honest", ks)
	}
}

// --- credential hygiene (Boss's provider ruling on #55) ---------------------

// The credential must reach the wire only via the Authorization header —
// never as a query parameter, a body field, or logged anywhere this test
// can see (the fake server itself records every request path; none of
// them may contain the password).
func TestCredential_NeverTravelsOutsideTheAuthorizationHeader(t *testing.T) {
	f := newFakeServer(t)
	d := newTestDriver(t, f)
	ref := createOne(t, d, "/work/x", "key-1")
	_, _ = d.State(context.Background(), fleet.RequestFrom(fleet.Caller{}), fleet.SessionRef{ID: ref.ID})

	for _, r := range f.requestsSnapshot() {
		if strings.Contains(r.path, f.password) {
			t.Fatalf("credential leaked into a request path: %s %s", r.method, r.path)
		}
	}
}
