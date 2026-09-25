-- Copyright 2025 The Wardyn Authors
-- SPDX-License-Identifier: Apache-2.0

-- Per-resource availability (0.8, user-types design section 2.6): the
-- "Available to: Only..." bit on one value of one capability kind. A row here
-- means that value is RESTRICTED: only a caller holding an allow grant naming
-- it exactly may use it, whatever the kind's enforcement switch says
-- (capBatch.decide's step 3). Who is listed is not stored here; it is the
-- allow rows in capability_grants, so a type, group or person is listed by the
-- same row every other grant is.
--
-- Its own table beside capability_enforcement, for the reason that one is:
-- a stale client round-tripping PUT /site-config can never switch it off.
-- ABSENT ROW = NOT RESTRICTED ("Everyone"), so an upgrade changes nothing.
--
-- No CHECK on capability, as in 0042: the closed kind set and which kinds can
-- be restricted live in Go (capKinds) and are validated at the write boundary.
CREATE TABLE IF NOT EXISTS capability_restrictions (
    capability TEXT NOT NULL,
    value      TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    created_by TEXT NOT NULL DEFAULT '',
    PRIMARY KEY (capability, value)
);
