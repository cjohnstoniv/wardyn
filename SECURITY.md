# Security Policy

Wardyn is a governance and isolation control plane for coding agents. We treat
security reports as first-class and we publish what we do **not** defend against
(see [`threatmodel/THREAT-MODEL.md`](threatmodel/THREAT-MODEL.md)) rather than
overclaim. Please read the threat model before reporting — known, already-published
residual risks are listed there and are out of scope for this process (see below).

## Reporting a vulnerability

**Do not open a public issue for a security vulnerability.**

Preferred channel: **GitHub private vulnerability reporting** — use the
repository's *Security → Report a vulnerability* (private advisory) flow. This
works with no email setup and keeps the report confidential until a coordinated
fix is released.

> Email channel `security@<project-domain>` is reserved and will be published once
> the project name and domain are finalized (the name "Wardyn" is a working
> placeholder pending trademark search). Until then, use the GitHub private
> advisory flow above.

In your report, please include:

- The affected component(s): `wardynd` (control plane, which is also where the
  docker runner driver runs), `wardyn-proxy`, `wardyn-rec`, `wardyn-git-helper`,
  the `wardyn` CLI, or a deployment artifact (`deploy/compose`, `deploy/helm`).
- The version / commit, deployment surface (Docker Compose), and Confinement
  Class in use (CC1/CC2/CC3 — CC3/Vault is experimental; see `ARCHITECTURE.md`
  "Security invariants" #5).
- A clear description, impact, and the most minimal reproduction you can provide.
- Which **security invariant** you believe is broken (see `ARCHITECTURE.md`):
  (1) secrets never enter the sandbox, (2) approval mints the credential,
  (3) L0 structural egress, (4) per-run identity with full attribution,
  (5) fail-closed / never overclaim, (6) audit append-only.

## What is in scope

Reports that demonstrate a break of a **claimed, shipped** control, for example:

- A path by which a credential or secret value reaches the sandbox process
  (env, disk, args, or a leaked bearer token) — invariant 1.
- Minting a credential whose scope exceeds the approved scope, or minting without
  an `APPROVED` approval in the same transaction — invariant 2.
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
`threatmodel/THREAT-MODEL.md §5`, and are not eligible as new reports (we already
disclose them — but a *more severe than documented* instance is in scope):

- The model-API channel as a data-exit path (logged, not blocked, by design).
- Domain-fronting / DNS-tunnel exfil on hosts that are not TLS-intercepted —
  interception ships only for operator-listed MITM-eligible hosts (opt-in, off by
  default); arbitrary-domain interception is the v0.5 target.
- Kernel 0-day on a CC1 (shared-kernel runc) host; gVisor-sentry 0-day on CC2.
- The `ld-linux`/`mmap` bypass of in-guest exec hooks (detection, not prevention).
- The bounded minted-token usage window before kill-switch revocation.
- Compromised platform operator / admin (no separation-of-duty until v1.0).
- Any control the threat model tags as building or planned rather than shipped —
  report *design* concerns via a normal issue, not this process.

## Published residual risk (lives in the threat model)

Two disclosures that used to be restated here — the unreachable
`golang.org/x/crypto/openpgp` advisory (GO-2026-5932, no fix available, 0 called
symbols, `govulncheck` in CI on every push) and the console admin token's at-rest
posture (`sessionStorage` by default, `localStorage` only if you opt in — script
that runs in the page can read it, which the response headers bound but do not
eliminate) — are
residual-risk disclosures, so they live with the others in
[`threatmodel/THREAT-MODEL.md` §5](threatmodel/THREAT-MODEL.md). They are
published, not hidden; they are out of scope for this process as written, and a
*more severe than documented* instance is in scope.

## Coordinated disclosure

- We aim to **acknowledge** a report within **3 business days** and to provide an
  initial assessment within **10 business days**.
- We follow coordinated disclosure with a default embargo of **90 days** from
  acknowledgement, or until a fix ships — whichever is sooner — and will agree on
  timing with the reporter.
- We credit reporters in the release notes and the advisory unless you prefer to
  remain anonymous.

## Supported versions

Wardyn is **pre-alpha**; interfaces are not stable and there is no LTS. Security
fixes land on the default branch and the most recent tagged release. Do not run
pre-alpha builds for production workloads.
