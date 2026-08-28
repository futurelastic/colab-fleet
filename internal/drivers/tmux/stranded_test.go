package tmux

import (
	"context"
	"strings"
	"testing"
	"time"

	fleet "github.com/godx-jp/colab-fleet"
	"github.com/godx-jp/colab-fleet/internal/driver"
	"github.com/godx-jp/colab-fleet/internal/state"
)

// #11: a delivery this driver made and could not confirm used to live only in
// d.stranded, a plain in-memory map. A restart dropped it — and restarting is
// how this service is deployed — so resumeIfStranded, the one door out of
// §2.4's busy-composer refusal, stopped working on exactly the deploys it
// exists to survive. These tests exercise the fix: a durable record,
// corroborated on more than the id (§5.4), swept after strandedRetention.
//
// The restart itself is simulated exactly as idempotency_test.go's
// TestIdempotencyKeysSurviveARestart does: a new Driver constructed over the
// same state directory. Confirmed to fail against the pre-fix code (reverting
// noteStranded/strandedMatches to the plain map[string]string and dropping
// loadStranded makes TestStrandedRecordSurvivesARestart's resume return
// refused instead of submitted, because the second Driver's d.stranded starts
// empty).
func TestStrandedRecordSurvivesARestart(t *testing.T) {
	dir := t.TempDir()
	f := twoSessions()
	f.noEcho = true // composer never renders what was pasted, so it strands
	ref := fleet.SessionRef{Machine: "testbox", ID: "alpha💬"}
	const text = "text that stranded before the restart"

	first := stateDriver(t, f, dir)
	got, err := first.Send(context.Background(), testCaller, ref, text, driver.SendOptions{Submit: true})
	if err != nil {
		t.Fatal(err)
	}
	if got.Outcome != fleet.OutcomeUnknown {
		t.Fatalf("setup: outcome = %s, want unknown so the text is recorded as stranded", got.Outcome)
	}

	// A new process, same state directory — this is the restart. Nothing
	// about the composer changed: the text this driver placed is still
	// sitting there, unsubmitted, exactly as a restart leaves it. #109: the
	// capture must echo OUR OWN text so confirmLanded's resume-time gate can
	// attribute it — an unrelated composer fixture would now (correctly)
	// refuse to submit blind.
	second := stateDriver(t, f, dir)
	f.setCapture("%1", "transcript\n✻ Brewed for 1m 0s\n"+rule+"\n❯ "+text+"\n"+rule+"\n")

	resumed, err := second.Send(context.Background(), testCaller, ref, text,
		driver.SendOptions{Submit: true, ResumeIfStranded: true})
	if err != nil {
		t.Fatal(err)
	}
	// #101: a confirmed resume now reports the same outcome the first-attempt
	// path reports for the same evidence — queued, never submitted, since
	// this substrate cannot observe agent receipt either way (§4.3). Refused
	// or unknown here would mean the record did not survive the restart.
	if resumed.Outcome != fleet.OutcomeQueued {
		t.Fatalf("resume after restart: outcome = %s (%s), want queued — the stranded "+
			"record did not survive the restart", resumed.Outcome, resumed.Reason)
	}
}

// The lifetime half of #11: a record kept forever eventually matches text a
// human typed that happens to be identical, which is precisely what
// strandedMatches's exact-match rule exists to exclude. Mirrors
// TestExpiredKeysAreSweptOnLoad's shape: sweep happens on LOAD, using the
// loading driver's own clock, not the clock that wrote the record.
func TestStrandedRecordExpiresOnReload(t *testing.T) {
	dir := t.TempDir()
	f := twoSessions()
	st, err := state.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	old := time.Unix(1785760000, 0)

	first := New("testbox", withExec(f.exec), withNonce(func() string { return testNonce }),
		withClock(func() time.Time { return old }), WithState(st))
	first.noteStranded("alpha💬", "/work/alpha", "text nobody came back for", "")

	// Reload well past strandedRetention.
	second := New("testbox", withExec(f.exec), withNonce(func() string { return testNonce }),
		withClock(func() time.Time { return old.Add(strandedRetention + time.Minute) }), WithState(st))
	if second.strandedMatches("alpha💬", "/work/alpha", "text nobody came back for") {
		t.Error("an expired stranded record survived a reload")
	}
}

// §5.4 applied to this record: an id match alone is not identity. A durable
// record can outlive the session it describes, and a resume must not adopt
// whatever unrelated session later recycled the same id — corroborated here
// by cwd, the same attribute resolvePending already uses for the analogous
// idempotency case.
func TestStrandedMatchRequiresTheSameCwd(t *testing.T) {
	f := twoSessions()
	d := newTestDriver(f)
	d.noteStranded("alpha💬", "/work/alpha", "the original delivery", "")

	if d.strandedMatches("alpha💬", "/somewhere/else", "the original delivery") {
		t.Error("matched a stranded record against a different cwd — the exact " +
			"failure §5.4 exists to prevent: a recycled id is not the session " +
			"that record was made for")
	}
	if !d.strandedMatches("alpha💬", "/work/alpha", "the original delivery") {
		t.Error("the record's own cwd should still match")
	}
}

// Once a session is destroyed there is nothing left to resume into, so Close
// forgets any stranded record for it rather than leaving it to wait out
// strandedRetention — a context-free leftover is exactly what could collide
// if the same name were reused before the window lapsed.
func TestCloseForgetsAStrandedRecord(t *testing.T) {
	f := twoSessions()
	d := newTestDriver(f)
	d.noteStranded("alpha💬", "/work/alpha", "never resumed", "")

	if _, err := d.Close(context.Background(), expectStarted(1785600000),
		fleet.SessionRef{Machine: "testbox", ID: "alpha💬"}); err != nil {
		t.Fatal(err)
	}

	if d.strandedMatches("alpha💬", "/work/alpha", "never resumed") {
		t.Error("a stranded record survived Close of the session it was made for")
	}
}

// colab-fleet #112: the three-case refusal replacing the single "text a
// human typed" answer, and the ReplaceIfStranded door out of it.

// Case 3: the composer holds THIS driver's own stranded delivery, the new
// text is identical, but ResumeIfStranded was not set. The refusal must
// name the record and point at resumeIfStranded — never claim a human
// typed it, which is simply false here.
func TestSendRefusesOwnStrandedSameTextWithoutResumeFlagAccurately(t *testing.T) {
	f := twoSessions()
	f.swallowSubmit = true // lands and renders, submit swallowed — strands with the record's ComposerDigest populated
	d := newTestDriver(f)
	ref := fleet.SessionRef{Machine: "testbox", ID: "alpha💬"}
	const text = "the instruction"

	strand, err := d.Send(context.Background(), testCaller, ref, text, driver.SendOptions{Submit: true})
	if err != nil {
		t.Fatal(err)
	}
	if strand.Outcome != fleet.OutcomeUnknown {
		t.Fatalf("setup: outcome = %s, want unknown so the text strands", strand.Outcome)
	}

	got, err := d.Send(context.Background(), testCaller, ref, text, driver.SendOptions{Submit: true})
	if err != nil {
		t.Fatal(err)
	}
	if got.Outcome != fleet.OutcomeRefused {
		t.Fatalf("outcome = %s, want refused — no resumeIfStranded was set", got.Outcome)
	}
	if strings.Contains(got.Reason, "a human typed") {
		t.Errorf("reason = %q; this driver's OWN record shows this is its own text, not a human's", got.Reason)
	}
	if !strings.Contains(got.Reason, "resumeIfStranded") {
		t.Errorf("reason = %q; must name the way back in", got.Reason)
	}
}

// Case 4, the third case #112 asks for and the one that used to have no
// distinct answer at all: the composer holds THIS driver's own stranded
// delivery, but the new text is DIFFERENT and neither flag was set. The
// refusal must say so, and must name BOTH resumeIfStranded (finishes the
// old delivery) and replaceIfStranded (replaces it) — not the "a human
// typed" wording, which is what this exact case used to get.
func TestSendRefusesOwnStrandedDifferentTextNamingBothDoors(t *testing.T) {
	f := twoSessions()
	f.swallowSubmit = true
	d := newTestDriver(f)
	ref := fleet.SessionRef{Machine: "testbox", ID: "alpha💬"}

	strand, err := d.Send(context.Background(), testCaller, ref, "the original", driver.SendOptions{Submit: true})
	if err != nil {
		t.Fatal(err)
	}
	if strand.Outcome != fleet.OutcomeUnknown {
		t.Fatalf("setup: outcome = %s, want unknown so the text strands", strand.Outcome)
	}
	f.swallowSubmit = false

	got, err := d.Send(context.Background(), testCaller, ref, "something else entirely", driver.SendOptions{Submit: true})
	if err != nil {
		t.Fatal(err)
	}
	if got.Outcome != fleet.OutcomeRefused {
		t.Fatalf("outcome = %s, want refused — no flag was set", got.Outcome)
	}
	if strings.Contains(got.Reason, "a human typed") {
		t.Errorf("reason = %q; this driver's OWN record shows this is its own text, not a human's — "+
			"the exact misattribution #112 exists to fix", got.Reason)
	}
	if !strings.Contains(got.Reason, "resumeIfStranded") || !strings.Contains(got.Reason, "replaceIfStranded") {
		t.Errorf("reason = %q; must name BOTH doors — finish the old delivery, or replace it", got.Reason)
	}
}

// Case 5, unchanged: no stranded record exists at all, so the composer
// really might be a person's own unsent draft. This is the one case that
// keeps the original wording — regression guard against #112 diluting the
// genuinely-unknown-provenance answer.
func TestSendStillRefusesGenuineThirdPartyTextWithOriginalWording(t *testing.T) {
	f := twoSessions()
	d := newTestDriver(f) // beta's fixture (fixtureUnsent) is a human's typing, nothing to do with this driver
	got, err := d.Send(context.Background(), testCaller,
		fleet.SessionRef{Machine: "testbox", ID: "beta"}, "hello", driver.SendOptions{Submit: true})
	if err != nil {
		t.Fatal(err)
	}
	if got.Outcome != fleet.OutcomeRefused {
		t.Fatalf("outcome = %s, want refused", got.Outcome)
	}
	if !strings.Contains(got.Reason, "a human typed") {
		t.Errorf("reason = %q; with no stranded record at all, this is still the honest "+
			"answer — a human MAY have typed this, and this driver cannot rule it out", got.Reason)
	}
}

// resumeIfStranded and replaceIfStranded together is a contradiction —
// finish the old delivery, or throw it away, not both — and is refused
// before anything else runs, the same way #53's runtime-syntax guard is
// checked before session state.
func TestSendRefusesContradictoryResumeAndReplaceFlags(t *testing.T) {
	f := twoSessions()
	d := newTestDriver(f)
	got, err := d.Send(context.Background(), testCaller,
		fleet.SessionRef{Machine: "testbox", ID: "alpha💬"}, "hello",
		driver.SendOptions{Submit: true, ResumeIfStranded: true, ReplaceIfStranded: true})
	if err != nil {
		t.Fatal(err)
	}
	if got.Outcome != fleet.OutcomeRefused {
		t.Fatalf("outcome = %s, want refused — both flags set is a contradiction", got.Outcome)
	}
	if len(f.callsSnapshot()) != 0 {
		t.Errorf("the contradiction check must run BEFORE touching the substrate at all; "+
			"got %d subprocess calls", len(f.callsSnapshot()))
	}
}

// The headline fix: a session whose creation prompt (or any delivery)
// strands is no longer a dead end for a caller that has decided it wants
// DIFFERENT text. Before #112 the only escape was destroy-and-recreate;
// this delivers the replacement and leaves the session alive, no DELETE
// involved.
func TestReplaceIfStrandedDeliversDifferentTextBreakingTheDeadlock(t *testing.T) {
	f := twoSessions()
	f.swallowSubmit = true // lands and renders, submit swallowed — strands with a real ComposerDigest
	d := newTestDriver(f)
	ref := fleet.SessionRef{Machine: "testbox", ID: "alpha💬"}
	const original = "the original instruction"
	const replacement = "actually, do this instead"

	strand, err := d.Send(context.Background(), testCaller, ref, original, driver.SendOptions{Submit: true})
	if err != nil {
		t.Fatal(err)
	}
	if strand.Outcome != fleet.OutcomeUnknown {
		t.Fatalf("setup: outcome = %s, want unknown so the text strands", strand.Outcome)
	}
	f.swallowSubmit = false // the replacement's own submit must be allowed to register

	got, err := d.Send(context.Background(), testCaller, ref, replacement,
		driver.SendOptions{Submit: true, ReplaceIfStranded: true})
	if err != nil {
		t.Fatal(err)
	}
	if got.Outcome != fleet.OutcomeQueued {
		t.Fatalf("outcome = %s (%s), want queued — the replacement text was delivered and "+
			"submitted successfully", got.Outcome, got.Reason)
	}

	// No DELETE was needed: the session is still alive.
	col, err := d.List(context.Background(), testCaller, driver.ListFilter{})
	if err != nil {
		t.Fatal(err)
	}
	alive := false
	for _, s := range col.Items() {
		if s.ID == "alpha💬" {
			alive = true
		}
	}
	if !alive {
		t.Fatal("session no longer listed after replaceIfStranded — it must not require destroying the session")
	}

	// The stranded record for the ORIGINAL text is gone: a resume against
	// it now would find nothing to resume.
	if d.strandedMatches("alpha💬", "/work/alpha", original) {
		t.Error("the original stranded record survived a successful replace")
	}
}

// Safety rail: a residue already proven (#87) not to move must refuse the
// replace attempt outright, the same discipline Discard already applies,
// rather than pressing C-u into a composer known not to respond.
func TestReplaceIfStrandedRefusesWhenAPriorClearProvenFutile(t *testing.T) {
	f := twoSessions()
	f.swallowSubmit = true
	d := newTestDriver(f)
	ref := fleet.SessionRef{Machine: "testbox", ID: "alpha💬"}
	const original = "the original instruction"

	if _, err := d.Send(context.Background(), testCaller, ref, original, driver.SendOptions{Submit: true}); err != nil {
		t.Fatal(err)
	}
	f.swallowSubmit = false

	// A prior clear pass already spent a full window against this EXACT
	// residue and made no progress — recorded directly, the same way
	// Discard's own futility tests set this up.
	digest := screenDigest(original)
	d.noteFutile(ref.ID, "/work/alpha", digest)

	got, err := d.Send(context.Background(), testCaller, ref, "something else",
		driver.SendOptions{Submit: true, ReplaceIfStranded: true})
	if err != nil {
		t.Fatal(err)
	}
	if got.Outcome != fleet.OutcomeRefused {
		t.Fatalf("outcome = %s, want refused — this residue is already proven futile", got.Outcome)
	}
	if countClears(f.callsSnapshot()) != 0 {
		t.Errorf("a residue already proven futile must be refused BEFORE pressing C-u, "+
			"got %d clear keystrokes", countClears(f.callsSnapshot()))
	}
}

// Safety rail: the record's ComposerDigest not matching the composer's
// CURRENT content means something changed since the strand was recorded —
// possibly a human typing — and this driver must refuse to guess rather
// than clear text it cannot corroborate is still only its own.
func TestReplaceIfStrandedRefusesWhenComposerDigestNoLongerMatches(t *testing.T) {
	f := twoSessions()
	f.swallowSubmit = true
	d := newTestDriver(f)
	ref := fleet.SessionRef{Machine: "testbox", ID: "alpha💬"}
	const original = "the original instruction"

	if _, err := d.Send(context.Background(), testCaller, ref, original, driver.SendOptions{Submit: true}); err != nil {
		t.Fatal(err)
	}
	f.swallowSubmit = false

	// Something changed the composer since the strand was recorded — a
	// human attaching and typing over it, modelled directly on the fake.
	f.setCapture("%1", "transcript\n"+rule+"\n❯ somebody else's half-typed line\n"+rule+"\n")
	f.pasted["%1"] = ""

	got, err := d.Send(context.Background(), testCaller, ref, "something else",
		driver.SendOptions{Submit: true, ReplaceIfStranded: true})
	if err != nil {
		t.Fatal(err)
	}
	if got.Outcome != fleet.OutcomeRefused {
		t.Fatalf("outcome = %s, want refused — the composer's content changed since the strand", got.Outcome)
	}
	if !strings.Contains(got.Reason, "discard") {
		t.Errorf("reason = %q; must point at discard as the safe way to clear text this "+
			"driver cannot corroborate is still only its own", got.Reason)
	}
	if countClears(f.callsSnapshot()) != 0 {
		t.Errorf("a digest mismatch must be refused BEFORE pressing C-u, got %d clear keystrokes",
			countClears(f.callsSnapshot()))
	}
}

// colab-fleet #135: resumeIfStranded/replaceIfStranded should discard-then-
// send internally instead of dead-ending at §2.4 — the shape Case 5 above
// (TestSendStillRefusesGenuineThirdPartyTextWithOriginalWording) keeps
// unchanged for a bare send with NEITHER flag set. These exercise the two
// flags' new door out of that same starting point: no stranded record at
// all, only an opt-in flag on the call itself.
//
// beta's own fixture (twoSessions, fixtureUnsent) is used untouched — this
// driver never called Send against it, so there is genuinely no record, the
// exact shape #135 targets, as opposed to stranded_test.go's other cases
// above which all strand the record first via a real Send call.
const betaPendingText = "yes, update the skill"

// ReplaceIfStranded's door: with no record at all, there is nothing to
// "replace" in the record sense — this is #135's headline case, the one
// that used to dead-end at the unconditional §2.4 refusal regardless of
// either flag.
func TestReplaceIfStrandedClearsComposerWithNoStrandedRecord(t *testing.T) {
	f := twoSessions()
	d := newTestDriver(f)
	ref := fleet.SessionRef{Machine: "testbox", ID: "beta"}

	got, err := d.Send(context.Background(), testCaller, ref, "something new entirely",
		driver.SendOptions{Submit: true, ReplaceIfStranded: true})
	if err != nil {
		t.Fatal(err)
	}
	if got.Outcome != fleet.OutcomeQueued {
		t.Fatalf("outcome = %s (%s), want queued — an unrecorded composer must still clear "+
			"and deliver when replaceIfStranded opts in", got.Outcome, got.Reason)
	}

	col, err := d.List(context.Background(), testCaller, driver.ListFilter{})
	if err != nil {
		t.Fatal(err)
	}
	alive := false
	for _, s := range col.Items() {
		if s.ID == "beta" {
			alive = true
			if s.State.Status == fleet.StatusWaitingInput {
				t.Error("composer still reads waiting_input after a successful clear-and-deliver")
			}
		}
	}
	if !alive {
		t.Fatal("session no longer listed — clearing an unrecorded composer must not destroy the session")
	}
}

// ResumeIfStranded's door: there is nothing of ours to RESUME without a
// record of what this driver sent, so resumeIfStranded takes the identical
// clear-and-deliver path replaceIfStranded does above — the two converge on
// the same outcome once there is no record to distinguish "finish the old
// one" from "throw it away", because there is no old one either flag can
// name.
func TestResumeIfStrandedClearsComposerWithNoStrandedRecord(t *testing.T) {
	f := twoSessions()
	d := newTestDriver(f)
	ref := fleet.SessionRef{Machine: "testbox", ID: "beta"}

	got, err := d.Send(context.Background(), testCaller, ref, "something new entirely",
		driver.SendOptions{Submit: true, ResumeIfStranded: true})
	if err != nil {
		t.Fatal(err)
	}
	if got.Outcome != fleet.OutcomeQueued {
		t.Fatalf("outcome = %s (%s), want queued — an unrecorded composer must still clear "+
			"and deliver when resumeIfStranded opts in", got.Outcome, got.Reason)
	}
}

// Ask 1: every refusal whose actual remedy is /discard must name the exact
// call, composer digest included, not just gesture at the concept — this
// covers the one refusal #135 touched that keeps refusing (neither flag
// set), regression-guarding both the unchanged "a human typed" wording
// (Case 5, above) and the new exact-call addition alongside it.
func TestSendWithNeitherFlagNamesTheExactDiscardCall(t *testing.T) {
	f := twoSessions()
	d := newTestDriver(f)
	ref := fleet.SessionRef{Machine: "testbox", ID: "beta"}

	got, err := d.Send(context.Background(), testCaller, ref, "hello", driver.SendOptions{Submit: true})
	if err != nil {
		t.Fatal(err)
	}
	if got.Outcome != fleet.OutcomeRefused {
		t.Fatalf("outcome = %s, want refused", got.Outcome)
	}
	if !strings.Contains(got.Reason, "discard?expect="+screenDigest(betaPendingText)) {
		t.Errorf("reason = %q; must name the exact discard call with the composer's own "+
			"digest inlined, not just gesture at the concept", got.Reason)
	}
	if !strings.Contains(got.Reason, "resumeIfStranded") || !strings.Contains(got.Reason, "replaceIfStranded") {
		t.Errorf("reason = %q; must also name both opt-in doors now available", got.Reason)
	}
}

// Safety rail, generalised from TestReplaceIfStrandedRefusesWhenAPriorClearProvenFutile
// to the no-record case: #87's discipline (refuse before pressing again once
// a full pass already proved this exact residue does not move) must hold
// here too — futileClearAttempts is keyed on id+cwd+digest, independent of
// any stranded record, so it was already reachable from this new path
// without change; this pins that it actually fires.
func TestReplaceIfStrandedRefusesUnrecordedResidueAlreadyProvenFutile(t *testing.T) {
	f := twoSessions()
	d := newTestDriver(f)
	ref := fleet.SessionRef{Machine: "testbox", ID: "beta"}

	digest := screenDigest(betaPendingText)
	d.noteFutile(ref.ID, "/work/beta", digest)

	got, err := d.Send(context.Background(), testCaller, ref, "something new",
		driver.SendOptions{Submit: true, ReplaceIfStranded: true})
	if err != nil {
		t.Fatal(err)
	}
	if got.Outcome != fleet.OutcomeRefused {
		t.Fatalf("outcome = %s, want refused — this residue is already proven futile", got.Outcome)
	}
	if countClears(f.callsSnapshot()) != 0 {
		t.Errorf("a residue already proven futile must be refused BEFORE pressing C-u, "+
			"got %d clear keystrokes", countClears(f.callsSnapshot()))
	}
}

// The other half of #135's own safety analysis: a clear pass against an
// unrecorded composer that makes real progress and then genuinely stops
// (#87's stall shape) must be reported honestly — not as success, and not
// indistinguishable from "nothing happened" — the same discipline
// discardIncomplete already applies inside Discard itself, mirrored here
// because this path returns a DeliveryReceipt rather than an error.
func TestReplaceIfStrandedReportsAnUnrecordedComposerThatOnlyPartlyClears(t *testing.T) {
	f := twoSessions()
	lines := []string{"one", "two", "three", "four", "five", "six"}
	f.setMultilineComposer("%2", lines)
	f.setComposerFloor("%2", 3) // real progress (6 -> 3), then genuinely stops

	// Real clock: clearComposer's inter-press wait is a real timer regardless
	// of an injected clock — same reasoning as the Discard stall tests this
	// mirrors (TestDiscardStopsPressingOnceTheComposerStopsMoving).
	d := New("testbox", withExec(f.exec), withNonce(func() string { return testNonce }),
		withClock(time.Now))
	ref := fleet.SessionRef{Machine: "testbox", ID: "beta"}

	got, err := d.Send(context.Background(), testCaller, ref, "something new",
		driver.SendOptions{Submit: true, ReplaceIfStranded: true})
	if err != nil {
		t.Fatal(err)
	}
	if got.Outcome != fleet.OutcomeRefused {
		t.Fatalf("outcome = %s (%s), want refused — a partial clear must never be reported "+
			"as a successful delivery", got.Outcome, got.Reason)
	}
	if strings.Contains(got.Reason, "a human typed") {
		t.Errorf("reason = %q; this is a failed clear attempt this driver itself made, not "+
			"the genuinely-unknown-provenance answer", got.Reason)
	}
	if got := countClears(f.callsSnapshot()); got == 0 {
		t.Error("no clear keystroke was even attempted")
	}
}
