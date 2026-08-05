package main

import (
	"slices"
	"testing"
)

// The rule under test: after this function, the machine can always reach its
// own service. See withLoopback's comment for the incident — a tunnel-only
// bind makes the service unreachable from the host running the diagnostics,
// where it is indistinguishable from a wedged process.
func TestWithLoopback(t *testing.T) {
	cases := []struct {
		name string
		in   []string
		want []string
	}{{
		name: "a tunnel address gains loopback on the same port",
		in:   []string{"10.0.0.5:9999"},
		want: []string{"10.0.0.5:9999", "127.0.0.1:9999"},
	}, {
		name: "an explicit loopback bind is left alone",
		in:   []string{"127.0.0.1:9999"},
		want: []string{"127.0.0.1:9999"},
	}, {
		name: "localhost counts as loopback",
		in:   []string{"localhost:9999"},
		want: []string{"localhost:9999"},
	}, {
		name: "a wildcard already includes loopback",
		in:   []string{"0.0.0.0:9999"},
		want: []string{"0.0.0.0:9999"},
	}, {
		name: "loopback anywhere in the list satisfies the rule",
		in:   []string{"10.0.0.5:9999", "127.0.0.1:9999"},
		want: []string{"10.0.0.5:9999", "127.0.0.1:9999"},
	}, {
		name: "an ephemeral port has nothing to mirror",
		in:   []string{"10.0.0.5:0"},
		want: []string{"10.0.0.5:0"},
	}, {
		name: "the port comes from the first address that has a real one",
		in:   []string{"10.0.0.5:0", "10.0.0.6:9999"},
		want: []string{"10.0.0.5:0", "10.0.0.6:9999", "127.0.0.1:9999"},
	}}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := withLoopback(slices.Clone(tc.in))
			if !slices.Equal(got, tc.want) {
				t.Errorf("withLoopback(%v) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}

func TestSplitList(t *testing.T) {
	got := splitList(" a=1 , b=2 ,, ")
	want := []string{"a=1", "b=2"}
	if !slices.Equal(got, want) {
		t.Errorf("splitList = %v, want %v", got, want)
	}
}
