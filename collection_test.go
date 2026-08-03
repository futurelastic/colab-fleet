package fleet

import (
	"encoding/json"
	"errors"
	"testing"
	"time"
)

func TestNewCollection_RequiresSources(t *testing.T) {
	_, err := NewCollection[Session](nil, nil)
	if !errors.Is(err, ErrCollectionNeedsSources) {
		t.Fatalf("NewCollection(nil, nil) err = %v, want ErrCollectionNeedsSources", err)
	}

	_, err = NewCollection([]Session{{}}, nil)
	if !errors.Is(err, ErrCollectionNeedsSources) {
		t.Fatalf("NewCollection(items, nil) err = %v, want ErrCollectionNeedsSources — items with no sources is exactly the bug §9 forbids", err)
	}
}

func TestCollection_CompleteIsDerivedNotSupplied(t *testing.T) {
	now := time.Now()

	allOK, err := NewCollection([]int{1, 2}, []SourceStatus{
		{Machine: "a", Status: SourceOK, ObservedAt: now},
		{Machine: "b", Status: SourceOK, ObservedAt: now},
	})
	if err != nil {
		t.Fatalf("NewCollection: %v", err)
	}
	if !allOK.Complete() {
		t.Fatal("Complete() = false, want true when every source is ok")
	}

	oneUnreachable, err := NewCollection([]int{1}, []SourceStatus{
		{Machine: "a", Status: SourceOK, ObservedAt: now},
		{Machine: "b", Status: SourceUnreachable, ObservedAt: now},
	})
	if err != nil {
		t.Fatalf("NewCollection: %v", err)
	}
	if oneUnreachable.Complete() {
		t.Fatal("Complete() = true, want false when a source is unreachable")
	}

	oneDegraded, err := NewCollection([]int{1}, []SourceStatus{
		{Machine: "a", Status: SourceDegraded, ObservedAt: now},
	})
	if err != nil {
		t.Fatalf("NewCollection: %v", err)
	}
	// A degraded source answered, but its data is not to be trusted at
	// face value (§13.2) — Complete must not read as "fully answered"
	// merely because the source technically replied.
	if oneDegraded.Complete() {
		t.Fatal("Complete() = true, want false when a source reports degraded")
	}
}

func TestCollection_JSONRoundTrip(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	original, err := NewCollection([]string{"x", "y"}, []SourceStatus{
		{Machine: "m1", Status: SourceOK, ObservedAt: now},
	})
	if err != nil {
		t.Fatalf("NewCollection: %v", err)
	}

	b, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	var decoded Collection[string]
	if err := json.Unmarshal(b, &decoded); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}

	if len(decoded.Items()) != 2 || decoded.Items()[0] != "x" || decoded.Items()[1] != "y" {
		t.Fatalf("Items() = %v, want [x y]", decoded.Items())
	}
	if len(decoded.Sources()) != 1 || decoded.Sources()[0].Machine != "m1" {
		t.Fatalf("Sources() = %v", decoded.Sources())
	}
	if !decoded.Complete() {
		t.Fatal("Complete() = false, want true")
	}
}

func TestCollection_UnmarshalRejectsEmptySources(t *testing.T) {
	wire := `{"items":[],"sources":[],"complete":true}`
	var c Collection[int]
	if err := json.Unmarshal([]byte(wire), &c); !errors.Is(err, ErrCollectionNeedsSources) {
		t.Fatalf("Unmarshal err = %v, want ErrCollectionNeedsSources — a wire envelope claiming completeness with zero sources is the bug §9 exists to prevent", err)
	}
}

func TestCollection_UnmarshalRecomputesCompleteFromSources(t *testing.T) {
	// A foreign implementation's envelope claims complete:true while a
	// source clearly failed. This process must not trust that boolean
	// verbatim — see NewCollection's doc comment.
	wire := `{"items":[],"sources":[{"machine":"m1","status":"unreachable","observedAt":"2026-01-01T00:00:00Z"}],"complete":true}`
	var c Collection[int]
	if err := json.Unmarshal([]byte(wire), &c); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if c.Complete() {
		t.Fatal("Complete() = true, want false — recomputed from Sources, not trusted from the wire")
	}
}

func TestCollection_EmptyItemsWithOKSourceIsNotAFailure(t *testing.T) {
	// §5.7: "no sessions on that machine" and "could not reach that
	// machine" are different facts. This is the first one: zero items,
	// source ok.
	now := time.Now()
	c, err := NewCollection[Session](nil, []SourceStatus{{Machine: "m1", Status: SourceOK, ObservedAt: now}})
	if err != nil {
		t.Fatalf("NewCollection: %v", err)
	}
	if len(c.Items()) != 0 {
		t.Fatalf("Items() = %v, want empty", c.Items())
	}
	if !c.Complete() {
		t.Fatal("Complete() = false, want true — an ok source that legitimately found nothing is still complete")
	}
}
