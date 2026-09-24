package compat

// Spec describes one check: what it asserts and which driver code depends on
// it. It is the single source for three things that must not drift apart —
// the "relied on by" suffix in every report detail, the additive reliedOn
// array in the report, and the catalogue table in docs/compat.md. Tests hold
// the three together.
type Spec struct {
	// ID is stable once shipped. Renaming or removing one is a schema bump.
	ID   string
	Gate Gate
	// Asserts says, in one sentence, what the check asserts about the
	// candidate.
	Asserts string
	// ReliedOn lists the driver code that depends on the behaviour, as
	// "<repo-relative file>#<top-level identifier>". A test resolves each
	// entry against the source, so a rename fails the build.
	ReliedOn []string
}

// catalogue is in report order. Add rows together with the check that
// evaluates them and the matching row in docs/compat.md.
var catalogue = []Spec{
	{
		ID:       "F-LIMIT",
		Gate:     GateWarn,
		Asserts:  "The candidate still contains the usage-limit notice wording the screen classifier recognises. Static text only: the screen cannot be produced on demand.",
		ReliedOn: []string{"internal/drivers/tmux/classify.go#usageLimit"},
	},
	{
		ID:       "F-APIERR",
		Gate:     GateWarn,
		Asserts:  "The candidate still contains the API-error wording the classifier reads to tell a failed turn from a finished one. Static text only: the screen cannot be produced on demand.",
		ReliedOn: []string{"internal/drivers/tmux/classify.go#lastTurnFailed"},
	},
	{
		ID:       "H-RC",
		Gate:     GateWarn,
		Asserts:  "The candidate still contains the four remote-control footer labels the control-channel reader maps. Static text only: a check never attaches a bridge.",
		ReliedOn: []string{"internal/drivers/tmux/controlchannel.go#controlStates"},
	},
	{
		ID:       "C1",
		Gate:     GateMust,
		Asserts:  "A working directory the driver seeds as trusted starts without the folder-trust dialog, so a session created there reaches its composer on its own.",
		ReliedOn: []string{"internal/trustseed/trustseed.go#Seeder"},
	},
	{
		ID:       "F-TRUST",
		Gate:     GateMust,
		Asserts:  "A directory outside the trust root shows the folder-trust dialog, which the driver classifies as such, reads as an unnumbered menu, and can find exactly one affirmative option in. Observed only: the dialog is never answered.",
		ReliedOn: []string{"internal/drivers/tmux/classify.go#classifyPromptKind", "internal/drivers/tmux/tmux.go#affirmativeOption"},
	},
	{
		ID:       "B5",
		Gate:     GateMust,
		Asserts:  "A session started in bypass-permissions mode reaches its composer with no acceptance screen in the way, given the user setting that suppresses it.",
		ReliedOn: []string{"internal/drivers/tmux/tmux.go#claudeCodeCommand"},
	},
	{
		ID:       "F-BYPASS",
		Gate:     GateWarn,
		Asserts:  "The bypass-acceptance screen, when it can be produced, is classified as such; otherwise the wording of its two options is still present in the candidate. Observed only: it is never answered.",
		ReliedOn: []string{"internal/drivers/tmux/tmux.go#acceptanceScreen"},
	},
	{
		ID:       "D1",
		Gate:     GateMust,
		Asserts:  "The runtime's per-process session record appears within fifteen seconds of launch carrying the fields this service reads, with the expected types and values.",
		ReliedOn: []string{"internal/drivers/tmux/terminalpath2_transcript.go#processSessionRecord"},
	},
	{
		ID:       "D3",
		Gate:     GateMust,
		Asserts:  "The record's process start time is UTC text that corroborates the running process, so the record can be trusted to belong to that process and not to a recycled pid.",
		ReliedOn: []string{"internal/drivers/tmux/terminalpath2_transcript.go#parseProcessSessionRecordStartTime"},
	},
	{
		ID:       "D4",
		Gate:     GateMust,
		Asserts:  "With remote control off, the record carries no bridge id and the screen shows no control-channel label. The negative half only: the positive half needs a bridge, which a check never creates.",
		ReliedOn: []string{"internal/drivers/tmux/controlchannel.go#controlChannelOf"},
	},
	{
		ID:       "F-COMPOSER",
		Gate:     GateMust,
		Asserts:  "The composer is the prompt glyph between two rules, an empty composer reads as empty even when a dim placeholder is painted in it, and a typed draft reads back exactly.",
		ReliedOn: []string{"internal/drivers/tmux/classify.go#composerText"},
	},
	{
		ID:       "F-MLDRAFT",
		Gate:     GateMust,
		Asserts:  "A multi-line draft pasted into the composer reads back as the text that was pasted.",
		ReliedOn: []string{"internal/drivers/tmux/composertext.go#composerMatchesText"},
	},
	{
		ID:       "F-PASTEMARK",
		Gate:     GateMust,
		Asserts:  "A long multi-line paste collapses to a [Pasted text #N +M lines] marker and one long line to a bare [Pasted text #N] marker, and both are counted the way delivery confirmation counts them.",
		ReliedOn: []string{"internal/drivers/tmux/tmux.go#markerCounts", "internal/drivers/tmux/tmux.go#composerHoldsCollapsedPaste"},
	},
	{
		ID:       "F-WRAP",
		Gate:     GateMust,
		Asserts:  "A draft longer than a row wraps onto rows of one width, and the wrapped rows read back as the text that was pasted.",
		ReliedOn: []string{"internal/drivers/tmux/composertext.go#composerRegion"},
	},
	{
		ID:       "G6",
		Gate:     GateMust,
		Asserts:  "The prompt-mode characters behave as the input guard assumes: a leading ! in an empty composer enters shell mode, while the same text after a space, and a slash after a space, stay plain prompt text.",
		ReliedOn: []string{"internal/drivers/tmux/inputguard.go#refuseAsRuntimeSyntax"},
	},
}

// Catalogue returns a copy of the catalogue, in report order.
func Catalogue() []Spec {
	out := make([]Spec, len(catalogue))
	for i, s := range catalogue {
		s.ReliedOn = append([]string(nil), s.ReliedOn...)
		out[i] = s
	}
	return out
}

// Lookup returns the catalogue entry for id.
func Lookup(id string) (Spec, bool) {
	for _, s := range catalogue {
		if s.ID == id {
			s.ReliedOn = append([]string(nil), s.ReliedOn...)
			return s, true
		}
	}
	return Spec{}, false
}
