-- Copyright 2026 The Wardyn Authors
-- SPDX-License-Identifier: Apache-2.0

-- store.ListMembershipsForQ and store.LegacyWinnersQ find a person's memberships by subject type and
-- subject. The only index naming those columns, governance_memberships_unit_subject_key (0208), leads with
-- unit_id, so each read scanned the whole table, and a unit delete, move or rename's legacy shift ran
-- that scan once for every person it reads, under the governance graph lock.
--
-- A plain CREATE INDEX blocks writes to governance_memberships while it builds. The table holds one row
-- per subject per unit, so the block is brief at typical sizes; the build was not timed, and the
-- largest table the boundedness test seeds is 57,087 rows. CONCURRENTLY cannot run inside the
-- migration runner's transaction.
CREATE INDEX IF NOT EXISTS governance_memberships_subject ON governance_memberships (subject_type, subject);
