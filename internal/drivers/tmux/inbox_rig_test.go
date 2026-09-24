package tmux

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	fleet "github.com/godx-jp/colab-fleet"
	"github.com/godx-jp/colab-fleet/internal/inboxclient"
)

// A test window short enough that a delivery nothing confirms costs a fraction
// of a second, and long enough that a receiver appending in its own goroutine
// is never missed.
const rigWindow = 400 * time.Millisecond

// receiverMode is what a fake receiving runtime does with a message it reads
// off its inbox socket.
type receiverMode int

const (
	// receiverRecordsEnvelope writes the message into the transcript exactly as
	// received, in a user entry the runtime marks as a peer's.
	receiverRecordsEnvelope receiverMode = iota
	// receiverRecordsBody writes only the text inside the envelope — a runtime
	// that records the message without its wrapper.
	receiverRecordsBody
	// receiverSilent reads the message and records nothing: held, dropped, or
	// simply not yet written.
	receiverSilent
)

// inboxReceiver models the receiving side of an inbox delivery for one session:
// a transcript file the runtime appends to, and a socket that reads what a
// sender writes. It records every byte it is given so a test can say exactly
// what reached the wire, and how many messages that amounted to.
type inboxReceiver struct {
	t          *testing.T
	root       string
	transcript string

	mu           sync.Mutex
	messages     []string // the content of every message line received
	lines        []string // every raw line received, auth included
	partialBytes int      // bytes a budgetDialer connection accepted
	dials        int
}

// newInboxReceiver lays down the session's conversation record so the driver
// can locate its transcript, exactly as conversation_test.go does.
func newInboxReceiver(t *testing.T) *inboxReceiver {
	t.Helper()
	root := t.TempDir()
	writeRecord(t, root, "/work/alpha", "rec-alpha", "alpha💬", sessionStart.Add(4*time.Second))
	return &inboxReceiver{
		t:          t,
		root:       root,
		transcript: filepath.Join(root, recordDirFor("/work/alpha"), "rec-alpha.jsonl"),
	}
}

// option points a driver at this receiver's record store and shortens the wait.
func (r *inboxReceiver) options() []Option {
	return []Option{WithRecordRoot(r.root), withInboxConfirmWindow(rigWindow)}
}

// append writes one transcript entry, the way the runtime would.
func (r *inboxReceiver) append(entry map[string]any) {
	r.t.Helper()
	raw, err := json.Marshal(entry)
	if err != nil {
		r.t.Fatal(err)
	}
	f, err := os.OpenFile(r.transcript, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		r.t.Error(err)
		return
	}
	defer f.Close()
	if _, err := f.Write(append(raw, '\n')); err != nil {
		r.t.Error(err)
	}
}

// recordPeerMessage appends a user entry marked as a peer's.
func (r *inboxReceiver) recordPeerMessage(content string) {
	r.append(map[string]any{
		"type": "user", "sessionId": "rec-alpha",
		"origin":  map[string]any{"kind": "peer"},
		"message": map[string]any{"role": "user", "content": content},
	})
}

// dialer returns a dial function whose far end behaves as mode says.
func (r *inboxReceiver) dialer(mode receiverMode) inboxDialFunc {
	return r.dialerFunc(func(content string) {
		switch mode {
		case receiverRecordsEnvelope:
			r.recordPeerMessage(content)
		case receiverRecordsBody:
			r.recordPeerMessage(innerBody(content))
		}
	})
}

// dialerFunc is dialer with the receiver's reaction supplied by the test.
func (r *inboxReceiver) dialerFunc(onMessage func(content string)) inboxDialFunc {
	return func(ctx context.Context, network, address string) (net.Conn, error) {
		r.mu.Lock()
		r.dials++
		r.mu.Unlock()
		client, server := net.Pipe()
		go func() {
			defer server.Close()
			reader := bufio.NewReader(server)
			for i := 0; i < 2; i++ {
				line, err := reader.ReadString('\n')
				if line != "" {
					r.mu.Lock()
					r.lines = append(r.lines, line)
					r.mu.Unlock()
				}
				if err != nil {
					return
				}
				if i == 1 {
					var msg struct {
						Message struct {
							Content string `json:"content"`
						} `json:"message"`
					}
					if json.Unmarshal([]byte(line), &msg) == nil {
						r.mu.Lock()
						r.messages = append(r.messages, msg.Message.Content)
						r.mu.Unlock()
						onMessage(msg.Message.Content)
					}
				}
			}
		}()
		return client, nil
	}
}

// dialCount is how many connections were opened to the receiver.
func (r *inboxReceiver) dialCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.dials
}

// budgetDialer returns a dial function whose connection accepts at most budget
// bytes and then fails every Write — a connection that breaks part-way through
// a line. Whatever it accepted is counted in bytesReceived, on the WRITER's side
// and before Write returns, so a test that reads the count after Send returns
// can never race a goroutine that has not yet recorded a read.
func (r *inboxReceiver) budgetDialer(budget int) inboxDialFunc {
	return func(ctx context.Context, network, address string) (net.Conn, error) {
		r.mu.Lock()
		r.dials++
		r.mu.Unlock()
		client, server := net.Pipe()
		go func() {
			defer server.Close()
			_, _ = io.Copy(io.Discard, server)
		}()
		return &budgetPipeConn{Conn: client, budget: budget, rcv: r}, nil
	}
}

// budgetPipeConn forwards up to budget bytes and then breaks.
type budgetPipeConn struct {
	net.Conn
	budget int
	rcv    *inboxReceiver
}

func (b *budgetPipeConn) forward(p []byte) (int, error) {
	n, err := b.Conn.Write(p)
	b.rcv.mu.Lock()
	b.rcv.partialBytes += n
	b.rcv.mu.Unlock()
	return n, err
}

func (b *budgetPipeConn) Write(p []byte) (int, error) {
	if b.budget >= len(p) {
		b.budget -= len(p)
		return b.forward(p)
	}
	n := b.budget
	b.budget = 0
	if n > 0 {
		if _, err := b.forward(p[:n]); err != nil {
			return 0, err
		}
	}
	return n, errors.New("broken pipe")
}

// received is how many message lines the receiver was given.
func (r *inboxReceiver) received() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.messages)
}

// bytesReceived is the total the receiver was given, auth line included.
func (r *inboxReceiver) bytesReceived() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := r.partialBytes
	for _, l := range r.lines {
		n += len(l)
	}
	return n
}

// innerBody is the text between an envelope's opening tag line and its closing
// tag.
func innerBody(envelope string) string {
	if i := strings.Index(envelope, ">\n"); i >= 0 {
		envelope = envelope[i+2:]
	}
	return strings.TrimSuffix(envelope, "\n"+inboxEnvelopeClose)
}

// attestableResolver is a resolver that names a class, so the inbox path is
// available.
func attestableResolver(class inboxclient.ModeClass) InboxResolver {
	return func(context.Context, ProcessIdentity) (InboxAddress, bool, error) {
		return InboxAddress{Network: "unix", Socket: "/irrelevant", Token: "tok", ModeClass: class}, true, nil
	}
}

// newRoutedDriver builds a driver over twoSessions with an inbox resolver, the
// receiver's transcript store, and the given dial function, and returns the
// fake multiplexer so a test can see what reached the pane.
func newRoutedDriver(t *testing.T, r *inboxReceiver, resolver InboxResolver, dial inboxDialFunc, extra ...Option) (*Driver, *fakeMux) {
	t.Helper()
	f := twoSessions()
	ps := &fakePS{}
	ps.set(100, time.Now())
	ps.set(200, time.Now())
	d := newInboxTestDriverWith(f, ps, resolver, dial, append(r.options(), extra...)...)
	return d, f
}

// paneTouched reports whether anything was delivered to a pane: a paste, or the
// buffer load that precedes one.
func paneTouched(f *fakeMux) bool {
	for _, c := range f.callsSnapshot() {
		if len(c) > 0 && (c[0] == "paste-buffer" || c[0] == "load-buffer") {
			return true
		}
	}
	return false
}

// alphaRef is the session every route test sends to.
var alphaRef = fleet.SessionRef{Machine: "testbox", ID: "alpha💬"}

// deliveries is how many times the message reached a receiver by ANY path: a
// message line on the inbox socket, or a paste into a pane.
func deliveries(r *inboxReceiver, f *fakeMux) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return r.received() + len(f.pasteLog)
}

// pasteLogLen is how many payloads reached a pane.
func (f *fakeMux) pasteLogLen() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.pasteLog)
}
