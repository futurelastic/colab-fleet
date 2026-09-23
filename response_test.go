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
	ok := []Response{{}, {Choice: 2}, {Cancel: true}, {Choices: []int{1}}, {Choices: []int{3, 1}, Nonce: "n"}}
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
	}
	for _, r := range bad {
		if err := r.Validate(); err == nil {
			t.Errorf("%+v: valid, want an error", r)
		}
	}
}
