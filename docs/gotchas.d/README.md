# `docs/gotchas.d/` — one file per gotcha

This directory holds long-lived gotchas (bites again, not tied to one
feature) — one file per gotcha, never a single growing document. That is the
whole design: no shared counter to collide on, no citation to break when
something gets renumbered.

## Naming

`<issue-number>-<slug>.md` — the issue that surfaced the gotcha, then a short
slug. Example: `142-worktree-stash-collision.md`.

The issue number is the stable id. Citations elsewhere in the repo reference
the gotcha by this filename (or by issue number), never by a position/section
number — a filename never needs renumbering when a neighbour is added or
removed.

## Writing an entry

One file, one gotcha. Say what broke, the measured evidence (a number, an
incident, a specific failure), and the fix or the rule it produced. Keep it
short enough that the filename plus the first line already tells a reader
whether it's the one they're looking for.

**Append-only.** A new gotcha is a new file — never edit another entry's file
to extend it, and never insert into the *middle* of the directory's implied
order (there isn't one; filenames sort however they sort). Two branches
adding a gotcha each touch a different file, so they never conflict at merge.

## `docs/gotchas.md`, if this repo has one

An existing `docs/gotchas.md` is optional and orthogonal to this directory —
a **curated, hand-maintained topical guide** that points *into* entries here
(`See docs/gotchas.d/142-slug.md`), grouped however reads best for a human
skimming for a topic. It is never a second copy of an entry: don't paste a
gotcha's content into both places. If it drifts out of date, that's a stale
index, not two wrong documents — the entries in `gotchas.d/` stay the source
of truth either way.

Migration is lazy: an existing `docs/gotchas.md` is not split apart on
adoption. Only new gotchas from here on go into `gotchas.d/`.
