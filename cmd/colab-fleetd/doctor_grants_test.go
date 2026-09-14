package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// colab-fleet #154: peer.<m>.grants reads the peer's own answer when online.
// The row id never changes; only its answer does.
func TestDoctorPeerGrantsReadsThePeersAnswer(t *testing.T) {
	cases := []struct {
		name    string
		status  int
		body    string
		want    rowStatus
		mention string
	}{
		{"listed with read", 200, `{"grants":["read","send"],"listsYou":true}`, statusPass, "read, send"},
		{"not listed back", 200, `{"grants":["read"],"listsYou":false}`, statusWarn, "does not list"},
		{"no read there", 200, `{"grants":["create"],"listsYou":null}`, statusWarn, "lacks read"},
		{"older build", 200, `{"grants":["read"]}`, statusUnknown, "predates"},
		{"credential refused", 401, `{"error":{"kind":"unauthorized","message":"x"}}`, statusFail, "no principal"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var gotQuery string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/v1/whoami":
					gotQuery = r.URL.RawQuery
					w.WriteHeader(tc.status)
					_, _ = w.Write([]byte(tc.body))
				case "/v1/health":
					_, _ = w.Write([]byte(`{"build":{}}`))
				default:
					http.NotFound(w, r)
				}
			}))
			defer srv.Close()
			cfg := writeTestConfig(t, t.TempDir(), []testPrincipal{{Name: "system:m1", Token: "s", Grants: grants("read")}},
				[]testPeer{{Machine: "m2", URL: srv.URL, Token: "p"}})
			env := testDoctorEnv(map[string]string{"FLEET_MACHINE": "m1", "FLEET_CONFIG": cfg, "FLEET_RUNTIME": "stub"})
			row := rowByID(t, runChecks(context.Background(), env), "peer.m2.grants")
			if row.Status != tc.want || !strings.Contains(row.Summary, tc.mention) {
				t.Fatalf("row = %+v, want %s mentioning %q", row, tc.want, tc.mention)
			}
			if gotQuery != "peer=m1" {
				t.Errorf("whoami query = %q, want peer=m1", gotQuery)
			}
		})
	}
}
