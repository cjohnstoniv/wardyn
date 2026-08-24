# Ground-truth counter: E1 diagnosis (Workstream E, stage E1-diagnose)

Filed symptom (ROADMAP.md:150): *"the ground-truth pipeline's frozen control-plane
counter — tetragon exports, the ingest posts, the tally never moves."*

Diagnosis only. Nothing was fixed in this stage. Every claim below is a live
observation from a compose stack brought up with the `groundtruth` profile on
2026-08-20.

## Repro environment

    COMPOSE_PROJECT_NAME=wardyngt WARDYN_NS=wardyngt \
      docker compose -f deploy/compose/docker-compose.yaml up -d postgres wardynd
    docker compose ... --profile groundtruth up -d tetragon wardyn-tetragon-ingest

Host: WSL2 kernel 6.6.87.2-microsoft-standard-WSL2, Docker Desktop engine
(`node_name: docker-desktop` in every Tetragon event). Tetragon
v1.1.2 @sha256:fca204b0, shipped `deploy/compose/tetragon-policies/wardyn-groundtruth.yaml`.
Two governed runs (`--task-mode exec`, one short-lived, one ~160 s) plus five
synthetic `wardyn.managed=true / wardyn.component=agent / wardyn.run-id=<uuid>`
probe containers.

## Named root cause

**H2 — correlation → gate.** The container→run index is a snapshot of *running*
containers only. Any kernel event whose container has already exited by the time
the ingest maps it resolves `unmapped`, is dropped by `gatedMapper` **before**
`sink.markObserved`, and moves **no counter at all** — not `observed_total`, not
`dropped_total`. A short run therefore produces a heartbeat-only audit stream and
`/healthz` `ebpf_groundtruth.state = "idle", observed_total = 0`, which is the
filed symptom exactly.

The chain, seam by seam:

| seam | file:line | what it does |
|---|---|---|
| index build | `cmd/wardyn-tetragon-ingest/correlator.go:164-198` | `docker ps --no-trunc --filter label=wardyn.managed=true` — **no `-a`**, so an exited container is not in the index |
| index refresh | `correlator.go:114-146`, `main.go:106,150-154` | 15 s ticker + on-miss refresh throttled to 7.5 s |
| resolve | `internal/groundtruth/tetragon.go:270-281` | miss ⇒ `(nil, CorrelationUnmapped)` |
| **the drop** | `correlator.go:260-266` | `gatedMapper.MapLine` returns `ok=false` when `ev.RunID == nil` and `allowUnmapped` is false (the default, W24-S1-1) |
| **the blind spot** | `main.go:325-344` | `processLine` returns on `!ok` **before** `sink.markObserved(ev.Action)` — so a gated event is never counted as observed *or* as dropped |

`sink.dropped` (`sink.go:190-192`) only counts *backpressure* drops on a full
channel. There is no counter anywhere for "mapped a real kernel event, then
refused to forward it".

## Evidence ladder — every hypothesis confirmed or killed

### H1 — auth/drop (every POST 401s; rotator never writes / token expired) — **KILLED**

- Ingest boot log: `token source = file (rotatable) path=/var/run/wardyn-gt/token`.
  No `initial container index refresh failed` warning, no 401 warnings, ever.
- `wardyn-tetragon-ingest: stats posted=455 dropped=0` — POSTs succeed; the
  drop counter is flat at 0 for the whole session.
- Heartbeat rows land in Postgres from the first second
  (`kernel.sensor.heartbeat` max `2026-08-20 04:43:31`), so the ingest is
  authenticated and the control plane is writing.
- `/healthz` reports `idle` / `partial`, never `unavailable` — i.e. beats arrive.

### H2 — correlation → gate — **CONFIRMED (root cause)**

Two independent live proofs.

**(a) A real short run vanishes entirely.** Run `431bf644-e698-4ebd-8069-1541fdaf15a2`
(`--task-mode exec`, agent container alive ≈3 s before the task's `curl` failed
and the container exited):

    SELECT action, count(*), max(time) FROM audit_events WHERE action LIKE 'kernel.%' GROUP BY 1;
     kernel.sensor.heartbeat |     2 | 2026-08-20 04:44:00.827579+00
    (1 row)

    /healthz ebpf_groundtruth: {"state":"idle","observed_total":0,"dropped_total":0,
                                "reason":"no kernel events observed"}

Zero rows carry that run id. Tetragon *did* export its execs (the container id
`78fe9b3f…` appears in the export with `"docker":"78fe9b3f0c58f4e62423a9e172947a0"`).

**(b) A controlled one-shot container, seen by the sensor, dropped without a trace.**
A container labelled `wardyn.managed=true / wardyn.component=agent /
wardyn.run-id=11111111-2222-3333-4444-555555555555` running exactly
`/bin/echo shortlived-probe-marker` and exiting:

    # Tetragon exported it:
    grep -c 'shortlived-probe-marker' /var/log/tetragon/*.log  ->  3 + 2  (5 lines)
    # the ingest tailed past that point (later heartbeats posted, stats printed)
    # the control plane recorded nothing:
    SELECT count(*) FROM audit_events
      WHERE action='kernel.process.exec' AND data->>'argv' LIKE '%shortlived-probe-marker%';
     0
    # and no counter moved:
    /healthz ebpf_groundtruth.dropped_total  = 0   (unchanged)
    /healthz ebpf_groundtruth.observed_total = 423 (unchanged across the probe)
    ingest log                               dropped=0

An event the sensor demonstrably saw, from a correctly-labelled Wardyn agent
container, is invisible in all three counters. That is the defect.

**Contrast — the rest of the pipeline is healthy when the container outlives the lag.**
Run `c94bedba-4a14-4f10-b191-f2332d5d094c` (container alive ≈160 s):

    SELECT run_id, action, count(*) FROM audit_events WHERE action LIKE 'kernel.%' GROUP BY 1,2;
     c94bedba-…  | kernel.process.exec     | 177
                 | kernel.process.exec     | 170   (probe containers, run_id_not_found)
                 | kernel.sensor.heartbeat |  23

    SELECT target, count(*) ... AND run_id='c94bedba-…' AND action='kernel.process.exec';
     /usr/bin/mkdir 43 | /usr/bin/sleep 41 | /bin/echo 40 | /usr/bin/curl 40 | …

All 40 `curl` execs, all 40 `echo`s — correlated, keyed on the run id. So auth,
transport, mapping, correlation and recording all work; the failure is
specifically the running-only index versus the tail's lag.

### H3 — mapper/export shape (`process.docker` unpopulated on this cgroup layout) — **KILLED**

Tetragon v1.1.2 **does** populate `process.docker` here, as a **31-character**
prefix of the container id:

    agent container (docker ps --no-trunc): 78fe9b3f0c58f4e62423a9e172947a09ae9e027d291b2f1a9a772882450e9ed5
    tetragon export:                        "docker":"78fe9b3f0c58f4e62423a9e172947a0"

`dockerCorrelator.lookupContainer`'s prefix fallback (`correlator.go:100-107`,
`strings.HasPrefix(full, id)`) resolves that correctly — proven by run
c94bedba's 177 correlated rows. Not the cause.

Two true-but-secondary observations to settle in E2, *not* root causes:

- `internal/groundtruth/groundtruth.go:146-150` claims the correlator indexes
  "(and, where available, cgroup id)". No cgroup index exists in
  `dockerCorrelator`, **and** Tetragon emits no `cgroup_id` field at all in this
  export (0 occurrences across a 46,467-line sample). Either implement the
  index or delete the clause — the comment is currently false twice over.
- The index keys full ids and 12-char short ids; Tetragon emits 31. Correlation
  survives only via the O(n) prefix scan, which is a fallback the code describes
  as "a last resort" but which is in fact the *only* path that ever hits.

### H4 — sensor not exporting — **KILLED for exec/file; CONFIRMED as a SEPARATE second defect for connect**

Tetragon boots clean (`Added TracingPolicy with success`, both kprobes loaded,
`Listening for events...`) and writes ≈1 MB/s of JSONL. exec events flow.

But **`kernel.network.connect` is never observed, on any run, ever** — `/healthz`
is permanently capped at:

    {"state":"partial","missing_kinds":["kernel.network.connect"],
     "observed_by_kind":{"kernel.process.exec":423},"observed_total":423}

Across two real runs and four synthetic probes (≈120 real TCP connects made from
containers on this daemon — 40 proxied `curl`s from the agent, 16 `wget` +
34 `ssl_client` from one probe, plus loopback connects that returned
`Connection refused`, i.e. the SYN was genuinely sent), the Tetragon export
contains **zero** `tcp_connect` events attributable to any of them.

This is *not* transport loss, and it is not the `security_file_permission` flood:

- Tetragon's own metrics (started with `--metrics-server=:2112`):
  `tetragon_ringbuf_perf_event_lost_total 0`, `tetragon_ringbuf_queue_lost_total 0`,
  `tetragon_ringbuf_queue_received_total 95576`.
- The policy was temporarily reduced to `tcp_connect` + `security_socket_connect`
  only (flood removed, rotation stopped). A fresh probe container making 10 real
  connects still produced **0** connect events while its 10 `wget` / 10
  `ssl_client` **exec** events all landed.

The connect events that *do* appear are host-netns processes and
container-**loopback** connects (`::1:8080`, `127.0.0.1:2381`, own-IP
`172.31.0.2:6443`). Every connect that egresses a container's veth to a bridge is
absent. Read: on this host (WSL2 + Docker Desktop kernel, tetragon v1.1.2) the
`tcp_connect` kprobe does not deliver events for container egress. Environmental,
reproducible from the policy alone, independent of Wardyn's code.

Per the plan's own rule for an environmental root cause, the honest response is a
documented environment gate plus the visibility counter — **not** a forced code
change. `/healthz`'s `missing_kinds` already reports this one truthfully; it is
the one part of the ground-truth surface that is currently honest.

### Secondary finding — export noise and a 50-second retention horizon

Not a cause of either defect above; a real durability risk found on the way.

The shipped `security_file_permission` kprobe is host-wide with only a MAY_WRITE
mask selector, and the sensitive-path allowlist (`internal/groundtruth/sensitive.go`)
runs in the **sidecar, after transport**. Measured over one sampled window:

    44,642 of 46,467 export lines (96%) were security_file_permission
    ≈4,500 events/s; the 10 MB export rotated every ~9 s, keeping 5 backups
    => the entire on-disk ground-truth history is ~50 seconds deep

Removing that kprobe alone took rotation from every ~9 s to none in 3 minutes.
Any ingest stall longer than ~50 s loses ground truth outright, and the tail's
rotation handling re-seeks to offset 0 of whatever `tetragon.log` exists at that
moment — a whole rotated generation can be skipped. Worth a decision in E2/E3
(narrow the kprobe with a path/prefix selector, raise
`--export-file-max-size-mb`/`--export-file-max-backups`, or both).

## Which layer the regression test belongs in

1. **`cmd/wardyn-tetragon-ingest` (package `main`) — the H2 fix and its regression test.**
   This is where all three seams live (the lister, the gate, the counter), and it
   already has the right injection point: the `dockerLister` interface
   (`correlator.go:52-57`) and the existing `correlator_test.go` / `main_test.go`
   fakes. The regression test that must FAIL on the pre-fix commit:
   a lister that reports container X → run R, then stops reporting it (the exit),
   and an event carrying X's id mapped *after* that point must still resolve to R
   (recently-seen cache with a TTL, or `docker ps -a`). A second test asserts a
   gate-dropped event increments a new counter — `processLine` must stop being
   able to discard an event with every counter flat.
2. **`internal/groundtruth/sensor.go` + `internal/api/server.go` — the visibility half.**
   `HeartbeatEventWithDropped` gains the gated-drop count, and
   `ebpfGroundtruthStatus`'s `idle` branch gains a reason that separates "sensor
   saw nothing" from "saw N, correlated none". Unit-testable in
   `internal/api/groundtruth_test.go` alongside the existing state-machine tests.
3. **No Go test for the connect gap.** It is environmental. It belongs in
   `deploy/compose/README.md` + `docs/OPERATIONS.md` as a documented sensor
   ceiling, with `/healthz`'s existing `missing_kinds` as the runtime signal.

## Commands used (for E2 to re-run)

    # splitter
    docker exec wardyngt-postgres psql -U wardyn -d wardyn -c \
      "SELECT action, count(*), max(time) FROM audit_events WHERE action LIKE 'kernel.%' GROUP BY 1;"
    # health
    curl -s -H "Authorization: Bearer demo-admin-token" http://127.0.0.1:18080/healthz \
      | python3 -c "import json,sys;print(json.load(sys.stdin)['ebpf_groundtruth'])"
    # the one-shot drop repro
    docker run --rm --label wardyn.managed=true --label wardyn.component=agent \
      --label wardyn.run-id=<uuid> alpine:3 /bin/echo shortlived-probe-marker
    # tetragon loss metrics (add --metrics-server=:2112 to the tetragon command)
    docker run --rm --network host alpine:3 wget -qO- http://127.0.0.1:2112/metrics \
      | grep -E 'lost|missed|queue'
