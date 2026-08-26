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

- **No foreign copyright notice exists anywhere in the tracked tree.** Every
  `Copyright <year> <holder>` line in the repository, excluding `ui/node_modules`
  and `ui/dist`, reads `Copyright 2025 The Wardyn Authors` — 990 occurrences, zero
  exceptions.
- The two places third-party source *was* adapted into the tree are named in
  `THIRD-PARTY-NOTICES.md` under "In-tree components derived from third-party
  projects": the shadcn/ui console primitives and a vendored copy of
  asciinema-player used by the demo lane.
- `gitleaks` over the full history (1,839 commits, ~27 MB) reports no committed
  secrets.

## Contributors

Every commit to date is from a single contributor across two email addresses; see
`AUTHORS` and `git shortlog -sne`. There are no external human contributors yet.
`GOVERNANCE.md` is candid that the bus factor is one.

## Developer Certificate of Origin

Contributions are made under DCO 1.1 (`CONTRIBUTING.md`); there is no CLA and no
copyright assignment.

**Known gap, disclosed:** 167 of the 1,294 non-merge commits on the released line
carry no `Signed-off-by` trailer. The CI gate range-checks every commit in a pull
request, but a direct push to `main` checks only the tip commit, so commits
arriving that way were never examined. Since every commit is from the sole
copyright holder, the licensing position is unaffected; the written claim that
*every* commit is signed off was nonetheless inaccurate, and `CONTRIBUTING.md` now
states the enforcement scope accurately rather than overclaiming. Rewriting a
released history to backfill sign-offs would break every existing clone and
published tag for no legal gain, so it has not been done.

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
