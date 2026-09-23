---
name: wardyn-ci
description: Generate a Wardyn sandbox policy + CLI invocation (or full pipeline job) for running a governed sandboxed task in CI — GitHub Actions, Azure DevOps, or any headless/unattended launch. Use when the user wants to run Wardyn in CI/a pipeline, generate a wardyn policy or run config, launch a sandbox non-interactively, or asks about wardyn run --wait / --image / task_mode / ci-run.sh. Triggers on "wardyn in CI", "CI sandbox policy", "pipeline sandbox", "headless wardyn run", "unattended governed run", "generate a wardyn policy".
---

# Wardyn CI — generate the config and invocation

Goal: produce (a) a `RunPolicySpec` JSON, (b) the launch invocation — either a
`wardyn run` command against an existing control plane or a `scripts/ci-run.sh`
env block for a from-nothing pipeline job — and (c) if asked, the pipeline YAML.
Reuse the shipped machinery; never hand-roll what exists.

## Ground truth (read these, don't restate from memory)

- Policy schema + field semantics: `RunPolicySpec` is in `internal/types/policy.go`,
  `GrantSpec` is in `internal/types/types.go` — the validator is `validatePolicySpec`
  in `internal/api/policy.go`.
- Baseline policies: `examples/policies/` (`ci.json` = the unattended baseline;
  `claude-llm.json` = model-access grants; that dir's README explains each).
- The one-shot wrapper + env table + exit codes: `scripts/ci-run.sh` and `docs/CI.md`.
- Pipeline examples to copy from: `docs/ci/github-actions.yml`,
  `docs/ci/azure-pipelines.yml`.

## Recipe

1. **Classify the task.**
   - Plain command in a user image (tests, builds, tools) → BYOA exec mode:
     `--image <ref> --task-mode exec --task '<command>'`. No LLM credentials.
   - Agent task (e.g. claude-code fixing/refactoring) → harness mode: needs a
     model-access grant (see step 3).

2. **Author the policy from `examples/policies/ci.json`**, changing as little
   as possible. Non-negotiable for unattended runs:
   - `"first_use_approval": "always_deny"` (never `wait_for_review` — it holds
     connections for a human who isn't there).
   - `allowed_domains`: exactly the hosts the task needs — package registries,
     the SCM host if `--repo` is used, the model provider for harness mode.
     Empty = sealed (right for exec-mode tasks that need no network).
   - No `eligible_grants` entry with `"requires_approval": true` (it will never
     mint). Grants must be `requires_approval: false` with their secret seeded
     up front (`WARDYN_CI_SECRETS` / `wardyn secret set`).
   - Keep `auto_stop_after_sec` bounded (baseline: 3600).

3. **Model access (harness mode only).** If `site_config.model_providers` is
   configured, model access is a **person's own credential**, not a
   pipeline-seeded secret: the CI principal stores it once through
   `PUT /model-providers/{id}/credential` (or the console), and the pipeline
   sets `WARDYN_CI_MODEL_PROVIDER=<id>` so `ci-run.sh` checks it is connected
   before launching (`docs/CI.md` § "CI's identity"). Nothing in the policy or
   the env block carries the key. Otherwise (a from-nothing stack with no
   providers configured — `ci-run.sh`'s own default), the older
   zero-prior-state path still works: copy the `api_key` grant +
   `api.anthropic.com` egress from `examples/policies/claude-llm.json`, and
   the pipeline seeds `anthropic-api-key` from its secret store. Bedrock
   bearer is the enterprise alternative (`docs/CI.md` § model access).
   Subscription modes are refused in CI either way — do not propose them.

4. **Validate before shipping the config**: with a control plane up, POST the
   exact create-run body to `/api/v1/runs/preflight` (dry-run, mints nothing)
   and act on `setup_items`; `ci-run.sh` does this automatically. At minimum,
   `wardyn policy create -f <file> --name tmp` against a dev stack exercises
   the real validator.

5. **Least-privilege for complex tasks — derive, don't guess**: run once
   permissively in a trusted env, then `wardyn record synthesize <run-id>`
   (preview) / `wardyn record save <run-id> --name <n>` (persist) and export
   that spec as the CI policy file, flipping `first_use_approval` to
   `always_deny`.

6. **Emit the invocation.**
   - Fresh-stack pipeline job: a `ci-run.sh` env block (copy the shape from
     `docs/ci/github-actions.yml` / `azure-pipelines.yml`).
   - Existing control plane: `wardyn run --agent <a> [--image <ref>]
     [--task-mode exec] [--repo org/name] --task '<t>' --policy-file <f>
     --wait --timeout 30m` with `WARDYN_URL` + `WARDYN_TOKEN` set — the CI
     principal's own `wdn_` token (`docs/CI.md` § "CI's identity"), not the
     shared `WARDYN_ADMIN_TOKEN`.
   - State the exit-code contract (0 / task's code / 2 / 124 — table in
     `docs/CI.md`) so the pipeline gate is explicit.

## Sanity checks before handing over

- Policy JSON round-trips: `jq . <file>` and no fields outside `RunPolicySpec`.
- Every host the task touches is in `allowed_domains` (clone host included).
- No approval-gated grant, no `wait_for_review`, bounded `auto_stop_after_sec`.
- Harness mode: the model-provider host AND its `api_key` grant are both
  present (one without the other fails at run time, not create time).
