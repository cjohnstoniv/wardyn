-- Copyright 2026 The Wardyn Authors
-- SPDX-License-Identifier: Apache-2.0

-- Add the approval kind FILE_DOWNLOAD to the approvals.kind CHECK, and keep one review of a file waiting per run.
--
-- A person asks to download one file from a run whose desktop.download is review; see internal/types/types.go
-- ApprovalFileDownload. Its requested_scope is the file's name, size, SHA-256 and who asked. The partial unique
-- index admits one PENDING file_download row per (run_id, name), whatever the bytes, so a second ask for a
-- file whose review is still waiting is refused rather than queued beside it.
--
-- Replaces the CHECK for 0218_approval_human_takeover.sql's reason exactly: two CHECKs on one column are ANDed.
-- Drop-if-exists keeps it idempotent. ADDITIVE ONLY: an upgrade needs no backfill; a downgrade with
-- file_download rows present is unsupported (the old CHECK would refuse them).

ALTER TABLE approvals DROP CONSTRAINT IF EXISTS approvals_kind_check;

ALTER TABLE approvals ADD CONSTRAINT approvals_kind_check CHECK (kind IN (
    'credential',
    'egress_domain',
    'tool_call',
    'credential_reauth',
    'push_content',
    'human_takeover',
    'file_download'
));

CREATE UNIQUE INDEX IF NOT EXISTS approvals_pending_file_download_name_uniq
    ON approvals (run_id, (requested_scope->>'name'))
    WHERE state = 'PENDING' AND kind = 'file_download';
