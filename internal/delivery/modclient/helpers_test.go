package modclient_test

import (
	"fmt"
	"runtime"
	"runtime/pprof"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/godx-jp/colab-fleet/internal/delivery/modclient"
	"github.com/godx-jp/colab-fleet/internal/delivery/modclient/modtest"
)

// waitFor polls cond until it is true or d passes. The tests never sleep for a
// guessed time; they wait for the condition they mean.
func waitFor(t testing.TB, cond func() bool, d time.Duration, what ...string) {
	t.Helper()
	deadline := time.Now().Add(d)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out after %s waiting for %s", d, strings.Join(what, " "))
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// rec collects everything a Client reports through its callbacks.
type rec struct {
	mu      sync.Mutex
	counts  map[string]int
	logs    []string
	ready   []uint64
	changes int
}

func (r *rec) count(name string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.counts == nil {
		r.counts = map[string]int{}
	}
	r.counts[name]++
}

func (r *rec) Count(name string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.counts[name]
}

func (r *rec) logf(format string, args ...any) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.logs = append(r.logs, fmt.Sprintf(format, args...))
}

func (r *rec) Logs() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.logs...)
}

func (r *rec) onReady(gen uint64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.ready = append(r.ready, gen)
}

func (r *rec) Ready() []uint64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]uint64(nil), r.ready...)
}

func (r *rec) onChange() {
	r.mu.Lock()
	r.changes++
	r.mu.Unlock()
}

func (r *rec) Changes() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.changes
}

// fastConfig is a Config with every timer short enough that a whole test takes
// milliseconds, and the periodic health loop off unless a test turns it on.
func fastConfig(r *rec) modclient.Config {
	return modclient.Config{
		Name:              "fake",
		Path:              "/nonexistent/fake",
		Env:               []string{},
		HelloTimeout:      500 * time.Millisecond,
		BackoffMin:        10 * time.Millisecond,
		BackoffMax:        40 * time.Millisecond,
		BackoffResetAfter: time.Hour,
		HealthInterval:    -1,
		ShutdownGrace:     300 * time.Millisecond,
		Deadlines:         modclient.Deadlines{Default: 2 * time.Second, Prepare: 2 * time.Second, AttachExtra: 200 * time.Millisecond},
		Logf:              r.logf,
		Count:             r.count,
		OnReady:           r.onReady,
		OnChange:          r.onChange,
	}
}

// newClient builds (but does not start) a Client over the in-process fake f.
// On cleanup it stops the client and fails the test if goroutines were leaked.
func newClient(t *testing.T, f *modtest.Fake, mut ...func(*modclient.Config)) (*modclient.Client, *rec) {
	t.Helper()
	r := &rec{}
	cfg := fastConfig(r)
	if f != nil {
		cfg.Launcher = f.Launcher()
	}
	for _, m := range mut {
		m(&cfg)
	}
	base := runtime.NumGoroutine()
	c := modclient.New(cfg)
	t.Cleanup(func() { assertNoLeak(t, base) })
	t.Cleanup(c.Stop)
	return c, r
}

// startClient is newClient followed by Start and a wait for the module to be
// available.
func startClient(t *testing.T, f *modtest.Fake, mut ...func(*modclient.Config)) (*modclient.Client, *rec) {
	t.Helper()
	c, r := newClient(t, f, mut...)
	c.Start()
	waitFor(t, c.Usable, 3*time.Second, "the module to become usable")
	return c, r
}

// assertNoLeak fails when goroutines started during the test are still running
// after a short settle period.
func assertNoLeak(t *testing.T, base int) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for runtime.NumGoroutine() > base {
		if time.Now().After(deadline) {
			var b strings.Builder
			_ = pprof.Lookup("goroutine").WriteTo(&b, 1)
			t.Errorf("goroutines leaked: %d now, %d before\n%s", runtime.NumGoroutine(), base, b.String())
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
}
