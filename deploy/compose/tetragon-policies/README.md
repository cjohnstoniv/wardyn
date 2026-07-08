# Tetragon TracingPolicies for the Wardyn eBPF ground-truth stream

These policies are loaded by the `tetragon` host sensor (see the compose
`tetragon` service) and shape what kernel events Tetragon exports as JSON. The
`wardyn-tetragon-ingest` sidecar tails that JSON export, maps a bounded subset
to Wardyn audit events, and POSTs them to the control plane — the SECOND of
Wardyn's three audit streams (the others being the Postgres self-report log and
the PTY replay).

## What each policy reports

- `wardyn-groundtruth.yaml`
  - `tcp_connect` kprobe → `kernel.network.connect`
  - `security_file_permission` (write mask) kprobe → `kernel.file.write`
    (further narrowed to credential/security paths by the ingest sidecar's
    in-process sensitive-path allowlist).
  - `process_exec` needs no policy: Tetragon's base sensor emits exec/exit
    unconditionally, so `kernel.process.exec` (and the `data.loader` dynamic-
    linker flag derived from it) works with the base sensor alone.

## This is DETECTION, not prevention

These policies contain **no enforcement action** (`Sigkill` / `Override`). They
observe and export; they never block. Kernel `execve` blocking has a documented
`ld-linux`/`mmap` bypass (the "Ona Veto" lesson), so Wardyn FLAGS loader exec
(`data.loader=true`) rather than claiming to prevent it. The real boundary is
structural — L0 no-route egress + no-resident-credentials — enforced out of band
by `wardyn-proxy` and the broker.

## Honest degradation

On a host WITHOUT this sensor running (no Tetragon, no ingest sidecar), the
control plane's `/healthz` reports `ebpf_groundtruth.state = "unavailable"`. The
state is driven by the most recent `kernel.sensor.heartbeat` within a TTL, so it
reads `healthy` ONLY while the sensor is actually delivering events — the
overclaim ("we have eBPF ground truth") is structurally impossible.

## Host eBPF is blind inside CC3/Kata guests

A host eBPF sensor cannot see syscalls inside a CC3 (Kata microVM) guest. For
such runs the ingest sidecar emits a one-time `kernel.sensor.blind` event
(`data.reason="cc3-kata-host-ebpf-blind"`) so the coverage gap is VISIBLE in the
audit stream, never a silent absence. The mitigation path is an in-guest sensor
(a published gap, see `threatmodel/THREAT-MODEL.md`).
