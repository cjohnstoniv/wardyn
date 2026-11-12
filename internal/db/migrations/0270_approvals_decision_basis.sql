-- Copyright 2026 The Wardyn Authors
-- SPDX-License-Identifier: Apache-2.0

-- A decision made under a unit's deciders rule keeps, on its approval row, how the decider decided
-- (decision_basis: owner, unit_admin:<unit>, group:<name>, security, policy or admin_token) and the unit
-- whose rule it was (decision_rule_unit). Both stay NULL on a decision no rule governed, and on every row
-- decided before this migration. No foreign key: the row keeps the unit it was decided under after that
-- unit is deleted, as the approval.decide audit row does.
--
-- Re-runnable: every statement is IF NOT EXISTS.
ALTER TABLE approvals ADD COLUMN IF NOT EXISTS decision_basis TEXT;
ALTER TABLE approvals ADD COLUMN IF NOT EXISTS decision_rule_unit UUID;
