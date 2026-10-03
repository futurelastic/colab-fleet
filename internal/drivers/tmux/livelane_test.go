package tmux

// #257: while a session's delivery lane is live it is the session's only input
// path for a caller's /input (driver.SendOptions.LiveLaneOnly). The module
// delivers the text or the send is refused with nothing written; the terminal
// is never the answer.

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	fleet "github.com/futurelastic/muster"
	"github.com/futurelastic/muster/internal/delivery/modclient/modtest"
	"github.com/futurelastic/muster/internal/driver"
)

// sendLive is one /input as the service makes it: LiveLaneOnly set, the options
// otherwise exactly as given (no forced Submit, unlike modRig.send).
func (r *modRig) sendLive(id, text string, o driver.SendOptions) fleet.DeliveryReceipt {
	r.t.Helper()
	o.LiveLaneOnly = true
	if o.From == nil && !o.HumanRelay {
		o.From = agentFrom
	}
	got, err := r.d.Send(context.Background(), testCaller, fleet.SessionRef{Machine: "testbox", ID: id}, text, o)
	if err != nil {
		r.t.Fatalf("Send: %v", err)
	}
	return got
}

const noLaneSession = "alpha💬"

func TestLiveLane_PlainSendGoesToModule(t *testing.T) {
	r, id := liveRig(t, modtest.Behaviour{})
	got := r.sendLive(id, "do the thing", driver.SendOptions{Submit: true})
	if got.RouteOf() != fleet.RouteModule || got.ModuleOf() != modName {
		t.Fatalf("receipt = %+v, want the module", got)
	}
	if r.sends() != 1 || r.pastes() != 0 {
		t.Errorf("module sends = %d, pastes = %d; want 1 and 0", r.sends(), r.pastes())
	}
	if r.counter("route.decided.auto.module") != 1 {
		t.Errorf("route counters: %v", r.d.Counters())
	}
}

func TestLiveLane_RefusesComposerShapes(t *testing.T) {
	for _, tc := range []struct {
		name  string
		opts  driver.SendOptions
		field string
	}{
		{"explicit terminal", driver.SendOptions{Submit: true, Route: fleet.RouteTerminal}, "terminal"},
		{"explicit terminal from a human relay", driver.SendOptions{Submit: true, Route: fleet.RouteTerminal, HumanRelay: true}, "terminal"},
		{"submit false", driver.SendOptions{Submit: false}, "submit_false"},
		{"resume", driver.SendOptions{Submit: true, ResumeIfStranded: true}, "resume"},
		{"replace", driver.SendOptions{Submit: true, ReplaceIfStranded: true}, "replace"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, id := liveRig(t, modtest.Behaviour{})
			got := r.sendLive(id, "hello", tc.opts)
			if got.Outcome != fleet.OutcomeRefused {
				t.Fatalf("receipt = %+v, want refused", got)
			}
			for _, want := range []string{"live", modName, "Nothing was written"} {
				if !strings.Contains(got.Reason, want) {
					t.Errorf("reason %q does not contain %q", got.Reason, want)
				}
			}
			if got.RouteOf() != "" {
				t.Errorf("route = %q, want none: a refusal made before any path was chosen", got.RouteOf())
			}
			if r.sends() != 0 || r.pastes() != 0 {
				t.Errorf("module sends = %d, pastes = %d; want nothing written", r.sends(), r.pastes())
			}
			if r.counter(counterRouteRefusedLaneLive) != 1 || r.counter(counterRouteRefusedLaneLivePrefix+tc.field) != 1 {
				t.Errorf("counters: %v", r.d.Counters())
			}
		})
	}
}

// The create-time prompt and the title sync leave LiveLaneOnly false: the same
// shapes keep their terminal path on a live lane.
func TestLiveLane_InternalCallersKeepTheTerminal(t *testing.T) {
	r, id := liveRig(t, modtest.Behaviour{})
	got := r.send(id, "/rename x", driver.SendOptions{Route: fleet.RouteTerminal, HumanRelay: true})
	if got.RouteOf() != fleet.RouteTerminal || r.pastes() != 1 {
		t.Fatalf("receipt = %+v, pastes = %d; want the terminal for a caller without LiveLaneOnly", got, r.pastes())
	}
}

// Text this driver left stranded in the composer is not resumed on a live lane
// and not sent through the lane either (it would arrive twice).
func TestLiveLane_StrandedTextRefusedNotResumed(t *testing.T) {
	r, id := liveRig(t, modtest.Behaviour{})
	r.mux.noEcho = true
	first := r.send(id, "already in the composer", driver.SendOptions{Route: fleet.RouteTerminal})
	if first.Outcome != fleet.OutcomeUnknown {
		t.Fatalf("setup: receipt = %+v, want a stranded terminal delivery", first)
	}
	pastes := r.pastes()
	got := r.sendLive(id, "already in the composer", driver.SendOptions{Submit: true})
	if got.Outcome != fleet.OutcomeRefused || !strings.Contains(got.Reason, "discard") || !strings.Contains(got.Reason, "Nothing was written") {
		t.Fatalf("receipt = %+v, want a refusal naming discard", got)
	}
	if r.sends() != 0 || r.pastes() != pastes {
		t.Errorf("sends = %d, pastes %d -> %d; nothing may be written", r.sends(), pastes, r.pastes())
	}
	if r.counter(counterRouteRefusedLaneLivePrefix+"stranded_text") != 1 {
		t.Errorf("counters: %v", r.d.Counters())
	}
}

// A lane that read live at the gate and declined before any byte was written is
// a refusal, not a send to the terminal; the next send, seeing no live lane,
// takes the terminal honestly.
func TestLiveLane_LaneLostMidSendRefusedNotTerminal(t *testing.T) {
	r, id := liveRig(t, modtest.Behaviour{})
	var mu sync.Mutex
	refuse := true
	r.fake.SetHandler(func(req modtest.Request) (any, *modtest.WireError, time.Duration) {
		mu.Lock()
		defer mu.Unlock()
		if req.Op == "send" && refuse {
			return nil, &modtest.WireError{Code: "not-live", Message: "no client", Retryable: true}, 0
		}
		return nil, nil, 0
	})
	got := r.sendLive(id, "first", driver.SendOptions{Submit: true})
	if got.Outcome != fleet.OutcomeRefused || !strings.Contains(got.Reason, "Nothing was written") {
		t.Fatalf("receipt = %+v, want a refusal", got)
	}
	if r.pastes() != 0 {
		t.Errorf("pastes = %d: the terminal carried a send the lane lost", r.pastes())
	}
	if r.counter(counterRouteRefusedLaneLost) != 1 {
		t.Errorf("counters: %v", r.d.Counters())
	}
	next := r.sendLive(id, "second", driver.SendOptions{Submit: true})
	if next.RouteOf() != fleet.RouteTerminal {
		t.Errorf("the send after the lane degraded: %+v, want the terminal (no live lane)", next)
	}
}

// A forced module keeps its own rules under LiveLaneOnly.
func TestLiveLane_ForcedModuleUnchanged(t *testing.T) {
	r, id := liveRig(t, modtest.Behaviour{})
	got := r.sendLive(id, "hello", driver.SendOptions{Submit: true, Route: modName})
	if got.RouteOf() != fleet.RouteModule {
		t.Fatalf("receipt = %+v, want the module", got)
	}
	bad := r.sendLive(id, "hello", driver.SendOptions{Route: modName})
	if bad.Outcome != fleet.OutcomeRefused || !strings.Contains(bad.Reason, "submit:false") {
		t.Fatalf("receipt = %+v, want the forced-module shape refusal", bad)
	}
}

func TestLiveLane_HumanRelayAutoUsesModuleUnlabelled(t *testing.T) {
	r, id := liveRig(t, modtest.Behaviour{})
	got := r.sendLive(id, "approve it", driver.SendOptions{Submit: true, HumanRelay: true})
	if got.RouteOf() != fleet.RouteModule {
		t.Fatalf("receipt = %+v, want the module", got)
	}
	if r.lastSendText() != "approve it" {
		t.Errorf("the module was given %q, want the person's text unlabelled", r.lastSendText())
	}
}

// No live lane: auto from an agent goes to the terminal, labelled; from a human
// relay, unlabelled. Neither tries the inbox. An explicit submit:false still
// stages the text.
func TestNoLane_AutoGoesToTheTerminal(t *testing.T) {
	r, _ := liveRig(t, modtest.Behaviour{})
	agent := r.sendLive(noLaneSession, "hello", driver.SendOptions{Submit: true})
	if agent.RouteOf() != fleet.RouteTerminal {
		t.Fatalf("agent auto: %+v, want the terminal", agent)
	}
	human := r.sendLive(noLaneSession, "typed by a person", driver.SendOptions{Submit: true, HumanRelay: true})
	if human.RouteOf() != fleet.RouteTerminal {
		t.Fatalf("human auto: %+v, want the terminal", human)
	}
	if r.sends() != 0 {
		t.Errorf("the module was sent %d messages for a session without a lane", r.sends())
	}
	if _, ok := r.d.Counters()["route.auto_fallback"]; ok {
		t.Errorf("route.auto_fallback present: %v", r.d.Counters())
	}
}

func TestNoLane_SubmitFalseStillStages(t *testing.T) {
	r, _ := liveRig(t, modtest.Behaviour{})
	got := r.sendLive(noLaneSession, "staged", driver.SendOptions{Submit: false})
	if got.Outcome != fleet.OutcomeQueued || !strings.Contains(got.Reason, "not submitted") {
		t.Fatalf("receipt = %+v, want queued, placed in the composer, not submitted", got)
	}
}

// A degraded lane is not live: the send is carried by the built-in path.
func TestDegradedLane_AutoFallsBackToBuiltin(t *testing.T) {
	r, id := liveRig(t, modtest.Behaviour{})
	r.d.mods.degrade(id, "test", false)
	got := r.sendLive(id, "hello", driver.SendOptions{Submit: true})
	if got.RouteOf() != fleet.RouteTerminal {
		t.Fatalf("receipt = %+v, want the terminal", got)
	}
}
