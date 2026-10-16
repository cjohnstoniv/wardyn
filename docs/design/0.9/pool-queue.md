# 0.9 Pool queue: a full pool makes a run wait, not fail

Status: **Built in 0.9; the owner accepted every recommendation in [Owner options](#21-owner-options).** It designed the 2026-10-10 owner ruling: a launch into a runner pool at its concurrent-run cap queues until a slot frees.
It built on the pool-limits contract (`AtCapacity` returns a `*Wait`) and the accepted #1319 rulings.
Every product call was in [Owner options](#21-owner-options), each with a recommendation.

## 1. Problem and scope

A busy pool must not refuse a launch. The run waits holding nothing a running run holds, and starts under the authority its owner has at the start, not at the request.

| In scope | Out of scope |
|---|---|
| The `QUEUED` state, enqueue, promotion, start re-checks, max wait, cancellation | The person's cap: still a refusal at create |
| Order, flood limits, pool changes, runner availability | Deployment cap, host capacity, quota outside a capped pool: unchanged |
| Failover, audit, metrics, API, SDK, CLI, TS, console placement | Runner selection (D-117, H3), pool storage and screens (H10-pools) |
| The queue's refusal and end reasons, HTTP status, matchers | Server-authored runs (sign-in, scans, probes, record runs): never queue |

| Depends on | What this design consumes | State |
|---|---|---|
| C-pool-limits | `AtCapacity`, `Wait`, `WaitAtCapacity`, `Effective`, `PoolSource`, `CheckBarrier`, `types.RunEndReason` | On batch; needs the [§20](#20-coordination-notes) additions |
| D-117 / D-116 | The resolved pool at create, `agent_runs.runner_pool_id`, a request-free runner pick | In flight; the pool id on the run is a hard prerequisite |
| D-1319 | `ends_after_sec`, `no_end`, `lifetime_cap_at`, end reasons through `status_detail` | Accepted; anchors move ([§20](#20-coordination-notes)) |
| H10-pools | Pool rows, limits, use policies, delete and switch-off | In flight |

## 2. What exists today

| Piece | Behaviour on the batch tree |
|---|---|
| Create order | `handleCreateRun` folds, mints the run identity, inserts the row `PENDING`, answers `201`; `finishCreateRunLaunch` builds the image and dispatches in the background |
| Launch inputs | `createRunLaunch` is in memory only: request, spec, the ceiling resolved from the caller's sign-in claims, grant wiring, run token |
| Provider step | `enforceRunModelProvider(…, renew)`: create renews an expired AWS sign-in; preflight passes `renew=false` and keeps the status read |
| Capacity steps | `stepHostCapacity` (`503`), `stepRunCap` (`422 run_quota`), `stepRunFit` (`422`, refuses when a quota axis has `Need > Left`) run last, before the mint |
| Profile | `resolveAssignedProfile(users, groups, userType)`; a truncated group snapshot resolves on the answerable identity only |
| Caps | Person: `CountActiveRunsBy` over `NonTerminalRunStates`. Deployment: `CreateRunUnderCap` counts rows matching `HoldsSandboxSQL` under an advisory lock. Both `422 run_quota` |
| Watcher lease | `watcher_heartbeat` defaults to `now()` at insert; the plain path takes the lease only inside `dispatchRun`, just before `CreateSandbox`, after the build |
| Undispatched reaper | `finalizeUndispatchedRuns`, every 60 min, fails a sandbox-less non-terminal run older than 60 min from `created_at` |
| Revive and kill | Revive re-checks the owner by sub and user type (`capAllowedForSub`, deny wins) and takes a deployment slot. Kill CASes from the state it read to `KILLED`, then revokes |

## 3. Invariants

1. **Nothing is minted, renewed or delivered while a run is `QUEUED`.** No run identity, no grant rows, no broker or AWS renewal, no proxy config, no masking manifest, no image build, no sandbox, no egress, no pending runner action. A status read of a stored credential is neither.
2. A queued run holds **no slot**: no pool slot, no deployment slot, no host or quota capacity.
3. **Authority is re-checked at the start**, as the owner: identity, profile assignment, launch doors, policy, pool, use policy and limits ([§7](#7-starting-a-queued-run)).
4. The start **only narrows**: never more than the request asked for or enqueue admitted.
5. The queue is **durable in Postgres**. Promotion is one transaction under the pool row lock: exactly once per run, never more slot-holders than the cap.
6. `runner_pool_at_capacity` is a **wait, never a refusal**: no HTTP answer carries it.

## 4. The queued state

A new non-terminal `RunState`, `QUEUED`, before `PENDING`; `PENDING` keeps "admitted, being started". A `PENDING` sub-state is rejected ([Q1](#q1-the-queued-state-decided)).

| From | To | Who | When |
|---|---|---|---|
| (create) | `QUEUED` | The create door | The resolved pool has a concurrent-run cap |
| `QUEUED` | `PENDING` | The promoter | A slot is free and this is the oldest run that can be placed |
| `QUEUED` | `KILLED` | Owner, admin, suspension sweep, pool removal, runner revoke (the run waits for the one runner its launch named), session revoke, service identity retirement, person erasure | Cancelled |
| `QUEUED` | `FAILED` | The queue sweep | Its deadline passed |
| `PENDING` (promoted) | `FAILED` | The start | A start re-check refused |

| Count | Includes `QUEUED` | Why |
|---|---|---|
| Pool slots (`AtCapacity`'s `running`), deployment cap | No | Slot-holders only |
| Person cap (`CountActiveRunsBy`) | Per [Q2b](#q2b-do-waiting-runs-count-against-a-persons-run-limit) (recommended: yes) | "Runs you have going" |
| Suspension sweep, audit-retention live check, `wardyn_runs_active{state}` | Yes | A queued run is live and cancellable |

- A new `types.SlotHoldingRunStates` (non-terminal minus `QUEUED`) feeds the pool and deployment counts, the fleet view, the max-age reaper and the undispatched reaper. A slot-holder also matches `HoldsSandboxSQL`, so a run kept after its end frees its slot.
- **A `run_queue` row exists exactly while its run is `QUEUED`.** Every exit from `QUEUED` deletes the row in the same transaction as the state CAS: promotion, kill, the suspension sweep, the timeout sweep and pool removal. `casRunState` routes `from = QUEUED` to one store call that does both. The flood count, position, `queue_depth` and the promoter scan read `run_queue`.
- While `QUEUED`, `ends_at`, `lifetime_cap_at`, `governance_profile_id` and `run_limits` stay empty; start step 7 writes them. No reader may take a null `ends_at` on a `QUEUED` run as "No end".

## 5. Doors

| Create door | Queues |
|---|---|
| `POST /runs`: console New Run, `wardyn run`, the Go SDK, templates, presets, "new run from this run", a portal launching for a person | When the resolved pool has a cap ([Q11](#q11-should-an-interactive-launch-queue)) |
| Sign-in sandbox, workspace record runs, source scans, site-config probes | Never: no pool, not counted |

| Per-run door on a `QUEUED` run | Behaviour |
|---|---|
| `GET /runs/{id}`, runs list, run events | Shown with `queue` ([§13](#13-api-sdk-cli-and-ts)); title changes allowed |
| `POST /runs/{id}/kill` | CAS `QUEUED`→`KILLED` and delete the `run_queue` row in one transaction, `202`. A kill that won from `QUEUED` skips the identity and broker revocations: nothing was minted |
| `PATCH /runs/{id}` (end, wait budget) | `409 run_queued`: the end is set at the start |
| Attach, exec, SSH, files, output, revive, pause, resume | The existing no-sandbox and not-running refusals; the console hides them |
| `/internal` routes, broker, injection | No token exists; a state guard also refuses `QUEUED` |

## 6. Enqueue

For a pool with a cap, every run takes the queue path, so FIFO has one door:

1. Fold in a new `foldQueue` mode, `foldCreate` with three changes:
   - the provider step runs as preflight's: `renew=false`, status read only;
   - the fit step refuses only a run that can never fit (`Need > Hard` on an axis, a new verdict in `judgeFit`); `Need > Left` waits;
   - host capacity and the deployment cap are skipped; promotion applies them.
2. Insert the run `QUEUED` with `spiffe_id = ''` ([§15](#15-migration)), plus its `run_queue` row, in one transaction holding the pool row `FOR NO KEY UPDATE`. Write the component snapshot as today; no grants.
3. In that transaction, refuse `runner_pool_queue_full` at the per-person limit ([Q2a](#q2a-order-and-a-per-person-limit)). The person cap stays the fold's own check; under [Q2b](#q2b-do-waiting-runs-count-against-a-persons-run-limit)-A it counts waiting runs.
4. After commit, run one promotion transaction for this pool, bounded at 2 s, re-read the run and answer `201`: `QUEUED` with the wait sentence in `warnings[]`, or `PENDING`.
5. A promoted run's start runs detached (`context.WithoutCancel`), with `abortHalfBuiltRun` as its compensator, as the launch tail does today.
6. `run.create` records the queue facts and no `jti`.

- `FOR NO KEY UPDATE`, not `FOR UPDATE`: D-117's foreign key makes run inserts take `FOR KEY SHARE` on the pool.

## 7. Starting a queued run

**Promotion** judges without a lock, then moves runs in one transaction:

1. Count the pool's and the deployment's slot-holders. `AtCapacity` non-nil promotes nothing and records `runner_pool_at_capacity` on the waiting runs; a deployment at `WARDYN_MAX_CONCURRENT_RUNS` leaves heads waiting.
2. Candidates oldest first (`queued_at`, `run_id`), a page at a time until the free slots are filled; skip any whose eligible runners are all busy now (`runner_pool_no_runner_free`), and that person's later runs in the same pass. It keeps its place.
3. Host capacity and the fit check (`Need > Left`) also run before any lock; a no leaves the run waiting. A run that cannot be judged keeps its place, and the next is judged.
4. Lock the pool row `FOR NO KEY UPDATE`, then the deployment-cap advisory lock (always this order), and count the slot-holders again.
5. While there is room, CAS each admitted run `QUEUED`→`PENDING`; set `admitted_at`, the runner binding and the watcher heartbeat; delete the `run_queue` row, returning its inputs.

**The start**, in the promoting process:

| Step | Re-check or action | On refusal |
|---|---|---|
| 1 | Owner active (identity guard, enqueue epoch). A portal launch: the portal row is not revoked (`delegates.revoked_at IS NULL` for `authority.delegate_id`). The 10-minute delegated token's expiry is never read; `grant_id` only fills `via`, published so every row carries it | `FAILED identity_deactivated` or `FAILED queue_portal_revoked` |
| 2 | Profile assignment re-resolved from the enqueue snapshot (`resolveAssignedProfile`); the same profile is rebuilt as it stands now | Different profile: `FAILED queue_profile_changed` |
| 3 | Launch doors as the owner ([Q8](#q8-the-owners-authority-at-the-start)) | `FAILED`, `ownerCapabilityRefusal`'s reason |
| 4 | Pool exists and is on; its use policy admits the owner; `PoolSource`, `CheckBarrier` pass | `FAILED`, the pool reason |
| 5 | Policy unchanged since enqueue ([Q7](#q7-a-policy-edited-while-the-run-waits)) | `FAILED queue_policy_changed` |
| 6 | `Effective` recomputed; over-cap asks reduced with a warning | — |
| 7 | End and lifetime cap from `admitted_at`; `captureRunLimits`; profile id written | — |
| 8 | Provider check with renewal, `MintRunIdentity`, SPIFFE ID, `persistRunGrants`, `unionRunEgress` | `FAILED`, the provider reason |
| 9 | `run.queue.start` with the `jti`; `finishCreateRunLaunch` as today | Dispatch's own |

- Step 1 needs a store read of one portal by id; today there are only `GetDelegateByRaw` and `ListDelegates`.
- A refusal writes `queue: <token>` to `status_detail` and the sentence to `failure_hint`, audits `run.queue.start` `failure`, and kicks the promoter.
- The start holds the watcher lease from promotion, heartbeating through the build, until dispatch takes its own hold. It runs under `creates.track`; after the mint it re-reads the state and revokes if a kill won.
- **Who promotes**: any replica after a slot-freeing commit (a terminal CAS in `casRunState`, a run kept after its end, a cap raised), and the leader's `queue` sweep every 10 s. Correctness rests on the locks and CAS.

## 8. Ordering, fairness and flood limits

| Rule | Detail |
|---|---|
| Order | FIFO per pool by `queued_at`, then `run_id` ([Q2a](#q2a-order-and-a-per-person-limit)) |
| Head that cannot be placed | Skipped, keeps its place: a run whose owner's runners are all busy never blocks another person's |
| Flood | 100 waiting runs per person per pool, counted in `run_queue`, and the person cap under [Q2b](#q2b-do-waiting-runs-count-against-a-persons-run-limit)-A |
| Priority, revive | None ([Q3](#q3-priority)), super admins included; revive per [Q10](#q10-reviving-a-kept-run-in-a-full-pool) |
| Position | `run_queue` rows ahead in the pool; a count, never whose |

## 9. Max wait, ending a queued run, and the reasons

**Max wait** ([Q4](#q4-how-long-a-run-may-wait)): `deadline_at = queued_at + max wait`, fixed at enqueue. Default 24 h for Background, 1 h for Interactive; `max_wait_sec` (1 to 604800) changes it, cut at 7 days with a warning. At the deadline the sweep CASes `QUEUED`→`FAILED` ([Q5](#q5-what-a-timed-out-run-becomes)).

**Reason tokens** ([Q6](#q6-reason-tokens)). This table is part of the build's DONE WHEN.

| Token | Kind | HTTP | Where recorded | Console matcher |
|---|---|---|---|---|
| `runner_pool_at_capacity` | Wait | None, ever | `status_detail` `queue: …` → `status_reason`; `run.create` | `RUN_WAIT.AT_CAPACITY` (exists) |
| `runner_pool_no_runner_free` | Wait, for a run already queued whose runners cannot take it now (busy, or offline since it queued) | None, ever | As above | New `RUN_WAIT.NO_RUNNER_FREE` |
| `runner_pool_runners_at_capacity` | Wait, for a run already queued whose owner's runners are online but each holds its `max_runs` | None, ever | As above | New `RUN_WAIT.RUNNERS_AT_CAPACITY` |
| `runner_pool_queue_full` | Refusal, create | `422` | Error body; no row, no audit row | New `RUNNER_POOL_REFUSAL.QUEUE_FULL`, golden-pinned |
| `runner_pool_full` | Refusal, revive | `422` | Error body; revive refusal audit row | New `RUNNER_POOL_REFUSAL.FULL`, golden-pinned |
| `queue_timeout` | End, `FAILED` | None (no request open); `GET` is `200` | `status_detail`, `failure_hint`, `run.queue.expire` | New `RUN_END_REASON_SENTENCE` entry |
| `queue_profile_changed`, `queue_policy_changed`, `queue_portal_revoked` | End, `FAILED` | None | As above, `run.queue.start` failure | New `RUN_END_REASON_SENTENCE` entries |
| `identity_deactivated` | End, `FAILED` (existing token) | None at the start; `403` where it is a refusal today | As above | Its existing sentence, also added to `RUN_END_REASON_SENTENCE` |
| `runner_pool_removed` | End, `KILLED` | None | `status_detail`, `run.kill` (system) | New `RUN_END_REASON_SENTENCE` entry |
| `run_queued` | Refusal, per-run door | `409` | Error body | Route reason map |

| Token | Draft sentence (canon after the mock round) |
|---|---|
| `runner_pool_no_runner_free` | "None of your runners in {pool} is online. This run starts when one is." |
| `runner_pool_queue_full` | "You already have {n} runs waiting for {pool}. Cancel one or wait for one to start." |
| `runner_pool_full` | "{pool} is running its limit of {n}, and others are waiting. Try again when one finishes." |
| `queue_timeout` | "This run waited too long for a free slot and was not started." |
| `queue_profile_changed` | "Your governance profile changed while this run waited. Start it again." |
| `queue_portal_revoked` | "The portal that launched this run was removed while it waited." |
| `queue_policy_changed` | "The policy this run used changed while it waited. Start it again to review it." |
| `runner_pool_removed` | "The pool this run was waiting for was removed." |

## 10. Cancellation, pool changes and runners

| Event | Queued runs | Running runs |
|---|---|---|
| Owner or admin kill; owner suspended | `KILLED`, `run.kill` with `queued: true` | As today |
| Pool removed | `KILLED runner_pool_removed` in the delete transaction ([Q9](#q9-a-pool-removed-or-switched-off)) | H10-pools' rule |
| Pool switched off | Keep waiting; promotion skips it; the deadline runs | H10-pools' rule |
| Cap lowered below the running count | Wait until slot-holders fall under it | Keep running; a cap never kills |
| Run type, barrier, use policy, CPU, memory, lifetime tightened | Judged at the start; O6-A2 does not apply (no end yet) | CPU, memory: none. Lifetime: O6-A2 |
| Self-hosted runner offline | Not queued: a Review blocker and a create refusal (`422 runner_offline` for a self-hosted pool, `runner_pool_no_eligible_member` for a remote-provided one), as today, and never a `run_queue` row. Only a run already queued waits (`runner_pool_no_runner_free`): a runner that drops offline after enqueue is not free, so the run keeps its place until its deadline | Owner ruling O-A3 (DR-0085); #1319 and PLAN's pending kill |

- `run_queue.pool_id` is `ON DELETE RESTRICT`. Pool removal cancels each waiting run, which deletes its row, then deletes the pool in the same transaction; a delete that skips the cancellation fails instead of orphaning waiting runs.

## 11. Restart and leader failover

| Crash point | Outcome |
|---|---|
| While `QUEUED` | Durable; any replica's promoter or the next leader's sweep carries on |
| After promotion, before dispatch | The sweep's stale-lease pass fails it (revoking any identity) and frees the slot within one tick |
| Leader lost mid-sweep | Every write is a CAS under the pool lock; the next leader re-scans |

- **The stale-lease pass takes promoted runs only**: `PENDING`, no sandbox, `admitted_at IS NOT NULL`, heartbeat over 90 s old. Only promotion writes `admitted_at`, so a plain run in its build (heartbeat = insert time) is never touched.
- `finalizeUndispatchedRuns` iterates `SlotHoldingRunStates`, so it never sees a `QUEUED` run (no sandbox, heartbeat at insert, waiting past 60 min by design). It and the max-age reaper age runs from `COALESCE(admitted_at, created_at)`, so a long-waited run is not failed the moment it starts.
- Lock order is pool row, then the deployment advisory lock, at enqueue, promotion and revive. `CreateRunUnderCap` takes only the advisory lock, so no cycle exists.

## 12. Background and Interactive

- **Background**: waited on by `wardyn run --wait`, CI or the runs list; 24 h default wait; the agent starts as today.
- **Interactive**: the person waits on run detail, where the launch lands; 1 h default wait ([Q11](#q11-should-an-interactive-launch-queue)); idle stop counts from the start.

## 13. API, SDK, CLI and TS

| Surface | Change |
|---|---|
| `POST /runs` | `max_wait_sec`; `201` may carry `state: QUEUED` and the wait sentence in `warnings[]` |
| `AgentRun` | `admitted_at`; `queue` while `QUEUED`: `pool_id`, `queued_at`, `deadline_at`, `position`, `reason` (derived, never stored on the run) |
| Preflight and preview | Per pool: `slots_used`, `slots_max`, `waiting`, "starts now" or "waits" |
| Go SDK | `CreateRunRequest.MaxWaitSec`, `AgentRun.Queue`, `AgentRun.AdmittedAt`, `types.RunQueued` |
| CLI `wardyn run` | `--max-wait <duration>`. With `--wait` and no `--max-wait` it sends `--timeout` minus 60 s (at least 60 s), so `queue_timeout` lands before the CLI's own 124. `--wait` prints position changes to stderr |
| CLI exit | `queue_timeout` is `FAILED`: exit 1 with the sentence |
| TS | `RunState` gains `"QUEUED"`; `AgentRun.queue`, `admitted_at`; `wire-parity.test.ts` pins them |

## 14. Audit actions and metrics

| Action | Actor | Data |
|---|---|---|
| `run.create` | The person | `queue`: `pool_id`, `position`, `max_wait_sec`, `deadline_at`, `capped`; state `QUEUED`; no `jti` |
| `run.queue.start` | `system`/`wardynd`, with `via` for a portal launch | `success`: `waited_sec`, `slots`, applied limits, `jti`; `failure`: `reason` |
| `run.queue.expire` | `system`/`wardynd` | `waited_sec`, `deadline_at` |
| `run.kill` | Person, admin or `system` | `queued: true`; pool removal adds `reason: runner_pool_removed` |

| Metric | Type | Labels |
|---|---|---|
| `wardyn_runner_pool_queue_depth` (`run_queue` rows), `…_slots_used` | Gauges, 15 s snapshot, `max()` across replicas | `pool_id` |
| `wardyn_runner_pool_queue_wait_seconds` | Histogram | `outcome`: started, timeout, cancelled, refused |

## 15. Migration

One migration; its number comes from the lead.

| Item | Change | Legacy rows |
|---|---|---|
| `agent_runs_state_check` | Adds `'QUEUED'` | Unchanged |
| `agent_runs.admitted_at` | `TIMESTAMPTZ NULL`, written by promotion only | NULL: never queued |
| `agent_runs.spiffe_id` | Unchanged, `TEXT NOT NULL`: `''` while `QUEUED`, written at start step 8 | Unchanged |
| `run_queue` | `run_id` PK FK `ON DELETE CASCADE`; `pool_id` FK `ON DELETE RESTRICT`; `owner`, `queued_at`, `deadline_at`, `policy_ref`, `authority` JSONB, `launch` JSONB | New |
| Indexes | `(pool_id, queued_at, run_id)`, `(deadline_at)`, `(pool_id, owner)` | — |
| `agent_runs_watcher_sweep_idx`, audit-retention live-run function | State lists gain `QUEUED`, matching `nonTerminalRunStates` | Rebuilt |

- `spiffe_id`: `''` rather than `DROP NOT NULL`, so the column, `scanRun` and the wire type (`string`, no `omitempty`) stay as they are. Its readers: the store's insert and scan; `wardyn run`'s create output (`spiffe id:` line), which prints "assigned when the run starts" for `''`; the run-detail identity widget (`identity.tsx`), which shows the same; TS `AgentRun.spiffe_id`. The broker and identity read the SPIFFE ID from the token's claims, never the row.
- `launch`: the decoded request and the fold's outputs (spec, workspace ids, drive reference, ephemeral dirs, class, provider). Ids, names and references, no secret values; it holds the operator-only drive host root, so members are never served it.
- `policy_ref`: a stored policy's id and the SHA-256 of its `spec` (the table has no version); a preset's name and version. An inline or template policy is the request's own, so nothing can change under it.
- `authority`: the enqueue session's sub, email, user type, groups (with the truncated flag), role, authority epoch, and a portal's `{delegate_id, grant_id}`. Personal data: gone with the row; the leaver purge deletes it.

## 16. Console placement for the mock round

| Where | What |
|---|---|
| New Run, Runner tab, under the pool select | When full: "{pool} is running its limit of {n}. Your run will wait for a slot (up to {wait})." No wait picker in 0.9 |
| Review, Resolved Policy | A **Start** row: "Starts now" or "Waits for a slot in {pool} ({k} waiting)" |
| Runs list | Word "Waiting", subline "{ordinal} in line · {pool}", a "Waiting" filter chip; `PENDING`'s word becomes "Starting" |
| Run detail | A waiting card: reason sentence, position, "Waiting since 14:02 · gives up at 15:02", **Cancel** |
| Run detail, sign-in warning | When the AWS sign-in ends before the deadline: "Your AWS sign-in ends at {t}. If it cannot be renewed at the start, the run fails." |
| Fleet card, pool admin rows | "{used} of {max} running · {k} waiting", linking to the runs list filtered by pool and "Waiting" |

- `PENDING_NO_DETAIL` ("Queued — …") is rewritten so "queued" has one meaning. Strings are drafts until the mock round.

## 17. Threat-model delta

| Threat | Control | Residual |
|---|---|---|
| Credentials for a run nobody may start | Invariant 1: identity, grants and renewal only after promotion and re-checks | None while `QUEUED` |
| Profile reassigned while waiting | Re-resolved at the start; different profile fails | None |
| Authority lost while waiting | Start re-checks the identity, the revocation cutoff from `queued_at`, a service identity, and the role and user type the current mappings derive; suspension, a credential revocation and a retirement cancel queued runs | A group removal is never seen before the max wait: the epoch moves only on deactivation |
| Policy changed while waiting | Q7-A fails the run | None |
| Queue flood | 100 waiting per person per pool; Q2b | Without a cap, 100 rows per person per pool |
| Crashed promoter | Stale-lease pass on promoted runs | A dead promoted run holds its slot about 100 s |

## 18. Lane split

| Lane | Owns | DONE WHEN (binary) |
|---|---|---|
| Q-a store and state | Migration, `RunQueued`, `SlotHoldingRunStates`, `run_queue` calls, counts, reapers, projection | §19 PG tests pass; the report lists each reader of `NonTerminalRunStates`, `nonTerminalStateNames`, `IsTerminal` and its decision |
| Q-b create, start, sweep | `foldQueue`, the fit verdict, the promoter and kicks, the start, timeout sweep, kill and revive changes (revive takes the pool lock first), pool hooks, audit, metrics, §9 in Go | Every §9 token exists with its HTTP status, recording point and sentence; §19 invariant tests and mutations fail when broken |
| Q-c surface | API projection, SDK, CLI, TS, §9 TS twins and golden rows; console after the mock round | Wire parity, golden and vitest pass; browser spec green |
| Docs | AUDIT-ACTIONS rows, threat-model text, run-lifecycle and CLI docs | Doc gates pass |

- Q-a merges first. Q-b takes the security review track. Q-b's DONE WHEN includes, verbatim: the queue-timeout and queue-impossible reasons of §9, each with its reason string, HTTP status, recording point and console matcher.

## 19. Tests and mutation obligations

| Layer | Proof |
|---|---|
| Invariant 1 (PG) | A queued run has no identity, grant rows, proxy config or mask manifest, and a counting fake sees no renewal; promoted, it has them all |
| Exactly once, no double slot (PG) | Two promoters, cap 1, five waiting: one `PENDING`. Enqueue, promotion and revive racing never exceed the cap |
| FIFO and skip (PG) | Oldest placeable first; an unplaceable head keeps its place |
| Fit split | Enqueue with `Need > Left` and full host capacity: `QUEUED`. `Need > Hard`: `422` |
| Re-checks | Each §7 row refused in turn: `FAILED`, its reason; a reassigned profile: `queue_profile_changed`; portal launches' start rows carry `via` |
| Portal start | A portal launch that waited 11 min starts; one whose portal was revoked while waiting is `FAILED queue_portal_revoked` |
| Queue rows (PG) | Cancel and timeout leave no `run_queue` row; deleting a pool with waiting runs succeeds and leaves them `KILLED runner_pool_removed` |
| Reapers | A queued run past 60 min survives `finalizeUndispatchedRuns`; a promoted run with a stale lease is failed; a plain `PENDING` run 5 min into its build with a 2-min-old heartbeat survives |
| Timeout, kill | Past the deadline: `FAILED queue_timeout`. Cancel: `202`, `KILLED`, no `identity.revoke` row |
| Counts, CLI | Deployment and pool counts exclude `QUEUED`; person count per Q2b. `--wait` sends `--timeout` minus 60 s; exit 1 on `queue_timeout` |
| Browser | Full pool: "Waiting", cancel; a waiting run starts when one ends |

Mutations, each failing a named test: mint before the queue decision; renew in `foldQueue`; refuse `Need > Left` at enqueue; apply host capacity at enqueue; count `QUEUED` as a slot; drop the pool row lock; drop the `QUEUED` CAS predicate; drop `admitted_at IS NOT NULL` from the stale-lease pass; pin the captured profile id instead of re-resolving; read the delegated token's expiry at the start; keep the `run_queue` row on kill (fails the flood-limit test: 100 cancelled runs still block a new launch); skip one §7 re-check; age the reaper from `created_at`; answer `runner_pool_at_capacity` as an HTTP refusal.

## 20. Coordination notes

| Lane | Note |
|---|---|
| C-pool-limits | `wait.go` gains `runner_pool_no_runner_free`; `reasons.go` gains `runner_pool_queue_full` and `runner_pool_full` (`422`); `RunEndReason` gains `queue_timeout`, `queue_profile_changed`, `queue_policy_changed`, `runner_pool_removed` |
| D-1319 | Anchor `ends_after_sec` and `lifetime_cap_at` on `COALESCE(admitted_at, created_at)`; the "No end" filter (O8-A) excludes `QUEUED`; `projectStatusDetail` keeps `queue:` end reasons on `FAILED`; the O5-B sweep skips `QUEUED` |
| D-117 / D-116 | `runner_pool_id` on the run; a request-free runner pick; its FK makes the pool lock `FOR NO KEY UPDATE` |
| H10-pools | Pool delete cancels queued runs in its transaction; cap changes kick the promoter |

## 21. Owner options

### Q1. The queued state (decided)

**Decided, no answer needed:** a new `QUEUED` state. **Rejected:** a flag on `PENDING`: today's `PENDING` code mints credentials and reaps hour-old runs, so one missed check would hand a waiting run a credential.

### Q2a. Order and a per-person limit

**What it's for:** one person could fill a pool's queue.

| Option | What happens | Trade-off |
|---|---|---|
| A. First come, first served, 100 waiting runs per person per pool | Runs start in launch order; the 101st waiting launch is refused | Simple, no new setting; one person can still sit ahead of others |
| B. Take turns by person | The next start goes to whoever has the fewest runs in the pool | Fairest; a run's place in line jumps around |
| C. First come, first served, no limit | Nothing refused | Simplest; one script can queue without end |

**Recommendation:** A.

### Q2b. Do waiting runs count against a person's run limit?

**What it's for:** a governance profile's `max_concurrent_runs` caps how many runs a person has going.

| Option | What happens | Trade-off |
|---|---|---|
| A. Yes | Waiting plus running stays under the limit; one over is refused, as today | Same meaning as today; a CI account with limit 4 waits for at most 4 |
| B. No | Only running runs count; the limit is checked again before each start | People can queue more; the limit check moves to the start |

**Recommendation:** A.

### Q3. Priority

**What it's for:** some runs may matter more.

| Option | What happens | Trade-off |
|---|---|---|
| A. None | Everyone in launch order | Predictable; urgent work waits |
| B. Admin "start next" | An admin moves a run to the front | One more audited action |

**Recommendation:** A in 0.9; B later if asked for.

### Q4. How long a run may wait

**What it's for:** a run should not start long after anyone wanted it.

| Option | What happens | Trade-off |
|---|---|---|
| A. Fixed defaults | Background 24 h, Interactive 1 h; a request may ask up to 7 days | No new pool setting; the CLI's `--wait` default of 30 min caps its own runs at about 29 |
| B. Set per pool | Each pool sets a default and maximum per run type | Fits "each pool has its own limits"; changes the pool contract and admin screens |

**Recommendation:** A. Waiting costs nothing; B can follow later.

### Q5. What a timed-out run becomes

**What it's for:** CI scripts branch on how a run ended.

| Option | What happens | Trade-off |
|---|---|---|
| A. Failed | `FAILED`, "did not start"; `--wait` exits 1 | Reads as a start failure |
| B. Killed | `KILLED`; `--wait` exits 2 | Reads as if someone killed it |

**Recommendation:** A.

### Q6. Reason tokens

**What it's for:** your ruling made `runner_pool_at_capacity` the waiting reason and also "the refusal where queueing is impossible, e.g. a queue timeout".

| Option | What happens | Trade-off |
|---|---|---|
| A. Separate tokens | `runner_pool_at_capacity` only means waiting; a timeout ends as `queue_timeout`; refusals are `runner_pool_queue_full` and `runner_pool_full` | Departs from the ruling's letter, not its intent: a timeout is no HTTP answer (nobody is waiting on a request), and the pool contract already keeps the waiting reason out of refusals |
| B. One token | `runner_pool_at_capacity` is also the `422` and the timeout reason | Matches the letter; screens must tell waiting from ended by state, and the contract's guard is undone |

**Recommendation:** A.

### Q7. A policy edited while the run waits

**What it's for:** the reviewed policy may change before the start (a new stored-policy hash or preset version).

| Option | What happens | Trade-off |
|---|---|---|
| A. Fail the run | `FAILED queue_policy_changed`; the person launches again and reviews | Never runs what nobody reviewed; one admin edit fails every waiting run on that policy |
| B. Start with the old policy | The current profile's blocks are added, as revive does | Waiting CI survives edits; a removed grant may still be used |

**Recommendation:** A.

### Q8. The owner's authority at the start

**What it's for:** at the start nobody is signed in, so the owner's groups cannot be read fresh.

| Option | What happens | Trade-off |
|---|---|---|
| A. User id and type only | Revive's rule: group-based access does not count | Nothing stored; anyone whose profile or access comes from a group fails at the start |
| B. Groups saved at launch | Launch-time groups and role, checked against today's rules and profile assignment (a different profile fails the run) | Group access works; a group removal while waiting is never seen before the wait ends (1 h Interactive, 24 h Background by default) |

**Recommendation:** B.

### Q9. A pool removed or switched off

**What it's for:** an admin acts on a pool that has runs waiting.

| Option | What happens | Trade-off |
|---|---|---|
| A. Removing cancels, switching off pauses | Removal ends waiting runs; switch-off holds them until their deadline | Maintenance keeps CI work; removal is final |
| B. Both cancel | Either ends waiting runs | Simple; a short switch-off loses the queue |
| C. Removal refused while runs wait | `409` until the queue is empty | Nothing lost silently; the admin cancels by hand |

**Recommendation:** A.

### Q10. Reviving a kept run in a full pool

**What it's for:** a revive needs a slot and cannot wait.

| Option | What happens | Trade-off |
|---|---|---|
| A. Refused while others wait | Takes a free slot only when nobody waits; else `422 runner_pool_full` | Launch order holds; the owner retries |
| B. Revive goes first | Takes any free slot | The owner gets their environment back; waiting runs slip |

**Recommendation:** A. Either way revive takes the pool lock before the deployment lock, a build change.

### Q11. Should an Interactive launch queue?

**What it's for:** an Interactive person is at the screen; a run that starts later may find them gone.

| Option | What happens | Trade-off |
|---|---|---|
| A. Queue, 1 h default wait | The launch opens run detail with the waiting card and Cancel | Your ruling as written; New Run and Review say "will wait" first; if they leave, idle stop ends it |
| B. Refuse when full | `422 runner_pool_full` with the wait sentence; the person picks another pool or retries | Nobody comes back to a surprise environment; a busy pool means retrying by hand |

**Recommendation:** A.
