package inboxclient

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
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

// budgetConn accepts at most budget bytes and then fails every Write, the way a
// connection that breaks part-way through a line does. Only Write and
// SetWriteDeadline are exercised by Deliver.
type budgetConn struct {
	net.Conn
	budget      int
	got         []byte
	deadlineErr error
}

func (b *budgetConn) SetWriteDeadline(time.Time) error { return b.deadlineErr }

func (b *budgetConn) Write(p []byte) (int, error) {
	if b.budget >= len(p) {
		b.budget -= len(p)
		b.got = append(b.got, p...)
		return len(p), nil
	}
	n := b.budget
	b.got = append(b.got, p[:n]...)
	b.budget = 0
	return n, errors.New("broken pipe")
}

// #184: the fallback rule rests on this count. Every failure point must report
// exactly the bytes the connection accepted, and NothingWritten must be true
// only when that is zero.
func TestDeliver_WriteError_ReportsBytesAccepted(t *testing.T) {
	// Learn the real frame so the budgets below land at meaningful places.
	var full bytes.Buffer
	enc := json.NewEncoder(&full)
	_ = enc.Encode(authLine{Type: "auth", Token: "tok"})
	authLen := full.Len()
	_ = enc.Encode(messageLine{Type: "user", Message: messageLineBody{Role: "user", Content: "hello"}})
	total := full.Len()

	for _, tc := range []struct {
		name        string
		budget      int
		wantWritten int
		wantNothing bool
	}{
		{"nothing accepted", 0, 0, true},
		{"one byte of the auth line", 1, 1, false},
		{"whole auth line, none of the message", authLen, authLen, false},
		{"auth line and five bytes of the message", authLen + 5, authLen + 5, false},
		{"everything but the final newline", total - 1, total - 1, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			conn := &budgetConn{budget: tc.budget}
			_, err := Deliver(conn, "tok", "hello", time.Second)
			var we *WriteError
			if !errors.As(err, &we) {
				t.Fatalf("err = %v (%T), want a *WriteError", err, err)
			}
			if we.Written != tc.wantWritten {
				t.Errorf("Written = %d, want %d", we.Written, tc.wantWritten)
			}
			if got := NothingWritten(err); got != tc.wantNothing {
				t.Errorf("NothingWritten = %v, want %v", got, tc.wantNothing)
			}
			if len(conn.got) != tc.wantWritten {
				t.Errorf("connection actually accepted %d bytes, error claims %d", len(conn.got), tc.wantWritten)
			}
		})
	}

	// And a complete write is not an error at all, so there is nothing to
	// misread as "nothing written".
	conn := &budgetConn{budget: total}
	if _, err := Deliver(conn, "tok", "hello", time.Second); err != nil {
		t.Fatalf("a connection that accepts the whole frame failed: %v", err)
	}
	if !bytes.Equal(conn.got, full.Bytes()) {
		t.Errorf("wire bytes changed:\n got %q\nwant %q", conn.got, full.Bytes())
	}
}

func TestDeliver_DeadlineFailureIsNothingWritten(t *testing.T) {
	conn := &budgetConn{budget: 1 << 20, deadlineErr: errors.New("deadline unsupported")}
	_, err := Deliver(conn, "tok", "hello", time.Second)
	if !NothingWritten(err) {
		t.Fatalf("a failure before any write must report NothingWritten, got %v", err)
	}
	if len(conn.got) != 0 {
		t.Fatalf("Deliver wrote %d bytes after its deadline setup failed", len(conn.got))
	}
}

func TestNothingWritten_UntypedErrorProvesNothing(t *testing.T) {
	if NothingWritten(errors.New("something else")) {
		t.Fatal("an error that is not a *WriteError says nothing about bytes; it must not read as 'unsent'")
	}
	if NothingWritten(nil) {
		t.Fatal("nil is not a failed delivery")
	}
}
