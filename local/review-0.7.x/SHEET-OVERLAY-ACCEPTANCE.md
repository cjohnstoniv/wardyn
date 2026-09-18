# R076-024 — Sheet backdrop lifetime

Baseline: dfa89f608fa223b469b0a2435226538f0035d533.
Branch: review/0.7x-sheet-overlay-ref.
Signed original: `f6e787a5d37a2efac00881923865df06ed5eba53`.
Worktree: `/tmp/wardyn-review-0.7x-sheet-overlay-ref`.

This ledger copy preserves the lane's acceptance record. Its named local mock
and screenshot paths refer to that worktree. Raw baseline/focused/browser logs
are also preserved here as evidence/sheet-overlay-{baseline,focused,ui-e2e}.log.
Browser log SHA-256:
`5283c04f24c2deea11c1cff84b3daa142a623986b1ae6ee72c833a9d1f27e8d2`.
The right-side closing-state screenshots are preserved under
evidence/sheet-overlay/: baseline-right-0 is the original wrapper,
baseline-right-1 the approved ref-only mock, and patched-right-0 the real fix.
All show the same 75ms point; these are a focused state fixture, not a claim of
full production-screen screenshot coverage.

## Pre-implementation state/mock plan

1. Retain the existing backdrop for its existing exit animation, rather than
   removing it immediately when the Sheet closes.
2. Preserve every class, duration, side, DOM slot, focus and dismissal behavior.
3. Honor the existing global reduced-motion CSS; add no motion or design token.

The state mock uses the actual SheetContent and its private overlay with a
local-only forwarded DOM ref. Production source is unchanged for this round.
Compare baseline versus mock in Chromium at 75ms into closing for all four
sides. Check unmount and trigger-focus restoration after animation completion.
Open-state appearance must be identical. This mirrors the reviewed Dialog and
AlertDialog experiment but does not import or change that patch.

The overlay retains its own existing animation duration; it is not extended to
the Sheet panel's longer slide-out duration. No canonical product copy changes.

## Isolation and overlap

feature/0.7.6 at e918bfe3 has no sheet.tsx changes from baseline. Read-only status
checks of its UI worktrees found no dirty UI files. All-ref Sheet history has no
forward-ref correction. The wrapper originated in 031d1729; local Markdown notes
do not reference SheetOverlay or sheet-overlay. No other campaign file is edited.

Mock/browser artifacts stay local and are not included in the product commit.
Full browser acceptance will use review-owned ports18894/18895 and database
wardyn_review_sheet after this mock has been approved and the fix implemented.

## Baseline evidence (before product changes)

- `pnpm exec vitest run src/app/components/ui/sheet-overlay.test.tsx`: exit1,
  4 lifetime failures (top/right/bottom/left) and 2 focus passes. The same node
  is already absent immediately after Close; React also reports the swallowed ref.
- `node sheet-review.mjs`: exit0 after Chromium permission escalation. All four
  sides show baseline overlay absent while panel is still closed-state animating.
  Ref-only mock retains overlay with animation `exit`, opacity0.197597 at75ms.
- Open states agree exactly: identical classes/background, opacity1, enter
  duration0.15s, focusRuns. Both versions fully unmount and restore trigger focus.
- Screenshots: `baseline-{right,left,top,bottom}-{0,1}-{open,closing}.png`.
  Variant0 is untouched production; variant1 is the local-only ref mock.

## Approval and focused verification

Root approved the state mock after reading this plan and the complete console
rules and viewing the right-side before/after closing screenshots. Only then was
SheetOverlay changed to forwardRef; its classes and all other production lines
remain unchanged.

- Fixed Vitest: 50 tests in 4 files pass (Sheet overlay, AppShell, Policies,
  ProfileReview), exit0. See `fixed-vitest.log`.
- `pnpm typecheck`: exit0. The default/editor tsconfig's unrelated missing
  afterEach import remains outside this independent patch.
- `SHEET_REVIEW_PHASE=patched node sheet-review.mjs`: exit0. All four sides keep
  their exit backdrop at75ms, fully remove it after completion, and restore
  trigger focus. Normal duration remains0.15s; reduced-motion duration is
  0.00001s and Escape still removes the sheet and restores focus.
- The first patched Chromium attempt was interrupted during a permission wait;
  it created no patched artifacts. One resumed run completed. The Vite session
  was then stopped before starting the full browser gate; no duplicate servers.
- Full browser gate passed against isolated `wardyn_review_sheet`, ports
  18894/18895, `DOCKER_HOST=unix:///var/run/docker.sock`, shared review Go caches,
  `GOFLAGS=-buildvcs=false`, `GOMAXPROCS=4`. Log: `full-ui-e2e.log`.
- The original unified execution session 61215 returned exit 0: 347 tests in
  30 spec files passed, with 0 failed, 0 skipped and 0 flaky tests.
- `scripts/check-file-size.sh` and `git diff --check`: exit 0.
- Root independently reviewed the production diff and all six regression tests
  and approved them pending completion of the full browser gate.
- After successful browser completion, the optional listener-cleanup check
  (`ss -ltnp '( sport = :18894 or sport = :18895 )'`) lacked netlink permission.
  Its escalated retry was interrupted while awaiting approval; no duplicate
  browser gate was started. Do not treat that uncompleted diagnostic as a
  browser-test failure or proof of listener cleanup.

## Release handoff

Severity: low, visual UX bug. Suggested release note: preserve Sheet backdrop
exit animations by forwarding its DOM ref. The patch changes no styles, motion
durations, copy, defaults, API, schema or focus/dismissal behavior; it restores
the existing 150ms backdrop exit animation while retaining the 300ms panel exit.

Only `ui/src/app/components/ui/sheet.tsx` and
`ui/src/app/components/ui/sheet-overlay.test.tsx` belong in the product commit.
The source wrapper originated in `031d1729`; the independent patch remains
based on `dfa89f608fa223b469b0a2435226538f0035d533`. Mock assets remain local.
The other 0.7.6 release owner plans to fold this Sheet hunk together with the
Dialog/AlertDialog fix; do not apply this independent patch twice if already
incorporated there. Root recorded the signed commit SHA above and in the ledger.

After the lane's optional listener check was interrupted, root separately ran
the read-only listener check and confirmed ports 18894/18895 were released.

## Full browser reproduction

Run from this worktree, with the review-owned PostgreSQL container available:

```sh
set -o pipefail
GOFLAGS=-buildvcs=false \
GOCACHE=/tmp/wardyn-review-cache/go-build \
GOTMPDIR=/tmp/wardyn-review-cache/go-tmp \
GOPATH=/tmp/wardyn-review-cache/go-path \
GOMODCACHE=/tmp/wardyn-review-cache/go-mod \
GOMAXPROCS=4 \
DOCKER_HOST=unix:///var/run/docker.sock \
WARDYN_E2E_PG_CONTAINER=wardyn-review-07x-pg \
WARDYN_E2E_PG_HOSTPORT=localhost:58432 \
WARDYN_E2E_PG_DBNAME=wardyn_review_sheet \
WARDYN_E2E_ADDR=:18894 \
WARDYN_E2E_UI_ADDR=127.0.0.1:18895 \
scripts/run-ui-e2e.sh 2>&1 | tee local/sheet-overlay-ref/full-ui-e2e.log
```

The canonical browser runner reseeds only this dedicated database between spec
files. This is standalone browser acceptance, not a claim that the independent
branch ran the broader `make ci` gate; the integration lane owns that gate.
