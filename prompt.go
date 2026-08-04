package fleet

// SessionPrompt is a question a session is blocked on, exposed so a caller can
// present the choices and answer one (§2.3, §3's respond).
//
// # Why the options are enumerated and not just described
//
// A supervisor's job here is to put the question in front of a human — on a
// phone, in a console, wherever — and submit what they pick. Evidence prose
// naming only the highlighted option is enough to explain a state and not
// enough to act on it.
//
// It matters more than it sounds. Two boot prompts observed on the same fleet:
//
//	❯ 1. Yes, I trust this folder        ❯ 1. No, exit
//	  2. No, continue without these        2. Yes, I accept
//
// Same shape, same footer, and the safe answer is at a different index. A
// caller that accepted the highlighted default would proceed in one case and
// kill the session in the other. Enumerating is what makes an answer a choice
// rather than a guess.
type SessionPrompt struct {
	// Question is the text above the options, best effort. It may be empty
	// when the prompt is terse; the options are the load-bearing part.
	Question string `json:"question,omitempty"`

	// Options in the order shown, 1-based when referenced by Response.Choice.
	Options []string `json:"options"`

	// Selected is the 1-based index of the highlighted option — what a bare
	// confirmation would accept. Never assume it is the safe one.
	Selected int `json:"selected,omitempty"`

	// Nonce changes whenever the prompt changes.
	//
	// A caller reads a prompt, shows it to a human, and answers some seconds
	// or minutes later. In between, the session may have moved on and be
	// showing a different question at the same place on screen — and an
	// answer meant for the old one would be applied to the new one, silently
	// and by index. The nonce turns that into a refusal.
	//
	// This is §5.4's "a proxy for identity is not identity" arriving in a
	// third place: an option index is not an option.
	Nonce string `json:"nonce"`
}
