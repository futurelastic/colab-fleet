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

func TestCheckReservedEnvPrefixes(t *testing.T) {
	err := CheckReservedEnvPrefixes(map[string]string{"MOD_B": "", "MOD_A": "", "OK": ""}, []string{"MOD_"})
	if err == nil || !strings.Contains(err.Error(), "MOD_A, MOD_B") || strings.Contains(err.Error(), "OK") {
		t.Fatalf("err = %v", err)
	}
	if err := CheckReservedEnvPrefixes(map[string]string{"OK": ""}, []string{"MOD_"}); err != nil {
		t.Fatal(err)
	}
	if err := CheckReservedEnvPrefixes(map[string]string{"MOD_A": ""}, nil); err != nil {
		t.Fatal(err)
	}
	// An empty prefix reserves nothing: a malformed reservation must never
	// become "every variable is reserved".
	if err := CheckReservedEnvPrefixes(map[string]string{"ANY": ""}, []string{""}); err != nil {
		t.Fatalf("an empty prefix matched: %v", err)
	}
	if HasReservedPrefix("mod_a", []string{"MOD_"}) {
		t.Error("prefix match must be case-sensitive")
	}
}

func TestValidModuleName(t *testing.T) {
	for _, ok := range []string{"a", "relay", "relay-a", "r2", "a-b-c"} {
		if !ValidModuleName(ok) {
			t.Errorf("%q refused", ok)
		}
	}
	long := strings.Repeat("a", 33)
	for _, bad := range []string{"", "-a", "2a", "A", "re lay", "re/lay", "..", "a.b", "a_b", long,
		"auto", "terminal", "inbox", "module", "relay\n"} {
		if ValidModuleName(bad) {
			t.Errorf("%q accepted", bad)
		}
	}
	if !ValidModuleName(strings.Repeat("a", 32)) {
		t.Error("a 32-byte name is the longest legal one")
	}
}
