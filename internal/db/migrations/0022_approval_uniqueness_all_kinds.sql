-- At most ONE open (PENDING) approval per (run, kind, requested_scope) for the
-- NON-credential kinds (egress_domain, tool_call). This backs approval.
-- RequestApproval's list-then-create dedup with a real constraint, closing the
-- same SELECT-then-INSERT race migration 0002 closed for credentials: two
-- concurrent raises of the same run+kind+scope can no longer persist duplicate
-- PENDING rows.
--
-- The credential kind keeps its own grant_id-keyed index from 0002
-- (approvals_pending_credential_uniq) — its dedup key is the grant, not the
-- scope — so this index deliberately excludes kind='credential' and does not
-- change the credential path's behavior.
--
-- requested_scope is JSONB, whose stored form is key-order-normalized, matching
-- RequestApproval's unmarshal/remarshal scope normalization closely enough to
-- serve as a backstop; the app-level dedup remains the primary path and this
-- index is the last-writer-loses safety net under concurrency.
-- ponytail: JSONB btree equality is the backstop; if scope canonicalization
-- ever needs to be byte-exact with the Go hash, add a stored scope_hash column.
CREATE UNIQUE INDEX IF NOT EXISTS approvals_pending_noncred_uniq
    ON approvals (run_id, kind, requested_scope)
    WHERE state = 'PENDING' AND kind <> 'credential';
