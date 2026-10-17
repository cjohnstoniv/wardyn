-- Copyright 2025 The Wardyn Authors
-- SPDX-License-Identifier: Apache-2.0

-- The approval notification outbox: one row per (approval, tier, channel), written in the SAME
-- transaction that creates the approval, so a crash cannot leave an approval nobody was told about.
-- A worker on every replica claims due rows under a lease and delivers each at least once. A row
-- stores the channel's operator-chosen id and never its URL, and never a rendered payload: the body
-- is built at send time, so no requester identity lands in a second table that erasure must reach
-- (deleting a run cascades to its approvals and from there to these rows). last_error is a class
-- ("http_status:503", "timeout"), never text a server or a URL error wrote.
CREATE TABLE IF NOT EXISTS approval_notifications (
    id              UUID PRIMARY KEY,
    approval_id     UUID NOT NULL REFERENCES approvals(id) ON DELETE CASCADE,
    tier            SMALLINT NOT NULL CHECK (tier BETWEEN 0 AND 4),
    channel         TEXT NOT NULL CHECK (channel ~ '^[a-z0-9_-]{1,32}$'),
    due_at          TIMESTAMPTZ NOT NULL,
    state           TEXT NOT NULL DEFAULT 'pending'
                      CHECK (state IN ('pending','sending','sent','cancelled','dead')),
    attempts        SMALLINT NOT NULL DEFAULT 0,
    next_attempt_at TIMESTAMPTZ NOT NULL,
    lease_until     TIMESTAMPTZ,
    last_error      TEXT NOT NULL DEFAULT '',
    last_attempt_at TIMESTAMPTZ,
    sent_at         TIMESTAMPTZ,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (approval_id, tier, channel)
);

CREATE INDEX IF NOT EXISTS approval_notifications_due_idx
    ON approval_notifications (next_attempt_at) WHERE state IN ('pending','sending');
