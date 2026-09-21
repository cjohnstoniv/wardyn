-- Copyright 2025 The Wardyn Authors
-- SPDX-License-Identifier: Apache-2.0

-- Hybrid enrolment and audit federation, phase one (docs/design/0.8/PLAN.md,
-- issue #101, part of epic #78). Three tables:
--
--   * devices                  — organisation-side inventory of enrolled
--                                 laptops, one row per device credential.
--   * device_enrolment_tokens  — single-use admin-minted tokens a laptop's
--                                 first boot exchanges for a device credential.
--   * org_federation           — the LAPTOP side's single-row durable cursor:
--                                 how far its forwarder has pushed its own
--                                 local audit_events upward. Present in every
--                                 deployment's schema (the same wardynd binary
--                                 runs both roles), but only a laptop daemon
--                                 ever writes it.
--
-- Both credential tables store only a SHA-256 hash of the bearer they name,
-- never the raw value, matching attach_tickets (0026) and api_tokens (0045):
-- a live-DB reader (a reporting role, a hot standby, a pg_dump) must not be
-- able to lift a usable credential off a row.

CREATE TABLE IF NOT EXISTS devices (
    id                UUID        PRIMARY KEY,
    name              TEXT        NOT NULL,
    credential_sha256 TEXT        NOT NULL UNIQUE,
    enrolled_by       TEXT        NOT NULL DEFAULT '',
    created_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_seen_at      TIMESTAMPTZ,
    -- revoked_at is a SOFT delete, like api_tokens.revoked_at: the row
    -- survives revocation so a later audit read still resolves the device id
    -- it names. GetDeviceByRaw filters WHERE revoked_at IS NULL, which is
    -- what makes a revoked credential indistinguishable from an unknown one
    -- at the auth boundary.
    revoked_at        TIMESTAMPTZ,
    -- last_seq/last_row_hash are the ORGANISATION's own record of how far it
    -- has ingested THIS device's federated chain — never the device's local
    -- state, which the organisation never reads directly. Both advance
    -- atomically with IngestDeviceAudit's insert, inside the same
    -- transaction that verified the batch. last_row_hash empty means nothing
    -- has been ingested from this device yet, which is also what licenses
    -- accepting a genesis (empty-PrevHash) row unconditionally.
    last_seq          BIGINT      NOT NULL DEFAULT 0,
    last_row_hash     TEXT        NOT NULL DEFAULT ''
);

CREATE TABLE IF NOT EXISTS device_enrolment_tokens (
    id           UUID        PRIMARY KEY,
    token_sha256 TEXT        NOT NULL UNIQUE,
    device_name  TEXT        NOT NULL DEFAULT '',
    minted_by    TEXT        NOT NULL DEFAULT '',
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    expires_at   TIMESTAMPTZ NOT NULL,
    -- consumed_at, not a DELETE-on-consume row (attach_tickets' shape):
    -- ConsumeEnrolmentToken is a conditional UPDATE ... WHERE consumed_at IS
    -- NULL ... RETURNING, so the row (and who minted it, and when) survives
    -- its own redemption for the device inventory that consume then creates.
    consumed_at  TIMESTAMPTZ
);

-- Singleton, exactly like site_config (0013): a laptop daemon has exactly one
-- federation cursor, so the primary key is the boolean itself, CHECKed true,
-- which makes a second row impossible at the schema level.
CREATE TABLE IF NOT EXISTS org_federation (
    singleton          BOOLEAN     NOT NULL DEFAULT true CHECK (singleton),
    last_forwarded_seq BIGINT      NOT NULL DEFAULT 0,
    updated_at         TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (singleton)
);
