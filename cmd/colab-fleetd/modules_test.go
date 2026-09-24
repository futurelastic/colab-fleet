package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// #185: the switch. Everything here is about what the composition root resolves
// from three environment variables — never about what a module then does.

func envOf(vars map[string]string) func(string) string {
	return func(k string) string { return vars[k] }
}

func installFakeModule(t *testing.T, dir, name string) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o755); err != nil { // WriteFile honours the umask
		t.Fatal(err)
	}
	return path
}

// Nothing set: no module, no client, nothing to log — a daemon exactly as it was
// before modules existed.
func TestModuleConfig_Unset(t *testing.T) {
	for _, v := range []string{"", "   ", ",,", " , "} {
		got := loadDeliveryModules(envOf(map[string]string{"FLEET_DELIVERY_MODULES": v}), "/nowhere/bin/colab-fleetd", "")
		if len(got.Enabled) != 0 || len(got.Config.Clients) != 0 || len(got.Problems) != 0 || len(got.Invalid) != 0 {
			t.Errorf("FLEET_DELIVERY_MODULES=%q resolved to %+v, want nothing", v, got)
		}
		if lines := got.describe(); len(lines) != 0 {
			t.Errorf("FLEET_DELIVERY_MODULES=%q logged %v", v, lines)
		}
	}
}

func TestModuleConfig_Enabled(t *testing.T) {
	dir := t.TempDir()
	path := installFakeModule(t, dir, "relay-a")
	got := loadDeliveryModules(envOf(map[string]string{
		"FLEET_DELIVERY_MODULES": "relay-a, relay-b,relay-a",
		"FLEET_MODULES_DIR":      dir,
		"FLEET_STATE_DIR":        "/state",
		"PATH":                   "/usr/bin",
	}), "/nowhere/bin/colab-fleetd", "/state")

	if strings.Join(got.Enabled, ",") != "relay-a,relay-b" {
		t.Fatalf("Enabled = %v, want the deduplicated list in the configured order", got.Enabled)
	}
	if len(got.Config.Clients) != 1 || got.Config.Clients[0].Name != "relay-a" || got.Config.Clients[0].Path != path {
		t.Fatalf("Clients = %+v, want only the module whose executable exists", got.Config.Clients)
	}
	if strings.Join(got.Config.Enabled, ",") != "relay-a,relay-b" {
		t.Errorf("the driver must be told the whole enabled list, installed or not: %v", got.Config.Enabled)
	}
	if len(got.Problems) != 1 || got.Problems[0].Name != "relay-b" {
		t.Fatalf("Problems = %+v, want relay-b named", got.Problems)
	}
	env := strings.Join(got.Config.Clients[0].Env, "\n")
	for _, want := range []string{"PATH=/usr/bin", "FLEET_STATE_DIR=/state"} {
		if !strings.Contains(env, want) {
			t.Errorf("child env %q lacks %s", env, want)
		}
	}
}

func TestModuleConfig_InvalidNameSkipped(t *testing.T) {
	dir := t.TempDir()
	installFakeModule(t, dir, "relay-a")
	got := loadDeliveryModules(envOf(map[string]string{
		"FLEET_DELIVERY_MODULES": "Relay, auto, terminal, ../x, relay-a",
		"FLEET_MODULES_DIR":      dir,
	}), "", "")
	if strings.Join(got.Enabled, ",") != "relay-a" {
		t.Fatalf("Enabled = %v, want only the legal name", got.Enabled)
	}
	if len(got.Invalid) != 4 {
		t.Fatalf("Invalid = %v, want the four illegal entries", got.Invalid)
	}
	if lines := strings.Join(got.describe(), "\n"); !strings.Contains(lines, "not a legal module name") {
		t.Errorf("an illegal name must be reported once: %q", lines)
	}
}

// An enabled name with no executable is reported once, by name and reason —
// never with the directory it looked in — and is not an error: a machine that
// could not fetch the module is a supported state.
func TestModuleConfig_MissingExecutableLoggedOnce(t *testing.T) {
	dir := t.TempDir()
	got := loadDeliveryModules(envOf(map[string]string{
		"FLEET_DELIVERY_MODULES": "relay-a",
		"FLEET_MODULES_DIR":      dir,
	}), "", "")
	lines := got.describe()
	if len(lines) != 1 || !strings.Contains(lines[0], `"relay-a"`) {
		t.Fatalf("lines = %q, want exactly one naming the module", lines)
	}
	if strings.Contains(lines[0], dir) {
		t.Errorf("a log line named the modules directory: %q", lines[0])
	}
	if len(got.Config.Clients) != 0 {
		t.Errorf("Clients = %+v, want none", got.Config.Clients)
	}
}

func TestModuleConfig_DefaultDirFromExecutable(t *testing.T) {
	root := t.TempDir()
	bin := filepath.Join(root, "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	exe := filepath.Join(bin, "colab-fleetd")
	if err := os.WriteFile(exe, nil, 0o755); err != nil {
		t.Fatal(err)
	}
	modules := filepath.Join(root, "libexec", "colab-fleet", "modules")
	path := installFakeModule(t, modules, "relay-a")

	got := loadDeliveryModules(envOf(map[string]string{"FLEET_DELIVERY_MODULES": "relay-a"}), exe, "")
	if len(got.Config.Clients) != 1 {
		t.Fatalf("the default directory (beside the binary) was not used: %+v", got)
	}
	wantDir, _ := filepath.EvalSymlinks(modules)
	gotDir, _ := filepath.EvalSymlinks(filepath.Dir(got.Config.Clients[0].Path))
	if gotDir != wantDir {
		t.Errorf("looked in %q, want %q", gotDir, wantDir)
	}
	_ = path
}

// FLEET_DELIVERY_MODULE_ENV names the only extras a module child receives, and
// a FLEET_ name is never one of them: this service's own credentials are not
// the module's.
func TestModuleConfig_ForwardedEnvExcludesFleetNames(t *testing.T) {
	dir := t.TempDir()
	installFakeModule(t, dir, "relay-a")
	got := loadDeliveryModules(envOf(map[string]string{
		"FLEET_DELIVERY_MODULES":    "relay-a",
		"FLEET_MODULES_DIR":         dir,
		"FLEET_DELIVERY_MODULE_ENV": "RELAY_TOKEN_FILE, FLEET_TOKEN",
		"RELAY_TOKEN_FILE":          "/somewhere",
		"FLEET_TOKEN":               "secret-value",
		"UNRELATED":                 "x",
	}), "", "")
	env := strings.Join(got.Config.Clients[0].Env, "\n")
	if !strings.Contains(env, "RELAY_TOKEN_FILE=/somewhere") {
		t.Errorf("the forwarded name was not forwarded: %q", env)
	}
	if strings.Contains(env, "FLEET_TOKEN") || strings.Contains(env, "secret-value") || strings.Contains(env, "UNRELATED") {
		t.Errorf("the child received something it was not entitled to: %q", env)
	}
	if lines := strings.Join(got.describe(), "\n"); !strings.Contains(lines, "FLEET_TOKEN") || strings.Contains(lines, "secret-value") {
		t.Errorf("the refusal must be reported by name and never by value: %q", lines)
	}
}

// Per ADR 160 the row only ever warns: it names the difference and lets the
// install proceed.
func TestDoctor_DeliveryModulesRow(t *testing.T) {
	rowFor := func(vars map[string]string) doctorRow {
		return checkDeliveryModules(envOf(vars))
	}

	if r := rowFor(nil); r.Status != statusPass || !strings.Contains(r.Summary, "built-in") {
		t.Errorf("unset: %+v, want a pass naming the built-in path", r)
	}

	dir := t.TempDir()
	if r := rowFor(map[string]string{"FLEET_DELIVERY_MODULES": "relay-a", "FLEET_MODULES_DIR": dir}); r.Status != statusWarn ||
		!strings.Contains(r.Detail, `"relay-a"`) || strings.Contains(r.Detail, dir) {
		t.Errorf("enabled but absent: %+v, want a warn naming the module and not the directory", r)
	}

	installFakeModule(t, dir, "relay-a")
	if r := rowFor(map[string]string{"FLEET_DELIVERY_MODULES": "relay-a", "FLEET_MODULES_DIR": dir}); r.Status != statusPass {
		t.Errorf("enabled and present: %+v, want a pass", r)
	}

	if r := rowFor(map[string]string{"FLEET_DELIVERY_MODULES": "Relay"}); r.Status != statusWarn {
		t.Errorf("an illegal name: %+v, want a warn", r)
	}

	// The row never fails, whatever it finds.
	for _, vars := range []map[string]string{
		{"FLEET_DELIVERY_MODULES": "a,b,c", "FLEET_MODULES_DIR": "/does/not/exist"},
		{"FLEET_DELIVERY_MODULES": "auto,terminal"},
	} {
		if r := rowFor(vars); r.Status == statusFail {
			t.Errorf("%v: the row failed; ADR 160 says a missing module is a warn", vars)
		}
	}

	// It is a real row of the doctor: --skip accepts its id.
	env := testDoctorEnv(map[string]string{"FLEET_MACHINE": "m", "FLEET_TOKEN": "t"})
	env.Skip = map[string]bool{"delivery.modules": true}
	rows := runChecks(t.Context(), env)
	if r := rowByID(t, rows, "delivery.modules"); r.Status != statusSkip {
		t.Errorf("--skip=delivery.modules did not skip it: %+v", r)
	}
}
