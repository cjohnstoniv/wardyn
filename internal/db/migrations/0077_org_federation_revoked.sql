-- Copyright 2025 The Wardyn Authors
-- SPDX-License-Identifier: Apache-2.0

-- Persists a hybrid laptop's revocation across a daemon restart (issue #103).
--
-- The laptop's forwarder (internal/federation) learns that the organisation
-- revoked its device credential when a push or heartbeat answers 401/410, and
-- from then on every run-creating path answers 503. Held only in memory, that
-- mark was lost on a restart: with the organisation unreachable at boot, the
-- new forwarder's first call failed as a network error, not a revocation, and
-- every run-creating path opened again. revoked_at records it beside the
-- cursor on the org_federation singleton (migration 0066), read at boot
-- (store.PG.FederationRevoked), set by Forwarder.revoke
-- (store.PG.MarkFederationRevoked) and cleared only by a re-enrolment
-- (store.PG.ResetFederation), which also returns the cursor to 0.
--
-- Additive and nullable: NULL is "not revoked", which is every existing row's
-- truth, so no backfill.
ALTER TABLE org_federation ADD COLUMN IF NOT EXISTS revoked_at TIMESTAMPTZ;
