-- Copyright 2025 The Wardyn Authors
-- SPDX-License-Identifier: Apache-2.0

-- Attach tickets (0026) gain a role, stamped at mint time from the minting
-- caller's OWN role (admin/member -- B1's oidc.RoleAdmin/RoleMember). B2 moves
-- POST /runs/{id}/attach-ticket from admin-only to owner-or-admin, so a member
-- who owns the run can mint one too; the WS attach route's ?ticket= lane
-- bypasses humanOrAdminAuth entirely (browsers cannot carry a session cookie
-- into that handshake reliably), so this column is the ONLY role source
-- available when the WS handler re-checks owner-or-admin at consume time.
--
-- Numbered 0033, not 0032: the SSH lane (C2) already claimed 0032 on a
-- sibling branch, and migration prefixes must be contiguous with no gaps
-- (TestMigrationPrefixesNoGapsOrDupes).
--
-- NOT NULL DEFAULT 'admin' is a rolling-upgrade safety net for a ticket
-- minted by a pre-migration binary mid-deploy -- tickets are 30s-TTL, so the
-- exposure window is vanishingly small either way, and 'admin' is the
-- STRICTER of the two values (never widens what an old ticket could do).
ALTER TABLE attach_tickets ADD COLUMN IF NOT EXISTS role TEXT NOT NULL DEFAULT 'admin';
