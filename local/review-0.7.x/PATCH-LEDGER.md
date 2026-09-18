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
| R076-003 | Autonomous Kubernetes Go/npm caches can escape the two metered scratch paths. | candidate | lifecycle lane | ephemeralScratchVolumes, runs_dispatch_mounts.go. Requires compatibility and live proof. |
| R076-004 | Successful AWS capture from Runs leaves the login sandbox alive to its idle cap. | candidate | lifecycle lane | handleUploadSSOToken. Stop must allow upload response to finish. |
| R076-005 | Recording-pane member hint names admin only although its gate admits security admins. Low. | checks-passed (focused) | review/0.7x-record-tier-hint / 6ee6f585 | Red member assertion before fix; 51 component tests and typecheck pass. Independent mock review passed. Full browser suite pending. Supersedes unsigned a470804d; do not cherry-pick both. |
| R076-006 | Console rules say reduced motion is unimplemented although global CSS guard and tests ship. Low. | checks-passed (focused) | review/0.7x-doc-motion / 3f1f4c95 | theme.css global guard predates rubric claim; existing theme suite 31 passed. |
| R076-007 | Suspected personal-sign-in CTA for members with expiring shared Bedrock access. | rejected as current reachable bug | diagnostic worktree only; no product commit | Synthetic UI fixture reproduced it, but memberModelAccess maps shared expiring to live and strips action/deadline. Current normal server response cannot produce the alleged state. Existing CHANGELOG gap is stale; no historical rewrite. |
| R076-008 | Nightly fixtures and proxy logger race already have fixes on another branch. | duplicate | fix/nightly-harness / 6a18f2b1 | Six owner commits beyond baseline. Do not duplicate their implementation. |
| R076-009 | Brokered upload cap silently truncates oversized input; scan API can accept the truncated prefix as complete data. Medium integrity/correctness. | checks-passed (focused + race) | review/0.7x-sec-upload-cap / fe5c40a9 | Baseline oversize scan returns 204 and forwards; patched returns 413 with no upstream call and deny audit. Below/exact/unknown-length cases pass; independent lifecycle-lane review passed. |
| R076-010 | Terminal Kubernetes pod with absent/stale agent status can strand a run; failed main-container unknown exit becomes false success. Medium operational correctness. | checks-passed (package + race) | review/0.7x-life-terminal-pod / 46dbcf6d | Six phase/status combinations and unknown-main-exit repro fail on baseline; full k8s package, race, vet, size guard pass. Independent security review passed. Dedicated live conformance pending. |
| R076-011 | Contributor UI recipe leaves shell in ui/, breaking subsequent root commands. Low. | checks-passed (focused) | review/0.7x-doc-ui-recipe / ed9a56d4 | Original make target fails from ui; corrected root dry-runs resolve; local Playwright executable used. |
| R076-012 | Contributor/release docs conflate CI conformance, manual Kubernetes walks and service-dependent local gates. Low. | checks-passed (focused) | review/0.7x-doc-live-gates / 58557df6 | Compared workflows, scripts and targets; existing release-job and PG-race guards pass. Makefile comments only. |
| R076-013 | Release checklist chooses a second patch number after version preparation and falsely claims concurrent tag safety. Medium operational. | checks-passed (focused) | review/0.7x-doc-release-order / 0a7e23ec | Choose before bumps, tag validated version; guards pass, provenance recorded in commit. No tags/releases created. |
| R076-014 | OIDC threat model, config comment and CLI help promise verified email when empty domain list does not check it. Low documentation; existing trust assumption remains. | checks-passed (focused) | review/0.7x-doc-oidc-email / 117c9e04 | Callback branch, OPERATIONS and legacy tests agree. Four callback and 17 doc/citation tests pass; one rootless guard skipped. No enforcement/default change. |

## Coverage matrix

| Area | Inspected evidence | Current acceptance gaps |
| --- | --- | --- |
| Identity / authority | Router split, CSRF host guard, recording-pane tier split. | Cross-user runtime checks, revocation and session scenarios. |
| Credentials / egress | Login launch/supersede/capture; shared-versus-personal onboarding. | Failure paths, concurrent users, SSO walk, broker/redirect checks. |
| Lifecycle / storage | Completion watcher; Kubernetes scratch accounting and residuals. | Restart/eviction/reconcile and drive tests on dedicated substrates. |
| Product / UX | Recording pane, reduced-motion guard, member onboarding. | Browser suite, mock reviews, complete role-aware journeys. |
| Deployment / recovery | E2E isolation controls, release instructions, resource inventory. | Fresh Helm, upgrade, outage and backup/restore walk. |
| Engineering / docs | DCO, gates, nightly overlap, test-gap inventory, console rules. | Full gate output and docs/SDK/supply-chain review. |

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
  make ci in progress (evidence/integration-ci.log). Do not cherry-pick integration
  on top of individual patches.

## Patch compatibility and release notes

Every product commit listed above is based independently on dfa89f60, signed off,
and can be reviewed/cherry-picked separately. No schema, API, configured cap,
deployment requirement or enforcement default changes. Rollback is an ordinary
revert; no migration to undo. Docs patches touch distinct sections where files
overlap. All four initial docs patches have an independent security-lane factual
review. Selection still needs release-owner gates after any 0.7.6 integration.

- R076-005: Correct the recording pane's permission hint to include security admins.
- R076-006: Document the console's existing reduced-motion support.
- R076-009: Reject oversized brokered uploads instead of forwarding truncated data.
- R076-010: Finalize terminated Kubernetes runs even when the agent exit status is
  missing; report unknown exits as failure and attempt normal credential cleanup.
- R076-011: Keep contributor UI commands at repository root.
- R076-012: Clarify automated conformance versus manual live acceptance gates.
- R076-013: Select a patch version before preparing and validating its release.
- R076-014: Correct when OIDC email-verification restrictions are enforced.

## Open findings, not implemented

R076-002 needs launch serialization across quota checks, row insertion and
supersede, not just changing the timestamp comparison. R076-003 needs live
storage-enforcement and toolchain-compatibility proof before relocating caches.
R076-004 cannot tear down the login sandbox before the helper receives its upload
response and emits the PTY marker the console uses to confirm success. These
remain possible 0.7 follow-ups pending a safe design, not silently assigned to 0.8.

Security review also noted that supported API-token SSH registration outlives
token revocation: incident response must remove SSH keys and stop affected runs.
Email-based OIDC role mapping without a configured domain allowlist trusts the
provider's email claim without requiring email_verified; changing that would
require an explicit compatibility decision, especially for Entra. Split-horizon
OIDC HTTP/HTTPS scheme support remains an unresolved compatibility question.

## Review-owned test resources

- PostgreSQL: wardyn-review-07x-pg, unix:///var/run/docker.sock, loopback port 58432.
- Browser database: wardyn_review_record; backend/gateway ports 18888/18889.
- Operations check databases: wardyn_review_ops_* (documentation lane).
- Dedicated Kubernetes acceptance cluster: wardyn-review-07x-life (lifecycle lane).
- No existing Wardyn databases, instances or clusters are used by these checks.

## Release owner

Review and cherry-pick product commits individually. Draft release notes,
compatibility, rollback and dependencies will accompany completed findings here.
Release numbers and historical changelog entries belong to integration.
Real Entra/AWS, private endpoints and owner hardware remain manual acceptance gaps
until exercised. This ledger does not declare Wardyn ready.
