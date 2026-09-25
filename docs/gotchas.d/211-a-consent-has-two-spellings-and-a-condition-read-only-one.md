# A consent has two spellings, and a condition that reads only one silently drops the other

`SessionSpec` carries the folder-trust consent twice: the older boolean
`trustCwd` and the general `consents` list. `SessionSpec.ConsentsTo` reconciles
them so no caller has to remember both exist. The condition that decides whether
a create starts the step that ANSWERS a boot question did not use it:

```go
if spec.Prompt != "" || spec.TrustCwd { go d.settleNewSession(...) }
```

So a create carrying only `consents` — no initial prompt, no `trustCwd` — got its
201 and left every question standing. It went unnoticed because every consent
until then arrived with a prompt beside it, and the only consent that arrived
alone was the boolean. The acceptance case of #211 (`consents:
["external-imports"]` and nothing else) was the first one that could not hide.

The rule: **a check on "did the caller consent to anything" reads the list, not the
legacy field** — `len(spec.Consents) > 0` here, `ConsentsTo(kind)` when the kind
matters. A new spelling of an old field is a place every condition written
against the old one is wrong until proven otherwise.

Pinned by `TestAConsentsOnlyCreateStillAnswersTheQuestion`, which drives `Create`
itself against a multiplexer that parks the new session on the question; it fails
if the condition is reverted.
