# clients/

Working clients of this service. They exist to be run, not just read: a client
that nobody has executed is a guess about the API, and the guesses in
`docs/client-guide.md` were wrong three times before anything ran.

## `fcode.zsh`

A session launcher over the fleet service — list, attach, create, destroy,
watch — in about 320 lines of zsh with no dependencies beyond `curl` and
`python3`.

```sh
export FLEET_URL=http://127.0.0.1:<your port>
source clients/fcode.zsh

fcode                     # every session in the fleet, with state
fcode <prefix>            # attach to the one that matches
fcode watch               # stream state changes
fcode up                  # is the layer up, what build, which machines
fcode new <machine> <name> <cwd>
fcode kill <prefix>
```

Configuration is environment only — there are no machine names in the file:

| variable | meaning | default |
|---|---|---|
| `FLEET_URL` | the service on **this** machine | none — required |
| `FLEET_TOKEN_FILE` | file holding this client's token | `~/.config/colab-fleet/token` |
| `FLEET_SSH_FMT` | how to reach another machine, `%s` = machine id | `ssh -t %s` |

### It is deliberately a second tool

It does not replace, wrap, patch or import any existing launcher, and sourcing
it shadows nothing — the command names differ on purpose. Run both, compare,
and keep whichever earns it. The incumbent stays in production untouched until
somebody decides otherwise.

### What it demonstrates

Three behaviours that are awkward without a session service, and fall out here:

- **One endpoint for the whole fleet.** `fcode` lists every machine's sessions
  in a single round trip. There is no peer list in this file.
- **Attach without knowing the substrate.** The service returns argv; the
  client only decides *where* to run it, by comparing the session's machine
  against `self` from `/v1/machines`. Verified across two machines whose
  multiplexer binaries live at different paths, because they are different
  architectures — a client that hardcoded either would be wrong on one.
- **Watching instead of polling.** `fcode watch` is a push stream. It is
  usually silent, because events fire on state transitions and not on output.

### What it refuses to do

- It never renders an empty list when it could not reach the service; that
  reads as "no sessions" and is a lie.
- It never collapses "not found in a partial view" into "gone" — if a machine
  did not answer, the session may be alive on it, and the exit code says
  unknown rather than absent.
- It quotes `startedAt` back when destroying, so it cannot kill a different
  session that inherited a recycled id.
- It sends an idempotency key on every create.

### Three zsh names that are not free

Written down because all three were hit while building this, and each fails
differently enough to waste an afternoon:

| name | what happens |
|---|---|
| `path` | tied to `$PATH` — a `local path=` empties the search path, and everything afterwards is "command not found" |
| `status` | **read-only** (it mirrors `$?`) — assigning it aborts the function |
| `argv` | the positional parameters |

Also: a `while` loop fed by a **pipe** runs in a subshell, where `local` is
outside any function and prints its declaration instead of declaring quietly.
Feed loops with a here-string.
