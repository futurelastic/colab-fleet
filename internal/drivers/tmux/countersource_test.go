package tmux

import "testing"

// TestCounters_MergesCounterSource is colab-fleet #163's surface half: a count
// kept outside the driver reaches Counters — and therefore GET /v1/health —
// under its own name, read fresh on every call rather than captured once at
// construction.
func TestCounters_MergesCounterSource(t *testing.T) {
	var n int64
	d := New("testbox", WithCounterSource(func() map[string]int64 {
		return map[string]int64{"outside.thing": n}
	}))

	if got, ok := d.Counters()["outside.thing"]; !ok || got != 0 {
		t.Fatalf(`Counters()["outside.thing"] = %d, present=%v; want 0, present`, got, ok)
	}
	n = 3
	if got := d.Counters()["outside.thing"]; got != 3 {
		t.Errorf(`Counters()["outside.thing"] = %d after the source moved, want 3`, got)
	}
}

// TestCounters_CounterSourceNeverOverwritesOwnName pins the collision rule: a
// source reusing a name the driver already reports is dropped, never merged
// over it, because one name holding two facts is not a count.
func TestCounters_CounterSourceNeverOverwritesOwnName(t *testing.T) {
	d := New("testbox", WithCounterSource(func() map[string]int64 {
		return map[string]int64{counterInboxWritten: 99}
	}))
	d.counters.incr(counterInboxWritten)

	if got := d.Counters()[counterInboxWritten]; got != 1 {
		t.Errorf("%s = %d, want 1 — the driver's own count must keep its name", counterInboxWritten, got)
	}
}

// TestCounters_NoSourceNoNames is the off-by-default half: a nil source and no
// source both add nothing.
func TestCounters_NoSourceNoNames(t *testing.T) {
	d := New("testbox", WithCounterSource(nil))
	if got := d.Counters(); len(got) != 0 {
		t.Errorf("Counters() = %v, want empty for a fresh driver with no usable source", got)
	}
}
