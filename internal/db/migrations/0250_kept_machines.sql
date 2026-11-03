-- Copyright 2026 The Wardyn Authors
-- SPDX-License-Identifier: Apache-2.0

-- Kept machines: one person's macOS guest disk, kept on that person's own Mac between runs. The disk is
-- on the Mac (runner_id); this row is the organisation's record of it. Every session is a new run, and
-- run_id is the run that has the machine, or last had it; agent_runs gets no column.
--
-- state: creating (a keep run's row, before its sandbox is up), in_use (a run holds it), kept (stopped,
-- on the Mac), deleting (the tombstone, delete_started_at, set by every delete: the Mac's delete may wait
-- for the Mac to come back). delete_by and delete_killer say who started that delete, for its audit row.
--
-- delete_intent_*: a kill by someone other than the owner deletes the machine. The kill writes this
-- intent before it claims the run, so a replica that dies between the two leaves the intent, and the
-- reconcile sweep finishes the delete from it. It is cleared if the kill does not happen.
--
-- name is unique per person, compared case-blind. run_id is cleared, not the row removed, if its run row
-- is ever deleted. scrubbed is whether the last stop scrubbed the machine; disk_bytes is what the Mac
-- last reported it holds.
--
-- Split-role installs grant the app role SELECT, INSERT, UPDATE, DELETE on it (CHANGELOG, Before you
-- upgrade).
CREATE TABLE IF NOT EXISTS kept_machines (
    id                   uuid PRIMARY KEY,
    name                 text NOT NULL CHECK (name <> ''),
    owner_subject        text NOT NULL CHECK (owner_subject <> ''),
    runner_id            uuid NOT NULL REFERENCES runners (id),
    base                 text NOT NULL,
    base_prepared_at     timestamptz NOT NULL,
    state                text NOT NULL CHECK (state IN ('creating', 'in_use', 'kept', 'deleting')),
    run_id               uuid REFERENCES agent_runs (id) ON DELETE SET NULL,
    scrubbed             boolean NOT NULL DEFAULT false,
    disk_bytes           bigint NOT NULL DEFAULT 0 CHECK (disk_bytes >= 0),
    created_at           timestamptz NOT NULL DEFAULT now(),
    last_used_at         timestamptz NOT NULL DEFAULT now(),
    expires_at           timestamptz NOT NULL,
    delete_started_at    timestamptz,
    delete_by            text NOT NULL DEFAULT '',
    delete_killer        text NOT NULL DEFAULT '',
    delete_intent_at     timestamptz,
    delete_intent_by     text NOT NULL DEFAULT '',
    delete_intent_killer text NOT NULL DEFAULT '',
    CONSTRAINT kept_machines_tombstone CHECK ((state = 'deleting') = (delete_started_at IS NOT NULL)),
    CONSTRAINT kept_machines_intent CHECK ((delete_intent_at IS NULL) = (delete_intent_by = ''))
);
CREATE UNIQUE INDEX IF NOT EXISTS kept_machines_owner_name_uniq ON kept_machines (owner_subject, lower(name));
-- One machine per run: the by-run read and the kill's intent each name exactly one row.
CREATE UNIQUE INDEX IF NOT EXISTS kept_machines_run_uniq ON kept_machines (run_id) WHERE run_id IS NOT NULL;
CREATE INDEX IF NOT EXISTS kept_machines_runner_idx ON kept_machines (runner_id);
