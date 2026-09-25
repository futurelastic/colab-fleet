package fleet

import (
	"encoding/json"
	"testing"
)

// colab-fleet#176: the wire shape a peer receives. Response is forwarded
// verbatim to a peer, so its encoding is the contract, not an implementation
// detail.
func TestResponseChoicesWireShape(t *testing.T) {
	b, err := json.Marshal(Response{Choices: []int{1, 3}, Nonce: "n"})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(b), `{"choices":[1,3],"nonce":"n"}`; got != want {
		t.Errorf("marshal = %s, want %s", got, want)
	}
	var back Response
	if err := json.Unmarshal(b, &back); err != nil || len(back.Choices) != 2 || back.Choices[1] != 3 {
		t.Errorf("round trip = %+v (%v)", back, err)
	}
}

func TestResponseValidate(t *testing.T) {
	ok := []Response{{}, {Choice: 2}, {Cancel: true}, {Choices: []int{1}}, {Choices: []int{3, 1}, Nonce: "n"},
		{Text: strp("in my own words"), Nonce: "n"},
		{Text: strp("and a tick"), Choices: []int{2}}}
	for _, r := range ok {
		if err := r.Validate(); err != nil {
			t.Errorf("%+v: %v, want valid", r, err)
		}
	}
	bad := []Response{
		{Choices: []int{}},
		{Choices: []int{1}, Choice: 1},
		{Choices: []int{1}, Cancel: true},
		{Choices: []int{2, 2}},
		{Choices: []int{0}},
		{Choices: []int{-1}},
		// colab-fleet#206: an empty or blank answer is never sent, and text is
		// its own way of answering — not an addition to a choice or a cancel.
		{Text: strp("")},
		{Text: strp("  \t\n ")},
		{Text: strp("x"), Choice: 1},
		{Text: strp("x"), Cancel: true},
		{Text: strp("x"), Choices: []int{}},
	}
	for _, r := range bad {
		if err := r.Validate(); err == nil {
			t.Errorf("%+v: valid, want an error", r)
		}
	}
}

func strp(s string) *string { return &s }

// colab-fleet#206: Text is a pointer so that an empty string is still a field
// when a peer-relaying driver marshals the body onward. With a plain string
// and omitempty, {"text":""} would arrive at the peer as {} — "accept the
// highlighted option" — which is the trap an empty Choices set is refused for.
func TestResponseTextWireShape(t *testing.T) {
	b, err := json.Marshal(Response{Text: strp("blue-ish"), Nonce: "n"})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(b), `{"text":"blue-ish","nonce":"n"}`; got != want {
		t.Errorf("marshal = %s, want %s", got, want)
	}
	if b, _ := json.Marshal(Response{Text: strp("")}); string(b) != `{"text":""}` {
		t.Errorf("an empty text marshalled as %s; it must survive so the receiver can refuse it", b)
	}
	if b, _ := json.Marshal(Response{Choice: 2}); string(b) != `{"choice":2}` {
		t.Errorf("an absent text marshalled as %s; it must stay absent", b)
	}
	var back Response
	if err := json.Unmarshal([]byte(`{"text":"","nonce":"n"}`), &back); err != nil || back.Text == nil || *back.Text != "" {
		t.Errorf("decoding an empty text lost it: %+v (%v)", back, err)
	}
	var absent Response
	if err := json.Unmarshal([]byte(`{"nonce":"n"}`), &absent); err != nil || absent.Text != nil {
		t.Errorf("an absent text decoded as present: %+v (%v)", absent, err)
	}
}
