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
