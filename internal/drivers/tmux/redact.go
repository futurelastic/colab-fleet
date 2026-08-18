package tmux

import (
	"strconv"
	"strings"
)

// RedactCapture is the mechanism this repository requires before any real
// pane capture may live in this PUBLIC repo's git history. See
// testdata/corpus/README.md for why a corpus exists at all and what it may
// hold; this file is how that boundary is enforced rather than trusted.
//
// It classifies each line of a capture independently into one of the
// shapes this package's own classifier reads — a rule, a spinner line, the
// composer's marker, a numbered prompt option, a known footer, the
// model/plan line, a usage-limit or turn-failure notice, the response
// bullet — and keeps ONLY the part of that line that is fixed runtime
// vocabulary or a structural marker. Everything else is replaced by a
// fixed placeholder that preserves nothing but leading indentation.
//
// # The default is DISCARD
//
// A line this function does not recognise is discarded, not kept. That is
// the entire design, stated once: a redactor that keeps by default has to
// be told about every way content can appear on a pane, and missing one is
// exactly how a testdata directory of real captures becomes a transcript
// store — a human glancing at each line and occasionally missing one,
// forever, in a repository that cannot un-publish a commit.
//
// Line-local classification is enough because the classifier's own
// positional logic (composerText's fencing check, parsePrompt's run of
// numbered options, usageLimit's and lastTurnFailed's live-tail window) is
// driven by which lines ARE rules, markers and options — never by what
// prose says. Redacting content within an already-shape-classified line
// changes nothing about which shape that line is, so every classify.go
// function that matters for a corpus's purpose reads a redacted screen
// exactly as it would read the real one.
//
// # Idempotent by construction
//
// Every branch below either returns its input unchanged (fixed runtime
// vocabulary needs no redacting) or returns one of a small set of FIXED
// placeholder tokens that do not themselves match any of the recognised
// shapes — so redacting an already-redacted line reproduces it exactly.
// TestCorpusIsFullyRedacted relies on exactly this property: it re-runs
// RedactCapture over every committed corpus screen and requires the output
// to be byte-identical to what is on disk. A committed fixture that is not
// a fixed point of this function is a fixture this function would still
// change — evidence that a human (or a bug) let something past it.
func RedactCapture(raw string) string {
	lines := strings.Split(raw, "\n")
	out := make([]string, len(lines))
	for i, line := range lines {
		out[i] = redactLine(line)
	}
	return strings.Join(out, "\n")
}

// placeholderToken replaces any line — or any part of a line — this
// function does not recognise as structural chrome.
const placeholderToken = "[redacted]"

// inputPlaceholder replaces composer text: what a human typed, submitted
// history the TUI echoes back, or a selected menu item's own composer-style
// marker line. The classifier never needs the words, only whether the
// composer is empty, and — for the placeholder-vs-typed distinction —
// whether the fragment is rendered dim.
const inputPlaceholder = "<input>"

func redactLine(raw string) string {
	stripped := stripEscapes(raw)
	trimmed := strings.TrimRight(stripped, " \t\r")
	content := strings.TrimSpace(trimmed)
	leading := leadingSpaces(trimmed)

	if content == "" {
		return ""
	}

	// Pure box-drawing. isRule tolerates a centred label but requires the
	// rule characters to dominate, so this never carries content by
	// construction — see isRule's own comment for why that threshold is
	// what it is.
	if isRule(stripped) {
		return stripped
	}

	// The spinner/status line. Its verb is drawn from the runtime's own
	// closed vocabulary (classify.go's package comment) and its duration
	// says nothing about what was worked on — kept verbatim, matching
	// every fixture already committed to this package.
	if _, ok := statusLine(content); ok {
		return stripped
	}

	// A line beginning with the composer marker is EITHER the live
	// composer or a selected menu item — parsePrompt strips the marker
	// before checking for a numbered option, and this mirrors that order
	// exactly, because getting it backwards would send a selected option's
	// real text through composer redaction instead of option redaction (or
	// vice versa) and corrupt what the corpus is testing.
	selected := strings.HasPrefix(content, composerRuneMarker)
	body := content
	if selected {
		body = strings.TrimSpace(strings.TrimPrefix(content, composerRuneMarker))
	}
	if n, text, ok := numberedOption(body); ok {
		prefix := ""
		if selected {
			prefix = composerRuneMarker + " "
		}
		return leading + prefix + redactOption(n, text)
	}
	if selected {
		return leading + redactComposerLine(raw)
	}

	// The response bullet is structural: usageLimit and lastTurnFailed both
	// key on its PRESENCE to tell live agent output from chrome above it
	// (F55). What follows it is the agent's own words, so the bullet
	// survives and the words do not.
	if strings.HasPrefix(content, responseBullet) {
		return leading + responseBullet + " " + placeholderToken
	}

	// A known footer is fixed runtime vocabulary end to end — nothing in
	// any of these strings varies with what a session was doing.
	if isKnownFooter(content) {
		return stripped
	}

	if red, ok := redactModelLine(content); ok {
		return leading + red
	}

	if red, ok := redactNoticeLine(content); ok {
		return leading + red
	}

	// Everything else is transcript: the agent's plan, its tool calls, a
	// todo list, prose. None of it is read by this package's classifier,
	// so none of it earns a place in a corpus this repository publishes.
	return leading + placeholderToken
}

func leadingSpaces(s string) string {
	n := len(s) - len(strings.TrimLeft(s, " "))
	return s[:n]
}

// redactComposerLine handles the one line where a rendering ATTRIBUTE, not
// just a marker, is structural: composerText tells a placeholder hint from
// real typed input by SGR intensity (allDim), so the redacted form must
// keep that signal rather than merely keep the marker.
func redactComposerLine(raw string) string {
	frag := afterMarker(raw)
	text := strings.TrimSpace(stripEscapes(frag))
	if text == "" {
		return composerRuneMarker
	}
	if allDim(frag) {
		return composerRuneMarker + " " + sgrDimOn + inputPlaceholder + sgrReset
	}
	return composerRuneMarker + " " + inputPlaceholder
}

// knownOptionPhrases is the same closed, runtime-sourced vocabulary
// classifyPromptKind already trusts enough to dispatch on (see its own
// comment on why option text — never question text — is the only prose
// this package matches, and commit 2ce367a for where this list was read
// out of the runtime's own binary rather than sampled from a capture).
// Reusing it here for redaction, rather than keeping every option's real
// text, means a corpus case can carry the runtime's own dialogs without
// carrying whatever an agent typed into an option that merely resembles
// one.
var knownOptionPhrases = []string{
	"yes, i trust this folder",
	"no, continue without these permissions",
	"yes, i trust these settings",
	"no, exit claude code",
	"no, exit",
	"yes, i accept",
	"yes, allow this",
	"yes, and don't ask again",
	"no, tell claude what to do",
	"don't ask me again",
	"resume from summary",
	"resume full session as-is",
	"chat about this",
	"type something",
}

func redactOption(n int, text string) string {
	lower := strings.ToLower(strings.TrimSpace(text))
	for _, phrase := range knownOptionPhrases {
		if strings.HasPrefix(lower, phrase) {
			return strconv.Itoa(n) + ". " + strings.TrimSpace(text)
		}
	}
	return strconv.Itoa(n) + ". " + placeholderToken
}

// isKnownFooter matches the runtime's fixed menu/status footers. Every
// phrase here is UI chrome with no variable content — see classify.go's own
// constants for the two that are load-bearing for prompt detection.
func isKnownFooter(content string) bool {
	for _, phrase := range []string{selectFooter, confirmFooter, amendFooter, "auto mode on", "shift+tab to cycle"} {
		if strings.Contains(content, phrase) {
			return true
		}
	}
	return false
}

// redactModelLine handles the plan/model status line (`▸ Opus 5 ·
// <project>`). Everything up to and including the last " · " is fixed
// runtime chrome; what follows it is the working directory or session
// name — exactly the vocabulary this public repo's own conventions forbid
// naming (CLAUDE.local.md, "no application, repo... names").
func redactModelLine(content string) (string, bool) {
	if !strings.HasPrefix(content, "▸") {
		return "", false
	}
	if idx := strings.LastIndex(content, " · "); idx >= 0 {
		return content[:idx+len(" · ")] + "<project>", true
	}
	return content, true
}

// redactNoticeLine reconstructs a usage-limit or turn-failure notice from a
// fixed template, keyed on the same marker substrings usageLimit and
// lastTurnFailed already match. Reconstructing from a template — rather
// than surgically editing the original line — is what keeps this
// idempotent: the template's own text contains the same marker substrings,
// so redacting it again detects the same triggers and reproduces the same
// template.
func redactNoticeLine(content string) (string, bool) {
	lower := strings.ToLower(content)
	switch {
	case strings.Contains(lower, "/usage-credits"):
		return "/usage-credits to finish what you're working on.", true
	case strings.Contains(lower, "limit") &&
		(strings.Contains(lower, "hit your") || strings.Contains(lower, "reached") || strings.Contains(lower, "usage")):
		if strings.Contains(lower, "resets ") || strings.Contains(lower, "try again ") || strings.Contains(lower, "available again ") {
			return "You've hit your weekly limit · resets <redacted>", true
		}
		return "You've hit your weekly limit", true
	case strings.Contains(lower, "api error"):
		reason := "API Error: <redacted>"
		if strings.Contains(lower, "temporary") || strings.Contains(lower, "try again") {
			reason += " (usually temporary — try again in a moment)"
		}
		return reason, true
	}
	return "", false
}
