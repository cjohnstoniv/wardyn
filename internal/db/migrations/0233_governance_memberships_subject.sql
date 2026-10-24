-- Copyright 2026 The Wardyn Authors
-- SPDX-License-Identifier: Apache-2.0

-- store.ListMembershipsForQ and store.LegacyWinnersQ find a person's memberships by subject type and
-- subject. The only index naming those columns, governance_memberships_unit_subject_key (0208), leads with
-- unit_id, so each read scanned the whole table, and a unit delete, move or rename's legacy shift ran
-- that scan once for every person it reads, under the governance graph lock.
--
-- A plain CREATE INDEX blocks writes to governance_memberships while it builds, which at this table's
-- size (one row per subject per unit) lasts well under a second. CONCURRENTLY cannot run inside the
-- migration runner's transaction.
CREATE INDEX IF NOT EXISTS governance_memberships_subject ON governance_memberships (subject_type, subject);
