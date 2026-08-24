package tmux

import (
	"context"
	"testing"
)

func TestSanitizeName(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{name: "drops control characters", in: "\x01alpha\x02", want: "alpha"},
		{name: "drops colon", in: "a:b", want: "ab"},
		{name: "turns dot to hyphen", in: "a.b", want: "a-b"},
		{name: "trims leading hyphens", in: "--alpha", want: "alpha"},
		{name: "keeps clean name", in: "already-clean", want: "already-clean"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := sanitizeName(tc.in); got != tc.want {
				t.Errorf("sanitizeName(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestDecoration(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{name: "all body chars", in: "alpha-1", want: ""},
		{name: "trailing non-body", in: "alpha💬", want: "💬"},
		{name: "entire non-body", in: "§", want: ""},
		{name: "mixed trailing non-body only", in: "alpha-§", want: "§"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := decoration(tc.in); got != tc.want {
				t.Errorf("decoration(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestApplyMarker(t *testing.T) {
	cases := []struct {
		name   string
		inName string
		marker string
		known  markerState
		want   string
	}{
		{name: "marker already present (both body)", inName: "alpha-1", marker: "alpha-1", known: markerUnknown, want: "alpha-1"},
		{name: "marker already present after stacking", inName: "alpha-1alpha-1", marker: "alpha-1", known: markerUnknown, want: "alpha-1alpha-1"},
		{name: "non-body marker not yet present", inName: "alpha", marker: "§", known: markerUnknown, want: "alpha§"},
		{name: "non-body marker already present", inName: "alpha§", marker: "§", known: markerUnknown, want: "alpha§"},
		{name: "empty marker leaves name", inName: "alpha", marker: "", known: markerUnknown, want: "alpha"},
		{name: "existing decoration prevents appending a different marker", inName: "alpha§", marker: "alpha-1", known: markerUnknown, want: "alpha§"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := applyMarker(tc.inName, tc.marker, tc.known); got != tc.want {
				t.Errorf("applyMarker(%q, %q, %v) = %q, want %q", tc.inName, tc.marker, tc.known, got, tc.want)
			}
		})
	}
}

// colab-fleet #96: once a session record answers the question, applyMarker
// stops guessing from the string — including in the exact case the old
// heuristic could not tell apart, a marker drawn from the same alphabet as
// the name body that coincidentally matches the name's own trailing
// characters.
func TestApplyMarkerIsExactWhenTheRecordKnows(t *testing.T) {
	cases := []struct {
		name   string
		inName string
		marker string
		known  markerState
		want   string
	}{
		{
			name:   "record says absent: append even though the string coincidentally ends in the marker",
			inName: "append", marker: "end", known: markerAbsent,
			want: "appendend",
		},
		{
			name:   "record says applied: leave it, no string comparison needed",
			inName: "alpha-1", marker: "alpha-1", known: markerApplied,
			want: "alpha-1",
		},
		{
			name:   "record says applied, non-body marker: still exact",
			inName: "alpha§", marker: "§", known: markerApplied,
			want: "alpha§",
		},
		{
			name:   "record says absent, non-body marker: still appends",
			inName: "alpha", marker: "§", known: markerAbsent,
			want: "alpha§",
		},
		{
			name:   "empty marker leaves name regardless of known state",
			inName: "alpha", marker: "", known: markerApplied,
			want: "alpha",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := applyMarker(tc.inName, tc.marker, tc.known); got != tc.want {
				t.Errorf("applyMarker(%q, %q, %v) = %q, want %q", tc.inName, tc.marker, tc.known, got, tc.want)
			}
		})
	}
}

func TestNumberedName(t *testing.T) {
	cases := []struct {
		name   string
		inName string
		n      int
		want   string
	}{
		{name: "n < 2 returns name", inName: "alpha💬", n: 1, want: "alpha💬"},
		{name: "counter before non-ASCII marker", inName: "alpha💬", n: 2, want: "alpha-2💬"},
		{name: "ascii-alphabet suffix is not mistaken for a marker (issue #90)", inName: "append", n: 2, want: "append-2"},
		{name: "no marker, decoration empty", inName: "alpha", n: 2, want: "alpha-2"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := numberedName(tc.inName, tc.n); got != tc.want {
				t.Errorf("numberedName(%q, %d) = %q, want %q", tc.inName, tc.n, got, tc.want)
			}
		})
	}
}

func TestResolveNameIdempotent(t *testing.T) {
	ctx := context.Background()

	t.Run("ASCII marker", func(t *testing.T) {
		d := newTestDriver(&fakeMux{})
		first, _, ok := d.resolveName(ctx, "alpha-1", "alpha-1")
		if !ok || first != "alpha-1" {
			t.Fatalf("first = %q, ok=%v, want %q", first, ok, "alpha-1")
		}
		second, _, ok := d.resolveName(ctx, first, "alpha-1")
		if !ok || second != first {
			t.Fatalf("second = %q, ok=%v, want same %q", second, ok, first)
		}
	})

	t.Run("non-nameBody marker", func(t *testing.T) {
		d := newTestDriver(&fakeMux{})
		first, _, ok := d.resolveName(ctx, "alpha", "§")
		if !ok || first != "alpha§" {
			t.Fatalf("first = %q, ok=%v, want %q", first, ok, "alpha§")
		}
		second, _, ok := d.resolveName(ctx, first, "§")
		if !ok || second != first {
			t.Fatalf("second = %q, ok=%v, want same %q", second, ok, first)
		}
	})
}
