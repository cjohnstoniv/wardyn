> Part of the [Operations](../OPERATIONS.md) split (task pages under `docs/operations/`).

# Managed laptops: client-mode runners

- In 0.9 a laptop joins an org as a **client-mode runner**: it runs `wardyn-runnerd`, not a control plane, and the person who owns it claims it.
- Design: [docs/design/0.9/PLAN.md](../design/0.9/PLAN.md). Org enrolment of a full daemon (`WARDYN_ORG_URL`) still works in 0.9, logs a deprecation warning at boot, and is removed in 1.0; see [Legacy org enrolment](#legacy-org-enrolment-deprecated).

> [!NOTE]
> The runner daemon and its routes land in the 0.9 lanes. Until a build carries them, this page describes the contract, not a binary you can install.

## Where each thing lives

| Concern | Org | Laptop |
| --- | --- | --- |
| Scheduling, placement, the ceiling | Yes, the only scheduler | Nothing decides locally |
| Credentials | Held in the org; the laptop can use a `via_org` credential for a run's life | Only what the person's own keystore already holds |
| Audit | The org is the only writer; every local run is an org run record | The runner carries bytes and spools rows while offline |
| Control plane, Postgres, OIDC client, `age.key` | Yes | None of them |
| Sandbox, proxy sidecar, local Docker | No | Yes, driven through `wardyn-runnerd` |
| The runner's private key | Never seen | `runner.key`, `0600`, never leaves the host |

## Claiming a runner

1. The owner (or an admin, for a named person) mints a single-use `wdr_` registration token. The owner's token lasts 1 hour; an admin-minted one lasts 72 hours so MDM can deliver it.
2. On the laptop, `wardyn-runnerd register --org <url> --token-file <file>` generates an Ed25519 key pair locally and prints the key fingerprint.
3. The org records an `unclaimed` runner. It is offered no run and relays nothing, and it expires after 24 hours.
4. The owner, signed in as themselves, runs `wardyn runner claim` (or confirms the fingerprint in *My runner*). The claim succeeds only for the token's owner and only when the fingerprint matches.
5. The runner is now `claimed` and accepts runs owned by its owner.

- An admin can mint a token for a person but can never complete the claim.
- A stolen token shows up in *My runner* as a runner the owner did not register.

## Offline and revocation

| Situation | Behaviour |
| --- | --- |
| Link down at run creation | Refused `runner_offline`; no run row exists |
| Link drops mid-run | Nothing is torn down; the proxy enforces its cached policy and spools decisions |
| Link down past the lapse (about 1h5m) | The runner stops the proxy itself and keeps the agent and files |
| Revoked by the owner or a security operator | The org marks that runner's runs lost and runs the kill cascade; the runner stops every governed proxy |

"Offline" and "ungoverned" are never the same thing: an offline runner is still governed and runs nothing new.

## Legacy org enrolment (deprecated)

- A full `wardynd` in member mode (topology m′) can still enrol into an org with `WARDYN_ORG_URL` and `WARDYN_ORG_ENROLMENT_TOKEN`; see [DESKTOP.md](../DESKTOP.md#enrolling-into-an-org-control-plane).
- Moving to a client-mode runner removes the laptop's own control plane and its audit chain.

| Action | Route | Who |
| --- | --- | --- |
| Mint a single-use token (72 hours) | `POST /api/v1/admin/devices/enrolment-tokens` | Admin |
| List enrolled devices | `GET /api/v1/admin/devices` | Security admin |
| Revoke a device | `DELETE /api/v1/admin/devices/{id}` | Security admin |

- The token TTL is `deviceEnrolmentTokenTTL` in [`internal/api/devices.go`](../../internal/api/devices.go); the audit rows are `device.enrolment_token.create`, `device.enrol` and `device.revoke` ([AUDIT-ACTIONS.md](../AUDIT-ACTIONS.md#devices-hybrid-enrolment)).
- A revoked device is answered `401`, and every run-creating path on that laptop answers `503` until it is re-enrolled.
- `deviceAuth` ([`internal/api/devices_auth.go`](../../internal/api/devices_auth.go)) resolves a `wdd_` bearer to routes under `/api/v1/devices/{id}/*` only; a stolen credential can forge audit rows about that one device and nothing else (threat-model residuals #50–#52).
- Federation lag shows as `wardyn_org_federation_lag` on the laptop ([Monitoring](monitoring.md)) and `org_federation.lag` on its `/healthz`. A refused batch writes `device.audit.ingest` failure rows and halts the forwarder until `wardynd` restarts; an unreachable org produces neither.
