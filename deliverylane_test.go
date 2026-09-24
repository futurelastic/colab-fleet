package fleet

import (
	"encoding/json"
	"reflect"
	"testing"
	"time"
)

func TestDeliveryLane_JSON(t *testing.T) {
	since := time.Date(2026, 9, 25, 1, 2, 3, 0, time.UTC)
	lane := DeliveryLane{Lane: "relay-a", ClientConnected: true, Evidence: "attach live", Since: since}
	b, err := json.Marshal(lane)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"lane":"relay-a","clientConnected":true,"evidence":"attach live","since":"2026-09-25T01:02:03Z"}`
	if string(b) != want {
		t.Fatalf("wire = %s, want %s", b, want)
	}
	var out DeliveryLane
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(out, lane) {
		t.Fatalf("round trip: %+v != %+v", out, lane)
	}
}

// A session with no lane encodes with no `delivery` key at all: absent is
// "not stated", never "terminal", and an older reader sees nothing new.
func TestDeliveryLane_AbsentOnSessionEncodesNoKey(t *testing.T) {
	b, err := json.Marshal(Session{State: SessionState{Status: StatusIdle, Confidence: ConfidenceInferred}})
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	if _, ok := m["delivery"]; ok {
		t.Fatalf("a session with no lane encoded a delivery key: %s", b)
	}
}

func TestDeliveryModuleStatus_JSON(t *testing.T) {
	yes := true
	in := DriverCapabilities{DeadlineMs: 1, Source: CapabilitiesObserved, DeliveryModules: []DeliveryModuleStatus{{
		Name: "relay-a", Status: DeliveryModuleAvailable, Protocol: 1, Version: "1.2.3",
		Platform: "darwin", PeerCheck: &yes, Lanes: map[string]int{"live": 2},
	}}}
	b, err := json.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	var out DriverCapabilities
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(out.DeliveryModules, in.DeliveryModules) {
		t.Fatalf("round trip: %+v != %+v", out.DeliveryModules, in.DeliveryModules)
	}
	// No module enabled: the key is absent, exactly as before #185.
	b, err = json.Marshal(DriverCapabilities{DeadlineMs: 1, Source: CapabilitiesObserved})
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	if _, ok := m["deliveryModules"]; ok {
		t.Fatalf("capabilities with no modules encoded deliveryModules: %s", b)
	}
}
