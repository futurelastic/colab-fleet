package fleet

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestValidateLaunchSettings(t *testing.T) {
	big := `{"k":"` + strings.Repeat("x", MaxLaunchSettingsBytes) + `"}`
	cases := []struct {
		name    string
		mode    string
		raw     string
		want    string
		wantErr string
	}{
		{"absent", "", "", "", ""},
		{"absent with bypass", PermissionModeBypass, "", "", ""},
		{"null literal is absent", "", "null", "", ""},
		{"bypass object is compacted", PermissionModeBypass, ` { "crossSessionInbound" : "accept" } `, `{"crossSessionInbound":"accept"}`, ""},
		{"no mode refuses", "", `{"a":1}`, "", "only together with permissionMode"},
		{"unknown mode refuses", "plan", `{"a":1}`, "", "only together with permissionMode"},
		{"invalid JSON", PermissionModeBypass, `{"a":`, "", "not valid JSON"},
		{"array", PermissionModeBypass, `["a"]`, "", "must be a JSON object"},
		{"scalar", PermissionModeBypass, `"a"`, "", "must be a JSON object"},
		{"over the limit", PermissionModeBypass, big, "", "limit"},
	}
	for _, tc := range cases {
		got, err := ValidateLaunchSettings(tc.mode, json.RawMessage(tc.raw))
		if tc.wantErr == "" {
			if err != nil || got != tc.want {
				t.Errorf("%s: got (%q, %v), want (%q, nil)", tc.name, got, err, tc.want)
			}
			continue
		}
		if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
			t.Errorf("%s: err = %v, want it to contain %q", tc.name, err, tc.wantErr)
		}
	}
}
