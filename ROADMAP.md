# Wardyn roadmap

- Wardyn is **pre-alpha**. Interfaces are not stable and there is no semantic-versioning promise.
- Nothing here is a date commitment; planned rows are ordered by intent.
- [CHANGELOG.md](CHANGELOG.md) holds the detail of every release.
- [docs/PLUGGABILITY.md](docs/PLUGGABILITY.md) says which implementations exist for each seam and which are only an interface.

## Shipped

| Milestone | Highlights | Status |
|---|---|---|
| **v0.1** | Per-run identity, approval FSM, credential broker, L2 egress proxy, append-only Postgres audit and PTY replay, CC1/CC2 confinement gating | **Shipped (pre-alpha)** |
| **v0.2** | Docker-only: secret-output masking, eBPF/Tetragon ground-truth audit stream, pinned seccomp and AppArmor, interactive attach sessions, control-plane TLS, a conformance gate and supply-chain CI | **Shipped (pre-alpha)** |
| **v0.3** | CI mode: headless launches with no pre-running control plane (`wardyn run --wait`, `--image`, `task_mode: exec`, [`scripts/ci-run.sh`](scripts/ci-run.sh)); from v0.3.1, repo-scoped git egress through the proxy-side git broker ([docs/CI.md](docs/CI.md)) | **Shipped (pre-alpha)** |
| **v0.4** | Containerized setup as the default, credential CLI, YAML policies, container workspaces with their own model credentials, Bedrock SSO, and corporate-network build and egress lanes | **Shipped (pre-alpha)** — `v0.4.0` to `v0.4.5` (see [CHANGELOG.md](CHANGELOG.md)) |
| **v0.5** | Kubernetes runner substrate and Helm deploy, native SSH into a run, admin and member RBAC with owner scoping, signed release images | **Shipped (pre-alpha)** — `v0.5.0`, 2026-08-18 (see [CHANGELOG.md](CHANGELOG.md)) |
| **v0.6** | Cloud deployment and real permissioning: capability grants, Kubernetes as the base deployment, `wardyn ssh`, governed UI sandboxes | **Shipped (pre-alpha)** — `v0.6.0` (see [CHANGELOG.md](CHANGELOG.md)) |
| **v0.7** | Governance an organisation can delegate: Workspace Providers, an agent roster, per-person Azure DevOps access on Entra ID, sealed stored credentials. 0.7.13 refuses to start on a database a newer wardynd migrated, so upgrade to it before 0.8 | **Shipped (pre-alpha)** — `v0.7.0` to `v0.7.13`, 2026-09-09 to 2026-09-28 (see [CHANGELOG.md](CHANGELOG.md)) |
| **v0.8** | Least-privilege and per-person Azure DevOps tokens, Azure Key Vault and Transit key custody, audit retention, multi-replica HA, SCIM leaver deprovisioning, run-level custom components, a four-panel New Run, one noun-verb CLI | **Shipped (pre-alpha)** — `v0.8.0` to `v0.8.9`, 2026-09-29 to 2026-10-09 (see [CHANGELOG.md](CHANGELOG.md)) |
| **v0.9** | Hybrid local and remote runs: client-mode runners on laptops and in Kubernetes, runner pools and a run queue, runner-resident credentials, a redesigned New Run with run documents and templates ([design](docs/design/0.9/PLAN.md)) | **Shipped (pre-alpha)** — `v0.9.0` to `v0.9.9`, 2026-10-13 to 2026-10-28 (see [CHANGELOG.md](CHANGELOG.md)) |
| **v0.10.0** | Developer sandboxes: Linux VM and macOS VM sandboxes (macOS is a preview, off by default), organisation walls, local model servers, folders from your computer, desktop controls, services. The macOS parts have not run end to end on a real Mac | **Shipped (pre-alpha)** — `v0.10.0-rc.1` to `v0.10.0-rc.13`, 2026-10-24 to 2026-11-04 (see [CHANGELOG.md](CHANGELOG.md)) |
| **v0.10.0-rc.14** | rc.14: image signature admission, signed audit receipts, per-run model token usage, MCP step-up approval, Idempotency-Key on run creation, macOS GUI sessions; Docker runs need Engine 28 or later. The macOS parts are not retested on a real Mac. | **Shipped (pre-alpha)** — `v0.10.0-rc.14`, 2026-11-07 (see [CHANGELOG.md](CHANGELOG.md)) |
| **v0.10.0-rc.15** | rc.15: a refused request no longer keeps a run awake, gateway rerouting keys refused under the allowed-models list, cleaner macOS VM bases, the guest agent signed in-process. The macOS parts are not retested on a real Mac. | **Shipped (pre-alpha)** — `v0.10.0-rc.15`, 2026-11-08 (see [CHANGELOG.md](CHANGELOG.md)) |
| **v0.10.0-rc.16** | rc.16: unit deciders for held approvals, platform envelopes, file download approvals, kubectl restart and per-write approval, model gateway and detector pages. | **Shipped (pre-alpha)** — `v0.10.0-rc.16`, 2026-11-08 (see [CHANGELOG.md](CHANGELOG.md)) |
| **v0.10.0-rc.17** | rc.17: macOS VM idle and Setup Assistant fixes, working edit-base buttons, `update-tools`, and credentials withheld unless a policy names their host exactly. The macOS parts are not retested on a real Mac. | **Shipped (pre-alpha)** — `v0.10.0-rc.17`, 2026-11-09 (see [CHANGELOG.md](CHANGELOG.md)) |
| **v0.10.0-rc.18** | rc.18: the rc.17 macOS VM login fix, per-host method and path rules, the IP-address switch, a No network preset, proxy policy confirmation and DNS audit rows. The macOS parts are not retested on a real Mac. | **Shipped (pre-alpha)** — `v0.10.0-rc.18`, 2026-11-10 (see [CHANGELOG.md](CHANGELOG.md)) |
| **v0.10.0-rc.19** | rc.19: computer folders kept until their copy back finishes, Docker network edits and revives that fail closed, a clear refusal for live network edits on Kubernetes, exact answers for exec on background runs and for No network launches, and Go 1.26.11. | **Shipped (pre-alpha)** — `v0.10.0-rc.19`, 2026-11-11 (see [CHANGELOG.md](CHANGELOG.md)) |

## Planned

Everything below is planned and undated unless a row says it shipped.

### v0.10

- The flagship is developer sandboxes (epic #2944): a real machine for a developer or their agent, with a desktop and a browser, inside nested walls.
- Each layer can only narrow: the organisation's outer limits, then teams, the person, the sandbox and the agent.

Built, from 0.10.0-rc.1 to rc.13:

| Item | Status |
|---|---|
| The Wardyn boundary with a dry-run report, bound super admins and an audited break-glass, every limit naming its layer, a final seal | Built |
| Linux VM sandboxes, with the proxy as the VM's only network | Built |
| macOS VM sandboxes on company-managed Macs, and `wardyn window` for the Mac's own window | Built as a preview, off by default |
| A governed local-model endpoint for the person's own GPU | Built |
| Folders on your computer in remote sandboxes | Built |
| Desktop in the console, Needs you with take-over, clipboard and file-transfer controls, desktop recording, sign-ins inside a desktop | Built |
| Scheduled runs (`WARDYN_SCHEDULED_RUNS`), several agents in one run (`WARDYN_MULTI_AGENT_TASKS`), in-sandbox MCP servers (`WARDYN_MCP_LOCAL_SERVERS`), kubectl (`WARDYN_KUBECTL_CLI`) | Built, off by default |
| Team-shared drives, keeping a run's work folder as a drive, services and inbound apps | Built |
| Session continuity across sign-ins (#1995) and session lifetime (#1996, `WARDYN_OIDC_SESSION_RENEW_CAP`, `WARDYN_OIDC_SESSION_IDLE_TIMEOUT`) | Built |

Not built:

| Item | Issue |
|---|---|
| Proof of a Mac's identity by an MDM device certificate (a Mac is trusted today by its `managed_device` class and posture file) | #2975 |
| Windows sandboxes from organisation-supplied images under a licence pool | #2955 |
| A local native window for sandboxes other than macOS VMs | #2961 |
| GPU in container sandboxes, and an opt-in GPU desktop for Linux VMs | #2958, #2959 |
| Snapshots, fork, warm pools and sleep on disconnect | #2960 |
| Per-setting delegation, agent rules, place rules | #2947, #2949, #2950 |
| Live two-way folder sync with a conflict view, and organisation controls for local folders | #2987, #2990 |
| Pause on every runtime, cold move between machines, and organisation sites with a reference edge box | #2991 |
| Preview URLs, in-browser approvals, action gates, watermark, browser lock-down, Record Mode for desktops | #2965, #2964, #2971, #2969, #2970, #2966 |
| Google Cloud CLI access | #1990 |
| The AI composer | #1993 |

### v1.0

| Item | Status |
|---|---|
| SPIRE identity provider and OpenBao secret store | Not built; the `identity.Provider` and `secretstore.Store` seams ship |
| L3 MCP and tool gateway | Partly built: the per-tool-call decision plane ships. Interception of tool-call egress that bypasses the proxy does not ([THREAT-MODEL.md](threatmodel/THREAT-MODEL.md)) |
| Arbitrary-domain L2 TLS interception | Not built; targeted model and registry interception ships, opt-in |
| Cloud STS federation | Not built |
| OCSF SIEM sink | Not built; file, webhook, syslog and OTLP sinks ship |
| Docker and Compose L1 default-deny by nftables | Not built; Docker relies on L0 structural confinement. Kubernetes L1 ships as a NetworkPolicy with a boot canary |
| Cilium `toFQDNs` | Not built |
| High-availability completion | Open blockers a second replica still hits, chiefly that a registered runner holds its session with one replica; see [High availability](docs/operations/high-availability.md) |
| Kubernetes parity with Docker | Open gaps listed in [deploy/helm/wardyn/README.md](deploy/helm/wardyn/README.md) "Known gaps" |
| CC3/Vault (Kata) packaged and GA | Experimental |
| Signed action receipts | Not built; the audit hash chain ships (migration `0047`) |
| Separation of duty on the control plane (super-admin tier) | Not built; `security_admin` separates governance authority ([THREAT-MODEL.md](threatmodel/THREAT-MODEL.md)) |

### Git-token ref confinement

- The proxy-side push-ref check is on by default and binds the brokered GitHub App path (`WARDYN_GIT_BROKER_ENFORCE_BRANCH_NS`, `agent-run` pushes `wardyn/<run-id>/work`).
- A GitHub installation token cannot restrict itself to a ref prefix. Wardyn reads a repository ruleset back ([`internal/broker/ruleset.go`](internal/broker/ruleset.go)), grades it on the setup checklist, and refuses every `github_token` mint until one verifies when `WARDYN_GITHUB_REQUIRE_REF_RULESET` is on (default off).
- Not built: Wardyn never creates the ruleset, since that needs repo-admin access it does not request; creating one stays an operator step ([docs/POLICIES.md](docs/POLICIES.md)).
- `git_pat` pushes have receive-pack parsing (the pat scope of the same setting, default off) and `push_rules`. Only `ssh_key` sits outside any receive-pack parser, because git's SSH transport has no broker seam.

### Named gaps without a milestone

These are known ceilings. None is scheduled. Accepted residuals of the security model are in [THREAT-MODEL.md](threatmodel/THREAT-MODEL.md), not here.

- **`--agent` is required for a run that names no image.** A `task_mode=exec` run with an `--image` or an attached workspace needs no agent. A bare `wardyn run --task-mode exec --task 'echo hi'` is refused `400` because there is nothing to run it in. Defaulting the agent to `claude-code` would make an agentless run eligible for the operator's subscription credential.
- **GitLab PATs are kept out of the sandbox but not scoped.** A `git_pat` for a non-GitHub forge is injected at the proxy. GitLab has no token-minting integration, so the operator PAT's own scope is the boundary, and `WARDYN_GIT_PAT_BROKER` controls residency, not least privilege. Azure DevOps mints a run-scoped PAT in the person's name ([docs/AZURE-DEVOPS.md](docs/AZURE-DEVOPS.md)).
- **An `ssh_key` grant covers the start-up clone only.** The proxy's SSH agent closes when the clone finishes, so a `local_dir` workspace, which has no clone, has no SSH credential for pull or push. Rewrite the remote to HTTPS.
- **Team mode as a sealed multi-user product.**
  - Shipped: admin and member roles, owner scoping, capability grants and per-user `wdn_` tokens ([docs/OPERATIONS.md](docs/OPERATIONS.md) "Multi-user: who can change what").
  - Not built: SAML, SCIM provisioning of joiners and movers (SCIM removes leavers only; see "Leavers and SCIM"), a tenant structure, and custom roles beyond admin and member.
  - The admin token and local mode stay one shared always-admin credential.
- **Age-key rotation is offline.** `wardynd -rotate-age-key` re-encrypts every stored secret in one transaction ([docs/operations/secrets-and-keys.md](docs/operations/secrets-and-keys.md#rotating-the-age-key)). The daemon must be stopped and nothing enforces that. There is no scheduled rotation.
- **A member's inline model-access grant needs an operator integration.**
  - `filterUserGrants` ([`internal/api/inline_policy.go`](internal/api/inline_policy.go)) drops a `{host, secret}` pairing the operator did not list as eligible, and audits the drop as `authz.denied` with reason `grant_pairing_not_eligible`.
  - A member whose model access rests only on a raw operator secret, with no model provider and no workspace requirement, launches without model access.
  - The supported flow, a model provider or a workspace requirement, is unaffected.
- **Drives.** Not built: cloud-drive providers (rclone, OneDrive), csi `subDir` templating, and wardynd performing NFS or SMB mounts itself. Self-service reset and a same-drive collision warning are non-goals ([docs/USERS.md](docs/USERS.md), [docs/design/user-drives-prompt.md](docs/design/user-drives-prompt.md)).

## What is not on the roadmap

- **Bring-your-own arbitrary Kubernetes manifests.** One blessed Helm chart, or nothing.
- **A feature that passes on only one target.** A feature is not done until it passes the conformance suite on Docker and kind ([ARCHITECTURE.md](ARCHITECTURE.md) "Parity rule"). The Kubernetes substrate is not at feature parity yet; see its "Known gaps" above.
- **A paid or open-core edition.** Apache-2.0 everything.
- **Interactive tool-approval routing through the toolgate FSM.** `wardyn-toolgate` routes only a non-interactive run's tool calls to the approval FSM. Upstream pins `claude`'s `--permission-prompt-tool` to non-interactive use, and the only alternative, a hook, fails open on a timeout. A person can already attach to the run and answer the prompt. An interactive run's `tool_approvals=hold` is refused `400` naming the field.
