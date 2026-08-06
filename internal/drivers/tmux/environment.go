package tmux

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	fleet "github.com/godx-jp/colab-fleet"
)

// The environment a created session receives, and the record of it.
//
// # The problem, stated as it was measured
//
// This driver spawns `new-session -d -s <name> -c <cwd> -- <argv>`. The `--`
// means the agent binary is executed directly by the multiplexer: not a login
// shell, not a shell at all. So the agent inherited the SERVICE's environment —
// which is written by hand in a process-manager unit and holds that service's
// own configuration and nothing else. No agent credentials, because there was
// nowhere for them to come from.
//
// A launcher-created session takes the opposite route deliberately, through a
// shell that reads the user's startup files, specifically so the credentials
// those files export for the agent's tool servers are present.
//
// # Why the obvious fix is not `-lc`
//
// "Wrap it in a login shell" is the right instinct and the wrong flag, and the
// difference is the whole defect in miniature.
//
//	<shell> -lc  → login, NOT interactive
//	<shell> -lic → login AND interactive
//
// A non-interactive login shell reads the login startup files and NOT the
// interactive one. On a normal developer machine the interactive file is where
// credentials are exported — so `-lc` produces a session that is genuinely
// started through a genuinely-a-login-shell, and still has none of them.
//
// Measured on this runtime with a cleared environment, asking for a variable
// the interactive file exports:
//
//	<shell> -lc  '...'  → MISSING   (16 PATH entries)
//	<shell> -lic '...'  → present   (19 PATH entries)
//
// That is exactly the false pass the issue behind this work warned about: the
// session starts, the mechanism looks right, and the failure waits until the
// agent's first tool call. So the wrap is interactive, and SessionEnvironment
// carries `interactive` as a field rather than an assumption.
//
// # The residual cost, named rather than hidden
//
// The environment now depends on a file this service does not own, cannot
// validate and cannot version. That was accepted deliberately: parity is
// automatic, including for a credential somebody adds next month who never
// reads any of this. What the record below buys is that the dependency stops
// being invisible — the service cannot control the file, but it can say what
// came out of it.

// envCaptureWindow bounds the wait for a session to write its record. An
// interactive shell reading a real startup file is not instant, and a window
// too short would report "no environment" for a session that has a perfectly
// good one — which is the confident wrong answer §5.7 exists to prevent.
const envCaptureWindow = 20 * time.Second

// envCaptureInterval is the poll gap while waiting for the record.
const envCaptureInterval = 250 * time.Millisecond

// defaultLoginShell is used when the service's own environment does not say
// what the user's shell is. A process manager frequently does not export
// SHELL, so this is the ordinary case rather than the fallback.
const defaultLoginShell = "/bin/zsh"

// loginShell reports the interpreter the agent argv is wrapped in.
func (d *Driver) loginShell() string {
	if d.shell != "" {
		return d.shell
	}
	if s := os.Getenv("SHELL"); s != "" {
		return s
	}
	return defaultLoginShell
}

// envRecordScript runs inside the created session, immediately before the
// agent replaces it.
//
// Three properties, each of which took a measurement to get right:
//
//   - It never writes a VALUE. `sed 's/=.*//'` keeps only the part before the
//     first '=', so no byte of any credential can reach the file even
//     momentarily. A file that briefly holds secrets is a file that holds
//     secrets when the process crashes before deleting it.
//
//   - It is robust against values that contain newlines. `env -0` emits
//     NUL-separated records; deleting newlines FIRST, then splitting on NUL,
//     means an embedded newline cannot fabricate an extra line that looks like
//     another variable. The naive `env | sed` form invents names out of value
//     content — verified by exporting a value containing "FAKE=injected" and
//     watching FAKE appear in the output.
//
//   - It cannot break the session. Redirection failure, a missing `env`, a
//     read-only directory: all are swallowed, and the `exec` runs regardless.
//     Recording the environment is worth nothing if it can cost a session.
//
// The agent argv is bound as POSITIONAL PARAMETERS, never spliced into this
// string — `$1` is the record path, and `shift; exec "$@"` runs the caller's
// command. Nothing the caller supplies is ever parsed as shell syntax, so the
// usual quoting hazard around a name or a working directory does not exist
// here at all.
const envRecordScript = `{ printf '%s\n' "$PATH"; env -0 | tr -d '\n' | tr '\0' '\n' | sed 's/=.*//' | sort; } > "$1" 2>/dev/null || true
shift
exec "$@"`

// loginWrap wraps the agent argv in a login+interactive shell, and returns the
// argv the multiplexer should run.
//
// recordPath may be empty, in which case the environment is still inherited
// from the shell but nothing is recorded — the wrap is the fix, the record is
// the evidence, and one working without the other is a legitimate state.
func loginWrap(shell, recordPath string, argv []string) []string {
	wrapped := []string{shell, "-lic", envRecordScript, "colab-fleet", recordPath}
	return append(wrapped, argv...)
}

// envRecordPath picks somewhere to stage one session's record.
func (d *Driver) envRecordPath() string {
	dir := os.TempDir()
	if d.store != nil {
		if sub := filepath.Join(d.store.Dir(), "env"); os.MkdirAll(sub, 0o700) == nil {
			dir = sub
		}
	}
	return filepath.Join(dir, "env-"+d.nonce())
}

// captureEnvironment waits for a created session to write its record, parses
// it, and remembers it against the session id.
//
// Runs in its own goroutine with its own deadline: the caller's Create has
// already returned by the time an interactive shell finishes reading a startup
// file, and blocking a create on a diagnostic would be the wrong trade in the
// other direction.
func (d *Driver) captureEnvironment(id, recordPath string) {
	ctx, cancel := context.WithTimeout(context.Background(), envCaptureWindow)
	defer cancel()
	defer os.Remove(recordPath)

	env := fleet.SessionEnvironment{
		Shell:       d.loginShell(),
		Login:       true,
		Interactive: true,
	}
	for {
		raw, err := os.ReadFile(recordPath)
		if err == nil && len(raw) > 0 {
			at := d.now()
			env.Known = true
			env.CapturedAt = &at
			env.Path, env.Names = parseEnvRecord(string(raw))
			env.ServicePath, env.ServiceNames = serviceEnvironment()
			d.rememberEnvironment(id, env)
			return
		}
		select {
		case <-ctx.Done():
			// Honest failure, not an empty record: a caller must be able to
			// tell "the session had nothing" from "we never found out".
			env.Reason = "the session did not write an environment record within the capture window"
			env.ServicePath, env.ServiceNames = serviceEnvironment()
			d.rememberEnvironment(id, env)
			return
		case <-time.After(envCaptureInterval):
		}
	}
}

// parseEnvRecord splits the record into PATH entries and variable names. The
// first line is PATH; every line after it is one name.
func parseEnvRecord(raw string) (path, names []string) {
	lines := strings.Split(strings.TrimRight(raw, "\n"), "\n")
	if len(lines) == 0 {
		return nil, nil
	}
	if lines[0] != "" {
		path = strings.Split(lines[0], ":")
	}
	for _, n := range lines[1:] {
		if n = strings.TrimSpace(n); n != "" {
			names = append(names, n)
		}
	}
	return path, names
}

// serviceEnvironment enumerates this process's own environment, by the same
// rule and with the same discipline: names only, plus PATH.
func serviceEnvironment() (path, names []string) {
	for _, kv := range os.Environ() {
		name, value, ok := strings.Cut(kv, "=")
		if !ok || name == "" {
			continue
		}
		names = append(names, name)
		if name == "PATH" && value != "" {
			path = strings.Split(value, ":")
		}
	}
	sort.Strings(names)
	return path, names
}

func (d *Driver) rememberEnvironment(id string, env fleet.SessionEnvironment) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.environments == nil {
		d.environments = map[string]fleet.SessionEnvironment{}
	}
	d.environments[id] = env
}

// Environment reports what a session's process received (see
// fleet.SessionEnvironment).
//
// In memory only, and deliberately so: it describes one process's environment
// at one instant, and a service restart does not make the old answer stale — it
// makes it unowned. A record restored from disk would claim to describe a
// session this instance never started and never watched.
//
// A session this driver did not create answers Known=false with a reason. That
// is the honest answer rather than a gap: the record is written by the wrapper
// this driver installs, and no supported means exists on this substrate to read
// an arbitrary process's environment after the fact — `ps` does not expose it,
// which was checked rather than assumed.
func (d *Driver) Environment(ctx context.Context, req fleet.Request, ref fleet.SessionRef) (fleet.SessionEnvironment, error) {
	d.mu.Lock()
	env, ok := d.environments[ref.ID]
	d.mu.Unlock()
	if ok {
		return env, nil
	}
	path, names := serviceEnvironment()
	return fleet.SessionEnvironment{
		Reason: "no environment record: this session was not created by this service instance, " +
			"or the instance has restarted since it was",
		ServicePath:  path,
		ServiceNames: names,
	}, nil
}
