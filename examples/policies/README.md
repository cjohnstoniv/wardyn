# Example policies

Every field these files can set is listed in
[docs/POLICIES.md](../../docs/POLICIES.md). This page is the notes on the shipped
examples — `RunPolicySpec` JSON has no comment field (`LoadPolicySpec` uses
`DisallowUnknownFields`), so they live here instead.

**JSON or YAML.** `wardyn run --policy-file` and `wardyn policy create/update -f`
accept either — YAML is decoded to the same schema, so it also lets you keep
inline comments the JSON files can't have (see `sandbox.yaml` below).
`wardyn policy render -f <file>` converts either to canonical JSON and fails on a
misspelled field, so you can sanity-check a policy before you launch.

## Console template chips

The policy panel's template chips (`/runs/new` and `/policies`) are seeded
from three of these files as compiled-in consts in
`ui/src/app/components/wardyn/policy-panel.tsx` — **not** read from disk at
runtime: `default.json` → **Package registries**, `ci.json` → **CI baseline**,
`ci-claude-llm.json` → **Model provider only**. Editing one of these files does
not change what the console offers; the chip's JSON has to be updated in the
panel component too. `claude-subscription.template.json` is deliberately
excluded — its `__comment` key and machine-specific `__WARDYN_CRED_DIR__`
mounts don't validate as shipped. The **Minimal** and **Allow-all — observe
first** chips are authored directly in the panel and have no example-file
source.

## sandbox.yaml / sandbox-claude.yaml / sandbox-workspace.yaml

The keyless quick-start trio: sealed floor -> real Claude -> real code.
`sandbox.yaml` is the sealed floor (empty allowlist, `always_deny`, `CC1`) you
pass straight to `wardyn run --policy-file` to prove the egress boundary with a
plain `--task-mode exec` command — no keys, no repo.
`sandbox-claude.yaml` is the "give it a real Claude" step: Anthropic egress and a
read-only git-broker grant, and deliberately **no** `api_key` grant so dispatch
falls through to the managed Claude subscription (injected proxy-side; the sandbox
holds only an inert sentinel). `sandbox-workspace.yaml` is the "give it real
code" step: the same keyless subscription path plus one operator-authored
`workspace_mounts` entry, so the agent edits REAL files in the one host
directory you name — edit `source`, then onboard it once with `wardyn workspace
create --kind local_dir --source <path>` (a run may only mount an ONBOARDED
source; the gate is un-bypassable). All three are commented.

## default.json

The out-of-the-box policy (`WARDYN_DEFAULT_POLICY`, also the ceiling every run
is clamped to — `composer.Clamp` — when no override is set). `allowed_domains` carries the standard
registries for common dev tooling — npm/yarn, PyPI, GitHub (clone/API/release
assets), Go modules, and crates.io — so a first manual run works without an
egress-approval round trip for ordinary `npm install`/`pip install`/`go get`/
`cargo build` traffic. This widens the ALLOWLIST only: `first_use_approval`
stays `"deny_with_review"` and the policy is still default-deny
(`allow_all_egress` unset), so any domain outside this list still hits the
approval flow, never a silent allow.

## claude-llm.json / claude-llm-inspected.json

Claude coding policies: Anthropic + GitHub + common registry egress, an
`api_key` grant (no approval) plus an approval-gated `github_token` grant,
`first_use_approval: "deny_with_review"`. The `-inspected` variant is the
LLM-egress-inspection example: it adds `llm_inspection` (`mode: "alert"`,
`detect_secrets: true`, `on_scanner_error: "pass"`) so prompt traffic to the
model provider is scanned and alerts are logged without blocking.

## ci-claude-llm.json

`ci.json`'s CI baseline plus exactly what model access needs: `api.anthropic.com`
in `allowed_domains` and one no-approval `api_key` grant
(`anthropic-api-key`). Every other field — `first_use_approval: "always_deny"`,
`auto_stop_after_sec: 3600`, `CC1` floor — is `ci.json` verbatim, so unlike
`claude-llm.json` (a dev ceiling: `deny_with_review`, an approval-gated
`github_token` grant, unbounded `auto_stop_after_sec: 0`) this one is safe to
point a pipeline's `--policy-file` at as-is. See docs/CI.md's "Model access
for harness mode".

## demo.json

The zero-dependency floor policy (`min_confinement_class: "CC1"` — runs
anywhere Docker runs, no gVisor needed): `proxy.golang.org` egress only,
`first_use_approval: "deny_with_review"`, one approval-gated read-only
`github_token` grant. `github.com` is deliberately **not** allowlisted — that
grant routes git through the repo-scoped git-broker instead, so the sandbox
never dials GitHub directly. The compose launcher auto-picks it when gVisor/`runsc`
is absent (see the `deploy/compose/.env.example` header for the pick order).

## ci.json

The unattended/CI baseline (see docs/CI.md): `first_use_approval:
"always_deny"` (hard deny, no pending approvals — nothing ever waits on a
human), empty egress allowlist (add exactly what the task needs), no grants,
`CC1` floor so it runs on plain runc CI runners, and a 1-hour
`auto_stop_after_sec` bound. `scripts/ci-run.sh` uses it as the default
`--policy-file`.

## claude-llm.json

The developer ceiling for a run that needs a model: `default.json`-style
registry egress plus `api.anthropic.com`/`api.openai.com`, a no-approval
`api_key` grant (`anthropic-api-key`) so the run can actually reach a model, and
an approval-gated `github_token` grant (contents + pull-requests write). Both
`scripts/up.sh`'s `pick_policy` and host mode pick it when a real model path is
configured. (It replaced `composer-dev.json`, which was named for the AI Run
Composer and went with it in 0.5 — same ceiling, honest name.)

## claude-subscription.template.json

A TEMPLATE, not a usable policy — do not point `WARDYN_DEFAULT_POLICY` at it.
`scripts/stage-claude-creds.sh` replaces `__WARDYN_CRED_DIR__` with a
machine-specific read-only staging dir and writes the real policy to
`~/.wardyn/claude-subscription.json`.
