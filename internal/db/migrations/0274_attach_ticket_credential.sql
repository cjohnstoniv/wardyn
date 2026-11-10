-- Copyright 2026 The Wardyn Authors
-- SPDX-License-Identifier: Apache-2.0

-- The credential an attach ticket was minted under: the personal API token's id, or the CLI session's id
-- when a CLI sign-in's access token minted it. An open terminal asks after that credential every 30
-- seconds and closes when it is revoked, expired or signed out. NULL on a ticket minted with a console
-- session or the admin token, and on any row from before the columns. Additive: an older replica neither
-- writes nor reads these columns. No foreign key: a ticket lives 30 seconds and the credential may be
-- deleted before it.
ALTER TABLE attach_tickets ADD COLUMN IF NOT EXISTS token_id UUID;
ALTER TABLE attach_tickets ADD COLUMN IF NOT EXISTS cli_session_id UUID;

-- A CLI refresh now deletes the access token it replaces instead of leaving a revoked row (a session's
-- first token is kept: its creation is audited). This removes the rows earlier refreshes left behind: a
-- revoked cli_access token that has an older sibling in its session and was revoked before the session
-- ended. The session's first token, its cli_admin tokens and every personal token stay.
DELETE FROM api_tokens t
 WHERE t.kind = 'cli_access'
   AND t.revoked_at IS NOT NULL
   AND t.cli_session_id IS NOT NULL
   AND EXISTS (SELECT 1 FROM api_tokens o
                WHERE o.cli_session_id = t.cli_session_id AND o.kind = 'cli_access' AND o.id <> t.id
                  AND (o.created_at, o.id) < (t.created_at, t.id))
   AND NOT EXISTS (SELECT 1 FROM cli_sessions cs
                    WHERE cs.id = t.cli_session_id AND cs.ended_at IS NOT NULL AND cs.ended_at <= t.revoked_at);
