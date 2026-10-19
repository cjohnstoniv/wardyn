# Security Policy

Wardyn is a governance and isolation control plane for coding agents. We treat
security reports as first-class and we publish what we do **not** defend against
(see [`threatmodel/THREAT-MODEL.md`](threatmodel/THREAT-MODEL.md)) rather than
overclaim. Please read the threat model before reporting — known, already-published
residual risks are listed there and are out of scope for this process (see below).

## Reporting a vulnerability

> [!WARNING]
> **Do not open a public issue for a security vulnerability.**

- Preferred channel: **GitHub private vulnerability reporting** — use the repository's *Security → Report a vulnerability* (private advisory) flow.
- This works with no email setup and keeps the report confidential until a coordinated fix is released.

> [!NOTE]
> Email channel `security@<project-domain>` is reserved and will be published once
> the project name and domain are finalized (the name "Wardyn" is a working
> placeholder pending trademark search). Until then, the **GitHub private
> advisory flow above is the canonical reporting channel** — no email address is
> live yet.

In your report, please include:

- The affected component(s): `wardynd` (control plane and docker runner driver),
  `wardyn-{proxy,rec,git-helper}`, `wardyn-runnerd`,
  `wardyn-{toolgate,scan,aws-sso,tetragon-ingest}`, the `wardyn` CLI, or a
  deployment artifact (`deploy/{compose,helm,desktop}`,
  [`deploy/runner/install.sh`](deploy/runner/install.sh)).
- The version / commit, deployment (Compose, Kubernetes/Helm,
  laptop runner or desktop), runner trust class (`personal|managed_device|org_cluster`),
  and Confinement Class in use (CC1/CC2/CC3 — CC3/Vault is experimental; see
  [`ARCHITECTURE.md`](ARCHITECTURE.md) "Security invariants" #5).
- A clear description, impact, and the most minimal reproduction you can provide.
- Which **security invariant** you believe is broken (see [`ARCHITECTURE.md`](ARCHITECTURE.md)):
  - (1) credentials stay out of the sandbox wherever Wardyn can broker them (the kinds that enter are in [THREAT-MODEL.md](threatmodel/THREAT-MODEL.md) section 5.1a),
  - (2) approval mints the credential, (3) L0 structural egress, (4) per-run identity with full attribution, (5) fail-closed / never overclaim, (6) audit append-only.

## Which credentials enter the sandbox

Wardyn keeps credentials out of the sandbox wherever it can broker them. The run's egress proxy adds these on the way out, and the sandbox holds a placeholder or nothing:

- Model-provider keys and tokens: Anthropic and OpenAI API keys, custom endpoints, Bedrock bearer keys and a Claude subscription sign-in.
- `api_key` grants, and the header secrets of components and integrations.
- GitHub App tokens.
- `git_pat` tokens while the PAT broker is on (the default).
- Each person's Azure DevOps credential.
- The Entra token behind the Azure CLI, and the GitHub CLI's token.
- The AWS sign-in session behind Bedrock and the AWS CLI (the default).

Some kinds do enter the sandbox:

- `env_secret` and `file_secret` grants, and component secrets delivered as a variable or a file. These stay for the whole run and cannot be revoked.
- `ssh_key` grants: the key file exists in the sandbox during the clone.
- `git_pat` with `WARDYN_GIT_PAT_BROKER=off`.
- The AWS sign-in token with `WARDYN_AWS_SSO_PROXY_INJECT=off`.
- The short-lived AWS role credentials that every `bedrock_sso` run and every AWS CLI component derives inside the sandbox.
- The Wardyn-minted toolgate credential file, a local credential the sandbox's own tool gate reads.
- The new credential inside a provider sign-in sandbox, which exists to create it.

Each of these is opt-in or depends on a setting. Wardyn scopes them where it can, masks them in the output it relays, and audits each delivery by name. On a laptop runner or a cluster runner, the runner host also holds values for the run. An allowed destination that echoes request headers can send a proxy-added credential back to the sandbox. [docs/CREDENTIALS.md](docs/CREDENTIALS.md) lists every kind, and [THREAT-MODEL.md](threatmodel/THREAT-MODEL.md) section 5.1a bounds each one.

## What is in scope

Reports that demonstrate a break of a **claimed, shipped** control, for example:

- A path by which a credential reaches the sandbox (env, disk, args, or a leaked bearer token) — invariant 1.
  - It must not be one of the documented exceptions in [THREAT-MODEL.md](threatmodel/THREAT-MODEL.md) section 5.1a.
  - A credential that outlives or exceeds the bounds stated there is also in scope.
- Minting a credential whose scope exceeds the approved scope, or minting a
  `requires_approval: true` grant without an `APPROVED` approval in the same transaction —
  invariant 2. A grant without `requires_approval` is policy-eligible and mints by design.
- Egress from a sandbox that does not traverse `wardyn-proxy` (a default route, a
  direct-IP path, a metadata-server reach) — invariant 3.
- Forging or stripping the `sub`/`act`/`sponsor` attribution chain — invariant 4.
- A control that is enforced more weakly than the documentation claims, or a
  documentation claim with no enforcing code (an **overclaim** — we consider these
  bugs, per invariant 5).
- Tampering with the append-only audit log, or bypassing the audit trail for an
  action that should be recorded — invariant 6.

## What is out of scope

The following are **published residual risks**, documented in
[`threatmodel/THREAT-MODEL.md §5`](threatmodel/THREAT-MODEL.md), and are not eligible as new reports (we already
disclose them — but a *more severe than documented* instance is in scope):

- The model-API channel as a data-exit path (logged, not blocked, by design).
- Domain-fronting / DNS-tunnel exfil on hosts that are not TLS-intercepted —
  interception ships only for operator-listed MITM-eligible hosts (opt-in, off by
  default); arbitrary-domain interception is planned for v1.0 (see [`ROADMAP.md`](ROADMAP.md)).
- Kernel 0-day on a CC1 (shared-kernel runc) host; gVisor-sentry 0-day on CC2.
- The `ld-linux`/`mmap` bypass of in-guest exec hooks (detection, not prevention).
- The bounded minted-token usage window before kill-switch revocation.
- Compromised **unbounded platform operator / super admin** (the `admin` tier is
  trusted by design and writes policy, site-config, secrets and the role map).
  - **In scope, and please do report:** anything that crosses the
    `security_admin`/super-admin boundary —
    - a principal holding only
      `security_admin` reaching a super-admin-only surface (`/access` role mappings,
      `POST /api/v1/admin/sandboxes/sweep`, attach / attach-ticket / take-over on a
      run they do not own, an SSH key stamped above `user`),
    - or a `user` reaching either admin tier.
  - A bypass of the four-eyes rule on egress approvals
    (`WARDYN_EGRESS_SECOND_HUMAN=1`) or on Azure DevOps capability escalations
    (`WARDYN_CAPABILITY_SECOND_HUMAN=1`;
    - it governs escalation decisions during a run, not the capability list an admin-tier creator sets at launch, which the organisation's ceiling bounds)
  - is likewise in scope — except by the admin
    token, which is a documented, deliberate exemption
    ([`threatmodel/THREAT-MODEL.md`](threatmodel/THREAT-MODEL.md) § "Four-eyes on egress approvals is bypassable by
    the admin token, by design").
  - Separation of duty WITHIN the super-admin tier
    remains a v1.0 item ([`ROADMAP.md`](ROADMAP.md)); separation between the two admin tiers ships
    in v0.7 and is a real boundary — residual #14 states which is which.
- Any control the threat model tags as building or planned rather than shipped —
  report *design* concerns via a normal issue, not this process.

## Published residual risk (lives in the threat model)

- Two disclosures that used to be restated here —
  - the unreachable `golang.org/x/crypto/openpgp` advisory (GO-2026-5932, no fix available, 0 called
    symbols, `govulncheck` in CI on every push)
  - and the console admin token's at-rest
    posture (`sessionStorage` by default, `localStorage` only if you opt in — script
    that runs in the page can read it, which the response headers bound but do not
    eliminate) —
- are residual-risk disclosures, so they live with the others in
  [`threatmodel/THREAT-MODEL.md` §5](threatmodel/THREAT-MODEL.md).
- They are published, not hidden; they are out of scope for this process as written, and a
  *more severe than documented* instance is in scope.

Dependency security tracking here is **advisory-lagged**:

- `govulncheck` matches known advisory IDs, so an upstream security release is invisible to it until an advisory is filed against that module —
  - go-jose/v4 v4.1.5 (2026-09-03) shipped seven upstream-labelled security fixes with no advisory, and the scan stayed green throughout.

Where a dependency sits on a token-verification path, the floor is therefore also held by a behavioural test that fails on a downgrade ([`internal/auth/oidc/r3b_jose_currency_test.go`](internal/auth/oidc/r3b_jose_currency_test.go)), not by the scan alone.

## Coordinated disclosure

- We aim to **acknowledge** a report within **3 business days** and to provide an
  initial assessment within **10 business days**.
- We follow coordinated disclosure with a default embargo of **90 days** from
  acknowledgement, or until a fix ships — whichever is sooner — and will agree on
  timing with the reporter.
- We credit reporters in the release notes and the advisory unless you prefer to
  remain anonymous.

## Supported versions

- Wardyn is **pre-alpha**; interfaces are not stable and there is no LTS.
- Security fixes land on the default branch and the most recent tagged release.
- Do not run pre-alpha builds for production workloads.
