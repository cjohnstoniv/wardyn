-- Copyright 2025 The Wardyn Authors
-- SPDX-License-Identifier: Apache-2.0

-- Per-capability permissioning (0.6 pillar 2): which of the powers a MEMBER
-- already has may that member actually use. Two tables, deliberately separate.
--
-- capability_grants is the grant list. subject_type/subject name WHO:
--   user  — a lowercased OIDC "sub" OR an email; a grant on either hits, so an
--           admin can write down the identity they actually know.
--   group — one entry of the login-time union of the ID token's roles+groups
--           claims (Entra App Roles are grantable for free this way).
--   all   — every signed-in human; the baseline for IdPs with no usable groups
--           claim. subject is '' for this type.
-- capability/value name WHAT (see internal/api/capabilities.go), and effect is
-- allow or deny, with DENY BEATING ALLOW at resolution time.
--
-- NO CHECK ON capability, on purpose. The closed kind set lives in exactly one
-- Go slice (capabilityKinds, internal/api/capabilities.go) and is validated at
-- the API write boundary — the same "closed Go enum validated in application
-- code" rule 0037 names for ui_run_layouts.preset. A fifth kind is then a Go
-- constant plus its call site and zero DDL; a CHECK here would make every new
-- kind a migration, and a value this binary does not know is inert anyway (no
-- resolver asks for it).
--
-- subject_type and effect DO carry CHECKs: both are complete, closed enums
-- that will not grow, and internal/db/migrations_check_test.go's
-- TestClosedEnumChecksMatchConstants pins each against its Go constants.
--
-- UNIQUE(subject_type, subject, capability, value) is the natural key and
-- exactly what the CRUD upsert needs: re-granting the same triple flips the
-- effect in place instead of accumulating a contradictory second row (which
-- would resolve as a permanent deny and be near-impossible to explain).
CREATE TABLE IF NOT EXISTS capability_grants (
    id           UUID PRIMARY KEY,
    subject_type TEXT NOT NULL CHECK (subject_type IN ('user', 'group', 'all')),
    subject      TEXT NOT NULL,
    capability   TEXT NOT NULL,
    value        TEXT NOT NULL,
    effect       TEXT NOT NULL CHECK (effect IN ('allow', 'deny')),
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    created_by   TEXT NOT NULL DEFAULT '',
    UNIQUE (subject_type, subject, capability, value)
);

-- Serves the resolver's per-request read: every grant that could apply to this
-- caller, fetched by (subject_type, subject) across the 'all' row, the two user
-- subjects and the session's groups. capability is NOT in the index — a
-- deployment's whole grant list is small and one round trip answers both
-- capAllowed and GET /me/capabilities.
CREATE INDEX IF NOT EXISTS capability_grants_subject_idx
    ON capability_grants (subject_type, subject);

-- capability_enforcement is the per-kind ON/OFF switch, and it is its OWN TABLE
-- rather than a SiteConfig field because PUT /site-config is a FULL REPLACE: a
-- stale client round-tripping an older config would silently switch an authz
-- control back off. Fail-open is unacceptable for a permission gate, so the
-- switches live where nothing else can overwrite them.
--
-- ABSENT ROW = NOT ENFORCED. That is the whole back-compat story: a 0.5
-- deployment upgraded with zero configuration keeps today's member powers
-- byte-for-byte, and an admin turns kinds on one at a time. Deny grants still
-- apply with the switch off (deny beats the switch) so a single host can be
-- blacklisted for one contractor without going fail-closed deployment-wide.
CREATE TABLE IF NOT EXISTS capability_enforcement (
    capability TEXT PRIMARY KEY,
    enabled    BOOLEAN NOT NULL DEFAULT FALSE,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
