# Wardyn agent images

This directory contains the OCI image definitions for coding-agent sandboxes
governed by Wardyn.  Each subdirectory is one agent image.

## Image contract

Every image that runs a real agent task MUST satisfy the invariants below.  What
actually enforces them: `internal/envbuild/builder.go`'s `requiredTools` plus a
fail-closed per-run `agent-run --selftest` for wrapped/BYOI images
(`internal/api/runs_dispatch.go`, `make test-e2e-byoi`), and the live e2e suite
for the images its task corpus actually launches — `claude-code` and `oracle`
(`test/e2e/live/live_test.go`); `codex-cli` and `aws-sso` rely on their own
`--selftest` plus manual runs, not a nightly launch.

`oracle/` is the ONE deliberate exception: it is the e2e stand-in, never brokers
git, and satisfies neither §4 nor §6 (its `--selftest` checks only `sh`, `curl`,
`python3`).  Its own Dockerfile header states the reduced contract it does keep.

### 1. No ENTRYPOINT

The image must declare **no ENTRYPOINT**.  The Wardyn docker driver sets only
the container *Cmd* to `["sleep", "infinity"]` at create time so the sandbox
stays alive while the driver calls `Exec` to launch the agent process.  An
ENTRYPOINT would wrap the driver-supplied Cmd (making it an argument to the
entrypoint), and if that entrypoint exits it tears the sandbox down immediately.

### 2. `/usr/local/bin/agent-run "<task>"`

Every image ships an executable shell script at this path.  The driver (or an
operator) calls it to run one task non-interactively:

```
agent-run "<task text>"   # run one task; exit with the CLI's code
agent-run --selftest      # verify binaries + env wiring; no API key needed
```

`--selftest` is used by e2e validation to confirm the image contract before a
live task is scheduled.

When the control plane sets `WARDYN_TASK_MODE=exec` (BYOA/CI lane — see
`docs/CI.md`), `agent-run` runs the task as a plain shell command
(`/bin/sh -lc "<task>"`) instead of the agent harness: same MITM-CA/clone/
brokered-credential wiring, no model call. The selftest relaxes accordingly
(the harness binary becomes optional; `git` gates only when repo wiring is
present).

### 3. `/usr/local/bin/wardyn-rec`

The session recorder binary, built from `cmd/wardyn-rec`.  It wraps the agent
process and records the PTY session via `asciinema` (GPL subprocess, never
linked) or falls back to a plain `.log` file when asciinema is absent.  The
docker driver calls `wardyn-rec` (via `Exec`) instead of calling `agent-run`
directly when recording is enabled.

### 4. `/usr/local/bin/wardyn-git-helper`

The Git credential helper binary, built from `cmd/wardyn-git-helper`.  It
speaks the [Git credential protocol][cred-proto] and obtains tokens by calling
`POST /wardyn/v1/credentials/mint` on the proxy (see credential flow below).
The system `gitconfig` wires it GLOBALLY (all hosts, not just GitHub) — see §6.

### 5. USER agent (uid 1000)

The image creates user `agent` with uid 1000, home `/home/agent`, and
workspace `/home/agent/work`.  All agent activity runs as this user.

### 6. System gitconfig

Set during the image build, verbatim:

```dockerfile
RUN git config --system credential.helper \
        '/usr/local/bin/wardyn-git-helper --secret-file /home/agent/.wardyn/git-helper.secret'
```

Two details are load-bearing, and copying a shortened form silently drops them:

- **No host scoping.** The helper is wired for ALL hosts, not just
  `https://github.com`, because grants may name any host
  (`WARDYN_GIT_PAT_GRANTS`).  A host with no matching grant is safe: the helper
  emits nothing and git falls through to its normal behavior.
- **`--secret-file`.** The per-run caller-auth gate — `agent-run` provisions a
  0400 secret at task time and the helper mints only when the caller presents
  it, raising the bar from "any in-sandbox process" to "code running as the
  agent uid". When no secret is provisioned (e.g. interactive runs) the helper
  falls back to run-token-only auth — a bound the helper's own doc states. The
  config lives in the ROOT-owned system gitconfig precisely so the agent cannot
  rewrite the path.

`--system` (not `--global`) also means every `git` invocation in the sandbox is
covered with no per-user configuration.  `agent-run --selftest` reports what it
finds (`agent-run-lib.sh`'s `selftest_report_repo_and_git`).

---

## Credential and recording flow

```
┌──────────────────────────────────────────────────────────┐
│  agent sandbox (no default route — L0 structural egress) │
│                                                          │
│  agent-run / claude / codex / git                        │
│    │  ANTHROPIC_BASE_URL=http://wardyn-proxy:3128/wardyn/llm/anthropic
│    │  HTTP_PROXY=http://wardyn-proxy:3128                │
│    │                                                     │
│    ▼                                                     │
│  wardyn-proxy:3128  (only egress path)                  │
│    ├─ /wardyn/v1/credentials/mint  ──► wardynd /api/v1/internal/credentials/mint
│    │     proxy injects run token; response (minted token) passed through
│    ├─ /wardyn/v1/approvals/{id}    ──► wardynd /api/v1/internal/approvals/{id}
│    │     proxy injects run token; response passed through
│    ├─ /wardyn/llm/anthropic/<rest> ──► https://api.anthropic.com/<rest>
│    │     proxy applies brokered api_key InjectionRule (host=api.anthropic.com)
│    │     if no rule is configured: 404 (no LLM credential brokered)
│    └─ all other outbound requests: enforced against the run's egress policy
│                                                          │
│  wardyn-rec (PTY recorder)                              │
│    wraps agent process; delivers cast to shared volume   │
│    or via PUT /api/v1/internal/recordings/{run_id}       │
└──────────────────────────────────────────────────────────┘
```

### Git credential flow

1. `git clone https://github.com/...` triggers the system credential helper.
2. `wardyn-git-helper get` is called; it reads `WARDYN_GITHUB_GRANT_ID` from
   the sandbox env (set by the driver at dispatch — this is the UUID of the
   run's `github_token` CredentialGrant, not a secret).
3. The helper calls `POST /wardyn/v1/credentials/mint` via the proxy (origin-
   form URL — the proxy serves it locally, injecting the run token toward
   `wardynd`).
4. `wardynd` verifies the run token, checks the approval gate, and mints a
   short-lived GitHub installation token (via the GitHub App).
5. The helper emits the token to git's stdout in the credential protocol format.
6. git uses the token for the clone/fetch; the token is never stored to disk
   (git credential store is not configured in the sandbox image).

The run token itself NEVER appears in the sandbox environment.  The proxy holds
it and injects it only when forwarding internal API calls.

### Recording flow

1. The docker driver calls `wardyn-rec -run <uuid> -cast-dir /var/log/wardyn
   [-out-dir /wardyn/recordings] -- <agent argv>`.
2. `wardyn-rec` execs `asciinema rec` (asciinema is GPL; wardyn-rec is Apache-
   2.0; they are never linked — this is the boundary).
3. When `asciinema` is absent (stripped image), `wardyn-rec` tees combined
   stdout/stderr to a `.log` file (degraded but non-blocking).
4. After the agent process exits, `wardyn-rec` copies the finished cast to the
   shared `wardyn-recordings` volume (`-out-dir`), from where `wardynd`'s
   recording store serves it.

---

## Available images

| Directory       | Image tag                    | CLI        |
|-----------------|------------------------------|------------|
| `claude-code/`  | `wardyn/agent-claude-code:local` | `claude` (`@anthropic-ai/claude-code`) |
| `codex-cli/`    | `wardyn/agent-codex-cli:local`   | `codex`   (`@openai/codex`) |
| `oracle/`       | `wardyn/agent-oracle:local`      | none (e2e stand-in; §4/§6 exempt — no git broker) |
| `full/`         | `wardyn/agent-full:local`    | `claude` (inherited) |
| `aws-sso/`      | `wardyn/agent-aws-sso:local` | `aws` (AWS CLI v2, no LLM harness) |

`claude-code/` and `codex-cli/` are the two user-facing agent harnesses
(`make agent-images-core`). `make agent-images` additionally builds `oracle/`
— NOT a real coding agent: it runs a task's scripted, known-good solution so
the e2e suite can prove each task in `test/e2e/tasks/` is solvable and its
grader scores correctly — and `aws-sso/`, also not a coding agent: its only
job is to host an interactive
`aws sso login --sso-session wardyn --no-browser --use-device-code` session
(device-code flow) so an operator can `wardyn attach` in and authenticate an
AWS SSO profile from inside a governed sandbox. The `[sso-session wardyn]`
block that command reads (`sso_start_url` from the setup UI + `sso_region`
from the daemon's config — no credential) is seeded by the control plane via
`WARDYN_AWS_SSO_CONFIG_B64`. It carries AWS
CLI **v2** — there is no apt package for v2, so the Dockerfile downloads the
official per-version zip installer and GPG-verifies it against AWS's published
public key (baked into the image, same convention as the baked GitHub/Azure
DevOps SSH host keys in `claude-code/`); `AWS_CLI_INSTALL=staged` swaps in a
host-staged installer zip for offline/strict-allowlist builds, mirroring
`claude-code`'s `CLAUDE_INSTALL=native` pattern. `full/` is a separate, fat
opt-in image (`make agent-image-full`) built `FROM wardyn/agent-claude-code:local`
plus real Go/Python/Rust/JDK+Maven/pnpm toolchains, for workspaces whose import
Record/Verify setup commands need an actual toolchain rather than the
toolchain-less core image (which dies "command not found").

---

## Bring your own image (BYOI)

You do not have to satisfy the full contract by hand. Two paths exist:

**Wrapped (per-run, recommended).** Pass a `image` on `POST /api/v1/runs`
(the New Run wizard's *Custom sandbox image (advanced)* field) — any OCI base.
Wardyn WRAPS it with the trusted finalize stage (`FROM <your image>` +
`COPY` the runner tools onto PATH + `ENTRYPOINT []`), so the recorder,
git-brokering, verify, and the no-ENTRYPOINT rule are satisfied for you. A
per-run `agent-run --selftest` runs before the task and fails the run closed if
the image can't meet the contract, so a broken image surfaces honestly instead
of hanging. Requires wardynd built with `-tags docker` and
`WARDYN_ENVBUILD_TOOLS_DIR` set (the `-envbuild` path).

What the wrap does NOT add — your base must still provide:

- **A shell.** `agent-run` is `bash`; `wardyn attach` falls back through
  `tmux → bash → /bin/sh`. A fully distroless/shell-less base fails the selftest.
- **The harness CLI**, for a *task* (autonomous) run — e.g. `claude` for a
  `claude-code` task. Interactive/BYOI login boxes don't need it. Wardyn installs
  nothing at runtime.
- Non-root is recommended (Claude Code refuses `--dangerously-skip-permissions`
  as root); the wrap does not remap USER/HOME.

**Private registries:** pre-pull on the host (`docker pull …`) — the driver
short-circuits the pull when the image is already present, so no registry-auth
wiring is needed. Pin a `@sha256:` digest to avoid mutable-tag drift between runs
(digest refs are presence-checked by inspect, not the tag filter).

**TLS-MITM trust per runtime:** the per-run proxy CA is delivered at
`/tmp/wardyn/mitm-ca.pem` (bare CA) and `/tmp/wardyn/ca-bundle.pem` (system roots
+ CA). Node clients trust it via `NODE_EXTRA_CA_CERTS`; OpenSSL-family clients
(curl, Python `requests`, Ruby) via `SSL_CERT_FILE` / `REQUESTS_CA_BUNDLE` /
`CURL_CA_BUNDLE`, all set by dispatch. **Not covered:** JVM keystores and Deno
(`DENO_CERT`) — a JVM/Deno toolchain in a BYOI image must trust the CA itself.

**Containment holds regardless of image; two defense-in-depth caveats.** The
wrap clears the base's ENTRYPOINT and overwrites its runner tools from the
trusted host copies, and egress allow-listing, confinement, mounts, and
capability drops are applied by the runner at container-create — none of it
depends on image contents, so a hostile base cannot escape the sandbox. Two
image-controlled surfaces remain, both bounded by the egress allowlist and
neither an escape: (1) a base with `USER root` runs the workload as
root-in-container — primary confinement (cap-drop, no-new-privileges, seccomp,
apparmor, userns-remap) still holds, but the non-root defense-in-depth the
convention images provide is waived; prefer a non-root base. (2) The combined CA
bundle concatenates the base's own system trust store, so an interactive
`wardyn attach` shell trusts whatever CAs the base ships — only relevant if you
attach a shell to an untrusted image on a non-MITM'd allowed host.

**Raw (deploy-time).** If you build an image that already satisfies the contract
below, register it under an agent name in `WARDYN_AGENT_IMAGES` (a JSON
`{"<agent>":"<ref>"}` map) and launch that agent — no per-run wrap.

## Adding a new agent image

1. Create `deploy/images/<name>/Dockerfile` (multi-stage: Go builder + runtime).
2. Copy both binaries from the builder stage:
   ```dockerfile
   COPY --from=builder /out/wardyn-rec        /usr/local/bin/wardyn-rec
   COPY --from=builder /out/wardyn-git-helper /usr/local/bin/wardyn-git-helper
   ```
3. Add `deploy/images/<name>/agent-run` implementing the `--selftest` and task
   modes. Source `/usr/local/bin/agent-run-lib.sh` and use its helpers rather
   than hand-copying a sibling image's script — in particular
   `selftest_check_bins <your-cli> || ok=0` for the required-binary block, which
   is what carries the `WARDYN_TASK_MODE=exec` relaxation described in §2 (the
   hand-copied version drifted and one image shipped without it).
4. Set the system gitconfig credential helper — copy the exact `RUN git config
   --system ...` line from §6, `--secret-file` included (required for git
   brokering).
5. Create user `agent` uid 1000, home `/home/agent`, work `/home/agent/work`.
6. Do NOT add an ENTRYPOINT.
7. Add a `profiles: [build-only]` stanza to `deploy/compose/docker-compose.yaml`
   mirroring the `proxy-image` and `agent-claude-code` stanzas.
8. Add the image to the `agent-images` Makefile target.
9. If the image runs `go mod download`, `npm install`, or `pnpm install`, wire the
   **corporate-build convention** below into its build stages.

## Corporate-build convention (TLS-MITM proxy / internal mirror)

Adding or editing a Dockerfile here on a corporate network — corp CA staging,
`GOTOOLCHAIN=local`, npm/pnpm proxy ARGs, native agent-binary installs — see
[docs/adoption/corp-image-authoring.md](../../docs/adoption/corp-image-authoring.md).

[cred-proto]: https://git-scm.com/docs/gitcredentials#_custom_helpers
