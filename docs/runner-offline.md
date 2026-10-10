# Runner offline enforcement

A local runner retains agents and their files after losing its organisation connection. Its authenticated lease uses the organisation clock and local elapsed time. At the token lapse or run end, the runner stops the proxy and retries failed containment. Reconnecting does not extend an existing deadline. Revocation is latched locally and refuses further work.

Runner lifecycle state and decision spools live under the runner state directory with private permissions. Only the proxy receives its run's spool mount; the agent does not. A durable creation intent records the deterministic sandbox reference before provisioning so a failed create or restart can still contain a partially created proxy.

Each spool is bounded by 50,000 records and 32 MiB. Capacity is reserved before forwarding. Overflow refuses new egress until acknowledged replay drains it; a synthetic `egress.deny` with `rule_source: builtin:spool-full` reports the refusal count first. Records and acknowledgements survive process restarts. Storage corruption refuses admission instead of resetting history.

Replay uses the runner's proof-of-possession stream, including after a run token expires. The organisation verifies the runner's current claim, owner, revocation state and run binding. It commits audit rows and replay receipts together, applying the same masking and sealing rules as other audit ingestion. A lost acknowledgement can replay safely. Scan summaries retain their separate `llm.scan.*` events.

Migration `0141_runner_spool_cursors` adds metadata-only receipts with run and runner erasure cascades. Audit retention does not remove receipts, and runner erasure does not erase audit history. A changed spool identity, sequence gap or conflicting record hash is refused.

Historical replay for a killed run records audit history only. It cannot revive the run, renew credentials, update the activity clock or grant authority. Audit events use the organisation arrival time; the runner's timestamp appears as `observed_at` with `spooled: true` and `evidence_source: runner_asserted`.

Org execution identity is stored separately in `runner_executions`; immutable
`runner_execution_exits` receipts bind each accepted exit to its generation.
Migration `0142_runner_executions.sql` adds these tables. Generation issuance
and terminal teardown share the existing per-run operation lock. An incomplete
accepted tail blocks the next generation; a completed old receipt is a no-op.
The org binds the expected execution handle from its correlated command result
or explicit reconciliation, never from the first exit event.
