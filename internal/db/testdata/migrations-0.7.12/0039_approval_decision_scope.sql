-- How far a human's approve/deny decision reaches: once (one connection), run
-- (rest of this run — the legacy meaning of every decision made before this
-- column existed), until (bounded by decision_expires_at), or always (also
-- persisted onto the workspace, see 0040). Only egress_domain approvals ever
-- carry a non-default scope.
--
-- NOT NULL DEFAULT '' rather than nullable, because scanApproval scans this
-- positionally into a plain string-kind field and pgx cannot scan SQL NULL into
-- one. That is the same idiom decided_by / minted_jti / reason already use
-- (0001_init.sql). '' means "no decision recorded" — honest for a PENDING row,
-- and omitempty keeps it off the wire there, so we never assert a decision
-- nobody made. DEFAULT 'run' would.
--
-- The CHECK deliberately hoists '' into its own OR instead of listing it inside
-- IN (...). internal/db/migrations_check_test.go's quotedRe is `'([^']+)'`,
-- which requires at least one character between the quotes: on
-- IN ('','once','run','until','always') it tokenizes to [",", ",", ",", ","]
-- rather than the five values. Written this way, checkInRe/quotedRe parse the
-- four real scopes cleanly and TestClosedEnumChecksMatchConstants can carry an
-- approvals.decision_scope case against the four Go constants with no
-- test-helper changes and no '' special case.
--
-- decision_expires_at IS nullable — it scans into *time.Time, matching
-- decided_at. It is NOT the same clock as the EXPIRED state or the
-- approval.expire sweeper, which age out stale PENDING requests; this bounds a
-- grant that was actually made.
ALTER TABLE approvals
    ADD COLUMN IF NOT EXISTS decision_scope TEXT NOT NULL DEFAULT ''
        CHECK (decision_scope IN ('once', 'run', 'until', 'always') OR decision_scope = ''),
    ADD COLUMN IF NOT EXISTS decision_expires_at TIMESTAMPTZ;
