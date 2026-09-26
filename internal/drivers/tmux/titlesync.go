package tmux

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"os"
	"strings"
	"time"
	"unicode"

	fleet "github.com/godx-jp/colab-fleet"
	"github.com/godx-jp/colab-fleet/internal/driver"
)

// SyncTitle implements driver.TitleSyncer (colab-fleet #222).
//
// # Why this exists, in one sentence
//
// A rename changes the multiplexer's own idea of this session's name; the
// Claude Code runtime inside the pane keeps its OWN title, and nothing used
// to tell it a rename had happened — so a client that trusts the runtime's
// title over the multiplexer's would see the two disagree and "repair" the
// rename right back out. This brings the runtime's title to the same name,
// by delivering it exactly what a human would type: "/rename <name>".
//
// # Called after the id half, targeting the NEW id
//
// The caller (internal/service's handleRename, via rename_title.go) invokes
// this AFTER Driver.Rename has already renamed the multiplexer session and
// announced session.renamed — ref.ID is the session's CURRENT (new) id.
// There is no old id left to address by the time this runs; Rename's own
// aliasComposerLock call (tmux.go) is what makes a delivery against the new
// id safe to race against anything still in flight under the old one.
//
// # Never fails the caller's rename
//
// Every path returns a fleet.TitleSync value with its own status and
// evidence — a refused delivery, an unreadable transcript, an unusable
// name — never an error that would make a successful id-rename look like
// it failed. The multiplexer half already succeeded; this reports honestly
// on a SEPARATE fact.
//
// # Confirmation is the runtime's own transcript, never the screen
//
// "synced" is earned only by a `custom-title` entry the runtime itself
// wrote, read from its own per-process record — the same discipline
// resolveTranscriptSource/confirmSubmittedFromSource already apply to an
// ordinary send's OWN text, extended here to a fact neither of those
// functions reads: not "did this text arrive as a turn", but "does the
// runtime's own title now read this name". A local_command entry
// confirming the /rename command itself ran (colab-fleet #187) is weaker
// evidence — it says the runtime accepted the command, not that its title
// moved — so it is reported as `pending`, not `synced`, when a
// custom-title entry has not (yet) followed it. What is NOT measured
// anywhere in this repo is whether/when the runtime writes that
// custom-title entry after a programmatic /rename at all; where the
// evidence runs out, this degrades honestly to `pending` rather than
// guessing (see this driver's own #222 plan notes).
func (d *Driver) SyncTitle(ctx context.Context, req fleet.Request, ref fleet.SessionRef) fleet.TitleSync {
	ctx, cancel := d.bounded(ctx)
	defer cancel()

	name := ref.ID
	if name == "" || strings.ContainsFunc(name, unicode.IsControl) {
		d.counters.incr(counterTitleSyncFailed)
		return fleet.TitleSyncFailed(
			"the name to sync is empty or contains a control character; refusing to deliver it as a command", nil)
	}
	text := "/rename " + name

	rows, _, err := d.enumerate(ctx)
	if err != nil {
		d.counters.incr(counterTitleSyncFailed)
		return fleet.TitleSyncFailed("could not enumerate sessions to sync the title on: "+err.Error(), nil)
	}
	var target *paneRow
	for i := range rows {
		if rows[i].session == name {
			target = &rows[i]
			break
		}
	}
	if target == nil {
		d.counters.incr(counterTitleSyncFailed)
		return fleet.TitleSyncFailed("no session named "+name+" was found to sync the title on", nil)
	}

	// Resolved BEFORE delivery, the same ordering resolveTranscriptSource's
	// own caller contract requires of an ordinary send (terminalpath2_transcript.go):
	// the offset must describe "before this delivery", or a title the
	// runtime already carried would falsely confirm this one.
	src, srcOK := d.resolveTranscriptSource(ctx, ref, target)

	opts := driver.SendOptions{Submit: true, Route: fleet.RouteTerminal}
	if rec, ok := d.strandedRecordFor(ref.ID, target.cwd); ok && rec.Text == text {
		// Finishing our OWN prior title-sync attempt — never anyone else's
		// text (Send's own draft rule refuses that regardless of this flag).
		opts.ResumeIfStranded = true
	}

	receipt, err := d.Send(ctx, req, ref, text, opts)
	if err != nil {
		d.counters.incr(counterTitleSyncFailed)
		return fleet.TitleSyncFailed("delivering the title-sync command failed: "+err.Error(), nil)
	}
	if receipt.Outcome == fleet.OutcomeRefused {
		// #2.4's own protection did its job: a busy or genuinely stranded
		// (someone else's) composer refused rather than overwrite anything.
		// Nothing was pasted — the rename itself already succeeded, and
		// only the title half is reported as not done.
		d.counters.incr(counterTitleSyncFailed)
		return fleet.TitleSyncFailed(
			"the rename succeeded, but the runtime's own title could not be synced: "+receipt.Reason, &receipt)
	}

	if !srcOK {
		d.counters.incr(counterTitleSyncPending)
		return fleet.TitleSyncPending(
			"the rename succeeded and the title-sync command was sent, but no transcript could be "+
				"resolved for this session to confirm the runtime's own title changed", &receipt)
	}

	deadline := d.now().Add(submitConfirmWindow)
	var last titleScanResult
	for {
		scan, scanErr := transcriptTitleScan(src.path, src.offset, text)
		if scanErr != nil {
			d.counters.incr(counterTranscriptScannerUnreadable)
		} else {
			last = scan
		}
		if last.sawTitle {
			if last.title == name {
				d.counters.incr(counterTitleSyncSynced)
				return fleet.TitleSyncSynced(
					"the runtime's own transcript now carries this name as its custom-title", receipt)
			}
			d.counters.incr(counterTitleSyncRecordedOtherTitle)
			return fleet.TitleSyncFailed(
				"the runtime recorded a DIFFERENT custom-title (\""+last.title+"\") after this delivery, not \""+name+"\"",
				&receipt)
		}
		if d.now().After(deadline) || ctx.Err() != nil {
			break
		}
		select {
		case <-ctx.Done():
		case <-time.After(submitConfirmInterval):
		}
	}

	if last.ran {
		d.counters.incr(counterTitleSyncCommandWithoutTitle)
		return fleet.TitleSyncPending(
			"the runtime's own transcript confirms the /rename command ran, but no custom-title entry "+
				"followed it within the confirmation window", &receipt)
	}
	d.counters.incr(counterTitleSyncPending)
	return fleet.TitleSyncPending(
		"the title-sync command was sent (outcome: "+string(receipt.Outcome)+"), but the runtime's own "+
			"transcript recorded neither the command running nor a custom-title change within the "+
			"confirmation window", &receipt)
}

// titleScanResult is what transcriptTitleScan found scanning a transcript
// tail for evidence of a "/rename "+name delivery.
type titleScanResult struct {
	// title is the LAST custom-title entry's own value seen after the
	// offset — not trimmed, so an exact-string caller decides what counts
	// as a match. Meaningful only when sawTitle is true; a session whose
	// runtime never writes one at all reads identically to one this scan
	// simply has not caught up with yet, which is exactly why SyncTitle
	// never claims `synced` on sawTitle alone once the window has closed —
	// see this scan's own caller.
	title    string
	sawTitle bool
	// ran is true once a "command" kind entry matching sent was seen
	// (colab-fleet #187's local_command / <command-name> confirmation) —
	// evidence the runtime accepted and ran the /rename, independent of
	// whether its title has moved yet.
	ran bool
}

// transcriptTitleScan reads path from offset looking for what became of a
// "/rename "+name delivery: the runtime's own custom-title entries (the
// signal SyncTitle needs to report `synced`) and, as a weaker, secondary
// signal, whether a local_command/<command-name> entry confirms the
// command itself ran (colab-fleet #187) — evidence the command was
// accepted even when no custom-title has followed it yet, which SyncTitle
// reports as `pending` rather than a bare, unexplained non-confirmation.
//
// Unlike transcriptTailScan (terminalpath2_transcript.go), this does not
// stop at the first match: a custom-title entry can, in principle, be
// followed by another before this scan runs (a fast second rename, a
// runtime that writes more than one) and the LAST one is what the runtime's
// title actually reads as right now — the same "last one wins" rule
// extractTranscriptText's own siblings apply to a delivery's own confirming
// turn.
func transcriptTitleScan(path string, offset int64, sent string) (titleScanResult, error) {
	f, err := os.Open(path)
	if err != nil {
		return titleScanResult{}, err
	}
	defer f.Close()
	if _, err := f.Seek(offset, io.SeekStart); err != nil {
		return titleScanResult{}, err
	}
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), recordLineLimit)
	var out titleScanResult
	for sc.Scan() {
		line := sc.Bytes()
		if len(strings.TrimSpace(string(line))) == 0 {
			continue
		}
		var probe struct {
			Type        string `json:"type"`
			CustomTitle string `json:"customTitle"`
		}
		if json.Unmarshal(line, &probe) == nil && probe.Type == "custom-title" {
			out.title, out.sawTitle = probe.CustomTitle, true
			continue
		}
		if kind, candidateText, _, ok := extractTranscriptCandidate(line); ok && kind == "command" && commandMatches(candidateText, sent) {
			out.ran = true
		}
	}
	if err := sc.Err(); err != nil {
		return out, err
	}
	return out, nil
}

var _ driver.TitleSyncer = (*Driver)(nil)
