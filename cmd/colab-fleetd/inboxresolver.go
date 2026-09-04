package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/godx-jp/colab-fleet/internal/drivers/tmux"
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
//   - StartedAt binds the record to one exact process run, in the identical
//     textual form ResolveProcessIdentity already produces
//     (tmux.ParseProcessStartTime) — compared exactly, never a copy that
//     could itself go stale.
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
	StartedAt string `json:"started_at"`
}

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

		startedAt, err := tmux.ParseProcessStartTime(entry.StartedAt)
		if err != nil {
			return tmux.InboxAddress{}, false, fmt.Errorf(
				"inbox index: %s has an unparseable start time %q: %w", path, entry.StartedAt, err)
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

		network := entry.Network
		if network == "" {
			network = "unix"
		}
		return tmux.InboxAddress{
			Network: network,
			Socket:  entry.Socket,
			Token:   strings.TrimSpace(string(token)),
		}, true, nil
	}
}
