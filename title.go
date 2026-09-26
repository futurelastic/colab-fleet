package fleet

import (
	"encoding/json"
	"errors"
)

// TitleSyncStatus is the closed vocabulary TitleSync reports (colab-fleet
// #222): a rename changes a session's id unconditionally (Ack.Accepted /
// RenameAck.Accepted); on a substrate whose runtime ALSO keeps its own idea
// of a title, apart from the id, this is whether that second, separate fact
// was brought along too.
//
// This is deliberately its own vocabulary rather than a reuse of Outcome
// (delivery.go). Outcome answers "was a message delivered", and its
// `queued` already means something a title sync must not borrow: the
// terminal path reports `queued` for a submit the SCREEN confirmed, and the
// whole point of this type is that a title is never reported synced on
// screen evidence — only on the runtime's own record. Receipt below still
// carries the underlying send's own Outcome verbatim, so a caller reading
// "the same rules as /input" applied to this delivery can still see them;
// Status is a judgement ABOUT that receipt, not a copy of it.
type TitleSyncStatus string

const (
	// TitleSynced means the runtime's own record — never the screen — now
	// carries the new name as its title.
	TitleSynced TitleSyncStatus = "synced"
	// TitlePending means a title-sync delivery was attempted and neither
	// confirmed nor refused within the time this call had: the composer
	// may have accepted it, but nothing yet proves the runtime's title
	// moved. A caller may retry by renaming to the SAME name again — this
	// re-attempts only the title half; the id has nothing left to change.
	TitlePending TitleSyncStatus = "pending"
	// TitleFailed means the title half did not happen, and, on this call's
	// own evidence, will not happen without the caller doing something
	// about it: a busy or stranded composer refused the delivery (nothing
	// was overwritten — see Receipt.Reason), the runtime recorded a
	// DIFFERENT title than the one asked for, or the name itself was
	// unusable and never even sent. This is never the multiplexer rename
	// failing — that is reported through Accepted/the driver error before
	// a title sync is ever attempted.
	TitleFailed TitleSyncStatus = "failed"
	// TitleNotApplicable means this session's runtime keeps no title of
	// its own apart from the id a rename already changed, so there is
	// nothing for this half to do. Produced ONLY by the service, for a
	// driver that does not implement TitleSyncer (internal/service's
	// rename_title.go) — a driver implementing the capability for its own
	// runtime never returns this value itself; see driver.TitleSyncer.
	TitleNotApplicable TitleSyncStatus = "not_applicable"
)

func (s TitleSyncStatus) valid() bool {
	switch s {
	case TitleSynced, TitlePending, TitleFailed, TitleNotApplicable:
		return true
	default:
		return false
	}
}

// TitleSync is RenameAck's second half. See TitleSyncStatus for the
// vocabulary and RenameAck for why this is not folded into the bare Ack
// every other intent-only operation returns.
type TitleSync struct {
	Status TitleSyncStatus `json:"status"`

	// Evidence is prose for humans, present in every state — never parsed
	// (§2.3's rule; the same discipline PromptDelivery.Evidence already
	// follows, applied here to the identical shape of problem: a status a
	// machine can branch on, plus prose a person can read, kept apart).
	Evidence string `json:"evidence"`

	// Receipt is the "/rename <name>" delivery's own DeliveryReceipt, when
	// a delivery was actually attempted — nil when nothing was ever sent
	// (TitleNotApplicable always; TitleFailed sometimes, when the name
	// itself was refused before any send — e.g. it contained a control
	// character, or no session could be found to send it to). This is the
	// SAME Outcome vocabulary an ordinary send already returns, carried
	// verbatim rather than re-encoded into Status, so "a busy or stranded
	// composer is refused under the same rules as /input" is something a
	// caller can read directly off Receipt.Reason rather than take on
	// faith from Status alone.
	Receipt *DeliveryReceipt `json:"receipt,omitempty"`
}

func (t TitleSync) coherent() bool {
	if !t.Status.valid() || t.Evidence == "" {
		return false
	}
	if t.Status == TitleNotApplicable && t.Receipt != nil {
		return false
	}
	return true
}

// ErrTitleSyncIncoherent is returned when a TitleSync would state something
// it cannot support — an invalid status, empty evidence, or a receipt
// alongside TitleNotApplicable (nothing is ever sent when a runtime has no
// title concept to sync).
var ErrTitleSyncIncoherent = errors.New(
	"fleet: a TitleSync must carry a valid status and non-empty evidence, and never a receipt when its status is not_applicable")

type titleSyncWire TitleSync

// MarshalJSON refuses an incoherent value rather than emitting one — the
// same discipline PromptDelivery and DeliveryPath already apply: a status
// outside the closed set, or a receipt paired with not_applicable, would
// otherwise decode on the other end as a plausible-looking but false state.
func (t TitleSync) MarshalJSON() ([]byte, error) {
	if !t.coherent() {
		return nil, ErrTitleSyncIncoherent
	}
	return json.Marshal(titleSyncWire(t))
}

// UnmarshalJSON applies the identical rule to what a peer sends — a
// federated read is not a more trustworthy source than our own encoder.
func (t *TitleSync) UnmarshalJSON(b []byte) error {
	var w titleSyncWire
	if err := json.Unmarshal(b, &w); err != nil {
		return err
	}
	got := TitleSync(w)
	if !got.coherent() {
		return ErrTitleSyncIncoherent
	}
	*t = got
	return nil
}

// TitleSyncSynced reports that the runtime's own record now carries name as
// its title — receipt is the delivery that confirmed it, never nil (there
// is no way to observe a synced title without having sent something).
func TitleSyncSynced(evidence string, receipt DeliveryReceipt) TitleSync {
	return TitleSync{Status: TitleSynced, Evidence: evidence, Receipt: &receipt}
}

// TitleSyncPending reports that a title-sync delivery was attempted and its
// outcome has not resolved either way within the time this call had.
// receipt is nil only when nothing could even be attempted (for instance,
// no transcript could be resolved to confirm against, so nothing further
// was tried) — pass the DeliveryReceipt whenever a send was actually made.
func TitleSyncPending(evidence string, receipt *DeliveryReceipt) TitleSync {
	return TitleSync{Status: TitlePending, Evidence: evidence, Receipt: receipt}
}

// TitleSyncFailed reports that the title half did not happen and will not
// without the caller acting again. receipt is nil when nothing was ever
// sent (an unusable name, or no session found to address).
func TitleSyncFailed(evidence string, receipt *DeliveryReceipt) TitleSync {
	return TitleSync{Status: TitleFailed, Evidence: evidence, Receipt: receipt}
}

// TitleSyncNotApplicable reports that this session's runtime keeps no title
// of its own apart from its id. Called only from the service, for a driver
// that does not implement TitleSyncer — see this type's own doc comment.
func TitleSyncNotApplicable(evidence string) TitleSync {
	return TitleSync{Status: TitleNotApplicable, Evidence: evidence}
}
