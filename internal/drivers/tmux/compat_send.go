package tmux

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	fleet "github.com/godx-jp/colab-fleet"
	"github.com/godx-jp/colab-fleet/internal/compat"
	"github.com/godx-jp/colab-fleet/internal/driver"
)

// The send probes: five model turns on the trusted session, each through the
// driver's real Send. They are the only checks that spend tokens, and they spend
// them on nonce-tagged, synthetic text at the smallest model and lowest effort.

// compatSample is one reading of the per-process record while a turn ran.
type compatSample struct {
	status    string
	updatedAt float64
	at        time.Duration
}

// compatSend is what one Send showed.
type compatSend struct {
	ran     bool
	skipped string
	// text is what was handed to Send; sent is what delivery confirmation
	// matches against — the same text after the driver's sanitiser, because a
	// control byte never reaches the composer.
	text, sent string
	rcpt       fleet.DeliveryReceipt
	err        error
	dur        time.Duration

	// The transcript entries appended while this send ran, in order.
	entries []map[string]any
	// matches are the user entries the driver's own matcher attributes to this
	// send; queued counts enqueue operations that match it.
	matches []map[string]any
	queued  int

	idle     bool   // the session came back to idle
	composer string // what the composer read afterwards
	samples  []compatSample
}

// compatTranscript locates session A's transcript by the driver's own
// encoding of its working directory.
func (h *compatHarness) transcriptPath() (string, bool) {
	rec := h.ev.a.rec
	if rec == nil {
		return "", false
	}
	m, err := recordFields(rec)
	if err != nil {
		return "", false
	}
	id, _ := m["sessionId"].(string)
	if !uuidShaped(id) {
		return "", false
	}
	return filepath.Join(h.world.store.projects, recordDirFor(h.world.dirs["a"]), id+".jsonl"), true
}

// readEntries parses every line of the file from offset, skipping ones that are
// not JSON objects. The lines that ARE parsed are kept as generic maps: a check
// judges shape, so it must not decode into a type that would hide a new key.
func readEntries(path string, offset int64) ([]map[string]any, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	if _, err := f.Seek(offset, 0); err != nil {
		return nil, err
	}
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), recordLineLimit)
	var out []map[string]any
	for sc.Scan() {
		var m map[string]any
		if json.Unmarshal(sc.Bytes(), &m) == nil {
			out = append(out, m)
		}
	}
	return out, sc.Err()
}

// attribute finds which of the entries the driver would count as this send's
// own turn: exactly the matching the delivery confirmation performs, so "exactly
// once" means what the driver means by it.
func attribute(entries []map[string]any, sent string) (matches []map[string]any, queued int) {
	for _, e := range entries {
		line, err := json.Marshal(e)
		if err != nil {
			continue
		}
		kind, text, _, ok := extractTranscriptCandidate(line)
		if !ok || !transcriptTurnMatches(text, sent) {
			continue
		}
		switch kind {
		case "queue-operation":
			queued++
		case "user":
			matches = append(matches, e)
		}
	}
	return matches, queued
}

// readStatus reads the two fields of the per-process record that move while a
// turn runs.
func (h *compatHarness) readStatus(pid int) (string, float64, bool) {
	b, ok := h.world.d.processSessionRecordBytes(pid)
	if !ok {
		return "", 0, false
	}
	m, err := recordFields(b)
	if err != nil {
		return "", 0, false
	}
	st, _ := m["status"].(string)
	at, _ := m["statusUpdatedAt"].(float64)
	return st, at, st != ""
}

const compatTurnWait = 120 * time.Second

// sendInto submits text through the driver's own Send, waits for the turn to
// finish, and records what the transcript then showed.
func (h *compatHarness) sendInto(ctx context.Context, name, text string) error {
	s := h.sendOf(name)
	s.text, s.sent = text, sanitizeForBracketedPaste(text)
	a := h.ev.a
	switch {
	case !a.ready:
		s.skipped = "the trusted session never showed a composer (see F-COMPOSER), so nothing could be sent"
		return nil
	case h.ev.dirty != "":
		s.skipped = "an earlier draft could not be cleared (" + h.ev.dirty + "), so nothing could be sent cleanly"
		return nil
	}
	path, ok := h.transcriptPath()
	if !ok {
		s.skipped = "no per-process record was found (see D1), so the transcript cannot be located"
		return nil
	}
	var offset int64
	if fi, err := os.Stat(path); err == nil {
		offset = fi.Size()
	}

	// Sample the record's status while the turn runs (D2).
	var mu sync.Mutex
	stop := make(chan struct{})
	done := make(chan struct{})
	started := time.Now()
	go func() {
		defer close(done)
		for {
			select {
			case <-stop:
				return
			default:
			}
			if st, at, ok := h.readStatus(a.pid); ok {
				mu.Lock()
				s.samples = append(s.samples, compatSample{status: st, updatedAt: at, at: time.Since(started)})
				mu.Unlock()
			}
			time.Sleep(100 * time.Millisecond)
		}
	}()

	s.ran = true
	h.turns++
	s.rcpt, s.err = h.world.d.Send(ctx, compatRequest, a.ref, text, driver.SendOptions{Submit: true})
	if s.err == nil && s.rcpt.Outcome != fleet.OutcomeRefused {
		// "Idle with an empty composer" alone is not the end of the turn: it is
		// also true in the moment after the submit and before the runtime has
		// begun (measured — a wait on it alone returned early, so the record
		// sampler stopped and the transcript had no reply yet). The turn is over
		// when the transcript holds the runtime's reply AND the session is idle.
		replied := func() bool {
			es, err := readEntries(path, offset)
			if err != nil {
				return false
			}
			for _, e := range es {
				if t, _ := e["type"].(string); t == "assistant" {
					return true
				}
			}
			return false
		}
		shot, ok := h.world.waitShot(ctx, a.ref, compatTurnWait, func(sh compatShot) bool {
			return sh.state.Status == fleet.StatusIdle && sh.scan == composerFound && sh.composer == "" && replied()
		})
		s.idle, s.composer = ok, shot.composer
	}
	close(stop)
	<-done
	s.dur = time.Since(started)

	entries, err := readEntries(path, offset)
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("reading the transcript: %w", err)
	}
	s.entries = entries
	s.matches, s.queued = attribute(entries, s.sent)
	return nil
}

func (h *compatHarness) sendOf(name string) *compatSend {
	if h.ev.sends == nil {
		h.ev.sends = map[string]*compatSend{}
	}
	if h.ev.sends[name] == nil {
		h.ev.sends[name] = &compatSend{}
	}
	return h.ev.sends[name]
}

// Payloads. Each one carries the run's nonce so it cannot be confused with text
// from anywhere else, and asks for the smallest possible reply.
func (h *compatHarness) sendWarm() string {
	return "Reply with exactly this token and nothing else: CFC-W-" + h.world.nonce
}

// The short send also carries Vietnamese in DECOMPOSED form (a base letter plus
// combining marks, written here as escapes so the form is unambiguous), which
// the delivery confirmation must match after normalisation. It is deliberately
// compatible with the system-prompt file's instruction (B7a): a request for
// "exactly this token" would contradict an instruction to end every reply with
// another, and a model may rightly obey the more specific.
func (h *compatHarness) sendShort() string {
	return "Say hello in one short sentence. (Ignore this tag, and this decomposed Vie\u0323\u0302t Nam: CFC-S-" + h.world.nonce + ")"
}

// One line of more than 800 bytes: long enough to collapse into a bare marker.
func (h *compatHarness) sendLong() string {
	return "Reply with only OK. Ignore this filler: " + strings.Repeat("filler-", 130) + "CFC-L-" + h.world.nonce
}

// Forty lines: a collapsed multi-line paste.
func (h *compatHarness) sendMulti() string {
	return "Reply with only OK. Ignore the lines below.\n" + compatLines(40) + "\nCFC-M-" + h.world.nonce
}

// Control bytes and a tab. The sanitiser removes the control bytes and keeps
// the tab, which the runtime records as four spaces.
func (h *compatHarness) sendControl() string {
	return "Reply with only OK. Ignore: CFC-C-" + h.world.nonce + " a\x01b\x1bc\x7fd\te"
}

// addSends registers the five probes. They run in order, on one session, so each
// send finds the transcript the one before it created.
func (h *compatHarness) addSends(s *compat.Suite) {
	send := func(id, name string, needs []string, text func() string) compat.Probe {
		return compat.Probe{ID: id, Stage: 4, Needs: needs, Budget: 3 * time.Minute, Run: func(ctx context.Context) error {
			return h.sendInto(ctx, name, text())
		}}
	}
	// Every send needs the drafts before it (a clean composer, and proof it can be
	// cleared), so the drafts run first even when only a send check is selected.
	drafts := []string{"boot.a", "draft.line"}
	s.Probes = append(s.Probes,
		// The first send finds no transcript yet — the runtime writes it at the
		// first turn — so the driver confirms it by the screen. It is sent first to
		// create the transcript; G1 is asserted on the send after it.
		send("send.warm", "warm", drafts, h.sendWarm),
		send("send.short", "short", []string{"send.warm"}, h.sendShort),
		send("send.long", "long", []string{"send.short"}, h.sendLong),
		send("send.multi", "multi", []string{"send.long"}, h.sendMulti),
		send("send.control", "control", []string{"send.multi"}, h.sendControl),
	)
}
