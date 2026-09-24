# `colab-fleetd compat` — does this runtime build still behave the way this service assumes?

`colab-fleetd compat` runs this service's assumptions about the agent runtime
against **one candidate binary** and prints a versioned report. It exists to be
run *before* a fleet takes a new runtime build, not after something has already
quietly stopped working.

It only reports. Installing, pinning, switching and promoting a runtime build are
the caller's decision; nothing here changes any of them.

## Why it exists

This service does not call an API. It drives the runtime through a terminal and
reads a small set of things the runtime writes: the screen grammar its
classifiers read, the composer's geometry and wrap width, how a bracketed paste
is handled, the shape of the transcript entries, and the fields of the runtime's
per-process session record. **No public contract guarantees any of them**, and a
new release can change any of them without raising an error — a changed glyph
silently reclassifies, a moved field silently stops joining. The first sign is a
fleet that has quietly stopped delivering or classifying.

The remedy is to pin one supported build and to test every candidate before the
fleet takes it. This command is the part of that gate that belongs here: the
part that knows what *this service* relies on. What to do with the answer is not
its business.

## Usage

```
colab-fleetd compat --claude <absolute path to a claude binary>
                    [--json] [--pack <dir>] [--only <id[,id…]>] [--timeout <duration>]
```

| Flag | Meaning |
|---|---|
| `--claude PATH` | The candidate. Required, and it must be an **absolute** path to an executable file. It is resolved through symlinks once and launched by that resolved path — never looked up on `PATH`, because a login shell's `PATH` would silently put the installed build back in front of the candidate. |
| `--json` | Print the schema 1 report as JSON on stdout (and nothing else on stdout). Without it, a table. |
| `--pack DIR` | Save the raw evidence each check produced (see [Pack layout](#pack-layout)). The directory must be new or empty. |
| `--only IDS` | Run only these check IDs, comma-separated. The probes they need are pulled in automatically. The report then carries an `only` field: a partial run is not a certification. An ID that is unknown, or known but not implemented in this build, is a usage error. |
| `--timeout D` | Overall limit for the whole run. Default `20m`. |

Both `--flag=value` and `--flag value` are accepted. Progress goes to stderr, so
`--json` output can be piped.

### Exit codes

| Code | Meaning |
|---|---|
| `0` | Every `must` check passed. |
| `1` | A `must` check **failed**: the candidate does something this service does not expect. |
| `2` | Could not certify: a `must` check could not run (see `error` below), or the invocation was bad. A bad invocation prints no report. |

When a `must` check has failed *and* another `must` check could not run, the exit
code is `1`: a proven failure outranks "could not certify", because retrying
cannot clear it.

## Isolation — what a run touches, and what it never does

A check that needs a session runs it in a **private multiplexer server** started
for that run alone:

- **One door.** The driver is built with a small wrapper script as its multiplexer
  binary, and that script is the only thing that names a socket:
  `env -i <minimal environment> tmux -L <label> -f /dev/null "$@"`. `env -i` matters
  as much as `-L`: it drops `$TMUX`, so no command can fall back to an implicit
  "current server", and it makes the server's own environment — which every
  session inherits — exactly the minimal set below. The harness never runs the
  multiplexer except through that script.
- **Proven before it is trusted.** The server must report the expected socket path
  and list only its own keeper session, or the run stops and every check that
  needs a session is `error`.
- **A minimal environment.** `HOME`, `USER`, `LOGNAME`, `SHELL`, `LANG`, `TMPDIR`,
  `TERM`, a bare `PATH`, and `DISABLE_UPDATES=1` and `DISABLE_AUTOUPDATER=1`, so a
  check can never update the binary it is checking. Nothing else is inherited: not
  the caller's variables, and no `CLAUDE_*` names.
- **Throwaway working directories** under a scratch directory (`/tmp/cfc-<nonce>`):
  a trusted root, with folder trust seeded through the driver's own path so that
  path is exercised too, and one directory outside it that is *not* trusted. Every
  one carries a project-local setting that turns remote control off — a
  project-local `false` wins over the user's own setting — so no check registers
  anything outside the machine.
- **The candidate by its resolved path**, as the first word of the command line —
  never `claude` looked up on `PATH`.
- **Cheap model settings** for the few checks that need a turn: the smallest model at
  low effort, synthetic nonce-tagged prompts only. The report's `turns` counts them.
- **The service's own sessions are never addressed.** The harness only ever talks
  to the server it started, and finds its own processes by asking that server, not
  the process table. A test runs the whole thing with `$TMUX` pointing at a
  stand-in server that has a session of its own, and requires that server's session
  ids and panes to be identical afterwards.

### Teardown always runs

On failure, on a panic, and on `SIGINT`, `SIGTERM` or `SIGHUP`, on its own deadline
(so a cancelled run still cleans up): the private server is killed, the socket
removed, the throwaway transcript directories and per-process records deleted, any
straggling process ended (only after its pid *and* start time are verified
unchanged, so a recycled pid is never touched), and the scratch directory removed.
Every step is attempted, and every failure is reported: the report is still printed,
the problems go to stderr, and the exit code is `2`. Every deletion is guarded by
the run's own nonce, so a path that does not carry it is never removed. For the one
case teardown cannot cover — the process being killed outright with `SIGKILL` — the
keeper session runs a small watchdog that waits for the process to disappear and then
removes what it left.

### What a run does leave

- **One folder-trust entry per run** in the runtime's own state file. There is no API
  to remove one, and editing that file while live sessions rewrite it is the race the
  trust seeder exists to survive.
- **The model turns it spent**, against whatever account the runtime is signed in to.
- While it runs, the throwaway sessions are visible to anything that lists the
  runtime's per-process records, as any session is. They are removed afterwards.

A run restricted with `--only` to checks that need no session (the static ones) never
starts a server and creates no scratch directory.

## The report

The report is a **public, versioned contract**. This is a complete example:

```json
{
  "schema": 1,
  "claude": {
    "path": "/path/to/claude",
    "version": "9.9.9",
    "resolved": "/path/to/versions/9.9.9",
    "sha256": "0000000000000000000000000000000000000000000000000000000000000000",
    "arch": "x86_64"
  },
  "colabFleet": {
    "version": "v0.0.0",
    "commit": "abc1234"
  },
  "checks": [
    {
      "id": "F-LIMIT",
      "gate": "warn",
      "pass": true,
      "error": false,
      "detail": "all 4 marker strings are present · relied on by: internal/drivers/tmux/classify.go#usageLimit",
      "reliedOn": ["internal/drivers/tmux/classify.go#usageLimit"]
    }
  ],
  "turns": 0,
  "pass": true,
  "durationMs": 1075
}
```

<!-- compat:fields:begin -->
| Field | Type | Required |
|---|---|---|
| `schema` | integer | required |
| `claude` | object | required |
| `claude.path` | string | required |
| `claude.version` | string | required |
| `claude.resolved` | string | additive |
| `claude.sha256` | string | additive |
| `claude.arch` | string | additive |
| `colabFleet` | object | additive |
| `colabFleet.version` | string | additive |
| `colabFleet.commit` | string | additive |
| `colabFleet.modified` | boolean | additive |
| `checks` | array | required |
| `checks[].id` | string | required |
| `checks[].gate` | string | additive |
| `checks[].pass` | boolean | required |
| `checks[].error` | boolean | additive |
| `checks[].detail` | string | required |
| `checks[].reliedOn` | array | additive |
| `turns` | integer | additive |
| `pass` | boolean | required |
| `only` | array | additive |
| `durationMs` | integer | additive |
<!-- compat:fields:end -->

The **required core** — the fields a caller may rely on — is `schema`,
`claude.path`, `claude.version`, `checks[].id`, `checks[].pass`, `checks[].detail`
and the top-level `pass`. Everything else is additive: a caller must ignore
fields it does not know.

### `error` is not a failure

`pass: false, error: false` means the candidate **behaved differently** from what
this service expects. `error: true` means the check **could not run** — the
environment failed, a timeout left no evidence either way, or a probe it depends
on did not complete. The two are reported apart on purpose so a caller can retry
an error and reject a failure. `error: true` always comes with `pass: false`.

### Gates

Each check has a gate, taken from the catalogue and never chosen by the check:

- **`must`** — anything the driver needs in order to deliver, confirm, classify a
  dialog or identify a session. A `must` check that fails or cannot run keeps
  the top-level `pass` `false`.
- **`warn`** — cosmetic, or a path that is not live yet. Reported; never changes
  the verdict or the exit code.

The top-level `pass` is true when every `must` check passed with no `must` error.
A report that judged no check at all does not pass.

### Partial runs

A run restricted with `--only` carries an `only` array. Its `pass` says the checks
it ran passed; it says nothing about the rest, and is not a certification.

## Versioning

- **Any change to the shape bumps `schema`.** That means removing, renaming or
  retyping a field, or changing what a field means.
- **Adding a field does not.** Callers ignore what they do not know.
- **A check ID is stable once shipped.** Renaming or removing one is a schema bump.
  The list of shipped IDs is `internal/compat/testdata/shipped-ids.txt`, and a test
  fails when a listed ID is missing from the catalogue, when a catalogue ID is not
  listed, or when the file's `schema` header does not match the code — so a rename
  cannot happen by accident.
- A check's `gate` may change without a bump; the gate is data, not shape.

| Schema | What changed |
|---|---|
| 1 | The first version. |

## The check catalogue

Every check names the driver code that depends on the behaviour it asserts. That
text is written once, in the catalogue in `internal/compat/catalogue.go`, and
appears in three places that tests hold together: the ` · relied on by: …` suffix
of every report `detail`, the additive `reliedOn` array, and this table. A
rename in the driver that leaves a `relied on` entry pointing at nothing fails the
build.

<!-- compat:catalogue:begin -->
| ID | Gate | What it asserts | Relied on by |
|---|---|---|---|
| `F-LIMIT` | warn | The candidate still contains the usage-limit notice wording the screen classifier recognises. Static text only: the screen cannot be produced on demand. | `internal/drivers/tmux/classify.go#usageLimit` |
| `F-APIERR` | warn | The candidate still contains the API-error wording the classifier reads to tell a failed turn from a finished one. Static text only: the screen cannot be produced on demand. | `internal/drivers/tmux/classify.go#lastTurnFailed` |
| `H-RC` | warn | The candidate still contains the four remote-control footer labels the control-channel reader maps. Static text only: a check never attaches a bridge. | `internal/drivers/tmux/controlchannel.go#controlStates` |
<!-- compat:catalogue:end -->
