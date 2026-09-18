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
| R076-005 | Recording-pane member hint names admin only although its gate admits security admins. Low, confirmed. | implemented | review/0.7x-security-tier-copy / a470804d (provisional) | Earlier 51 component tests/typecheck pass. Missing DCO, mock review, browser suite, completed make ci. |
| R076-006 | Console rules say reduced motion is unimplemented although global CSS guard and tests ship. Low, confirmed. | confirmed | docs lane | CONSOLE-RULES rubric versus theme.css and theme-contrast.test.ts. |
| R076-007 | Shared Bedrock expiry offers a member a personal sign-in that the server refuses. Low, candidate. | candidate | root UX lane | member-getting-started summary CTA versus authorizeHarnessLogin. |
| R076-008 | Nightly fixtures and proxy logger race already have fixes on another branch. | duplicate | fix/nightly-harness / 6a18f2b1 | Six owner commits beyond baseline. Do not duplicate their implementation. |

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

## Release owner

Review and cherry-pick product commits individually. Draft release notes,
compatibility, rollback and dependencies will accompany completed findings here.
Release numbers and historical changelog entries belong to integration.
Real Entra/AWS, private endpoints and owner hardware remain manual acceptance gaps
until exercised. This ledger does not declare Wardyn ready.
