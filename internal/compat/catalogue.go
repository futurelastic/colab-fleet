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
