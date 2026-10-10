# Governance writes, and the second human

The optional switches that hold a write until a second human decides it, and the walkthrough
an operator follows to turn one on and run the queue.

**Optional: require a SECOND human on egress decisions.**

- Set `WARDYN_EGRESS_SECOND_HUMAN=1` and the human who DECIDES an `egress_domain` approval may not be the human who created the run.
- Off by default: turning it on unprompted would deadlock every single-operator deployment.
- Both verbs are covered — a self-*deny* is refused too.
- A refusal is a `403` recorded as `authz.denied` with `reason: second_human_required`, landing **before** the decision is written, so a refused decision leaves the approval `PENDING`.
- Scoped to `egress_domain` only; `credential` and every other `tool_call` approval are admin-only, and the one a run's owner may decide, an Azure DevOps access request, has its own switch, `WARDYN_CAPABILITY_SECOND_HUMAN`.
- A run with an empty `created_by` (system-created follow-on runs) has no human creator to be the same as, so the rule cannot apply.
- **Local mode REFUSES the switch** (`503`) rather than enforcing it:
  - local mode authenticates nobody, so both the decider and the run's `created_by` come from the same client-supplied source — the DEV-ONLY `X-Wardyn-Principal` header, honored there by design —
  - and no request in that mode can prove a second human decided.
- Configure SSO to use this switch, or leave it unset.
- The refusal is scoped to `egress_domain` decisions, so nothing else in local mode changes.

**Optional: the same for Azure DevOps capability escalations.**

- A member may decide an Azure DevOps capability escalation on a run they own, and ownership is the whole member rule, so a run's creator can approve their own escalation, admin-class capabilities included.
- Set `WARDYN_CAPABILITY_SECOND_HUMAN=1` and the human who creates the run can neither approve nor deny its Azure DevOps escalation; a different administrator decides.
- It is a separate switch, off by default, with every rule above:
  - the same `403` / `second_human_required`, the approval left `PENDING`, the `503` in local mode (plus a boot warning), and the `admin-token` break-glass, whose `approval.second_human.bypass` row names this switch in its `switch` field.
- Each switch governs only its own kind.
- A run's attention state follows the setting, as it does for egress: it stops naming the creator as the person who can act.

**Optional: four-eyes on governance writes.**

- Set `WARDYN_GOVERNANCE_SECOND_HUMAN=1` and no single administrator can change a governance profile, an assignment, a capability grant, the enforcement map, a value's availability, a user type's priority, a role mapping or a key-domain assignment alone.
- With it on, a human's write to `POST/PUT/DELETE /governance/profiles`, `POST/DELETE /governance/assignments`, `POST/DELETE /permissions/grants`, `PUT /permissions/enforcement`, `PUT /permissions/availability/{kind}/*`, `PUT /user-types/{id}` (when the priority changes), `POST/DELETE /access/mappings` or `PUT/DELETE /key-domains/assignments/{subject_type}/{subject}` is decoded and validated exactly as before and then stored as a pending change, answered `202` with `Location: /api/v1/governance/changes/{id}` and `{"pending_change": {id, target_kind, op, target_key, state, proposed_by, proposed_at, expires_at, diff}}`.
- Nothing is applied.
- `diff` is rendered by the server, never by a client:
  - the target's current row and the proposed one (every ceiling passes the same read-redaction as a read of it) plus the changed field paths;
  - for an assignment, `diff.after` embeds the profile it points at as it stood at the proposal.
- A second human approves it with `POST /governance/changes/{id}/approve`, or any authorised human rejects it with `POST /governance/changes/{id}/reject` (an optional `reason`, at most 512 characters, no control characters, recorded on the change and its audit row only).
- `GET /governance/changes` lists the queue (`?state=` narrows it; pending by default) and `GET /governance/changes/{id}` reads one.
- A change nobody decides expires after `WARDYN_GOVERNANCE_CHANGE_TTL` (default `72h`).
- Nothing notifies anyone that a change is waiting: approvers find them through `wardyn governance changes list` or the API.

- **Who may approve.**
  - The approver must pass the predicate of the tier the write was proposed on, never a weaker one.
  - Profile, assignment, grant, enforcement, availability and user-type priority changes are proposed on the security tier, so a security admin or a super admin approves; a security admin may approve a super admin's change.
  - A role mapping is written on the super-admin tier, so only a super admin approves one:
    - a security admin who tries is refused `403` `authz.denied` reason `admin_surface` (target `governance.change`),
    - and role-mapping changes are neither listed to a security admin nor readable by one (`GET /governance/changes/{id}` answers `404`).
  - The predicate is evaluated again inside the approval transaction:
    - an API token that was revoked after it authenticated, one cut off by a session revocation, or one whose role no longer passes is refused and the change stays pending.
- **Distinct human.**
  - An approval is refused (`403`, `authz.denied`, reason `second_human_required`, target `governance.change`) when the approver's principal equals the proposer's, or when both emails are non-empty and equal once case-folded.
  - The proposer may reject their own change.
- **What applies.**
  - One transaction locks the change, requires it pending and unexpired, compares the target (and, for an assignment, the profile it points at) and the deployment default with what the proposal reviewed, applies the write,
    - and moves the change to `applied`.
  - A write to the target in between (the break-glass included), a rename or ceiling change of the profile an assignment points at, or a changed deployment default makes the change `stale` (`409` `governance_change_stale`): it never overwrites.
  - The write is re-validated against the current deployment default, so an approval cannot apply what a direct write would refuse.
  - Two approvals racing on one change apply it once; the other is `409` `governance_change_not_pending`.
  - A failure after the write rolls it back.
- **Exemptions.**
  - Only these apply directly, with no second human:
    - a profile update whose new effective profile is no more permissive than the current one (the same resolved comparison composition uses, `Leq`),
    - a rename that changes nothing else,
    - an availability `PUT` that leaves the restricted bit as it is,
    - and a user type's name or description edit that leaves its priority alone.
  - A profile update that changes the contact is held even when it narrows.
  - Everything else is held, deletes and every assignment write included: deleting an assignment widens its subjects back to the deployment ceiling.
  - No grant, enforcement, availability, priority or role-mapping write has a narrowing exemption, because each can widen:
    - deleting a deny grant,
    - lifting a restriction (which admits every person with no grant write),
    - turning enforcement on,
    - raising a priority (which moves a person matching two types onto the other at their next sign-in).
- **Per target.**
  - *Capability grant* (`POST`/`DELETE /permissions/grants`): the target is the grant's natural key (subject type, subject, capability, value);
    - a delete by id is resolved to it at proposal, so a pending delete and a pending upsert of one grant collide.
    - Staleness covers the row at that key.
  - *Enforcement* (`PUT /permissions/enforcement`): the whole-map replacement.
    - A stale `If-Match` is refused `412` at proposal as for a direct write; the approval then compares the map's ETag, and the approval transaction, not the in-process lock a direct write takes, is what serializes it.
  - *Availability* (`PUT /permissions/availability/{kind}/*`): held whenever the stored restricted bit changes, in either direction.
    - Staleness covers the bit and the allow rows naming the value, so a restriction accepted at proposal is applied only while its list is what it was.
  - *User-type priority* (`PUT /user-types/{id}`): held when the priority changes, whole (a name and a priority edit together are one held change).
    - A name or description edit alone applies directly.
  - *Role mapping* (`POST`/`DELETE /access/mappings`): the posture-flip acknowledgement is judged at proposal and carried in the payload, and judged again when the change is applied.
    - The lockout guard runs when the change is applied, against the claim snapshot of the **approver**;
      - the proposer's own facts are not consulted (they may have been demoted since, and the second human is the safeguard against them).
    - The `admin-token` is exempt from the guard as it is on a direct write.
    - The tokens a demotion strands are revoked after the approval commits, as after a direct write.
- **Break-glass and local mode.**
  - The `admin-token` principal applies a covered write directly and approves a change, each writing `governance.change.bypass` beside the target's own row.
  - A deployment that wants this gate to bind holds the token out of band.
  - Local mode authenticates nobody, so the proposer and approver are both client-supplied: with the switch on it answers every covered write and every approve or reject `503` (`governance_second_human_local_mode`).
- **Audit.**
  - `governance.change.propose`, `.approve`, `.reject`, `.expire` and `.bypass` ([AUDIT-ACTIONS.md](../AUDIT-ACTIONS.md)).
  - On approval the target's own row (`governance.profile.write`, `governance.assignment.write`, ...) is written too, its actor the approver, carrying `change_id` and `proposed_by`.
  - The proposer, the approver, their emails and a rejection's `reason` are personal fields:
    - the `audit_personal_fields` erasure scope clears them from the change rows,
    - and a pending change whose proposer is erased expires, so a change with no recorded proposer can never be approved.
- **Residual risks.**
  - The `admin-token` is single-human by design.
  - A database writer can change the tables directly: the audit chain then shows a target row with no `propose`/`approve` pair, which is detection, not prevention.
  - Each change is reviewed alone: two separately approved changes can compose into a widening neither diff shows.
  - Not covered here: the rest of the governance-adjacent writes (workspace egress lists, `/policies`, `/site-config`, `/integrations`, the approval `always` scope, and user-type create and delete stay single-human until their own lanes).
- **In the console.**
  - The Governance screen's Changes tab lists the pending changes with the server's diff, and an approver approves or rejects there (a reason is optional).
  - A covered write made in the console that is held shows "Submitted for approval" at the place it was made, never a save.
  - Approve is disabled on your own proposal, and the server remains the authority.

> [!IMPORTANT]
> **The `admin-token` principal BYPASSES it**, and you should plan around that.

- A bare `WARDYN_ADMIN_TOKEN` caller is attributed `system`/`admin-token` because a shared token carries no per-human identity
  - there is no second human to compare it against,
  - and `X-Wardyn-Principal` is ignored off local mode specifically so a token bearer cannot forge one.
- Refusing the token instead would lock you out of your own approval queue the moment SSO breaks, so the bypass is the deliberate break-glass.
- It is not silent: every one writes an `approval.second_human.bypass` audit event beside the `actor_type=system` `approval.decide`.
- **For this gate to actually bind, treat the admin token as a break-glass credential** — configure SSO, and hold the token out of band.
## Four-eyes on governance writes: a walkthrough

- The rules are in "Optional: four-eyes on governance writes" above; this is the order an operator works in.
- The switch and its TTL are in [ENV.md](../ENV.md); the audit rows are in [AUDIT-ACTIONS.md](../AUDIT-ACTIONS.md); the residuals are in [THREAT-MODEL.md](../../threatmodel/THREAT-MODEL.md) §5 "Four-eyes on governance writes".

**1. Before you turn it on.** All four must hold, or the switch deadlocks you or does not bind:

- **SSO is configured.** Local mode authenticates nobody, so with the switch on every covered write and every approve or reject answers `503` `governance_second_human_local_mode`.
- **The admin token is held out of band.** It applies a covered write directly and approves a change, each audited as `governance.change.bypass`. It is the break-glass and the one way past this gate.
- **Two humans can approve.** Every covered target except role mappings needs a second security admin or super admin; role mappings need a second super admin (see step 6).
- **Every CLI and SDK caller is upgraded.** A client built before 0.8.6 decodes the `202` as an empty object ([sdk.md](../sdk.md) "Old clients"). Nothing applies without approval, but the error it reports is confusing.

Then set `WARDYN_GOVERNANCE_SECOND_HUMAN` (and, if `72h` is wrong for your approvers, `WARDYN_GOVERNANCE_CHANGE_TTL`) in the daemon's environment; on Kubernetes that is the chart's existing `env` map.

- Both are read at boot.
- Turning the switch off later leaves pending changes approvable and rejectable under the same rules, self-approval included; new writes apply directly.

**2. What is held, and who approves it.** A write the table lists is held when the condition in its row is true, and approved at the tier it was proposed on.

| Write | Held when | Approver |
|---|---|---|
| Governance profile create, update, delete | the write is not exempt (below) | security admin or super admin |
| Governance assignment upsert, delete | always, deletes included | security admin or super admin |
| Capability grant upsert, delete | always | security admin or super admin |
| Enforcement map replace | always | security admin or super admin |
| Availability set | the stored restricted bit changes, in either direction | security admin or super admin |
| User-type priority update | the priority changes | security admin or super admin |
| Role mapping upsert, delete | always | **super admin only** |
| Key-domain assignment set, delete | always | security admin or super admin |

Exempt, so applied directly:

- a profile update whose new effective profile is no more permissive than the current one,
- a rename or description edit that changes nothing else,
- an availability `PUT` that leaves the restricted bit as it is,
- and a user-type name or description edit that leaves its priority alone.

Nothing else narrows by shape: a delete can widen (a deny grant, an assignment, a restriction), so no delete is exempt on its own.

**3. Propose, review, decide.**

- A held write answers `202` with the change.
- Its `diff` is the server's: the current row, the proposed row and the changed field paths.

The proposer, or anyone else, then:

```
wardyn governance set ci-governance.json          # held writes print as pending; exit 0
wardyn governance changes list                    # pending by default; --state applied|rejected|expired|stale
wardyn governance changes approve <change-id>     # a different human, at the right tier
wardyn governance changes reject <change-id> --reason "widens egress past the review"
```

- `governance set` exits 0 on a pending result and skips `--prune` until every write is decided, so run it again afterwards.
- Running the SAME document again before then is safe: a target whose held change is that very write is reported as pending, with the held change named, and the rest of the file is applied.
- A document edited since is refused at that target (`409` `governance_change_pending`, naming the held change) until the held change is decided; nothing of the edit is stored.
- The console does the same on the Governance screen's Changes tab, with the diff in a drawer; Approve is disabled on your own proposal.
- Nothing notifies an approver that a change is waiting, so name who looks, and how often, in your own runbook.
- There is at most one pending change per target:
  - a second proposal at the same target is `409` `governance_change_pending`, which names the first and carries it in `pending_change`,
  - with `pending_change_matches` saying whether it is the very write that was refused (same operation, same payload).
- A new profile's target is its name while its create waits, so creating the same name again (by `POST`, or `PUT` at any new id) meets the held create.

**4. Expiry and stale changes.**

- A change nobody decides within the TTL reads as `expired` and cannot be approved (`409` `governance_change_not_pending`); propose it again.
- A change is `stale` (`409` `governance_change_stale`) when, at approval, the target, the profile an assignment points at or the deployment default differs from what the proposer's diff showed.
- Approval never overwrites: a stale change is dead, and the remedy is to propose the write again against the current state.
- A direct write in between (the break-glass included) is the usual cause.

**5. Reading the audit.**

- Filter `GET /audit` by `action_prefix=governance.change.`.
- A normal change is a `governance.change.propose` row (actor the proposer, `Target` the change id) followed by a `governance.change.approve` row and, beside it, the target's own row (`governance.profile.write`, `capability.grant.create`, `access.role_mapping.write`, ...).
- That row's actor is the approver and it carries `change_id` and `proposed_by`: two named humans for one change.
- Also look for:
  - `governance.change.bypass`: the admin token wrote or approved. Alert on it.
  - `governance.change.approve` with outcome `failure`: the approval did not apply (`error` is `stale`, `not_pending` or `error`).
  - `authz.denied` with reason `second_human_required` or `admin_surface` and target `governance.change`: a self-approval, or a security admin at a role-mapping change.
  - A target row with **no** `propose`/`approve` pair beside it, switch on: a write that did not come through the API. See the threat model's "database is not four-eyed" residual.

**6. One super admin.**

- Role mappings are written on the super-admin tier, so only a super admin may approve one, and nobody approves their own.
- A deployment with a single super admin therefore cannot change a role mapping while the switch is on, except through the admin token (audited as a bypass).
- Add a second super admin before you enable the switch, or accept the token as the path for mapping changes.
- The lockout guard is judged against the **approver's** own roles at apply, so the approver cannot be the person whose mapping change would strip their own admin.

**7. What stays single-human.**

- Workspace approved and denied egress lists, record-egress promotion, `/policies`, `/site-config`, `/integrations`, the approval `always` scope, user-type create and delete, and the SCIM purge's deletion of a person's own assignments and grants.
- The threat model lists each with its reason.
