-- Copyright 2025 The Wardyn Authors
-- SPDX-License-Identifier: Apache-2.0

-- operator_owned records, at create, that the run's owner is the operator
-- itself (the admin token, local mode) rather than a person. It is decided from
-- what authenticated the creating request, never from created_by, which an
-- identity provider could spell like the admin token. An owner_only grant on
-- such a run reads the operator's row as its own (#1106). Every existing row
-- reads false: the stricter answer, and none of them carries an owner_only
-- grant, which did not exist before this column.
ALTER TABLE agent_runs ADD COLUMN IF NOT EXISTS operator_owned BOOLEAN NOT NULL DEFAULT false;
