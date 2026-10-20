-- Copyright 2026 The Wardyn Authors
-- SPDX-License-Identifier: Apache-2.0

-- store.PG.PrincipalsNamed folds a name's case when no principal is spelled exactly like it, and does so
-- over principal_identities, people, api_tokens and workspaces with lower(principal) (lower(owned_by) on
-- workspaces). Only the exact spelling had an index, so every such name read all four tables, and a SCIM
-- group removal resolves each of a member's emails this way. The case-folded email tier is served by
-- 0200's email_lower index, people_email_lower_uniq (0090) and api_tokens_lower_email_idx (0176).
CREATE INDEX IF NOT EXISTS principal_identities_lower_principal_idx ON principal_identities (lower(principal));
CREATE INDEX IF NOT EXISTS people_lower_principal_idx ON people (lower(principal));
CREATE INDEX IF NOT EXISTS api_tokens_lower_principal_idx ON api_tokens (lower(principal));
CREATE INDEX IF NOT EXISTS workspaces_lower_owned_by_idx ON workspaces (lower(owned_by));
