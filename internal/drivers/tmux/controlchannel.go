package tmux

import (
	"strings"

	fleet "github.com/godx-jp/colab-fleet"
)

// Reading the runtime's own remote-control status label off the pane.
//
// # What is being read, and why it can be a closed set
//
// The runtime renders one status label for its control channel, computed by a
// single function with four outcomes. Read out of the runtime binary rather
// than sampled from a screen, that function is:
//
//	error        -> "/rc failed"        (error)
//	reconnecting -> "/rc reconnecting"  (warning)
//	connected    -> "/rc active"        (success)
//	otherwise    -> "/rc connecting…"   (warning)
//
// Four labels, no others, and the vocabulary is fixed at the source rather
// than inferred from what a capture happened to contain. That is the standard
// this package already holds itself to for prompt kinds — read the runtime, do
// not guess from a capture — and it is the difference between a closed set and
// a loose prose match somebody has to keep widening.
//
// # The connected row above is the binary's function; it is not what renders (#75)
//
// A real `active` capture (colab-fleet #70, #65) showed the connected state
// rendering as a bare `/rc` hyperlink with NOTHING after it — no space, no
// word, nothing an anchor requiring `"/rc "` could ever match. Two
// independent live captures agree; controlStateIn now treats the anchor with
// no word following it as `active` on that evidence.
//
// This is confirmed for `active` alone. Whether `failed`, `reconnecting` or
// `connecting` ever render bare too — or always carry their word the way the
// table above assumes — has NOT been measured against a real capture; only
// `active` has been captured live so far. Do not read the table above as
// confirmed rendering for the other three; it is the binary's function
// signature, not a screen anyone has seen. Confirm before relying on it
// further (tracked on #75).
//
// # Why only the footer, and why that is the entire safety property
//
// The incident that motivated this had a supervisor grepping panes for the
// disconnection notice, because nothing in the API carried it. That grep
// self-contaminated: the session running it had printed the very strings it
// was searching for into its own transcript — its tool output contained
// `/rc failed` verbatim — so it classified ITSELF as broken.
//
// That is not a quirk of one grep. This file's package comment already states
// the general rule: scrollback above the composer is transcript, and transcript
// is whatever the agent chose to print, "including, potentially, text designed
// to look like the TUI's own chrome." A matcher that scanned the whole capture
// would be readable, and forgeable, by every agent this service watches — and
// the ones most likely to print these strings are exactly the ones doing
// recovery work, which is when the answer matters most.
//
// So detection is confined to the FOOTER: the lines below the composer's
// closing fence, which is chrome the runtime redraws and the agent cannot write
// into. This is the same move `usageLimit` makes for the same reason, pointed
// the other way — it reads the live region ABOVE the fence, this reads the one
// below it.
//
// # What is deliberately NOT read here
//
// The disconnection MESSAGE. The runtime prints one into the transcript as an
// informational system message, and it carries the one thing this field cannot:
// whether a retry can help. Its close codes separate a transient failure
// ("Couldn't reconnect … Retry, or start a fresh session without --resume")
// from a terminal one ("this session was ended or archived from another device
// or app (code 4090)"), and those need opposite responses.
//
// It is not read because of where it lives. The transcript is the forgeable
// region — the exact region the self-contamination came from — so promoting
// prose out of it into a structured field would rebuild the hazard this file
// exists to avoid, one layer higher and harder to see. The distinction is worth
// having and is recorded as its own issue (colab-fleet #65) rather than taken
// on unsound evidence.
//
// The close codes underneath, read out of the runtime binary rather than
// guessed from a capture, recorded here so the next attempt does not have to
// re-derive them. Which ones are transient versus terminal is NOT recorded
// below because it was not established by that reading — the binary
// distinguishes these codes; it is not confirmed here which ones it treats
// as retryable. Do not infer that mapping from the code numbers or from
// which two happened to come with example message text; measure it before
// building anything that branches on it.
//
//	4090  session ended or archived from another device or app, with four
//	      sub-reasons the binary distinguishes but the message text above
//	      does not surface separately
//	4091  transport init failure
//	4092  no close reason given
//	4093  heartbeat failure
//	4094  worker credential rejected
//
// Whoever solves #65 soundly needs a way to read this that is not "trust the
// transcript" — the runtime would need to put it somewhere an agent cannot
// write, the way the footer label already is, or this field would need to
// come from the runtime's own durable record the way QuotaBlock.since and
// TurnEnd do (colab-fleet #56) rather than from a screen at all.
//
// # A label that is absent is absent, never "connected"
//
// A session started without remote control renders no label at all. Returning
// "active" for that would invent a channel out of a configuration, and
// returning "failed" would invent a fault. Nil, and
// DriverCapabilities.ObservesControlChannel is what tells a caller that this
// driver does look.

// controlLabel is the token the runtime's status label always begins with. It
// is the slash-command name, which is what makes it a stable anchor: the label
// is telling a human what to type, so it cannot drift without the command
// drifting too.
//
// No trailing space (#75): an earlier version of this anchor required one,
// on the assumption every state renders a word after the command name. A
// real `active` capture showed that wrong — it renders bare, nothing after
// the anchor at all — so requiring a space made the anchor unable to match
// the very state it is most often looking for. Whatever follows the bare
// anchor, word or nothing, is controlStateIn's job to interpret.
const controlLabel = "/rc"

// controlStates maps the runtime's four labels onto the model's closed set.
//
// Keyed on the word after the anchor rather than on the whole rendered string,
// because the connecting label carries a trailing ellipsis the runtime is free
// to render as one rune or three periods, and neither spelling changes what the
// label means. Matching the WORD is F37's rule again: match what the line is
// for, not how it is decorated.
var controlStates = map[string]fleet.ControlChannelState{
	"failed":       fleet.ControlChannelFailed,
	"reconnecting": fleet.ControlChannelReconnecting,
	"active":       fleet.ControlChannelActive,
	"connecting":   fleet.ControlChannelConnecting,
}

// controlChannelOf reports what the runtime's own status label says about this
// session's control channel, or nil when it says nothing.
func controlChannelOf(s screen) *fleet.ControlChannel {
	for _, line := range footerLines(s) {
		state, ok := controlStateIn(line)
		if !ok {
			continue
		}
		return &fleet.ControlChannel{State: state}
	}
	return nil
}

// controlStateIn finds the label anywhere on one footer line.
//
// Anywhere, not at a fixed column: the label is right-aligned and shares its
// row with whatever else the footer is showing, and a matcher pinned to a
// position would silently stop firing the first time the runtime rearranged the
// row. A detector that silently stops firing reports every session as
// connected, which is the failure this whole field exists to end — so the
// tolerant match is the safe one here, and the region check above is what keeps
// tolerance from becoming credulity.
//
// The tolerance is deliberate, and stays exactly as it is (#75): the label's
// position moved between every real capture taken so far — the model/plan
// row once, an unrelated runtime-hint row another time — which is live
// confirmation of the reason this was never pinned to a column in the first
// place. Tightening it now would be trading a real safety property for a
// guess dressed as precision, real captures or not.
//
// What DID need fixing, once real captures existed to check it against
// (colab-fleet #70, #65), was the anchor match itself — see controlLabel's
// own comment (#75) for the bare-`active` shape this line now handles.
func controlStateIn(line string) (fleet.ControlChannelState, bool) {
	idx := strings.Index(line, controlLabel)
	if idx < 0 {
		return "", false
	}
	rest := strings.TrimLeft(line[idx+len(controlLabel):], " ")
	// The word runs to the first separator. The label's own suffixes — an
	// ellipsis on `connecting…`, or a `·` joining it to the next footer item —
	// are decoration, not part of the state's name.
	word := rest
	if cut := strings.IndexAny(word, " ·…."); cut >= 0 {
		word = word[:cut]
	}
	if word == "" {
		// #75: the only state measured rendering with nothing after the
		// anchor at all is `active` (two independent live captures, #70 and
		// #65) — no ellipsis, no word, not even a trailing space. This is
		// NOT confirmed for the other three; it is entirely possible a real
		// `failed`/`reconnecting`/`connecting` capture always carries its
		// word and this branch never fires for them. Treat that as open,
		// not assumed — see #75's own note asking for it to be checked
		// against a real capture before anyone relies on it either way.
		return fleet.ControlChannelActive, true
	}
	state, ok := controlStates[strings.ToLower(word)]
	if !ok {
		// A fifth label would land here. Unrecognised is reported as nothing
		// rather than as the nearest neighbour: this package's whole discipline
		// is that a screen it cannot read produces no claim, and guessing which
		// of four a new word resembles is how a wrong claim gets made
		// confidently.
		return "", false
	}
	return state, true
}

// footerLines returns the chrome BELOW the composer's closing fence.
//
// The composer is fenced by a rule above and below it. Everything after that
// closing rule is footer — the runtime's own status row — and everything before
// the opening one is transcript. Finding the fence by walking up from the bottom
// to the last composer marker, then down past its rule, keeps this independent
// of how many footer rows the runtime happens to render.
//
// A screen with no composer yields NOTHING, deliberately. That is not a
// degenerate case: it is what a full-screen dialog looks like, and it is the
// state a session lands in when the runtime takes the screen over. Reporting a
// channel state from a screen whose structure this driver cannot locate would
// be reading a label out of a layout it does not recognise.
func footerLines(s screen) []string {
	marker := -1
	for i := len(s.lines) - 1; i >= 0; i-- {
		if strings.Contains(s.lines[i], composerRuneMarker) {
			marker = i
			break
		}
	}
	if marker < 0 {
		return nil
	}
	start := marker + 1
	for start < len(s.lines) && isRule(s.lines[start]) {
		start++
	}
	if start >= len(s.lines) {
		return nil
	}
	return s.lines[start:]
}
