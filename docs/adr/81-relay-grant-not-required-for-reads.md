# ADR: a peer-reaching read stays on the `read` grant, never upgraded to `relay`

**Issue:** #81
**Status:** decided

## Context

`mutating()` upgrades the required grant from the specific verb grant to
`GrantRelay` whenever a mutation's target machine is not the one the caller
talked to — reaching another machine to change its state is treated as its
own privilege, separate from being allowed to make that change at all
(colab-fleet #68's federated-keypress finding is the same shape: a proxied
mutation needs both the local verb grant and `relay`).

Closing #80 added `reading()`, the read-side counterpart that finally checks
`GrantRead` on every read route — until then any authenticated principal
could list every session on every configured peer regardless of its grant
list. That fix deliberately left one question open rather than deciding it
as a side effect: should a `scope=fleet` or peer-targeted read *also* cost
`relay`, the way a relayed mutation does?

## Decision

**No. A read that reaches beyond the machine the caller talked to needs only
`GrantRead`, never `GrantRelay`.** `reading()` does not carry `mutating()`'s
relay-target upgrade, and this is now documented as deliberate rather than
an oversight, in `docs/spec/api-http.md` §5, `docs/api.md`, and at
`GrantRelay`'s declaration in `internal/service/auth.go`.

The reasoning: a relayed mutation changes state on a machine the caller is
not talking to; a relayed read does not. Requiring the same grant for both
treats reaching and changing as one act, and they are not — `mutating()`'s
own argument for gating relay is specifically about altering another
machine's state, and that premise does not hold for a read.

## Alternatives considered

**Symmetric — require `relay` for any cross-machine read too.** Applies
`mutating()`'s upgrade uniformly regardless of verb. Rejected on a measured
cost, not a hypothetical one: at least one principal this fleet depends on
for its fleet-wide read (the surface this fleet is actually observed
through) holds `read` without `relay` today, and this option would have
refused that call silently the moment it landed, with no error a caller
could act on to fix it.

**A third grant, dedicated to cross-machine reach.** Separates "may read"
from "may reach a peer" without conflating peer reads with peer writes —
the most precise of the three shapes, and not rejected on its merits. Not
taken now because it costs a new grant in the model plus a migration for
every existing principal, to buy a distinction nothing has yet been harmed
by. Worth revisiting if the grant model is reworked for another reason.

## Consequences

- No behaviour changed by this decision — `reading()` already worked this
  way; what changed is that the asymmetry is now written down as chosen,
  not silent.
- The principal relying on fleet-wide `read` without `relay` keeps working,
  unaffected.
- If a future need arises to grant `read` locally while withholding peer
  reach specifically, that is the trigger to revisit option C above — not a
  reason to reopen this decision on its own.
