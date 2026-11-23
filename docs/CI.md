# Wardyn CI — governed sandboxes in your pipeline

- Run a sandboxed job from a CI/CD pipeline (GitHub Actions, Azure DevOps, or anything with a docker daemon) with **no pre-running Wardyn, no UI, and no human**.
- One script brings up a fresh control plane, launches one governed run, waits for its outcome, collects artifacts, tears everything down, and exits with the run's exit code. So your pipeline's pass/fail is the sandboxed task's pass/fail.
- Pointed at a control plane you already run (`WARDYN_URL`), the same script launches there instead, as a dedicated CI principal. Which is where an agent run, with its model credential, belongs (see "[CI's identity](#cis-identity)" below).
- This is a **BYOA (bring your own agent/container)** surface: you supply the image — an agent harness, your test container, or any stock image. And Wardyn supplies the governed sandbox around it (default-deny egress at the `wardyn-proxy` sidecar, brokered never-resident credentials, confinement classes, an append-only audit trail).
- It doesn't have to be an agent at all: `task_mode=exec` runs a plain shell command under the same governance.

## Quick start

- Copy the example for your CI system and adjust the env block:
  - GitHub Actions: [`docs/ci/github-actions.yml`](ci/github-actions.yml)
  - Azure DevOps: [`docs/ci/azure-pipelines.yml`](ci/azure-pipelines.yml)
- Both are the same three steps: check out wardyn, run [`scripts/ci-run.sh`](../scripts/ci-run.sh), upload `ci-artifacts/`.

```sh
# The whole flow, locally or on any runner with docker:
WARDYN_CI_IMAGE=ubuntu:24.04 \
WARDYN_CI_TASK_MODE=exec \
WARDYN_CI_TASK='echo hello from a governed sandbox' \
scripts/ci-run.sh
```

### Pin the wardyn checkout

> [!WARNING]
> - Step two **executes shell out of that checkout** — [`scripts/ci-run.sh`](../scripts/ci-run.sh), and everything it sources — inside the same job the same file hands your secrets (`secrets.WARDYN_CI_TOKEN`).
> - Fetched at tip of Wardyn's default branch, that is a job where a push to a repository you do not control runs in front of your credentials, on your runner, with your network position.

- So both examples pin it: `ref: v<release>` on the Actions checkout, `--branch v<release>` on the Azure clone.
- They ship pinned at the release that shipped them; bump that deliberately, to a release you verified ([VERIFY.md](VERIFY.md)).
- A full commit sha (`ref: <40-hex>`, or a `git checkout <sha>` after the clone) pins harder still — a tag can be moved, a sha cannot.
- The same reasoning is why the third-party actions in these files carry versions rather than floating refs.

## scripts/ci-run.sh

| Env | Meaning | Default |
|---|---|---|
| `WARDYN_CI_TASK` | task text (`exec` mode: the shell command) | **required** |
| `WARDYN_CI_IMAGE` | BYOA base image ref; wrapped + governed | unset (agent's own image) |
| `WARDYN_CI_TASK_MODE` | `harness` (run the agent) or `exec` (plain command) | `harness` |
| `WARDYN_CI_AGENT` | agent for harness mode / runner-tools source | `claude-code` |
| `WARDYN_CI_REPO` | `org/name` cloned into the workspace (needs egress + creds, see below) | unset (ephemeral scratch) |
| `WARDYN_CI_POLICY_FILE` | `RunPolicySpec` JSON | [`examples/policies/ci.json`](../examples/policies/ci.json) |
| `WARDYN_URL` | an existing control plane to launch on, instead of a throwaway stack (see "[CI's identity](#cis-identity)" below) | unset (throwaway stack) |
| `WARDYN_CI_TOKEN` | with `WARDYN_URL`: the CI [service identity](operations/api-tokens.md#service-identities)'s `wdn_` token — every `wardyn` call authenticates as it | **required** with `WARDYN_URL` |
| `WARDYN_CI_MODEL_PROVIDER` | with `WARDYN_URL`, harness mode: the model provider the run uses, checked before launch | **required** there |
| `WARDYN_CI_TIMEOUT` | `wardyn run --wait` bound | `30m` |
| `WARDYN_CI_IDEMPOTENCY_KEY` | with `WARDYN_URL`: passed to the launch as `wardyn run --idempotency-key`; see "[Retrying a launch](#driving-an-existing-control-plane-instead)" | unset |
| `WARDYN_CI_OUT` | artifact dir (`run.json`, `audit.json`, `run.log`, `session.cast` — the run's terminal recording, when it has one) | `./ci-artifacts` |
| `WARDYN_CI_KEEP` | throwaway stack: `1` = leave it up for debugging | unset |
| `WARDYN_CI_SKIP_BUILD` | throwaway stack: `1` = reuse existing local images | unset |
| `WARDYN_SETUP_RUNNER_DISK_GB` | throwaway stack: GB of disk free for runs, where this host cannot see Docker's data root (Docker Desktop); the stack's runner takes no run without it there | measured on this host |
| `WARDYN_SETUP_RUNNER_LIMITS` | throwaway stack: the runner's whole limit set as `wardyn runner set` flags, instead of half this host | unset |
| `WARDYN_DOCKER_SOCK` | throwaway stack: which host Docker socket the job drives (two-daemon hosts: pick the daemon with the runtimes you need) | `/var/run/docker.sock` |
| `WARDYN_ADMIN_TOKEN` | throwaway stack: its own bootstrap bearer, gone with the stack; never sent to `WARDYN_URL` | a random value per job |

> [!NOTE]
> `WARDYN_CI_SECRETS` is gone: `ci-run.sh` refuses it rather than seed a credential into the operator's namespace for every run to share.

> [!IMPORTANT]
> **If this deployment has an agent roster, the CI agent needs a row in it.**
> - `ci-run.sh` defaults `WARDYN_CI_AGENT` to `claude-code`, and since 0.7.2 a deployment that HAS a `site_config.agent_providers` block refuses run creation for an agent with no enabled row in it (`422`, `agentRosterRefusal`). So a job that worked against a roster-less install starts failing at the door the day an admin writes the first roster row.
> - It binds `exec` mode too: `ci-run.sh` always passes `--agent`, since the agent is also where it sources the runner tools.
> - Either give the agent CI uses an enabled row (`PUT /api/v1/agent-providers`, or the Providers screen) or point `WARDYN_CI_AGENT` at one that has one.
> - A deployment with NO `agent_providers` block refuses nothing — which is every install upgraded from 0.7.1 until someone writes the first row.

### CI's identity

A run's model credential is its owner's, and CI is no exception. So `ci-run.sh` has two modes:

- **A throwaway stack** (the default).
  - `ci-run.sh` brings up its own control plane from nothing and drives it with that stack's own bootstrap bearer (`WARDYN_ADMIN_TOKEN`, which dies with the stack).
  - Nobody is signed in to it, so nobody's model credential is there: it runs **`exec` mode only**, and refuses a harness run up front.
- **An existing control plane** (`WARDYN_URL` set).
  - No stack, no build; the `wardyn` CLI must be on `PATH`.
  - Every call authenticates as `WARDYN_CI_TOKEN` — a **dedicated CI service identity**'s `wdn_` token, never a person's — and `WARDYN_ADMIN_TOKEN` is cleared for those calls, since the CLI would otherwise try it first.
  - A harness run needs `WARDYN_CI_MODEL_PROVIDER`: `ci-run.sh` reads that identity's `provider_access` (`wardyn setup status --json`), stores nothing, and fails naming the provider unless its credential is `live` or `expiring` (it warns on `expiring`).
  - It then launches on exactly that provider (`--model-provider`).

* The CI principal is a **service identity**, `service:<name>`: automation that runs under its own name, with a named person as its sponsor.
  * An admin creates it (`wardyn service-identity create --name ci-bot --sponsor <principal>`); the sponsor mints its token in the console (Account, "Service identities I sponsor"), shown once, with an expiry of at most `90d`.
  * It stores its own model-provider credential once (`PUT /model-providers/{id}/credential`, with its own token) — see "[Provisioning the CI principal](#driving-an-existing-control-plane-instead)" below.
* Its audit trail says "CI" and names its sponsor on every row, rather than one human's token paying for and being blamed for every pipeline run.
  * Scope it with `capModelProvider`/`capAgent` through the `user` subject `service:<name>`.
* A person's token is not the CI credential: it acts as that person, and leaves with them when they leave.
* A service token reaches only what a pipeline needs. Anything else answers `403` `service_scope`.
  * Launch and dry-run, read, `PATCH` and kill its own runs, `GET /me` (with its reach), `GET /setup/status` and its model-provider credential.
  * Read the audit rows of its own runs (`GET /audit?run_id=<id>`) and the presets and workspaces it may launch with (`GET /presets`, `GET /presets/{name}`, `GET /workspaces`).
  * Follow a run with `GET /runs/{id}/events`, a `text/event-stream`, rather than polling. It sends a keepalive every 15s and ends with `ended`. It carries no exit code: after `ended`, read `GET /runs/{id}` for the state and `GET /audit?run_id=<id>` for the task's code (`run.complete` `.data.exit_code`).
  * That audit read covers only runs it created (any other id answers `200` `[]`), so `run --wait` reports the task's own code and `ci-run.sh` collects `audit.json`.
  * It has no recording read, so `ci-run.sh` collects no `session.cast`: read it as the sponsor or an admin.
* A sponsor does not need a live session; a deactivated sponsor ends the identity. When the sponsor leaves, is suspended or is replaced, the identity's tokens stop. A SCIM suspension of the sponsor also ends its live runs; a sponsor change leaves them running. Change the sponsor (`wardyn service-identity update <name> --sponsor <principal>`) and mint a new token.
* The deployment's admin token is no substitute: under OIDC it holds no model credential, and `ci-run.sh` says so if it is handed one.
* Do not use the Claude subscription kind for CI ([below](#model-access-for-harness-mode-running-a-real-agent)).

### Exit codes

`ci-run.sh` propagates `wardyn run --wait`'s exit code. Every `wardyn` command shares one taxonomy — codes `2`-`5` classify a failed CLI-to-control-
plane request itself (auth/client/server/network), independent of what the run inside it did:

| Exit | Meaning |
|---|---|
| `0` | ok — the command succeeded (`run --wait`: the run `COMPLETED`, task/agent exited 0) |
| `2` | auth — the control plane rejected the request as unauthenticated/unauthorized. **Also** `run --wait`: the run ended `KILLED`, `STOPPED`, or `ARCHIVED` (lifecycle termination, not a task result) |
| `3` | client — any other non-2xx response; see [below](#exit-3) |
| `4` | server-5xx — the control plane returned a server error |
| `5` | network — couldn't reach the control plane at all (DNS, connection refused, TLS) |
| `124` | `run --wait` and `run wait-ready`: the wait timed out (the run keeps running); see [below](#exit-124) |
| `1` | the invocation failed **locally**: a usage error, an unreadable `--policy-file`, and more; see [below](#exit-1) |
| _task's own code_ | `run --wait`: the run ended `FAILED` — the exit is the task/agent's own real exit code (from the `run.complete` audit event), or `1` if that code is missing/unreadable (never `0` on `FAILED`) |
| _ssh's own remote status_ | `wardyn run ssh` (once connected): ssh(1)'s own remote exit status; see [below](#sshs-own-remote-status) |
| _clean detach = `0`_ | `wardyn attach`: a clean detach is `0`; see [below](#clean-detach) |

- `wardyn run get <id> --json` (or the `run.complete` audit event) always has the authoritative outcome — check it when the process's own exit status alone doesn't say why a run didn't complete.
- That is also how a pipeline tells the two meanings of `1` apart:
  - a **usage** failure never created a run, so no run id exists;
  - a `FAILED` run whose exit code was unreadable has one, and its `run.complete` event says so.

#### Exit 3

- client — any other non-2xx response:
  - a 4xx (bad request, not found, conflict, ...), or an unfollowed 3xx redirect
  - (an interposed proxy or a mistyped `--url` — the CLI and the Go SDK never follow redirects, so a redirected write fails instead of being replayed, with its body and bearer, at the `Location`).

#### Exit 124

- `run --wait` (the run reached no terminal state in time) and `run wait-ready` (the run was not ready in time): the wait timed out, or `--timeout` was zero or negative (no request is made).
- The timeout bounds the requests too, so a slow read is cut at the budget.
- It does **not** stop the run: it keeps running, and holds its sandbox and credentials until it ends, so kill it ([ci-jobs-as-runs.md](ci-jobs-as-runs.md))

#### Exit 1

- the invocation failed **locally**, before or beside the request:
  - a usage error (unknown flag or argument), a malformed id, an unreadable or schema-invalid `--policy-file`, or a response the CLI could not classify (e.g. a 2xx whose body did not decode).
- This is `exitCodeFor`'s catch-all in [`cmd/wardyn/main.go`](../cmd/wardyn/main.go), so a future local failure lands here too.
- **Also** `run --wait`: see [the row below](#exit-codes)
- `wardyn policy check`: the candidate is not within the other policy, and the fields are printed. With nothing printed, exit 1 is the local failure above: the check did not run.

#### ssh's own remote status

- `wardyn run ssh` (once connected): the process's exit code is ssh(1)'s own remote exit status, passed straight through — **not** this taxonomy's `2`-`5`.
- Those still classify a FAILED *connection attempt* (gateway disabled, no advertised address, handshake rejected) that never got as far as running ssh(1).
- A remote command that happens to exit `2`/`3`/`4`/`5`/`124` is coincidence, not this table — check it against what you ran, not against this list.
- A signal-killed ssh child (`exec.ExitError.ExitCode()` is `-1` in that case) is clamped to `1` instead of leaking a negative code out.

#### Clean detach

- `wardyn attach`: a clean detach (TERM/HUP/INT, the remote side closing, or stdin EOF on a piped/non-tty session) is `0`, never re-labelled as a task result —
  - attach has no task exit code of its own to report.
- **Not** Ctrl-C on the primary (tty) path: raw mode clears ISIG, so it is relayed to the remote PTY as input rather than detaching locally.
- A FAILED attempt to open the session (rejected/unreachable) classifies with the same `2`-`5` as any other request.

## Writing a CI policy

- `--policy-file` accepts **JSON or YAML** (same schema); YAML also lets you keep inline comments.
- `wardyn policy render -f <file>` converts either to canonical JSON and rejects a misspelled field, so you can validate a policy in the pipeline before launching.
- `wardyn policy check --within <evaluated> -f <candidate>` fails the pipeline with exit 1 when the candidate admits anything the evaluated policy does not, or cannot be shown not to, and names each field. `<evaluated>` is a stored policy's id, a run template's id, or a file that holds a policy, such as the output of `record save --json`.
  - A template is checked as it resolves for whoever runs the check, so a person who will launch the run should run it.
  - Values your own read of the policy hides are checked as that read shows them. A candidate that spells a mount's host path or the workspace secret names the way your read does is within on that field, and one that names the real value is reported as wider.
  - A candidate that carries a grant whose secret name your read hides is reported as wider on `eligible_grants`, whether or not it names the secret.
  - A service identity's token cannot check against a template or read a stored policy: give it a file.
- To hold a promoted workload to the policy you checked, read the `launch policy digest:` line of `wardyn apply -f FILE --dry-run`, run `wardyn workload pin NAME <digest>`, then `wardyn apply -f FILE`. A launch whose policy has changed since is refused. A new workload is applied first and pinned after its first launch.
- A pin does not hold everything a run is given. [Known limits](operations/workloads.md#known-limits) lists what it leaves out.
- Start from [`examples/policies/ci.json`](../examples/policies/ci.json) — the unattended baseline — and add exactly what the task needs:
  - **`"first_use_approval": "always_deny"`** — non-negotiable for unattended runs. Anything off the allowlist is hard-denied instantly; nothing ever waits on a human. (`wait_for_review` holds connections for a reviewer; `deny_with_review` files approvals nobody will decide.)
  - **Complete `allowed_domains`** — list every host the task legitimately needs (package registries, your model provider). The sealed default (empty list) means the sandbox can reach nothing. GitHub is the exception: it is reached through the Wardyn git-broker (see below), not via `allowed_domains`.
  - **No `requires_approval: true` grants** — an approval-gated credential never mints without a human. Use `requires_approval: false` grants whose secret the CI principal already holds on the control plane.
  - **Decide tool calls in the policy**: with `tool_approvals: hold`, give [`tool_rules`](POLICIES.md#tool_rules--toolrule) an `allow` or `deny` for each tool the task uses and a `"*"` default of `deny`, so no call waits on a human.
    - Carry the policy in a launch preset (`preset` and `preset_version` on `POST /runs`) so every run of the job gets the same answers.
    - Rows decided this way create no approval.
  - **Bound the run** — `auto_stop_after_sec` (ci.json: 1 hour) is the reaper backstop behind `WARDYN_CI_TIMEOUT`.
  - Against `WARDYN_URL`, `ci-run.sh` also kills the run on exit `124` and when the job is cancelled, once the CLI has printed the run id.

> [!NOTE]
> - Cloning a **GitHub** repo (`WARDYN_CI_REPO`) does **not** put `github.com` in `allowed_domains`. It is routed through the Wardyn git-broker: add a `github_token` grant and the run reaches only that repo (via `wardyn-proxy`, token minted proxy-side, never in the sandbox).
> - An un-granted GitHub repo is denied.
> - A non-GitHub SCM host still needs its own `allowed_domains` entry plus a `git_pat`/`ssh_key` grant whose secret the CI principal holds on the control plane (`WARDYN_URL`).

### Model access for harness mode (running a real agent)

- Model access is the CI principal's own credential for a model provider on the control plane `WARDYN_URL` names (see "[CI's identity](#cis-identity)" above):
  - it stores it once, through `PUT /model-providers/{id}/credential` (or the console),
  - and the pipeline sets `WARDYN_CI_MODEL_PROVIDER` to that provider's id.
- Nothing is seeded pre-run and the policy carries no key; the pipeline's env block holds no key to rotate or leak.
- The throwaway stack has no such credential, so it runs `exec` mode only.

> [!WARNING]
> - **Claude subscription: not available in CI, deliberately.**
> - A subscription credential belongs to one human, and CI runs work on behalf of everyone who can trigger the pipeline — so wiring one here means that person's subscription serves other people's work.
> - Anthropic's terms for running Claude Code in agent infrastructure require each end user to authenticate with their own credential and prohibit intermediating usage on their behalf, which would put **you**, the operator, in breach rather than Wardyn.

- The shared-subscription switches are retired ([`ENV.md`](ENV.md) lists them).
- Give the CI principal a provider whose credential it stores as a key or token: `anthropic_api_key`, `openai_api_key`, `bedrock_bearer` (a Bedrock API key) or `custom_endpoint`. The Claude subscription, `bedrock_sso` and `azure_foundry` are signed into, and a service identity cannot sign in, so its run is refused `422` `model_credential`.

### Least-privilege, derived not guessed

Don't hand-author the allowlist for a complex task — record it once:

1. In a trusted environment, run the task under a permissive policy.
2. `wardyn record synthesize <run-id>` previews the least-privilege policy Wardyn derived from the run's audit trail; `wardyn record save <run-id> --name my-ci-task` stores it as a policy, which is an administrator's to write. A member runs it with `--template` to keep it as their own template instead.
3. Export the stored policy's spec to your repo as the pipeline's `WARDYN_CI_POLICY_FILE` (set `first_use_approval` to `always_deny`).

What synthesis does and does not derive:

- The **allowlist** is proxy-observed egress only — the exact hosts the run was actually ALLOWED to reach.
  - Never wildcarded (a recording cannot prove a wildcard is needed);
  - a host that was only denied or only held pending is excluded and reported as a warning, since promoting it would widen past what even the open run was permitted.
- Observed **execs / connects / sensitive writes** are printed as review warnings, not policy: the policy model has no exec/connect/file allowlist, and `workspace_mounts` are operator-authored and never synthesized.

> [!WARNING]
> - **KNOWN GAP** — that exec/write evidence needs the opt-in eBPF/Tetragon sensor, and the sensor is blind inside CC3/Kata guests.
> - `synthesize` does not say so: a CC3 (or sensor-off) run's proposal reads exactly like a fully-observed one.
> - Read a silent proposal as "nothing was observed", not as "nothing happened".

- A recording proves only what the run HAPPENED to do, so synthesis fails toward escalation: it forces `allow_all_egress=false` and `first_use_approval=deny_with_review`.
- Step 3's `always_deny` is you overriding that for an unattended pipeline — tighter, and it means an un-recorded host fails the job instead of raising an approval nobody is watching.

## Concurrent jobs on a shared host

- Several jobs can run on one build host at the same time, under one trusted operator (e.g. a CI fleet on one service account).
- [`scripts/ci-run.sh`](../scripts/ci-run.sh) already does most of this for you: it scopes every compose object to a unique project + namespace and binds the control plane's own host ports ephemerally —
  - UI, Postgres, the devcontainer-build registry sidecar (`registry`, a `wardynd` dependency — `up -d postgres wardynd` always starts it too), AND `wardynd`'s own SSH and UI-sandbox relay ports —
  - so parallel invocations don't collide on container names, the control-plane network, the recordings volume, or any of those five host ports,
  - and one job's `down --volumes` never tears down another's.
- The default project name's suffix comes from `/dev/urandom`,
  - so it is unique even between jobs that cannot see each other's PIDs (the usual shape: every job in its own container, all driving one shared host Docker socket).
- And because a job's teardown is `down --volumes`, `ci-run.sh` refuses to start at all when its project name already has **running** containers —
  - a pinned `WARDYN_CI_PROJECT` reused while the previous job is still live is an error, not a silent teardown of that job.
- (Stopped leftovers don't count, so retrying a pinned name after its stack exited still works.)

The variables that do it (see [docs/ENV.md](ENV.md#compose--scripts-shell-only--not-read-by-go)):

```sh
# ci-run.sh derives all of this from one name:
WARDYN_CI_PROJECT="$CI_JOB_ID" scripts/ci-run.sh
#   -> COMPOSE_PROJECT_NAME=$CI_JOB_ID   (compose bookkeeping + unnamed volumes)
#   -> WARDYN_NS=$CI_JOB_ID              (container names, network, recordings volume)
#   -> WARDYN_UP_PORT=0, WARDYN_PG_PORT=0, WARDYN_REGISTRY_PORT=0,
#      WARDYN_SSH_PORT=0, WARDYN_UI_SANDBOX_PORT=0 (OS-assigned ephemeral host ports)

# Driving compose directly (no ci-run.sh) needs the same, set by hand — seven
# variables if another job may run at the same time:
COMPOSE_PROJECT_NAME=job-a WARDYN_NS=job-a WARDYN_UP_PORT=0 WARDYN_PG_PORT=0 \
  WARDYN_REGISTRY_PORT=0 WARDYN_SSH_PORT=0 WARDYN_UI_SANDBOX_PORT=0 \
  docker compose -p job-a -f deploy/compose/docker-compose.yaml up -d
```

- `WARDYN_NS` and `COMPOSE_PROJECT_NAME` must be the **same** value, and `WARDYN_NS` must match wardynd's `WARDYN_INTERNAL_NETWORK` (compose derives it from `WARDYN_NS`) — otherwise a run's proxy sidecar joins another job's bridge.
- Leaving `WARDYN_CI_PROJECT` unset is fine: the default is unique per invocation.
- Isolation is not asserted, it is tested: `make test-e2e-concurrent` ([`scripts/test-concurrent.sh`](../scripts/test-concurrent.sh)) brings up two stacks concurrently and checks that
  - both come up healthy with no collision,
  - that a container on job A's network cannot reach job B's wardynd,
  - that A survives B's `down --volumes`,
  - and that each tears down independently.
- It needs a live daemon and `wardyn/wardynd:local` (the target builds it if absent), so it is outside the per-push CI; the `test-e2e-concurrent` job in [.github/workflows/nightly.yml](../.github/workflows/nightly.yml) runs it nightly.
- Its `compose_ns` helper scopes the same five ports as `ci-run.sh` above.

> [!WARNING]
> **Scope of this:** one trusted operator on one host. Concurrent jobs share the docker daemon, so this is job *isolation*, not a multi-tenant boundary — a job that can reach the daemon can reach everything on it.

## Operator scripts that are deliberately not in CI

These scripts stay out of the per-PR [`ci.yml`](../.github/workflows/ci.yml) **by design**. This is not an oversight — most run in no workflow at all; where a nightly job covers one, its entry says so:

- **[`scripts/test-podman.sh`](../scripts/test-podman.sh)** — rootless Podman divergence probe.
  - It needs root-installed prerequisites (podman, `uidmap`, crun, fuse-overlayfs, slirp4netns, `/etc/subuid`+`/etc/subgid`) and a `podman.socket` the runner points `DOCKER_HOST` at.
  - GitHub's `ubuntu-latest` runs dockerd, so a CI copy would test nothing it doesn't already test.
  - Run it by hand when re-validating the Podman claim in [threatmodel/THREAT-MODEL.md](../threatmodel/THREAT-MODEL.md).
- **[`scripts/stage-agent-binary.sh`](../scripts/stage-agent-binary.sh)** — stages a checksum-verified agent CLI binary into `deploy/images/<agent>/` for corp networks where public npm is blocked.
  - The output is gitignored and the default image build installs from npm, so CI has nothing to run; it is invoked by an operator before `make agent-images-core` (see [deploy/images/README.md](../deploy/images/README.md)).
- **[`scripts/run-e2e-byoi.sh`](../scripts/run-e2e-byoi.sh)** (`make test-e2e-byoi`) — live BYOI wrap + selftest proof.
  - It requires a wardynd ALREADY running with `-envbuild` and a staged `WARDYN_ENVBUILD_TOOLS_DIR`, and — unlike the SSH and UI-sandbox scripts below — starts no control plane of its own.
  - That is setup this repo does not put on every PR,
  - so it runs in [`nightly.yml`](../.github/workflows/nightly.yml)'s `byoi-e2e-live` job (which boots the compose stack first, the way `ci.yml`'s `helm-install-test` job does in its desktop-envelope half) rather than in `ci.yml`, and remains runnable by hand.
  - See [RELEASING.md](../RELEASING.md) when re-validating BYOI.
- **[`scripts/run-e2e-ui-sandbox.sh`](../scripts/run-e2e-ui-sandbox.sh)** (`make test-e2e-ui-sandbox`) — live UI-sandbox relay proof:
  - the ticket → enter → cookie → code-server handoff on the second origin,
  - the `ui.*` audit rows (and the `session.attach` row that must NOT appear),
  - the header strips both ways,
  - and the pooled-exec baseline.
  - It brings up its own uniquely-named compose stack on its own ports and tears it down on every exit path, and it builds `wardyn/agent-vscode:local` (`make agent-image-vscode`, +~228 MiB) if that image is not already local —
  - too heavy for every PR, so it runs in `nightly.yml`'s `ui-sandbox-e2e-live` job rather than `ci.yml`, and remains runnable by hand.
  - Needs Docker; self-skips unless `WARDYN_TEST_DOCKER=1`.
- **[`WARDYN_KIND_SSO_PROFILE=ado scripts/kind-sso-walk.sh`](../scripts/kind-sso-walk.sh)** ([`scripts/lib/ kind-sso-walk-ado.sh`](../scripts/lib/kind-sso-walk-ado.sh)) — live Azure DevOps SSO walk on kind:
  - the console signs in against a fake Entra tenant instead of Dex,
  - the member's login captures their Azure DevOps credential,
  - and their run redeems it (REST + `git ls-remote`) through the proxy's gate and broker.
  - It needs its own kind cluster, image tag and port ([`deploy/kind/sso/README.md`](../deploy/kind/sso/README.md), "The Azure DevOps profile"), so it runs in `nightly.yml`'s `kind-sso-ado-walk` job rather than `ci.yml`, and remains runnable by hand.
  - Needs `kubectl`/`helm`/`kind`; self-skips unless `WARDYN_TEST_K8S=1`.
- **[`scripts/compose-sso-roles.sh`](../scripts/compose-sso-roles.sh)** — the role walk on both compose-shaped SSO deployments (`mprime`, the desktop member-mode envelope, and `compose-sso`, the plain `docker compose --profile sso` stack), hermetic and local:
  - every Dex identity — admin, allowlist-only operator, security admin, two members and one that matches no role — signed in through the real console on each shape.
  - It brings up its own uniquely-named compose project per shape and tears it down on every exit path, so it runs in `nightly.yml`'s `compose-sso-roles` job rather than `ci.yml`, and remains runnable by hand.
  - Needs Docker; self-skips unless `WARDYN_TEST_SSO_ROLES=1`.
- **[`scripts/k6-kind-conformance.sh`](../scripts/k6-kind-conformance.sh)** — the Kubernetes runner's kind conformance:
  - per kubelet version (the 1.31 floor and the latest kind image) it creates a `wardyn-k6-<pid>` cluster on Calico, stands up a real organisation and enrols the `wardyn-runnerd` chart with it.
  - [`test/k6`](../test/k6) then runs under runnerd's own token; its bind/delete race cases gate the unbound-pod arm.
  - It deletes its clusters and images on exit, and runs in `nightly.yml`'s `k6-kind-conformance` job, one leg per kubelet and suite (`K6_SUITES`); self-skips unless `WARDYN_TEST_K8S=1`.
  - `K6_SUITES=workload` runs `TestK6Workload` instead, with a throwaway in-cluster Dex for real sign-ins. A bound namespace's WardynWorkload launches a run on the bound pool; an unbound one is refused on its status. The `k6-workload-e2e` job runs it.
- **[`scripts/kind-components-walk.sh`](../scripts/kind-components-walk.sh)** and **[`scripts/kind-run-output-walk.sh`](../scripts/kind-run-output-walk.sh)** each make and delete their own quickstart cluster, and run in `nightly.yml`'s `kind-acceptance-walks` job; self-skip unless `WARDYN_TEST_K8S=1`.
- **[`scripts/runner-compat-walk.sh`](../scripts/runner-compat-walk.sh)** — runner and organisation version compatibility against the newest published 0.9.Z release:
  - this tree's `wardyn-runnerd` registers with the released organisation and completes one exec run. Below 0.9.3 that run must fail instead, with the error that says to upgrade the organisation first.
  - the released `wardyn-runnerd` serves a candidate runner's state directory and completes a run, then registers with this tree's organisation and completes one.
  - Runs in `nightly.yml`'s `runner-compat-walk` job, which `release.yml` watches; needs a native Docker engine; self-skips unless `WARDYN_TEST_DOCKER=1`.
- **[`scripts/run-e2e-ssh-k8s.sh`](../scripts/run-e2e-ssh-k8s.sh)** (`make test-e2e-ssh-k8s`) — the SSH gateway proven against a **Pod** rather than a container: the run's sandbox confirmed through `kubectl`, `ssh <run-id>@host <cmd>` over the k8s exec lane,
  - a nonzero exit code surviving the exec path's out-of-band status channel, the interactive shell reaching tmux, the `ssh.exec` / `session.attach{transport:ssh}` rows for both,
  - and both authorization arms — a second principal's `member` key refused on a run it does not own (audited `ssh.authenticate` failure) and a third principal's `admin` key reaching that same run with `data.override=true`.
  - Those two principals go in through `kubectl exec deploy/postgres`, because the API only ever stamps the *caller's* key and this install has one credential.
  - It does **not** create or delete a cluster — it runs against the one `make kind-quickstart` leaves behind, and cleans up only its own run and the keys it registered.
  - Needs `kubectl` and a real `ssh` client; self-skips unless `WARDYN_TEST_K8S=1`.
- **[`deploy/kind/quickstart.sh`](../deploy/kind/quickstart.sh)** (`make kind-quickstart`, `make kind-down`) — not a test at all: it builds `wardynd`, creates a `kind` cluster with a pinned Calico CNI and the k8s runner substrate on,
  - `helm install`s the chart, waits for a healthy control plane,
  - and prints the URL and admin token it minted.
  - CI proves the same install path through its own `helm-install-test` and `conformance-k8s` jobs rather than by running this target;
  - an operator runs it to get the cluster the target above needs, and to rehearse the day-2 commands in [OPERATIONS.md](OPERATIONS.md#kubernetes-day-2).
  - It binds `127.0.0.1:8080`, so a leftover compose stack on that port fails it loudly at cluster-create.

Notes on this repository's own GitHub Actions (jobs, required checks, nightly scope, flake history) are for contributors; they live in [REPO-CI.md](REPO-CI.md), which also describes the image-build knobs `WARDYN_MITM_CA_DIR`, `WARDYN_TRUSTED_CA_PATH` and `WARDYN_BUILD_GOMODCACHE`.

## Driving an existing control plane instead

- If you already run wardynd somewhere, set `WARDYN_URL` + `WARDYN_CI_TOKEN` and `ci-run.sh` launches there (see "[CI's identity](#cis-identity)" above).
- Or use the CLI directly — fully non-interactive with `WARDYN_URL` + `WARDYN_TOKEN`, the CI service identity's `wdn_` token, not a person's.
  - The shared `WARDYN_ADMIN_TOKEN` is no substitute: under OIDC it holds no model credential to launch a model run with.
- `--task-mode exec` runs the task as a plain shell command, so **no `--agent` is needed** — the image is what the run needs.

```sh
wardyn run --image ubuntu:24.04 --task-mode exec \
  --task 'make test' --policy-file ci.json --dry-run   # resolve + check, launch nothing
wardyn run --image ubuntu:24.04 --task-mode exec \
  --task 'make test' --policy-file ci.json --wait --timeout 30m
wardyn run get <id> --json     # final state, resolved image
wardyn run grants <id>         # what the run was ELIGIBLE for (not a service token: 403 service_scope)
wardyn audit <id> --json       # a service token reads only its own runs' rows
```

- `--dry-run` posts the same body to `POST /api/v1/runs/preflight`, a dry-run of launch resolution that mints nothing and prints the `setup_items` blockers plus the confinement class that would be enforced. A class raised to CC3 by the blast-radius floor also carries `confinement_raise_cause: blast_radius_floor` and a warning.
- `--placement remote|local` chooses where the sandbox lives. A local run goes to your own runners through `--pool <id>` or your default pool, else is refused `runner_pool_required`. `--runner <id>` pins one of your runners in that pool; unset, the pool picks an online one with room. Other refusals include `runner_offline` and `runner_at_capacity`.
- `ci-run.sh` calls the same endpoint before launching.
- A command that must stay up is a [service](SERVICES.md).

**Retrying a launch.** `wardyn run` sends an `Idempotency-Key` and never retries a create itself. To make a job re-run safe, pass a key that is the same on every re-run of the job and differs between jobs and between the legs of a matrix. `ci-run.sh` reads it from `WARDYN_CI_IDEMPOTENCY_KEY`, and both example pipelines set it:

```sh
WARDYN_CI_IDEMPOTENCY_KEY="${{ github.run_id }}-${{ github.job }}-${{ strategy.job-index }}"   # GitHub Actions (job-index is the matrix leg)
WARDYN_CI_IDEMPOTENCY_KEY="${BUILD_BUILDID}-${SYSTEM_JOBNAME}-${SYSTEM_JOBPOSITIONINPHASE:-0}"   # Azure Pipelines (position is set under strategy: parallel)
wardyn run ... --idempotency-key "$WARDYN_CI_IDEMPOTENCY_KEY"      # the CLI directly
```

- For 24 hours the same key and request get the first run back; `wardyn run` says so on stderr. A different request with the key gets `422` `idempotency_key_reused`.
- Re-run only once every control-plane replica runs 0.10.0 or later. An older replica ignores the key and starts a second run.

**Provisioning the CI principal, once, against this deployment:**

```sh
# 1. An admin creates the service identity, naming its sponsor:
#      wardyn service-identity create --name ci-bot --sponsor <principal>
#    The sponsor signs in to the console and mints its token from Account,
#    "Service identities I sponsor" — shown exactly once. Store it as the
#    pipeline secret WARDYN_CI_TOKEN; export it as WARDYN_TOKEN for the calls
#    below.

# 2. Store its model-provider credential once (repeat per provider CI needs):
curl -sS -X PUT "$WARDYN_URL/api/v1/model-providers/$PROVIDER_ID/credential" \
  -H "Authorization: Bearer $WARDYN_TOKEN" -d '{"value":"'"$KEY"'"}'

# 3. Confirm it is live before wiring the pipeline up to launch anything:
wardyn setup status --json | jq --arg p "$PROVIDER_ID" \
  '.provider_access[] | select(.provider == $p)'
```

## Apply from a pipeline

A pipeline can keep a [workload](operations/workloads.md) declared with `wardyn apply`. Use a service identity's token as `WARDYN_TOKEN`, as above, with `WARDYN_URL`.

- A service identity's token may `apply`, `diff`, `apply --dry-run`, `workload list|get|delete|restart` and read runs.
- `apply` exits `0` when the workload is applied, `1` when the organisation refuses the new run (the spec is stored and the old run keeps running). [Exit codes](operations/workloads.md#apply)
- Before the approval gate, run `wardyn apply --dry-run`, as both examples do. It exits `1` when the apply would be refused and stores nothing. It calls `POST /api/v1/workloads/{name}/preview`, which answers created, updated or unchanged, each changed field, whether a run starts, and any refusal. [Preview an apply](operations/workloads.md#preview-an-apply)
- Put the step behind your platform's approval gate: a GitHub environment with required reviewers, or an Azure DevOps environment with approvals.
- The workload runs under the service identity's walls and grants, like any run it launches.

These two files are examples. No Wardyn job runs them: [docs/ci/github-actions-apply.yml](ci/github-actions-apply.yml) and [docs/ci/azure-pipelines-apply.yml](ci/azure-pipelines-apply.yml).

## Images

- `wardynd` publishes to `ghcr.io/cjohnstoniv/wardynd` after CI passes on `main` ([.github/workflows/publish-image.yml](../.github/workflows/publish-image.yml));
- every release tag publishes all ten images (`wardynd`, `wardynd-fips`, `wardyn-proxy`, `wardyn-runnerd`, `agent-base`, `agent-codex-cli`, `agent-aws-sso`, `agent-vscode`, `agent-novnc`, `agent-novnc-signin`) cosign-signed, each with an attested SBOM and build provenance — see [VERIFY.md](VERIFY.md) to check them —
- ([.github/workflows/release.yml](../.github/workflows/release.yml) — see [RELEASING.md](../RELEASING.md) and the Helm chart's [README](../deploy/helm/wardyn/README.md)).
- This BYOA pipeline (`ci-run.sh`) does not consume them, though: it still builds wardynd, the `wardyn-proxy` sidecar, and the agent image from source on every invocation (a few minutes per job).
- [`scripts/up.sh`](../scripts/up.sh) now pulls the published images instead, falling back to a build when any is missing; `ci-run.sh` has not yet been switched to the same path, so a pipeline still pays the build.
- Wiring it up is open work.
