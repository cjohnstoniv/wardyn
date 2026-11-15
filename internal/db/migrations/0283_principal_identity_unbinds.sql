-- Copyright 2026 The Wardyn Authors
-- SPDX-License-Identifier: Apache-2.0

-- principal_identity_unbinds: one row per unbind of an identity row (POST /admin/identities/{id}/unbind),
-- written in the unbind's transaction. principal held the row until unbound_at, the database clock at the
-- unbind's last statement, so an audit row that carries the row's subject and was written at or before then
-- is that principal's (audit.Sealer renderSubject). principal is NULL once that person's audit personal
-- fields are erased, and is written NULL when they already were. An unbind made before this table existed
-- has no row.
CREATE TABLE IF NOT EXISTS principal_identity_unbinds (
    identity_id UUID        NOT NULL REFERENCES principal_identities(id) ON DELETE CASCADE,
    unbound_at  TIMESTAMPTZ NOT NULL,
    principal   TEXT,
    PRIMARY KEY (identity_id, unbound_at)
);
