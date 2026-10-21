-- Copyright 2026 The Wardyn Authors
-- SPDX-License-Identifier: Apache-2.0

-- Shares the desktop sessions their person closed across replicas and restarts.
--
-- A desktop session lives in a signed cookie, so Close is a mark the relay checks on every request.
-- Until now the mark lived only in one process's memory: another replica, or the same one after a
-- restart, admitted the closed session's cookie again until it expired.
--
-- key_digest is the hex SHA-256 of the session's relay key (uiReassertKey: principal, run, app and
-- issue time), so this table holds no principal text. expires_at is the cookie's own bound, at most
-- an hour out; past it the cookie is refused anyway, so a row is read only while expires_at > now
-- and the reaper-tick prune deletes it after.
--
-- ADDITIVE ONLY: a new table with no foreign keys. A release before this one has no desktop, so a
-- downgrade leaves the table unread.
CREATE TABLE IF NOT EXISTS ui_desktop_closed (
    key_digest TEXT PRIMARY KEY,
    expires_at TIMESTAMPTZ NOT NULL
);

CREATE INDEX IF NOT EXISTS ui_desktop_closed_expires_at_idx ON ui_desktop_closed (expires_at);
