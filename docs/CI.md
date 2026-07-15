# Wardyn CI — governed sandboxes in your pipeline

Run a sandboxed job from a CI/CD pipeline (GitHub Actions, Azure DevOps, or
anything with a docker daemon) with **no pre-running Wardyn, no UI, and no
human**. One script brings up a fresh control plane, launches one governed
run, waits for its outcome, collects artifacts, tears everything down, and
exits with the run's exit code — so your pipeline's pass/fail is the sandboxed
task's pass/fail.

This is a **BYOA (bring your own agent/container)** surface: you supply the
image — an agent harness, your test container, or any stock image — and Wardyn
supplies the governed sandbox around it (default-deny egress at a TLS-MITM
proxy, brokered never-resident credentials, confinement classes, an
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
| `WARDYN_CI_OUT` | artifact dir (`run.json`, `audit.json`, `run.log`) | `./ci-artifacts` |
| `WARDYN_CI_KEEP` | `1` = leave the stack up for debugging | unset |
| `WARDYN_CI_SKIP_BUILD` | `1` = reuse existing local images | unset |

### Exit codes

`ci-run.sh` propagates `wardyn run --wait`'s exit code:

| Outcome | Exit |
|---|---|
| run `COMPLETED` (task/agent exited 0) | `0` |
| run `FAILED` | the task's real exit code (fallback `1`) |
| run `KILLED` / `STOPPED` (lifecycle termination, not a task result) | `2` |
| `--wait` timeout | `124` |

## Writing a CI policy

Start from [`examples/policies/ci.json`](../examples/policies/ci.json) — the
unattended baseline — and add exactly what the task needs:

- **`"first_use_approval": "always_deny"`** — non-negotiable for unattended
  runs. Anything off the allowlist is hard-denied instantly; nothing ever
  waits on a human. (`wait_for_review` holds connections for a reviewer;
  `deny_with_review` files approvals nobody will decide.)
- **Complete `allowed_domains`** — list every host the task legitimately
  needs (package registries, `github.com`, your model provider). The sealed
  default (empty list) means the sandbox can reach nothing.
- **No `requires_approval: true` grants** — an approval-gated credential
  never mints without a human. Use `requires_approval: false` grants with
  secrets seeded via `WARDYN_CI_SECRETS`.
- **Bound the run** — `auto_stop_after_sec` (ci.json: 1 hour) is the reaper
  backstop behind `WARDYN_CI_TIMEOUT`.

Cloning a repo (`WARDYN_CI_REPO`) needs `github.com` (or your SCM host) in
`allowed_domains`; private repos additionally need a `git_pat` (or
`github_token`) grant whose secret you seed via `WARDYN_CI_SECRETS`.

### Model access for harness mode (running a real agent)

Two paths work from zero prior state:

- **API key** (simplest): policy grants an `api_key` scoped to
  `api.anthropic.com` (see
  [`examples/policies/claude-llm.json`](../examples/policies/claude-llm.json))
  and the pipeline seeds it: `WARDYN_CI_SECRETS=anthropic-api-key=$KEY`. The
  key is injected proxy-side; the sandbox only ever holds a placeholder.
- **AWS Bedrock**: set `WARDYN_BEDROCK_REGION`/`WARDYN_BEDROCK_MODEL` on the
  stack and seed a `bedrock-api-key` bearer secret (never-resident,
  proxy-injected).

Claude **subscription** modes need an operator's one-time interactive login
and are not suitable for a fresh CI machine.

### Least-privilege, derived not guessed

Don't hand-author the allowlist for a complex task — record it once:

1. In a trusted environment, run the task under a permissive policy.
2. `wardyn record synthesize <run-id>` previews the least-privilege policy
   Wardyn derived from the run's audit trail (observed domains, execs,
   writes); `wardyn record save <run-id> --name my-ci-task` stores it.
3. Export the stored policy's spec to your repo as the pipeline's
   `WARDYN_CI_POLICY_FILE` (set `first_use_approval` to `always_deny`).

## Driving an existing control plane instead

If you already run wardynd somewhere, skip `ci-run.sh` and use the CLI
directly — it is fully non-interactive with `WARDYN_URL` +
`WARDYN_ADMIN_TOKEN`:

```sh
wardyn run --agent claude-code --image ubuntu:24.04 --task-mode exec \
  --task 'make test' --policy-file ci.json --wait --timeout 30m
wardyn runs get <id> --json   # final state, resolved image
wardyn audit --run <id> --json
```

`POST /api/v1/runs/preflight` (same body as create) is a dry-run of launch
resolution — `ci-run.sh` calls it before launching and prints the
`setup_items` blockers.

## Images

Nothing is published to a registry yet: every job builds wardynd, the
`wardyn-proxy` sidecar, and the agent image from source (a few minutes per
job). Publishing signed images — which would turn the builds into pulls and
enable a reusable one-line GitHub Action — is the v0.5 release-pipeline task.
