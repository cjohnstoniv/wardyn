-- Copyright 2026 The Wardyn Authors
-- SPDX-License-Identifier: Apache-2.0

-- The audit event an owed ledger row writes keeps one id and time across attempts: the first drain stores
-- them here, and a reopened row (a new occurrence) clears them. NULL until pinned.
ALTER TABLE deprovision_jobs ADD COLUMN IF NOT EXISTS audit_event_id UUID;
ALTER TABLE deprovision_jobs ADD COLUMN IF NOT EXISTS audit_event_at TIMESTAMPTZ;
