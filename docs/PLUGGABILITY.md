# Wardyn Pluggable Components

Wardyn's thesis is that **identity, controls, and audit are the product; the
substrate is a pluggable commodity.** In practice that means every major
subsystem sits behind an interface (a *seam*), Wardyn ships a blessed default it
tests with, and a competing component can be swapped in behind the same seam —
held to the same conformance contract.

This document is the standard: how a seam works, how to add an implementation,
and the blessed-default-vs-alternate matrix per subsystem.

> **Honesty rule (read this first).** The *shipped out-of-box default* and the
> *recommended production default* are deliberately kept as **two separate
> columns** below, and for every seam `/healthz.components` covers — exactly
> `identity`, `secret_store`, `recording`, `policy_engine`, and `sandbox` — it
> reports the **actual running** impl in `components.<seam>.selected` with
> `recommended_production` beside it (the other matrix rows surface elsewhere;
> see §2 step 5). A row that
> recommends, say, SPIRE does **not** mean the running binary is SPIRE — it means
> SPIRE is the standard Wardyn recommends converging to, and the seam + a
> conformance suite are ready to hold an implementation to contract when it
> lands. Until then, `selected` is what you are running, and `/healthz` says so.

---

## 1. The standard

A pluggable seam in Wardyn has five parts. Four seams implement all of parts
1–4 today — identity, secret store, recording, and the confinement substrate
(`internal/runner/substrate/registry.go`; its `docker` impl self-registers under
`-tags docker`) — the rest carry the honest per-row "Seam status" in §3:

1. **An interface** (`identity.Provider`, `secretstore.Store`, `recording.Store`,
   `runner.Runner`, `audit.Sink`, …) — the contract the control plane talks to.
   Most carry a `Name()` method so the running impl is self-describing.
2. **A registry** built on the shared `internal/component.Registry[C]`: a
   name→constructor map with default resolution and duplicate-name detection
   (one tested implementation, reused by every seam).
3. **A typed `Deps` struct** per seam — the platform primitives a constructor may
   use (a pool, a signing key, a directory) plus an `Options map[string]string`
   from `WARDYN_<SEAM>_*` env, so an alternate reads impl-specific config without
   changing `Deps`.
4. **Self-registration**: each implementation calls `Register(name, ctor)` from an
   `init()`, so a blank import (`_ "…/secretstore/pg"`) makes it selectable. The
   registered default name maps to the current built-in, so an unset selector
   reproduces today's behavior exactly.
5. **A conformance suite** (`<seam>test.RunConformance(t, factory)`) that ANY
   implementation — the blessed default and every alternate — must pass.

**The blessed-vs-registered distinction is load-bearing:** registration makes an
impl *selectable*; it is only **blessed** once it passes its seam's
`RunConformance` suite. That is what makes a "recommended production default" a
falsifiable engineering promise rather than a marketing claim.

**Selection convention:** every seam selects via a `WARDYN_<SEAM>` env var (CLI flag
default < env var < explicit flag, via the `flagEnv` helper). Defaults reproduce
today's behavior; no flag flip changes runtime behavior on its own.

**Security invariants stay above every seam.** A pluggable policy engine, egress
gateway, or confinement substrate may **never** weaken Wardyn's non-negotiables:
default-deny + the unconditional private-IP guard, first-use approval, proxy-side
credential injection, L0 gatewayless egress, and fail-closed confinement gating.
These run in the control plane / proxy *around* the seam, not inside it.

---

## 2. How to add a component

1. Implement the seam interface.
2. `Register("<name>", <constructor>)` from your package's `init()`.
3. Make it pass `<seam>test.RunConformance` (a 3-line `_test.go` calling the suite).
4. Add a row to the matrix below.
5. For the registry-backed seams (identity, secret store, recording, confinement
   substrate) it now appears in `/healthz.components.<seam>.available`
   automatically and is selectable via `WARDYN_<SEAM>=<name>` (the substrate
   selects via `-runner`/`WARDYN_RUNNER`; a build-tag-gated impl like the OCI
   substrate registers — and thus appears — only in builds that compile it, so a
   tagless binary honestly advertises `sandbox.available: []` and fails closed
   on `-runner docker`). Seams **without** a registry are wired through their own
   config knobs and surface elsewhere at runtime, not in `components`: the LLM
   gateway and content detection are builtin (proxy-side), audit sinks via
   `WARDYN_AUDIT_SINKS` (boot log), composer backends via
   `WARDYN_COMPOSER_CONFIG` (`/api/v1/setup/status`), eBPF ground-truth via
   `/healthz.ebpf_groundtruth`, and per-class substrate runtimes via
   `/healthz.confinement_substrates`.

For the **policy-evaluator**, **egress-gateway**, and **confinement-substrate**
seams, also honor the security rules: the evaluator's verdict cannot relax the
IP guard / approval FSM; a gateway forwarder is invoked only *after* allow +
IP-vetting and its own endpoint must be allowlisted + IP-vetted; a substrate must
preserve L0 (the agent's sole egress is the wardyn-proxy endpoint) and fail closed
when a demanded class cannot be enforced.

---

## 3. Blessed-default-vs-alternate matrix

`Shipped` = the out-of-box default that runs today. `Recommended (prod)` = the
standard Wardyn recommends for hostile-multi-tenant production (may differ from
shipped — see the honesty rule). `Seam status`: **shipped** = the interface +
registry + conformance exist today; **planned** = interface lands on the roadmap.

| Subsystem | Interface | Shipped out-of-box default | Recommended (prod) | Registered alternates | Conformance | Seam status |
|---|---|---|---|---|---|---|
| Sandbox / confinement | `substrate.Substrate` (under the `orchestrator` `runner.Runner`) | `docker`/OCI substrate; CC1 runc / CC2 runsc / CC3 kata\* | **Kata-CC3** (QEMU by default; experimental today, see README Confinement Classes) | OCI runtime pins via `WARDYN_CONFINEMENT_MAP` (kata-qemu, kata-clh, gVisor, sysbox); non-OCI VMM (SmolVM/Firecracker) via a new `Substrate` impl | `test/conformance` (Runner) + orchestrator routing tests | shipped (registry + `init()` self-registration — the `docker` impl registers under `-tags docker`; runtime-pluggable + substrate sub-interface); non-OCI VMM impl planned |
| Identity | `identity.Provider` | `embedded` (SPIFFE-shaped JWT-SVID) | **SPIRE** (attestation, short-lived SVIDs) | `spire` (planned) | `identity/identitytest` | shipped |
| Secret store | `secretstore.Store` | `pg` (age-encrypted Postgres) | **OpenBao** (LF, Vault-compatible) | `openbao` / `vault` / cloud KMS (planned) | `secretstore/secretstoretest` | shipped |
| Recording | `recording.Store` | `fs` | `fs` (object storage optional) | S3/GCS object store (planned) | `recording/recordingtest` | shipped |
| Egress policy | `egress.Evaluator` | `builtin` (RunPolicySpec) | **OPA/Rego** | `opa` / `cedar` (planned) | `egress/evaluatortest` | shipped (seam); evaluator alternates planned |
| LLM gateway | `proxy.Forwarder` | `direct` (pinned-IP RoundTrip) | LiteLLM / Portkey / Envoy AI GW behind wardyn-proxy | external gateway (planned) | — | shipped (seam); external gateway planned |
| Content detection | `contentscan.Detector` | builtin (known-secret / regex / entropy / PII) + sidecar | builtin + **LLM Guard / Presidio** sidecar | `DetectorSidecarURL` (shipped) | — | shipped (detector seam) |
| Audit sinks | `audit.Sink` | none (Postgres recorder always) | OpenTelemetry → SIEM | `file` / `webhook` / `syslog` (shipped) via `WARDYN_AUDIT_SINKS` | — | shipped |
| Composer backends | `composer.Registry` | none (opt-in) | — | `anthropic` / `openai` / `cli` / `fake` via `WARDYN_COMPOSER_CONFIG` | — | shipped |
| eBPF ground-truth | host sensor ingest | none (honest-degraded `/healthz`) | **Tetragon** (enforcement) | Falco / Tracee (ingest-compatible) | — | shipped (ingest seam) |

All recommended-prod candidates are Apache-2.0 / permissive, self-hostable, and
CNCF-graduated/incubating where available (Kata, SPIRE, OPA, Tetragon, Cilium),
or LF-governed (OpenBao). None are built in this effort — each is a documented
row with a seam (or a planned one) and a conformance contract ready to hold it.

---

## 4. The recommended-vs-shipped tension, made honest

Wardyn deliberately lets the *recommended production default* differ from the
*shipped out-of-box default*: we recommend SPIRE / OpenBao / OPA / Kata-CC3 /
Tetragon for hostile-multi-tenant production while the binary ships
embedded / pg / builtin / docker(runc..kata) / heartbeat-only. The risk is a
credibility gap — a matrix that says "recommended: SPIRE" must never be misread
as "you are running SPIRE." Three mechanisms keep it honest:

1. **Two never-merged columns.** "Shipped out-of-box" and "Recommended (prod)"
   are separate above; promoted entries are bold and the alternate is marked
   *planned* until built.
2. **Machine-honest `/healthz`.** For every seam `components` covers (§2 step 5
   lists the exact set and where the other rows surface),
   `components.<seam>.selected` reports the *actual* running impl and
   `components.<seam>.available` what this build's registry actually holds;
   `recommended_production` sits beside them; `source` distinguishes `default`
   from `configured`. The gap is visible at runtime, not
   just in prose — the same structural anti-overclaim guarantee as
   `ebpf_groundtruth` (healthy only while real heartbeats arrive) and runner
   `Capabilities` (a class is advertised only when its runtime is registered).
3. **One shared conformance contract.** A future SPIRE / OpenBao / OPA impl must
   pass the *same* `RunConformance` suite the shipped default passes, so the
   recommendation is a roadmap commitment backed by a live gate. Until an
   alternate is built and green, the shipped default is what you run — and
   `/healthz` will say so.

---

## 5. Where the seams live (code map)

- `internal/component/registry.go` — the shared `Registry[C]`.
- `internal/identity/{registry.go,revocation.go}` + `identity/embedded/register.go` + `identity/identitytest`.
- `internal/secretstore/{registry.go,secretstore.go}` + `secretstore/pg/register.go` + `secretstore/secretstoretest`.
- `internal/recording/registry.go` + `recording/recordingtest`.
- `internal/runner/substrate/{substrate.go,registry.go}` (the `Substrate` sub-interface + `ClassSupport` + the substrate registry) + `internal/runner/docker/register.go` (the OCI substrate's `init()` self-registration, `-tags docker` only) + `internal/runner/orchestrator/` (the build-tag-free `runner.Runner` that multiplexes substrates by Confinement Class, with a durable pg-backed ref→substrate `RefStore` — `internal/store/store_sandbox_ref.go` — so kill-switch routing survives control-plane restarts) + `internal/runner/docker/hardening.go` (`resolveRuntime`, `capabilitiesForWith`) + `cmd/wardynd` `WARDYN_CONFINEMENT_MAP` — the confinement substrate/runtime seam.
- `internal/egress/evaluator.go` (policy evaluator, overridable via `Options.Evaluator` in `internal/egress/proxy/proxy.go`) + `internal/egress/proxy/local_routes.go` (LLM-route forwarding) — the egress seams.
- `internal/runner/runner.go` (`Capabilities.Resolved`) + `internal/api/server.go` (`/healthz` `confinement_substrates` + `components`).
- Planned impls (seams ready, alternates not built): the non-OCI VMM `Substrate` (SmolVM/Firecracker), SPIRE identity, OpenBao secrets, OPA/Cedar evaluator, an external LLM gateway.
