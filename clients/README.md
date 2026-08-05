# clients/

Working clients of this service. They exist to be run, not just read: a client
that nobody has executed is a guess about the API, and the guesses in
[`../docs/client-guide.md`](../docs/client-guide.md) were wrong three times
before anything ran.

| | |
|---|---|
| [`fcode/`](fcode/) | **A session launcher over this service.** `fcode` (this machine), `sfcode` (the whole fleet), and `fleetctl` (standalone, no launcher needed). Installed and in daily use on two machines. |

Start at [`fcode/README.md`](fcode/README.md). Its
[`NOTES.md`](fcode/NOTES.md) is the more interesting half — six things that
cost real time, including the subscription that exhausted a machine and made a
supervisor report 67 live sessions as vanished.
