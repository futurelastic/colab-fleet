package main

import "errors"

// The inbox route needs a principal table (colab-fleet #196, ruled on #195,
// option 2).
//
// Why. Since #184 a send with no `route` is `auto`, and `auto` means "by who is
// sending": a caller holding the human-relay grant goes through the terminal,
// unlabelled, as the user's own turn; everyone else goes through the inbox
// whenever the session can take it, where the runtime marks the text as a peer
// message that did not come from the user. That split is only right if the
// service KNOWS who is a human relay. With a principal table it knows it from a
// grant. With none, every caller presents the one shared token, nothing tells a
// person's client from an agent, and the only way to express "this is a person"
// is a request header any bearer of the token can set — the fact ADR 184 says
// must never be inferred from what a caller sets.
//
// What is refused. Turning the inbox route on — pointing FLEET_INBOX_INDEX at an
// index — while no principal table is configured. Nothing else changes: a
// machine with no table and no index has no inbox route, so every delivery goes
// through the terminal, nobody is diverted into a peer message, and the
// single-token relay a machine like that already has keeps working. The
// alternative that was rejected (#195 option 1, and #196's first draft) was to
// stop honouring the relay headers at request time; on a single-token machine
// that leaves no way at all to relay a person unlabelled.
//
// Where it is decided. Here, once, as a pure function of the two facts that
// decide it, so the service's startup and `doctor` cannot disagree about it —
// the same reason service.Grants() is the one definition of the grant set. The
// startup gate is a refusal to start (like the missing-token gate beside it) and
// not a quiet "ignore the index": a machine whose operator set an index and got
// a service that silently never used it is the failure class #122 already was.

// inboxNeedsTableSummary is the one-line statement of the refusal.
const inboxNeedsTableSummary = "FLEET_INBOX_INDEX is set but no principal table is configured"

// inboxNeedsTableRemedy says why, and names both ways out.
const inboxNeedsTableRemedy = "the inbox route is only available with a principal table, so that who relays a " +
	"person's messages is a grant the table holds and never a header a caller sets (ADR 184, #195). " +
	"Write a principal table (docs/install.md step 5) and give the principal that relays a person's messages the " +
	"human-relay grant, or unset FLEET_INBOX_INDEX to keep every delivery on the terminal path"

// errInboxNeedsTable is what startup refuses with.
var errInboxNeedsTable = errors.New(inboxNeedsTableSummary + " — " + inboxNeedsTableRemedy)

// requireTableForInbox reports whether the inbox route may be turned on. indexDir
// is FLEET_INBOX_INDEX as read; tableMode is whether a validated principal table
// is in hand (main's cfgFile != nil, doctor's cfg != nil — loadConfig only
// returns a table that names at least one principal).
//
// An empty indexDir is the feature being off, which needs nothing. A set one
// needs the table.
func requireTableForInbox(indexDir string, tableMode bool) error {
	if indexDir == "" || tableMode {
		return nil
	}
	return errInboxNeedsTable
}
