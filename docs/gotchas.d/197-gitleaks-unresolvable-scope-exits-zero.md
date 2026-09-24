# A secret scan given a scope git cannot resolve scans nothing and exits 0

**Issue:** #197 (scoping the CI secret scan to the ref that triggered it)

## What happened

The CI secret scan used to be a bare `gitleaks detect`, which walks
`git log --all` — every branch on the remote, because the checkout fetches
them all. Trunk's verdict therefore depended on whatever unmerged branches
were pushed at that moment. Scoping it means passing `--log-opts` (trunk:
`HEAD`; any other ref: `origin/<trunk>..HEAD`).

Passing a scope the scanner cannot resolve does not fail. Measured on the
pinned 8.30.1, with a revision git rejects:

```
ERR error="stderr is not empty"
INF 0 commits scanned.
INF no leaks found
```

Exit status 0. The git error is logged and then swallowed, so a scope typo,
or a checkout that lacks the remote-tracking ref the range names, turns the
gate green over a tree nobody read.

## What to do

Resolve the scope before handing it over and fail there:

```sh
count="$(git rev-list --count "$scope")" || exit 1
```

`git rev-list` exits non-zero on a bad revision. The step also prints
`count`, so a run's log states how many commits it actually covered. Zero is
legitimate for a ref that adds nothing over trunk — do not treat "0 commits"
as the failure signal; treat an unresolvable scope as one.

Re-check this on a gitleaks upgrade: the fix lives in this repo's workflow
step, so a newer scanner that fails loudly makes the guard redundant, not
wrong.

## How to test a scoped scan

Use a synthetic leaky commit on a throwaway local branch as the positive
control, not a real in-flight branch. The branch that motivated this issue
already carried an ignore entry for its own findings at its tip, so scanning
it in its own scope correctly reported clean — a control that could not have
failed.
