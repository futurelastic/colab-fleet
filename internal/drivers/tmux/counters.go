package tmux

import "sync"

// counterSet is a tiny, generalisable named-counter registry.
//
// It exists because of a distinction #44 drew explicitly: a retry that
// silently clears a strand still needs to be COUNTED, or the rate — the
// actual signal, per #44's measurement of three consecutive strands on one
// busy machine — is laundered away the moment it stops being a failure. A
// one-off `log.Printf` at the call site would have recorded that this
// happened; it would not have given anything a number to read later, and
// #44 asks for a counter, not a line that scrolls off.
//
// This is deliberately the smallest thing that satisfies that: a name and a
// count, nothing wired to an HTTP surface yet. #9 ("the service cannot see
// itself") already describes the larger shape this is one piece of, and
// names its own reason for staying unscheduled — no corpus yet to compare a
// surface against. Building that surface here, for one caller's two
// counters, would be answering a question #9 has not asked yet. What #44
// owes #9 is a registry the next fact can join without a new struct field
// and a new accessor — only a new name — and that is what this is.
type counterSet struct {
	mu sync.Mutex
	n  map[string]int64
}

func (c *counterSet) incr(name string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.n == nil {
		c.n = map[string]int64{}
	}
	c.n[name]++
}

// Snapshot returns a copy safe for a caller to read without racing further
// increments. Exported on the struct (unlike incr) because the day
// something reads this — a test, a health endpoint, a future #9 surface —
// it should not have to move packages to reach it.
func (c *counterSet) Snapshot() map[string]int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make(map[string]int64, len(c.n))
	for k, v := range c.n {
		out[k] = v
	}
	return out
}

// Names a future reader looks for. Kept beside the registry rather than
// scattered at each call site — the same discipline WaitingReason's const
// block applies to the strings a caller might branch on.
const (
	// counterInitialPromptRetried counts every time §2.1's initial-prompt
	// delivery needed the one in-window retry #44 added, whether or not
	// that retry went on to succeed. Incrementing it on a successful retry
	// too is the point: the count is what says a busy machine is racing
	// this keystroke, and a retry that quietly fixes it every time would
	// hide exactly that.
	counterInitialPromptRetried = "initial_prompt.delivery_retried"
	// counterInitialPromptStranded counts every time that retry was not
	// enough and the prompt was still unsent when deliverInitialPrompt gave
	// up on it — the case that used to reach nobody at all.
	counterInitialPromptStranded = "initial_prompt.delivery_stranded"
)
