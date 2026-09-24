package tmux

import (
	"context"
	"strings"
	"sync"
	"testing"

	fleet "github.com/godx-jp/colab-fleet"
	"github.com/godx-jp/colab-fleet/internal/delivery"
	"github.com/godx-jp/colab-fleet/internal/driver"
)

// recordingModule is a delivery module that touches nothing and remembers
// what it was asked to deliver.
type recordingModule struct {
	mu       sync.Mutex
	got      []delivery.Delivery
	reserved []string
	result   delivery.Result
}

func (m *recordingModule) Name() string          { return "test" }
func (m *recordingModule) ReservedEnv() []string { return m.reserved }
func (m *recordingModule) Deliver(_ context.Context, in delivery.Delivery) (delivery.Result, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.got = append(m.got, in)
	return m.result, nil
}

// TestSendIsRoutedThroughTheDeliveryModule pins #180's seam: once Send has
// decided everything about the request, the installed module — and only it —
// delivers, and the caller gets the module's receipt verbatim.
func TestSendIsRoutedThroughTheDeliveryModule(t *testing.T) {
	f := twoSessions()
	m := &recordingModule{result: delivery.Result{
		Receipt: fleet.DeliveryReceipt{Outcome: fleet.OutcomeQueued, Reason: "from the test module"},
		Class:   delivery.Delivered,
	}}
	d := New("testbox", withExec(f.exec), withNonce(func() string { return testNonce }), WithDeliveryModule(m))
	ref := fleet.SessionRef{Machine: "testbox", ID: "alpha💬"}
	got, err := d.Send(context.Background(), testCaller, ref, "hel\x1blo", driver.SendOptions{Submit: true})
	if err != nil {
		t.Fatal(err)
	}
	if got.Reason != "from the test module" {
		t.Fatalf("receipt = %+v, want the module's own receipt verbatim", got)
	}
	if len(m.got) != 1 || m.got[0].Text != "hello" || m.got[0].Ref != ref || !m.got[0].Opts.Submit {
		t.Fatalf("module got %+v, want one sanitised delivery for %v", m.got, ref)
	}
	for _, c := range f.callsSnapshot() {
		if c[0] == "load-buffer" || c[0] == "send-keys" {
			t.Fatalf("the built-in terminal path ran although a module was installed: %v", c)
		}
	}
	if n := d.Counters()["delivery.test.outcome.queued"]; n != 1 {
		t.Fatalf("delivery.test.outcome.queued = %d, want 1", n)
	}
}

// TestSendRefusalsBeforeTheModuleNeverReachIt: the runtime-syntax guard and
// the contradictory-flags check are decisions about the request, made before
// any module runs, and still counted under the module.
func TestSendRefusalsBeforeTheModuleNeverReachIt(t *testing.T) {
	f := twoSessions()
	m := &recordingModule{}
	d := New("testbox", withExec(f.exec), WithDeliveryModule(m))
	ref := fleet.SessionRef{Machine: "testbox", ID: "alpha💬"}
	for _, tc := range []struct {
		text string
		opts driver.SendOptions
	}{
		{"!rm -rf /", driver.SendOptions{Submit: true}},
		{"hello", driver.SendOptions{Submit: true, ResumeIfStranded: true, ReplaceIfStranded: true}},
	} {
		got, err := d.Send(context.Background(), testCaller, ref, tc.text, tc.opts)
		if err != nil {
			t.Fatal(err)
		}
		if got.Outcome != fleet.OutcomeRefused {
			t.Fatalf("%q: outcome = %q, want refused", tc.text, got.Outcome)
		}
	}
	if len(m.got) != 0 {
		t.Fatalf("module was reached for a request refused on its own terms: %+v", m.got)
	}
	if n := d.Counters()["delivery.test.refused"]; n != 2 {
		t.Fatalf("delivery.test.refused = %d, want 2", n)
	}
}

// TestEverySendIsClassifiedExactlyOnce: across queued, refused, stranded
// and unknown outcomes on the built-in module, the outcome counters sum to
// the number of Send calls, each outcome is counted under the class that
// describes it, and nothing lands in "unclassified".
func TestEverySendIsClassifiedExactlyOnce(t *testing.T) {
	ref := fleet.SessionRef{Machine: "testbox", ID: "alpha💬"}
	calls := 0
	total := map[string]int64{}
	add := func(d *Driver) {
		for k, v := range d.Counters() {
			if strings.HasPrefix(k, "delivery.tmux.") {
				total[k] += v
			}
		}
	}

	// queued
	f := twoSessions()
	d := newTestDriver(f)
	if got, _ := d.Send(context.Background(), testCaller, ref, "hello", driver.SendOptions{Submit: true}); got.Outcome != fleet.OutcomeQueued {
		t.Fatalf("setup: %+v", got)
	}
	calls++
	// refused by the guard
	if got, _ := d.Send(context.Background(), testCaller, ref, "!id", driver.SendOptions{Submit: true}); got.Outcome != fleet.OutcomeRefused {
		t.Fatalf("setup: %+v", got)
	}
	calls++
	add(d)

	// stranded: the submit is swallowed, a record is kept
	f = twoSessions()
	f.swallowSubmit = true
	d = newTestDriver(f)
	if got, _ := d.Send(context.Background(), testCaller, ref, "an instruction", driver.SendOptions{Submit: true}); got.Outcome != fleet.OutcomeUnknown {
		t.Fatalf("setup: %+v", got)
	}
	calls++
	add(d)

	var outcomes int64
	for k, v := range total {
		if strings.HasPrefix(k, "delivery.tmux.outcome.") {
			outcomes += v
		}
	}
	if outcomes != int64(calls) {
		t.Fatalf("outcome counters sum to %d over %d sends: %v", outcomes, calls, total)
	}
	for _, want := range []string{"delivery.tmux.delivered", "delivery.tmux.refused", "delivery.tmux.stranded"} {
		if total[want] != 1 {
			t.Errorf("%s = %d, want 1 (%v)", want, total[want], total)
		}
	}
	if total["delivery.tmux.unclassified"] != 0 {
		t.Fatalf("a send went unclassified: %v", total)
	}
	if total["delivery.tmux.confirmed.screen"] != 1 {
		t.Errorf("the one confirmed send should be screen-confirmed on a fake with no transcript: %v", total)
	}
}

// TestCreateRefusesCallerEnvNamingAReservedName: the service is the sole
// setter of a name the delivery module reserves (#180). The built-in module
// reserves none; a test module stands in for one that does.
func TestCreateRefusesCallerEnvNamingAReservedName(t *testing.T) {
	f := twoSessions()
	m := &recordingModule{reserved: []string{"FLEET_TEST_RESERVED"}}
	d := New("testbox", withExec(f.exec), WithDeliveryModule(m))
	_, err := d.Create(context.Background(), testCaller, "key-1", fleet.SessionSpec{
		Machine: "testbox", Cwd: "/work/new", Env: map[string]string{"FLEET_TEST_RESERVED": "x", "OTHER": "y"},
	})
	if err == nil || !strings.Contains(err.Error(), "FLEET_TEST_RESERVED") {
		t.Fatalf("err = %v, want a refusal naming the reserved variable", err)
	}
	for _, c := range f.callsSnapshot() {
		if c[0] == "new-session" {
			t.Fatal("a session was created despite the refusal")
		}
	}
	if got := newTestDriver(f).ReservedEnv(); len(got) != 0 {
		t.Fatalf("the built-in module reserves %v, want none", got)
	}
}

func TestSessionEnvNamingAReservedNameIsRejectedAtStartup(t *testing.T) {
	entries := []SessionEnvEntry{{Name: "FLEET_TEST_RESERVED", FromFile: "/nonexistent"}}
	d := New("testbox", WithSessionEnv(entries),
		WithDeliveryModule(&recordingModule{reserved: []string{"FLEET_TEST_RESERVED"}}))
	if err := d.ValidateSessionEnvReserved(); err == nil || !strings.Contains(err.Error(), "FLEET_TEST_RESERVED") {
		t.Fatalf("err = %v, want a startup refusal naming it", err)
	}
	if err := New("testbox", WithSessionEnv(entries)).ValidateSessionEnvReserved(); err != nil {
		t.Fatalf("the built-in module reserves nothing, yet: %v", err)
	}
}
