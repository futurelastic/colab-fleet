# `refs/stash` is shared across worktrees in one repo — a bare pop/drop can silently discard another session's work

**Issue:** #226

## What happened

While wrapping a different feature, a session found the main checkout dirty at
start with pre-existing uncommitted work for a *different*, unclaimed issue —
left behind by an earlier session that had edited the main checkout directly.
Following the self-relocate sequence (stash → worktree → pop), that content
was isolated into its own `git stash push -u -m <tag> -- <path>` entry,
kept separate from the wrapping session's own edits.

Between two of that session's own commands — neither a `pop` nor a `drop` —
the entry disappeared from `git stash list` and from `git reflog show stash`
entirely. No trace remained in the stash ref's history at all, not even a
recorded drop. The change itself survived only because git had not yet
garbage-collected the now-unreferenced commit object: it was recoverable with
`git fsck --no-reflog --unreachable --tags`, then grepping the dangling
commits for known content.

There was exactly one other live session on the same machine, in the same
repo, at the time — a plausible but unconfirmed cause, since `refs/stash` is
one ref shared by the main checkout and every `git worktree` added to it. A
bare `git stash pop`/`drop`/`clear` run from *any* of them mutates that same
ref for all of them.

## The rule

A `git worktree` isolates one session's working tree and index from every
other session in the same repo — but `refs/stash` is **not** covered by that
isolation. It lives in the shared `.git` common directory, so any session in
any worktree (or the main checkout) that runs a bare `git stash pop` / `git
stash drop` / `git stash clear` can silently discard *another* session's
stashed work, with no ownership check, and — per this measurement — no
reliable trace left behind once it happens (not even a reflog entry survived,
only the not-yet-GC'd object itself).

Treat `refs/stash` as fleet-shared state within a repo, the same category as
a branch ref, not as private per-worktree state. The safe sequence:

1. `git stash push -u -m "<unique-tag>" [-- <path>]` — tag it identifiably,
   never leave it anonymous among concurrent sessions' entries.
2. Immediately record the entry's SHA: `git stash list --format='%H %gs'`.
   The index (`stash@{n}`) is not stable once another session pushes or pops
   anything on the same ref — the SHA is the only thing that survives that.
3. Restore with `git stash apply <sha>` — **never `pop`**. `apply` leaves the
   stash entry in place until you have confirmed the restore is what you
   wanted; `pop` removes it unconditionally in the same step, which is the
   half of this incident that left no trace to recover from.
4. Drop only the verified SHA, re-finding its *current* `stash@{n}` by the
   tag first (never a hardcoded index) — `git stash drop <verified-sha's
   current index>`.

## What is still not covered

The suspected cause (a concurrent bare `pop`/`drop` from the other live
session) was never confirmed — only the disappearance and the recovery path
were measured directly. Whether a mechanical warning belongs in the `colab`
CLI, in the `code-start`/`code-wrap` skills, or only as a doctor check is a
call for whoever owns that tooling; this repo's own deliverable is this
measurement, recorded so the safe sequence above doesn't have to be
rediscovered the same way twice.
