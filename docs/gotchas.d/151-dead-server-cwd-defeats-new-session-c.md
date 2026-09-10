# A multiplexer server whose own working directory was deleted hands every new pane that dead directory, and `new-session -c` does not rescue it

**Issue:** #151 (a session created through the API exited within a second of a
`201`, reported `dead` with an empty cwd)

## What happened

A long-running tmux server (3.7c, macOS) had been started from a directory
that was later deleted. From then on, every **new** pane started with that
dead directory as its cwd: inside the pane `PWD=.`, `pwd` printed `.` or
`pwd: .: No such file or directory`, and `process.cwd()` failed with `ENOENT`.
This held **even though `new-session -c <existing directory>` named a
directory that existed**. Two different existing directories were probed and
both got the dead cwd.

Nothing on the create path can see it. `new-session` exits 0, the session
exists, and the create answers success. The agent then exits at once ("the
current working directory was deleted"), so the session is dead a second
later with no error anywhere a caller reads.

## The rule going forward

Do not treat `-c` as the thing that puts a process in its directory. Anything
that starts a pane and cares where it runs must `cd` to an **absolute** path
from inside the pane, then `exec`. An absolute `cd` escapes the inherited dead
directory; this was verified on the same server state. Keep `-c` as well,
because it is still what the multiplexer reports as the pane's path.

The driver does this in its login-shell wrapper (see `envRecordScript` in
`internal/drivers/tmux/environment.go`). A **bare-exec** session has no shell
to run the `cd`, so it is still exposed. The same applies to any script that
calls `tmux new-session -c … -- <binary>` directly.

## Reproducing it

```sh
mkdir -p /tmp/x/dead /tmp/x/target
(cd /tmp/x/dead && tmux -L probe new-session -d -s boot sleep 600)
rmdir /tmp/x/dead
tmux -L probe new-session -d -s t -c /tmp/x/target -- sh -c 'pwd > /tmp/x/out 2>&1'
cat /tmp/x/out        # pwd: .: No such file or directory
tmux -L probe kill-server
```

The fix for an already-affected server is to restart it. The fix that makes a
launcher indifferent to how its server was started is the explicit `cd`.
