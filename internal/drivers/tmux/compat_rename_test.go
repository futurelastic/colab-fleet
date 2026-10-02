package tmux

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	fleet "github.com/futurelastic/muster"
)

// H-RENAME (muster #227): every branch of its Eval, built by hand the
// same way TestFImportsJudgesTheDialogItIsShown and
// TestC1NamesTheImportsKeysWhenTheSeededDirectoryStillAsks build theirs — the
// multiplexer-gated tests would prove the same findings end to end against a
// real runtime, but CI never has one; these run everywhere.
func TestHRenameJudgesWhatThePollFound(t *testing.T) {
	for _, tc := range []struct {
		name   string
		rename compatRename
		pass   bool
		want   []string
	}{
		{
			"never ran: no composer",
			compatRename{skipped: "the trusted session never showed a composer (see F-COMPOSER), so nothing could be renamed"},
			false,
			[]string{"never showed a composer"},
		},
		{
			"the id-half rename itself failed",
			compatRename{ran: true, to: "cfc-x-renamed", renameErr: errors.New("boom")},
			false,
			[]string{`"cfc-x-renamed"`, "boom", "no programmatic /rename could be measured"},
		},
		{
			"the id-half rename was not accepted",
			compatRename{ran: true, to: "cfc-x-renamed", ack: fleet.RenameAck{Accepted: false}},
			false,
			[]string{"was not accepted"},
		},
		{
			"a matching custom-title followed",
			compatRename{
				ran: true, to: "cfc-x-renamed", ack: fleet.RenameAck{Accepted: true},
				sync:       fleet.TitleSyncPending("delivered", nil),
				scan:       titleScanResult{sawTitle: true, title: "cfc-x-renamed", ran: true},
				titleAfter: 250 * time.Millisecond,
			},
			true,
			[]string{"was followed by a custom-title", "250ms after delivery"},
		},
		{
			"a DIFFERENT custom-title followed",
			compatRename{
				ran: true, to: "cfc-x-renamed", ack: fleet.RenameAck{Accepted: true},
				sync:       fleet.TitleSyncPending("delivered", nil),
				scan:       titleScanResult{sawTitle: true, title: "someone-else", ran: true},
				titleAfter: time.Second,
			},
			false,
			[]string{"names", `"someone-else"`, "instead"},
		},
		{
			"the command ran but no custom-title ever followed",
			compatRename{
				ran: true, to: "cfc-x-renamed", ack: fleet.RenameAck{Accepted: true},
				sync: fleet.TitleSyncPending("the command ran, no title yet", nil),
				scan: titleScanResult{ran: true},
			},
			false,
			[]string{"confirms the /rename command ran", "does not write one"},
		},
		{
			"the transcript could not be read",
			compatRename{
				ran: true, to: "cfc-x-renamed", ack: fleet.RenameAck{Accepted: true},
				sync:    fleet.TitleSyncPending("delivered", nil),
				scanErr: errors.New("no such file"),
			},
			false,
			[]string{"could not be read", "no such file"},
		},
		{
			"neither the command nor a title ever showed",
			compatRename{
				ran: true, to: "cfc-x-renamed", ack: fleet.RenameAck{Accepted: true},
				sync: fleet.TitleSyncPending("nothing confirmed either way", nil),
			},
			false,
			[]string{"recorded neither the command running nor a custom-title"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := &compatHarness{}
			h.ev.rename = tc.rename
			v := evalCheck(t, h, "H-RENAME")
			if v.Pass != tc.pass || v.Err {
				t.Fatalf("verdict = %+v, want pass=%v and not an error", v, tc.pass)
			}
			for _, w := range tc.want {
				if !strings.Contains(v.Detail, w) {
					t.Errorf("detail %q does not contain %q", v.Detail, w)
				}
			}
		})
	}
}

// renameInto's skip branches never touch h.world, so they are checkable
// without a multiplexer at all: a session that never showed a composer, one
// left dirty by an earlier draft that could not be cleared, and one with no
// per-process record to locate the transcript from.
func TestRenameIntoSkipsWithoutASession(t *testing.T) {
	t.Run("no composer", func(t *testing.T) {
		h := &compatHarness{}
		h.ev.a = compatBoot{ready: false}
		if err := h.renameInto(context.Background()); err != nil {
			t.Fatalf("renameInto returned an error, want evidence recorded instead: %v", err)
		}
		if h.ev.rename.ran {
			t.Error("ran should stay false")
		}
		if !strings.Contains(h.ev.rename.skipped, "never showed a composer") {
			t.Errorf("skipped = %q", h.ev.rename.skipped)
		}
	})

	t.Run("an earlier draft left the composer dirty", func(t *testing.T) {
		h := &compatHarness{}
		h.ev.a = compatBoot{ready: true}
		h.ev.dirty = "after long: discard: boom"
		if err := h.renameInto(context.Background()); err != nil {
			t.Fatalf("renameInto returned an error, want evidence recorded instead: %v", err)
		}
		if !strings.Contains(h.ev.rename.skipped, "an earlier draft could not be cleared") {
			t.Errorf("skipped = %q", h.ev.rename.skipped)
		}
	})

	t.Run("no per-process record to locate the transcript from", func(t *testing.T) {
		h := &compatHarness{}
		h.ev.a = compatBoot{ready: true} // rec is nil
		if err := h.renameInto(context.Background()); err != nil {
			t.Fatalf("renameInto returned an error, want evidence recorded instead: %v", err)
		}
		if !strings.Contains(h.ev.rename.skipped, "no per-process record was found") {
			t.Errorf("skipped = %q", h.ev.rename.skipped)
		}
	})
}
