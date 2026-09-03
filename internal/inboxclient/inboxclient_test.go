package inboxclient

import (
	"bufio"
	"encoding/json"
	"net"
	"path/filepath"
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

// runFakeServer reads exactly the two lines Deliver is documented to send
// and hands them to onRequest. It never writes anything back — #143
// measured that a real inbox does not, even on a delivery that fully
// succeeds, and #144 is what stopped Deliver waiting for something that
// never arrives. Runs in its own goroutine because net.Pipe is synchronous:
// a write on one end blocks until the other end reads.
func runFakeServer(t *testing.T, server net.Conn, onRequest func(serverGot)) {
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
		onRequest(got)
	}()
}

func TestDeliver_SendsAuthThenMessage_ReportsDelivered(t *testing.T) {
	client, server := net.Pipe()
	defer client.Close()

	gotCh := make(chan serverGot, 1)
	runFakeServer(t, server, func(got serverGot) { gotCh <- got })

	receipt, err := Deliver(client, "tok-1", "hello", time.Second)
	if err != nil {
		t.Fatalf("Deliver: %v", err)
	}
	if receipt.Outcome != OutcomeDelivered {
		t.Fatalf("outcome = %q, want delivered", receipt.Outcome)
	}

	select {
	case got := <-gotCh:
		if got.auth.Type != "auth" {
			t.Errorf("auth.Type = %q, want %q", got.auth.Type, "auth")
		}
		if got.auth.Token != "tok-1" {
			t.Errorf("auth.Token = %q, want %q", got.auth.Token, "tok-1")
		}
		if got.msg.Type != "user" {
			t.Errorf("msg.Type = %q, want %q", got.msg.Type, "user")
		}
		if got.msg.Message.Role != "user" {
			t.Errorf("msg.Message.Role = %q, want %q", got.msg.Message.Role, "user")
		}
		if got.msg.Message.Content != "hello" {
			t.Errorf("msg.Message.Content = %q, want %q", got.msg.Message.Content, "hello")
		}
	case <-time.After(time.Second):
		t.Fatal("fake server never observed a complete request")
	}
}

func TestDeliver_DoesNotWaitForAResponse(t *testing.T) {
	// #144's own regression case: the old contract read one response line
	// and blocked for the full timeout when nothing arrived. #143 measured
	// that nothing arrives even on success, so Deliver must return well
	// before timeout once both lines are written.
	client, server := net.Pipe()
	defer client.Close()
	runFakeServer(t, server, func(serverGot) {})

	const budget = 2 * time.Second
	start := time.Now()
	receipt, err := Deliver(client, "tok", "hello", budget)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("Deliver: %v", err)
	}
	if receipt.Outcome != OutcomeDelivered {
		t.Fatalf("outcome = %q, want delivered", receipt.Outcome)
	}
	if elapsed >= budget {
		t.Fatalf("Deliver took %s, at or beyond its own %s budget — it waited on a response line that #144 says never arrives", elapsed, budget)
	}
}

func TestDeliver_WriteDeadlineExceeded_ReturnsError(t *testing.T) {
	client, server := net.Pipe()
	defer client.Close()
	defer server.Close()
	// No fake server reads anything: Deliver's own write blocks on
	// net.Pipe's synchronous semantics until the deadline fires.
	_, err := Deliver(client, "tok", "text", 50*time.Millisecond)
	if err == nil {
		t.Fatal("expected a deadline error, got nil")
	}
}

func TestDeliver_ConnectionClosedBeforeAnyRead_ReturnsError(t *testing.T) {
	client, server := net.Pipe()
	defer client.Close()
	server.Close() // closed before Deliver writes anything

	if _, err := Deliver(client, "tok", "text", time.Second); err == nil {
		t.Fatal("expected an error writing to an already-closed connection, got nil")
	}
}

// TestDeliver_OverARealSocket exercises Deliver against an actual unix
// domain socket rather than an in-memory net.Pipe — colab-fleet #144's own
// requirement, filed because this subsystem shipped broken twice
// (#122: the resolver was never wired; #143: this package's framing was
// never validated against anything real) and every test passed both times,
// because every test ran over net.Pipe against this package's own
// assumptions. This does not reach a real inbox — that is machine-local and
// this repository is public — but it does prove Deliver works over a real
// kernel socket with real Accept/Read/Write semantics, not just a
// zero-latency in-process pipe.
func TestDeliver_OverARealSocket(t *testing.T) {
	dir := t.TempDir()
	sockPath := filepath.Join(dir, "inbox.sock")

	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()

	gotCh := make(chan serverGot, 1)
	go func() {
		conn, aerr := ln.Accept()
		if aerr != nil {
			return
		}
		runFakeServer(t, conn, func(got serverGot) { gotCh <- got })
	}()

	conn, err := net.Dial("unix", sockPath)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	receipt, err := Deliver(conn, "tok-real", "hello over a real socket", 2*time.Second)
	if err != nil {
		t.Fatalf("Deliver: %v", err)
	}
	if receipt.Outcome != OutcomeDelivered {
		t.Fatalf("outcome = %q, want delivered", receipt.Outcome)
	}

	select {
	case got := <-gotCh:
		if got.auth.Type != "auth" || got.auth.Token != "tok-real" {
			t.Errorf("auth line = %+v", got.auth)
		}
		if got.msg.Type != "user" || got.msg.Message.Role != "user" || got.msg.Message.Content != "hello over a real socket" {
			t.Errorf("message line = %+v", got.msg)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("fake server never observed a complete request over the real socket")
	}
}
