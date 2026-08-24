package opencode

import (
	"context"
	"testing"

	fleet "github.com/godx-jp/colab-fleet"
)

// colab-fleet #84: unlike the tmux driver, this substrate's own create
// response names the agent it started with, so a requested agent is
// genuinely observable rather than only assumed — and Session.Agent
// reports that observed value, never an echo of the request.
func TestCreate_PinsReportsObservedAgent(t *testing.T) {
	f := newFakeServer(t)
	d := newTestDriver(t, f)

	sess, err := d.Create(context.Background(), fleet.RequestFrom(fleet.Caller{Principal: "test"}), "key-1",
		fleet.SessionSpec{Cwd: "/work/x", Name: "t", Agent: "coder"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if sess.Pins == nil || sess.Pins.Agent == nil {
		t.Fatalf("Pins = %+v, want an Agent entry", sess.Pins)
	}
	a := sess.Pins.Agent
	if a.Requested != "coder" {
		t.Errorf("Requested = %q, want coder", a.Requested)
	}
	if a.Honoured == nil || !*a.Honoured {
		t.Errorf("Honoured = %v, want true — the fake server echoes the same agent back", a.Honoured)
	}
	if a.Source != fleet.PinObserved {
		t.Errorf("Source = %q, want observed", a.Source)
	}
	if a.Applied != "coder" {
		t.Errorf("Applied = %q, want coder", a.Applied)
	}
	if sess.Agent != "coder" {
		t.Errorf("Session.Agent = %q, want the observed applied value coder", sess.Agent)
	}
}

// A requested model has no echo anywhere in this substrate's create
// response, so it must be reported unresolved rather than assumed honoured.
func TestCreate_PinsReportsModelUnresolved(t *testing.T) {
	f := newFakeServer(t)
	d := newTestDriver(t, f)

	sess, err := d.Create(context.Background(), fleet.RequestFrom(fleet.Caller{Principal: "test"}), "key-1",
		fleet.SessionSpec{Cwd: "/work/x", Name: "t", Model: "anthropic/claude"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if sess.Pins == nil || sess.Pins.Model == nil {
		t.Fatalf("Pins = %+v, want a Model entry", sess.Pins)
	}
	if sess.Pins.Model.Honoured != nil {
		t.Errorf("Honoured = %v, want nil — this substrate's response never names the model", sess.Pins.Model.Honoured)
	}
}

func TestCreate_NoPinsRequestedLeavesFieldAbsent(t *testing.T) {
	f := newFakeServer(t)
	d := newTestDriver(t, f)

	sess, err := d.Create(context.Background(), fleet.RequestFrom(fleet.Caller{Principal: "test"}), "key-1",
		fleet.SessionSpec{Cwd: "/work/x", Name: "t"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if sess.Pins != nil {
		t.Errorf("Pins = %+v, want nil", sess.Pins)
	}
}
