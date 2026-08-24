# ADR: the create response reports what was APPLIED, driver-built, not service-synthesized

**Issues:** #84, #85, #86 (`group:create-response-contract`)
**Status:** decided

## Context

Three separate, measured defects — a silently-dropped pin still reported as
applied (#84), a session reachable on a runtime-hosted surface with nothing
in the response saying so or where (#85), and a create-time prompt with no
delivery receipt so a caller cannot tell "not delivered yet" from "never
delivered" (#86) — traced back to one mechanical cause none of the three
issues named: `internal/driver.Driver.Create` returned only a bare session
ref, so the HTTP handler had nothing to build the 201 response from except
the caller's own request body. Every field beyond the ref was the request
echoed back wearing the service's voice — the same shape `runtimeForResponse`
already refused to do for one field (§4.3), generalized to the rest.

## Decision

**`Driver.Create` now returns the full session shape `List` already builds
per session, not a bare ref.** The driver — the only party that knows what a
create actually did — builds its own create response; the HTTP handler stops
being a second, poorer producer of that shape. This is not a public API
break: `internal/driver` lives under `internal/`, Go forbids an out-of-tree
implementer, and there was exactly one non-test caller of `Create`.

On top of that plumbing, three new wire fields, each following the same
absent/unresolved-or-pending/resolved three-state discipline this package
already uses for `ConversationRef`/`ResumeOutcome` (§5.7 — absence and
failure are different answers):

- `pins` (#84) — per-pin requested value and, once it can be told, whether
  the runtime is honouring it. `session.agent`/`.model` are redefined as the
  APPLIED values, never an echo of the request.
- `runtimeSurface` (#85) — a sibling of `ConversationRef`, not an extension
  of the local-attach hint (see "Alternatives considered"). Four states
  rather than three: unlike a conversation lookup, "not yet resolved" and
  "settled, none" are operationally opposite here, not the same "look
  again" fact.
- `promptDelivery` (#86) — reuses `send`'s own delivery-outcome vocabulary
  rather than a second one, resolving after the 201 the same way
  `resumeOutcome` resolves after its own create.

All three are read from one shared per-session record a driver writes once
at create time and reads back on every listing, so the 201 body and the
first 200 body cannot drift apart from each other.

## Alternatives considered

**An optional side-interface on `Driver`** (e.g. a second method a driver
may implement to report what it observed), instead of changing `Create`'s
own signature. Rejected: it would put the honest answer behind a type
assertion, which is exactly how a driver ends up silently degrading back to
the fabricated-echo shape this whole change exists to close. §5.6 already
says degrade, never emulate — a driver that cannot fill a field leaves it
empty in the one shape everyone returns, it does not fail to implement a
second interface.

**Extending the local-attach hint to also describe a runtime-hosted surface**
(#85), instead of a new sibling type. Rejected on the attach type's own
stated boundary: its doc comment already says "this machine does not know
how you reach it" for the local-terminal case, and its `command` field is
argv, meaningless for a hosted surface. Splitting the type would have been a
bigger break than adding one.

**A `waitingOn` value for the pending prompt-delivery state** (#86), instead
of a new field. Rejected on a type-correctness ground, not a preference:
`waitingOn` is documented as populated only when status is `waiting_input`,
and the measured harm is a session correctly reading `idle`. Reusing it
would either lie about the present status or break the field's own stated
contract.

**An end-of-options `--` separator, or the `--flag=value` attached form**,
as a fix for the local driver's flag-injection guard on pins (#84).
Rejected/deferred: `--` cannot help because the flags here are interleaved
and it protects positional operands, which this argv has none of; whether
the agent CLI accepts the attached form is unmeasured and not assumed.

## Consequences

- `session.agent`/`.model` on a create response from the local multiplexer
  driver are now empty rather than echoing the request — that driver never
  observes the applied value. Wire-visible; the requested value is not lost,
  it moved to `pins.<field>.requested`, correctly labelled as a request
  rather than a fact.
- A pin value that would be misread as a flag is now refused at creation
  (`invalid`, naming the field) rather than silently dropped — creation can
  now fail for a request that used to silently succeed with a substituted
  default.
- A relay driver now adopts a peer's whole create response instead of
  discarding everything but the bare ref — a relayed create's response
  quality is now bounded by the peer's own driver, not truncated a second
  time on the way through.
