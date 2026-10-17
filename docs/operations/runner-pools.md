> Part of the [Operations](../OPERATIONS.md) split (task pages under `docs/operations/`).

# Runner pools: where a run goes

- Every run lands in a runner pool. The pool names the runners that may take the run, narrows its size and barrier, and holds it in a queue while they are busy.
- This page is for the admin who keeps organization pools and for the person who launches into a pool or keeps their own.
- Registering a runner and declaring its limits is [Register a runner](../runner-registration.md#runner-limits). Every reason code named here is in [sdk.md](../sdk.md).

## Runners and pools

| Thing | What it is | Who keeps it |
|---|---|---|
| Self-hosted runner | A person's own registered computer. It takes only its owner's runs. | Its owner |
| Remote-hosted runner | The server's own Docker host or Kubernetes cluster. | Admins |
| Organization pool | Remote-hosted runners and the organization's own runners. Everyone who may launch remote runs may use it until a use policy narrows it. | Admins |
| User pool | One person's pool: their own runners, and the remote-hosted runners made available to them. Only its owner uses it. | Its owner, up to `10` pools |

- A pool never claims or transfers a runner, and a user pool never makes another person's runner a candidate (`stepPlacement`, [`internal/api/run_fold_steps.go`](../../internal/api/run_fold_steps.go)).
- A remote-hosted runner joins a person's user pool only while its "Available to people's pools" setting admits them: off (the default), everyone, or listed people, groups and roles (`wardyn runner remote people set`).
- A use policy and a people policy are held for a second human under `WARDYN_GOVERNANCE_SECOND_HUMAN`; see [admin-only writes](admin-only-writes.md).
- On its first boot with pools, a deployment gets one organization pool, named "Organization", that holds its remote-hosted runners and is the organization's default.

| Task | Command |
|---|---|
| List the pools you may use | `wardyn runner pool list`; BARRIERS marks the default barrier with `*` |
| Create a pool | `wardyn runner pool create --name <name>`; an admin adds `--scope organization` |
| Put runners in a pool, or take them out | `wardyn runner pool add <pool> <runner>...`, `wardyn runner pool remove <pool> <runner>...`; an admin may add organization runners; each refused runner gets its own line and the command exits non-zero |
| Rename, switch off or re-limit a pool | `wardyn runner pool update <pool>` with `--name`, `--switch on\|off` or the limit flags |
| Delete a pool | `wardyn runner pool delete <pool> --yes`: its runners leave it and runs waiting for it end; running runs are not touched |

## Which pool, and which runner

| The launch names | Pool it runs in |
|---|---|
| A pool (`runner_pool_id`, `wardyn run --pool`) | That pool, when you may use it. |
| No pool, and you have a default | Your default pool (`wardyn runner pool default set <pool>`). |
| No pool, and only the organization has a default | The organization's (`wardyn runner pool org-default set --pool <pool>`, admin). |
| No pool, and no default applies | Refused `runner_pool_required`. |

- A default that is gone or switched off is refused `runner_pool_default_unavailable`, never replaced by another pool (`Resolve`, [`internal/runnerpool/resolve.go`](../../internal/runnerpool/resolve.go)).
- A launch that names no kind of runner goes to one of your own runners when one has room, otherwise to a remote-hosted runner (`placementForUnasked`, [`internal/api/run_placement_resolve.go`](../../internal/api/run_placement_resolve.go)).
- `placement: local` or `placement: remote` (`wardyn run --placement`) fixes the kind, and `runner_id` (`--runner`) pins one of your own runners. A pool with no runner of that kind is refused `runner_pool_member_mismatch`.
- A governance profile's `deny_placements` holds each kind of runner in a pool, not the pool as a whole; see [the three roles](three-roles.md).
- A run with drives goes to the pool's remote-hosted runners.
- A runner takes the run only when it is online, enforces the run's barrier, declares limits that hold the run's size, and has room.

## Run types

- A pool takes background tasks, interactive environments or both: its limits carry a `background` block, an `interactive` block or both (`--run-type`).
- The pool list answers `run_types` and `run_types_source`: `pool` when the pool's own limits name them, `none` for a pool with no limits, which takes both.
- A pool's limits, once set, are edited, never removed: `{"background":{},"interactive":{},"barriers":[]}` takes both run types and bounds nothing, though a request with no run type is then refused.
- A launch of a type the pool does not take is refused `runner_pool_run_type_not_allowed`. A request with no run type (an older client) is refused `runner_pool_run_type_missing` (`422`) by any pool that has limits: pass `--experience background|interactive` or upgrade the client (`PoolSource`, [`internal/runnerpool/limits.go`](../../internal/runnerpool/limits.go)).

## Limits: the strictest wins

A run's CPU, memory, main drive, lifetime and idle stop are each the strictest of every source that applies (`Effective`, [`internal/runnerpool/limits.go`](../../internal/runnerpool/limits.go)). Preflight, the policy preview and the pool list name the source that bound each one.

| Source | What it bounds |
|---|---|
| `pool` | The pool's own limits for the run type. The console says "Set by this pool". A limit the pool leaves out adds no bound. |
| `governance` | The person's governance profile; an operator has none. |
| `deployment` | The server's default sandbox size, `WARDYN_RUN_MAX_AGE` and the organization's storage maximum. Always applies. |
| `runner` | One runner's whole per-run limit: a runner that takes everything the run asks for, else the largest by CPU, then memory, then disk. The run is placed only on a runner that holds the size it gets. |

- A larger ask is reduced to the limit, never refused; the answer's `resources_applied` says what was asked, what was applied and which source capped it.
- A run that asks for no size gets the lowest default any source names, never above the maximum.
- A pool only narrows its runners. A pool limit wider than its runners' is accepted and never takes effect.
- Runs at once across a pool is `max_concurrent_runs` (`--max-runs`). Left out, it is set by its runners: each runner's own Runs at once and totals still hold.
- A pool none of whose runners has declared limits refuses the launch `runner_limits_unset`.
- A run's live terminal sessions are capped per replica, not across the deployment. With several wardynd replicas and no sticky routing, a ticket mint that lands on a replica already at the cap is refused `attach_session_cap_reached` (503) even when another replica has room; try again.

### A remote-hosted runner's limits

- The server proposes a remote-hosted runner's limits at its first boot from what it measures. It takes half of each as the total, Runs at once as half its CPUs, and per run the total divided by Runs at once. An amount it cannot measure is never guessed, so it sets none and the runner takes no run.
- The usual gap is disk. Docker Desktop's disk is inside its VM, and `wardynd` in a container (the Compose stack) cannot see the host's Docker data root. A Kubernetes cluster is measured from the runs namespace's ResourceQuota, else, with `k8s.readNodes`, from the nodes; the kind quickstart sets `readNodes`.
- `make setup`, `make demo`, `make test-drive` and the repo's walk scripts set it after boot ([`scripts/lib/runner-limits.sh`](../../scripts/lib/runner-limits.sh)), and say each number and where it came from:
  - at a terminal, the proposal is shown to accept or change, and a free disk nothing measured is asked for, with no default;
  - without one, the free disk comes from `WARDYN_SETUP_RUNNER_DISK_GB`, or the whole set from `WARDYN_SETUP_RUNNER_LIMITS` (the `wardyn runner set` flags), or setup stops and names the variable;
  - a runner that already has limits is left alone.
- A Compose stack brought up any other way (`make compose-up`, `docker compose up`) needs this one admin step:

| Task | Command |
|---|---|
| Find runners with no limits | `wardyn runner remote list --no-limits` |
| See the proposal, setting nothing | `wardyn runner remote propose <id>`; add `--free-disk GB` when the runner reports no free disk |
| Set them | `wardyn runner set <id>` with the seven flags `propose` printed, or in the console: Runners, the runner, Edit limits (super admin) |

## Barriers

The pool list's `barrier_options` always answers Fence, Wall and Vault (`CC1`, `CC2`, `CC3`), each `available` or with one `reason_code`. When several apply, the first in this table wins (`BarrierReasons`, [`internal/runnerpool/barrier.go`](../../internal/runnerpool/barrier.go)).

| `reason_code` | Why the barrier is not available |
|---|---|
| `below_floor` | It is below your governance floor. |
| `policy_floor` | It is below this run's policy floor (Review only). |
| `access_floor` | This run's access needs Vault (Review only). |
| `pool_not_allowed` | The pool's limits do not allow it (`barriers`, `--barrier`). A launch that names it is refused `runner_pool_barrier_not_allowed`. |
| `unknown` | No candidate runner's facts are known, for example every one of your runners in the pool is offline. |
| `not_offered` | No known candidate enforces it. `reason_detail` is `kvm_missing` when Vault is missing because the Docker host has no KVM. |

- A pool with an empty `barriers` list allows every barrier its runners offer.
- A launch that names no barrier gets the organization's default barrier when it is available (`default_source: organisation`), otherwise the strongest available one (`default_source: server`).
- An admin sets the organization's default barrier with `wardyn runner pool org-default set --barrier CC1|CC2|CC3|none`, or `PUT /api/v1/runner-pool-defaults` with `barrier`. That `PUT` replaces the document, so one that leaves `barrier` out clears it; one that leaves `recording` out keeps it.
- The organization's default has a `revision` (migration `0185_runner_pool_org_default_revision`). A `PUT` naming a revision that has moved on is refused `runner_pool_stale` (409); one naming none writes unconditionally.
- A person's own default names a pool only; it cannot set a barrier.

## The queue

A launch waits, stored `QUEUED`, when its pool is running its Runs at once or when no runner that could take it has room. A waiting run holds no credential and no sandbox.

| Wait reason | When | The run says |
|---|---|---|
| `runner_pool_at_capacity` | The pool is running its Runs at once. | "{pool} is running its limit of {n}. This run starts when there's room." |
| `runner_pool_runners_at_capacity` | A runner the run could start on is usable, and each one is at one of its limits. | "Every runner in {pool} is at its limit. This run starts when one has room." |
| `runner_pool_no_runner_free` | No runner the run could start on is usable yet: each is offline, declares no limits or cannot enforce the run's barrier. | "None of your runners in {pool} is online. This run starts when one is." |

- The oldest waiting run that can be placed starts first, after its owner's access, role and user type are checked again. A run that cannot be placed yet keeps its place.
- A waiting run starts only on a runner its launch could use (`queueRunnerFree`, [`internal/api/run_queue_promote.go`](../../internal/api/run_queue_promote.go)): one that enforces the barrier the run was queued at, and the kind or the runner the launch named.
- It never starts on one of your own runners when it has drives or could not have been launched there.
- A waiting run is sized again for the runner it starts on, as a fresh launch there would be, and holds that size on it.
- A run waits at most `86400` seconds (24 hours) as a background task and `3600` seconds (1 hour) as an interactive environment. `max_wait_sec` (`wardyn run --max-wait`) changes that, up to `604800` seconds (7 days).
- A run that waits too long ends `FAILED` with `queue_timeout`. Killing a waiting run cancels it.
- A waiting run also ends when its pool is deleted (`runner_pool_removed`) or its owner's sessions are revoked.
- It ends too when, while it waited, its owner's governance profile changed (`queue_profile_changed`), its policy changed (`queue_policy_changed`) or the portal that launched it was removed (`queue_portal_revoked`).
- One person may have `100` runs waiting for one pool; the next launch is refused `runner_pool_queue_full`. A revive into a pool at its limit, or with others waiting, is refused `runner_pool_full`.
- A pool with no runner online that can take the run does not queue the launch. It is refused: `runner_offline` in a user pool, `runner_pool_no_eligible_member` in an organization pool.

## Your own pools

- You create, edit and delete your user pools, and nobody else sees or uses them; an admin lists them read-only with their owner.
- That admin list holds at most 1,000 pools: the organisation's, then the admin's own, then everyone else's by name, with `X-Wardyn-Truncated` set when more exist.
- You put in only your own claimed runners and the remote-hosted runners available to you. No route adds another person's runner.
- A user pool whose runners are all yours is available while one of them is online. With none online the pool list says `runner_pool_no_online_runner`, and a launch is refused `runner_offline`: "None of your runners in {pool} is online."
- A remote-hosted runner stops taking your runs as soon as its people policy no longer admits you.
- Your runner's limits are yours to set (`wardyn runner set <runner>`); a runner with none declared takes no run.
