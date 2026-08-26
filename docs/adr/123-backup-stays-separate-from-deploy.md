# ADR: backup stays a separate, sequenced step — `deploy.sh` does not refuse
# to run without one

**Issue:** #123
**Status:** decided

## Context

Two scripts were written and used, outside the repository, to carry out a real
two-machine deploy of a binary that `git diff --quiet` could not reproduce
(both machines were running a build stamped `modified: true`). One captures
the installed binary, its checksum, the state directory and the reported
revision before anything moves; the other restores that capture, reinstalls
atomically, restarts, and polls health until the restored revision is
confirmed running. Both are moving into `scripts/`, beside `deploy.sh`.

`scripts/deploy.sh` already refuses to guess an operational fact it has no
business guessing — a dirty tree (no build identity), an unset `REMOTE_PATH`
(#66), a missing verification credential (#93, #108). The question this ADR
answers: should "no backup exists" join that list, so `deploy.sh` itself
refuses to run without one?

## Decision

**No. Backup and deploy stay two separate commands**, sequenced by procedure
and documentation, not enforced by one script requiring the other's evidence.
`docs/deploy.md`'s numbered procedure now opens with backup as **step 0**,
before the build-identity check that was step 1 — worded with the same weight
as every other step, not as an appendix a reader can skip past.

This mirrors a design decision already made one script over: `fleet-revert.sh`
restores the binary by default and the state directory only behind an explicit
`--with-state`, because rolling back code and rolling back data are different
decisions that must not be spelled the same way. Backing up and deploying are
also different decisions — one produces an undo path, the other performs the
forward action — and coupling them the same way would remove exactly the
flexibility that distinction protects: running one backup before a batch of
deploys, or deploying to a host whose backup posture is handled by a mechanism
this repository does not own.

## Alternatives considered

**`deploy.sh` refuses unless a backup exists.** Rejected: "exists" is not
answerable without inventing new operational facts this script does not
currently know — where backups live (a convention `fleet-backup.sh` owns, not
`deploy.sh`), and how recent is recent enough. A dirty-tree check is a yes/no
fact about the tree in front of the script; "is there an acceptable backup" is
not a fact, it is a policy this script would have to invent a default for —
the same class of silent guess `REMOTE_PATH` (#66) and the verification
credential (#93, #108) were fixed by refusing to make, not a good precedent to
extend into new territory.

**`deploy.sh` refuses unless a backup was taken in this same invocation
(spawns `fleet-backup.sh` itself).** Rejected: it would make every deploy carry
`fleet-backup.sh`'s own required variables (`FLEET_STATE_DIR`, plus wherever
backups are conventionally kept) even for a caller who already holds a
sufficient backup from a minute ago, or who is scripting backup once against
several sequential deploys. It also changes what a `deploy.sh` failure means —
today it means "the build, install, restart or verify step did not succeed";
coupling it to backup would let a healthy build fail the whole run over an
unrelated concern (state directory unreadable, disk full on the backup
destination) that has nothing to do with whether the deploy itself worked.

**A soft warning instead of a hard refusal (print, do not exit).** Rejected on
this repository's own established pattern: `FLEET_HEALTH_URL` unset already
demonstrates what a soft warning is for here — skipping *verification*, an
optional add-on to a deploy that already happened. Skipping a *backup* is not
analogous: it is choosing to have no way back from a build with no other
identity, before the fact, which is exactly the condition #123's own incident
was filed to describe. A warning nobody stops to read is not different in
practice from no warning at all; a step in the documented procedure is read
because the procedure is what an operator follows.

## Consequences

- `scripts/deploy.sh` gains no new required variables and no new failure mode
  from this issue. Existing automation that calls it directly is unaffected.
- The safety this issue is about is carried entirely by `docs/deploy.md`:
  backup is step 0, phrased with the same "refuses rather than proceeds on
  doubt" weight the rest of the procedure already uses, immediately followed
  by the two commands (`scripts/fleet-backup.sh`, `scripts/fleet-revert.sh`)
  that make it real.
- This is a documentation-enforced discipline, not a script-enforced one — an
  operator who skips step 0 is not stopped by anything but the page. If a
  future incident shows that gap is real (someone measurably ran a deploy
  with no backup because nothing stopped them, the same way #66's `REMOTE_PATH`
  gap and #108's credential gap were each found by actually hitting them),
  this decision is the one to revisit — not to relitigate. Revisiting it with
  a specific caller's need in hand keeps whatever runtime check follows from
  being the same kind of guess this ADR just rejected.
