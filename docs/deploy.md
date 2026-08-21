# Deploy — from a merged commit to a running service

This is the document `.github/project.yml` refers to. Its `exposure: self` is
justified by one sentence: *merging to trunk reaches nothing by itself; the
service changes only when a human runs the documented build-install-restart
procedure on each machine.* Until now that procedure was not written down —
only [`scripts/deploy.sh`](../scripts/deploy.sh) existed, undiscoverable from
the descriptor and unable to target the machine most likely to need it. This
page is the missing pointer, and the missing case.

If you take one thing from this page: **`scripts/deploy.sh` is the procedure.**
Read its header before reading further — it explains, in the same order as
below, why each step exists and what it refuses to do silently.

## The procedure

1. **Build with the version-control stamp intact.** A binary built from a
   modified tree has no identity and can never be compared against a peer or
   against itself. The script refuses a dirty tree by default; do not override
   that on a deploy you intend to keep.
2. **Install atomically** — write beside the target and rename into place.
   Writing over a running binary is how you get a half-written executable and
   a service that will not start.
3. **Restart through the machine's own service manager.** The script never
   invents one; it runs whatever command you give it and stops there.
4. **Verify by asking the running service what it is**, and compare that
   answer to the commit just built. A deploy that does not verify is a deploy
   that can silently not have happened — this is the step that makes it a
   deploy rather than a copy.
5. **Then the peer, and only then.** One machine deployed is a fleet at two
   revisions, not a finished deploy.

**A verified deploy is not yet a usable one, for `keys` specifically
(colab-fleet #68).** `deliversRawKeys: true` on a runtime is a statement about
what the driver can do, not about who may ask it to — the `keys` grant is
separate, denied by default, and nothing above grants it. A fresh deployment
that never ran `colab-fleetd principal add ... --grants=...,keys` will verify
clean and then refuse every keypress with `401 ... does not hold the keys
grant`, which reads like a permissions bug rather than the setup step it
actually is. Across a peer relay it is two grants, on two machines: `keys` at
the far end that runs the key, `relay` at the near end that forwards the
request there — see api-http.md §3 for why fixing the first refusal does not
fix the call.

## Running it

```sh
scripts/deploy.sh HOST REMOTE_PATH     # a peer, over ssh
scripts/deploy.sh local REMOTE_PATH    # this machine, no ssh
```

The local form exists because the ordinary case is deploying to the machine a
session is already running on, and until now the script could not do that —
its only argument was an ssh destination. Every step is identical either way,
including the read-back at the end; only how a command reaches its target
changes.

`REMOTE_PATH`, `FLEET_RESTART` and `FLEET_HEALTH_URL` are not defaulted, on
purpose — see the script header for why. Set all three, or the script tells
you loudly what it could not do on your behalf. `REMOTE_PATH` joined the other
two after colab-fleet issue #66: a default here is exactly the operational
fact this script otherwise refuses to guess, and guessing it wrong produced a
deploy that looked FAILED while every step had actually succeeded — see the
trap below.

## Traps, measured running this

Each of these cost real time before it earned a place here.

**A deploy can succeed at every step and still verify as FAILED — for two
different reasons that read almost identically (colab-fleet #66).**

The first is an install path the service manager does not exec. `REMOTE_PATH`
used to default to `~/bin/colab-fleetd`; on both machines here the service
definition execs `~/.local/bin/colab-fleetd`. The script wrote a correct,
freshly-stamped binary to a path nothing runs, restarted the service, and the
service dutifully kept running what it has always run. Verification then
correctly reported a mismatch — but its wording named `FLEET_RESTART`, the one
thing that was actually right, because "the running revision does not match"
looks exactly like a bad restart command from the outside. `REMOTE_PATH` is
now required, the same way `FLEET_RESTART` and `FLEET_HEALTH_URL` already
were: the fix is not a smarter guess, it is refusing to guess at all.

The second is a health URL that reaches something, just not this service. A
stray port or an unrelated server on the same host answers with a real HTTP
status — `403` was the one measured — and the script's old `curl -f` discarded
the body on any non-2xx response, so "reached the wrong thing" and "reached
nothing" produced the same empty `RUNNING` and the same generic FAILED. The
script now keeps the status and the first line of the body specifically for
this case, so a wrong URL says *what answered* instead of pointing at whichever
step ran most recently.

**An untracked file the committed ignore rules do not cover makes the build
report itself modified, and a modified build compares equal to nothing.**
This happened: an agent-settings file was ignored on one machine by that
machine's own *global*, private ignore rule — nothing in this repository's own
`.gitignore` knew about it. On a second machine the same path was untracked
and unignored, `git diff --quiet HEAD` still passed because nothing tracked
had changed, but the moment it did not pass was exactly the moment the file
would have been silently eligible for `git add -A`. The fix has to live in
this repository's own `.gitignore`, not in any one machine's private
configuration — a rule only one machine holds does not travel to its peer, to
a fresh clone, or to CI.

**A machine's git metadata can be far behind while its working tree is
current.** A file-sync tool used elsewhere in this fleet mirrors a working
tree between machines but deliberately excludes the git directory — syncing
history has corrupted a repository before, so excluding it was the right call
for that failure. The consequence for deploy: the tree looks right and the
build behaves right, but the revision stamped comes from `.git` on the machine
doing the build, and that can lag behind what the tree already reflects.
Detect it before trusting a deploy from a machine you have not driven in a
while:

```sh
git fetch
git rev-parse HEAD                # what this machine's .git actually has
git rev-parse '@{u}'              # what trunk is, on the remote it tracks
```

If they differ, do not force anything into place — fast-forward only:

```sh
git merge --ff-only '@{u}'
```

A fast-forward merge either lands cleanly or refuses outright; it cannot
silently overwrite a real divergence, which is the property that matters here.
If it refuses, something other than a stale fetch is going on and is worth
looking at before building anything, not worth forcing past.

**A fix on the federated path only helps once BOTH ends run it.** A defect
that lives in how one machine talks to a peer is not fixed by deploying the
patched build to one side — the request still crosses to a peer running the
old code, and the old failure still reproduces, indistinguishable from the fix
having done nothing. Step 5 above is not a formality for this class of change;
verify the peer's own reported revision after its deploy, not only this
machine's.

**Restarting is safe for running sessions.** They live in the multiplexer, not
in this service, so a restart costs a moment of unavailability against this
service's own API — not lost work. Nothing about a session's own state needs
attention before you restart.

## Why this is the procedure the descriptor means

`.github/project.yml`'s `exposure: self` comment and this repository's
`CLAUDE.md` both cite "the documented build-install-restart procedure" as the
reason merging to trunk is not itself a ship. This page, plus
`scripts/deploy.sh`, is that procedure. If either drifts from the other,
trust the script — this page describes it, not the other way round.
