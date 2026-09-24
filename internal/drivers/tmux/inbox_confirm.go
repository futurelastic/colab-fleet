package tmux

// This file is colab-fleet #184's answer to the question the inbox path could
// never answer before: did the receiver actually take the message?
//
// An inbox write has no reply channel (#120), so a clean write only ever proved
// that bytes reached a socket. #148 is what that cost: messages the receiver
// parked for a human and then dropped, reported delivered for three days. The
// terminal path never had this problem because it confirms from the receiver's
// own transcript (terminalpath2_transcript.go), and the inbox now does the same.
//
// # What is looked for, and why two shapes
//
// The receiver writes what it accepted into its transcript, but this repository
// has never seen the exact entry a peer message produces — the terminal path's
// matcher deliberately rejects every entry that is not a human turn, and a peer
// message is one of the things it rejects. So the probe carries two independent
// fingerprints of the delivery and either can confirm it:
//
//   - the ENVELOPE. The receiver accepts an envelope only after rebuilding it
//     and comparing bytes (ADR 148), so an accepted envelope is byte-identical
//     to the one written. If the entry holds that exact span anywhere — in a
//     string, a text block, a queued-operation payload — this delivery is in the
//     transcript. Unambiguous: Attest refuses every opening-bracket lookalike
//     in the body, so the first closing tag after an opening one is ours.
//   - the BODY, as the text of a user entry the runtime marked as NOT coming
//     from a human. For a runtime that records the message without its wrapper.
//
// Either matching is a confirmation; neither matching is silence, never a
// verdict. There is no "recorded a different turn" outcome here as there is on
// the terminal path: a peer message arrives beside other traffic, so an
// unrelated entry proves nothing about this delivery.
//
// What no offline test can establish is which shape the runtime really writes
// and how long a busy receiver takes to write it. inbox.confirmed_by_envelope,
// inbox.confirmed_by_origin_body and inbox.unconfirmed exist so that a live
// fleet answers both. A matcher that never matches is SAFE — every inbox send
// ends `unknown` and nothing is sent twice — but it makes the path useless, and
// unconfirmed ≈ written is how that would show.

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"os"
	"strings"
	"time"
)

// inboxConfirmWindow bounds the wait for the receiver's transcript to record an
// inbox write. It is the terminal path's own window: the same runtime records
// its own turns on the same clock, and a longer wait would hold the session's
// composer lock, which every other operation on the session queues behind.
const inboxConfirmWindow = submitConfirmWindow

// The envelope's tags, as the receiver writes them. They are the same literals
// inboxclient.Attest emits; only their spelling is needed here, to find spans.
const (
	inboxEnvelopeOpen  = "<cross-session-message"
	inboxEnvelopeClose = "</cross-session-message>"
)

// inboxProbe identifies one inbox delivery in a transcript. It holds digests
// only, never the message: it is persisted with the unconfirmed-delivery ledger
// (route.go), and a state file has no business holding message content.
type inboxProbe struct {
	// EnvelopeDigest is the digest of the exact envelope written to the socket.
	EnvelopeDigest string `json:"envelopeDigest,omitempty"`
	// BodyDigest is the digest of the body inside it, normalised the way every
	// transcript comparison here normalises text.
	BodyDigest string `json:"bodyDigest,omitempty"`
}

func digestHex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// newInboxProbe fingerprints a delivery: envelope is what was written as the
// message content, body is the text inside it.
func newInboxProbe(envelope, body string) inboxProbe {
	return inboxProbe{
		EnvelopeDigest: digestHex(envelope),
		BodyDigest:     digestHex(normalizeForMatch(body)),
	}
}

// inboxScan is what one look at the transcript found.
type inboxScan string

const (
	inboxScanSilent       inboxScan = ""
	inboxScanByEnvelope   inboxScan = "envelope"
	inboxScanByOriginBody inboxScan = "origin_body"
)

// inboxTailScan reads path from offset to the end, once, for an entry recording
// the delivery p identifies. offset is the transcript's size taken BEFORE the
// write, which is what makes an identical earlier message unable to count: text
// before the offset is never read.
//
// A scanner error (one line beyond recordLineLimit, say) is returned so the
// caller can count it as "could not read", which is not evidence of absence.
func inboxTailScan(path string, offset int64, p inboxProbe) (inboxScan, error) {
	f, err := os.Open(path)
	if err != nil {
		return inboxScanSilent, err
	}
	defer f.Close()
	if _, err := f.Seek(offset, io.SeekStart); err != nil {
		return inboxScanSilent, err
	}
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), recordLineLimit)
	for sc.Scan() {
		line := sc.Bytes()
		if len(strings.TrimSpace(string(line))) == 0 {
			continue
		}
		obj, ok := parseTranscriptObject(line)
		if !ok {
			continue
		}
		// A compaction summary quotes earlier turns and a sidechain is another
		// agent's conversation: neither is this session accepting a message.
		if truthy(obj["isCompactSummary"]) || truthy(obj["isSidechain"]) {
			continue
		}
		if p.EnvelopeDigest != "" && entryHoldsEnvelope(obj, p.EnvelopeDigest) {
			return inboxScanByEnvelope, nil
		}
		// The body-only shape is weaker evidence, so it is held to the stricter
		// standard the terminal matcher already applies: not a meta entry, and
		// marked by the runtime as coming from something other than a human.
		if truthy(obj["isMeta"]) {
			continue
		}
		if p.BodyDigest != "" && entryIsPeerBody(obj, p.BodyDigest) {
			return inboxScanByOriginBody, nil
		}
	}
	return inboxScanSilent, sc.Err()
}

// parseTranscriptObject decodes one transcript line. A line that is not a JSON
// object is not an entry, and is skipped rather than treated as an error: a
// transcript is written by another program while this one reads it, and a
// half-written last line is ordinary.
func parseTranscriptObject(line []byte) (map[string]any, bool) {
	var obj map[string]any
	if err := json.Unmarshal(line, &obj); err != nil || obj == nil {
		return nil, false
	}
	return obj, true
}

// entryHoldsEnvelope reports whether any string anywhere in obj contains an
// envelope span whose digest is want. Walking every string rather than one
// named field is deliberate: the shape is unmeasured, and the digest, not the
// location, is what makes a hit unambiguous.
func entryHoldsEnvelope(obj map[string]any, want string) bool {
	found := false
	walkStrings(obj, func(s string) bool {
		for from := 0; ; {
			i := strings.Index(s[from:], inboxEnvelopeOpen)
			if i < 0 {
				return false
			}
			start := from + i
			end := strings.Index(s[start:], inboxEnvelopeClose)
			if end < 0 {
				return false
			}
			end += start + len(inboxEnvelopeClose)
			if digestHex(s[start:end]) == want {
				found = true
				return true
			}
			from = start + len(inboxEnvelopeOpen)
		}
	})
	return found
}

// entryIsPeerBody reports whether obj is a user entry the runtime recorded as
// NOT coming from a human, whose text is exactly the delivery's body.
func entryIsPeerBody(obj map[string]any, wantDigest string) bool {
	if t, _ := obj["type"].(string); t != "user" {
		return false
	}
	origin, ok := obj["origin"].(map[string]any)
	if !ok {
		return false
	}
	kind, _ := origin["kind"].(string)
	if kind == "" || kind == "human" {
		return false
	}
	text := messageText(obj)
	if text == "" {
		return false
	}
	return digestHex(normalizeForMatch(text)) == wantDigest
}

// messageText is the plain text of an entry's message: a string content, or the
// text blocks of a content array, joined by newlines.
func messageText(obj map[string]any) string {
	msg, _ := obj["message"].(map[string]any)
	switch c := msg["content"].(type) {
	case string:
		return c
	case []any:
		var parts []string
		for _, b := range c {
			if m, ok := b.(map[string]any); ok {
				if s, ok := m["text"].(string); ok {
					parts = append(parts, s)
				}
			}
		}
		return strings.Join(parts, "\n")
	}
	return ""
}

// walkStrings calls visit on every string value under v, depth first, stopping
// as soon as visit returns true.
func walkStrings(v any, visit func(string) bool) bool {
	switch x := v.(type) {
	case string:
		return visit(x)
	case map[string]any:
		for _, e := range x {
			if walkStrings(e, visit) {
				return true
			}
		}
	case []any:
		for _, e := range x {
			if walkStrings(e, visit) {
				return true
			}
		}
	}
	return false
}

// confirmInbox polls the transcript until the delivery p identifies is recorded
// or the confirmation window closes, whichever comes first. It runs on a real
// timer, not the driver's clock: the clock is a seam tests freeze, and a frozen
// clock would make a deadline computed from it either instant or endless.
//
// It returns the shape that confirmed the delivery, or inboxScanSilent when
// nothing did — in which case the delivery is UNKNOWN, never failed: the
// message may yet be recorded, and the caller must not send it again.
func (d *Driver) confirmInbox(ctx context.Context, src transcriptSource, p inboxProbe) inboxScan {
	window := d.inboxWindow
	if window <= 0 {
		window = inboxConfirmWindow
	}
	deadline := time.Now().Add(window)
	for {
		res, err := inboxTailScan(src.path, src.offset, p)
		if err != nil {
			d.counters.incr(counterTranscriptScannerUnreadable)
		}
		if res != inboxScanSilent {
			return res
		}
		if !time.Now().Before(deadline) || ctx.Err() != nil {
			return inboxScanSilent
		}
		select {
		case <-ctx.Done():
			return inboxScanSilent
		case <-time.After(submitConfirmInterval):
		}
	}
}

// withInboxConfirmWindow shortens the confirmation wait. Unexported: tests only.
func withInboxConfirmWindow(w time.Duration) Option {
	return func(d *Driver) { d.inboxWindow = w }
}
