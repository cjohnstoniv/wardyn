# Wardyn agent images

This directory contains the OCI image definitions for coding-agent sandboxes
governed by Wardyn.  Each subdirectory is one agent image, except `common/`
(a shared shell library COPY'd into them).

## Image contract

Every image that runs a real agent task MUST satisfy the invariants below.  What
actually enforces them: `internal/envbuild/builder.go`'s `requiredTools` plus a
fail-closed per-run `agent-run --selftest` for wrapped/BYOI images
(`internal/api/runs_dispatch.go`, `make test-e2e-byoi`), and the live e2e suite
for the images its task corpus actually launches — `claude-code` and `oracle`
(`test/e2e/live/live_test.go`); `codex-cli`, `full`, and `aws-sso` rely on their
own `--selftest` (full's is inherited from the claude-code base) plus manual
runs, not a nightly launch.

`oracle/` is the ONE deliberate exception: it is the e2e stand-in, never brokers
git, and satisfies neither §4 nor §6 (its `--selftest` checks only `sh`, `curl`,
`python3`).  Its own Dockerfile header states the reduced contract it does keep.

### 1. No ENTRYPOINT

The image must declare **no ENTRYPOINT**.  The Wardyn drivers (docker and k8s)
set only the container/pod *Cmd* at create time so the sandbox stays alive
while the driver calls `Exec` to launch the agent process — for a
NON-interactive run that Cmd is `sh -c "$AgentIdleScript"` (the MITM-CA install
idle script, `internal/runner/sandbox.go`), and for an INTERACTIVE run it is
`agent-run --idle` (mode 3 below).  Neither is literally `sleep infinity`; both
share the same contract this section states.  An ENTRYPOINT would wrap the
driver-supplied Cmd (making it an argument to the entrypoint), and if that
entrypoint exits it tears the sandbox down immediately.

### 2. `/usr/local/bin/agent-run "<task>"`

Every image ships an executable shell script at this path.  The driver (or an
operator) calls it to run one task, in one of three modes:

```
agent-run "<task text>"   # mode 1: run one task; exit with the CLI's code
agent-run --selftest      # mode 2: verify binaries + env wiring; no API key needed
agent-run --idle          # mode 3: interactive runs only — see step 3
```

`--selftest` is used by e2e validation to confirm the image contract before a
live task is scheduled.

**Mode 3, `--idle`, is REQUIRED, not optional.** Both drivers (`internal/
runner/docker/driver.go`, `internal/runner/k8s/sandbox.go`) launch `agent-run
--idle` as the ENTIRE main container/pod process for every INTERACTIVE run —
never Exec'ing a task into it, unlike the non-interactive path in step 1. It
must therefore hold the process open (prepare the workspace, then idle/exec
into a long-running process) rather than exit: an image whose `agent-run`
lacks this branch treats `--idle` as an unrecognized/missing task, exits
immediately, and the run strands RUNNING with a dead main process and no
attach target. `deploy/images/oracle/agent-run` is the minimal reference
implementation of this branch (idle only — it skips the workspace prep the
real harness images perform, since oracle brokers neither git nor a model).

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
directly when recording is enabled (`Config.Record`, opt-in on that substrate).

**On the Kubernetes runner substrate (`internal/runner/k8s`, `-tags k8s`) this
binary is NOT optional.** That substrate always advertises
`SessionRecording:true` with no opt-out, so `Exec` unconditionally wraps every
launch with `wardyn-rec` (`exec.go`'s `recordCmd`) — an image without it fails
every `Exec` closed (the ephemeral container can't start: `wardyn-rec` does
not resolve). This is deliberate fail-closed behavior, not a bug: a driver
that claims `SessionRecording:true` must actually deliver it, never silently
downgrade to unrecorded. The k8s conformance suite's own agent image
(`deploy/kind/Dockerfile.conformance-agent`) exists specifically to carry a
real `wardyn-rec` so this path is exercised for real.

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
finds (`agent-run-lib.sh`'s `selftest_report_repo_and_git`) — and fails the
selftest closed when a git grant is present but no credential helper is wired,
so a BYOI-wrapped base that never ran this `RUN git config --system …` line
(the wrap COPYs the `wardyn-git-helper` binary onto PATH but does not itself
wire the system gitconfig — only the prebuilt `claude-code`/`codex-cli` images
bake this `RUN` line in) surfaces as an honest selftest FAIL instead of a
silent no-op the first time the agent tries to clone a private repo.

### SSH relay binaries (`claude-code`, `codex-cli`)

Not (yet) part of the numbered contract above — `oracle`, `full`, and
`aws-sso` do not carry them — but both user-facing agent images ship two
binaries the SSH gateway ([docs/SSH.md](../../docs/SSH.md)'s "Image contract
(BYOI)") execs **inside the sandbox**, by convention, never by
reimplementing their protocols:

| Feature | Binary | Path | Invocation | apt package |
|---|---|---|---|---|
| sftp subsystem | sftp-server | `/usr/lib/openssh/sftp-server` | `sftp-server -e` | `openssh-sftp-server` |
| `-L` port forwarding | socat | `/usr/bin/socat` | `socat - TCP:127.0.0.1:<port>` | `socat` |

A BYOI run on an image missing either gets a clean channel error naming the
missing binary the moment that specific feature (sftp/`-L`) is used — never
a hang — per docs/SSH.md; the interactive shell is unaffected either way.

`claude-code` already ships `tmux` + `openssh-client` (the interactive attach
shell and the SSH-clone lane, §5/§6 above) — only the two binaries above are
new there. `codex-cli` had neither `tmux` nor an SSH client before; it now
gains `tmux` (the same attach-shell fallback chain as every other image) and
the same two SSH-gateway binaries, but deliberately **not** `openssh-client`/
`corkscrew`/baked host keys — its SSH-clone story is unchanged.

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
│    │     (or an operator-configured internal gateway — WARDYN_ANTHROPIC_BASE_URL)
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
| `vscode/`       | `wardyn/agent-vscode:local`  | `claude` (inherited); adds `code-server` behind the UI-sandbox relay |
| `aws-sso/`      | `wardyn/agent-aws-sso:local` | `aws` (AWS CLI v2, no LLM harness) |

`claude-code/` and `codex-cli/` are the two user-facing agent harnesses;
`make agent-images-core` builds them plus `base/`, which carries the setup
connectivity probe. `make agent-images` additionally builds `oracle/`
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

`vscode/` is likewise a separate, opt-in image (`make agent-image-vscode`)
built `FROM wardyn/agent-claude-code:local` plus a pinned, sha256-verified
`code-server` — see "UI-sandbox image (`vscode/`)" below for the launcher
contract and BYOI table.

---

## UI-sandbox image (`vscode/`)

`vscode/` exists for the UI-sandbox relay (`docs/UI-SANDBOXES.md`,
`internal/api/uigateway.go`): a run whose policy declares a `ui_apps` entry
named `"vscode"` (`RunPolicySpec.ui_apps`, see `docs/POLICIES.md`) gets that
entry relayed to a browser over the existing exec lane — a governed,
ticket-gated HTTP proxy to a declared sandbox loopback port, never a new
network path out of the sandbox. `code-server --auth none --bind-addr
127.0.0.1:8080` is safe specifically *because* nothing else can reach that
port and every relay hop is gated by its own ticket/cookie/origin controls;
`--auth none` would be unsafe on any port with a route in from outside the
relay.

### Launcher contract (BYOI)

The relay never runs a command string from policy — it execs a *convention*
path, `/usr/local/bin/wardyn-ui-<app-name>`, with **no arguments and no
tty**, backgrounded by the caller once it starts listening
(`internal/api/uigateway.go`'s `uiLauncherScript`). Any image — including a
BYOI one — can serve a declared app by shipping a launcher at that path:

| Requirement | What `vscode/`'s launcher (`deploy/images/vscode/wardyn-ui-vscode`) does |
|---|---|
| Path | `/usr/local/bin/wardyn-ui-vscode` (app name `vscode`, matching the policy's `ui_apps[].name`) |
| Invocation | No args, no tty; reads nothing from stdin, discards stdout/stderr |
| Must bind | `127.0.0.1:<declared port>` (`8080` here) within the gateway's polling window — it polls up to 20s before giving up |
| Lifetime | Foreground process; the caller backgrounds it (`&`) and it is reparented to the sandbox's PID 1 — it must not `setsid`/daemonize itself, or it outlives the poll with nothing tracking it |
| `--help` | Exits 0 (not part of the runtime contract — kept only so the image is sanity-checkable standalone: `docker run --rm --entrypoint /usr/local/bin/wardyn-ui-vscode <image> --help`) |
| Missing binary | Not this image's concern — the gateway's own probe exits 3 and the console shows a frozen "no UI launcher in this image" error naming the path (`docs/design/ui-sandboxes-prompt.md` §7) |

A BYOI image registers under its own agent name in `WARDYN_AGENT_IMAGES` (Raw
path, see "Bring your own image" below) and ships its own
`/usr/local/bin/wardyn-ui-<name>` for whatever app it declares — the wrapped
BYOI path does NOT add one for you, since the app name and port are policy
authored per run, not fixed at wrap time.

### Size delta

Measured locally (`docker image inspect --format '{{.Size}}'`), your numbers
will vary with the pinned `code-server` version and base-layer drift:

| Image | Size |
|---|---|
| `wardyn/agent-claude-code:local` (base) | 336.6 MiB |
| `wardyn/agent-vscode:local` | 564.8 MiB |
| **Delta (code-server + launcher)** | **+228.3 MiB (≈239 MB)** |

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
When the operator sets `WARDYN_TRUSTED_CA_FILE` (docs/OPERATIONS.md § "Corporate
TLS-inspection root"), its PEM rides the same `ca-bundle.pem` — appended even on
a run with no per-run MITM CA of its own — so a BYOI base missing every system
CA-bundle path loses OpenSSL-family public trust entirely once that knob is set.

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
