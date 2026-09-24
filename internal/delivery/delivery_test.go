package delivery

import (
	"reflect"
	"strings"
	"testing"

	fleet "github.com/godx-jp/colab-fleet"
)

func TestObserveRecordsOutcomeClassAndSignal(t *testing.T) {
	got := map[string]int{}
	incr := func(n string) { got[n]++ }
	Observe(incr, "m", Result{Receipt: fleet.DeliveryReceipt{Outcome: fleet.OutcomeQueued}, Class: Delivered, ConfirmedBy: SignalTranscript})
	Observe(incr, "m", Result{Receipt: fleet.DeliveryReceipt{Outcome: fleet.OutcomeRefused}})
	want := map[string]int{
		"delivery.m.outcome.queued": 1, "delivery.m.delivered": 1, "delivery.m.confirmed.transcript": 1,
		"delivery.m.outcome.refused": 1, "delivery.m.unclassified": 1,
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestCheckReservedEnvNamesEveryOffenderSorted(t *testing.T) {
	err := CheckReservedEnv(map[string]string{"B": "", "A": "", "C": ""}, []string{"B", "A", "Z"})
	if err == nil || !strings.Contains(err.Error(), "A, B") {
		t.Fatalf("err = %v", err)
	}
	if err := CheckReservedEnv(map[string]string{"C": ""}, []string{"A"}); err != nil {
		t.Fatal(err)
	}
	if err := CheckReservedEnv(map[string]string{"A": ""}, nil); err != nil {
		t.Fatal(err)
	}
}
