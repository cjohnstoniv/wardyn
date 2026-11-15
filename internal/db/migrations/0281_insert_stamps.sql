-- Copyright 2026 The Wardyn Authors
-- SPDX-License-Identifier: Apache-2.0

-- inserted_at: the database's own clock when a run, an API token or an SSH key row is inserted. Only the column
-- default writes it, never the application, so it is on the same clock as an identity's deactivated_at, and a
-- SCIM suspension compares the two to reach only the runs, tokens and keys made before it. Rows that exist
-- when this runs keep NULL, which counts as made before any suspension; the default is set after the column is
-- added, so adding it rewrites no row.
ALTER TABLE agent_runs ADD COLUMN IF NOT EXISTS inserted_at timestamptz;
ALTER TABLE agent_runs ALTER COLUMN inserted_at SET DEFAULT clock_timestamp();
ALTER TABLE api_tokens ADD COLUMN IF NOT EXISTS inserted_at timestamptz;
ALTER TABLE api_tokens ALTER COLUMN inserted_at SET DEFAULT clock_timestamp();
ALTER TABLE ssh_public_keys ADD COLUMN IF NOT EXISTS inserted_at timestamptz;
ALTER TABLE ssh_public_keys ALTER COLUMN inserted_at SET DEFAULT clock_timestamp();
