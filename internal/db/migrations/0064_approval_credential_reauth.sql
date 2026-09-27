-- Add the approval kind CREDENTIAL_REAUTH to the approvals.kind CHECK.
--
-- A run's own model credential can lapse MID-RUN. Before 0.7.6 that was
-- terminal: the sandbox's next credential exchange failed and the agent lost
-- its context. 0.7.6 HOLDS that one request in the proxy sidecar while the
-- credential's owner signs in again, and this row is how the wait is made
-- visible to the person -- see internal/types/types.go ApprovalCredentialReauth,
-- internal/api/injection_awssso.go (the raise) and
-- internal/egress/proxy/credhold.go (the hold).
--
-- It is NOT a decision kind. Nobody approves or denies it: the capture of a new
-- sign-in resolves it (internal/api ResolveReauth, audit action
-- credential.reauth.resolve), and Server.decide answers 409 for it so a
-- security operator cannot Deny a row whose next poll would simply raise a
-- fresh one. The row still moves to APPROVED, so every existing list, count,
-- sweeper and terminal-run cascade reads it unchanged.
--
-- The 0001 CHECK is an inline column CHECK, so Postgres named it
-- "approvals_kind_check"; this REPLACES it rather than adding a second one, for
-- 0062_approval_cancelled.sql's reason exactly: two CHECKs on one column are
-- ANDed, which would leave the admitted set as their intersection --
-- 'credential_reauth' rejected by the first constraint, with every gate green.
-- 0062 (and 0003_run_state_completed.sql before it) is the precedent for this
-- drop-then-add shape; drop-if-exists keeps it idempotent and tolerant of an
-- environment where the constraint name differs.
--
-- ADDITIVE ONLY: it widens an admitted set, so an upgrade needs no backfill and
-- no row changes. A DOWNGRADE to 0.7.5 with credential_reauth rows present is
-- UNSUPPORTED (the old CHECK would refuse them) -- the upgrade runbook's
-- pg_dump is the rollback, as docs/OPERATIONS.md says.
--
-- The partial unique index from 0022 (approvals_pending_noncred_uniq, WHERE
-- state='PENDING' AND kind <> 'credential') now also covers this kind, which is
-- exactly what is wanted: at most ONE PENDING credential_reauth per
-- (run, requested_scope), backing approval.RequestApproval's list-then-create
-- dedup with a real constraint. One lapsed credential per run is one request,
-- however many of the sandbox's concurrent calls discover it.

ALTER TABLE approvals DROP CONSTRAINT IF EXISTS approvals_kind_check;

ALTER TABLE approvals ADD CONSTRAINT approvals_kind_check CHECK (kind IN (
    'credential',
    'egress_domain',
    'tool_call',
    'credential_reauth'
));
