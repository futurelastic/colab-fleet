package fleet

import (
	"encoding/json"
	"testing"
)

func TestSession_JSONFlattensRef(t *testing.T) {
	s := Session{
		SessionRef: SessionRef{Machine: "m1", ID: "abc", Name: "my-session"},
		Runtime:    "tmux",
		Cwd:        "/work",
		State:      ObservedState(StatusIdle, "checked", nil),
	}

	b, err := json.Marshal(s)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	var raw map[string]any
	if err := json.Unmarshal(b, &raw); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}

	for _, field := range []string{"machine", "id", "name", "runtime", "cwd", "state"} {
		if _, ok := raw[field]; !ok {
			t.Errorf("field %q missing from flattened JSON: %s", field, b)
		}
	}
	if _, ok := raw["ref"]; ok {
		t.Errorf("unexpected nested %q field — SessionRef must be flattened, matching api-http.md §3.3's example", "ref")
	}
}

func TestErrorKind_DefaultHTTPStatus(t *testing.T) {
	cases := []struct {
		kind ErrorKind
		want int
	}{
		{ErrorInvalid, 400},
		{ErrorUnauthorized, 401},
		{ErrorNotFound, 404},
		{ErrorConflict, 409},
		{ErrorUnsupported, 501},
		{ErrorUnreachable, 504},
	}
	for _, c := range cases {
		if got := c.kind.DefaultHTTPStatus(); got != c.want {
			t.Errorf("%q.DefaultHTTPStatus() = %d, want %d", c.kind, got, c.want)
		}
	}
}

func TestNotFoundAndUnreachableAreDistinctKinds(t *testing.T) {
	// api-http.md §2: "the single most important line in this document" —
	// never conflate these two.
	if ErrorNotFound == ErrorUnreachable {
		t.Fatal("ErrorNotFound and ErrorUnreachable must be distinct values")
	}
	if ErrorNotFound.DefaultHTTPStatus() == ErrorUnreachable.DefaultHTTPStatus() {
		t.Fatal("ErrorNotFound and ErrorUnreachable must map to distinct HTTP statuses")
	}
}
