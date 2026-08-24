package fleet

import (
	"encoding/json"
	"testing"
)

// colab-fleet #85: "not yet resolved" and "settled, none" must round-trip
// as distinguishable values — a caller that cannot tell them apart either
// polls forever on a session that will never have a surface, or gives up on
// one that would have had one.
func TestRuntimeSurfaceRef_PendingRoundTrips(t *testing.T) {
	out := PendingRuntimeSurface("remote control was requested; the runtime has not reported it yet")
	b, err := json.Marshal(out)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var back RuntimeSurfaceRef
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if back.Known != nil {
		t.Errorf("Known = %v, want nil (unresolved, never a settled no)", back.Known)
	}
	if back.Kind != "" || back.Target != "" || back.Source != "" {
		t.Errorf("pending ref carries kind/target/source: %+v", back)
	}
	if back.Evidence == "" {
		t.Error("a pending ref must still explain itself")
	}
}

func TestRuntimeSurfaceRef_SettledNoRoundTrips(t *testing.T) {
	out := NoRuntimeSurface("the create opted out of remote control")
	b, err := json.Marshal(out)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var back RuntimeSurfaceRef
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if back.Known == nil || *back.Known {
		t.Errorf("Known = %v, want false (settled)", back.Known)
	}
	if back.Kind != "" || back.Target != "" || back.Source != "" {
		t.Errorf("a settled-no ref carries kind/target/source: %+v", back)
	}
}

func TestRuntimeSurfaceRef_ResolvedRoundTrips(t *testing.T) {
	out := ResolvedRuntimeSurface(RuntimeSurfaceControlChannel, "fleet-abc123", RuntimeSurfaceDerived,
		"the runtime reports its own control channel active")
	b, err := json.Marshal(out)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var back RuntimeSurfaceRef
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if back.Known == nil || !*back.Known {
		t.Errorf("Known = %v, want true", back.Known)
	}
	if back.Kind != RuntimeSurfaceControlChannel {
		t.Errorf("Kind = %q, want %q", back.Kind, RuntimeSurfaceControlChannel)
	}
	if back.Target != "fleet-abc123" {
		t.Errorf("Target = %q, want fleet-abc123", back.Target)
	}
	if back.Source != RuntimeSurfaceDerived {
		t.Errorf("Source = %q, want derived", back.Source)
	}
}

// A ref that cannot support what it claims must neither encode nor decode —
// the same discipline every other §5.7 type in this package holds itself to.
func TestRuntimeSurfaceRef_RefusesToPresentSomethingItCannotSupport(t *testing.T) {
	known := true
	bad := []struct {
		name string
		ref  RuntimeSurfaceRef
	}{
		{"known true with no kind", RuntimeSurfaceRef{Known: &known, Target: "t", Source: RuntimeSurfaceObserved, Evidence: "e"}},
		{"known true with no target", RuntimeSurfaceRef{Known: &known, Kind: "k", Source: RuntimeSurfaceObserved, Evidence: "e"}},
		{"known true with no source", RuntimeSurfaceRef{Known: &known, Kind: "k", Target: "t", Evidence: "e"}},
		{"pending but carries a target anyway", RuntimeSurfaceRef{Target: "t", Evidence: "e"}},
		{"settled-no but carries a kind anyway", RuntimeSurfaceRef{Known: boolPtr(false), Kind: "k", Evidence: "e"}},
		{"no evidence at all", RuntimeSurfaceRef{}},
	}
	for _, c := range bad {
		if b, err := json.Marshal(c.ref); err == nil {
			t.Errorf("%s: must not encode, got %s", c.name, b)
		}
	}

	raw := []byte(`{"known":true,"kind":"k","target":"t","source":"not-a-real-source","evidence":"e"}`)
	var back RuntimeSurfaceRef
	if err := json.Unmarshal(raw, &back); err == nil {
		t.Errorf("an unrecognised source must not decode, got %+v", back)
	}

	good := PendingRuntimeSurface("not resolved yet")
	b, err := json.Marshal(good)
	if err != nil {
		t.Fatalf("a well-formed ref must encode: %v", err)
	}
	var got RuntimeSurfaceRef
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("round trip: %v", err)
	}
	if got.Evidence != good.Evidence || got.Known != nil {
		t.Errorf("round trip changed the ref: %+v vs %+v", got, *good)
	}
}
