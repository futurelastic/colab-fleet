package tmux

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"
)

// errBracketPasteUnavailable wraps every refusal pasteBracketed returns, so a
// caller (Send) can tell "this pane cannot take a safe bracketed delivery"
// apart from an ordinary multiplexer failure with errors.Is, the same shape
// this package's other sentinel errors already give their callers.
var errBracketPasteUnavailable = errors.New("tmux: bracketed paste is not available on this pane")

// Terminal path v2 — colab-fleet round-1 research answered here (see the
// scratch research this prototype was built against; every item below is
// keyed to that research's own D-numbering so a reviewer can trace fix to
// defect without re-deriving it):
//
//	D1  landed-check false negative on Claude Code's own word-wrap  -> confirmLandedV2 (this file)
//	D2  non-bracketed paste, CR-converted, swallows/splits          -> pasteBracketed (this file)
//	D3  long single-line pastes collapse/drop bytes                 -> pasteBracketed uses -p -r (no CR conversion, no chunk boundary this driver introduces)
//	D4  no per-session serialisation                                -> terminalpath2_lock.go
//	D5  stranded-record lifecycle gaps                               -> terminalpath2_stranded.go
//	D6  screen-based confirmation is the weak link                  -> terminalpath2_transcript.go
//	D7  routing (force the terminal path)                           -> ForceTerminalRoute (driver.SendOptions), inbox.go's inboxEligible
//
// This file holds the two D1/D2 fixes: bracketed, literal-newline delivery
// (replacing the CR-converting, non-bracketed paste-buffer call D2 names),
// and a composer-region-scoped, wrap-tolerant landed confirmation (replacing
// the raw-capture 24-byte-tail substring match D1 names).

// sanitizeForBracketedPaste strips bytes that could otherwise let the
// PAYLOAD end the bracket-paste sequence early — an embedded literal
// ESC[201~ (the bracketed-paste END marker) in a message this driver pastes
// would, if delivered unmodified, close the bracket ahead of schedule and
// leave the remainder of the message interpreted as ordinary (unbracketed)
// terminal input instead of pasted text. That is an injection: the payload
// would be deciding, mid-delivery, that the rest of itself is no longer a
// paste.
//
// ESC (0x1B) is dropped outright rather than merely refused-on-detection,
// because a caller has no legitimate reason to deliver a raw escape byte
// through this path — every documented reason to affect the runtime by key
// name goes through Keys (fleet.KeyName), never through Send's text.
//
// Every other C0 control byte (0x00-0x1F) is dropped too, except the two
// this driver's own delivery shape depends on: \n (the payload's own line
// breaks — the entire point of asking tmux for literal newlines with -r
// instead of the old CR-converting default) and \t (an ordinary character a
// human or an agent may legitimately type). \r is deliberately NOT kept: it
// is not content, and letting it through would reintroduce exactly the
// "did the multiplexer just turn a payload byte into an Enter-equivalent"
// ambiguity this whole change exists to remove — see -r's own semantics
// (tmux(1): "-r causes tmux to send \r rather than \n to the pane"), which
// is why this driver asks for -r AND never emits a \r of its own into the
// buffer it hands tmux.
//
// DEL (0x7F) is dropped for the same reason as the other C0 controls: it is
// a control code by convention even though it sits outside the 0x00-0x1F
// block.
func sanitizeForBracketedPaste(text string) string {
	if !strings.ContainsAny(text, "\x00\x01\x02\x03\x04\x05\x06\x07\x08\x0b\x0c\x0d\x0e\x0f\x10\x11\x12\x13\x14\x15\x16\x17\x18\x19\x1a\x1b\x1c\x1d\x1e\x1f\x7f") {
		return text
	}
	var b strings.Builder
	b.Grow(len(text))
	for _, r := range text {
		switch {
		case r == '\n' || r == '\t':
			b.WriteRune(r)
		case r == 0x7f:
			continue
		case r < 0x20:
			continue
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// bracketPasteFlag asks tmux whether the target pane's current occupant has
// asked the terminal for bracketed-paste mode (`#{bracket_paste_flag}`,
// tmux(1)) — the flag every terminal application that wants literal pastes
// distinguished from typed input sets, and the flag D2's non-bracketed
// `paste-buffer` call ignored entirely.
//
// ok is false only when the query itself could not be answered (the
// multiplexer call failed) — never a stand-in for "flag is 0". A caller must
// tell those apart: an unanswerable query is "cannot confirm safety",
// which fails closed the same as an answered "0" does (see pasteBracketed),
// but for a different, worth-counting reason.
func (d *Driver) bracketPasteFlag(ctx context.Context, paneID string) (enabled, ok bool) {
	out, err := d.run(ctx, d.bin, "display-message", "-p", "-t", paneID, "#{bracket_paste_flag}")
	if err != nil {
		return false, false
	}
	return strings.TrimSpace(string(out)) == "1", true
}

// pasteBracketed is terminal-path-v2's replacement for the plain
// `paste-buffer -d` call D2 names: bracketed (`-p`) so the occupant receives
// the payload as one paste rather than as a stream of key-equivalent bytes,
// and literal-newline (`-r`) so an embedded "\n" in a multi-line message
// arrives as "\n", not as the CR D2 measured being read as Enter mid-payload
// (9/9 labelled sends swallowing their own first line; 27/27 short
// multi-line payloads splitting/early-submitting).
//
// # Why this refuses rather than falling back to the old paste on flag=0
//
// The task that asked for this file was explicit: falling back silently
// reintroduces D2 exactly on the pane where this driver could not first
// confirm the runtime asked for bracketed mode — the one case the fallback
// would be exercised on is the one case nobody could have tested it against,
// because #{bracket_paste_flag} said no. Round-1's own measurement is that
// every live Claude Code pane sampled reported bracket_paste_flag=1, so this
// refusal is expected to fire on live traffic approximately never; it exists
// for the pane that is not yet running Claude Code, or is running something
// else in the same slot (a shell before the agent starts, e.g.) — a case
// where guessing which delivery shape is safe is exactly the guess this
// driver's own §2.4/§5.6 discipline (fail closed, never emulate) refuses
// elsewhere.
//
// The alternative considered and rejected: keep the old call as a fallback
// gated on flag=0. Rejected because nothing in this codebase, before this
// change, measured what flag=0 actually looks like on a real Claude Code
// pane (round-1 measured only flag=1), so a fallback branch would be
// deploying UNMEASURED behaviour disguised as graceful degradation — the
// same shape of mistake §5.6's "degrade, never emulate" already names.
func (d *Driver) pasteBracketed(ctx context.Context, paneID, text string) error {
	enabled, ok := d.bracketPasteFlag(ctx, paneID)
	if !ok {
		d.counters.incr(counterBracketPasteFlagUnknown)
		return fmt.Errorf("%w: could not confirm this pane's occupant has bracketed paste mode "+
			"enabled (the #{bracket_paste_flag} query itself failed) — refusing to guess between "+
			"a bracketed delivery and the older CR-converting one; see pasteBracketed's own doc "+
			"comment for why there is no fallback", errBracketPasteUnavailable)
	}
	if !enabled {
		d.counters.incr(counterBracketPasteFlagOff)
		return fmt.Errorf("%w: this pane's occupant has not asked the terminal for bracketed "+
			"paste mode (#{bracket_paste_flag} reads 0) — delivering literal newlines un-bracketed "+
			"would be read as Enter keystrokes mid-message (colab-fleet round-1 D2); refusing "+
			"rather than falling back to the CR-converting paste this change replaces", errBracketPasteUnavailable)
	}

	sanitized := sanitizeForBracketedPaste(text)

	// load-buffer reads from stdin when given "-"; this driver writes to a
	// temp file instead so the payload never traverses argv (unchanged
	// rationale from the original Send: §5.3, "a shared namespace is a
	// shared namespace").
	f, err := os.CreateTemp("", "fleet-send-*")
	if err != nil {
		return fmt.Errorf("send: staging payload: %w", err)
	}
	defer os.Remove(f.Name())
	if _, err := f.WriteString(sanitized); err != nil {
		f.Close()
		return fmt.Errorf("send: staging payload: %w", err)
	}
	f.Close()

	bufName := "fleet-" + d.nonce()
	args := []string{
		"load-buffer", "-b", bufName, f.Name(), ";",
		// -p: bracket the payload so the occupant's own bracketed-paste
		// handling receives it as one paste. -r: send "\n" as "\n" rather
		// than translating it to "\r" (tmux's default) — the literal-newline
		// half of this change; see this function's own doc comment.
		"paste-buffer", "-p", "-r", "-b", bufName, "-t", paneID, "-d",
	}
	if _, err := d.run(ctx, d.bin, args...); err != nil {
		return fmt.Errorf("send: delivering: %w", err)
	}
	return nil
}

// composerRegionMatch is D1's fix: it compares the composer's own STRUCTURAL
// region (composerText, which already walks composerSpan's fence-to-fence
// scan rather than treating the pane as an undifferentiated blob of text)
// against the sent text, instead of the old confirmLanded's 24-byte tail
// substring taken from a raw, unstructured capture-pane -J read.
//
// # Why this closes D1 specifically
//
// The old needle straddled Claude Code's OWN word-wrap: the TUI wraps its
// composer at a fixed content width, breaking at a space (dropping it) and
// indenting the continuation by two more spaces — a wrap the TUI performs
// itself, by printing real additional rows, not a wrap tmux's terminal
// emulation performs on one logical row. `-J` rejoins rows THE TERMINAL
// soft-wrapped; it has no way to know the TUI printed two rows that are
// logically one, so a needle landing across that boundary can never match,
// deterministically (round-1: 27/27 predicted cases reproduced).
//
// composerText already solves the identical problem for a DIFFERENT
// question ("is there unsent input", the §2.4 busy-composer check) by
// walking the composer's rows structurally and joining continuation rows
// with a single space of its own choosing — a join that has nothing to do
// with where the TUI happened to wrap. Reusing it here means the comparison
// never sees the TUI's own wrap points at all; both sides
// (composerRegionMatch's pending text and its own needle) go through
// normalizeForMatch, which strips ALL whitespace, so composerText's joining
// space and the TUI's dropped wrap-space both disappear identically instead
// of needing to be told apart.
//
// # Equality vs. suffix
//
// Ordinary case: the composer holds exactly the sent text (pre-paste-empty
// is already a gate the caller enforces before this is ever called) ->
// require exact equality of the normalised forms.
//
// Scrolled case: Claude Code's own composer view can scroll internally once
// a single very tall paste exceeds the rows the TUI allocates it (this is
// the TUI's OWN inner scroll, independent of and in addition to tmux's `-S`
// history-margin scrolling that composerClipped already handles) — leaving
// only the TAIL of the pasted text visible even though the fence is fully
// in view and composerText reports composerFound. In that shape the visible
// region is, by construction, a SUFFIX of what was sent and nothing else;
// requiring it to be a non-trivial suffix (composerSuffixMinBytes) keeps a
// short, coincidental tail match (a trailing "." or a repeated common word)
// from passing.
const composerSuffixMinBytes = 24

// allowSuffix controls whether composerRegionMatch's own scrolled-composer
// suffix rule (this constant's own doc comment) may fire at all.
//
// # Review fix: the suffix rule is a resume-path hazard, not a fresh-send one
//
// A FRESH send only ever calls this with text this driver JUST pasted a
// moment ago — the suffix rule's own scenario (Claude Code's inner composer
// scroll on a single tall paste) is real there and stays enabled.
//
// A RESUME (Send's ResumeIfStranded branch, tmux.go) is a different
// question: "does the composer, RIGHT NOW, hold a complete, attributable
// copy of a delivery this driver stranded EARLIER". A tail-only match there
// proves nothing of the kind — a person's own, unrelated message can
// coincidentally end in the same words as this driver's stranded text (a
// short common closing phrase, punctuation, ...), and unlike the fresh-send
// case there is no freshly-observed paste to anchor "this must be a scroll of
// what I just delivered" against. The review's own reproduction: composer
// holds the tail 60 of a person's unrelated 136-character message, matches
// the suffix rule, and Send/resume presses Enter on it. Resume call sites
// pass allowSuffix=false for exactly this reason — see confirmLandedV2's own
// strict parameter, which threads this through from Send.
func composerRegionMatch(sc screen, text string, allowSuffix bool) (matched bool, evidence string) {
	pending, scan := composerText(sc)
	if scan != composerFound || pending == "" {
		return false, ""
	}
	normPending := normalizeForMatch(pending)
	if normPending == "" {
		return false, ""
	}
	normText := normalizeForMatch(text)
	if normPending == normText {
		return true, "composer region matches the sent text exactly (whitespace- and composing-form-insensitive)"
	}
	if allowSuffix && len(normPending) >= composerSuffixMinBytes && strings.HasSuffix(normText, normPending) {
		return true, "composer region is a substantial suffix of the sent text — read as the TUI's own composer having scrolled to show only the tail of a tall paste"
	}
	return false, ""
}

// confirmLandedV2 replaces confirmLanded's needle-substring check with
// composerRegionMatch AS THE PRIMARY SIGNAL, and keeps the OLD needle
// substring (confirmLanded's own tail-24-byte match against a raw,
// unstructured `-J` capture) as a SECONDARY, fallback signal rather than
// deleting it — a deliberate choice, not an oversight:
//
//   - composerRegionMatch is the fix for D1 proper: it is scoped to the
//     composer's own fenced region, so it cannot be fooled by transcript
//     text above the composer the way the raw substring can (round-1's own
//     background: "capture -S -6 actually reads the whole visible pane...
//     so the needle can falsely match transcript text above the composer"),
//     and it survives Claude Code's own word-wrap by construction rather
//     than by needle-length tuning.
//   - the OLD needle is kept as a fallback for a substrate that echoes a
//     paste WITHOUT re-drawing it inside a structurally fenced composer at
//     all — this package's own test harness (fakeMux) is exactly that
//     substrate for its DEFAULT paste model (it appends delivered bytes
//     after whatever screen a test pre-armed, rather than re-painting a
//     fenced composer around them), and a REAL runtime that behaved the
//     same way would have nothing for composerRegionMatch to find either.
//     Trying composerRegionMatch FIRST means a real Claude Code pane's own
//     wrap defect is fixed without this fallback ever firing for it —
//     counterLandConfirmByLegacyNeedle staying near zero on live traffic is
//     exactly the confirmation of that, the same "count it to find out if
//     it's dead" idiom counterSubmitConfirmedByMarkerCleared already uses.
//
// The collapsed-multi-line-paste marker path (gained/markerCounts,
// colab-fleet#37/#143/#145's own fix for F49) is unchanged, and reads from
// the SAME raw `-J` capture the legacy needle check now shares — one
// subprocess call serving both, not two.
//
// # strict — the review fix that scopes the two fallbacks by call site
//
// strict=false (every FRESH delivery — Send's own first-attempt paste) keeps
// this function's original two fallbacks available exactly as before: the
// legacy needle (gated, new in this review pass, to only fire on a capture
// that actually classifies as composerFound — see below) and
// composerRegionMatch's suffix rule.
//
// strict=true (Send's ResumeIfStranded branch ONLY) disables both. A resume
// exists to answer "is OUR EARLIER delivery, unconfirmed then, actually
// sitting there now" — and both fallbacks are evidence about something else.
// The legacy needle scans the WHOLE visible pane, so on a resume it is
// overwhelmingly likely to match the ALREADY-STRANDED text sitting in the
// transcript above an otherwise-unrelated composer (the review's own
// reproduction: a person's unrelated draft in the composer, this driver's
// earlier, already-submitted text still visible above it, needle matches,
// Enter gets pressed on the person's draft). The suffix rule has the
// matching resume-specific hazard — see composerRegionMatch's own
// allowSuffix doc comment. Neither hazard exists on a fresh send, where the
// text being matched was never on screen a moment ago.
//
// # The legacy needle is now gated on composerFound, not unconditional
//
// A capture that composerText classifies as composerAbsent (no fenced
// composer at all — the shape a full-screen dialog or selection menu paints,
// per composerSpan's own doc comment) or composerClipped (this driver's own
// capture window ended before it could tell) is never evidence that
// something landed IN A COMPOSER: there may be no composer to have landed
// in, or this driver cannot see the one there might be. The review's own
// reproduction had a permission dialog appear while the sent text was
// separately visible in the transcript; the old, unconditional needle
// matched that transcript text and Enter approved "1. Yes". Restricting the
// needle to captures that DO classify as composerFound closes that
// specifically, while leaving it exactly as available as before for the
// shape it exists for: a composer is present and structurally intact, its
// own echo just does not visibly match (this package's own test harness,
// whose default paste model appends past the composer's closing fence rather
// than re-rendering inside it, is exactly this shape — composerFound stays
// true throughout, only the text comparison fails).
//
// # The dialog race — refusing a match made alongside a visible selection menu
//
// Both the composer-match and the legacy-needle/marker successes below now
// additionally require that the SAME capture which produced the match does
// not also show a selection menu (awaitingSelection) — the review's fix for
// "pressing Enter after the landed check can approve a dialog": a modal
// appearing in between this driver's last look and the submit keystroke a
// few lines later, in the caller, must not have that keystroke read as
// approving whatever the dialog is showing instead of submitting the
// message. This does not eliminate the gap entirely (a dialog could still
// appear in the handful of instructions between this function returning and
// the caller's own send-keys call), but it closes the much larger window
// this function's own multi-second poll loop was open for.
//
// digestVerified, threaded from the resume call site ONLY: whether the
// caller already corroborated, via strandedRecord.ComposerDigest against the
// composer's CURRENT digest, that this composer holds exactly the same
// content it held the moment this driver stranded its own delivery. When
// true, composerRegionMatch's suffix rule (the scrolled-tall-composer shape,
// composerRegionMatch's own doc comment) is allowed even under strict=true —
// see this function's own doc comment section on why the suffix rule is
// otherwise a resume-path hazard, and why digest verification removes it.
// digestVerified is never true for a fresh send (strict=false already allows
// the suffix rule there, for a different reason — the delivery is one this
// driver JUST pasted, not one it is trying to corroborate against a stranded
// record) and never assumed true for a resume with an EMPTY recorded digest
// (strandedRecord.ComposerDigest's own doc comment: absent evidence is not
// the same as corroborating evidence).
//
// # Review fix (review-regression): a strict resume could not finish a real,
// tall composer's own tail-only render
//
// Real Claude Code caps its composer at a fixed row count and scrolls
// INSIDE it (colab-fleet's own #143/M2 fixtures), so a resume's own re-read
// of a composer holding a message in roughly the 450-800B / multi-line range
// can see only the TAIL of what this driver sent — composerRegionMatch's
// suffix rule is exactly the evidence for that shape, and strict=true (every
// resume, unconditionally, before this fix) disabled it along with the
// legacy needle and the marker path, for a hazard (a person's unrelated
// draft coincidentally ending in the same words) that digest verification
// already rules out structurally: the composer is PROVEN byte-identical
// (modulo the resize-tolerant normalisation composerTextDigest already
// applies) to what this driver itself saw land there. A resume that can
// prove that has no need of the blanket refusal, and reintroducing the
// suffix rule for it alone — never the legacy needle, never the marker path,
// both of which read the WHOLE pane rather than just the composer's own
// region and stay exactly as hazardous as before — closes the regression
// without reopening the finding it was closed for.
func (d *Driver) confirmLandedV2(ctx context.Context, paneID, text string, before map[pasteKey]int, strict bool, digestVerified bool) (pasteKey, int, bool) {
	needle := strings.TrimSpace(text)
	if needle == "" {
		return pasteKey{}, 0, true
	}
	// The legacy needle: last line, last 24 bytes — confirmLanded's own
	// shape, unchanged, kept only as the fallback described above.
	if idx := strings.LastIndexByte(needle, '\n'); idx >= 0 {
		needle = needle[idx+1:]
	}
	if len(needle) > 24 {
		needle = needle[len(needle)-24:]
	}
	allowSuffix := !strict || digestVerified

	deadline := d.now().Add(submitConfirmWindow)
	for {
		sc, capOK := d.captureForClassify(ctx, paneID)
		dialogShowing := capOK && awaitingSelection(sc)
		if capOK && !dialogShowing {
			if matched, _ := composerRegionMatch(sc, text, allowSuffix); matched {
				d.counters.incr(counterLandConfirmByComposerMatch)
				return pasteKey{}, 0, true
			}
		}
		if dialogShowing {
			d.counters.incr(counterSendRefusedDialogRace)
		}
		if capOK && !dialogShowing {
			// Review fix (review-safety): this used to be a SECOND,
			// independent `capture-pane -J` subprocess call — a separate
			// snapshot of the pane taken a moment after the one
			// captureForClassify (above) already read, wide open to a
			// modal appearing in between the two: the review's own
			// reproduction had captureForClassify see a composer while
			// this second call saw a permission dialog with the sent text
			// still visible in the transcript above it, and the legacy
			// needle matched THAT. Deriving the joined text from sc.lines
			// instead — already fetched, already the capture dialogShowing
			// was decided from — means the needle and marker checks below
			// can never disagree with the dialog gate about what the pane
			// looked like: there is only one observation per iteration, not
			// two. `-J`'s own rejoining of a TUI-soft-wrapped row is not
			// missed here: composerRegionMatch (the PRIMARY signal, tried
			// first) already handles Claude Code's real word-wrap
			// structurally, and the fallbacks below exist for exactly the
			// substrate (this package's own fakeMux) that does not wrap at
			// all — see this function's own top doc comment.
			_, scan := composerText(sc)
			strippedPainted := strings.Join(sc.lines, "\n")
			if !strict && scan == composerFound && needle != "" && strings.Contains(strippedPainted, needle) {
				d.counters.incr(counterLandConfirmByLegacyNeedle)
				return pasteKey{}, 0, true
			}
			// Review fix (review-safety/review-confirmation/review-regression,
			// one finding from three directions): the marker/gained path
			// stays disabled on every STRICT call (every resume), full stop
			// — never re-enabled by digestVerified, unlike the suffix rule
			// above. A resume's own before-map is always empty
			// (map[pasteKey]int{} — there is no "fresh paste" on a resume to
			// diff against), so ANY single marker present on the pane
			// "gains" from zero — including a person's own, unrelated
			// collapsed paste that happens to be sitting in the composer.
			// digestVerified does not rescue this the way it rescues the
			// suffix rule: the digest check corroborates the COMPOSER's
			// content, and markerCounts/gained here read the WHOLE painted
			// pane, not the composer's own region — a verified composer
			// digest says nothing about whether some OTHER marker elsewhere
			// on the same pane is this delivery's own. A fresh send
			// (strict=false) is unaffected: P1's own fix (a bare
			// count-less "[Pasted text #N]" for a collapsed single-line
			// paste) keeps working there.
			if !strict {
				after := markerCounts(strippedPainted)
				if key, ok := gained(before, after); ok {
					d.counters.incr(counterLandConfirmByMarker)
					return key, after[key], true
				}
			}
		}
		if d.now().After(deadline) || ctx.Err() != nil {
			d.counters.incr(counterLandConfirmTimeout)
			return pasteKey{}, 0, false
		}
		select {
		case <-ctx.Done():
			d.counters.incr(counterLandConfirmTimeout)
			return pasteKey{}, 0, false
		case <-time.After(submitConfirmInterval):
		}
	}
}
