-- Copyright 2026 The Wardyn Authors
-- SPDX-License-Identifier: Apache-2.0

-- deprovision_jobs.audit_event_payload: the immutable payload of the audit event a portal person's suspension or
-- purge step writes, pinned beside the event's id and time (0196) by the first attempt. Every later attempt, on
-- any replica, replays that event whole, so a completion row that one replica spooled and lost is written again
-- by another with the same identity, actor and data. It holds the actor and the counts the step reports, no secret.
-- Reopening a step clears it with the id and time.
ALTER TABLE deprovision_jobs ADD COLUMN IF NOT EXISTS audit_event_payload JSONB;
