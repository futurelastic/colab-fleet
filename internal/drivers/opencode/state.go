package opencode

import (
	"fmt"

	fleet "github.com/godx-jp/colab-fleet"
)

// classify turns one session's entry in a SUCCESSFULLY-read status map —
// or its demonstrated absence from one — into a fleet.SessionState.
//
// present must be true only when the read that produced this map is known
// to have succeeded AND the session is known to exist (checked by the
// caller before this is reached, e.g. via a prior 200 on GET /session/{id}
// or membership in a GET /session listing). classify never decides
// existence; it only decides busy/retry/idle GIVEN existence, which is the
// shape #55's ruling asks for: "present ⇒ busy/retry; absent ⇒ idle ONLY
// IF THE READ ITSELF DEMONSTRABLY SUCCEEDED." A failed read must never
// reach this function at all — see ops.go, which returns the read's error
// directly instead of calling classify.
func classify(present bool, st wireStatus) fleet.SessionState {
	if !present {
		return fleet.ObservedState(fleet.StatusIdle,
			"absent from the runtime's status map: no active turn reported", nil)
	}
	switch st.Type {
	case "busy":
		return fleet.ObservedState(fleet.StatusWorking, "runtime reports an active turn", nil)
	case "retry":
		// colab-fleet issue #52's direction note, repeated in #55: the
		// runtime's own "retry" has no member on fleet.Status and must
		// not get one. It maps onto working plus TurnEnd.Retryable —
		// "poke-it-and-it-continues", not a status of its own. Outcome is
		// deliberately left empty: the turn has not ended, so "failed"
		// would be a claim this state does not make.
		s := fleet.ObservedState(fleet.StatusWorking,
			fmt.Sprintf("retrying turn (attempt %d): %s", st.Attempt, st.Message), nil)
		s.LastTurn = &fleet.TurnEnd{Retryable: true, Reason: st.Message}
		return s
	case "idle":
		// Measured behaviour (#55) omits idle sessions from the map
		// entirely, so this arm should be unreachable in practice. It is
		// kept as a defensive, honest no-op rather than falling into
		// "unrecognised" below, in case a future release starts reporting
		// it explicitly — the classification is the same either way.
		return fleet.ObservedState(fleet.StatusIdle, "runtime explicitly reported idle", nil)
	default:
		// A status type this driver does not recognise is §2.3's "carry,
		// don't flatten" rule with nothing to carry it in: fleet.Status is
		// a closed set (§2.3, F52's own lesson) and must not be extended
		// per-driver. Reporting unknown, honestly, is the only answer
		// that does not silently misclassify a future runtime release.
		return fleet.UnknownState(fleet.ConfidenceObserved,
			fmt.Sprintf("runtime reported an unrecognised status type %q", st.Type))
	}
}
