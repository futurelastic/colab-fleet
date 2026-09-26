package fleet

import (
	"encoding/json"
	"testing"
)

func TestTitleSyncConstructorsMarshal(t *testing.T) {
	receipt := DeliveryReceipt{Outcome: OutcomeQueued, Reason: "confirmed"}
	cases := map[string]TitleSync{
		"synced":             TitleSyncSynced("the runtime's transcript now carries this title", receipt),
		"pending":            TitleSyncPending("neither confirmed nor refused in time", &receipt),
		"pending-no-receipt": TitleSyncPending("no transcript could be resolved", nil),
		"failed":             TitleSyncFailed("composer busy, nothing was overwritten", &receipt),
		"not-applicable":     TitleSyncNotApplicable("this runtime keeps no title of its own"),
	}
	for name, ts := range cases {
		t.Run(name, func(t *testing.T) {
			b, err := json.Marshal(ts)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			var back TitleSync
			if err := json.Unmarshal(b, &back); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			if back.Status != ts.Status || back.Evidence != ts.Evidence {
				t.Fatalf("round-trip mismatch: got %+v, want %+v", back, ts)
			}
			if (back.Receipt == nil) != (ts.Receipt == nil) {
				t.Fatalf("receipt presence mismatch: got %+v, want %+v", back.Receipt, ts.Receipt)
			}
		})
	}
}

func TestTitleSyncRefusesIncoherentShapes(t *testing.T) {
	receipt := DeliveryReceipt{Outcome: OutcomeQueued}
	cases := map[string]TitleSync{
		"invalid status":                {Status: "bogus", Evidence: "x"},
		"empty evidence":                {Status: TitleSynced, Evidence: ""},
		"not_applicable with a receipt": {Status: TitleNotApplicable, Evidence: "x", Receipt: &receipt},
	}
	for name, ts := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := json.Marshal(ts); err == nil {
				t.Fatalf("expected marshal to refuse %+v", ts)
			}
		})
	}
}

func TestTitleSyncUnmarshalRefusesIncoherentShapes(t *testing.T) {
	for name, raw := range map[string]string{
		"unknown status":              `{"status":"bogus","evidence":"x"}`,
		"empty evidence":              `{"status":"synced","evidence":""}`,
		"not_applicable with receipt": `{"status":"not_applicable","evidence":"x","receipt":{"outcome":"queued"}}`,
	} {
		t.Run(name, func(t *testing.T) {
			var ts TitleSync
			if err := json.Unmarshal([]byte(raw), &ts); err == nil {
				t.Fatalf("expected unmarshal to refuse %s", raw)
			}
		})
	}
}

// TestRenameAckBareEncodingIsUnchanged is the compatibility guarantee this
// type exists to keep: a RenameAck with no title must encode byte-identical
// to the bare Ack every caller already reads, so an older peer or an older
// client is unaffected by this field's addition.
func TestRenameAckBareEncodingIsUnchanged(t *testing.T) {
	b, err := json.Marshal(RenameAck{Accepted: true})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if got, want := string(b), `{"accepted":true}`; got != want {
		t.Fatalf("got %s, want %s", got, want)
	}
}

func TestRenameAckRoundTripsATitle(t *testing.T) {
	ack := RenameAck{Accepted: true, Title: func() *TitleSync {
		ts := TitleSyncSynced("confirmed by the transcript", DeliveryReceipt{Outcome: OutcomeQueued})
		return &ts
	}()}
	b, err := json.Marshal(ack)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var back RenameAck
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !back.Accepted || back.Title == nil || back.Title.Status != TitleSynced {
		t.Fatalf("got %+v", back)
	}
}

// TestRenameAckDecodesAnUnknownTitleAsAbsent is the leniency this type's own
// doc comment promises: a title shape this build cannot make sense of must
// not fail the whole decode — the rename it reports on already happened.
func TestRenameAckDecodesAnUnknownTitleAsAbsent(t *testing.T) {
	raw := `{"accepted":true,"title":{"status":"some-future-status","evidence":"x"}}`
	var ack RenameAck
	if err := json.Unmarshal([]byte(raw), &ack); err != nil {
		t.Fatalf("unmarshal should not fail on an unrecognised title shape: %v", err)
	}
	if !ack.Accepted {
		t.Fatalf("accepted must still decode true: %+v", ack)
	}
	if ack.Title != nil {
		t.Fatalf("an undecodable title must read as absent, got %+v", ack.Title)
	}
}

func TestRenameAckDecodesMalformedTopLevelJSONAsError(t *testing.T) {
	var ack RenameAck
	if err := json.Unmarshal([]byte(`not json`), &ack); err == nil {
		t.Fatal("expected an error decoding malformed JSON")
	}
}
