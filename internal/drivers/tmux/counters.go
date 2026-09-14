package tmux

import (
	"sync"
	"time"
)

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
	// counterInitialPromptSessionGone counts every time settleNewSession gave
	// up because the session itself was confirmed gone (colab-fleet #125),
	// never because a timer expired — there is no timer any more. A nonzero
	// rate here says sessions are dying before their initial prompt lands,
	// which is a different signal from delivery_stranded and worth telling
	// apart from it.
	counterInitialPromptSessionGone = "initial_prompt.delivery_session_gone"

	// colab-fleet #104: confirmSubmitted's own doc comment already names two
	// INDEPENDENT confirming signals — the composer reading fully empty, or
	// this delivery's own attributed marker count falling below what it was
	// pasted at — added in that order because the second one was found
	// necessary AFTER the first shipped (residue on the composer line can
	// keep it non-empty forever). #104's suspicion is that this history could
	// repeat in the other direction: nothing proves both branches still fire
	// in practice, so "confirmation rests on two signals" could quietly be
	// "confirmation rests on one, and the other is dead code nobody removed."
	// A live capture can't answer that — an agent session is refused from
	// driving the multiplexer directly, by design (#104's own text) — but
	// which branch decided a given confirmation is knowable at the moment it
	// is decided, inside this driver's own call. Counting it here turns an
	// unobservable question into a rate anyone can read afterwards: if
	// counterSubmitConfirmedByMarkerCleared stays at zero across real
	// traffic, that is the dead branch #104 suspects, found without a single
	// capture — the
	// same idiom #116 used for counterIdentityContested's sibling question.
	counterSubmitConfirmedByComposerEmpty = "submit_confirm.by_composer_empty"
	counterSubmitConfirmedByMarkerCleared = "submit_confirm.by_marker_cleared"
	// counterSubmitConfirmTimeout counts a confirmSubmitted call that never
	// saw either signal inside submitConfirmWindow. This is not new
	// information — the caller already turns this into an `unknown` receipt
	// and a stranded record — but it gives the timeout itself a rate,
	// independent of what the caller went on to do with it.
	counterSubmitConfirmTimeout = "submit_confirm.timeout"

	// #104's second question: submitConfirmWindow (4s) was inherited, never
	// derived from how long a real submit actually takes to show. A live
	// capture would answer that by watching one pane; this answers it the
	// same way as the branch question above — by having the service notice,
	// on every call it already makes, how long confirmation actually took.
	// Bucketed rather than summed/averaged: this repo takes no third-party
	// dependency (no histogram library), and a mean would hide exactly the
	// tail this question is about — a window near the tail bucket is a
	// budget worth revisiting even if the mean looks comfortable. Buckets
	// are exclusive (each confirmation lands in exactly one), the top one
	// capped at submitConfirmWindow itself since nothing here confirms past
	// it — a call that reaches the window without either signal firing is
	// counterSubmitConfirmTimeout instead, not a "latency" of any length.
	counterSubmitConfirmLatencyUnder250ms = "submit_confirm.latency_under_250ms"
	counterSubmitConfirmLatencyUnder500ms = "submit_confirm.latency_under_500ms"
	counterSubmitConfirmLatencyUnder1s    = "submit_confirm.latency_under_1s"
	counterSubmitConfirmLatencyUnder2s    = "submit_confirm.latency_under_2s"
	counterSubmitConfirmLatencyUnder4s    = "submit_confirm.latency_under_4s"

	// colab-fleet#156: the batched capture's TIME wall. chunk_failed counts
	// every capture chunk that came back with nothing parseable, whatever
	// the cause and whether or not a retry then recovered it. The rate of
	// failures is the signal, and a retry that quietly fixes each one would
	// hide it (same argument as counterInitialPromptRetried). retry_recovered
	// and retry_failed split the retries that were attempted, and their ratio
	// answers whether retrying is worth doing. A stall across the whole server
	// shows up as retry_failed, and more retrying will not fix that.
	// slow_invocation counts every multiplexer invocation, listing included,
	// that SUCCEEDED but took longer than slowInvocationLine. It gives a wall
	// time distribution before the next failure, which the failure line
	// alone cannot.
	counterCaptureChunkFailed      = "capture.chunk_failed"
	counterCaptureRetryRecovered   = "capture.retry_recovered"
	counterCaptureRetryFailed      = "capture.retry_failed"
	counterEnumerateSlowInvocation = "enumerate.slow_invocation"

	// colab-fleet#150: the inbox send path's exits. sendViaInbox reports
	// every fallback as the same ok=false, and a fallback costs only
	// latency, so nothing outside this driver can tell the reasons apart —
	// the same laundering #44 names. attempted counts every call that had a
	// resolver to try; each such call then ends in exactly one of the ten
	// exits below it, so attempted equals their sum and a health read checks
	// itself. A machine with no inbox configured counts nothing at all: a
	// zero there would claim a measurement that was never taken.
	//
	// written, not delivered: nothing observes delivery on this path (#144).
	counterInboxAttempted                  = "inbox.attempted"
	counterInboxFallbackIdentityUnresolved = "inbox.fallback_identity_unresolved"
	counterInboxErrorIdentityResolve       = "inbox.error_identity_resolve"
	counterInboxFallbackResolverError      = "inbox.fallback_resolver_error"
	counterInboxFallbackResolverDeclined   = "inbox.fallback_resolver_declined"
	counterInboxRefusedIdentityUnverified  = "inbox.refused_identity_unverified"
	counterInboxFallbackNoModeClass        = "inbox.fallback_no_mode_class"
	counterInboxFallbackBodyUnattestable   = "inbox.fallback_body_unattestable"
	counterInboxFallbackDialFailed         = "inbox.fallback_dial_failed"
	counterInboxFallbackWriteFailed        = "inbox.fallback_write_failed"
	counterInboxWritten                    = "inbox.written"

	// The number #150's decision reads. attest_checked counts every call
	// that reached attestation; attest_body_lookalike counts those whose
	// body the current rule refuses, WHETHER OR NOT the class was also
	// missing. Attest checks the class first, so while an index still omits
	// classes a body refusal hides behind the class one, and
	// fallback_body_unattestable alone would read as a clean rate that is
	// only a masked one. See docs/adr/150-count-inbox-fallbacks-before-widening.md.
	counterInboxAttestChecked       = "inbox.attest_checked"
	counterInboxAttestBodyLookalike = "inbox.attest_body_lookalike"

	// colab-fleet#149: every refusal of a composer taller than the capture
	// window (#134's composerClipped), one name per verb. ADR 149 found no
	// evidence inside this driver that makes acting on such a composer safe,
	// so these refusals are permanent for as long as the state lasts. What
	// the ADR can still use is a rate: how often real traffic reaches this
	// state, and through which verb. That is the measurement its reopen
	// condition names. Counted at the refusal, not at the classification: a
	// read that sees a clipped composer and does nothing destroys nothing.
	counterComposerClippedRefusedDiscard = "composer_clipped.refused_discard"
	counterComposerClippedRefusedKeys    = "composer_clipped.refused_keys"
	counterComposerClippedRefusedSend    = "composer_clipped.refused_send"
)

// confirmLatencyBucket maps an observed confirm latency onto one of the five
// exclusive buckets declared above. See their doc comment for why buckets,
// and why the top one is capped at submitConfirmWindow rather than open-ended.
func confirmLatencyBucket(elapsed time.Duration) string {
	switch {
	case elapsed < 250*time.Millisecond:
		return counterSubmitConfirmLatencyUnder250ms
	case elapsed < 500*time.Millisecond:
		return counterSubmitConfirmLatencyUnder500ms
	case elapsed < time.Second:
		return counterSubmitConfirmLatencyUnder1s
	case elapsed < 2*time.Second:
		return counterSubmitConfirmLatencyUnder2s
	default:
		return counterSubmitConfirmLatencyUnder4s
	}
}
