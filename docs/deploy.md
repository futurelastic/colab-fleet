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

0. **Back up what you are about to replace, before anything else.**
   `scripts/fleet-backup.sh` captures the installed binary (checksum-verified
   against its source right after copying), the state directory, and the
   revision the running service currently reports — refusing rather than
   backing up a service it cannot identify if health does not answer with a
   build. This is not optional housekeeping: a build stamped `modified: true`
   has no commit that reproduces it, and the only way back from a bad deploy
   of one is a copy of the binary itself. See `docs/adr/123-backup-stays-separate-from-deploy.md`
   for why this is a separate command rather than something `deploy.sh`
   refuses to run without — the short version is that "no backup" is a policy
   this script would have to invent a default for, not a fact it can check the
   way a dirty tree is a fact.
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

**If a deploy needs undoing, `scripts/fleet-revert.sh` is the other half of
step 0.** It checks the backup against its own manifest before touching
anything — a corrupt backup discovered during a rollback is the worst possible
moment to discover it — installs atomically, re-checksums, restarts, and polls
health the same way step 4 does, refusing to report success until the running
service reports the revision the backup recorded. It restores the **binary**
by default; the **state directory** only behind an explicit `--with-state`,
because state is forward-compatible far more often than not and rolling it
back can discard real session records the new binary wrote correctly. Rolling
back code and rolling back data are different decisions and are not spelled
the same way here.

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

**A verified deploy is not yet a usable inbox delivery path either
(colab-fleet #122).** `deliversToInbox: true` on `GET /v1/runtimes` is
contingent on `FLEET_INBOX_INDEX` being set on that machine — a deploy that
verifies clean but never sets that variable runs with #119's resolver nil,
and every delivery keeps falling through to the pane path exactly as #122
found it doing in production. Setting it is an operator step this script
does not perform and cannot: the directory it names, and what populates it,
are machine-local facts this repository never commits (`cmd/colab-fleetd`'s
own doc comment names the variable; it does not name a value). Check
`deliversToInbox` after any deploy you expect this path to be live on,
rather than assuming a clean verify implies it.

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

Four more variables tune verification itself, all optional and all defaulted
to the prior behaviour (colab-fleet #93 — see the trap below for why they
exist):

- `FLEET_VERIFY_TIMEOUT` — seconds to poll the health URL before giving up.
  Default **180**.
- `FLEET_VERIFY_INTERVAL` — seconds between polls. Default **2**.
- `FLEET_HEALTH_TOKEN` — the literal bearer token to verify with. Takes
  precedence when set.
- `FLEET_HEALTH_TOKEN_FILE` — a path, read on the host via `cat`.

One of the last two is **required** whenever `FLEET_HEALTH_URL` is set
(colab-fleet #108) — see the trap below for why there is no longer a
hardcoded fallback. If your convention is a token file at
`~/.config/colab-fleet/token`, set `FLEET_HEALTH_TOKEN_FILE` to that path
explicitly; it is no longer assumed on your behalf.

## Running the backup and the revert

```sh
scripts/fleet-backup.sh HOST     # captures a peer's binary + state, over ssh
scripts/fleet-backup.sh local    # captures this machine's own
```

Four variables are required, no defaults (colab-fleet #123 — the same
discipline as `deploy.sh`, for the same reason): `FLEET_BIN` (the installed
binary path on the target), `FLEET_STATE_DIR` (the state directory on the
target), `FLEET_HEALTH_URL` (curled ON THE TARGET), and one of
`FLEET_HEALTH_TOKEN` / `FLEET_HEALTH_TOKEN_FILE` — the same credential pair
`deploy.sh` uses for verification (#93, #108), for the same federation reason:
the credential that answers for a peer is not guaranteed to be one the peer's
own on-host token file holds. The backup refuses rather than proceeds if
health does not answer with a build, and refuses rather than trusts itself if
the copy it just made does not checksum-match the source. On success it prints
the exact `fleet-revert.sh` command that undoes it.

```sh
scripts/fleet-revert.sh HOST <backup-dir>                # binary only
scripts/fleet-revert.sh HOST <backup-dir> --with-state    # binary + state, explicit
```

Required: `FLEET_BIN`, `FLEET_RESTART`, `FLEET_HEALTH_URL`, and one of
`FLEET_HEALTH_TOKEN` / `FLEET_HEALTH_TOKEN_FILE` — `FLEET_STATE_DIR` joins that
list only when `--with-state` is passed. It re-verifies the backup's own
checksum against its own manifest before touching anything, installs
atomically, and polls health afterward exactly like `deploy.sh` step 4 does,
refusing to call it done until the reported revision matches what the backup
recorded. `--with-state` is never implied — see the design note under "The
procedure" above for why.

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

**Two more deploys succeeded at every step and still verified as FAILED, for
two more reasons neither of the above covers (colab-fleet #93).**

The first is startup that is not instant. Verification used to probe once and
declare the service dead if that single probe found nothing. Startup does real
work after the process is back — a trust-seed pass and a session
reconciliation over everything the machine was carrying — and that work scales
with how much the machine is carrying, so the busiest machine is the one most
likely to be declared dead. One measured case came back **98 seconds** after
the restart and was healthy from then on; the single-probe script had already
printed the most alarming message it has ("the service did not come back up")
and exited 1, inviting a rollback of a deploy that was already fine. `scripts/deploy.sh`
now polls to a deadline (`FLEET_VERIFY_TIMEOUT`, default 180s — chosen with
slack above that 98s measurement) instead of probing once, and distinguishes
"not up yet" (an interim notice while still inside the deadline — expected,
not alarming) from "did not come up" (the deadline was reached with nothing
ever answering — a real failure). A probe that reaches something concrete — a
real HTTP status, even a bad one — still fails fast rather than waiting out
the whole deadline, because retrying will not change a stable answer like that.

The second is a credential mismatch across a federation. Verification curls the
health URL **on the host**, with a token file read on that same host. On a
federated fleet that file is not guaranteed to hold a credential the far
machine's own service accepts — the credential that answers for a peer can
instead be one only the machine driving the deploy holds. The deploy itself was
completely fine; verification simply asked with the wrong credential and got a
`401` with no build identity in the body — indistinguishable, to the operator,
from the deploy having actually failed, and landing on exactly the step that
talks you out of finishing a two-machine deploy. `FLEET_HEALTH_TOKEN` (a
literal token) and `FLEET_HEALTH_TOKEN_FILE` (an alternate path, still read on
the host) make the credential configuration instead of an assumption. At the
time, neither was required — a caller who set neither still fell back to a
hardcoded path. Colab-fleet #108, below, is what closed that gap.

**A deploy can succeed at every step, verification can reach the service and
authenticate cleanly, and the deploy can STILL report FAILED — because the
credential that authenticated was never going to be accepted (colab-fleet
#108).** The hardcoded fallback the previous paragraph describes
(`~/.config/colab-fleet/token`, read on the host) is correct for a
single-token deployment, where that file conventionally holds the same value
as the service's own token. It is silently wrong for a deployment configured
with a principal table: that value is never one of the table's principals,
every principal in the table authenticates fine, and the one credential a
table-mode deployment cannot accept is exactly the one nobody told this
script to use anything else. `FLEET_HEALTH_TOKEN` and `FLEET_HEALTH_TOKEN_FILE`
are now **required** whenever `FLEET_HEALTH_URL` is set — the script refuses
before making a network call rather than guess, the same call already made
for `REMOTE_PATH` (#66). This is the second instance of one pattern: the
service's own peer credential was empty in table-only mode until the table
was given a way to name the service's own identity (#98); here the default
credential a *caller* presents needed the identical fix — stop assuming a
value that only ever meant something in single-token mode.

**An untracked file the committed ignore rules do not cover makes the build
report itself modified, and a modified build compares equal to nothing.**
This happened: an agent-settings file was ignored on one machine by that
machine's own *global*, private ignore rule — nothing in this repository's own
`.gitignore` knew about it. On a second machine the same path was untracked
and unignored. At the time the script's gate was `git diff --quiet HEAD`,
which only inspects tracked files, so it still passed — but the build it let
through was already stamped `modified: true`, because Go's own VCS stamp
(`cmd/go/internal/vcs`) computes dirtiness from plain `git status --porcelain`,
which flags untracked files too. The two checks disagreed, and the gate's
answer was the wrong one to trust (colab-fleet #139). Two per-issue worktree
checkouts and this repo's own `.claude/plans/`, `.claude/briefs/` reproduced
the same gap later, at repo root.

The fix is two parts, not one: the gate itself now runs `git status
--porcelain` (matching what Go actually checks) instead of `git diff --quiet
HEAD`, so it agrees with the build stamp it is supposed to be a proxy for; and
the paths this repo is expected to always carry untracked — `.worktrees/`,
`.claude/plans/`, `.claude/briefs/` — are in this repository's own
`.gitignore`, not any one machine's private configuration, so a normal working
session does not trip the now-stricter gate. A rule only one machine holds
does not travel to its peer, to a fresh clone, or to CI.

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
