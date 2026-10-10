# What a member's own run may reach

The scopes an approval decision can carry, the clamp on a member's inline policy, and what
an interactive first run still asks the operator.

## Decision scopes, the clamp, and the first run

**A `credential` decision carries one scope: `run` — the per-run credential lease.**

- Every other scope on a `credential` approval is a `400`, and so is any scope on a `tool_call`.
- `run` exists because a `git_pat` installs a *standing* credential helper git invokes on every operation ([`docs/adoption/corp-network-onboarding-findings.md`](../adoption/corp-network-onboarding-findings.md) B2).
- Approving with `decision_scope=run` (`wardyn approval approve <id> --scope run`) makes that one decision re-mintable for the rest of the run.
- Three things bound it:
  - **`git_pat` only** (`github_token` is brokered proxy-side, `ssh_key` is materialized once and wiped, `api_key` never leaves the broker, so none has the standing-consumer problem, and `broker.leaseCoversRemint` refuses a lease for them even under a `run`-scoped decision);
  - **per scope** (if the grant's scope no longer matches what the human approved, the lease does not carry over);
  - and **killed by revocation** (a leased re-mint still runs the whole mint transaction, so the kill-switch cascade ends it the moment the revocation commits).

`credential.mint` carries `lease: true` plus the raw `decision_scope`, so the audit says which mints were the human's and which the lease's.

- The comparison is deliberately **raw**, never `ApprovalScope.Normalize()`d
  - an empty `decision_scope` normalizes to `run`, and every credential approval decided before this feature carries an empty one,
  - so a normalized comparison would have turned every legacy approval into a standing lease on upgrade.
- Nothing you approved before v0.6 leases anything.

**An `egress_domain` decision's *scope* adds a second, narrower gate — and one of the four scopes is gated on ROLE, not ownership.**

- A member who owns the run may choose `once`, `run`, or `until`; each stays inside that run's own proxy cache.
- `always` is **admin or `security_admin`, regardless of run ownership** (`decide()` rule 6, same file; the check is `isSecurityOperator`, in LOCKSTEP with `authorizeUserDecision`):
  - it writes a durable entry onto the run's workspace (`approved_egress` on approve, `denied_egress` on deny)
  - the SAME two columns the `approved-egress`/`denied-egress` routes write, and those routes sit on that same `securityOps` tier,
  - so the gate keeps the approval queue from being a way around them for anyone below it.
- The refusal is a `403`, not the ownership checks' `404`: the caller has already proven the approval exists, is `egress_domain`, and is theirs.
- It is checked before the run is confirmed to reference a workspace at all
  - authorization before validation, so a member's rejection depends only on role (see [POLICIES.md](../POLICIES.md) "Approval decision scopes" for the workspace-resolution check this precedes).

**`PUT /workspaces/{id}/denied-egress` is the only way to undo a `deny · always` decision.**

- Full-replace, same shape as `approved-egress` (omit a host to un-deny it — there is no per-host delete).
- Deny beats allow everywhere the proxy evaluates policy, so a `deny · always` on a model-provider host, or one a required integration injects into, permanently breaks that workspace's proxy-side injection for every future run.
- `denied-egress`'s validator is deliberately narrower than `approved-egress`'s (plain host shape only, no model-provider reject set) so it keeps working as the escape hatch even when the workspace is already bricked (`handleSetDeniedEgress`, [`internal/api/workspaces.go`](../../internal/api/workspaces.go)).

**A member's own policy is clamped, not trusted.**

- `POST /runs`' `inline_policy` (and its preflight dry-run) is clamped to the operator's `DefaultPolicy` ceiling before resolution (`composer.Clamp`, [`internal/composer/clamp.go`](../../internal/composer/clamp.go) — the same clamp bounds an AI-composed policy and a Record Mode-synthesized one):
  - confinement raised to the floor, egress intersected down to the allowlist,
  - `first_use_approval` raised to the stricter of the two, `llm_inspection` inherited when the ceiling sets one,
  - resources and `auto_stop_after_sec` capped, grants narrowed to what the ceiling allows,
  - and `workspace_mounts` dropped entirely.
- An admin's own `inline_policy` is not clamped.

**A first claude-code run parks on nothing the product itself needs — on an image rebuilt from the 0.7.5 tree.**

- The claude-code image turns off the CLI's own fetches — `CLAUDE_CODE_DISABLE_OFFICIAL_MARKETPLACE_AUTOINSTALL=1` (the plugin-marketplace auto-install, which is what reached `downloads.claude.ai` and `github.com`), `DISABLE_AUTOUPDATER=1` and `CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1`; see [corp-image-authoring.md](../adoption/corp-image-authoring.md) "Stop the agent fetching on its own behalf"
  - and writes `{"hasCompletedOnboarding": true}` into the sandbox's `~/.claude.json` before the CLI starts.
- Without those, the first Claude Code run anybody launched met the CLI's theme picker and then first-use approvals for hosts nobody had asked for.
- The shipped default policy ([`examples/policies/default.json`](../../examples/policies/default.json)) is deliberately unchanged: the fix is that the traffic no longer happens, not that those hosts are now allowed.
- **This applies to the claude-code image only** — the `codex-cli` image still reaches for several hosts of its own at start (see the CHANGELOG's known gaps).
- **And only to an image actually carrying the three `ENV` lines**:
  - `agent-claude-code` (where they were measured) is not a published image — `agent-base` is what ships, and it now carries the three lines too, so any image built `FROM ghcr.io/cjohnstoniv/agent-base:0.7.6` inherits them.
- An image on another base, or an older tag pinned in `WARDYN_AGENT_IMAGES`, still parks on the CLI's own bootstrap; see [corp-image-authoring.md](../adoption/corp-image-authoring.md) for the rebuild recipe and the CHANGELOG's Known gaps for the full statement.

What an interactive run still shows on first use is Claude Code's **workspace-trust** prompt (`Accessing workspace: …` / `1. Yes, I trust this folder`).

- That one is a security question — it gates a cloned repo's own project settings taking effect — and Wardyn does not answer it for you.
- Pressing Enter there raises no approvals.
- A run launched with *"Let it use tools before I attach"* also parks on Claude Code's own *Bypass Permissions mode* confirmation (default "No, exit") until someone attaches and chooses Yes
  - Wardyn does not answer that one either ([THREAT-MODEL.md](../../threatmodel/THREAT-MODEL.md) §4.7).

> [!WARNING]
> **The desktop tier's standing honesty gap: the operator IS the admin.**
> - [The desktop tier](../../deploy/desktop/) (`WARDYN_LOCAL_MODE=true`) has no member role at all — local-mode callers are *always* admins (`Server.requireOperator`'s own doc says so), so the unclamped branch above is the default there.
> - An `inline_policy` the developer submits is bounded by nothing `WARDYN_DEFAULT_POLICY` sets, and setting one is one ordinary API call.
> - What still holds: the unclamped spec lands on the audit feed as `policy.inline.apply` before `run.create` ([AUDIT-ACTIONS.md](../AUDIT-ACTIONS.md)), egress still has no route off the sandbox except `wardyn-proxy`, and the session is still recorded.
> - A governance control, not a containment boundary against the operator holding the laptop.
> - Full accounting: [docs/DESKTOP.md](../DESKTOP.md) "Tamper posture, stated honestly".
