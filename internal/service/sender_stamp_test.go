package service

import (
	"net/http"
	"net/http/httptest"
	"testing"

	fleet "github.com/godx-jp/colab-fleet"
	"github.com/godx-jp/colab-fleet/internal/drivers/stub"
)

// colab-fleet #158: the machine in a sender label is stamped by the service,
// never taken from the caller. These cases are the whole rule.
func TestStampSender(t *testing.T) {
	svc := New("entrybox")
	if err := svc.RegisterPeerDriver("peerbox", &stub.Driver{}); err != nil {
		t.Fatal(err)
	}

	direct := httptest.NewRequest(http.MethodPost, "/v1/machines/entrybox/sessions/s/input", nil)
	relayed := httptest.NewRequest(http.MethodPost, "/v1/machines/entrybox/sessions/s/input", nil)
	relayed.Header.Set(onBehalfOfHeader, "addr:10.0.0.1 via peer")

	for _, tc := range []struct {
		name    string
		r       *http.Request
		claimed fleet.MachineId
		want    fleet.MachineId
	}{
		{"direct, nothing claimed: stamped with this machine", direct, "", "entrybox"},
		{"direct, a machine claimed: ignored, stamped with this machine", direct, "forged", "entrybox"},
		{"direct, a configured peer claimed: still ignored", direct, "peerbox", "entrybox"},
		{"relayed from a configured peer: its stamp is kept", relayed, "peerbox", "peerbox"},
		{"relayed, naming no configured peer: omitted, not guessed", relayed, "stranger", ""},
		{"relayed, naming nothing: omitted", relayed, "", ""},
		{"relayed, naming this machine: omitted (this machine did not stamp it)", relayed, "entrybox", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in := &fleet.MessageFrom{Agent: "agent-a", Session: "s-158", RelayOfHuman: true, Machine: tc.claimed}
			got := svc.stampSender(tc.r, in)
			if got == nil {
				t.Fatal("a present from came back nil")
			}
			if got.Machine != tc.want {
				t.Errorf("machine = %q, want %q", got.Machine, tc.want)
			}
			if got.Agent != "agent-a" || got.Session != "s-158" || !got.RelayOfHuman {
				t.Errorf("caller statements were altered: %+v", *got)
			}
			if in.Machine != tc.claimed {
				t.Errorf("stampSender mutated the caller's value: %q", in.Machine)
			}
		})
	}

	if got := svc.stampSender(direct, nil); got != nil {
		t.Errorf("no from must stay unlabelled, got %+v", *got)
	}
}
