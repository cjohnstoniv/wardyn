> Part of the [Operations](../OPERATIONS.md) split (task pages under `docs/operations/`).

# Monitoring

`GET /metrics` (admin bearer required, next to the unauthenticated `/healthz`)
serves Prometheus text exposition — stdlib-only, no client library. Counters: runs
by terminal state, approval decisions by outcome, egress denies, credential mints;
plus sandbox launch-latency sum/count. Two gauges sit beside them, because every
counter only moves on success — a dead store and an idle cluster otherwise scrape
identically: `wardyn_store_up` (1 when Postgres answers the same bounded ping
`/readyz` makes) and `wardyn_audit_spool_lines` (audit events waiting in the local
JSONL fallback spool — a value that never returns to 0 means the drain loop is not
working; it is the count of events still to replay, which mid-drain can be lower
than the line count of the file on disk). Beside them, `wardyn_audit_spool_quarantined_total` counts events the
store permanently refused and the drain moved aside (see the spool paragraph
above): non-zero means the trail is missing those events even though the spool
drained.

Three more counters cover loss the gauges above cannot see. `wardyn_audit_spool_torn_total`
counts spool lines dropped as unparseable — a torn tail from an ENOSPC or a
partial write, distinct from a store refusal: these never reach the quarantine
count above because they never parsed far enough to be replayed at all.
`wardyn_audit_sink_drops_total{sink}` counts events an off-box SIEM sink
dropped (buffer overflow or retry exhaustion) even though Postgres — the
primary — still got the row; non-zero means SIEM-side loss only, not a gap in
the append-only trail itself. `wardyn_drive_refusals_total{reason}` counts
runs refused their user drive, by reason — a signal for the drive-claim
allocator, not the audit pipeline. `wardyn_sso_refresh_total{outcome}` counts
control-plane AWS SSO `CreateToken` renewal attempts (`awssso_refresh.go`), by
`success` / `spent` / `transport_error` / `unavailable` — the same distinction
the `harness.credential.refresh` audit row's `spent` field and expiry check
already make, graphable without grepping the audit trail. `spent` increments
ONCE per refresh token AWS retires, at the CreateToken call that discovers it
(the `invalid_grant`/`expired_token`/`invalid_client`/`unauthorized_client`
codes); every later dispatch on that same token exits at an earlier,
UNCOUNTED short-circuit (the in-memory dead-mark check, before CreateToken is
ever called again), so the series reads "how many distinct sessions AWS
retired," not "how many times people hit a dead one." A climbing
`transport_error`/`unavailable` series is the SSO-OIDC endpoint itself in
trouble.

The eBPF ground-truth sensor's cumulative counts scrape here too —
`wardyn_groundtruth_observed_total`, `wardyn_groundtruth_dropped_total`,
`wardyn_groundtruth_dropped_unmapped_total` and
`wardyn_groundtruth_observed_by_kind_total{kind=…}` — and only here. They used
to ride the anonymous `/healthz`, which handed the fleet's kernel-event volume
to anyone who could reach the port; `/healthz` now publishes the VERDICT only
(`state`, `last_heartbeat`, `reason`, `missing_kinds`), and the reason sentence
still names the unmapped-drop count for an operator reading a broken
correlation. The series are omitted entirely when no sensor has ever beaten.

Two counters cover the authentication lane, where a failure otherwise leaves no
trace at all. `wardyn_auth_failed_suppressed_total` counts `auth.fail` audit
rows the rate limiter dropped — the trail is capped at roughly one row per
second, so past a small burst it stops describing the volume it is bounding and
**a credential-stuffing run reads quieter than a handful of typos**. Alert on
this series, not on the audit row count: flat rows with this climbing is the
attack. Both the public lane and the INTERNAL lane (the sandbox's run token and
the host sensor's token) feed that one limiter and that one counter, so a
process inside a sandbox brute-forcing run tokens is visible on this series
without being able to flood the append-only log; the `auth.fail` row's actor
(`wardyn/adminAuth` vs `wardyn/internalAuth` / `wardyn/internalAuthGroundtruth`
/ `wardyn/internalApproval`) is what tells the two incidents apart. `wardyn_auth_store_errors_total` counts requests an authentication lane
could not decide because its store read failed and answered `500` — a state with
no audit row (there is no authenticated principal to attribute one to) and no
client-visible cause.

That second counter exists because **`wardyn_store_up` cannot answer for it**.
The gauge is a *ping*: it says the pool is reachable, and a reachable pool still
fails individual queries — one table denying a read, one statement timing out.
So it can scrape `1` throughout an outage that is 500ing every token-authenticated
request, which is worse than no signal, because it argues against the operator's
own evidence. Read `wardyn_store_up` as reachability and the two counters above
as whether the work is actually succeeding. Scrape with any Prometheus
`authorization` config carrying the admin token.

**On Kubernetes the scrape must also be let through the NetworkPolicy.** The
Helm chart renders a default-deny policy whose only ingress peer is *this
namespace*, so a Prometheus in a `monitoring` namespace is denied before it
reaches `/metrics` — and a scrape a NetworkPolicy dropped looks exactly like a
target that is down. `networkPolicy.ingress.from` opens it, but that value
**REPLACES** the same-namespace default rather than adding to it, so list every
peer that must reach wardynd:

```yaml
networkPolicy:
  ingress:
    from:
      - namespaceSelector:
          matchLabels: {kubernetes.io/metadata.name: ingress-nginx}
      - namespaceSelector:
          matchLabels: {kubernetes.io/metadata.name: monitoring}
```

`deploy/helm/wardyn/ci/all-on-values.yaml` carries exactly that pair, beside the
`prometheus.io/scrape` pod annotation it advertises.

`/healthz` stays the liveness/component surface (identity, runner classes,
eBPF ground-truth state); `/metrics` is the trend surface. Audit sinks
(`WARDYN_AUDIT_SINKS`, [ENV.md](../ENV.md)) are the event stream for SIEMs — metrics
carry no per-run detail.

### No core dumps, no attaching

wardynd and wardyn-proxy hold credentials in memory, so each sets `RLIMIT_CORE`
to 0 and marks itself non-dumpable (`PR_SET_DUMPABLE` 0) as its first act. A
crash writes no core file and a `core_pattern` handler receives nothing, and a
process of the same user can no longer `strace -p` or attach delve or gdb to it,
or read its `/proc/<pid>/environ` or `/proc/<pid>/mem`. This is intentional and
not configurable.

