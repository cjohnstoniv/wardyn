# Wardyn

[![License: Apache-2.0](https://img.shields.io/badge/License-Apache_2.0-blue.svg)](LICENSE)
[![Status: pre-alpha](https://img.shields.io/badge/Status-pre--alpha-orange.svg)](#status)
[![Go 1.26](https://img.shields.io/badge/Go-1.26-00ADD8.svg)](go.mod)
[![CI](https://github.com/cjohnstoniv/wardyn/actions/workflows/ci.yml/badge.svg)](https://github.com/cjohnstoniv/wardyn/actions/workflows/ci.yml)

**The open-source governance control plane for coding agents —
identity, controls, and audit are the product; the sandbox is a pluggable
commodity.** Wardyn governs workload run-identity and tokens: a *wardyn*
authorizes one specific, scoped action — which is exactly what the broker mints.

> **Name & trademark.** "Wardyn" is a working name — a formal trademark
> clearance (USPTO full-text + GitHub org / domain / package handles) is still
> pending, so the name may change before a 1.0. The module path
> `github.com/cjohnstoniv/wardyn` is a personal namespace for now; re-homing to a
> dedicated org later is a one-line change.

![Wardyn's Getting-started page — pick a confinement barrier (Fence / Wall / Vault) with your host's real capabilities detected live](docs/img/getting-started.png)

---

## What Wardyn Is

Coding agents (Claude Code, Codex CLI, and successors) inherit the full developer
credential they launch under — a prompt-injected agent, a poisoned dependency, or a
compromised MCP server inherits the same repos, cloud access, and blast radius.
Wardyn is the governance layer between a human operator and a running agent:

- **Per-run identity.** Every run gets a SPIFFE ID
  (`spiffe://<trust-domain>/agent-run/<id>`) distinct from the human; the human's
  `sub`, the run's `act`, and the accountable `sponsor` travel together in every
  token, commit, and audit event. **[shipped]** (embedded JWT-SVID issuer;
  SPIRE-backed **[v0.5+ — planned]**).

- **Broker-minted scoped credentials.** The agent never holds a credential; the
  broker mints short-lived, repo-scoped, down-scoped credentials on demand, injected
  proxy-side. Approval-required grants mint inside the Postgres transaction that
  verifies an `APPROVED` request for that exact run+scope — no widening. **[shipped]**

- **Layered egress.** A run's sandbox is gatewayless — its only path off-host is the
  `wardyn-proxy` sidecar (L0 **[shipped]**), which enforces an L7 allowlist, method
  rules, first-use approval, and proxy-side credential injection (L2 **[shipped]**).
  The env-var-bypass class is defended *structurally*: with no route, an agent that
  ignores `HTTP_PROXY` reaches nothing. L1 default-deny and an MCP gateway are
  **[v0.5+ — planned]**. Full table in [ARCHITECTURE.md](ARCHITECTURE.md).

- **Three-stream append-only audit.** The control-plane event log (Postgres trigger
  blocks UPDATE/DELETE), PTY replay via `wardyn-rec`, and the eBPF/Tetragon
  ground-truth stream (`kernel.*` correlated on `run_id`) — all **[shipped]**.
  Ground-truth is **detection, not prevention** and **honestly degradable**
  (`/healthz` reports `ebpf_groundtruth=unavailable` without a sensor; blind inside
  CC3/Kata guests; its Tetragon mapper is validated against documented shapes, not a
  live deployment yet). SIEM export (JSON webhook/syslog/file) is **[shipped]** and
  free; OTLP/OCSF **[v0.5+ — planned]**.

- **Confinement Classes.** Friendly UI names — **Fence** = CC1, **Wall** = CC2,
  **Vault** = CC3. CC1/Fence (hardened shared-kernel runc) **[shipped]**, CC2/Wall
  (gVisor userspace kernel — default) **[shipped]**,
  CC3/Vault (Kata microVM) **[experimental]** — live-proven on a Docker daemon with
  the Kata runtime registered (needs `/dev/kvm`; not on Docker Desktop). Fence needs
  only Docker; Wall adds `runsc`; Vault adds `/dev/kvm` + Kata. Policy can mandate a
  minimum class; the plane refuses a run a substrate cannot satisfy.

- **Bring Your Own Image (BYOI).** A run may name an arbitrary base image; the plane
  wraps it with the runner tools (digest-pinned, opt-in via `WARDYN_ENVBUILD`) and
  gates launch on an in-sandbox self-test, fail-closed. **[shipped]**

- **Model access without resident keys.** An Anthropic API key or a Claude
  subscription (from the operator's live login) is injected proxy-side, never resident;
  a containerized plane connects a **Wardyn-managed Claude subscription** via container
  login. AWS Bedrock is operator-configured (bearer proxy-injected; SigV4 keys resident, documented). **[shipped]**

---

## Architecture at a glance

<!-- Designed in the "Wardyn Architecture Diagrams" Figma file; the code-true,
     CI-label-checked mermaid version of this and the other diagrams lives in
     ARCHITECTURE.md and threatmodel/THREAT-MODEL.md. Regenerate from Figma if
     the component set changes. -->
![Wardyn system overview: a trusted control plane (wardynd + Postgres) launches each coding agent into an untrusted, gatewayless per-run sandbox whose only path off-host is the wardyn-proxy egress sidecar](docs/img/architecture.png)

A trusted control plane (`wardynd` + Postgres) launches each agent into an
untrusted, gatewayless sandbox whose only path out is the `wardyn-proxy`
sidecar; credentials are injected at the proxy, and every decision and PTY
session streams back into the append-only audit log.

---

## Why Now

Coding agents today inherit developer credentials at launch. No purpose-built,
open-source governance layer existed to scope, gate, and attribute what an
agent can do. The incumbent tools have filled parts of the gap in ways that
create new problems:

- Existing governance controls ship as paid tiers of commercial platforms,
  closed-source services, or vendor-bundled runtimes — you cannot audit what
  you cannot read, and you cannot run it on your own infrastructure without a
  commercial agreement.
- Existing sandboxing tools have overclaimed their isolation properties: in
  documented red-team exercises, hook-based in-agent enforcement was bypassed
  via the dynamic linker (`ld-linux`/`mmap`); at least one commercially-shipped
  sandbox escaped via a CVE in the container runtime.

Wardyn's thesis is that the real boundary is structural (no network path, no
resident credentials, enforcement outside the agent process) and that honest
disclosure of residual risks is a feature, not a liability. See
[`threatmodel/`](threatmodel/THREAT-MODEL.md).

The substrate is a pluggable commodity: every major subsystem (sandbox, identity,
secrets, egress, policy, audit) sits behind a seam with a blessed, tested default
and documented swappable alternates. See
[`docs/PLUGGABILITY.md`](docs/PLUGGABILITY.md).

---

## What It Does

### Identity, approval, and the credential mint

The delegation chain is `human sub → agent-run SPIFFE ID → scoped minted credential`;
`wardynd` mints it at sandbox start and every downstream token and commit carries it
(embedded identity provider, or SPIRE at v0.5). A `CredentialGrant` is eligibility, not
issuance — with `RequiresApproval` set the broker mints only inside the Postgres
transaction that verifies `approvals.state = 'APPROVED'` for this `run_id`+`grant_id`
and claims `minted_jti`, so minted scope equals what the approver saw. On run stop the
kill-switch cascades automatically: teardown → deny-list run token → revoke credentials → durable state.

### Egress model

L0 gatewayless sandbox → L1 default-deny nftables/NetworkPolicy (blocks
`169.254.169.254`) → L2 `wardyn-proxy` (L7 allowlist, method rules, first-use approval
— `always_deny`/`deny_with_review`/`wait_for_review` holds the connection for a live
decision — plus proxy-side credential injection) → L3 MCP/tool gateway (v0.5). The
same seam carries the corporate lanes (upstream proxy hop, artifact-mirror
redirection, brokered SCM creds). Full table in [ARCHITECTURE.md](ARCHITECTURE.md).

### Audit streams

1. `audit_events` (Postgres, append-only — `UPDATE`/`DELETE` trigger raises). System of record.
2. eBPF/Tetragon ground-truth — a tamper-proof counterpart to agent self-report:
   detection, not prevention (flags the `ld-linux`/`mmap` bypass, never blocks),
   honestly degradable via `/healthz`, blind inside CC3/Kata guests. Opt-in.
3. PTY session replay via `wardyn-rec` (execs `asciinema`; GPL subprocess, never
   linked). Each stream is keyed on `run_id` and exported to customer SIEM free.

### Recorded profiles → governed reruns

The primary way to onboard real work: **record** a named interactive session in a
workspace, then rerun it as a governed profile — the New Run dialog offers recorded
sessions as fast-track profiles with observed egress preloaded. **Verify** re-runs the
steps under least privilege in a fresh CONFINED session (a live re-run, not a
byte-for-byte replay). See [TRY-IT Level 2.5](docs/TRY-IT.md).

### AI Run Composer (optional)

Describe a task in plain English and Wardyn proposes a confined run (agent, repo,
confinement, egress, grants) and grades it deterministically. It's **advisory**
(backend never sees run credentials) and **off by default**; configure a backend via
`WARDYN_COMPOSER_CONFIG` (see [`examples/composer-configs/`](examples/composer-configs/);
`fake.json` needs no key) and the Describe surface appears in the New Run dialog —
else it falls back to the manual wizard. See [TRY-IT Level 3](docs/TRY-IT.md).

---

## Honest Security Posture

Wardyn publishes what it does **not** defend against. The full published
residual-risk list is in [`threatmodel/THREAT-MODEL.md`](threatmodel/THREAT-MODEL.md).

Notable residual risks:

- **The model-API channel is an unavoidable data-exit path.** The LLM gateway
  logs every prompt/token/tool call but cannot prevent an agent from encoding
  data into a prompt to a model it is permitted to call.
- **Domain fronting and DNS-tunnel exfil** are not closed below the optional
  L2 TLS-intercept tier. Per-run TLS interception **is** shipped for
  operator-listed MITM-eligible hosts (LLM endpoints, artifact registries —
  opt-in, off by default); interception of **arbitrary** domains is the v0.5
  target, so most non-LLM HTTPS egress remains an opaque CONNECT tunnel.
- **Tier-1 hardened-runc shares the host kernel.** A kernel 0-day on a
  runc-only host defeats the sandbox boundary. Wardyn defaults to CC2 (gVisor)
  for this reason.
- **The 1-hour minted-token window** before kill-switch revocation is a real
  exposure window that cannot be fully eliminated, only minimized by TTL.

---

## Requirements

- **Docker** with the `compose` v2 plugin (Docker Desktop or a native Docker
  Engine). Fence/CC1 runs need nothing more; Wall/CC2 adds gVisor's `runsc`,
  Vault/CC3 adds `/dev/kvm` + a Kata runtime — `wardyn setup wall|vault`
  prints the exact steps for your machine.
- **Go 1.26+** and **Node 22 + pnpm 9** — only for building from source
  (host mode builds `bin/wardynd` + the UI locally).
- **Claude Code CLI** (optional, host mode) — `make setup` stages an existing
  `claude` login so sandboxes can use your subscription; without one, runs
  have no model access until you add an API key (or Bedrock) in the UI.
- Postgres is **included** in the compose file — nothing external to install,
  no hosted service to sign up for.

`go install github.com/cjohnstoniv/wardyn/cmd/wardyn@latest` installs the **CLI**. But
`wardynd` is **not** `go install`-able (it needs `-tags docker` + a built `ui/dist`; a
bare install has no embedded UI or sandbox runner) — run it via `make setup` or the container image.

## Quickstart (pre-alpha)

> **Status: pre-alpha.** Interfaces are not stable. Do not run production
> workloads.

```sh
git clone https://github.com/cjohnstoniv/wardyn
cd wardyn
make setup   # one installer: detects your host, sets up host mode, launches + opens the UI
```

`make setup` (== `scripts/setup.sh`) runs **host mode** — the default: sandbox agents
run on this machine with your Claude login (staged and injected proxy-side, so no
long-lived secret is resident), builds `bin/wardynd` + the UI, starts Postgres on
loopback, launches `wardynd` in the background (PID/log in `~/.wardyn/`), opens the UI,
and prints commands for any missing confinement barrier. Stop with `make stop-host`.

**Containerized mode** (`WARDYN_SETUP_MODE=container`) runs `wardynd` in a compose
container instead — the fix for workspace **Verify/Record** on Docker Desktop + WSL2
NAT; add an API key or Bedrock for model access. **Team mode** (multi-user SSO/RBAC) is **coming soon**.

Prove the egress boundary hands-on first? The UI's **/demos** screen launches
interactive sandboxes with no repo and no keys — see [TRY-IT Level 0.5](docs/TRY-IT.md).

### The local access model in one picture

![The local access model: your machine is the hard ceiling — everything you can do bounds everything a sandbox can; the Wardyn policy ceiling clamps that down; and each run receives only the minimal subset (scoped credential, task egress allowlist, onboarded mounts) its task needs](docs/img/access-model.png)

On your desktop, Wardyn never *adds* power: a sandbox reaches at most what you (the
operating user) already can, the operator policy clamps that to what you allow, and
each run gets only the minimal subset (scoped credential, egress allowlist, mounts) it needs.

New to Wardyn? [`docs/TRY-IT.md`](docs/TRY-IT.md) is the guided walkthrough (no-key
governance demo, hands-on demo sandboxes, then a real Claude Code run);
[`docs/sdk.md`](docs/sdk.md) covers the Go SDK + raw curl API;
[`deploy/compose/README.md`](deploy/compose/README.md) the compose stack, no-login
local mode, and TLS; and [`examples/`](examples/) holds demo policies and configs.

---

## Deployment Surface

Two paths — only one runs sandboxes today:

| Path | Location | Status |
|---|---|---|
| Docker Compose | `deploy/compose/` | **[shipped]** — the only working data plane today |
| Helm chart (one blessed chart) | `deploy/helm/wardyn/` | **[v0.5 — planned]** — render-checked only (`helm lint` + `helm template` in CI); there is no Kubernetes runner driver yet, so the chart deploys the control plane but cannot create sandboxes yet |

No "bring your own arbitrary Kubernetes manifests." The parity rule: a
feature is not done until it passes the conformance suite on both Docker and
kind — today only the Docker target runs functionally (see Status below).

---

## Status

| Milestone | Highlights | Status |
|---|---|---|
| **v0.1** | Per-run identity (embedded provider), approval FSM, credential broker, L2 egress proxy, append-only Postgres audit + PTY replay, CC1/CC2 confinement gating, Compose deploy | **Shipped (pre-alpha)** |
| **v0.2** | Open-source pilot bar (Docker-only): secret-output masking, eBPF/Tetragon ground-truth audit stream, pinned seccomp + AppArmor, interactive attach sessions, policy CRUD, run-completion state, control-plane TLS, real conformance gate + supply-chain CI | **Shipped (pre-alpha)** |
| **v0.5** | SPIRE identity provider, OpenBao secret store, L3 MCP/tool gateway, arbitrary-domain L2 TLS-intercept (targeted LLM/registry MITM already ships), Helm chart, cloud STS federation, OTLP/OCSF SIEM sinks | Planned |
| **v1.0** | CC3 Kata packaged/GA (experimental today — see Confinement Classes), Cilium toFQDNs, hash-chained audit + signed action receipts, separation-of-duty on control plane, conformance suite across both targets | Planned |

---

## Components

`wardynd` (control plane: REST API, embedded UI, policy engine, approval FSM,
token broker, audit ingest) · `wardyn-runner` (docker driver) · `wardyn-proxy`
(L2 egress sidecar) · `wardyn-rec` (PTY recorder) · `wardyn-tetragon-ingest`
(ground-truth ingest, opt-in) · `wardyn-git-helper` (stdout-only git credential
broker) · `wardyn-scan` / `wardyn-verify` (workspace onboarding) · `wardyn` (CLI).
Full per-binary roles in [ARCHITECTURE.md](ARCHITECTURE.md).

---

## License and Governance

Apache-2.0. Contributor sign-off via DCO (`Signed-off-by`). No `enterprise/`
directory — every control is in the open. No hosted backend either: it runs on
your infrastructure or it doesn't run. Nothing here is a free tier of a paid
product — there is no paid product. Every control described above (identity,
egress, audit, approvals) is in this repo under Apache-2.0, not gated behind a
commercial license. CNCF Sandbox is the governance target.

Contributions welcome. See `CONTRIBUTING.md`.

---

*Wardyn is a working name, pending trademark search.*
