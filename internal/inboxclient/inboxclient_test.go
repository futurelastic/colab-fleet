package inboxclient

import (
	"bufio"
	"encoding/json"
	"errors"
	"net"
	"strings"
	"testing"
	"time"
)

// serverGot is what a fake server observed a Deliver call send it — used to
// assert the auth/message lines were shaped as this package promises,
// without either side knowing a real socket path or a real token format.
type serverGot struct {
	auth authLine
	msg  messageLine
}

// runFakeServer reads exactly the two lines Deliver is documented to send,
// hands them to onRequest, and writes back whatever raw line onRequest
// returns (already newline-terminated or not — the test controls that, so a
// malformed-response case can be expressed directly). Runs in its own
// goroutine because net.Pipe is synchronous: a write on one end blocks until
// the other end reads.
func runFakeServer(t *testing.T, server net.Conn, onRequest func(serverGot) string) {
	t.Helper()
	go func() {
		defer server.Close()
		reader := bufio.NewReader(server)
		authRaw, err := reader.ReadString('\n')
		if err != nil {
			return
		}
		var got serverGot
		if jerr := json.Unmarshal([]byte(strings.TrimSpace(authRaw)), &got.auth); jerr != nil {
			return
		}
		msgRaw, err := reader.ReadString('\n')
		if err != nil {
			return
		}
		if jerr := json.Unmarshal([]byte(strings.TrimSpace(msgRaw)), &got.msg); jerr != nil {
			return
		}
		reply := onRequest(got)
		if reply == "" {
			return // simulate a close with no response line
		}
		_, _ = server.Write([]byte(reply))
	}()
}

func TestDeliver_SendsAuthThenMessage_ParsesReceipt(t *testing.T) {
	client, server := net.Pipe()
	defer client.Close()

	var gotReq serverGot
	runFakeServer(t, server, func(got serverGot) string {
		gotReq = got
		return `{"outcome":"delivered"}` + "\n"
	})

	receipt, err := Deliver(client, "tok-1", "hello", time.Second)
	if err != nil {
		t.Fatalf("Deliver: %v", err)
	}
	if receipt.Outcome != OutcomeDelivered {
		t.Fatalf("outcome = %q, want delivered", receipt.Outcome)
	}
	if gotReq.auth.Token != "tok-1" {
		t.Fatalf("server saw token %q, want tok-1", gotReq.auth.Token)
	}
	if gotReq.msg.Text != "hello" {
		t.Fatalf("server saw text %q, want hello", gotReq.msg.Text)
	}
}

func TestDeliver_EveryOutcomeSurfacesDistinctly(t *testing.T) {
	// #119's own requirement: the six-value vocabulary must reach a caller
	// unflattened. Exercised here at the protocol layer — one for one, not
	// collapsed into a subset.
	cases := []Outcome{OutcomeDelivered, OutcomeHeld, OutcomeDenied, OutcomeExpired, OutcomeRefused, OutcomeDropped}
	for _, want := range cases {
		t.Run(string(want), func(t *testing.T) {
			client, server := net.Pipe()
			defer client.Close()
			runFakeServer(t, server, func(serverGot) string {
				return `{"outcome":"` + string(want) + `","reason":"because"}` + "\n"
			})
			receipt, err := Deliver(client, "tok", "text", time.Second)
			if err != nil {
				t.Fatalf("Deliver: %v", err)
			}
			if receipt.Outcome != want {
				t.Fatalf("outcome = %q, want %q", receipt.Outcome, want)
			}
			if receipt.Reason != "because" {
				t.Fatalf("reason = %q, want %q", receipt.Reason, "because")
			}
		})
	}
}

func TestDeliver_UnrecognisedOutcome_IsAnError(t *testing.T) {
	client, server := net.Pipe()
	defer client.Close()
	runFakeServer(t, server, func(serverGot) string {
		return `{"outcome":"something-new"}` + "\n"
	})
	if _, err := Deliver(client, "tok", "text", time.Second); err == nil {
		t.Fatal("expected an error for an outcome outside the closed set, got nil")
	}
}

func TestDeliver_NoResponseBeforeClose_IsErrNoReceipt(t *testing.T) {
	client, server := net.Pipe()
	defer client.Close()
	runFakeServer(t, server, func(serverGot) string {
		return "" // server closes without ever answering
	})
	_, err := Deliver(client, "tok", "text", time.Second)
	if !errors.Is(err, ErrNoReceipt) {
		t.Fatalf("err = %v, want wrapping ErrNoReceipt", err)
	}
}

func TestDeliver_DeadlineExceeded_ReturnsError(t *testing.T) {
	client, server := net.Pipe()
	defer client.Close()
	defer server.Close()
	// No fake server reads anything: Deliver's own write blocks on
	// net.Pipe's synchronous semantics until the deadline fires — this
	// case is exercised at the write step, not the read step, but the
	// caller-facing contract is the same either way: an error, never a
	// receipt guessed from silence.
	_, err := Deliver(client, "tok", "text", 50*time.Millisecond)
	if err == nil {
		t.Fatal("expected a deadline error, got nil")
	}
}

func TestDeliver_MalformedReceiptLine_IsAnError(t *testing.T) {
	client, server := net.Pipe()
	defer client.Close()
	runFakeServer(t, server, func(serverGot) string {
		return "not json at all\n"
	})
	if _, err := Deliver(client, "tok", "text", time.Second); err == nil {
		t.Fatal("expected an error for an unparseable receipt line, got nil")
	}
}
