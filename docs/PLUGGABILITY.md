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
> reports the **actual running** impl in `components.<seam>.selected`, plus
> `available` and `source` — the recommended-production column lives in the
> matrix below and ROADMAP.md, because a runtime endpoint reports runtime
> facts (see §2 step 5). A row that
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
   `runner.Runner`/`substrate.Substrate`, …) — the contract the control plane talks
   to. Most carry a `Name()` method so the running impl is self-describing. (Some
   extension points — audit sinks, content detection — are interfaces too but are
   wired through config, not a registry; see §3.) One documented quirk, not a
   bug: `orchestrator.Orchestrator` (which implements `runner.Runner` over one
   or more `substrate.Substrate`s) reports the literal string `"orchestrator"`
   from `Name()` — and so from `/healthz`'s `runner` field — only when it
   wraps MORE than one substrate at once; wrapping exactly one (today's only
   real path: `WARDYN_RUNNER`/`-runner` selects a single substrate name, so
   `buildRunnerFromFlags` always constructs the orchestrator with exactly one)
   reports THAT substrate's own name instead, e.g. `docker` or `k8s`. A
   `-tags docker,k8s` build registers both substrates as *selectable*, but
   `WARDYN_RUNNER` still names only one at a time — multi-substrate wiring is
   a capability the orchestrator's constructor has (`New(substrates
   ...substrate.Substrate)`), not a shipped deployment shape, so `"orchestrator"`
   is unreachable via any flag/env combination today.
2. **A registry** built on the shared `internal/component.Registry[C]`: a
   name→constructor map with default resolution and duplicate-name detection
   (one tested implementation, reused by every seam).
3. **A typed `Deps` struct** per seam — the platform primitives a constructor may
   use (a pool, a signing key, a directory). Impl-specific config is NOT carried
   on `Deps`: an implementation reads its own `WARDYN_*` env inside its
   constructor, the way the docker substrate reads `WARDYN_INTERNAL_NETWORK` /
   `WARDYN_RECORDING_MOUNT` in `internal/runner/docker/register.go` — so an
   alternate configures itself without changing `Deps` at all.
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

**Selection convention:** every **registry-backed** seam selects via a
`WARDYN_<SEAM>` env var (CLI flag default < env var < explicit flag, via the
`flagEnv` helper). Defaults reproduce today's behavior; no flag flip changes
runtime behavior on its own.

> **Not every seam is registry-backed, and this rule used to claim they all
> were.** `egress.Evaluator` has a real interface AND a conformance suite, but
> **no registry, no selector env var, and exactly one implementation** — the only
> override is an in-process `Options.Evaluator` field that nothing outside tests
> sets. `/healthz` correspondingly reports `policy_engine: {selected: "builtin"}`
> with no `available` list, which is honest; step 5 below used to imply otherwise.
> An interface plus a conformance suite is a real head start on pluggability. It
> is not the same thing as a swappable seam, and the difference is exactly what
> this document exists to keep straight.

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
5. For the registry-backed seams — identity, secret store, recording, confinement
   substrate, and **only** those four; `policy_engine` is NOT among them — it now
   appears in `/healthz.components.<seam>.available`
   automatically (`<seam>` ∈ `identity`, `secret_store`, `recording`, `sandbox`)
   and is selectable via that seam's selector env var — `WARDYN_IDENTITY`,
   `WARDYN_SECRET_STORE`, and `WARDYN_RECORDING_STORE` respectively (the substrate
   selects via the `-runner` flag / `WARDYN_RUNNER`; a build-tag-gated impl like the OCI
   substrate registers — and thus appears — only in builds that compile it, so a
   tagless binary honestly advertises `sandbox.available: []` and fails closed
   on `-runner docker`). Seams **without** a registry are wired through their own
   config knobs and surface elsewhere at runtime, not in `components`: the LLM
   gateway and content detection are builtin (proxy-side), audit sinks via
   `WARDYN_AUDIT_SINKS` (boot log), eBPF ground-truth via
   `/healthz.ebpf_groundtruth`, and per-class substrate runtimes via
   `/healthz.confinement_substrates`.

For the **policy-evaluator**, **egress-gateway**, and **confinement-substrate**
seams, also honor the security rules: the evaluator's verdict cannot relax the
IP guard / approval FSM; a gateway forwarder is invoked only *after* allow +
IP-vetting and its own endpoint must be allowlisted + IP-vetted; a substrate must
preserve L0 (the agent's sole egress is the wardyn-proxy endpoint) and fail closed
when a demanded class cannot be enforced.

---

## 2b. The four-layer question, answered honestly

An agent-sandboxing platform decomposes into four layers, and the question an
architecture review actually asks is which of them you can swap:

| Layer | Wardyn today |
|---|---|
| **Physical sandbox** — the isolation boundary | **Genuinely pluggable.** `substrate.Substrate` has **two independently-built implementations** — Docker and Kubernetes — held to **one** conformance contract, and both are run in CI against a live daemon and a real kind+Calico cluster. Inside the Docker substrate there is a second, cheaper swap point: `WARDYN_CONFINEMENT_MAP` pins a different OCI runtime per confinement class (runc / gVisor / Kata / sysbox), so the isolation *technology* changes with no new substrate at all. |
| **Ingress / egress controls** | **Split, and the halves differ.** The *decision* is a seam — `egress.Evaluator`, with a conformance suite — but it has one implementation and no selector (see §1). The *enforcement point*, the `wardyn-proxy` sidecar, is **not** swappable: a replacement would have to reimplement credential minting, the approval channel, recording upload, the git broker, the LLM routes, decision-log streaming, run-token handling and the MITM CA. There is no published contract for that, and there should not be one yet. Ingress is three hard-wired lanes (browser attach, SSH, UI relay) sharing one primitive, `ExecStream`. |
| **LLM gateway** | **Hard-wired.** The provider hosts are string constants in `internal/egress/proxy/local_routes.go`; there is no swap point. What already ships on that path is real and often mistaken for less: proxy-side credential injection so the key is never resident, TLS-MITM of the CONNECT tunnel, and outbound content inspection whose coverage is reported honestly per endpoint rather than assumed. |
| **MCP / tool gateway** | **Does not exist.** `cmd/wardyn-toolgate` is **not** one, despite speaking MCP: it is a single-tool stdio server that relays one harness's own permission prompt into Wardyn's approval FSM, and it is wired so it is the only MCP endpoint the agent can see. It has no policy, no tool allowlist, and no view of third-party MCP traffic. The L3 layer that would govern that is planned, not built. |

**One of four is genuinely pluggable.** That is the honest answer, and it reads
better than four seams that are really one seam and three intentions. The
strongest thing to say here is the first row: *two implementations, one
conformance contract, both enforced in CI* — which is a materially stronger claim
than "we have an interface", and it is the only one of the four that has earned
it.

## 3. Blessed-default-vs-alternate matrix

`Shipped` = the out-of-box default that runs today. `Recommended (prod)` = the
standard Wardyn recommends for hostile-multi-tenant production (may differ from
shipped — see the honesty rule). `Seam status`: **shipped** = the interface +
registry + conformance exist today; **planned** = interface lands on the roadmap.
Planned entries that carry a MILESTONE — and what they are planned *for* — are
enumerated in [`../ROADMAP.md`](../ROADMAP.md) (SPIRE, OpenBao); the rest are
seam-ready candidates with no scheduled work. This table is the per-seam view,
not a second roadmap.

| Subsystem | Interface | Shipped out-of-box default | Recommended (prod) | Registered alternates | Conformance | Seam status |
|---|---|---|---|---|---|---|
| Sandbox / confinement | `substrate.Substrate` (under the `orchestrator` `runner.Runner`) | `docker`/OCI substrate; CC1 runc / CC2 runsc / CC3 kata\* | **Kata-CC3** (QEMU by default; experimental today, see README Confinement Classes) | `k8s` substrate (`internal/runner/k8s`, `-tags k8s`, `WARDYN_RUNNER=k8s`) — pods instead of containers, L1/NetworkPolicy-backed (not L0/structural: the boot-time egress canary refuses to construct unless the cluster's CNI actually enforces `NetworkPolicy`), CC1-only until `WARDYN_CONFINEMENT_MAP`/the Helm chart's `k8s.runtimeClasses` pins CC2/CC3 to a registered RuntimeClass; OCI runtime pins via `WARDYN_CONFINEMENT_MAP` (kata-qemu, kata-clh, gVisor, sysbox); non-OCI VMM (SmolVM/Firecracker) via a new `Substrate` impl | `test/conformance` (Runner) + orchestrator routing tests, run in CI against **both** targets — `conformance` (docker, live daemon) and `conformance-k8s` (a real kind cluster, `disableDefaultCNI` + pinned Calico, since kind's default CNI does not enforce `NetworkPolicy`); a shared case with no k8s equivalent (the L0-specific one) self-skips there by design rather than faking a result, and a dedicated L1 case proves the k8s substrate's actual claim instead | shipped (registry + `init()` self-registration — the `docker` impl registers under `-tags docker`, the `k8s` impl under `-tags k8s`; runtime-pluggable + substrate sub-interface). The `k8s` substrate is registered and conformance-green on kind+Calico, but is NOT the shipped out-of-box default (that stays `docker`) and is not at parity with it — no BYOI/devcontainer builds, no `local_dir` mounts, no ground-truth correlator (`docs/OPERATIONS.md`'s "Kubernetes: known gaps"). Non-OCI VMM impl planned |
| Identity | `identity.Provider` | `embedded` (SPIFFE-shaped JWT-SVID) | **SPIRE** (attestation, short-lived SVIDs) | `spire` (planned) | `identity/identitytest` | shipped |
| Secret store | `secretstore.Store` (0.7: `For(owner)` is part of the interface — per-principal namespacing, not a `pg`-only feature) | `pg` (age-encrypted Postgres) | **OpenBao** (LF, Vault-compatible) | `openbao` / `vault` / cloud KMS (planned) | `secretstore/secretstoretest` | shipped |
| Recording | `recording.Store` | `pg` (`fs` via `WARDYN_RECORDING_STORE=fs`) | `pg` (readable from any process, unlike `fs`; object storage optional at scale). **Not** an HA recommendation — wardynd is single-replica by construction and the Helm chart refuses more (docs/OPERATIONS.md) | `fs`; S3/GCS object store (planned) | `recording/recordingtest` | shipped |
| Egress policy | `egress.Evaluator` | `builtin` (RunPolicySpec) | **OPA/Rego** | `opa` / `cedar` (planned) | `egress/evaluatortest` | shipped (seam); evaluator alternates planned |
| LLM gateway | none — the route is hard-wired in `internal/egress/proxy/local_routes.go` (no swap point) | `direct` (pinned-IP RoundTrip) | LiteLLM / Portkey / Envoy AI GW behind wardyn-proxy | external gateway (planned) | — (nothing to conform to) | no seam yet; external gateway planned |
| Interactive access lane | none — the browser terminal (`Runner.Attach`), the SSH gateway (`internal/api/sshgateway.go`) and the UI-sandbox relay (`internal/api/uigateway.go` over `runner.ExecSession`) are three hard-wired lanes, not implementations of one interface | browser terminal; `ssh`/sftp/`-L` and the `ui_apps` relay both off unless their listener env var is set | same (each lane is opt-in per deployment) | Apps ride the **image**, not a seam: `vscode` and `novnc` today (0.7 added the second and changed no server code, which is the claim this row was making), and any other loopback-HTTP app the same way. Native clients beyond VS Code Remote-SSH — JetBrains Gateway, RDP, Xpra — are **exploratory**: nothing built, no seam, criteria in [UI-SANDBOXES.md](UI-SANDBOXES.md#native-clients-exploratory) | `test/conformance`'s `ExecStreamLoopbackRelay` — the ONE layer these lanes share: `ExecStream` reaching a port the sandbox already listens on inside its own netns, full-duplex with stdin still open. Run on **both** substrates (docker + kind/Calico), so the relay's transport is parity-gated even though the lanes themselves are hard-wired. The browser-level e2e above it (`make test-e2e-ui-sandbox`) is compose-only and says so | no seam yet; native lane exploratory |
| Content detection | `contentscan.Detector` | builtin (known-secret / regex / entropy / PII) + sidecar | builtin + **LLM Guard / Presidio** sidecar | `DetectorSidecarURL` (shipped) | — | shipped (detector seam) |
| Audit sinks | `audit.Sink` | none (Postgres recorder always) | OpenTelemetry → SIEM | `file` / `webhook` / `syslog` (shipped) via `WARDYN_AUDIT_SINKS` | — | shipped (non-registry: struct-field JSON parse, `internal/audit/sinks/config.go` — not a `/healthz` component) |
| eBPF ground-truth | host sensor ingest | none (honest-degraded `/healthz`) | **Tetragon** (enforcement) | Falco / Tracee (ingest-compatible) | — | shipped (ingest seam) |

All recommended-prod candidates are Apache-2.0 / permissive, self-hostable, and
CNCF-graduated/incubating where available (Kata, SPIRE, OPA, Tetragon, Cilium),
or LF-governed (OpenBao). None are built in this effort — each is a documented
row with a seam (or a planned one) and a conformance contract ready to hold it.

---

## 4. The recommended-vs-shipped tension, made honest

We recommend SPIRE / OpenBao / OPA / Kata-CC3 / Tetragon for hostile-multi-tenant
production while the binary ships embedded / pg / builtin / docker(runc..kata) /
heartbeat-only. "Recommended: SPIRE" must never be misread as "you are running
SPIRE", so three things keep the gap visible:

1. The two columns above never merge.
2. **`/healthz` is machine-honest.** For every seam `components` covers,
   `components.<seam>.selected` is the *actual* running impl,
   `components.<seam>.available` is what this build's registry actually holds, and
   `source` distinguishes `default` from `configured`. Same structural anti-overclaim guarantee as
   `ebpf_groundtruth` and runner `Capabilities`.
3. **One shared conformance contract.** A future SPIRE / OpenBao / OPA impl must pass
   the *same* `RunConformance` suite the shipped default passes — until an alternate
   is built and green, the shipped default is what you run.

---

## 5. Where the seams live (code map)

- `internal/component/registry.go` — the shared `Registry[C]`.
- `internal/identity/{registry.go,revocation.go}` + `identity/embedded/register.go` + `identity/identitytest`.
- `internal/secretstore/{registry.go,secretstore.go}` + `secretstore/pg/register.go` + `secretstore/secretstoretest`. `Store.For(owner)` (0.7, migration `0050`) is part of the seam contract every implementation must honor — `RunConformance`'s `owner_*` cases hold a future alternate to the same fallback/isolation rules `pg` implements today.
- `internal/recording/registry.go` + `recording/recordingtest`.
- `internal/runner/substrate/{substrate.go,registry.go}` (the `Substrate` sub-interface + `ClassSupport` + the substrate registry) + `internal/runner/docker/register.go` (the OCI substrate's `init()` self-registration, `-tags docker` only) + `internal/runner/orchestrator/` (the build-tag-free `runner.Runner` that multiplexes substrates by Confinement Class, with a durable pg-backed ref→substrate `RefStore` — `internal/store/store_sandbox_ref.go` — so kill-switch routing survives control-plane restarts) + `internal/runner/docker/hardening.go` (`resolveRuntime`, `capabilitiesForWith`) + `cmd/wardynd` `WARDYN_CONFINEMENT_MAP` — the confinement substrate/runtime seam.
- `internal/egress/evaluator.go` (policy evaluator, overridable via `Options.Evaluator` in `internal/egress/proxy/proxy.go`) + `internal/egress/proxy/local_routes.go` (LLM-route forwarding) — the egress seams.
- `internal/runner/runner.go` (`Capabilities.Resolved`) + `internal/api/server.go` (`/healthz` `confinement_substrates` + `components`).
- Planned impls (seams ready, alternates not built): the non-OCI VMM `Substrate` (SmolVM/Firecracker), SPIRE identity, OpenBao secrets, OPA/Cedar evaluator, an external LLM gateway.
