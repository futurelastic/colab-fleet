package main

import (
	"errors"
	"strings"
	"testing"
)

// colab-fleet #196 (ruled on #195, option 2): the inbox route is not available
// without a principal table. requireTableForInbox is the ONE definition — main
// refuses to start on it and doctor fails on it — so this table is what pins the
// rule for both.
func TestRequireTableForInbox(t *testing.T) {
	cases := []struct {
		name      string
		indexDir  string
		tableMode bool
		refused   bool
	}{
		{"an index and no table — the refused shape", "/some/index", false, true},
		{"an index and a table", "/some/index", true, false},
		{"no index and no table — the inbox route is simply off", "", false, false},
		{"no index and a table", "", true, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := requireTableForInbox(tc.indexDir, tc.tableMode)
			if (err != nil) != tc.refused {
				t.Fatalf("requireTableForInbox(%q, %v) = %v, refused want %v", tc.indexDir, tc.tableMode, err, tc.refused)
			}
		})
	}
}

// The refusal is read by an operator whose service just did not start, so it has
// to say what is wrong, why, and BOTH ways out — a table with the grant, or no
// index — and where the table is written. It must not name the index path: this
// process's own output is not the place machine-local layout belongs (see the
// inbox wiring in main).
func TestInboxNeedsTableRefusalIsActionable(t *testing.T) {
	err := requireTableForInbox("/machine/local/index/dir", false)
	if err == nil {
		t.Fatal("expected a refusal")
	}
	if !errors.Is(err, errInboxNeedsTable) {
		t.Fatalf("got %v, want errInboxNeedsTable", err)
	}
	msg := err.Error()
	for _, want := range []string{
		"FLEET_INBOX_INDEX", "principal table", "docs/install.md step 5", "human-relay", "unset FLEET_INBOX_INDEX",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("the refusal must mention %q: %s", want, msg)
		}
	}
	if strings.Contains(msg, "/machine/local/index/dir") {
		t.Errorf("the refusal must not echo the index path: %s", msg)
	}
}
