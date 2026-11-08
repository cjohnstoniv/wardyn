> Part of the [Operations](../OPERATIONS.md) split (task pages under `docs/operations/`).

# Who decides a hold

- By default a hold is decided by a security admin, or by the run's owner for the three kinds a member decides today.
- A unit's **deciders** add people to that list, per kind of hold, for the runs launched in that unit.
- A unit with no rules changes nothing: the verdicts, the `404`s and the `approval.decide` audit rows are the ones a deployment without deciders gives.
- Nothing refuses more on upgrade. Rules reach holds already pending, because they are read when a hold is decided.

## Today's rule

| Kind of hold | Who decides without a rule |
|---|---|
| `egress_domain` | the run's owner, with the `egress_host` grant on the host; a security admin |
| Azure DevOps escalation, one MCP call | the run's owner; a security admin |
| `credential`, `push_content` | a security admin only |
| `credential_reauth`, `human_takeover` | nobody, by approve or deny; no rule governs them |

- `WARDYN_EGRESS_SECOND_HUMAN` and `WARDYN_CAPABILITY_SECOND_HUMAN` still apply on top of any rule ([second human](../ENV.md#wardyn_egress_second_human)).
- A portal's delegated token never decides ([`delegation_scope`](api-tokens.md#delegated-run-management-portals)).

## The rules

`PUT /api/v1/governance/units/{id}/deciders` replaces the unit's rules. An empty list clears them.

```json
{"rules": [
  {"kinds": ["egress_domain"], "who": ["owner", "group:eng-leads"], "max_scope": "run"},
  {"kinds": ["ado_escalation", "mcp_call"], "who": ["unit_admins", "group:eng-peers"], "second_human": true},
  {"kinds": ["credential"], "who": ["unit_admins"], "second_human": true, "max_scope": "once"}
]}
```

| Field | Meaning |
|---|---|
| `kinds` | at least one of the kinds below. A kind is named once per unit, so order carries no meaning |
| `who` | up to 8 of `owner`, `unit_admins` and `group:<name>`. Empty means the security tier alone |
| `second_human` | the run's own people never decide it, security admins included |
| `max_scope` | the widest scope one decision may have: `once`, `until` or `run`. `always` is never allowed |
| `max_elevation` | `elevation` rules only: `15m` to `24h`, stored in its shortest form |
| `eligible_requesters` | `elevation` rules only, and required there: one to eight `group:<name>` entries, who may ask ([elevations](#elevations)) |
| `renew_after` | `elevation` rules only: how long after an elevation ends before the same person may ask again in the unit, `0s` to `168h`. Absent means no wait |
| `ticket_pattern` | `elevation` rules only: a regular expression of at most 256 characters a request's ticket must match whole |

- Security is always a decider and is never listed. `security`, `manager` and `delegate` are refused.
- A bad rule answers `400` `invalid_deciders`, naming it (`rules[1]: ...`).

### Kinds

| Kind | Governs | Refused in `who` | `second_human` |
|---|---|---|---|
| `egress_domain` | an egress host hold | none | optional |
| `ado_escalation` | an Azure DevOps capability escalation | none | optional |
| `mcp_call` | one MCP call a person approves with its arguments | none | optional |
| `credential` | a credential hold | `owner` | required |
| `push_content` | a held push | `owner` | required |
| `kube_call` | nothing in this build | none | optional |
| `elevation` | a person's request to be a member of the unit for a while ([elevations](#elevations)) | `owner` | refused |
| `image` | a person's request for one image by digest ([image requests](#image-requests)) | `owner` | refused |
| `file_download` | nothing in this build | `owner` | stored as true |

- `elevation`, `image` and `file_download` are each their own rule and take no `max_scope`. An `elevation` rule also needs `max_elevation` and `eligible_requesters`.
- The last row's kind is stored and shown, but no hold in this build maps to it, so a rule for it decides nothing.
- A plain tool call is no kind here and is decided as before.
- The rule for one kind never binds another: an Azure DevOps rule leaves MCP calls alone.

### Who

| Entry | Matches |
|---|---|
| `owner` | the person who created the run. For `egress_domain` they still need the `egress_host` grant on the host |
| `unit_admins` | a person with a unit-admin grant on the run's unit or one above it |
| `group:<name>` | a person whose session holds the group or App Role, from a complete snapshot |

- A device, a service identity and a delegated portal token match none of these. The admin token, local mode and a stale or truncated group snapshot match neither `unit_admins` nor a group.
- `unit_admins` and `group:<name>` never match the run's own people: its creator, the sponsor of a service identity that created it, or its recorded author. The creator decides only through `owner`, so the `egress_host` grant still stands between them and a host. The sponsor of a service identity and the recorded author decide none of the run's holds.
- A service identity's run with no recorded author is decided by security alone.
- An approval rule (`policy:`) never matches `who`. It decides only as the security tier's automation, and `second_human` refuses it.

## How a hold is resolved

1. The run's frozen unit is read, then its ancestors. Every rule for the kind on that chain binds: deciders are the entries all of them name, `second_human` holds if any sets it, and the narrowest `max_scope` and `max_elevation` win. `rule` in the audit row names the nearest unit with a rule.
2. A security admin decides. The rule's `second_human` and `max_scope` still bind them; the admin token is the audited break-glass for `second_human` (`approval.second_human.bypass`, `switch` `unit deciders (<unit id>)`).
3. Anyone else decides through the first `who` entry that matches. Otherwise they get the `404` they get without rules, so no one learns of a hold they cannot decide.
4. The run's owner, when `who` leaves them out of an egress, Azure DevOps or MCP hold, gets `403` `not_a_decider`, which names who decides.

- Nothing is stored on the hold. Rules, grants and groups are read again when the decision arrives, so a revoked grant, a group left or an edited rule applies at once.
- A run with no unit, or a deleted unit, is decided as without rules.
- Rules or the unit tree that cannot be read answer the run's owner `503` `deciders_unreadable`, and anyone else the usual `404`. A security admin still decides, with no rule applied: see [the residual](../../threatmodel/THREAT-MODEL.md#unit-deciders-who-may-decide-a-hold-and-five-residuals).
- The widest scope under any rule is `run`. A decision sent with no scope keeps the kind's default, except an egress decision under a cap below `run`, which is stored as `once`. A wider scope answers `403` `decision_scope_above_rule`.

| Reason | Status | When |
|---|---|---|
| `not_a_decider` | `403` | the owner is left out of an egress, Azure DevOps or MCP hold |
| `decision_scope_above_rule` | `403` | the scope is wider than `max_scope`, or is `always` |
| `deciders_unreadable` | `503` | the rules or the unit tree could not be read |
| `second_human_required` | `403` | a rule's `second_human` refuses the decider |
| `unit_knob_locked` | `409` | a unit above locks `deciders` |

## Who may write rules

Set a unit's rules with `PUT /api/v1/governance/units/{id}/deciders` or `wardyn governance deciders set <unit> --file <f>`, where the file is `{"rules": [...]}`.

| Writer | What happens |
|---|---|
| Security admin | applies directly, or is held when `WARDYN_GOVERNANCE_SECOND_HUMAN` is on |
| Unit admin | only for a unit strictly below one they govern; always held, and decided by a security admin or a unit admin strictly above |

- Deciders are decision power, so nobody sets them on their own unit, as with a unit-admin grant ([four-eyes](unit-admins.md#four-eyes)).
- The `deciders` knob is lockable. A write under a lock, or a lock over units that state rules, answers `409` `unit_knob_locked` ([locks](unit-admins.md#locks)).
- A held write answers stale when the stored rules changed after it was proposed.
- `GET /governance` lists each unit's `deciders` with `from`: the unit whose rule governs each kind there. A unit admin sees the units of their subtree.
- The audit row is `governance.deciders.set` ([audit actions](../AUDIT-ACTIONS.md)).

### A unit's rule only narrows the rules above it

- A rule may not reach further than the rules for the same kind on the units above. One that names a decider they do not, has a wider `max_scope` or `max_elevation`, or leaves off a `second_human` one sets answers `422` `decider_rule_wider_than_parent`. The message names the rule, the field and the nearest unit above with a rule.
- For `elevation`, who may ask is the groups every rule on the chain names, `renew_after` is the longest on the chain, and a ticket must match every `ticket_pattern` on the chain. A rule naming an `eligible_requesters` group the rules above do not, or a shorter `renew_after`, answers the same `422`.
- It is checked at the write, when a held write is proposed, and when it is approved. It binds every writer, the admin token included. Clearing rules is never refused.
- A rule above that is narrowed later refuses nothing below it, and a unit moved under a narrower parent keeps its rules. Both narrow the holds at decide time.
- To widen a parent and a child, write the parent first. Propose the child's widening once the parent's change has applied: until then it is refused.
- `GET /governance` shows `bounded_by`, every unit whose rule for a kind binds the unit, nearest first, and `effective`, the rule they leave. An `effective` `who` of `[]` means only the security tier decides.

## What deciders see

- A pending hold a rule governs carries `deciders`: who may decide it, with `security` last.
- `GET /api/v1/approvals?decidable_by=me` lists the pending holds the caller may decide now, newest first, at most 500 (`X-Wardyn-Truncated: true` marks more). `wardyn approval list --waiting-on-me` prints it. Rows served to someone other than the run's creator carry `requested_by`.
- A decider sees the host, the tool call, the push paths and who asked. They never see the run, its output or a terminal.
- `approval.decide` gains `data.basis` and, under a rule, `data.rule` ([audit actions](../AUDIT-ACTIONS.md)).

## Elevations

An elevation is membership of a unit for a while, asked for by the person and given by the unit's deciders. A unit offers one when an `elevation` rule is on it or above it.

```json
{"rules": [
  {"kinds": ["elevation"], "who": ["unit_admins", "group:support-leads"], "max_elevation": "4h",
   "eligible_requesters": ["group:support"], "renew_after": "24h", "ticket_pattern": "INC-[0-9]{4,}"}
]}
```

- **Who may ask.** A person in a group of the met `eligible_requesters`, matched as a `group:<name>` decider is ([sign-in age](#group-deciders-and-sign-in-age)): a stale or truncated group snapshot matches none, and a bare API token only with `WARDYN_ROLE_STAMP_TTL` set.
- A person a membership already places in the unit is not offered it. The admin token, a service identity, a device and a portal cannot ask.
- **Asking.** `POST /api/v1/governance/requests` with `kind` `elevation`, the `unit_id`, a `duration` from `15m` to the met `max_elevation`, a required `reason` and, when a rule on the chain has a `ticket_pattern`, a matching `ticket`.
- It is held as a governance change, one pending per person and unit, and lapses undecided like any other.
- `GET /api/v1/governance/requests` lists the caller's requests and the ones they may decide (pending, and decided in the last day), and `offered`, the units they may ask in.
- **Deciding.** The met rule's `who` decides at `/governance/requests/{id}/approve` or `/reject`, and a security admin always may. The requester never approves their own request; their reject withdraws it. Rules, grants and groups are read again inside the decision, and an approval whose rule, limit, ticket pattern or wait no longer allows the request answers stale.
- **The membership.** An approval writes a user membership in the unit that ends `duration` after the decision. It is never the person's default unit: a run lands there only when it names the unit (`run_in`). A run launched there has its lifetime cap at the elevation's end ([at the lifetime cap](run-lifetime.md#at-the-lifetime-cap)), and is not revived after it.
- **The end.** The membership counts for nothing once it ends, and a periodic sweep deletes it (`governance.elevation.expire`). `renew_after` is how long after that end the person waits before asking again in the unit; the refusal carries `retry_after`.
- **Ending it early** is a membership delete. Runs already started under it still end at the elevation's end.
- A unit admin's membership write for the same person in the unit makes the membership permanent.
- For standing access that follows a group at the identity provider, a membership for the group (or JIT group assignment at the provider) is the alternative to elevations.

## Image requests

An image request is one image, by digest, for the person who asks, given by the deciders of their default unit. A person is offered it when an `image` rule is on their default unit or above it.

```json
{"rules": [{"kinds": ["image"], "who": ["unit_admins", "group:platform-leads"]}]}
```

- **Who may ask.** A signed-in person whose default unit, the one a run lands in when it names none, has an `image` rule on it or above it. The rule is the meet of every `image` rule from that unit up. A unit the person could pick with `run_in` is never asked, and Default offers nothing.
- A stale or truncated group snapshot is not offered. The admin token, a service identity, a device and a portal cannot ask.
- **Asking.** `POST /api/v1/governance/requests` with `kind` `image`, the `image` as `<name>@sha256:<64 hex>`, a required `reason` and an optional `ticket`. It takes no `unit_id` or `duration`.
- It is held as a governance change, one pending per person and digest, so the same digest under another name is the same request. An image the person already holds exactly answers `409`.
- `GET /api/v1/governance/requests` sets `image_offered` when an `image` rule is on the caller's default unit or above it. A launch refused for an image the person does not hold says "request it in Approvals" only when that holds and the grant would launch. A grant launches when the `image` kind's switch is on, or the image is a restricted value.
- **Deciding.** As for an elevation: the met rule's `who` at the unit the offer was read on, and a security admin always, never the requester. The rule is read again inside the decision; one removed since the request, or an equal grant written meanwhile, answers stale.
- **The grant.** An approval writes one allow grant of `image` for the person, of exactly the ref asked for, created by the decider. It is a user grant, so it holds in any unit the person runs in.
- **What still binds.** Nothing is checked ahead of a launch. The digest rule, a deny of the image and image admission ([capabilities](capabilities.md)) apply at every launch, as for any grant. Deleting the grant ends it.
- With the `image` kind's switch off, nothing is offered: an approved grant would not launch.
- An `image` rule takes none of the elevation fields: `eligible_requesters`, `renew_after` or `ticket_pattern` on one answers `400` `invalid_deciders`.

## Group deciders and sign-in age

- A `group:<name>` decider matches the groups in the person's session. A group left at the identity provider applies when the session next signs in, or when SCIM removes the person.
- A group decider matches an API token only when [`WARDYN_ROLE_STAMP_TTL`](../ENV.md#wardyn_role_stamp_ttl) is set, and then the token's groups are at most that old when it decides. A console session and a CLI session (`wardyn login`) match as before; their groups are as old as their sign-in. `unit_admins` through a group grant reads the same snapshot.

## Before you downgrade

An older build ignores the `governance_unit_deciders` table. Every rule then reverts silently to the default verdicts, which widens a hold whose rule left out `owner`: the run's owner decides it again. Clear the rules you rely on, or stay on this build, before you roll back.
