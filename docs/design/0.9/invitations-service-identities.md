# 0.9 owner-redeemed invitations and service identities (#1508)

Status: **Draft for owner approval.** Build slices start only after [Owner decisions](#11-owner-decisions) are answered; the design assumes each recommended option.
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
| Invitation reuses the hashed single-use token shape | No secret: the target's own session is the only authority ([OD-1](#11-owner-decisions)) | A claim needs the target signed in anyway, so a code adds a thing to steal and protects nothing |
| Redeem route | `POST /me/invitations/{id}/claim`, audit `invitation.claim` | `claim` is already on the closed verb list ([Grammar](../../AUDIT-ACTIONS.md#grammar)); `redeem` would extend it |
| C1–C3 core, M1–M3 mechanical | Adds U1, a console slice | A claim needs a browser session: the CLI has no sign-in, and an API token may not mint |
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
| `expires_at` | Default `72h` after create, at most `14d` ([OD-2](#11-owner-decisions)) |
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
- `MintedBy` stays empty: the owner minted it. A claim does not grant the `api_token` feature ([OD-3](#11-owner-decisions)).

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
| `PUT`, `DELETE /model-providers/{id}/credential` | Its own model key, in its own namespace ([OD-13](#11-owner-decisions)) |

- Everything else answers `403` `service_scope`, including any route added later ([OD-9](#11-owner-decisions)).
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
| Sponsor change | Every live token of the identity is revoked in the same transaction ([OD-7](#11-owner-decisions)) | `PATCH /service-identities/{name}` |
| Sponsor's full session revoke | Tokens minted before the cutoff stop at once; the sweep also marks them revoked | `revokePersonCredentials` |
| Sponsor suspended by SCIM | State `sponsor_inactive`; tokens refused; live runs continue ([OD-6](#11-owner-decisions)) | `CheckSession` deactivated arm |
| Sponsor's session-only cut | No effect, as for the sponsor's own tokens | epoch `-1` |
| Admin suspend or retire | Tokens refused; retire also revokes them; live runs continue until killed | `PATCH`, `DELETE /service-identities/{name}` |

- Only the current sponsor mints, from a signed-in session that is not a token, a portal or member mode ([OD-4](#11-owner-decisions)).
- Because a sponsor change revokes, every live token was minted by the current sponsor, so checking the sponsor alone is exact.
- Service tokens require an expiry: default and maximum `90d`, or the deployment cap if lower ([OD-5](#11-owner-decisions)).

### 4.7 Runs

- `CreatedBy` is `service:<name>`; `OperatorOwned` is false; `MintRunIdentity` gets the sponsor as `sponsor`.
- Dispatch and revive re-mint read the sponsor from the identity row, not `run.CreatedBy`.
- The console shows the service as owner and the sponsor beside it.
- The sponsor may read and kill the service's runs, never attach ([OD-11](#11-owner-decisions)); admins keep today's reach.

### 4.8 Audit actor

- New `actor_type` value `service`, with `actor` `service:<name>` and `data.sponsor` on every row ([OD-8](#11-owner-decisions)).
- Migration 0196 widens the check on `audit_events` and `audit_events_legacy`. C1 measures the validation cost on a seeded partition set.
- Sealing is unchanged: `Sealer.sealsActor` seals human actors only, and a service name is not personal data.

## 5. Owner-authorised delegation

- No new mechanism in 0.9 ([OD-12](#11-owner-decisions)).
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
- The old token is revoked by `DELETE /tokens/{id}` once the job uses the new one. Nothing revokes it automatically ([OD-14](#11-owner-decisions)).
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
- CLI ([OD-15](#11-owner-decisions)): `wardyn person invite <principal>`, `wardyn person invitations`, `wardyn invitation revoke <id>`, `wardyn service-identity create|list|get|update|retire`, `wardyn service-identity token create|list|revoke`, `wardyn token move <id> <name>`. No CLI claim: it needs a browser session.
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

Service-specific negatives, all C2: `TestServiceIdentity_NeverOperatorNeverSecondHuman`, `TestServiceIdentity_AllowListRouteWalk`, `TestServiceIdentity_PrefixReservedAtEveryDoor`, `TestServiceIdentity_SponsorInactiveRefusesItsTokens`, `TestServiceIdentity_SponsorChangeRevokesTokens`, `TestServiceIdentity_RateLimitedLikeAPerson`, `TestServiceIdentity_HiddenFromPeopleAndMintedInventory`.

## 9. Residuals and threat-model delta

> [!WARNING]
> **The sponsor holds the service's reach.** Whoever holds a service token acts with the service's grants and ceiling, which may be wider than the sponsor's own. Rows name the service and sponsor, not the hand on the token ([OD-10](#11-owner-decisions)).

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
| C3 | core, Sonnet high | Dispositions; `POST /tokens/{id}/move`; sponsored-token sweep in `revokePersonCredentials`; directory exclusions | C1, C2 |
| U1 | Sonnet, console | Mock round, then the Account claim card and the disposition column | C1, C3 |
| M1 | free or Haiku | `pkg/client` methods, CLI verbs, TS mirror, wire-parity tests | C1–C3 |
| M2 | free or Haiku | `TestNoRouteReturnsAnotherPersonsCredential`, `TestDelegation_PortalRevokeKeepsThePersonSignedIn`, the caller-by-route probe table from §3.4 and §4.4 | C1–C3 |
| M3 | free or Haiku | [api-tokens.md](../../operations/api-tokens.md), [CI.md](../../CI.md), [AUDIT-ACTIONS.md](../../AUDIT-ACTIONS.md), [sdk.md](../../sdk.md) rows; §9 text in REPORT | C1–C3 |

Every slice: each named test fails with its fix reverted and passes restored; the full tree with Postgres, lint, staticcheck, file size and lane preflight exit `0`.

### 10.1 DONE WHEN

| Slice | DONE WHEN |
|---|---|
| C1 | 0196 applies on a seeded 0195 database and `TestMigration0196_TokensAndRunsUnchanged` passes; a seeded `service:` principal aborts it |
| C1 | The widened `actor_type` check accepts `service`; the report times it on `12` seeded partitions of `100k` rows |
| C1 | The three C1 tests of [§8](#8-acceptance-to-tests) pass, each with a revert mutation; the mint-site pin names exactly two mint-core callers and no `MintedBy` write |
| C2 | `TestServiceIdentity_RunsUnderItsOwnPrincipal` and the seven negatives pass; `TestAuthzMatrix` covers every new route |
| C2 | The report lists every comparison against `types.ActorSystem` or `types.ActorHuman` with its verdict for `service` |
| C3 | `TestMintedTokenDispositions` drives a seeded admin-minted token to each of the six dispositions by its route |
| C3 | A sponsor's full session revoke shows their sponsored tokens revoked in `GET /tokens`; `GET /people` and `knownPrincipals` hold no `service:` principal |
| U1 | Mock approved; the claim shows the token once; a Playwright spec for the claim and the disposition column passes |
| M1 | `TestNewSpellingsAreReachable` covers every new verb; each client method has a request-shape test; TS types match the Go JSON |
| M2 | The probe table runs every route against target, other person, admin, super admin, admin token, `wdn_`, `wdg_`, service, expired and claimed; every cell matches |
| M3 | `make lint` exits `0`; guarded docs keep every citation; the report carries CHANGELOG lines and the §9 text |

## 11. Owner decisions

1. **Does an invitation carry a secret?** A: no, the target's session is the only authority. B: a hashed single-use code in the link, plus the session.
   Recommendation: **A**. B adds a credential to steal and protects nothing a session does not.
2. **Invitation lifetime.** A: default `72h`, maximum `14d`. B: default `7d`, maximum `30d`. C: fixed `72h`, like device enrolment.
   Recommendation: **A**.
3. **Does a claim grant the `api_token` feature?** A: no, a claim obeys the self-mint's capability check. B: yes, for that one token.
   Recommendation: **A**. An admin who wants the feature grants it.
4. **Who mints a service identity's tokens?** A: the sponsor only, from a signed-in session. B: the sponsor or the security tier. C: the security tier only.
   Recommendation: **A**. The person accountable is the person holding the plaintext.
5. **Service token lifetime.** A: expiry required, default and maximum `90d`, or the deployment cap if lower. B: the person-token rules. C: expiry required, maximum `30d`.
   Recommendation: **A**.
6. **The sponsor is suspended or deactivated.** A: tokens refused at once, live runs continue. B: A, and kill the service's live runs. C: nothing until an admin acts.
   Recommendation: **A**. Killing CI mid-job on an HR event surprises; an admin can still kill.
7. **The sponsor changes.** A: revoke every live token. B: tokens survive.
   Recommendation: **A**. The old sponsor may still hold a plaintext.
8. **Audit actor form.** A: new `actor_type` `service`, widening the audit check in 0196. B: `actor_type` `system` and actor `service:<name>`, as devices are.
   Recommendation: **A**, unless C1 measures the check rewrite as too slow for large audit tables; then **B**.
9. **A service identity's reach.** A: the allow-list of [§4.4](#44-reach), member reach, never an operator. B: every member route.
   Recommendation: **A**. A route added later stays closed until listed.
10. **Is the service's ceiling bounded by its sponsor's?** A: no, it is assigned like any user subject, and the gap is a residual. B: resolve the intersection with the sponsor's ceiling.
    Recommendation: **A** for 0.9. B changes reach whenever the sponsor's groups change.
11. **The sponsor's reach over the service's runs.** A: none beyond today. B: read and kill, never attach. C: read, kill and attach.
    Recommendation: **B**. The sponsor holds the token, so B adds no reach.
12. **A new owner-authorised delegation grant in 0.9?** A: no, portal delegation and service identities cover the issue. B: a per-person grant letting a service act for an absent person.
    Recommendation: **A**.
13. **How a service identity gets its model credential.** A: its own token stores it in its own namespace. B: the sponsor stores it through a new route. C: the operator namespace only.
    Recommendation: **A**. It matches today's CI provisioning.
14. **Admin-minted tokens after 0.9.** A: no automatic revoke; dispositions and a boot warning with the `needs_decision` count; refusal decided for 1.0. B: refuse them at the 0.9 upgrade. C: refuse them a fixed time after upgrade.
    Recommendation: **A**. Each token moves only when a person acts.
15. **Names.** A: CLI and routes as in [§7](#7-wire), audit verb `claim`, console noun "Service identities". B: `redeem` added to the closed verb list, and "Automation accounts".
    Recommendation: **A**.
16. **Console in 0.9.** A: the claim card and disposition column after a mock round; service identities by API and CLI only. B: A plus a service-identity page. C: none, leaving invitations unclaimable.
    Recommendation: **A**.
17. **Who creates, suspends and retires service identities.** A: the security tier, as `POST /people`. B: super admin only, as portal registration.
    Recommendation: **A**. An identity reaches nothing beyond member reach under governance.
