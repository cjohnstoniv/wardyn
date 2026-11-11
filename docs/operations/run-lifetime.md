> Part of the [Operations](../OPERATIONS.md) split (task pages under `docs/operations/`).

# Run lifetime: lease, extend, revive, ends

- How a run's lease is captured, extended and ended, and what reviving a kept run can and can't do.
- The full audit contract for every event named here is [`docs/AUDIT-ACTIONS.md`](../AUDIT-ACTIONS.md)'s `run.end.set`, `run.wait_budget.set`, `run.ended`, `run.ended.expired`, `run.lost`, `run.lost.expired` and `run.revive` rows.

## Capturing the lease

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
| A run already kept by its own end, still inside its files grace | Allowed — moving `ends_at` into the future is the first step toward reviving it |
| A run kept by its own end, past its files grace | Refused: "its end cannot be moved" — start a new run |

- Every extend also re-checks the owner's CURRENT authority over the run's agent, workspaces, model provider, stored policy and git provider, and that the owner's profile still allows the run's placement (`placement_denied`).
- An owner who has since lost one of those gets the extend refused, naming the capability — exactly as a revive does.

## End

A run's lease ends it, or someone ends it now.

- **End run** (the console button, or `POST /runs/{id}/kill`) ends it at once, with no grace, tearing down broker credentials, identity and the sandbox together. Any admin may, not only the owner or a super admin.
- **Kept**: when `ends_at` passes, the periodic lease sweep stops the run and, substrate and grace allowing, keeps it:

| Component | When the run is kept |
| --- | --- |
| Agent container | Stopped (not removed) |
| Proxy | Stopped and removed, so the run has no network |
| Approvals and credentials | Pending approvals are cancelled; broker credentials get a best-effort, audit-only revoke |

- The run's own identity is not revoked, so the run stays `RUNNING` and holds its quota slot until `WARDYN_ENDED_RUN_GRACE` (default 7 days; `0` tears the run down at once) or an End run.
- A run lost to an outage or a reboot (`run.lost`) is a separate path; see [Revive](#revive).
- `run.ending_soon` warns at 24h, 1h and 10m before the end.
- A kept run's token is refused all the same: every `/internal/*` door refuses it (ended, or lost to a reboot or an outage) with `403` and an `authz.denied` row, reason `run_kept`. Nothing can mint a credential, resolve an injection or decide an approval with it.
- Only the three tail uploads are still accepted, for five minutes after the run was kept. A revive gives the new proxy a fresh token.

### At the lifetime cap

The cap (`lifetime_cap_at`) is a run's hard end. An extension never passes it, and a run launched under an elevation is capped at the elevation's end.

- From the cap, every request the run's sandbox makes to the organisation is refused (`authz.denied`, `run_terminal`, with `lifetime_cap_at`), its token is not renewed, and its pending approvals cannot be decided (`409 approval_run_past_lifetime`).
- Within a minute the lease sweep kills it (`run.kill`, `max_lifetime_reached`).
- The three tail uploads are still accepted for five minutes from the cap.

At the kill:

| What | Outcome |
| --- | --- |
| The sandbox and its network | Gone |
| Pending approvals | Cancelled |
| Azure DevOps run tokens and broker credentials | Revoked |
| An Azure DevOps credential on the Entra lane, and a cluster token | Live only in the run's proxy and cut with it, not revoked; a cluster token lasts at most 10 minutes |

- A queued run whose cap passed while it waited ends without starting.
- A kept run past its cap cannot be revived.
- A run in a unit its owner has left, an elevation's unit after the elevation ends for instance, cannot be revived or restarted from the queue.
- An elevation revoked early does not end runs already started under it: they still end at the elevation's end.
- Not recalled: env and file secrets already handed to the sandbox (they go with it) and anything a person copied out of the run.

### Where the proxy config lives while kept

No container holds the proxy's rendered configuration at rest: its per-run TLS-MITM CA private key, its run token and any authenticated upstream-proxy URL.

- On Docker the proxy reads the configuration from its stdin once at start, so neither the container's config nor its environment carries it, and the proxy is removed when the run is kept.
- Kept for a revive instead is one database row per run (`run_proxy_configs`), sealed with AES-256-GCM under the `wardyn-run-config-key` boot key.
- The secret store holds that key under its own key-encryption key like every boot key (a `-rewrap` or `-rotate-age-key` moves it with the rest).
- The row is deleted when the run goes terminal, with a purge at boot and on the orphan sweep's cadence behind it (threat model residual 59).
- The kept agent container still holds whatever the agent wrote to its own writable layer for the whole grace window. Nothing on the run's page or in the admin runs list reports how much that is today.

- A run with no stored proxy config (one started before the row existed) can't be revived: `409`, start a new run. It runs, ends and is torn down as before.
- `WARDYN_PROXY_IMAGE` must name a proxy image from the same release as wardynd: an older proxy doesn't read its config from stdin and exits at start.

## Revive

`POST /runs/{id}/revive` gives a run a NEW proxy sidecar, built from the run's stored proxy config (never from a container) with a fresh run token and the same per-run MITM CA, under the OWNER's current governance-profile denies.

| Kind | What it does |
| --- | --- |
| Proxy-only revive | For a run lost to a control-plane outage — a new proxy, without touching a running agent |
| Reboot revive | For a run lost to a reboot — also restarts the kept agent container behind the new proxy, so Claude Code can continue its conversation |

- Authority is always the owner's: an admin's click re-asserts the owner's own ceiling, never the caller's, so revive can't hand a member's run limits it doesn't itself hold.
- Nothing scopes WHO may click it beyond "any super admin," over any run in the deployment (see the threat model).

Revive is refused, nothing changed, when:

- The run's captured profile no longer exists, or now denies a host its git broker or its Azure CLI needs. A live run that is refused keeps the access it started with until it ends.
- The model credential its proxy would inject has been erased, or its provider disabled.
- The run is past its end and not yet extended — extend it first.
- The run is already kept by its own end, and either it's a task run (not interactive) or it's past its files grace — the agent can't be started again; start a new run instead.

A live run, one lost to an `outage` or a `reboot`, or a kept run that's interactive, extended past its own end and still inside its files grace, is revivable (`reviveEligible`, [`internal/api/run_revive.go`](../../internal/api/run_revive.go)).

- Admins get the same path in bulk over LIVE runs only: `POST /admin/runs/restart` ("Restart with current limits") and `GET /admin/runs/proxy-window` (listing runs on an out-of-window proxy release).
- A revived run keeps its ORIGINAL agent image: a reboot revive restarts the same, already-created agent container rather than recreating it from an agent image an operator may have since patched.
- The only way onto a newer image is to end the run and start a new one.

### Policy readback

Each proxy start, at dispatch or revive, is recorded as pending before the proxy starts. The record holds the run's policy generation and the start's fresh run token.

1. The driver answers with the SHA-256 of the exact config bytes it handed the proxy.
2. The proxy posts the SHA-256 of the bytes it loaded to `POST /internal/proxy/loaded`, on its run token, and retries until it gets a 2xx.
3. A report that arrives before the driver answers is held. The start is `enforced` only when both hashes are in and equal (`run.policy.enforced`).

The proxy is stopped (`run.policy.unconfirmed`, outcome `failure`) when:

- the driver answers without a hash (`hash_missing`), at once;
- the two hashes differ (`hash_mismatch`);
- no matching report arrives within 60 seconds of the driver's answer (`deadline`).

A create that is still running has no readback deadline. A dispatch whose start fails fails the run. A revive's failed start puts the run back to lost.

A failed start records whether its proxy stop was confirmed. Until it is, the start is unresolved, and the reconcile sweep retries the stop. The retry is for that start only: once a newer start exists, it stops nothing. The stop runs under the run's operation lock, so a revive cannot start a newer proxy between the check and the stop. While another operation holds that lock, the start waits for the next sweep.

A report counts only for the run's newest start. Its token must be that start's own token, or one renewed from it since the start: the run's current or previous token. Any other report is ignored and audited as `run.policy.report_stale`.

A start is `legacy` only when a runner's registered version, or the proxy image tag, names a release older than 0.10.0-rc.18. A legacy start runs as before, unconfirmed (`run.policy.unconfirmed`, reason `legacy`). An empty or unparsable version is not legacy. A revive that must confirm, on a substrate that cannot answer the hash, is refused before anything changes (`409 revive_readback_unsupported`).

A proxy image named by digest alone, untagged or `:latest` names no release, so it is read as current; a tag with a digest names its tag's release. On a runner, the runner's registered version decides, whatever its `--proxy-image`. A proxy from before 0.10.0-rc.18 read as current never reports, so every run on it fails at the deadline (reason `deadline`). The failure hint names this as one possible cause.

Move `WARDYN_PROXY_IMAGE`, and any runner's `--proxy-image` pin, in the same step as the server.

`GET /runs/{id}` shows the newest start as `policy_readback`: its `state`, `generation` and `confirmed_at`, without either hash. The address the agent reaches its proxy on is stored with the run for the audit and is not on the run's JSON.

### Live egress edit

`PATCH /runs/{id}/egress` narrows a live run's egress at once, by the run's owner or a super admin, with the owner's authority as for revive. The body is `{"remove_hosts": [...], "deny_hosts": [...]}`.

| Field | What it does |
| --- | --- |
| `remove_hosts` | Each entry must be on the run's allowlist. It leaves the allowlist and is denied for the rest of the run, so a later approval cannot reopen it |
| `deny_hosts` | Each host is denied for the rest of the run |

The edit never adds a host: any other field is a `400`. Adding a host to a running run stays an approval.

1. The edit is refused, nothing changed, when the run is not live (`409 egress_edit_run_not_live`), another edit of it has not answered (`409 egress_edit_in_progress`), or a remove names a host not on the allowlist (`422 egress_edit_host_not_listed`).
2. It is also refused when the new proxy could not confirm what it loaded: a deployment or substrate without the readback (`409 egress_edit_unsupported`), or a runner or proxy image older than 0.10.0-rc.18 (`409 egress_edit_legacy_proxy`).
3. Otherwise the run's proxy is replaced through the revive path, rate-bounded as a live revive is. Each edited host is withheld as a profile deny is: its credential lanes go, and a host the git broker, Azure CLI or kubectl door needs refuses the edit.
4. Its `via_org` routes go too. The edited config is stored with the edit's policy generation, in one write before the new proxy starts, and the organisation's cached engine for the run is retired. The answer then waits for the new proxy's policy readback: `200 {"policy_generation": N}` once that start is `enforced`.
5. Any other end stops the new proxy, and the run is lost until revived. The answer is `503 egress_edit_unconfirmed` once that stop is confirmed. A report from the pre-edit proxy is stale for the new start and never confirms the edit.
6. While that stop is not confirmed the answer is `503 egress_edit_containment_unresolved`, and a sweep retries the stop. The one case nothing retries: both writes of the start's answer were refused, so it stays pending with no deadline and the answer names `readback_wait_timeout`. An operator must stop or end that run.
7. A headless run that is not on your own machine cannot be kept, so it is ended instead of lost.
8. When another revive replaced the edit's start, nothing is stopped for the edit, and the answer is `503 egress_edit_superseded`. That revive started from the edited config, which the edit stored before its replace.

An edit changes nothing, and the run keeps its current proxy (`502 revive_proxy_kept_current`), when its config cannot be stored, when the run changed since the edit read its policy generation, or when it lost the run's lock before its replace. A revive's claim and its later lost marks land only while its generation is the newest. The stops after a mark lands, and a kept run's stop, are not fenced.

Each edit past the body check writes one `run.egress.edit` row. Open connections drop when the proxy is replaced.

## Rename

`PATCH /runs/{id}/title` lets the run's OWNER change its title. It's a display field, not part of the lease above, so it needs no captured limit and is allowed in ANY run state, including a terminal or kept one.

- The gate is OWNER ONLY (`ownsRun`), stricter than every other `/runs/{id}` route: neither an admin nor a `security_admin` may rename someone else's run — both get the byte-identical 404 a non-owner gets.
- A rename carries no security or incident-response warrant the way `GET /runs/{id}`'s inspect-or-stop bypass or the lease PATCH's owner-or-SUPER bypass do.
- The console's New Run screen prefills the title from the task's own first line (up to 80 characters, cut at a word boundary) and leaves it editable; the server itself never required one.

## Kubernetes cannot keep, revive, restart or pause a run

The Kubernetes substrate doesn't implement the optional runner capabilities the Docker driver does. An agent pod pins its proxy pod's IP, so there's no "same address, new container" to replace, and a stopped pod is gone, not kept.

- A Kubernetes run's end and limits still fire on schedule.
- But ending, losing its sandbox, or being idle all degrade to an immediate, non-resumable teardown rather than a grace window. The pause half of [the clocks](#the-clocks-that-end-or-freeze-a-run) never applies.
- Its proxy config reaches the proxy pod through a per-run Secret staged into an in-memory volume, not stdin. The run is never kept, so that Secret lives as long as the run and is never re-created from the stored row.
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
- A pool's Interactive idle stop bounds it: its default applies when the policy sets none, and its maximum is never exceeded.
- A member under a positive ceiling already holds that ceiling's maximum by then (next bullet), so for a member only the pool's maximum bites; an administrator gets the pool's default.
- A member's value is clamped to the ceiling's positive maximum. The `-1` that interactive and SSH sessions use becomes that maximum too, so under the shipped default a member's session has an hour of idleness. Admins are not clamped.
- To give members a longer window, raise `auto_stop_after_sec` in the ceiling (the default policy, `defaultPolicy` in the chart). Do not set it to `0`: a ceiling of `0` removes the cap, and it also turns idle stop off for every run that does not set its own value.

**Idle pause.**

- A run is frozen only when its profile sets `pause_idle_after_sec` (at least 630 seconds is used), nothing has happened for that long, its CPU reads quiet, and its substrate can freeze its confinement class (Docker `runc` today).
- A run waiting on a decision is frozen after 15 minutes whatever the profile says.
- The proxy keeps running and the run stays `RUNNING`. Typing into the run, attaching, opening an SSH channel or an in-sandbox UI connection thaws it, as does `POST /runs/{id}/resume`.
- On Kubernetes the runner cannot freeze a pod in place, so the setting is inert there and an idle run is stopped by idle stop.
- The limit is set in a governance profile's run limits; see the [profile table](../OPERATIONS.md#multi-user-who-can-change-what) for how profiles combine.

**The lease.** A run has no `ends_at` on the defaults. A governance profile's run limits set one at create, from `default_end_sec` or `max_end_ahead_sec`; the owner moves it later within the captured maximum (see [Extend](#extend)). For a service with a lifetime cap, `max_end_ahead_sec` is also the renewal unit ([Services](../SERVICES.md)).

**`WARDYN_RUN_MAX_AGE`.** Off by default (unset, `0` or negative). It is an absolute cap on a run's age since creation and is not the lease: a person cannot extend it. See [ENV.md](../ENV.md).
