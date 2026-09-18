# Wardyn 0.7.x readiness review — release handoff

Baseline: `v0.7.5` / `dfa89f608fa223b469b0a2435226538f0035d533`.
Ledger branch: `review/0.7x-ledger`. This branch intentionally tracks this local
directory; product branches do not include it. Nothing is pushed or merged.

## Agreement and isolation

Include compatible fixes to longstanding bugs, security, documentation, UX and
existing workflows. Enterprise Kubernetes is the primary acceptance target;
Docker/Compose and desktop retain regression coverage. No 0.8+ work, new public
interface, migration, enforcement default or deployment requirement is authorized
by inference. Release selection belongs to the release owner.

The shared checkout is `/home/cjohn/containerized-agent-envs`; implementation is
only in `/tmp/wardyn-review-0.7x-*` worktrees. Before each patch, compare current
branches and local handoffs for overlap. Do not change another agent's work.
Test resources must be independently named, including image tags and databases.
Existing clusters (`dazz-next`, `dazz-prod`, `wardyn-entra`) and containers are
outside scope. Review resource names use `wardyn-review-07x`.

Recover this ledger with `git show review/0.7x-ledger:local/review-0.7.x/PATCH-LEDGER.md`.
The commits, rather than the temporary worktree directories, are the durable handoff.

## 2026-09-18 plan-review checkpoint

Product work paused for the owner's requested independent review of the other
0.7.6 campaign. Three parallel reviewers checked security, lifecycle/integration,
and UI/docs. `OTHER-076-PLAN-REVIEW.md` is the complete feedback; the exact reviewed
plan is retained in `OTHER-076-PLAN-SNAPSHOT.md` (SHA-256
`4fd2933a21c9c8870e62123f060a7d15e1830625a54176c71d0a8d088d6b5428`).
Feedback was copied to the owner's Windows clipboard and verified against the
file's SHA-256. Main remained clean at the baseline; no product changes were made
for the plan review. The feedback records integration overlaps and superseded commits.

The owner subsequently resumed the original campaign. Support-bundle redaction,
dialog-overlay refs, two additional operational docs fixes, and a test-import
cleanup are now independently committed below. The frozen combined batch has
passed make ci, the separate PostgreSQL report/race gate and all 347 browser
tests. A sixteenth independent patch (R023) subsequently passed its full default
Go tree and recording-package race tests; it is not in that frozen batch.
The historical plan-review snapshot is not a current patch-status list.

## Round 2 — resumed after release-owner handoff

The owner sent the first ledger to the separate 0.7.6 agent and requested continued
parallel 0.7.x work. The original frozen integration and its evidence are retained.
A new integration at `cd179c617acee6dd14ba6bc273db44569310355b` combines that batch
with R023 solely to finish its complete merge-gate verification; uninterrupted
`make ci` passed at that exact SHA, with a clean worktree. New independent fixes
below remain based on `dfa89f60`, not on the
other agent's changing feature branch. Current overlap checks found its SSO,
credential-hold, model-access and startup-detail work outside these new seams.
The four newer patches are now independently committed. Their combined selection
is frozen at `6a3dc8659ea7105ae81d127b5f3f1a198333f67d`; uninterrupted full CI,
same-tip browser acceptance, both typechecks and real PostgreSQL report/race gates
all passed in separate clean worktrees. See ROUND2-HANDOFF.md for the delta.

## Status definitions

`candidate` → `confirmed` → `implemented` → `checks-passed` → `release-ready`.
Other dispositions: `duplicate`, `rejected`, `blocked`, `out-of-scope`.
`checks-passed` names only the checks actually run. `release-ready` additionally
requires the repository's applicable gates on the recorded commit. A started
command, skipped suite, or passing mock is never a completed live verification.

## Findings and release selection

| ID | Finding / risk | Disposition | Branch / commit | Evidence and remaining work |
| --- | --- | --- | --- | --- |
| R076-001 | Prior ledger was untracked in /tmp; statuses overstated verification. | implemented | review/0.7x-ledger | Track handoff and distinguish focused checks from release readiness. |
| R076-002 | Concurrent login launches can leave two live sandboxes when timestamp and insert order disagree. Low, documented residual. | candidate | lifecycle lane | harnesscred_supersede.go, CHANGELOG 0.7.5. Reconcile ownership before fixing. |
| R076-003 | Autonomous Kubernetes Go/npm caches can escape the two metered scratch paths. | confirmed; design/live proof pending | lifecycle lane; no product commit | Image login profile overrides Go cache env defaults; leaf volumes can hide prewarmed contents. Other campaign's local selection recommends DEFER-0.7.7. Requires compatible cache layout and live toolchain/eviction proof, not an automatic env-only patch. See R003-K8S-CACHE-ASSESSMENT.md. |
| R076-004 | Successful AWS capture from Runs leaves the login sandbox alive to its idle cap. | candidate | lifecycle lane | handleUploadSSOToken. Stop must allow upload response to finish. |
| R076-005 | Recording-pane member hint names admin only although its gate admits security admins. Low. | checks-passed (focused + browser) | review/0.7x-record-tier-hint / 6ee6f585 | Red member assertion before fix; 51 component tests and typecheck pass. Independent mock review passed. Full browser suite passed: 30 spec files, no failures/skips/flakes; evidence/record-tier-ui-e2e.log. Supersedes unsigned a470804d; do not cherry-pick both. |
| R076-006 | Console rules say reduced motion is unimplemented although global CSS guard and tests ship. Low. | checks-passed (focused) | review/0.7x-doc-motion / 3f1f4c95 | theme.css global guard predates rubric claim; existing theme suite 31 passed. |
| R076-007 | Suspected personal-sign-in CTA for members with expiring shared Bedrock access. | rejected as current reachable bug | diagnostic worktree only; no product commit | Synthetic UI fixture reproduced it, but memberModelAccess maps shared expiring to live and strips action/deadline. Current normal server response cannot produce the alleged state. Existing CHANGELOG gap is stale; no historical rewrite. |
| R076-008 | Nightly fixtures and proxy logger race already have fixes on another branch. | duplicate | fix/nightly-harness / 6a18f2b1 | Six owner commits beyond baseline. Do not duplicate their implementation. |
| R076-009 | Brokered upload cap silently truncates oversized input; scan API can accept the truncated prefix as complete data. Medium integrity/correctness. | checks-passed (focused + race) | review/0.7x-sec-upload-cap / fe5c40a9 | Baseline oversize scan returns 204 and forwards; patched returns 413 with no upstream call and deny audit. Below/exact/unknown-length cases pass; independent lifecycle-lane review passed. |
| R076-010 | Terminal Kubernetes pod with absent/stale agent status can strand a run; failed main-container unknown exit becomes false success. Medium operational correctness. | checks-passed (package + race + scoped live) | review/0.7x-life-terminal-pod / 46dbcf6d | Six phase/status combinations and unknown-main-exit repro fail on baseline; full k8s package, race, vet, size guard pass. Independent security review passed. Dedicated live WaitExitCode/EphemeralDiskLimit passed (164.39 s); missing-status cases unit-only. Minimal-spec proxy lacked ControlPlaneURL, so this is NOT proxy/recording/full API-finalization proof. Dedicated cluster removed; existing clusters untouched. |
| R076-011 | Contributor UI recipe leaves shell in ui/, breaking subsequent root commands. Low. | checks-passed (focused) | review/0.7x-doc-ui-recipe / ed9a56d4 | Original make target fails from ui; corrected root dry-runs resolve; local Playwright executable used. |
| R076-012 | Contributor/release docs conflate CI conformance, manual Kubernetes walks and service-dependent local gates. Low. | checks-passed (focused) | review/0.7x-doc-live-gates / 58557df6 | Compared workflows, scripts and targets; existing release-job and PG-race guards pass. Makefile comments only. |
| R076-013 | Release checklist chooses a second patch number after version preparation and falsely claims concurrent tag safety. Medium operational. | checks-passed (focused) | review/0.7x-doc-release-order / 0a7e23ec | Choose before bumps, tag validated version; guards pass, provenance recorded in commit. No tags/releases created. |
| R076-014 | OIDC threat model, config comment and CLI help promise verified email when empty domain list does not check it. Low documentation; existing trust assumption remains. | checks-passed (focused) | review/0.7x-doc-oidc-email / 117c9e04 | Callback branch, OPERATIONS and legacy tests agree. Four callback and 17 doc/citation tests pass; one rootless guard skipped. No enforcement/default change. |
| R076-015 | SSH keys and existing connections outlive API-token/session revocation; incident-response docs blur the boundaries. | checks-passed (focused; combined citations repaired by R022) | review/0.7x-sec-ssh-revocation-docs / 8da1885b | SSH/OPERATIONS/threat-model wording checked against token/key/session lifecycles; lifecycle peer approved. See citation integration dependency below. |
| R076-016 | Recovery docs treat undrained audit spool as disposable derived data. | checks-passed (focused; combined citations repaired by R022) | review/0.7x-doc-audit-spool / b2042f52 | Preserve spool, consumed cursor and quarantine with matching database backup; focused operational guards and lifecycle peer review passed. See ops-evidence/ for scoped restore rehearsal, not a full production DR claim. |
| R076-017 | Support-bundle line redaction leaks multiline, aliased and embedded Compose credentials and can produce invalid YAML. | checks-passed (regression + package race) | review/0.7x-cli-bundle-redaction / c4c5fa16 | New cases fail against untouched baseline production and pass with parsed-YAML redaction; actual archive test covers multiline/embedded values. Complete CLI race suite passed (4.602 s); independent security reviewer found and verified alias-key fix. Comments/invalid or multi-document YAML omitted; opaque credential-shaped scalars fully redacted. No universal secret-detector guarantee. |
| R076-018 | Dialog and AlertDialog overlay wrappers omit the ref needed for Radix exit-animation lifetime. | checks-passed (focused + browser + typecheck) | review/0.7x-alert-overlay-ref / 9ca33584 | Both wrappers forward the underlying ref; 4 focused and 101 related assertions passed. Full browser run: 347 tests, 30 specs, no failures/skips/flakes. Official typecheck passed; approved mock, actual Chromium lifetime/focus/reduced-motion proof, and independent docs-lane code review. No style/timing changes. |
| R076-019 | Restore instructions say an incorrect age key fails only on first application-secret use, although persisted signing-key loading can refuse startup. | checks-passed (focused + citations) | review/0.7x-doc-age-key-boot / 7d155903 | 25 focused key/ops tests plus live audit-action citation guard passed; security peer approved. Keeps separate application-secret verification. |
| R076-020 | Desktop/SSH docs call all shell recordings unmasked and permanently retained. | checks-passed (focused + citations) | review/0.7x-doc-desktop-masking / baa4611e | 15 tests cover registered/mid-session masking, retention and doc/citation guards; lifecycle peer approved. Preserves unregistered-output and restart-loss residuals. |
| R076-021 | Run-detail test uses afterEach without an explicit import, breaking default/editor tsc configuration. | checks-passed (focused + both typechecks) | review/0.7x-ui-test-hook-import / 94c4fa84 | Baseline default tsc reproduced 3 TS2304 errors; one import fixes them; 36 run-detail tests and both typecheck configurations pass. Official repository typecheck already included Vitest globals, so this was NOT a failing merge/runtime gate. |
| R076-022 | Selected operational docs move guarded audit citations. Integration dependency, not an independent product feature. | checks-passed (focused + combined make ci) | review/0.7x-doc-integration-citations / 5adf090c | Five citation-only replacements; original evidence and guards preserved. Depends on R015/R016/R019/R020 plus the initial eight-patch integration. Must re-point again if a release selects a different documentation combination. |
| R076-023 | Filesystem recording reads follow shared-mount symlinks outside the recording root. Conditional security issue; default PostgreSQL unaffected. | checks-passed (full default Go tree + package race + peer + combined make ci) | review/0.7x-recording-root-read / 18fb91fd | Separate post-freeze patch, NOT in 0d63ab2e. Four synthetic escape cases fail on baseline; rooted reads pass them and compatibility checks. Full make ci passed in cd179c61; original package race/full default Go tree and independent peer review passed. See RECORDING-ROOT-READ.md for preconditions and residuals. |
| R076-024 | SheetOverlay drops the backdrop before its existing exit animation finishes. Low UX. | checks-passed (focused + browser + typecheck + peer) | review/0.7x-sheet-overlay-ref / f6e787a5 | Four lifetime regressions fail on baseline; two focus tests pass. Mock approved before edits; actual Chromium verifies all sides/reduced motion. 50 focused tests, typecheck and full browser (347 tests/30 specs, no failures/skips/flakes) pass. Root peer approved. Existing styles, timing and focus preserved. Other owner may fold this into R018; do not apply twice. |
| R076-025 | CLI recording/bundle exports reuse a predictable .part file, truncating unrelated files or colliding across simultaneous downloads. | checks-passed (regression + package race + peer + combined gates) | review/0.7x-cli-private-temp / 2307b07c | Six regular/symlink/rename-failure cases and synchronized concurrent download fail on baseline. Both exporters now use unique same-directory 0600 files; full CLI race suite passed, interruption cleanup checked. Final target replacement remains last successful rename wins. Combined gates passed at 6a3dc865. |
| R076-026 | CLI policy conversion silently ignores later YAML documents, including malformed ones and intended restrictions. | checks-passed (regression + package race + peer) | review/0.7x-cli-policy-document / f5fbb166 | Red-first converter and create/update/render/run policy-file regressions; shared converter rejects extra documents before API calls. Valid single JSON/YAML with markers/comments unchanged; full CLI race passed (4.844s). Independent docs peer approved. Ambiguous multi-document input intentionally becomes an error. |
| R076-027 | Recording-upload masking cleanup can block on an upstream body read after filesystem storage has already failed. | checks-passed (full default Go tree + package race + peer) | review/0.7x-recording-upload-cancel / 0bb4699b | Real FSStore early-failure regression failed on baseline and passes with demand-driven bounded masking. Root peer approved; complete API/secretmask race passed (92.161s/1.019s), full Go tree including citation guards passed. Existing masking, cap and error/tail ordering preserved. Handler/masking lifetime fix, not a new HTTP transport deadline or PG-outage claim. |
| R076-028 | FSStore SaveCast leaves its owned temporary file when the final rename fails. Low cleanup/disk accumulation risk; PostgreSQL unaffected. | candidate (code-reviewed; reproduction pending) | no product commit | Direct os.Rename return lacks the unlink used by copy/close failures; SaveCastNamed shares the seam. Optional age-based Sweep mitigates old orphans only when retention is enabled. Keep out of frozen round-two selection; next isolated patch needs deterministic bare/named rename-failure and replacement/cleanup tests. |

## Coverage matrix

| Area | Inspected evidence | Current acceptance gaps |
| --- | --- | --- |
| Identity / authority | Router split, CSRF host guard, recording-pane tier split; OIDC email claim and SSH revocation boundaries checked against code; recording owner/tier and run-token checks inspected. | Broad live identity/revocation/session scenarios; no new live cross-user certification. |
| Credentials / egress | Upload boundary/race tests; support-bundle baseline/fixed structured-redaction and archive tests; login launch/capture review. | Real SSO/concurrent-user walk and broader broker/redirect adversarial checks; other campaign owns SSO changes. |
| Lifecycle / storage | Six terminal-status combinations; scoped live WaitExitCode and actual emptyDir eviction; synthetic PG audit/secret/recording restore; filesystem recording read-confinement regression, fix and combined make ci. | Full API finalization, restart/reconcile, user-drive restore, healthy-proxy lifecycle proof. |
| Product / UX | Approved mocks; recording-pane browser sweep; overlay and SheetOverlay each passed full browser plus DOM lifetime/focus checks; original and round-two frozen combinations each passed all 347 browser tests. | Complete real-identity recovery journeys remain separate from hermetic acceptance. |
| Deployment / recovery | Isolated PG dump/restore with append-only guards; corrected age-key and audit-spool runbooks. | Fresh Helm/upgrade, outage/full application recovery, split runtime roles and pending-spool replay. |
| Engineering / docs | Independent DCO commits, nightly overlap check, guard-preserving citation repair, release/UI recipes; uninterrupted final combined make ci passed. | Live deployment acceptance remains outside make ci. |

## Verification record

- Earlier make ci attempts have no captured completion or exit status: no passing gate evidence.
- Earlier R076-005 component pass emitted React ref/act warnings; assertions passed.
- Initial read-only inventory: shared checkout clean at dfa89f60.
- Capture commands, commits, exit codes, executed/skipped counts and relevant artifacts.
- User explicitly authorized parallel agents: documentation, security, lifecycle/storage;
  root owns UX, isolation, broad checks, reconciliation and this ledger.
- Baseline make ci: build all tags, tidy, vet/lint, shell guards and default/Docker
  reports passed; k8s report stopped at TestStubMain_HonestlyRefusesWithoutDocker.
  Its nested go build failed VCS discovery at the environment's /tmp/.git boundary,
  not a test assertion. Log: evidence/baseline-ci.log. No overall gate pass.
- Temporary test binaries use GOFLAGS=-buildvcs=false to bypass that environment
  discovery issue. Actual source revisions remain recorded explicitly here.
- Initial browser attempt built successfully with that workaround but ran zero
  browser tests: common.sh picked another Docker daemon. Pinning
  DOCKER_HOST=unix:///var/run/docker.sock addresses test-infrastructure selection.
- review/0.7x-integration at a6c47ae5 combines the eight patches above from the
  baseline solely for checks; original patch branches remain independent. Full
  make ci stopped at notices because this new worktree lacked node_modules;
  prior build/report/race/staticcheck/vulnerability/license gates passed, but
  later gates did not run (evidence/integration-ci.log). The notices check modified
  generated files in that integration worktree; install frozen UI dependencies
  and regenerate before resuming. Do not cherry-pick integration on top of
  individual patches. That attempt did not establish a combined green make ci.
- Real PostgreSQL integration test-report-pg and test-race-pg both passed at
  a6c47ae5 (evidence/integration-pg.log): 69.5% PG report coverage; broker/store
  race tests passed. This does not certify later uncombined documentation patches.
- Frozen dependency installation and notice regeneration restored the integration
  worktree to HEAD-equivalent artifacts. A subsequent full gate at 43aa1dc1
  correctly failed seven audit-action citation checks after SSH/OPERATIONS doc
  line shifts (evidence/integration-ci-43aa1dc1-failed.log and failure-details).
  The separate integration-dependent citation repair R022 subsequently passed its
  focused guard; no guard was weakened or removed. The final uninterrupted gate
  includes all selected patches and that repair.
- Final selected batch frozen at `0d63ab2e3ade4302b9f51b08146965cf21655c46`
  on `review/0.7x-integration`: 15 independent patches plus R022 (integrated as
  `4640554c`). Uninterrupted make ci passed (exit 0) against this SHA; its worktree
  remained clean. Go report and race suites passed for default/Docker/Kubernetes;
  required probe parents passed and coverage met the gate. UI unit tests passed:
  155 files, 2,880 tests, 94.91% statements / 90.25% branches / 82.35% functions.
  Log: evidence/integration-ci-final.log; SHA-256
  `dbcbff0ff04d68e604f92a2cf3d875f40a8e29e48b229219e97fe3a6b6a0f080`.
  Combined browser acceptance uses a separate worktree at the SAME SHA, with its
  own database and ports so Vite builds cannot race the CI worktree. It passed
  (exit 0): 30 spec files / 347 tests, zero failures, skips or flakes; tracked
  worktree clean and isolated ports released. Same-tip PostgreSQL report/race
  checks passed (exit 0):
  69.5% report coverage, all nine required TestPG_ProbeF11 parents executed and
  passed; broker race 2.684 s / store race 22.737 s. Log:
  evidence/integration-pg-final.log. Both official and default UI typechecks also
  passed at this SHA (evidence/integration-ui-typecheck.log).
- The final govulncheck scans for all three shipped build-tag configurations
  reported zero affected symbols and zero imported-package findings. Verbose
  follow-up identifies the module-only advisory as GO-2026-5932 for the unused
  golang.org/x/crypto/openpgp package in required x/crypto v0.56.0, fixed version
  N/A (evidence/integration-govuln-verbose.log). This is not a blanket security
  certification or a reason to silently change dependencies in the frozen batch.
- BATCH-ACCEPTANCE.md, evidence/INTEGRATION-VERIFICATION.md and
  evidence/integration-ui-acceptance.md consolidate the final commands, source
  SHAs, counts, hashes and limits. The raw final CI/PG/browser/typecheck/scan logs
  are preserved with this ledger rather than relying only on temporary reports.
- Post-freeze R023 at 18fb91fd passed the complete default Go tree (including
  cmd/wardynd guards), exit 0, clean worktree before/after; log
  evidence/recording-root-full-tree.log. It also passed recording package race,
  conformance, vet and size checks, with independent peer review. This does not
  extend 0d63ab2e's combined make-ci/browser acceptance to R023.
- The subsequent integration `cd179c617acee6dd14ba6bc273db44569310355b` adds only
  R023 to the original frozen batch. Its uninterrupted `make ci` passed, exit 0,
  clean worktree: Go union coverage 78.4%, all three race configurations passed,
  UI 155 files / 2,880 tests passed. Evidence: evidence/round2-ci-recording-root.log,
  SHA-256 `94d1883e2140c2cb9dd1d2d9d09a67f1b98189d8baca67242ee94c24cebed948`.
  This is not yet acceptance for the four newer patches R024–R027.
- Final round-two selection `6a3dc8659ea7105ae81d127b5f3f1a198333f67d` contains
  all 20 independent product/docs patches plus the original R022 selection-specific
  citation repair. One uninterrupted `make ci` passed, exit 0, clean worktree:
  Go union coverage 78.5%, all three report/race configurations passed, and UI
  156 files / 2,886 tests passed. Log: evidence/round2-ci-final.log; SHA-256
  `ed717e2b34afcffaaa5e4b37b7688ea7c43a3959392aecf2c87ae7c75209db31`.
  Same-tip separate browser acceptance passed all 347 tests / 30 specs with no
  failures, skips or flakes; both TypeScript configurations passed and ports were
  released. Same-tip real PostgreSQL report/race passed: 69.6% coverage, all nine
  required probe parents executed, 5,525 passed / five skipped test events
  including subtests, no failures; broker/store race 2.764s/16.772s. The skipped
  cases are named in ROUND2-PG-ACCEPTANCE.md and are not fixture self-skips.
  Exact commands, source isolation, counts, hashes and limits are in
  ROUND2-HANDOFF.md, ROUND2-UI-ACCEPTANCE.md, ROUND2-PG-ACCEPTANCE.md and
  evidence/ROUND2-FINAL-VERIFICATION.md. Original frozen checkpoints are unchanged.
- Support-bundle red-first and fixed logs are evidence/redaction-baseline.log and
  evidence/redaction-fixed.log. The detached baseline reproduction worktree has
  only the new test file; its production source remains exactly dfa89f60.
- The actual support-bundle CLI also passed both Docker-resolved and raw-file
  fallback collection with synthetic multiline/embedded/aliased/commented
  credentials. Both archives omitted all six test fragments and both exported
  YAML documents passed Compose validation. See REDACTION-ACCEPTANCE.md for the
  exact scope, artifacts and hashes; no live backend or production secrets used.

## Patch compatibility and release notes

Every individual product/docs patch (excluding R022's explicit integration dependency)
is based independently on dfa89f60, signed off,
and can be reviewed/cherry-picked separately. No schema migration, new API surface,
configured cap, deployment requirement or enforcement default changes. R009
deliberately enforces the existing upload cap with HTTP 413 where oversized input
previously received success after truncation. Rollback is an ordinary
revert; no migration to undo. Docs patches touch distinct sections where files
overlap. All four initial docs patches have an independent security-lane factual
review. Selection still needs release-owner gates after any 0.7.6 integration.
Line-number citation repairs depend on the selected documentation combination:
re-point live references after combining docs, never remove the guarded citations.
Do not cherry-pick an integration umbrella on top of the independent patches.
For the original `0d63ab2e` selection, independent composition review verified
all 15 patch IDs match their integrated copies, each original has the baseline
as its sole parent and a DCO sign-off, and the 26-file combined diff contains no
local mocks/evidence or dependency changes. R022's patch ID also matches its
integrated copy. The round-two extension reviews the five additional originals
and the full 41-file product diff at `6a3dc865`; see
evidence/round2-composition-review.md for mappings and the R027 citation-context
qualification. This is provenance and scope verification, not a substitute for
runtime acceptance.

- R076-005: Correct the recording pane's permission hint to include security admins.
- R076-006: Document the console's existing reduced-motion support.
- R076-009: Reject oversized brokered uploads instead of forwarding truncated data.
- R076-010: Finalize terminated Kubernetes runs even when the agent exit status is
  missing; report unknown exits as failure and attempt normal credential cleanup.
- R076-011: Keep contributor UI commands at repository root.
- R076-012: Clarify automated conformance versus manual live acceptance gates.
- R076-013: Select a patch version before preparing and validating its release.
- R076-014: Correct when OIDC email-verification restrictions are enforced.
- R076-015: Document SSH key removal and run shutdown separately from token revocation.
- R076-016: Preserve undrained audit fallback state during backup and recovery.
- R076-017: Prevent multiline and aliased Compose credentials from leaking in
  support bundles; omit comments/unparseable config and require review before sharing.
- R076-018: Preserve dialog backdrop exit animations by forwarding their DOM refs.
- R076-019: Explain how a mismatched age key can prevent restored daemon startup.
- R076-020: Describe SSH shell masking and recording retention accurately.
- R076-021: Make the run-detail test's lifecycle-hook import explicit.
- R076-023 (post-freeze): Confine filesystem recording replay and metadata reads
  to the configured recording directory, including symlink resolution.
- R076-024: Preserve sheet backdrop exit animations by forwarding the DOM ref.
- R076-025: Write recording downloads and support bundles through unique private
  temporary files. Completed exports now have mode 0600; intentional replacement
  of the requested final path remains unchanged.
- R076-026: Reject ambiguous multi-document policy files before any API call;
  ordinary single-document YAML and JSON remain supported.
- R076-027: Stop reading recording bodies when storage does not request more
  data; preserve masking and existing upload limits without an eager copy goroutine.

## 0.7.6 release selection (2026-09-18, the release owner's ruling)

The 0.7.6 campaign ran four blind reviewers over rows R076-001..024 (A security/proxy/CLI, B console, C docs/release,
D open candidates; per-patch evidence at `local/v076/evidence/patch-review/{A,B,C,D,SELECTION}.md` in the campaign's
`~/wt-v076` worktree) answering, per row: real issue? right fix? include in 0.7.6? The owner accepted every INCLUDE
and deferred/rejected the rest. Rows R076-025..028 landed after those reviewers read the ledger; batch E (evidence at
`local/v076/evidence/patch-review/E.md`) reviewed them and the owner's ruling covers them too. Application: code/console picks are cherry-picked individually
(`-x`) onto `lane/v0.7.6-patches` from `feature/0.7.6`; docs picks go through the 0.7.6 docs lane with their own
same-commit window-0 re-cites; never the umbrella integration branches.

| Finding | 0.7.6 decision | Notes from the review |
|---|---|---|
| R076-001 | n/a | ledger bookkeeping |
| R076-002 | DEFER 0.7.7 | race REPRODUCED (two live sandboxes, neither KILLED); fix = per-actor advisory lock over quota → insert → supersede, ~1 session; 0.7.6 corrects the false "newest sandbox wins" sentence in its CHANGELOG Known gaps |
| R076-003 | DEFER 0.7.7 | real (GOCACHE/npm/pip/rust outside both metered emptyDirs); needs a third scratch emptyDir + live eviction proof |
| R076-004 | DEFER 0.7.7 | real but NARROWED by 0.7.6: the banner-mounted sign-in door survives navigation and kills the login run after server confirmation; only the three route-scoped pane mounts and a closed tab still orphan; 0.7.6 ships that narrower Known-gaps bullet |
| R076-005 | INCLUDE | `6ee6f585`; never `a470804d` |
| R076-006 | INCLUDE | `3f1f4c95` |
| R076-007 | REJECT | rejection confirmed at the 0.7.6 tip; the stale 0.7.5 Known gap is retired with its citation |
| R076-008 | REJECT (duplicate) | PR #71 merges independently; no overlap either order |
| R076-009 | INCLUDE | `fe5c40a9`; low severity (only the scan 8 MiB == 8 MiB window); auto-merges with 0.7.6's proxy hold |
| R076-010 | already in base | `46dbcf6d` == `ade005b2` (diff empty); the plan's eviction → `approval.cancelled` coverage was missing and is now a test at 79f206ab |
| R076-011 | INCLUDE | `ed9a56d4` |
| R076-012 | INCLUDE | `58557df6` |
| R076-013 | INCLUDE | `0a7e23ec`; the 0.7.6 runbook gained the fetch-tags/collision prerequisite |
| R076-014 | INCLUDE | `117c9e04` |
| R076-015 | INCLUDE (landed) | `8da1885b` on the 0.7.6 docs lane with the guard's own re-cites |
| R076-016 | INCLUDE (landed) | `b2042f52` on the 0.7.6 docs lane with the guard's own re-cites |
| R076-017 | INCLUDE + Known gap | `c4c5fa16`; marker-named structural keys are redacted whole (Compose-invalid output, valid YAML); comments dropped |
| R076-018 | INCLUDE with re-cites | `9ca33584`; CONSOLE-RULES L107 anchors dialog.tsx:66→68, alert-dialog.tsx:61→63 re-cited by hand |
| R076-019 | INCLUDE | `7d155903` |
| R076-020 | INCLUDE | `baa4611e` |
| R076-021 | INCLUDE | `94c4fa84`; the only such file at the 0.7.6 tip |
| R076-022 | REJECT | its OPERATIONS.md value is off by +2 on the 0.7.6 tree and the drift set is 7 not 5; the 0.7.6 docs lane re-derives every citation from the window-0 guard per commit |
| R076-023 | INCLUDE + Known gap | `18fb91fd`; absolute symlinks INSIDE the root now refuse (replay 500 / list degrades); hard links still read; NFS untested |
| R076-024 | INCLUDE | `f6e787a5` (the official patch; batch B had judged the same hunk before it was committed) with sheet.tsx:61→63 re-cited |
| R076-025 | INCLUDE + changelog note | `2307b07c`; low (shared `.part` inode / pre-existing-file follow); exports become owner-only 0600 on Linux (drvfs ignores it) |
| R076-026 | INCLUDE + changelog note | `f5fbb166`; low-medium; one compat change: a file ending in a bare `---` is now rejected |
| R076-027 | INCLUDE | `0bb4699b`; mechanism real, low in production (the proxy forwards a `bytes.Reader`); audit re-cite 125→119 verified |
| R076-028 | INCLUDE as a 3-line fix | reproduced at base and tip; unlink the owned `.tmp-cast-*` on the rename error path + a test, applied by the 0.7.6 patches lane (no ledger commit) |

## Open findings, not implemented

R076-002 needs launch serialization across quota checks, row insertion and
supersede, not just changing the timestamp comparison. R076-003 needs live
storage-enforcement and toolchain-compatibility proof before relocating caches.
R076-004 cannot tear down the login sandbox before the helper receives its upload
response and emits the PTY marker the console uses to confirm success. These
remain possible 0.7 follow-ups pending a safe design, not silently assigned to 0.8.
The other active 0.7.6 campaign owns SSO/capture/lifecycle changes, including its
capture-cleanup plan; reconcile that work before acting on R002/R004. No duplicate
implementation or modification of its worktrees is authorized by this ledger.

R076-028 was found during the frozen round-two acceptance window and independently
confirmed by code review, not a runtime reproduction. SaveCast's direct rename
return dates to `031d1729`; existing local security notes describe atomic
last-write-wins behavior, not intentional orphan preservation. A future minimal
fix should remove only its own temporary file after rename failure, preserve the
original error and destination, and test both bare and named saves. The retention
sweeper already removes aged `.tmp-cast-*` files, but retention defaults to off
and an enabled sweep is periodic. No separate security boundary bypass is claimed.

Security review also noted that supported API-token SSH registration outlives
token revocation: incident response must remove SSH keys and stop affected runs.
Email-based OIDC role mapping without a configured domain allowlist trusts the
provider's email claim without requiring email_verified; changing that would
require an explicit compatibility decision, especially for Entra. Split-horizon
OIDC HTTP/HTTPS scheme support remains an unresolved compatibility question.

The first frozen batch's unit run emitted a SheetOverlay ref warning in
MobileNav tests. R024 is now separately committed with baseline regression,
mock/actual-browser and full browser verification. Its exact hunk may already be
included by the other release owner's planned R018 combination; compare before
cherry-picking. That owner also plans to re-point the console rules' overlay
source citations against their final selection.

## Review-owned test resources

- PostgreSQL: wardyn-review-07x-pg, unix:///var/run/docker.sock, loopback port 58432.
- Browser database: wardyn_review_record; backend/gateway ports 18888/18889.
- Overlay browser database: wardyn_review_overlay; ports 18890/18891; completed.
- Combined browser acceptance: wardyn_review_combined; ports 18892/18893; isolated
  from both prior runs and the other 0.7.6 campaign.
- Sheet browser acceptance: wardyn_review_sheet; ports 18894/18895; isolated from
  the other browser runs and the other 0.7.6 campaign; complete, ports released.
- Round-two combined browser: wardyn_review_round2; ports 18896/18897; separate
  worktree at the frozen round-two tip; complete, ports released.
- Operations check databases: wardyn_review_ops_* (documentation lane).
- Dedicated Kubernetes acceptance cluster wardyn-review-07x-life was deleted
  after scoped live tests; existing clusters were untouched. Review images remain.
- No existing Wardyn databases, instances or clusters are used by these checks.

## Release owner

Review and cherry-pick product commits individually. Draft release notes,
compatibility, rollback and dependencies will accompany completed findings here.
Release numbers and historical changelog entries belong to integration.
Real Entra/AWS, private endpoints and owner hardware remain manual acceptance gaps
until exercised. This ledger does not declare Wardyn ready.
