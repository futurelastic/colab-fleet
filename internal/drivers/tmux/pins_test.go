package tmux

import (
	"context"
	"testing"

	fleet "github.com/godx-jp/colab-fleet"
)

// colab-fleet #84: a pin that reaches the argv intact — never a value this
// driver would refuse — must be reported unresolved rather than echoed as
// applied. TestFlagShapedPinIsRefusedRatherThanDropped (create_capabilities_test.go)
// covers the detectable, refused half; these cover the other half.

func TestCreate_PinsReportUnresolvedNotEchoed(t *testing.T) {
	f := twoSessions()
	d := newTestDriver(f)
	sess, err := d.Create(context.Background(), testCaller, "key-1",
		fleet.SessionSpec{Name: "pinned", Cwd: "/work/x", Model: "opus"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if sess.Pins == nil || sess.Pins.Model == nil {
		t.Fatalf("Pins = %+v, want a Model entry", sess.Pins)
	}
	m := sess.Pins.Model
	if m.Requested != "opus" {
		t.Errorf("Requested = %q, want opus", m.Requested)
	}
	if m.Honoured != nil {
		t.Errorf("Honoured = %v, want nil — this driver cannot read a pin back", m.Honoured)
	}
	if m.Applied != "" {
		t.Errorf("Applied = %q, want empty", m.Applied)
	}
	if m.Evidence == "" {
		t.Error("an unresolved pin must still explain itself")
	}
	// The defect this closes: Session.Model must not echo the request.
	if sess.Model != "" {
		t.Errorf("Session.Model = %q, want empty — this driver never observes the applied "+
			"model, so echoing the request is exactly colab-fleet #84's fabricated answer", sess.Model)
	}
	// Agent/Effort were never requested — must not manufacture entries.
	if sess.Pins.Agent != nil || sess.Pins.Effort != nil {
		t.Errorf("Pins = %+v, want agent and effort both absent", sess.Pins)
	}
}

func TestCreate_NoPinsRequestedLeavesFieldAbsent(t *testing.T) {
	f := twoSessions()
	d := newTestDriver(f)
	sess, err := d.Create(context.Background(), testCaller, "key-1",
		fleet.SessionSpec{Name: "unpinned", Cwd: "/work/x"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if sess.Pins != nil {
		t.Errorf("Pins = %+v, want nil (an empty struct is not the same fact as absent)", sess.Pins)
	}
}

// All three pins requested at once each get their own, independent entry.
func TestCreate_AllThreePinsReportedIndependently(t *testing.T) {
	f := twoSessions()
	d := newTestDriver(f)
	sess, err := d.Create(context.Background(), testCaller, "key-1", fleet.SessionSpec{
		Name: "triple", Cwd: "/work/x", Agent: "coder", Model: "opus", Effort: "high",
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if sess.Pins == nil {
		t.Fatal("Pins is nil")
	}
	for name, got := range map[string]*fleet.PinResult{
		"agent": sess.Pins.Agent, "model": sess.Pins.Model, "effort": sess.Pins.Effort,
	} {
		if got == nil {
			t.Errorf("%s: entry is nil", name)
			continue
		}
		if got.Honoured != nil {
			t.Errorf("%s: Honoured = %v, want nil", name, got.Honoured)
		}
	}
	if sess.Pins.Agent.Requested != "coder" || sess.Pins.Model.Requested != "opus" || sess.Pins.Effort.Requested != "high" {
		t.Errorf("Pins = %+v, requested values do not match", sess.Pins)
	}
}

// pinOutcomeFor's own mapping, exercised directly against a createRecord.
func TestPinOutcomeFor(t *testing.T) {
	if got := pinOutcomeFor(createRecord{}); got != nil {
		t.Errorf("no pins requested: got %+v, want nil", got)
	}
	got := pinOutcomeFor(createRecord{Model: "opus"})
	if got == nil || got.Model == nil || got.Model.Requested != "opus" || got.Model.Honoured != nil {
		t.Errorf("model requested: got %+v", got)
	}
	if got.Agent != nil || got.Effort != nil {
		t.Errorf("only model was requested: got %+v", got)
	}
}
