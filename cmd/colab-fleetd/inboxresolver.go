package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"

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
type inboxIndexEntry struct {
	Network string `json:"network"`
	Socket  string `json:"socket"`
	Token   string `json:"token"`
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
// value that must never go stale in memory between calls.
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
			return tmux.InboxAddress{}, false, fmt.Errorf("inbox index: %s is not valid JSON: %w", path, err)
		}
		if entry.Socket == "" || entry.Token == "" {
			return tmux.InboxAddress{}, false, fmt.Errorf(
				"inbox index: %s is missing socket or token", path)
		}
		network := entry.Network
		if network == "" {
			network = "unix"
		}
		return tmux.InboxAddress{
			Network: network,
			Socket:  entry.Socket,
			Token:   entry.Token,
		}, true, nil
	}
}
