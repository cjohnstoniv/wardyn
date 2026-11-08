# Wardyn docs

- Start at the repo [README](../README.md); the console's Getting started page then walks a first run and the built-in demos.

## Getting started

| Video | Who it's for | Page |
|---|---|---|
| Install | Admin | [TRY-IT.md](TRY-IT.md), [DESKTOP.md](DESKTOP.md), [operations/k8s-runners.md](operations/k8s-runners.md) |
| First governed run | Member | [NEW-RUN.md](NEW-RUN.md) |
| Approvals | Member and approver | [operations/approval-rules.md](operations/approval-rules.md) |
| A Mac inside your Mac | Developer | [operations/macos-vm-use.md](operations/macos-vm-use.md) |
| A unit admin's day | Unit admin | [operations/unit-admins.md](operations/unit-admins.md), [operations/three-roles.md](operations/three-roles.md) |
| Security admin setup | Security admin | [operations/security-admin-setup.md](operations/security-admin-setup.md) |
| CI | Platform engineer | [CI.md](CI.md), [ci-jobs-as-runs.md](ci-jobs-as-runs.md) |

## Evaluate

| If you want to… | Read |
|---|---|
| Run it locally and watch the boundary hold | [TRY-IT.md](TRY-IT.md) |
| Answer a vendor security questionnaire: what leaves the box | [DATA-FLOW.md](DATA-FLOW.md) |
| Verify a release artifact (cosign signatures, SBOM attestations) | [VERIFY.md](VERIFY.md) |
| Answer a data-subject request: what Wardyn retains | [OPERATIONS.md](OPERATIONS.md#retention-erasure-and-gdpr--a-residual-not-a-solved-problem) |
| State an export-control classification (ECCN, EAR) for a procurement form | [EXPORT.md](EXPORT.md) |
| Know how Wardyn is built: AI-written code under a human maintainer | [HOW-WARDYN-IS-BUILT.md](HOW-WARDYN-IS-BUILT.md) |
| Know what Wardyn does *not* defend against | [../threatmodel/THREAT-MODEL.md](../threatmodel/THREAT-MODEL.md) |
| Reason about agent risk in general: terms and threat categories | [../threatmodel/AGENT-THREAT-MODEL.md](../threatmodel/AGENT-THREAT-MODEL.md) |
| See what is shipped vs. planned | [../ROADMAP.md](../ROADMAP.md), [../CHANGELOG.md](../CHANGELOG.md) |

## Use a Wardyn someone else runs

| If you want to… | Read |
|---|---|
| Use a Wardyn someone else operates (you are a member) | [USERS.md](USERS.md) |
| Start a run from the console: New run steps, Review, templates | [NEW-RUN.md](NEW-RUN.md) |
| Reach a run over SSH, sftp, port-forward or VS Code Remote-SSH | [SSH.md](SSH.md) |
| Relay a UI app in a run (editor, dev server) to your browser | [UI-SANDBOXES.md](UI-SANDBOXES.md) |
| Keep a command running, with restarts and a health check | [SERVICES.md](SERVICES.md) |

## Operate a deployment

| If you want to… | Read |
|---|---|
| Configure a deployment (every `WARDYN_*` variable) | [ENV.md](ENV.md) |
| Back up, restore or upgrade a deployment, or put it behind a corporate proxy | [OPERATIONS.md](OPERATIONS.md) |
| Run the blessed compose stack (no-login local mode, TLS) | [../deploy/compose/README.md](../deploy/compose/README.md) |
| Deploy to a Kubernetes cluster (Helm chart, quickstart) | [../deploy/helm/wardyn/README.md](../deploy/helm/wardyn/README.md) |
| Run a daemon on each developer's managed laptop (MDM envelope, ceiling) | [DESKTOP.md](DESKTOP.md) + [../deploy/desktop/](../deploy/desktop/) |
| Register a self-hosted runner, and what it does offline | [runner-registration.md](runner-registration.md), [runner-offline.md](runner-offline.md), [operations/hybrid-laptops.md](operations/hybrid-laptops.md) |
| Decide where runs go: runner pools, limits, barriers, the queue | [operations/runner-pools.md](operations/runner-pools.md) |
| Offer Linux VM sandboxes (QEMU/KVM) from a Linux runner host | [operations/linux-vm-sandboxes.md](operations/linux-vm-sandboxes.md) |
| Offer macOS VM sandboxes from a managed Mac: requirements, switches, settings | [operations/macos-vms.md](operations/macos-vms.md) |
| Use a macOS VM sandbox: first run, keep a machine, windows, recording, folders | [operations/macos-vm-use.md](operations/macos-vm-use.md) |
| Prepare and customise a Mac's base image; set up the guest by hand | [operations/macos-vm-base-image.md](operations/macos-vm-base-image.md) |
| Run a Mac that offers macOS VMs: capacity, disk, the shared ledger, credentials | [operations/macos-vm-host.md](operations/macos-vm-host.md) |
| Fix a Mac that stopped offering macOS VMs; see what has not run on a real Mac | [operations/macos-vm-troubleshooting.md](operations/macos-vm-troubleshooting.md) |
| Run the organisation's own runner in a Kubernetes cluster | [operations/k8s-runners.md](operations/k8s-runners.md) + [design/0.9/k8s-runnerd.md](design/0.9/k8s-runnerd.md) + the `wardyn-k8s-runner-setup` Claude Code skill |
| Set up SSO (Entra ID / OIDC) and admin/member roles on a cluster | [operations/who-gets-in.md](operations/who-gets-in.md) + the `wardyn-k8s-setup` Claude Code skill |
| Run more than one wardynd replica, and see its limits | [operations/high-availability.md](operations/high-availability.md) |
| Require a signed image when a member creates a run (audit first, then enforce) | [operations/image-signature-checks.md](operations/image-signature-checks.md) |
| Make a cluster refuse unsigned Wardyn images (policy-controller or Kyverno) | [operations/image-admission.md](operations/image-admission.md) |
| Let an agent-sandbox controller make the agent pods (experimental) | [operations/kubernetes-agent-sandbox.md](operations/kubernetes-agent-sandbox.md) |
| See which run and person a Kubernetes kernel-sensor alert belongs to | [operations/kubernetes-ground-truth.md](operations/kubernetes-ground-truth.md) |
| Set up as a security admin: a first-setup checklist | [operations/security-admin-setup.md](operations/security-admin-setup.md) |
| Monitor a deployment: metrics, health and audit sinks | [operations/monitoring.md](operations/monitoring.md) |
| Rotate the age key, or use an external key store | [operations/secrets-and-keys.md](operations/secrets-and-keys.md) |
| Know what the shared-mount recording fallback's read confinement covers | [RECORDING-ROOT-READ.md](RECORDING-ROOT-READ.md) |
| Set anonymous-route rate limits behind a shared ingress (`WARDYN_TRUSTED_PROXIES`) | [operations/ingress-rate-limits.md](operations/ingress-rate-limits.md) |
| Sign a CLI component in | [operations/cli-component-sign-in.md](operations/cli-component-sign-in.md) |
| Take a recovery set and rehearse a restore | [operations/recovery-rehearsal.md](operations/recovery-rehearsal.md) |
| Set a run's toolchain, or build the Claude sign-in image | [operations/build-images.md](operations/build-images.md) |
| Brand the console as the organisation's own | [operations/console-branding.md](operations/console-branding.md) |
| See what the Kubernetes runner substrate does not do yet | [operations/kubernetes-known-gaps.md](operations/kubernetes-known-gaps.md) |
| Give each person persistent storage, and reclaim it | [operations/user-drives.md](operations/user-drives.md) |
| Offboard a person: sessions, tokens, runs, credentials, drives | [operations/ownership-and-offboarding.md](operations/ownership-and-offboarding.md) |
| Keep a run declared from a file or a WardynWorkload resource | [operations/workloads.md](operations/workloads.md) |
| Set a run's lease, extend it, or revive a kept run | [operations/run-lifetime.md](operations/run-lifetime.md) |

## Govern as an admin

| If you want to… | Read |
|---|---|
| Author a run policy (every `RunPolicySpec` field) | [POLICIES.md](POLICIES.md) + [examples/policies/](../examples/policies/) |
| Choose a barrier for an agent | [CONFINEMENT.md](CONFINEMENT.md) |
| See where a credential is while a run works, and each residual | [CREDENTIALS.md](CREDENTIALS.md) |
| Connect a model provider, or route model traffic through an AI gateway | [operations/integrations.md](operations/integrations.md#model-access-comes-from-a-model-provider), [operations/ai-gateway.md](operations/ai-gateway.md) |
| Sign people in to a custom model endpoint's identity service | [operations/endpoint-sign-in.md](operations/endpoint-sign-in.md) |
| Give each person their own Azure DevOps access | [AZURE-DEVOPS.md](AZURE-DEVOPS.md) |
| See which writes are admin-only or security-admin | [operations/admin-only-writes.md](operations/admin-only-writes.md) |
| Cap what one person or group may do: governance profiles, capability grants | [operations/three-roles.md](operations/three-roles.md) + [operations/capabilities.md](operations/capabilities.md) |
| Narrow what a member's own run may reach | [operations/member-runs.md](operations/member-runs.md) + [operations/capabilities.md](operations/capabilities.md) |
| Turn a governance write into a two-human decision | [operations/governance-writes.md](operations/governance-writes.md) |
| Set the Wardyn boundary around every run, and use break-glass | [operations/boundary.md](operations/boundary.md) |
| Let a security admin's rule approve a service identity's held requests | [operations/approval-rules.md](operations/approval-rules.md) |
| Stop sharing the admin token: a token per person, and for service use | [operations/api-tokens.md](operations/api-tokens.md) |
| Write the organisation's provider policy — console, CLI or MDM | [operations/provider-policy.md](operations/provider-policy.md) |
| Configure an org component, and what a launch leaves behind | [operations/custom-components.md](operations/custom-components.md) |
| Name a launch preset for a non-console launcher | [operations/launch-presets.md](operations/launch-presets.md) |
| Govern one part of the tree as a unit admin | [operations/unit-admins.md](operations/unit-admins.md) |
| See what a member sees, as an admin | [operations/member-mode.md](operations/member-mode.md) |
| Look up why a member was refused (`authz.denied` reasons) | [operations/denials.md](operations/denials.md) |
| Look up an audit action's meaning and emit site | [AUDIT-ACTIONS.md](AUDIT-ACTIONS.md) |

## Build on it

| If you want to… | Read |
|---|---|
| Run a governed sandbox from a pipeline, headless | [CI.md](CI.md) + [ci/](ci/) |
| Run each CI job as a confined one-shot run on a control plane you operate | [ci-jobs-as-runs.md](ci-jobs-as-runs.md) |
| Build against the API in Go, or with curl | [sdk.md](sdk.md) |
| Understand or debug a devcontainer / BYOI image build | [ENVBUILD.md](ENVBUILD.md) |
| Swap a component (identity, secret store, recording, substrate) | [PLUGGABILITY.md](PLUGGABILITY.md) |
| Find the layers of the code and the interface each extension implements | [EXTENDING.md](EXTENDING.md) |
| Runnable sample workspaces, one per governance control | [../examples/](../examples/) |

## Contribute

| If you want to… | Read |
|---|---|
| Understand the design, or contribute | [../ARCHITECTURE.md](../ARCHITECTURE.md), [../CONTRIBUTING.md](../CONTRIBUTING.md) |
| Plan a change: issues, branches, pull requests | [../CONTRIBUTING.md](../CONTRIBUTING.md#branching-issues-and-pull-requests) |
| Write or edit a doc: form, size caps, links, visuals | [STYLE.md](STYLE.md) |
| Design or review a console screen | [design/CONSOLE-RULES.md](design/CONSOLE-RULES.md) |
| Read a design note, current or a record | [design/README.md](design/README.md) |
| See which exported functions have no test (`make test-gaps`) | [TEST-GAPS.md](TEST-GAPS.md) |
| Run the live tests against real Entra/ADO/Bedrock | [LIVE-TESTS.md](LIVE-TESTS.md) |
| Prepare or cut a release | [../RELEASING.md](../RELEASING.md#how-a-release-is-prepared) |
| Record or re-record the demo video series | [DEMO-SCRIPT.md](DEMO-SCRIPT.md) |

## Field reports

- [adoption/](adoption/) is different in kind: point-in-time field reports from real deployments, kept verbatim (including the gaps still open).
- They are evidence, not guides — a report describes one host on one date and is never updated to match the current release.
