package modclient_test

import (
	"bufio"
	"bytes"
	"io"
	"runtime"
	"strings"
	"testing"

	"github.com/godx-jp/colab-fleet/internal/delivery/modclient"
)

// repeat is an io.Reader of n copies of b that holds none of them in memory.
type repeat struct {
	b byte
	n int
}

func (r *repeat) Read(p []byte) (int, error) {
	if r.n == 0 {
		return 0, io.EOF
	}
	n := len(p)
	if n > r.n {
		n = r.n
	}
	for i := 0; i < n; i++ {
		p[i] = r.b
	}
	r.n -= n
	return n, nil
}

func TestReadLine_OversizeDiscardedAndReaderResyncs(t *testing.T) {
	src := io.MultiReader(
		strings.NewReader("first\n"),
		&repeat{b: 'x', n: modclient.MaxLineBytes + 1<<20}, // one line, over the limit
		strings.NewReader("\nsecond\r\n"),
		&repeat{b: 'y', n: 3 * modclient.MaxLineBytes}, // a second, larger one
		strings.NewReader("\nthird\n"),
	)
	br := bufio.NewReaderSize(src, 4096)

	want := []struct {
		line     string
		oversize bool
	}{{"first", false}, {"", true}, {"second", false}, {"", true}, {"third", false}}
	for i, w := range want {
		line, over, err := modclient.ReadLine(br)
		if err != nil {
			t.Fatalf("read %d: %v", i, err)
		}
		if over != w.oversize || string(line) != w.line {
			t.Fatalf("read %d = (%q, oversize=%v), want (%q, %v)", i, clip(line), over, w.line, w.oversize)
		}
		if over && line != nil {
			t.Errorf("read %d: an oversize line must not be returned", i)
		}
	}
	if _, _, err := modclient.ReadLine(br); err != io.EOF {
		t.Fatalf("after the last line: %v, want io.EOF", err)
	}
}

func clip(b []byte) string {
	if len(b) > 40 {
		return string(b[:40]) + "..."
	}
	return string(b)
}

// TestReadLine_OversizeIsNotBuffered shows the drain is bounded: a line 16 times
// the limit must not cost anything like 64 times the limit in allocations.
func TestReadLine_OversizeIsNotBuffered(t *testing.T) {
	br := bufio.NewReaderSize(io.MultiReader(&repeat{b: 'z', n: 16 * modclient.MaxLineBytes}, strings.NewReader("\nok\n")), 4096)
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	_, over, err := modclient.ReadLine(br)
	runtime.ReadMemStats(&after)
	if err != nil || !over {
		t.Fatalf("got oversize=%v err=%v", over, err)
	}
	if got, limit := after.TotalAlloc-before.TotalAlloc, uint64(6*modclient.MaxLineBytes); got > limit {
		t.Errorf("draining a %d MiB line allocated %d MiB (limit %d MiB): it is being buffered",
			16*modclient.MaxLineBytes>>20, got>>20, limit>>20)
	}
	if line, _, err := modclient.ReadLine(br); err != nil || string(line) != "ok" {
		t.Errorf("did not resync: %q %v", line, err)
	}
}

func TestReadLine_ExactLimitIsAccepted(t *testing.T) {
	body := bytes.Repeat([]byte("a"), modclient.MaxLineBytes)
	br := bufio.NewReader(bytes.NewReader(append(append([]byte{}, body...), '\n', 'b', '\n')))
	line, over, err := modclient.ReadLine(br)
	if err != nil || over || len(line) != modclient.MaxLineBytes {
		t.Fatalf("a line of exactly MaxLineBytes: len=%d oversize=%v err=%v", len(line), over, err)
	}
	br = bufio.NewReader(bytes.NewReader(append(append([]byte{}, body...), 'a', '\n')))
	if _, over, _ := modclient.ReadLine(br); !over {
		t.Fatal("a line one byte over the limit must be oversize")
	}
}

func TestReadLine_TornFinalLineIsDiscarded(t *testing.T) {
	br := bufio.NewReader(strings.NewReader("whole\npartial"))
	if line, _, err := modclient.ReadLine(br); err != nil || string(line) != "whole" {
		t.Fatalf("first: %q %v", line, err)
	}
	if line, over, err := modclient.ReadLine(br); err != io.EOF || line != nil || over {
		t.Fatalf("a torn final line must surface only io.EOF, got (%q, %v, %v)", line, over, err)
	}
}

func TestReadLine_EmptyLinesAreLines(t *testing.T) {
	br := bufio.NewReader(strings.NewReader("\n\nx\n"))
	for _, want := range []string{"", "", "x"} {
		line, _, err := modclient.ReadLine(br)
		if err != nil || string(line) != want {
			t.Fatalf("got (%q, %v), want %q", line, err, want)
		}
	}
}

func TestReadLine_ReturnedLineIsNotAliased(t *testing.T) {
	br := bufio.NewReaderSize(strings.NewReader("one\ntwo\n"), 16)
	first, _, _ := modclient.ReadLine(br)
	_, _, _ = modclient.ReadLine(br)
	if string(first) != "one" {
		t.Errorf("the first line was overwritten by the next read: %q", first)
	}
}
