package main

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/godx-jp/colab-fleet/internal/drivers/tmux"
)

// TestFileInboxResolver_EntryPresent_ResolvesAddress proves the acceptance
// criterion colab-fleet #122 states for the half that has an inbox: a
// target with an index entry resolves to the address and token that entry
// names, with the caller told ok=true.
func TestFileInboxResolver_EntryPresent_ResolvesAddress(t *testing.T) {
	dir := t.TempDir()
	writeIndexEntry(t, dir, 4242, `{"network":"unix","socket":"/tmp/whatever.sock","token":"tok-1"}`)

	resolve := newFileInboxResolver(dir)
	addr, ok, err := resolve(context.Background(), tmux.ProcessIdentity{PID: 4242})
	if err != nil {
		t.Fatalf("resolve() error = %v, want nil", err)
	}
	if !ok {
		t.Fatal("ok = false, want true — an index entry exists for this pid")
	}
	if addr.Network != "unix" || addr.Socket != "/tmp/whatever.sock" || addr.Token != "tok-1" {
		t.Fatalf("addr = %+v, want the exact fields the index entry named", addr)
	}
}

// TestFileInboxResolver_DefaultsNetworkToUnix mirrors InboxAddress's own doc
// comment ("Network and Socket name a net.Dial target... in practice unix"):
// an index entry that omits network must not become an unusable address.
func TestFileInboxResolver_DefaultsNetworkToUnix(t *testing.T) {
	dir := t.TempDir()
	writeIndexEntry(t, dir, 99, `{"socket":"/tmp/whatever.sock","token":"tok-1"}`)

	resolve := newFileInboxResolver(dir)
	addr, ok, err := resolve(context.Background(), tmux.ProcessIdentity{PID: 99})
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
	addr, ok, err := resolve(context.Background(), tmux.ProcessIdentity{PID: 7})
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
	_, ok, err := resolve(context.Background(), tmux.ProcessIdentity{PID: 5})
	if err == nil {
		t.Fatal("err = nil, want a parse error for a malformed index entry")
	}
	if ok {
		t.Error("ok = true, want false alongside a resolver error")
	}
}

// TestFileInboxResolver_IncompleteEntry_ReportsError covers the
// present-but-unusable case InboxAddress cannot represent: a file exists
// but is missing the socket or the token a delivery actually needs.
func TestFileInboxResolver_IncompleteEntry_ReportsError(t *testing.T) {
	dir := t.TempDir()
	writeIndexEntry(t, dir, 6, `{"socket":"/tmp/whatever.sock"}`) // no token

	resolve := newFileInboxResolver(dir)
	_, ok, err := resolve(context.Background(), tmux.ProcessIdentity{PID: 6})
	if err == nil {
		t.Fatal("err = nil, want an error for an entry missing its token")
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
	writeIndexEntry(t, dir, 111, `{"socket":"/tmp/a.sock","token":"tok-a"}`)
	writeIndexEntry(t, dir, 222, `{"socket":"/tmp/b.sock","token":"tok-b"}`)

	resolve := newFileInboxResolver(dir)
	addr, ok, err := resolve(context.Background(), tmux.ProcessIdentity{PID: 333})
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
