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
-- NUMBERING (merge-time renumber): the wave-2 SSH lane originally authored
-- this as 0032, but the v0.5-k8s-cloud merge into main found main had already
-- shipped 0032_attach_tickets_token_sha256.sql — the exact parallel-lane
-- collision this file's original header anticipated ("a merge-time collision
-- for the orchestrator to renumber one side of"). Renumbered to 0033 so the
-- prefixes stay contiguous and duplicate-free (db_test.go's
-- TestMigrationPrefixesNoGapsOrDupes): 0031, 0032 (token_sha256),
-- 0033 (this), 0034 (attach_ticket_role). Both 0032/0033 only ALTER/CREATE
-- distinct objects, so ordering among them is immaterial.
CREATE TABLE IF NOT EXISTS ssh_public_keys (
    fingerprint TEXT PRIMARY KEY,
    principal   TEXT NOT NULL,
    name        TEXT NOT NULL DEFAULT '',
    public_key  TEXT NOT NULL,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Serves the self-service list (GET /me/ssh-keys, WHERE principal = $1).
CREATE INDEX IF NOT EXISTS ssh_public_keys_principal_idx ON ssh_public_keys (principal);
