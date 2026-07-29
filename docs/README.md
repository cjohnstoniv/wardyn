# Wardyn docs

Start at the repo [README](../README.md) — it, the in-product Getting Started
wizard and `/demos` are the first-run path. Everything here is the next question.

| If you want to… | Read |
|---|---|
| Run it locally and watch the boundary hold | [TRY-IT.md](TRY-IT.md) |
| Configure a deployment (every `WARDYN_*` variable, defaults, which binary reads it) | [ENV.md](ENV.md) |
| Run a governed sandbox from a pipeline, headless | [CI.md](CI.md) + [ci/](ci/) |
| Build against the API in Go, or with curl | [sdk.md](sdk.md) |
| Understand or debug a devcontainer / BYOI image build | [ENVBUILD.md](ENVBUILD.md) |
| Swap a component (identity, secret store, recording, substrate) | [PLUGGABILITY.md](PLUGGABILITY.md) |
| Understand the design, or contribute | [../ARCHITECTURE.md](../ARCHITECTURE.md), [../CONTRIBUTING.md](../CONTRIBUTING.md) |
| Know what Wardyn does *not* defend against | [../threatmodel/THREAT-MODEL.md](../threatmodel/THREAT-MODEL.md) |
| See what is shipped vs. planned | [../ROADMAP.md](../ROADMAP.md), [../CHANGELOG.md](../CHANGELOG.md) |

[adoption/](adoption/) is different in kind: point-in-time field reports from real
deployments, kept verbatim (including the gaps still open). They are evidence, not
guides — a report describes one host on one date and is never updated to match the
current release.
