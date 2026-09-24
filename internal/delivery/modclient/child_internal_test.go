package modclient

import (
	"errors"
	"testing"
	"time"
)

// outcomeOf returns the call's outcome, failing rather than hanging when there
// is none: a call nobody completed must be a failed test, not a stuck one.
func outcomeOf(t *testing.T, cl *call) outcome {
	t.Helper()
	select {
	case o := <-cl.out:
		return o
	case <-time.After(3 * time.Second):
		t.Fatal("the call was never completed")
		return outcome{}
	}
}

// A request in flight when the child dies is lost, and it stays lost: an answer
// that shows up for it afterwards must not turn it into a success. The exit path
// takes every pending call out of the table before failing it, so a late
// response finds nothing to resolve.
func TestChild_LateResponseCannotResolveACallTheExitPathFailed(t *testing.T) {
	ch := &child{c: &Client{}, pending: map[string]*call{}}
	cl := &call{id: "7", out: make(chan outcome, 1)}
	cl.state.Store(csSent) // written, so its fate is unknown
	if !ch.register(cl) {
		t.Fatal("register refused a call on a child that is still open")
	}

	ch.failPending("the module process ended")
	if n := len(ch.pending); n != 0 {
		t.Fatalf("%d calls still in the pending table after the exit path ran: a late answer could find them", n)
	}

	// The child's output still carries an answer to that very request.
	if !ch.dispatch([]byte(`{"id":"7","ok":true,"result":{"laneKey":"late"}}`)) {
		t.Fatal("a well-formed envelope must be reported as a response, however late")
	}

	o := outcomeOf(t, cl)
	if !errors.Is(o.err, ErrLost) || errors.Is(o.err, ErrNotSent) {
		t.Fatalf("outcome = %+v, want ErrLost: the late answer resolved a call the exit path had failed", o)
	}
	select {
	case extra := <-cl.out:
		t.Fatalf("the call was completed twice; second outcome %+v", extra)
	default:
	}
	if ch.register(&call{id: "8", out: make(chan outcome, 1)}) {
		t.Error("a call registered after the exit path ran would be stranded: register must refuse")
	}
}

// The other order: an answer that was already delivered stays the answer. The
// exit path finds the call gone from the table and has nothing to fail.
func TestChild_ExitPathDoesNotOverwriteAnAnswerAlreadyDelivered(t *testing.T) {
	ch := &child{c: &Client{}, pending: map[string]*call{}}
	cl := &call{id: "7", out: make(chan outcome, 1)}
	cl.state.Store(csSent)
	if !ch.register(cl) {
		t.Fatal("register refused a call on a child that is still open")
	}

	if !ch.dispatch([]byte(`{"id":"7","ok":true,"result":{"laneKey":"k"}}`)) {
		t.Fatal("a well-formed envelope must be reported as a response")
	}
	ch.failPending("the module process ended")

	o := outcomeOf(t, cl)
	if o.err != nil || o.env.OK == nil || !*o.env.OK {
		t.Fatalf("outcome = %+v, want the answer that arrived before the exit", o)
	}
	select {
	case extra := <-cl.out:
		t.Fatalf("the call was completed twice; second outcome %+v", extra)
	default:
	}
}
