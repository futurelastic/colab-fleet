package service

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	fleet "github.com/godx-jp/colab-fleet"
	"github.com/godx-jp/colab-fleet/internal/driver"
	"github.com/godx-jp/colab-fleet/internal/drivers/stub"
)

// reservingStubDriver is stub.Driver whose delivery module reserves one
// environment name (#180).
type reservingStubDriver struct{ stub.Driver }

func (*reservingStubDriver) ReservedEnv() []string { return []string{"FLEET_TEST_RESERVED"} }

var _ driver.ReservedEnvReporter = (*reservingStubDriver)(nil)

// TestCreateSession_ReservedEnvIs400: a caller may not set a variable the
// machine's delivery module reserves. Refused as a 400 naming it, before
// the driver is asked to create anything (the stub would answer 501).
func TestCreateSession_ReservedEnvIs400(t *testing.T) {
	svc := New("test-machine")
	if err := svc.RegisterLocalDriver("stub", &reservingStubDriver{stub.Driver{DeadlineMs: 200}}); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(NewMux(svc, Config{Token: testToken, AllowLocalMutations: true}))
	t.Cleanup(srv.Close)

	post := func(env map[string]string) *http.Response {
		body, _ := json.Marshal(map[string]any{"runtime": "stub", "cwd": "/tmp", "env": env})
		req := authedRequest(t, http.MethodPost, srv.URL+"/v1/machines/test-machine/sessions", body)
		req.Header.Set("Idempotency-Key", "key-"+strings.Join(keys(env), "-"))
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		return resp
	}

	resp := post(map[string]string{"FLEET_TEST_RESERVED": "x"})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
	env := decodeError(t, resp)
	if env.Error.Kind != fleet.ErrorInvalid || !strings.Contains(env.Error.Message, "FLEET_TEST_RESERVED") {
		t.Fatalf("error = %+v, want invalid naming the variable", env.Error)
	}

	other := post(map[string]string{"OTHER": "y"})
	defer other.Body.Close()
	if other.StatusCode != http.StatusNotImplemented {
		t.Fatalf("an unreserved name reached status %d, want the stub's own 501", other.StatusCode)
	}
}

func keys(m map[string]string) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	return out
}
