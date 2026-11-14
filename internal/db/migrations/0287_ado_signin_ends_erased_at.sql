-- Copyright 2026 The Wardyn Authors
-- SPDX-License-Identifier: Apache-2.0

-- erased_at (0129's ado_signin_ends): when an erase of the person's credentials last began or finished
-- its end of their Azure DevOps sign-in. Any other end, a disconnect, overwrites reason and updated_at
-- but leaves this. The run token sweep deletes a token row of theirs whose revoke Azure DevOps refused
-- once it is past its valid_to, when the row was created before this time. A row whose last end was an
-- erase takes that end's updated_at; every other row stays NULL. Nullable with no default, so the
-- column is metadata-only; the table's grants cover it.
ALTER TABLE ado_signin_ends ADD COLUMN IF NOT EXISTS erased_at TIMESTAMPTZ;
UPDATE ado_signin_ends SET erased_at = updated_at WHERE reason = 'offboarding' AND erased_at IS NULL;
