package opencode

import (
	"testing"

	fleet "github.com/godx-jp/colab-fleet"
)

// classify is the heart of #55's mapping ruling: present ⇒ busy/retry
// (observed); absent ⇒ idle ONLY given a demonstrated existence/success.
// classify itself never sees a failed read (ops.go returns those as
// errors before reaching it) — these tests cover only the
// given-a-successful-read half of the discrimination; TestState_* and
// TestList_* in ops_test.go cover the failed-read half.
func TestClassify_BusyMapsToWorkingObserved(t *testing.T) {
	st := classify(true, wireStatus{Type: "busy"})
	if st.Status != fleet.StatusWorking {
		t.Errorf("Status = %q, want working", st.Status)
	}
	if st.Confidence != fleet.ConfidenceObserved {
		t.Errorf("Confidence = %q, want observed", st.Confidence)
	}
	if st.LastTurn != nil {
		t.Errorf("LastTurn = %+v, want nil for a plain busy read", st.LastTurn)
	}
}

// The runtime's "retry" must not become a seventh Status value (#52, #55):
// it maps onto working plus LastTurn.Retryable, never onto its own status.
func TestClassify_RetryMapsToWorkingPlusRetryableLastTurn_NeverItsOwnStatus(t *testing.T) {
	st := classify(true, wireStatus{Type: "retry", Attempt: 3, Message: "rate limited, backing off"})

	if st.Status != fleet.StatusWorking {
		t.Errorf("Status = %q, want working — retry must not get its own Status value (#52)", st.Status)
	}
	if st.Confidence != fleet.ConfidenceObserved {
		t.Errorf("Confidence = %q, want observed", st.Confidence)
	}
	if st.LastTurn == nil {
		t.Fatal("LastTurn is nil, want a TurnEnd carrying the retry")
	}
	if !st.LastTurn.Retryable {
		t.Error("LastTurn.Retryable = false, want true")
	}
	if st.LastTurn.Reason != "rate limited, backing off" {
		t.Errorf("LastTurn.Reason = %q, want the runtime's own message", st.LastTurn.Reason)
	}
	if st.LastTurn.Outcome != "" {
		t.Errorf("LastTurn.Outcome = %q, want empty — a retry has not ended the turn", st.LastTurn.Outcome)
	}
}

// Absence, given a read that is known to have succeeded, is idle — the
// measured shape of #55: a session created but never prompted, and a
// session that finished its turn, are BOTH absent from the map and BOTH
// legitimately idle. This driver does not attempt to split that collapse
// any further than the runtime itself can (documented, not solved).
func TestClassify_AbsentGivenSuccessfulReadIsIdleObserved(t *testing.T) {
	st := classify(false, wireStatus{})
	if st.Status != fleet.StatusIdle {
		t.Errorf("Status = %q, want idle", st.Status)
	}
	if st.Confidence != fleet.ConfidenceObserved {
		t.Errorf("Confidence = %q, want observed — this driver DID successfully read the runtime, it just found nothing", st.Confidence)
	}
}

func TestClassify_ExplicitIdleIsAlsoIdle(t *testing.T) {
	st := classify(true, wireStatus{Type: "idle"})
	if st.Status != fleet.StatusIdle {
		t.Errorf("Status = %q, want idle", st.Status)
	}
}

// A status type this driver has never seen must not be guessed at — carry
// it as unknown rather than flatten it into idle or working.
func TestClassify_UnrecognisedTypeIsUnknown_NeverGuessed(t *testing.T) {
	st := classify(true, wireStatus{Type: "some-future-type"})
	if st.Status != fleet.StatusUnknown {
		t.Errorf("Status = %q, want unknown for an unrecognised type", st.Status)
	}
	if st.Confidence != fleet.ConfidenceObserved {
		t.Errorf("Confidence = %q, want observed — the runtime DID answer, just with something new", st.Confidence)
	}
}
