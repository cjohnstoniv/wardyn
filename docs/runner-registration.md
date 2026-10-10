# Register a runner

A runner holds its own Ed25519 key. Registration creates an **unclaimed** runner: it cannot receive a run or relay traffic until its owner confirms the fingerprint. The organisation must configure `WARDYN_RUNNER_ORG_URL` with its public HTTPS URL; HTTP is accepted only for loopback development.

1. Sign in under your personal account and run `wardyn runner token`. The token is printed once to stdout and expires after one hour. Save it in a private file for the runner.
2. On the runner host, run `wardyn-runnerd register --org https://wardyn.example.com --token-file ./registration-token --name my-laptop`. The runner generates its key locally and prints its fingerprint.
3. Under that same owner's personal account, run `wardyn runner claim --url https://wardyn.example.com`. The CLI reads the local runner identity and confirms its fingerprint. An administrator's personal account can claim its own runner; administrator credentials cannot claim for another person.

An operator can mint for a named person with `wardyn runner token --owner <principal>`. That token expires after 72 hours and still requires the named owner's fingerprint claim. Every token is single-use, including self-minted tokens. An unclaimed runner expires after 24 hours.

The default state directory is `wardyn/runner` beneath the operating system's user configuration directory. Pass the same `--state-dir` to registration, serving and claim when overriding it. The key and identity metadata are regular files with mode `0600` in a private directory. Loading refuses an unsafe key file, a changed fingerprint or a different organisation URL.

Registration never overwrites an existing identity. If registration fails after key creation, obtain a fresh token and use a new private state directory. A revoked runner must register a new key and complete a new owner claim; reusing its old key does not reactivate it. Delete the single-use token file after registration.

The API doors are `POST /api/v1/me/runners/tokens`, operator-only `POST /api/v1/runners/tokens`, anonymous token redemption at `POST /api/v1/runners/register`, and owner-only `POST /api/v1/me/runners/{id}/claim`. Registration responses carry a runner id, fingerprint, state and organisation binding, never a human identity. Inventory and revocation use the separate runners management surface.

Migration `0142_runner_registration` adds hashed single-use registration tokens and organisation URL bindings to the existing runners table. It does not change run placement records.

## Runner enablement

| Stored configuration | Admission |
|---|---|
| `runners` absent or `runners.enabled` false | Refused before registration, mint, or claim effects. |
| Site configuration unreadable | Refused; retry after configuration recovers. |
| `runners.enabled` true | Identity and route authorization checks still apply. |
| Generic `PUT /site-config` | Retains stored runner settings; cannot enable or disable runners. |

An administrator turns runners on at **Admin → Runners**, or with `PUT /api/v1/runners/settings {"enabled": true}`; only a super admin can, and each change is audited as `runners.enabled.set`. Turning on is refused until `WARDYN_RUNNER_ORG_URL` (and the control-plane URL) is HTTPS; `http` is accepted only for localhost. Turning off is never refused. Off stops new registrations, claims and stream connections; a runner that is already connected stays connected until it reconnects. Ending sessions on the switch (`Config.RunnersDisabled`) and re-reading it at run placement belong to the runner hub's wiring and are not in place yet. `PUT /site-config` names `runners` only to echo the stored value: a different one is refused with `site_config_runners_via_own_route`.

## Runner inventory and unused tokens

| Route | Who | Answers |
|---|---|---|
| `GET /api/v1/runners?state=active\|revoked\|all&limit=&offset=` | admin or `security_admin` | Every person's runners, owner named; default `active` (unclaimed and claimed). `online` is the live session, and `runs_active` counts non-terminal runs on the runner. |
| `GET /api/v1/runners/{id}` | admin or `security_admin` | One runner, revoked included. |
| `GET /api/v1/me/runners`, `GET /api/v1/me/runners/{id}` | the owner | The caller's own runners only; another person's runner and an absent id answer alike. An unclaimed runner's fingerprint is abbreviated here, so it cannot be copied from the console into the claim. |
| `GET /api/v1/runners/tokens?owner=&limit=&offset=` | admin or `security_admin` | Unused, unexpired registration tokens, optionally one person's. No token value is ever returned. |
| `DELETE /api/v1/runners/tokens/{id}` | admin or `security_admin` | The token stops being redeemable at once; audited as `runner.token.revoke`. Minting stays admin only. |
| `GET /api/v1/runners/settings` | admin or `security_admin` | `{"enabled": bool}`, readable while the runner routes refuse. |

Both lists are paged and set `X-Wardyn-Truncated` when more exist. An unclaimed runner older than 24 hours is not listed, and the periodic sweep deletes it, freeing its key to register again. On a person's own list `minted_by_email` names the minter when someone else minted the token and an email is held. The runner views report what a runner said about itself as reported, never as verified.

`wardyn runner list` (`--all` for every person's runners) and `wardyn runner tokens list [--owner]|revoke` use these routes.

Migration `0191_runner_minted_by` records on each runner who minted the registration token it redeemed, so the owner's list can tell a token they made from one an administrator made for them.
