# ADR: deploy verification's default credential is removed, not replaced

**Issue:** #108
**Status:** decided

## Context

`scripts/deploy.sh` verifies a deploy by curling a health URL on the target
host and comparing the reported revision to the one just built. Until now,
when neither `FLEET_HEALTH_TOKEN` nor `FLEET_HEALTH_TOKEN_FILE` was set, it
fell back to reading `~/.config/colab-fleet/token` on the host.

That fallback is correct for a single-token deployment, where the file
conventionally holds the same value the service itself checks incoming
requests against. It is silently wrong for a deployment configured with a
principal table: that value is never one of the table's principals, so the
service correctly rejects it with `401`. Measured on a real deploy: every
step succeeded — build, install, restart, and the new revision confirmed
running by a direct query — and the script still reported `FAILED`, because
verification alone used the wrong credential.

This is the second instance of one pattern. The first (#98) was the
service's OWN peer credential being empty in table-only mode, fixed by
letting the table name the service's own identity. Here the defaulted
credential a *caller* (this script) presents needed the same class of fix,
not the same mechanism — see Alternatives.

Established from the script before designing around it: verification's curl
already runs ON THE HOST (`run()` — ssh for a peer, `sh -c` for local), so the
token-file read was already host-side. "Reading the wrong machine's config"
was never this bug; the bug was that the *value* defaulted to was only ever
correct for one deployment shape.

## Decision

**Refuse to guess.** `FLEET_HEALTH_TOKEN` or `FLEET_HEALTH_TOKEN_FILE` is now
required whenever `FLEET_HEALTH_URL` is set. With neither present, the script
exits 2 before making any network call, naming both variables and why one is
needed. Once either is set, behaviour is unchanged from #93. This is the same
call already made for `REMOTE_PATH` (#66): "the fix is not a smarter guess,
it is refusing to guess at all."

## Alternatives considered

**Read a principal out of the host's principal table.** Rejected on access
alone: `FLEET_CONFIG` (the table's path) is a daemon-only env var, never
threaded through to this script — the same category of operational fact this
script already refuses to guess for `REMOTE_PATH` / `FLEET_RESTART` /
`FLEET_HEALTH_URL`. Even granted the path, a table can hold more than one
principal, so this still needs a second decision — which one — that is
either an arbitrary pick or a second new variable naming it. Not a smaller
guess than today's; the same guess with more steps and a new configuration
surface to hold it.

**Reuse the service's own outbound peer identity** (`system:`+machine,
`selfCredential`, #98). Rejected for the same access problem as above (still
needs the table's path to find that entry), plus one #108's own filing
flagged as unestablished: nothing guarantees that identity carries a local
`GrantRead` grant on the SAME host's own table. #98 only required the
**peer's** table to grant that identity read — for a different purpose,
authenticating this host's own outbound subscriptions to a peer, not for
authenticating a caller checking this host's own health. Grants are
per-principal and explicitly configured (`internal/service/auth.go`), never
implied by a name; reusing a credential scoped for peer-federation as a
general verification credential would also be a scope widening in the cases
where it happened to work.

Both alternatives trade one silent, sometimes-wrong guess for a different
guess plus new configuration surface. Requiring the two variables #93
already added needs zero new surface — it only removes the implicit fallback
whose sole virtue was not asking, which is exactly what produced the false
`FAILED`.

## Consequences

- A deploy to a host this script has never successfully verified against
  before now requires the operator to set `FLEET_HEALTH_TOKEN` or
  `FLEET_HEALTH_TOKEN_FILE` explicitly — no deployment shape gets a free
  pass, including single-token ones that previously worked by coincidence of
  convention (the file happening to hold the right value already).
- Verification itself is unchanged: still not skippable, still fails loudly
  (never a warning) on a genuine mismatch or an unreachable service. Omitting
  `FLEET_HEALTH_URL` entirely still skips verification with its own loud
  warning — untouched by this change.
- No new configuration surface, no new script dependency (no JSON parsing of
  a table this script was never given the path to).
