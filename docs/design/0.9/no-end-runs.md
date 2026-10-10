# 0.9 "No end" runs: choosing a run's end at launch (#1319)

Status: **Draft r2 for owner options, then the mock round, then build.** It designs the create-time end for #1319 against the pool-limits contract.
Background and Interactive behaviour follow the 2026-10-10 owner rulings on run types and pool limits; nothing here reopens them.
Every open product call is in [Owner options](#14-owner-options), each with a recommendation; the rest of the doc holds under either answer, or says which answer it assumes.

## 1. Scope and dependencies

| In scope | Out of scope |
|---|---|
| A create-run end field: a duration, No end, or the default | The decision wait (`wait_budget_sec`) at create; it stays a run-time change |
| Validation against the same limits as `PATCH /runs/{id}`, strictest-wins with pool × run type | Idle stop values: the run-mode lane owns them; this doc only states how they meet the end |
| Background and Interactive behaviour at the end, extension, audit, run detail, API, CLI, SDK, TS | Pool storage and admin screens (H10-pools) |
| The console picker's placement and states for the mock round | Concurrency caps (their owner lane) |
| The run-end persistence: `lifetime_cap_at` and the end reason (lead ruling: the D-1319 build lane owns it) | Any run column in C-pool-limits, which stays a pure contract |

| Depends on | What this design consumes | State |
|---|---|---|
| C-pool-limits | `runnerpool.Effective` and its `EffectiveRunLimits.Lifetime`: an `EffectiveLimit` with default, cap, `Unlimited`, `DefaultSource` and `CapSource`. Also `types.RunEndMaxLifetimeReached` and its sentence | In flight; needs the changes in [§13](#13-coordination-notes) |
| C-mode | The run's canonical type: `CreateRunRequest.EffectiveExperience` at create and `AgentRun.Experience` after | In flight |
| D-116, D-117 | The resolved pool for the request, and the pool recorded on the run | In flight; the run's pool id is a [§13](#13-coordination-notes) ask |
| A-L5 | The per-pool preview shape the lifetime preview sits beside | Session 1 building |
| Existing | `planEnd`, `captureRunLimits`, the lease sweep (`Server.endRun`), the lifecycle reaper, `status_detail` | On batch |

## 2. What exists today

| Piece | Behaviour today |
|---|---|
| End at create | `captureRunLimits` sets `ends_at` from the profile's `default_end_sec`, else `max_end_ahead_sec`; both 0 is no end. The request cannot choose |
| End after launch | `PATCH /runs/{id}` with `ends_at` (a time, or `null` for No end), decided by `planEnd` against the captured run limits |
| The limit's clock | `max_end_ahead_sec` is measured from **now**: each extension may push the end that far again, so a lease is renewable without bound |
| The gate | Moving the end later within the max is always allowed. Shortening it, setting an end where there was none, and No end need `user_changes_limits`; No end also needs `allow_no_end` |
| A run with no end | `planEnd` treats `null` asked on a run with no end as no change, and refuses any finite end without the gate |
| At the end | The lease sweep keeps the run: agent stopped, proxy removed, files kept for `WARDYN_ENDED_RUN_GRACE`. Only an Interactive run may be revived. Kubernetes cannot keep, so it tears down |
| Deployment age cap | `WARDYN_RUN_MAX_AGE` (off by default): the reaper STOPS a run older than it that still holds a sandbox, writing `run.max_age.expire`. A kept run is left to its grace |
| Age cap on Kubernetes | Each run pod also carries `activeDeadlineSeconds` of the cap plus 10 minutes, so the pod fails then whatever the control plane decided |
| Idle stop | The reaper's policy idle stop (`auto_stop_after_sec`) is run-type agnostic today |
| Tightened profile | `reclampRun` cuts a live run's end to now plus the new limit and writes `run.end.set` with `reason: limits_tightened` |
| Super admin | `planRunEndWait` bounds a super admin by the deployment alone; `run.end.set` marks it `limits_exempt: true` |

## 3. The create-run field

### 3.1 Wire shape

Two optional fields on `CreateRunRequest` (one struct shared by the server, the Go SDK and the CLI):

| Field | Type | Meaning |
|---|---|---|
| `ends_after_sec` | integer, 1 to 2147483647 | End this many seconds after the run is created |
| `no_end` | boolean | Ask for No end |
| (neither) | | The effective default ([§3.3](#33-outcomes)) |

- Setting both is a `400`. A relative duration, not a time, because templates, "new run from this run" and presets must carry an end that still means something next week.
- The server computes the end from its own clock and `created_at`; no client clock is trusted.
- `PATCH /runs/{id}` keeps its absolute `ends_at`: a live run's end is a moment the person can see.

### 3.2 Resolving the end at create

The end is decided in the shared fold, so Review, the policy preview and the launch give the same answer.

1. Resolve the run type (C-mode) and the pool (D-117). With no pool (runners off, or today's placement), the pool source is absent.
2. Call `runnerpool.Effective(runType, pool, governance, deployment)` and read `Lifetime`. Under [O9](#o9-the-deployment-age-cap)-A the deployment source carries `WARDYN_RUN_MAX_AGE` as a maximum.
3. Compute the absolute cap ([O1](#o1-the-lifetime-clock)-A): `created_at` plus the lowest pool or deployment maximum, or none.
4. Decide the asked end with a create-only planner, `planCreateEnd`. It shares two helpers with `planEnd`: the latest finite end, and whether No end is permitted.
5. Store `ends_at` and `lifetime_cap_at` on the run row with the captured governance limits, as today.
6. Record what was asked, what was applied, whether it was capped and which source bound it, on the `run.create` row.

`planCreateEnd` is separate from `planEnd` on purpose. `planEnd` decides a change to an existing end, and its "no end yet" branches would refuse a finite end and pass a `null` unchecked at create.

### 3.3 Outcomes

| Request | Result | Status and reason |
|---|---|---|
| Neither field, finite default | `created_at` plus `Lifetime.Default` | `201` |
| Neither field, default unlimited | Background: no end. Interactive: per [O7](#o7-no-end-as-a-default) | `201` |
| `ends_after_sec` within the finite cap, at or after a finite default | That end | `201` |
| `ends_after_sec` past the finite cap | Cut to the cap, never refused (the #1949 rule for CPU and memory) | `201` with a warning naming the source |
| `ends_after_sec` before a finite default | Per [O4](#o4-the-gate-at-create) | `201`, or `403 run_limits_gate_denied` |
| `ends_after_sec` when the default is No end | Per [O4](#o4-the-gate-at-create); capped as above | `201`, or `403 run_limits_gate_denied` |
| `no_end` when the default is already No end | The default; it grants nothing an untouched run lacks | `201` |
| `no_end`, finite default, No end permitted | `ends_at` null | `201` |
| `no_end`, the pool or the deployment bounds the lifetime | Refused, naming the source | `403 run_end_no_end_not_allowed` |
| `no_end`, governance lacks `allow_no_end` | Refused, as `PATCH` refuses it | `403 run_end_no_end_not_allowed` |
| `no_end`, governance lacks the gate | Refused, as `PATCH` refuses it | `403 run_limits_gate_denied` |
| Both fields, or `ends_after_sec` below 1 | Refused before the fold | `400 run_end_conflicting_fields`, `400 run_end_after_too_small` |

- "No end permitted" means: the pool and deployment lifetimes are unlimited, and governance has `allow_no_end` with the gate.
- A default is already No end only when no source names a lifetime at all, today's "no end on the defaults".
- A refusal is answered like the fold's other refusals: no run row, no identity minted, and no audit row.
- The two new reasons join the route reason constants and the console's refusal map.

### 3.4 Who is bound by what

| Caller | Governance limits | Pool limits | Deployment cap |
|---|---|---|---|
| Member, security admin | Yes | Yes | Yes |
| Super admin (`runUngoverned`) | No, as today | Per [O10](#o10-the-super-admin-exemption) | Yes |

- Under O10-A, a super admin's `PATCH` is bounded by the run's captured absolute cap, not the deployment alone. `limits_exempt: true` then means "governance skipped", and its AUDIT-ACTIONS wording changes with it.

## 4. Background and Interactive

| | Background task | Interactive environment |
|---|---|---|
| Normal end | When its command or agent exits | When a person or an admin ends it, or the end-on-exit choice on Tools & Image fires |
| Idle stop | None. Pool limits carry no Background idle field, and the policy idle reaper must skip it ([§13](#13-coordination-notes)) | Per the effective idle value, including never |
| Its end time passes | Per [O2](#o2-a-background-task-at-its-end-time) | Kept, today's lease behaviour (torn down on Kubernetes) |
| Reaches the maximum lifetime | **KILLED**, reason `max_lifetime_reached` (owner ruling) | Per [O3](#o3-an-interactive-environment-at-its-maximum-lifetime) |
| Extension | Allowed before the end, up to the effective maximum (owner ruling) | Allowed, as today |
| No end | Runs until it exits or is killed | Runs until ended; idle stop still applies unless the idle value is never |

- KILLED goes through the existing kill teardown: broker credentials revoked, identity revoked, sandbox removed. The kill row's actor is `system`/`wardynd`.
- On a self-hosted runner that is offline at the cap, the KILLED transition lands at the org at once. The sandbox stop is a pending kill, applied when the runner reconnects (§11).
- A Background task holds no interactive door, so killing rather than keeping loses only its writable layer ([O2](#o2-a-background-task-at-its-end-time) trade-off).

## 5. What "No end" means

No end is offered only when the pool and the deployment leave the lifetime unlimited and governance permits No end. A pool never widens governance and governance never widens a pool.

| Pool lifetime | Governance (captured) | `WARDYN_RUN_MAX_AGE` | No end offered | Longest finite end |
|---|---|---|---|---|
| Unlimited, or no pool | No lifetime limit (max and default 0) | Off | It is the default | None |
| Unlimited, or no pool | Max 7 days, No end allowed, gate held | Off | Yes | 7 days |
| Unlimited | Max 7 days, No end not allowed | Off | No: "your admin's limit" | 7 days |
| 30 days | No end allowed, gate held | Off | No: the pool's name | 30 days |
| Unlimited | No end allowed, gate held | 14 days | No: "this deployment's limit" (O9-A) | 14 days |

- The second row is today's legal profile shape: No end, or a finite end up to 7 days. The finite cap and the No end permission are separate answers, and the preview carries both.
- A No end run still stops on: its own exit (Background), a kill, idle stop (Interactive), a suspension's kill sweep, a revoked runner (RN-Q29), and a tightened profile or pool that reimposes an end.
- `ends_at` null keeps its one meaning, so legacy rows and the existing lease sweep need no change.

## 6. Extension

| Rule | Detail |
|---|---|
| Who | The owner, or a super admin with the owner's authority, as today |
| How far | The earliest of: now plus the captured governance `max_end_ahead_sec`, and the run's `lifetime_cap_at` ([O1](#o1-the-lifetime-clock)) |
| Response | `latest_end` as today, plus `latest_end_source` (`pool`, `governance` or `deployment`) so the console names the limit |
| Authority | Every later end or No end re-runs `extendRefusal` against the owner's current authority, as today |
| Background | Same route and rules; refused once the run is terminal |
| Pool changed after launch | Per [O6](#o6-a-pool-tightened-after-launch) |

- `Effective` folds governance's "ahead of now" maximum and the pool's lifetime into one number. That is exact at create, when now is `created_at`, but not later, so extension reads the two captured bounds separately.
- `latest_end_source: pool` and O6 both need the run to record its pool ([§13](#13-coordination-notes)).

## 7. Audit and run-detail wording

| Event | Audit | Run detail (draft copy for the mock round) |
|---|---|---|
| Launch with an end | `run.create` gains `end`: `asked_sec`, `no_end`, `applied` (time or null), `capped`, `source` | "Ends Tue 18:00 (in 8 hours)" |
| Launch with No end | Same, `applied: null` | "No end" |
| Background with an end | Same | "Ends when it finishes, or at Tue 18:00 at the latest" |
| Capped at launch | `capped: true`, `source` | Toast: "That's as far as {source} allows ({n} days)." |
| Killed at the maximum | `run.kill`, actor `system`/`wardynd`, `reason: max_lifetime_reached`, `ends_at`, `lifetime_cap_at`, `source` | "This run reached its maximum lifetime and was ended." |
| Extension | `run.end.set` as today, plus `source` when capped | Unchanged |

- `{source}` is the pool's name, "your admin" or "this deployment". No profile number reaches a member: profiles stay readable at the security-ops tier only, as for `ends_cap_loosened`.
- The run-detail sentence comes from `status_reason` ([§10](#10-migration)), so no new run field reaches the wire for it.
- The AUDIT-ACTIONS text for `run.create`, `run.kill`, `run.end.set` and `limits_exempt` is in the lane report, not in this lane's diff.

## 8. API, CLI, SDK and TS surface

| Surface | Change |
|---|---|
| `POST /runs` | `ends_after_sec`, `no_end` (§3.1); the `201` warning on a cap |
| Policy preview and Review | Per pool, per run type: lifetime default, finite cap, their sources, and `no_end_allowed` as its own answer |
| `PATCH /runs/{id}` | Response gains `latest_end_source`; the bound gains `lifetime_cap_at` |
| `AgentRun` | Gains `lifetime_cap_at`; the end reason rides the existing `status_detail` and `status_reason` |
| Go SDK | Fields on `pkg/client.CreateRunRequest`, so the type alias carries them to the server with no parity test |
| CLI `wardyn run` | `--ends-in <duration>` (Go duration, `d` accepted) and `--no-end`; both is a usage error. The create output prints the applied end and any cap warning |
| TS types | `CreateRunRequest`, `AgentRun` and the PATCH response mirror the Go fields; `wire-parity.test.ts` pins them |
| Templates and clone | Carry `ends_after_sec` or `no_end` as a request, revalidated at launch; never a permission |

- A `wardyn run end` command for the live change is a follow-up, not this lane: the Go SDK has no `PATCH` client today.

## 9. Console placement for the mock round

| Where | What |
|---|---|
| Runner tab, Lifetime section (after CPU and memory) | The **Ends** picker. Hidden until the run type is chosen, like Tools & Image |
| Picker options | The default (labelled "default"), presets within the cap, "Pick a date…", and "No end" |
| No end when not offered | Shown disabled with an accessible tooltip naming the source, as the barrier choices do |
| Hint under the picker | "Up to {n} days ({source})" for a finite end, or nothing when unlimited |
| Background wording | "Ends when it finishes, or at the latest:" before the picker. No agent wording: Runner may not assume an agent |
| Interactive | The Ends picker, then "When nobody is using it" (idle, run-mode lane) |
| Tools & Image | The exit line: Background "The run ends when the {agent or command} exits, or at the end time on Runner." Interactive keeps end-versus-keep |
| Resolved Policy | One Lifetime row: the applied end or No end, the cap and its source |
| Run detail | The existing Ends row and Change… dialog, with `latest_end_source` in the cap copy |
| Runs list | The existing "No end" value; a filter for No end runs is [O8](#o8-visibility-of-no-end-runs) |

- A pool or run-type change re-resolves the picker. A chosen value past the new cap shows as capped, with the reason, never silently replaced.
- Strings are drafts until the mock round; canon strings then live in `run-lifetime.ts`.

## 10. Migration

The D-1319 build lane owns the run-end persistence in its own migration (lead ruling, 2026-10-10). The number comes from the lead; this lane never takes "next free".

| Item | Where | Why | Legacy rows |
|---|---|---|---|
| `lifetime_cap_at` | New `agent_runs` column, `TIMESTAMPTZ NULL`, only under [O1](#o1-the-lifetime-clock)-A | The absolute latest end from the pool and deployment, captured at create | NULL: no absolute cap, today's behaviour |
| End reason | The existing `agent_runs.status_detail`, read as `status_reason`. No `end_reason` column | See below | Empty, as today |

- The KILLED transition writes `status_detail` as `lifetime: max_lifetime_reached` in the same store write as the state.
- `projectStatusDetail` gains a KILLED branch: it keeps a detail whose token is a valid `types.RunEndReason`, sets `status_reason` to it, and blanks anything else.
- Why it fits: `status_detail` already keeps a terminal reason for a postmortem, and `status_reason` is already the token the console maps to a sentence.
- The column add is metadata-only and joins `scanRun` and every place the run columns are listed.

## 11. Threats and abuse

| Threat | Today's control | This design | Residual |
|---|---|---|---|
| Runaway cost: a looping No end Background task | None at create | No end needs the pool, the deployment and governance to permit it | A loop inside an unlimited pool runs until killed ([O7](#o7-no-end-as-a-default), [O8](#o8-visibility-of-no-end-runs)) |
| Squatting a pool's capacity | Admin kill | The pool's concurrency cap still counts No end runs | A person may hold every slot the cap allows |
| Authority drift on a long run | Re-checked at extend and revive | A No end run never extends, so it is never re-checked there | [O5](#o5-authority-re-check-for-no-end-runs) |
| Credential lifetime | Run token 1-hour TTL; broker credentials short-lived per request | Unchanged; the run keeps renewing for as long as it lives | Renewal continues while the run lives; suspension and runner revocation end it |
| Self-hosted resident credentials | Per-kind bounds (OD-12) | Unchanged | Minted tokens on a runner keep being renewed for a No end run |
| Offline self-hosted runner at the cap | Pending kills applied before replay or READY | The run is KILLED at the org; broker credentials are refused from that moment | The sandbox and any runner-resident minted token outlive the cap until the runner reconnects |
| Mass end-setting by a sweep on a store blip | — | O5's re-check treats a read failure as no answer | None if O5-B is built as written |
| Client clock or overflow | — | Server clock; seconds bounded at `MaxInt32` | None |

- Threat-model delta text goes in the lane report for the docs lane.

## 12. Test plan

| Layer | Proof |
|---|---|
| `planCreateEnd`, table tests | Each outcome row of §3.3, including a default of No end with a finite ask and with `no_end`; each source binding in turn |
| Super admin | Bound per O10; `limits_exempt` on the `PATCH` row matches |
| Mutation proofs | Drop the pool term, drop the deployment term, drop the gate, drop the `allow_no_end` check, pass `no_end` through when the default is finite: each fails a named test |
| Shared helpers | The same finite ask gives the same latest end at create and at `PATCH` |
| Postgres | Create captures `ends_at` and `lifetime_cap_at`; `scanRun` round-trip; legacy NULL rows; KILLED writes `status_detail` with the state |
| Lease sweep | Background at its end and at the cap: per O2, KILLED with `status_reason`; Interactive at its end: kept; at the cap: per O3 |
| Reaper and Kubernetes (O9-A) | The reaper skips a run carrying `lifetime_cap_at`; a run pod's deadline follows the run's own cap |
| Projection | KILLED keeps only a valid end-reason token; any other stale detail is blanked |
| Extension | `latest_end` is the earlier of the two bounds; `latest_end_source` names it; the stored cap binds after the pool loosens |
| Audit | `run.create` `end` fields, `run.kill` reason; a create refusal writes no audit row |
| CLI and SDK | Flag parsing, both flags refused, the alias test, the printed applied end |
| Console (vitest) | Picker hidden before run type; options per cap; No end disabled with each source's reason; Background wording; Resolved Policy row |
| Browser spec | Launch with a duration, a capped duration and No end; run detail shows each |

## 13. Coordination notes

| Lane | Note |
|---|---|
| C-pool-limits (required change, routed by the lead) | `GovernanceSource` disagrees with `planEnd` twice. It reads `max_end_ahead_sec` 0 as unlimited even without `allow_no_end`, and it drops a positive max whenever No end is allowed |
| C-pool-limits (the fix asked) | Keep governance's max as the finite cap, and expose No end permission separately (`no_end_allowed`) using `planEnd`'s rule. Preview and launch then agree |
| C-pool-limits | `RunEndReason` is persisted by D-1319 through `status_detail`; its "stored on the run" comment should say so. O2-A adds `end_reached`, widening the enum and its TS pin |
| C-pool-limits (under O7-B) | Validation: an Interactive lifetime that is unlimited must still carry a finite default |
| D-116, D-117 | The run's pool id on `agent_runs`, for O6 and `latest_end_source` |
| C-mode | The run type is resolved before the end. The policy idle reaper (`auto_stop_after_sec`) still stops Background tasks; one lane must make it skip them |
| A-L5 | Field names for the lifetime entry beside `resources[]` in the preview |
| Kubernetes runner | Under O9-A, D-1319's build sets each run pod's `activeDeadlineSeconds` from the run's own cap |
| Docs lane | AUDIT-ACTIONS rows, `run-lifetime.md`, `ENV.md` note on `WARDYN_RUN_MAX_AGE` as a lifetime source |

## 14. Owner options

### O1. The lifetime clock

**What it's for:** a pool's "maximum lifetime" must be measured from somewhere. Today's governance limit is measured from now, so extending renews it without bound.

| Option | Behaviour | Trade-off |
|---|---|---|
| A. From launch | The pool and deployment maximum is `created_at` + max. Extensions stop there. Governance keeps its "from now" lease inside it | Matches "maximum lifetime" and bounds cost. Needs `lifetime_cap_at` |
| B. From now | Every source is a renewable lease: an extension may push the end to now + max again | No migration; but no run ever reaches its "maximum", so the KILLED reason would never fire except by neglect |

**Recommendation:** A.

### O2. A Background task at its end time

**What it's for:** the ruling covers the maximum. A Background task may also have a shorter end (the default), and that end can pass before the maximum.

| Option | Behaviour | Trade-off |
|---|---|---|
| A. Killed, own reason | KILLED with a new reason `end_reached`; `max_lifetime_reached` only at the maximum | Honest run detail; one more reason, sentence and contract change |
| B. Killed, one reason | KILLED with `max_lifetime_reached` whenever its end passes | One reason, but it says "maximum" when the person picked a shorter end |
| C. Kept, as today | Kept for the files grace, then torn down; killed only at the maximum | Writable layer survives for inspection; it holds a quota slot and an unusable sandbox |

**Recommendation:** A.

### O3. An Interactive environment at its maximum lifetime

**What it's for:** the ruling only names Background. An Interactive run at its end is kept today and may be revived after an extension.

| Option | Behaviour | Trade-off |
|---|---|---|
| A. KILLED if live at the maximum | A live run at the maximum is KILLED `max_lifetime_reached`. A run already kept at an earlier end keeps its files grace, then is torn down as today | One meaning for a live run at the maximum; a kept run, already stopped, is not cut short |
| B. Kept at both | The maximum behaves like any end: kept, revivable only by extending, which the maximum forbids | Files survive for the grace; the run is kept but can never run again |

**Recommendation:** A. On Kubernetes both options tear down at once, since it cannot keep.

### O4. The gate at create

**What it's for:** at run time, a shorter end, an end on a run with none, and No end need `user_changes_limits`. #1319 says create uses the same limits.

| Option | Behaviour | Trade-off |
|---|---|---|
| A. Same as PATCH | An end before the default, or any finite end when the default is No end, needs the gate | Identical rules; a person without the gate cannot pick an end that costs less than the default |
| B. Any finite end is free at create | Any finite end up to the cap is allowed; only No end needs the gate | Lets people choose shorter, cheaper runs; create and PATCH then differ for shortening |

**Recommendation:** B. Shortening at create removes nothing an admin granted to a running run, and extending is already free within the cap.

### O5. Authority re-check for No end runs

**What it's for:** an owner who loses a repository or provider keeps a live run until it next extends or revives. A No end run never does.

| Option | Behaviour | Trade-off |
|---|---|---|
| A. As today | Suspension kills; a tightened profile reclamps; nothing else | No new sweep; a No end run keeps capabilities its owner lost |
| B. Daily re-check, set an end | A daily sweep re-checks the owner's authority; a refusal sets an end one hour out, with a warning | Bounds the drift to a day; one more sweep and audit row |
| C. Daily re-check, kill | As B, but a refusal kills at once | Tightest; kills work in progress with no warning |

- B and C need a request-free form of `extendRefusal`, taking a context and the run, since today it reads the caller from the request.
- In both, a read failure (`owner_unverifiable`, a store or proxy-config read error) changes nothing and is retried the next day. Only a definite capability refusal acts.

**Recommendation:** B.

### O6. A pool tightened after launch

**What it's for:** governance tightening already cuts live runs' ends, measured from now. Under O1-A a pool cap is measured from launch, so a lower cap can already be in the past for an old run.

| Option | Behaviour | Trade-off |
|---|---|---|
| A1. Tighten, cap as computed | `lifetime_cap_at` becomes `created_at` + the new max | Exact; a run past the new cap is KILLED the moment the admin saves |
| A2. Tighten, with notice | As A1, but never earlier than now + 1 hour, with the ending-soon warning | Admin intent applies within an hour; work gets a warning first |
| B. New runs only | A run keeps the pool limits it launched under | Predictable for the person; an admin must kill runs to apply a lower limit |

- Loosening never widens a live run in any option. A and B both need the run's pool id ([§13](#13-coordination-notes)).

**Recommendation:** A2.

### O7. No end as a default

**What it's for:** the pool contract allows an unlimited lifetime with no default, so an untouched run would have no end.

| Option | Behaviour | Trade-off |
|---|---|---|
| A. Allowed | An untouched run gets No end when no source names a default | Fits "app server" pools; a forgotten run never ends |
| B. Explicit only | A pool with an unlimited lifetime must carry a finite default; No end must be picked | Safer default; needs the C-pool-limits validation change; app-server launches need one more click |

- Neither option changes a deployment without pools: a profile with no lifetime keeps today's no end on the defaults.

**Recommendation:** B for Interactive, A for Background, since a task ends on exit anyway.

### O8. Visibility of No end runs

**What it's for:** runaway cost is found by people, not by limits.

| Option | Behaviour | Trade-off |
|---|---|---|
| A. Filter only | The runs list gains a "No end" filter for the owner and admins | Small; nobody is reminded |
| B. Filter and weekly reminder | As A, plus a weekly notice to the owner for each No end run older than 7 days | Catches forgotten runs; one more notice kind |
| C. Nothing new | The existing "No end" value in the list | No work; relies on admins looking |

**Recommendation:** A in 0.9; B as a follow-up if cost reports show forgotten runs.

### O9. The deployment age cap

**What it's for:** `WARDYN_RUN_MAX_AGE` stops runs today through the reaper, with no end shown. The pool contract also counts it as a lifetime source.

| Option | Behaviour | Trade-off |
|---|---|---|
| A. A lifetime source | The cap feeds `lifetime_cap_at`, so the end is shown. Runs carrying the cap end by the lease sweep. The reaper skips them; each Kubernetes pod's deadline follows the run's cap | One end, one outcome per run type. Changes the reaper and the pod deadline; the reaper still covers older runs |
| B. Reaper only | The cap only removes No end from the picker; the reaper STOPS old runs as today | No behaviour change; the shown end can be later than the real one, and the reaper's STOPPED races the lease sweep's KILLED or kept |

**Recommendation:** A.

### O10. The super admin exemption

**What it's for:** today a super admin's run end is bounded by the deployment alone. The pool ruling names the pool and the person's governance, not this exemption.

| Option | Behaviour | Trade-off |
|---|---|---|
| A. Pools bind super admins | A super admin skips governance only; the pool and the deployment still bound their runs | A pool's limit holds for everyone on it; `limits_exempt` changes meaning |
| B. Today's exemption | A super admin is bounded by the deployment alone, pool or not | No change to the exemption; a super admin can hold a capped pool's slot indefinitely |

**Recommendation:** A.
