// Package fleet defines the wire and domain types shared by every
// colab-fleet client and driver.
//
// The split is deliberate: this package depends on nothing under internal/,
// so a third party writing only an HTTP client against
// docs/spec/api-http.md can import it without pulling in a driver, a
// service, or anything that starts a process. Drivers live in
// internal/driver; the HTTP service and its routing live in
// internal/service. See docs/spec/session-abstraction.md for the domain
// model this package transcribes and docs/spec/api-http.md for the wire
// protocol built on it.
//
// The package name is "fleet", not "colabfleet" — the module's "colab-"
// prefix is an org/product namespace, not part of the identifier a caller
// types at every call site (fleet.SessionSpec, fleet.Collection[...]).
//
// # Findings from transcribing the spec
//
// Each of these is a place the prose admitted more than one reading, or
// where the two spec documents' own pseudocode didn't survive being made to
// compile. Resolutions are recorded next to the type or function they
// touch; this list is the index.
//
//   - Ack (ack.go): named in session-abstraction.md §3's operations table
//     but never given a shape, unlike DeliveryReceipt. Given one here.
//   - Collection[T]'s Complete field (collection.go): the spec says it's
//     "false if any source failed to answer" but never says who computes
//     it, or whether a "degraded" (answered, but unhealthy) source counts
//     as a failure the same way "unreachable" (didn't answer) does. This
//     package derives Complete automatically from Sources — a caller
//     cannot get it wrong because it never supplies it — and treats
//     SourceDegraded as also flipping Complete false. See
//     session-abstraction.md §9's amendment.
//   - SessionState.Since's coupling to Confidence for the unknown status
//     (state.go, UnknownState): the spec doesn't say whether a StatusUnknown
//     reading has a fixed Confidence. It doesn't — a driver can be certain
//     an API told it "I don't know" (observed) or merely guessing that it
//     doesn't know (inferred) — so UnknownState takes Confidence as a
//     parameter rather than fixing one.
//   - list()'s return shape (session-abstraction.md §3's operations table
//     says `list(filter?) -> SessionRef[]`; internal/driver.Driver.List
//     returns Collection[Session] instead. See that method's doc comment
//     and the session-abstraction.md §3 amendment for why a bare slice of
//     refs cannot satisfy §9's envelope rule or §13.2's "adopt, don't
//     resynthesize a peer's SourceStatus" rule.
//   - SourceState's closed set (ok/unreachable/unauthorized/degraded) has no
//     member for "this source is reachable but the operation it was asked
//     to perform isn't supported" — a real outcome (see
//     internal/drivers/stub) that doesn't fit any of the four cleanly.
//     internal/service maps it to SourceDegraded as the nearest honest fit;
//     this is a judgement call this package did not have spec cover for.
//   - The single-session URL shape (api-http.md §3.3,
//     /v1/machines/{machine}/sessions/{id}) carries no runtime segment, yet
//     SessionRef.ID is documented as scoped to (machine, runtime) — not
//     (machine) alone (session-abstraction.md §2.2). Two different runtimes
//     on one machine can legally reuse the same id, and the URL alone
//     cannot disambiguate. api-http.md's own header says the abstraction
//     wins when the two documents disagree and "this document is the bug";
//     fixed there by adding an optional `runtime` query parameter. See
//     internal/service's resolveSessionDriver and the api-http.md §3.3
//     amendment.
//   - GET /v1/machines' per-machine reachability doesn't map onto any of
//     the seven Driver operations (§3) — List is scoped to one runtime's
//     sessions, not machine-level liveness. internal/service.ListMachines
//     reuses List() as a liveness probe (a real, working mechanism, not a
//     fabricated one) rather than inventing an eighth Driver operation;
//     flagged rather than quietly assumed correct, since it means a machine
//     is "reachable" here iff at least one of its drivers answers promptly,
//     which is a proxy for liveness, not liveness itself.
//   - Event's SSE line framing (events.go): the spec never states whether
//     Kind travels as the SSE `event:` field, a JSON `kind` property in
//     `data:`, or both. Left unresolved — no SSE encoder/decoder exists in
//     this skeleton (see internal/service's /v1/events handler).
package fleet
