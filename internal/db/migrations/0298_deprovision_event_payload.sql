-- Copyright 2026 The Wardyn Authors
-- SPDX-License-Identifier: Apache-2.0

-- deprovision_jobs.audit_event_payload: the payload of the audit event a portal person's suspension or purge step
-- writes, pinned beside the event's id and time (0196) by the first attempt, so a completion row that one replica
-- spooled and lost is written again by another with the same identity, actor and data. A human actor's payload is
-- stored sealed under that person's audit key, so erasing the key erases it; the step's finish clears it in the same
-- statement that marks the step done, once the event is in the audit table. Reopening a step clears it with the id and
-- time.
-- deprovision_jobs.kill_claimed_at: when a worker claimed a portal suspension's kill of one run. A second claim of
-- the same occurrence waits out the claim's lease instead of tearing the run down again.
ALTER TABLE deprovision_jobs ADD COLUMN IF NOT EXISTS audit_event_payload JSONB;
ALTER TABLE deprovision_jobs ADD COLUMN IF NOT EXISTS kill_claimed_at TIMESTAMPTZ;
-- deprovision_jobs.kill_claim_id and kill_from: the acquisition that holds the kill claim, so only its holder releases
-- or finishes the row, and the state the run was killed from, so a repair of a run that left the queue is a queued
-- kill's repair.
ALTER TABLE deprovision_jobs ADD COLUMN IF NOT EXISTS kill_claim_id UUID;
ALTER TABLE deprovision_jobs ADD COLUMN IF NOT EXISTS kill_from TEXT;

