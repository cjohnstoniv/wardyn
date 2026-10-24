> Part of the [Operations](../OPERATIONS.md) split (task pages under `docs/operations/`).

# Unit admins: governing one part of the tree

- A unit admin is a person or an IdP group granted on one unit. They govern that unit and every unit below it, and nothing else.
- It is a grant on a unit, not a fourth role: the session role stays `user`, `security_admin` or `admin` ([three roles](three-roles.md)).
- A security admin is never narrowed by a grant. Super and security admins reach every unit, as before.
- The audit contract is [`docs/AUDIT-ACTIONS.md`](../AUDIT-ACTIONS.md)'s `governance.unit_admin.grant` and `governance.unit_admin.revoke` rows.

## Who holds a grant

| Subject | Matches |
|---|---|
| `user` | the person's `sub`, or their email when sign-in vouches for it |
| `group` | an IdP group in the session's snapshot; a stale or truncated snapshot contributes none |

- A device, a service identity, a delegated portal, the admin token and local mode hold no grant.
- A person whose groups cannot be read keeps their member reach and is never refused outright for it.

## What a unit admin may do

| Route | Scope |
|---|---|
| `GET /governance` | the units of their subtrees, the memberships and grants on them, and `parent_walls` |
| `POST /governance/units` | `parent_id` in their units |
| `PUT /governance/units/{id}` | the unit and any new parent in their units |
| `DELETE /governance/units/{id}` | an empty unit strictly below their own; its grants go with it |
| `POST /governance/units/{id}/admins`, `DELETE …/admins/{admin}` | a unit strictly below one of theirs |
| `POST /governance/memberships` | the unit in their units, for a person already holding a membership there |
| `PUT /governance/memberships/{id}` (move) | both units in their units |
| `DELETE /governance/memberships/{id}` | when the person keeps another membership in their units |
| `POST /governance/check`, `POST /governance/dry-run` | a target in their units; the boundary target stays the security tier's |
| `GET /governance/changes` and the decisions | a change touching only their units; nobody decides their own |

- Anything else is refused `403` `unit_scope_denied`: the promotion, the boundary, the restrictions and every other admin route.
- A caller with no grant at all is refused the same reason with "this needs a unit admin or a security admin".

## Four-eyes

A unit admin's widening is always held for a second human, whatever `WARDYN_GOVERNANCE_SECOND_HUMAN` says.

| Act by a unit admin | Applies directly | Held |
|---|---|---|
| a walls edit or re-parent | the subtree's effective walls only narrow, and no lock is removed | otherwise |
| creating or deleting an empty unit; adding a lock | ✓ | — |
| adding a membership | — | always |
| deleting a membership | the person keeps another membership in the unit admin's units | a legacy row that may make another unit someone's context |
| moving a membership | the destination path narrows the source path, and the move activates or vacates no other legacy context | otherwise, for an approver who governs every unit involved |
| granting or revoking a unit admin | — | always |

- A security admin's writes, grants included, follow `WARDYN_GOVERNANCE_SECOND_HUMAN` unchanged. A single-admin install can create unit admins.
- A held change may be decided by a security admin, or by a unit admin who governs every unit it touches. For a grant that means strictly above the granted unit.
- Nobody grants on their own unit, so a unit admin cannot mint a peer to approve their own widening.
- The change read and the dry-run report list `approvers`: who could decide it, the proposer left out.

## Locks

- A unit's `locked_knobs`, and the boundary's, name knobs no unit below may change, a narrowing included.
- `desktop.paste_in`, `desktop.copy_out`, `desktop.upload` and `desktop.download` each lock one desktop direction at the locker's rule.
- A write that changes a locked knob is `409` `unit_knob_locked`: "`<knob>` is locked by `<locker>`; ask `<contact>`".
- Locking a knob a unit below sets to another value is the same `409`, listing those units. Their values are never cleared for you.
- A unit's own admins, or anyone above them, may lock it. Only a security admin locks on the boundary, whose locks bind while it is in `hard_limits`.
- Adding a lock applies directly; removing one is held. Restrictions ignore locks.
- A run's provenance marks a locked knob `locked`, named for the nearest locker.

## Making someone a unit admin

1. Sign in as a security admin, or as a unit admin of a unit above the one to grant.
2. `POST /api/v1/governance/units/{id}/admins` with `{"subject_type":"user","subject":"<sub or email>"}`, or `"group"` and the group name. The CLI is `wardyn governance admins add <unit> --user <x>` (or `--group`).
3. A unit admin's grant, or any grant with the switch on, answers `202` with the held change. A second human approves it under Governance changes.
4. The person's units appear in `GET /me`'s `unit_admin_of`.
5. To take it back, `DELETE /api/v1/governance/units/{id}/admins/{admin}`, or `wardyn governance admins remove <unit> --user <x>`.
