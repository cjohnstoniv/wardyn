-- Copyright 2026 The Wardyn Authors
-- SPDX-License-Identifier: Apache-2.0

-- Governed inbound: a service run's UI app may be marked inbound, callable by a named person or a
-- machine through the UI gateway.
--
-- Inbound is off until an admin turns it on. Declaring an inbound app needs the `feature` capability
-- value `inbound`. This row restricts that value: only an allow grant that names it admits anyone, so
-- a `*` allow grants nobody (capBatch.decide's step 3), as 0167 does for `service_attach`. Lifting the
-- restriction is a deliberate PUT /permissions/availability/feature/inbound.
--
-- ON CONFLICT keeps an admin's own choice: a database that already holds the row is left as it is.
INSERT INTO capability_restrictions (capability, value, created_by)
VALUES ('feature', 'inbound', 'migration:0240')
ON CONFLICT (capability, value) DO NOTHING;

-- api_tokens.kind admits `inbound`: a token that reaches an inbound app and nothing on the console API.
-- It belongs to no CLI session, so it pairs with cli_session_id IS NULL as an `api` token does. The
-- pairing is written as equalities and the enum column keeps exactly one `kind IN (` list: the
-- closed-enum guards count only that list (see 0199).
ALTER TABLE api_tokens DROP CONSTRAINT IF EXISTS api_tokens_kind_check;
ALTER TABLE api_tokens ADD CONSTRAINT api_tokens_kind_check CHECK (kind IN ('api', 'cli_access', 'cli_admin', 'inbound'));
ALTER TABLE api_tokens DROP CONSTRAINT IF EXISTS api_tokens_cli_session_kind;
ALTER TABLE api_tokens ADD CONSTRAINT api_tokens_cli_session_kind CHECK ((kind = 'api' OR kind = 'inbound') = (cli_session_id IS NULL));

-- An attach ticket minted for an inbound caller records which entry of the app's callers list matched
-- (caller) and which app it opens (app). Both are empty on every existing ticket, which is not a
-- caller ticket.
--
-- ADDITIVE ONLY: two constant-default columns, so no row is rewritten. A downgrade stops reading them.
ALTER TABLE attach_tickets ADD COLUMN IF NOT EXISTS caller TEXT NOT NULL DEFAULT '';
ALTER TABLE attach_tickets ADD COLUMN IF NOT EXISTS app TEXT NOT NULL DEFAULT '';
