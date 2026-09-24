package tmux

import (
	"testing"

	fleet "github.com/godx-jp/colab-fleet"
	"github.com/godx-jp/colab-fleet/internal/delivery/modclient"
	"github.com/godx-jp/colab-fleet/internal/delivery/modclient/modtest"
	"github.com/godx-jp/colab-fleet/internal/driver"
)

// The whole path, over a REAL child process: a module executable is discovered
// in a modules directory, enabled by name, spawned by the real launcher with a
// minimal environment, asked for a lane at create, attached, sent a message, and
// asked to confirm it — and stopped by closing its stdin.
func TestModuleEndToEnd_DiscoveredEnabledDeliversConfirms(t *testing.T) {
	dir := t.TempDir()
	path := modtest.Install(t, dir, modName, reserving(modtest.Behaviour{}))

	found, problems := modclient.Discover(dir, []string{modName})
	if len(problems) != 0 || len(found) != 1 || found[0].Path != path {
		t.Fatalf("discovery: found %+v, problems %+v", found, problems)
	}

	r := newModRig(t, rigOptions{realPath: found[0].Path})
	sess := r.create("e2e1", nil)
	r.waitLive(sess.ID)

	got := r.send(sess.ID, "over a real process", driver.SendOptions{})
	if got.RouteOf() != fleet.RouteModule || got.ModuleOf() != modName || got.Outcome != fleet.OutcomeQueued {
		t.Fatalf("receipt = %+v, want queued, carried by the module", got)
	}
	if r.pastes() != 0 {
		t.Error("the built-in path was touched")
	}
	st := r.d.Capabilities().DeliveryModules
	if len(st) != 1 || st[0].Status != fleet.DeliveryModuleAvailable || st[0].Lanes[laneStateLive] != 1 {
		t.Errorf("capabilities = %+v", st)
	}
	if rp := r.d.ReservedEnvPrefixes(); len(rp) != 1 || rp[0] != modPrefix {
		t.Errorf("the module's reserved prefix was not learned: %v", rp)
	}
	r.d.StopDeliveryModules()
	if r.client().Usable() {
		t.Error("the module is still usable after Stop")
	}
}
