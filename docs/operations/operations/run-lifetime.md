> Part of the [Operations](../OPERATIONS.md) split (task pages under `docs/operations/`).

# Run lifetime: lease, extend, revive, ends

- How a run's lease is captured, extended and ended, and what reviving a kept run can and can't do.
- The full audit contract for every event named here is [`docs/AUDIT-ACTIONS.md`](../AUDIT-ACTIONS.md)'s `run.end.set`, `run.wait_budget.set`, `run.ended`, `run.ended.expired`, `run.lost`, `run.lost.expired` and `run.revive` rows.

```mermaid
flowchart LR
    running["RUNNING"] -->|ends_at passes| kept["KEPT<br/>agent stopped, proxy gone"]
    running -->|kill| gone["torn down"]
    kept -->|grace expires| gone
    kept -->|extend, revive| running
```

## Capturing the lease

> [!NOTE]
> The long-holds design (RL-0..RL-11) landed across 0.8; this page covers what is actually wired today. The diagram's "extend, revive" edge is narrower than it looks: only an interactive run, extended past its own end and still inside its files grace, is eligible for revive (`reviveEligible`, [`internal/api/run_revive.go`](../../internal/api/run_revive.go), #1061).

Every run captures a **lease** at create:

| Part | Holds |
| --- | --- |
| Run limits | Its owner's per-user-type run limits, resolved off the owner's user, groups and user type through the same governance-profile resolution as [Multi-user](../OPERATIONS.md#multi-user-who-can-change-what). |
| End | `ends_at`; `null` is "no end," only where the ceiling allows it. |
| Decision-wait budget | `wait_budget_sec`; it does not bound a run's start, see [Start deadlines](../OPERATIONS.md#the-start-deadlines). |

- A run keeps the limits it captured at create even if the profile that produced them changes later; extending or shortening the lease is bounded by what was captured, not by the profile's current shape.

## Extend

`PATCH /runs/{id}` moves a run's `ends_at` and `wait_budget_sec` — the run's owner, or a super admin acting with the owner's own authority, never a security admin's own ceiling.

| Change | Rule |
| --- | --- |
| Moving the end LATER, within the captured maximum | Always allowed |
| Shortening it, granting "No end," or changing the wait | Needs the captured `user_changes_limits` gate; an over-ask is capped at the limit and the response says so |
| A run already kept by its own end, still inside its files grace | Allowed — moving `ends_at` into the future is the first step toward reviving it (#1061) |
| A run kept by its own end, past its files grace | Refused: "its end cannot be moved" — start a new run |

- Every extend also re-checks the owner's CURRENT authority over the run's agent, workspaces, model provider, stored policy and git provider.
- An owner who has since lost one of those gets the extend refused, naming the capability — exactly as a revive does.

## End

A run's own lease is what ends it — there is no separate "end now" action.

- **Kill** (`POST /runs/{id}/kill`) ends it immediately, with no grace. A different, faster and more forceful path, available to any admin (not just the owner or a super admin), tearing down broker credentials, identity and the sandbox all at once.
- **Kept**: when `ends_at` passes instead, the periodic lease sweep stops the run and, substrate and grace allowing, KEEPS it:

| Component | When the run is kept |
| --- | --- |
| Agent container | Stopped (not removed) |
| Proxy sidecar | Stopped and removed, so the run has no network |
| Approvals and credentials | Pending approvals are cancelled; broker credentials get a best-effort, audit-only revoke |

- The run's own identity is NOT revoked, so the run stays `RUNNING` and holds its quota slot until `WARDYN_ENDED_RUN_GRACE` (default 7 days; `0` tears the run down at once) or a kill.
- `run.ending_soon` warns at 24h, 1h and 10m before the end.
- A kept run's token is refused all the same: every `/internal/*` door refuses it (ended, or lost to a reboot or an outage) with `403` and an `authz.denied` row, reason `run_kept`. Nothing can mint a credential, resolve an injection or decide an approval with it.
- Only the three tail uploads are still accepted, for five minutes after the run was kept. A revive gives the new proxy a fresh token.

### Where the proxy config lives while kept

No container holds the proxy's rendered configuration at rest (#1176): its per-run TLS-MITM CA private key, its run token and any authenticated upstream-proxy URL.

- On Docker the proxy reads the configuration from its stdin once at start, so neither the container's config nor its environment carries it, and the proxy is removed when the run is kept.
- Kept for a revive instead is one database row per run (`run_proxy_configs`), sealed with AES-256-GCM under the `wardyn-run-config-key` boot key.
- The secret store holds that key under its own key-encryption key like every boot key (a `-rewrap` or `-rotate-age-key` moves it with the rest).
- The row is deleted when the run goes terminal, with a purge at boot and on the orphan sweep's cadence behind it (threat model residual 59).
- The kept agent container still holds whatever the agent wrote to its own writable layer for the whole grace window. Nothing on the run's page or in the admin runs list reports how much that is today.

> [!IMPORTANT]
> **Upgrading to this release:** a run started before it has no stored proxy config, so it can't be revived (`409`, start a new run). It runs, ends and is torn down as before. `WARDYN_PROXY_IMAGE` must name a proxy image from this release or later: an older proxy doesn't read its config from stdin and exits at start.

## Revive

`POST /runs/{id}/revive` gives a run a NEW proxy sidecar, built from the run's stored proxy config (never from a container) with a fresh run token and the same per-run MITM CA, under the OWNER's current governance-profile denies.

| Kind | What it does |
| --- | --- |
| Proxy-only revive | For a run lost to a control-plane outage — a new proxy, without touching a running agent |
| Reboot revive | For a run lost to a reboot — also restarts the kept agent container behind the new proxy, so Claude Code can continue its conversation |

- Authority is always the owner's: an admin's click re-asserts the owner's own ceiling, never the caller's, so revive can't hand a member's run limits it doesn't itself hold.
- Nothing scopes WHO may click it beyond "any super admin," over any run in the deployment (see the threat model).

Revive is refused, nothing changed, when:

- The run's captured profile no longer exists, or now denies a host its git broker needs.
- The model credential its proxy would inject has been erased, or its provider disabled.
- The run is past its end and not yet extended — extend it first.
- The run is already kept by its own end, and either it's a task run (not interactive) or it's past its files grace — the agent can't be started again; start a new run instead.

A live run, one lost to an `outage` or a `reboot`, or a kept run that's interactive, extended past its own end and still inside its files grace (#1061), is revivable today.

- Admins get the same path in bulk over LIVE runs only: `POST /admin/runs/restart` ("Restart with current limits") and `GET /admin/runs/proxy-window` (listing runs on an out-of-window proxy release).
- A revived run keeps its ORIGINAL agent image: a reboot revive restarts the same, already-created agent container rather than recreating it from an agent image an operator may have since patched.
- The only way onto a newer image is to end the run and start a new one.

## Rename

`PATCH /runs/{id}/title` (#1197) lets the run's OWNER change its title. It's a display field, not part of the lease above, so it needs no captured limit and is allowed in ANY run state, including a terminal or kept one.

- The gate is OWNER ONLY (`ownsRun`), stricter than every other `/runs/{id}` route: neither an admin nor a `security_admin` may rename someone else's run — both get the byte-identical 404 a non-owner gets.
- A rename carries no security or incident-response warrant the way `GET /runs/{id}`'s inspect-or-stop bypass or the lease PATCH's owner-or-SUPER bypass do.
- The console's New Run screen prefills the title from the task's own first line (up to 80 characters, cut at a word boundary) and leaves it editable; the server itself never required one.

## Kubernetes cannot keep, revive, restart or pause a run

The k8s runner substrate doesn't implement the optional runner capabilities the docker driver does. A k8s agent pins its sidecar proxy's pod IP, so there's no "same address, new container" to replace, and a stopped pod is gone, not kept.

- A k8s run's end and limits still fire on schedule — that half is NOT a gap.
- But ending, losing its sandbox, or being idle all degrade to an immediate, non-resumable teardown rather than a grace window; the pause half of [the clocks](#the-clocks-that-end-or-freeze-a-run) never applies.
- Its proxy config reaches the sidecar through a per-run Secret staged into an in-memory volume (#688), not stdin. Since the run is never kept, that Secret lives exactly as long as the run and is never re-created from the stored row.
- The row is still written for a k8s run (and deleted with it), unused until k8s can revive.

See [Kubernetes: known gaps](kubernetes-known-gaps.md) for the revive and restart refusals.

## The clocks that end or freeze a run

Four clocks can end a run or freeze its agent. They are independent; the first one that fires wins. A person attached to a run holds only the first one off.

| Clock | What it does | Attached | On Kubernetes | Audit |
| --- | --- | --- | --- | --- |
| **Idle stop**, `auto_stop_after_sec` | Stops the run once it has been idle that long, plus a 30 second slack | Not stopped, as long as the attach's keepalive writes succeed | Applies | `run.autostop` |
| **Idle pause**, `pause_idle_after_sec` | Freezes the agent in place, and thaws it on presence | A person's input thaws it | Never applies: the substrate cannot freeze | `run.pause`, `run.resume` |
| **The lease**, `ends_at` | Ends the run: stopped and kept for `WARDYN_ENDED_RUN_GRACE` | Ends it all the same | Applies, but tears the run down at once instead of keeping it | `run.ended` |
| **`WARDYN_RUN_MAX_AGE`** | Stops any run older than the cap, busy or not | Stops it all the same | Applies, and also sets the pods' deadlines | `run.max_age.expire` |

**Idle stop.**

- Idleness is the age of the run's last activity: an egress call, CPU use, or an attach.
- Every attached surface resets the clock when it opens and every 30 seconds while it is held. The surfaces are the browser terminal, an SSH shell, an SSH exec, sftp or `-L` channel, and a relayed connection to an in-sandbox UI.
- So a run someone is attached to is not idle-stopped, as long as those writes succeed. They are best effort and a failed one is dropped, so a database that refuses them for long enough lets an attached run look idle.
- A run nobody is attached to is idle like any other, which is why a run can end an hour after its owner last looked at it.
- The value is `auto_stop_after_sec` in the run's policy. The shipped `default.json` sets `3600`; `0` and a negative value both mean never idle-stop.
- A member's value is clamped to the ceiling's positive maximum. The `-1` that interactive and SSH sessions use becomes that maximum too, so under the shipped default a member's session has an hour of idleness. Admins are not clamped.
- To give members a longer window, raise `auto_stop_after_sec` in the ceiling (the default policy, `defaultPolicy` in the chart). Do not set it to `0`: a ceiling of `0` removes the cap, and it also turns idle stop off for every run that does not set its own value.

**Idle pause.**

- A run is frozen only when its profile sets `pause_idle_after_sec` (at least 630 seconds is used), nothing has happened for that long, its CPU reads quiet, and its substrate can freeze its confinement class (Docker `runc` today).
- A run waiting on a decision is frozen after 15 minutes whatever the profile says.
- The proxy keeps running and the run stays `RUNNING`. Typing into the run, attaching, opening an SSH channel or an in-sandbox UI connection thaws it, as does `POST /runs/{id}/resume`.
- On Kubernetes the runner cannot freeze a pod in place, so the setting is inert there and an idle run is stopped by idle stop.
- The limit is set in a governance profile's run limits; see [three-roles.md](three-roles.md#three-roles-and-who-sets-the-walls) for how profiles combine.

**The lease.** A run has no `ends_at` on the defaults. A governance profile's run limits set one at create, from `default_end_sec` or `max_end_ahead_sec`; the owner moves it later within the captured maximum (see [Extend](#extend)).

**`WARDYN_RUN_MAX_AGE`.** Off by default (unset, `0` or negative). It is an absolute cap on a run's age since creation and is not the lease: a person cannot extend it. See [ENV.md](../ENV.md).
