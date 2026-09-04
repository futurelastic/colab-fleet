package tmux

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

// sequentialFiller returns n bytes of filler with no repeating substring
// longer than a few characters — "0000-0001-0002-…" truncated to length.
// Tests in this file assert on WHERE a needle is found (or not found) inside
// a painted composer, so the filler must not accidentally reproduce a
// substring at the wrong offset the way a uniform "aaaa…" or a short
// repeating pattern could.
func sequentialFiller(n int) string {
	var b strings.Builder
	i := 0
	for b.Len() < n {
		fmt.Fprintf(&b, "%04d-", i)
		i++
	}
	return b.String()[:n]
}

// colab-fleet#143, mechanism 1: `confirmLanded` used to take a needle from
// the raw payload's first 24 bytes regardless of line breaks. When the
// payload has more than one line and the first line is under 24 characters,
// that needle embeds a raw '\n' — but the rendered composer indents
// continuation lines (a marker on the first line, plain leading whitespace
// after it), so the raw newline-joined needle can never match the painted
// text, at any payload size.
//
// This reverts to `text[:24]` un-split — no SplitN, no tail — making every
// sub-24-char-first-line case below fail exactly as measured (a 38-byte
// three-line payload strands just like a 130-byte one), while the >=24
// cases keep passing either way.
func TestConfirmLandedNeedleNeverStraddlesTheNewline(t *testing.T) {
	const rule = "────────────────────"
	const marker = "❯ "
	const indent = "  "

	cases := []int{5, 10, 15, 20, 23, 24, 25, 30, 40}

	for _, firstLineLen := range cases {
		t.Run(fmt.Sprintf("first line %dch", firstLineLen), func(t *testing.T) {
			firstLine := sequentialFiller(firstLineLen)
			second := "a continuation line the raw source has no indent for"
			text := firstLine + "\n" + second

			// The painted composer, as the runtime actually renders it: a
			// marker on row one, plain leading whitespace on the
			// continuation row — never the raw '\n'-joined source.
			painted := "  transcript line\n" + rule + "\n" +
				marker + firstLine + "\n" +
				indent + second + "\n" + rule

			// Sanity: the OLD 24-byte raw-prefix needle is corrupted by the
			// indent — i.e. genuinely absent from painted — only once it
			// reaches past the line break into the second line's own
			// content (indented in painted, bare in the raw source). At
			// exactly the boundary the raw needle stops AT the newline and
			// never touches the second line, so it is not a useful case for
			// proving the bug — this only asserts the fixture where it
			// actually is corrupted.
			rawNeedle := text
			if len(rawNeedle) > 24 {
				rawNeedle = rawNeedle[:24]
			}
			corrupted := len(rawNeedle) > firstLineLen+1 // extends past "firstLine\n"
			if corrupted && strings.Contains(painted, rawNeedle) {
				t.Fatalf("setup: painted composer contains the OLD raw needle %q — "+
					"this fixture cannot distinguish the fix from the bug", rawNeedle)
			}

			f := twoSessions()
			f.setCapture("%2", painted)
			d := newTestDriver(f)

			_, _, ok := d.confirmLanded(context.Background(), "%2", text, map[pasteKey]int{})
			if !ok {
				t.Fatalf("confirmLanded did not confirm a landed multi-line paste "+
					"(first line %d chars) against:\n%s", firstLineLen, painted)
			}
		})
	}
}

// colab-fleet#143, mechanism 2: a composer taller than confirmLanded's `-S
// -6` capture scrolls to keep the cursor — parked at the end of a fresh
// paste — in view, so the HEAD of a long single-line payload can end up in a
// row the runtime never painted at all. No capture depth recovers a row
// that was never rendered; only matching the TAIL survives the scroll.
//
// Rows are pre-wrapped here at a fixed column width rather than left to a
// real terminal (the fake multiplexer does no wrapping of its own), landing
// deliberately on the measured boundary: 7 rows visible in full, 8 rows with
// the first scrolled out of the window.
func TestConfirmLandedMatchesTheTailWhenTheHeadScrolledOut(t *testing.T) {
	const rule = "────────────────────"
	const colWidth = 80
	const windowRows = 7 // what a `-S -6` capture leaves visible

	cases := []struct {
		name       string
		payloadLen int
		wantRows   int
		wantClip   bool
	}{
		{"7 rows, fits the window whole", colWidth * windowRows, windowRows, false},
		{"8 rows, head scrolled out of the window", colWidth * (windowRows + 1), windowRows + 1, true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			full := sequentialFiller(tc.payloadLen)

			var rows []string
			for i := 0; i < len(full); i += colWidth {
				end := i + colWidth
				if end > len(full) {
					end = len(full)
				}
				rows = append(rows, full[i:end])
			}
			if len(rows) != tc.wantRows {
				t.Fatalf("setup: wrapped to %d rows, want %d", len(rows), tc.wantRows)
			}

			visible := rows
			clipped := false
			if len(visible) > windowRows {
				visible = visible[len(visible)-windowRows:]
				clipped = true
			}
			if clipped != tc.wantClip {
				t.Fatalf("setup: window clipped = %v, want %v", clipped, tc.wantClip)
			}
			painted := strings.Join(visible, "\n") + "\n" + rule

			head := full[:24]
			if tc.wantClip && strings.Contains(painted, head) {
				t.Fatalf("setup: painted composer still contains the head %q — "+
					"this fixture does not exercise the scrolled-out case", head)
			}

			f := twoSessions()
			f.setCapture("%2", painted)
			d := newTestDriver(f)

			_, _, ok := d.confirmLanded(context.Background(), "%2", full, map[pasteKey]int{})
			if !ok {
				t.Fatalf("confirmLanded did not confirm a landed single-line paste "+
					"wrapped to %d rows (window shows the last %d) against:\n%s",
					len(rows), windowRows, painted)
			}
		})
	}
}

// colab-fleet#145: a genuinely multi-line payload — real newlines, not a
// single line wrapped by the terminal — can lose its FIRST line entirely to
// the same scroll-to-follow-the-cursor behavior #143 fixed for a single
// wrapped line. A composer taller than the `-S -6` capture window scrolls to
// keep the cursor, parked at the delivery's own end, in view, so with enough
// real lines the very first one is never painted at all — no wrapping, no
// long single line, just too many short ones.
//
// Measured live: a 10-line, 149-byte payload scrolled its first line
// entirely off an `-S -6` window that still painted lines 5-11. A needle
// built from the first line (#143's fix) cannot match a row that was never
// rendered; a needle built from the LAST line survives, because the cursor's
// own line is exactly what the scroll keeps on screen.
func TestConfirmLandedMatchesTheLastLineWhenTheFirstLineScrolledOut(t *testing.T) {
	const rule = "────────────────────"
	const marker = "❯ "
	const indent = "  "
	const windowRows = 7 // what a `-S -6` capture leaves visible

	lines := make([]string, 10)
	for i := range lines {
		lines[i] = fmt.Sprintf("payload line %d of the message", i)
	}
	text := strings.Join(lines, "\n")

	// The painted composer, as the runtime actually renders it: a marker on
	// row one, plain leading whitespace on every continuation row.
	rows := make([]string, len(lines))
	for i, l := range lines {
		if i == 0 {
			rows[i] = marker + l
			continue
		}
		rows[i] = indent + l
	}

	visible := rows
	if len(visible) > windowRows {
		visible = visible[len(visible)-windowRows:]
	}
	if strings.Contains(strings.Join(visible, "\n"), lines[0]) {
		t.Fatalf("setup: painted composer still shows the first line " +
			"— this fixture does not exercise the scrolled-out case")
	}
	painted := strings.Join(visible, "\n") + "\n" + rule

	f := twoSessions()
	f.setCapture("%2", painted)
	d := newTestDriver(f)

	_, _, ok := d.confirmLanded(context.Background(), "%2", text, map[pasteKey]int{})
	if !ok {
		t.Fatalf("confirmLanded did not confirm a landed multi-line paste "+
			"(first line scrolled fully out of the window) against:\n%s", painted)
	}
}
