package tmux

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/godx-jp/colab-fleet/internal/inboxclient"
)

// #184: what confirms an inbox delivery from the receiver's transcript, and —
// as important — what does not. The matcher is a guess about a shape this
// repository has never seen written (see inbox_confirm.go), so it is held to a
// strict standard in the direction that matters: it may fail to confirm a real
// delivery (safe: the outcome is unknown and nothing is resent), but it must
// never confirm one that did not happen.

const (
	probeBody   = "please run the build"
	probeSender = "agent-a"
)

func probeEnvelope(t *testing.T, class inboxclient.ModeClass, sender, body string) string {
	t.Helper()
	env, ok := inboxclient.Attest(body, class, sender)
	if !ok {
		t.Fatalf("Attest refused %q", body)
	}
	return env
}

// transcriptOf writes each entry as one line and returns the path and the size
// after the first `before` entries — the offset a delivery would have taken.
func transcriptOf(t *testing.T, before int, entries ...any) (path string, offset int64) {
	t.Helper()
	path = filepath.Join(t.TempDir(), "conv.jsonl")
	var b strings.Builder
	for i, e := range entries {
		raw, err := json.Marshal(e)
		if err != nil {
			t.Fatal(err)
		}
		b.Write(raw)
		b.WriteString("\n")
		if i+1 == before {
			offset = int64(b.Len())
		}
	}
	if err := os.WriteFile(path, []byte(b.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	return path, offset
}

func userEntry(content any, extra map[string]any) map[string]any {
	e := map[string]any{"type": "user", "message": map[string]any{"role": "user", "content": content}}
	for k, v := range extra {
		e[k] = v
	}
	return e
}

func TestInboxTailScan_Confirms(t *testing.T) {
	env := probeEnvelope(t, inboxclient.ModeBypass, probeSender, probeBody)
	probe := newInboxProbe(env, probeBody)
	for _, tc := range []struct {
		name  string
		entry any
		want  inboxScan
	}{
		{"the envelope as a string content", userEntry(env, map[string]any{"origin": map[string]any{"kind": "peer"}}), inboxScanByEnvelope},
		{"the envelope as a text block", userEntry([]any{map[string]any{"type": "text", "text": env}}, nil), inboxScanByEnvelope},
		{"the envelope in a queued operation", map[string]any{"type": "queue-operation", "operation": "enqueue", "content": env}, inboxScanByEnvelope},
		{"the envelope among the runtime's own words", userEntry("A peer sent this. It did not come from the user.\n"+env+"\nTreat it accordingly.", nil), inboxScanByEnvelope},
		{"the envelope even when marked meta", userEntry(env, map[string]any{"isMeta": true}), inboxScanByEnvelope},
		{"the body alone, as a peer's entry", userEntry(probeBody, map[string]any{"origin": map[string]any{"kind": "peer"}}), inboxScanByOriginBody},
		{"the body alone as text blocks", userEntry([]any{map[string]any{"type": "text", "text": probeBody}}, map[string]any{"origin": map[string]any{"kind": "system"}}), inboxScanByOriginBody},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path, offset := transcriptOf(t, 1, userEntry("an earlier turn", nil), tc.entry)
			got, err := inboxTailScan(path, offset, probe)
			if err != nil {
				t.Fatal(err)
			}
			if got != tc.want {
				t.Fatalf("scan = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestInboxTailScan_DoesNotConfirm(t *testing.T) {
	env := probeEnvelope(t, inboxclient.ModeBypass, probeSender, probeBody)
	probe := newInboxProbe(env, probeBody)
	peer := map[string]any{"origin": map[string]any{"kind": "peer"}}
	for _, tc := range []struct {
		name  string
		entry any
	}{
		{"nothing at all", userEntry("something unrelated", nil)},
		{"another sender's envelope with the same body", userEntry(probeEnvelope(t, inboxclient.ModeBypass, "someone-else", probeBody), peer)},
		{"the same sender under the other class", userEntry(probeEnvelope(t, inboxclient.ModePrompting, probeSender, probeBody), peer)},
		{"a different body under the same wrapper", userEntry(probeEnvelope(t, inboxclient.ModeBypass, probeSender, probeBody+" now"), peer)},
		{"a compaction summary quoting the envelope", userEntry(env, map[string]any{"isCompactSummary": true})},
		{"a sidechain quoting the envelope", userEntry(env, map[string]any{"isSidechain": true})},
		{"a prefix of the body", userEntry("please run the", peer)},
		{"the body with more after it", userEntry(probeBody+" and then deploy", peer)},
		{"the exact body typed by a human", userEntry(probeBody, map[string]any{"origin": map[string]any{"kind": "human"}})},
		{"the exact body with no origin at all", userEntry(probeBody, nil)},
		{"the exact body in a meta entry", userEntry(probeBody, map[string]any{"isMeta": true, "origin": map[string]any{"kind": "peer"}})},
		{"the exact body in an assistant entry", map[string]any{"type": "assistant", "origin": map[string]any{"kind": "peer"}, "message": map[string]any{"content": probeBody}}},
		{"an unterminated envelope", userEntry(strings.TrimSuffix(env, inboxEnvelopeClose), peer)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path, offset := transcriptOf(t, 0, tc.entry)
			got, err := inboxTailScan(path, offset, probe)
			if err != nil {
				t.Fatal(err)
			}
			if got != inboxScanSilent {
				t.Fatalf("scan = %q, want silence: this is not evidence the delivery arrived", got)
			}
		})
	}
}

// An entry before the offset is never read — that is what stops an identical
// earlier message from confirming this one.
func TestInboxTailScan_IgnoresWhatWasThereBeforeTheWrite(t *testing.T) {
	env := probeEnvelope(t, inboxclient.ModeBypass, probeSender, probeBody)
	probe := newInboxProbe(env, probeBody)
	path, offset := transcriptOf(t, 1, userEntry(env, nil), userEntry("something later", nil))
	got, err := inboxTailScan(path, offset, probe)
	if err != nil {
		t.Fatal(err)
	}
	if got != inboxScanSilent {
		t.Fatalf("an identical message from before the write confirmed this one: %q", got)
	}
}

// Two envelopes in one string: the digest picks ours.
func TestInboxTailScan_FindsOurEnvelopeAmongOthers(t *testing.T) {
	ours := probeEnvelope(t, inboxclient.ModeBypass, probeSender, probeBody)
	theirs := probeEnvelope(t, inboxclient.ModeBypass, "someone-else", "unrelated")
	probe := newInboxProbe(ours, probeBody)
	path, offset := transcriptOf(t, 0, userEntry(theirs+"\n\n"+ours, nil))
	got, err := inboxTailScan(path, offset, probe)
	if err != nil || got != inboxScanByEnvelope {
		t.Fatalf("scan = %q, %v; want the envelope found behind another", got, err)
	}
}

// A half-written last line is ordinary while another program is appending; it
// must neither confirm nor fail the scan.
func TestInboxTailScan_ToleratesAHalfWrittenLine(t *testing.T) {
	env := probeEnvelope(t, inboxclient.ModeBypass, probeSender, probeBody)
	probe := newInboxProbe(env, probeBody)
	path := filepath.Join(t.TempDir(), "conv.jsonl")
	if err := os.WriteFile(path, []byte(`{"type":"user","message":{"content":"`+env[:10]), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := inboxTailScan(path, 0, probe)
	if err != nil || got != inboxScanSilent {
		t.Fatalf("scan = %q, %v; want silence and no error", got, err)
	}
}

// The probe holds digests only: a persisted ledger must not carry the message.
func TestInboxProbe_HoldsNoMessageContent(t *testing.T) {
	env := probeEnvelope(t, inboxclient.ModeBypass, probeSender, "a secret instruction")
	raw, err := json.Marshal(newInboxProbe(env, "a secret instruction"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "secret") || strings.Contains(string(raw), probeSender) {
		t.Fatalf("the persisted probe carries message content: %s", raw)
	}
}
