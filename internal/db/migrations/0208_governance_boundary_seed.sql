-- Copyright 2026 The Wardyn Authors
-- SPDX-License-Identifier: Apache-2.0

-- seeded_at marks the Wardyn boundary's one-shot fresh-install seed (internal/db/fresh_seed.go), and
-- every boundary write sets it, so the seed never overwrites a document a person wrote. The singleton
-- row every install then has is no_limits, which resolves every run exactly as no row did. Whether
-- this install is fresh is decided by the migrator, never here.
ALTER TABLE governance_boundary ADD COLUMN IF NOT EXISTS seeded_at TIMESTAMPTZ NULL;
INSERT INTO governance_boundary (mode) VALUES ('no_limits') ON CONFLICT (singleton) DO NOTHING;
