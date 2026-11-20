> Part of the [Operations](../OPERATIONS.md) split (task pages under `docs/operations/`).

# Signing people in to a custom model endpoint

- A `custom_endpoint` model provider can sign each person in to the endpoint's own identity service, instead of asking each person to paste a token.
- Each person signs in once from the console. Wardyn keeps their refresh token sealed under their own name and hands a run only a short-lived access token, through the run's proxy.
- This page is for the admin who sets up the provider, and the person who signs in.

## Set it up

1. Register a client with the endpoint's identity service. Use the code flow with PKCE. Do not give it client-credentials; Wardyn sends only `authorization_code` and `refresh_token` grants.
2. Register one redirect URL on that client: the console's origin plus `/api/v1/auth/provider-oidc/callback`, under the base path if you set one. It sits beside the console's own sign-in callback.
3. In Settings, edit the `custom_endpoint` provider and add an `oidc` block with the fields below.
4. Ask a person to sign in from the console, then launch a run on the provider.

| Field | Required | Meaning |
|---|---|---|
| `issuer` | Yes | The authority. Wardyn discovers the sign-in and key-set addresses from it and, unless `require_id_token` is `false`, checks the identity token against it |
| `authorize_url`, `token_url` | No | Override what discovery returns |
| `client_id` | Yes | 1 to 256 printable characters, no spaces |
| `client_secret_ref` | No | The name of an operator secret holding the client secret. Leave it out for a public client |
| `scopes` | Yes | 1 to 32 scopes, each one word and none ending in `.default`. `offline_access` is always added, and so is `openid` unless `require_id_token` is `false` |
| `identity_binding` | No | `console` (the default) or `none` ([Who the sign-in is for](#who-the-sign-in-is-for)) |
| `require_id_token` | No | `true` (the default) or `false`. `false` is for an authority that never grants `openid` ([When the authority does not grant `openid`](#when-the-authority-does-not-grant-openid)) |

- Every address is `https` with no user information. A private address follows the same rule as any admin-typed model address (`internal_hosts`).
- PKCE with an S256 challenge is always on. It is not a setting.
- A row with an `oidc` block takes no pasted token. A paste is refused with "signs people in, so a token cannot be pasted for it. Use Sign in".
- **An edit purges every sign-in.** A change to the issuer, either address, the client id, the scopes, the binding or `require_id_token` deletes every person's stored sign-in for the row. Everyone signs in again.
- The sign-in needs the console's own OIDC login. Without it, the door answers `endpoint_oidc_unconfigured`: "This deployment offers no endpoint sign-in".

## Who the sign-in is for

| `identity_binding` | What Wardyn checks | Use it when |
|---|---|---|
| `console` (default) | The identity token's issuer is the one the console signs people in with, and the person it names is the person signed in to this browser | The endpoint trusts the same identity service as the console. This is the strongest tie between the platform account and the Wardyn person |
| `none` | Nothing. The sign-in records the authority's issuer and subject, so a review can see which account a person connected. With `require_id_token: false` nothing is recorded, so a review cannot see which account was connected | The endpoint uses a different identity service. It is no stronger than a pasted token |

- Wardyn never matches on an email or a user name. Those can be changed by an admin.
- A `console` sign-in that fails the check is refused. The reason is one of two:
  - "The identity token was issued by an authority other than the one the console signs you in with";
  - "The identity token is not for the person signed in to this browser session".

## When the authority does not grant `openid`

Some identity services never grant `openid` to a client, so no identity token is ever issued and the sign-in fails. Set `require_id_token: false` on the `oidc` block to sign in against one.

- It is allowed only with `identity_binding: none`. The `console` binding matches the person by their identity token, so it keeps requiring `openid`, and a save with `false` and `console` is refused with a 400.
- It is also refused when `scopes` names `openid`, because a row that asks for no identity token cannot ask for `openid`.
- With `false`, Wardyn does not request `openid`. A token response with no identity token is accepted. If the authority sends one anyway, Wardyn ignores it: it is not verified, stored or logged.
- The authority must still publish its OIDC discovery document with a usable `jwks_uri`. Only the `openid` grant is not needed.
- No issuer or subject is recorded for the sign-in. The tokens are stored under the signed-in console person, as for any other sign-in. State, PKCE, the person binding of the browser session and the one-use callback are unchanged, and so are the authority and TLS checks.
- Renewal works as it does for any other sign-in.
- Turning `require_id_token` on or off is an address change: it deletes every person's stored sign-in for the row, and a sign-in that was started or stored under the other setting is refused. Leaving it out is the same as `true`, and a row that never set it keeps its sign-ins.
- The capture audit row records `id_token: not_required` for such a row, whether or not a token came back ([Audit actions](../AUDIT-ACTIONS.md)).
- In this release the field is set through the API and the policy file. The console editor keeps a value set that way when an unrelated change is saved, and shows no control for it.

## How a person signs in

1. The person starts from a signed-in console session: `GET /api/v1/model-providers/<uid>/oidc/sign-in`. An admin token cannot, because the sign-in is stored under a person.
2. Wardyn finds the authority, sets short-lived cookies for the state, nonce and verifier, and sends the browser to the authority.
3. The authority sends the browser back to the callback. Wardyn checks the cookies, the row and, unless `require_id_token` is `false`, the identity token, then stores the refresh token.
4. The console returns to `/?provider_oidc=connected`. A failure returns to `/?provider_oidc_error=<reason>`.

| Reason on a failed return | What to do |
|---|---|
| `row_changed` | An admin changed or removed the provider while you signed in. Start again |
| `consent_required` | The authority has not consented to this client for these scopes. An admin of the authority grants them |
| `interaction_required` | The authority wants the person to do something more. Sign in again |
| `exchange_failed` | The authority could not be reached or refused the code. Try again, then ask the admin to check the issuer and client |
| `store_error` | Wardyn could not store the sign-in. Try again |

- A disconnect deletes the stored sign-in. If discovery lists a revocation address, Wardyn also asks the authority to revoke the token, once, and ignores a failure.
- The console says a sign-in is due: "Sign in to `<name>`" when there is none, and "Your sign-in to `<name>` has ended — sign in again." when the last renewal found it dead. It shows no countdown, because identity services publish no refresh-token lifetime.

## What a run gets

- The run starts only if its owner has a sign-in. Otherwise Wardyn refuses the launch with "you are not signed in to its endpoint" or "your sign-in to its endpoint has ended and needs a new one".
- The sandbox holds only a placeholder. The run's proxy asks the control plane for the real access token and adds it to requests to the provider's own host, over TLS.
- The access token is never stored. The refresh token never leaves the sealed `wardyn-provider-<uid>-oidc` secret, and no grant can name it ([Secrets and keys](secrets-and-keys.md)).
- The token is the run owner's own. The control plane checks that the run's owner, the provider, its address and its harness are still the ones the run launched with. If any changed, it refuses with "Relaunch the run."

## How it renews

- The proxy asks for a fresh access token about every 10 minutes, or sooner if the token's own expiry is sooner ([`storedKeyTTL`](../../internal/api/injection.go)). A run is renewed unattended for as long as the refresh token lives.
- Each renewal can rotate the refresh token. It is saved under a lock and a revision check, one renewal at a time per person.
- A renewal sends the refresh token only to the token URL and provider address it was signed in at. If either moved, the run gets the no-sign-in answer and the stored sign-in is kept.
- A disconnect stops the injection within one renewal, so a disconnected person's token is injected for at most 10 minutes.
- Disconnecting a sign-in made before this release deletes it here but does not revoke it at the authority.

| Renewal fails because | What the run sees | What to do |
|---|---|---|
| The sign-in no longer renews | "can no longer be renewed — sign in again, then relaunch". A sign-in request is held on the run | The person signs in again. A held run resumes when the sign-in lands |
| The authority has not consented | "an administrator of the endpoint must grant them, then relaunch" | An admin of the authority grants the scopes |
| The authority wants the person present | "refused to renew the sign-in without the person present — sign in again" | Sign in again. Wardyn renews from the control plane and cannot meet a device or fresh-challenge rule |
| The authority did not answer | "did not complete; nothing about the credential is known to be wrong" | Wait, or retry |
| The secret store refused | "sign in again, or ask an administrator to check the store" | Check the store's access |
| The provider changed after launch | "Relaunch the run." | Relaunch |
| The sign-in was made before this release, or its address or token URL moved | "has not signed in to this model provider's endpoint — sign in from the console, then relaunch". Nothing is sent and the stored sign-in is kept | The person signs in again once |

- **A residual: reuse detection.** If saving a rotated refresh token fails, Wardyn serves the renewal from memory. An authority that revokes a token family on reuse then revokes the person's session on the next renewal. It fails closed and the person signs in again.
- Whether an authority's own rate limits bind at one renewal per run every 10 minutes has not been measured.
