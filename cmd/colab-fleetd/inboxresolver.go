package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/godx-jp/colab-fleet/internal/drivers/tmux"
	"github.com/godx-jp/colab-fleet/internal/inboxclient"
)

// This file is colab-fleet #122's wiring for #119's inbox delivery path: a
// real tmux.InboxResolver implementation, built here in the composition root
// exactly the way the ADR (docs/adr/119-inbox-delivery.md, "Why a resolver
// function, not a path convention") says one must be — from machine-local
// configuration this repository never commits, never as a hardcoded path or
// naming convention borrowed from the runtime this driver targets.
//
// # What this deliberately is not
//
// It is not a scan of anything. It never lists a directory, never guesses a
// socket name from a pid, and never learns the real third-party address
// convention #118's own spike test refused to commit. It is a single keyed
// lookup — "does an entry exist for this exact pid" — against an index whose
// LOCATION an operator supplies (FLEET_INBOX_INDEX) and whose SHAPE this
// file defines for itself: one small JSON object per numeric process id,
// written there by whatever machine-local mechanism populates it, which this
// repository does not need to know and does not describe. That mechanism —
// and the real socket paths and tokens it writes — lives entirely outside
// this PUBLIC repo, per CLAUDE.local.md.

// inboxIndexEntry is this file's own index record — not a wire format any
// other system defines, just the smallest shape InboxAddress needs. Network
// is optional; an empty value defaults to "unix" the same way tmux.InboxAddress's
// own doc comment says sendViaInbox already treats an empty Network.
//
// colab-fleet #146: a record keyed by pid alone cannot tell "the process
// this driver resolved a moment ago" apart from "an unrelated process the
// kernel has since handed the same pid" — the same hazard #116 named for
// this driver's own ProcessIdentity, one layer up at this index. Two fields
// close that gap:
//
//   - StartedAt binds the record to one exact process run — but NOT in the
//     textual form ResolveProcessIdentity produces (`ps -o lstart=`, no zone
//     field). colab-fleet #147 measured that layout going into this index
//     verbatim from a UTC-rendering writer, compared via
//     time.ParseInLocation(layout, s, time.Local) on the identity side too —
//     a comparison that is false for every session, on every call,
//     permanently, and fails silently (a resolver error reads as
//     capability-absent, so delivery just falls back to the pane and nothing
//     ever says the index never matches). RFC 3339 closes that: the writer
//     must emit a zone-bearing instant, this file compares real instants
//     (time.Time.Equal), and a writer still emitting the old bare layout now
//     gets a loud parse error here instead of a silent, permanent mismatch.
//   - TokenPath is a LOCATOR, never the credential's value. #117's grant
//     authorises this service to hold a per-session token; it never
//     authorised a second directory holding a copy of one with its own,
//     longer lifetime. Reading the real source fresh at resolve time — the
//     same source keyed the same way the address itself is — means a
//     recycled pid can never read forward a stale token, with or without
//     the StartedAt check above.
type inboxIndexEntry struct {
	Network   string `json:"network"`
	Socket    string `json:"socket"`
	TokenPath string `json:"token_path"`
	// StartedAt is an RFC 3339 timestamp (zone-bearing — e.g. an offset or
	// "Z"), naming the exact instant the process at Socket's other end
	// started. Not the `ps -o lstart=` textual form tmux.ParseProcessStartTime
	// parses — that layout carries no zone, so a value rendered in one zone
	// and parsed in another silently denotes the wrong instant (#147). The
	// writer converts once, at write time, to whatever RFC 3339 rendering is
	// convenient for it; this file compares instants, never text.
	StartedAt string `json:"started_at"`
	// ModeClass is the permission-mode class the process at Socket's other
	// end is RUNNING IN — "bypass" or "prompting", the receiving runtime's
	// own two-value vocabulary (inboxclient.ModeClass). colab-fleet #148.
	//
	// Optional, and its absence is the ordinary shape of "this writer does
	// not know yet": the entry still resolves, delivery simply falls back to
	// the pane path because nothing can be attested. That is deliberate — a
	// wrong class is held exactly as firmly as a missing one, so guessing
	// here would reproduce #148 rather than fix it.
	//
	// Machine-local knowledge, like every other field in this record: the
	// writer knows how each session was launched, this repository does not
	// and must not.
	ModeClass string `json:"mode_class"`
}

// indexStartTimeMismatches counts every resolve call that found an index
// entry for the requested pid whose StartedAt did not match — colab-fleet
// #147's own "also worth fixing while here": a mismatch that happens on
// every call and is never surfaced is indistinguishable from one that never
// happens, the same reasoning internal/drivers/tmux/counters.go already
// gives for identity.process_unresolved. Deliberately the smallest thing
// that makes the rate readable (a test, an operator running the process
// with a debugger, or a future `colab-fleetd` status surface) — not wired to
// an HTTP endpoint here, matching that file's own precedent for landing a
// counter before its surface exists.
var indexStartTimeMismatches atomic.Int64

// indexStartTimeMismatchCount reports indexStartTimeMismatches' current
// value. Exported as a function rather than the raw var so a reader never
// takes a copy of the atomic itself.
func indexStartTimeMismatchCount() int64 { return indexStartTimeMismatches.Load() }

// indexUnattestableEntries counts every resolve call that found a live,
// start-time-matching entry carrying no usable permission-mode class — so
// every send it serves falls back to the pane path (#148).
//
// This exists for the same reason indexStartTimeMismatches does, and #147 is
// the precedent that makes it non-optional: on the day #148 lands this
// condition is true for EVERY entry, because no writer emits the new field
// yet. A condition true on every call and never surfaced is indistinguishable
// from one that never happens — which is exactly how #147's own permanent,
// silent mismatch survived. An operator rolling the writer out needs to watch
// this fall to zero; without a counter the only observable difference between
// "the field is not being written" and "the fix did not take" is neither.
var indexUnattestableEntries atomic.Int64

// indexUnattestableEntryCount reports indexUnattestableEntries' current value.
// Exported as a function for the same reason its sibling is.
func indexUnattestableEntryCount() int64 { return indexUnattestableEntries.Load() }

// newFileInboxResolver returns a tmux.InboxResolver that answers from one
// JSON file per pid under dir: "<dir>/<pid>.json". dir is never empty here —
// callers gate construction on FLEET_INBOX_INDEX being set (see main.go) —
// but this function does not itself assume that, so a test can exercise it
// directly against any directory.
//
// Read fresh on every call, matching InboxAddress.Token's own doc comment
// ("never cached by this driver"): a credential this service holds for
// every session on the machine (#117's full grant) is exactly the kind of
// value that must never go stale in memory between calls. #146 extends this
// to two reads per call instead of one — the index entry, then the
// credential its TokenPath names — because the second read is what makes
// "never cached" true of the token itself, not just of this function's own
// return value.
func newFileInboxResolver(dir string) tmux.InboxResolver {
	return func(_ context.Context, identity tmux.ProcessIdentity) (tmux.InboxAddress, bool, error) {
		path := filepath.Join(dir, strconv.Itoa(identity.PID)+".json")
		data, err := os.ReadFile(path)
		if err != nil {
			if os.IsNotExist(err) {
				// No entry for this pid: not every session has an inbox,
				// and this is the ordinary, expected shape of that fact —
				// see InboxResolver's own doc comment (inbox.go). Never
				// logged as an error, never surfaced as a refusal.
				return tmux.InboxAddress{}, false, nil
			}
			// Some other filesystem problem (permissions, a directory
			// where a file was expected) is a fact about THIS service's
			// plumbing, not about the target session — inbox.go's
			// sendViaInbox already treats a resolver error identically to
			// capability-absent, for exactly this reason.
			return tmux.InboxAddress{}, false, fmt.Errorf("inbox index: reading %s: %w", path, err)
		}

		var entry inboxIndexEntry
		if err := json.Unmarshal(data, &entry); err != nil {
			// #146: these records churn continuously (whatever populates
			// this directory rewrites them on every process transition), so
			// a torn read here is expected traffic, not a broken index —
			// still surfaced as an error like every other malformed-entry
			// case below (sendViaInbox already folds it into
			// capability-absent either way; see inbox.go's InboxResolver
			// doc comment), deliberately not "improved" into a silent
			// ok=false.
			return tmux.InboxAddress{}, false, fmt.Errorf("inbox index: %s is not valid JSON: %w", path, err)
		}
		if entry.Socket == "" || entry.TokenPath == "" || entry.StartedAt == "" {
			return tmux.InboxAddress{}, false, fmt.Errorf(
				"inbox index: %s is missing socket, token path, or start time", path)
		}

		// #147: RFC 3339, not tmux.ParseProcessStartTime's ps-lstart layout —
		// see inboxIndexEntry.StartedAt's doc comment for why. Both sides of
		// the Equal below are now real instants, so this comparison is
		// correct regardless of which zone the writer or this service's own
		// machine happens to be in.
		startedAt, err := time.Parse(time.RFC3339, entry.StartedAt)
		if err != nil {
			return tmux.InboxAddress{}, false, fmt.Errorf(
				"inbox index: %s has an unparseable start time %q (want RFC 3339): %w", path, entry.StartedAt, err)
		}
		if !startedAt.Equal(identity.StartedAt) {
			// #146's own proposed shape: on mismatch this is capability-
			// absent, not a refusal — a stale index is a fact about this
			// service's own plumbing, not about the target session, so it
			// must never be reported as the target refusing (that refusal
			// stays reserved for VerifyProcessIdentity failing immediately
			// before the write — inbox.go's sendViaInbox). The pane
			// fallback carries the message instead, which is the honest
			// response to half a capability.
			//
			// #147: this is exactly the case that used to be permanently,
			// silently true for every session — a genuine recycled-pid
			// mismatch is now indistinguishable, from inside this function,
			// from "the fix didn't take" or "the writer still emits the old
			// layout". indexStartTimeMismatches gives it a rate an operator
			// can check, the same idiom counters.go already uses elsewhere
			// in this repo for a coverage gap that must not go silent twice.
			indexStartTimeMismatches.Add(1)
			return tmux.InboxAddress{}, false, fmt.Errorf(
				"inbox index: %s describes pid %d started %s, but %d is now running as a process started %s (recycled)",
				path, identity.PID, startedAt, identity.PID, identity.StartedAt)
		}

		// Ruling 1 on #146: read the credential fresh from its own source
		// now, at resolve time — never from a copy this index file itself
		// might carry. TokenPath is keyed the same way the address already
		// is, so even a still-live-but-stale index row can never hand out a
		// token that belongs to a different run than the one just resolved.
		token, err := os.ReadFile(entry.TokenPath)
		if err != nil {
			if os.IsNotExist(err) {
				// The address row exists but the credential behind it does
				// not (yet, or no longer) — same "not every session has an
				// inbox" shape as the top-level not-exist case above, just
				// discovered one read later.
				return tmux.InboxAddress{}, false, nil
			}
			return tmux.InboxAddress{}, false, fmt.Errorf(
				"inbox index: reading token at %s: %w", entry.TokenPath, err)
		}

		// #148: validate the class STRICTLY against the closed set, and treat
		// an unrecognised value as a resolver error — capability-absent, the
		// same shape a bad start time already takes above. Passing an
		// uninterpretable string through to the wire would be worse than
		// asserting nothing: the receiver holds a wrong class just as firmly
		// as a missing one, and it would do so while this service reported
		// delivered. Absent (empty) is NOT an error — it is the ordinary "this
		// writer does not know yet", counted rather than complained about.
		var modeClass inboxclient.ModeClass
		switch {
		case entry.ModeClass == "":
			indexUnattestableEntries.Add(1)
		case inboxclient.ModeClass(entry.ModeClass).Valid():
			modeClass = inboxclient.ModeClass(entry.ModeClass)
		default:
			return tmux.InboxAddress{}, false, fmt.Errorf(
				"inbox index: %s has an unrecognised mode class %q (want %q or %q)",
				path, entry.ModeClass, inboxclient.ModeBypass, inboxclient.ModePrompting)
		}

		network := entry.Network
		if network == "" {
			network = "unix"
		}
		return tmux.InboxAddress{
			Network:   network,
			Socket:    entry.Socket,
			Token:     strings.TrimSpace(string(token)),
			ModeClass: modeClass,
		}, true, nil
	}
}
