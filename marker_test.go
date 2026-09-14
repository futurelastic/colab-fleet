package fleet

import (
	"encoding/json"
	"strings"
	"testing"
)

// colab-fleet #165: `marker` is on the session wire only when a marker was
// recorded, and is a separate field from labels — a label keyed "marker" is
// just a label.
func TestSessionMarkerWireShape(t *testing.T) {
	state := ObservedState(StatusIdle, "fixture", nil)

	withMarker, err := json.Marshal(Session{SessionRef: SessionRef{ID: "a"}, Marker: "💬", State: state})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(withMarker), `"marker":"💬"`) {
		t.Errorf("marker missing from wire: %s", withMarker)
	}

	labelled, err := json.Marshal(Session{
		SessionRef: SessionRef{ID: "b"}, Labels: map[string]string{"marker": "x"}, State: state,
	})
	if err != nil {
		t.Fatal(err)
	}
	var back Session
	if err := json.Unmarshal(labelled, &back); err != nil {
		t.Fatal(err)
	}
	if back.Marker != "" {
		t.Errorf("a label keyed marker leaked into Session.Marker: %q", back.Marker)
	}
	if back.Labels["marker"] != "x" {
		t.Errorf("label keyed marker lost: %v", back.Labels)
	}
}
