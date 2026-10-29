> Part of the [Operations](../OPERATIONS.md) split (task pages under `docs/operations/`).

# High availability: what a second replica still hits

- High availability is supported on Kubernetes with the control plane's own Kubernetes runner: [turn it on](../OPERATIONS.md#turning-it-on).
- **With registered runners, keep one replica.** A registered runner (an organisation runner or a laptop) holds its session with one replica, and the others read it offline.
- This page lists every known blocker for a second replica, its state, and what to do until it is closed.

## Blockers

| Id | Blocker | State | Meanwhile |
|---|---|---|---|
| B1 | A registered runner holds its session with one replica. A launch on another replica is refused `runner_offline`, or `runner_pool_no_eligible_member` in an organisation pool. Attach, exec, SSH, app gateway, desktop, folders and revive need the holding replica. | Open. Setup status warns with the row "Runners with several replicas" (`registered_runners_ha`) while `WARDYN_HA` is set and runners are on. | Keep one replica while you use runners. Runs on the control plane's own Kubernetes runner are not affected. |
| B2 | A kill, end or stop_proxy served by a replica that does not hold the runner's session is queued for the runner. | Shortened in rc.8. The replica that holds the session now ends it when another queues an action, so the runner reconnects and reads the action at once. A lost notice costs the old wait, until the runner next reconnects. | The kill still revokes the run's credentials at once. Until the runner reads the action, the agent keeps the egress it has cached. |
| B3 | High availability needs the control plane's own runner to be Kubernetes, even when every run goes to organisation runners. | By design. The Docker driver keeps per-process maps, and boot refuses `WARDYN_HA` with any other runner. | Use `k8s.enabled`. |
| B4 | A registered runner's completion watcher is claimed by whichever replica sweeps first, and only the replica that holds the session can probe. | Open, tracked in #3671. | Keep one replica, as for B1. |
| B5 | Per-replica caps multiply by the replica count: SSH and UI connections, rate limits, the attach-session cap, `via_org` bytes and runner-stream handshakes. | By design, documented. | Size each cap times the replicas: [What stays per replica](../OPERATIONS.md#what-stays-per-replica). |
| B6 | The audit spool is on the pod's `emptyDir`. A node failure loses what that pod had spooled and not yet drained. | Residual, documented. | Keep audit in Postgres. The spool is only the fallback for a failed write. |
| B7 | Upgrades use `Recreate`. Every replica stops before the new ones start. | By design: an upgrade is not zero-downtime. A mixed-version window is tracked in #3467. | Plan an upgrade as downtime. |
| B8 | Run event ids count per replica, so a `Last-Event-ID` does not resume a stream on another replica. | Documented in [`internal/api/run_events.go`](../../internal/api/run_events.go), and closed as a docs fix in #2793. | Do not resume a stream across replicas. |
| B9 | On a Postgres failover, advisory locks can overlap briefly, and a credential refresh in flight can be lost. | Residual, documented. | The person signs in again: [Residual risks](../OPERATIONS.md#residual-risks). |
| B10 | A few small states are per replica: the runner-event refusal window, the live-revive rate bound and the Azure DevOps Entra access cache. | Minor, tracked in #3661 and #3663. | Nothing to do: each is a per-replica bound or cache. |
| B11 | `wardyn-runnerd` is one replica, held by its state lock. | By design. Runner redundancy is a second runner in the pool. | [Organisation runners in a Kubernetes cluster](k8s-runners.md). |
