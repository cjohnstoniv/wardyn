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
| Relay a UI app inside a run — a code editor, a dev server — to your browser | [UI-SANDBOXES.md](UI-SANDBOXES.md) |
| Build against the API in Go, or with curl | [sdk.md](sdk.md) |
| Understand or debug a devcontainer / BYOI image build | [ENVBUILD.md](ENVBUILD.md) |
| Back up, restore, or upgrade a running deployment, or put it behind a corporate proxy | [OPERATIONS.md](OPERATIONS.md) |
| Run the blessed compose stack (no-login local mode, TLS) | [../deploy/compose/README.md](../deploy/compose/README.md) |
| Deploy to a Kubernetes cluster (Helm chart, quickstart, k8s runner substrate) | [../deploy/helm/wardyn/README.md](../deploy/helm/wardyn/README.md) |
| Run a local daemon on each developer's managed laptop (MDM envelope, and its ceiling) | [DESKTOP.md](DESKTOP.md) + [../deploy/desktop/](../deploy/desktop/) |
| Set up SSO (Entra ID / OIDC) and admin/member RBAC on a cluster install | [OPERATIONS.md](OPERATIONS.md#multi-user-who-can-change-what) + the `wardyn-k8s-setup` Claude Code skill |
| Runnable sample workspaces, one per governance control | [../examples/](../examples/) |
| See which exported functions have no test (`make test-gaps`) | [TEST-GAPS.md](TEST-GAPS.md) |
| Swap a component (identity, secret store, recording, substrate) | [PLUGGABILITY.md](PLUGGABILITY.md) |
| Understand the design, or contribute | [../ARCHITECTURE.md](../ARCHITECTURE.md), [../CONTRIBUTING.md](../CONTRIBUTING.md) |
| Know what Wardyn does *not* defend against | [../threatmodel/THREAT-MODEL.md](../threatmodel/THREAT-MODEL.md) |
| See what is shipped vs. planned | [../ROADMAP.md](../ROADMAP.md), [../CHANGELOG.md](../CHANGELOG.md) |

## Field reports

[adoption/](adoption/) is different in kind: point-in-time field reports from real
deployments, kept verbatim (including the gaps still open). They are evidence, not
guides — a report describes one host on one date and is never updated to match the
current release.
