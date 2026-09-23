# Wardyn CI — governed sandboxes in your pipeline

Run a sandboxed job from a CI/CD pipeline (GitHub Actions, Azure DevOps, or
anything with a docker daemon) with **no pre-running Wardyn, no UI, and no
human**. One script brings up a fresh control plane, launches one governed
run, waits for its outcome, collects artifacts, tears everything down, and
exits with the run's exit code — so your pipeline's pass/fail is the sandboxed
task's pass/fail.

This is a **BYOA (bring your own agent/container)** surface: you supply the
image — an agent harness, your test container, or any stock image — and Wardyn
supplies the governed sandbox around it (default-deny egress at the
`wardyn-proxy` sidecar, brokered never-resident credentials, confinement classes, an
append-only audit trail). It doesn't have to be an agent at all:
`task_mode=exec` runs a plain shell command under the same governance.

## Quick start

Copy the example for your CI system and adjust the env block:

- GitHub Actions: [`docs/ci/github-actions.yml`](ci/github-actions.yml)
- Azure DevOps: [`docs/ci/azure-pipelines.yml`](ci/azure-pipelines.yml)

Both are the same three steps: check out wardyn, run
[`scripts/ci-run.sh`](../scripts/ci-run.sh), upload `ci-artifacts/`.

```sh
# The whole flow, locally or on any runner with docker:
WARDYN_CI_IMAGE=ubuntu:24.04 \
WARDYN_CI_TASK_MODE=exec \
WARDYN_CI_TASK='echo hello from a governed sandbox' \
scripts/ci-run.sh
```

### Pin the wardyn checkout

Step two **executes shell out of that checkout** — `scripts/ci-run.sh`, and
everything it sources — inside the same job the same file tells you to seed
with `secrets.ANTHROPIC_API_KEY`. Fetched at tip of Wardyn's default branch,
that is a job where a push to a repository you do not control runs in front of
your credentials, on your runner, with your network position.

So both examples pin it: `ref: v<release>` on the Actions checkout,
`--branch v<release>` on the Azure clone. They ship pinned at the release that
shipped them; bump that deliberately, to a release you verified
([VERIFY.md](VERIFY.md)). A full commit sha (`ref: <40-hex>`, or a
`git checkout <sha>` after the clone) pins harder still — a tag can be moved,
a sha cannot. The same reasoning is why the third-party actions in these files
carry versions rather than floating refs.

## scripts/ci-run.sh

| Env | Meaning | Default |
|---|---|---|
| `WARDYN_CI_TASK` | task text (`exec` mode: the shell command) | **required** |
| `WARDYN_CI_IMAGE` | BYOA base image ref; wrapped + governed | unset (agent's own image) |
| `WARDYN_CI_TASK_MODE` | `harness` (run the agent) or `exec` (plain command) | `harness` |
| `WARDYN_CI_AGENT` | agent for harness mode / runner-tools source | `claude-code` |
| `WARDYN_CI_REPO` | `org/name` cloned into the workspace (needs egress + creds, see below) | unset (ephemeral scratch) |
| `WARDYN_CI_POLICY_FILE` | `RunPolicySpec` JSON | `examples/policies/ci.json` |
| `WARDYN_CI_SECRETS` | `name=value[,name=value...]` seeded into the secret store pre-run | unset |
| `WARDYN_CI_TIMEOUT` | `wardyn run --wait` bound | `30m` |
| `WARDYN_CI_OUT` | artifact dir (`run.json`, `audit.json`, `run.log`, `session.cast` — the run's terminal recording, when it has one) | `./ci-artifacts` |
| `WARDYN_CI_KEEP` | `1` = leave the stack up for debugging | unset |
| `WARDYN_CI_SKIP_BUILD` | `1` = reuse existing local images | unset |
| `WARDYN_DOCKER_SOCK` | which host Docker socket the job drives (two-daemon hosts: pick the daemon with the runtimes you need) | `/var/run/docker.sock` |

**If this deployment has an agent roster, the CI agent needs a row in it.**
`ci-run.sh` defaults `WARDYN_CI_AGENT` to `claude-code`, and since 0.7.2 a
deployment that HAS a `site_config.agent_providers` block refuses run creation
for an agent with no enabled row in it (`422`, `agentRosterRefusal`) — so a job
that worked against a roster-less install starts failing at the door the day an
admin writes the first roster row. It binds `exec` mode too: `ci-run.sh` always
passes `--agent`, since the agent is also where it sources the runner tools.
Either give the agent CI uses an enabled row (`PUT /api/v1/agent-providers`, or
the Providers screen) or point `WARDYN_CI_AGENT` at one that has one. A
deployment with NO `agent_providers` block refuses nothing — which is every
install upgraded from 0.7.1 until someone writes the first row.

### Exit codes

`ci-run.sh` propagates `wardyn run --wait`'s exit code. Every `wardyn`
command shares one taxonomy — codes `2`-`5` classify a failed CLI-to-control-
plane request itself (auth/client/server/network), independent of what the
run inside it did:

| Exit | Meaning |
|---|---|
| `0` | ok — the command succeeded (`run --wait`: the run `COMPLETED`, task/agent exited 0) |
| `2` | auth — the control plane rejected the request as unauthenticated/unauthorized. **Also** `run --wait`: the run ended `KILLED`, `STOPPED`, or `ARCHIVED` (lifecycle termination, not a task result) |
| `3` | client — any other non-2xx response: a 4xx (bad request, not found, conflict, ...), or an unfollowed 3xx redirect (an interposed proxy — the CLI never follows redirects) |
| `4` | server-5xx — the control plane returned a server error |
| `5` | network — couldn't reach the control plane at all (DNS, connection refused, TLS) |
| `124` | `run --wait` only: the wait timed out before the run reached a terminal state |
| `1` | the invocation failed **locally**, before or beside the request: a usage error (unknown flag or argument), a malformed id, an unreadable or schema-invalid `--policy-file`, or a response the CLI could not classify (e.g. a 2xx whose body did not decode). This is `exitCodeFor`'s catch-all in `cmd/wardyn/main.go`, so it is also what a future local failure lands on. **Also** `run --wait`: see the row below |
| _task's own code_ | `run --wait`: the run ended `FAILED` — the exit is the task/agent's own real exit code (from the `run.complete` audit event), or `1` if that code is missing/unreadable (never `0` on `FAILED`) |
| _ssh's own remote status_ | `wardyn ssh` (once connected): the process's exit code is ssh(1)'s own remote exit status, passed straight through — **not** this taxonomy's `2`-`5`. Those still classify a FAILED *connection attempt* (gateway disabled, no advertised address, handshake rejected) that never got as far as running ssh(1). A remote command that happens to exit `2`/`3`/`4`/`5`/`124` is coincidence, not this table — check it against what you ran, not against this list. A signal-killed ssh child (`exec.ExitError.ExitCode()` is `-1` in that case) is clamped to `1` instead of leaking a negative code out. |
| _clean detach = `0`_ | `wardyn attach`: a clean detach (TERM/HUP/INT, the remote side closing, or stdin EOF on a piped/non-tty session) is `0`, never re-labelled as a task result — attach has no task exit code of its own to report. **Not** Ctrl-C on the primary (tty) path: raw mode clears ISIG, so it is relayed to the remote PTY as input rather than detaching locally. A FAILED attempt to open the session (rejected/unreachable) classifies with the same `2`-`5` as any other request. |

`wardyn run get <id> --json` (or the `run.complete` audit event) always has
the authoritative outcome — check it when the process's own exit status alone
doesn't say why a run didn't complete. That is also how a pipeline tells the two
meanings of `1` apart: a **usage** failure never created a run, so there is no
run id to get; a `FAILED` run whose exit code was unreadable has one, and its
`run.complete` event says so.

## Writing a CI policy

`--policy-file` accepts **JSON or YAML** (same schema); YAML also lets you keep
inline comments. `wardyn policy render -f <file>` converts either to canonical
JSON and rejects a misspelled field, so you can validate a policy in the pipeline
before launching.

Start from [`examples/policies/ci.json`](../examples/policies/ci.json) — the
unattended baseline — and add exactly what the task needs:

- **`"first_use_approval": "always_deny"`** — non-negotiable for unattended
  runs. Anything off the allowlist is hard-denied instantly; nothing ever
  waits on a human. (`wait_for_review` holds connections for a reviewer;
  `deny_with_review` files approvals nobody will decide.)
- **Complete `allowed_domains`** — list every host the task legitimately
  needs (package registries, your model provider). The sealed default (empty
  list) means the sandbox can reach nothing. GitHub is the exception: it is
  reached through the Wardyn git-broker (see below), not via `allowed_domains`.
- **No `requires_approval: true` grants** — an approval-gated credential
  never mints without a human. Use `requires_approval: false` grants with
  secrets seeded via `WARDYN_CI_SECRETS`.
- **Bound the run** — `auto_stop_after_sec` (ci.json: 1 hour) is the reaper
  backstop behind `WARDYN_CI_TIMEOUT`.

Cloning a **GitHub** repo (`WARDYN_CI_REPO`) does **not** put `github.com` in
`allowed_domains` — it is routed through the Wardyn git-broker: add a
`github_token` grant and the run reaches only that repo (via `wardyn-proxy`,
token minted proxy-side, never in the sandbox). An un-granted GitHub repo is
denied. A non-GitHub SCM host still needs its own `allowed_domains` entry plus a
`git_pat`/`ssh_key` grant whose secret you seed via `WARDYN_CI_SECRETS`.

### Model access for harness mode (running a real agent)

Two paths work from zero prior state:

- **API key** (simplest): policy grants an `api_key` scoped to
  `api.anthropic.com` (see
  [`examples/policies/ci-claude-llm.json`](../examples/policies/ci-claude-llm.json)
  — `ci.json` plus exactly that grant and egress entry, so it's CI-safe as-is;
  `examples/policies/claude-llm.json` is a DEV ceiling, not a CI policy —
  `deny_with_review`, an approval-gated grant, and an unbounded run all need
  stripping before it belongs in a pipeline) and the pipeline seeds it:
  `WARDYN_CI_SECRETS=anthropic-api-key=$KEY`. The key is injected proxy-side;
  the sandbox only ever holds a placeholder.
- **AWS Bedrock**: set `WARDYN_BEDROCK_REGION`/`WARDYN_BEDROCK_MODEL` on the
  stack and seed a `bedrock-api-key` bearer secret (never-resident,
  proxy-injected).

- **Claude subscription: not available in CI, deliberately.** A subscription
  credential belongs to one human, and CI runs work on behalf of everyone who can
  trigger the pipeline — so wiring one here means that person's subscription serves
  other people's work. Anthropic's terms for running Claude Code in agent
  infrastructure require each end user to authenticate with their own credential and
  prohibit intermediating usage on their behalf, which would put **you**, the
  operator, in breach rather than Wardyn.

  Wardyn now refuses it structurally: shared subscription injection is limited to a
  single-user desktop posture (see `WARDYN_ALLOW_SHARED_SUBSCRIPTION` in
  [`ENV.md`](ENV.md)), and CI is not one. Use an **API key** or **Bedrock** above —
  both are per-deployment credentials that belong to the organisation rather than to
  a person, which is what a pipeline actually wants.

### Least-privilege, derived not guessed

Don't hand-author the allowlist for a complex task — record it once:

1. In a trusted environment, run the task under a permissive policy.
2. `wardyn record synthesize <run-id>` previews the least-privilege policy
   Wardyn derived from the run's audit trail; `wardyn record save <run-id>
   --name my-ci-task` stores it.
3. Export the stored policy's spec to your repo as the pipeline's
   `WARDYN_CI_POLICY_FILE` (set `first_use_approval` to `always_deny`).

What synthesis does and does not derive:

- The **allowlist** is proxy-observed egress only — the exact hosts the run was
  actually ALLOWED to reach. Never wildcarded (a recording cannot prove a
  wildcard is needed); a host that was only denied or only held pending is
  excluded and reported as a warning, since promoting it would widen past what
  even the open run was permitted.
- Observed **execs / connects / sensitive writes** are printed as review
  warnings, not policy: the policy model has no exec/connect/file allowlist,
  and `workspace_mounts` are operator-authored and never synthesized.
- **KNOWN GAP** — that exec/write evidence needs the opt-in eBPF/Tetragon
  sensor, and the sensor is blind inside CC3/Kata guests. `synthesize` does not
  say so: a CC3 (or sensor-off) run's proposal reads exactly like a
  fully-observed one. Read a silent proposal as "nothing was observed", not as
  "nothing happened".
- A recording proves only what the run HAPPENED to do, so synthesis fails
  toward escalation: it forces `allow_all_egress=false` and
  `first_use_approval=deny_with_review`. Step 3's `always_deny` is you
  overriding that for an unattended pipeline — tighter, and it means an
  un-recorded host fails the job instead of raising an approval nobody is
  watching.

## Concurrent jobs on a shared host

Several jobs can run on one build host at the same time, under one trusted
operator (e.g. a CI fleet on one service account). `scripts/ci-run.sh` already
does most of this for you: it scopes every compose object to a unique project +
namespace and binds the control plane's own host ports ephemerally — UI,
Postgres, AND the devcontainer-build registry sidecar (`registry`, a `wardynd`
dependency — `up -d postgres wardynd` always starts it too) — so parallel
invocations don't collide on container names, the control-plane network, the
recordings volume, or any of those three host ports, and one job's
`down --volumes` never tears down another's.

The default project name's suffix comes from `/dev/urandom`, so it is unique
even between jobs that cannot see each other's PIDs (the usual shape: every job
in its own container, all of them driving one shared host Docker socket). And
because a job's teardown is `down --volumes`, `ci-run.sh` refuses to start at
all when its project name already has **running** containers — a pinned
`WARDYN_CI_PROJECT` reused while the previous job is still live is an error,
not a silent teardown of that job. (Stopped leftovers don't count, so retrying
a pinned name after its stack exited still works.)

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

`WARDYN_NS` and `COMPOSE_PROJECT_NAME` must be the **same** value, and
`WARDYN_NS` must match wardynd's `WARDYN_INTERNAL_NETWORK` (compose derives it
from `WARDYN_NS`) — otherwise a run's proxy sidecar joins another job's bridge.
Leaving `WARDYN_CI_PROJECT` unset is fine: the default is unique per invocation.

Isolation is not asserted, it is tested: `make test-e2e-concurrent`
(`scripts/test-concurrent.sh`) brings up two stacks concurrently and checks that
both come up healthy with no collision, that a container on job A's network
cannot reach job B's wardynd, that A survives B's `down --volumes`, and that each
tears down independently. It needs a live daemon and `wardyn/wardynd:local` (the
target builds it if absent), so it is a manual/pre-release check, not a CI job.
Its `compose_ns` helper scopes the same `WARDYN_UP_PORT`/`WARDYN_PG_PORT`/
`WARDYN_REGISTRY_PORT` trio as `ci-run.sh` above.

**Scope of this:** one trusted operator on one host. Concurrent jobs share the
docker daemon, so this is job *isolation*, not a multi-tenant boundary — a job
that can reach the daemon can reach everything on it.

## Operator scripts that are deliberately not in CI

These scripts stay out of the per-PR `ci.yml` **by design**. This is not an
oversight — most run in no workflow at all; where a nightly job covers one,
its entry says so:

- **`scripts/test-podman.sh`** — rootless Podman divergence probe. It needs
  root-installed prerequisites (podman, `uidmap`, crun, fuse-overlayfs,
  slirp4netns, `/etc/subuid`+`/etc/subgid`) and a `podman.socket` the runner
  points `DOCKER_HOST` at. GitHub's `ubuntu-latest` runs dockerd, so a CI copy
  would test nothing it doesn't already test. Run it by hand when re-validating
  the Podman claim in
  [threatmodel/THREAT-MODEL.md](../threatmodel/THREAT-MODEL.md).
- **`scripts/stage-agent-binary.sh`** — stages a checksum-verified agent CLI
  binary into `deploy/images/<agent>/` for corp networks where public npm is
  blocked. The output is gitignored and the default image build installs from
  npm, so there is nothing for CI to run; it is invoked by an operator before
  `make agent-images-core` (see
  [deploy/images/README.md](../deploy/images/README.md)).
- **`scripts/run-e2e-byoi.sh`** (`make test-e2e-byoi`) — live BYOI wrap +
  selftest proof. It requires a wardynd ALREADY running with `-envbuild` and a
  staged `WARDYN_ENVBUILD_TOOLS_DIR`, and — unlike the SSH and UI-sandbox
  scripts below — starts no control plane of its own. That is setup this
  repo does not put on every PR, so it runs in `nightly.yml`'s `byoi-e2e-live`
  job (which boots the compose stack first, the way `ci.yml`'s
  `desktop-envelope` does) rather than in `ci.yml`, and remains runnable by
  hand. See RELEASING.md when re-validating BYOI.
- **`scripts/run-e2e-subscription.sh`** (`make test-e2e-subscription`) — live
  subscription proxy-injection proof. It needs a real operator
  `claude setup-token`; no repository secret carries one. Run by hand before a
  release that touches the credential path.
- **`scripts/run-e2e-ui-sandbox.sh`** (`make test-e2e-ui-sandbox`) — live
  UI-sandbox relay proof: the ticket → enter → cookie → code-server handoff on
  the second origin, the `ui.*` audit rows (and the `session.attach` row that
  must NOT appear), the header strips both ways, and the pooled-exec baseline.
  It brings up its own uniquely-named compose stack on its own ports and tears
  it down on every exit path, and it builds `wardyn/agent-vscode:local`
  (`make agent-image-vscode`, +~228 MiB) if that image is not already local —
  too heavy for every PR, so it runs in `nightly.yml`'s `ui-sandbox-e2e-live`
  job rather than `ci.yml`, and remains runnable by hand. Needs Docker;
  self-skips unless `WARDYN_TEST_DOCKER=1`.
- **`scripts/run-e2e-ssh-k8s.sh`** (`make test-e2e-ssh-k8s`) — the SSH
  gateway proven against a **Pod** rather than a container: the run's sandbox
  confirmed through `kubectl`, `ssh <run-id>@host <cmd>` over the k8s exec
  lane, a nonzero exit code surviving that lane's out-of-band status channel,
  the interactive shell reaching tmux, the `ssh.exec` /
  `session.attach{transport:ssh}` rows for both, and both authorization arms —
  a second principal's `member` key refused on a run it does not own (audited
  `ssh.auth` failure) and a third principal's `admin` key reaching that same
  run with `data.override=true`. Those two principals go in through
  `kubectl exec deploy/postgres`, because the API only ever stamps the
  *caller's* key and this install has one credential. It does **not** create or
  delete a cluster — it runs against the one `make kind-quickstart` leaves
  behind, and cleans up only its own run and the keys it registered. Needs
  `kubectl` and a real `ssh` client; self-skips unless `WARDYN_TEST_K8S=1`.
- **`deploy/kind/quickstart.sh`** (`make kind-quickstart`, `make kind-down`) —
  not a test at all: it builds `wardynd`, creates a `kind` cluster with a
  pinned Calico CNI and the k8s runner substrate on, `helm install`s the
  chart, waits for a healthy control plane, and prints the URL and admin token
  it minted. CI proves the same install path through its own
  `helm-install-test` and `conformance-k8s` jobs rather than by running this
  target; an operator runs it to get the cluster the lane above needs, and to
  rehearse the day-2 commands in
  [OPERATIONS.md](OPERATIONS.md#kubernetes-day-2). It binds `127.0.0.1:8080`,
  so a leftover compose stack on that port fails it loudly at cluster-create.

## This repository's own CI

Everything above is about running Wardyn in *your* pipeline. This section is the
budget for Wardyn's own [`ci.yml`](../.github/workflows/ci.yml), which runs on
every pull request and every push to `main`. What it runs short of is runner
slots, not the length of any one run: with many pull requests open, a run waits
in the queue far longer than it executes. So the budget is counted in checks and
runner-minutes as well as minutes.

`pull_request:` carries no `branches:` filter — that field matches the PR's
*base*, and the 0.8 working practice stacks lanes on `<kind>/<issue#>-<slug>`
branches (#90), not on `main`, so a filtered trigger gave a stacked PR no
checks at all. `push:` stays narrow to `main`, `master`, `release/**` and
`feature/**`, since every commit already gets a run from its own PR.

**Before and after #211**, measured from the GitHub Actions API: job times over
the 60 most recent completed `ci.yml` runs as of 2026-09-21 05:00Z (a "green run" is one
of the 26 whose `build` passed); superseded runs over all 215 `ci.yml` runs
created from 2026-09-20 01:34Z to 2026-09-21 05:03Z.

| | Before | After |
|---|---|---|
| Checks per pull-request run | 28 | 24 |
| Required contexts (branch protection) | 14 | 14, same names |
| Runner-minutes per green run, median | 84.2 | 74.6 (see below) |
| Longest job per green run, median | 18.4 min (`build`) | 18.4 min (`build`) |
| Green run, created to completed | median 50 min, range 19 to 86 | re-measure once runs on this workflow accumulate |
| Pull-request runs still queued or running when a newer push to the same pull request arrived | 24 of 176 | cancelled by the workflow's `concurrency` group |

The after runner-minutes are the same 26 runs without the three
`multi-arch build` cells (now `nightly.yml`'s `buildx-smoke`) and
`screenshots-fresh` (now an advisory annotation from `diagrams`); every job that
remains is unchanged, so its measured time carries over. No run is shorter than
its longest job, so 19 minutes is the floor while `build` is the critical path.

**Where `build`'s time goes**, in one green run (35555137810): `make test-race`
537 s, `make cover-check` 395 s, `make lint` 120 s.

**Why `ui-e2e` is one job.** In the same run its Playwright step took 492 s:
Playwright itself 418 s, the backend and UI build plus the first seed 32 s, and
the 30 per-spec reseeds 40 s (1.3 s each). About 60 s of setup precedes that
step. Three shards would add two checks and repeat that setup and build, roughly
90 s, in each, while `build` stays the critical path; seeding once per shard
would save the 40 s and give up the per-spec isolation `scripts/run-ui-e2e.sh`
exists to provide.

**Per-job budget.** A job's budget is its `timeout-minutes`, at least twice its
measured maximum with a ten-minute floor. Minutes, successful runs only:

| Check | Runs | Median | Max | Timeout |
|---|---|---|---|---|
| `build` | 26 | 18.4 | 18.9 | 40 |
| `conformance-k8s` | 58 | 11.2 | 12.8 | 35 |
| `ui-e2e` | 42 | 9.2 | 10.1 | 25 |
| `test-pg` | 31 | 5.0 | 5.2 | 15 |
| `ui` | 60 | 4.5 | 4.8 | 20 |
| `conformance` | 60 | 4.2 | 4.7 | 45 |
| `envbuild-integration` | 60 | 3.6 | 4.0 | 20 |
| `gates (staticcheck)` | 60 | 2.7 | 2.9 | 15 |
| `helm-install-test` | 60 | 2.6 | 3.2 | 15 |
| `desktop-envelope` | 60 | 2.2 | 2.5 | 15 |
| `trivy (wardynd)` | 60 | 1.8 | 2.0 | 40 |
| `notices` | 60 | 1.5 | 1.9 | 15 |
| `gates (licenses)` | 60 | 1.4 | 2.0 | 15 |
| `trivy (agent-codex-cli)` | 60 | 1.2 | 1.6 | 40 |
| `trivy (agent-base)` | 60 | 1.1 | 1.4 | 40 |
| `trivy (agent-aws-sso)` | 60 | 1.1 | 1.6 | 40 |
| `gates (gitleaks)` | 60 | 0.8 | 1.0 | 15 |
| `trivy (wardyn-proxy)` | 60 | 0.7 | 1.1 | 40 |
| `gates (govulncheck)` | 60 | 0.7 | 1.0 | 15 |
| `diagrams` | 60 | 0.6 | 0.9 | 10 |
| `compose` | 60 | 0.3 | 0.5 | 10 |
| `gates (license-headers)` | 60 | 0.3 | 0.4 | 15 |
| `helm` | 60 | 0.1 | 0.7 | 10 |
| `dco` | 60 | 0.1 | 0.1 | 10 |
| `multi-arch build (agent-claude-code)`, nightly | 60 | 3.5 | 3.8 | 45 |
| `multi-arch build (wardynd)`, nightly | 60 | 3.1 | 3.5 | 45 |
| `multi-arch build (agent-aws-sso)`, nightly | 60 | 2.9 | 4.0 | 45 |

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

`make ci`, the local merge gate, prints the same kind of table for its own
targets when it finishes or stops.

## Driving an existing control plane instead

If you already run wardynd somewhere, skip `ci-run.sh` and use the CLI
directly — it is fully non-interactive with `WARDYN_URL` +
`WARDYN_ADMIN_TOKEN`:

`--task-mode exec` runs the task as a plain shell command, so **no `--agent` is
needed** — the image is what the run needs, and naming an agent it never invokes
was a formality this documentation used to demonstrate.

```sh
wardyn run --image ubuntu:24.04 --task-mode exec \
  --task 'make test' --policy-file ci.json --dry-run   # resolve + check, launch nothing
wardyn run --image ubuntu:24.04 --task-mode exec \
  --task 'make test' --policy-file ci.json --wait --timeout 30m
wardyn run get <id> --json     # final state, resolved image
wardyn run grants <id>         # what the run was ELIGIBLE for
wardyn audit <id> --json
```

`--dry-run` posts the same body to `POST /api/v1/runs/preflight`, a dry-run of
launch resolution that mints nothing and prints the `setup_items` blockers plus
the confinement class that would be enforced. `ci-run.sh` calls the same
endpoint before launching.

## Images

`wardynd` publishes to `ghcr.io/cjohnstoniv/wardynd` on every push to `main`
([.github/workflows/publish-image.yml](../.github/workflows/publish-image.yml));
every release tag publishes all five images (`wardynd`, `wardyn-proxy`,
`agent-base`, `agent-codex-cli`, `agent-aws-sso`) cosign-signed, each with an
attested SBOM and build provenance — see [VERIFY.md](VERIFY.md) to check them —
([.github/workflows/release.yml](../.github/workflows/release.yml) — see
[RELEASING.md](../RELEASING.md) and the Helm chart's
[README](../deploy/helm/wardyn/README.md)). This BYOA pipeline (`ci-run.sh`)
does not consume them, though: it still builds wardynd, the `wardyn-proxy`
sidecar, and the agent image from source on every invocation (a few minutes
per job). `scripts/up.sh` now pulls the published images instead, falling back to
a build when any is missing; `ci-run.sh` has not yet been switched to the same
path, so a pipeline still pays the build. Wiring it up is open work.
