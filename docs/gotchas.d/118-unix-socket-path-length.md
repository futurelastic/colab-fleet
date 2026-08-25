# A unix-domain-socket path under the language runtime's default temp dir can be too long to bind, and the failure looks like a permissions problem

**Issue:** #118 (found while probing whether a plain process can create an
endpoint inside another application's private per-process socket
directory)

## What happened

A sandboxed test tried to reproduce "bind a unix-domain-socket file inside a
directory" using Go's `t.TempDir()` as the stand-in directory. `bind` failed
with:

```
bind: invalid argument
```

No permission error, no "name too long" — just `EINVAL`. The actual cause:
`t.TempDir()` nests several path segments deep under this OS's per-process
temp root (`$TMPDIR`, itself already a long, per-boot-randomized path on
macOS), and the resulting path exceeded the ~104-byte `sun_path` limit a
`struct sockaddr_un` is allowed to hold on BSD-derived systems (Linux allows
108). Once the constructed path crosses that limit, the kernel refuses the
bind call outright, with an error code that gives no hint the problem is
length.

## The rule going forward

Any code (or test) that creates a unix-domain-socket endpoint must not rely
on the language runtime's usual "give me a scratch directory" helper
(`t.TempDir()` in Go, and the equivalent in most languages) if that
directory can be deep. Use a short, fixed base instead —
`os.MkdirTemp("/tmp", …)` on Go, or the real, short, fixed path a running
system actually uses for this purpose. This is also *why* long-lived
per-process socket directories on this kind of system tend to live at a
short, hand-picked path rather than under a per-run temp root: it isn't
stylistic, it's this limit.

## The check, if you hit the same symptom

```sh
python3 -c "print(len('<the path you tried to bind>'))"   # >~104 on macOS/BSD, >~108 on Linux ⇒ this is it
```

A generic `EINVAL` from `bind()` on a unix socket, with no permissions issue
otherwise, is worth checking against path length before anything else.
