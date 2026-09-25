# 201 — the pre-commit secret guard is per-clone, so its absence is reported, not repaired

**Issue:** #201 (follows #199)
**Status:** decided; the direction was left open by the issue and is a choice
between three, recorded here so it can be reopened on evidence.

## Context

The guard that runs before a value is published is `.githooks/pre-commit`. It is
switched on by `git config core.hooksPath`, which is stored in one clone's own
`.git/config`. Nothing committed to the repository can carry it into another
clone, so a clone that never ran `.githooks/install.sh` commits with no scan in
front of it and prints nothing.

Measured: in one such clone a commit quoting a made-up 16-character hex string
under a key-looking name went through with no output. The setting was unset and
the hooks directory held only git's samples. Staging the same line in a scratch
repository and running the scanner by hand exited 1 with a `generic-api-key`
finding, so the hook, had it been on, would have refused that commit. The value
was invented and the commit was replaced before anything shipped; the point is
that the guard did not run and nothing said so.

It matters more since #199. That ruling declined a CI scan of branch pushes on
the premise that the hook is the guard that works before a value is published.
That is true of the hook where it is on. Where it is off, a key-shaped value
reaches a public branch with no scan in front of it, and the first scan to read
it is trunk's, after landing.

## Decision

**Report the absence: a `doctor` row, `hooks.pre-commit`.**

- It is answered only when `doctor` is run from inside a clone of this repository
  (a tracked `.githooks/pre-commit` beside the service's command directory), and
  is `skip` anywhere else. Every other row describes the installation; this is the
  one about a clone, and a service host usually has none.
- It asks git which file it would run as the pre-commit hook
  (`git rev-parse --git-path hooks/pre-commit`), not whether one spelling of the
  setting is present. That folds in a relative or absolute path, a linked worktree
  and a global setting. The file is then compared with this repository's tracked
  hook, and must be executable, because git skips a hook without the bit.
- It also requires `gitleaks` on `PATH`. With the hook on and no scanner, the hook
  prints a notice and exits 0, so a `pass` that ignored it would be the silent
  verify `doctor` exists to end.
- **`warn`, never `fail`** (ADR 160). A clone kept only to read or to build from
  legitimately has no hook, and the hook is no dependency of the service. The row
  names why it might be wrong and the one command that fixes it, and `--skip`
  marks a clone that never commits.
- The tests run real git against repositories they build, and switch the hook on
  with the repository's own installer, so a change to how the installer enables it
  cannot leave the row answering for a spelling nobody uses.

## Alternatives rejected

- **Run the installer when a session opens in this repository.** It closes the
  gap for whoever goes through that opening and for no one else: a person
  committing from a terminal or an editor never passes it, and neither does a
  clone made by an outside contributor. It also writes to a clone's git
  configuration unasked, which a report does not. And it is still silent when it
  fails or is not used, so a report is needed underneath it anyway. It stays
  available as a later step on top of this one; it is a different decision, a
  write rather than a read.
- **Say so in the documentation and stop.** Done as well (`docs/internals.md`,
  `CLAUDE.md`), but a paragraph nobody is reading at the moment of the commit does
  not make an absence visible. That was the gap.
- **A Go test that fails when the hook is off.** A test would be reading whichever
  clone it runs in. CI's checkout never has the setting, so it would fail every CI
  run or be skipped there; failing a developer's suite over their own git
  configuration is the wrong instrument, and `go test` is not run before every
  commit.
- **`fail`.** It would leave `doctor` permanently red for anyone who keeps a clone
  to read, and teach people to ignore the exit code, which is the argument ADR 160
  already made for the inbox index.

## Consequences

- **The gap is visible on request, not pushed at the moment of a commit.** Nothing
  local can warn at that moment: the only thing git runs then is the hook that is
  missing. The row is only as useful as the times `doctor` is run from a clone, and
  the build-install-restart procedure is one of them. `CLAUDE.md` tells a session
  to look before its first commit in a clone it has not used.
- The row id `hooks.pre-commit` is a contract (ADR 160): `--skip` and `--json`
  consumers key on it.
- `doctor` now runs `git` for one row. It is read-only, and it drops the variables
  that make git ignore the directory it was pointed at (a hook or a rebase step
  sets them), so the answer is for the clone asked about.
- The row reads the invoking shell's git configuration and `PATH`, as every row
  reads the invoking shell's environment. A commit made from an editor with a
  barer `PATH` can still find no scanner; the row says what this shell would do.
- A `pass` means git would run this repository's hook and the scanner is
  findable. It does not say the scanner is the pinned version, that the hook's
  content is unmodified, or that nobody passes `--no-verify`.
