# 0.9 owner-redeemed invitations and service identities (#1508)

Status: **Owner rulings recorded 2026-10-10** ([§11](#11-owner-rulings-2026-10-10)); the build slices wait only on review.
A person gets their own credential through an invitation only they can claim. Automation runs as a named, sponsored service identity, never as a person.
Admin-minted tokens from 0.8.5 get a visible path to one of the two, and nothing that works today stops working.

## 1. Scope

| In scope | Out of scope |
|---|---|
| Invitations: create, list, revoke, claim by the signed-in target | The 0.8.5 restriction and its inventory, which have shipped |
| Service identities: principal, sponsor, tokens, reach, runs, audit actor | Consented support sessions on another person's run (a separate 0.9 issue) |
| A disposition for every admin-minted token, and the moves to an invitation or a service identity | Workload federation (a CI job's own OIDC token instead of a bearer) |
| Owner-authorised delegation: the decision, and the one missing test | Any change to the portal delegation lane (#1142) |

### 1.1 Changes to the planner's proposal

| Brief item | This design | Why |
|---|---|---|
| Invitation reuses the hashed single-use token shape | No secret: the target's own session is the only authority ([D1](#11-owner-rulings-2026-10-10)) | A claim needs the target signed in anyway, so a code adds a thing to steal and protects nothing |
| Redeem route | `POST /me/invitations/{id}/claim`, audit `invitation.claim` | `claim` is already on the closed verb list ([Grammar](../../AUDIT-ACTIONS.md#grammar)); `redeem` would extend it |
| C1–C3 core, M1–M3 mechanical | Adds C4 for the amended D11, and U1, a console slice | A claim needs a browser session: the CLI has no sign-in, and an API token may not mint |
| Migration per slice | One file, `0196_invitations_service_identities.sql`, owned by C1 | The lead gave one number; C2 and C3 build on C1's tables |
| Delegation, if needed | No new mechanism ([§5](#5-owner-authorised-delegation)) | The portal lane already records both parties and revokes without ending sessions |

## 2. Facts this design builds on

| Fact | Where |
|---|---|
| No route creates a token that acts as another person; the old route answers `403` `person_token_mint_removed` | `Server.handleMintPersonAPIToken` in [`internal/api/people.go`](../../../internal/api/people.go) |
| `insertAPIToken` has one caller and no code writes `MintedBy` | `TestAPITokenMintSites_OnlyTheSelfServiceMint` in [`internal/api/apitoken_mint_sites_test.go`](../../../internal/api/apitoken_mint_sites_test.go) |
| The self-mint stamps `authorizedAt` before reading the body, refuses tokens, delegated tokens and member mode, then re-checks the session late | `Server.handleCreateAPIToken` in [`internal/api/apitokens.go`](../../../internal/api/apitokens.go) |
| A `wdn_` token republishes its row through `withHumanIdentity`; unknown, revoked and cut-off rows fall through to one `401` | `Server.apiTokenAuth` |
| The inventory of admin-minted tokens is `MintedBy != "" && MintedBy != Principal`, live only | `Server.handleListAllAPITokens` (`?minted_for_others=true`) |
| A principal is the IdP `sub`, or `entra:<tid>:<oid>` when issuer, `tid` and `oid` match a person row exactly | `peopleKeying.PrincipalFor` in [`internal/api/people.go`](../../../internal/api/people.go) |
| Reserved principals are refused at every human door: callback, cookie, `wdn_` token, portal exchange | `Server.isReservedPrincipal` in [`internal/api/reserved_principal.go`](../../../internal/api/reserved_principal.go) |
| A portal's token publishes the person at user reach, on an allow-list, with `data.via` on every row | `Server.delegatedTokenAuth`, `delegationAllowed` in [`internal/api/delegation.go`](../../../internal/api/delegation.go) |
| A device, a portal and SCIM are never an operator, whatever the context holds | `neverOperator` in [`internal/api/delegation.go`](../../../internal/api/delegation.go) |
| `identity.Claims` already carries `Sponsor`, "the accountable human owner"; run create passes the creator | `Claims` in [`internal/identity/identity.go`](../../../internal/identity/identity.go) |
| Operator-owned runs come from what authenticated the request: local mode, or a system actor that is not a device | `operatorOwnedRequest` in [`internal/api/runs_policy.go`](../../../internal/api/runs_policy.go) |
| `audit_events.actor_type` is checked to `human`, `agent`, `system`; the partitioned table copied the check | [`0001_init.sql`](../../../internal/db/migrations/0001_init.sql), [`0111_audit_partitioned.sql`](../../../internal/db/migrations/0111_audit_partitioned.sql) |
| A SCIM suspension cuts sessions, deactivates the identity, sweeps credentials and kills runs, keyed by the person's forms | `Server.suspendIdentity` in [`internal/api/scim_suspend.go`](../../../internal/api/scim_suspend.go) |
| `oidc.CheckSession` answers live, revoked or deactivated for a principal and an issue time; `-1` epoch ignores a session-only cut | `CheckSession` in [`internal/auth/oidc/identity_gate.go`](../../../internal/auth/oidc/identity_gate.go) |

## 3. Invitations

### 3.1 Record

Table `person_invitations`, in migration 0196. No secret column.

| Field | Meaning |
|---|---|
| `id` | UUID; appears in the console link |
| `target` | The principal a claim must be signed in as, exactly |
| `created_by`, `created_at` | The admin, and when |
| `expires_at` | Default `72h` after create, at most `14d` ([D2](#11-owner-rulings-2026-10-10)) |
| `token_name` | The name the claimed token gets unless the claimer gives one |
| `token_ttl_seconds` | Upper bound on the claimed token's lifetime; `NULL` is the deployment rule; `WARDYN_API_TOKEN_MAX_TTL` still caps it |
| `replaces_token_id` | An admin-minted token this invitation retires when claimed ([§6.2](#62-admin-minted-tokens)) |
| `claimed_at`, `token_id` | Set together, once, by the claim |
| `revoked_at`, `revoked_by` | Set by an admin revoke |

- State is derived, never stored: `pending`, `claimed`, `expired` or `revoked`.
- `token_id` is metadata; the plaintext exists only in the claim's `201`. No auth branch reads the table.

### 3.2 Create

`POST /people/{principal}/invitations` on `securityOps`, the tier of `POST /people`. Callers that pass it today pass it here, the admin token included, since nothing is returned that authenticates.

| Condition | Answer |
|---|---|
| Principal fails `validSubject`, is reserved, or is a `service:` principal | `422` `invitation_target_invalid` |
| No person row and no bound identity has that principal | `422` `invitation_target_unknown`: "create the person first with POST /people" |
| `replaces_token_id` is not live, not admin-minted, or belongs to another principal | `422` `invitation_replaces_invalid` |
| `expires_in_seconds` outside `1` to `14d`, or `token_ttl_seconds` negative | `400` `invitation_ttl_invalid` |
| Ten pending invitations already name the target | `422` `invitation_cap_reached` |
| Otherwise | `201` with the record and `console_path`, a row `invitation.create` |

- The target is never inferred from an email, a role or the caller.
- An Entra person known by a pairwise `sub` keeps it (`person.attach` `sub_known`); invite them by that `sub`, never by `entra:`.

### 3.3 Claim

`POST /me/invitations/{id}/claim`, body `{"name"?, "ttl_seconds"?}`. It shares one mint core with `POST /me/tokens`, split out of `handleCreateAPIToken`.

1. Stamp `authorizedAt` before the body is read.
2. Run the self-mint guards in their order, with their byte-identical refusals, plus one for a service context.
3. Read the invitation by `id AND target = <session principal>`; no row answers `404` `invitation_not_found`.
4. Refuse a non-pending state with `409` `invitation_not_pending` and the state.
5. Resolve name and lifetime: the request's, bounded by `token_ttl_seconds` and the deployment cap.
6. Check the per-principal cap, then re-check the session late, as the self-mint does.
7. In one transaction: mark it claimed if still pending, insert the token, revoke `replaces_token_id`. A lost race answers `409`.
8. Write `token.create` (with `invitation`), `invitation.claim`, and `token.revoke` (reason `replaced_by_owner`) when a token was retired.
9. Answer `201` with the token, plaintext once, to the claimer only.

- The token's principal, role, type and groups come from the claimer's session, never from the invitation.
- `MintedBy` stays empty: the owner minted it. A claim does not grant the `api_token` feature ([D3](#11-owner-rulings-2026-10-10)).

### 3.4 Who gets what

| Caller on `POST /me/invitations/{id}/claim` | Answer | Why this status |
|---|---|---|
| The target, signed in, invitation pending | `201` | — |
| The target, invitation expired, claimed or revoked | `409` `invitation_not_pending`, `state` | Their own invitation: its state is no oracle |
| Another person, admin, security admin or super admin, signed in | `404` `invitation_not_found`, body of a missing id | `403` would confirm an invitation for someone else, and the link travels by email |
| The admin token, or local mode | `403` `api_token_no_human` | No person to mint for |
| Any `wdn_` token, the target's included | `403` `api_token_from_api_token` | A token never mints a token |
| A portal's `wdg_` token | `403` `delegation_scope` | Not on `delegationAllowed` |
| A service identity's token | `403` `service_scope` | Not on its allow-list ([§4.4](#44-reach)) |
| The target in member mode | `409` `api_token_member_mode_mint` | As the self-mint |

Admin reads (`GET /invitations`, `GET /people/{principal}/invitations`) return the record and `token_id`, never `token`. `DELETE /invitations/{id}` revokes a pending one (`invitation.revoke`); otherwise `404`.

## 4. Service identities

### 4.1 Principal

- `service:<name>`, `name` matching `^[a-z][a-z0-9-]{2,62}$`, so it is already in `CanonicalUserSubject` form.
- `isReservedPrincipal` gains the `service:` prefix, so no sign-in, cookie, portal exchange or `POST /people` can become one.
- A name is never reused: a retired row stays, so no later identity inherits its grants, ceiling or audit history.

### 4.2 Record

Table `service_identities`, in migration 0196.

| Field | Meaning |
|---|---|
| `name` | Primary key |
| `description` | Free text, `200` characters, control-free |
| `sponsor` | A person's principal: known, not reserved, not deactivated, never another service |
| `user_type` | An existing user type; default `standard` |
| `created_by`, `created_at` | Who created it |
| `suspended_at`, `suspended_by` | Set by an admin suspend; cleared by resume |
| `retired_at`, `retired_by` | Terminal |

- State is derived at read: `active`, `suspended`, `retired`, or `sponsor_inactive` when `CheckSession` on the sponsor is not live.
- Its tokens are ordinary `api_tokens` rows: principal `service:<name>`, role `member`, no email, empty non-nil groups, `groups_truncated` false, no `MintedBy`.

### 4.3 Authentication

`apiTokenAuth` branches on the row's principal after the lookup and before `refuseReservedPrincipal`:

1. Read the identity by name. Missing, suspended or retired falls through to the one `401`; `auth.fail` reason `service_identity_inactive`.
2. Ask `oidc.CheckSession(sponsor, "", token.CreatedAt, -1)`. Not live falls through; reason `service_sponsor_inactive`. A store failure is `503`, as today.
3. Skip `refuseStaleRoleStamp`: the role and type come from the live row on every request, not a sign-in stamp.
4. Publish `withHumanIdentity(principal, "", member, user_type, [], false)`, then `withServiceIdentity(name, sponsor)` and `withAPITokenID`.
5. Refuse a route outside the allow-list with `403` `service_scope` and an `authz.denied` row, as the delegated lane does.

- `actorFromContext` returns `service` and the principal, ahead of its human arm.
- `neverOperator` is true, so neither admin tier answers yes and no second-human gate counts it.
- The head recorder that stamps `data.via` stamps `data.sponsor` on every row the request writes.
- `operatorOwnedRequest` is false: a service run is never operator-owned.
- C2 reviews every actor-type comparison: `refusePolicyPreviewRate` and `refusePreflightRate` exempt non-humans and must limit a service; `sessionRevokedSince` must re-check service and sponsor.

### 4.4 Reach

| Route | Why it is on the list |
|---|---|
| `POST /runs`, `POST /runs/preflight` | Launch, and the dry run CI uses |
| `GET /runs`, `GET /runs/{id}`, `GET /runs/{id}/events`, `GET /runs/{id}/output` | Read its own runs |
| `PATCH /runs/{id}`, `POST /runs/{id}/kill` | Manage its own runs |
| `GET /me`, `GET /setup/status` | Who am I; `ci-run.sh` reads `provider_access` |
| `PUT`, `DELETE /model-providers/{id}/credential` | Its own model key, in its own namespace ([D13](#11-owner-rulings-2026-10-10)) |

- Everything else answers `403` `service_scope`, including any route added later ([D9](#11-owner-rulings-2026-10-10)).
- Absent on purpose: attach, approvals, secrets, tokens, SSH keys, invitations, runner claims, every admin route.

### 4.5 Grants, ceiling and credentials

- Capability grants and governance assignments bind with the existing `user` subject type and the subject `service:<name>`. No new subject type.
- With empty groups, no group-tier row matches; `all` and `user_type` rows do, as for any member.
- An identity with no user-tier assignment resolves the deployment default, like a member with none.
- A run's secret reads use its subject, so they read the `service:<name>` namespace, then the operator's unless the grant is `owner_only` (`grantReadOwner`).
- An `owner_only` grant on a service run fails closed when the service stored nothing.

### 4.6 Sponsor lifecycle

| Event | Effect | Where |
|---|---|---|
| Create (security tier) | Identity exists with no token; inert until the sponsor mints one | `POST /service-identities` |
| Sponsor mints | `201` with plaintext once, to the sponsor; row `service_identity.token.create` | `POST /me/service-identities/{name}/tokens` |
| Sponsor change | Every live token of the identity is revoked in the same transaction ([D7](#11-owner-rulings-2026-10-10)) | `PATCH /service-identities/{name}` |
| Sponsor's full session revoke | Tokens minted before the cutoff stop at once; the sweep also marks them revoked | `revokePersonCredentials` |
| Sponsor suspended by SCIM | State `sponsor_inactive`; tokens refused; live runs continue ([D6](#11-owner-rulings-2026-10-10)) | `CheckSession` deactivated arm |
| Sponsor's session-only cut | No effect, as for the sponsor's own tokens | epoch `-1` |
| Admin suspend or retire | Tokens refused; retire also revokes them; live runs continue until killed | `PATCH`, `DELETE /service-identities/{name}` |

- Only the current sponsor mints, from a signed-in session that is not a token, a portal or member mode ([D4](#11-owner-rulings-2026-10-10)).
- Because a sponsor change revokes, every live token was minted by the current sponsor, so checking the sponsor alone is exact.
- Service tokens require an expiry: default and maximum `90d`, or the deployment cap if lower ([D5](#11-owner-rulings-2026-10-10)).

### 4.7 Runs

- `CreatedBy` is `service:<name>`; `OperatorOwned` is false; `MintRunIdentity` gets the sponsor as `sponsor`.
- Dispatch and revive re-mint read the sponsor from the identity row, not `run.CreatedBy`.
- The console shows the service as owner and the sponsor beside it.
- The sponsor may read and kill the service's runs, and attach only when governed in ([§4.9](#49-sponsor-reach-over-the-services-runs)). Admins keep today's reach.

### 4.8 Audit actor

- New `actor_type` value `service`, with `actor` `service:<name>` and `data.sponsor` on every row ([D8](#11-owner-rulings-2026-10-10)).
- Migration 0196 widens the check on `audit_events` and `audit_events_legacy`. C1 measures the validation cost on a seeded partition set.
- Sealing is unchanged: `Sealer.sealsActor` seals human actors only, and a service name is not personal data.

### 4.9 Sponsor reach over the service's runs

[D11](#11-owner-rulings-2026-10-10) splits the sponsor's reach in two: read and kill always, attach only when an admin turns it on.

| Act | Sponsor may | Seam |
|---|---|---|
| Read (`GET /runs`, `/runs/{id}`, events, output) | Always, while they are the current sponsor | `ownsRunOrAdmin` gains a sponsor arm |
| Kill | Always, while they are the current sponsor | the same arm, on `POST /runs/{id}/kill` |
| Attach: terminal, UI app, SSH | Only with the `service_attach` feature, and only through the run's own `deny_interactive` and `deny_ui_apps` doors | `mayEnterRun` gains a sponsor arm |

The carrier is the existing capability kind `capFeature`, with one new value in its closed set `featureValues`:

- `service_attach`: "may enter the runs of service identities I sponsor". It is checked by `capAllowed`, the one grant resolver, at the sponsor arm.
- Default off: migration 0196 writes a `capability_restrictions` row for (`feature`, `service_attach`). A restricted value admits only an allow naming it, so a `*` allow grants nobody, whether or not `feature` is enforced.
- Admins turn it on per person, group or user type with an ordinary grant: `POST /permissions/grants`, kind `feature`, value `service_attach`.
- That write is security tier, audited `capability.grant.create`, and held for a second human under `WARDYN_GOVERNANCE_SECOND_HUMAN`. A deny row for a user type blocks it as for any value.
- Opening it to every sponsor is a deliberate `PUT /permissions/availability/feature/service_attach` back to Everyone, audited `capability.availability.write`.
- A super admin sponsor passes as on every capability (`capBatch.decide` step 1), unless `WARDYN_GOVERN_ADMIN_RUNS` governs their runs. A security admin is governed like a member.

How an entry is checked:

1. The ticket mint (`handleAttachTicket`) and the SSH gateway ask the sponsor arm with the caller's session: current sponsor, identity active, `service_attach` allowed.
2. Ticket consume and the UI session's 30-second re-check ask only the structural part: still the current sponsor, identity active. A sponsor change or suspend ends the next re-check.
3. A withdrawn grant refuses the next ticket. An open terminal stream keeps working until it closes, as a withdrawn `feature` value never re-checks what exists.
4. A refusal is the foreign-run `404` a member gets today, with an `authz.denied` row of reason `capability_feature`.

The record always names the sponsor:

- The ticket is minted by the sponsor's own session or token, so `session.attach`, `ui.open` and `ssh.authenticate` rows carry `actor_type` `human` and the sponsor as actor.
- Those rows add `service_identity` and `entry` `sponsor`, and the run's own audit trail shows them.
- A service token never reaches an attach route ([§4.4](#44-reach)), so the service is never the actor of an entry.

## 5. Owner-authorised delegation

- No new mechanism in 0.9 ([D12](#11-owner-rulings-2026-10-10)).
- Acting for a present person is the portal lane: the person is the actor and `data.via` names portal and grant. Revoking the portal leaves the person's sessions alone.
- Unattended work is a service identity acting as itself, so no grant lets anything act as an absent person.
- The missing proof is one test, `TestDelegation_PortalRevokeKeepsThePersonSignedIn` ([§8](#8-acceptance-to-tests)).

## 6. Migration

### 6.1 Schema 0196

- One file, `0196_invitations_service_identities.sql`, provisional and renumbered at merge.
- `person_invitations` ([§3.1](#31-record)) and `service_identities` ([§4.2](#42-record)).
- `api_token_moves (token_id PK → api_tokens, service_identity → service_identities, linked_by, linked_at)`.
- The `actor_type` check widened to add `service`.
- The `service:` collision check, which aborts with the offending table named.
- `api_tokens` is untouched: no column added, no row relabelled, `minted_by` kept.

### 6.2 Admin-minted tokens

Each row of `?minted_for_others=true` gains `disposition`, derived at read. Revoked rows stay out by default; `&include_revoked=true` returns them, so a finished move stays visible.

| `disposition` | Condition |
|---|---|
| `needs_decision` | Live; no pending invitation replaces it; no move |
| `awaiting_owner` | Live; a pending invitation has it as `replaces_token_id` |
| `replaced_by_owner` | Revoked by that invitation's claim |
| `linked_to_service_identity` | Live; an `api_token_moves` row names a service identity |
| `moved_to_service_identity` | Revoked, with a move row |
| `revoked` | Revoked, neither of the above |

- Invite the owner: `POST /people/{token.principal}/invitations` with `replaces_token_id`. The owner is the token's principal, never `minted_by`.
- Move to a service identity: `POST /tokens/{id}/move {"service_identity":"<name>"}` (security tier, `token.update`). An admin names the sponsor when creating the identity; nothing defaults it.
- The old token is revoked by `DELETE /tokens/{id}` once the job uses the new one. Nothing revokes it automatically ([D14](#11-owner-rulings-2026-10-10)).
- Service principals never appear in this inventory, in `GET /people`, or in `knownPrincipals` email pairing.

### 6.3 What keeps working

| Today | Proof |
|---|---|
| A legacy admin-minted token acts as its person until revoked | `TestPeople_LegacyMintedTokenActsAsTheNeverSignedInPerson` |
| Admin-token runs are operator-owned and read the operator namespace | `TestOwnerOnlyGrant_OperatorOwnedRunReadsTheOperatorRow`, `TestRunComponents_OperatorOwnedRunReadsTheOperatorNamespace` |
| The admin token still authenticates | `TestAPITokenAuth_AdminTokenStillWorks` |
| A CI principal that is an IdP user keeps its own token | `TestAPITokens_CreateListRevoke` |
| Local mode, with no sign-in, is unchanged; both new families answer `409` `needs_sso` there | `TestServiceIdentity_LocalModeRefusesAndRunsStillLaunch` (new) |
| The upgrade itself changes no token | `TestMigration0196_TokensAndRunsUnchanged` (new): seeded 0195 database, every `api_tokens` and `agent_runs` row byte-identical after |

## 7. Wire

### 7.1 Routes

| Route | Tier | Answer |
|---|---|---|
| `POST /people/{principal}/invitations` | `securityOps` | `201` invitation |
| `GET /invitations?state=`, `GET /people/{principal}/invitations` | `securityOps` | metadata, paged |
| `DELETE /invitations/{id}` | `securityOps` | `204` |
| `GET /me/invitations` | signed-in human | own pending |
| `POST /me/invitations/{id}/claim` | the target ([§3.4](#34-who-gets-what)) | `201` token |
| `POST /service-identities`, `GET /service-identities`, `GET`/`PATCH`/`DELETE /service-identities/{name}` | `securityOps` | record with derived state |
| `GET /me/service-identities` | signed-in human | identities they sponsor |
| `POST`/`GET /me/service-identities/{name}/tokens`, `DELETE …/tokens/{id}` | the sponsor; others `404` `service_identity_not_found` | `201` token once / list / `204` |
| `POST /tokens/{id}/move` | `securityOps` | `200` with `disposition` |

Every route joins `TestAuthzMatrix` in [`internal/api/authz_test.go`](../../../internal/api/authz_test.go).

### 7.2 Types, SDK, CLI, console

- Go: `types.Invitation`, `types.ServiceIdentity`, `types.ActorService`; the request bodies are the SDK's types, aliased as `mintEnrolmentTokenRequest` is.
- `pkg/client`: one method per route; `APIToken` gains `disposition`.
- CLI ([D15](#11-owner-rulings-2026-10-10)): `wardyn person invite <principal>`, `wardyn person invitations`, `wardyn invitation revoke <id>`, `wardyn service-identity create|list|get|update|retire`, `wardyn service-identity token create|list|revoke`, `wardyn token move <id> <name>`. No CLI claim: it needs a browser session.
- Console (U1, after a mock round under [CONSOLE-RULES.md](../CONSOLE-RULES.md)): a pending-invitation card on Account with the existing show-once dialog, and a `disposition` column on Admin > Credentials.

### 7.3 Reasons

`invitation_target_invalid`, `invitation_target_unknown`, `invitation_replaces_invalid`, `invitation_ttl_invalid`, `invitation_cap_reached`, `invitation_not_found`, `invitation_not_pending`, `service_identity_name_invalid`, `service_identity_name_taken`, `service_identity_sponsor_invalid`, `service_identity_not_found`, `service_identity_inactive`, `service_scope`, `token_move_invalid`, `needs_sso`. Each gets a row in [sdk.md](../../sdk.md), where `reason_docs_guard_test.go` checks it.

### 7.4 Audit actions

| Action | Actor, target | Data |
|---|---|---|
| `invitation.create` | admin; invitation id | `target`, `expires_at`, `replaces_token_id` |
| `invitation.revoke` | admin; invitation id | `target` |
| `invitation.claim` | the target; invitation id | `token_id`; `denied` rows carry `reason` |
| `token.create` | the claimer; token id | adds `invitation` |
| `token.revoke` | as today | adds `reason` `replaced_by_owner` |
| `token.update` | admin; token id | `disposition`, `service_identity` |
| `service_identity.create` | admin; `service:<name>` | `sponsor`, `user_type` |
| `service_identity.update` | admin; `service:<name>` | `sponsor_from`, `sponsor_to`, `state`, `tokens_revoked` |
| `service_identity.retire` | admin; `service:<name>` | `tokens_revoked` |
| `service_identity.token.create` | the sponsor; token id | `service_identity`, `expires_at` |
| `session.attach`, `ui.open`, `ssh.authenticate` | the sponsor, `human`; as today | adds `service_identity`, `entry` `sponsor` on a sponsor entry |

Every row a service's own request writes has `actor_type` `service` and `data.sponsor`. `auth.fail` gains reasons `service_identity_inactive` and `service_sponsor_inactive`.

## 8. Acceptance to tests

| Issue bullet | Test | Slice |
|---|---|---|
| Only the signed-in target can claim; another person or the admin cannot | `TestInvitation_ClaimOnlyByTheSignedInTarget` (every row of [§3.4](#34-who-gets-what)) | C1 |
| The credential is shown only to the claimer; the admin sees metadata | `TestInvitation_AdminSeesMetadataNeverTheToken`: lists, get, audit rows hold no `wdn_` | C1 |
| An unclaimed or expired invitation grants nothing | `TestInvitation_UnclaimedOrExpiredGrantsNothing`: its id as a bearer is `401`; no token row before claim | C1 |
| A service runs under its own principal; rows name it and its sponsor | `TestServiceIdentity_RunsUnderItsOwnPrincipal`: `CreatedBy`, `OperatorOwned` false, `Claims.Sponsor`, `actor_type`, `data.sponsor` | C2 |
| A delegation shows both parties and is revocable without ending sessions | `TestDelegation_HappyPathActsAsThePerson` (existing), `TestDelegation_PortalRevokeKeepsThePersonSignedIn` (new) | M2 |
| No route returns a credential that authenticates as a person other than the caller | `TestAPITokenMintSites_OnlyTheSelfServiceMint` (widened), `TestNoRouteReturnsAnotherPersonsCredential` | C1, M2 |
| Service and local runs keep working through migration | [§6.3](#63-what-keeps-working) | C1, C2 |
| No admin or super admin claims for someone else | `TestInvitation_ClaimOnlyByTheSignedInTarget` admin rows; `TestNoRouteReturnsAnotherPersonsCredential` | C1, M2 |

`TestNoRouteReturnsAnotherPersonsCredential` walks the `TestAuthzMatrix` route table as super admin, security admin and member. It presents every bearer-shaped string in a `2xx` body to `GET /me` and requires the caller or a `service:` principal.

Sponsor reach, all C4: `TestServiceIdentity_SponsorReadsAndKills`, `TestServiceIdentity_SponsorAttachIsOffByDefault`, `TestServiceIdentity_SponsorAttachFollowsTheGrant` (person, group, user-type deny, `*` allow), `TestServiceIdentity_SponsorAttachRecordsTheSponsor`, `TestServiceIdentity_SponsorEntryEndsWithSponsorship`.

Service-specific negatives, all C2: `TestServiceIdentity_NeverOperatorNeverSecondHuman`, `TestServiceIdentity_AllowListRouteWalk`, `TestServiceIdentity_PrefixReservedAtEveryDoor`, `TestServiceIdentity_SponsorInactiveRefusesItsTokens`, `TestServiceIdentity_SponsorChangeRevokesTokens`, `TestServiceIdentity_RateLimitedLikeAPerson`, `TestServiceIdentity_HiddenFromPeopleAndMintedInventory`.

## 9. Residuals and threat-model delta

> [!WARNING]
> **The sponsor holds the service's reach.** Whoever holds a service token acts with the service's grants and ceiling, which may be wider than the sponsor's own. Rows name the service and sponsor, not the hand on the token ([D10](#11-owner-rulings-2026-10-10)). A sponsor granted `service_attach` also enters the service's sandbox and its connections.

> [!WARNING]
> **A sponsor disabled only at the identity provider is not seen.** Without SCIM or a session revoke, Wardyn sees no deactivation. The service's tokens work until expiry, at most `90d`, and its live runs continue in every case.

> [!WARNING]
> **A service token is a bearer.** It is not bound to a workload or network. Bounds: the allow-list, member reach, the required expiry, the sponsor check and revoke. Workload federation is the upgrade path, not built.

> [!NOTE]
> **A stolen invitation link reaches nothing.** Claiming needs the target's own session, which is already enough to mint a token. A mistyped target leaves an invitation nobody can claim.

> [!NOTE]
> **Admin-minted tokens from before 0.8.5 still act as their person** until revoked or replaced. The dispositions make each one visible. Nothing retires them automatically in 0.9.

The docs lane places these as new numbered residuals. [`ARCHITECTURE.md`](../../../ARCHITECTURE.md) invariant 4 gains one bullet: a service identity is its own actor, and every row names its sponsor.

## 10. Slices

| Slice | Executor | Content | After |
|---|---|---|---|
| C1 | core, Sonnet high | Migration 0196; invitation store and routes; the shared mint core; claim; mint-site pin | owner decisions |
| C2 | core, Sonnet high | Service identity store, routes and auth branch; `ActorService`; allow-list; sponsor checks; run sponsor; actor-type call-site review | C1 |
| C4 | core, Sonnet high | Sponsor read and kill; the `service_attach` feature value and its 0196 restriction row; the sponsor arm of `mayEnterRun` at every entry door ([§4.9](#49-sponsor-reach-over-the-services-runs)) | C2 |
| C3 | core, Sonnet high | Dispositions; `POST /tokens/{id}/move`; sponsored-token sweep in `revokePersonCredentials`; directory exclusions | C1, C2 |
| U1 | Sonnet, console | Mock round, then the Account claim card and the disposition column | C1, C3 |
| M1 | free or Haiku | `pkg/client` methods, CLI verbs, TS mirror (the `service_attach` feature label included), wire-parity tests | C1–C4 |
| M2 | free or Haiku | `TestNoRouteReturnsAnotherPersonsCredential`, `TestDelegation_PortalRevokeKeepsThePersonSignedIn`, the caller-by-route probe table from §3.4, §4.4 and §4.9 | C1–C4 |
| M3 | free or Haiku | [api-tokens.md](../../operations/api-tokens.md), [CI.md](../../CI.md), [AUDIT-ACTIONS.md](../../AUDIT-ACTIONS.md), [sdk.md](../../sdk.md) rows; §9 text in REPORT | C1–C4 |

Every slice: each named test fails with its fix reverted and passes restored; the full tree with Postgres, lint, staticcheck, file size and lane preflight exit `0`.

### 10.1 DONE WHEN

| Slice | DONE WHEN |
|---|---|
| C1 | 0196 applies on a seeded 0195 database and `TestMigration0196_TokensAndRunsUnchanged` passes; a seeded `service:` principal aborts it |
| C1 | The widened `actor_type` check accepts `service`; the report times it on `12` seeded partitions of `100k` rows |
| C1 | The three C1 tests of [§8](#8-acceptance-to-tests) pass, each with a revert mutation; the mint-site pin names exactly two mint-core callers and no `MintedBy` write |
| C2 | `TestServiceIdentity_RunsUnderItsOwnPrincipal` and the seven negatives pass; `TestAuthzMatrix` covers every new route |
| C2 | The report lists every comparison against `types.ActorSystem` or `types.ActorHuman` with its verdict for `service` |
| C4 | `TestServiceIdentity_SponsorReadsAndKills` and the four attach tests pass, each with a revert mutation; with no grant row a sponsor's ticket mint answers `404` |
| C4 | Every `mayEnterRun` call site reaches the sponsor arm; the report lists them; an attach row names the sponsor as `human` actor with `service_identity` |
| C3 | `TestMintedTokenDispositions` drives a seeded admin-minted token to each of the six dispositions by its route |
| C3 | A sponsor's full session revoke shows their sponsored tokens revoked in `GET /tokens`; `GET /people` and `knownPrincipals` hold no `service:` principal |
| U1 | Mock approved; the claim shows the token once; a Playwright spec for the claim and the disposition column passes |
| M1 | `TestNewSpellingsAreReachable` covers every new verb; each client method has a request-shape test; TS types match the Go JSON |
| M2 | The probe table runs every route against target, other person, sponsor with and without `service_attach`, admin, super admin, admin token, `wdn_`, `wdg_`, service, expired and claimed; every cell matches |
| M3 | `make lint` exits `0`; guarded docs keep every citation; the report carries CHANGELOG lines and the §9 text |

## 11. Owner rulings (2026-10-10)

The owner accepted every recommendation except D11, which is amended. The design above already follows each ruling.

| # | Ruling |
|---|---|
| D1 | No secret code; the target's signed-in session claims |
| D2 | Invitation lifetime: default `72h`, maximum `14d` |
| D3 | Claiming never grants the `api_token` feature |
| D4 | Only the sponsor mints service tokens |
| D5 | Service-token expiry required: at most `90d`, or the deployment cap if lower |
| D6 | Sponsor suspended or deactivated: service tokens refused at once; live runs continue |
| D7 | A sponsor change revokes every token of the identity |
| D8 | New `service` audit actor type. Fallback to `system` with actor `service:<name>` only if C1 measures the constraint migration as too slow |
| D9 | Fixed route allow-list; never an operator |
| D10 | The service's ceiling is not bounded by the sponsor's; a published residual |
| D11 | **Amended.** The sponsor may read and kill the service's runs. Attach is optional and governed: off by default, enabled per person or group ([§4.9](#49-sponsor-reach-over-the-services-runs)) |
| D12 | No new delegation grant in 0.9 |
| D13 | The service stores its own model credential in its own namespace |
| D14 | Admin-minted tokens kept with a visible state; refusal decided for 1.0 |
| D15 | Names as in [§7](#7-wire); audit verb `claim`; console noun "Service identities" |
| D16 | Console: the claim card and the token-state column after a mock round; service identities by API and CLI only in 0.9 |
| D17 | Create, suspend and retire on the security tier, as `POST /people` |
