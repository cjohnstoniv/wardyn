-- Copyright 2025 The Wardyn Authors
-- SPDX-License-Identifier: Apache-2.0

-- Remembers the row hash a hybrid laptop's forwarder last had acknowledged,
-- beside its seq cursor on the org_federation singleton (issue #520).
--
-- Seq alone does not identify a local audit row: a table reset that restarts
-- seq (TRUNCATE ... RESTART IDENTITY, a restore) and then writes past the old
-- cursor leaves a row at the cursor's seq that the organisation never saw. The
-- forwarder then pushed the rows after it, which link to nothing the
-- organisation holds, got a 422 and halted. It now checks that the local row
-- at last_forwarded_seq still carries last_forwarded_row_hash; if not, the
-- chain was reset and it resends from genesis (internal/federation.Forwarder).
--
-- '' on an existing row means "not known": the check fails, and the forwarder
-- resends from the start once, which the organisation's hash-checked re-send
-- rule (store.PG.IngestDeviceAudit) turns into a no-op for rows it holds.
ALTER TABLE org_federation ADD COLUMN IF NOT EXISTS last_forwarded_row_hash TEXT NOT NULL DEFAULT '';
