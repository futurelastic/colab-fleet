package tmux

import (
	"strings"
	"unicode"
)

// The composer text model (#180): how text read back off a composer is
// compared with text this driver sent, and how it is fingerprinted.
//
// # What a composer does to text
//
// The runtime renders a composer's content as rows. A row break is either a
// real newline in the text or the runtime's own word wrap, which breaks at a
// space and drops it — or, for a token longer than a row, breaks inside it
// with nothing dropped. Continuation rows are indented. composerText joins the
// rows back with one space each.
//
// So a comparison must ignore what the RENDERING decided (where rows break,
// how they are indented) and nothing else. Whitespace inside a row is the
// text's own: "rm -rf /tmp/build" and "rm -rf / tmp/build" are different
// commands, and a person's whitespace-only edit to text this driver left in a
// composer is a person's edit. The prototype stripped all whitespace to get
// resize tolerance, which made those two equal; this model keeps interior
// whitespace and gives the tolerance only to row joins.

// collapseSpace composes text to NFC (as far as nfclite covers) and folds
// every run of whitespace — including a newline — into one space, trimming
// both ends. A newline and the space a word wrap drops both become the one
// space composerText joins rows with, so text survives the trip through a
// composer unchanged by this function wherever the rows broke at a space.
func collapseSpace(s string) string {
	s = composeNFCLite(s)
	var b strings.Builder
	b.Grow(len(s))
	space := false
	for _, r := range s {
		if unicode.IsSpace(r) {
			space = true
			continue
		}
		if space && b.Len() > 0 {
			b.WriteByte(' ')
		}
		space = false
		b.WriteRune(r)
	}
	return b.String()
}

// composerTextDigest fingerprints a composer's content as composerText reads
// it. Stable across a resize wherever the runtime wraps at a space; a wrap
// inside an over-long token changes it, which is why no decision rests on
// the digest ALONE when the driver's own record can corroborate the text
// instead (composerMatchesText).
func composerTextDigest(pending string) string {
	return screenDigest(collapseSpace(pending))
}

// legacyComposerDigest is the composer digest as the build before #180
// computed it: screenDigest of composerText's own output, raw. Stranded
// records persist across a restart, and a caller may hold a digest it read
// before an upgrade, so a comparison accepts this form too. It is exact about
// whitespace, so accepting it gives up nothing the current form protects.
func legacyComposerDigest(pending string) string {
	return screenDigest(pending)
}

// composerDigestMatches reports whether claimed fingerprints pending in
// either the current or the legacy form. An empty claim never matches.
func composerDigestMatches(claimed, pending string) bool {
	if claimed == "" {
		return false
	}
	return claimed == composerTextDigest(pending) || claimed == legacyComposerDigest(pending)
}

// composerSegments returns a found composer's content rows, each trimmed
// and whitespace-collapsed, blank and dim (placeholder) rows dropped — the
// same rows composerText joins, kept apart so a comparison knows where the
// joins are.
func composerSegments(s screen) ([]string, composerScan) {
	prompt, last, scan := composerSpan(s)
	if scan != composerFound {
		return nil, scan
	}
	if prompt < len(s.raw) && allDim(afterMarker(s.raw[prompt])) {
		return nil, composerFound
	}
	var segs []string
	if first := collapseSpace(strings.TrimPrefix(strings.TrimSpace(s.lines[prompt]), composerRuneMarker)); first != "" {
		segs = append(segs, first)
	}
	for i := prompt + 1; i < last; i++ {
		if i < len(s.raw) && allDim(s.raw[i]) {
			continue
		}
		if seg := collapseSpace(s.lines[i]); seg != "" {
			segs = append(segs, seg)
		}
	}
	return segs, composerFound
}

// segmentsMatchText reports whether rows, read off a composer, render sent:
// sent (whitespace-collapsed) must equal the rows concatenated with, at each
// row join, either one space or nothing — the two things a row break can
// stand for. Whitespace inside a row must match exactly.
//
// allowSuffix accepts rows that render a TAIL of sent, at least
// composerSuffixMinBytes long — the runtime's own composer scrolls inside
// itself on a tall paste and shows only the end. Only a fresh send may use
// it; see confirmLandedV2.
func segmentsMatchText(segs []string, sent string, allowSuffix bool) bool {
	if len(segs) == 0 {
		return false
	}
	t := collapseSpace(sent)
	if t == "" {
		return false
	}
	if matchSegmentsFrom(segs, t, 0) {
		return true
	}
	if !allowSuffix {
		return false
	}
	total := len(segs) - 1
	for _, s := range segs {
		total += len(s)
	}
	if total < composerSuffixMinBytes {
		return false
	}
	for start := 1; start < len(t); start++ {
		if strings.HasPrefix(t[start:], segs[0]) && matchSegmentsFrom(segs, t, start) {
			return true
		}
	}
	return false
}

func matchSegmentsFrom(segs []string, t string, pos int) bool {
	for i, s := range segs {
		if i > 0 && strings.HasPrefix(t[pos:], " ") && strings.HasPrefix(t[pos+1:], s) {
			pos++
		}
		if !strings.HasPrefix(t[pos:], s) {
			return false
		}
		pos += len(s)
	}
	return pos == len(t)
}

// composerMatchesText is segmentsMatchText over a screen: whether the
// composer, as found on sc, renders exactly text (or a substantial tail of
// it, with allowSuffix).
func composerMatchesText(sc screen, text string, allowSuffix bool) bool {
	segs, scan := composerSegments(sc)
	return scan == composerFound && segmentsMatchText(segs, text, allowSuffix)
}

// composerRegion returns the rows a composer occupies on sc, fence to fence:
// from the prompt row to the row above the closing rule when the composer is
// found; when it is clipped, from the first VISIBLE row to the row above the
// closing rule — everything between the visible top and that rule is
// composer, or composerSpan would not have called it clipped. Nothing else
// on a screen may confirm a delivery: transcript above the composer is
// whatever the agent printed, and text below the closing rule belongs to
// something else entirely — a shell drawing under a composer frame the
// runtime left behind when it exited (#180 H2).
func composerRegion(sc screen) (rows []string, scan composerScan) {
	prompt, last, scan := composerSpan(sc)
	switch scan {
	case composerFound:
		return sc.lines[prompt:last], composerFound
	case composerClipped:
		closing := -1
		for i := len(sc.lines) - 1; i >= 0; i-- {
			if isRule(sc.lines[i]) {
				closing = i
				break
			}
		}
		top := sc.visibleTop
		if closing <= top {
			return nil, composerClipped
		}
		return sc.lines[top:closing], composerClipped
	}
	return nil, scan
}

// composerMarkers counts collapsed-paste markers inside the composer's own
// region only (#180 L2). A marker in scrollback — the transcript's echo of an
// earlier paste, or residue in the history margin above the visible pane —
// is never this delivery's.
func composerMarkers(sc screen) map[pasteKey]int {
	rows, scan := composerRegion(sc)
	if scan != composerFound && scan != composerClipped {
		return map[pasteKey]int{}
	}
	return markerCounts(strings.Join(rows, "\n"))
}
