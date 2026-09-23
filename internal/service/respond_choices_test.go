package service

import (
	"net/http"
	"strings"
	"testing"
)

// colab-fleet#176: a respond body whose choices contradict the rest of it is
// the caller's fault, rejected as a 400 in the handler, before the driver —
// which may be a peer's, forwarding the body verbatim — sees it. The empty set matters most: marshalled onward with omitempty it would
// arrive at a peer as {}, which means "accept the highlighted option".
func TestRespondRejectsContradictoryChoicesBeforeAnyDriver(t *testing.T) {
	srv := principalSrv(t, []Principal{
		{Name: "operator", Token: "tok", Grants: []Grant{GrantRead, GrantSend}},
	})
	post := func(machine, body string) int {
		t.Helper()
		req, err := http.NewRequest(http.MethodPost,
			srv.URL+"/v1/machines/"+machine+"/sessions/s1/respond", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer tok")
		req.Header.Set("Idempotency-Key", "k-"+machine+body)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	for _, machine := range []string{"testbox"} {
		for _, body := range []string{
			`{"choices":[]}`,
			`{"choices":[1],"choice":2}`,
			`{"choices":[1],"cancel":true}`,
			`{"choices":[1,1]}`,
			`{"choices":[0]}`,
		} {
			if got := post(machine, body); got != http.StatusBadRequest {
				t.Errorf("%s %s → %d, want 400", machine, body, got)
			}
		}
		if got := post(machine, `{"choices":[1,3],"nonce":"n"}`); got == http.StatusBadRequest {
			t.Errorf("%s: a well-formed choices body was rejected as invalid", machine)
		}
	}
}
