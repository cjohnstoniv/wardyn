# Tetragon TracingPolicies for the Wardyn eBPF ground-truth stream

These policies are loaded by the `tetragon` host sensor and shape what kernel
events it exports as JSON. What the stream is, how it degrades, and what it
cannot see: [`../README.md`](../README.md) → "eBPF ground-truth tier".

## What each policy reports

- `wardyn-groundtruth.yaml`
  - `tcp_connect` kprobe → `kernel.network.connect`
  - `security_file_permission` (write mask) kprobe → `kernel.file.write`
    (further narrowed to credential/security paths by the ingest sidecar's
    in-process sensitive-path allowlist).
  - `process_exec` needs no policy: Tetragon's base sensor emits exec/exit
    unconditionally, so `kernel.process.exec` (and the `data.loader` dynamic-
    linker flag derived from it) works with the base sensor alone.
