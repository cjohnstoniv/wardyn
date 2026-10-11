# 0.9 Pool queue: a full pool makes a run wait, not fail

Status: **Draft r1 for owner options, then review, the mock round and the build.** It designs the 2026-10-10 owner ruling: a launch into a runner pool at its concurrent-run cap queues until a slot frees.
It builds on the pool-limits contract (`AtCapacity` returns a `*Wait`) and the accepted #1319 rulings.
Every open product call is in [Owner options](#21-owner-options) with a recommendation; the rest holds under either answer or names the answer it assumes.

## 1. Problem and scope

A busy pool must not refuse a launch. The run waits holding nothing a running run holds, and starts under the authority its owner has at the start, not at the request.

| In scope | Out of scope |
|---|---|
| The `QUEUED` state, enqueue, promotion, the start re-checks, max wait, cancellation | Queueing on the person's or the deployment's cap: both stay refusals at create, as today |
| FIFO order, flood limits, pool changes and runner availability while a run waits | Runner selection itself (D-117 and H3); this design calls it at promotion |
| Restart and failover, audit, metrics, API, SDK, CLI, TS, console placement | Pool storage and the pool admin screens (H10-pools) |
| The refusal and end reasons the queue adds, with their HTTP status and matchers | Server-authored runs (sign-in sandbox, scans, probes, record runs): they carry no pool and never queue |

| Depends on | What this design consumes | State |
|---|---|---|
| C-pool-limits | `AtCapacity(pool, running)`, `Wait`, `WaitAtCapacity`, `Effective`, `PoolSource`, `CheckBarrier`, `types.RunEndReason` | `f9d9c537d`; needs the [§20](#20-coordination-notes) additions |
| D-117 / D-116 | The resolved pool at create, `agent_runs.runner_pool_id`, and a request-free candidate pick for the runner binding | In flight; the pool id on the run is a hard prerequisite |
| D-1319 | `ends_after_sec`, `no_end`, `lifetime_cap_at`, the lifetime end reasons through `status_detail` | Design accepted; its anchor moves to the start ([§20](#20-coordination-notes)) |
| H10-pools | Pool rows, limits, use policies, delete and switch-off | In flight |

## 2. What exists today

| Piece | Behaviour on the batch tree |
|---|---|
| Create order | `handleCreateRun` folds, mints the run identity, inserts the row `PENDING`, answers `201`, then `finishCreateRunLaunch` builds the image and dispatches in the background |
| Launch inputs | `createRunLaunch` is in memory only: request, spec, the ceiling resolved from the caller's sign-in claims, grant wiring, run token |
| Renewal | `stepModelProvider` renews an expired AWS sign-in on `foldCreate` only |
| Capacity steps | `stepHostCapacity` (`503`, `Retry-After: 30`), `stepRunCap` (`422 run_quota`), `stepRunFit` (`422`) run last, before the mint |
| Person cap | `runQuotaRefusal` counts `CountActiveRunsBy` over `NonTerminalRunStates`; refused `422 run_quota` |
| Deployment cap | `CreateRunUnderCap` counts and inserts under one advisory lock; the count is non-terminal rows matching `HoldsSandboxSQL` |
| Kill | `claimKillTransition` CASes from whatever non-terminal state it read to `KILLED`, then the teardown tail revokes |
| Undispatched reaper | `finalizeUndispatchedRuns` fails any non-terminal run with no sandbox older than `undispatchedGrace` (60 min) counted from `created_at` |
| Revive | Re-checks the owner by sub and user type (`capAllowedForSub`, deny wins); starting the agent takes a deployment-cap slot or is refused `422 run_quota` |

## 3. Invariants

1. **Nothing is minted, renewed or delivered while a run is `QUEUED`.** No run identity, no credential-grant rows, no broker or AWS renewal, no proxy config, no masking manifest, no image build, no sandbox, no egress, no pending runner action.
2. A queued run holds **no slot**: no pool slot, no deployment-cap slot, no host or namespace-quota capacity.
3. **Authority is re-checked at the start**, as the owner, never only at enqueue: the owner's identity, the captured governance profile, the launch doors, the policy, the pool, its use policy and its limits ([§7](#7-starting-a-queued-run)).
4. The start **only narrows**. It never grants more than the request asked for or the run was admitted with at enqueue.
5. The queue is **durable in Postgres**. Promotion is one transaction under the pool row lock: exactly once per run, never more slot-holders than the cap.
6. `runner_pool_at_capacity` is a **wait, never a refusal**: no HTTP answer carries it.

## 4. The queued state

A new non-terminal `RunState`, `QUEUED` ([Q1](#q1-a-new-state-or-a-sub-state)). It sits before `PENDING`; `PENDING` keeps its meaning, "admitted, being started".

| From | To | Who | When |
|---|---|---|---|
| (create) | `QUEUED` | The create door | The resolved pool has a concurrent-run cap |
| `QUEUED` | `PENDING` | The promoter | A slot is free and this run is the oldest that can be placed |
| `QUEUED` | `KILLED` | Owner, admin, suspension sweep, pool removal | Cancelled |
| `QUEUED` | `FAILED` | The queue sweep | Its wait passed its deadline |
| `PENDING` (promoted) | `FAILED` | The start | A start re-check refused |

| Count | Includes `QUEUED`? | Why |
|---|---|---|
| Pool slots (`AtCapacity`'s `running`) | No | Slot-holders only, as the contract says |
| Deployment cap (`activeRunsCountSQL`) | No | It counts runs holding a sandbox |
| Person cap (`CountActiveRunsBy`) | Yes | "Runs you have going", unchanged; also the main flood limit ([Q2](#q2-order-and-flood-limits)) |
| Suspension kill sweep, audit-retention live check | Yes | A queued run is live and must be cancellable |
| `wardyn_runs_active{state}` | Yes | It reports each non-terminal state |

- A new `types.SlotHoldingRunStates` (non-terminal minus `QUEUED`) feeds the pool count, the deployment count, the fleet capacity view and the max-age reaper. `NonTerminalRunStates` gains `QUEUED`; the pinned parity tests follow.
- A slot-holder is a run in `SlotHoldingRunStates` that matches `HoldsSandboxSQL`, so a run kept after its end frees its slot, as it frees a deployment slot today.

## 5. Doors

| Create door | Queues |
|---|---|
| `POST /runs`: console New Run, `wardyn run`, the Go SDK, templates, presets, "new run from this run", a portal launching for a person | Yes, when the resolved pool has a cap |
| Harness sign-in sandbox, workspace record runs, source scans, site-config probes | No: no pool in 0.9, not counted in any pool |
| Preflight and the policy preview | Never; they report "starts now" or "waits" for the chosen pool |

| Per-run door on a `QUEUED` run | Behaviour |
|---|---|
| `GET /runs/{id}`, runs list, run events | Shown with the `queue` object ([§13](#13-api-sdk-cli-and-ts)); title changes allowed |
| `POST /runs/{id}/kill` | Cancels: CAS `QUEUED`→`KILLED`; the teardown tail finds nothing to stop or revoke; `202` |
| `PATCH /runs/{id}` (end, wait budget) | `409 run_queued`: the end is set when the run starts |
| Attach, exec, SSH, files, output, revive, pause, resume | The existing no-sandbox, not-lost and not-running refusals; the console hides them |
| `/internal` routes, broker, injection | No token exists; a state guard also refuses `QUEUED` |

## 6. Enqueue

For a pool with a cap, every run takes the queue path, so FIFO has one door:

1. Fold in a new `foldQueue` mode: `foldCreate`'s gates and refusals, but `stepModelProvider` authorizes the selection without renewing, as the preview does. Refusals stay synchronous, with no row and no audit row.
2. Insert the run `QUEUED` with no SPIFFE ID, and its `run_queue` row, in one transaction under the pool row lock (`SELECT … FOR UPDATE` on `runner_pools`). Write the component snapshot as today; write no grants.
3. In that transaction, refuse `runner_pool_queue_full` when the owner already has the per-person limit of waiting runs in this pool ([Q2](#q2-order-and-flood-limits)).
4. After commit, run the promoter for this pool synchronously (bounded at 2 s), re-read the run and answer `201` with its state: `QUEUED` with the wait sentence in `warnings[]`, or `PENDING`.
5. The `run.create` row records the queue facts and no `jti`.

- A run that finds a free slot and nobody waiting is promoted in step 4 and starts through the same path as one that waited.
- The capacity steps still refuse at enqueue as they do today. A run is never accepted into a queue on a deployment that is refusing launches.

## 7. Starting a queued run

**Promotion**, one transaction:

1. Lock the pool row, then take the deployment-cap advisory lock (always in this order).
2. Count the pool's and the deployment's slot-holders. `AtCapacity(pool, running)` non-nil promotes nothing; a deployment at `WARDYN_MAX_CONCURRENT_RUNS` leaves heads waiting with `runner_pool_no_runner_free`.
3. Take candidates oldest first (`queued_at`, `run_id`) `FOR UPDATE SKIP LOCKED`. Skip any the runner pick cannot place now; it keeps its place.
4. Before step 5, the lock-free host-capacity and quota-fit checks run; a no leaves the run waiting with `runner_pool_no_runner_free`.
5. CAS `QUEUED`→`PENDING`, set `admitted_at`, the runner binding and the watcher lease; delete the `run_queue` row, returning its launch inputs.

**The start**, in the promoting process, in this order:

| Step | Re-check or action | On refusal |
|---|---|---|
| 1 | Owner identity active, under the identity guard (under Q8-B, with the epoch captured at enqueue) | `FAILED identity_deactivated` |
| 2 | Captured governance profile still exists; dispatch ceiling rebuilt from it as it stands now (revive's rule) | `FAILED`, revive's reason |
| 3 | Launch doors as the owner ([Q8](#q8-the-owners-authority-at-the-start)) | `FAILED`, `ownerCapabilityRefusal`'s reason |
| 4 | Pool exists, is switched on, its use policy still admits the owner; `PoolSource` and `CheckBarrier` pass | `FAILED`, the pool reason |
| 5 | The stored policy is unchanged since enqueue ([Q7](#q7-a-policy-edited-while-the-run-waits)) | `FAILED queue_policy_changed` |
| 6 | `Effective` recomputed (current pool, captured governance, deployment); over-cap asks reduced with a warning, never refused | — |
| 7 | End and lifetime cap computed from `admitted_at` (D-1319's helper); `captureRunLimits` | — |
| 8 | Model credential check and renewal, then `MintRunIdentity`, SPIFFE ID written, `persistRunGrants`, `unionRunEgress` | `FAILED`, the provider reason |
| 9 | `run.queue.start` with the `jti`; then `finishCreateRunLaunch` as today | Dispatch's own, as today |

- Every refusal writes the reason to `status_detail` (`queue: <token>`) and the sentence to `failure_hint`, audits `run.queue.start` with outcome `failure`, and kicks the promoter.
- The start runs under `creates.track`. A kill that lands between promotion and step 8 wins the CAS; the start re-reads the state after the mint and revokes on a lost race.
- The start holds the run's watcher lease from promotion, heartbeating through the image build, until dispatch takes its own hold.

**Who promotes**: any replica, right after a slot-freeing commit (a terminal CAS in `casRunState`, a run kept after its end, a cap raised), and the leader's `queue` sweep every 10 s as the backstop. Correctness rests on the locks and CAS, so the leader gate only saves duplicate scans.

## 8. Ordering, fairness and flood limits

| Rule | Detail |
|---|---|
| Order | FIFO per pool by `queued_at`, then `run_id` |
| Head that cannot be placed | Skipped, keeps its place: a self-hosted run waiting for its owner's laptop never blocks another person's |
| Flood | Queued runs count toward the owner's governance `max_concurrent_runs`, plus a fixed limit of 100 waiting runs per person per pool ([Q2](#q2-order-and-flood-limits)) |
| Priority | None in 0.9 ([Q3](#q3-priority)); super admins wait like everyone (#1319 O10-A) |
| Revive | Takes a free slot only when nobody waits in the pool ([Q10](#q10-reviving-a-kept-run-in-a-full-pool)) |
| Position | `position` = waiting runs ahead in the pool; a count only, never whose |

## 9. Max wait, ending a queued run, and the reasons

**Max wait** ([Q4](#q4-where-the-maximum-wait-comes-from)): `deadline_at = queued_at + max wait`, fixed at enqueue.

- Default 24 h for Background, 1 h for Interactive. `max_wait_sec` on the request (1 to 604800) changes it; above 7 days it is cut, with a warning.
- At the deadline the queue sweep CASes `QUEUED`→`FAILED` ([Q5](#q5-what-a-timed-out-run-becomes)) with `queue_timeout`.

**Reason tokens** ([Q6](#q6-reason-tokens)). This table is the build's contract and part of its DONE WHEN.

| Token | Kind | HTTP status | Where recorded | Console matcher |
|---|---|---|---|---|
| `runner_pool_at_capacity` | Wait | None, ever | `status_detail` `queue: runner_pool_at_capacity`, projected as `status_reason`; `run.create` `queue.reason` | `RUN_WAIT.AT_CAPACITY` via `RUN_WAIT_REASON` (exists) |
| `runner_pool_no_runner_free` | Wait | None, ever | As above | New `RUN_WAIT.NO_RUNNER_FREE`, two sentences: own runners offline, or the pool's runners busy |
| `runner_pool_queue_full` | Refusal, create | `422` | Error body `reason`; no row, no audit row (a fold refusal) | New `RUNNER_POOL_REFUSAL.QUEUE_FULL` in `RUNNER_POOL_REASONS`, golden-pinned |
| `runner_pool_full` | Refusal, revive | `422` | Error body `reason`; the revive refusal audit row | New `RUNNER_POOL_REFUSAL.FULL`, golden-pinned |
| `queue_timeout` | End reason on `FAILED` | None (no request is open); `GET` answers `200` | `status_detail` `queue: queue_timeout`, `failure_hint`, `run.queue.expire` | New `RUN_END_REASON_SENTENCE.queue_timeout` |
| `queue_policy_changed` | End reason on `FAILED` | None | As above, `run.queue.start` failure | New `RUN_END_REASON_SENTENCE.queue_policy_changed` |
| `runner_pool_removed` | End reason on `KILLED` | None | `status_detail`, `run.kill` (system) | New `RUN_END_REASON_SENTENCE.runner_pool_removed` |
| `run_queued` | Refusal, per-run door | `409` | Error body `reason` | Route reason map |

Draft sentences (canon after the mock round):

| Token | Sentence |
|---|---|
| `runner_pool_no_runner_free` | "None of your runners in {pool} is online. This run starts when one is." / "{pool}'s runners are busy. This run starts when there is room." |
| `runner_pool_queue_full` | "You already have {n} runs waiting for {pool}. Cancel one or wait for one to start." |
| `runner_pool_full` | "{pool} is running its limit of {n}, and others are waiting. Try again when one finishes." |
| `queue_timeout` | "This run waited too long for a free slot and was not started." |
| `queue_policy_changed` | "The policy this run used changed while it waited. Start it again to review the new policy." |
| `runner_pool_removed` | "The pool this run was waiting for was removed." |

## 10. Cancellation, pool changes and runners

| Event | Queued runs | Running runs |
|---|---|---|
| Owner or admin kill | `KILLED`, `run.kill` with `queued: true` | As today |
| Owner suspended | `KILLED` by the existing sweep | As today |
| Pool removed | `KILLED runner_pool_removed` in the delete transaction ([Q9](#q9-a-pool-removed-or-switched-off)) | H10-pools' rule |
| Pool switched off | Keep waiting; promotion skips the pool; the deadline still runs | H10-pools' rule |
| Cap lowered below the running count | Wait; promotion resumes once slot-holders fall under the cap | Keep running; a cap never kills |
| Run type, barrier, use policy, CPU, memory or lifetime tightened | Judged at the start ([§7](#7-starting-a-queued-run)); #1319 O6-A2 does not apply, as there is no end yet | CPU and memory: none. Lifetime: #1319 O6-A2 |
| Self-hosted runner offline | Stays waiting with `runner_pool_no_runner_free`; others pass it | #1319 and PLAN's pending kill |

- `run_queue.pool_id` is `ON DELETE RESTRICT`, so a pool delete that skips the cancellation fails instead of orphaning waiting runs.

## 11. Restart and leader failover

| Crash point | Outcome |
|---|---|
| While `QUEUED` | Durable; any replica's promoter or the next leader's sweep carries on |
| After promotion, before dispatch | `PENDING` with no sandbox and a lease stale after 90 s: the queue sweep fails it (`FAILED`, revoking any identity) and frees the slot within one tick |
| Leader lost mid-sweep | Every write is a CAS under the pool lock; the next leader re-scans |

- `finalizeUndispatchedRuns` runs every 60 minutes and ages runs from `created_at`: alone, it would hold a dead run's slot for up to two hours, and fail a long-waited run at once. Hence the sweep's stale-lease pass, and `admitted_at` (equal to `created_at` for a run that never waited) as the age there and in the max-age reaper.
- Lock order is pool row, then the deployment-cap advisory lock, at enqueue, promotion and revive alike. `CreateRunUnderCap` alone takes only the advisory lock, so no cycle exists.

## 12. Background and Interactive

| | Background task | Interactive environment |
|---|---|---|
| Who waits | `wardyn run --wait`, a CI job, the runs list | The person on run detail, after the launch navigates there |
| Default max wait | 24 h | 1 h |
| At the start | Agent starts as today | Environment comes up; idle stop counts from the start |

## 13. API, SDK, CLI and TS

| Surface | Change |
|---|---|
| `POST /runs` | `max_wait_sec`; `201` may carry `state: QUEUED` and the wait sentence in `warnings[]` |
| `AgentRun` | `admitted_at`; `queue` while `QUEUED`: `pool_id`, `queued_at`, `deadline_at`, `position`, `reason` (derived from `run_queue`, never stored on the run) |
| Preflight and preview | Per pool: `slots_used`, `slots_max`, `waiting`, and "starts now" or "waits" |
| Go SDK | `CreateRunRequest.MaxWaitSec`, `AgentRun.Queue`, `AgentRun.AdmittedAt`, `types.RunQueued` through the shared aliases |
| CLI `wardyn run` | `--max-wait <duration>`. With `--wait` and no `--max-wait`, it sends `--timeout`, so a job never leaves a run waiting past its own deadline. `--wait` prints position changes to stderr |
| CLI exit | A `queue_timeout` run is `FAILED`: exit 1 with the sentence; the CLI's own timeout stays 124 |
| TS | `RunState` gains `"QUEUED"`; `AgentRun.queue`, `admitted_at`; `wire-parity.test.ts` pins them |

## 14. Audit actions and metrics

| Action | Actor | Data |
|---|---|---|
| `run.create` | The person | `queue`: `pool_id`, `position`, `max_wait_sec`, `deadline_at`, `capped`; state `QUEUED`; no `jti` |
| `run.queue.start` | `system`/`wardynd` | `success`: `waited_sec`, `slots`, applied limits, `jti`; `failure`: `reason` |
| `run.queue.expire` | `system`/`wardynd` | `waited_sec`, `deadline_at` |
| `run.kill` | Person, admin or `system` | `queued: true`; pool removal adds `reason: runner_pool_removed` |

| Metric | Type | Labels |
|---|---|---|
| `wardyn_runner_pool_queue_depth` | Gauge from the 15 s snapshot; `max()` across replicas | `pool_id` |
| `wardyn_runner_pool_slots_used` | Gauge, same snapshot | `pool_id` |
| `wardyn_runner_pool_queue_wait_seconds` | Histogram | `outcome`: started, timeout, cancelled, refused |

## 15. Migration

One migration; its number comes from the lead.

| Item | Change | Legacy rows |
|---|---|---|
| `agent_runs_state_check` | Adds `'QUEUED'` | Unchanged |
| `agent_runs.admitted_at` | `TIMESTAMPTZ NULL` | NULL reads as `created_at` |
| `run_queue` | `run_id` PK FK `ON DELETE CASCADE`; `pool_id` FK `ON DELETE RESTRICT`; `owner`; `queued_at`; `deadline_at`; `profile_id`; `policy_version`; `authority` JSONB; `launch` JSONB | New |
| Indexes | `(pool_id, queued_at, run_id)`, `(deadline_at)`, `(pool_id, owner)` | — |
| `agent_runs_watcher_sweep_idx` | Recreated with `QUEUED` in its predicate, matching `nonTerminalRunStates` | Rebuilt |
| Audit-retention live-run function | Its state list gains `QUEUED` | — |

- `launch` holds the decoded request and the fold's outputs: resolved spec, workspace ids, drive reference, ephemeral dirs, enforced class, model provider choice. Secret names only, never values.
- `authority` holds the enqueue session's group snapshot, email and authority epoch under [Q8](#q8-the-owners-authority-at-the-start)-B. It is personal data; it dies with the row, and the leaver purge deletes the person's rows.

## 16. Console placement for the mock round

| Where | What |
|---|---|
| New Run, Runner tab, under the pool select | When the pool is full: "{pool} is running its limit of {n}. Your run will wait for a slot (up to {wait})." No wait picker in 0.9 |
| Review, Resolved Policy | A **Start** row: "Starts now" or "Waits for a slot in {pool} ({k} waiting)" |
| Runs list | Word "Waiting" for `QUEUED`, subline "{ordinal} in line · {pool}"; `PENDING`'s word changes from "Queued" to "Starting" |
| Runs list filter | A "Waiting" chip beside the live filters |
| Run detail | A waiting card in place of start-up progress: the reason sentence, position, "Waiting since 14:02 · gives up at 15:02", **Cancel** |
| Fleet capacity card, pool admin rows | Per pool: "{used} of {max} running · {k} waiting", linking to the runs list filtered by pool and "Waiting" |

- `PENDING_NO_DETAIL` ("Queued — …") is rewritten so "queued" has one meaning.
- Strings are drafts until the mock round; canon then lives in `runner-pool-refusals.ts` and its golden table.

## 17. Threat-model delta

| Threat | Control | Residual |
|---|---|---|
| Credentials for a run nobody may start | Invariant 1: identity, grants and renewal happen only after promotion and the re-checks | None while `QUEUED` |
| Authority lost while waiting | Start re-checks (§7); suspension kills queued runs | Under Q8-B a removed group membership is not seen until the epoch moves; bounded by the max wait |
| Policy changed while waiting | Q7-A fails the run | None |
| Double slot across replicas | Count and CAS under the pool row lock | None |
| Queue flood | Person cap counts queued runs; 100 waiting per person per pool | Without a cap, 100 waiting rows per person per pool |
| Position leak | Position is a count, never whose runs | Members learn how busy a pool is |
| Crashed promoter | The sweep's stale-lease pass | A dead promoted run holds its slot about 100 s |

## 18. Lane split

| Lane | Owns | DONE WHEN (binary) |
|---|---|---|
| Q-a store and state | Migration, `RunQueued`, `SlotHoldingRunStates`, `run_queue` store calls, counts, reapers, projection | §19 PG tests pass; the report lists every reader of `NonTerminalRunStates`, `nonTerminalStateNames` and `IsTerminal` with its decision |
| Q-b create, start, sweep | `foldQueue`, the create branch, the promoter and kicks, the start re-checks, timeout sweep, kill and revive changes, pool delete and tighten hooks, audit, metrics, the §9 reasons in Go | Every §9 token exists with its status, recording point and Go sentence; invariant tests and mutations in §19 fail when broken |
| Q-c surface | API projection, SDK, CLI, TS, the §9 TS twins and golden rows; console after the mock round | Wire parity, golden and vitest pass; browser spec in §19 green |
| Docs | AUDIT-ACTIONS rows, threat-model text, run-lifecycle and CLI docs | Doc gates pass |

- Q-a merges first; Q-b and Q-c build on it. Q-b is security-sensitive and takes the security review track.
- Q-b's DONE WHEN includes, verbatim: the queue-timeout and queue-impossible refusals of §9, each with its reason string, HTTP status, where it is recorded and its console matcher.

## 19. Tests and mutation obligations

| Layer | Proof |
|---|---|
| Invariant 1 (PG) | A run enqueued into a full pool has no identity, no `credential_grants` rows, no proxy config, no mask manifest, and no renewal call on a counting fake; then promoted, it has them all |
| Exactly once (PG) | Two promoters racing on one pool with cap 1 and five waiting: exactly one `PENDING`, the rest `QUEUED` |
| No double slot (PG) | Enqueue, promote and revive racing at the cap never exceed it |
| FIFO and skip (PG) | Oldest placeable first; an unplaceable head keeps its place |
| Re-checks | Each §7 row refused in turn gives `FAILED` with its reason and a `run.queue.start` failure |
| Timeout | Past `deadline_at`: `FAILED queue_timeout`; a promoted run is never expired |
| Reapers | A queued run past 60 minutes survives `finalizeUndispatchedRuns`; a promoted run with a stale lease is failed |
| Counts | Person cap counts `QUEUED`; deployment and pool counts do not |
| Kill | Cancelling a queued run: `202`, `KILLED`, no revoke error |
| CLI | `--max-wait`; `--wait` sends `--timeout`; exit 1 on `queue_timeout` |
| Browser | Launch into a full pool, see "Waiting", cancel; and a second run that starts when the first ends |

Mutations, each failing a named test: mint before the queue decision; renew in `foldQueue`; count `QUEUED` as a slot; drop the pool row lock; drop the `state = 'QUEUED'` CAS predicate; skip one §7 re-check; measure the reaper's age from `created_at`; drop `QUEUED` from the person count; answer `runner_pool_at_capacity` as an HTTP refusal.

## 20. Coordination notes

| Lane | Note |
|---|---|
| C-pool-limits | `wait.go` gains `runner_pool_no_runner_free`; `reasons.go` gains `runner_pool_queue_full` and `runner_pool_full` (`422`); `RunEndReason` gains `queue_timeout`, `queue_policy_changed`, `runner_pool_removed`. Under Q6-A the r1 "Wait is not a refusal" guard stays |
| D-1319 | Anchor `ends_after_sec` and `lifetime_cap_at` on `admitted_at`; `projectStatusDetail` keeps `queue:` end reasons on `FAILED` beside its `KILLED` branch; the O5-B daily sweep skips `QUEUED` |
| D-117 / D-116 | `runner_pool_id` on the run; a request-free candidate pick for promotion |
| H10-pools | Pool delete cancels queued runs in its transaction; cap changes kick the promoter |

## 21. Owner options

### Q1. A new state or a sub-state

**What it's for:** every reaper, count and screen must tell a waiting run from one being started.

| Option | Behaviour | Trade-off |
|---|---|---|
| A. New `QUEUED` state | Its own state before `PENDING` | One migration and a parity sweep; "no credential while `QUEUED`" is checkable by state |
| B. `PENDING` sub-state | `PENDING` with a queued flag | No enum change; every `PENDING` reader must learn the flag, and a miss mints for or fails a waiting run |

**Recommendation:** A.

### Q2. Order and flood limits

**What it's for:** one person can fill a pool's queue.

| Option | Behaviour | Trade-off |
|---|---|---|
| A. FIFO, person cap counts waiting runs, 100 waiting per person per pool | The governance cap also bounds waiting; a fixed limit catches the rest | No new setting; one person can still sit ahead of others |
| B. Round robin by person | Next start goes to the person with the fewest runs in the pool | Fairest; positions jump, harder to explain |
| C. FIFO only | No limit | Simplest; one script can queue without bound |

**Recommendation:** A.

### Q3. Priority

**What it's for:** some runs may matter more.

| Option | Behaviour | Trade-off |
|---|---|---|
| A. None | FIFO for everyone | Predictable; urgent work waits |
| B. Admin "start next" | An admin moves a run to the front | Small; one more audited action |

**Recommendation:** A in 0.9; B as a follow-up if asked for.

### Q4. Where the maximum wait comes from

**What it's for:** a run must not start days after anyone wanted it.

| Option | Behaviour | Trade-off |
|---|---|---|
| A. Fixed defaults | Background 24 h, Interactive 1 h, any request up to 7 days | No pool field; one number for every pool |
| B. Per pool, per run type | A pool limit with default and maximum | Fits "each pool has its own limits"; changes the pool contract, storage and admin screens |

**Recommendation:** A. A waiting run holds nothing; B can follow later.

### Q5. What a timed-out run becomes

**What it's for:** CI branches on the end state.

| Option | Behaviour | Trade-off |
|---|---|---|
| A. `FAILED queue_timeout` | "Failed: did not start" | `run --wait` exits 1, like a start failure |
| B. `KILLED queue_timeout` | "Ended by Wardyn" | Exits 2; reads as if someone killed it |

**Recommendation:** A.

### Q6. Reason tokens

**What it's for:** your ruling named `runner_pool_at_capacity` for both the wait and the refusal; the pool contract has since made it a wait only.

| Option | Behaviour | Trade-off |
|---|---|---|
| A. Distinct tokens | The wait keeps `runner_pool_at_capacity`; refusals and endings get their own (§9) | Each token means one thing; the guard that a wait is never answered as a refusal stays |
| B. One token | `runner_pool_at_capacity` is also the `422` and the end reason | Fewer tokens; screens must branch on the state, and the guard is undone |

**Recommendation:** A.

### Q7. A policy edited while the run waits

**What it's for:** the person reviewed one policy; another may be in force at the start.

| Option | Behaviour | Trade-off |
|---|---|---|
| A. Fail the run | `FAILED queue_policy_changed`; the person starts again and reviews | Never runs what nobody reviewed; an admin edit fails every waiting run on that policy |
| B. Start with the old policy | The current ceiling's denies are added (revive's rule) | Waiting CI survives edits; a narrowed grant may still be used |

**Recommendation:** A.

### Q8. The owner's authority at the start

**What it's for:** no sign-in session exists at the start, so the owner's groups cannot be read fresh.

| Option | Behaviour | Trade-off |
|---|---|---|
| A. Revive's rule | Sub and user type only; deny wins; group allow rows do not count | No stored groups; a person whose access comes from a group is refused at the start |
| B. Enqueue snapshot | Groups and email from the enqueue session, judged against today's rows; any authority-epoch change fails the run | Group access works; a group removal while waiting is missed until the epoch moves |

**Recommendation:** B.

### Q9. A pool removed or switched off

**What it's for:** an admin acts on a pool with runs waiting.

| Option | Behaviour | Trade-off |
|---|---|---|
| A. Remove cancels, switch-off waits | Removal kills waiting runs (`runner_pool_removed`); switching off pauses promotion | Maintenance does not lose CI work; removal is final |
| B. Both cancel | Either kills waiting runs | Simple; a short switch-off loses the queue |
| C. Remove refused while runs wait | `409` until the queue is empty | Nothing lost silently; the admin cancels by hand |

**Recommendation:** A.

### Q10. Reviving a kept run in a full pool

**What it's for:** a revive needs a slot, and a revive cannot wait.

| Option | Behaviour | Trade-off |
|---|---|---|
| A. Refused while others wait | Takes a free slot only when nobody waits; else `422 runner_pool_full` | FIFO holds; the owner retries |
| B. Revive jumps the queue | Takes any free slot | Owners get their environment back; waiting runs slip |

**Recommendation:** A.
