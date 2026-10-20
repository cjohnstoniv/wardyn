-- Copyright 2026 The Wardyn Authors
-- SPDX-License-Identifier: Apache-2.0

-- Add the approval kind HUMAN_TAKEOVER ("Needs you") to the approvals.kind CHECK, and the two columns
-- its takeover writes.
--
-- The run's agent asks a person to take over its desktop or terminal (`wardyn needs-you <reason>`);
-- see internal/types/types.go ApprovalHumanTakeover. Take over moves the row to APPROVED and sets
-- takeover_holder; a second person taking over replaces the holder; hand back sets handed_back_at.
-- Both stay empty on every other kind.
--
-- Replaces the CHECK for 0075_approval_push_content.sql's reason exactly: two CHECKs on one column
-- are ANDed. Drop-if-exists keeps it idempotent. ADDITIVE ONLY: an upgrade needs no backfill; a
-- downgrade with human_takeover rows present is unsupported (the old CHECK would refuse them).

ALTER TABLE approvals DROP CONSTRAINT IF EXISTS approvals_kind_check;

ALTER TABLE approvals ADD CONSTRAINT approvals_kind_check CHECK (kind IN (
    'credential',
    'egress_domain',
    'tool_call',
    'credential_reauth',
    'push_content',
    'human_takeover'
));

ALTER TABLE approvals ADD COLUMN IF NOT EXISTS takeover_holder TEXT NOT NULL DEFAULT '';
ALTER TABLE approvals ADD COLUMN IF NOT EXISTS handed_back_at TIMESTAMPTZ;
