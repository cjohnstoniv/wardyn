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
| `WARDYN_CI_TOKEN` | with `WARDYN_URL`: the CI principal's own `wdn_` token — every `wardyn` call authenticates as it | **required** with `WARDYN_URL` |
| `WARDYN_CI_MODEL_PROVIDER` | with `WARDYN_URL`, harness mode: the model provider the run uses, checked before launch | **required** there |
| `WARDYN_CI_TIMEOUT` | `wardyn run --wait` bound | `30m` |
| `WARDYN_CI_OUT` | artifact dir (`run.json`, `audit.json`, `run.log`, `session.cast` — the run's terminal recording, when it has one) | `./ci-artifacts` |
| `WARDYN_CI_KEEP` | throwaway stack: `1` = leave it up for debugging | unset |
| `WARDYN_CI_SKIP_BUILD` | throwaway stack: `1` = reuse existing local images | unset |
| `WARDYN_DOCKER_SOCK` | throwaway stack: which host Docker socket the job drives (two-daemon hosts: pick the daemon with the runtimes you need) | `/var/run/docker.sock` |
| `WARDYN_ADMIN_TOKEN` | throwaway stack: its own bootstrap bearer, gone with the stack; never sent to `WARDYN_URL` | `demo-admin-token` |

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
  - Every call authenticates as `WARDYN_CI_TOKEN` — a **dedicated CI principal**'s own `wdn_` token — and `WARDYN_ADMIN_TOKEN` is cleared for those calls, since the CLI would otherwise try it first.
  - A harness run needs `WARDYN_CI_MODEL_PROVIDER`: `ci-run.sh` reads that identity's `provider_access` (`wardyn setup status --json`), stores nothing, and fails naming the provider unless its credential is `live` or `expiring` (it warns on `expiring`).
  - It then launches on exactly that provider (`--model-provider`).

* The CI principal is a user in your IdP (or a local user) who signs in once, mints their own token (`POST /api/v1/me/tokens`, or the console),
  * and stores their model-provider credential once (`PUT /model-providers/{id}/credential`, or the console) — see "[Provisioning the CI principal](#driving-an-existing-control-plane-instead)" below.
* Its audit trail then says "CI", and it can be scoped with `capModelProvider`/`capAgent` like any other person, rather than one human's token paying for and being blamed for every pipeline run.
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
| _clean detach = `0`_ | `wardyn run attach`: a clean detach is `0`; see [below](#clean-detach) |

- `wardyn run get <id> --json` (or the `run.complete` audit event) always has the authoritative outcome — check it when the process's own exit status alone doesn't say why a run didn't complete.
- That is also how a pipeline tells the two meanings of `1` apart:
  - a **usage** failure never created a run, so there is no run id to get;
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
- This is `exitCodeFor`'s catch-all in [`cmd/wardyn/main.go`](../cmd/wardyn/main.go), so it is also what a future local failure lands on.
- **Also** `run --wait`: see [the row below](#exit-codes)

#### ssh's own remote status

- `wardyn run ssh` (once connected): the process's exit code is ssh(1)'s own remote exit status, passed straight through — **not** this taxonomy's `2`-`5`.
- Those still classify a FAILED *connection attempt* (gateway disabled, no advertised address, handshake rejected) that never got as far as running ssh(1).
- A remote command that happens to exit `2`/`3`/`4`/`5`/`124` is coincidence, not this table — check it against what you ran, not against this list.
- A signal-killed ssh child (`exec.ExitError.ExitCode()` is `-1` in that case) is clamped to `1` instead of leaking a negative code out.

#### Clean detach

- `wardyn run attach`: a clean detach (TERM/HUP/INT, the remote side closing, or stdin EOF on a piped/non-tty session) is `0`, never re-labelled as a task result —
  - attach has no task exit code of its own to report.
- **Not** Ctrl-C on the primary (tty) path: raw mode clears ISIG, so it is relayed to the remote PTY as input rather than detaching locally.
- A FAILED attempt to open the session (rejected/unreachable) classifies with the same `2`-`5` as any other request.

## Writing a CI policy

- `--policy-file` accepts **JSON or YAML** (same schema); YAML also lets you keep inline comments.
- `wardyn policy render -f <file>` converts either to canonical JSON and rejects a misspelled field, so you can validate a policy in the pipeline before launching.
- Start from [`examples/policies/ci.json`](../examples/policies/ci.json) — the unattended baseline — and add exactly what the task needs:
  - **`"first_use_approval": "always_deny"`** — non-negotiable for unattended runs. Anything off the allowlist is hard-denied instantly; nothing ever waits on a human. (`wait_for_review` holds connections for a reviewer; `deny_with_review` files approvals nobody will decide.)
  - **Complete `allowed_domains`** — list every host the task legitimately needs (package registries, your model provider). The sealed default (empty list) means the sandbox can reach nothing. GitHub is the exception: it is reached through the Wardyn git-broker (see below), not via `allowed_domains`.
  - **No `requires_approval: true` grants** — an approval-gated credential never mints without a human. Use `requires_approval: false` grants whose secret the CI principal already holds on the control plane.
  - **Bound the run** — `auto_stop_after_sec` (ci.json: 1 hour) is the reaper backstop behind `WARDYN_CI_TIMEOUT`.

> [!NOTE]
> - Cloning a **GitHub** repo (`WARDYN_CI_REPO`) does **not** put `github.com` in `allowed_domains`. It is routed through the Wardyn git-broker: add a `github_token` grant and the run reaches only that repo (via `wardyn-proxy`, token minted proxy-side, never in the sandbox).
> - An un-granted GitHub repo is denied.
> - A non-GitHub SCM host still needs its own `allowed_domains` entry plus a `git_pat`/`ssh_key` grant whose secret the CI principal holds on the control plane (`WARDYN_URL`).

### Model access for harness mode (running a real agent)

- Model access is the CI principal's own credential for a model provider on the control plane `WARDYN_URL` names (see "[CI's identity](#cis-identity)" above):
  - it stores it once, through `PUT /model-providers/{id}/credential` (or the console),
  - and the pipeline sets `WARDYN_CI_MODEL_PROVIDER` to that provider's id.
- Nothing is seeded pre-run and the policy carries no key; there is no key on the pipeline's env block to rotate or leak.
- The throwaway stack has no such credential, so it runs `exec` mode only.

> [!WARNING]
> - **Claude subscription: not available in CI, deliberately.**
> - A subscription credential belongs to one human, and CI runs work on behalf of everyone who can trigger the pipeline — so wiring one here means that person's subscription serves other people's work.
> - Anthropic's terms for running Claude Code in agent infrastructure require each end user to authenticate with their own credential and prohibit intermediating usage on their behalf, which would put **you**, the operator, in breach rather than Wardyn.

- The shared-subscription switches are retired ([`ENV.md`](ENV.md) lists them).
- Give the CI principal an **API-key** or **Bedrock** provider instead, with its own credential.

### Least-privilege, derived not guessed

Don't hand-author the allowlist for a complex task — record it once:

1. In a trusted environment, run the task under a permissive policy.
2. `wardyn record synthesize <run-id>` previews the least-privilege policy Wardyn derived from the run's audit trail; `wardyn record save <run-id> --name my-ci-task` stores it.
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
  - UI, Postgres, AND the devcontainer-build registry sidecar (`registry`, a `wardynd` dependency — `up -d postgres wardynd` always starts it too) —
  - so parallel invocations don't collide on container names, the control-plane network, the recordings volume, or any of those three host ports,
  - and one job's `down --volumes` never tears down another's.
- The default project name's suffix comes from `/dev/urandom`,
  - so it is unique even between jobs that cannot see each other's PIDs (the usual shape: every job in its own container, all of them driving one shared host Docker socket).
- And because a job's teardown is `down --volumes`, `ci-run.sh` refuses to start at all when its project name already has **running** containers —
  - a pinned `WARDYN_CI_PROJECT` reused while the previous job is still live is an error, not a silent teardown of that job.
- (Stopped leftovers don't count, so retrying a pinned name after its stack exited still works.)

The variables that do it (see [docs/ENV.md](ENV.md#compose--scripts-shell-only--not-read-by-go)):

```sh
# ci-run.sh derives all of this from one name:
WARDYN_CI_PROJECT="$CI_JOB_ID" scripts/ci-run.sh
#   -> COMPOSE_PROJECT_NAME=$CI_JOB_ID   (compose bookkeeping + unnamed volumes)
#   -> WARDYN_NS=$CI_JOB_ID              (container names, network, recordings volume)
#   -> WARDYN_UP_PORT=0, WARDYN_PG_PORT=0, WARDYN_REGISTRY_PORT=0 (OS-assigned ephemeral host ports)

# Driving compose directly (no ci-run.sh) needs the same, set by hand — five
# variables if another job may run at the same time:
COMPOSE_PROJECT_NAME=job-a WARDYN_NS=job-a WARDYN_UP_PORT=0 WARDYN_PG_PORT=0 \
  WARDYN_REGISTRY_PORT=0 \
  docker compose -p job-a -f deploy/compose/docker-compose.yaml up -d
```

- `WARDYN_NS` and `COMPOSE_PROJECT_NAME` must be the **same** value, and `WARDYN_NS` must match wardynd's `WARDYN_INTERNAL_NETWORK` (compose derives it from `WARDYN_NS`) — otherwise a run's proxy sidecar joins another job's bridge.
- Leaving `WARDYN_CI_PROJECT` unset is fine: the default is unique per invocation.
- Isolation is not asserted, it is tested: `make test-e2e-concurrent` ([`scripts/test-concurrent.sh`](../scripts/test-concurrent.sh)) brings up two stacks concurrently and checks that
  - both come up healthy with no collision,
  - that a container on job A's network cannot reach job B's wardynd,
  - that A survives B's `down --volumes`,
  - and that each tears down independently.
- It needs a live daemon and `wardyn/wardynd:local` (the target builds it if absent), so it is a manual/pre-release check, not a CI job.
- Its `compose_ns` helper scopes the same `WARDYN_UP_PORT`/`WARDYN_PG_PORT`/ `WARDYN_REGISTRY_PORT` trio as `ci-run.sh` above.

> [!WARNING]
> **Scope of this:** one trusted operator on one host. Concurrent jobs share the docker daemon, so this is job *isolation*, not a multi-tenant boundary — a job that can reach the daemon can reach everything on it.

## Operator scripts that are deliberately not in CI

These scripts stay out of the per-PR [`ci.yml`](../.github/workflows/ci.yml) **by design**. This is not an oversight — most run in no workflow at all; where a nightly job covers one, its entry says so:

- **[`scripts/test-podman.sh`](../scripts/test-podman.sh)** — rootless Podman divergence probe.
  - It needs root-installed prerequisites (podman, `uidmap`, crun, fuse-overlayfs, slirp4netns, `/etc/subuid`+`/etc/subgid`) and a `podman.socket` the runner points `DOCKER_HOST` at.
  - GitHub's `ubuntu-latest` runs dockerd, so a CI copy would test nothing it doesn't already test.
  - Run it by hand when re-validating the Podman claim in [threatmodel/THREAT-MODEL.md](../threatmodel/THREAT-MODEL.md).
- **[`scripts/stage-agent-binary.sh`](../scripts/stage-agent-binary.sh)** — stages a checksum-verified agent CLI binary into `deploy/images/<agent>/` for corp networks where public npm is blocked.
  - The output is gitignored and the default image build installs from npm, so there is nothing for CI to run; it is invoked by an operator before `make agent-images-core` (see [deploy/images/README.md](../deploy/images/README.md)).
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
- **[`scripts/run-e2e-ssh-k8s.sh`](../scripts/run-e2e-ssh-k8s.sh)** (`make test-e2e-ssh-k8s`) — the SSH gateway proven against a **Pod** rather than a container: the run's sandbox confirmed through `kubectl`, `ssh <run-id>@host <cmd>` over the k8s exec lane,
  - a nonzero exit code surviving that lane's out-of-band status channel, the interactive shell reaching tmux, the `ssh.exec` / `session.attach{transport:ssh}` rows for both,
  - and both authorization arms — a second principal's `member` key refused on a run it does not own (audited `ssh.authenticate` failure) and a third principal's `admin` key reaching that same run with `data.override=true`.
  - Those two principals go in through `kubectl exec deploy/postgres`, because the API only ever stamps the *caller's* key and this install has one credential.
  - It does **not** create or delete a cluster — it runs against the one `make kind-quickstart` leaves behind, and cleans up only its own run and the keys it registered.
  - Needs `kubectl` and a real `ssh` client; self-skips unless `WARDYN_TEST_K8S=1`.
- **[`deploy/kind/quickstart.sh`](../deploy/kind/quickstart.sh)** (`make kind-quickstart`, `make kind-down`) — not a test at all: it builds `wardynd`, creates a `kind` cluster with a pinned Calico CNI and the k8s runner substrate on,
  - `helm install`s the chart, waits for a healthy control plane,
  - and prints the URL and admin token it minted.
  - CI proves the same install path through its own `helm-install-test` and `conformance-k8s` jobs rather than by running this target;
  - an operator runs it to get the cluster the lane above needs, and to rehearse the day-2 commands in [OPERATIONS.md](OPERATIONS.md#kubernetes-day-2).
  - It binds `127.0.0.1:8080`, so a leftover compose stack on that port fails it loudly at cluster-create.

## This repository's own CI

Wardyn's own [`ci.yml`](../.github/workflows/ci.yml) runs on every pull request and push to `main`. Runner slots constrain throughput: with many open pull requests, queueing takes longer than execution. This section therefore measures check counts, runner-minutes and elapsed time.

- `pull_request:` carries no `branches:` filter — that field matches the PR's *base*, and the 0.8 working practice stacks lanes on `<kind>/<issue#>-<slug>` branches (#90), not on `main`, so a filtered trigger gave a stacked PR no checks at all.
- `push:` stays narrow to `main`, `master` and `feature/**`, since every other commit already gets a run from its own PR.
- It has no `release/**`: `release.yml` accepts CI by tree (#1461), and [`release-branch-checks.yml`](../.github/workflows/release-branch-checks.yml) runs only DCO and gitleaks there (#1466).
- The push-gate DCO exemption for GitHub-made merges needs gpg and GitHub's web-flow key id `B5690EEEBB952194`; if GitHub rotates that key, update the id in the Makefile's `dco` recipe.

Image-script tests set `WARDYN_MITM_CA_DIR` to a private temporary directory to isolate generated CA files.
The agent script defaults to `/tmp/wardyn`; deployment behavior is unchanged.

**Before and after #211**, measured from the GitHub Actions API:

- Job times over the 60 most recent completed `ci.yml` runs as of 2026-09-21 05:00Z (a "green run" is one of the 26 whose `build` passed);
- superseded runs over all 215 `ci.yml` runs created from 2026-09-20 01:34Z to 2026-09-21 05:03Z.

| | Before | After |
|---|---|---|
| Checks per pull-request run | 28 | 24 |
| Required contexts (branch protection) | 14 | 14, same names |
| Runner-minutes per green run, median | 84.2 | 74.6 (see below) |
| Longest job per green run, median | 18.4 min (`build`) | 18.4 min (`build`) |
| Green run, created to completed | median 50 min, range 19 to 86 | re-measure once runs on this workflow accumulate |
| Pull-request runs still queued or running when a newer push to the same pull request arrived | 24 of 176 | cancelled by the workflow's `concurrency` group |

- The after runner-minutes are the same 26 runs without the three `multi-arch build` cells (now `nightly.yml`'s `buildx-smoke`) and `screenshots-fresh` (now an advisory annotation from `diagrams`); every job that remains is unchanged, so its measured time carries over.
- No run is shorter than its longest job, so 19 minutes is the floor while `build` is the critical path.

**Where `build`'s time goes**, in one green run (35555137810, pre-#467): `make test-race` 537 s, `make cover-check` 395 s, `make lint` 120 s — two full passes over the same three tag sets (coverage, then race).

- #467 merged them: `cover-check`'s three `test-report` suites now run under `-race -covermode=atomic` in one pass each, and the `build` job no longer runs `make test-race` or `make build-docker` as separate steps (`cover-check` already compiles and races those tag sets).
- Re-measure `cover-check`'s new time once green runs accumulate on this workflow; it should land below the old 537+395=932 s combined, not above it.
- `make lint` has since gained the console's ESLint ([`ui/eslint.config.js`](../ui/eslint.config.js)), so the `go (lint)` leg also sets up pnpm and node; that install and lint pass are not in the 120 s above.

**Why `ui-e2e` runs as two shards.**

- It used to be one job, because `build` was the critical path and a shard repeats about 90 s of setup.
- Once `go` ran its tag sets in parallel and `test-pg` was split, `ui-e2e` became the longest job of a pull request: 518 to 531 s of Playwright in a job of about 10.5 minutes (runs 37392974500 and 37391169188).
- It now runs as `ui-e2e (1/2)` and `ui-e2e (2/2)`.
- Each shard takes every other spec of the sorted list (`WARDYN_E2E_SHARD`, [`scripts/run-ui-e2e.sh`](../scripts/run-ui-e2e.sh)), with the same three lanes and the same fresh seed per spec, so the per-spec isolation is unchanged.
- The four-eyes run and the `e2etmux` harness pins run once, in shard 1.
- It is not a required check, so it needs no aggregator.

**Per-job budget.** A job's budget is its `timeout-minutes`, at least twice its measured maximum with a ten-minute floor. Minutes, successful runs only. The old `build` samples (26 runs, median 18.4, maximum 18.9 minutes) preceded the tag-set split below; timings for the current aggregator and the new jobs remain pending:

| Check | Runs | Median | Max | Timeout |
|---|---|---|---|---|
| `changes` | – | pending | pending | 10 |
| `go` (lint, unit, docker, k8s matrix) | – | pending | pending | 40 |
| `build` | – | pending | pending | 10 |
| `conformance-k8s` | 58 | 11.2 | 12.8 | 35 |
| `ui-e2e` (1/2, 2/2 matrix) | 1 | 7.0 | 7.0 | 25 |
| `test-pg-shard` (api, store, race matrix) | – | pending | pending | 20 |
| `test-pg` (aggregator) | – | pending | pending | 10 |
| `ui` | 60 | 4.5 | 4.8 | 20 |
| `conformance` | 60 | 4.2 | 4.7 | 45 |
| `envbuild-integration` | 60 | 3.6 | 4.0 | 20 |
| `gates (staticcheck)` | 60 | 2.7 | 2.9 | 15 |
| `helm-install-test` | – | pending | pending | 20 |
| `trivy-wardynd` (check `trivy (wardynd)`) | 50 | 2.0 | 2.5 | 40 |
| `trivy-wardynd-fips` (check `trivy (wardynd-fips)`) | 49 | 2.0 | 2.3 | 40 |
| `trivy-agent-novnc` (check `trivy (agent-novnc)`) | 51 | 1.6 | 2.2 | 40 |
| `trivy-agent-vscode` (check `trivy (agent-vscode)`) | 50 | 1.6 | 2.0 | 40 |
| `notices` | 60 | 1.5 | 1.9 | 15 |
| `gates (licenses)` | 60 | 1.4 | 2.0 | 15 |
| `trivy-agent-codex-cli` (check `trivy (agent-codex-cli)`) | 51 | 1.4 | 1.7 | 40 |
| `trivy-agent-base` (check `trivy (agent-base)`) | 50 | 1.3 | 1.6 | 40 |
| `trivy-agent-aws-sso` (check `trivy (agent-aws-sso)`) | 52 | 1.3 | 1.8 | 40 |
| `gates (gitleaks)` | 60 | 0.8 | 1.0 | 15 |
| `trivy-wardyn-proxy` (check `trivy (wardyn-proxy)`) | 53 | 0.9 | 1.2 | 40 |
| `gates (govulncheck)` | 60 | 0.7 | 1.0 | 15 |
| `diagrams` | 60 | 0.6 | 0.9 | 10 |
| `compose` | 60 | 0.3 | 0.5 | 10 |
| `gates (license-headers)` | 60 | 0.3 | 0.4 | 15 |
| `helm` | 60 | 0.1 | 0.7 | 10 |
| `dco` | 60 | 0.1 | 0.1 | 10 |
| `main-red` | – | pending | pending | 10 |
| `notify-flaky` | – | pending | pending | 10 |
| `multi-arch build (agent-claude-code)`, nightly | 60 | 3.5 | 3.8 | 45 |
| `multi-arch build (wardynd)`, nightly | 60 | 3.1 | 3.5 | 45 |
| `multi-arch build (agent-aws-sso)`, nightly | 60 | 2.9 | 4.0 | 45 |

- `helm-install-test` now also runs the old `desktop-envelope` job's proof (#472).
- Before the merge the two took 3.2 and 2.5 minutes at most, so 20 minutes clears twice their sum.
- Fill in its row once the merged job has about ten green runs.

Re-measure (job name, runs, median and maximum over successful jobs):

```sh
gh run list -R cjohnstoniv/wardyn --workflow ci.yml --status completed --limit 60 \
    --json databaseId --jq '.[].databaseId' |
  while read -r id; do
    gh api "repos/cjohnstoniv/wardyn/actions/runs/$id/jobs?per_page=100" --jq '.jobs[]
      | select(.conclusion == "success")
      | [.name, ((.completed_at | fromdateiso8601) - (.started_at | fromdateiso8601))] | @tsv'
  done | sort -t$'\t' -k1,1 -k2,2n |
  awk -F'\t' '{n[$1]++; v[$1,n[$1]]=$2} END {for (j in n) printf "%-40s n=%-3d median=%5.1fm max=%5.1fm\n", j, n[j], v[j,int((n[j]+1)/2)]/60, v[j,n[j]]/60}' | sort
```

`make ci`, the local merge gate, prints the same kind of table for its own targets when it finishes or stops.

### Incremental CI

Three mechanisms (#932), on top of the per-job Go cache (#470) and the single race + coverage pass per tag set (#467):

1. **Tag sets in parallel.**
   - The `go` matrix job runs `go (lint)` (`make build tidy-check lint`), `go (unit)`, `go (docker)` and `go (k8s)` (`make test-report`, `-docker`, `-k8s`) on four runners instead of one after another.
   - Each test leg uploads its reports as `go-test-reports-<suite>`.
   - `build` waits for all four, downloads the three profiles and runs `make cover-union`, the same `COVER_MIN` floor over the same union that `make cover-check` enforces locally.
2. **Skip what a change cannot affect.**
   - The `changes` job classifies the pull request's changed paths (`git diff --name-only --no-renames HEAD^1 HEAD` on GitHub's merge commit, so a moved file counts at both its old and its new path).
   - Each path gets one class, by the first rule that matches, so where a file lives decides before its extension does:

   | Order | Paths | Class |
   |---|---|---|
   | 1 | `deploy/**` and `LICENSING.md` | backend |
   | 2 | `docs/**`, `threatmodel/**` | docs |
   | 3 | `ui/**` | ui |
   | 4 | anything else under a directory, `.md` files included | backend |
   | 5 | a `.md` at the repository root | docs |
   | 6 | any other file at the repository root | backend |

   - Rules 1 and 4 are why a `.md` is documentation only at the repository root.
   - Helm renders every file under `deploy/helm/wardyn/templates/` as a manifest, whatever its extension, and every Dockerfile copies `LICENSING.md` and [`deploy/images/README.md`](../deploy/images/README.md).
   - When any `.md` counted as docs, a NetworkPolicy written in `templates/x.md` skipped `helm`, `helm-install-test`, both conformance jobs, `test-pg`, the image scans and `notices`,
     - and deleting a copied `.md` broke every image build with none built on the pull request.
   - `TestCIClassifierPutsWhereAFileLivesBeforeItsExtension` pins the order.
   - [`scripts/check-helm-templates.sh`](../scripts/check-helm-templates.sh) holds the chart half without the classifier: it refuses any file under `templates/` that is not a `*.yaml`, a `*.tpl` or `NOTES.txt`, from `make lint` on every change and again from `make helm-lint`.
   - (`THIRD-PARTY-NOTICES.md`, the third `.md` the Dockerfiles copy, stays docs: it sets `notices`, and `make notices` fails when the file is missing.)
   - The classes set five outputs, each `true` when at least one changed path sets it:

   | Output | Set by | Read by |
   |---|---|---|
   | `code` | any ui or backend path | `ui-e2e`, `helm-install-test` |
   | `backend` | any backend path (Go, `deploy/**`, `scripts/**`, `.github/**`, …) | `go` (full suites or guard packages), `build` (union), `test-pg-shard`, `test-pg`, `conformance`, `conformance-k8s`, `envbuild-integration`, `helm`, `helm-install-test`'s kind half |
   | `ui` | `ui/**`, the `Makefile`, [`.github/workflows/ci.yml`](../.github/workflows/ci.yml) | `ui` (whole vitest suite, or only the files that read outside `ui/`) |
   | `images` | a `backend` path other than a Go `*_test.go` file | the eight `trivy (<image>)` jobs |
   | `notices` | `images`, [`ui/package.json`](../ui/package.json), [`ui/pnpm-lock.yaml`](../ui/pnpm-lock.yaml), `THIRD-PARTY-NOTICES.md` | `notices` |

   - A path that matches no earlier rule counts as backend, so a new directory runs everything until someone classifies it.
   - A push, a merge queue run, a pull request from a `train/*` branch and a pull request into `release/*` always run everything.
   - So, for example, a docs-only change skips `ui-e2e`, `helm-install-test`, both conformance jobs, `envbuild-integration`, `helm`, `test-pg`, `notices` and every `trivy` scan;
     - a console-only change skips the same except `ui-e2e` and `helm-install-test`'s desktop-envelope half;
     - a change to Go test files only skips the `trivy` scans and `notices`.
   - Go source counts as an image input:
     - of the 52 pull requests that changed non-test Go among the last 66, 43 changed an import of a module-qualified package, which is what decides which modules a binary links and so what `trivy` and `notices` report.
   - The Go jobs (`go`, `build`) never skip, whatever changed.
   - Dozens of Go test files read the docs, the CHANGELOG, `ui/src` or the workflows (the citation, CHANGELOG-freeze, RELEASING job-list and copy-parity guards among them),
     - so skipping Go on a docs-only change would let a docs change break the guards that check docs.
   - **Docs or console only: guard packages only.**
     - A change confined to the docs and `ui/` cannot change compiled Go, race behaviour or coverage.
     - So when `backend` is `false`, the three test legs do not run the race and coverage suites.
     - They run plain `go test -count=1`, with no race detector and no coverage, over the guard packages only.
     - Those are the directories of every `*_test.go` file that names `docs/`, `threatmodel/`, `ui/`, `.github/`, `CHANGELOG.md`, `RELEASING.md`, `AGENTS.md` or any `.md` file.
     - The step finds them from the test sources at run time, so a new guard is picked up without a list to maintain.
     - (Every Go test that walks the tree from its root filters to `.go` or `.sql` files, so none reads `ui/` without naming it.)
     - `go (unit)` runs them with no tags.
     - `go (docker)` and `go (k8s)` run, with their tag, only the packages whose guard files carry that build tag.
     - Today that is `./internal/runner/docker` for docker (`hardening_test.go` reads [`threatmodel/THREAT-MODEL.md`](../threatmodel/THREAT-MODEL.md)) and none for k8s, so the k8s leg says so and passes.
     - If the tagless leg finds no guard file at all, it fails: that would mean the search pattern broke.
     - `build` then needs every leg green and skips the coverage union, since no profiles were written.
     - `go (lint)` runs in full on every change: it carries the console's ESLint.
   - **No console change: the vitest files that read outside `ui/` only.**
     - When `ui` is `false`, the `ui` job still installs, audits, typechecks and builds, but a vitest verdict can move only in a test that reads a file outside `ui/`: the Go wire-parity tests and the `docs/design` canon pins.
     - A test reads a file through `node:fs` (or `child_process`, a cwd- or module-relative path, a `?raw` or glob import),
       - so the step runs the test files that do, plus the callers of the two helpers that read a path their caller passes (`copy-doc-parity.ts`, and `test-fixtures.ts`'s `expectNoOwnCopy`):
       - 76 of 341 files, 71 s where the whole suite takes 547 s, without coverage.
     - A relative path counts as leaving `ui/` when it climbs to the name of any entry at the repository root other than `ui`;
       - the step reads those names from the tree, so a new top-level directory needs no edit here.
     - It fails closed:
       - if any other module under `ui/src` reads files or names a path outside `ui/`, a test could read through it unseen, so the whole suite runs instead; and finding no test at all fails.
3. **Docker layer cache.**
   - `helm-install-test` (wardynd, wardyn-proxy), `conformance-k8s` (wardyn-proxy) and `conformance` (wardyn-proxy, agent-claude-code) build through `docker/build-push-action` with `cache-from: type=gha,scope=<image>`.
   - `cache-to` (`mode=max`) is written only from a push to `main`, like the Go caches, so pull requests read main's layers and add no cache entries of their own.
   - Not cached:
     - `trivy` (a cached `apt-get` layer would scan older packages than the release builds, which changes what the gate says),
     - the conformance agent image (`make build-conformance-agent-image`)
     - and `trivy`'s `agent-vscode`/`agent-novnc` jobs, which build `FROM` a local image that a buildx builder cannot see.

**Required checks and skipped jobs.**

- GitHub reports a job skipped by a job-level `if:` as "skipped", and branch protection accepts that as passing (pull request #1863, a console-only change, was mergeable with `envbuild-integration` skipped).
- A skipped job takes no runner, where a job that only skips its steps still waits for one:
  - in run 37392974500, a console-only pull request, `conformance` queued 1426 s to run for 6 s, `helm` 1779 s and `test-pg` 1785 s.
- So a job that a change cannot affect skips at the job level, under two rules:
  - A job reads the classification only as `!cancelled() && … != 'false'`. If `changes` *failed*, every output is empty, so the job runs instead of skipping.
  - A required job with a **matrix** never skips at the job level.
    - GitHub does not expand a skipped matrix: it reports one check with the name unexpanded (`gates (${{ matrix.gate }})`, the same row [`scripts/green-by-tree.sh`](../scripts/green-by-tree.sh) documents for the nightly),
      - so `gates (gitleaks)` would never report and the pull request would wait on it forever.
    - This is why the image scans are eight jobs (`trivy-wardynd` … `trivy-agent-novnc`, reporting as `trivy (<image>)`) and not a matrix:
      - their steps are written once and shared through a YAML anchor, and each job skips on `images == 'false'`.

So:

- `test-pg` (the aggregator over its `test-pg-shard` matrix, which fails unless every shard passed), `conformance`, `conformance-k8s`, `helm` and `envbuild-integration` skip on `backend == 'false'`, `notices` on `notices == 'false'`, the eight `trivy (<image>)` jobs on `images == 'false'`.
- `build` needs every `go` leg and runs with `if: !cancelled()`.
  - Its first step fails unless every leg passed, so the `build` context is green only when all four legs and the union floor are.
  - The union steps carry `if: needs.changes.outputs.backend != 'false'`, so only an explicit docs- or console-only classification skips them.
  - A failed or missing classification runs the full suites in the legs and requires the union.
- `ui` always runs and narrows only vitest (above).
- Non-required jobs (`ui-e2e`, `helm-install-test`) skip at the job level on `code`.
  - `helm-install-test` puts `backend != 'false'` on its kind half's steps, so a console-only change runs just its desktop-envelope half.
  - `notify-flaky` skips when both `ui-e2e` shards said their flaky list was empty, and runs on any other answer, including none.
- `compose`, `dco`, `diagrams` and `gates (…)` do not read the classification and run on every change.
  - (`helm` stays on `backend`, not on the chart's own paths: `make helm-lint` also renders [`examples/policies/demo.json`](../examples/policies/demo.json) and [`cmd/wardynd/testdata/values-0.7.yaml`](../cmd/wardynd/testdata/values-0.7.yaml).)
- Only a run whose five outputs are all `true` uploads the `ci-full-tree-<tree>` marker that [`scripts/green-by-tree.sh`](../scripts/green-by-tree.sh) accepts as release evidence: a run that narrowed anything did not test everything, even where every check it reported is green.

**Start order under the 20-job cap.**

- The account runs 20 jobs at once and a change to Go asks for about 35, so some wait, and GitHub picks which among the jobs that became ready together.
- The long jobs (`go`, `ui`, `ui-e2e`, `helm-install-test`, both conformance jobs, `envbuild-integration`, `test-pg-shard`) need only `changes`.
- The short ones (`gates`, `notices`, the `trivy` scans) also need `diagrams`, a 45-second job that starts with the run,
  - so they become ready about half a minute after the long jobs, which by then hold their runners, and share what is left.
- Nothing reads `diagrams`' result: each of them runs on `!cancelled()`, whether it passed or failed.
- The job they wait on has to start with the run: one that itself needs `changes` would send the short jobs through the queue a third time when the account is busy.
- Before this, a nine-minute job could sit behind eight two-minute scans: in run 37403763766 `conformance` queued 98 s and `go (unit)` 90 s while four `trivy` jobs started at once.

**Before and after.**

- "Before" is main's last green run before this change, 35918188245 (a push: every job ran).
- Wall time runs from the first job's start to the last job's end.
- The 7.3 minutes the run queued before any job started are not included.
- `build` itself queued 26.5 minutes for a runner.
- On this repository, runner slots and not job length set the wall time (see above), so the "after" rows are measured, not predicted.

| Run | `build` | `conformance-k8s` | `ui-e2e` | `test-pg` | Wall |
|---|---|---|---|---|---|
| Before: 35918188245, push to main | 22.0 min | 13.1 min | 8.0 min | 6.2 min | 41.1 min |
| After: 36043678020, full pull-request run | `go` legs: 3.2 (lint), 6.0 (unit), 5.1 (docker), 5.8 (k8s) min | 13.0 min | 11.2 min | 7.0 min | 14.0 min |

- Run 36043678020 changed Go and `ci.yml`, so every job ran.
- A pull request that touches one Go package, or a train pull request, runs the same full set of jobs.
- `build` is now only the aggregator, and `conformance-k8s` is the new critical path.

**By kind of change (2026-10-06).**

- One pull request of each kind, each run on its own (nothing else running in the account), against the last run of that kind before the classification gained `ui`, `images` and `notices`.
- "Jobs" counts the jobs that took a runner; a skipped job takes none.

| Change | Run | Jobs | Runner-minutes | Wall | Longest job |
|---|---|---|---|---|---|
| Everything, before: push to main | 37269326378 | 32 | 116.9 | 18.3 min | `test-pg` 17.4 min |
| Go only, after | 37405900397 | 35 | 108.2 | 9.1 min | `conformance-k8s` 8.9 min |
| Console only, before | 37392974500 | 31 | 70.0 | 41.9 min, of which up to 29.8 queued | `ui-e2e` 10.3 min |
| Console only, after | 37406693248 | 18 | 39.6 | 10.5 min | `ui` 10.3 min |
| Docs only, before | 36789150048 | 27 | 33.5 | 11.3 min, of which up to 7.7 queued | `ui` 6.6 min |
| Docs only, after | 37407613617 | 15 | 18.1 | 4.1 min | `go (unit)` 3.8 min |

- A change to Go still asks for 35 runners.
- Branch protection requires 24 contexts and a change to Go can affect every one of them,
  - so that count moves only if contexts are merged (the eight image scans into one or two jobs, the five `gates` into one), which is a branch-protection change.
- What changed for it is the wall time: `test-pg` and `ui-e2e` are split, the long jobs get their runners first, and vitest runs 76 files (71 s) where it ran 341 (547 s).
- A console-only change is held at 10.5 minutes by the whole vitest suite in `ui`; two vitest shards behind a `ui` aggregator would bring it to about 7.5 minutes for two more jobs.
- A run of everything (a push to `main`, a `train/*` pull request) runs that same `ui` job, so `ui` is its longest job as well.

## Driving an existing control plane instead

- If you already run wardynd somewhere, set `WARDYN_URL` + `WARDYN_CI_TOKEN` and `ci-run.sh` launches there (see "[CI's identity](#cis-identity)" above).
- Or use the CLI directly — it is fully non-interactive with `WARDYN_URL` + `WARDYN_TOKEN`, the CI principal's own `wdn_` token (not the shared `WARDYN_ADMIN_TOKEN`, which under OIDC holds no model credential to launch a model run with):
- `--task-mode exec` runs the task as a plain shell command, so **no `--agent` is needed** — the image is what the run needs, and naming an agent it never invokes was a formality this documentation used to demonstrate.

```sh
wardyn run --image ubuntu:24.04 --task-mode exec \
  --task 'make test' --policy-file ci.json --dry-run   # resolve + check, launch nothing
wardyn run --image ubuntu:24.04 --task-mode exec \
  --task 'make test' --policy-file ci.json --wait --timeout 30m
wardyn run get <id> --json     # final state, resolved image
wardyn run grants <id>         # what the run was ELIGIBLE for
wardyn audit <id> --json
```

- `--dry-run` posts the same body to `POST /api/v1/runs/preflight`, a dry-run of launch resolution that mints nothing and prints the `setup_items` blockers plus the confinement class that would be enforced.
- `--placement remote|local` chooses where the run's sandbox lives, and `--runner <id>` names the runner for a local run when more than one of yours is online. Unset, the server fills the placement only when exactly one is eligible. The CLI passes both through; the server refuses with a reason such as `placement_required`, `placement_unavailable` or `runner_offline`.
- `ci-run.sh` calls the same endpoint before launching.

**Provisioning the CI principal, once, against this deployment:**

```sh
# 1. Sign in to the console once as the CI principal and create its own token
#    from Account (POST /api/v1/me/tokens behind it) — shown exactly once.
#    Store it as the pipeline secret WARDYN_CI_TOKEN; export it as WARDYN_TOKEN
#    for the calls below.

# 2. Store its model-provider credential once (repeat per provider CI needs):
curl -sS -X PUT "$WARDYN_URL/api/v1/model-providers/$PROVIDER_ID/credential" \
  -H "Authorization: Bearer $WARDYN_TOKEN" -d '{"value":"'"$KEY"'"}'

# 3. Confirm it is live before wiring the pipeline up to launch anything:
wardyn setup status --json | jq --arg p "$PROVIDER_ID" \
  '.provider_access[] | select(.provider == $p)'
```

## Images

- `wardynd` publishes to `ghcr.io/cjohnstoniv/wardynd` after CI passes on `main` ([.github/workflows/publish-image.yml](../.github/workflows/publish-image.yml));
- every release tag publishes all seven images (`wardynd`, `wardyn-proxy`, `agent-base`, `agent-codex-cli`, `agent-aws-sso`, `agent-vscode`, `agent-novnc`) cosign-signed, each with an attested SBOM and build provenance — see [VERIFY.md](VERIFY.md) to check them —
- ([.github/workflows/release.yml](../.github/workflows/release.yml) — see [RELEASING.md](../RELEASING.md) and the Helm chart's [README](../deploy/helm/wardyn/README.md)).
- This BYOA pipeline (`ci-run.sh`) does not consume them, though: it still builds wardynd, the `wardyn-proxy` sidecar, and the agent image from source on every invocation (a few minutes per job).
- [`scripts/up.sh`](../scripts/up.sh) now pulls the published images instead, falling back to a build when any is missing; `ci-run.sh` has not yet been switched to the same path, so a pipeline still pays the build.
- Wiring it up is open work.
