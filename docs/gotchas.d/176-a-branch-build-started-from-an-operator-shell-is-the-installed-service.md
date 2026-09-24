# A branch build started from an operator's shell runs against the installed service's configuration

**Issue:** #176 (found while live-testing a branch build of the respond path)

## What happened

A freshly built daemon was run with `-h`, meaning to read its usage. It
had no usage flag then: an argument that was neither `doctor` nor a principal
subcommand was ignored and the service started. (#177 fixed that part: `-h`
now prints usage, and any other unknown argument is refused with exit 2
before any startup work. A bare invocation still starts the service, so the
rule below still stands.) The shell it ran in was an
operator's shell, which exports the installed service's `FLEET_*`
settings, so it came up as that service, with the same configuration file,
state directory, multiplexer binary, trust roots and peers. Before it could
fail on the listen address (already bound by the real instance), it had:

- run the trust-seed startup pass against the real trust roots (this time
  it granted nothing);
- reconciled the real state directory against the live multiplexer
  (adopting 25 sessions).

Both are ordinary startup work for the real service, which is why nothing
broke. Only the port collision stopped it. On a machine where the real
instance was down, it would have served the live fleet from an unmerged
branch.

## The rule going forward

Never start a branch build in a shell you have not emptied. Start the
service with `env -i` and name every variable yourself:

```sh
tmux -L probe new-session -d -s t 'claude'           # a throwaway session on a PRIVATE socket
printf '#!/bin/sh\nexec %s -L probe "$@"\n' "$(command -v tmux)" > ./tmux-probe
chmod +x ./tmux-probe
env -i HOME="$HOME" PATH="$PATH" USER="$USER" \
  FLEET_TOKEN=test FLEET_ADDR=127.0.0.1:<spare port> FLEET_MACHINE=testbox \
  FLEET_STATE_DIR=<scratch dir> FLEET_TMUX_BIN=./tmux-probe FLEET_ALLOW_MUTATIONS=1 \
  ./colab-fleetd
```

A private socket plus a wrapper binary is how this service sees only the
throwaway sessions: the driver calls whatever `FLEET_TMUX_BIN` names, and
a wrapper that adds `-L <name>` confines every call to that server. It is
the same `env -i` discipline `docs/install.md` asks of an installed
instance, applied to a test instance, where it is easier to forget.
