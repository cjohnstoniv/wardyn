-- Copyright 2026 The Wardyn Authors
-- SPDX-License-Identifier: Apache-2.0

-- approvals_pending_noncred_uniq (0022, widened to credential_reauth by 0064 and push_content by 0075) kept
-- the whole requested_scope JSONB in a btree key. A btree entry is capped near 2.7 KB, so a tool_call whose
-- command is a few KB of high-entropy text (it does not compress) or a push_content scope listing many refs
-- failed the INSERT with SQLSTATE 54000, the request answered 500 and no approval row was ever created.
-- The key is now a sha256 of the scope's jsonb binary-send form: jsonb stores keys normalized, so equal
-- scopes hash equal, and the entry is a fixed 32 bytes. jsonb_send is immutable where convert_to is not.
-- The predicate is unchanged. The new index is built before the old one is dropped, so uniqueness holds
-- throughout, and the existing rows already satisfy it because the old index held.
CREATE UNIQUE INDEX IF NOT EXISTS approvals_pending_noncred_hash_uniq
    ON approvals (run_id, kind, sha256(jsonb_send(requested_scope)))
    WHERE state = 'PENDING' AND kind <> 'credential';

DROP INDEX IF EXISTS approvals_pending_noncred_uniq;
