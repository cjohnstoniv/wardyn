-- Copyright 2025 The Wardyn Authors
-- SPDX-License-Identifier: Apache-2.0

-- Console branding (#1125): the one org-wide record a super admin writes from
-- the Branding card. No row is the unbranded console. Colours are stored as
-- validated lower-case #rrggbb; dark_primary/dark_primary_text are '' when the
-- console derives the dark pair itself. logo is the sanitised SVG or the PNG
-- exactly as uploaded (at most 512 KB), served by wardynd from 'self'.
CREATE TABLE IF NOT EXISTS branding (
    singleton         BOOLEAN PRIMARY KEY DEFAULT true CHECK (singleton),
    org_name          TEXT NOT NULL,
    name_format       TEXT NOT NULL CHECK (name_format IN ('prefix', 'suffix')),
    primary_color     TEXT NOT NULL,
    primary_text      TEXT NOT NULL,
    dark_primary      TEXT NOT NULL DEFAULT '',
    dark_primary_text TEXT NOT NULL DEFAULT '',
    support_url       TEXT NOT NULL DEFAULT '',
    logo              BYTEA,
    logo_type         TEXT NOT NULL DEFAULT '' CHECK (logo_type IN ('', 'image/png', 'image/svg+xml')),
    updated_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_by        TEXT NOT NULL DEFAULT ''
);
