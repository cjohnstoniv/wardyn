# 0.9 owner-redeemed invitations and service identities (#1508)

Status: **Owner rulings recorded 2026-10-10**, amended after review r1 ([§11](#11-owner-rulings-2026-10-10)). The service-identity mechanism is on its own page, [service-identities.md](service-identities.md).
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
| The target without the `api_token` feature | `403` `capability_feature` | `denyUserCapability`, as the self-mint |
| A service identity's token | `403` `service_scope` | Not on its allow-list ([reach](service-identities.md#4-reach)) |
| The target in member mode | `409` `api_token_member_mode_mint` | As the self-mint |

Admin reads (`GET /invitations`, `GET /people/{principal}/invitations`) return the record and `token_id`, never `token`. `DELETE /invitations/{id}` revokes a pending one (`invitation.revoke`); otherwise `404`.

## 4. Service identities

The mechanism is on [service-identities.md](service-identities.md). In short:

- A reserved principal `service:<name>` with a required sponsor, a person bound in `principal_identities`.
- Ordinary `api_tokens` rows, checked on every request against the live identity, `sponsor_since` and the sponsor's session state.
- A fixed route allow-list at member reach, never an operator, and a `service` audit actor with the sponsor stamped and sealed.
- The sponsor mints from a console card, reads and kills the service's runs, and attaches only with an explicit `service_attach` grant, never over SSH.

## 5. Owner-authorised delegation

- No new mechanism in 0.9 ([D12](#11-owner-rulings-2026-10-10)).
- Acting for a present person is the portal lane: the person is the actor and `data.via` names portal and grant. Revoking the portal leaves the person's sessions alone.
- Unattended work is a service identity acting as itself, so no grant lets anything act as an absent person.
- The missing proof is one test, `TestDelegation_PortalRevokeKeepsThePersonSignedIn` ([§8](#8-acceptance-to-tests)).

## 6. Migration

### 6.1 Schema 0196

- One file, `0196_invitations_service_identities.sql`, provisional and renumbered at merge.
- `person_invitations` ([§3.1](#31-record)) and `service_identities` ([record](service-identities.md#2-record)).
- `api_token_moves (token_id PK → api_tokens, service_identity → service_identities, linked_by, linked_at)`.
- The `actor_type` check on the partitioned `audit_events` parent widened to add `service`.
- The `service:` collision check, in any case, which aborts naming the table. It reads `principal_identities`, `people`, `api_tokens`, `ssh_keys`, `agent_runs.created_by`, `workspaces.owned_by`, capability grants, governance assignments and secret owners.
- `POST /service-identities` asks the same tables for a new name, so a grant written early is never inherited.
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
- CLI ([D15](#11-owner-rulings-2026-10-10)): `wardyn person invite <principal>`, `wardyn person invitations`, `wardyn invitation revoke <id>`, `wardyn service-identity create|list|get|update|retire`, `wardyn service-identity token list|revoke`, `wardyn token move <id> <name>`. No CLI claim and no CLI service-token mint: both need a browser session (D16).
- Console (U1, after a mock round under [CONSOLE-RULES.md](../CONSOLE-RULES.md)): on Account, a pending-invitation card and a "Service identities I sponsor" mint card, both with the existing show-once dialog; a `disposition` column on Admin > Credentials.

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
| `service_identity.retire` | admin; `service:<name>` | `tokens_revoked`; a `credential.erase` row for the identity's namespace beside it |
| `service_identity.token.create` | the sponsor; token id | `service_identity`, `expires_at` |
| `session.attach`, `ui.open` | the sponsor, `human`; as today | adds `service_identity`, `entry` `sponsor` on a sponsor entry |

Every row a service's own request writes has `actor_type` `service` and `data.sponsor`, sealed under `WARDYN_AUDIT_SEAL=full` ([audit actor](service-identities.md#9-audit-actor)). `auth.fail` gains reasons `service_identity_inactive` and `service_sponsor_inactive`.

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

`TestNoRouteReturnsAnotherPersonsCredential` walks the `TestAuthzMatrix` route table as super admin, security admin and member. Bearer-shaped means a string starting `wdn_`, `wdg_`, `wdp_`, `wdr_`, `wdd_` or `wde_`, an attach `ticket`, or a `Set-Cookie` value. Each one found in a `2xx` is presented to its door, and the identity it yields must be the caller or a `service:` principal.

Sponsor reach, all C4: `TestServiceIdentity_SponsorReadsAndKillsOnly` (files, approvals and run tokens refused), `TestServiceIdentity_SponsorAttachIsOffByDefault` (super admin sponsor included), `TestServiceIdentity_SponsorAttachFollowsTheGrant` (person, group, user-type deny, `*` allow), `TestServiceIdentity_SponsorAttachKeepsTheDoors` (super admin sponsor, `deny_interactive`, `WARDYN_GOVERN_ADMIN_RUNS` off: refused), `TestServiceIdentity_SponsorAttachRecordsTheSponsor`, `TestServiceIdentity_SponsorEntryEndsWithSponsorship`, `TestServiceIdentity_SSHRefusesSponsorEntry`.

Service-specific negatives, all C2: `TestServiceIdentity_NeverOperatorNeverSecondHuman`, `TestServiceIdentity_AllowListRouteWalk`, `TestServiceIdentity_PrefixReservedAtEveryDoor`, `TestServiceIdentity_SponsorInactiveRefusesItsTokens`, `TestServiceIdentity_SponsorChangeRevokesTokens` (with a mint committing after the revoke's snapshot), `TestServiceIdentity_EntraSponsorSuspensionIsSeen`, `TestIdentityUnbind_RefusesASponsorOrInvitee`, `TestServiceIdentity_RetireErasesItsCredentials`, `TestServiceIdentity_SponsorFieldsSealed`, `TestServiceIdentity_RateLimitedLikeAPerson`, `TestServiceIdentity_HiddenFromPeopleAndMintedInventory`.

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
| C4 | core, Sonnet high | `ownsRunOrSponsorOrAdmin` on the read and kill routes only; the `service_attach` value and its 0196 restriction row; the sponsor arm of `mayEnterRun`; the sponsor ticket stamp ([sponsor reach](service-identities.md#8-sponsor-reach-over-the-services-runs)) | C2 |
| C3 | core, Sonnet high | Dispositions; `POST /tokens/{id}/move`; sponsored-token sweep in `revokePersonCredentials`; directory exclusions | C1, C2 |
| U1 | Sonnet, console | One mock round, then the Account claim card, the sponsor's mint card and the disposition column | C1–C3 |
| M1 | free or Haiku | `pkg/client` methods, CLI verbs, TS mirror (the `service_attach` feature label included), wire-parity tests | C1–C4 |
| M2 | free or Haiku | `TestNoRouteReturnsAnotherPersonsCredential`, `TestDelegation_PortalRevokeKeepsThePersonSignedIn`, the caller-by-route probe table from [§3.4](#34-who-gets-what) and the service page's §4, §6 and §8 | C1–C4 |
| M3 | free or Haiku | [api-tokens.md](../../operations/api-tokens.md), [CI.md](../../CI.md), [AUDIT-ACTIONS.md](../../AUDIT-ACTIONS.md), [sdk.md](../../sdk.md) rows; §9 text in REPORT | C1–C4 |

Every slice: each named test fails with its fix reverted and passes restored; the full tree with Postgres, lint, staticcheck, file size and lane preflight exit `0`.

### 10.1 DONE WHEN

| Slice | DONE WHEN |
|---|---|
| C1 | 0196 applies on a seeded 0195 database and `TestMigration0196_TokensAndRunsUnchanged` passes; a seeded `service:` principal aborts it |
| C1 | The widened `actor_type` check accepts `service`; the report gives the lock mode it takes and its time on `12` seeded partitions of `100k` rows |
| C1 | The three C1 tests of [§8](#8-acceptance-to-tests) pass, each with a revert mutation; the mint-site pin names exactly two mint-core callers and no `MintedBy` write |
| C2 | `TestServiceIdentity_RunsUnderItsOwnPrincipal` and the C2 negatives pass, the first two under both actor forms; `TestAuthzMatrix` covers every new route |
| C2 | The report lists every comparison against `types.ActorSystem` or `types.ActorHuman` with its verdict for `service` |
| C4 | The seven C4 tests pass, each with a revert mutation; with no grant row a sponsor's ticket mint answers `404`, a super admin sponsor's too |
| C4 | `ownsRunOrSponsorOrAdmin` gates exactly the five read and kill routes; every `mayEnterRun` call site is listed; an attach row names the sponsor as `human` actor with `service_identity` |
| C4 | `sshAuth` refuses a sponsor entry with `run_owner_only` whatever grant the sponsor holds; `TestServiceIdentity_SSHRefusesSponsorEntry` proves it |
| C3 | `TestMintedTokenDispositions` drives a seeded admin-minted token to each of the six dispositions by its route |
| C3 | A sponsor's full session revoke shows their sponsored tokens revoked in `GET /tokens`; `GET /people` and `knownPrincipals` hold no `service:` principal |
| U1 | Mock approved; the claim and the sponsor's mint each show the token once; a Playwright spec for both cards and the disposition column passes |
| M1 | `TestNewSpellingsAreReachable` covers every new verb; each client method has a request-shape test; TS types match the Go JSON |
| M2 | The probe table runs every route against target, other person, sponsor with and without `service_attach`, admin, super admin, admin token, `wdn_`, `wdg_`, service, expired and claimed; every cell matches |
| M3 | `make lint` exits `0`; guarded docs keep every citation; the report carries CHANGELOG lines and the §9 text |

## 11. Owner rulings (2026-10-10)

The owner accepted every recommendation except D11, amended, and re-ruled four review findings, amending D16. The design follows each ruling.

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
| D11 | **Amended.** The sponsor may read and kill the service's runs, nothing more. Attach is optional and governed: off by default, enabled per person or group, never over SSH, same rules for super admins ([sponsor reach](service-identities.md#8-sponsor-reach-over-the-services-runs)) |
| D12 | No new delegation grant in 0.9 |
| D13 | The service stores its own model credential in its own namespace |
| D14 | Admin-minted tokens kept with a visible state; refusal decided for 1.0 |
| D15 | Names as in [§7](#7-wire); audit verb `claim`; console noun "Service identities" |
| D16 | **Amended.** Console: the claim card, the sponsor's service-token mint card and the token-state column, in one mock round. Create, suspend and retire stay API and CLI only; the CLI has no token mint |
| D17 | Create, suspend and retire on the security tier, as `POST /people` |
