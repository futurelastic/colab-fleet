package fleet

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// Session labels (colab-fleet #153).
//
// A label is an opaque key/value pair a caller attaches to a session. The
// service stores them, returns them on every read, filters on them, and
// otherwise attaches no meaning at all — the same stance it takes on Marker.
//
// # Why bounded, and why these bounds
//
// A label is metadata about a session, not a place to keep content (§5.8):
// a unit-of-work id, a tree name, a session kind. Sixteen short pairs is
// generous for that and small enough that a fleet-wide listing never grows
// by more than a few kilobytes per session. The bounds are fixed rather than
// configurable because a caller writing to two machines must be able to rely
// on one answer; they are reported on GET /v1/health so a caller can ask
// rather than find out by exceeding them.
const (
	MaxLabels          = 16
	MaxLabelKeyBytes   = 128
	MaxLabelValueBytes = 128
)

// LabelSeparator divides key from value in the `label=key:value` list
// filter. Keys may not contain it, so that split is never ambiguous; values
// may, because the split is on the FIRST separator.
const LabelSeparator = ":"

// LabelLimits is how a service reports the bounds above (GET /v1/health's
// `labels`). Its presence is also how a relaying service learns that a peer
// carries labels at all: a peer that omits it predates them, and would
// silently drop a create's labels rather than refuse them.
type LabelLimits struct {
	MaxKeys       int `json:"maxKeys"`
	MaxKeyBytes   int `json:"maxKeyBytes"`
	MaxValueBytes int `json:"maxValueBytes"`
}

// SelfLabelLimits is what this build enforces.
func SelfLabelLimits() LabelLimits {
	return LabelLimits{MaxKeys: MaxLabels, MaxKeyBytes: MaxLabelKeyBytes, MaxValueBytes: MaxLabelValueBytes}
}

// ValidateLabelKey reports why a key cannot be used, or nil.
func ValidateLabelKey(k string) error {
	switch {
	case k == "":
		return fmt.Errorf("label keys must be non-empty")
	case len(k) > MaxLabelKeyBytes:
		return fmt.Errorf("label key %q is %d bytes, over the %d-byte limit", clip(k), len(k), MaxLabelKeyBytes)
	case strings.Contains(k, LabelSeparator):
		return fmt.Errorf("label key %q contains %q, which the label=key%svalue filter reserves as its separator",
			clip(k), LabelSeparator, LabelSeparator)
	}
	return nil
}

// ValidateLabels reports the first reason a label map cannot be stored, or
// nil. Keys are checked in sorted order so the same bad map always produces
// the same message.
func ValidateLabels(m map[string]string) error {
	if len(m) > MaxLabels {
		return fmt.Errorf("labels carries %d keys, over the %d-key limit", len(m), MaxLabels)
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if err := ValidateLabelKey(k); err != nil {
			return err
		}
		if v := m[k]; len(v) > MaxLabelValueBytes {
			return fmt.Errorf("label %q is %d bytes, over the %d-byte value limit", clip(k), len(v), MaxLabelValueBytes)
		}
	}
	return nil
}

// MatchesLabels reports whether have carries every pair in want — the AND
// semantics of a repeated `label=` filter. Exact match on key and value; an
// empty want matches everything.
func MatchesLabels(have, want map[string]string) bool {
	for k, v := range want {
		got, ok := have[k]
		if !ok || got != v {
			return false
		}
	}
	return true
}

// CopyLabels returns an independent copy, never nil.
func CopyLabels(m map[string]string) map[string]string {
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

// MarshalJSON writes a Session with `labels` always present: `{}` when it has
// none. See Session.Labels for why absence has to stay reserved for a service
// that predates the field.
func (s Session) MarshalJSON() ([]byte, error) {
	type wire Session
	w := wire(s)
	if w.Labels == nil {
		w.Labels = map[string]string{}
	}
	return json.Marshal(w)
}

// clip keeps an over-long key from turning an error message into a copy of
// the request.
func clip(s string) string {
	if len(s) <= 32 {
		return s
	}
	return s[:32] + "…"
}
