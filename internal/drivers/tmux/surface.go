package tmux

import fleet "github.com/godx-jp/colab-fleet"

// runtimeSurfaceFor turns a create record into a fleet.RuntimeSurfaceRef —
// colab-fleet #85. The record's own SurfaceSeen is the latch this driver
// commits to once corroborated (see List's row loop, where it is set): once
// true it is never read back to false here, which is what makes this an
// identity answer rather than a health one (state.controlChannel is the
// field for health — see the type's own doc comment on why the two must
// not be conflated).
func runtimeSurfaceFor(rec createRecord, name string) *fleet.RuntimeSurfaceRef {
	if rec.SurfaceSeen {
		return fleet.ResolvedRuntimeSurface(fleet.RuntimeSurfaceControlChannel, name, fleet.RuntimeSurfaceDerived,
			"the runtime reports its own control channel active; the identifier it was given "+
				"at creation is this session's resolved name, which is also its multiplexer "+
				"session name and the name the agent calls itself")
	}
	if !rec.SurfaceRequested {
		return fleet.NoRuntimeSurface("the create opted out of remote control (remoteControl: false), " +
			"so the runtime was never asked to register a surface")
	}
	return fleet.PendingRuntimeSurface("remote control was requested at creation; the runtime registers " +
		"its surface after the process starts and has not reported it yet — the identifier it was asked " +
		"to register under is this session's own resolved name")
}
