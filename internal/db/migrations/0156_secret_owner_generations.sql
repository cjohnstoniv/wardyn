-- Copyright 2025 The Wardyn Authors
-- SPDX-License-Identifier: Apache-2.0

-- One counter per person whose credentials have been written or erased: an
-- erase advances it, and a write that began before the advance is refused.
-- A number and a time, never a credential; an owner with no row is generation 0,
-- so every existing credential keeps working and the row appears on first use.
CREATE TABLE IF NOT EXISTS secret_owner_generations (
    owned_by text PRIMARY KEY CHECK (owned_by <> ''),
    generation bigint NOT NULL DEFAULT 0 CHECK (generation >= 0),
    updated_at timestamptz NOT NULL DEFAULT now()
);
