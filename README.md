# Wardyn

[![License: Apache-2.0](https://img.shields.io/badge/License-Apache_2.0-blue.svg)](LICENSE)
[![Status: pre-alpha](https://img.shields.io/badge/Status-pre--alpha-orange.svg)](#status)
[![Go 1.26](https://img.shields.io/badge/Go-1.26-00ADD8.svg)](go.mod)
[![CI](https://github.com/cjohnstoniv/wardyn/actions/workflows/ci.yml/badge.svg)](https://github.com/cjohnstoniv/wardyn/actions/workflows/ci.yml)

**The open-source governed-sandbox control plane for any workload — identity,
controls, and audit are the product; the sandbox is a pluggable commodity.**
Anything you run under your own credentials inherits your full blast radius;
Wardyn is the layer in between — per-run identity, credentials minted and
revoked per run, one audited path off-host, no resident key. Coding agents are
the flagship use.

> **Status: pre-alpha.** Interfaces are not stable. Do not run production
> workloads. "Wardyn" is a working name — trademark clearance (USPTO full-text +
> org / domain / package handles) is still pending, so the name and the personal
> `github.com/cjohnstoniv/wardyn` module path may change before a 1.0.

![Wardyn detecting this host's confinement capabilities](docs/img/getting-started.png)

## Install

Pick by **who runs this box**. Everything here pulls cosign-signed, SBOM-attested images — [`docs/VERIFY.md`](docs/VERIFY.md) checks that — no clone needed.

### Single-user — you are the operator

**Your own machine** — no SSO; the installer prints an admin token you paste once:

```sh
curl -fsSL https://github.com/cjohnstoniv/wardyn/releases/download/v0.6.6/install.sh | sh
```

Cosign-signed, not tip-of-`main` — only the script is pinned; it installs the newest release (`WARDYN_VERSION` overrides). Installs into `~/.wardyn`, opens Getting Started on <http://127.0.0.1:8080> as an **admin** (`WARDYN_HOME`, `WARDYN_PORT` override).
**A managed laptop, one daemon per developer** — desktop profile a′: [`docs/DESKTOP.md`](docs/DESKTOP.md).

### Multi-user — an admin sets the ceiling, members run inside it

Each human gets an SSO identity and an **admin** or **member** role ([`docs/OPERATIONS.md`](docs/OPERATIONS.md#multi-user-who-can-change-what)).

- **A shared host** — compose `--profile sso` + `WARDYN_OIDC_ROLE_MAP`: [OPERATIONS.md §"Second user, same host"](docs/OPERATIONS.md#second-user-same-host).
- **Kubernetes** — a published OCI chart (the command after this list); `.claude/skills/wardyn-k8s-setup/` carries an agent-readable recipe for the same path.
- **Managed laptops under org governance** — desktop profile m′: [`docs/DESKTOP.md`](docs/DESKTOP.md#the-member-mode-profile-topology-m).

```sh
# NOT releases/latest — it excludes pre-releases, and every Wardyn release is
# one, so that endpoint 404s and leaves this empty.
WARDYN_VERSION=$(curl -fsSL "https://api.github.com/repos/cjohnstoniv/wardyn/releases?per_page=1" \
                 | sed -n 's/.*"tag_name": *"v\([^"]*\)".*/\1/p' | head -1)

helm install wardyn oci://ghcr.io/cjohnstoniv/charts/wardyn \
  --version "${WARDYN_VERSION:?could not resolve a version — refusing an unpinned install}" \
  --namespace wardyn --create-namespace \
  --set auth.adminToken.secretRef.name=wardyn-auth
```

Needs an admin token or OIDC, a Postgres DSN and an age identity — role map, substrate
and values table: [`deploy/helm/wardyn/README.md`](deploy/helm/wardyn/README.md).

### Joining a Wardyn someone else runs

Install nothing — sign in with SSO and read **[`docs/MEMBERS.md`](docs/MEMBERS.md)**: what a member can do, your first run, and what to ask your admin for.

**Building from source is a contributor path** — see [`CONTRIBUTING.md`](CONTRIBUTING.md). Clone, then `make setup`
(a failed image pull falls back to building from this checkout; `WARDYN_BUILD_LOCAL=1` forces it).

**That is the whole setup.** The barrier is the only requirement — no model, no
API key, no agent. Put the sandbox rules in a small **YAML** (or JSON) policy and
hand it to one `wardyn run` — interactive or unattended:

```sh
wardyn run --agent claude-code --task-mode exec \
  --task 'echo hello from a governed sandbox' \
  --policy-file examples/policies/sandbox.yaml --wait
```

That runs a plain shell command in a governed sandbox: `--task-mode exec` means
no agent and no model are involved at all. (`--agent` still names which sandbox
image to launch — it is an image label, not a statement that an AI runs your
task.) That [file](examples/policies/sandbox.yaml) is a commented, sealed floor;
`wardyn policy render -f <file>` checks it. `--image` brings your own base
([docs/ENVBUILD.md](docs/ENVBUILD.md)); `make compose-down` stops everything.

**Want an agent to write the code?** *Then* connect a model — optional, and
equally first-class at the CLI or in the UI:

```sh
claude setup-token | wardyn subscription connect   # subscription (never resident)
echo "$KEY"        | wardyn secret set anthropic-api-key   # API key
# Bedrock: WARDYN_BEDROCK_REGION/MODEL (+ WARDYN_BEDROCK_AWS_DIR for ~/.aws SSO)
wardyn setup status   # what's configured + the next command per unmet check
```

Skipping this is a supported end state, not an unfinished setup: `setup status`
reports model access as **optional** and never as a gap to clear. Most of the
built-in demos need no model either — see
[TRY-IT.md](docs/TRY-IT.md)'s "governance demo (no keys)".

### Requirements

- **Docker** + `compose` v2 (Postgres rides in the compose file). Fence/CC1
  needs nothing more; Wall/CC2 adds gVisor's `runsc`, Vault/CC3 `/dev/kvm` +
  Kata — `wardyn setup wall|vault` prints the steps for your host.
- **Go 1.26+**, **Node 22 + pnpm 9** — only to build from source.
- `go install …/cmd/wardyn@latest` gives the **CLI** only; `wardynd` needs
  `-tags docker` + a built `ui/dist` — use `make setup` or the image.

## What you get

| Capability | What it does | Detail |
|---|---|---|
| Governed runs | Per-run identity in a gatewayless sandbox, driven from a terminal-first cockpit | [ARCHITECTURE.md](ARCHITECTURE.md) |
| Egress + approvals | Only path out is the proxy; an unlisted host can hold mid-flight — once, run, until, always | [POLICIES.md](docs/POLICIES.md) |
| Record Mode | Run once open, get the minimal policy, replay confined | [TRY-IT.md](docs/TRY-IT.md) |
| Workspaces & secrets | Mounts only what the workspace declares; secrets write-only, never readable back | [OPERATIONS.md](docs/OPERATIONS.md) |
| Policies & confinement | One policy picks the barrier: Fence (runc), Wall (gVisor), Vault (Kata, experimental); a host that can't enforce it refuses. Right-size it — a read-only scan or an indexer does not need what an autonomous agent needs | [POLICIES.md](docs/POLICIES.md) |
| Model access | Key, subscription or Bedrock injected proxy-side; the sandbox holds an inert sentinel | [TRY-IT.md](docs/TRY-IT.md) |
| CI / headless | No UI, no human: the governed run's exit code becomes the pipeline's | [CI.md](docs/CI.md) |
| Audit + attach | Three append-only streams a Postgres trigger won't let you rewrite; attach live from browser or SSH | [SSH.md](docs/SSH.md) |
| UI sandbox gateway | Relay a declared loopback port (editor, dev server) to a browser over its own origin — a per-run origin is the documented production default | [UI-SANDBOXES.md](docs/UI-SANDBOXES.md) |

Everything else — env and policy reference, deployment, sample workspaces — is
indexed in [docs/](docs/README.md).

## Watch it work

Thirteen narrated walkthroughs, about 72 minutes end to end. Every one drives
the real console against real sandboxes — the policies are live, the refusals
are real, and the audit rows on screen were written by the run you are watching.
Start with **03a** if you only watch one; it is the boundary itself.

The series uses a coding agent as its worked example, because that is the case
most people arrive for. The mechanics on screen — the egress boundary, the
credential brokering, the audit trail — are the same for any sandboxed workload.

| Episode | What it shows | Length |
|---|---|---|
| [01 — Why govern agents][v01] | The blast radius anything inherits when it runs as you | 5:51 |
| [02 — Set up the host][v02] | `make setup`, from a bare host to a running control plane | 6:27 |
| [03a — What it stops][v03a] | **The core.** Four things that happen to a host a run may not reach, then the secret the sandbox is never handed | 11:40 |
| [03b — The network, three more ways][v03b] | A real agent boxed in, a policy recorded from a run, an approval that lasts one connection | 5:13 |
| [03c — Authorized, then issued][v03c] | A bearer token attached at the boundary; a PAT that only ever exists in a pipe | 7:26 |
| [03d — The kinds that can't use a header][v03d] | SSH keys, brokered GitHub tokens, cloud STS — credentials no header injection can carry | 6:10 |
| [04 — Add a workspace][v04] | Onboarding a source, so a run can mount only what was declared | 4:06 |
| [05 — Your first policy][v05] | Writing the ceiling every run is clamped to | 3:32 |
| [06 — Your first run][v06] | One governed run, launched and read back from its record | 3:51 |
| [07 — Interactive runs][v07] | Attaching a live terminal to a running sandbox | 4:04 |
| [08 — An autonomous agent][v08] | A real coding agent doing real work inside the boundary | 4:14 |
| [09 — Record a run][v09] | Run open, derive the minimal policy, replay it confined | 6:06 |
| [10 — Approvals and egress][v10] | Deciding a held request — once, this run, until, always | 3:40 |

They ship as [release assets](https://github.com/cjohnstoniv/wardyn/releases/tag/v0.6.0),
not in the repo, so a clone stays small. Links pin `v0.6.0`; later releases
re-publish under the same filenames.

[v01]: https://github.com/cjohnstoniv/wardyn/releases/download/v0.6.0/wardyn-01-why-govern-agents.mp4
[v02]: https://github.com/cjohnstoniv/wardyn/releases/download/v0.6.0/wardyn-02-set-up-the-host.mp4
[v03a]: https://github.com/cjohnstoniv/wardyn/releases/download/v0.6.0/wardyn-03a-what-it-stops.mp4
[v03b]: https://github.com/cjohnstoniv/wardyn/releases/download/v0.6.0/wardyn-03b-the-network-three-more-ways.mp4
[v03c]: https://github.com/cjohnstoniv/wardyn/releases/download/v0.6.0/wardyn-03c-authorized-then-issued.mp4
[v03d]: https://github.com/cjohnstoniv/wardyn/releases/download/v0.6.0/wardyn-03d-the-kinds-that-cant-use-a-header.mp4
[v04]: https://github.com/cjohnstoniv/wardyn/releases/download/v0.6.0/wardyn-04-add-a-workspace.mp4
[v05]: https://github.com/cjohnstoniv/wardyn/releases/download/v0.6.0/wardyn-05-your-first-policy.mp4
[v06]: https://github.com/cjohnstoniv/wardyn/releases/download/v0.6.0/wardyn-06-your-first-run.mp4
[v07]: https://github.com/cjohnstoniv/wardyn/releases/download/v0.6.0/wardyn-07-interactive-runs.mp4
[v08]: https://github.com/cjohnstoniv/wardyn/releases/download/v0.6.0/wardyn-08-autonomous-agent.mp4
[v09]: https://github.com/cjohnstoniv/wardyn/releases/download/v0.6.0/wardyn-09-record-a-run.mp4
[v10]: https://github.com/cjohnstoniv/wardyn/releases/download/v0.6.0/wardyn-10-approvals-and-egress.mp4

## Architecture at a glance

```mermaid
flowchart LR
  entry(["Human operator<br/>UI or wardyn CLI"])
  subgraph control["Control plane (trusted)"]
    wardynd["wardynd<br/>REST API + embedded UI<br/>policy · approvals · broker · audit"]
    pg[("Postgres<br/>append-only audit")]
    wardynd --> pg
  end
  subgraph sandbox["Per-run sandbox (UNTRUSTED) — gatewayless network"]
    %% rec declared first: else dagre routes the launch edge through it
    rec["wardyn-rec<br/>PTY recorder"]
    agent["Coding agent<br/>claude-code / codex-cli"]
    rec -->|"cast, brokered to wardynd"| proxy["wardyn-proxy<br/>L2 egress sidecar"]
    agent -->|"only path out"| proxy
  end
  entry --> wardynd
  wardynd -->|"launch (docker driver)"| agent
  proxy -->|"allowlisted L7, creds injected"| net(("Internet / APIs"))
```

A trusted control plane launches each run into an untrusted, gatewayless sandbox
whose only path out is the `wardyn-proxy` sidecar, credentials injected there.
Decision logs and masked casts flow back into the append-only audit log
([THREAT-MODEL.md](threatmodel/THREAT-MODEL.md) §8). Wardyn never *adds* power:
a run reaches at most what you can, clamped by policy.

## Honest security posture

What Wardyn does **not** defend against is published in full
([THREAT-MODEL.md](threatmodel/THREAT-MODEL.md)). Notable residuals:

- **The model-API channel is an unavoidable data-exit path.** Prompts and tool
  calls are logged; nothing stops an agent encoding data into a permitted
  prompt.
- **Domain fronting and DNS-tunnel exfil** need TLS interception, which ships
  only for operator-listed hosts (off by default) — most non-LLM egress stays
  opaque.
- **CC1/Fence shares the host kernel**, and the 1-hour minted-token window
  before revocation is minimized by TTL, never eliminated.
- **The UI sandbox gateway defaults to a shared browser origin** across runs,
  separated only by a path-scoped cookie, unless the operator sets a per-run
  origin template — the documented production default
  ([UI-SANDBOXES.md](docs/UI-SANDBOXES.md#4-deployment)).

## Status

**v0.6.0 (pre-alpha)** is the current release, adding capability grants,
Kubernetes as the base deployment story, `wardyn ssh`, governed UI sandboxes,
member-owned workspaces and a hash-chained audit log. Two deployment lanes,
both running real sandboxes, not one inverted into the other:

- **`deploy/compose`** — the local 10-minute trial. The only lane that runs on
  a laptop without a real cluster, and the only one with recorded demos
  (the Getting Started demo steps).
- **Kubernetes**: `helm install wardyn oci://ghcr.io/cjohnstoniv/charts/wardyn
  --version <release>` — the chart is published as a signed OCI artifact, so no
  clone and no `helm repo add`. See
  **[`deploy/helm/wardyn`](deploy/helm/wardyn/README.md)** — the deployment
  story: `make kind-quickstart` for a one-command real-cluster install, or a
  production Helm install onto your own Kubernetes. Not yet at Compose parity
  (see the chart README's "Known gaps").

Still unbuilt: SPIRE, OpenBao, an MCP gateway, arbitrary-domain TLS
interception, OTLP/OCSF sinks, SAML/SCIM-provisioned team SSO, Compose's own
L1 default-deny — see [ROADMAP.md](ROADMAP.md) and [CHANGELOG.md](CHANGELOG.md).

## License and governance

**Apache-2.0, and free for anyone to use for any purpose, commercial use
included — at any scale, with no fee, no seat limit, no registration and no
telemetry.** There is no paid edition and no hosted backend: every control above
is in this repo and runs on your infrastructure, or it doesn't run. There is also no CLA, which means no single party — including the
maintainer — can relicense this project's accumulated work later.

[`LICENSING.md`](LICENSING.md) is the one page to hand your legal team: the grant,
what the artifacts contain, redistribution obligations, patents, warranty, and the
disclosed risks. [`TRADEMARKS.md`](TRADEMARKS.md) covers what you may call things,
which Apache-2.0 §6 deliberately does not.

Every published image is cosign-signed with an attested SBOM and build
provenance — [`docs/VERIFY.md`](docs/VERIFY.md) shows how to check that yourself.

Contributor sign-off via DCO (`Signed-off-by`). CNCF Sandbox is the governance
target, not a status. Contributions welcome — see `CONTRIBUTING.md`.
