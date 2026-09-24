package modclient_test

import (
	"context"
	"encoding/json"
	"errors"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/godx-jp/colab-fleet/internal/delivery/modclient"
	"github.com/godx-jp/colab-fleet/internal/delivery/modclient/modtest"
)

func bg() context.Context { return context.Background() }

func TestClient_AllOperationsRoundTrip(t *testing.T) {
	f := modtest.NewFake(modtest.Behaviour{})
	c, r := startClient(t, f)

	pl, err := c.PrepareLaunch(bg(), modclient.PrepareLaunchArgs{ClaudeVersion: "9.9.9"})
	if err != nil {
		t.Fatal(err)
	}
	if !regexp.MustCompile(`^[0-9a-f]{16}$`).MatchString(pl.LaneKey) || pl.ClaudeVersion != "9.9.9" || pl.Env == nil {
		t.Errorf("prepare-launch = %+v", pl)
	}
	at, err := c.Attach(bg(), modclient.AttachArgs{LaneKey: pl.LaneKey, PID: 4242, Cwd: "/work"})
	if err != nil || !at.Live || at.State != "live" || at.LaneKey != pl.LaneKey {
		t.Fatalf("attach = %+v, %v", at, err)
	}
	sd, err := c.Send(bg(), modclient.SendArgs{LaneKey: pl.LaneKey, Text: "hello", AllowLeadingSlash: true, ConfirmWaitMs: 5})
	if err != nil || !sd.Written || sd.SendID == "" || sd.Transcript.SessionID != "x" {
		t.Fatalf("send = %+v, %v", sd, err)
	}
	cf, err := c.Confirm(bg(), modclient.ConfirmArgs{LaneKey: pl.LaneKey, SendID: sd.SendID, WaitMs: 5})
	if err != nil || cf.Verdict != "confirmed" || !cf.Final || !cf.Enqueued {
		t.Fatalf("confirm = %+v, %v", cf, err)
	}
	cl, err := c.CloseLane(bg(), modclient.CloseArgs{LaneKey: pl.LaneKey})
	if err != nil || !cl.Removed {
		t.Fatalf("close = %+v, %v", cl, err)
	}
	h, err := c.Health(bg(), modclient.HealthArgs{})
	if err != nil || !h.OK || h.Module != "fake" || !h.PeerCheck {
		t.Fatalf("health = %+v, %v", h, err)
	}

	// The wire carried exactly what the typed arguments said.
	byOp := map[string]json.RawMessage{}
	for _, q := range f.Requests() {
		byOp[q.Op] = q.Args
	}
	var sent modclient.SendArgs
	if err := json.Unmarshal(byOp["send"], &sent); err != nil || sent.Text != "hello" || !sent.AllowLeadingSlash || sent.ConfirmWaitMs != 5 {
		t.Errorf("send args on the wire = %s (%v)", byOp["send"], err)
	}
	if !strings.Contains(string(byOp["attach"]), `"pid":4242`) || !strings.Contains(string(byOp["attach"]), `"cwd":"/work"`) {
		t.Errorf("attach args on the wire = %s", byOp["attach"])
	}
	if r.Count("hello_ok") != 1 || r.Count("spawned") != 1 || r.Count("restarted") != 0 {
		t.Errorf("counters = %+v", r.counts)
	}
	if path, args, _ := f.LastLaunch(); path != "/nonexistent/fake" || len(args) != 1 || args[0] != "serve" {
		t.Errorf("launched %q %q, want the configured path with [serve]", path, args)
	}
}

func TestClient_ResponsesMatchedByIDNotOrder(t *testing.T) {
	// Three sends are in flight at once and the module answers them in the
	// REVERSE of the order they were asked; each caller must still get its own
	// answer. The order is forced with gates, not timers.
	f := modtest.NewFake(modtest.Behaviour{})
	relB, relA := make(chan struct{}), make(chan struct{})
	f.SetHandler(func(q modtest.Request) (any, *modtest.WireError, time.Duration) {
		if q.Op != "send" {
			return nil, nil, 0
		}
		var a modclient.SendArgs
		_ = json.Unmarshal(q.Args, &a)
		switch a.Text {
		case "a":
			<-relA
		case "b":
			<-relB
		}
		return map[string]any{"sendId": "id-" + a.Text, "written": true, "transcript": map[string]any{"sessionId": "s", "offset": 0}}, nil, 0
	})
	c, _ := startClient(t, f)

	type res struct {
		text string
		out  modclient.SendResult
		err  error
	}
	got := make(chan res, 3)
	for _, text := range []string{"a", "b", "c"} {
		go func() {
			out, err := c.Send(bg(), modclient.SendArgs{LaneKey: "k", Text: text})
			got <- res{text, out, err}
		}()
	}
	var order []string
	take := func() {
		t.Helper()
		select {
		case r := <-got:
			if r.err != nil || r.out.SendID != "id-"+r.text {
				t.Fatalf("caller %q got %+v, %v: an answer went to the wrong caller", r.text, r.out, r.err)
			}
			order = append(order, r.text)
		case <-time.After(3 * time.Second):
			t.Fatal("no answer")
		}
	}
	take() // c answers at once
	close(relB)
	take() // then b
	close(relA)
	take() // and a last, though it was asked first (or second)
	if strings.Join(order, "") != "cba" {
		t.Errorf("answers arrived in order %v, want c b a", order)
	}
}

func TestClient_SlowAttachDoesNotDelaySend(t *testing.T) {
	f := modtest.NewFake(modtest.Behaviour{})
	gate := make(chan struct{})
	f.SetHandler(func(q modtest.Request) (any, *modtest.WireError, time.Duration) {
		if q.Op == "attach" {
			select {
			case <-gate:
			case <-q.Ctx.Done():
			}
		}
		return nil, nil, 0
	})
	c, _ := startClient(t, f)

	attached := make(chan error, 1)
	go func() {
		_, err := c.Attach(bg(), modclient.AttachArgs{LaneKey: "slow"})
		attached <- err
	}()
	waitFor(t, func() bool { return f.CountOp("attach") == 1 }, 3*time.Second, "the module to receive the attach")

	// The attach is held inside the module. A send on another lane must not
	// wait behind it.
	ctx, cancel := context.WithTimeout(bg(), time.Second)
	defer cancel()
	if _, err := c.Send(ctx, modclient.SendArgs{LaneKey: "other", Text: "x"}); err != nil {
		t.Fatalf("a send behind a slow attach: %v", err)
	}
	select {
	case err := <-attached:
		t.Fatalf("the attach finished before it was released: %v", err)
	default:
	}
	close(gate)
	select {
	case err := <-attached:
		if err != nil {
			t.Fatalf("attach: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the attach never finished")
	}
}

func TestClient_DeadlineMissIsUnavailableNotResult(t *testing.T) {
	f := modtest.NewFake(modtest.Behaviour{Ops: map[string]modtest.OpScript{"attach": {HangForever: true}}})
	c, r := startClient(t, f, func(cfg *modclient.Config) {
		cfg.Deadlines = modclient.Deadlines{Default: 2 * time.Second, Prepare: 2 * time.Second, AttachExtra: 100 * time.Millisecond}
	})

	// From here the module answers health only when told to, so the suspicion a
	// missed deadline raises is observable before the probe clears it.
	gate := make(chan struct{})
	f.SetHandler(func(q modtest.Request) (any, *modtest.WireError, time.Duration) {
		if q.Op == "health" {
			select {
			case <-gate:
			case <-q.Ctx.Done():
			}
		}
		return nil, nil, 0
	})

	start := time.Now()
	_, err := c.Attach(bg(), modclient.AttachArgs{LaneKey: "k", TimeoutMs: 100})
	if err == nil {
		t.Fatal("a missed deadline came back as a result")
	}
	if !errors.Is(err, modclient.ErrDeadline) || !errors.Is(err, modclient.ErrLost) || errors.Is(err, modclient.ErrNotSent) {
		t.Errorf("err = %v: a deadline miss after the write must be ErrDeadline + ErrLost", err)
	}
	var me *modclient.Error
	if errors.As(err, &me) {
		t.Errorf("a deadline miss must not look like the module's own answer: %v", me)
	}
	if took := time.Since(start); took < 190*time.Millisecond {
		t.Errorf("returned after %v, before TimeoutMs + AttachExtra", took)
	}
	if r.Count("deadline_missed") != 1 {
		t.Errorf("deadline_missed = %d, want 1", r.Count("deadline_missed"))
	}
	st := c.Status()
	if !st.Suspect || c.Usable() {
		t.Errorf("status = %+v, usable=%v: a module that missed a deadline is suspect and not usable", st, c.Usable())
	}
	// An immediate health probe was sent; it is held, so the suspicion stays.
	waitFor(t, func() bool { return f.CountOp("health") == 2 }, 3*time.Second, "the immediate health probe")
	if c.Usable() {
		t.Error("usable before the probe answered")
	}
	close(gate)
	waitFor(t, c.Usable, 3*time.Second, "the probe to clear the suspicion")
	if c.Status().Suspect || c.Generation() != 1 || f.Starts() != 1 {
		t.Errorf("a passing probe must clear Suspect without a restart: %+v starts=%d", c.Status(), f.Starts())
	}
}

func TestClient_HelloRefused(t *testing.T) {
	huge := strings.Repeat("x", 5<<20)
	cases := []struct {
		name   string
		script modtest.HelloScript
		reason string
	}{
		{"WrongProtocol", modtest.HelloScript{Protocol: modtest.Int(2)}, "protocol 2"},
		{"MissingOp", modtest.HelloScript{Ops: []string{"prepare-launch", "attach", "send", "confirm", "close"}}, "health"},
		{"InvalidJSON", modtest.HelloScript{Raw: "this is not json{"}, "not a valid hello"},
		{"Silence", modtest.HelloScript{NoLine: true}, "no hello line"},
		{"OversizeLine", modtest.HelloScript{Raw: huge}, "line limit"},
		{"MalformedPrefix", modtest.HelloScript{ReservedEnvPrefixes: []string{"OK_", "not ok"}}, "reserved environment prefix"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			script := tc.script
			f := modtest.NewFake(modtest.Behaviour{Hello: &script})
			c, r := newClient(t, f, func(cfg *modclient.Config) { cfg.HelloTimeout = 150 * time.Millisecond })
			c.Start()

			waitFor(t, func() bool { return c.Status().State == modclient.StateDisabled }, 3*time.Second, "the module to be disabled")
			st := c.Status()
			if !strings.Contains(st.Reason, "hello refused") || !strings.Contains(st.Reason, tc.reason) {
				t.Errorf("reason = %q, want it to say the hello was refused and why (%q)", st.Reason, tc.reason)
			}
			if r.Count("hello_refused") != 1 || r.Count("hello_ok") != 0 || c.Generation() != 0 || st.Hello != nil {
				t.Errorf("counters=%v generation=%d hello=%v", r.counts, c.Generation(), st.Hello)
			}
			if c.Usable() {
				t.Error("a refused module is usable")
			}
			// The refused child is killed, and never restarted: the same
			// binary would say the same thing again.
			waitFor(t, func() bool { return f.Kills() >= 1 }, 3*time.Second, "the refused child to be killed")
			time.Sleep(120 * time.Millisecond) // many backoff periods
			if f.Starts() != 1 {
				t.Errorf("Starts = %d: a refused module must not be restarted", f.Starts())
			}
			// Callers see it as "nothing was sent", so they fall back safely.
			if _, err := c.PrepareLaunch(bg(), modclient.PrepareLaunchArgs{}); !errors.Is(err, modclient.ErrNotSent) {
				t.Errorf("PrepareLaunch on a refused module: %v, want ErrNotSent", err)
			}
			// The reason is logged once.
			n := 0
			for _, l := range r.Logs() {
				if strings.Contains(l, "hello refused") {
					n++
				}
			}
			if n != 1 {
				t.Errorf("the refusal was logged %d times, want once: %q", n, r.Logs())
			}
		})
	}
}

func TestClient_OversizeResponseLineHarmless(t *testing.T) {
	f := modtest.NewFake(modtest.Behaviour{Ops: map[string]modtest.OpScript{"attach": {OversizeBytes: 5 << 20}}})
	c, r := startClient(t, f)

	for i := 0; i < 2; i++ {
		// The oversize answer is dropped, so this attach gets no answer.
		_, err := c.Attach(bg(), modclient.AttachArgs{LaneKey: "big", TimeoutMs: 50})
		if !errors.Is(err, modclient.ErrLost) {
			t.Fatalf("attach %d: %v, want ErrLost (the answer was dropped)", i, err)
		}
		// The connection is intact: the very next request is answered.
		if _, err := c.Send(bg(), modclient.SendArgs{LaneKey: "k", Text: "after"}); err != nil {
			t.Fatalf("send after an oversize line: %v", err)
		}
	}
	if c.Generation() != 1 || f.Starts() != 1 || r.Count("exited") != 0 {
		t.Errorf("an oversize line must not cost the connection: generation=%d starts=%d exited=%d", c.Generation(), f.Starts(), r.Count("exited"))
	}
	waitFor(t, c.Usable, 3*time.Second, "the suspicion the missed attach raised to clear")
	n := 0
	for _, l := range r.Logs() {
		if strings.Contains(l, "dropped a response line over") {
			n++
		}
	}
	if n != 1 {
		t.Errorf("the oversize line was logged %d times, want once per child: %q", n, r.Logs())
	}
}

func TestClient_ResponseWithAnotherIDMatchesNothing(t *testing.T) {
	// The module answers an attach under an id nobody is waiting for. It must
	// not be delivered to that attach, to a neighbouring request, or anywhere:
	// matching is by id and only by id.
	f := modtest.NewFake(modtest.Behaviour{Ops: map[string]modtest.OpScript{"attach": {IDOverride: "424242"}}})
	c, _ := startClient(t, f)

	sent := make(chan error, 1)
	go func() {
		_, err := c.Send(bg(), modclient.SendArgs{LaneKey: "k", Text: "neighbour"})
		sent <- err
	}()
	_, err := c.Attach(bg(), modclient.AttachArgs{LaneKey: "k", TimeoutMs: 50})
	if !errors.Is(err, modclient.ErrLost) || !errors.Is(err, modclient.ErrDeadline) {
		t.Errorf("attach answered under the wrong id: %v, want it to time out as ErrLost", err)
	}
	if err := <-sent; err != nil {
		t.Errorf("the neighbouring send was disturbed: %v", err)
	}
}

func TestClient_UnknownFieldsIgnored(t *testing.T) {
	extra := modtest.OpScript{ExtraFields: true}
	f := modtest.NewFake(modtest.Behaviour{Ops: map[string]modtest.OpScript{
		"prepare-launch": extra, "attach": extra, "send": extra, "confirm": extra, "close": extra, "health": extra,
	}})
	c, _ := startClient(t, f)

	if pl, err := c.PrepareLaunch(bg(), modclient.PrepareLaunchArgs{}); err != nil || pl.LaneKey == "" {
		t.Fatalf("prepare-launch: %+v %v", pl, err)
	}
	if at, err := c.Attach(bg(), modclient.AttachArgs{LaneKey: "k"}); err != nil || !at.Live {
		t.Fatalf("attach: %+v %v", at, err)
	}
	if sd, err := c.Send(bg(), modclient.SendArgs{LaneKey: "k", Text: "x"}); err != nil || !sd.Written {
		t.Fatalf("send: %+v %v", sd, err)
	}
	if cf, err := c.Confirm(bg(), modclient.ConfirmArgs{LaneKey: "k", SendID: "s"}); err != nil || cf.Verdict != "confirmed" {
		t.Fatalf("confirm: %+v %v", cf, err)
	}
	if cl, err := c.CloseLane(bg(), modclient.CloseArgs{LaneKey: "k"}); err != nil || !cl.Removed {
		t.Fatalf("close: %+v %v", cl, err)
	}
	if h, err := c.Health(bg(), modclient.HealthArgs{}); err != nil || !h.OK {
		t.Fatalf("health: %+v %v", h, err)
	}
}

func TestClient_ChildExitFailsPendingAndRestartsWithBackoff(t *testing.T) {
	const min = 60 * time.Millisecond
	f := modtest.NewFake(modtest.Behaviour{})
	hold := func(q modtest.Request) (any, *modtest.WireError, time.Duration) {
		if q.Op == "attach" {
			<-q.Ctx.Done()
		}
		return nil, nil, 0
	}
	f.SetHandler(hold)
	c, r := startClient(t, f, func(cfg *modclient.Config) {
		cfg.BackoffMin, cfg.BackoffMax = min, 4*min
	})

	// A request in flight when the child dies fails as LOST: it was written and
	// its fate is unknown. It must not hang until its deadline.
	failed := make(chan error, 1)
	go func() {
		_, err := c.Attach(bg(), modclient.AttachArgs{LaneKey: "k"})
		failed <- err
	}()
	waitFor(t, func() bool { return f.CountOp("attach") == 1 }, 3*time.Second, "the attach to reach the module")
	killedAt := time.Now()
	f.Kill()
	select {
	case err := <-failed:
		if !errors.Is(err, modclient.ErrLost) || errors.Is(err, modclient.ErrNotSent) {
			t.Errorf("pending call on a dead child: %v, want ErrLost", err)
		}
		if time.Since(killedAt) > time.Second {
			t.Error("the pending call waited for its deadline instead of failing at once")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the pending call was never failed")
	}

	// Until the restart, nothing is sent: callers fall back safely.
	if c.Generation() == 1 {
		if _, err := c.Send(bg(), modclient.SendArgs{LaneKey: "k", Text: "x"}); c.Generation() == 1 && !errors.Is(err, modclient.ErrNotSent) {
			t.Errorf("send while the module is down: %v, want ErrNotSent", err)
		}
		if c.Usable() && c.Generation() == 1 {
			t.Error("usable while the child is dead")
		}
	}

	// Restarts back off: the gap between a kill and the next launch is at least
	// the current backoff — 60, 120, 240, 240 ms.
	gaps := []time.Duration{min, 2 * min, 4 * min, 4 * min}
	for i, gap := range gaps {
		if i > 0 {
			waitFor(t, c.Usable, 3*time.Second, "the module to be back")
			killedAt = time.Now()
			f.Kill()
		}
		want := i + 2
		waitFor(t, func() bool { return f.Starts() == want }, 3*time.Second, "the restart")
		if got := f.StartTimes()[want-1].Sub(killedAt); got < gap {
			t.Errorf("restart %d came %v after the kill, want at least %v", i+1, got, gap)
		}
	}
	waitFor(t, c.Usable, 3*time.Second, "the module to be back")
	if r.Count("exited") != 4 || r.Count("restarted") != 4 || r.Count("spawned") != 5 || r.Count("hello_ok") != 5 {
		t.Errorf("counters = %+v", r.counts)
	}
	if st := c.Status(); st.Restarts != 4 || st.Generation != 5 {
		t.Errorf("status = restarts %d generation %d", st.Restarts, st.Generation)
	}
	waitFor(t, func() bool { return len(r.Ready()) == 5 }, 3*time.Second, "OnReady for every generation")
	for i, g := range r.Ready() {
		if g != uint64(i+1) {
			t.Errorf("OnReady generations = %v, want 1..5 in order", r.Ready())
			break
		}
	}
}

func TestClient_SpawnFailureRetriesWithBackoff(t *testing.T) {
	f := modtest.NewFake(modtest.Behaviour{})
	f.FailNextLaunches(2)
	c, r := newClient(t, f)
	c.Start()
	waitFor(t, c.Usable, 3*time.Second, "the module to come up after two failed launches")
	// `spawned` counts real starts only, and a first real start is not a restart.
	if r.Count("spawned") != 1 || r.Count("restarted") != 0 || f.Starts() != 1 || c.Status().Restarts != 0 {
		t.Errorf("counters = %v starts=%d", r.counts, f.Starts())
	}
	logged := 0
	for _, l := range r.Logs() {
		if strings.Contains(l, "could not start") {
			logged++
		}
	}
	if logged != 2 {
		t.Errorf("spawn failures logged %d times, want 2: %q", logged, r.Logs())
	}
}

func TestClient_StaleGenerationResponseIgnored(t *testing.T) {
	f := modtest.NewFake(modtest.Behaviour{})
	release := make(chan struct{})
	f.SetHandler(func(q modtest.Request) (any, *modtest.WireError, time.Duration) {
		if q.Op != "attach" {
			return nil, nil, 0
		}
		if q.Incarnation == 1 {
			<-q.Ctx.Done() // the first child never answers, and dies
			return nil, nil, 0
		}
		select {
		case <-release:
		case <-q.Ctx.Done():
		}
		var a modclient.AttachArgs
		_ = json.Unmarshal(q.Args, &a)
		return map[string]any{"laneKey": a.LaneKey, "live": true, "state": "live", "sinceMs": 1}, nil, 0
	})
	c, _ := startClient(t, f)

	old := make(chan error, 1)
	go func() {
		_, err := c.Attach(bg(), modclient.AttachArgs{LaneKey: "first"})
		old <- err
	}()
	waitFor(t, func() bool { return f.CountOp("attach") == 1 }, 3*time.Second, "the first attach")
	staleID := lastID(t, f, "attach")
	f.Kill()
	if err := <-old; !errors.Is(err, modclient.ErrLost) {
		t.Fatalf("first attach: %v", err)
	}
	waitFor(t, func() bool { return c.Generation() == 2 && c.Usable() }, 3*time.Second, "the second generation")

	// A request in the new generation, held inside the module...
	type res struct {
		out modclient.AttachResult
		err error
	}
	got := make(chan res, 1)
	go func() {
		out, err := c.Attach(bg(), modclient.AttachArgs{LaneKey: "real"})
		got <- res{out, err}
	}()
	waitFor(t, func() bool { return f.CountOp("attach") == 2 }, 3*time.Second, "the second attach")
	freshID := lastID(t, f, "attach")
	if freshID <= staleID {
		t.Fatalf("request ids went from %d to %d across a restart: they must never repeat or shrink", staleID, freshID)
	}

	// ...while a late answer to the DEAD generation's request shows up. It must
	// match nothing. (If ids restarted at 1 per child, it would match this one.)
	stale := `{"id":"` + strconv.FormatUint(staleID, 10) + `","ok":true,"result":{"laneKey":"STALE","live":true,"state":"live","sinceMs":9}}`
	if err := f.Inject(stale); err != nil {
		t.Fatal(err)
	}
	if err := f.Inject(`{"id":"999999","ok":true,"result":{}}`); err != nil { // an id nobody asked
		t.Fatal(err)
	}
	if err := f.Inject(`{"ok":true,"result":{}}`); err != nil { // no id at all
		t.Fatal(err)
	}
	if err := f.Inject(`not json at all`); err != nil {
		t.Fatal(err)
	}
	close(release)
	select {
	case r := <-got:
		if r.err != nil || r.out.LaneKey != "real" {
			t.Fatalf("attach = %+v, %v: the stale response was taken for this request's answer", r.out, r.err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the attach never completed")
	}
}

// lastID returns the wire id of the most recent recorded request for op.
func lastID(t *testing.T, f *modtest.Fake, op string) uint64 {
	t.Helper()
	var id string
	for _, q := range f.Requests() {
		if q.Op == op {
			id = q.ID
		}
	}
	n, err := strconv.ParseUint(id, 10, 64)
	if err != nil {
		t.Fatalf("request id %q is not a decimal counter: %v", id, err)
	}
	return n
}

func TestClient_RequestIDsAreStrictlyIncreasing(t *testing.T) {
	f := modtest.NewFake(modtest.Behaviour{})
	c, _ := startClient(t, f)
	for i := 0; i < 5; i++ {
		if _, err := c.Send(bg(), modclient.SendArgs{LaneKey: "k", Text: "x"}); err != nil {
			t.Fatal(err)
		}
	}
	f.Kill()
	waitFor(t, func() bool { return c.Generation() == 2 && c.Usable() }, 3*time.Second, "the restart")
	for i := 0; i < 5; i++ {
		if _, err := c.Send(bg(), modclient.SendArgs{LaneKey: "k", Text: "x"}); err != nil {
			t.Fatal(err)
		}
	}
	seen := map[string]bool{}
	for _, q := range f.Requests() {
		if seen[q.ID] {
			t.Fatalf("id %s was used twice", q.ID)
		}
		seen[q.ID] = true
	}
	if len(seen) < 12 {
		t.Errorf("only %d distinct ids", len(seen))
	}
}

func TestClient_NotSentVsLost(t *testing.T) {
	t.Run("NoChildYet", func(t *testing.T) {
		c, _ := newClient(t, modtest.NewFake(modtest.Behaviour{})) // never started
		_, err := c.Attach(bg(), modclient.AttachArgs{LaneKey: "k"})
		if !errors.Is(err, modclient.ErrNotSent) || errors.Is(err, modclient.ErrLost) {
			t.Errorf("err = %v, want ErrNotSent", err)
		}
	})

	t.Run("ContextAlreadyDone", func(t *testing.T) {
		f := modtest.NewFake(modtest.Behaviour{})
		c, _ := startClient(t, f)
		ctx, cancel := context.WithCancel(bg())
		cancel()
		_, err := c.Send(ctx, modclient.SendArgs{LaneKey: "k", Text: "x"})
		if !errors.Is(err, modclient.ErrNotSent) || !errors.Is(err, context.Canceled) || errors.Is(err, modclient.ErrLost) {
			t.Errorf("err = %v, want ErrNotSent wrapping context.Canceled", err)
		}
		if f.CountOp("send") != 0 {
			t.Error("a request with a dead context reached the module")
		}
	})

	t.Run("QueuedThenCancelledNeverReachesTheModule", func(t *testing.T) {
		// The module says hello and then stops reading, so the first frame the
		// client writes sits half-delivered in the pipe and every frame behind
		// it stays queued. Three callers race for the pipe; at most one can be
		// inside the write, so at least two are still queued when their
		// contexts end — and those are ErrNotSent, and must never be written.
		f := modtest.NewFake(modtest.Behaviour{StallReads: true})
		c, _ := newClient(t, f)
		c.Start()
		waitFor(t, func() bool { return c.Generation() == 1 }, 3*time.Second, "the hello")

		keys := []string{"m1", "m2", "m3"}
		errs := make([]error, len(keys))
		var wg sync.WaitGroup
		for i, key := range keys {
			wg.Add(1)
			go func() {
				defer wg.Done()
				ctx, cancel := context.WithTimeout(bg(), 100*time.Millisecond)
				defer cancel()
				_, errs[i] = c.CloseLane(ctx, modclient.CloseArgs{LaneKey: key})
			}()
		}
		wg.Wait()
		unsent := map[string]bool{}
		for i, err := range errs {
			notSent, lost := errors.Is(err, modclient.ErrNotSent), errors.Is(err, modclient.ErrLost)
			if notSent == lost || !errors.Is(err, context.DeadlineExceeded) || errors.Is(err, modclient.ErrDeadline) {
				t.Fatalf("call %d: %v, want exactly one of ErrNotSent/ErrLost, caused by the caller's deadline", i, err)
			}
			if notSent {
				unsent[keys[i]] = true
			}
		}
		if len(unsent) < 2 {
			t.Fatalf("only %d of 3 callers were ErrNotSent (%v): at most one frame can be inside the write", len(unsent), errs)
		}

		// Let the module read again. Frames abandoned while queued must never
		// be written; the one that was mid-write may be.
		f.Resume()
		waitFor(t, c.Usable, 3*time.Second, "the module to catch up")
		if _, err := c.Send(bg(), modclient.SendArgs{LaneKey: "k", Text: "marker"}); err != nil { // FIFO: the writer is past every abandoned frame
			t.Fatal(err)
		}
		for _, q := range f.Requests() {
			if q.Op != "close" {
				continue
			}
			var a modclient.CloseArgs
			_ = json.Unmarshal(q.Args, &a)
			if unsent[a.LaneKey] {
				t.Errorf("close for %q was reported ErrNotSent yet reached the module", a.LaneKey)
			}
		}
	})

	t.Run("WrittenThenDeadlineIsLost", func(t *testing.T) {
		f := modtest.NewFake(modtest.Behaviour{Ops: map[string]modtest.OpScript{"attach": {HangForever: true}}})
		c, _ := startClient(t, f)
		_, err := c.Attach(bg(), modclient.AttachArgs{LaneKey: "k", TimeoutMs: 20})
		if !errors.Is(err, modclient.ErrLost) || !errors.Is(err, modclient.ErrDeadline) || errors.Is(err, modclient.ErrNotSent) {
			t.Errorf("err = %v, want ErrLost + ErrDeadline", err)
		}
		waitFor(t, func() bool { return f.CountOp("attach") == 1 }, 3*time.Second, "the module to have seen the request the client calls lost")
	})

	t.Run("WrittenThenChildDiesIsLost", func(t *testing.T) {
		f := modtest.NewFake(modtest.Behaviour{Ops: map[string]modtest.OpScript{"send": {HangForever: true}}})
		c, _ := startClient(t, f, func(cfg *modclient.Config) { cfg.BackoffMin, cfg.BackoffMax = time.Hour, time.Hour })
		errc := make(chan error, 1)
		go func() {
			_, err := c.Send(bg(), modclient.SendArgs{LaneKey: "k", Text: "x"})
			errc <- err
		}()
		waitFor(t, func() bool { return f.CountOp("send") == 1 }, 3*time.Second, "the send to reach the module")
		f.Kill()
		if err := <-errc; !errors.Is(err, modclient.ErrLost) || errors.Is(err, modclient.ErrNotSent) {
			t.Errorf("err = %v, want ErrLost", err)
		}
	})

	t.Run("ModuleRefusalIsNeither", func(t *testing.T) {
		f := modtest.NewFake(modtest.Behaviour{Ops: map[string]modtest.OpScript{
			"send":  {Error: &modtest.WireError{Code: "not-live", Message: "no client\nconnected", Retryable: true}},
			"close": {Error: &modtest.WireError{Code: "unknown-lane", Message: "no such lane"}},
		}})
		c, _ := startClient(t, f)
		_, err := c.Send(bg(), modclient.SendArgs{LaneKey: "k", Text: "x"})
		var me *modclient.Error
		if !errors.As(err, &me) || me.Code != "not-live" || !me.Retryable || !modclient.IsCode(err, "not-live") {
			t.Fatalf("err = %v, want the module's not-live error with Retryable", err)
		}
		if errors.Is(err, modclient.ErrNotSent) || errors.Is(err, modclient.ErrLost) {
			t.Error("an ok:false answer is a complete answer")
		}
		if strings.ContainsAny(me.Message, "\n\r") {
			t.Errorf("control characters survived in the message: %q", me.Message)
		}
		if _, err := c.CloseLane(bg(), modclient.CloseArgs{LaneKey: "k"}); !modclient.IsCode(err, "unknown-lane") {
			t.Errorf("close: %v", err)
		}
		if !c.Usable() {
			t.Error("module refusals are not a health signal")
		}
	})

	t.Run("UnusableAnswerIsLostToo", func(t *testing.T) {
		// The request reached the module, so an answer we cannot use leaves the
		// outcome unknown — never "nothing was written".
		for name, result := range map[string]string{
			"null":         `null`,
			"a string":     `"written"`,
			"an array":     `[]`,
			"wrong type":   `{"sendId":"s","written":"yes"}`,
			"bad send id":  `{"sendId":"a\nb","written":true}`,
			"long send id": `{"sendId":"` + strings.Repeat("s", 500) + `","written":true}`,
		} {
			t.Run(name, func(t *testing.T) {
				f := modtest.NewFake(modtest.Behaviour{Ops: map[string]modtest.OpScript{"send": {Results: []json.RawMessage{json.RawMessage(result)}}}})
				c, _ := startClient(t, f)
				_, err := c.Send(bg(), modclient.SendArgs{LaneKey: "k", Text: "x"})
				if !errors.Is(err, modclient.ErrBadResponse) || !errors.Is(err, modclient.ErrLost) || errors.Is(err, modclient.ErrNotSent) {
					t.Errorf("err = %v, want ErrBadResponse + ErrLost", err)
				}
			})
		}
	})
}

func TestClient_PrepareLaunchResultIsChecked(t *testing.T) {
	bad := map[string]string{
		"no lane key":      `{"env":{}}`,
		"lane key control": `{"laneKey":"a\u0000b","env":{}}`,
		"bad env name":     `{"laneKey":"k","env":{"A=B":"x"}}`,
		"env name digit":   `{"laneKey":"k","env":{"1A":"x"}}`,
		"env NUL":          `{"laneKey":"k","env":{"A":"x\u0000y"}}`,
		"env huge":         `{"laneKey":"k","env":{"A":"` + strings.Repeat("v", 40<<10) + `"}}`,
	}
	for name, result := range bad {
		t.Run(name, func(t *testing.T) {
			f := modtest.NewFake(modtest.Behaviour{Ops: map[string]modtest.OpScript{"prepare-launch": {Results: []json.RawMessage{json.RawMessage(result)}}}})
			c, _ := startClient(t, f)
			if _, err := c.PrepareLaunch(bg(), modclient.PrepareLaunchArgs{}); !errors.Is(err, modclient.ErrBadResponse) {
				t.Errorf("err = %v, want ErrBadResponse", err)
			}
		})
	}

	// An opaque key with path separators is fine — it is the CALLER's rule never
	// to build a path from it — and the environment comes back verbatim.
	f := modtest.NewFake(modtest.Behaviour{Ops: map[string]modtest.OpScript{"prepare-launch": {Results: []json.RawMessage{
		json.RawMessage(`{"laneKey":"../../etc/x","claudeVersion":"1.0","env":{"ACME_SOCK":"/run/a b/c","ACME_X":""}}`)}}}})
	c, _ := startClient(t, f)
	pl, err := c.PrepareLaunch(bg(), modclient.PrepareLaunchArgs{})
	if err != nil {
		t.Fatal(err)
	}
	if pl.LaneKey != "../../etc/x" || pl.Env["ACME_SOCK"] != "/run/a b/c" || len(pl.Env) != 2 {
		t.Errorf("prepare-launch = %+v", pl)
	}
}

func TestClient_TooLargeRequestIsRefusedLocally(t *testing.T) {
	f := modtest.NewFake(modtest.Behaviour{})
	c, _ := startClient(t, f)
	_, err := c.Send(bg(), modclient.SendArgs{LaneKey: "k", Text: strings.Repeat("a", modclient.MaxLineBytes+1)})
	if !modclient.IsCode(err, "too-large") || errors.Is(err, modclient.ErrLost) {
		t.Fatalf("err = %v, want the too-large refusal", err)
	}
	if f.CountOp("send") != 0 {
		t.Error("an oversized request was sent")
	}
	// The connection is unharmed.
	if _, err := c.Send(bg(), modclient.SendArgs{LaneKey: "k", Text: "small"}); err != nil {
		t.Fatal(err)
	}
}

func TestClient_PerOperationDeadlines(t *testing.T) {
	hang := modtest.OpScript{HangForever: true}
	hung := modtest.Behaviour{Ops: map[string]modtest.OpScript{"prepare-launch": hang, "attach": hang, "send": hang, "confirm": hang, "close": hang}}
	timed := func(fn func() error) (time.Duration, error) {
		start := time.Now()
		err := fn()
		return time.Since(start), err
	}

	// Default is long here, so "returned well before Default" proves the
	// operation had its own, shorter limit.
	t.Run("PrepareAndAttachHaveTheirOwnLimits", func(t *testing.T) {
		c, _ := startClient(t, modtest.NewFake(hung), func(cfg *modclient.Config) {
			cfg.Deadlines = modclient.Deadlines{Default: 5 * time.Second, Prepare: 60 * time.Millisecond, AttachExtra: 40 * time.Millisecond}
		})
		// prepare-launch is on the create path: it gets the SHORT limit.
		d, err := timed(func() error { _, err := c.PrepareLaunch(bg(), modclient.PrepareLaunchArgs{}); return err })
		if !errors.Is(err, modclient.ErrDeadline) || d < 55*time.Millisecond || d > 3*time.Second {
			t.Errorf("prepare-launch: %v after %v, want ErrDeadline near 60ms", err, d)
		}
		// attach waits its own TimeoutMs plus AttachExtra.
		d, err = timed(func() error { _, err := c.Attach(bg(), modclient.AttachArgs{LaneKey: "k", TimeoutMs: 100}); return err })
		if !errors.Is(err, modclient.ErrDeadline) || d < 135*time.Millisecond || d > 3*time.Second {
			t.Errorf("attach: %v after %v, want ErrDeadline near 140ms", err, d)
		}
	})

	t.Run("EverythingElseGetsDefault", func(t *testing.T) {
		c, _ := startClient(t, modtest.NewFake(hung), func(cfg *modclient.Config) {
			cfg.Deadlines = modclient.Deadlines{Default: 250 * time.Millisecond, Prepare: 5 * time.Second, AttachExtra: 40 * time.Millisecond}
		})
		d, err := timed(func() error { _, err := c.CloseLane(bg(), modclient.CloseArgs{LaneKey: "k"}); return err })
		if !errors.Is(err, modclient.ErrDeadline) || d < 245*time.Millisecond {
			t.Errorf("close: %v after %v, want ErrDeadline near 250ms", err, d)
		}
		// An attach with no TimeoutMs of its own also gets Default.
		d, err = timed(func() error { _, err := c.Attach(bg(), modclient.AttachArgs{LaneKey: "k"}); return err })
		if !errors.Is(err, modclient.ErrDeadline) || d < 245*time.Millisecond {
			t.Errorf("attach without TimeoutMs: %v after %v, want ErrDeadline near 250ms", err, d)
		}
	})

	t.Run("AWaitingOperationIsNotJudgedByDefault", func(t *testing.T) {
		// A send that asks the module to wait a while must be given that while.
		// Prove it by letting the CALLER's context end first: had Default (250ms)
		// applied, the error would be the module's ErrDeadline instead.
		c, _ := startClient(t, modtest.NewFake(hung), func(cfg *modclient.Config) {
			cfg.Deadlines = modclient.Deadlines{Default: 250 * time.Millisecond, Prepare: 5 * time.Second, AttachExtra: 5 * time.Second}
		})
		for name, call := range map[string]func(context.Context) error{
			"send": func(ctx context.Context) error {
				_, err := c.Send(ctx, modclient.SendArgs{LaneKey: "k", Text: "x", ConfirmWaitMs: 2000})
				return err
			},
			"confirm": func(ctx context.Context) error {
				_, err := c.Confirm(ctx, modclient.ConfirmArgs{LaneKey: "k", SendID: "s", WaitMs: 2000})
				return err
			},
		} {
			ctx, cancel := context.WithTimeout(bg(), 500*time.Millisecond)
			err := call(ctx)
			cancel()
			if errors.Is(err, modclient.ErrDeadline) || !errors.Is(err, context.DeadlineExceeded) {
				t.Errorf("%s with a wait: %v, want the caller's own deadline, not the module's", name, err)
			}
		}
	})
}

func TestClient_ShutdownClosesStdinThenKills(t *testing.T) {
	t.Run("CooperativeModuleExitsOnStdinClose", func(t *testing.T) {
		f := modtest.NewFake(modtest.Behaviour{})
		c, _ := startClient(t, f)
		c.Stop()
		if f.StdinClosed() != 1 {
			t.Errorf("StdinClosed = %d: shutdown must close the module's stdin", f.StdinClosed())
		}
		if f.Kills() != 0 {
			t.Errorf("Kills = %d: a module that exits when its stdin closes must not be killed", f.Kills())
		}
		st := c.Status()
		if st.State != modclient.StateDisabled || st.Reason != "stopped" || c.Usable() {
			t.Errorf("status after Stop = %+v", st)
		}
		if _, err := c.Send(bg(), modclient.SendArgs{LaneKey: "k", Text: "x"}); !errors.Is(err, modclient.ErrNotSent) {
			t.Errorf("send after Stop: %v, want ErrNotSent", err)
		}
	})

	t.Run("StubbornModuleIsKilledAfterTheGrace", func(t *testing.T) {
		f := modtest.NewFake(modtest.Behaviour{IgnoreStdinClose: true})
		c, _ := startClient(t, f, func(cfg *modclient.Config) { cfg.ShutdownGrace = 150 * time.Millisecond })
		start := time.Now()
		c.Stop()
		if took := time.Since(start); took < 145*time.Millisecond {
			t.Errorf("Stop returned after %v: the module ignoring its stdin must be given the grace first", took)
		}
		if f.StdinClosed() != 1 || f.Kills() != 1 {
			t.Errorf("StdinClosed = %d Kills = %d, want stdin closed first, then one kill", f.StdinClosed(), f.Kills())
		}
	})

	t.Run("IdempotentAndSafeWhenNeverStarted", func(t *testing.T) {
		c, _ := newClient(t, modtest.NewFake(modtest.Behaviour{}))
		c.Stop()
		c.Stop()
		c.Start() // after Stop it must not launch anything
		if st := c.Status(); st.State != modclient.StateDisabled {
			t.Errorf("state = %s", st.State)
		}

		f := modtest.NewFake(modtest.Behaviour{})
		c2, _ := startClient(t, f)
		var wg sync.WaitGroup
		for i := 0; i < 4; i++ {
			wg.Add(1)
			go func() { defer wg.Done(); c2.Stop() }()
		}
		wg.Wait()
		c2.Stop()
		c2.Start()
		if f.Starts() != 1 {
			t.Errorf("Starts = %d after Stop+Start", f.Starts())
		}
	})

	t.Run("StopDuringBackoffDoesNotWaitForIt", func(t *testing.T) {
		f := modtest.NewFake(modtest.Behaviour{})
		c, _ := startClient(t, f, func(cfg *modclient.Config) { cfg.BackoffMin, cfg.BackoffMax = time.Hour, time.Hour })
		f.Kill()
		waitFor(t, func() bool { return c.Status().State == modclient.StateUnavailable }, 3*time.Second, "the module to be down")
		start := time.Now()
		c.Stop()
		if took := time.Since(start); took > 2*time.Second {
			t.Errorf("Stop waited %v for a backoff timer", took)
		}
	})
}

func TestClient_ManyConcurrentCallers(t *testing.T) {
	f := modtest.NewFake(modtest.Behaviour{})
	f.SetHandler(func(q modtest.Request) (any, *modtest.WireError, time.Duration) {
		if q.Op != "send" {
			return nil, nil, 0
		}
		var a modclient.SendArgs
		_ = json.Unmarshal(q.Args, &a)
		// Uneven delays so the answers interleave differently every run.
		return map[string]any{"sendId": "id-" + a.Text, "written": true, "transcript": map[string]any{"sessionId": "s", "offset": 0}}, nil,
			time.Duration(len(a.Text)*3%7) * time.Millisecond
	})
	c, _ := startClient(t, f)
	const n = 200
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			text := "t" + strconv.Itoa(i)
			out, err := c.Send(bg(), modclient.SendArgs{LaneKey: "k", Text: text})
			if err != nil {
				errs <- err
			} else if out.SendID != "id-"+text {
				errs <- errors.New("answer for " + text + " went to " + out.SendID)
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
}

func TestClient_OnChangeFires(t *testing.T) {
	f := modtest.NewFake(modtest.Behaviour{})
	c, r := startClient(t, f)
	waitFor(t, func() bool { return r.Changes() > 0 }, 3*time.Second, "OnChange")
	before := r.Changes()
	f.Kill()
	waitFor(t, func() bool { return r.Changes() > before }, 3*time.Second, "OnChange after the child died")
	_ = c
}
