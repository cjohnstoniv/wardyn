-- Copyright 2026 The Wardyn Authors
-- SPDX-License-Identifier: Apache-2.0

-- governance_changes.drafted_with_session: the assistant session a held change was drafted in, when the server
-- verified that it was. It is a mark for the people who review the change and it changes no verdict, risk or
-- approval count. The column keeps the id only and has no foreign key on purpose: a session's events are swept
-- with the retention window and an erased person's session is blanked, and the change must outlive both.
ALTER TABLE governance_changes ADD COLUMN IF NOT EXISTS drafted_with_session uuid NULL;

-- The server match in PG.DraftingAssistantSession (internal/store/governance_changes.go) runs on every run create, template save and unit propose, and the existing indexes are partial on state <> 'ended'.
CREATE INDEX IF NOT EXISTS assistant_sessions_draft_lookup ON assistant_sessions (principal, purpose, target_kind, target_key);
