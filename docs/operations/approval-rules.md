> Part of the [Operations](../OPERATIONS.md) split (task pages under `docs/operations/`).

# Approval rules: a security admin's rule approves a service identity's held call

- An approval rule approves a held `tool_call` or a first-use `egress_domain` approval on a service identity's run, in the rule's name.
- It never decides a `credential`, `credential_reauth` or `push_content` approval, an Azure DevOps escalation, or any run a person created.
- A rule goes through the same decide path a person's approval does, so every gate a person meets binds the rule too. It is never a second human.
- The audit contract is [`docs/AUDIT-ACTIONS.md`](../AUDIT-ACTIONS.md)'s `approval.decide`, `approval.policy.would_approve`, `approval.policy.skip`, `governance.approval_rule.write` and `governance.approval_rule.delete` rows.

## Switch

| `WARDYN_APPROVAL_RULES` | Effect |
| --- | --- |
| `off` (default, unset) | The raise reads no rule. The routes below still work, so rules can be staged. |
| `dry_run` | Every rule acts as `mode: dry_run`, whatever its own mode says. |
| `on` | A rule in `mode: enforce` decides; a rule in `mode: dry_run` writes would-approve rows. |

Any other value exits 2 at boot. The switch is env-only, so turning it on needs deploy access. Set `dry_run` first.

## Routes (security tier: admin or `security_admin`)

| Route | What it does |
| --- | --- |
| `GET /governance/approval-rules` | Lists the rules by name, with the switch's value. |
| `PUT /governance/approval-rules/{name}` | Creates or replaces a rule; the store raises `version` by one on every write. A new rule starts `dry_run` unless the body names `enforce`. Held for a second human under `WARDYN_GOVERNANCE_SECOND_HUMAN`, an edit included. |
| `DELETE /governance/approval-rules/{name}` | Removes a rule. Never held: removing a rule only narrows. |
| `POST /governance/approval-rules/dry-run` | Reports what a drafted rule (the body) or a stored one (a body naming only `name`) would do. See below. |

The dry run writes nothing, not even an audit row.

- Each `PENDING` approval the rule matches runs the dry-run decide path: `would_approve`, or `skip` with the gate's reason.
- Approvals from the last 7 days are matched on the rule's predicates only, labelled `predicate_match`. `recent_truncated` says the look back stopped at 1,000 rows.

## Rule

```json
{"mode":"dry_run","kinds":["tool_call","egress_domain"],
 "match":{"repos":["org/repo"],"presets":[{"name":"ci-review","version":4}],"capabilities_within":["contents:read"]},
 "approve":{"tools":["mcp__github__create_pull_request_review"],"hosts":["registry.npmjs.org"],"egress_scope":"run"}}
```

- `match` is AND across fields and OR within one, and names at least one repo or preset. A repo matches the run's repo, case-folded. A preset matches its name and version both, so editing the preset stops the match until the rule is rewritten.
- `capabilities_within` matches only when the run's recorded GitHub or Azure DevOps capability list is non-empty and inside it.
- `approve.tools` and `approve.hosts` are exact: no `*`, no glob, no port. `egress_scope` is `once` or `run`, never `until` or `always`.
- A host the Wardyn boundary does not allow in `hard_limits` is refused at write, `409 approval_rule_host`, and again when a rule decides.
- A rule only approves. A deny already wins before an approval row exists: `tool_rules` deny, `policy:denied` and `always_deny` raise nothing.

## What bounds a rule

- "The ceiling" here means the `tool_rules` clamp for `tool_call`, and the boundary plus the `egress_host` capability for `egress_domain`.
- `egress_domain`: the host must be in `approve.hosts`, and granted to the service identity (its principal and its user type, no groups) under the `egress_host` capability, as a person deciding their own run is held. A deny for the service or its type wins.
- The boundary, the run-ended refusal, the scope rules and four-eyes (`WARDYN_EGRESS_SECOND_HUMAN`) all apply. The private-address guard still refuses an approved host that resolves into private space.
- Only a row the raise created meets a rule. A dedup, a rejoin, a sweep or a rule write never decides an older row.
- The rule is re-read when it decides. If it changed after it matched, it decides nothing (`approval.policy.skip`, `approval_rule_changed`).
- Use `wait_for_review` for hosts a rule covers: under `deny_with_review` the first connection fails and the retry is admitted.

## Audit

- A decision is the ordinary `approval.decide` row, actor `system`, `policy:<rule_id>`, with `data.policy` naming the rule, its version, its digest and what matched.
- A dry run writes `approval.policy.would_approve`; a gate's refusal writes `approval.policy.skip` and, for a refusal a person would also have logged, `authz.denied` with the rule as its actor.
