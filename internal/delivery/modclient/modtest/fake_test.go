package modtest

import (
	"bufio"
	"context"
	"io"
	"strings"
	"testing"
	"time"
)

// serveHarness runs f.Serve over a pair of pipes and collects every line the
// fake writes.
type serveHarness struct {
	t      *testing.T
	in     *io.PipeWriter
	out    *io.PipeWriter
	cancel context.CancelFunc
	lines  chan string
	done   chan error
}

func startServe(t *testing.T, f *Fake) *serveHarness {
	t.Helper()
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	ctx, cancel := context.WithCancel(context.Background())
	h := &serveHarness{t: t, in: inW, out: outW, cancel: cancel, lines: make(chan string, 16), done: make(chan error, 1)}
	go func() {
		sc := bufio.NewScanner(outR)
		for sc.Scan() {
			h.lines <- sc.Text()
		}
		close(h.lines)
	}()
	go func() { h.done <- f.Serve(ctx, inR, outW) }()
	t.Cleanup(cancel)
	return h
}

func (h *serveHarness) send(line string) {
	h.t.Helper()
	if _, err := io.WriteString(h.in, line+"\n"); err != nil {
		h.t.Fatal(err)
	}
}

// finish ends the input, waits for Serve to return (which waits for every
// request goroutine), and returns every line written, in order.
func (h *serveHarness) finish() []string {
	h.t.Helper()
	_ = h.in.Close()
	select {
	case <-h.done:
	case <-time.After(5 * time.Second):
		h.t.Fatal("Serve did not return")
	}
	_ = h.out.Close()
	var all []string
	for l := range h.lines {
		all = append(all, l)
	}
	return all
}

// A handler that a module's death releases (the usual way to hold a request
// until the process dies) returns a success. That success must never reach the
// wire: the incarnation has ended, and a client reading it would see an answer
// from a dead process instead of the exit it is supposed to observe.
func TestFake_EndedIncarnationAnswersNothing(t *testing.T) {
	f := NewFake(Behaviour{})
	inside := make(chan struct{})
	f.SetHandler(func(q Request) (any, *WireError, time.Duration) {
		close(inside)
		<-q.Ctx.Done()
		return map[string]any{"laneKey": "k", "live": true, "state": "live", "sinceMs": 1}, nil, 0
	})
	h := startServe(t, f)
	h.send(`{"id":"1","op":"attach","args":{"laneKey":"k"}}`)
	select {
	case <-inside:
	case <-time.After(3 * time.Second):
		t.Fatal("the request never reached the handler")
	}

	h.cancel() // the incarnation ends; the handler is released with a success to return
	lines := h.finish()

	if len(lines) != 1 || !strings.Contains(lines[0], `"event":"hello"`) {
		t.Fatalf("lines written = %q, want only the hello: an ended incarnation must answer nothing", lines)
	}
}

// The positive control for the test above: the same fake, not ended, still
// answers. Without it the silence above would prove nothing.
func TestFake_LiveIncarnationStillAnswers(t *testing.T) {
	f := NewFake(Behaviour{})
	h := startServe(t, f)
	h.send(`{"id":"1","op":"attach","args":{"laneKey":"k"}}`)

	hello := <-h.lines
	if !strings.Contains(hello, `"event":"hello"`) {
		t.Fatalf("first line = %q, want the hello", hello)
	}
	select {
	case resp := <-h.lines:
		if !strings.Contains(resp, `"id":"1"`) || !strings.Contains(resp, `"ok":true`) {
			t.Fatalf("response = %q, want an ok answer to request 1", resp)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("a live incarnation did not answer")
	}
	if rest := h.finish(); len(rest) != 0 {
		t.Errorf("extra lines after the answer: %q", rest)
	}
}
