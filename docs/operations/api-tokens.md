> Part of the [Operations](../OPERATIONS.md) split (task pages under `docs/operations/`).

# Per-user API tokens

Stop sharing the admin token: a token per person, what it may reach, the stamp that keeps
it honest, tokens for a person who never signs in, and the portals that act for a group.

## Per-user API tokens: stop sharing the admin token

- `WARDYN_ADMIN_TOKEN` is one string, deployment-wide admin, attributable to nobody.
- A **per-user API token** is the replacement: a human mints one for their own automation, it carries *their* identity and *their* role, and it is revocable on its own.

| Call | Who | What |
|---|---|---|
| `POST /api/v1/me/tokens` | any signed-in human | mint one for yourself — the response is the **only** time the plaintext exists |
| `GET /api/v1/me/tokens` | any signed-in human | your own tokens, revoked ones included |
| `DELETE /api/v1/me/tokens/{id}` | any signed-in human | revoke one of your own |
| `GET /api/v1/tokens` | admin or `security_admin` | every token in the deployment |
| `DELETE /api/v1/tokens/{id}` | admin or `security_admin` | revoke anyone's |

- Revoking a human (`POST /api/v1/sessions/revoke`, `wardyn session revoke`) also revokes their API tokens, removes their registered SSH keys, and refuses their UI-app sessions at the next re-check, whether the revoke names the subject or the email.
  - The `all` arm applies all three actions deployment-wide, including the calling admin's own credentials.
  - Plan to re-mint tokens and register SSH keys again after a global revoke.
- To end only a person's browser sessions, send `"sessions_only": true` with `sub`: a session-only cut is stamped and nothing else changes.
  - Their browser sessions end on every instance; their API tokens, SSH keys and portal-delegated tokens keep working, and an API token can still approve a governance change.
  - No token-borne credential is refused by the cut; an event stream a token opened before the cut closes at its next keepalive and reconnects.
  - It is refused (`sessions_revoke_param_invalid`) with `all`.
  - The audit row is the usual `session.revoke`, with `sessions_only` set and both counts `0`.
  - The People drawer's "Sign out everywhere" sends it.
- **No `role` parameter on `POST /me/tokens`.**
  - A token always mints at the caller's own current role; there is no deliberately-downgraded mint.
  - Still open at 0.8.
- **Deleting one API token leaves its registered SSH keys in place.**
  - Use session revocation to remove the person's tokens and keys together, or `DELETE /api/v1/people/{principal}/ssh-keys` (admin or `security_admin`) to remove only their keys and receive `{"count": N}`.
  - A deleted key cannot authenticate again or open a new channel on an established SSH connection.
  - Existing channels continue until they close or their run is torn down.
  - Follow [SSH access revocation](../SSH.md#revoking-access-during-an-incident) for the full offboarding sequence, including stored credentials and affected-run teardown.
- **Name them by either identity.**
  - `--sub` takes the OIDC `sub` **or** the email.
  - The session cutoff and token sweep match an exact subject or a case-insensitive email.
  - SSH-key removal resolves the stored principal, giving an exact known subject precedence over an email alias; an ambiguous name or unresolved email cannot be reported as completed key removal.
  - Session revocation also stamps the resolved subject's cutoff, because SSH keys carry a subject without an email.
  - SSH registration and access check the cutoff so a registration in flight cannot outlive the revoke.
- A complete revoke answers `204`.
  - A `500` may follow a successful session cutoff if token revocation, SSH-principal resolution, canonical-subject cutoff or key deletion then fails.
  - Both credential operations are attempted, and the `session.revoke` audit records `tokens_revoked`, `ssh_keys_deleted` and outcome `failure` for partial work.
  - Resolve the reported failure and retry; each count describes that call only.

> [!NOTE]
> Sessions remain stateless signed cookies, so the audit cannot count active browser sessions or prove that a person has no already-open SSH channels.

- Use one as an ordinary bearer: `Authorization: Bearer wdn_…`.
  - Downstream it is indistinguishable from that human's console session — run ownership, the admin/member gate and capability grants all resolve to the owning human.
  - So a member's token reaches exactly the routes their session reaches, and no more.
  - A token is **never** the admin identity: minting one requires a verified SSO human, so neither the admin token nor local mode can mint one, and a token cannot mint a second API token.
- Only `hex(sha256(token))` is stored, so a lost token is re-minted, never recovered, and a database reader (a reporting role, a hot standby, a `pg_dump` in a backup bucket) cannot lift a usable credential off a row.
  - `last_used_at` is best effort and is the signal for "which of these are dead"; revoke those.
- **Both halves are stamps re-checked at login.**
  - A token carries the role AND the group snapshot its owner held when they minted it, and every request it authenticates republishes them.
  - So downstream it is that human as they were at mint time, or at their most recent sign-in since — whichever is later.
  - Their next successful sign-in **re-stamps the role, the group snapshot, and the snapshot's own completeness bit** on every unrevoked token they hold.
  - This is the same `OnLogin` hook that has re-stamped their SSH keys since 0.6, now widened to carry groups too.
  - So a demotion, or a group membership change, reaches outstanding tokens at that human's own next login rather than immediately.
  - A token's stamp is timed: `api_tokens.identity_stamped_at` is set at mint and again at every one of those re-stamps.
  - By default nothing ages a stamp out on its own, so a human who is demoted in the identity provider and never signs in again keeps the role and groups their tokens were minted with.
  - Set `WARDYN_ROLE_STAMP_TTL` (default off) to bound that.
  - A token whose stamp is older than the TTL is refused with `401` and the reason `role_stamp_stale` (an `authz.denied` audit row) until its owner signs in again, which re-stamps it with their current role.
  - A console session older than the TTL is sent back through sign-in.
  - So a stamp you can tune is also the age of the longest role a session can carry.
  - (With `WARDYN_OIDC_SESSION_TTL` unset a session already ends at the ID token's own expiry, and with it set at that TTL after sign-in, so the role-stamp TTL only matters when it is shorter than the session.)
  - The ID token's expiry is today the only identity-provider-driven bound on a console session, and `WARDYN_ROLE_STAMP_TTL` is off by default.
  - So a session lengthened with `WARDYN_OIDC_SESSION_TTL` (at most `24h`) should be paired with a role-stamp TTL, or a person disabled only at the identity provider keeps the console until the session TTL runs out.
  - Wardyn keeps no identity-provider token and cannot re-derive a role on a timer; re-login is the only refresh.
  - Turning the TTL on asks every token holder to sign in once, because the backfill dated existing stamps at mint.
  - A login never revives a revoked token, and a stamp is one statement, so a failed re-stamp leaves that person's tokens stale, never half-updated.
  - What ends a token is its **expiry** (below) or **explicit revocation**; neither waits for that next login, and neither needs the TTL.
- **A token can expire.**
  - `api_tokens.expires_at` is nullable: NULL means the token never expires, which is every token minted before 0.8.6.
  - `POST /me/tokens` takes an optional `ttl_seconds`.
  - Omitted or zero gets `WARDYN_API_TOKEN_MAX_TTL` when you have set one (see [ENV.md](../ENV.md)) and no expiry when you have not.
  - A value above that cap is clamped to it, and the `token.create` audit row records the clamp (`ttl_clamped_from_seconds`); a negative value is a `400` and mints nothing.
  - The console's mint form sends no TTL, so on a deployment with a cap it gets the cap.
  - An expired token is refused as a revoked one is, the same `401` with the same body, so expiry is no way to learn that a token once existed.
  - Both lists show `expires_at`.
  - The cap applies to tokens minted after it is set; it never shortens one already minted, so an operator who wants those gone revokes them.
  - There is no default cap: turning one on is what starts failing CI tokens on its own schedule.
  - Console sessions are unaffected and keep their own cookie expiry.
- A demotion made on the People page is now one of those explicit revocations.
  - When a role-mapping write or delete takes a tier away from a value, Wardyn revokes the outstanding tokens of every principal whose own derivation that edit demotes and whose stamp still carries what was lost.
  - It reports the number as `tokens_revoked` in the response and the audit row.
  - It is scoped to that demotion — a promotion, an unrelated value, and a member-stamped credential naming the same group are all left alone.
  - A token whose group snapshot is missing or partial cannot be re-derived, so an elevated stamp in that state is revoked rather than assumed safe.
- **A token carries its holder's user type too** (`api_tokens.user_type`, stamped at mint and re-stamped with the role at the next sign-in), and a type change made on the People page revokes rather than waits.
  - When a role-mapping write or delete changes the user type a value derives, Wardyn revokes every live token:
    - still carrying the old type that names the value (by principal, email or group), or
    - whose group snapshot is missing or partial.
  - It counts them in the same `tokens_revoked`.
  - On the first type assignment to a value that derived Standard user before, that last arm is every Standard-user token whose snapshot is missing or partial.
  - That is every token minted before 0.7 whose holder has not signed in since, and every truncated-snapshot token, whether or not its holder has anything to do with the value.
  - `stale_token_snapshots` counts only the tokens that name the value, so `tokens_revoked` can exceed it.
  - The holder mints a new token after signing in.
  - A type change made in `WARDYN_OIDC_ROLE_MAP` has no People-page edit to act on, so it reaches a token only at its holder's next sign-in — revoke explicitly when that is too late.
  - A user type a live token still carries cannot be deleted (`409`, naming the count).
  - The type arm only compares the edited value's own before/after type against a token's stamp.
  - So consider a holder whose effective type shifts because a different, higher-priority group is the one actually edited — or because the edited value's own prior derivation was empty rather than `standard`.
  - That holder keeps a stale stamp until that holder's next sign-in or an explicit revoke, the same as a `WARDYN_OIDC_ROLE_MAP` edit above.
- That matters most for the tier 0.7 added.
  - A human demoted out of `security_admin` keeps, through any token they minted while they held it, exactly what the tier governs:
    - profile authoring and assignment, capability-grant writes, session and token revocation, escalated approval decisions on anyone's run, workspace `approved-egress`/`denied-egress` writes, and audit-chain verify.
  - What it does not gain is anything the tier itself never had — a token reaches no shell, no attach ticket on a foreign run, and no capability grant widens it to admin.

**So revoke it, and check that you named the right person.**

```sh
# Everything live in the deployment, with owner, name and last_used_at:
curl -H "Authorization: Bearer $TOKEN" $WARDYN/api/v1/tokens

# One token:
curl -X DELETE -H "Authorization: Bearer $TOKEN" $WARDYN/api/v1/tokens/<id>

# A whole human — sessions AND every unrevoked token they hold, in one call.
# "sub" takes EITHER identity: the OIDC subject or the email. Use the one you
# actually know; on an IdP whose sub is an opaque per-app id (Entra), that is
# the email.
curl -X POST -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' \
  -d '{"sub":"alice@corp.com"}' $WARDYN/api/v1/sessions/revoke

# Only her browser sessions; her API tokens and SSH keys keep working:
curl -X POST -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' \
  -d '{"sub":"alice@corp.com","sessions_only":true}' $WARDYN/api/v1/sessions/revoke
```

- A revoked token is never re-stamped — it keeps whatever role it carried when it was revoked, so the trail still says what that credential actually was.
- The `session.revoke` audit row carries `tokens_revoked`.
  - That count is the receipt: a **zero** against a human you believe holds tokens means the identifier matched nobody, not that there was nothing to revoke.
  - Sessions are stateless, so that half cannot be counted, and only this half can tell you.
- Both `token.create` and `token.revoke` are audited ([`docs/AUDIT-ACTIONS.md`](../AUDIT-ACTIONS.md)); the revoke row names the token's owner.

> [!IMPORTANT]
> Offboarding a person means revoking their tokens explicitly.
> A demoted or departed human who never signs in again is not caught by the login-time re-stamp, and the row outlives their access to your IdP either way.
> It is published as a residual ([`threatmodel/THREAT-MODEL.md`](../../threatmodel/THREAT-MODEL.md) §5, "A per-user API token's role AND group snapshot are bounded-stale, not frozen").
## Tokens for a person who never signs in

- **No one can create a token that acts as another person (0.8.5).**
  - An admin or `security_admin` can still set a person up before their first sign-in, but cannot mint a token for them.
  - `POST /api/v1/people/{principal}/tokens` answers every caller `403` with reason `person_token_mint_removed` ("No one can create a token that acts as another person. They sign in and create their own."), whether or not the person exists.
  - No role, flag or environment variable turns it back on.
  - A token an admin created for someone else acted as that person while the admin held its plaintext, which is the reach this closes.
- For people who never open the console, the interim path is that the person signs in once and creates their own token (`POST /api/v1/me/tokens`, or the console; see [CI.md](../CI.md)).
  - It is never the deployment's admin token.
  - A trusted front-end that acts for people who ARE signed in to it uses [delegation](#delegated-run-management-portals) instead: it never holds a long-lived token for anyone.

| Call | What |
|---|---|
| `POST /api/v1/people` `{"principal":"<sub>","email":"<email>"}` | create the person, or confirm the one already there (`201` / `200`) |
| `POST /api/v1/people` `{"tenant_id":"<tid>","object_id":"<oid>","email":"<email>"}` | Entra ID only: the same, keyed by the tenant and object id (see below); the principal is `entra:<tid>:<oid>` |
| `POST /api/v1/people/{principal}/tokens` | refused: `403` `person_token_mint_removed`, one `person.token.create` audit row with outcome `denied` |
| `GET /api/v1/people/{principal}/tokens` | that person's tokens, revoked ones included |
| `GET /api/v1/tokens?minted_for_others=true` | every live token an admin created for someone else before this change (metadata only, never a plaintext); also on the console's Admin > Credentials page |

- **Tokens already minted keep working until you revoke them.**
  - Nothing is revoked by the upgrade.
  - List them with `GET /api/v1/tokens?minted_for_others=true` (each row carries `minted_by`, the admin who created it), and revoke one with `DELETE /api/v1/tokens/{id}`, or all of a person's with `POST /sessions/revoke` for their `sub`.
  - The revoke's `token.revoke` audit row carries `minted_by`.
  - The owner sees `minted_by` on their own `GET /me/tokens`.
  - At the person's sign-in the usual login re-stamp applies, with one difference.
  - If their real role differs from the token's stamp, a token an admin created for them is **revoked** instead of re-stamped, otherwise whoever kept the plaintext would hold a higher tier's credential.
  - When the role is unchanged, the token is re-stamped with the person's real groups at that sign-in and keeps working.
  - Until then its groups are unknown, so every group-tier ceiling, drive allocation or deny grant fails closed for it, as for a truncated session.
- **Keying rule: a person is their identity provider's `sub`.**
  - A sign-in resolves to exactly the id_token's `sub`, case-sensitive, and nothing else.
  - So `principal` must be that string exactly.
  - A person's first sign-in attaches to the row by subject equality alone and stamps `first_signed_in_at`.
  - The email is your assertion.
  - Before a first sign-in it is the only input role derivation has (an email-keyed role mapping, else the default role), and it **never attaches anyone**.
  - Someone else who signs in with the same email has a different subject, lands on a different principal and reaches none of this person's runs, tokens, secrets or drive.
  - The real person signing in under a subject you mistyped also attaches to nothing: revoke the orphaned tokens and create the person again.
- **On Entra ID, key a person who has never signed in by object id.**
  - Entra's `sub` is pairwise: it is different for every app registration and unknown until the person's first sign-in.
  - So on a deployment whose issuer is Entra ID (`login.microsoftonline.com`, `.us`, `login.partner.microsoftonline.cn` or `sts.windows.net`), create them with `tenant_id` and `object_id` instead of `principal`.
  - Both are GUIDs; find them as described in [deploy/azure-entra-sso/README.md](../../deploy/azure-entra-sso/README.md#pre-creating-a-person-by-object-id).
  - Their principal is `entra:<tenant_id>:<object_id>`, which is what you pass as `{principal}` to list their tokens.
  - Wardyn records this deployment's issuer with them.
  - A sign-in becomes this person only when its issuer, `tid` and `oid` claims all equal the recorded ones exactly, and then on every sign-in, so re-registering the app does not orphan them.
  - Nothing else attaches them: not the email, not the object id under another tenant or issuer, and not a `sub` spelling their principal.
  - A sign-in whose `sub` starts with `entra:`, in any case, is refused (no real Entra `sub` has a colon).
  - Each such sign-in writes a `person.attach` audit row naming the person and the pairwise `sub`.
- Set up by object id only someone who has **never signed in**.
  - Someone who has already signed in is known under their pairwise `sub`, and an object-id record never re-keys them.
  - If you give their email, `POST /people` refuses the record with `409`, because the email already names their subject.
  - Without an email Wardyn cannot tell at create time (it does not record a sign-in's `oid`), so the check happens at sign-in instead.
  - A sign-in that matches the record but whose `sub` already names someone here keeps that `sub`:
    - a person record, or an API token, SSH key, run, workspace or stored secret they own (a secret includes the credential a sign-in captures for them).
  - It does not attach, and it writes a `person.attach` row with outcome `denied` and `reason:"sub_known"` naming both.
  - The record then stays unused.
  - Confirm such a person by `principal` (the `sub` on one of their tokens or runs) instead.
  - Setting up an Entra person by email alone is not supported.
  - On an Entra issuer the plain form refuses a `principal` in the `entra:` namespace (`422`, `person_principal_reserved`), since no sign-in can become it.
  - On any other issuer the object-id form is refused `422`, and sign-in keys people by `sub` exactly as before.
- **After replacing the app registration (Entra ID).**
  - A new app registration gives every person a new pairwise `sub`.
  - Everyone whose identity row is bound to a `sub` is then refused at sign-in (the log names both principals).
  - People keyed by object id (`entra:<tenant id>:<object id>`) are unaffected.
  - Adding the person on People by object id does not help: the row stays bound to the old `sub`.
  - Unbind the row so the next sign-in binds it to the new `sub`: `POST /api/v1/admin/identities/{id}/unbind` (admin only; no body).
  - Find the id with `SELECT id, principal, email_lower FROM principal_identities WHERE issuer = '<issuer>' AND object_id <> '' AND principal NOT LIKE 'entra:%' AND deactivated_at IS NULL;`.
  - Only a row keyed by tenant and object id can be unbound (`409`, `identity_not_rebindable`, otherwise); any other is found by its principal alone and would be orphaned.
  - A deactivated or purged identity is refused too (`409`, `identity_deactivated`).
  - The unbind clears only the row's principal. It keeps `authority_epoch`, `deactivated_at` and the SCIM linkage, and it cuts the released `sub`'s sessions.
  - **The old `sub` must hold nothing first.** After the re-bind a suspension reaches only the new `sub`, so the unbind is refused (`409`, `identity_principal_in_use`, with the counts) while the old `sub` still holds an API token, an SSH key or a run that has not ended. Revoke or end them, then unbind.
  - Its stored credentials, workspaces and drive stay under the old principal, which the person no longer signs in as: expect to re-create them.
  - It writes an `identity.unbind` audit row naming the principal it released and the admin who did it. The next sign-in's bind is not audited; it shows as the identity's new principal on People.
- `POST /people` answers `409` rather than create an ambiguous identity.
  - That happens when the email already names another known subject, when the subject is already known under a different email, when the subject differs from a known one only by case, or when the subject is another person's email.
  - It answers `422` for the reserved subjects `admin-token`, the local-mode operator, `local:…`, `device:…`, `delegate:…` and `subject:…`, in any case — the same set a sign-in is refused for (see "Some subjects never sign in").
- Revocation of any such token is immediate either way: `DELETE /api/v1/tokens/{id}`, or the person's own `DELETE /api/v1/me/tokens/{id}`.
## Delegated run management (portals)

- A trusted front-end — a portal — can create, list, extend, stop and open runs for the person signed in to it, without holding that person's API token.
- The portal trades the person's own live identity-provider token for a short delegated token (RFC 8693 token exchange).
- The rule is **no impersonation; delegation is recorded as delegation**: the person owns and is the actor of everything the token does, and every audit row names the portal beside them.

- **Register a portal** (super admin only).
  - The portal must sign people in against the same identity provider and issuer as Wardyn, with a group claim in its tokens.

| Call | What |
|---|---|
| `POST /api/v1/admin/delegates` `{"name":"…","idp_client_id":"<the portal's client id>","group":"<group>"}` | register a portal; the `credential` (`wdp_…`) is in this response only, the row keeps its hash |
| `GET /api/v1/admin/delegates` | every portal, revoked ones included (admin or `security_admin`) |
| `DELETE /api/v1/admin/delegates/{id}` | revoke it (admin or `security_admin`) |

- The portal acts only for people in its `group`, matched against the group claim of the person's own token (canonicalized the way a sign-in snapshot is).
- `idp_client_id` cannot be Wardyn's own client id.

- **Exchange.**
  - `POST /api/v1/token`, form-encoded, the portal's id and credential as HTTP Basic:

```
grant_type=urn:ietf:params:oauth:grant-type:token-exchange
subject_token=<the person's token>
subject_token_type=urn:ietf:params:oauth:token-type:access_token   (or …:id_token, …:jwt)
```

- The subject token must verify against Wardyn's issuer and key set, be unexpired, carry an `iat`, and be either:
  - an access token for Wardyn (`aud` holds Wardyn's client id) that the portal requested (`azp` is the portal's client id), or
  - a token issued to the portal itself (`aud` is exactly the portal's client id).
- The person is then admitted exactly as a sign-in would admit them — reserved subjects, the email-domain gate, role and user-type derivation from the token's own claims.
- On success the answer is `{"access_token":"wdg_…","token_type":"Bearer","expires_in":600,…}`: ten minutes, no refresh token.
- A portal that needs longer exchanges the person's live token again.
- `actor_token` is refused (the authenticated portal is the actor), and a delegated token cannot itself be exchanged.

| Answer | Why |
|---|---|
| `401 invalid_client` | no, wrong or revoked portal credential (`auth.fail`, actor `wardyn/delegation`) |
| `400 invalid_grant` | the subject token did not verify, was not issued to or for the portal, or its person was refused as a sign-in would be, or their sessions were revoked after it was issued |
| `403 access_denied` | the person is not in the portal's group |

- **What a delegated token can do.**
  - Exactly: `POST /runs`, `POST /runs/preflight`, `GET /runs`, `GET /runs/{id}`, `GET /runs/{id}/events` (the lifecycle stream, at most 32 open per person across every portal and their own clients), `GET /runs/{id}/output` (command output only; recording-derived output stays the person's own), `PATCH /runs/{id}` (end and wait), `POST /runs/{id}/kill`, `POST /runs/{id}/attach-ticket` (also `/attach/ticket`, and the UI-gateway ticket), and `GET /me`.
  - Every other route answers `403` with reason `delegation_scope` and an `authz.denied` row — including secrets, API tokens, SSH keys, approving or denying the person's own held egress, revive, and every admin route.
  - Setting a secret and adding an SSH key also refuse a delegated request in their own handlers, so a later change to the allow-list cannot open them.
  - The person is always treated at **user** reach, whatever their own role: an admin acting through a portal reaches only their own runs.
  - Ownership, secrets, drives and the governance ceiling all resolve on the person.
- **What is recorded.**
  - Each exchange writes `delegation.exchange` (actor `delegate:<id>`, target the person).
  - Every row a delegated request writes — the API's, the identity provider's, the attach and UI-gateway rows of a ticket it minted — has the person as actor and `data.via = {"delegate":"<portal id>","grant":"<token id>"}`.
  - A run it launches carries `created_via` (the portal id) on the run row and in the API.
- **Revocation.**
  - Revoking the portal ends every delegated token it holds on their next request.
  - `POST /api/v1/sessions/revoke` for the person ends theirs the same way and refuses new exchanges of tokens issued before it.
  - A disable done only at the identity provider takes effect at the next exchange, so at most ten minutes.
  - An open `GET /runs/{id}/events` stream re-checks its token at each keepalive (about every 15 seconds) and ends when the portal has been revoked or the token has expired, rather than at its five-minute hold.
  - A UI-app session a portal opened (through an attach ticket) is a credential derived from that grant and is bounded by it.
  - Redemption and every 30-second re-check ask whether the portal is registered and the grant unexpired, and the session cookie is capped at the grant's expiry, so it lasts at most about ten minutes.
  - Revoking the portal ends it at the next re-check.
  - Open WebSocket streams are the exception: a terminal or relayed socket already established keeps working until it closes or the run ends.
  - Runs a portal launched keep running after it is revoked: they are the person's runs.
