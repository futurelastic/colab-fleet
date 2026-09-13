# Install — from nothing to a running service

[`deploy.md`](deploy.md) starts from a service that already runs: it backs up
the installed binary, restarts through the machine's service manager, and
verifies by asking the running service what it is. On a machine that has never
run this, there is nothing to back up, no path anything executes, and nothing
to restart or ask. This page is the part before that.

The [Quickstart](../README.md#quickstart) is a demo: a foreground process with
a token that lives as long as the shell. This page ends with a service that
survives a reboot and a deploy procedure that applies to it.

It does not install or configure a service manager, for the same reason
`deploy.md` step 3 does not: which one a machine runs is that machine's fact.
It states what the unit must execute, what its environment must contain, and
in what order — and it ends with a command that checks all of it.

## What a working installation is

Every row below is something an installation can be missing and still start.
Several of them are missing silently, and each has cost somebody a day: a
supervising principal without the `keys` grant verifies clean and then refuses
every keypress (#68); an unset inbox index leaves every delivery on the pane
path (#122); an index without a permission-mode class uses the inbox for
nothing while reporting that it delivers to it (#148).

`colab-fleetd doctor` reports each of these as a row — step 9 below — so the
order of this page and the order of its output are the same.

## The procedure

1. **Build with the version-control stamp, and choose where the binary
   lives.** Build from a clean checkout — a binary built from a modified tree
   has no identity (`deploy.md` step 1). Pick an absolute install path and
   keep it: the service unit will execute that path, and it is the value
   `scripts/deploy.sh` later needs as `REMOTE_PATH` (#66). A path nothing
   executes is not an install path.

   ```sh
   go build -o ./colab-fleetd ./cmd/colab-fleetd
   install -m 0755 ./colab-fleetd /usr/local/bin/colab-fleetd
   ```

2. **Pick this machine's id** — `FLEET_MACHINE`. It is the name every peer
   uses for this machine in its own peer list, so choose it once. Unset, it
   defaults to `local`, which no peer can name. Row: `machine.id`.

3. **Pick the listen address** — `FLEET_ADDR`. A specific interface and a
   fixed port. The default is loopback on an ephemeral port, deliberately: an
   unconfigured service is reachable only from its own machine, and it has no
   stable health URL either. Never `0.0.0.0` — the service reads paths and, when
   permitted, starts processes. Loopback is added on the same port
   automatically. Row: `bind.addr`.

4. **Create the state directory**, mode `0700` — `FLEET_STATE_DIR`. Without it,
   idempotency keys and the event sequence live in memory and are lost on
   every restart (defect D5). The service creates it if it is missing; creating
   it yourself lets you choose the owner. Row: `state.dir`.

5. **Write a principal table** — `FLEET_CONFIG`. A fleet runs in table mode:
   one identity per caller, per-verb grants, per-peer credentials. Start from
   an empty document and enrol with the command that validates grants before
   it writes:

   ```sh
   mkdir -p -m 0700 ~/.config/colab-fleet
   printf '{}\n' > ~/.config/colab-fleet/config.json
   export FLEET_CONFIG=~/.config/colab-fleet/config.json

   colab-fleetd principal add supervisor \
     --grants=read,create,send,interrupt,close,rename,discard,keys,relay
   ```

   The token lands in a `0600` file beside the config; clients read it from
   there, never from an environment variable. **Give the supervising client
   the same principal name, with the same grants, on every machine** — a
   machine added later with a differently named principal lacking one grant
   is exactly the drift nothing else surfaces. `keys` is its own grant and is
   denied by default (#68); `relay` is only needed where peers exist.
   Rows: `config.load`, `token.source`, `principals.supervisor`,
   `principals.relay`, `local.mutations`.

6. **Peers, both halves** — only for a fleet. Each direction needs one secret
   in two places, and the far half is invisible from the near machine:

   - On the machine that **owns the sessions** (call it b), enrol the calling
     machine a as a principal holding the verbs a will relay — including `keys`
     if a will relay keys:

     ```sh
     colab-fleetd principal add machine-a --grants=read,send,keys \
       --token-file=./machine-a-on-b.token
     ```

   - On the **calling** machine a, put that token in the peer entry — it is a's
     identity on b, not b's on a — and name a principal for a's own system
     identity holding the **same** token, so a has something to present for
     its long-lived peer reads (#98). The peer compares the token value, never
     the name; a name mismatch is harmless and a token mismatch is fatal:

     ```json
     {
       "principals": [
         { "name": "supervisor", "token": "…", "grants": ["read", "…", "relay"] },
         { "name": "system:machine-a", "token": "<machine-a-on-b token>", "grants": ["read"] }
       ],
       "peers": [
         { "machine": "machine-b", "url": "http://<b's address>:<port>", "token": "<machine-a-on-b token>" }
       ]
     }
     ```

   The address is one **you** confirmed reachable from a, never b's own idea of
   its name. Rows: `peer.self-credential`, `peer.<m>.url`,
   `peer.<m>.credential`, `peer.<m>.reachable`, `peer.<m>.grants`.

7. **Inbox delivery, if this machine has an index writer** —
   `FLEET_INBOX_INDEX`. Absent means every delivery uses the pane path, which is
   correct on a machine with nothing populating an index and is #122 on a
   machine you expected to deliver to an inbox. Where it is set, the writer
   must emit `mode_class` for each entry, and emit the class the session is
   actually running in — an entry without one cannot be attested and is sent
   through the pane path (#148). Rows: `inbox.index`, `inbox.mode-class`.

8. **Write the service unit.** Whatever the machine's service manager is, the
   unit must:

   - execute the absolute path from step 1, with no arguments;
   - set `FLEET_MACHINE`, `FLEET_ADDR`, `FLEET_STATE_DIR`, `FLEET_CONFIG`, and
     where they apply `FLEET_INBOX_INDEX` and `FLEET_PEERS`;
   - set `FLEET_TMUX_BIN` to the multiplexer's absolute path — a service
     manager starts processes with a bare `PATH` that usually lacks it, and
     the failure appears only under the manager, never in your shell
     (row: `runtime`);
   - run as the user who owns the sessions it will manage;
   - restart the process when it exits, and start it at boot;
   - put **no secret** in the unit's environment. With a principal table,
     `FLEET_TOKEN` and `FLEET_ALLOW_*` are ignored; leaving them set only
     misleads the next reader (row: `local.mutations`).

9. **Run `doctor` under the unit's environment, before starting it.**

   ```sh
   env -i FLEET_MACHINE=… FLEET_ADDR=… FLEET_STATE_DIR=… FLEET_CONFIG=… \
     FLEET_TMUX_BIN=… /usr/local/bin/colab-fleetd doctor --principal=supervisor
   ```

   Your login shell's environment is not the unit's. A doctor run from the
   shell answers for a service that does not exist; `env -i` with exactly the
   unit's variables answers for the one that will. It exits `1` if any row
   fails, `0` otherwise; `warn`, `unknown` and `skip` never change that. It
   never prints a token, a path or a peer address, so its output can be pasted
   into an issue as it is. `--json` is for scripts; `--offline` skips the peer
   probe; `--skip=<row>` marks a row deliberate (for example
   `--skip=inbox.index` on a machine without an index writer).

   One row is always `unknown` for now: `peer.<m>.grants`. Whether a peer
   grants this machine `keys` is not readable from this machine (#106) — run
   `doctor` on that peer too.

10. **Start the service and verify it** with a principal's token file:

    ```sh
    curl -s -H "Authorization: Bearer $(cat ~/.config/colab-fleet/supervisor.token)" \
      http://127.0.0.1:<port>/v1/health
    ```

    A `build` in the answer is the service telling you what it is. That URL
    and that token file are what `deploy.md` uses as `FLEET_HEALTH_URL` and
    `FLEET_HEALTH_TOKEN_FILE` from now on.

11. **Then the peer, and only then** — the same procedure on each machine, and
    a `doctor` run on each: a peer's grants to this machine are that peer's row
    to report, not this machine's.

From here on, every change is a deploy: [`deploy.md`](deploy.md).
