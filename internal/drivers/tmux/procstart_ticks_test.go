package tmux

import (
	"errors"
	"strings"
	"testing"
	"time"
)

// pinBootTime replaces linuxBootTime for one test, so the tick-shaped procStart
// is converted against a known boot instant instead of this machine's /proc.
func pinBootTime(t *testing.T, boot time.Time, err error) {
	t.Helper()
	prev := linuxBootTime
	linuxBootTime = func() (time.Time, error) { return boot, err }
	t.Cleanup(func() { linuxBootTime = prev })
}

// The Linux shape of procStart: /proc/<pid>/stat's starttime, in USER_HZ ticks
// since boot. The fixture is the measured pair — "343765263" ticks against a
// boot instant, which `ps -o lstart=` rendered as boot + 3437652 s — so the
// sub-second remainder (63 ticks) must truncate, as procps truncates it.
func TestParseProcessSessionRecordStartTime_LinuxTicks(t *testing.T) {
	boot := time.Unix(1787247685, 0)
	pinBootTime(t, boot, nil)

	got, err := parseProcessSessionRecordStartTime("343765263")
	if err != nil {
		t.Fatalf("tick-shaped procStart did not parse: %v", err)
	}
	if want := boot.Add(3437652 * time.Second); !got.Equal(want) {
		t.Errorf("parsed %s, want %s (boot + whole seconds of ticks)", got.UTC(), want.UTC())
	}
}

// The textual shape is unchanged: still read as UTC, never as local time.
func TestParseProcessSessionRecordStartTime_TextStillUTC(t *testing.T) {
	pinBootTime(t, time.Time{}, errors.New("must not be consulted for text"))

	got, err := parseProcessSessionRecordStartTime("Mon Jan  2 15:04:05 2026")
	if err != nil {
		t.Fatalf("text procStart did not parse: %v", err)
	}
	if want := time.Date(2026, time.January, 2, 15, 4, 5, 0, time.UTC); !got.Equal(want) {
		t.Errorf("parsed %s, want %s", got, want)
	}
}

// Without a boot time (off Linux, or /proc unreadable) a tick count cannot be
// placed in time, so it stays a parse failure — the behaviour before this
// shape was understood, and never a guessed instant.
func TestParseProcessSessionRecordStartTime_TicksWithoutBootTime(t *testing.T) {
	pinBootTime(t, time.Time{}, errors.New("no /proc/stat"))

	if got, err := parseProcessSessionRecordStartTime("343765263"); err == nil {
		t.Fatalf("parsed %s with no boot time, want an error", got)
	} else if !strings.Contains(err.Error(), "tick count") {
		t.Errorf("error %q does not say the value was a tick count", err)
	}
}

// End to end through the check that consumes it: a tick-shaped record
// corroborates the live process whose `ps` start time matches, and a record
// from a different process generation under the same pid still does not.
func TestCorroborateProcessRecord_LinuxTicks(t *testing.T) {
	boot := time.Unix(1787247685, 0)
	pinBootTime(t, boot, nil)
	live := ProcessIdentity{PID: 24080, StartedAt: boot.Add(3437652 * time.Second)}

	rec := processSessionRecord{SessionID: "44444444-4444-4444-8444-444444444444", CWD: "/work/a", ProcStart: "343765263"}
	got, why := corroborateProcessRecord(rec, live)
	if why != "" {
		t.Fatalf("tick-shaped record for the running process was refused: %s", why)
	}
	if got.sessionID != rec.SessionID {
		t.Errorf("sessionID = %q, want %q", got.sessionID, rec.SessionID)
	}

	recycled := rec
	recycled.ProcStart = "343700000" // same pid, an earlier process generation
	if _, why := corroborateProcessRecord(recycled, live); !strings.Contains(why, "different time") {
		t.Errorf("a record from an earlier process under the same pid was not refused as recycled: %q", why)
	}
}
