-- Copyright 2026 The Wardyn Authors
-- SPDX-License-Identifier: Apache-2.0
--
-- A held governance change may move a membership to another unit (op move) or make a legacy
-- membership a context (op promote), the policy tree's two membership writes (0.10.0). The op CHECK
-- of governance_changes gains both. Widening a CHECK admits every value it admitted before, so no
-- existing row can fail it and none is rewritten.
--
-- No table is new, so a split-role install needs no grant change.
--
-- Re-runnable: one ALTER TABLE drops the named constraint if it is there and adds it back, in one
-- statement, so re-applying it over a migrated schema leaves the same constraint.

ALTER TABLE governance_changes
    DROP CONSTRAINT IF EXISTS governance_changes_op_check,
    ADD CONSTRAINT governance_changes_op_check
        CHECK (op IN ('create','update','delete','upsert','replace','set','move','promote'));
