-- Copyright 2025 The Wardyn Authors
-- SPDX-License-Identifier: Apache-2.0

-- SSH gateway key registry (Wave-2 C2/C3): a human's registered public keys,
-- self-service via GET/POST/DELETE /api/v1/me/ssh-keys. This is the gateway's
-- trust root — distinct from the pre-existing "ssh_key" GRANT kind (a resident
-- private key materialized for git-over-SSH cloning, unrelated table).
--
-- fingerprint is the SHA256 form (ssh.FingerprintSHA256: "SHA256:<base64>"),
-- computed server-side from the parsed key, never client-supplied. It is the
-- natural primary key: a fingerprint is intrinsic to the key's bytes, so two
-- rows can never disagree about which key they name, and re-registering the
-- identical key (by anyone) is a conflict, not a silent duplicate — the
-- gateway's PublicKeyCallback binds exactly one principal per fingerprint.
--
-- principal is the owning human (the same string agent_runs.created_by
-- carries); it scopes self-service list/delete AND the gateway's owner-only
-- authorization (run.created_by == the key's principal). EXPAND-ONLY,
-- matching every migration since 0001.
--
-- NUMBERING DEVIATION (documented, not silent): the wave-2 lane brief named
-- this migration 0033, treating 0032 as reserved by 0031's own header for a
-- LATER sources-column-drop ("must ship in a later release, never this
-- one"). db_test.go's TestMigrationPrefixesNoGapsOrDupes enforces STRICT
-- contiguity with no skip-list, so a 0031 -> 0033 gap fails that test outright
-- — and as of this migration no other wave-2 lane worktree has claimed 0032
-- either (checked: a/b1/b2/d all top out at 0031). 0031's reservation is a
-- NAME/INTENT reservation ("the next release's column-drop"), not a promise
-- the number stays empty across an intervening release — so this migration
-- takes 0032, and the sources column-drop remains real, deferred work that
-- gets the next free number (0033+) whenever it actually lands. If a
-- concurrent lane independently also claimed 0032, that is a merge-time
-- collision for the orchestrator to renumber one side of, same as any other
-- migration-number collision between parallel lanes.
CREATE TABLE IF NOT EXISTS ssh_public_keys (
    fingerprint TEXT PRIMARY KEY,
    principal   TEXT NOT NULL,
    name        TEXT NOT NULL DEFAULT '',
    public_key  TEXT NOT NULL,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Serves the self-service list (GET /me/ssh-keys, WHERE principal = $1).
CREATE INDEX IF NOT EXISTS ssh_public_keys_principal_idx ON ssh_public_keys (principal);
