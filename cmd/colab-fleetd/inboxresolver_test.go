package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/godx-jp/colab-fleet/internal/drivers/tmux"
)

// referenceStartedAt is one fixed instant, in the shape ProcessIdentity.StartedAt
// actually carries at runtime (ResolveProcessIdentity's ps-lstart parse, via
// tmux.ParseProcessStartTime — see that function's own doc comment). Fixtures
// below render the SAME instant as an inbox-index entry's StartedAt in RFC
// 3339 (referenceStartedAtRFC3339), matching colab-fleet #147's fixed
// contract: the identity side keeps parsing the process table exactly as it
// always did; only the index side's wire format changed.
var referenceStartedAt = mustParseStartTime("Wed Aug 26 10:15:23 2026")

// referenceStartedAtRFC3339 is referenceStartedAt, rendered the way #147's
// fixed inboxresolver.go now expects an index entry's StartedAt to read —
// zone-bearing, so it denotes the same instant regardless of which zone
// produced the text or which zone reads it back.
var referenceStartedAtRFC3339 = referenceStartedAt.Format(time.RFC3339)

func mustParseStartTime(s string) time.Time {
	t, err := tmux.ParseProcessStartTime(s)
	if err != nil {
		panic(err)
	}
	return t
}

// TestFileInboxResolver_EntryPresent_ResolvesAddress proves the acceptance
// criterion colab-fleet #122 states for the half that has an inbox: a
// target with an index entry resolves to the address and token that entry
// names, with the caller told ok=true. #146: the entry's StartedAt now has
// to match the identity passed in, and the token is read from the separate
// location TokenPath names — not from the index file itself.
func TestFileInboxResolver_EntryPresent_ResolvesAddress(t *testing.T) {
	dir := t.TempDir()
	tokenPath := writeToken(t, dir, "tok-1")
	writeIndexEntry(t, dir, 4242, fmt.Sprintf(
		`{"network":"unix","socket":"/tmp/whatever.sock","token_path":%q,"started_at":%q}`,
		tokenPath, referenceStartedAtRFC3339))

	resolve := newFileInboxResolver(dir)
	addr, ok, err := resolve(context.Background(), tmux.ProcessIdentity{PID: 4242, StartedAt: referenceStartedAt})
	if err != nil {
		t.Fatalf("resolve() error = %v, want nil", err)
	}
	if !ok {
		t.Fatal("ok = false, want true — an index entry exists for this pid and its start time matches")
	}
	if addr.Network != "unix" || addr.Socket != "/tmp/whatever.sock" || addr.Token != "tok-1" {
		t.Fatalf("addr = %+v, want the exact fields the index entry and its token file named", addr)
	}
}

// TestFileInboxResolver_DefaultsNetworkToUnix mirrors InboxAddress's own doc
// comment ("Network and Socket name a net.Dial target... in practice unix"):
// an index entry that omits network must not become an unusable address.
func TestFileInboxResolver_DefaultsNetworkToUnix(t *testing.T) {
	dir := t.TempDir()
	tokenPath := writeToken(t, dir, "tok-1")
	writeIndexEntry(t, dir, 99, fmt.Sprintf(
		`{"socket":"/tmp/whatever.sock","token_path":%q,"started_at":%q}`,
		tokenPath, referenceStartedAtRFC3339))

	resolve := newFileInboxResolver(dir)
	addr, ok, err := resolve(context.Background(), tmux.ProcessIdentity{PID: 99, StartedAt: referenceStartedAt})
	if err != nil || !ok {
		t.Fatalf("resolve() = (%+v, %v, %v), want a resolved address", addr, ok, err)
	}
	if addr.Network != "unix" {
		t.Errorf("network = %q, want the default %q", addr.Network, "unix")
	}
}

// TestFileInboxResolver_EntryAbsent_FallsBackCleanly proves colab-fleet
// #122's other acceptance half: a target with no index entry answers
// ok=false, err=nil — capability-absent, not a refusal and not an error —
// exactly the shape InboxResolver's own doc comment (inbox.go) requires so
// sendViaInbox falls through to the pane path.
func TestFileInboxResolver_EntryAbsent_FallsBackCleanly(t *testing.T) {
	dir := t.TempDir()
	// Deliberately nothing written for pid 7 — this is the ordinary shape
	// of "this session has no inbox", not a missing-setup error.

	resolve := newFileInboxResolver(dir)
	addr, ok, err := resolve(context.Background(), tmux.ProcessIdentity{PID: 7, StartedAt: referenceStartedAt})
	if err != nil {
		t.Fatalf("err = %v, want nil — an absent entry is not a resolver failure", err)
	}
	if ok {
		t.Fatalf("ok = true, want false — no index entry exists for this pid")
	}
	if addr != (tmux.InboxAddress{}) {
		t.Errorf("addr = %+v, want the zero value when ok=false", addr)
	}
}

// TestFileInboxResolver_StartTimeMismatch_CapabilityAbsentNotRefusal proves
// colab-fleet #146's core fix: an index row that names a start time other
// than the one just resolved describes a DIFFERENT run of this pid (the
// process #146 filed against — the kernel recycled a pid the writer had not
// caught up on). This must never be treated as the target refusing; it
// falls through to the pane path the same as every other capability-absent
// case above.
func TestFileInboxResolver_StartTimeMismatch_CapabilityAbsentNotRefusal(t *testing.T) {
	dir := t.TempDir()
	tokenPath := writeToken(t, dir, "tok-belongs-to-the-old-occupant")
	writeIndexEntry(t, dir, 4242, fmt.Sprintf(
		`{"socket":"/tmp/whatever.sock","token_path":%q,"started_at":%q}`,
		tokenPath, referenceStartedAtRFC3339))

	// The pid was recycled: the process live right now started at a
	// different instant than the index row describes.
	recycled := tmux.ProcessIdentity{PID: 4242, StartedAt: referenceStartedAt.Add(1 * time.Hour)}

	before := indexStartTimeMismatchCount()
	resolve := newFileInboxResolver(dir)
	addr, ok, err := resolve(context.Background(), recycled)
	if ok {
		t.Fatalf("ok = true, want false — the index entry describes a different process run")
	}
	if addr != (tmux.InboxAddress{}) {
		t.Errorf("addr = %+v, want the zero value on a start-time mismatch", addr)
	}
	if err == nil {
		t.Fatal("err = nil, want a reported mismatch (still capability-absent at the sendViaInbox call site)")
	}
	// #147's own "also worth fixing while here": this exact case must leave
	// a trace a caller can count, not just an error string nobody aggregates.
	if got := indexStartTimeMismatchCount(); got != before+1 {
		t.Errorf("indexStartTimeMismatchCount() = %d, want %d (one mismatch just happened)", got, before+1)
	}
}

// TestFileInboxResolver_RFC3339CrossZoneMatch proves colab-fleet #147's
// actual fix: an index entry whose StartedAt is rendered in a DIFFERENT zone
// than the one ProcessIdentity.StartedAt happens to carry must still match,
// because both are now compared as real instants (RFC 3339 parses its own
// offset) rather than as zone-blind text. This is deliberately the same
// shape #147 measured in practice — a UTC-rendering writer, a nine-hour
// offset — reproduced deterministically so the test never depends on the
// zone the test runner itself happens to be in.
func TestFileInboxResolver_RFC3339CrossZoneMatch(t *testing.T) {
	dir := t.TempDir()
	tokenPath := writeToken(t, dir, "tok-cross-zone")

	// One instant, as the identity side would carry it: rendered here nine
	// hours off UTC, matching #147's own measured offset.
	jst := time.FixedZone("JST", 9*60*60)
	instant := time.Date(2026, time.August, 26, 19, 15, 23, 0, jst)

	// The index entry names the SAME instant, rendered in UTC — the shape
	// #147's writer produces, and the exact case a zone-blind parse (the
	// pre-#147 tmux.ParseProcessStartTime-on-the-index-side bug) got wrong.
	writeIndexEntry(t, dir, 4343, fmt.Sprintf(
		`{"socket":"/tmp/whatever.sock","token_path":%q,"started_at":%q}`,
		tokenPath, instant.UTC().Format(time.RFC3339)))

	resolve := newFileInboxResolver(dir)
	addr, ok, err := resolve(context.Background(), tmux.ProcessIdentity{PID: 4343, StartedAt: instant})
	if err != nil {
		t.Fatalf("resolve() error = %v, want nil — same instant, only the rendered zone differs", err)
	}
	if !ok {
		t.Fatal("ok = false, want true — the index entry describes the same instant the identity carries")
	}
	if addr.Token != "tok-cross-zone" {
		t.Fatalf("token = %q, want %q", addr.Token, "tok-cross-zone")
	}
}

// TestFileInboxResolver_TokenReadFreshFromItsOwnLocation proves colab-fleet
// #146 ruling 1: the index entry's token field is a LOCATOR, and the
// resolver reads whatever that locator currently names — never a value
// baked into the index file at write time. Changing the token file's
// contents between two resolves, with the index entry never rewritten,
// must change what the resolver returns.
func TestFileInboxResolver_TokenReadFreshFromItsOwnLocation(t *testing.T) {
	dir := t.TempDir()
	tokenPath := writeToken(t, dir, "tok-first")
	writeIndexEntry(t, dir, 555, fmt.Sprintf(
		`{"socket":"/tmp/whatever.sock","token_path":%q,"started_at":%q}`,
		tokenPath, referenceStartedAtRFC3339))

	resolve := newFileInboxResolver(dir)
	identity := tmux.ProcessIdentity{PID: 555, StartedAt: referenceStartedAt}

	addr, ok, err := resolve(context.Background(), identity)
	if err != nil || !ok {
		t.Fatalf("first resolve() = (%+v, %v, %v), want a resolved address", addr, ok, err)
	}
	if addr.Token != "tok-first" {
		t.Fatalf("token = %q, want %q", addr.Token, "tok-first")
	}

	writeToken(t, dir, "tok-second") // same path, rewritten — the index entry itself is untouched

	addr, ok, err = resolve(context.Background(), identity)
	if err != nil || !ok {
		t.Fatalf("second resolve() = (%+v, %v, %v), want a resolved address", addr, ok, err)
	}
	if addr.Token != "tok-second" {
		t.Fatalf("token = %q, want the freshly-written %q — the resolver must not have cached the first read", addr.Token, "tok-second")
	}
}

// TestFileInboxResolver_TokenLocationAbsent_FallsBackCleanly covers the
// address row existing while its credential does not (yet, or no longer) —
// same capability-absent shape as an absent index row, discovered one read
// later.
func TestFileInboxResolver_TokenLocationAbsent_FallsBackCleanly(t *testing.T) {
	dir := t.TempDir()
	writeIndexEntry(t, dir, 8, fmt.Sprintf(
		`{"socket":"/tmp/whatever.sock","token_path":%q,"started_at":%q}`,
		filepath.Join(dir, "no-such-token"), referenceStartedAtRFC3339))

	resolve := newFileInboxResolver(dir)
	addr, ok, err := resolve(context.Background(), tmux.ProcessIdentity{PID: 8, StartedAt: referenceStartedAt})
	if err != nil {
		t.Fatalf("err = %v, want nil — an absent token location is not a resolver failure", err)
	}
	if ok {
		t.Fatalf("ok = true, want false — no credential exists at the named location")
	}
	if addr != (tmux.InboxAddress{}) {
		t.Errorf("addr = %+v, want the zero value when ok=false", addr)
	}
}

// TestFileInboxResolver_MalformedEntry_ReportsErrorNotFallback proves a
// malformed index entry is treated as a resolver-side problem (this
// service's own plumbing), not silently swallowed into "no inbox here" —
// the same distinction inbox.go's InboxResolver doc comment draws between
// ok=false and err!=nil, both of which sendViaInbox then folds into the
// same "no usable capability" outcome, but only one of which is this file's
// job to raise loudly enough to be logged.
func TestFileInboxResolver_MalformedEntry_ReportsErrorNotFallback(t *testing.T) {
	dir := t.TempDir()
	writeIndexEntry(t, dir, 5, `{not json`)

	resolve := newFileInboxResolver(dir)
	_, ok, err := resolve(context.Background(), tmux.ProcessIdentity{PID: 5, StartedAt: referenceStartedAt})
	if err == nil {
		t.Fatal("err = nil, want a parse error for a malformed index entry")
	}
	if ok {
		t.Error("ok = true, want false alongside a resolver error")
	}
}

// TestFileInboxResolver_IncompleteEntry_ReportsError covers the
// present-but-unusable case InboxAddress cannot represent: a file exists
// but is missing the socket, the token path, or the start time a delivery
// actually needs.
func TestFileInboxResolver_IncompleteEntry_ReportsError(t *testing.T) {
	dir := t.TempDir()
	writeIndexEntry(t, dir, 6, `{"socket":"/tmp/whatever.sock"}`) // no token_path, no started_at

	resolve := newFileInboxResolver(dir)
	_, ok, err := resolve(context.Background(), tmux.ProcessIdentity{PID: 6, StartedAt: referenceStartedAt})
	if err == nil {
		t.Fatal("err = nil, want an error for an entry missing its token path and start time")
	}
	if ok {
		t.Error("ok = true, want false alongside a resolver error")
	}
}

// TestFileInboxResolver_UnparseableStartTime_ReportsError covers a start
// time this file cannot parse as RFC 3339 — a resolver-side data problem,
// same bucket as a malformed or incomplete entry.
func TestFileInboxResolver_UnparseableStartTime_ReportsError(t *testing.T) {
	dir := t.TempDir()
	tokenPath := writeToken(t, dir, "tok-1")
	writeIndexEntry(t, dir, 9, fmt.Sprintf(
		`{"socket":"/tmp/whatever.sock","token_path":%q,"started_at":"not-a-timestamp"}`, tokenPath))

	resolve := newFileInboxResolver(dir)
	_, ok, err := resolve(context.Background(), tmux.ProcessIdentity{PID: 9, StartedAt: referenceStartedAt})
	if err == nil {
		t.Fatal("err = nil, want an error for an unparseable start time")
	}
	if ok {
		t.Error("ok = true, want false alongside a resolver error")
	}
}

// TestFileInboxResolver_OldPsLayoutStartTime_ReportsError locks in colab-fleet
// #147's own stated goal for a writer that has not been updated yet: a
// pre-#147 index entry — StartedAt in the ps-lstart textual form, no zone —
// must now fail loudly (a parse error, folded into capability-absent at the
// sendViaInbox call site) instead of silently comparing as if it were local
// time. That silent-wrong-instant comparison is exactly the bug #147 found;
// this test is what stops it coming back as "well, it still parses".
func TestFileInboxResolver_OldPsLayoutStartTime_ReportsError(t *testing.T) {
	dir := t.TempDir()
	tokenPath := writeToken(t, dir, "tok-1")
	writeIndexEntry(t, dir, 10, fmt.Sprintf(
		`{"socket":"/tmp/whatever.sock","token_path":%q,"started_at":"Wed Aug 26 10:15:23 2026"}`, tokenPath))

	resolve := newFileInboxResolver(dir)
	_, ok, err := resolve(context.Background(), tmux.ProcessIdentity{PID: 10, StartedAt: referenceStartedAt})
	if err == nil {
		t.Fatal("err = nil, want an error — the old ps-lstart layout is no longer an accepted StartedAt format")
	}
	if ok {
		t.Error("ok = true, want false alongside a resolver error")
	}
}

// TestFileInboxResolver_NeverListsTheDirectory is this file's own privacy
// contract made mechanical: newFileInboxResolver must resolve a single
// named pid without ever reading the directory itself, so it can never
// observe — let alone report — an entry it was not asked about by identity.
// A directory with entries for OTHER pids and none for the one asked about
// must still answer ok=false, never "found something else".
func TestFileInboxResolver_NeverListsTheDirectory(t *testing.T) {
	dir := t.TempDir()
	tokenA := writeToken(t, dir, "tok-a")
	tokenB := writeToken(t, dir, "tok-b")
	writeIndexEntry(t, dir, 111, fmt.Sprintf(
		`{"socket":"/tmp/a.sock","token_path":%q,"started_at":%q}`, tokenA, referenceStartedAtRFC3339))
	writeIndexEntry(t, dir, 222, fmt.Sprintf(
		`{"socket":"/tmp/b.sock","token_path":%q,"started_at":%q}`, tokenB, referenceStartedAtRFC3339))

	resolve := newFileInboxResolver(dir)
	addr, ok, err := resolve(context.Background(), tmux.ProcessIdentity{PID: 333, StartedAt: referenceStartedAt})
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if ok {
		t.Fatalf("ok = true with addr %+v, want false — pid 333 has no entry of its own", addr)
	}
}

func writeIndexEntry(t *testing.T, dir string, pid int, body string) {
	t.Helper()
	path := filepath.Join(dir, strconv.Itoa(pid)+".json")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("writing fixture %s: %v", path, err)
	}
}

// writeToken writes (or overwrites) a fixed-name token file inside dir and
// returns its path — standing in for the separate, machine-local credential
// source colab-fleet #146 ruling 1 asks the index to point at rather than
// copy.
func writeToken(t *testing.T, dir, value string) string {
	t.Helper()
	path := filepath.Join(dir, "token")
	if err := os.WriteFile(path, []byte(value), 0o600); err != nil {
		t.Fatalf("writing token fixture %s: %v", path, err)
	}
	return path
}
