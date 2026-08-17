# Wardyn docs

Start at the repo [README](../README.md) — it, the in-product Getting Started
wizard and `/demos` are the first-run path. Everything here is the next question.

| If you want to… | Read |
|---|---|
| Run it locally and watch the boundary hold | [TRY-IT.md](TRY-IT.md) |
| Configure a deployment (every `WARDYN_*` variable, defaults, which binary reads it) | [ENV.md](ENV.md) |
| Author a run policy (every `RunPolicySpec` field, defaults, legal values) | [POLICIES.md](POLICIES.md) + [examples/policies/](../examples/policies/) |
| Run a governed sandbox from a pipeline, headless | [CI.md](CI.md) + [ci/](ci/) |
| SSH / sftp / port-forward / VS Code Remote-SSH into a run | [SSH.md](SSH.md) |
| Build against the API in Go, or with curl | [sdk.md](sdk.md) |
| Understand or debug a devcontainer / BYOI image build | [ENVBUILD.md](ENVBUILD.md) |
| Back up, restore, or upgrade a running deployment, or put it behind a corporate proxy | [OPERATIONS.md](OPERATIONS.md) |
| Run the blessed compose stack (no-login local mode, TLS) | [../deploy/compose/README.md](../deploy/compose/README.md) |
| Runnable sample workspaces, one per governance control | [../examples/](../examples/) |
| See which exported functions have no test (`make test-gaps`) | [TEST-GAPS.md](TEST-GAPS.md) |
| Swap a component (identity, secret store, recording, substrate) | [PLUGGABILITY.md](PLUGGABILITY.md) |
| Understand the design, or contribute | [../ARCHITECTURE.md](../ARCHITECTURE.md), [../CONTRIBUTING.md](../CONTRIBUTING.md) |
| Know what Wardyn does *not* defend against | [../threatmodel/THREAT-MODEL.md](../threatmodel/THREAT-MODEL.md) |
| See what is shipped vs. planned | [../ROADMAP.md](../ROADMAP.md), [../CHANGELOG.md](../CHANGELOG.md) |

## The video series

Ten short videos, in order — the whole product in about half an hour. *Links go
live with the v0.5.0 release assets.*

#### V01 — Getting started

One command to a governed host. 6:00–6:30 ·
[watch](https://github.com/cjohnstoniv/wardyn/releases/download/v0.5.0/wardyn-01-getting-started.mp4)

#### V02 — Your first run

A real agent does real work inside the boundary. 2:30–3:00 ·
[watch](https://github.com/cjohnstoniv/wardyn/releases/download/v0.5.0/wardyn-02-your-first-run.mp4)

#### V03 — The run cockpit

The terminal is the run. 2:00–2:30 ·
[watch](https://github.com/cjohnstoniv/wardyn/releases/download/v0.5.0/wardyn-03-run-cockpit.mp4)

#### V04 — Workspaces & secrets

What a run may touch, and what it never holds. 2:00–2:30 ·
[watch](https://github.com/cjohnstoniv/wardyn/releases/download/v0.5.0/wardyn-04-workspaces-and-secrets.mp4)

#### V05 — Approvals & egress

Once, this run, until, always. 2:30–3:00 ·
[watch](https://github.com/cjohnstoniv/wardyn/releases/download/v0.5.0/wardyn-05-approvals-and-egress.mp4)

#### V06 — Policies & confinement

Fence, Wall, Vault, one reusable policy. 1:30–2:00 ·
[watch](https://github.com/cjohnstoniv/wardyn/releases/download/v0.5.0/wardyn-06-policies-and-confinement.mp4)

#### V07 — Record Mode

Watch it once, enforce it forever. 2:30–3:00 ·
[watch](https://github.com/cjohnstoniv/wardyn/releases/download/v0.5.0/wardyn-07-record-mode.mp4)

#### V08 — Model access

The key never enters the box. 1:30–2:00 ·
[watch](https://github.com/cjohnstoniv/wardyn/releases/download/v0.5.0/wardyn-08-model-access.mp4)

#### V09 — CI & headless

One governed run, no human, an exit code. 2:00–2:30 ·
[watch](https://github.com/cjohnstoniv/wardyn/releases/download/v0.5.0/wardyn-09-ci-and-headless.mp4)

#### V10 — Audit & attach

Who did what, and watching live from two places. 2:00–2:30 ·
[watch](https://github.com/cjohnstoniv/wardyn/releases/download/v0.5.0/wardyn-10-audit-and-attach.mp4)

## Field reports

[adoption/](adoption/) is different in kind: point-in-time field reports from real
deployments, kept verbatim (including the gaps still open). They are evidence, not
guides — a report describes one host on one date and is never updated to match the
current release.
