# `filepath.WalkDir` `Lstat`s its own root argument, so a symlinked root is never descended

`internal/trustseed`'s `discoverIslands` called `filepath.WalkDir(root, ...)`
directly on every configured root. `os.Stat(root)` — used first, to check the
root is even present — follows a symlink, so a symlinked root reads as an
ordinary, present directory and is never reported missing. `filepath.WalkDir`
disagrees: it opens its own `root` argument with `os.Lstat`, not `os.Stat`, to
build the `fs.DirEntry` it hands its first callback. A root that is itself a
symlink therefore arrives at that first call as "not a directory," and the
walk stops right there — it never descends into what the symlink points at.

Measured: a configured root that is a symlink to a real directory holding a
repository. A full `SeedAll` pass reported `Islands: 0` for that root — no
error, no `root_missing` count, nothing that would tell an operator their
configuration was wrong. `SeedPath` (the per-session, per-directory entry
point) never hit this, because it is handed a real directory to seed rather
than asked to discover one by walking — so a symlinked root worked for every
session the service itself created, and silently seeded nothing for a session
started any other way under it.

**Fix:** resolve every configured root's symlinks once, at construction
(`New`), into a `configuredRoot{spelling, walk}` pair — never per pass.
`discoverIslands` walks `.walk`, the resolved form, so `WalkDir` sees a real
directory at the root and descends normally. Every "is this under a
configured root" check (`underConfiguredRoot`, and so `SeedPath`'s scope
guard) keeps comparing against `.spelling` — the form the operator actually
wrote, which is also the form a caller's directory arguments are spelled in.
An island found by walking the resolved form is translated back onto the
root's own spelling (`reSpell`) before being handed to `ensureKeys`, so
`lookupKeys` still produces both project-key spellings the runtime might look
the answer up under — the same pair `SeedPath` already wrote for a symlinked
target.

Pinned by `TestSeedAllFindsARepositoryUnderASymlinkedRoot`.
