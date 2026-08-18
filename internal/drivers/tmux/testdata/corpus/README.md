# Classifier corpus

This directory is why #8 exists: every heuristic in `classify.go` so far was
scored against the one screen that motivated it, and F49–F57 (see
`docs/spec/session-abstraction.md`, Appendix A) are nine incidents that shape
followed from that gap. A corpus makes every incident a permanent regression
case, replayed against the whole set on every change — not verified once,
by eye, on the machine that happened to be blocked.

## What is here, and why it is a directory of directories

Each case is `<name>/screen.txt` (the redacted pane) plus `<name>/case.json`
(what it is FOR, where it came from, and the state — or sequence of states —
it must classify to). `corpus_test.go`'s `TestCorpusReplaysToItsStatedState`
reads every case directory automatically; a new case needs no test-list
update, the same reasoning this repo already applies to hooks over habits
(`private-terms-guard`) rather than trust.

A case with no `for` fails to load. That is deliberate: a fixture whose
expected classification nobody can explain is a fixture nobody can safely
change later — it either gets deleted by someone who cannot tell if it still
matters, or kept forever as superstition.

## The redaction rule — what is kept, what is discarded

**Kept:** the structural chrome the classifier actually reads — rule lines,
the composer's marker and its SGR-dim intensity (the one rendering attribute
that is itself structural: it is how `composerText` tells a placeholder hint
from real typed input), the spinner/status line, known runtime footers, a
numbered prompt option's index and — only when the option text matches the
same closed, binary-verified vocabulary `classifyPromptKind` already trusts
— the option text itself, and the fixed marker phrases inside a usage-limit
or turn-failure notice.

**Discarded:** everything else, by default. Transcript prose, an agent's own
words, a human's typed composer text, tool-call arguments, file paths, a
menu option that does not match the known runtime vocabulary. Discarding is
the default rather than an enumerated exception list, on purpose: a redactor
that keeps unless told otherwise has to be told about every way content can
appear on a pane, and missing one is exactly how this directory would become
the transcript store this service was explicitly ruled to keep content out
of everywhere except one audited, revocable API route (#14) — a route this
directory is not, since it is committed to a PUBLIC repository's git
history, permanently, unauthenticated, the moment it merges.

The rule is implemented once, in `internal/drivers/tmux/redact.go`
(`RedactCapture`), and reused by both the curation tool and the CI check
below — never re-derived by a human reading a screen.

## How discarding is verified, not trusted

`TestCorpusIsFullyRedacted` re-runs `RedactCapture` over every committed
`screen.txt` and requires the output to be byte-identical to what is on
disk. `RedactCapture` is built to be idempotent — every kept line is either
unchanged fixed vocabulary or one of a small set of fixed placeholder tokens
that do not themselves match any recognised shape — so a screen that is NOT
a fixed point of redaction is a screen someone (or some past version of the
tool) let content past. That test is part of `go test ./...` and therefore
part of this repository's ordinary gate; it is not a courtesy check run only
at curation time.

## How a capture gets in

1. Save the raw capture to a scratch file **outside this repository** (or
   under a `*.rawcapture` name — excluded by `.gitignore` as a second line
   of defence, the same belt-and-suspenders pattern `*.local.md` already
   uses here). It is never copied into the working tree and never
   committed, redacted or not.
2. Run
   `go run ./internal/drivers/tmux/testdata/cmd/corpus-redact <scratch-file> <case-name>`.
   This calls the exact `RedactCapture` function CI re-checks, writes
   `testdata/corpus/<case-name>/screen.txt`, and prints the redacted screen
   plus a `case.json` stub for review.
3. A human fills in the stub: `for` (why this case exists), `source` (the
   issue or finding it came from), and `want` for each observation. This is
   the one part that is inherently judgement — what SHOULD this screen
   classify to, and why does that matter — and it is exactly the part the
   tool does not attempt.
4. Review `git diff` before committing, same as any other change. The tool
   having already redacted mechanically means the review is "is this
   manifest right", not "did I remember to scrub everything".

A capture never enters the corpus by a driver or a live poll writing to this
directory automatically. Every case here was chosen by a human deciding an
incident was worth keeping.

## Historical cases: reconstructed, not captured

`command-output-above-empty-composer` and `fleet-recovery-simultaneous-ambiguity`
predate this directory. This driver has never persisted a raw screen to go
back and redact — `resolveAmbiguity` compares a `screenDigest`, deliberately,
because "keeping the text would be the obvious implementation and is the
wrong one" (its own comment). So these two are reconstructed from what was
written down about the incidents at the time (#13, and commit `5bee844`'s own
measurement), marked as such in `redactedFrom`, and scoped to the SHAPE that
mattered rather than claimed as byte-for-byte captures. Every case added from
here forward should be real, via the tool above.

## What this corpus cannot express

`classify()`, `classifyAged()` and `classifyPaneRemembering()` all take
**one pane**. `fleet-recovery-simultaneous-ambiguity` documents a property of
a **set** of panes — every session in a fleet entering the same ambiguity
branch in the same read cycle, because a restart clears `paneMemory` for all
of them at once (#7) — and a corpus of single-screen cases can pin down the
per-pane MECHANISM that produces that (one case, asserting `unknown` on a
first sighting with no prior memory) but cannot assert the fleet-wide
SYMPTOM itself: that N such panes read together is the moment a supervisor
is least able to tell "recovering" from "still broken".

That would need a test at a layer this package does not have — something
that reads a cohort of panes in one cycle and asserts a property of the
cohort, not of any one member. Recorded here as a stated limitation, not
solved by pretending a single-pane corpus covers a multi-pane claim.

## Provenance and privacy

This repository is PUBLIC. Every case here is additionally checked by this
repo's ordinary privacy grep for internal machine hostnames and local
filesystem paths (the exact pattern lives only in the machine-local notes
this repo's own conventions keep out of git — never spelled out in a
committed file, including this one) before it is committed — redaction
removes prose, the grep catches a name that slipped through some other way
(e.g. inside a "kept" footer line this package had not anticipated). Both
checks are required; neither substitutes
for the other.
