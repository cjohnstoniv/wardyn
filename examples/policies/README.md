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

## remote-workspace.yaml

The external-tool lane ([docs/SSH.md](../../docs/SSH.md) "Other tools over
SSH"): a human drives this sandbox from their own IDE or agent workbench over
the SSH gateway instead of handing Wardyn a task string. `auto_stop_after_sec:
-1` because the reaper's idle clock cannot see a human thinking in their own
editor, or that tool's own agent working between calls that touch the sandbox
at all — both would otherwise read as idle. `first_use_approval:
wait_for_review` holds an unknown-host connection for a live decision instead
of denying it outright, on the premise that someone is already at the
keyboard to decide. `git_push_any_branch: true` turns off the default
branch-namespace confinement on the accompanying write-capable `github_token`
grant, because the external tool names its own branch instead of the
`wardyn/<run-id>/*` one `agent-run` sets up — see
[docs/POLICIES.md](../../docs/POLICIES.md)'s "Bound the token itself" for what
still bounds the token when this is on.

## default.json

The out-of-the-box policy (`WARDYN_DEFAULT_POLICY`, also the ceiling every run
is clamped to — `composer.Clamp` — when no override is set). `allowed_domains` carries the standard
registries for common dev tooling — npm/yarn, PyPI, Go modules, crates.io —
plus the two model APIs (`api.anthropic.com`/`api.openai.com`), so a first
manual run works without an
egress-approval round trip for ordinary `npm install`/`pip install`/`go get`/
`cargo build` traffic. This widens the ALLOWLIST only: `first_use_approval`
stays `"deny_with_review"` and the policy is still default-deny
(`allow_all_egress` unset), so any domain outside this list still hits the
approval flow, never a silent allow. (No GitHub hosts here: GitHub clone/API
for a `github_token` run rides the proxy-side broker route, which moves the
GitHub hosts into `denied_domains` for that run.)

## claude-llm.json / claude-llm-inspected.json

Claude coding policies: `api.anthropic.com` + npm/Go registry egress (GitHub
arrives via the brokered `github_token` grant — contents read, approval-gated
— not the allowlist), plus a no-approval `api_key` grant
(`anthropic-api-key`); `first_use_approval: "deny_with_review"`. Both
`scripts/up.sh`'s `pick_policy` and host mode pick it when a real model path
is configured (it replaced `composer-dev.json` — same ceiling, honest name).
The `-inspected` variant is the LLM-egress-inspection example: it adds
`llm_inspection` (`mode: "alert"`, `detect_secrets: true`,
`on_scanner_error: "pass"`) so prompt traffic to the model provider is
scanned and alerts are logged without blocking.

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

## claude-subscription.template.json

A TEMPLATE, not a usable policy — do not point `WARDYN_DEFAULT_POLICY` at it.
`scripts/stage-claude-creds.sh` replaces `__WARDYN_CRED_DIR__` with a
machine-specific read-only staging dir and writes the real policy to
`~/.wardyn/claude-subscription.json`.

## `ui-sandbox.json` — relaying an in-container editor to the browser

The only shipped example that declares `ui_apps`. Before it, `grep -rl ui_apps
examples/policies/` returned nothing, so the one policy field the UI-sandbox
feature turns on had no worked example anywhere.

`ui_apps` names a loopback port INSIDE the sandbox that the gateway may relay —
`{name, port, path}`, operator-authored, never a command. `name` selects the
launcher the image must ship as `/usr/local/bin/wardyn-ui-<name>`: **declaring
an app does not install one**, and an image without that launcher fails closed
with the path it looked for. `code` matches `deploy/images/vscode/`.

Two things this example deliberately does NOT do:

- **It does not open the extension marketplace.** `allowed_domains` covers
  package registries a build needs, not Microsoft's rotating marketplace CDNs.
  In-editor extension installation is **not supported by default** — see
  [docs/UI-SANDBOXES.md](../../docs/UI-SANDBOXES.md). Bake the extensions you
  need into the image instead; that is auditable, and it survives a CDN change.
- **It grants nothing.** `eligible_grants` is empty. A browser-relayed editor is
  a place to read and edit code, and adding a credential to it widens the
  blast radius of a UI the operator's own browser renders — see the relay's
  residuals in `threatmodel/THREAT-MODEL.md`.

`auto_stop_after_sec` is set because an editor session is the easiest thing in
the product to leave open overnight.

## `autonomous-tool-rules.json` — an autonomous run that is not all-or-nothing

Launch it with `--tool-approvals hold`. Without `tool_rules` that setting means
**every** gated call wakes a human, which nobody sustains — and an operator who
tires of approving sets `auto` instead, which gates nothing at all. This policy
is the middle: reading is free, writing and shell ask, web fetch is refused, and
anything unlisted asks.

The `"*"` rule is last and set to `hold`, deliberately. A default of `allow`
would mean a tool added by a future harness release is permitted before anyone
has looked at it; `hold` means it surfaces as a question instead. Ordering in the
file is cosmetic — an exact match always beats `"*"` — but reading it last
matches how it behaves.

`WebFetch` is `deny` rather than absent to make the point that these are two
different statements: absent means "ask me", `deny` means "the answer is already
no, do not ask". The second is what stops a prompt-injected agent from generating
approval requests until someone clicks yes.

Confinement is CC2 rather than CC1 because this profile is for a run left
genuinely unattended.
