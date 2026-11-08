> Part of the [Operations](../OPERATIONS.md) split (task pages under `docs/operations/`).

# Platform tenancy: running a platform inside an envelope

- A platform team governs its own part of the tree: a unit, the units below it, and the people in them.
- Without an envelope, a change to a unit's own walls could be approved by a peer. Since the strict approver rule, a change to a unit itself is decided strictly from above it ([four-eyes](unit-admins.md#four-eyes)).
- An envelope lets a platform's unit admins move fast inside walls the security tier sets.
- It changes no run. It changes which of a unit admin's writes need a second person.

## The envelope

An envelope names the unit that roots a platform and the IdP groups security approved for it.

| Part | What it is |
|---|---|
| Write | `PUT` and `DELETE /api/v1/governance/units/{id}/envelope`, with `{"approved_groups": [...]}` of at most 64 groups |
| Who sets it | A security admin or a super admin. It is held under `WARDYN_GOVERNANCE_SECOND_HUMAN` ([governance writes](governance-writes.md)) |
| One envelope per path | A unit above or below one that has an envelope is refused `409` `envelope_nested` |
| One envelope per group | A group already approved on another envelope is refused `409` `envelope_group_taken` |
| The Default unit | Refused `400` `envelope_invalid`: the deployment sets its walls |
| At run time | Nothing. No run is affected until a unit admin writes inside the envelope |

- `GET /governance` lists the envelopes. A unit admin reads each one's setter as `security`.
- The audit row is `governance.envelope.set` ([audit actions](../AUDIT-ACTIONS.md)).

## What applies directly and what is held

The table is for a unit admin who governs the unit. A security admin's writes follow `WARDYN_GOVERNANCE_SECOND_HUMAN` as before.

| Write | Applies directly | Needs a second person | Only security decides |
|---|---|---|---|
| A unit below the root: walls or parent | The new walls stay within the root's ceiling and limits, with no higher member CPU and memory cap, and the unit stays below the same root | Otherwise, unless the change only narrows. A change that removes a lock or changes the contact is held whatever the walls | — |
| Renaming a profile | The rename cannot change which profile a person is given | It could: a group assignment of the profile shares its priority with another profile's. This is checked before the envelope, so an envelope never admits it | — |
| The root itself | — | — | Every held change of the unit itself (walls, locks, contact, name, parent, delete), and every unit admin grant or revoke on it |
| A group in the unit's membership | Adding a group on the approved list to the root or a unit below it, or moving that group's membership between two units of the envelope | A group off the list, as without an envelope. A write that changes which membership selects a person's profile | — |
| A person or user type in the unit's membership | — | Always, as without an envelope | — |
| Deleting a membership | — | As without an envelope ([four-eyes](unit-admins.md#four-eyes)) | — |
| A unit admin grant or revoke below the root | — | Always, decided strictly from above | — |
| Deciders on a unit | — | Always, as without an envelope | — |
| An envelope that cannot be read | — | The envelope admits nothing: each write takes the path it takes without one | The root's changes |

- A held change is decided as in [four-eyes](unit-admins.md#four-eyes): a security admin, or a unit admin strictly above the unit. For the root it is a security admin alone, and a unit admin above the root who proposes one still sees it, can add a note and can withdraw it.
- Composition already keeps a unit below the root within the root's walls, so the comparison with the root's walls is a guard.

## Recipes

### Images through requests

1. A security admin, or a unit admin above the root, writes an `image` rule on the platform root that names the platform's admins or group ([kinds](deciders.md#kinds)).
2. A person whose default unit is the root or below it asks for one image by digest ([image requests](deciders.md#image-requests)).
3. A decider the rule names approves it. The approval writes an allow grant of exactly that image for the person.

### Grants to platform groups

1. A security admin writes a capability grant with `subject_type` `group` for the platform's IdP group ([capabilities](capabilities.md)).
2. The platform administers who is in the group at the identity provider.
3. A unit admin adds that group, when it is on the approved list, to the platform's units without a second person.

### Platform tools as inline components

1. The platform's portal sends the tool as an inline component in `POST /runs`, so it travels with the run and names no organisation secret ([custom components](custom-components.md)).
2. Anything that names a secret, a CLI card or MCP authentication stays an organisation component.

### Keep production out

1. A security admin adds the production component's id to `deny_components` on the platform root ([`deny_components`](../POLICIES.md#deny_components-and-deny_model_providers)). It applies to the root and every unit below it, and a child cannot lift it.
2. Pin the production component to the production unit ([custom components](custom-components.md)). A pinned component is usable only by runs in that unit or below it.

### Route held changes to your team

1. Wardyn sends no message when a change waits. Relay the acknowledged audit webhook ([governance writes](governance-writes.md)).
2. Keep the rows whose `action` is `governance.change.propose`, the row of a held change, or `governance.elevation.request` or `governance.image_request.request`.
3. Route on `data.units`, the ids of the units the change touches.

### A portal pinned to the platform

1. Register the portal with `"run_in_under":"<the platform root>"` ([delegated run management](api-tokens.md#delegated-run-management-portals)).
2. A run it launches must resolve to the root or a unit below it, or `POST /runs` answers `403` `portal_unit_scope`.

### The platform's own file

1. Read the platform with `wardyn governance get --under <root> > platform.json`.
2. Review the steps with `wardyn governance plan platform.json --under <root>`.
3. Apply them with `wardyn governance set platform.json --under <root>`. Entries outside the root are refused ([one file per platform](governance-writes.md#one-file-per-platform)).

### A pool per platform

1. Add a use policy row to an organisation pool with `subject_type` `unit` and the platform root's id ([runner pools](runner-pools.md#runners-and-pools)).
2. A run launched in the root or a unit below it may then use the pool.

## Where the recording and deploy floors live

- The recording floor is not a boundary field. It is the organisation's recording setting with a level and `locked` on, the minimum no policy goes under (`PUT /governance/recording`, [admin-only writes](admin-only-writes.md); [recording](../POLICIES.md#recording)).
- "No production deploy" is the provider row's capability ceiling, or the boundary's `spec.azure_devops_capabilities` ([boundary](boundary.md)).

## Data that leaves a run

What a run may send out is the [`data_out`](../POLICIES.md#data_out) rule, set by a policy, a unit's walls, a restriction or the boundary.

## In the console

- **Governance › Profiles** marks a platform root with **Envelope root** and a profile inside one with **In envelope** and the root's name.
- The **Platform envelopes** card lets a security admin set, edit and remove an envelope's approved groups. A unit admin sees **Your platform's envelope**, read only.
- A held envelope change is listed under **Governance › Changes** as **Platform envelope**.

## Before you downgrade

An older version ignores envelopes. A held change of the root, and a unit admin grant or revoke on it, can again be approved by a unit admin who governs it, and writes below the root are held again. Read the "Before you downgrade" notes in the [changelog](../../CHANGELOG.md).
