-- Copyright 2025 The Wardyn Authors
-- SPDX-License-Identifier: Apache-2.0

-- Add the approval kind PUSH_CONTENT to the approvals.kind CHECK.
--
-- A brokered git push that touches a path the run's
-- push_rules.require_review_paths names is HELD in the proxy sidecar while an
-- admin decides (internal/egress/proxy/push_hold.go); this row is the question.
-- See internal/types/types.go ApprovalPushContent and
-- internal/types/push_content.go PushContentScope for its requested_scope.
--
-- Replaces the CHECK for 0064_approval_credential_reauth.sql's reason exactly:
-- two CHECKs on one column are ANDed, so adding a second would leave
-- 'push_content' rejected by the first with every gate green. Drop-if-exists
-- keeps it idempotent.
--
-- ADDITIVE ONLY: it widens an admitted set, so an upgrade needs no backfill. A
-- downgrade with push_content rows present is unsupported (the old CHECK would
-- refuse them) -- the upgrade runbook's pg_dump is the rollback.
--
-- The partial unique index from 0022 (approvals_pending_noncred_uniq, WHERE
-- state='PENDING' AND kind <> 'credential') covers this kind too: at most ONE
-- PENDING push_content per (run, requested_scope), and the scope carries the
-- push's commits and a digest of every review-matched path, so two raises of
-- the same push collapse into one row.

ALTER TABLE approvals DROP CONSTRAINT IF EXISTS approvals_kind_check;

ALTER TABLE approvals ADD CONSTRAINT approvals_kind_check CHECK (kind IN (
    'credential',
    'egress_domain',
    'tool_call',
    'credential_reauth',
    'push_content'
));
