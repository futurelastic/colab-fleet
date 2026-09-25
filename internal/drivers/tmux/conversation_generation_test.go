package tmux

import (
	"strings"
	"testing"
	"time"
)

// #202, at the store: what counts as "a different process", and what does not.

func liveAnswering(id string, started time.Time) liveConversationSource {
	return func() liveConversation {
		return liveConversation{id: id, evidence: "test record for " + id, startedAt: started}
	}
}

func liveUnable(why string) liveConversationSource {
	return func() liveConversation { return liveConversation{evidence: why} }
}

func TestProcessGenerationReplacedBy(t *testing.T) {
	t0 := time.Date(2026, time.January, 2, 15, 4, 5, 0, time.Local)
	for _, c := range []struct {
		name      string
		was, now  processGeneration
		wantMoved bool
	}{
		{"same pid, nothing measured", processGeneration{pid: 100}, processGeneration{pid: 100}, false},
		{"different pid", processGeneration{pid: 100}, processGeneration{pid: 300}, true},
		{"same pid, same start time", processGeneration{100, t0}, processGeneration{100, t0}, false},
		{"same pid, different start time: a recycled pid", processGeneration{100, t0}, processGeneration{100, t0.Add(time.Hour)}, true},
		{"same pid, start time measured on one side only", processGeneration{100, t0}, processGeneration{pid: 100}, false},
		{"no pid now is absence of evidence, not a change", processGeneration{pid: 100}, processGeneration{}, false},
		{"no pid then is absence of evidence, not a change", processGeneration{}, processGeneration{pid: 100}, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := c.was.replacedBy(c.now); got != c.wantMoved {
				t.Fatalf("%+v replacedBy %+v = %v, want %v", c.was, c.now, got, c.wantMoved)
			}
		})
	}
}

// A pid the multiplexer could not report on a later read says nothing about
// whether the process changed, so it must not blank a sound answer.
func TestConversationMemoSurvivesAReadThatCarriesNoPid(t *testing.T) {
	s := newConversationStore(t.TempDir())
	key := conversationKey{pane: "%1", created: sessionStart}
	first := s.lookup(key, "/work/alpha", "alpha💬", sessionStart, processGeneration{pid: 100}, liveAnswering(resumedConv, processStart))
	if first == nil || !first.Known {
		t.Fatalf("precondition: %+v", first)
	}
	got := s.lookup(key, "/work/alpha", "alpha💬", sessionStart, processGeneration{}, liveUnable("no pid"))
	if got != first {
		t.Fatalf("a read with no pid must be answered from the memo, got %+v", got)
	}
}

// A memo made while the pid was unknown adopts the pid the next read reports,
// so a replacement after that is noticed — without treating the first known pid
// as a replacement of "nothing".
func TestConversationMemoMadeWithoutAPidAdoptsTheNextOne(t *testing.T) {
	s := newConversationStore(t.TempDir())
	key := conversationKey{pane: "%1", created: sessionStart}
	first := s.lookup(key, "/work/alpha", "alpha💬", sessionStart, processGeneration{}, liveAnswering(resumedConv, processStart))
	if first == nil || !first.Known {
		t.Fatalf("precondition: %+v", first)
	}
	if got := s.lookup(key, "/work/alpha", "alpha💬", sessionStart, processGeneration{pid: 100}, liveUnable("not asked")); got != first {
		t.Fatalf("the first known pid is not a replacement, got %+v", got)
	}
	got := s.lookup(key, "/work/alpha", "alpha💬", sessionStart, processGeneration{pid: 300}, liveUnable("record not written yet"))
	if got == nil || got.Known {
		t.Fatalf("a different pid after that is a replacement, got %+v", got)
	}
}

// A recycled pid is the same number and a different process. It is noticed
// whenever a caller has measured the start time — the send path always has —
// and not otherwise, because measuring it costs a `ps` on a path that answers a
// whole fleet's listing.
func TestConversationMemoIsDroppedWhenARecycledPidIsMeasured(t *testing.T) {
	s := newConversationStore(t.TempDir())
	key := conversationKey{pane: "%1", created: sessionStart}
	then := processGeneration{pid: 100, startedAt: processStart}
	first := s.lookup(key, "/work/alpha", "alpha💬", sessionStart, then, liveAnswering(resumedConv, processStart))
	if first == nil || !first.Known {
		t.Fatalf("precondition: %+v", first)
	}
	// Not measured: a hit, and no question put to the record.
	if got := s.lookup(key, "/work/alpha", "alpha💬", sessionStart, processGeneration{pid: 100}, liveUnable("must not be asked")); got != first {
		t.Fatalf("an unmeasured start time is not a change, got %+v", got)
	}
	// Measured and equal: still a hit.
	if got := s.lookup(key, "/work/alpha", "alpha💬", sessionStart, then, liveUnable("must not be asked")); got != first {
		t.Fatalf("the same measured start time is not a change, got %+v", got)
	}
	// Measured and different: the same pid, a different process.
	now := processGeneration{pid: 100, startedAt: processStart.Add(time.Hour)}
	got := s.lookup(key, "/work/alpha", "alpha💬", sessionStart, now, liveUnable("the record belongs to an earlier process"))
	if got == nil || got.Known {
		t.Fatalf("a recycled pid must not keep the old conversation, got %+v", got)
	}
	if !strings.Contains(got.Evidence, "earlier process") {
		t.Errorf("the refusal must carry why the record was not used: %q", got.Evidence)
	}
}

// What the store remembers is the measured start time, taken from the answer
// that established it: the list path presents only a pid, and must still be
// able to tell a recycled pid from a later measured read.
func TestConversationMemoKeepsTheStartTimeTheLiveRecordWasCorroboratedAgainst(t *testing.T) {
	s := newConversationStore(t.TempDir())
	key := conversationKey{pane: "%1", created: sessionStart}
	first := s.lookup(key, "/work/alpha", "alpha💬", sessionStart, processGeneration{pid: 100}, liveAnswering(resumedConv, processStart))
	if first == nil || !first.Known {
		t.Fatalf("precondition: %+v", first)
	}
	got := s.lookup(key, "/work/alpha", "alpha💬", sessionStart,
		processGeneration{pid: 100, startedAt: processStart.Add(time.Minute)}, liveUnable("no record"))
	if got == nil || got.Known {
		t.Fatalf("a measured start time that differs from the one corroborated must drop the memo, got %+v", got)
	}
}
