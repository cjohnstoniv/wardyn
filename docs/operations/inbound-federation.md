> Part of the [Operations](../OPERATIONS.md) split (task pages under `docs/operations/`).

# Inbound federation

A machine that already holds a token from its organization's own identity provider, such as a CI job's OIDC token,
swaps it at Wardyn's door for a short-lived [inbound token](api-tokens.md#inbound-tokens) of one service identity.
It needs no Wardyn credential. It is off until an admin or a security admin, signed in, turns it on.

## Where to start

| You want to | Do |
|---|---|
| Turn it on or off | [The switch](#the-switch) |
| Let one workload in | [Add a trust](#add-a-trust) |
| Swap a token | [Exchange a token](#exchange-a-token) |
| See, remove or confirm the trusts of an identity you sponsor | [The sponsor's view](#the-sponsors-view) |
| Read a refusal | [Answers](#answers) |

## Before you start

- Single sign-on and the Postgres store. In local mode the exchange answers `404` and the other routes `409` `needs_sso`.
- A [service identity](api-tokens.md#service-identities) whose sponsor's sessions are live.
- That identity named as a caller, `service:<name>`, on the runs it must reach ([Inbound](../UI-SANDBOXES.md#inbound-let-named-callers-reach-a-port)).
- At least one of this Wardyn's own addresses set: `WARDYN_OIDC_REDIRECT_URL` or `WARDYN_RUNNER_ORG_URL`.
- `WARDYN_TRUSTED_PROXIES` set to your ingress, so the exchange's rate limit counts each caller on its own ([Ingress rate limits](ingress-rate-limits.md)).

| Who | May |
|---|---|
| An admin or a security admin, signed in to the console | turn the switch on and add trusts. An API token or the admin token is refused. Under four-eyes a second admin, also signed in, approves each |
| An admin or a security admin, with any credential | turn the switch off, and list and delete trusts |
| The identity's sponsor | list and delete that identity's trusts; confirm one again, signed in |
| Anyone who can reach the door | exchange a provider token that a trust matches |

Four-eyes is `WARDYN_GOVERNANCE_SECOND_HUMAN` ([Four-eyes on governance writes](governance-writes.md)). Turning the
switch off and deleting a trust are never held: they only stop tokens. An approval by anyone who is not a signed-in person is refused
`403` `inbound_federation_needs_person`.

## The switch

1. Turn it on from your signed-in console session: `PUT /api/v1/inbound-federation` with `{"enabled":true}`. An API token or the admin token is refused `403` `inbound_federation_needs_person`.
2. Under four-eyes the answer is `202` with the pending change's `Location`, and the switch turns on once a second admin approves it from a signed-in session.
3. Read it: `GET /api/v1/inbound-federation` answers `enabled`, `updated_at` and `updated_by`.
4. Turn it off with `{"enabled":false}`, with any admin credential, an API token included, even when it is already off. Every token exchanged under any trust is revoked at once, and `tokens_revoked` counts them. A pending turn-on is cancelled, and `changes_cancelled` names it. The trusts stay.

## Add a trust

A trust matches one exact issuer, audience and subject, and names the one service identity a matching token becomes.

| Field | Required | Rule |
|---|---|---|
| `name` | yes | 3 to 63 of `a-z`, `0-9` and `-`, starting with a letter |
| `issuer` | yes | the provider's `iss`, an `https` URL, byte for byte |
| `internal_issuer` | no | the address Wardyn fetches discovery and keys from, when it is not `issuer`; its host must be on `WARDYN_PORTAL_INTERNAL_ISSUER_HOSTS` |
| `audience` | yes | the token's one `aud`: one of this Wardyn's own addresses (below) |
| `subject` | yes | the token's `sub`, byte for byte |
| `required_claim`, `required_claim_value` | no, both or neither | one more string claim the token must carry with exactly that value |
| `service_identity` | yes | the identity's name |
| `token_ttl_seconds` | no | 60 to 3600, default 900: the longest an exchanged token lives |
| `max_token_age_seconds` | no | 60 to 3600, default 300: how old the provider token's `iat` may be, less 5 seconds |
| `max_live` | no | 1 to 1000, default 100: live exchanged tokens this trust may hold |

**The audience is this Wardyn's own address.**
- It is the origin of `WARDYN_OIDC_REDIRECT_URL` or `WARDYN_RUNNER_ORG_URL`: `scheme://host`, plus `:port` when it is not the default. The scheme and host are lower-case, with no path and no trailing slash.
- The UI-sandbox address, `WARDYN_UI_SANDBOX_ADVERTISE`, is never one: the apps of every run can share its origin.
- With `WARDYN_OIDC_REDIRECT_URL=https://wardyn.example.com/auth/callback`, the audience is `https://wardyn.example.com`.
- With `WARDYN_BASE_PATH` set, the origin is followed by the base path: with `/wardyn`, the audience is `https://wardyn.example.com/wardyn`. The bare origin is refused, since every service behind that host shares it.
- Ask your provider for a token with that audience. A token made for any other service never carries it.
- The exchange checks it again, so a trust stops working once its audience is no longer one of these addresses, or once a live portal's client id or audience holds it, whichever was registered first. It answers the one `401`, and the denied row's reason is `audience_not_this_deployment` or `audience_reserved`.

**Refused** with `400` `inbound_trust_reserved`:
- an issuer on one of this Wardyn's own hosts, a run's own UI-sandbox host (`WARDYN_UI_SANDBOX_ORIGIN_TEMPLATE`) included;
- an audience that is not one of this Wardyn's own addresses;
- an audience a provider gives many services: `sts.amazonaws.com`, `api://AzureADTokenExchange`, the Kubernetes API's own, or GitHub's default `https://github.com/<owner>`;
- an audience equal to the issuer, the console sign-in client id, or a live portal's client id or audience.

A `*` anywhere is refused `400` `inbound_trust_invalid`: there are no wildcards. One trust holds each name, and each issuer,
audience and subject, and a second is `409` `inbound_trust_exists`.

From your signed-in console session, `POST /api/v1/inbound-trusts` with:

```json
{"name":"deploy-main","issuer":"https://token.actions.githubusercontent.com","audience":"https://wardyn.example.com","subject":"repo:example-org/app:ref:refs/heads/main","service_identity":"deployer"}
```

- The answer is `201` with the trust, or `202` under four-eyes. An API token or the admin token is refused `403` `inbound_federation_needs_person`.
- `GET /api/v1/inbound-trusts` lists every trust and `GET /api/v1/inbound-trusts/{id}` reads one.
- `DELETE /api/v1/inbound-trusts/{id}` removes one and revokes the tokens exchanged under it.
- A trust cannot be edited: delete it and add a new one.

## Exchange a token

```sh
curl -X POST -H 'Content-Type: application/json' \
  -d "{\"subject_token\":\"$PROVIDER_TOKEN\"}" "$WARDYN/api/v1/inbound/exchange"
```

- While the switch is off the answer is `404` `inbound_federation_off`, before the provider token is verified.
- The answer is `201` with `token`, `expires_at` and `service_identity`. The token is shown once.
- `ttl_seconds` is optional. It is lowered to the trust's `token_ttl_seconds` and to `WARDYN_API_TOKEN_MAX_TTL`. A negative value, or one past the API token bound, is `400`.
- The token never outlives the provider token: `expires_at` is at most the provider token's `exp`.
- The token is an [inbound token](api-tokens.md#inbound-tokens) of `service:<name>`. It calls the inbound ports of runs that name the identity ([Call it with a token](../UI-SANDBOXES.md#call-it-with-a-token)), and nothing on this API.
- There is no refresh: exchange a fresh provider token before `expires_at`.
- The provider token is checked with RS256 only. It must have exactly one audience and an `iat`, and it must not be expired.
- A trust holds at most `max_live` live exchanged tokens. They do not count against the identity's cap of `20`.
- The same provider token can be exchanged again while its age allows, and each swap counts against `max_live`.
- Each client address gets 10 attempts, then one every 10 seconds. A successful exchange is refunded.

## The sponsor's view

- `GET /api/v1/me/service-identities/{name}/inbound-trusts` lists the identity's trusts, each with `confirmed_at` and `confirmed_by`.
- `DELETE /api/v1/me/service-identities/{name}/inbound-trusts/{id}` removes one and revokes its tokens.
- `POST /api/v1/me/service-identities/{name}/inbound-trusts/{id}/confirm`, from your signed-in console session, confirms a trust again.
- A revoke of all your sessions, or of everyone's, stops every trust of the identities you sponsor until you confirm each one. Signing out does not.
- Anyone but the sponsor gets the `404` of a missing identity.
- The audit row of each trust create, and of each exchange, names the sponsor.

## What ends an exchanged token

| Event | Effect |
|---|---|
| Its trust is deleted, by an admin or the sponsor | its tokens are revoked at once |
| The switch is turned off | every exchanged token is revoked |
| The identity's sponsor changes, or the identity is retired | its trusts are deleted and its tokens revoked; the audit row's `trusts_removed` counts the trusts |
| The identity is suspended, or its sponsor is deactivated | refused on the next call, as for any service token |
| The sponsor's sessions are revoked in full, or everyone's are | refused on the next call. The exchange answers `409` `inbound_trust_unconfirmed` until the sponsor confirms each trust again |
| `expires_at` passes | refused |

A trust delete and the switch stop at once on the replica that made the change; every other event, and every other
replica, within 30 seconds.

## Answers

| Status | Reason | When |
|---|---|---|
| `201` | — | swapped |
| `400` | `invalid_request_body` | the body is not one JSON object, or is over 16 KiB |
| `400` | `api_token_ttl_invalid` | `ttl_seconds` is negative, or past the API token bound |
| `401` | `inbound_subject_token_invalid` | every refused provider token, one answer: no trust matches, the signature or algorithm, the audience, subject or claim, its age, an audience no longer this Wardyn's, or an audience a live portal holds |
| `404` | `inbound_federation_off` | the switch is off, the server runs in local mode, or the store keeps no trusts |
| `409` | `service_identity_inactive` | the identity is suspended or retired, its sponsor is deactivated, or the deployment has no revocation store |
| `409` | `inbound_trust_unconfirmed` | the sponsor's sessions were revoked in full, or everyone's were, after the trust was last confirmed |
| `422` | `api_token_cap_reached` | the trust holds `max_live` live tokens |
| `429` | `inbound_exchange_rate_limited` | this address spent its attempts; `Retry-After: 10` |
| `503` | `inbound_issuer_unavailable` | the trusted provider cannot be reached; `Retry-After` |
| `503` | `token_lookup_unavailable` | Wardyn's own store cannot be read |

On the trust and switch routes, `403` `inbound_federation_needs_person` answers an API token, the admin token or a
delegated request that turns the switch on, adds a trust or confirms one. It also answers an approval by anyone who is
not a signed-in person. `202` answers a write held for a second admin. A trust cannot be added or confirmed when its
sponsor's revocation order reads below zero: that answers `503` `token_lookup_unavailable`, and nothing is clamped.

## Audit

| Action | Recorded |
|---|---|
| `inbound.exchange` | each swap, as the service identity, with the trust, issuer, subject, sponsor, expiry and the caller's address. A refusal is `denied` with its reason, written at most as often as `auth.fail` |
| `inbound_trust.create` | the trust's fields and the identity's sponsor |
| `inbound_trust.confirm` | the sponsor's confirm of a trust |
| `inbound_trust.delete` | the trust, the tokens revoked, and `by_sponsor` |
| `inbound_federation.update` | the switch and the tokens revoked |

Neither the provider token nor the exchanged token is ever recorded. An exchanged token's own row is deleted by a
later exchange under the same trust, at the earliest a day after it expires, or, once it has expired, when its trust is removed. Its
`inbound.exchange` row is the record. See [Audit actions](../AUDIT-ACTIONS.md).

## Limits

- RS256 only: a provider that signs with ES256 or anything else is refused.
- One trust per exact subject. A workload whose subject changes per run (a per-job id) cannot be trusted by name.
- While a trusted provider is down, the exchange answers `503` only for a token whose issuer, audience and subject match a trust. At any time, a matching token also takes longer to answer, since its signature is checked. A caller who knows those three values can learn that the trust exists, and gets no token ([residual](../../threatmodel/THREAT-MODEL.md#5-out-of-scope--published-residual-risks)).
- Whoever controls the trusted provider, or the workload a trust's subject names, calls as the identity until the trust is deleted or the switch turned off ([residual](../../threatmodel/THREAT-MODEL.md#5-out-of-scope--published-residual-risks)).
- A captured provider token can be exchanged again until its age limit, and can fill the trust's `max_live` until those tokens expire.
- With four-eyes off, one signed-in admin turns the switch on and adds a trust alone.
- Erasing a person clears their name from the trusts they added or confirmed, and from the switch. The trusts stay.
- A revoke of the sponsor stops a trust until the sponsor confirms it; it does not delete it.
- Tokens are capped per trust (`max_live`). Trusts per identity are not capped, and only an admin or a security admin adds one.
- The rate-limit buckets are per replica.
