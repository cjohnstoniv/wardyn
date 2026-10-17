# Runner offline enforcement

A local runner retains agents and their files after losing its organisation connection. Its authenticated lease uses the organisation clock and local elapsed time. At the token lapse or run end, the runner stops the proxy and retries failed containment. Reconnecting does not extend an existing deadline. Revocation is latched locally and refuses further work.

Runner lifecycle state and decision spools live under the runner state directory with private permissions. Only the proxy receives its run's spool mount; the agent does not. A durable creation intent records the deterministic sandbox reference before provisioning so a failed create or restart can still contain a partially created proxy.

Each spool is bounded by 50,000 records and 32 MiB. Capacity is reserved before forwarding. A full spool holds a new request for up to 5 seconds, then refuses it with a retryable 503 and counts it; a synthetic `egress.deny` with `rule_source: builtin:spool-full` reports the refusal count first on replay. At most 32 requests hold a reservation at once; the next one waits for room the same way. A request is refused only while the spool is full, not until that count is reported. A request whose reservation timed out behind others or whose client cancelled gets a 503 and is not counted as a dropped decision. Records and acknowledgements survive process restarts. Storage corruption refuses admission instead of resetting history.

Replay uses the runner's proof-of-possession stream, including after a run token expires. The organisation verifies the runner's current claim, owner, revocation state and run binding. It commits audit rows and replay receipts together, applying the same masking and sealing rules as other audit ingestion. A lost acknowledgement can replay safely. Scan summaries retain their separate `llm.scan.*` events.

Migration `0147_runner_spool_cursors` adds metadata-only receipts with run and runner erasure cascades. Audit retention does not remove receipts, and runner erasure does not erase audit history. A changed spool identity, sequence gap or conflicting record hash is refused.

Migration `0149_runner_inventories` stores an authenticated session's original run candidates and bounded STATE page observations. Page progress and observations commit together. Only a complete inventory can identify an omitted, unchanged original run; interrupted pages and newly created runs cannot imply loss. These internal records neither acknowledge audit history nor authorize execution. A fresh session replaces the preceding inventory records.

Migration `0150_runner_action_binding` binds each queued runner action to the runner owner, the run's exact reference and execution generation, and for a proxy stop the exact loss it contains. The binding is written once and checked again when the runner reports the result; a result from another runner, run or identity is refused. Results are `success`, `deferred` or, since migration `0175_runner_action_refused`, `refused`, and use the organisation's arrival time. A repeated identical result is acknowledged without a second effect; a changed one is refused. A deferred result keeps the run fenced. A `refused` result means the runner changed nothing, because the reference is not its own or names a container it cannot prove it created. It closes the action without the reference and generation check, so it no longer fences the run, and it is audited `runner.action.refuse`. Actions queued before this migration still fence their run but are never delivered or settled. Each reconnect delivers at most 100 actions, oldest first, and readiness waits until none remain open. A database trigger refuses any change to the binding or to a written result. An organisation teardown issued while the runner is offline queues the matching action for its current run, and proxy-stop requirements found in a reconnect report commit with that report page. Revoking the runner settles its open and deferred actions in the same transaction that ends their runs KILLED; the rows are kept.

Migration `0155_runner_asserted_subjects` lets a queued action name a run the organisation has no row for, such as an erased or restored-over run that a runner still reports. Such an asserted subject has no owner and no execution generation. It is contained by a kill and then a quarantine bound to the runner-reported run and reference; the runner's own generation travels only inside the quarantine proof, marked asserted. Its other runs reach READY as usual. A runner can have at most 100 asserted subjects, settled ones included; past that its inventory is held not-ready and `runner.inventory.hold` is audited. That row repeats on each reconnect while the hold lasts, and is the operator's signal. The hold lifts as settled subjects age out, 90 days after their quarantine succeeded, or at once when the operator revokes the runner and enrolls a new one. A deferred kill or quarantine continues in one successor action after each completed inventory. Nothing for an asserted subject is acknowledged or retired. Its local spool, output and exit are deleted by the runner 90 days after the quarantine, or at once when the runner is revoked, in either case after any call already in flight for the run finishes; the organisation deletes its action rows 90 days after the quarantine succeeded.

Historical replay is accepted for a run that is `RUNNING` or in any terminal state, not only a killed one, and records audit history only. It cannot revive the run, renew credentials, update the activity clock or grant authority. Audit events use the organisation arrival time; the runner's timestamp appears as `observed_at` with `spooled: true` and `evidence_source: runner_asserted`.

Org execution identity is stored separately in `runner_executions`; immutable
`runner_execution_exits` receipts bind each accepted exit to its generation.
Migration `0148_runner_executions.sql` adds these tables. Generation issuance
and terminal teardown share the existing per-run operation lock. An incomplete
accepted tail blocks the next generation; a completed old receipt is a no-op.
The org binds the expected execution handle from its correlated command result
or explicit reconciliation, never from the first exit event.

Exit delivery first commits an immutable pending receipt. This ACK does not bind
the reported execution handle or finish the run. The org independently recovers
the generation's durable command result, then a background worker commits the
matching terminal transition and audit. Pending receipts and incomplete cleanup
block another generation. The worker retries shared terminal effects after
restart; checked stream callbacks never wait for same-peer teardown RPCs.

Live activity uses a separate, bounded per-run marker under the proxy-only state
mount. Admission captures a fresh connection epoch; reconnect invalidates it.
Only a new admitted decision from the current epoch may send an unbuffered
activity signal. Historical spool replay and retained old markers cannot renew
the idle clock, mint credentials, or revive a run.

Reconnect STATE cannot restore credentials or lifecycle state. The org rejects
stale execution assertions and missing live sandboxes before READY. A fresh
connection drop alone has no teardown effect; database token lapse enters the
existing kept-loss path, including for headless local runs. Missing or omitted
already-kept sandboxes do not create a permanent reconnect loop, while pending
containment actions continue to block admission independently.

A kept outage sandbox whose proxy remains up requires a durable stop-proxy action
before the next STATE/READY. Only that action's first exact successful result can
record late containment, in the same transaction as its acknowledgment. The
proof binds the owner, runner, reference, lost timestamp and issued execution
generation; a pre-execution sandbox instead requires that no execution has yet
been issued. The queued proof stays immutable, and another incarnation remains
fenced until settlement. The audit records organisation arrival time and labels
the runner's observed time as asserted evidence. It never restores credentials.
