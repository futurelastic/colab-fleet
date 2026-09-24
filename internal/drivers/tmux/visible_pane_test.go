package tmux

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	fleet "github.com/godx-jp/colab-fleet"
	"github.com/godx-jp/colab-fleet/internal/driver"
)

// colab-fleet#169: a composer is read from the VISIBLE pane only. Rows above
// it came from the `-S -N` history margin — scrollback, which may be an older
// frame of the composer or, on an alternate-screen runtime, output from before
// the runtime drew anything at all.

// visibleFence is a 40-rune box-drawing rule: narrow enough never to wrap in a
// real 80-column pane, far past isRule's minimum.
var visibleFence = strings.Repeat("─", 40)

// tallComposerCapture renders a capture of `transcript` transcript rows, then a
// fenced composer holding 1+conts text rows, then its closing rule and footer.
// Every row is newline-terminated, as capture-pane prints it. fenceRow is the
// opening fence's row index.
func tallComposerCapture(transcript, conts int) (raw string, fenceRow, rows int) {
	var b strings.Builder
	row := func(s string) {
		b.WriteString(s)
		b.WriteString("\n")
		rows++
	}
	for i := 0; i < transcript; i++ {
		row(fmt.Sprintf("  transcript row %d", i))
	}
	fenceRow = rows
	row(visibleFence)
	row("❯ first line of a tall message")
	for i := 0; i < conts; i++ {
		row(fmt.Sprintf("  continuation row %d", i))
	}
	row(visibleFence)
	row("  ⏵⏵ auto mode on")
	return b.String(), fenceRow, rows
}

func TestNewScreenVisibleCountsTheBoundaryBeforeTrimming(t *testing.T) {
	var b strings.Builder
	for i := 0; i < 48; i++ {
		fmt.Fprintf(&b, "row %d\n", i)
	}
	full := b.String()
	for _, tc := range []struct {
		name   string
		raw    string
		height int
		want   int
	}{
		{"margin plus pane", full, 24, 24},
		{"no margin: capture is exactly the pane", full, 48, 0},
		{"height unknown", full, 0, 0},
		{"height larger than the capture", full, 60, 0},
		// capture-pane pads the pane with blank rows; newScreen drops them,
		// and that must not move the boundary.
		{"padded tail", full + strings.Repeat("\n", 6), 24, 30},
		{"unterminated last row", strings.TrimSuffix(full, "\n"), 24, 24},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := newScreenVisible(tc.raw, tc.height).visibleTop; got != tc.want {
				t.Errorf("visibleTop = %d, want %d", got, tc.want)
			}
		})
	}
	if s := newScreenVisible(full+strings.Repeat("\n", 6), 24); len(s.lines) != 48 {
		t.Errorf("trailing blank rows should still be dropped: %d lines, want 48", len(s.lines))
	}
}

func TestComposerSpanReadsTheComposerFromTheVisiblePaneOnly(t *testing.T) {
	raw, fence, rows := tallComposerCapture(20, 30)
	for _, tc := range []struct {
		name        string
		height      int
		want        composerScan
		onlyBecause bool
	}{
		// The pane shows the tail; fence and ❯ row are both in the margin.
		// Before #169 this read composerFound, digest and all.
		{"fence and prompt in the margin", 24, composerClipped, true},
		// The ❯ row is the first visible row, but the fence above it is not:
		// the row that settled "this is a composer" is still scrollback.
		{"only the fence in the margin", rows - fence - 1, composerClipped, true},
		{"fence is the first visible row", rows - fence, composerFound, false},
		{"whole capture visible", rows, composerFound, false},
		{"height unknown keeps the old reading", 0, composerFound, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newScreenVisible(raw, tc.height)
			if _, _, got := composerSpan(s); got != tc.want {
				t.Errorf("composerSpan = %v, want %v (visibleTop %d, fence row %d)", got, tc.want, s.visibleTop, fence)
			}
			if got := clippedOnlyAboveVisiblePane(s); got != tc.onlyBecause {
				t.Errorf("clippedOnlyAboveVisiblePane = %v, want %v", got, tc.onlyBecause)
			}
			if text, scan := composerText(s); scan != composerFound && text != "" {
				t.Errorf("a composer that is not found must yield no text, got %q", text)
			}
		})
	}
}

// A composer clipped for #134's reason — no fence anywhere in the capture — is
// not the visible-pane rule's doing, and must not be counted as such.
func TestClippedOnlyAboveVisiblePaneIgnoresAClassicClip(t *testing.T) {
	s := newScreenVisible(clippedComposerFixture()+"\n", 10)
	if _, _, scan := composerSpan(s); scan != composerClipped {
		t.Fatalf("fixture should read clipped, got %v", scan)
	}
	if clippedOnlyAboveVisiblePane(s) {
		t.Error("a composer clipped by the capture window was attributed to the visible-pane rule")
	}
}

func TestSplitCapturesReadsThePaneHeight(t *testing.T) {
	const mark = "NONCEP"
	out := "noise before the first marker\n" +
		mark + "0 24\nrow a\nrow b\n" +
		mark + "1 " + paneHeightFormat + "\nrow c\n" + // a fake that does not expand formats
		mark + "2\nrow d\n" + // the pre-#169 marker shape
		mark + "3 0\nrow e\n"
	got := splitCaptures(out, mark)
	for _, tc := range []struct {
		key    string
		text   string
		height int
	}{
		{"0", "row a\nrow b\n", 24},
		{"1", "row c\n", 0},
		{"2", "row d\n", 0},
		{"3", "row e\n", 0},
	} {
		c, ok := got[tc.key]
		if !ok {
			t.Errorf("pane %s missing from %v", tc.key, got)
			continue
		}
		if c.text != tc.text || c.height != tc.height {
			t.Errorf("pane %s = {%q, %d}, want {%q, %d}", tc.key, c.text, c.height, tc.text, tc.height)
		}
	}
}

func TestSplitHeightTrailerReadsOnlyTheLastLine(t *testing.T) {
	const mark = "NONCEH"
	for _, tc := range []struct {
		name   string
		out    string
		text   string
		height int
	}{
		{"trailer present", "a\nb\n" + mark + "24\n", "a\nb\n", 24},
		{"no trailer", "a\nb\n", "a\nb\n", 0},
		{"a pane row that looks like a trailer", mark + "99\nb\n" + mark + "24\n", mark + "99\nb\n", 24},
		{"trailer that does not parse", "a\n" + mark + paneHeightFormat + "\n", "a\n", 0},
		{"empty capture", mark + "24\n", "", 24},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := splitHeightTrailer(tc.out, mark)
			if c.text != tc.text || c.height != tc.height {
				t.Errorf("got {%q, %d}, want {%q, %d}", c.text, c.height, tc.text, tc.height)
			}
		})
	}
}

// End to end through the batched capture: the height rides in the marker, a
// read publishes no composerDigest, and discard refuses before pressing a key.
func TestDiscardRefusesAComposerWhoseFenceIsAboveTheVisiblePane(t *testing.T) {
	raw, _, _ := tallComposerCapture(20, 30)
	// The digest a pre-#169 read would have published for this composer —
	// exactly what a caller could be holding as expect.
	stalePending, scan := composerText(newScreen(raw))
	if scan != composerFound || stalePending == "" {
		t.Fatalf("fixture should read as a found, non-empty composer without a pane boundary: %v %q", scan, stalePending)
	}

	t.Run("height known", func(t *testing.T) {
		f := twoSessions()
		f.captures["%2"] = raw
		f.heights = map[string]int{"%2": 24}
		d := newTestDriver(f)

		list, err := d.List(context.Background(), testCaller, driver.ListFilter{})
		if err != nil {
			t.Fatal(err)
		}
		for _, s := range list.Items() {
			if s.ID != "beta" {
				continue
			}
			if s.State.ComposerDigest != "" {
				t.Errorf("a composer read through scrollback must publish no composerDigest, got %q", s.State.ComposerDigest)
			}
			if !strings.Contains(s.State.Evidence, "visible pane") {
				t.Errorf("evidence should say why the composer could not be read: %q", s.State.Evidence)
			}
		}

		_, err = d.Discard(context.Background(), testCaller,
			fleet.SessionRef{Machine: "testbox", ID: "beta"}, composerTextDigest(stalePending), driver.DiscardOptions{})
		if !errors.Is(err, fleet.ErrAmbiguousTarget) {
			t.Fatalf("want ErrAmbiguousTarget, got %v", err)
		}
		for _, call := range f.callsSnapshot() {
			if len(call) > 0 && call[0] == "send-keys" {
				t.Errorf("nothing may be pressed against a composer read from scrollback; saw %v", call)
			}
		}
		c := d.Counters()
		if c[counterComposerClippedRefusedDiscard] != 1 || c[counterComposerClippedAboveVisiblePane] != 1 {
			t.Errorf("counters %s=%d %s=%d, want 1 and 1",
				counterComposerClippedRefusedDiscard, c[counterComposerClippedRefusedDiscard],
				counterComposerClippedAboveVisiblePane, c[counterComposerClippedAboveVisiblePane])
		}
	})

	// The marker must actually target the captured pane: without -t the format
	// would expand against the current pane, not the one being captured.
	t.Run("marker targets its pane", func(t *testing.T) {
		f := twoSessions()
		d := newTestDriver(f)
		if _, err := d.List(context.Background(), testCaller, driver.ListFilter{}); err != nil {
			t.Fatal(err)
		}
		seen := 0
		for _, call := range f.callsSnapshot() {
			for i := 0; i+4 < len(call); i++ {
				if call[i] == "display-message" && call[i+1] == "-t" && call[i+3] == "-p" &&
					strings.HasSuffix(call[i+4], " "+paneHeightFormat) {
					seen++
				}
			}
		}
		if seen != 2 {
			t.Errorf("want 2 targeted height markers (one per pane), saw %d", seen)
		}
	})

	t.Run("height unknown keeps the old reading", func(t *testing.T) {
		f := twoSessions()
		f.captures["%2"] = raw
		d := newTestDriver(f)
		list, err := d.List(context.Background(), testCaller, driver.ListFilter{})
		if err != nil {
			t.Fatal(err)
		}
		for _, s := range list.Items() {
			if s.ID == "beta" && s.State.ComposerDigest != composerTextDigest(stalePending) {
				t.Errorf("with no height, the composer should read as before: digest %q, want %q",
					s.State.ComposerDigest, composerTextDigest(stalePending))
			}
		}
		if got := d.Counters()[counterComposerClippedAboveVisiblePane]; got != 0 {
			t.Errorf("%s = %d, want 0", counterComposerClippedAboveVisiblePane, got)
		}
	})
}

// TestLiveCaptureCarriesThePaneHeight runs both capture shapes against a REAL
// multiplexer. The fake cannot prove the parts that matter here: that
// `display-message -t <pane> -p` expands #{pane_height} for the captured pane
// inside a chained invocation, and that the rows before the last pane_height
// really are the history margin.
func TestLiveCaptureCarriesThePaneHeight(t *testing.T) {
	if os.Getenv("FLEET_TMUX_INTEGRATION") != "1" {
		t.Skip("set FLEET_TMUX_INTEGRATION=1 to run against a real multiplexer")
	}
	bin, err := exec.LookPath("tmux")
	if err != nil {
		t.Skip("no multiplexer on PATH")
	}
	dir := t.TempDir()
	socket := filepath.Join("/tmp", "fl-vispane-"+randomNonce())
	t.Cleanup(func() { _ = os.Remove(socket) })
	wrapper := filepath.Join(dir, "mux")
	script := "#!/bin/sh\nexec " + bin + " -u -S " + socket + " \"$@\"\n"
	if err := os.WriteFile(wrapper, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	tall, fence, _ := tallComposerCapture(20, 30) // 54 rows: taller than a 24-row pane
	short, _, _ := tallComposerCapture(2, 3)      // 9 rows: fits
	start := func(name, body string) string {
		file := filepath.Join(dir, name+".txt")
		if err := os.WriteFile(file, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		cmd := exec.Command(wrapper, "new-session", "-d", "-x", "80", "-y", "24", "-s", name,
			"sh", "-c", "cat '"+file+"'; exec sleep 600")
		cmd.Env = append(os.Environ(), "LANG=en_US.UTF-8", "LC_ALL=en_US.UTF-8")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Skipf("could not start a private multiplexer server: %v (%s)", err, out)
		}
		out, err := exec.Command(wrapper, "list-panes", "-t", name, "-F", "#{pane_id}").Output()
		if err != nil {
			t.Fatalf("list-panes: %v", err)
		}
		return strings.TrimSpace(string(out))
	}
	tallPane := start("vispane-tall", tall)
	shortPane := start("vispane-short", short)
	t.Cleanup(func() { _ = exec.Command(wrapper, "kill-server").Run() })

	d := New("vispane-host", WithBinary(wrapper))
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	// Wait for the tall pane's output to land.
	var tallScreen screen
	for deadline := time.Now().Add(5 * time.Second); ; {
		sc, ok := d.captureForClassify(ctx, tallPane)
		if ok && strings.Contains(strings.Join(sc.lines, "\n"), "auto mode on") {
			tallScreen = sc
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("tall pane never rendered its fixture: ok=%v lines=%q", ok, sc.lines)
		}
		time.Sleep(100 * time.Millisecond)
	}

	if tallScreen.visibleTop == 0 {
		t.Fatalf("captureForClassify did not learn the pane height: %d lines", len(tallScreen.lines))
	}
	if _, _, scan := composerSpan(tallScreen); scan != composerClipped {
		t.Errorf("tall composer (fence row %d, visibleTop %d) read %v through captureForClassify, want clipped",
			fence, tallScreen.visibleTop, scan)
	}
	if !clippedOnlyAboveVisiblePane(tallScreen) {
		t.Error("the tall composer is inside the capture margin; its clip should be attributed to the visible-pane rule")
	}

	shortScreen, ok := d.captureForClassify(ctx, shortPane)
	if !ok {
		t.Fatal("short pane capture failed")
	}
	if _, _, scan := composerSpan(shortScreen); scan != composerFound {
		t.Errorf("a composer inside the visible pane read %v, want found", scan)
	}

	_, captures, err := d.enumerate(ctx)
	if err != nil {
		t.Fatalf("enumerate: %v", err)
	}
	for _, p := range []string{tallPane, shortPane} {
		if got := captures[p].height; got != 24 {
			t.Errorf("batched capture of %s carried height %d, want 24", p, got)
		}
	}
	if _, _, scan := composerSpan(captures[tallPane].screen()); scan != composerClipped {
		t.Errorf("tall composer read %v through the batched capture, want clipped", scan)
	}
	if _, _, scan := composerSpan(captures[shortPane].screen()); scan != composerFound {
		t.Errorf("short composer read %v through the batched capture, want found", scan)
	}
}
