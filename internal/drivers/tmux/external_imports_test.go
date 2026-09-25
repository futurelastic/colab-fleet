package tmux

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	fleet "github.com/godx-jp/colab-fleet"
)

// The runtime's second boot question about a directory (colab-fleet #211): the
// instruction files its working directory loads import a file from outside it.

// fixtureExternalImportsMenu is that question as captured from the installed
// runtime on a session started in a directory it had never asked about, whose
// instruction file imports one file from elsewhere. Verbatim, apart from two
// things this repository does not publish: the imported file's path, and the
// rule's width (the capture was 120 columns wide).
//
// What matters in it: the DECLINE is the highlighted row, both rows are
// indented two columns rather than one and carry no number, and the words that
// identify the question sit in BOTH options.
const fixtureExternalImportsMenu = `
` + rule + `
  Allow external CLAUDE.md file imports?

  This project's CLAUDE.md imports files outside the current working directory. Never allow this for third-party
  repositories.

  External imports:
    /shared/rules.md

  Important: Only use Claude Code with files you trust. Accessing untrusted files may pose security risks
  https://code.claude.com/docs/en/security

  ❯ No, disable external imports
    Yes, allow external imports

  Enter to confirm · Esc to cancel
`

// fakeImportsMenu is fakeMenu drawn the way THIS question paints: the layout in
// fixtureExternalImportsMenu, with the same key behaviour the unnumbered menu
// was measured to have.
type fakeImportsMenu struct{ fakeMenu }

func newImportsMenu() *fakeImportsMenu {
	return &fakeImportsMenu{fakeMenu{options: []string{"No, disable external imports", "Yes, allow external imports"}}}
}

func (m *fakeImportsMenu) screen() string {
	if m.chosen != 0 {
		return idleFixtureFor("answered")
	}
	var b strings.Builder
	b.WriteString("\n" + rule + "\n  Allow external CLAUDE.md file imports?\n\n" +
		"  This project's CLAUDE.md imports files outside the current working directory.\n\n" +
		"  External imports:\n    /shared/rules.md\n\n")
	for i, o := range m.options {
		if i == m.sel {
			fmt.Fprintf(&b, "  ❯ %s\n", o)
		} else {
			fmt.Fprintf(&b, "    %s\n", o)
		}
	}
	b.WriteString("\n  Enter to confirm · Esc to cancel\n")
	return b.String()
}

// The measured screen is a prompt, is read as an unnumbered menu (so a digit is
// never sent to it), highlights the DECLINE, and is named for what it asks.
func TestExternalImportsMenuIsAPromptAndIsClassified(t *testing.T) {
	p, unnumbered := parsePromptMenu(newScreen(fixtureExternalImportsMenu))
	if p == nil {
		t.Fatal("the measured external-imports menu is not recognised as a prompt")
	}
	if !unnumbered {
		t.Error("unnumbered = false; Respond would send a digit, which this menu ignores")
	}
	want := []string{"No, disable external imports", "Yes, allow external imports"}
	if fmt.Sprint(p.Options) != fmt.Sprint(want) {
		t.Errorf("options = %q, want %q", p.Options, want)
	}
	if p.Selected != 1 {
		t.Errorf("selected = %d, want 1 — the highlight defaults to the decline", p.Selected)
	}
	p.Kind = classifyPromptKind(p)
	if p.Kind != fleet.PromptExternalImports {
		t.Errorf("kind = %q, want %q", p.Kind, fleet.PromptExternalImports)
	}
	// By index, never by highlight: the highlighted row is the wrong one.
	if got, ok := affirmativeOption(p); !ok || got != 2 {
		t.Errorf("affirmativeOption = %d, %v; want 2 — the highlighted row is the decline", got, ok)
	}
}

// The kind reads the OPTIONS only, so it needs both halves of the runtime's
// accept/decline pair; a screen that carries one of them is not a screen this
// rule was measured on, and a consent must never be spent on it.
func TestExternalImportsIsNamedOnlyFromBothOfItsOptions(t *testing.T) {
	for _, tc := range []struct {
		name string
		opts []string
	}{
		{"only the accept", []string{"Yes, allow external imports", "Cancel"}},
		{"only the decline", []string{"No, disable external imports", "Cancel"}},
		{"the words spread across options", []string{"Allow", "Disable", "External", "Imports"}},
		{"an unrelated accept beside the decline", []string{"No, disable external imports", "Yes, proceed"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if k := classifyPromptKind(&fleet.SessionPrompt{Options: tc.opts}); k != "" {
				t.Errorf("kind = %q, want none", k)
			}
		})
	}
}

// A question an AGENT asks is never a runtime dialog, whatever its options say.
// The runtime appends its own escape hatches to an agent's question, and their
// presence disqualifies classification outright — otherwise an agent could
// write these two option labels into its own question and have the answer to
// its own decision chosen by a consent.
func TestAnAgentQuestionThatQuotesTheImportsOptionsIsNotClassified(t *testing.T) {
	p := &fleet.SessionPrompt{
		Question: "Shall I widen the import list?",
		Options: []string{"No, disable external imports", "Yes, allow external imports",
			"Type something.", "Chat about this"},
		Selected: 1,
	}
	if k := classifyPromptKind(p); k != "" {
		t.Errorf("an agent's own question was labelled %q — a consent would then answer it", k)
	}
	// And the same words in a QUESTION only, with unrelated options, are read as
	// nothing: the question is written by the agent and is never eligible.
	q := &fleet.SessionPrompt{
		Question: "Allow external CLAUDE.md file imports? Yes, allow external imports / No, disable external imports",
		Options:  []string{"Deploy", "Cancel"},
	}
	if k := classifyPromptKind(q); k != "" {
		t.Errorf("words in the question produced kind %q", k)
	}
}

// If a rewording ever put "allow" in both rows, one answer would mean the
// opposite of consent. Exactly one row may match, or nothing is answered.
func TestExternalImportsIsNotAnsweredWhenTwoRowsMatch(t *testing.T) {
	p := &fleet.SessionPrompt{
		Kind: fleet.PromptExternalImports,
		Options: []string{"No, do not allow external imports",
			"Yes, allow external imports"},
	}
	if got, ok := affirmativeOption(p); ok {
		t.Errorf("affirmativeOption = %d on an ambiguous rewording; want no answer", got)
	}
}

// The consent is spent on the affirmative row, by index, and only once.
func TestExternalImportsConsentAnswersTheMenuWithTheAllowRow(t *testing.T) {
	f := twoSessions()
	m := newImportsMenu()
	armDialog(f, "%1", m)
	d := newTestDriver(f)

	go d.settleNewSession(testCaller,
		fleet.SessionRef{Machine: "testbox", ID: "alpha💬"},
		fleet.SessionSpec{Cwd: "/work/alpha", Consents: []fleet.PromptKind{fleet.PromptExternalImports}})

	deadline := time.Now().Add(5 * time.Second)
	for {
		f.mu.Lock()
		chosen := m.chosen
		f.mu.Unlock()
		if chosen != 0 {
			if chosen != 2 {
				t.Fatalf("consent confirmed row %d (%s), want 2 (Yes, allow external imports)",
					chosen, m.options[chosen-1])
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("the external-imports consent never answered the menu")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// A consent is per question. A caller that consented to folder trust has said
// nothing about this one, and the menu must be left standing for a human.
func TestFolderTrustConsentDoesNotAnswerTheImportsQuestion(t *testing.T) {
	f := twoSessions()
	m := newImportsMenu()
	armDialog(f, "%1", m)
	d := newTestDriver(f)

	go d.settleNewSession(testCaller,
		fleet.SessionRef{Machine: "testbox", ID: "alpha💬"},
		fleet.SessionSpec{Cwd: "/work/alpha", TrustCwd: true,
			Consents: []fleet.PromptKind{fleet.PromptFolderTrust}})

	time.Sleep(3 * promptPollInterval)
	f.mu.Lock()
	chosen := m.chosen
	f.mu.Unlock()
	if chosen != 0 {
		t.Errorf("row %d was confirmed by a consent that named a different question", chosen)
	}
}

// And the other way: the imports consent leaves the trust question alone.
func TestImportsConsentDoesNotAnswerTheTrustQuestion(t *testing.T) {
	f := twoSessions()
	m := newTrustMenu()
	armDialog(f, "%1", m)
	d := newTestDriver(f)

	go d.settleNewSession(testCaller,
		fleet.SessionRef{Machine: "testbox", ID: "alpha💬"},
		fleet.SessionSpec{Cwd: "/work/alpha",
			Consents: []fleet.PromptKind{fleet.PromptExternalImports}})

	time.Sleep(3 * promptPollInterval)
	f.mu.Lock()
	chosen := m.chosen
	f.mu.Unlock()
	if chosen != 0 {
		t.Errorf("row %d was confirmed by a consent that named a different question", chosen)
	}
}

// Create refuses a kind with no consent entry, and accepts this one. The check
// runs before anything is started, so a caller learns it from the create.
func TestCreateAcceptsTheExternalImportsConsent(t *testing.T) {
	if _, ok := consentableKinds[fleet.PromptExternalImports]; !ok {
		t.Fatal("external-imports is not a consentable kind")
	}
	if needles := consentableKinds[fleet.PromptExternalImports]; len(needles) != 3 {
		t.Errorf("needles = %v; the affirmative is identified by allow + external + imports", needles)
	}
	// The resume chooser and the administrator's settings payload stay out.
	for _, k := range []fleet.PromptKind{fleet.PromptResumeChooser, fleet.PromptSettingsTrust} {
		if _, ok := consentableKinds[k]; ok {
			t.Errorf("%s became consentable", k)
		}
	}
}

// Through Create itself, with NOTHING but the consent: no initial prompt and no
// trustCwd. The step that answers a boot question used to start only when the
// create carried a prompt or the older trust boolean, so a consents-only create
// — the acceptance case of #211 — returned 201 and left the question standing.
//
// The fake multiplexer's new-session is a no-op against its own session table,
// so this wraps it: the moment the driver asks for a session, the pane that
// session would have is added, already parked on the question.
func TestAConsentsOnlyCreateStillAnswersTheQuestion(t *testing.T) {
	f := twoSessions()
	m := newImportsMenu()
	spawned := func(ctx context.Context, name string, args ...string) ([]byte, error) {
		out, err := f.exec(ctx, name, args...)
		if len(args) > 0 && args[0] == "new-session" {
			var session, cwd string
			for i, a := range args {
				switch a {
				case "-s":
					session = args[i+1]
				case "-c":
					cwd = args[i+1]
				}
			}
			f.mu.Lock()
			f.sessions = append(f.sessions, fakeSession{
				name: session, paneID: "%3", cwd: cwd, pid: 999, created: 1785760000,
			})
			f.mu.Unlock()
			armDialog(f, "%3", m)
		}
		return out, err
	}
	d := New("testbox", withExec(spawned),
		withNonce(func() string { return testNonce }),
		withClock(func() time.Time { return time.Unix(1785760000, 0) }))

	if _, err := d.Create(context.Background(), testCaller, "key-imports",
		fleet.SessionSpec{Name: "gamma", Cwd: "/work/gamma",
			Consents: []fleet.PromptKind{fleet.PromptExternalImports}}); err != nil {
		t.Fatalf("create: %v", err)
	}

	waitFor(t, "the consent to answer the imports question after a consents-only create", func() bool {
		f.mu.Lock()
		defer f.mu.Unlock()
		return m.chosen != 0
	})
	f.mu.Lock()
	chosen := m.chosen
	f.mu.Unlock()
	if chosen != 2 {
		t.Errorf("confirmed row %d, want 2 (Yes, allow external imports)", chosen)
	}
}
