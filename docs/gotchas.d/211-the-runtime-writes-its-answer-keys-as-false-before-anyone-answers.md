# The runtime writes a boot question's answer keys as `false` before anyone has answered

A project entry in the runtime's state file that carries
`hasClaudeMdExternalIncludesApproved: false` does **not** mean somebody was
asked and said no. Measured on a real state file: 15 of 833 project entries
carried both imports keys as `false`, none carried the "warning shown" key as
`true`, and a directory launched once and killed on the question got the same
pair — the runtime creates them at first launch, unanswered.

Two rules follow, and `internal/trustseed` keeps both:

- **Judge each key on its own.** The seeder used to skip a project entry the
  moment its trust key was already `true`. That shortcut is wrong the day a
  second key exists: a project the runtime already trusts still has an
  unanswered imports question, and skipping the whole entry would have left it
  asking forever. Set every key that is not already `true`; leave the ones that
  are.
- **`false` is "not yet answered", never a decision to preserve.** Add-only means
  never rewriting a key already `true` and never removing one. It does not mean
  keeping a `false`. The one thing this cannot tell apart is the runtime's
  default from a person's explicit "No" — the runtime writes the same value for
  both — so a seeded root overrides a past refusal, which is what the operator
  stated by naming the root.

Pinned by `TestImportsKeysAreSeededOnAProjectTheRuntimeAlreadyTrusts` and
`TestAKeyAlreadyTrueIsNeverRewrittenAndAFullySeededFileIsUntouched`.
