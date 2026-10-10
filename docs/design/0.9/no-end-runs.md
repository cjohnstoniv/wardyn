# 0.9 "No end" runs: choosing a run's end at launch (#1319)

Status: **Draft for owner options, then the mock round, then build.** It designs the create-time end for #1319 against the pool-limits contract.
Background and Interactive behaviour follow the 2026-10-10 owner rulings on run types and pool limits; nothing here reopens them.
Every open product call is in [Owner options](#14-owner-options), each with a recommendation; the rest of the doc is written so it holds under either answer, or says which answer it assumes.

## 1. Scope and dependencies

| In scope | Out of scope |
|---|---|
| A create-run end field: a duration, No end, or the default | The decision wait (`wait_budget_sec`) at create; it stays a run-time change |
| Validation against the same limits as `PATCH /runs/{id}`, strictest-wins with pool × run type | Idle stop values: the run-mode lane owns them; this doc only states how they meet the end |
| Background and Interactive behaviour at the end, extension, audit, run detail, API, CLI, SDK, TS | Pool storage and admin screens (H10-pools) |
| The console picker's placement and states for the mock round | Concurrency caps (their owner lane) |

| Depends on | What this design consumes | State |
|---|---|---|
| C-pool-limits | `runnerpool.Effective` and its `EffectiveRunLimits.Lifetime` (an `EffectiveLimit` with default, cap, `Unlimited` and the source that bound each), `types.RunEndMaxLifetimeReached` and its sentence | In flight; names may move before merge |
| C-mode | The run's canonical type: `CreateRunRequest.EffectiveExperience` at create and `AgentRun.Experience` after | In flight |
| D-117 | The resolved pool for the request (one pool; candidates intersected) | In flight |
| A-L5 | The per-pool preview shape the lifetime preview sits beside | Session 1 building |
| Existing | `planEnd` and `captureRunLimits` (run limits), the lease sweep (`Server.endRun`), `WARDYN_RUN_MAX_AGE` | On batch |

## 2. What exists today

| Piece | Behaviour today |
|---|---|
| End at create | `captureRunLimits` sets `ends_at` from the profile's `default_end_sec`, else `max_end_ahead_sec`; both 0 is no end. The request cannot choose |
| End after launch | `PATCH /runs/{id}` with `ends_at` (a time, or `null` for No end), decided by `planEnd` against the captured run limits |
| The limit's clock | `max_end_ahead_sec` is measured from **now**: each extension may push the end that far again, so a lease is renewable without bound |
| The gate | Moving the end later within the max is always allowed. Shortening it, setting an end where there was none, and No end need `user_changes_limits`; No end also needs `allow_no_end` |
| At the end | The lease sweep keeps the run: agent stopped, proxy removed, files kept for `WARDYN_ENDED_RUN_GRACE`. Only an Interactive run may be revived |
| Deployment age cap | `WARDYN_RUN_MAX_AGE` (off by default): the reaper stops any run older than it, writing `run.max_age.expire`. Nobody can extend it |
| Tightened profile | `reclampRun` cuts a live run's end to the new limit and writes `run.end.set` with `reason: limits_tightened` |

## 3. The create-run field

### 3.1 Wire shape

Two optional fields on `CreateRunRequest` (one struct shared by the server, the Go SDK and the CLI):

| Field | Type | Meaning |
|---|---|---|
| `ends_after_sec` | integer, 1 to 2147483647 | End this many seconds after the run is created |
| `no_end` | boolean | Ask for No end |
| (neither) | | The effective default, exactly as an untouched run gets today |

- Setting both is a `400`. A relative duration, not a time, because templates, "new run from this run" and presets must carry an end that still means something next week.
- The server computes the end from its own clock and `created_at`; no client clock is trusted.
- `PATCH /runs/{id}` keeps its absolute `ends_at`: a live run's end is a moment the person can see.

### 3.2 Resolving the end at create

The end is decided in the shared fold, so Review, the policy preview and the launch give the same answer.

1. Resolve the run type (C-mode) and the pool (D-117). With no pool (runners off, or today's placement), the pool source is absent.
2. Call `runnerpool.Effective(runType, pool, governance, deployment)` and read `Lifetime`. The deployment source carries `WARDYN_RUN_MAX_AGE` as a maximum.
3. Build the run's captured bounds: the governance run limits as today, plus the absolute cap from step 5.
4. Plan the asked end with the existing `planEnd`, treating the default end as the "current" end. One function decides create and `PATCH`, so the two cannot drift.
5. Cap the result at the absolute lifetime cap ([O1](#o1-the-lifetime-clock)), then store `ends_at` and the cap on the run row.
6. Record what was asked, what was applied, whether it was capped and which source bound it, on the `run.create` row.

### 3.3 Outcomes

| Request | Result | Status and reason |
|---|---|---|
| Neither field | The effective default: `created_at + Lifetime.Default`, or no end when the default is unlimited | `201` |
| `ends_after_sec` within the cap, at or after the default | That end | `201` |
| `ends_after_sec` past the cap | Cut to the cap, never refused (the #1949 rule for CPU and memory) | `201` with a warning naming the source |
| `ends_after_sec` before the default | Allowed only under [O4](#o4-the-gate-at-create) | `201`, or `403 run_limits_gate_denied` |
| `no_end`, every source unlimited and the gate passes | `ends_at` null | `201` |
| `no_end`, a source bounds the lifetime | Refused, naming the source: pool, governance or deployment | `403 run_end_no_end_not_allowed` |
| `no_end` without `allow_no_end` or without the gate | Refused, as `PATCH` refuses it | `403 run_end_no_end_not_allowed` or `run_limits_gate_denied` |
| Both fields, or `ends_after_sec` below 1 | Refused before the fold | `400 run_end_conflicting_fields`, `400 run_end_after_too_small` |

- A refusal is answered like the fold's other refusals: no run row, no identity minted.
- The two new reasons join the route reason constants and the console's refusal map.

### 3.4 Who is bound by what

| Caller | Governance limits | Pool limits | Deployment cap |
|---|---|---|---|
| Member, security admin | Yes | Yes | Yes |
| Super admin (`runUngoverned`) | No, as today | Yes: the pool is the machine's limit, not a person's ceiling | Yes |

- This changes the super admin's `PATCH` path: today `planRunEndWait` bounds them by the deployment alone. With pools, the run's captured pool cap binds them too.

## 4. Background and Interactive

| | Background task | Interactive environment |
|---|---|---|
| Normal end | When its command or agent exits | When a person or an admin ends it, or the end-on-exit choice on Tools & Image fires |
| Idle stop | None: pool limits carry no Background idle field | Per the effective idle value, including never |
| Its end time passes | Ended as in [O2](#o2-a-background-task-at-its-end-time) | Kept, today's lease behaviour, unless [O3](#o3-an-interactive-environment-at-its-maximum-lifetime) says otherwise at the maximum |
| Reaches the maximum lifetime | **KILLED**, reason `max_lifetime_reached` (owner ruling) | Per [O3](#o3-an-interactive-environment-at-its-maximum-lifetime) |
| Extension | Allowed before the end, up to the effective maximum (owner ruling) | Allowed, as today |
| No end | Runs until it exits or is killed | Runs until ended; idle stop still applies unless the idle value is never |

- KILLED goes through the existing kill teardown: broker credentials revoked, identity revoked, sandbox removed. The kill row's actor is `system`/`wardynd`.
- A Background task holds no interactive door, so nothing it holds is lost by killing rather than keeping, except its writable layer ([O2](#o2-a-background-task-at-its-end-time) trade-off).

## 5. What "No end" means

"No end" is available only when **every** source leaves the lifetime unlimited. A pool never widens governance and governance never widens a pool.

| Pool lifetime | Governance (captured) | `WARDYN_RUN_MAX_AGE` | No end offered | Longest end |
|---|---|---|---|---|
| Unlimited, or no pool | No end allowed and the gate held | Off | Yes | None |
| Unlimited | Max 7 days, No end not allowed | Off | No: "your admin's limit" | 7 days |
| 30 days | No end allowed | Off | No: the pool's name | 30 days |
| Unlimited | No end allowed | 14 days | No: "this deployment's limit" | 14 days |

- A No end run still stops on: its own exit (Background), a kill, idle stop (Interactive), a suspension's kill sweep, a revoked runner (RN-Q29), and a tightened profile or pool that reimposes an end.
- `ends_at` null keeps its one meaning, so legacy rows and the existing lease sweep need no change.
- Coordination: `GovernanceSource` treats a profile with `max_end_ahead_sec` 0 as unlimited even when `allow_no_end` is false, while `planEnd` refuses No end there. Step 4 of §3.2 keeps `planEnd`'s rule, so the console must offer No end only when both say yes.

## 6. Extension

| Rule | Detail |
|---|---|
| Who | The owner, or a super admin with the owner's authority, as today |
| How far | The earliest of: now plus the captured governance `max_end_ahead_sec`, and the run's captured absolute lifetime cap ([O1](#o1-the-lifetime-clock)) |
| Response | `latest_end` as today, plus `latest_end_source` (`pool`, `governance` or `deployment`) so the console names the limit |
| Authority | Every later end or No end re-runs `extendRefusal` against the owner's current authority, as today |
| Background | Same route and rules; refused once the run is terminal |
| Pool changed after launch | Per [O6](#o6-a-pool-tightened-after-launch) |

- Coordination: `Effective` folds governance's "ahead of now" maximum and the pool's lifetime into one number. That is exact at create, when now is `created_at`, but not at a later extension. Extension therefore reads the two captured bounds separately.

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
- The AUDIT-ACTIONS rows for `run.create`, `run.kill` and `run.end.set` gain these fields; the text is in the lane report, not in this lane's diff.

## 8. API, CLI, SDK and TS surface

| Surface | Change |
|---|---|
| `POST /runs` | `ends_after_sec`, `no_end` (§3.1); the `201` warning on a cap |
| Policy preview and Review | Per pool, per run type: lifetime default, cap, `unlimited`, source, and whether No end is offered (`no_end_allowed`) |
| `PATCH /runs/{id}` | Response gains `latest_end_source`; the bound gains the captured absolute cap |
| `AgentRun` | Gains `lifetime_cap_at` and `end_reason` ([§10](#10-migration)) |
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
| Hint under the picker | "Up to {n} days ({source})", or nothing when unlimited |
| Background wording | "Ends when it finishes, or at the latest:" before the picker. No agent wording: Runner may not assume an agent |
| Interactive | The Ends picker, then "When nobody is using it" (idle, run-mode lane) |
| Tools & Image | The exit line: Background "The run ends when the {agent or command} exits, or at the end time on Runner." Interactive keeps end-versus-keep |
| Resolved Policy | One Lifetime row: the applied end or No end, the cap and its source |
| Run detail | The existing Ends row and Change… dialog, with `latest_end_source` in the cap copy |
| Runs list | The existing "No end" value; a filter for No end runs is [O8](#o8-visibility-of-no-end-runs) |

- A pool or run-type change re-resolves the picker. A chosen value past the new cap shows as capped, with the reason, never silently replaced.
- Strings are drafts until the mock round; canon strings then live in `run-lifetime.ts`.

## 10. Migration

| Column on `agent_runs` | Type | Why | Legacy rows |
|---|---|---|---|
| `lifetime_cap_at` | `TIMESTAMPTZ NULL` | The absolute latest end, captured at create from the pool and deployment; needed only under [O1](#o1-the-lifetime-clock) option A | NULL: no absolute cap, today's behaviour |
| `end_reason` | `TEXT NULL` | Why the platform ended the run (`max_lifetime_reached`), read by run detail | NULL |

- Both adds are metadata-only. The number comes from the lead; this lane never takes "next free".
- If C-pool-limits or H10-pools lands `end_reason` first, this lane uses theirs and drops its own add.
- The two columns join `scanRun` and every place the run columns are listed.

## 11. Threats and abuse

| Threat | Today's control | This design | Residual |
|---|---|---|---|
| Runaway cost: a looping No end Background task | None at create | No end needs every source unlimited: two admin decisions (pool and governance) plus no deployment cap | A loop inside an unlimited pool runs until killed ([O7](#o7-no-end-as-a-default), [O8](#o8-visibility-of-no-end-runs)) |
| Squatting a pool's capacity | Admin kill | The pool's concurrency cap still counts No end runs | A person may hold every slot the cap allows |
| Authority drift on a long run | Re-checked at extend and revive | A No end run never extends, so it is never re-checked there | [O5](#o5-authority-re-check-for-no-end-runs) |
| Credential lifetime | Run token 1-hour TTL; broker credentials short-lived per request | Unchanged; the run keeps renewing for as long as it lives | Renewal continues while the run lives; suspension and runner revocation end it |
| Self-hosted resident credentials | Per-kind bounds (OD-12) | Unchanged | Minted tokens on a runner keep being renewed for a No end run |
| Client clock or overflow | — | Server clock; seconds bounded at `MaxInt32` | None |

- Threat-model delta text goes in the lane report for the docs lane.

## 12. Test plan

| Layer | Proof |
|---|---|
| Fold, table tests | Each outcome row of §3.3; each source binding in turn (pool, governance, deployment); super admin bound by pool and deployment only |
| Mutation proofs | Drop the pool term, drop the deployment term, drop the gate, accept `no_end` with a bounded source: each fails a named test |
| `planEnd` reuse | The same asked end gives the same answer at create and at `PATCH` |
| Postgres | Create captures `ends_at` and `lifetime_cap_at`; `scanRun` round-trip; legacy NULL rows |
| Lease sweep | Background at its end and at the cap: KILLED with `end_reason`; Interactive at its end: kept; at the cap: per O3 |
| Extension | `latest_end` is the earlier of the two bounds; `latest_end_source` names it; a stored absolute cap binds after the pool loosens |
| Audit | `run.create` `end` fields, `run.kill` reason, denied rows on refusals |
| CLI and SDK | Flag parsing, both flags refused, the alias test, the printed applied end |
| Console (vitest) | Picker hidden before run type; options per cap; No end disabled with each source's reason; Background wording; Resolved Policy row |
| Browser spec | Launch with a duration, a capped duration and No end; run detail shows each |

## 13. Coordination notes

| Lane | Note |
|---|---|
| C-pool-limits | `GovernanceSource` reads unlimited for `max_end_ahead_sec` 0 regardless of `allow_no_end` (§5); and the folded lifetime is exact only at create (§6) |
| C-pool-limits, H10-pools | Who adds `end_reason` (§10) |
| C-mode | The run type must be resolved before the end; idle stop stays Interactive-only there |
| A-L5 | Field names for the lifetime entry beside `resources[]` in the preview |
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
| A. Killed, own reason | KILLED with a new reason `end_reached`; `max_lifetime_reached` only at the maximum | Honest run detail; one more reason and sentence |
| B. Killed, one reason | KILLED with `max_lifetime_reached` whenever its end passes | One reason, but it says "maximum" when the person picked a shorter end |
| C. Kept, as today | Kept for the files grace, then torn down; killed only at the maximum | Writable layer survives for inspection; it holds a quota slot and an unusable sandbox |

**Recommendation:** A.

### O3. An Interactive environment at its maximum lifetime

**What it's for:** the ruling only names Background. An Interactive run at its end is kept today and may be revived after an extension.

| Option | Behaviour | Trade-off |
|---|---|---|
| A. Kept at its end, KILLED at the maximum | Today's keep at a normal end; at the maximum, KILLED `max_lifetime_reached` like Background | One meaning for the maximum across run types; a kept run cannot outlive it by revival |
| B. Kept at both | The maximum behaves like any end: kept, revivable only by extending, which the maximum forbids | Files survive for the grace; the run is kept but can never run again |

**Recommendation:** A.

### O4. The gate at create

**What it's for:** at run time, a shorter end and No end need `user_changes_limits`. #1319 says create uses the same limits.

| Option | Behaviour | Trade-off |
|---|---|---|
| A. Same as PATCH | Create is a PATCH from the default: a shorter end or No end needs the gate | One rule, one function; a person without the gate cannot pick a shorter end, which costs less |
| B. Shorter is free at create | Any end up to the cap is allowed at create; No end still needs the gate | Lets people pick short ends; create and PATCH then differ, and a person could create short then extend |

**Recommendation:** A, the issue's literal reading. B is safe too, since extending is already free within the cap.

### O5. Authority re-check for No end runs

**What it's for:** an owner who loses a repository or provider keeps a live run until it next extends or revives. A No end run never does.

| Option | Behaviour | Trade-off |
|---|---|---|
| A. As today | Suspension kills; a tightened profile reclamps; nothing else | No new sweep; a No end run keeps capabilities its owner lost |
| B. Daily re-check | A sweep runs `extendRefusal` for No end runs once a day and, on refusal, sets an end one hour out, with a warning | Bounds the drift to a day; one more sweep and audit row |
| C. Daily re-check, kill | As B, but kills at once | Tightest; kills work in progress on a transient read failure unless failures are excluded |

**Recommendation:** B.

### O6. A pool tightened after launch

**What it's for:** governance tightening already cuts live runs' ends. Pools are new.

| Option | Behaviour | Trade-off |
|---|---|---|
| A. Tighten live runs | Lowering a pool's lifetime cuts live runs' ends and caps, as `reclampRun` does for profiles | Admin intent takes effect; a No end run can gain an end mid-work |
| B. New runs only | A run keeps the pool limits it launched under | Predictable for the person; an admin must kill runs to apply a lower limit |

**Recommendation:** A, matching profiles; loosening never widens a live run in either case.

### O7. No end as a default

**What it's for:** a pool with an unlimited lifetime may also have no default, so an untouched run would have no end.

| Option | Behaviour | Trade-off |
|---|---|---|
| A. Allowed | An untouched run gets No end when every source's default is unlimited | Fits "app server" pools; a forgotten run never ends |
| B. Explicit only | An untouched run always gets a finite end; No end must be picked | Safer default; a pool must then carry a finite default, and app-server launches need one more click |

**Recommendation:** B for Interactive, A for Background, since a task ends on exit anyway.

### O8. Visibility of No end runs

**What it's for:** runaway cost is found by people, not by limits.

| Option | Behaviour | Trade-off |
|---|---|---|
| A. Filter only | The runs list gains a "No end" filter for the owner and admins | Small; nobody is reminded |
| B. Filter and weekly reminder | As A, plus a weekly notice to the owner for each No end run older than 7 days | Catches forgotten runs; one more notice kind |
| C. Nothing new | The existing "No end" value in the list | No work; relies on admins looking |

**Recommendation:** A in 0.9; B as a follow-up if cost reports show forgotten runs.

### O9. The deployment age cap as a visible end

**What it's for:** `WARDYN_RUN_MAX_AGE` stops runs today without showing an end. As a lifetime source, it gives every run a visible end.

| Option | Behaviour | Trade-off |
|---|---|---|
| A. A visible end | With the cap set, every new run gets `ends_at` at most `created_at` + cap, shown in the picker and run detail; the reaper stays as a backstop | People see the real end; an Interactive run at that end is now kept, not reaper-stopped |
| B. Reaper only | The cap stays invisible; it only removes No end from the picker | No behaviour change; the shown end can be later than the real one |

**Recommendation:** A.
