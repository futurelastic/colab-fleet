# clients/

A working client of this service. It exists to be run, not just read: a client
that nobody has executed is a guess about the API, and the guesses in
[`../docs/client-guide.md`](../docs/client-guide.md) were wrong three times
before anything ran.

| | |
|---|---|
| [`fleetctl.zsh`](fleetctl.zsh) | **A standalone client — no launcher required.** `ls`, `up`, `watch`, `new`, `kill`, `attach`, across every machine in the fleet. Plain zsh; needs only `curl` and `python3`. In daily use on two machines. |

```sh
export FLEET_URL=http://127.0.0.1:<port>
export FLEET_TOKEN_FILE=~/.config/colab-fleet/<name>.token
source clients/fleetctl.zsh
```

## Why only one client lives here

There were two. The other wrapped a specific machine's session launcher, and it
has moved to the workspace that owns that launcher.

The split is not tidiness, and it was measured rather than argued. The launcher
client carried 14 references to the multiplexer and 23 to the launcher it
borrowed its interface from — it could not run without either, and most of its
content was local convention: host aliases, session-name rules, how one machine
reaches another. `fleetctl.zsh` carries **zero of each**, and states "no machine
names in this file" as a property it keeps.

So they are different kinds of thing. One is glue for a particular pair of
machines; the other is **executable documentation of a public API**, which is
what belongs beside the API. That difference also decides exposure: this
repository is public, and a file whose job is naming internal hosts should not
be in it.

**What that costs, said plainly:** the departed client and this service used to
share a revision, because one commit stamped both. It no longer does, and the
service has no way to notice — it reports its own build at `/v1/health`, while a
sourced shell file has nothing equivalent to report. Client/service drift went
from impossible to silent. No mechanism replaces it; the loss is recorded rather
than papered over.
