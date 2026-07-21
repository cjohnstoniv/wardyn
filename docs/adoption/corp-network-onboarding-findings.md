<!-- Copyright 2025 The Wardyn Authors -->
<!-- SPDX-License-Identifier: Apache-2.0 -->

# Corporate-network adoption: findings, local fixes, and open gaps (post-0.4.1)

**Date:** 2026-07-20
**Assessed against:** `v0.4.1`
**Context:** bringing up the containerized stack on a locked-down corporate laptop — a
TLS-intercepting forward proxy, an artifact mirror that does not serve every public package, cloud
model access via SSO (not static keys), a self-hosted SCM over SSH, and a container runtime running
inside a Linux VM (Rancher Desktop / Lima on macOS).

Everything below is **environment-agnostic**: no company names, hostnames, IPs, org identifiers, or
repository names. Substitute your own for the placeholders (`CORP_PROXY_HOST:PORT`,
`ARTIFACT_MIRROR_URL`, `MODEL_ENDPOINT`, `SCM_HOST`).

> **Maintainer note.** Several claims below were re-tested against the code and on a native Linux
> host with all four confinement runtimes installed. Where the test disagreed with the report, the
> **verified** result is recorded — see A2 and B4 in particular. That is not a criticism of the
> report; it is how the report got turned into correct fixes.

---

## A. Local changes (now upstreamed)

### A1. The `make setup` UI-build retry must not reinstall — reuse `node_modules`

**Problem.** On a mirror that proxies public npm metadata but does not host every tarball, the
0.4.1 pnpm-404 fallback still failed. The retry ran the full `make ui`
(`pnpm install --frozen-lockfile && pnpm build`), and the *reinstall* re-triggers package
*postinstall* scripts. Those fetch platform-specific dependencies (this lockfile fans out
`@tailwindcss/oxide-*` and `@esbuild/*` per platform) that the mirror also does not carry — so the
fallback died even though `node_modules` was already fully populated and `pnpm build` alone succeeds.

**Fixed in `scripts/up.sh`.** The retry now rebuilds `ui/dist` from an existing `node_modules` when
one is present and matches the lockfile, and only falls back to a full `make ui` when it is absent or
stale. The guard compares `ui/pnpm-lock.yaml` against pnpm's own copy of the lockfile it installed
from (`ui/node_modules/.pnpm/lock.yaml`), so a drifted tree still reinstalls, and
`vite`'s `emptyOutDir` means the rebuild can never ship a stale bundle.

**Honest ceiling:** a *fresh clone* has no `node_modules`, so this does nothing for a true cold
start — it fixes the second and later `make setup`. The failure message now names the
platform-postinstall case and asks for the exact package + URL, because the underlying question
(does a warm `--frozen-lockfile` re-hit the mirror at all?) could not be reproduced off-site.

### A2. Sandbox egress via a loopback-only host forward proxy

**Problem (the biggest one).** Some corporate connectivity clients expose their forward proxy on a
**loopback address only** (`127.0.0.1:PORT`). That is reachable from host processes but
**structurally unreachable from inside the container-runtime VM or any container** — their
`127.0.0.1` is their own. Verified consequences:

- Direct outbound TCP from any container fails (default bridge and per-run nets).
- The corp proxy's own routable IP is *also* unreachable from the VM.
- So `wardyn-proxy` has no usable upstream: an **approved** sandbox egress returned
  `200 Connection Established` and then **hung**. This blocked model calls, SCM, and the approve-path
  onboarding demos — on-corp and off, since the client forces all egress through its loopback proxy
  regardless of location.

**This part WAS a Wardyn bug.** The report generously called it "not a Wardyn bug." It was one:
`dialThroughUpstream` wrote the upstream `CONNECT` and then called `http.ReadResponse` **with no read
deadline**. An upstream that accepts TCP and never answers blocked forever, and because the MITM path
answers the agent's `200 Connection Established` *before* that dial, the operator saw an approved
request hang with nothing to act on. Fixed: the CONNECT handshake is now bounded, and the failure
surfaces as a normal dial error → the existing deny + 502. A regression test asserts a silent
upstream fails fast instead of hanging (it hangs, and fails, without the deadline).

**Also shipped:**
- **Detection.** Getting Started → Host proxy now **warns** when a detected proxy is bound to
  loopback, naming the symptom and the fix, instead of leaving it to be discovered at first launch.
  Visible in `wardyn setup status` too.
- **A relay.** `wardyn setup proxy-relay <listen-port> <proxy-port>` forwards a reachable port to the
  loopback-bound proxy, so the sandbox gets an address it can reach. Foreground, in Go (no Python
  dependency), and deliberately **not** auto-started or supervised — Wardyn owns no host daemons.
  It fails fast if nothing is listening on the target port.

```sh
wardyn setup proxy-relay 18080 CORP_PROXY_PORT     # host, foreground
wardyn secret set upstream-proxy-url               # paste http://<host-gateway>:18080
wardyn site-config apply corp-baseline.json        # reference the secret
```

`<host-gateway>` is the address your sandbox reaches the host on (e.g. the VM's gateway on
Rancher/Lima). Egress policy is still enforced by `wardyn-proxy` before anything reaches the relay.

### A3. Persisting the corp wiring across `make reset` / `make reset-all`

Both reset paths delete the Postgres volume, and the upstream-proxy secret **and** the site-config
live only there — so a plain `reset && setup` returned with broken egress and no step in between
where the operator could notice.

- `wardyn site-config get|apply` now exists, so the baseline is a file you can keep. It carries
  secret **names**, never values, so it is safe to store beside the repo.
- `make reset-all`'s manifest now says explicitly that the corporate baseline dies with the volume,
  and prints the capture command *before* asking for confirmation.
- Restore secrets with `wardyn secret set` (reads the value on stdin; never argv, never `.env`).

### A4. Native agent-CLI install works offline from a host-staged binary — no change needed

Confirmed the `CLAUDE_INSTALL=native` path is the right escape hatch when public npm is blocked and
the mirror hasn't onboarded the agent package. On a fully air-gapped build, staging the checksummed
native binary on disk and building with the native flag avoids npm entirely. **This shipped feature
is correct**; recorded here only as a confirmation that it solves the corp case.

---

## B. Structural gaps

### B4. Writable `local_dir` mounts under a VM-backed host *(re-scoped by testing)*

**Reported:** a `local_dir` workspace onboarded `writable: true` mounts `rw` at the Docker layer and
the agent *reads* it fine, but under CC2 (gVisor) writes were denied even on a `0777` host dir, with
the bind presented as owned by `nobody` (`dfltuid=4294967294`). CC1 (runc) worked.

**Verified on a native Linux host with all four runtimes** (identical `0777` host dir, agent uid
1000, `rw` bind):

| Confinement | Runtime | In-sandbox uid | Writable host mount? |
|---|---|---|---|
| CC1 (Fence) | runc | 1000 | **yes** |
| CC2 (Wall) | gVisor / runsc | 1000 | **yes** — did *not* reproduce |
| CC3 (Vault) | krun / libkrun | **0 (root)** | **yes** |
| CC3 (Vault) | Kata | 1000 | **yes** |

So **gVisor does not deny bind-mount writes as such**, and the repo's own live e2e already mounts
read-write at the strongest installed tier and passes. `4294967294` is the classic 9p/NFS **squash**
value, which points at the **macOS→VM file-sharing layer** (virtiofs / reverse-sshfs, as used by
Rancher Desktop and Lima) sitting *underneath* the runtime — not at gVisor itself.

**Shipped:** the Workspaces writable banner now names the VM-backed-host caveat, scoped to that case
rather than claiming "the Wall tier cannot write" — which would have been a *new* false statement of
exactly the kind the rest of this report is about. The agent entrypoint now also registers the
workspace as a git `safe.directory`, so a uid mismatch surfaces as itself rather than as git's
"detected dubious ownership".

**Still open:** the exact failing layer on a VM-backed macOS host. If you hit this, the useful
datapoint is `stat -c '%u %g' <workspace>` inside the sandbox under CC1 vs CC2 on the *same* mount.

### B5. New Run wizard showed a false "no model access" warning — fixed

The Access step correctly disables the cloud-model radio (that transport is operator-configured and
applied automatically at dispatch, overriding the per-run api-key selection). But it still defaulted
to the api-key option and rendered a warning that the run "will 404" — telling an operator whose
model access demonstrably works, in red, that they had none, and pointing them at a secret list
holding no model key.

**Fixed:** the preflight now asks the *same* resolver dispatch uses, so an operator-configured cloud
transport reports model access as provisioned; and the Review banner defers to that verdict instead
of guessing locally. When the preflight is absent or still loading, the local guess still applies, so
a genuine gap is never hidden.

### B1 / B2 / B3 — open, need a maintainer design call

Each is confirmed real. None is shipped, because each widens what a credential or an approval
authorizes, and that is a design decision rather than a patch.

- **B1 — no grant delivers a secret as a sandbox environment variable.** The grant kinds are
  `github_token`, `cloud_sts`, `api_key`, `git_pat`, `ssh_key`. `git_pat` wires git's credential
  helper; `api_key` is proxy-injected; none places a secret in the sandbox env. So a PAT-authenticated
  CLI or REST tool (anything reading a `*_TOKEN` env var, or `curl` against a REST API) **cannot be
  brokered at all**. Workaround today: log in inside the sandbox terminal on an *interactive* run and
  paste the token on stdin — fine interactively, useless for autonomous runs.
  *Ask:* an `env_secret` grant kind (secret name → sandbox env var; resident, masked, run-scoped).
  *Implementation note for whoever takes it:* the closest precedent is **not** `ssh_key` — it is the
  resident Bedrock SigV4 path, which already injects secret material at dispatch with no broker mint,
  no proxy route, and no agent-image change. Note that masking is **not** automatic; it is per-call-site.

- **B2 — `git_pat` approval is single-use, so there is no "approve once per sandbox".** `git_pat`
  installs a *standing* credential helper that git invokes on *every* operation, but approval is
  single-use: with `requires_approval: true` a `pull` then a `push` in one sandbox raise **two**
  approvals (the second mint returns `ErrAlreadyMinted`); with `requires_approval: false` the helper
  auto-mints silently for the whole session. No middle ground, so any iterative git workflow forces
  the operator to choose between click-per-op and standing auto-issue of a real personal credential.
  *Ask:* a per-run credential **lease** — approve once, cache for the run's lifetime (or a TTL),
  reuse across git ops in that run, revoke at run end. A lease widens what one approval authorizes,
  so the audit event must say so explicitly.

- **B3 — the `ssh_key` grant is clone-only and doesn't fit a bind-mounted workspace.** The key is
  minted at *clone* time, used, then wiped (git has no SSH credential-helper seam, so it must be
  resident briefly). A `local_dir` workspace has no clone step, so the key is provisioned at startup
  and wiped before the agent runs interactive git — leaving SSH `pull`/`push` unauthenticated.
  Combined with B2: for a mounted checkout whose remote is SSH, **neither grant fits an interactive
  pull-then-push**; the operator must rewrite the remote to HTTPS or accept clone-only SSH.
  *Ask:* a session-scoped SSH agent (or a run-lifetime key file with the same wipe-on-run-end
  guarantee) so SSH remotes work for interactive git on a mounted workspace.

---

## C. Feature proposals — a Wardyn-owned confinement runtime

These came out of B4: on a laptop the achievable confinement tier is dictated by whatever container
runtime the user already has, and the common ones each fall short somewhere. The unifying idea is to
let Wardyn own (and optionally provision) its own confinement runtime, so the tier is a Wardyn
guarantee rather than a property of the host's pre-existing daemon.

### C1. A working CC3 (microVM) path on macOS / Apple Silicon

**More of this is already shipped than the report assumed**, and the central unknown is now answered:

- `krun`/libkrun is **already a first-class CC3 runtime** in the runner, and the per-class runtime
  override (`{CC3: "..."}`) already exists and is documented. **No runner change is needed.**
- **The write path is proven** (table in B4): a libkrun microVM writes to a bind mount, and its guest
  runs as **root**, confirming the driver's own note. That was the spike the report said must be
  proven rather than assumed — it is now proven, on Linux, where libkrun is the same runtime.

**Two blockers the report did not find**, both worth knowing before anyone invests in this:

1. **One driver = one Docker endpoint.** The orchestrator *can* hold multiple substrates, but boot
   constructs exactly one. So "CC1/CC2 on your existing runtime, CC3 on a separate krunkit daemon"
   does **not** work today — pointing CC3 at another daemon moves *every* run there.
2. **The compose deployment cannot do CC3 at all.** The runner hands `/dev/kvm` into each agent
   container and looks up the `kvm` group on **wardynd's own** filesystem; the compose service is
   granted no device and no supplementary group, so the microVM never boots. CC3 is host-mode-only
   on compose today.

**Remaining unknown (macOS-only):** whether `podman machine` on Apple Silicon exposes `/dev/kvm`
inside its guest. `scripts/test-podman.sh` already probes exactly what gates this (the `Runtimes`
map) — reuse it rather than writing a new probe.

### C2. `make setup` provisioning Wardyn's own tier-2/3 runtime

**The premise needs correcting first.** The report describes `wardyn setup wall|vault` as printing
commands and "deliberately never installing anything." It already installs: under `--run` it
installs gVisor and Kata, and it already starts a VM on macOS. The promise that actually holds is
the narrower one written in the code — **wardynd (the daemon) is only ever *pointed at* the result**;
the CLI, under explicit operator consent, already provisions.

That makes the proposal a difference of degree rather than a new boundary crossing. **Recommendation:
adopt it, scoped to where the boundary already sits** — the CLI, under an explicit `--run` +
confirmation, running as the operator, never headless, with a documented uninstall. macOS currently
has no route to CC3 at all, which is the strongest argument for doing it.

---

## What was verified working end to end

- `make reset-all && make setup` → healthy stack, no manual `WARDYN_UI_STAGE` (A1).
- Model access over cloud SSO via the read-only credential mount (SSO auto-refresh).
- Sandbox internet egress through a host relay (A2) — model endpoint and SCM host reachable from
  inside a run with a real HTTP round-trip, where it previously hung.
- A `local_dir` workspace mounts and the agent reads the full tree; a stored policy attaches via
  `policy_id` and injects the mount + grant.
- Confinement, approvals, and the deny-path onboarding demos.

The environment is demanding but not unusual for a large enterprise: intercepting proxy, partial
mirror, SSO-only cloud creds, SSH SCM, VM-backed runtime. Each gap above is something the next such
adopter will hit.
