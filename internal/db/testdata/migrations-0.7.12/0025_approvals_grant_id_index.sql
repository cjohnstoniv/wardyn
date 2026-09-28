-- Copyright 2025 The Wardyn Authors
-- SPDX-License-Identifier: Apache-2.0

-- The broker's credential path reads approvals BY GRANT: ensureApproval's
-- newest-first lookup (WHERE grant_id=$1 AND kind='credential' ORDER BY
-- requested_at DESC LIMIT 1) and the mint transaction's LEFT JOIN ON
-- a.grant_id=g.id. The only grant_id index so far is 0002's partial UNIQUE
-- (WHERE kind='credential' AND state='PENDING'), which cannot serve either
-- read once an approval leaves PENDING — and the no-match case (every run's
-- FIRST mint for a new grant) had to scan the whole table to conclude no row
-- exists. Approvals grow without bound (0020), so per-mint cost grew linearly
-- with deployment age, inside the FOR UPDATE mint transaction.
--
-- Composite (grant_id, requested_at DESC) serves the newest-first per-grant
-- lookup as a bounded index scan and the join as a plain grant_id probe. The
-- kind filter rides the scan (a grant's approvals are single-kind in
-- practice); no partial predicate, so post-decision states stay indexed.
CREATE INDEX IF NOT EXISTS approvals_grant_id_requested_at_idx
    ON approvals (grant_id, requested_at DESC);
