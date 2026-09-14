package main

import (
	"strings"
	"testing"

	"github.com/godx-jp/colab-fleet/internal/drivers/tmux"
)

// TestInboxResolverCounters_ReachDriverCounters is colab-fleet #163: the two
// index counters the deploy notes tell an operator to watch must be readable
// through the terminal driver's Counters — the map GET /v1/health serves —
// once wired the way main.go wires them, zero included.
func TestInboxResolverCounters_ReachDriverCounters(t *testing.T) {
	floorMismatch := indexStartTimeMismatchCount()
	floorUnattestable := indexUnattestableEntryCount()

	d := tmux.New("testbox", tmux.WithCounterSource(inboxResolverCounters))
	got := d.Counters()

	for name, floor := range map[string]int64{
		counterIndexStartTimeMismatch: floorMismatch,
		counterIndexUnattestableEntry: floorUnattestable,
	} {
		v, ok := got[name]
		if !ok {
			t.Errorf("%s absent from Counters() — a wired resolver must report it, zero included", name)
			continue
		}
		// >= rather than ==: the counters are process-wide, so another test
		// in this package may have added to them between the two reads.
		if v < floor {
			t.Errorf("%s = %d, want >= %d (the atomic's value just before)", name, v, floor)
		}
	}
}

// TestInboxResolverCounters_StayOutOfInboxExitNamespace guards the naming
// rule: inbox.* is the send path's per-exit sum (ADR 150), and a send that
// never tried the inbox must leave no inbox.* name. Index lookups are a
// different fact and must never land inside that prefix.
func TestInboxResolverCounters_StayOutOfInboxExitNamespace(t *testing.T) {
	for name := range inboxResolverCounters() {
		if strings.HasPrefix(name, "inbox.") {
			t.Errorf("%s sits in the inbox.* exit namespace; resolver counters must use their own prefix", name)
		}
	}
}
