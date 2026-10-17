-- Copyright 2025 The Wardyn Authors
-- SPDX-License-Identifier: Apache-2.0

-- Postgres 13 is the floor from 0.8.6. This migration changes nothing: it refuses to run on an older
-- server, so that no later 0.8.6 migration is applied to, or recorded against, a database it was never
-- written for. It must stay the lowest-numbered 0.8.6 migration (TestMigratePG13Floor pins its place
-- right after the last 0.8.5 file); every migration sorts after it and so runs only once this passes.
--
-- A failed migration rolls back with its own bookkeeping row, so a refused boot leaves the database
-- exactly as 0.8.5 left it and the operator can restart the older release.
DO $$
BEGIN
    IF current_setting('server_version_num')::int < 130000 THEN
        RAISE EXCEPTION 'Wardyn 0.8.6 needs PostgreSQL 13 or newer, and this server is PostgreSQL %. Upgrade the database server (or run the previous Wardyn release); nothing was changed.',
            current_setting('server_version');
    END IF;
END
$$;
