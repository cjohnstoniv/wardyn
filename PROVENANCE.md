# Provenance

Whether this project can prove the code is its own to license. Written because the
answer has an honest caveat, and an evaluator will find that caveat whether or not
it is disclosed here.

## The squashed root commit

The public history begins at a single root commit, `031d1729` (2026-07-08,
"Wardyn v0.1.0 — initial public release"): 669 files, 140,472 insertions, **no
parent**. Development predating public release — including the project's passage
through the working names WRIT and Warden — was collapsed into it and is not
recoverable from this repository.

Consequence, stated plainly: **per-file blame for anything predating `031d1729`
cannot be reconstructed from git.** Everything after it has ordinary history.

## Attestation

The copyright holder attests that all code in the root commit is either original
work of the contributors listed in `AUTHORS`, or third-party work that is
permissively licensed and attributed in `THIRD-PARTY-NOTICES.md`; and that the
squash was a deliberate pre-launch history reset, not an attempt to obscure the
origin of any code.

Supporting evidence a reviewer can check independently:

- **No foreign copyright notice exists anywhere in the tracked tree, outside the
  places that exist to carry one.** Every `Copyright <year> <holder>` line reads
  `Copyright 2025 The Wardyn Authors`, once the attribution surfaces are excluded:
  vendored upstream licence texts (`licenses/`), the generated third-party notices
  (`NOTICE`, `THIRD-PARTY-NOTICES.md`, `ui/public/THIRD-PARTY-NOTICES.txt`) and the
  script that generates them (`scripts/third-party-notices.sh`), plus the untracked
  build outputs `ui/node_modules` and `ui/dist`. Stated as a command rather than a
  count, so it cannot rot — this returns nothing:

  ```
  git grep -hE 'Copyright [0-9(]' -- . \
    ':!ui/node_modules' ':!ui/dist' ':!licenses' ':!*NOTICE' \
    ':!THIRD-PARTY-NOTICES.md' ':!ui/public/THIRD-PARTY-NOTICES.txt' \
    ':!scripts/third-party-notices.sh' | grep -v 'The Wardyn Authors'
  ```

  A doc guard runs the same check on every `go test` of `cmd/wardynd`
  (`TestProvenanceDocCopyrightScopeIsTrue`), and fails if this document's stated
  exclusions drift from the ones the check actually applies.
- The two places third-party source *was* adapted into the tree are named in
  `THIRD-PARTY-NOTICES.md` under "In-tree components derived from third-party
  projects": the shadcn/ui console primitives and a vendored copy of
  asciinema-player used by the demo lane.
- `gitleaks` over the full history reports no committed secrets — it runs in CI on
  every push (`make gitleaks`), so this is a standing gate rather than a one-time
  sweep, and no commit count is quoted here because that number moves every day.

## Contributors

Every commit to date is from a single human contributor. `git shortlog -sne` lists
that person under more than one address (a GitHub `noreply` address plus personal
ones, from different machines); `AUTHORS` lists the canonical identity, and it is
`AUTHORS` — not the shortlog — that defines "The Wardyn Authors" in the copyright
notice. Run both: the addresses differ, the human does not. There are no external
human contributors yet, and `GOVERNANCE.md` is candid that the bus factor is one.

## Developer Certificate of Origin

Contributions are made under DCO 1.1 (`CONTRIBUTING.md`); there is no CLA and no
copyright assignment.

**Known gap, disclosed:** 167 non-merge commits on the released line carry no
`Signed-off-by` trailer. That number is historical and does not grow — every one
of them predates the CI fix described next. Count them yourself:

```
git log --no-merges --format='%H %(trailers:key=Signed-off-by,valueonly)' \
  | awk 'NF==1' | wc -l
```

(No denominator is quoted: the commit total moves every release, and a frozen
ratio is a claim that fails the moment someone runs it.)

**How the gate reads today.** `.github/workflows/ci.yml`'s `dco` job checks every
commit a change introduces, on both event shapes: on a pull request,
`make dco DCO_RANGE="$BASE..HEAD"`; on a push, `DCO_RANGE="$BEFORE..HEAD"` derived
from `github.event.before`. It narrows to `-1 HEAD` only when that previous tip is
unusable — a newly created ref (all-zeros) or a force-push whose old tip this
clone no longer has. The earlier behaviour, the one those 167 commits arrived
under, was `-1 HEAD` unconditionally on a push, so a multi-commit direct push
could land unsigned commits behind one signed HEAD.

Since every commit is from the sole copyright holder, the licensing position is
unaffected; the written claim that *every* commit is signed off was nonetheless
inaccurate, which is why this paragraph exists. Rewriting a released history to
backfill sign-offs would break every existing clone and published tag for no legal
gain, so it has not been done.

## AI assistance

Parts of this codebase were written with AI assistance. The project's position:

- AI assistance is permitted.
- The **human** who signs off under the DCO takes full responsibility for the
  contribution — that it is theirs to submit, that it is licence-clean, and that
  it does not reproduce third-party code without attribution.
- All such output is human-reviewed before it is committed.
- Commits authored by a tooling identity acting on a contributor's behalf are that
  contributor's responsibility. No such commit exists on the released line.

This is stated because it is an increasingly common intake question, and an
undisclosed non-human `Author:` line is a worse discovery than a disclosed policy.
