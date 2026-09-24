package tmux

import (
	"context"
	"path"
	"strconv"
	"strings"
)

// The pane's foreground process must be the runtime (#180 H2).
//
// A composer frame is only paint. When the runtime exits and an interactive
// shell is left in the pane — a session started by hand, `claude` run from a
// prompt — the runtime's last frame stays on screen and reads as an empty
// composer. The shell enables bracketed paste at its own prompt, so the
// bracket-paste gate passes too; the paste lands in the shell's line editor,
// and an Enter runs the message as a command. Measured on a private server:
// a message was executed by the shell and the receipt said queued.
//
// So before the paste, and again in the last look before Enter, this driver
// asks who is in the foreground:
//
//   - the multiplexer's #{pane_current_command} — the name of the pane's
//     foreground process. A live runtime pane reports the runtime's own
//     process title (its version string, measured), never a shell name, so
//     a shell name here is refused outright;
//   - ps over the pane's terminal, for the foreground process group
//     (pgid == tpgid) — a shell leading it is refused;
//   - and, when the runtime's per-process session records are configured, a
//     foreground process whose record matches this session's working
//     directory is positive identity.
//
// A missing record does not refuse by itself: not every runtime build writes
// one, and a false refusal of a healthy session is its own failure. The
// negative checks close the measured case; the positive one is counted, so
// how often it could not be established is visible.

// interactiveShells are program names that read a line and run it.
var interactiveShells = map[string]bool{
	"sh": true, "bash": true, "zsh": true, "fish": true, "dash": true, "ksh": true,
	"mksh": true, "tcsh": true, "csh": true, "nu": true, "xonsh": true, "elvish": true,
	"pwsh": true, "login": true,
}

// isShellName reports whether a process name is an interactive shell's,
// tolerating a login shell's leading "-" and a path.
func isShellName(name string) bool {
	name = strings.TrimPrefix(strings.TrimSpace(name), "-")
	return interactiveShells[path.Base(name)]
}

// paneForegroundFormat is the one query the check makes to the multiplexer.
const paneForegroundFormat = "#{pane_pid}|#{pane_tty}|#{pane_current_command}"

// foregroundIsRuntime reports whether the pane's foreground process is the
// agent runtime, and why not when it is not.
func (d *Driver) foregroundIsRuntime(ctx context.Context, target *paneRow) (ok bool, why string) {
	out, err := d.run(ctx, d.bin, "display-message", "-p", "-t", target.paneID, paneForegroundFormat)
	if err != nil {
		d.counters.incr(counterForegroundUnanswered)
		return false, "the multiplexer could not say which process is in this pane's foreground"
	}
	parts := strings.SplitN(strings.TrimSpace(string(out)), "|", 3)
	if len(parts) != 3 {
		d.counters.incr(counterForegroundUnanswered)
		return false, "the multiplexer's answer about this pane's foreground process did not parse"
	}
	tty, command := parts[1], parts[2]
	if isShellName(command) {
		d.counters.incr(counterForegroundShell)
		return false, "this pane's foreground process is a shell (" + command + "), not the agent " +
			"runtime — whatever is painted on screen, text pasted here would reach the shell"
	}

	group, psOK := d.foregroundGroup(ctx, tty)
	if psOK {
		for _, p := range group {
			if p.leader && isShellName(p.comm) {
				d.counters.incr(counterForegroundShell)
				return false, "this pane's foreground process group is led by a shell (" + p.comm +
					"), not the agent runtime — text pasted here would reach the shell"
			}
		}
		if d.processSessionsRoot != "" {
			for _, p := range group {
				if rec, found := d.readProcessSessionRecord(p.pid); found && rec.CWD == target.cwd {
					d.counters.incr(counterForegroundVerified)
					return true, ""
				}
			}
		}
	}
	d.counters.incr(counterForegroundUnverified)
	return true, ""
}

// fgProcess is one process in a terminal's foreground process group.
type fgProcess struct {
	pid    int
	comm   string
	leader bool
}

// foregroundGroup lists the processes in tty's foreground process group
// (pgid == tpgid). ok is false when ps could not answer; the caller then has
// only the multiplexer's word.
func (d *Driver) foregroundGroup(ctx context.Context, tty string) ([]fgProcess, bool) {
	tty = strings.TrimPrefix(strings.TrimSpace(tty), "/dev/")
	if tty == "" {
		return nil, false
	}
	out, err := d.psRun(ctx, d.psBin, "-o", "pid=,pgid=,tpgid=,comm=", "-t", tty)
	if err != nil {
		return nil, false
	}
	return parseForegroundGroup(string(out)), true
}

// parseForegroundGroup parses `ps -o pid=,pgid=,tpgid=,comm=` output into the
// foreground process group's members. comm is everything after the third
// column, so a name with spaces survives.
func parseForegroundGroup(out string) []fgProcess {
	var group []fgProcess
	for _, line := range strings.Split(out, "\n") {
		f := strings.Fields(line)
		if len(f) < 4 {
			continue
		}
		pid, err1 := strconv.Atoi(f[0])
		pgid, err2 := strconv.Atoi(f[1])
		tpgid, err3 := strconv.Atoi(f[2])
		if err1 != nil || err2 != nil || err3 != nil || pgid != tpgid {
			continue
		}
		group = append(group, fgProcess{pid: pid, comm: strings.Join(f[3:], " "), leader: pid == pgid})
	}
	return group
}
