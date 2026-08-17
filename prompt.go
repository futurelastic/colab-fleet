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
	// PromptSettingsTrust: an ADMINISTRATOR's managed-policy payload asking to
	// be approved. Not a working directory's own settings at all — read out of
	// the runtime binary, its neighbours are `policySettings`,
	// `managed-settings.json`, `managed-settings.d`, `/etc/claude-code` and the
	// per-user/device Managed Preferences directory, which are sources an
	// administrator controls, not sources a working directory carries.
	// (`projectSettings` is a different source in that same enumeration, and
	// raises no dialog at all.) The screen re-arms whenever that payload
	// changes, which is why it is its own kind rather than folded into
	// PromptFolderTrust.
	//
	// It is deliberately NOT consentable, and for a different reason than
	// PromptFolderTrust: a create-time consent here would let a
	// session-creating caller accept an ADMINISTRATOR's policy change on
	// behalf of an operator who never saw it — categorically outside what a
	// caller of this layer can speak for. Folder trust is not that: the caller
	// genuinely does own the directory it just named in its own request.
	//
	// (Read from one installed build; this repository's README pins the span
	// it is tested against, and that build sits outside it — a fact read from
	// one build is evidence about that build, not a guarantee across the span.)
	PromptSettingsTrust PromptKind = "settings-trust"
	// PromptToolPermission: a tool asking to run something.
	PromptToolPermission PromptKind = "tool-permission"
	// PromptBypassAcceptance: the permission-mode acceptance screen.
	//
	// # This kind is never produced by option matching, and that is deliberate
	//
	// It was, in intent, for a long time — by a rule requiring "bypass" and
	// "permissions" in one option. That rule could not fire. Read out of the
	// runtime's own binary rather than sampled from a screen capture, the
	// options it ships for that screen are "Yes, I accept" and "No, exit".
	// The identifying words live in the question, and the question is the one
	// thing option matching must not read (see the doc above: a ship decision
	// was once labelled with THIS kind because an agent typed "No auth bypass"
	// into its own prompt).
	//
	// So a driver may emit this kind only when it has evidence the screen text
	// cannot give it — for example, having started the session in that mode
	// itself. A caller must therefore treat its ABSENCE as uninformative here
	// in a way that does not apply to the other kinds: an unclassified boot
	// screen may well be this one.
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
