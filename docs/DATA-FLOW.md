# Data flow & sub-processors

A one-page answer to the question a vendor-security questionnaire always asks
first: **where does data leave, and who receives it?** Every claim below cites
the code that makes it true; there is no separate "trust us" layer.

## No vendor telemetry from `wardynd`

Wardyn is self-hosted: the control plane (`wardynd`) makes **zero** outbound
calls to any Wardyn-owned or Wardyn-operated service. There is no phone-home,
no crash-reporting beacon, no usage-analytics endpoint, and no update-check
ping.

This was not always true — 0.4.x shipped, then removed, a funnel-tracking
beacon (`POST /api/v1/runs/compose/telemetry`; see `CHANGELOG.md`'s
"Removed" entry for 0.4.4/0.4.5). The route now 404s and nothing calls it,
and `internal/api/routes.go` has no telemetry endpoint. Every destination
`wardynd` itself dials is one of the operator-configured integrations below —
there is no hardcoded destination that isn't either a customer-supplied
integration or bundled open-source infrastructure the operator runs
themselves (compose's local registry/Dex/Postgres, all on the operator's own
Docker network). The one *literal* hostname in the daemon's own code is
Microsoft Graph, in the identity-directory connector (`internal/directory`),
and it is dialled only when an operator sets `WARDYN_DIRECTORY_PROVIDER` — the
directory-connector row below, and `docs/OPERATIONS.md`'s "Directory
autocomplete: the one path where the daemon dials out".

The one telemetry source in the system that is *not* Wardyn's is the agent
harness's own: Claude Code ships its own Datadog usage telemetry
(`http-intake.logs.us5.datadoghq.com`), which egress policy sees and can
gate like any other host — it is Anthropic's product behavior, not Wardyn's.
Wardyn suppresses it by default inside the sandbox; set
`WARDYN_ALLOW_AGENT_TELEMETRY=1` to let it through.

## Outbound destinations, by component

| Component | Talks to | Configured by | Purpose |
|---|---|---|---|
| **`wardyn-proxy`** (the **L2** egress gateway every sandboxed process's traffic transits) | Whatever host the run's policy allowlists — typically an LLM API (`api.anthropic.com`, `api.openai.com`, a Bedrock regional endpoint), a git host, or an operator-declared integration host | The operator, per-policy (`docs/POLICIES.md`) | The model-API and any other allowed egress for the run. This is the one channel the threat model names as an unavoidable data-exit path by design (`threatmodel/THREAT-MODEL.md` §5.1) — the proxy logs it, it does not silently hide it. |
| **git broker** (`internal/broker`) | `github.com` / a GitHub Enterprise host, or `dev.azure.com` for Azure DevOps | The operator, via the configured SCM integration | Mints short-lived, scope-bound git credentials (installation tokens / PATs) — never a long-lived credential resident in the sandbox. |
| **OIDC auth** (`internal/auth/oidc`) | The operator's own IdP issuer URL (`WARDYN_OIDC_ISSUER`) — in the bundled Docker Compose dev stack this is a local Dex sidecar the operator runs themselves, not a Wardyn-hosted service | The operator | Human SSO login: OIDC discovery, JWKS, and the token exchange at `/auth/callback`. Group/directory *reads* are a separate opt-in component — the row below. |
| **Audit sinks** (`internal/audit/sinks`: `webhook.go`, `syslog.go`) | An operator-configured SIEM webhook URL or syslog endpoint (file sink is local-disk-only, no network) | The operator, opt-in per sink (off by default) | Streams the append-only audit log to the operator's own SIEM. Each event carries its hash-chain `prev_hash`/`row_hash` (migration `0047`), so what the SIEM holds off-box is a head hash Wardyn cannot later disown — that comparison, not anything in the database, is what detects a rewritten or truncated trail (`docs/OPERATIONS.md`). Delivery loss on that endpoint being unreachable is a tracked gap, not a claim of guaranteed delivery. |
| **Agent image pulls** (Docker daemon `docker pull`) | `ghcr.io/cjohnstoniv/agent-*` by default (Wardyn's own published OCI images — public, unauthenticated pulls, no data sent besides the standard registry protocol), or an operator-supplied registry via `WARDYN_AGENT_IMAGES` (`docs/ENV.md`) | Ships with a default; fully overridable by the operator | Pulls the agent runtime container image. No source code, secrets, or run data is part of this pull — it is a one-way image download. |
| **Upstream corporate proxy** (optional, `docs/OPERATIONS.md`'s "upstream-proxy" section) | A URL the operator pastes in as a secret | The operator | Lets `wardyn-proxy` itself egress through a corporate forward proxy — this is the operator routing Wardyn's own egress through infrastructure *they* control, the reverse direction of a sub-processor relationship. |
| **Identity-directory connector** (`internal/directory`, off unless `WARDYN_DIRECTORY_PROVIDER=entra`) | `graph.microsoft.com`, `login.microsoftonline.com` — the only literal hostnames in the daemon's own code (`internal/directory/entra.go`) | The operator, by setting `WARDYN_DIRECTORY_PROVIDER` (unset = the whole feature off, `503 directory_unconfigured`, no connector and no read) | Backs the console's "who" autocomplete: reads users and groups (and App Roles where consented) from the operator's OWN Entra tenant so an admin picks a name and Wardyn stores the claim value. **Daemon-side outbound HTTPS**, not sandbox egress and not proxied by the egress sidecar — the same class as the IdP row above. Suggestions are cached in memory for 60s and never persisted; unsetting the var fully retracts the capability. |

Every row above is either (a) a destination the operator explicitly configures
(an integration, an IdP, a SIEM endpoint, a registry override, the directory
connector), or (b) a default that ships with the product and does nothing
beyond that default's stated, narrow purpose (pulling the agent image). None
of it is Wardyn collecting data *from* the deployment and sending it *to*
Wardyn.

## Sub-processors are the operator's own choices, not Wardyn's

Wardyn is not a SaaS with a fixed sub-processor list — it is software the
operator runs on infrastructure they control. Every third party a deployment
actually talks to (Anthropic, OpenAI/Azure OpenAI, AWS Bedrock, GitHub /
GitHub Enterprise, Azure DevOps, the organization's own IdP, the
organization's own SIEM) is a **customer-supplied integration**: the operator
holds the account, the credentials, and the data-processing relationship with
that vendor directly. Wardyn's own outbound reach, as the table above shows,
is limited to routing traffic *to* those operator-chosen destinations and
pulling its own OCI image — Wardyn itself is never a party the operator's
data passes through to reach a fourth party.

## What is logged where

- **Application audit trail** — Postgres, append-only (`internal/db/migrations/0001_init.sql`,
  `0004`; see `docs/OPERATIONS.md`'s audit-retention section for the
  retention/erasure posture). Never leaves the deployment unless the operator
  wires an audit sink (above).
- **Session recordings** (asciinema PTY capture) — Postgres, subject to the
  age-based retention knob (`docs/ENV.md:47`). Never leaves the deployment.
- **Kernel ground-truth stream** (optional eBPF/Tetragon) — lands in the same
  Postgres audit table (`internal/groundtruth`), same retention, same
  non-export-by-default posture.
- **Model prompts/completions** — traverse `wardyn-proxy` (row 1, above) to
  whichever model API the policy allows; this is the one path the threat
  model explicitly does not claim to close (§5.1a, `llm_inspection` narrows
  it for the honest-agent case but is not exfil-proof).

## Related documents

- `threatmodel/THREAT-MODEL.md` §5 — the full published residual-risk list,
  including the model-API exit path this page summarizes.
- `docs/OPERATIONS.md` — audit retention/erasure, upstream-proxy setup,
  audit-sink configuration.
- `docs/POLICIES.md` — how egress destinations are allowlisted per run.
- `docs/AUDIT-ACTIONS.md` — the full audit action vocabulary (what gets
  recorded, not where it's sent).
