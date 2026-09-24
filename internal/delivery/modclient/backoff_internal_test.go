package modclient

import (
	"testing"
	"time"
)

func TestNextBackoff(t *testing.T) {
	cfg := Config{BackoffMin: 10 * time.Millisecond, BackoffMax: 50 * time.Millisecond, BackoffResetAfter: time.Second}
	wait := cfg.BackoffMin
	var waits []time.Duration
	for i := 0; i < 6; i++ {
		var w time.Duration
		w, wait = nextBackoff(wait, 0, cfg) // children that die at once
		waits = append(waits, w)
	}
	want := []time.Duration{10, 20, 40, 50, 50, 50}
	for i, w := range want {
		if waits[i] != w*time.Millisecond {
			t.Fatalf("waits = %v, want doubling from 10ms capped at 50ms", waits)
		}
	}

	// A child that stayed up long enough earns a fresh start.
	if w, next := nextBackoff(50*time.Millisecond, time.Second, cfg); w != 10*time.Millisecond || next != 20*time.Millisecond {
		t.Errorf("after a long run: wait=%v next=%v, want a reset to the minimum", w, next)
	}
	// One that ran, but not long enough, does not.
	if w, _ := nextBackoff(40*time.Millisecond, time.Second-time.Millisecond, cfg); w != 40*time.Millisecond {
		t.Errorf("after a short run: wait=%v, want the backoff to keep growing", w)
	}
}

func TestConfigDefaults(t *testing.T) {
	c := Config{}.withDefaults()
	if c.HelloTimeout != 3*time.Second || c.BackoffMin != time.Second || c.BackoffMax != 60*time.Second ||
		c.BackoffResetAfter != 60*time.Second || c.HealthInterval != 30*time.Second || c.ShutdownGrace != 2*time.Second || c.HealthRetryAfter != time.Second ||
		c.Deadlines != (Deadlines{Default: 15 * time.Second, Prepare: 5 * time.Second, AttachExtra: 5 * time.Second}) {
		t.Errorf("defaults = %+v", c)
	}
	if c.Launcher == nil {
		t.Error("a nil Launcher must default to ExecLauncher")
	}
	if got := (Config{HealthInterval: -1}).withDefaults().HealthInterval; got >= 0 {
		t.Errorf("a negative HealthInterval must stay negative (periodic loop off), got %v", got)
	}
	if got := (Config{BackoffMin: time.Minute, BackoffMax: time.Second}).withDefaults(); got.BackoffMax < got.BackoffMin {
		t.Errorf("BackoffMax %v below BackoffMin %v", got.BackoffMax, got.BackoffMin)
	}
}
