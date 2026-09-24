package modclient_test

import (
	"encoding/json"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/godx-jp/colab-fleet/internal/delivery/modclient"
	"github.com/godx-jp/colab-fleet/internal/delivery/modclient/modtest"
)

func healthResult(over map[string]any) map[string]any {
	m := map[string]any{
		"ok": true, "module": "fake", "protocol": 1, "version": "0.0.1", "platform": "test",
		"peerCheck": true, "lanes": []any{}, "counters": map[string]any{},
	}
	for k, v := range over {
		m[k] = v
	}
	return m
}

func TestHealth_AvailableAfterHelloAndHealth(t *testing.T) {
	f := modtest.NewFake(modtest.Behaviour{Hello: &modtest.HelloScript{ReservedEnvPrefixes: []string{"ACME_"}, Version: "2.0.0"}})
	c, r := newClient(t, f, func(cfg *modclient.Config) { cfg.HealthInterval = 20 * time.Millisecond })

	// Before Start nothing is known and nothing is usable.
	if st := c.Status(); st.State != modclient.StateStarting || st.Generation != 0 || st.Hello != nil || st.Health != nil || c.Usable() {
		t.Errorf("status before Start = %+v", st)
	}
	if _, ok := c.Hello(); ok {
		t.Error("Hello() before any hello")
	}
	if _, ok := c.LastHealth(); ok {
		t.Error("LastHealth() before any probe")
	}

	c.Start()
	waitFor(t, c.Usable, 3*time.Second, "the module to become available")
	waitFor(t, func() bool { return len(r.Ready()) == 1 }, 3*time.Second, "OnReady")

	st := c.Status()
	if st.State != modclient.StateAvailable || st.Reason != "" || st.Suspect || st.Generation != 1 || st.Restarts != 0 || st.Since.IsZero() {
		t.Errorf("status = %+v", st)
	}
	if st.Hello == nil || st.Hello.Module != "fake" || st.Hello.Protocol != 1 || !reflect.DeepEqual(st.Hello.ReservedEnvPrefixes, []string{"ACME_"}) {
		t.Errorf("hello = %+v", st.Hello)
	}
	if st.Health == nil || !st.Health.OK || st.Health.Module != "fake" || st.Health.Platform != "test" || !st.Health.PeerCheck {
		t.Errorf("health = %+v", st.Health)
	}
	if h, ok := c.Hello(); !ok || h.Module != "fake" {
		t.Errorf("Hello() = %+v %v", h, ok)
	}
	if h, ok := c.LastHealth(); !ok || !h.OK {
		t.Errorf("LastHealth() = %+v %v", h, ok)
	}
	if got := c.ReservedEnvPrefixes(); !reflect.DeepEqual(got, []string{"ACME_"}) {
		t.Errorf("ReservedEnvPrefixes = %v", got)
	}
	if reqs := f.Requests(); len(reqs) == 0 || reqs[0].Op != "health" {
		t.Errorf("the first request must be the health probe that makes the module available: %+v", reqs)
	}

	// The loop keeps probing, and OnReady is still one call per generation.
	waitFor(t, func() bool { return f.CountOp("health") >= 4 }, 3*time.Second, "periodic health probes")
	if got := r.Ready(); !reflect.DeepEqual(got, []uint64{1}) {
		t.Errorf("OnReady calls = %v, want exactly [1]", got)
	}

	// The snapshot is a copy: changing it must not change the client.
	st.Hello.ReservedEnvPrefixes[0] = "MUTATED_"
	if got := c.ReservedEnvPrefixes(); got[0] != "ACME_" {
		t.Errorf("mutating a Status snapshot changed the client: %v", got)
	}
}

func TestHealth_OkFalseUnhealthy(t *testing.T) {
	f := modtest.NewFake(modtest.Behaviour{})
	var calls atomic.Int32
	proceed := make(chan struct{})
	f.SetHandler(func(q modtest.Request) (any, *modtest.WireError, time.Duration) {
		if q.Op != "health" {
			return nil, nil, 0
		}
		switch calls.Add(1) {
		case 1:
			return healthResult(map[string]any{"ok": false}), nil, 0
		case 2:
			select {
			case <-proceed:
			case <-q.Ctx.Done():
			}
		}
		return nil, nil, 0
	})
	c, r := newClient(t, f, func(cfg *modclient.Config) { cfg.HealthInterval = 20 * time.Millisecond })
	c.Start()

	// ok:false is unhealthy AT ONCE — no second strike — and the child is kept:
	// it says it is alive, and it may recover.
	waitFor(t, func() bool { return c.Status().Reason == "health not ok" }, 3*time.Second, "the module to be marked unhealthy")
	st := c.Status()
	if st.State != modclient.StateUnavailable || c.Usable() {
		t.Errorf("status = %+v", st)
	}
	if st.Health == nil || st.Health.OK {
		t.Errorf("the ok:false result must be visible in Status: %+v", st.Health)
	}
	if f.Starts() != 1 || f.Kills() != 0 {
		t.Errorf("starts=%d kills=%d: an unhealthy module is not restarted", f.Starts(), f.Kills())
	}
	if len(r.Ready()) != 0 {
		t.Errorf("OnReady fired for a module that never passed a health probe: %v", r.Ready())
	}

	// A later ok:true restores it, and only THEN does OnReady fire.
	close(proceed)
	waitFor(t, c.Usable, 3*time.Second, "the module to recover")
	waitFor(t, func() bool { return len(r.Ready()) == 1 }, 3*time.Second, "OnReady after recovery")
	if !reflect.DeepEqual(r.Ready(), []uint64{1}) || c.Generation() != 1 || f.Starts() != 1 {
		t.Errorf("ready=%v generation=%d starts=%d", r.Ready(), c.Generation(), f.Starts())
	}
	if h, _ := c.LastHealth(); !h.OK {
		t.Error("LastHealth still says not ok")
	}
}

func TestHealth_TwoFailuresUnhealthy(t *testing.T) {
	f := modtest.NewFake(modtest.Behaviour{})
	var calls atomic.Int32
	gate3, gate4 := make(chan struct{}), make(chan struct{})
	failure := &modtest.WireError{Code: "internal", Message: "boom"}
	f.SetHandler(func(q modtest.Request) (any, *modtest.WireError, time.Duration) {
		if q.Op != "health" {
			return nil, nil, 0
		}
		switch calls.Add(1) {
		case 2:
			return nil, failure, 0
		case 3:
			select {
			case <-gate3:
			case <-q.Ctx.Done():
			}
			return nil, failure, 0
		case 4:
			select {
			case <-gate4:
			case <-q.Ctx.Done():
			}
		}
		return nil, nil, 0
	})
	c, r := newClient(t, f, func(cfg *modclient.Config) { cfg.HealthInterval = 20 * time.Millisecond })
	c.Start()
	waitFor(t, c.Usable, 3*time.Second, "the module to become available")

	// Probe 2 failed. One failure is a strike, not a verdict.
	waitFor(t, func() bool { return f.CountOp("health") == 3 }, 3*time.Second, "the third probe to be in flight")
	if st := c.Status(); st.State != modclient.StateAvailable || !c.Usable() {
		t.Fatalf("after ONE failed probe: %+v", st)
	}
	// Probe 3 fails too: two in a row.
	close(gate3)
	waitFor(t, func() bool { return c.Status().State == modclient.StateUnavailable }, 3*time.Second, "the second failure to count")
	if st := c.Status(); !strings.Contains(st.Reason, "twice") || c.Usable() {
		t.Errorf("status = %+v", st)
	}
	// The child is kept (these are errors, not deadline misses), and probing
	// goes on: probe 4 is held, so the unhealthy state is visible.
	waitFor(t, func() bool { return f.CountOp("health") == 4 }, 3*time.Second, "the fourth probe")
	if c.Usable() || f.Starts() != 1 || f.Kills() != 0 {
		t.Errorf("usable=%v starts=%d kills=%d", c.Usable(), f.Starts(), f.Kills())
	}
	close(gate4)
	waitFor(t, c.Usable, 3*time.Second, "the module to recover")
	// Recovery inside one generation does not fire OnReady again.
	if got := r.Ready(); !reflect.DeepEqual(got, []uint64{1}) {
		t.Errorf("OnReady = %v, want exactly [1]", got)
	}
}

func TestHealth_TwoDeadlineMissesKillTheChild(t *testing.T) {
	f := modtest.NewFake(modtest.Behaviour{})
	var calls atomic.Int32
	f.SetHandler(func(q modtest.Request) (any, *modtest.WireError, time.Duration) {
		// Only the first child hangs, and only after it has said it is healthy.
		if q.Op == "health" && q.Incarnation == 1 && calls.Add(1) >= 2 {
			<-q.Ctx.Done()
		}
		return nil, nil, 0
	})
	c, r := newClient(t, f, func(cfg *modclient.Config) {
		cfg.HealthInterval = 20 * time.Millisecond
		cfg.Deadlines = modclient.Deadlines{Default: 200 * time.Millisecond, Prepare: time.Second, AttachExtra: time.Second}
	})
	c.Start()
	waitFor(t, func() bool { return c.Generation() == 2 && c.Usable() }, 5*time.Second, "the hung child to be replaced")

	// A module that cannot answer health twice running will not answer a send:
	// it is killed so the restart path runs, not left parked as "unhealthy".
	if f.Kills() != 1 || r.Count("exited") != 1 || r.Count("deadline_missed") < 2 {
		t.Errorf("kills=%d exited=%d deadline_missed=%d", f.Kills(), r.Count("exited"), r.Count("deadline_missed"))
	}
	waitFor(t, func() bool { return len(r.Ready()) == 2 }, 3*time.Second, "OnReady for the new generation")
	if !reflect.DeepEqual(r.Ready(), []uint64{1, 2}) {
		t.Errorf("OnReady = %v, want [1 2]", r.Ready())
	}
}

func TestHealth_PeerCheckRecorded(t *testing.T) {
	for _, peer := range []bool{false, true} {
		name := "PeerCheckTrue"
		if !peer {
			name = "PeerCheckFalse"
		}
		t.Run(name, func(t *testing.T) {
			f := modtest.NewFake(modtest.Behaviour{Ops: map[string]modtest.OpScript{
				"health": {Results: []json.RawMessage{modtest.JSON(healthResult(map[string]any{"peerCheck": peer, "version": "3.1.4", "platform": "plan9"}))}},
			}})
			c, _ := startClient(t, f)
			st := c.Status()
			if st.Health == nil || st.Health.PeerCheck != peer || st.Health.Version != "3.1.4" || st.Health.Platform != "plan9" {
				t.Fatalf("health = %+v, want peerCheck=%v recorded with version and platform", st.Health, peer)
			}
			if h, ok := c.LastHealth(); !ok || h.PeerCheck != peer {
				t.Errorf("LastHealth = %+v %v", h, ok)
			}
			// What peerCheck:false MEANS (not live for a session) is the
			// caller's policy; the client only reports it and stays usable.
			if !c.Usable() {
				t.Error("the client must not apply the peer-check policy itself")
			}
			if hello, _ := c.Hello(); hello.Version != "3.1.4" {
				t.Errorf("a health result refreshes the hello's version: %+v", hello)
			}
		})
	}
}

func TestHealth_ReservedPrefixesRefreshedAndRetained(t *testing.T) {
	t.Run("HealthOverridesAndInvalidOnesAreDropped", func(t *testing.T) {
		f := modtest.NewFake(modtest.Behaviour{
			Hello: &modtest.HelloScript{ReservedEnvPrefixes: []string{"OLD_"}},
			Ops: map[string]modtest.OpScript{"health": {Results: []json.RawMessage{
				modtest.JSON(healthResult(map[string]any{"reservedEnvPrefixes": []string{"NEW_", "bad prefix", "NEW_", "AB_", "", "9X"}})),
			}}},
		})
		c, _ := startClient(t, f, func(cfg *modclient.Config) { cfg.BackoffMin, cfg.BackoffMax = time.Hour, time.Hour })
		want := []string{"NEW_", "AB_"}
		if got := c.ReservedEnvPrefixes(); !reflect.DeepEqual(got, want) {
			t.Errorf("ReservedEnvPrefixes = %v, want %v (validated, deduped)", got, want)
		}
		if h, _ := c.Hello(); !reflect.DeepEqual(h.ReservedEnvPrefixes, want) {
			t.Errorf("hello prefixes = %v", h.ReservedEnvPrefixes)
		}

		// The reservation outlives the child: a guard must not lapse just
		// because the module is restarting.
		f.Kill()
		waitFor(t, func() bool { return c.Status().State == modclient.StateUnavailable }, 3*time.Second, "the module to be down")
		if got := c.ReservedEnvPrefixes(); !reflect.DeepEqual(got, want) {
			t.Errorf("after the child exited: %v, want %v retained", got, want)
		}
	})

	t.Run("AbsentFieldKeepsWhatHelloSaid", func(t *testing.T) {
		f := modtest.NewFake(modtest.Behaviour{
			Hello: &modtest.HelloScript{ReservedEnvPrefixes: []string{"OLD_"}},
			Ops:   map[string]modtest.OpScript{"health": {Results: []json.RawMessage{modtest.JSON(healthResult(nil))}}},
		})
		c, _ := startClient(t, f)
		if got := c.ReservedEnvPrefixes(); !reflect.DeepEqual(got, []string{"OLD_"}) {
			t.Errorf("ReservedEnvPrefixes = %v: a health result without the field must not clear the reservation", got)
		}
	})
}

func TestHealth_PublicQueryDoesNotMoveTheStateMachine(t *testing.T) {
	f := modtest.NewFake(modtest.Behaviour{})
	f.SetHandler(func(q modtest.Request) (any, *modtest.WireError, time.Duration) {
		if q.Op == "health" && strings.Contains(string(q.Args), "sick") {
			return healthResult(map[string]any{"ok": false, "version": "per-lane"}), nil, 0
		}
		return nil, nil, 0
	})
	c, _ := startClient(t, f)
	h, err := c.Health(bg(), modclient.HealthArgs{LaneKey: "sick"})
	if err != nil || h.OK {
		t.Fatalf("Health = %+v, %v", h, err)
	}
	if !c.Usable() {
		t.Error("a caller's per-lane health query flipped the module unavailable")
	}
	if last, _ := c.LastHealth(); !last.OK || last.Version == "per-lane" {
		t.Errorf("a per-lane answer replaced the module-wide LastHealth: %+v", last)
	}
	// A module-wide query is recorded.
	if _, err := c.Health(bg(), modclient.HealthArgs{}); err != nil {
		t.Fatal(err)
	}
}

func TestHealth_FailedProbeIsRetriedWithoutWaitingAnInterval(t *testing.T) {
	f := modtest.NewFake(modtest.Behaviour{})
	var calls atomic.Int32
	f.SetHandler(func(q modtest.Request) (any, *modtest.WireError, time.Duration) {
		if q.Op == "health" && calls.Add(1) == 1 {
			return healthResult(map[string]any{"ok": false}), nil, 0
		}
		return nil, nil, 0
	})
	// The periodic interval is an hour: only the retry can bring the module up.
	c, _ := newClient(t, f, func(cfg *modclient.Config) {
		cfg.HealthInterval = time.Hour
		cfg.HealthRetryAfter = 20 * time.Millisecond
	})
	c.Start()
	waitFor(t, c.Usable, 3*time.Second, "the retry to find the module healthy")
	if f.CountOp("health") < 2 || f.Starts() != 1 {
		t.Errorf("health probes = %d starts = %d", f.CountOp("health"), f.Starts())
	}
}
