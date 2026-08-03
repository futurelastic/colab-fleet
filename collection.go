package fleet

import (
	"encoding/json"
	"errors"
	"fmt"
)

// SourceState is one machine's contribution status to a fanned-out query
// (§9).
type SourceState string

const (
	SourceOK           SourceState = "ok"
	SourceUnreachable  SourceState = "unreachable"
	SourceUnauthorized SourceState = "unauthorized"
	SourceDegraded     SourceState = "degraded"
)

func (s SourceState) valid() bool {
	switch s {
	case SourceOK, SourceUnreachable, SourceUnauthorized, SourceDegraded:
		return true
	default:
		return false
	}
}

func (s SourceState) MarshalJSON() ([]byte, error) {
	if !s.valid() {
		return nil, fmt.Errorf("fleet: %q is not a valid SourceState", string(s))
	}
	return json.Marshal(string(s))
}

func (s *SourceState) UnmarshalJSON(b []byte) error {
	var raw string
	if err := json.Unmarshal(b, &raw); err != nil {
		return err
	}
	v := SourceState(raw)
	if !v.valid() {
		return fmt.Errorf("fleet: %q is not a valid SourceState", raw)
	}
	*s = v
	return nil
}

// SourceStatus is one machine's contribution to a Collection (§9). A failed
// source contributes a SourceStatus, never a silent absence from Items —
// see §5.7: "there are no sessions on that machine" and "I could not reach
// that machine to ask" are different facts, trivially easy to collapse into
// the same empty list.
type SourceStatus struct {
	Machine    MachineId   `json:"machine"`
	Status     SourceState `json:"status"`
	Count      *int        `json:"count,omitempty"`
	Error      string      `json:"error,omitempty"`
	ObservedAt Timestamp   `json:"observedAt"`
}

// ErrCollectionNeedsSources is returned by NewCollection when constructed
// with zero sources. An empty Items with no Sources is, verbatim, "the
// exact bug the design exists to prevent" (§9): it is indistinguishable
// from "genuinely nothing out there" and from "could not reach anyone to
// ask." Even an unfederated, single-machine answer carries at least the
// self source — api-http.md §3.2: "a scope=local response carries exactly
// one SourceStatus."
var ErrCollectionNeedsSources = errors.New("fleet: a Collection must have at least one source")

// Collection is the envelope every plural response wraps (§9). Its fields
// are unexported and reached only through NewCollection and the accessors
// below, so the shape the spec calls out by name — items with no sources —
// cannot be produced by ordinary use of this package.
//
// This is not a complete guarantee: Go's zero value, Collection[T]{}, still
// exists and still has exactly that shape, because nothing in the language
// lets a struct forbid its own zero value. NewCollection is the
// enforcement point for every other path; collection_test.go tests both
// that it rejects an empty Sources slice and that decoding a wire envelope
// with items but no sources fails the same way.
type Collection[T any] struct {
	items    []T
	sources  []SourceStatus
	complete bool
}

// NewCollection builds a Collection. It computes Complete itself rather
// than accepting it as a parameter: the spec (§9) says Complete is "false
// if any source failed to answer" but never says who computes it, and a
// caller-supplied bool is exactly the kind of value that can silently drift
// from what Sources actually reports — the same class of bug §9 warns a
// bare boolean invites callers to ignore, one level up.
//
// Complete is true iff every source's Status is SourceOK. SourceDegraded
// counts as not-fully-answered on the same footing as SourceUnreachable and
// SourceUnauthorized: a degraded source's data is present but not to be
// trusted at face value (§13.2), so treating it as "answered cleanly" would
// reintroduce the exact confidence-flattening §5.6 forbids. The spec's
// prose does not settle this explicitly; it is recorded as a decision here
// and in session-abstraction.md §9's amendment.
func NewCollection[T any](items []T, sources []SourceStatus) (Collection[T], error) {
	if len(sources) == 0 {
		return Collection[T]{}, ErrCollectionNeedsSources
	}
	complete := true
	for _, s := range sources {
		if s.Status != SourceOK {
			complete = false
			break
		}
	}
	return Collection[T]{items: items, sources: sources, complete: complete}, nil
}

// Items returns the collected values. May be empty even when Complete is
// true — that combination means "asked everyone, genuinely found nothing,"
// which is a different fact from any source being absent (§5.7).
func (c Collection[T]) Items() []T { return c.items }

// Sources returns every machine's contribution status. Never empty for a
// Collection built through NewCollection or decoded from the wire.
func (c Collection[T]) Sources() []SourceStatus { return c.sources }

// Complete reports whether every source answered ok. A caller that ignores
// this and reads only Items has made a choice, not an oversight (§9) — the
// field is deliberately impossible to miss at the top level.
func (c Collection[T]) Complete() bool { return c.complete }

// collectionWire is the JSON shape of Collection[T] (§9): items, sources,
// complete, all exported on the wire even though the Go type keeps them
// unexported for construction safety.
type collectionWire[T any] struct {
	Items    []T            `json:"items"`
	Sources  []SourceStatus `json:"sources"`
	Complete bool           `json:"complete"`
}

func (c Collection[T]) MarshalJSON() ([]byte, error) {
	items := c.items
	if items == nil {
		items = []T{}
	}
	return json.Marshal(collectionWire[T]{Items: items, Sources: c.sources, Complete: c.complete})
}

// UnmarshalJSON decodes a wire envelope through NewCollection, so a decoded
// Collection carries exactly the same guarantee a locally-built one does —
// including that Complete is recomputed from Sources rather than trusted
// verbatim from whichever peer produced the JSON. A different
// implementation's bug (or a differing reading of an ambiguous spec
// sentence) that emits a mismatched `complete` should not propagate into
// this process's view of the fleet.
func (c *Collection[T]) UnmarshalJSON(b []byte) error {
	var w collectionWire[T]
	if err := json.Unmarshal(b, &w); err != nil {
		return err
	}
	built, err := NewCollection(w.Items, w.Sources)
	if err != nil {
		return err
	}
	*c = built
	return nil
}
