# A machine with no multiplexer server has zero sessions — reporting it `unreachable` freezes every fleet read

**Issue:** #157 (one idle machine held `complete: false` on every machine in
the fleet for about 17 hours)

## What happened

tmux has no server once its last session closes. That is the normal resting
state of a machine that only sometimes hosts sessions. From then on,
`list-panes -a` exits 1. The tmux driver treated that like any failed listing
and reported the source as `unreachable`. `complete` is true only when every
source is `ok`, so every `scope=fleet` read on every machine came back
incomplete. Meanwhile the peer link to that machine answered fine.

Consumers are supposed to fail closed on `complete: false`, and the one
measured did. It treated every beat as "not measured" and paused decisions
that depend on a complete fleet view, on the machines that were not idle.
Opening one placeholder session on the idle machine fixed it on the next read.

## The rule going forward

"There is no server" is an answer: zero sessions, source `ok`. It is not a
failure to read. But be precise about what counts. Only the multiplexer's own
no-server messages qualify, read from the stderr of a process that exited.
Both were measured against tmux 3.7c:

| stderr | meaning | source |
|---|---|---|
| `error connecting to <path> (No such file or directory)` | no socket file | `ok`, empty |
| `no server running on <path>` | stale socket file, nobody listening | `ok`, empty |
| `error connecting to <path> (Permission denied)` | socket not readable | `unreachable` |
| `error connecting to <path> (File name too long)` | socket path over the cap (see `118-unix-socket-path-length.md`) | `unreachable` |

Matching the `error connecting to` prefix alone is the easy mistake. It turns
a socket the driver cannot read into a machine reported as empty. That is
§5.7's forbidden answer arriving from the other direction.

The classifier is `noServerRunning` in `internal/drivers/tmux/tmux.go`. The
tests are in `noserver_test.go`. One of them runs against the real binary with
an empty socket directory, so a future tmux that rewords the message fails a
test instead of quietly turning idle machines `unreachable` again.

## Reproducing it

```sh
mkdir -p /tmp/nosrv && env -u TMUX TMUX_TMPDIR=/tmp/nosrv tmux list-panes -a; echo "exit=$?"
# error connecting to /private/tmp/nosrv/tmux-<uid>/default (No such file or directory)
# exit=1
```
