-- Copyright 2025 The Wardyn Authors
-- SPDX-License-Identifier: Apache-2.0

-- MEMBER workspace ownership (W-MEMB, docs/design/member-role-desktop.md §b).
-- owned_by is the lowercased OIDC sub (or email) of the MEMBER who created the
-- workspace — the same dual-key identity a capability_grants `user` subject
-- carries (0042:8-9), so an admin who knows either identity can reason about
-- ownership.
--
-- '' (the DEFAULT) means OPERATOR-OWNED: every workspace that exists today, and
-- every workspace an admin creates from here on, keeps behaving exactly as it
-- does now — member-readable, admin-writable. Same back-compat shape as
-- capability_enforcement's "absent row = not enforced" (0042:59-63): a 0.5->0.6
-- upgrade changes nothing until a member creates their first owned workspace.
--
-- NOT NULL DEFAULT '' — a metadata-only add on PG11+ — so scanWorkspace keeps
-- scanning into a plain string rather than threading a nullable through the
-- workspace column list. No CHECK: it is an identity string, same rationale as
-- capability_grants.subject (0042:17-23).
ALTER TABLE workspaces ADD COLUMN IF NOT EXISTS owned_by TEXT NOT NULL DEFAULT '';

-- Serves the owner-scoped list read (handleListWorkspaces for a member:
-- WHERE owned_by IN ('', '<principal>')).
CREATE INDEX IF NOT EXISTS workspaces_owned_by_idx ON workspaces (owned_by);
