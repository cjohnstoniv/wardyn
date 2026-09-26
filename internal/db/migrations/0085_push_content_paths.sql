-- Copyright 2025 The Wardyn Authors
-- SPDX-License-Identifier: Apache-2.0

-- The complete review-matched path list of a held push (#1066). The
-- push_content requested_scope names only the first ten paths, because the
-- scope is the dedup key and the card's list; this row keeps the rest, bounded
-- (types.PushPathList: 10,000 paths / 1 MiB, truncated beyond), one per
-- approval, verified against the scope's paths_total and paths_digest before
-- it is written.
--
-- Immutable, as the audit log is: nothing in Wardyn updates or deletes a row,
-- and the triggers below refuse it at the database, TRUNCATE included. The FK
-- does not cascade, so the approval (and with it the run) cannot be deleted
-- out from under its list either; neither is deleted today.
CREATE TABLE IF NOT EXISTS push_content_paths (
    approval_id UUID PRIMARY KEY REFERENCES approvals(id),
    paths       TEXT[] NOT NULL,
    truncated   BOOLEAN NOT NULL,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE OR REPLACE FUNCTION push_content_paths_immutable() RETURNS trigger AS $$
BEGIN
    RAISE EXCEPTION 'push_content_paths is immutable';
END;
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS push_content_paths_no_update ON push_content_paths;
CREATE TRIGGER push_content_paths_no_update
    BEFORE UPDATE OR DELETE ON push_content_paths
    FOR EACH ROW EXECUTE FUNCTION push_content_paths_immutable();

DROP TRIGGER IF EXISTS push_content_paths_no_truncate ON push_content_paths;
CREATE TRIGGER push_content_paths_no_truncate
    BEFORE TRUNCATE ON push_content_paths
    FOR EACH STATEMENT EXECUTE FUNCTION push_content_paths_immutable();
