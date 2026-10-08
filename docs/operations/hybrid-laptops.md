> Part of the [Operations](../OPERATIONS.md) split (task pages under `docs/operations/`).

# Managed laptops: hybrid enrolment and audit federation

- This is the org-side half of [DESKTOP.md's Enrolling into an org control plane](../DESKTOP.md#enrolling-into-an-org-control-plane): an org control plane this Helm chart or compose stack runs can enrol member-mode laptops (topology m′) and receive their audit rows.

## Minting a token

> [!NOTE]
> It's issue #103's phase-one seam, not the full hybrid rollout — no run places on the org cluster because a laptop enrolled.
> See [docs/design/hybrid-0.8.md](../design/hybrid-0.8.md) for what is and isn't built.

1. Call `POST /api/v1/admin/devices/enrolment-tokens` (admin) with the device's name. This mints a single-use token for that one device.
2. The token is returned once and stored only as a hash — keep it wherever your MDM staging step reads it from.
3. It expires 72 hours after minting (`deviceEnrolmentTokenTTL`, [`internal/api/devices.go`](../../internal/api/devices.go)), so mint it close to when the laptop will first boot.
4. Deliver it as `WARDYN_ORG_ENROLMENT_TOKEN` in the laptop's `secret.env`, beside `WARDYN_ORG_URL` pointed at this control plane.
5. The audit row is `device.enrolment_token.create` ([AUDIT-ACTIONS.md](../AUDIT-ACTIONS.md)).

## Inventory and revocation

| Action | Route | Who |
| --- | --- | --- |
| List enrolled devices | `GET /api/v1/admin/devices` | Security admin |
| Revoke a device | `DELETE /api/v1/admin/devices/{id}` | Security admin |

- There is no console page for inventory yet — script against the endpoint, or read `device.enrol` audit rows.
- The admin-then-security-admin tiering matches every other inventory-then-revoke surface this document uses.

Revoking a device:

1. Its next push or heartbeat is answered `401`.
2. The laptop's own forwarder records that as a durable local mark.
3. Every run-creating path on that laptop answers `503` from then on, org reachable or not, until it is re-enrolled with a fresh `WARDYN_ORG_ENROLMENT_TOKEN`.

- A second revoke of an already-revoked device is a `404` and writes no row.
- `device.revoke` is the audit row.

## Watching federation lag

- Each enrolled device's forwarder pushes its local audit table upward every 15s from a durable cursor.

| Signal | Where | Meaning |
| --- | --- | --- |
| `wardyn_org_federation_lag` | That laptop's own metrics (present only when `WARDYN_ORG_URL` is set on it — see [Monitoring](monitoring.md)) | Local rows the organisation has not yet acknowledged |
| `org_federation.lag` | That laptop's `/healthz` | The same value, human-readable |

- Whenever forwarding isn't advancing, lag grows at the rate the laptop writes new audit rows.
- That's true whether the organisation is unreachable or has refused a batch, since the forwarder re-reads the local head on every tick.
- Lag alone can't tell the two apart. Once forwarding resumes, the backlog drains and the gauge falls.

#### A refusal's evidence is what tells them apart

- **On this side**, each refused batch writes `device.audit.ingest` failure rows: `reason` is `invalid_body` or `batch_too_large` at `400`/`413`, `invalid_row` at `400`, or `chain_mismatch`/`org_run` at `422` (see [AUDIT-ACTIONS.md's Devices table](../AUDIT-ACTIONS.md#devices-hybrid-enrolment)).
- **On the laptop**, `wardynd` logs `forwarding is halted until wardynd restarts` at ERROR.
- **An unreachable organisation** produces neither signal.

- The laptop's forwarder halts pushing on a definitive refusal, and only a `wardynd` restart on the laptop retries it.
- Growing lag with matching ingest failures is an operator page, not a network blip to wait out.

## Device credential scope

A device credential authenticates nothing but that device's own ingest routes:

- `deviceAuth` ([`internal/api/devices_auth.go`](../../internal/api/devices_auth.go)) resolves the `wdd_`-prefixed bearer to a device identity scoped to `/api/v1/devices/{id}/*`.
- It never resolves to an operator or a member, so a stolen device credential cannot create a run, read a workspace or reach any other admin surface.
- It can only forge audit rows *about that one device* until it is revoked. See threat-model residuals #50–#52.
