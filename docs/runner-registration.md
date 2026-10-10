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

> [!IMPORTANT]
> The dedicated audited administration operation and HTTPS enablement validation remain an integration prerequisite. No supported enablement operation ships in this registration slice.
