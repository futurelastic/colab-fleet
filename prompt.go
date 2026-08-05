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

// PromptKind names what a prompt is ASKING, when the driver can recognise it.
//
// # Why this exists, and why it is advisory
//
// A caller that wants to answer prompts safely needs to distinguish "the
// question I know how to answer" from "a question I have never seen". Without
// a kind, every caller does that by matching the option text itself — and that
// is the fragility this project has paid for twice already, once on a footer
// and once on an animated glyph. Duplicated into N clients it is N places to
// be wrong.
//
// So the matching is done ONCE, here, and it is deliberately quarantined:
//
//   - **It is advisory.** Nothing in the service behaves differently because
//     of it. Options and Selected remain the answer; Kind only says what the
//     driver thinks is being asked.
//   - **It fails to empty, never to a guess.** An unrecognised prompt has no
//     kind, which is §5.7 again: not "no kind", but "not classified".
//   - **Empty is not permission.** A caller may auto-answer a kind it knows.
//     It must NEVER treat an unclassified prompt as safe — the default option
//     on a real prompt in the wild is "No, exit", and a client that answers
//     what it cannot read will eventually kill the session it meant to rescue.
//
// That last rule is the whole reason policy stays with the caller. This type
// says what is being asked; deciding what to answer is a supervisor's job, and
// a session service that made that decision would have become one (§1).
type PromptKind string

const (
	// PromptResumeChooser: the runtime asking how to resume a prior session.
	// Every session restored after a crash meets this one, and until it is
	// answered the agent does nothing at all.
	PromptResumeChooser PromptKind = "resume-chooser"
	// PromptFolderTrust: "do you trust the files in this folder".
	PromptFolderTrust PromptKind = "folder-trust"
	// PromptToolPermission: a tool asking to run something.
	PromptToolPermission PromptKind = "tool-permission"
	// PromptBypassAcceptance: the bypass-permissions acceptance screen.
	PromptBypassAcceptance PromptKind = "bypass-permissions"
)

type SessionPrompt struct {
	// Question is the text above the options, best effort. It may be empty
	// when the prompt is terse; the options are the load-bearing part.
	Question string `json:"question,omitempty"`

	// Options in the order shown, 1-based when referenced by Response.Choice.
	Options []string `json:"options"`

	// Selected is the 1-based index of the highlighted option — what a bare
	// confirmation would accept. Never assume it is the safe one.
	Selected int `json:"selected,omitempty"`

	// Kind is what the driver thinks is being asked, or empty when it does
	// not recognise the question. Advisory — see PromptKind. Empty must never
	// be read as "safe to answer".
	Kind PromptKind `json:"kind,omitempty"`

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
