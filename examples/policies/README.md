# Example policies

`RunPolicySpec` JSON has no comment field (`LoadPolicySpec` uses
`DisallowUnknownFields`), so notes on the shipped policies live here instead.

## default.json

The out-of-the-box policy (`WARDYN_DEFAULT_POLICY`, also the composer's clamp
ceiling when no override is set). `allowed_domains` carries the standard
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

## demo.json

The zero-dependency floor policy (`min_confinement_class: "CC1"` — runs
anywhere Docker runs, no gVisor needed): GitHub + Go-proxy egress only,
`first_use_approval: "deny_with_review"`, one approval-gated read-only
`github_token` grant. The compose launcher auto-picks it when gVisor/`runsc`
is absent (see the `deploy/compose/.env.example` header for the pick order).

## composer-dev.json

The developer ceiling for composed runs: `default.json`-style registry egress
plus `api.anthropic.com`/`api.openai.com`, a no-approval `api_key` grant
(`anthropic-api-key`) so a composed run can actually reach a model, and an
approval-gated `github_token` grant (contents + pull-requests write). Host
mode picks it when a real model path is configured.

## composer-dev-subscription.template.json

A TEMPLATE, not a usable policy — do not point `WARDYN_DEFAULT_POLICY` at it.
`scripts/stage-claude-creds.sh` replaces `__WARDYN_CRED_DIR__` with a
machine-specific read-only staging dir and writes the real policy to
`~/.wardyn/composer-dev-subscription.json`.
