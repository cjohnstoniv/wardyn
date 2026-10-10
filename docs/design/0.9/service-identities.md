# 0.9 service identities: principal, sponsor, reach and audit (#1508)

Status: **Owner rulings recorded 2026-10-10**, amended after review r1 ([rulings](invitations-service-identities.md#11-owner-rulings-2026-10-10)).
This page is the service-identity half of the [#1508 design](invitations-service-identities.md); scope, migration, wire, tests, residuals and slices live there.
`Dn` names a ruling in that table.

## 1. Principal

- `service:<name>`, `name` matching `^[a-z][a-z0-9-]{2,62}$`, so it is already in `CanonicalUserSubject` form.
- `isReservedPrincipal` gains the `service:` prefix, so no sign-in, cookie, portal exchange or `POST /people` can become one.
- A name is never reused and never renamed: a retired row stays, so no later identity inherits its grants, ceiling or audit history.
- `POST /service-identities` refuses a name any principal-keyed table already holds (`409` `service_identity_name_taken`); the tables are listed in [Schema 0196](invitations-service-identities.md#61-schema-0196).

## 2. Record

Table `service_identities`, in migration 0196.

| Field | Meaning |
|---|---|
| `name` | Primary key |
| `description` | Free text, `200` characters, control-free |
| `sponsor` | Exactly the `principal_identities.principal` of a bound, non-deactivated identity; never reserved, never another service |
| `sponsor_since` | When the current sponsor took over; the read-side cutoff for older tokens ([§3](#3-authentication)) |
| `user_type` | An existing user type; default `standard` |
| `created_by`, `created_at` | Who created it |
| `suspended_at`, `suspended_by` | Set by an admin suspend; cleared by resume |
| `retired_at`, `retired_by` | Terminal |

- `sponsor` is resolved at create and at `PATCH`; an alias form (an email, or `entra:<tid>:<oid>` for a person bound under a pairwise `sub`) is refused `422` `service_identity_sponsor_invalid`.
- State is derived at read: `active`, `suspended`, `retired`, or `sponsor_inactive` when `CheckSession` on the sponsor is not live.
- Its tokens are ordinary `api_tokens` rows: principal `service:<name>`, role `member`, no email, empty non-nil groups, `groups_truncated` false, no `MintedBy`.

## 3. Authentication

`apiTokenAuth` branches on the row's principal after the lookup and before `refuseReservedPrincipal`:

1. Read the identity by name. Missing, suspended or retired falls through to the one `401`; `auth.fail` reason `service_identity_inactive`.
2. Refuse a token with `CreatedAt <= sponsor_since`, the repository's read-side cutoff rule; it falls through, reason `service_identity_inactive`.
3. Ask `oidc.CheckSession(sponsor, "", token.CreatedAt, -1)`. Not live falls through; reason `service_sponsor_inactive`. A store failure is `503`, as today.
4. Skip `refuseStaleRoleStamp`: the role and type come from the live row on every request, not a sign-in stamp.
5. Publish `withHumanIdentity(principal, "", member, user_type, [], false)`, then `withServiceIdentity(name, sponsor)` and `withAPITokenID`.
6. Refuse a route outside the allow-list with `403` `service_scope` and an `authz.denied` row, as the delegated lane does.

- `actorFromContext` returns `service` and the principal, ahead of its human arm.
- `neverOperator` is true, so neither admin tier answers yes and no second-human gate counts it.
- The head recorder that stamps `data.via` stamps `data.sponsor` on every row the request writes.
- `operatorOwnedRequest` is false: a service run is never operator-owned.
- C2 reviews every actor-type comparison: `refusePolicyPreviewRate` and `refusePreflightRate` exempt non-humans and must limit a service; `sessionRevokedSince` must re-check service and sponsor.

## 4. Reach

| Route | Why it is on the list |
|---|---|
| `POST /runs`, `POST /runs/preflight` | Launch, and the dry run CI uses |
| `GET /runs`, `GET /runs/{id}`, `GET /runs/{id}/events`, `GET /runs/{id}/output` | Read its own runs |
| `PATCH /runs/{id}`, `POST /runs/{id}/kill` | Manage its own runs |
| `GET /me`, `GET /setup/status` | Who am I; `ci-run.sh` reads `provider_access` |
| `PUT`, `DELETE /model-providers/{id}/credential` | Its own model key, in its own namespace (D13) |

- Everything else answers `403` `service_scope`, including any route added later (D9).
- Absent on purpose: attach, approvals, secrets, tokens, SSH keys, invitations, runner claims, every admin route.

## 5. Grants, ceiling and credentials

- Capability grants and governance assignments bind with the existing `user` subject type and the subject `service:<name>`. No new subject type.
- With empty groups, no group-tier row matches; `all` and `user_type` rows do, as for any member.
- An identity with no user-tier assignment resolves the deployment default, like a member with none.
- A run's secret reads use its subject, so they read the `service:<name>` namespace, then the operator's unless the grant is `owner_only` (`grantReadOwner`).
- An `owner_only` grant on a service run fails closed when the service stored nothing.

## 6. Sponsor lifecycle

| Event | Effect | Where |
|---|---|---|
| Create (security tier, API or CLI) | Identity exists with no token; inert until the sponsor mints one | `POST /service-identities` |
| Sponsor mints | Console mint card, show-once dialog; row `service_identity.token.create` (D4, D16) | `POST /me/service-identities/{name}/tokens` |
| Sponsor change | Live tokens revoked in the same transaction; `sponsor_since` set, so a mint committing after the revoke's snapshot still never authenticates (D7) | `PATCH /service-identities/{name}` |
| Sponsor's full session revoke | Tokens minted before the cutoff stop at once; the sweep also marks them revoked | `revokePersonCredentials` |
| Sponsor suspended by SCIM | State `sponsor_inactive`; tokens refused; live runs continue (D6) | `CheckSession` deactivated arm |
| Sponsor's identity unbind | Refused `409` `identity_principal_in_use` while the principal sponsors a non-retired identity or has a pending invitation | `handleUnbindIdentity`; `PrincipalInUseError` gains both counts |
| Sponsor's session-only cut | No effect, as for the sponsor's own tokens | epoch `-1` |
| Admin suspend | Tokens refused; live runs continue until killed | `PATCH /service-identities/{name}` |
| Admin retire | Tokens revoked; the identity's stored credentials erased through `eraseLocked` for owner `service:<name>`, row `credential.erase` | `DELETE /service-identities/{name}` |

- Service tokens require an expiry: default and maximum `90d`, or the deployment cap if lower (D5).
- Because the sponsor's identity row cannot be unbound, a SCIM suspension of an Entra-keyed sponsor always reaches the deactivated arm.

| Caller on `POST /me/service-identities/{name}/tokens` | Answer |
|---|---|
| The current sponsor, signed-in session, identity active | `201`, plaintext once |
| The sponsor, identity suspended, retired or `sponsor_inactive` | `409` `service_identity_inactive` |
| Another person, any role, signed in | `404` `service_identity_not_found`, body of a missing name |
| The admin token, or local mode | `403` `api_token_no_human` |
| Any `wdn_` token, the sponsor's included | `403` `api_token_from_api_token` |
| A portal's `wdg_` token | `403` `delegation_scope` |
| A service identity's token | `403` `service_scope` |
| The sponsor in member mode | `409` `api_token_member_mode_mint` |
| The sponsor at `20` live tokens for the identity | `422` `api_token_cap_reached` |

## 7. Runs

- `CreatedBy` is `service:<name>`; `OperatorOwned` is false; `MintRunIdentity` gets the sponsor as `sponsor`.
- Dispatch and revive re-mint read the sponsor from the identity row, not `run.CreatedBy`.
- The console shows the service as owner and the sponsor beside it.

## 8. Sponsor reach over the service's runs

The amended D11, with the re-rulings on review findings F3, F6 and F7.

| Act | Sponsor may | Seam |
|---|---|---|
| Read: `GET /runs`, `GET /runs/{id}`, `/events`, `/output` | Always, while the current sponsor | `ownsRunOrSponsorOrAdmin`, on these routes only |
| Kill: `POST /runs/{id}/kill` | Always, while the current sponsor | `ownsRunOrSponsorOrAdmin` |
| Decide held egress or Azure DevOps approvals, download sandbox files, read grants, profile, resources, run tokens | Never; admins decide the holds | `ownsRunOrAdmin` and `getRunAuthorized` stay unchanged |
| Attach: terminal or UI app | Only with an explicit `service_attach` grant, always through the run's doors | `mayEnterRun` gains a sponsor arm |
| Attach over SSH | Never in 0.9 | `sshAuth` refuses, reason `run_owner_only` |

The carrier is the existing capability kind `capFeature`, with one new value in its closed set `featureValues`:

- `service_attach`: "may enter the runs of service identities I sponsor". `capAllowed`, the one grant resolver, answers it at the sponsor arm.
- Default off: migration 0196 writes a `capability_restrictions` row for (`feature`, `service_attach`). A restricted value admits only an allow naming it, so a `*` allow grants nobody.
- Admins turn it on per person, group or user type with an ordinary grant: `POST /permissions/grants`, kind `feature`, value `service_attach`.
- That write is security tier, audited `capability.grant.create`, and held for a second human under `WARDYN_GOVERNANCE_SECOND_HUMAN`. A user-type deny blocks it as for any value.
- Opening it to every sponsor is a deliberate `PUT /permissions/availability/feature/service_attach` back to Everyone, audited `capability.availability.write`.
- Same rules for everyone: the batch is asked with its operator step off, so a super admin sponsor needs the grant too. A super admin without it has normal admin access, recorded as admin: read and kill, no entry.

How an entry is checked:

1. The ticket mint (`handleAttachTicket`) asks the sponsor arm with the caller's session: current sponsor, identity active, `service_attach` allowed.
2. A sponsor entry stamps the ticket role `member` and `entry` `sponsor`, whatever the minter's role, so `refuseAttachEntry` never takes the `adminDoorExempt` exemption and `refuseInteractiveAttach` always runs.
3. Ticket consume and the UI session's 30-second re-check ask the structural part: still the current sponsor, identity active. A sponsor change or suspend ends the next re-check.
4. A withdrawn grant refuses the next ticket. An open terminal stream keeps working until it closes, as a withdrawn `feature` value never re-checks what exists.
5. A refusal is the foreign-run `404` a member gets today, with an `authz.denied` row of reason `capability_feature`.

The record always names the sponsor:

- The ticket is minted by the sponsor's own session or token, so `session.attach` and `ui.open` rows carry `actor_type` `human` and the sponsor as actor.
- Those rows add `service_identity` and `entry` `sponsor`, and the run's own audit trail shows them.
- A service token never reaches an attach route ([§4](#4-reach)), so the service is never the actor of an entry.

## 9. Audit actor

- New `actor_type` value `service`, with `actor` `service:<name>` and `data.sponsor` on every row (D8).
- Migration 0196 widens the check on the partitioned `audit_events` parent only; `audit_events_legacy` takes no new rows. C1 reports the lock mode the rewrite takes, and its time.
- D8's fallback is taken only if C1 measures that rewrite as too slow. Its form is `actor_type` `system` with actor `service:<name>`.
- Under the fallback, `operatorOwnedRequest`, `sessionRevokedSince`, `mayEnterRun` and `neverOperator` key on `withServiceIdentity`, never on the actor type. Otherwise every service run would read as operator-owned.
- `TestServiceIdentity_RunsUnderItsOwnPrincipal` and `TestServiceIdentity_NeverOperatorNeverSecondHuman` run under both actor forms.

Sealing under `WARDYN_AUDIT_SEAL=full`:

- The service name is not personal data, so the actor column stays `service:<name>`.
- The sponsor is a person. `Sealer.Seal` gains a step after `sealActor` that stores `data.sponsor` as `subject:<id>` through `actorSubject`, with the same pending-key fallback.
- The same step covers `sponsor`, `sponsor_from` and `sponsor_to` on `service_identity.*` rows, and `target` on `invitation.*` rows.
- After the sponsor's erasure their subject no longer opens, so the service's trail names nobody.
- An invitation target with no identity row keeps their name, as `ActorSubject` does today for an actor. A sponsor always has one ([§2](#2-record)), so no new residual is published.
