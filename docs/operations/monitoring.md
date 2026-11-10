> Part of the [Operations](../OPERATIONS.md) split (task pages under `docs/operations/`).

# Monitoring

- `GET /metrics` (admin bearer required, next to the unauthenticated `/healthz`) serves Prometheus text exposition, stdlib-only, no client library.
- `/healthz` stays the liveness/component surface; `/metrics` is the trend surface.
- Audit sinks (`WARDYN_AUDIT_SINKS`, [ENV.md](../ENV.md)) are the event stream for SIEMs — metrics carry no per-run detail.

## Counters and gauges

| Name | What it counts |
| --- | --- |
| Runs by terminal state, approval decisions by outcome, egress denies, credential mints, sandbox launch-latency sum/count | The base counters. Every one only moves on success, so two gauges sit beside them: a dead store and an idle cluster otherwise scrape identically. |
| `wardyn_store_up` | Gauge, 1 when Postgres answers the same bounded ping `/readyz` makes. A ping, not proof of work: a reachable pool can still fail individual queries. |
| `wardyn_runner_up` | Gauge, per replica: 1 when its last probe of the sandbox runner's substrate succeeded, 0 when not; absent with no runner. Not part of `/readyz`; the `substrate_health` row of `/setup/status` says which way it failed. |
| `wardyn_sweep_last_tick_seconds{sweep,result}` | Gauge, Unix seconds. `result="attempt"` is when the sweep last started a tick, `result="success"` when it last finished one without error. A follower reports the leader's ticks. See [Substrate and sweep health](#substrate-and-sweep-health). |
| `wardyn_audit_partitions_ahead` | Gauge. Months past the current one that already have an audit partition. A write into a month with none fails and waits in the spool; `/setup/status` warns (`audit_partitions`) below 3. See [Audit retention](../OPERATIONS.md#audit-retention-the-attested-partition-drop). |
| `wardyn_audit_spool_lines` | Gauge. Audit events waiting in the local JSONL fallback spool. A value that never returns to 0 means the drain loop isn't working; mid-drain it can read lower than the file's line count. |
| `wardyn_audit_spool_quarantined_total` | Events the store permanently refused, moved aside by the drain. Non-zero means the trail is missing those events even though the spool drained. |
| `wardyn_audit_spool_torn_total` | Spool lines dropped as unparseable, such as a torn tail from an ENOSPC or a partial write. Distinct from a store refusal: these never parsed far enough to be replayed, so they are not in the quarantine count. |
| `wardyn_audit_outcome_refused_total` | Audit events refused before the store because their `outcome` was not `success`, `failure` or `denied`, each also logged at error level with its action. Non-zero is a code defect: report the logged action. |
| `wardyn_audit_sink_drops_total{sink}` | Events an off-box SIEM sink dropped (buffer overflow or retry exhaustion), even though Postgres, the primary, still got the row. Non-zero means SIEM-side loss only. |
| `wardyn_audit_checkpoint_lag_rows` | Gauge. Audit rows appended since the last signed checkpoint, as of this replica's last signer tick. Process-local: only the replica that signs a tick updates it, so read it from every replica. Staying high means signing stopped or fell behind. |
| `wardyn_audit_checkpoint_last_timestamp_seconds` | Gauge, Unix seconds this replica last wrote a signed checkpoint; 0 if it never did. Process-local. |
| `wardyn_audit_checkpoint_failures_total{reason}` | Counter by `key_unavailable`, `chain_break`, `foreign_row`, `write`. `chain_break` and `foreign_row` stop signing until restart, except a late retired-key checkpoint: it counts, and signing continues from a new genesis ([details](../OPERATIONS.md#signed-checkpoints-and-receipts)). Process-local. |
| `wardyn_audit_delivery_lag{destination}` | Gauge. Rows not yet acknowledged by the collector (head seq minus acknowledged seq, never below 0). Only with `"delivery": "acknowledged"` on the webhook sink. Rising with a healthy collector means the loop stopped: check `wardyn_audit_delivery_halted` and the leader's log. |
| `wardyn_audit_delivery_oldest_undelivered_age_seconds{destination}` | Gauge. Age of the oldest audit row the collector has not acknowledged; 0 when caught up. |
| `wardyn_audit_delivery_last_ack_timestamp_seconds{destination}` | Gauge, Unix seconds of the collector's last acceptance (HTTP 2xx); 0 if it never accepted. |
| `wardyn_audit_delivery_halted{destination}` | Gauge. 1 when the collector answered a terminal 4xx (not 408 or 429) and delivery is halted until wardynd restarts or another replica takes over delivery. The checkpoint's `last_error` holds the status, never the token. |
| `wardyn_audit_delivery_resets_total{destination}` | Counter. Times retention or a table reset removed the row at the checkpoint and delivery resent from the oldest retained row. Each one is also an `audit.delivery.reset` audit row. Non-zero means the collector may have missed events; see [Acknowledged delivery](../ENV.md#acknowledged-delivery-delivery-acknowledged). |
| `wardyn_approval_notify_failed_total{channel}` | Approval notifications dead on a channel: five failed sends, a non-retryable response, or still unsent an hour after due. Each also writes an `approval.notify.failed` audit row. Present only while `WARDYN_APPROVAL_NOTIFY` is set; per replica. |
| `wardyn_approval_notify_suppressed_total{channel}` | Approval raises that created no notification because the run already had 25 tier-0 outbox rows in the last hour. The approval itself still exists and shows in the console. Only present while `WARDYN_APPROVAL_NOTIFY` is set; per replica. See [Approval notifications](../OPERATIONS.md#approval-notifications). |
| `wardyn_runs_active{state}`, `wardyn_runs_unschedulable`, `wardyn_runs_cpu_millis_held{runner}`, `wardyn_runs_memory_mib_held{runner}`, `wardyn_runs_oldest_active_seconds` | Gauges. The fleet from a 15-second snapshot per replica, with no owner label. Aggregate with `max()` across replicas, never `sum()`. A scrape whose snapshot cannot refresh omits them, never writes zero (read `wardyn_store_up`). |
| `wardyn_drive_refusals_total{reason}` | Runs refused their user drive, by reason. A signal for the drive-claim allocator, not the audit pipeline. |
| `wardyn_drive_probe_errors_total` | Counter. Docker `host_path` drive agent-readability probes that could not run at all. The probe fails open: the caller falls back to "cannot tell" rather than refusing (`WARDYN_DRIVE_PROBE_IMAGE`, [ENV.md](../ENV.md)). |
| `wardyn_runner_pool_queue_depth{pool_id}`, `wardyn_runner_pool_slots_used{pool_id}` | Gauges. Runs waiting in each runner pool's queue and runs holding a slot there, from a 15-second snapshot; aggregate with `max()` across replicas, never `sum()`. The label is the pool's id, never its name or an owner. |
| `wardyn_runner_pool_queue_wait_seconds{outcome}` | Histogram. How long runs waited in a pool's queue, by how they left it: `started`, `timeout`, `cancelled` or `refused`. |
| `wardyn_run_start_wait_seconds{reason}` | Summary (sum and count). Time a sandbox still being created spent waiting on each substrate reason, such as pulling an image, waiting for a node, or a reference that will not pull. See [Start deadlines](../OPERATIONS.md#the-start-deadlines). |
| `wardyn_sso_refresh_total{outcome}` | Control-plane AWS SSO `CreateToken` renewal attempts (`awssso_refresh.go`), by `success`, `spent`, `transport_error` or `unavailable`. It graphs the same distinction the `harness.credential.refresh` audit row's `spent` field and expiry check make. |
| `wardyn_credential_reauth_total{outcome}` | Counter. Mid-run model-credential re-auth workflows, by outcome: `requested`, `resolved`, `expired`, `cancelled` or `timeout`. The timeout series is the one to watch for a sign-in nobody answered; the egress deny counter does not count it. |
| `wardyn_credential_reauth_wait_seconds` | Summary (sum and count). How long a re-auth request stayed open, from the raise to the sign-in that answered it. |
| `wardyn_groundtruth_observed_total`, `wardyn_groundtruth_dropped_total`, `wardyn_groundtruth_dropped_unmapped_total`, `wardyn_groundtruth_observed_by_kind_total{kind=…}` | The eBPF ground-truth sensor's cumulative counts. They are only here: `/healthz` publishes the verdict only (`state`, `last_heartbeat`, `reason`, `missing_kinds`), and the reason sentence still names the unmapped-drop count. Omitted entirely when no sensor has ever beaten. |
| `wardyn_auth_failed_suppressed_total` | Audit rows a rate limiter dropped: `auth.fail` and SSH or ticket refusals. The trail stops describing the volume, so a credential-stuffing run reads quieter than a few typos. Alert on this series, not the audit row count. |
| `wardyn_auth_store_errors_total` | Requests an authentication path couldn't decide because its store read failed and answered `503`. The SSO path records `auth.fail`; every other path records nothing. |
| `wardyn_ssh_connections_refused_total{bound}` | SSH gateway connections closed at a [bound](../SSH.md#bounds): `handshake`, `source`, `established` or `principal`; per replica. |
| `wardyn_device_ingest_failures_suppressed_total` | Counter. Refused device audit pushes whose `device.audit.ingest` row was folded into a streak or dropped by the per-device rate limit. |
| `wardyn_model_tokens_total{vendor,direction}` | Model tokens counted from providers' answers. `vendor`: `anthropic`, `openai`, `bedrock`, `ollama` or `other`. `direction`: `input`, `output`, `cache_read`, `cache_write` or `reasoning`. No run label. |
| `wardyn_org_federation_lag` | Gauge, on a hybrid laptop only. Local audit rows the organisation has not yet acknowledged. See [Managed laptops](hybrid-laptops.md). |

- The `wardyn_runner_up` probe is one read-only call (Kubernetes: a namespaced pod list of limit 1, which the chart's Role already grants; Docker: a daemon ping), cached for 5 seconds and bounded by a 3-second deadline.
- The held figures are summed per runner kind, never across kinds: the basis is requests for `k8s` and caps for `docker`. They are configured reservations, not observed use.
- `wardyn_sso_refresh_total{outcome="spent"}` increments once per refresh token AWS retires, at the `CreateToken` call that discovers it. Later dispatches on that token short-circuit uncounted, so the series reads how many sessions AWS retired, not how many times people hit a dead one.
- A climbing `transport_error` or `unavailable` series means the SSO-OIDC endpoint itself is in trouble.

## Model token usage

- Each run's page shows input, output and cache-read tokens per model; `GET /api/v1/runs/{id}/model-usage` adds cache-write and reasoning. All come from the provider, never estimated. They are readable by the run's owner and admins, for a finished run too.
- Counts are the provider's. Anthropic and Bedrock input excludes cache tokens; OpenAI input includes cached tokens. Read each model's line by its `vendor`; the totals add the fields as reported.
- It is on by default. `WARDYN_MODEL_USAGE=off` stops it ([ENV.md](../ENV.md)). A remote or hybrid laptop runner needs it set on the runner too; until then wardynd drops and does not count that runner's records.

| Not counted | Shown as |
|---|---|
| An encrypted connection to a model host that Wardyn did not open | `opaque_tunnels`: tunnels, not calls, counted whether or not content inspection is on |
| A Chat Completions stream without `stream_options.include_usage` (Wardyn does not rewrite the request) | uncounted, `stream_without_usage` |
| A stream cut before its final usage | partial, `stream_cut` |
| An answer whose `usage` is missing, for example from a gateway that omits it | uncounted, `no_usage` |
| A body over 8 MiB, one that does not parse, or a coding Wardyn cannot decode | uncounted, `body_oversize`, `parse_error`, `undecodable` |
| The meter itself failing | uncounted, `meter_error` |
| Batch APIs, whose results are fetched later as files, and upgraded (WebSocket) sessions | not seen |
| An answer that is not a success | not billed, so not recorded |

## OpenTelemetry logs

- The `otlp` audit sink sends each audit row to an OpenTelemetry collector as an OTLP/HTTP JSON log record; setup and the record shape are in [ENV.md](../ENV.md#opentelemetry-logs-otlp).
- It is best-effort logs, not lossless: a full queue or a batch that fails every retry is dropped and shows in `wardyn_audit_sink_drops_total{sink="otlp"}`. Postgres keeps every row. A 2xx reply that reports rejected records in `partialSuccess` adds that count to the same counter; the accepted records are not resent and the collector's message is not logged.
- The sink exports no traces or metrics. `/metrics` is the Prometheus surface.

## Substrate and sweep health

- `/readyz` answers for the store only, and stays that way. The chart's readinessProbe reads it.
- A substrate fault that pulled every replica out of the Service would take the console down with them, and the console is the one place the fault is visible.
- A broken substrate and a stalled sweep show instead on two gauges and on one `/setup/status` row, `substrate_health`, which an operator sees and a member does not.

| Row state | Cause | Meaning |
| --- | --- | --- |
| `fail` | `runner_unreachable` | The substrate does not answer from this replica: no reply, a transport error, or the probe's deadline |
| `fail` | `runner_auth` | The substrate refused the control plane: an expired or revoked token (unauthorized), or a missing grant such as a deleted RoleBinding (forbidden) |
| `warn` | `sweep_stale` | A sweep this install runs has gone three of its own intervals without a success |

- The row never blocks the console, and its detail names only the classified state and the stale sweeps, never the text of a substrate error.
- Each background sweep records when it last tried and last finished cleanly.
- A tick that returns an error moves `attempt` but not `success`, and a tick that panics leaves `success` where it was.

These are the sweeps, with the interval each ticks at:

| `sweep` | Interval | Runs when |
| --- | --- | --- |
| `idle_reaper` | `WARDYN_AUTOSTOP_INTERVAL`, default 1m | A runner exists and the interval is above 0 |
| `terminal_sandbox` | 5m | A runner exists |
| `approval_expiry` | `WARDYN_APPROVAL_EXPIRY_INTERVAL`, default 10m | The interval is above 0 |
| `run_secret` | 15m | Always, on every replica |
| `credential_expiry` | 24h | Always |
| `recording_retention` | 1h | The recording store can sweep and `WARDYN_RECORDING_RETENTION_DAYS` is above 0 (default 0, off) |
| `run_watcher` | 1m | A runner exists, on every replica |
| `orphaned_build` | 30m | The image builder can sweep orphaned builds |
| `run_output` | 1m | `WARDYN_RUN_OUTPUT_PERSIST` is on (the default): deletes run output past `WARDYN_RUN_OUTPUT_RETENTION_DAYS` and resolves abandoned pending rows. Runs on the sweeper leader; with persistence off the retention delete still runs but is not reported |
| `run_queue` | 10s | Always: ends waiting runs past their wait, fails promoted runs whose start died, and promotes for every pool with waiting runs. Runs on the sweeper leader |
| `aws_sso_renewal` | 1m | Always: renews a live run's captured AWS SSO session inside `WARDYN_AWS_SSO_REFRESH_SKEW` of its expiry. Runs on the sweeper leader |

- Every replica registers every sweep whose condition holds on the install, whichever replica holds the sweeper lock, so a follower notices a leader that stopped ticking.
- A sweep whose condition does not hold has no series and can never be stale.
- A sweep with no success on record warns only once three of its intervals have passed since this process started.

Alert on:

- `wardyn_runner_up == 0` on any replica. Each replica probes for itself, so one replica with a bad token or a network fault shows alone.
- `time() - wardyn_sweep_last_tick_seconds{result="success"}` exceeding the sweep's interval from the table, with margin for one missed tick.
- The console row warns at three intervals. A sweep with no `success` sample has not finished a tick yet, so alert on `absent` only after its first three intervals.

## Per-node sensor health

With the [Kubernetes ground-truth sensor](kubernetes-ground-truth.md) on, each node's sensor sends a `kernel.sensor.ping` that names its node. A node counts as live while its last beat is under 24 hours old.

| Live nodes | `/healthz` `ebpf_groundtruth` |
|---|---|
| None heard from, ever | `unavailable` |
| 0 or 1 | One verdict from the newest beat of any node, as before per-node health existed. No `sensors[]`, and a node that went silent is not shown |
| 2 or more | The worst live node's state (`degraded`, then `idle`, then `partial`, then `healthy`) and `sensors[]` |

- With two or more live nodes, `sensors[]` has one entry per node: `node`, `state` and `last_heartbeat`. A node silent for 24 hours to 7 days is listed as `gone` and left out of the verdict. After 7 days it is not read.
- A signed-in caller gets `sensors[]`, because node names describe your cluster. An anonymous caller gets the verdict, `last_heartbeat`, `reason` and `missing_kinds`, and no node list.
- The counters never reach `/healthz`. With two or more live nodes the `wardyn_groundtruth_*` series on `/metrics` sum the live nodes.
- When the worst state is not `healthy`, `reason` reads like "1 of 2 node sensors degraded", so an anonymous caller sees a node count only then.
- If the node list cannot be read, the failure is logged. If any heartbeat was ever recorded, `/healthz` reports `degraded` with "Node sensor health could not be read", to anonymous callers too. With no heartbeat on record it reports `unavailable`.
- A failed write of a node's row is logged and does not fail the heartbeat.
- With two or more live nodes, a run's recent kernel events are read from the node its newest event came from, and are empty if it has none.

## Why two counters exist beside `wardyn_store_up`

`wardyn_store_up` reads as reachability, not as whether the work succeeds. It scrapes `1` throughout an outage that is 500ing every token-authenticated request.

- `wardyn_auth_failed_suppressed_total` and `wardyn_auth_store_errors_total` cover the authentication path, where a failure otherwise leaves no trace.
- The public path and the internal path (the sandbox's run token and the host sensor's token) feed the same rate limiter and the same suppressed count. A process in a sandbox brute-forcing run tokens shows on this series without being able to flood the append-only log.
- The `auth.fail` row's actor (`wardyn/adminAuth`, `wardyn/internalAuth`, `wardyn/internalAuthGroundtruth` or `wardyn/internalApproval`) tells the two incidents apart.
- Scrape with any Prometheus `authorization` config carrying the admin token.

## Kubernetes: the scrape must clear the NetworkPolicy

- The Helm chart renders a default-deny policy whose only ingress peer is *this namespace*.
- A Prometheus in a `monitoring` namespace is denied before it reaches `/metrics` — a scrape a NetworkPolicy dropped looks exactly like a target that is down.
- `networkPolicy.ingress.from` opens it, but that value replaces the same-namespace default rather than adding to it, so list every peer that must reach wardynd:

```yaml
networkPolicy:
  ingress:
    from:
      - namespaceSelector:
          matchLabels: {kubernetes.io/metadata.name: ingress-nginx}
      - namespaceSelector:
          matchLabels: {kubernetes.io/metadata.name: monitoring}
```

[`deploy/helm/wardyn/ci/all-on-values.yaml`](../../deploy/helm/wardyn/ci/all-on-values.yaml) carries exactly that pair, beside the `prometheus.io/scrape` pod annotation it advertises.

## Scraping without a credential: the dedicated listener

For a scraper that sends no bearer token, such as a stock Prometheus reading `prometheus.io/*` pod annotations, set `WARDYN_METRICS_LISTEN` (flag `-metrics-listen`, e.g. `:9464`; [ENV.md](../ENV.md)). It is off by default.

- **What it serves:** `GET /metrics` in plain HTTP with no credential, the same body the gated route answers. Every other method and path is a `404`.
- **Where it may bind:** boot refuses an address equal to `WARDYN_LISTEN`, `WARDYN_INTERNAL_LISTEN`, `WARDYN_SSH_LISTEN` or `WARDYN_UI_SANDBOX_LISTEN`. A bind failure, including an occupied port or malformed address, also refuses boot before background workers and optional gateways start.
- **The console route:** `/metrics` on the console port keeps its operator gate either way, and the chart's ServiceMonitor keeps using it.
- **What a reader learns:** fleet and capacity counts, approval decisions, credential-mint and sign-in-renewal outcomes, and your approval-notification channel ids as labels. No label carries a person, a run id or a secret.
- [`threatmodel/THREAT-MODEL.md`](../../threatmodel/THREAT-MODEL.md) ("The unauthenticated metrics listener") lists every label and its source.

On Kubernetes, set `metrics.listener.enabled=true` in the Helm chart (default port `9464`). The chart then renders:

- a container port named `metrics` that is not on the Service;
- the `prometheus.io/scrape`, `prometheus.io/port` and `prometheus.io/path` pod annotations, where an operator's own `podAnnotations` key wins;
- its own NetworkPolicy ingress rule, admitting pods in the release namespace that are not run pods. `metrics.listener.from` replaces that peer, which a Prometheus in another namespace needs:

```yaml
metrics:
  listener:
    enabled: true
    from:
      - namespaceSelector:
          matchLabels: {kubernetes.io/metadata.name: monitoring}
        podSelector:
          matchLabels: {app.kubernetes.io/name: prometheus}
```

> [!WARNING]
> With `networkPolicy.enabled=false` there is no rule at all, and the port is open to the whole pod network.

Process memory protections (no core dumps, no attaching) are covered in [the boundary page](boundary.md#no-core-dumps-no-attaching).
