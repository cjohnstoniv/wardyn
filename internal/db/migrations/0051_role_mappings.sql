-- Copyright 2025 The Wardyn Authors
-- SPDX-License-Identifier: Apache-2.0

-- Console-managed (Getting Started -> People) role mappings: the store-backed
-- half of internal/auth/oidc's RoleMappingSource. Each row is one
-- "value=role" pair an admin adds from the console, merged at login with the
-- chart's WARDYN_OIDC_ROLE_MAP (see mergeRoleMaps) -- the chart always wins a
-- collision. This is what lets a deployment assign SSO roles without a helm
-- upgrade for every membership change.
--
-- ITS OWN TABLE, never a SiteConfig field -- the same reasoning 0042's
-- capability_enforcement gives, sharpened: PUT /site-config is a FULL
-- REPLACE, so a stale client round-tripping an older document would silently
-- drop every role mapping an admin had since added -- widening (a dropped
-- admin=... row demotes someone) or narrowing (a dropped member=... row can
-- flip an unmatched login to DefaultRole) either way with no admin action
-- taken. A permission-adjacent set of rows that can be added/removed one at a
-- time needs its own table so a whole-document write can never overwrite it.
--
-- value is stored ALREADY CANONICAL -- trimmed, lowercased, ASCII -- by the
-- API write boundary that owns this table (internal/api/access.go); the UNIQUE
-- index below is on that canonical form, matching how internal/auth/oidc's
-- mergeRoleMaps looks a claim value up (case-insensitively, via a lowered
-- key). role is checked here (unlike capability_grants.capability, which
-- deliberately has no CHECK) because "admin"/"member" is a small, complete,
-- unlikely-to-grow set with real Go constants (oidc.RoleAdmin/oidc.RoleMember)
-- to pin against -- internal/db/migrations_check_test.go's
-- TestClosedEnumChecksMatchConstants keeps the two in sync.
CREATE TABLE IF NOT EXISTS role_mappings (
    id         UUID PRIMARY KEY,
    value      TEXT NOT NULL,
    role       TEXT NOT NULL CHECK (role IN ('admin', 'member')),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    created_by TEXT NOT NULL DEFAULT ''
);

-- Serves both the login-time merge (dedupe against the chart map by value) and
-- the write boundary's own collision check (a new row must not name a value
-- another row already owns).
CREATE UNIQUE INDEX IF NOT EXISTS role_mappings_value_key ON role_mappings (value);
