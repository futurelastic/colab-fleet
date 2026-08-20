# Mechanical checks against these specs

This file exists because colab-fleet issue #57 found that a document
declaring itself normative was trusted over the code, and was wrong: §2.3's
`SessionState` block named four fields; the Go type carried ten. The
specific fields are fixed (see the type blocks themselves and their inline
notes), but the failure mode — a spec section nobody re-diffs against the
type it describes — is not, and is worth defending against mechanically
rather than by re-reading on a schedule.

## `TestSpecTypeBlocksMatchGoFields` (root package, `spec_fields_test.go`)

Parses every ` ``` `-fenced `Identifier { ... }` pseudocode block in
[`session-abstraction.md`](session-abstraction.md) and
[`api-http.md`](api-http.md), and compares the field names it finds against
the JSON-tagged fields of the corresponding Go struct via reflection. Runs
as part of `go test ./...`, so it is in CI on every push and PR — a type
that grows a field with no matching doc update fails the build, not a review
that happened to notice.

Two directions, both real bugs:

- **Go has a field the doc doesn't name** — #57's failure, and the common
  one: a struct grows a field and the type block is never revisited.
- **The doc names a field Go doesn't have** — rarer, but the same class:
  the spec describing a shape the code no longer has, which api-http.md's
  own §0 already calls "the document's bug, not the abstraction's."

### What it checks, and what it deliberately doesn't

Registered types live in `specFieldTypes` (`spec_fields_test.go`):
`SessionSpec`, `SessionRef`, `SessionState`, `DeliveryReceipt`, `Ack`,
`Request`, `Caller`, `Expectation`, `SessionPrompt`, `Response`,
`AttachHint`, `ConversationRef`, `DriverCapabilities`, `SourceStatus`.

**Field names only — never types, tags-minus-name, or ordering.** A field
that changes shape (say, a field renamed to a different key with the same
apparent meaning, or a `string` silently becoming a closed enum) without
being added or removed will not fire this check. That is a real gap; closing
it would mean parsing the pseudocode's type annotations too
(`MachineId` vs `string`, `?` optional-ness, union members) and comparing
those against Go's actual type and `omitempty`-ness — meaningfully more
parser surface, and more ways for the check itself to be wrong about a
line of pseudocode it misread. Left as a known limitation rather than
built, on the theory that a check silently wrong about types is worse than
no check: it would report false confidence exactly where #57's incident
happened (a doc that LOOKS complete).

**No recursion into inline nested objects.** A field whose value is written
as `{ model: boolean, effort: boolean, agent: boolean }` on one line (like
`DriverCapabilities.supportsPin`) is treated as one field — `supportsPin` —
and its own three sub-fields (`PinSupport`'s Go fields) are not separately
checked. `QuotaBlock` and `TurnEnd` avoid this by being written as their own
named blocks in the fence (following the `Request`/`Caller`/`Expectation`
pattern already established in §2.6), so they ARE checked as first-class
entries once added to `specFieldTypes` — but nothing enforces that a newly
introduced nested type gets the same treatment rather than the inline-object
shorthand. Reviewer judgement, not the check, catches that.

**Types with no formal block are not checked.** `Session` — the return type
of `state()` and the item type of `list()`, arguably the single most
important type in the model — has no `Session { ... }` block anywhere in
either document; api-http.md documents its shape only as an inline `GET`
response example, and session-abstraction.md only references it in prose
("Optional on `Session`", §2.8/§2.9). There is nothing named for this test
to parse a field list out of, so `Session` is not registered. Giving it a
proper block is a documentation improvement worth doing on its own; this
test cannot demand it, only benefit from it once it exists.

**`Collection<T>` is parsed (it has a named block, §9) but not registered.**
Its wire shape is `collectionWire[T]` (`collection.go`), a generic type
reflection cannot instantiate meaningfully without a concrete `T`; and its
`feed` field is documented correctly, just outside the formal block, in the
prose immediately following it (§3.2 of api-http.md). Not registering it is
a scoping choice, not a gap this test found and ignored.

### The escape hatch, and its rule

`specFieldExceptions` lets a Go field be genuinely absent from every
normative block without failing the build — for a field that is
deliberately, not accidentally, left out of the runtime-neutral model. As of
this writing it holds no entries: its one occupant,
`SessionState.screenDigest`, was removed when colab-fleet issue #59 ruled on
the open question it cited — whether `screenDigest` (and the `keys`
operation it corroborates) belongs in the model at all. #59 chose
capability-gated promotion over permanent wire-only status, so the field now
has a normative block of its own (§2.3, gated by `DriverCapabilities`'s new
`deliversRawKeys`, §4.3) rather than an exception explaining its absence.

An exception entry is a citation, not a excuse: the rule this file's own
test comment states is that a field simply forgotten must not be silenced by
adding it here — it must be documented, or genuinely justified with
something a reviewer can check. The map is left in place, empty, rather than
deleted, for the next field that earns one on the same terms.

## Why field-name-only is the check, not a rewrite of these docs into a schema

The tempting stronger version of this check is a real schema — JSON Schema
or a `.proto`, generated from the Go types, that both docs are checked
against byte-for-byte. That would catch everything this test doesn't. It
would also stop these documents from being prose a person can read: the
whole point of the current shape (readable pseudocode with a paragraph of
argument next to every field) is that §2.3's three rules, or the reasoning
for `waitingOn` existing at all, live in the same place as the field list,
which a generated schema cannot carry. This check is scoped to catch the
specific failure #57 found — a field silently missing — without asking the
specification to stop being written for a reader.
