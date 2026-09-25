# ADR 160 — what `colab-fleetd doctor` fails on, and what it only names

**Status:** accepted · **Issue:** #160

## Context

`doctor` exists to turn silent installation gaps (#68, #122, #148) into named
rows. Its exit code is what a script or an install procedure gates on, so every
row status is a decision about who gets stopped: `fail` stops the install, `warn`
and `unknown` name the condition and let it proceed.

Two pressures pull opposite ways. Every gap it reports was silent once, which
argues for failing on all of them. But much of this service is off by default
on purpose, and an adopter with no use for a feature would see `doctor` exit 1
for as long as the machine exists.

## Decision

**`fail` means the service will refuse to start, or will refuse a call a
configured setup plainly intends.** Examples: the config does not load; there
is no token and no table; a named supervising principal lacks a grant; peers
are configured but nobody holds `relay`; a table-only machine with peers has no
`system:<self>` credential (#98); a peer refuses the credential; an
unrecognised `mode_class` is set.

**`warn` means the state is legitimate somewhere, and the reason it might be
wrong is named in the row.** Examples:

- `FLEET_INBOX_INDEX` unset. The feature is off, the same "absent means the
  feature does nothing" rule every option in `main.go` follows. On a machine
  that expects inbox delivery, this is #122.
- Single-token mode without `FLEET_ALLOW_MUTATIONS`. A hardened read-only host
  is a documented shape (D6).
- Index entries without `mode_class`. #148's intended day-one state.
- The multiplexer found only on the invoking shell's `PATH`. This is the
  bare-`PATH` trap under a service manager.
- A peer whose build differs from, or cannot be proven equal to, ours.
- A source clone whose pre-commit secret guard is off (`hooks.pre-commit`, #201,
  [ADR 201](201-precommit-guard-visibility.md)). The one row about a clone rather
  than the installation; a clone kept only to read or to build from is legitimate.

A principal one grant short of the full supervisor set warns even when a full
supervisor exists. One missing grant is the shape of drift (a machine added
later with a principal that lacked one grant its peers held). A deliberately
limited client usually lacks several.

**`unknown` means it cannot be answered from this machine**, and the row says
why. The main case is `peer.<m>.grants`: whether a peer grants this machine's
credential `keys` or `send` is not exposed by any endpoint (#106). The row
ships now, always `unknown`, citing #154, and keeps its id when #154 makes the
read possible.

`--skip=<row>` is the explicit "this is deliberate" switch for any row. A
`--skip` that names no row is a usage error (exit 2), because a typo that
silently does nothing is the failure class the command exists for.

## Alternatives rejected

- **Fail on an unset inbox index.** It would make #122 impossible to miss, but
  it would also make `doctor` permanently red on every machine without an index
  writer. That includes every outside adopter, since this repository ships no
  writer. People would learn to ignore the exit code, and the other rows would
  lose their force.
- **Wait for #154 before shipping the peer-grant row.** That would couple a
  local diagnostic to a change in files another branch held at the time. It
  would also leave the local rows unshipped, and those are where every measured
  incident was.
- **Guess the peer's grants from our own table.** The peer compares token
  values against its own table, and no local file describes that table.
  Reporting a guess as `pass` would reproduce the silent-verify problem this
  command was built to end.

## Consequences

- A clean `doctor` exit does not mean a relayed `keys` call will work. Only the
  near half is checked here, and the far half is the peer's own `doctor` run.
  `docs/install.md` step 11 says so.
- Row ids are a contract (`--skip`, `--json` consumers). A row can change its
  answer, but renaming its id is a breaking change.
- `doctor` reads the environment of the process that runs it, not the service
  unit's. `docs/install.md` step 9 prescribes `env -i` with the unit's variables.
