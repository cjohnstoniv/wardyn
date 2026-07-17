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

- The affected component(s): `wardynd` (control plane), `wardyn-runner`,
  `wardyn-proxy`, `wardyn-rec`, `wardyn-git-helper`, the `wardyn` CLI, or a
  deployment artifact (`deploy/compose`, `deploy/helm`).
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
- Domain-fronting / DNS-tunnel exfil below the unbuilt L2 TLS-intercept tier.
- Kernel 0-day on a CC1 (shared-kernel runc) host; gVisor-sentry 0-day on CC2.
- The `ld-linux`/`mmap` bypass of in-guest exec hooks (detection, not prevention).
- The bounded minted-token usage window before kill-switch revocation.
- Compromised platform operator / admin (no separation-of-duty until v1.0).
- Controls tagged **[v0.2 — building]** or **[v0.5+ — planned]** in the threat
  model that are not yet merged — report *design* concerns via a normal issue,
  not this process.

## Known latent vulnerabilities

We publish known-uncalled findings here rather than let them sit silently in a
scanner's ignore-list.

- **GO-2026-5932** — `golang.org/x/crypto/openpgp` is flagged unmaintained and
  unsafe by design, with **no fix available** (`Fixed in: N/A`). The
  `golang.org/x/crypto` *module* reaches our build via two dependency paths —
  `filippo.io/age` (used for our secret-encryption primitives) imports
  `chacha20poly1305`, `hkdf`, `curve25519`, and `scrypt`, and our OpenAI
  composer backend pulls it in through Azure identity
  (`internal/composer/backends/openai` → `github.com/Azure/azure-sdk-for-go/sdk/azidentity`
  → `golang.org/x/crypto/pkcs12`) — all sibling packages of `openpgp` in the same
  module, which pulls in the *module* as a build dependency. No Wardyn code
  path, and no dependency Wardyn actually calls, imports the `openpgp`
  subpackage itself (`go mod why golang.org/x/crypto/openpgp` confirms: "main
  module does not need package golang.org/x/crypto/openpgp"). `govulncheck`'s
  symbol-level analysis agrees: "Your code is affected by 0 vulnerabilities" —
  GO-2026-5932 shows up only in the module-level "modules you require" tally,
  not the call-graph-verified findings.
  We accept this as a latent, unreachable finding rather than vendoring or
  forking `x/crypto` to drop the subpackage: there is no upstream fix to take,
  and the flagged code is dead weight in our binary, never on an execution
  path. `govulncheck` runs in CI on every push (`.github/workflows/ci.yml`,
  job `govulncheck`, both the default and `-tags docker` builds) specifically
  so that if a future dependency bump ever puts `openpgp` on a *called* path,
  the symbol-level scan flips from "0 vulnerabilities" to a real finding and
  CI goes red — this entry is not a standing exemption from that check.

## Console auth token storage

The web console (`ui/`) authenticates to the control plane one of two ways, with
**different at-rest posture**:

- **Admin token (the single-operator local path, shipped today).** `wardynd`
  prints a full-admin bearer on startup; you paste it into the sign-in screen and
  it is attached as an `Authorization: Bearer` header on every `/api/v1` request.
  This token is a full-admin credential held in browser storage, so it carries
  **XSS-equivalent risk**: any script that runs in the console origin can read it.
  By default it is kept in **`sessionStorage`** and is gone when the tab/browser
  closes; ticking **"Remember on this device"** on sign-in persists it to
  `localStorage` instead (survives restart, larger exposure window). Both stores
  are same-origin and readable by injected script — the checkbox trades restart
  convenience for a shorter at-rest window, not for a stronger boundary.
- **SSO session (the hardened path, `[multi-user — coming soon]`).** The session
  is carried in an **`HttpOnly` cookie** that page script cannot read, so an
  injected script cannot exfiltrate it. This is the stronger posture; the
  admin-token path above is the local/single-operator convenience alternative.

**Mitigations that exist:** the token is never written to `localStorage` unless
you opt in; the console is served same-origin (no cross-origin token leak); the
input field uses `type="password"`/`autoComplete="off"`.

**Mitigations that do NOT yet exist (honest gaps):** the UI-serving path
(`internal/api/ui.go`) sets **no** `Content-Security-Policy`, `X-Frame-Options`,
or `X-Content-Type-Options` header, so there is no defense-in-depth against an
XSS or clickjacking sink beyond the token-storage choice itself. Adding those
headers is the tracked follow-up; until then, treat the admin token as a
plaintext full-admin credential and prefer the SSO path once it ships.

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
