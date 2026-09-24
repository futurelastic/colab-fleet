package tmux

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	fleet "github.com/godx-jp/colab-fleet"
	"github.com/godx-jp/colab-fleet/internal/delivery/modclient"
	"github.com/godx-jp/colab-fleet/internal/delivery/modclient/modtest"
	"github.com/godx-jp/colab-fleet/internal/driver"
	"github.com/godx-jp/colab-fleet/internal/state"
)

// #185, create: what a session is offered, what it launches with, and what the
// record says. A module can only ever add a lane; it can never stop a create.

// A driver with no module enabled behaves exactly as it did before the feature:
// no state file, no field, no counter, no guard.
func TestModuleCreate_NoModules_ByteForByte(t *testing.T) {
	dir := t.TempDir()
	st, err := state.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	mux := twoSessions()
	d := New("testbox", withExec(mux.exec), withNonce(func() string { return testNonce }), WithState(st))

	// A name a module WOULD reserve is an ordinary variable here.
	sess, err := d.Create(context.Background(), testCaller, "key-1",
		fleet.SessionSpec{Name: "solo", Cwd: "/work/solo", Env: map[string]string{modPrefix + "X": "1"}})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	mux.addSession(fakeSession{name: sess.ID, paneID: "%9", cwd: "/work/solo", pid: 900, created: 1785600002, title: "2_1_220"}, idleFixtureFor("solo"))

	if _, err := os.Stat(filepath.Join(dir, lanesFileName+".json")); !os.IsNotExist(err) {
		t.Errorf("a machine with no module wrote a lanes file (err=%v)", err)
	}
	all, err := d.List(context.Background(), testCaller, driver.ListFilter{})
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range all.Items() {
		if s.Delivery != nil {
			t.Errorf("session %q carries a delivery field: %+v", s.ID, s.Delivery)
		}
	}
	for name := range d.Counters() {
		if strings.HasPrefix(name, "module.") {
			t.Errorf("counter %q exists with no module enabled", name)
		}
	}
	if caps := d.Capabilities(); caps.DeliveryModules != nil {
		t.Errorf("capabilities report modules: %+v", caps.DeliveryModules)
	}
	if got := d.ReservedEnvPrefixes(); got != nil {
		t.Errorf("reserved prefixes = %v, want none", got)
	}
}

// prepare-launch refuses: the session is created on the built-in lane and the
// record says why. The create is never affected.
func TestModuleCreate_PrepareErrorBuiltinWithReason(t *testing.T) {
	r := newModRig(t, rigOptions{behaviour: modtest.Behaviour{Ops: map[string]modtest.OpScript{
		"prepare-launch": {Error: &modtest.WireError{Code: "refused", Message: "no lane for you"}},
	}}})
	sess := r.create("alpha1", map[string]string{"CALLER_X": "1"})

	rec, ok := r.record(sess.ID)
	if !ok || rec.Module != "" || rec.LaneKey != "" {
		t.Fatalf("record = %+v, %v; want a reason-only record", rec, ok)
	}
	if !strings.Contains(rec.Reason, "refused prepare-launch") {
		t.Errorf("reason = %q, want it to say prepare-launch was refused", rec.Reason)
	}
	v := r.view(sess.ID)
	if v == nil || v.Lane != fleet.DeliveryLaneTerminal || v.ClientConnected || !strings.Contains(v.Evidence, "prepare-launch") {
		t.Errorf("view = %+v, want terminal with the reason as evidence", v)
	}
	if got := r.modCounter("prepare_refused"); got != 1 {
		t.Errorf("prepare_refused = %d, want 1", got)
	}
	if n := r.fake.CountOp("attach"); n != 0 {
		t.Errorf("a session with no lane was attached %d times", n)
	}
	if env := r.stagedEnv(); len(env) != 1 || env["CALLER_X"] != "1" {
		t.Errorf("staged env = %v, want only the caller's variable", env)
	}
}

// Success: the session→lane record is persisted BEFORE the process exists, and
// the attach runs afterwards, asynchronously.
func TestModuleCreate_LanePersisted(t *testing.T) {
	r := newModRig(t, rigOptions{})
	sess := r.create("alpha2", nil)

	var f lanesFile
	if ok, err := r.store.Load(lanesFileName, &f); err != nil || !ok {
		t.Fatalf("lanes file: ok=%v err=%v", ok, err)
	}
	rec := f.Lanes[sess.ID]
	if rec == nil || rec.Module != modName || rec.LaneKey == "" || rec.Cwd != "/work/alpha2" {
		t.Fatalf("persisted record = %+v", rec)
	}
	if got := r.modCounter("prepare_ok"); got != 1 {
		t.Errorf("prepare_ok = %d, want 1", got)
	}
	r.waitLive(sess.ID)
	if v := r.view(sess.ID); v.Lane != modName || !v.ClientConnected {
		t.Errorf("view = %+v", v)
	}
	// What the module holds is what the record says.
	var prepared, attached string
	for _, rec := range r.fake.Requests() {
		switch rec.Op {
		case "attach":
			var a modclient.AttachArgs
			mustUnmarshal(t, rec.Args, &a)
			attached = a.LaneKey
			if a.PID == 0 || a.Cwd != "/work/alpha2" {
				t.Errorf("attach args = %+v, want the pane's pid and the cwd", a)
			}
		}
	}
	prepared = f.Lanes[sess.ID].LaneKey
	if attached != prepared {
		t.Errorf("attached %q, prepared %q", attached, prepared)
	}
}

// The module's environment reaches the launched process exactly, alongside the
// caller's, and nowhere else: through the staged file, never a command line.
func TestModuleCreate_EnvMergedExactly(t *testing.T) {
	r := newModRig(t, rigOptions{behaviour: reserving(modtest.Behaviour{Ops: map[string]modtest.OpScript{
		"prepare-launch": {Results: []json.RawMessage{modtest.JSON(map[string]any{
			"laneKey": "lane-alpha", "claudeVersion": "1.0.0",
			"env": map[string]string{modPrefix + "LANE": "/p/lane one", modPrefix + "SOCK": "/p/sock"},
		})}},
	}})})
	r.create("alpha3", map[string]string{"CALLER_X": "1", "CALLER_Y": "two"})

	got := r.stagedEnv()
	want := map[string]string{
		"CALLER_X": "1", "CALLER_Y": "two",
		modPrefix + "LANE": "/p/lane one", modPrefix + "SOCK": "/p/sock",
	}
	if strings.Join(keysOf(got), ",") != strings.Join(keysOf(want), ",") {
		t.Fatalf("staged env names = %v, want %v", keysOf(got), keysOf(want))
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %q, want %q", k, got[k], v)
		}
	}
	// Never on a command line.
	for _, c := range r.mux.callsSnapshot() {
		for _, a := range c {
			if strings.Contains(a, "/p/lane one") || strings.Contains(a, "lane-alpha") {
				t.Errorf("a module value reached a command line: %v", c)
			}
		}
	}
}

// A module reaching outside the prefixes it reserved is refused wholesale: the
// session launches without a lane, the lane the module made is closed, and the
// bad variable never reaches the process.
func TestModuleCreate_ModuleEnvOutsidePrefixesRejected(t *testing.T) {
	r := newModRig(t, rigOptions{behaviour: reserving(modtest.Behaviour{Ops: map[string]modtest.OpScript{
		"prepare-launch": {Results: []json.RawMessage{modtest.JSON(map[string]any{
			"laneKey": "lane-beta", "claudeVersion": "1",
			"env": map[string]string{modPrefix + "OK": "/p", "PATH": "/evil"},
		})}},
	}})})
	sess := r.create("alpha4", map[string]string{"CALLER_X": "1"})

	rec, _ := r.record(sess.ID)
	if rec.Module != "" || !strings.Contains(rec.Reason, "outside the prefixes") {
		t.Fatalf("record = %+v, want a reason-only record naming the breach", rec)
	}
	env := r.stagedEnv()
	if _, bad := env["PATH"]; bad {
		t.Errorf("the module's PATH reached the launch: %v", env)
	}
	if _, bad := env[modPrefix+"OK"]; bad {
		t.Errorf("part of a refused answer was applied: %v", env)
	}
	waitFor(t, "the orphaned lane to be closed", func() bool { return r.fake.CountOp("close") == 1 })
	if got := r.modCounter("prepare_refused"); got != 1 {
		t.Errorf("prepare_refused = %d", got)
	}
}

// A caller may not set a name a module reserves: a 400-shaped refusal naming it,
// before anything is started or asked.
func TestModuleCreate_CallerReservedPrefixRefused(t *testing.T) {
	r := newModRig(t, rigOptions{behaviour: reserving(modtest.Behaviour{})})
	before := countCalls(r.mux, "new-session")
	_, err := r.d.Create(context.Background(), testCaller, "key-c",
		fleet.SessionSpec{Name: "alpha5", Cwd: "/work/alpha5", Env: map[string]string{modPrefix + "A": "x", "OK": "y"}})
	if err == nil || !strings.Contains(err.Error(), modPrefix+"A") || !strings.Contains(err.Error(), "reserved") {
		t.Fatalf("err = %v, want the reserved name refused", err)
	}
	if strings.Contains(err.Error(), "OK") {
		t.Errorf("the error names an unreserved variable: %v", err)
	}
	if countCalls(r.mux, "new-session") != before {
		t.Error("a session was started for a refused create")
	}
	if n := r.fake.CountOp("prepare-launch"); n != 0 {
		t.Errorf("the module was asked to prepare a lane for a refused create (%d)", n)
	}
	// The same guard is what the service asks the driver for.
	if got := r.d.ReservedEnvPrefixes(); len(got) != 1 || got[0] != modPrefix {
		t.Errorf("ReservedEnvPrefixes = %v", got)
	}
}

// A configured per-machine sessionEnv entry under a reserved prefix is dropped:
// the module is the only source of a reserved name.
func TestModuleCreate_SessionEnvReservedPrefixDropped(t *testing.T) {
	file := filepath.Join(t.TempDir(), "value")
	if err := os.WriteFile(file, []byte("from-config\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	r := newModRig(t, rigOptions{
		behaviour: reserving(modtest.Behaviour{}),
		driverOpts: []Option{WithSessionEnv([]SessionEnvEntry{
			{Name: modPrefix + "S", FromFile: file},
			{Name: "MACHINE_ID", FromFile: file},
		})},
	})
	r.create("alpha6", nil)
	env := r.stagedEnv()
	if _, bad := env[modPrefix+"S"]; bad {
		t.Errorf("a configured value under a reserved prefix reached the launch: %v", env)
	}
	if env["MACHINE_ID"] != "from-config" {
		t.Errorf("an unreserved configured value was dropped too: %v", env)
	}
	if got := r.counter("module.session_env_dropped"); got != 1 {
		t.Errorf("session_env_dropped = %d, want 1", got)
	}
}

// The guard does not depend on the module being installed or running: the
// prefixes were persisted the last time it declared them.
func TestModuleCreate_GuardHoldsWithModuleNotInstalled(t *testing.T) {
	r := newModRig(t, rigOptions{
		notInstalled: true,
		seed:         &lanesFile{Prefixes: map[string][]string{modName: {modPrefix}}},
	})
	if got := r.d.ReservedEnvPrefixes(); len(got) != 1 || got[0] != modPrefix {
		t.Fatalf("ReservedEnvPrefixes = %v, want the persisted prefix", got)
	}
	_, err := r.d.Create(context.Background(), testCaller, "key-n",
		fleet.SessionSpec{Name: "alpha7", Cwd: "/work/alpha7", Env: map[string]string{modPrefix + "A": "x"}})
	if err == nil || !strings.Contains(err.Error(), modPrefix+"A") {
		t.Fatalf("err = %v, want the reserved name refused with no module installed", err)
	}
	// And a create without it is unaffected: the session takes the built-in lane
	// and the record says the module is not installed.
	sess := r.create("alpha8", nil)
	rec, _ := r.record(sess.ID)
	if rec.Module != "" || !strings.Contains(rec.Reason, "not installed") {
		t.Errorf("record = %+v", rec)
	}
	st := r.d.Capabilities().DeliveryModules
	if len(st) != 1 || st[0].Name != modName || st[0].Status != fleet.DeliveryModuleUnavailable {
		t.Errorf("capabilities = %+v", st)
	}
}

// The create response never waits on the attach.
func TestModuleCreate_AttachAsyncDoesNotDelayCreate(t *testing.T) {
	r := newModRig(t, rigOptions{})
	g := newGate()
	r.fake.SetHandler(func(req modtest.Request) (any, *modtest.WireError, time.Duration) {
		if req.Op == "attach" {
			g.wait(req.Ctx)
		}
		return nil, nil, 0
	})
	start := time.Now()
	sess := r.create("alpha9", nil)
	if took := time.Since(start); took > 2*time.Second {
		t.Fatalf("create took %s; it must not wait on the attach", took)
	}
	if v := r.view(sess.ID); v == nil || v.ClientConnected {
		t.Fatalf("view = %+v, want the built-in lane until the attach answers", v)
	}
	g.release()
	r.waitLive(sess.ID)
}

// A module whose handshake was refused is disabled: sessions are still created,
// on the built-in lane, and the reason says so.
func TestModuleCreate_DisabledModuleBuiltinLane(t *testing.T) {
	r := newModRig(t, rigOptions{noStart: true, behaviour: modtest.Behaviour{Hello: &modtest.HelloScript{Protocol: modtest.Int(99)}}})
	r.d.StartDeliveryModules()
	waitFor(t, "the module to be refused", func() bool { return r.client().Status().State == modclient.StateDisabled })

	sess := r.create("alpha10", nil)
	rec, _ := r.record(sess.ID)
	if rec.Module != "" || !strings.Contains(rec.Reason, "disabled") {
		t.Fatalf("record = %+v, want the disabled reason", rec)
	}
	st := r.d.Capabilities().DeliveryModules
	if len(st) != 1 || st[0].Status != fleet.DeliveryModuleDisabled || st[0].Reason == "" {
		t.Errorf("capabilities = %+v", st)
	}
	// The built-in path still carries a send.
	got := r.send(sess.ID, "hello", driver.SendOptions{})
	if got.Outcome != fleet.OutcomeQueued || got.RouteOf() != fleet.RouteTerminal {
		t.Errorf("receipt = %+v", got)
	}
}

// A create that lands before the module is ready takes the built-in lane and
// says why; nothing waits.
func TestModuleCreate_BeforeTheModuleIsReady(t *testing.T) {
	r := newModRig(t, rigOptions{noStart: true})
	sess := r.create("alpha11", nil)
	rec, _ := r.record(sess.ID)
	if rec.Module != "" || !strings.Contains(rec.Reason, "not ready") {
		t.Fatalf("record = %+v", rec)
	}
	if n := r.fake.CountOp("prepare-launch"); n != 0 {
		t.Errorf("prepare-launch ran %d times against a module that was not ready", n)
	}
}

func TestModuleAttach_LiveSetsLane(t *testing.T) {
	r := newModRig(t, rigOptions{})
	sess := r.create("attach1", nil)
	r.waitLive(sess.ID)
	if got := r.modCounter("attach_live"); got != 1 {
		t.Errorf("attach_live = %d", got)
	}
	v := r.view(sess.ID)
	if v.Lane != modName || !v.ClientConnected || v.Since.IsZero() || v.Evidence == "" {
		t.Errorf("view = %+v", v)
	}
	rec, _ := r.record(sess.ID)
	if rec.PID == 0 || rec.State != laneStateLive {
		t.Errorf("record = %+v, want the pane's pid and a live state", rec)
	}
	if st := r.d.Capabilities().DeliveryModules; len(st) != 1 || st[0].Status != fleet.DeliveryModuleAvailable ||
		st[0].Lanes[laneStateLive] != 1 || st[0].PeerCheck == nil || !*st[0].PeerCheck {
		t.Errorf("capabilities = %+v", st)
	}
}

// live:false leaves the session on the built-in lane, with the module's reason.
func TestModuleAttach_NotLiveStaysTerminal(t *testing.T) {
	r := newModRig(t, rigOptions{behaviour: modtest.Behaviour{Ops: map[string]modtest.OpScript{
		"attach": {Results: []json.RawMessage{modtest.JSON(map[string]any{
			"live": false, "state": "connecting", "reason": "agent-not-ready", "sinceMs": 5})}},
	}}})
	sess := r.create("attach2", nil)
	waitFor(t, "the attach to be recorded", func() bool { return r.modCounter("attach_not_live") == 1 })
	v := r.view(sess.ID)
	if v.Lane != fleet.DeliveryLaneTerminal || v.ClientConnected || !strings.Contains(v.Evidence, "agent-not-ready") {
		t.Errorf("view = %+v", v)
	}
	// A reason that is not "record/socket missing" is not polled.
	time.Sleep(150 * time.Millisecond)
	if n := r.fake.CountOp("attach"); n != 1 {
		t.Errorf("attach ran %d times for a non-retryable reason, want 1", n)
	}
}

// session-record-missing is retried: the agent may be held back by a first-run
// trust dialog. It goes live as soon as the module says so.
func TestModuleAttach_RecordMissingRetried(t *testing.T) {
	missing := modtest.JSON(map[string]any{"live": false, "state": "connecting", "reason": "session-record-missing", "sinceMs": 1})
	live := modtest.JSON(map[string]any{"live": true, "state": "live", "sinceMs": 9})
	r := newModRig(t, rigOptions{behaviour: modtest.Behaviour{Ops: map[string]modtest.OpScript{
		"attach": {Results: []json.RawMessage{missing, missing, missing, live}},
	}}})
	sess := r.create("attach3", nil)
	r.waitLive(sess.ID)
	if n := r.fake.CountOp("attach"); n != 4 {
		t.Errorf("attach ran %d times, want 4 (three misses then live)", n)
	}
}

// ...but only up to the window: a record that never appears stops the poll.
func TestModuleAttach_RecordMissingStopsAtTheWindow(t *testing.T) {
	missing := modtest.JSON(map[string]any{"live": false, "state": "connecting", "reason": "socket-missing", "sinceMs": 1})
	r := newModRig(t, rigOptions{
		behaviour: modtest.Behaviour{Ops: map[string]modtest.OpScript{"attach": {Results: []json.RawMessage{missing}}}},
		tune:      func(c *ModulesConfig) { c.AttachWindow = 150 * time.Millisecond },
	})
	sess := r.create("attach4", nil)
	waitFor(t, "the poll to stop", func() bool {
		rec, _ := r.record(sess.ID)
		return !rec.attaching && r.fake.CountOp("attach") > 1
	})
	n := r.fake.CountOp("attach")
	time.Sleep(120 * time.Millisecond)
	if r.fake.CountOp("attach") != n {
		t.Error("the poll outlived its window")
	}
	if v := r.view(sess.ID); v.ClientConnected {
		t.Errorf("view = %+v", v)
	}
}

// A laneKey full of path separators is just an opaque string: kept as given,
// sent as given, and never turned into a path anywhere.
func TestModuleLane_OpaqueKeyNeverAPath(t *testing.T) {
	const key = "../../etc/passwd/../x"
	r := newModRig(t, rigOptions{behaviour: modtest.Behaviour{Ops: map[string]modtest.OpScript{
		"prepare-launch": {Results: []json.RawMessage{modtest.JSON(map[string]any{"laneKey": key, "claudeVersion": "1", "env": map[string]string{}})}},
	}}})
	sess := r.create("opaque1", nil)
	r.waitLive(sess.ID)
	rec, _ := r.record(sess.ID)
	if rec.LaneKey != key {
		t.Fatalf("laneKey = %q, want it kept byte for byte", rec.LaneKey)
	}
	got := r.send(sess.ID, "hello", driver.SendOptions{})
	if got.RouteOf() != fleet.RouteModule {
		t.Fatalf("receipt = %+v", got)
	}
	if a := r.lastSendArgs(); a.LaneKey != key {
		t.Errorf("send carried laneKey %q", a.LaneKey)
	}
	// Nothing was created anywhere named after it: the state directory holds
	// only what the driver itself writes.
	var entries []string
	_ = filepath.Walk(r.dir, func(p string, info os.FileInfo, err error) error {
		if err == nil && strings.Contains(p, "passwd") {
			entries = append(entries, p)
		}
		return nil
	})
	if len(entries) != 0 {
		t.Errorf("a path was built from the lane key: %v", entries)
	}
}

func TestCapabilities_DeliveryModules(t *testing.T) {
	r := newModRig(t, rigOptions{enabled: []string{modName, "relay-b"}})
	st := r.d.Capabilities().DeliveryModules
	if len(st) != 2 || st[0].Name != modName || st[1].Name != "relay-b" {
		t.Fatalf("capabilities = %+v, want both enabled modules in the configured order", st)
	}
	if st[0].Status != fleet.DeliveryModuleAvailable || st[0].Version != "0.0.1" || st[0].Platform != "test" || st[0].Protocol != 1 {
		t.Errorf("installed module = %+v", st[0])
	}
	if st[1].Status != fleet.DeliveryModuleUnavailable || !strings.Contains(st[1].Reason, "not installed") {
		t.Errorf("absent module = %+v", st[1])
	}
	// It is what /v1/runtimes serialises, and a module's own text is sanitised.
	if err := r.d.Capabilities().Validate(); err != nil {
		t.Errorf("Validate: %v", err)
	}
}

func mustUnmarshal(t *testing.T, raw []byte, v any) {
	t.Helper()
	if err := json.Unmarshal(raw, v); err != nil {
		t.Fatal(err)
	}
}
