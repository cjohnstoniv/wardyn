> Part of the [Operations](../OPERATIONS.md) split (task pages under `docs/operations/`).

# High availability: what a second replica still hits

- High availability is supported on Kubernetes with the control plane's own Kubernetes runner: [turn it on](../OPERATIONS.md#turning-it-on).
- **With registered runners, keep one replica** ([what that means](#running-one-replica)). A registered runner (an organisation runner or a laptop) holds its session with one replica, and the others read it offline.
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

## Running one replica

- One replica is the chart's default: `replicas: 1` ([`values.yaml`](../../deploy/helm/wardyn/values.yaml)), and the chart refuses more unless `ha.enabled` is set.
- Without `WARDYN_HA`, a second `wardynd` exits at boot, because the first holds the database's single-instance lock ([`cmd/wardynd/single_instance.go`](../../cmd/wardynd/single_instance.go)).
- One replica has no standby. While its pod restarts or is rescheduled, the console, the API and new launches are down.

### What a restart or an upgrade does

| Event | What happens | Code |
|---|---|---|
| Upgrade | The old pod stops before the new one starts, so an upgrade is downtime (B7). Kubernetes waits up to 85 seconds for the old pod to stop | [`deployment.yaml`](../../deploy/helm/wardyn/templates/deployment.yaml) `strategy: Recreate` and `terminationGracePeriodSeconds` |
| Start | `/healthz` answers only after wardynd's 30-second database connect and its migration, which `WARDYN_MIGRATE_TIMEOUT` bounds at 5 minutes by default. The startup probe is sized to outlast both | The `startupProbe` block in the same file |
| A run live across the restart | Output the new process does not hold is read back from the substrate when that is possible and safe. Otherwise a `capture_gap` row is written | [`internal/api/run_output_final.go`](../../internal/api/run_output_final.go) `prepareRunOutput` |
| A registered runner | Its session drops. The runner retries with a delay that starts at 1 to 2 seconds, grows, and stops growing at 60 seconds | [`internal/runnerwire/client.go`](../../internal/runnerwire/client.go) `Client.Run` |
| Audit rows while Postgres is unreachable | They go to a fallback spool. It is on the pod's `/tmp` volume by default, which survives a container restart but not a reschedule. With `persistence.enabled` it is on the data volume (B6) | The `WARDYN_AUDIT_SPOOL` block in the same `deployment.yaml` |

### What the health endpoints report

| Endpoint | Probe | Reports | Fails when |
|---|---|---|---|
| `/healthz` | Liveness and startup. Liveness checks every 10 seconds and restarts the container after 3 failures | `status: ok`, the version, the sign-in options, the runner, the confinement classes and the eBPF sensor's verdict. A signed-in caller also gets the substrates and the components. An unreadable Postgres only leaves out the sign-in help text | The process does not answer: it is down, or still starting ([`healthz.go`](../../internal/api/healthz.go)) |
| `/readyz` | Readiness, every 10 seconds, not ready after 3 failures | A Postgres ping with a 3-second limit. `200` with `{"status":"ok"}`, or `503` with `{"status":"error","postgres":"unreachable"}` | Postgres does not answer. The pod leaves the Service, so with one replica the Service has no ready pod ([`security_headers.go`](../../internal/api/security_headers.go) `handleReadyz`) |

- `/readyz` does not cover the sandbox runner. A runner fault shows on `wardyn_runner_up` and the `substrate_health` row of `/setup/status` instead ([Monitoring](monitoring.md#substrate-and-sweep-health)).
- A Postgres outage does not restart the pod, because liveness reads `/healthz`.

### What to watch

| Signal | Reads as |
|---|---|
| Pod restarts, and the pod's Ready state | A restart is the downtime in the first table. Not ready means `/readyz` is failing |
| `wardyn_store_up` | 0 means Postgres did not answer the same ping `/readyz` makes |
| `wardyn_runner_up` and the `substrate_health` row | The control plane's own runner stopped answering, which `/readyz` does not show |
| `wardyn_sweep_last_tick_seconds` | A background sweep that stopped ticking |
| `wardyn_audit_spool_lines` | A value that never returns to 0 means the spool is not draining |
| A registered runner's log line `runner still offline, retrying` | The runner has no session to the control plane. It repeats every minute until it reconnects |

- The metric names and their meanings are in [Monitoring](monitoring.md#counters-and-gauges).
- Recovery from a lost database is in [Recovery rehearsal](recovery-rehearsal.md).
