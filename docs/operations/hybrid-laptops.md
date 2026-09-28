> Part of the [Operations](../OPERATIONS.md) split (task pages under `docs/operations/`).

# Managed laptops: hybrid enrolment and audit federation

This is the org-side half of [DESKTOP.md's Enrolling into an org control
plane](../DESKTOP.md#enrolling-into-an-org-control-plane): an org control plane
this Helm chart or compose stack runs can enrol member-mode laptops
(topology m′) and receive their audit rows. It is issue #103's phase-one
seam, not the full hybrid rollout — no run ever places on the organisation's
cluster because a laptop enrolled; see [docs/design/hybrid-0.8.md](../design/hybrid-0.8.md)
for what is and is not built.

**Minting a token.** `POST /api/v1/admin/devices/enrolment-tokens` (admin) mints
a single-use token for one named device; it is returned once and stored only as
a hash, so keep it wherever your MDM staging step reads it from. It expires 72
hours after it is minted (`deviceEnrolmentTokenTTL`, `internal/api/devices.go`),
so mint it close to when the laptop will first boot. Deliver it as
`WARDYN_ORG_ENROLMENT_TOKEN` in the laptop's `secret.env`, beside `WARDYN_ORG_URL`
pointed at this control plane. `device.enrolment_token.create` is the audit row
([AUDIT-ACTIONS.md](../AUDIT-ACTIONS.md)).

**Inventory.** `GET /api/v1/admin/devices` (security admin) lists enrolled
devices — the admin-then-security-admin tiering matches every other
inventory-then-revoke surface this document uses. There is no console page for
it yet; script against the endpoint or read `device.enrol` audit rows.

**Revoking a device.** `DELETE /api/v1/admin/devices/{id}` (security admin).
Its next push or heartbeat is answered `401`, which the laptop's own forwarder
records as a durable local mark: every run-creating path on that laptop answers
`503` from then on, the organisation reachable or not, until the laptop is
re-enrolled with a fresh `WARDYN_ORG_ENROLMENT_TOKEN`. A second revoke of an
already-revoked device is a `404` and writes no row. `device.revoke` is the
audit row.

**Watching federation lag.** Each enrolled device's forwarder pushes its local
audit table upward every 15s from a durable cursor; `wardyn_org_federation_lag`
(present only when `WARDYN_ORG_URL` is set on THAT laptop — see
[Monitoring](monitoring.md) above) is the local rows the organisation has not
yet acknowledged, and `/healthz`'s `org_federation.lag` on that same laptop is
the human-readable twin. Whenever forwarding is not advancing, lag grows at
exactly the rate the laptop writes new audit rows, and it grows the same way
whether the organisation is unreachable or has refused a batch: the forwarder
re-reads the local head on every tick either way. Lag alone cannot tell the two
apart; once forwarding resumes, the backlog drains and the gauge falls. What
does tell them apart is a refusal's evidence. On THIS side, each refused batch
writes `device.audit.ingest` failure rows (`reason` is `invalid_body` or
`batch_too_large` at `400`/`413`, `invalid_row` at `400`, `chain_mismatch` or
`org_run` at `422`; see AUDIT-ACTIONS.md's Devices table). On the laptop,
`wardynd` logs `forwarding is halted until wardynd restarts` at ERROR. An
unreachable organisation produces neither. The laptop's forwarder halts
pushing on a refusal that definitive, and only a `wardynd` restart on the
laptop retries it, so growing lag with matching ingest failures is an operator
page, not a network blip to wait out.

**A device credential authenticates nothing but that device's own ingest
routes.** `deviceAuth` (`internal/api/devices_auth.go`) resolves the
`wdd_`-prefixed bearer to a device identity scoped to
`/api/v1/devices/{id}/*`; it never resolves to an operator or a member, so a
stolen device credential cannot create a run, read a workspace or reach any
other admin surface — it can only forge audit rows *about that one device*
until it is revoked. See threat-model residuals #50–#52.

