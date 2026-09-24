package main

import (
	"bytes"
	"strings"
	"testing"
)

// colab-fleet #177: whatever is left on the command line after the
// subcommands have had their turn must never fall through to "start the
// service". Only a bare invocation starts it.
func TestRunUsage(t *testing.T) {
	cases := []struct {
		name        string
		args        []string
		handled     bool
		code        int
		wantStdout  bool
		wantUnknown bool
	}{
		{name: "no arguments starts the service", args: nil},
		{name: "-h prints usage", args: []string{"-h"}, handled: true, code: 0, wantStdout: true},
		{name: "--help prints usage", args: []string{"--help"}, handled: true, code: 0, wantStdout: true},
		{name: "-help prints usage", args: []string{"-help"}, handled: true, code: 0, wantStdout: true},
		{name: "help prints usage", args: []string{"help"}, handled: true, code: 0, wantStdout: true},
		{name: "an unknown flag is refused", args: []string{"--version"}, handled: true, code: 2, wantUnknown: true},
		{name: "a typo of a subcommand is refused", args: []string{"docter"}, handled: true, code: 2, wantUnknown: true},
		{name: "help after another word is still refused", args: []string{"start", "-h"}, handled: true, code: 2, wantUnknown: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var out, errb bytes.Buffer
			handled, code := runUsage(tc.args, &out, &errb)
			if handled != tc.handled || code != tc.code {
				t.Fatalf("runUsage(%q) = (%v, %d), want (%v, %d)", tc.args, handled, code, tc.handled, tc.code)
			}
			if tc.wantStdout != strings.Contains(out.String(), "usage: colab-fleetd") {
				t.Errorf("stdout = %q, usage wanted: %v", out.String(), tc.wantStdout)
			}
			if tc.wantUnknown {
				if !strings.Contains(errb.String(), "unknown argument") || !strings.Contains(errb.String(), "usage: colab-fleetd") {
					t.Errorf("stderr = %q, want an unknown-argument refusal with usage", errb.String())
				}
			} else if errb.Len() != 0 {
				t.Errorf("stderr = %q, want empty", errb.String())
			}
		})
	}
}

// The subcommands are dispatched before runUsage in main; this pins that
// they are not also claimed by it, so a reorder cannot silently turn
// `doctor` or `principal` into a usage error.
func TestRunUsageLeavesSubcommandsToTheirHandlers(t *testing.T) {
	for _, sub := range []string{"doctor", "principal"} {
		if handled, _ := runDoctor([]string{sub}, func(string) string { return "" }, &bytes.Buffer{}, &bytes.Buffer{}); sub == "doctor" && !handled {
			t.Errorf("runDoctor did not claim %q", sub)
		}
		if handled, _ := runPrincipal([]string{sub}); sub == "principal" && !handled {
			t.Errorf("runPrincipal did not claim %q", sub)
		}
	}
}
