> Part of the [Operations](../OPERATIONS.md) split (task pages under `docs/operations/`).

# Per-user API tokens

Stop sharing the admin token: a token per person, what it may reach, the stamp that keeps
it honest, tokens for a person who never signs in, and the portals that act for a group.

| You need | Use |
|---|---|
| Automation that acts as you | [A per-user token](#per-user-api-tokens-stop-sharing-the-admin-token) |
| Automation under its own name | [A service identity](#service-identities) |
| A caller of a run's inbound port | [An inbound token](#inbound-tokens) |
| The CLI signed in as you | [`wardyn login`](#cli-sign-in-wardyn-login) |
| A portal acting for a group | [A delegated token](#delegated-run-management-portals) |
| A person who never signs in | [A person record](#tokens-for-a-person-who-never-signs-in) |

<a id="per-user-api-tokens-stop-sharing-the-admin-token"></a>

## Mint, list and revoke

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
  - Plan to re-mint tokens and re-register SSH keys after a global revoke.
- To end only a person's browser sessions, send `"sessions_only": true` with `sub`: a session-only cut is stamped and nothing else changes.
  - Their browser sessions end on every instance; their API tokens, SSH keys, portal-delegated tokens and sponsored services keep working; an API token can still approve a governance change.
  - No token-borne credential is refused by the cut; an event stream a token opened before the cut closes at its next keepalive and reconnects.
  - It is refused (`sessions_revoke_param_invalid`) with `all`.
  - The audit row is the usual `session.revoke`, with `sessions_only` set and both counts `0`.
  - The People drawer's "Sign out everywhere" sends it.
- **No `role` parameter on `POST /me/tokens`.**
  - A token always mints at the caller's own current role; there is no deliberately-downgraded mint.
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
  - The same `OnLogin` hook re-stamps their SSH keys.
  - So a demotion, or a group membership change, reaches outstanding tokens at that human's own next login rather than immediately.
  - A token's stamp is timed: `api_tokens.identity_stamped_at` is set at mint and again at every one of those re-stamps.
  - By default nothing ages a stamp out on its own, so a human who is demoted in the identity provider and never signs in again keeps the role and groups their tokens were minted with.
  - Set `WARDYN_ROLE_STAMP_TTL` (default off) to bound that.
  - A token whose stamp is older than the TTL is refused with `401` and the reason `role_stamp_stale` (an `authz.denied` audit row) until its owner signs in again, which re-stamps it with their current role.
  - A console session older than the TTL is sent back through sign-in.
  - So a stamp you can tune is also the age of the longest role a session can carry.
  - (With `WARDYN_OIDC_SESSION_TTL` unset a session already ends at the ID token's own expiry, and with it set at that TTL after sign-in, so the role-stamp TTL only matters when it is shorter than the session.)
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
- A demotion made on the People page is one of those explicit revocations.
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
- That matters most for the `security_admin` tier.
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

## Inbound tokens

- An **inbound token** is a `wdn_` token of kind `inbound`. It calls the inbound ports of runs that name its holder as a caller, through the [UI-sandbox gateway](../UI-SANDBOXES.md#inbound-let-named-callers-reach-a-port), and does nothing else.
- It authenticates nothing on this API: every console route answers it the one `401` of an unknown token, so the holder cannot launch a run or read anything.
- The gateway takes an inbound token and nothing else. An API, CLI, admin or portal token sent there is refused `401` `inbound_credential_invalid`.

| Call | Who | What |
|---|---|---|
| `POST /api/v1/me/tokens` with `{"kind":"inbound"}` | any signed-in human | an inbound token for yourself, as the person you are |
| `POST /api/v1/me/service-identities/{name}/tokens` with `{"kind":"inbound"}` | the identity's sponsor | an inbound token for the service identity, which a `service:<name>` caller entry names |

- `kind` is `api` when omitted. Any value other than `api` or `inbound` is a `400`.
- Every guard of an ordinary mint applies, because it is the same door: the `api_token` [feature](capabilities.md#feature), the show-once answer, and the cap of `20` live tokens per principal, inbound ones counted.
- **It must expire.**
  - The default and the maximum are `90d`, or the deployment cap `WARDYN_API_TOKEN_MAX_TTL` if lower, the rule a service token follows.
  - A `ttl_seconds` above that is clamped, and the `token.create` row records the clamp.
- Lists mark it: `GET /me/tokens`, `GET /tokens` and a service identity's token list carry `"inbound": true` on its row.
- **Every call is re-checked** against the token's owner as an API call is: revocation, expiry, a person's sign-in, and for a service identity its live record and sponsor.
  - The gateway reuses the result for 30 seconds per token, run and app.
  - A WebSocket or other upgraded connection an inbound token opened is re-checked every 15 seconds and closed within 30 seconds of its last admitting check once the token no longer holds.
  - A person's inbound token also answers `401` `role_stamp_stale` once `WARDYN_ROLE_STAMP_TTL` has passed since its owner last signed in.
- **Groups are the ones stamped on the token.** A `group:` caller entry matches a person's token by its stored groups: those of the mint, refreshed each time its owner signs in again. A group change reaches the token at that sign-in, not at once.
- Revoking a person's sessions revokes their inbound tokens with the rest. Changing a service identity's sponsor revokes its tokens, inbound ones included.
- The token never reaches the sandbox, which sees the caller as `X-Wardyn-Caller`. Wardyn does not bind it to a network.

## CLI sign-in (`wardyn login`)

Off unless the operator sets `WARDYN_CLI_SIGN_IN=true`; it needs SSO. The CLI half: `wardyn login`, `whoami`, `logout`;
a new login ends the last.

- **Sign-in.** The CLI asks for a sign-in and opens the console. The person checks the computer name, the code and the
  address on the approve page, which also shows the lifetimes that apply, and presses Approve or Deny. Approving needs
  what minting an API token needs: admin mode, the `api_token` feature, a browser session (never a token).
- **What the CLI keeps.** A session secret and a device key, in the keychain or in a file only its owner can read.
  They DO live on the laptop; the server keeps a hash. Requests use a 1-hour access token the CLI renews with them.
  What protects them and what does not is in [THREAT-MODEL.md](../../threatmodel/THREAT-MODEL.md), "CLI sign-in".
- **Lifetimes.** A session lasts 30 days (`WARDYN_CLI_SESSION_TTL`, 1 hour to 90 days) and ends after 7 days
  without a renewal (`WARDYN_CLI_SESSION_IDLE`). One kept in a file lasts at most 7 days, and
  `WARDYN_CLI_FILE_STORE=false` refuses the file. The access token lasts 1 hour, or `WARDYN_API_TOKEN_MAX_TTL` when
  that is shorter.
- **Every renewal re-checks.** A revoke, a sessions cut, deactivation, a stale role stamp and a lost `api_token`
  feature each refuse it, so a role or feature change reaches the CLI within the hour. Each renewal rotates the
  secret; an old secret presented again ends the session (`cli.session.reuse_detected`).
- **Ending it.** `POST /sessions/revoke` (every scope, including `sessions_only`), a console sign-out, Signed-in CLIs
  (`DELETE /me/cli-sessions/{id}`) and SCIM deactivation end the session and its tokens at once. A sessions-only cut
  leaves the person's own API tokens working, as before; `session.revoke` counts `cli_sessions_ended`.
  The CLI then says why only when the sign-in was signed out, reached its lifetime or went unused past its idle
  lifetime; every other end reads the same.
- **Admins.** An admin's CLI acts as role `user`. `wardyn login --admin` asks for a fresh approval and gives a 1-hour
  `cli_admin` token with the real role, never renewed, which ends with the session.
- **No browser.** `WARDYN_CLI_PASTE_SIGN_IN=true` lets the person paste a code back instead; the code lasts 5
  minutes and the request 10. Off by default, because a pasted code is easier to phish.
- CLI tokens count toward the 20 live tokens per person and show in `GET /me/tokens` as `CLI: <computer>`.

## Tokens for a person who never signs in

- **No one can create a token that acts as another person.**
  - An admin or `security_admin` can still set a person up before their first sign-in, but cannot mint a token for them.
  - `POST /api/v1/people/{principal}/tokens` answers every caller `403` with reason `person_token_mint_removed` ("No one can create a token that acts as another person. They sign in and create their own."), whether or not the person exists.
  - No role, flag or environment variable turns it back on.
  - A token an admin created for someone else acted as that person while the admin held its plaintext, which is the reach this closes.
- For people who never open the console, the person signs in once and creates their own token (`POST /api/v1/me/tokens`, or the console), or claims an [invitation](#invitations-and-service-identities) you send them.
  - Pipelines use a [service identity](#service-identities) instead of a person's token; see [CI.md](../CI.md).
  - It is never the deployment's admin token.
  - A trusted front-end that acts for people who ARE signed in to it uses [delegation](#delegated-run-management-portals) instead: it never holds a long-lived token for anyone.

| Call | What |
|---|---|
| `POST /api/v1/people` `{"principal":"<sub>","email":"<email>"}` | create the person, or confirm the one already there (`201` / `200`) |
| `POST /api/v1/people` `{"tenant_id":"<tid>","object_id":"<oid>","email":"<email>"}` | Entra ID only: the same, keyed by the tenant and object id (see below); the principal is `entra:<tid>:<oid>` |
| `POST /api/v1/people/{principal}/tokens` | refused: `403` `person_token_mint_removed`, one `person.token.create` audit row with outcome `denied` |
| `GET /api/v1/people/{principal}/tokens` | that person's tokens, revoked ones included |
| `GET /api/v1/tokens?minted_for_others=true` | every live token an admin created for someone else before this change, less those refused for their owner ([below](#what-happens-to-a-token-an-admin-created)), each with a [`disposition`](#what-happens-to-a-token-an-admin-created) (metadata only, never a plaintext); also on the console's Admin > Credentials page |

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
- <a id="after-replacing-the-app-registration"></a>**After replacing the app registration (Entra ID).**
  - A new app registration gives every person a new pairwise `sub`.
  - Everyone whose identity row is bound to a `sub` is then refused at sign-in (the log names both principals).
  - People keyed by object id (`entra:<tenant id>:<object id>`) are unaffected.
  - Adding the person on People by object id does not help: the row stays bound to the old `sub`.
  - Unbind the row so the next sign-in binds it to the new `sub`: `POST /api/v1/admin/identities/{id}/unbind` (admin only; no body).
  - Find the id with `SELECT id, principal, email_lower FROM principal_identities WHERE issuer = '<issuer>' AND object_id <> '' AND principal NOT LIKE 'entra:%' AND deactivated_at IS NULL;`.
  - Only a row keyed by tenant and object id can be unbound (`409`, `identity_not_rebindable`, otherwise); any other is found by its principal alone and would be orphaned.
  - A deactivated or purged identity is refused too (`409`, `identity_deactivated`).
  - The unbind clears only the row's principal. It keeps `authority_epoch`, `deactivated_at` and the SCIM linkage, and it cuts the released `sub`'s sessions.
  - **The old `sub` must hold nothing first.** After the re-bind a suspension reaches only the new `sub`, so the unbind is refused (`409`, `identity_principal_in_use`, with the counts) while the old `sub` still holds an API token, an SSH key, a claimed runner, an unspent runner registration token or a run that has not ended, sponsors a service identity, or is named by a pending invitation. Revoke or end them, then unbind.
  - Its stored credentials, workspaces and drive stay under the old principal, which the person no longer signs in as: expect to re-create them.
  - It writes an `identity.unbind` audit row naming the principal it released and the admin who did it. The next sign-in's bind is not audited; it shows as the identity's new principal on People.
- `POST /people` answers `409` rather than create an ambiguous identity.
  - That happens when the email already names another known subject, when the subject is already known under a different email, when the subject differs from a known one only by case, or when the subject is another person's email.
  - It answers `422` for the reserved subjects `admin-token`, the local-mode operator, `local:…`, `device:…`, `delegate:…` and `subject:…`, in any case — the same set a sign-in is refused for (see [Some subjects never sign in](who-gets-in.md)).
- Revocation of any such token is immediate either way: `DELETE /api/v1/tokens/{id}`, or the person's own `DELETE /api/v1/me/tokens/{id}`.
## Invitations and service identities

A person gets their own credential through an invitation only they can claim.
Automation runs as a named, sponsored service identity, never as a person.
Both replace a token an admin created for someone else, and nothing that works today stops working.

### Invite a person

1. Make sure the person exists: `POST /api/v1/people` (above). An unknown principal is refused `422` `invitation_target_unknown`.
2. Invite them with `wardyn person invite <principal>` or `POST /api/v1/people/{principal}/invitations` (security tier). The answer carries `console_path`.
3. Send them that link. They sign in as themselves and claim the invitation on Account, under Invitations.
4. The token is shown to them once. You never see it.

| Option | CLI flag | Meaning |
|---|---|---|
| `expires_in_seconds` | `--expires-in` | How long it stays claimable: default `72h`, at most `14d`. |
| `token_name` | `--token-name` | The name the token starts with; the person may change it. |
| `token_ttl_seconds` | `--token-ttl` | Upper bound on the token's lifetime; `WARDYN_API_TOKEN_MAX_TTL` still caps it. |
| `replaces_token_id` | `--replaces` | A live token an admin created for that person; the claim revokes it. |

| State | Meaning |
|---|---|
| `pending` | Claimable now. |
| `claimed` | Claimed once; `token_id` names the token, never its value. |
| `expired` | Past `expires_at`. |
| `revoked` | Withdrawn by an admin. |

- **The invitation holds no secret.**
  - Only the target, signed in as themselves, can claim it, so a forwarded link reaches nothing.
  - Anyone else who is signed in, an admin included, gets `404` `invitation_not_found`, the answer for a missing id.
  - The target's own claim of a dead invitation is `409` `invitation_not_pending`, with its `state`.
  - The target's own `wdn_` token may list their pending invitations (`GET /me/invitations`); only a signed-in claim mints.
- **A claim is a self-mint.**
  - The token carries the claimer's own role, user type and groups, exactly as `POST /me/tokens` does, and has no `minted_by`.
  - Claiming never grants the `api_token` feature: a person without it is refused `403` `capability_feature`.
  - The self-mint refusals apply unchanged: a `wdn_` token, the admin token, local mode and a portal's token cannot claim (`api_token_from_api_token`, `api_token_no_human`, `delegation_scope`), nor can a service identity's token (`service_scope`).
- **There is no CLI claim,** because the CLI has no browser sign-in.
- Ten pending invitations may name one person (`422` `invitation_cap_reached`).
- `wardyn person invitations <principal> [--state pending|claimed|expired|revoked]` and `GET /api/v1/invitations?state=` list them, never with a token.
- `wardyn invitation revoke <id>` or `DELETE /api/v1/invitations/{id}` withdraws a pending one; any other state answers `404`.
- Each step is audited: `invitation.create`, `invitation.claim` (with `token.create`) and `invitation.revoke` ([AUDIT-ACTIONS.md](../AUDIT-ACTIONS.md)).

### What happens to a token an admin created

Each row of `GET /api/v1/tokens?minted_for_others=true` carries a `disposition`, derived when you read it.
Revoked, expired and refused rows stay out without `&include_revoked=true`, so a finished move stays visible.
Admin > Credentials shows it as a State column and an "Also show revoked and expired tokens" toggle.

| `disposition` | Condition |
|---|---|
| `needs_decision` | Live, an expired token not yet revoked included (only `DELETE` acts on one); no pending invitation replaces it; not moved, or moved to an identity since retired. |
| `awaiting_owner` | Live; a pending invitation names it in `replaces_token_id`. |
| `replaced_by_owner` | Revoked by that invitation's claim. |
| `linked_to_service_identity` | Live; moved to a service identity that is not retired. |
| `moved_to_service_identity` | Revoked after a move. |
| `revoked` | Revoked, neither of the above. |

| Field | Set on a row |
|---|---|
| `owner_refused` | Live, but refused for its owner: deactivated, cut off by a later revocation of the owner's credentials, a reserved name, or a role stamp older than `WARDYN_ROLE_STAMP_TTL`. |

- **To hand the token to its owner,** invite the token's principal with `--replaces <token-id>`. The owner is the token's principal, never `minted_by`.
- **To hand it to automation,** link it to a service identity: `wardyn token move <token-id> <service-identity>`, or `POST /api/v1/tokens/{id}/move` with `{"service_identity":"<name>"}` (security tier, audited `token.update`).
  - A move only links: it revokes nothing, and the token keeps working.
  - Another move to a different identity is allowed. A retired or unknown identity, or a token that is revoked, expired or not admin-created, is refused `422` `token_move_invalid`.
- **Nothing revokes the old token for you.** Revoke it with `DELETE /api/v1/tokens/{id}` once the job uses the new one.

### Service identities

A service identity is automation that runs under its own name. It is not a person, never an administrator, and never an operator.

- **Principal.**
  - `service:<name>`, with `name` matching `^[a-z][a-z0-9-]{2,62}$`.
  - A name is never reused or renamed, and a retired identity stays on record, so no later identity inherits its grants or history.
  - `POST /service-identities` refuses a name any principal-keyed table already holds (`409` `service_identity_name_taken`).
- **Sponsor.**
  - Every identity names one sponsor: a person with a bound sign-in identity, given by exactly their principal.
  - An email or `entra:` alias is refused `422` `service_identity_sponsor_invalid`.
  - Only the sponsor mints its tokens.
- **Local mode** has no person to sponsor one: the service-identity routes answer `409` `needs_sso`, except the sponsor's token mint, which answers `403` `api_token_no_human`.

| Verb | Who | What |
|---|---|---|
| `wardyn service-identity create --name N --sponsor P [--user-type T] [--description D]` | security tier | create it; `--user-type` is an existing user type, default `standard`. It has no token yet. |
| `wardyn service-identity list`, `get <name>` | security tier | the identity with its state: `active`, `suspended`, `retired` or `sponsor_inactive`. |
| `wardyn service-identity update <name> [--description D] [--sponsor P] [--suspend] [--resume]` | security tier | change it. A new sponsor revokes every token of the identity. |
| `wardyn service-identity retire <name>` | security tier | end it for good. |
| `wardyn service-identity token list <name>`, `token revoke <name> <token-id>` | the sponsor | read and revoke its tokens, never their values. |

- **No CLI mint.** The sponsor mints in the console, signed in, from Account under "Service identities I sponsor", in the show-once dialog. The sponsor must be in admin view to mint: in member mode the mint answers `409` `api_token_member_mode_mint`.
- **Four-eyes.**
  - With `WARDYN_GOVERNANCE_SECOND_HUMAN` on, a create and any `PATCH` that changes the sponsor are held as governance change kind `service_identity` until a distinct human on the security tier approves ([governance writes](governance-writes.md)).
  - The `PATCH` is held whole. Description, suspend, resume and retire stay direct.
  - An approval applies only against the state the reviewer saw, otherwise `409` `governance_change_stale`.

| Caller of `POST /api/v1/me/service-identities/{name}/tokens` | Answer |
|---|---|
| The current sponsor, signed in, identity active, sponsor's session live | `201`, the token once |
| The sponsor, identity suspended or retired, or the sponsor's session not live | `409` `service_identity_inactive` |
| Another person, any role | `404` `service_identity_not_found`, the answer for a missing name |
| The admin token, or local mode | `403` `api_token_no_human` |
| Any `wdn_` token, the sponsor's included | `403` `api_token_from_api_token` |
| A portal's token | `403` `delegation_scope` |
| A service identity's token | `403` `service_scope` |
| The sponsor without the `api_token` feature | `403` `capability_feature` |
| The sponsor in member mode | `409` `api_token_member_mode_mint` |
| The sponsor, with `20` live tokens on the identity | `422` `api_token_cap_reached` |

- **A service token** is an ordinary `wdn_` token with principal `service:<name>`, role `user` and no email or groups.
  - The sponsor can also mint it as an [inbound token](#inbound-tokens), which a `service:<name>` caller entry of an inbound app names.
  - It must expire: the default and maximum are `90d`, or the deployment cap if lower.
  - It is a bearer. Wardyn does not bind it to a workload or a network.
- **Every request is re-checked** against the live identity and its sponsor.
  - A missing, suspended or retired identity, a token older than the current sponsor, or a sponsor who is deactivated, purged or fully revoked after the token was minted is refused with the one `401` of an unknown token.
  - A sponsor does not need a live session: signing out leaves the service's tokens working. A deactivated sponsor ends the identity.
  - The `auth.fail` row says why: `service_identity_inactive` or `service_sponsor_inactive`.
  - A sponsor disabled only at the identity provider is not seen. Without SCIM or a session revoke the service's tokens work until they expire. Without SCIM its live runs continue.

| A service identity's token may reach | Why |
|---|---|
| `POST /runs`, `POST /runs/preflight` | launch, and the dry run CI uses |
| `GET /runs`, `GET /runs/{id}`, `/events`, `/output` | read its own runs; `/events` is a stream to follow, not poll |
| `GET /audit?run_id=<id>` | the audit rows of a run it created, and `[]` for any other run id |
| `GET /presets`, `GET /presets/{name}` | the launch presets open to its user type |
| `GET /workspaces` | the workspaces it may launch with |
| `PATCH /runs/{id}`, `POST /runs/{id}/kill` | manage its own runs |
| `GET /me`, `GET /setup/status` | who am I and what can I reach (`GET /me` adds a `service` block), and which providers are live |
| `PUT`, `DELETE /model-providers/{id}/credential` | its own model key, in its own namespace |
| `PUT /workloads/{name}`, `GET /workloads`, `GET /workloads/{name}` | keep a named run request running, and read it |
| `DELETE /workloads/{name}`, `POST /workloads/{name}/restart` | remove a workload (ends its run), or restart it |
| `POST /workloads/{name}/preview` | what a `PUT` of the same body would do; stores nothing |
| `PUT`, `DELETE /workloads/{name}/policy-pin` | pin its own workload to the launch policy digest a preview reported, or clear the pin |
| `POST /templates/import` | the file check `diff` and `apply --dry-run` make; stores nothing |

- Every other route, including any added later, answers `403` `service_scope` with an `authz.denied` row.
  - That covers attach, an inbound app's browser ticket (`POST /runs/{id}/apps/{app}/ticket`), approvals, secrets, tokens, SSH keys, invitations, `GET /audit/export`, `GET /workspaces/{id}` and every admin route.
  - Its own calls to an inbound app go through the gateway, with its inbound token.
- **Grants and credentials.**
  - Capability grants and governance assignments bind to the `user` subject type with the subject `service:<name>`.
  - It has no groups, so only `all` and user-type rows reach it.
  - Its secrets live in its own namespace. An `owner_only` grant fails closed on its runs when it has stored nothing.
  - Its ceiling is its user type and grants, and is not bounded by its sponsor's: whoever holds a service token acts with the service's reach.
- **Runs.**
  - A run it launches belongs to `service:<name>` and is never operator-owned. The sponsor is the run's accountable human.
  - The current sponsor can list, read and kill its runs, and nothing more: not the held approvals, sandbox files or run tokens.
  - Entering one (a terminal or a UI app) needs an explicit `service_attach` grant ([Capabilities](capabilities.md#service_attach)), off for everyone by default, super admins included. SSH never admits it.
  - A portal's delegated token has none of this reach.

**Lifecycle.**

| Event | Effect |
|---|---|
| The sponsor changes | Every live token of the identity is revoked in the same transaction. |
| The sponsor's sessions are revoked in full | Their sponsored tokens are marked revoked and stop at once, except that an inbound token still calls a UI-sandbox port for up to 30 seconds. |
| The sponsor is suspended through SCIM | State `sponsor_inactive`; tokens refused; its live runs end with the sponsor's. |
| The sponsor's sign-in identity is unbound | Refused `409` `identity_principal_in_use` while they sponsor an identity or hold a pending invitation. |
| An admin suspends it | Tokens refused; its live runs end. |
| An admin retires it | Tokens revoked, its live runs ended and its stored credentials erased (`credential.erase`); the name stays taken. |

- Rows a service's own request writes have actor type `service`, actor `service:<name>` and `data.sponsor` ([AUDIT-ACTIONS.md](../AUDIT-ACTIONS.md#service-identities)).
  - They name the service and its sponsor, not whoever holds the token.

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
| `DELETE /api/v1/admin/delegates/{id}` | revoke it (admin or `security_admin`); `?cut_in_flight=true` also cuts the gateway access of the runs it launched, and on a revoked portal cuts it then |

- The portal acts only for people in its `group`, matched against the group claim of the person's own token (canonicalized the way a sign-in snapshot is).
- `idp_client_id` cannot be Wardyn's own client id.
- **Pin a portal to one unit** with `"run_in_under":"<unit id or exact name>"` at registration. The default is no pin.
  - A run through a pinned portal must resolve to that unit or one below it, or `POST /runs` and `POST /runs/preflight` answer `403` `portal_unit_scope`. A default context outside the unit is refused too.
  - The person's restrictions still apply after the pick.
  - The pin is set at registration only; register a new portal to change it. A deleted unit leaves a portal that refuses every launch. An unreadable portal record answers `503` `portal_unit_unavailable`.
  - The pin is not checked against any other setting: a super admin may pin any unit.
  - A platform's tools can travel as inline `components` on the portal's `POST /runs`. Anything naming a secret stays an organisation component.

- **Exchange.**
  - `POST /api/v1/token`, form-encoded, the portal's id and credential as HTTP Basic:

```
grant_type=urn:ietf:params:oauth:grant-type:token-exchange
subject_token=<the person's token>
subject_token_type=urn:ietf:params:oauth:token-type:access_token   (or …:id_token, …:jwt)
actor_token=<the acting agent's token>                              (optional)
actor_token_type=urn:ietf:params:oauth:token-type:access_token     (required with actor_token; or …:id_token, …:jwt)
```

- The subject token must verify against Wardyn's issuer and key set, be unexpired, carry an `iat`, and be either:
  - an access token for Wardyn (`aud` holds Wardyn's client id) that the portal requested (`azp` is the portal's client id), or
  - a token issued to the portal itself (`aud` is exactly the portal's client id).
    - On Entra ID its `sub` is pairwise to the portal's app registration, so the person is the principal their own Wardyn sign-in bound to the token's `tid` and `oid`, or a person set up by that object id.
    - With neither (the person has never signed in here), the exchange is refused (`subject_token_unbound`).
- The person is then admitted exactly as a sign-in would admit them — reserved subjects, the email-domain gate, role and user-type derivation from the token's own claims.
- On success the answer is `{"access_token":"wdg_…","token_type":"Bearer","expires_in":600,…}`: ten minutes, no refresh token.
- A portal that needs longer exchanges the person's live token again.
- A delegated token cannot itself be exchanged.
- **Naming the acting agent** (`actor_token`, on by default with no setting: it is recorded, never authorised, so it grants nothing).
  - The actor token must verify against Wardyn's issuer and key set, be unexpired, and carry an `iat` and a `sub`.
  - Its `aud` must hold the portal's client id and must not hold Wardyn's: a person's own Wardyn token can never name an agent.
  - It must not name the person (the same `sub`, or on Entra ID the same `tid` and `oid`).
  - When the person's token carries `may_act`, the agent's `sub` (and `iss`, if named) must match it.
  - A portal holding tokens for several people could name one person's token as another's agent. The identity provider bounds this; issue `may_act` where it supports it.

| Answer | Why |
|---|---|
| `401 invalid_client` | no, wrong or revoked portal credential (`auth.fail`, actor `wardyn/delegation`) |
| `400 invalid_grant` | the subject token did not verify, was not issued to or for the portal, or its person was refused as a sign-in would be, or their sessions were revoked after it was issued; or the actor token was refused |
| `403 access_denied` | the person is not in the portal's group |

- **What a delegated token can do.**
  - Exactly: `POST /runs`, `POST /runs/preflight`, `GET /runs`, `GET /runs/{id}`, `GET /runs/{id}/events` (the lifecycle stream, at most 32 open per person across every portal and their own clients), `GET /runs/{id}/output` (command output only: through an issuer portal, also its person's command output recovered from the run's recording; never an interactive run's last screen), `PATCH /runs/{id}` (end and wait), `POST /runs/{id}/kill`, `POST /runs/{id}/attach-ticket` (also `/attach/ticket`, and the UI-gateway ticket), and `GET /me`.
  - Every other route answers `403` with reason `delegation_scope` and an `authz.denied` row — including secrets, API tokens, SSH keys, approving or denying the person's own held egress, revive, and every admin route. A unit's [deciders](deciders.md) never change this: a delegated token decides nothing and lists nothing it could decide.
  - Setting a secret and adding an SSH key also refuse a delegated request in their own handlers, so a later change to the allow-list cannot open them.
  - The person is always treated at **user** reach, whatever their own role: an admin acting through a portal reaches only their own runs.
  - Ownership, secrets, drives and the governance ceiling all resolve on the person.
- **What is recorded.**
  - Each exchange writes `delegation.exchange` (actor `delegate:<id>`, target the person).
  - Every row a delegated request writes — the API's, the identity provider's, the attach and UI-gateway rows of a ticket it minted — has the person as actor and `data.via = {"delegate":"<portal id>","grant":"<token id>"}`.
  - When the exchange named an agent, `data.via.actor = {"sub":"…","iss":"…"}` on the rows the delegated request writes, and on the exchange's success row as `actor`.
  - A run it launches carries `created_via` (the portal id) on the run row and in the API, and `created_via_actor` (`sub` and `iss`) when the exchange named an agent. The run's gateway tokens and the rows of its queued start and attach tickets carry the same agent.
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
  - With `?cut_in_flight=true` those runs also get no more gateway tokens ([When a run's gateway access ends](ai-gateway.md#when-a-runs-gateway-access-ends)). It is off by default, so a revoke changes nothing else for the person's work.

### Portals with their own identity issuer

- A portal can sign its people in against its own identity provider, not Wardyn's. Register it with an `issuer`.
- Its people are the portal's own. They are never console sign-ins: their principal starts `delegate:person:`, which every human sign-in door refuses.
- The credential has the prefix `wdi_` followed by 64 lowercase hex characters. It is shown once and stored only as a hash.
- A portal without `issuer` is a console portal, and everything above this section stays as it was.
- A registration answers 409 `portal_issuers_not_activated` until an admin has [switched issuer portals on](#switch-issuer-portals-on), and 409 `portal_issuers_need_sso` while console sign-in is not configured.

| Field | Meaning |
|---|---|
| `issuer` | The exact issuer string in the tokens' `iss`, compared byte for byte. Another spelling of a bound issuer, even by a final `/`, is refused 409 `delegate_issuer_spelling_conflict`. |
| `internal_issuer` | Where Wardyn fetches discovery and keys, when that differs. Its host must be listed in `WARDYN_PORTAL_INTERNAL_ISSUER_HOSTS`. A portal on an issuer already bound must use the binding's address, or 409 `delegate_issuer_address_conflict`. |
| `token_profile` | `okta_id_token` or `okta_access_token`. A portal on a bound issuer must name a person by the same claim (`sub` or `uid`), or 409 `delegate_issuer_profile_conflict`. |
| `audience`, `required_scopes` | Access-token profile only: the one `aud` and the scopes the token must carry. |
| `email_domains` | ID-token profile only: the token's `email_verified` must be true and its domain listed. |
| `groups_claim`, `group` | The claim that lists groups (default `groups`) and the one group a person must be in. |
| `portal_managed_admission` | Set when the portal alone chooses its people: no group, and then no signal of Wardyn's own says the portal chose wrongly. A registration sets a group, or this flag, never both. |
| `required_claim` | One optional `{claim, value}` match, checked in addition to the group (or the flag), never instead of it: the token's top-level claim must be that string. The claim should be one the person cannot edit at the provider. |
| `user_type` | The user type every one of its people gets. It is never read from a claim, and their role is always `user`. |
| `max_subject_token_age_seconds` | 300 to 600, default 300. The oldest a token's `iat` may be. |
| `grant_ttl_seconds` | 300 up to the maximum age, default the maximum age. The longest a grant lives from its mint. A grant also ends at the token's own signed end, so it can live much less: read `expires_in` in the answer, which can be as low as 5 seconds. |
| `ui_session_renewal` | Optional, issuer portals only: a person's browser-app session renews on the portal's grants ([longer browser sessions](#longer-browser-sessions)). A console portal that sets it is refused 422 `delegate_renewal_needs_issuer`. |
| `run_in_under` | Required: the unit its runs live in. The pin covers that unit and every unit under it, for launching runs and for managing the runs its people own, so pinning a parent lets the portal's people work in any child unit. Its people need no membership for an ordinary run there: see [where a portal's person runs](#where-a-portals-person-runs). |

- A registration answers its refusals with 422 or 409 and a reason in [the SDK reference](../sdk.md). `GET /admin/delegates` shows `admission_rev` only for an issuer portal. A console portal's answer keeps its 0.10.0 shape.
- When changes are held for a second person, a registration by a signed-in person is a held change: the answer is 202, and the credential is in that answer only. The break-glass admin token is not held: its write applies at once, and each one writes a `governance.change.bypass` row.
- A portal that sets no group and no required claim carries a note in the review diff and the audit row: the portal chooses who can use it, and anyone it selects who meets the token requirements can act as a user in the pinned unit and every unit under it.
- The group claim of a token is read tolerantly: a value that is not a list of strings counts as no groups.
- An issuer portal keeps no groups for a person: their sign-in record is empty, so no group of theirs reaches any grant or restriction. The portal's own registered `group` still gates its exchange, from the token's claim.
- The People list keeps one display value per person and nothing authorises by it: the token's `email` claim for an ID token (empty if absent), the token's `sub` (the login name) for an access token.

**Who a person is.**

- A person's principal is `delegate:person:<k>:<e>`.
  - `k` is the first 26 characters of the lowercase, unpadded base32 of SHA-256 over a fixed prefix and the exact issuer string.
  - `e` is the lowercase, unpadded base32 of the person id's bytes.
- The person id is the token's `uid` for an access token and its `sub` for an ID token. It is 1 to 128 printable ASCII characters with no space.
- Two portals on one issuer share its people. What separates them is the unit each is pinned to: every request is checked against the portal's pin, and a person's own run outside it answers 403 `portal_unit_scope`.
- Changing the issuer string means new people. The internal address and the pin are not part of who a person is.
- `GET /api/v1/people` returns `issuer` for each portal person, and `internal_address` when the registration has an `internal_issuer` (it is left out when empty, so treat it as optional). The console's People screen does not show them yet.
- A person's first exchange through any portal writes one `person.create` row, which carries no email.

<a id="where-a-portals-person-runs"></a>**Where a portal's person runs.**

- The pin is the person's place to run. Through that portal, the pin unit and every unit under it are their contexts, at the `user` role, with no membership needed.
- With no `run_in`, a run lands in the pin unit, unless the person holds a home membership in the pin unit or a unit under it, which wins. A membership outside the pin is never picked by default. A workspace the run attaches can move it to the workspace's unit, as for a member.
- A strict workspace, or an organisation component with a unit pin, still needs a membership of the person's own in that unit or a unit under it. The portal's pin does not satisfy it: without one, the launch answers 403 `workspace_unit_not_yours` or `component_unit_pinned`. A run launched with one keeps needing it: without it, an extension, resume or revive is refused `unit_membership_gone`.
- `run_in` may name the pin unit or any unit under it. A unit outside the pin answers 403 `unit_not_a_context`, or 403 `portal_unit_scope` when the person holds a membership there.
- The unit's walls and the restrictions that cover the person apply as they do for a member.
- While the portal stays registered, its grant can extend, end, kill and attach to such a run, which resumes a paused one. A revoked portal's grant reaches nothing.
- A revive or an admin restart is not a portal action. When an operator revives or restarts a run outside the Default unit, Wardyn checks that its owner still holds the unit: through the pin of the live portal that started it, or else through a membership. A run in the Default unit needs neither.
- This holds for an issuer portal's own people only. A console portal and a console sign-in take their contexts from memberships alone.

**Exchange.** `POST /api/v1/token`, with the portal's id and `wdi_` credential as HTTP Basic, as above.

- `subject_token_type` must be the profile's: `urn:ietf:params:oauth:token-type:access_token` for `okta_access_token`, `…:id_token` for `okta_id_token`. Another type is 400 `invalid_request`.
- The client sees an OAuth code. The audit row's `reason` is finer, so read the two apart:

| Case | Client gets | `delegation.exchange` `reason` |
|---|---|---|
| `actor_token` sent | 400 `invalid_request` | `actor_token_unsupported` |
| A `wdg_` token as the subject | 400 `invalid_grant` | `subject_token_delegated` |
| The verifier refuses the token | 400 `invalid_grant`, or 400 `invalid_request` for a wrong type, or 403 `access_denied` when the portal may not act for them | the verifier's reason; `cause` or the claim's name beside it |
| Issuer or keys cannot be reached | 503 `temporarily_unavailable`, with `Retry-After` when the wait is above zero | `portal_issuer_unavailable`, with `cause` |
| The admission rule changed mid-exchange | 503 `temporarily_unavailable` | `portal_admission_changed` |
| Identity deactivated or purged, or the session revoked | 400 `invalid_grant`, nothing minted | `identity_deactivated`, `session_revoked` |
| A read failed | 503 `temporarily_unavailable` | `failure` row: the reply's code, `cause` names the step |
| The sign-in record write failed | 503 `temporarily_unavailable` | `failure` row: `cause` is `sign_in_record_write` |
| The identity or grant write failed unexpectedly | 500 `server_error` | `failure` row: `cause` is `issue_portal_identity` or `mint_portal_grant` |

- A wrong, revoked or missing portal credential, or a binding retired mid-exchange, is 401 `invalid_client` and writes `auth.fail`, not a `delegation.exchange` row. A malformed form (`grant_type`, `subject_token`, a type or the actor fields) is refused 400 before the exchange and writes no row.
- The answer is `{"access_token":"wdg_…","token_type":"Bearer","expires_in":N,…}`. The token has no refresh, and `expires_in` is the grant's remaining life, never below 5.
- Every decision past client authentication writes a `delegation.exchange` row. A success row carries `grant`, `expires_at`, `user_type` and `role`; every row names the issuer, `k`, the profile, the two freshness settings and the admission rule by names only. Its fields are in [the audit reference](../AUDIT-ACTIONS.md).

**What the verifier checks**, in this order for a token that passed the signature and `iss` check:

| Check | Rule |
|---|---|
| Times | `exp` present and ahead; `nbf`, if present, not ahead; `iat` a number at most 60 seconds ahead and no older than the maximum age. |
| Grant end | The earlier of `iat` plus the maximum age and `exp` must leave at least 5 seconds, or the token is stale. |
| Access token | `cid` is the registered client id; `scp` holds every required scope; `aud` is exactly the one audience; `uid` is present. |
| ID token | `aud` is exactly the client id; `azp`, if present, is it too; a header `typ`, if present, is `JWT`; no `cid`, `scp`, `scope` or `client_id` claim; `sub` is present. |
| Person | The id is 1 to 128 printable ASCII characters. A `sub` or id that is a reserved name is refused `reserved_principal`. |
| Admission | The `required_claim`, the group, and for an ID token the email domains. A miss is `subject_token_claim_mismatch`, `outside_scope`, `email_verified_absent`, `email_unverified` or `email_domain`. |

- A grant ends at the earliest of `iat` plus the maximum age, the token's `exp`, and its mint plus `grant_ttl_seconds`.
- A person disabled only at the provider keeps a grant for at most the maximum age (5 to 10 minutes) plus the 60-second clock allowance after their token was issued. That holds only if the provider issues no new token to a disabled user.
- A Wardyn suspension of the person, or a revoke of the portal, ends grants at once.
- Keep Wardyn's clock and the provider's synchronised. A token whose `iat` is more than 60 seconds ahead of Wardyn's clock is refused `subject_token_iat_future`.

**Reaching the provider.**

- Wardyn fetches discovery and keys itself: proxy environment variables never pick the destination, and the fetch goes direct unless the registration has no internal issuer address and the issuer's host is on `WARDYN_DAEMON_PROXY_ALWAYS`. It refuses redirects and caps each answer at 1 MiB.
- It resolves the host and dials the address it checked. Loopback, link-local, metadata and other reserved addresses are refused for every host.
- Any host may resolve to a public address. The extra private ranges (RFC 1918, unique-local and CGNAT, no other private space) and the extra CA from `WARDYN_PORTAL_INTERNAL_ISSUER_CA_FILE` apply only to the host and port a registration selects with `internal_issuer` that is also listed in `WARDYN_PORTAL_INTERNAL_ISSUER_HOSTS`. Listing a public issuer's host alone grants neither. An `internal_issuer` that is not listed is refused with `address_refused`, and nothing is resolved. Nothing skips verification.
- Once a discovery or key fetch fails, or breaks a rule above, the issuer fails closed: exchanges answer 503 `temporarily_unavailable`; the audit reason is `portal_issuer_unavailable` and its `cause` is `issuer_unreachable`, `keys_unreachable`, `keys_cooldown`, `keys_off_origin`, `issuer_mismatch`, `address_refused`, `unreachable` or `request_canceled`. A key fetch that breaks a rule drops the keys it brought.
- A failed fetch is enforced before it is logged or reported, so an exchange already under way is refused while the report runs. While a refusal of a discovery document or key address is in force, every unavailable answer carries that refusal's cause, through later failed retries and outages, until a refresh passes. Then `keys_cooldown` means a verified new key address is waiting for its keys.
- A token whose key is not known triggers at most one key fetch per 30 seconds. Inside that interval the answer is 503 `temporarily_unavailable`, cause `keys_cooldown`.
- An outage answer carries `Retry-After`, the rest of the 30-second interval. When a key fetch broke a rule, such as `address_refused`, the wait is zero and the header is left out.
- Wardyn learns of an outage only at its own fetches.
- Keys loaded by any fetch, one an unknown key triggers included, reopen an issuer whose discovery is healthy. A failing or refused discovery keeps it closed.
- A key fetch that breaks a rule drops the keys, their address and the discovery record. Only a newer verified discovery supersedes that, never a key fetch alone.
- A valid token whose keys reload after an address change gets 503 `temporarily_unavailable` (cause `keys_cooldown`), not `invalid_grant`. A key a successful fetch removed is still refused.
- Each discovery and key request has its own 10-second bound, and a caller that leaves does not mark the issuer down.
- A refresh failure counts on `wardyn_portal_issuer_refresh_failures_total{issuer_key,portal,source,cause}`.

**Reaching the issuer through a proxy.**

- For a registration without an internal issuer address, Wardyn uses the daemon proxy when the issuer host is on `WARDYN_DAEMON_PROXY_ALWAYS`. Registrations with an internal issuer address use the direct internal route. Wardyn still resolves the host and checks every address under the rules above, and asks the proxy to connect to an address it checked, never to the name.
- On the proxy route the issuer is trusted only under `WARDYN_PORTAL_PROXIED_ISSUER_CA_FILE`. With it unset, a registration without an internal issuer address whose issuer host is listed is refused at its exchanges. The proxy must pass the issuer's TLS through uninspected.
- The warning `portal issuer: refresh failed` names the `route` (`proxy` or `direct`) and a `detail`. A proxied fetch adds `proxy connect failed` (the proxy was not reached), `proxy target failed` (it was, and the issuer was not), `proxy status` and its family, such as `4xx` or `5xx` (the proxy answered the connect with anything but 200; a 302 logs `proxy status 3xx`), `proxy tls failed` (an https proxy's own handshake), `not the proxy` (cause `address_refused`) and `tls handshake failed`. Before any connect, `name not resolved` is an outage, `address refused` is a checked address Wardyn will not use, and `host not comparable` is an issuer host Wardyn cannot compare with the list (cause `address_refused`); `request failed` stands for any other transport error.
- After a proxy connect, target or status failure, Wardyn tries the next address it checked while addresses and time remain. On the proxy route with no CA file set, the detail is `no proxied issuer roots`.

#### Longer browser sessions

- Without `ui_session_renewal`, a person's browser-app session ends with the grant it was opened under, at the latest `WARDYN_UI_SANDBOX_SESSION_TTL` (default 8 hours) after the ticket was redeemed.
- With it, the session follows a chain: the grants this portal minted for that person while one of them was live. It ends at the redemption plus `WARDYN_UI_SANDBOX_SESSION_TTL` at the latest, and sooner when no grant of the chain is live.
- Every new request asks the chain again. It is refused once no grant of the chain is live, the portal is revoked, or the person or the portal's unit refuses. A stream that is already open never follows the chain: it ends at the end it was admitted with.
- Desktops (the sign-in desktop included), terminals and a helper's support sessions never renew, whatever the portal sets.
- Only an issuer portal can set it. A registration that sets it on a console portal answers 422 `delegate_renewal_needs_issuer`. This release has longer browser sessions switched on, so a registration is not refused `delegate_renewal_withheld`.

#### What a revoke or a suspension reaches

- A new request, and a new connection, is checked at its own opening with no cached answer: after a revoke of the portal, a suspension or purge of the person, or a change of the portal's unit that leaves the run outside the portal's new unit and the units under it, it is refused at once. A run still under the portal's unit passes.
- An open connection is re-checked on a beat, and ends at the first check that fails, or at the end it was admitted with. The design limits, counted from the event, are:

| Connection | Re-checked | Each check limited to | Ends within |
|---|---|---|---|
| Browser app, including a stream or upgrade | every 30 seconds | 5 seconds | about 35 seconds |
| Event stream | every 15 seconds | 5 seconds | about 20 seconds |
| Terminal | every 30 seconds | 5 seconds | about 35 seconds |
| Desktop | every 30 seconds | 10 seconds | about 40 seconds |

- These are limits the design sets, read from the code. They are not measured times.
- The person sees the connection drop with no clean close, and is refused when they reconnect.
- An event stream whose opening check fails answers 403 `revoked`; one that cannot be checked answers 503 `revocation_unavailable`. A ticket for a run that no longer exists is refused 403 `ui_gateway_ticket_invalid`.
- Every read that decides whether to open a session (the ticket, the run, the desktop's closed set, a terminal's opening) shares one 5-second budget and answers a retryable 503 when it gets no answer. An event stream whose run disappears during setup answers 404 `run_not_found`.
- Starting the sandbox's side of a connection is limited too. Wardyn must start a local operation or begin sending a remote start within 25 seconds of the check that admitted it; a remote runner can receive and execute it later. A write that never started passes no bytes. An app that needs longer than 25 seconds to start from cold can be refused with a retryable 503 even when nothing was revoked, and a retry may find it started. A write that began in time can still be interrupted with an unknown byte count, a terminal that needed a resume can answer 502 `attach_resume_failed`, and an app's launcher may never become ready. A session that opened in time is not cut by this.
- A request goes out on a connection of its own, never one shared with another request. A runner reached over a network link refuses a start whose time has passed at the link, and the runner needs no change for it. The bound ends where Wardyn hands the start to the link: a start handed over in time can still be delivered and run later on a busy runner.
- A terminal opened with a portal ticket attaches through the organisation. On the local route it answers 409 `attach_local_unavailable` with `detail` `issuer_portal`.
- Every browser-app or desktop connection the dispatch bound gates writes one `ui.dispatch` row when it closes; a terminal does not ([the audit reference](../AUDIT-ACTIONS.md)).

#### Suspend, reactivate and purge a portal's person

A person is one per issuer and provider id, so one suspension reaches them through every portal on that issuer. These routes are API only, and work only on a `delegate:person:` principal; any other principal is refused 422 `portal_person_only`.

1. Find them. `GET /people/portal-person?issuer=<exact issuer>&person_id=<their id at the provider>` answers `principal`, `state` (`active`, `deactivated` or `purged`), `display_email`, `deactivated_at`, `purged_at` and `portals_used`, a list of `{delegate, name, run_in_under, revoked_at}` that keeps revoked portals. `GET /people/{principal}/portals` answers the same shape. Neither carries `purge_after`. An issuer with no binding answers 404 `portal_issuer_not_bound` (check its spelling); a bound issuer with no such person answers 404 `person_not_found`, and asking creates nobody.
2. Suspend. `POST /people/{principal}/suspend` (admin or security admin) deactivates the person and deletes their grants in one step, so a grant stops at its next request, then sweeps their API tokens and SSH keys and kills every run of theirs. The body is optional: `purge_after_seconds` (0 to ten years, 0 or absent for none) schedules the permanent purge, and a positive value needs a super admin (403 `portal_purge_deadline_operator_only`).
3. If it answers 503 `portal_lifecycle_incomplete`, a step is still pending. Send the same request again. A sweep also retries pending suspensions and purges every 15 minutes, with or without SCIM set up. A backend failure that continues can leave the work pending across sweeps.
4. Look at what is stuck. `GET /admin/portal-lifecycle/status` (admin or security admin) answers `deactivated` (`person`, `deactivated_at`, `purge_after`), `pending` (`person`, `step`, `last_error`), `pending_suspensions` and `pending_purges`, naming each person by principal. It answers whether or not SCIM is configured. `GET /scim/status` stays the workforce's view: it lists only the people of its own SCIM issuer and tenant, and no portal person.
5. Undo it. `POST /people/{principal}/reactivate` (super admin) clears the deactivation and any purge date. Grants, tokens, keys and runs the suspension removed do not come back. A provider token issued before the suspension stays refused after the reactivation: its exchange answers 400 `invalid_grant`, audited `session_revoked`. A token issued after the suspension exchanges through a portal again, though the exchange can still be refused for any of its usual reasons.
6. Or make it permanent. `POST /people/{principal}/purge` (super admin) needs a suspended person (409 `person_not_suspended`, which is also the answer when a reactivation commits first) and cannot be undone: every portal refuses that person for good, and the same issuer and person id can never sign in again. A purge of a person already purged retries its pending steps. A purge at its scheduled date, by the sweep, writes no `person.purge` row.

Each answer reports `principal`, `state`, `deactivated_at`, `purge_after` and `purged_at`. A purged person answers 409 `person_purged` to a suspend or reactivate. The routes and their reasons are in [the SDK reference](../sdk.md); their audit rows are `person.suspend`, `person.reactivate`, `person.purge` and `identity.deactivate`.

#### Switch issuer portals on

Issuer portals are off in a new deployment. `POST /api/v1/admin/portal-issuers/activate` (admin only) switches them on, once, and nothing ever switches them off.

1. Stop every wardynd older than 0.10.2 that uses this database. An older server does not know the new tables, so it must not run beside one that does.
2. Wait for the server you call to have sampled the database's other sessions for two minutes. Until then the call answers 409 `portal_issuers_sample_warming` with a `Retry-After`.
3. Call it with `{"old_servers_stopped": true}`. Without that statement it answers 422 `portal_issuers_statement_required`.
4. If a session in the sample does not carry a label of 0.10.2 or later, the call answers 409 `portal_issuers_old_server_suspected` with `sessions`, a list of `{"label", "client_addr"}`. Stop them, or check each one and call again with `"confirm_listed_sessions": true`. The override audit row names them as `label@address`.
5. The answer is 200 with `activated` and `overridden_sessions`. A call after the switch is on answers 200 with `already_activated`.

- A 0.10.2 server labels each of its database sessions `application_name`: `wardynd/<version>/<replica>` for the server and `wardynd-<tool>/<version>` for a subcommand. A label of any other shape, a version before 0.10.2, or none is a suspect; a later version, or a prerelease of 0.10.2, is not.
- The sample reads the database's own session list every 10 seconds, so it sees only sessions that exist when it looks, and a label is only what the client chose. It is a veto of the statement, never proof that old servers are stopped.
- The call writes `portal_issuers.activate`, and `portal_issuers.activate_override` when sessions were confirmed past. After the switch each server checks hourly and writes `portal_issuers.old_client_seen` once per label and address per UTC day.
- A server that cannot read the session sample answers as still warming, never as clean.

#### Retire a binding, change an admission rule

- `DELETE /api/v1/admin/portal-issuers/{k}` retires an issuer binding that never admitted anyone. A binding that a portal, a person, a grant or a stored rule still names answers 409 `portal_issuer_in_use`.
- `PUT /api/v1/admin/delegates/{id}/admission` changes a portal's group, `portal_managed_admission` or required claim. The body names the rule you read as `expected`. A stale one answers 409 `delegate_admission_stale`.
- A change in the admission rule counts into `admission_rev`. An exchange that began under the old rule answers 503 `temporarily_unavailable`, with `portal_admission_changed` as its audit reason, and the portal retries under the new one.
- Both are admin only. With a second human required, a signed-in person's request is a held change that applies when approved. The break-glass admin token applies it directly and writes a `governance.change.bypass` row.

#### The console issuer

- A server with console sign-in configured records its console issuer at boot. Each console issuer a 0.10.2 server boots with stays recorded, so it can never become a portal's issuer on that deployment. The record starts empty at the upgrade: an issuer the console used only before it, or on a server that never booted 0.10.2, is not in it. Do not register a portal on an issuer the console has ever signed in against.
- Boot refuses to start when the console issuer equals a portal binding's issuer. The message names the portal registrations on that binding, by id and name with revoked ones marked, up to five and then `and N more`. When none names it, it says so.
- On a split-role install the app role needs `SELECT, INSERT` on `console_issuers` before that boot step can run: see [the migration list](../OPERATIONS.md#splitting-the-migrator-and-app-roles-wardyn_pg_migrate_dsn).
