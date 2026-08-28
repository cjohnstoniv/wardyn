# Audit action vocabulary

Every row in the append-only audit log (`internal/types/types.go`'s
`AuditEvent`) carries a dotted `action` string. There is no enum or schema
file for it — call sites are the source of truth — so this page is the
generated-by-hand vocabulary reference: every action literal live in the tree
today, grep-curated from `internal/api`, `internal/broker`, `internal/lifecycle`,
`internal/approval`, `internal/identity/embedded`, and `internal/groundtruth`,
with its `Data` field names read from that call site. **OPERATIONS.md and
SSH.md point here** rather than re-listing actions themselves.

To reproduce or extend this table: `grep -rn 'auditEvent(\|"[a-z_]\+\.[a-z_.]\+"' internal/api/*.go`
(excluding `_test.go`) finds every call site; each site's `mustJSON(map[string]any{...})`
or `Data:` literal is the field list. There is no CI check tying this file to
the source — a new action can go undocumented — that is a known gap, not a
promise this page is exhaustive (see `internal/api/audit.go`'s `parseAuditFilter`
for the filterable envelope fields, which are stable regardless of action).

**Stable vs internal**, as used below: *stable* means another Wardyn doc
already commits to the name and its fields as an integration point (a SIEM
rule, a filter, an operator runbook) — treat it as a compatibility surface.
*Internal* means today's implementation detail: the name and fields exist
because that's what the call site happened to write, not because anything
outside `wardynd` depends on the shape. Nothing here is versioned; an
internal action can rename across releases without notice.

**Envelope fields added by the hash chain.** Since migration `0047` every event
also carries `prev_hash` and `row_hash` (hex SHA-256, computed by Postgres —
see [OPERATIONS.md](OPERATIONS.md#the-hash-chain--what-a-rewritten-row-looks-like)).
They are action-independent, and they ride the **audit-sink stream**
(webhook/syslog/file), which is what puts a head hash in your SIEM. `GET /audit`
and `GET /audit/export` do not select them, so they are absent there, and
neither is filterable; the chain is checked through
`GET /api/v1/audit/chain/verify`, not by reading rows back. The sweep emits no
audit action of its own — writing an integrity finding into the log the finding
is *about* would record it in the one place already under suspicion.

## Runs

| Action | When | Data fields | Where | Stable? |
|---|---|---|---|---|
| `run.create` | A run row is created (`POST /runs`, or system-created for a follow-on workspace-step run) | `agent`, `repo`, `policy_id`, `confinement_class`, `jti`, `inline_policy`; conditional: `interactive_start`, `seed_auto_tools`, `task_mode`, `tool_approvals` | `internal/api/runs.go:267` | internal |
| `run.dispatch` | Sandbox dispatch attempted or completed | `note`, `sandbox_ref` | `internal/api/runs_dispatch.go:138` | internal |
| `run.build` | BYOI/devcontainer image build for a run | `byoi_base`, `devcontainer_repo`, `error`, `image` | `internal/api/runs_create.go:777` | internal |
| `run.complete` | Run reaches a terminal state | `error`, `exit_code`, `panic`, `state` | `internal/api/runs_lifecycle.go:60` | internal |
| `run.fail` | Run fails before the agent starts (the D9 FailureHint gap this same register names) | `error`, `from` | `internal/api/runs_lifecycle.go:371` | internal |
| `run.kill` | Operator or owner kills a run | (run state transition) | `internal/api/runs_lifecycle.go:507` | internal |
| `run.exec` | Exec into a dispatched sandbox | `argv`, `error` | `internal/api/runs_dispatch.go:555` | internal |
| `run.files` | A workspace file-browse operation on a run fails | `error` | `internal/api/run_files.go:315` | internal |
| `run.interactive` | `interactive_start` path taken | `note`, `sandbox_ref` | `internal/api/runs_dispatch.go:526` | internal |
| `run.resources` | A run-resources query/update fails | `error` | `internal/api/run_resources.go:188` | internal |
| `run.selftest` | Post-dispatch selftest gate (agent-image liveness check before a run is usable) | `confinement_class`, `detail`, `error`, `exit_code`, `fail_closed` | `internal/api/runs_dispatch.go:792` | internal |
| `run.record.start` | A Record Mode session starts or fails to start | `allow_all_egress`, `confined`, `confinement`, `confinement_class`, `detail`, `label`, `mode`, `record_run_id` | `internal/api/record.go:314` | internal |
| `run.record.synthesize` | Record→Promote profile synthesis | `allowed_domains`, `anomalies`, `eligible_grants` | `internal/api/profile.go:156` | internal |
| `run.revoke` | Credential/identity revoked on stop (API path), or the lifecycle reaper's revoke attempt fails | `runner_error`, `identity_error`, `broker_error` | `internal/api/runs_lifecycle.go:198`; reaper failure at `internal/lifecycle/lifecycle.go:368` | internal |
| `run.autostop` | Idle-timeout autostop fires | `idle_for_sec`, `reason`, `threshold_sec` | `internal/lifecycle/lifecycle.go:346` | internal |
| `run.reconcile` | The orphan-sandbox reconciler acts on a run at boot | (reconcile outcome) | `internal/api/reconcile.go:677` | internal |
| `sandbox.orphan_sweep` | The boot-time orphan reconciler (`reconcileOrphanedSandbox`) FAILS to tear down a terminal run's still-live sandbox — emitted on failed teardown only; a still-failing teardown leaves the ref set for the next boot to retry | `sandbox_ref`, `teardown_error` | `internal/api/reconcile.go:212` (`stopSandboxOrAudit`, `internal/api/runs_lifecycle.go:164`) | internal |
| `sandbox.sweep` | `SweepTerminalSandboxes` (the callable sweep primitive over terminal runs with a live probed sandbox) FAILS to tear one down — emitted on failed teardown only, like its boot-time sibling | `sandbox_ref`, `teardown_error` | `internal/api/runs_lifecycle.go:236` (`stopSandboxOrAudit`, `internal/api/runs_lifecycle.go:164`) | internal |
| `run.artifact.redirect` | Package-registry (artifact) redirect configured for a run | `detail`, `ecosystem`, `error`, `header`, `host`, `integration_id`, `port`, `secret_name` | `internal/api/artifact_redirect.go:231` | internal |
| `run.policy.effective` | The effective (post-merge) policy snapshot is recorded at dispatch | (full policy snapshot) | `internal/api/runs_dispatch.go:390` | internal |
| `run.upstream_proxy.resolve` | Upstream (corporate) proxy resolution for a run | `reason` | `internal/api/runs_dispatch_mounts.go:102` | internal |
| `run.llm.bedrock` | Bedrock LLM credential/config wired into a run | `detail`, `hosts`, `mode`, `model`, `region` | `internal/api/runs_dispatch_llm.go:262` | internal |
| `run.llm.subscription_inject` | Subscription OAuth proxy-side inject wired into a run (see `subscription-proxy-inject` — the operator's live OAuth is injected proxy-side, never resident in the sandbox) | `detail`, `host`, `source`, `tls_mitm` | `internal/api/runs_dispatch_llm.go:364` | internal |
| `run.llm_inspection.secrets_resolve` | LLM egress content-inspection secret names resolved for a run | `missing`, `names`, `reason`, `resolved` | `internal/api/runs_dispatch.go:615` | internal |
| `llm.scan.*` | An outbound LLM content-inspection pass completes for a run's egress — the literal suffix is `sc.Action` (`llm.scan.alert`, `llm.scan.block`, `llm.scan.skipped`, or `llm.scan.blind`, per `egress.ScanSummary.Action`); security-relevant, and deliberately CONTENT-FREE — `findings` carries detector names, field paths, offsets, counts and masked samples only, never the matched bytes | `channel`, `coverage`, `finding_count`, `findings`, `host`, `mode`, `scanned`, `skipped`, `skip_reason` | `internal/api/internal.go:135` (`recordLLMScanAudit`) | internal |
| `run.env_secret.resolve` | One `env_secret` grant resolved store→sandbox env at dispatch (one event per grant). `outcome=failure` means the grant was SKIPPED — `reason` says why (unreadable scope, reserved secret name, no secret store, unresolvable secret, or a refusal to overwrite a platform-authored variable that is already set to a non-empty value) — and the variable is left ABSENT, never blank-set. Carries the variable name and the SECRET NAME only; the value is never logged, and is mask-registered for the run | `name`, `reason`, `secret_name` | `internal/api/runs_dispatch.go:714` | internal |
| `run.git_pat.egress` | A `git_pat` grant adds egress domains at launch | `added_domains` | `internal/api/runs_create.go:755` | internal |
| `run.git_pat.brokered_forge` | A `git_pat` grant is withheld at dispatch because the run is brokered for that forge — the git-broker route is its only route to it by name, so withholding the PAT is load-bearing, not belt-and-braces | `dropped_hosts`, `note` | `internal/api/runs_dispatch.go:165` (`auditBrokeredGrantDrop`, `internal/api/runs_dispatch_gitbroker.go:217`) | internal |
| `run.site_config.egress` | Site-config-derived egress domains added at launch | `added_domains` | `internal/api/runs_create.go:746` | internal |
| `run.ssh.egress` | SSH gateway egress domains added at launch | `added_domains` | `internal/api/runs_create.go:751` | internal |
| `run.ssh.brokered_forge` | An `ssh_key` grant is withheld at dispatch for the same brokered-forge reason as `run.git_pat.brokered_forge` | `dropped_hosts`, `note` | `internal/api/runs_dispatch.go:169` (`auditBrokeredGrantDrop`, `internal/api/runs_dispatch_gitbroker.go:217`) | internal |
| `run.ssh.unsupported_agent` | SSH-enabled run launched against an agent image without SSH support; the SSH host set is dropped | `dropped_hosts` | `internal/api/runs_create.go:664` | internal |
| `run.workspace.clone_egress` | Workspace-clone egress domains added at launch | `added_domains` | `internal/api/runs_create.go:720` | internal |
| `run.workspace.collision` | Launch refused because the workspace is already in use by another run | `other_runs` | `internal/api/runs.go:73` | internal |
| `run.workspace.creds` | Workspace-scoped integration credential resolved for a run | `integration_ref`, `type` | `internal/api/runs.go:215` | internal |
| `run.workspace.egress` | Workspace-derived egress domains added at launch | `added_domains` | `internal/api/runs_create.go:703` | internal |
| `run.workspace.requirement.egress` | A workspace-needs-scanner egress requirement is satisfied at launch | `added_domains`, `workspace_id` | `internal/api/runs_create.go:338` | internal |
| `run.workspace.requirement.integration` | A workspace-needs-scanner integration requirement is satisfied at launch | `header`, `injected_hosts`, `integration_id` | `internal/api/integrations_run.go:88` | internal |
| `run.workspace.requirement.secret` | A workspace-needs-scanner secret requirement is satisfied at launch | `host`, `secret_name` | `internal/api/runs_create.go:400` | internal |

## Workspaces & sources

| Action | When | Data fields | Where | Stable? |
|---|---|---|---|---|
| `workspace.create` | `POST /workspaces` | `name`, `owned_by`, `sources` | `internal/api/workspaces.go:431` | internal |
| `workspace.update` | `PUT /workspaces/{id}` | `image_changed`, `name`, `rescan_required`, `sources` | `internal/api/workspaces.go:557` | internal |
| `workspace.delete` | `DELETE /workspaces/{id}` | — | `internal/api/workspaces.go:871` | internal |
| `workspace.scan` | A workspace directory/repo scan (needs-scanner) runs | `detail`, `reason`, `scan_run_ids`, `sources`, `workspace_id` | `internal/api/workspace_run.go:725`, `internal/api/source_scan.go:288,329,346` | internal |
| `workspace.record` | The "workspace record" onboarding-import run completes | `anomalies`, `domains`, `kernel_sensor_blind`, `minted_grants`, `mode`, `task` | `internal/api/workspace_run.go:902` | internal |
| `workspace.requirement.write` | An admin/member edits a workspace-needs requirement from the approval flow | (requirement diff) | `internal/api/approvals.go:971` | internal |
| `workspace.requirements.write` | An admin/member replaces the workspace's whole requirements contract (`PUT /workspaces/{id}/requirements`) — distinct from the singular `workspace.requirement.write` above (a single-requirement edit from the approval flow); this is the map-wide replace | `count` | `internal/api/workspace_requirements.go:137` | internal |
| `workspace.envcode.write` | The onboarding "env code" (devcontainer/setup snippet) is written for a workspace | `files`, `skipped`, `skipped_files`, `written_files` | `internal/api/workspace_envcode.go:70` | internal |
| `workspace.egress.approve` | Operator/member approves a pending workspace egress decision (`always`/`session` scope write-back — the D28 non-atomicity this register names) | `domains`, `source` | `internal/api/approvals.go:885`, `internal/api/record.go:623` | internal |
| `workspace.egress.deny` | Operator/member denies a workspace egress decision | `domains`, `source` | `internal/api/approvals.go:883`, `internal/api/workspaces.go:694` | internal |
| `workspace.llm_cred.set` | An operator binds (or clears) the model/harness credential for a workspace/container (`PUT /workspaces/{id}/llm-cred`) | `integration_ref` | `internal/api/workspaces.go:723` | internal |
| `workspace.reassign` | An admin returns a member-owned workspace to the operator, `owned_by=""` (`POST /workspaces/{id}/reassign`) — the offboarding path for a member who has left | `from_owner` (the departed member's principal; `""` when the row was already operator-owned) | `internal/api/workspace_owner.go:60` | internal |
| `source.write` | A workspace source (dir/repo) is added or updated | `kind`, `locator`, `ref`, `writable` | `internal/api/sources.go:260` | internal |
| `source.delete` | A workspace source is removed | `detached_from`, `forced` | `internal/api/sources.go:342` | internal |
| `source.scan` | A single source's onboarding scan runs | `ai_advisor`, `ai_changed`, `confidence`, `detail`, `leak_findings`, `reason`, `scan_run_id`, `secret_reqs` | `internal/api/workspace_run.go:691`, `internal/api/source_scan.go:62` | internal |

**`workspace_owner` — the cross-user marker on every workspace-scoped write.**
Any event in this section whose target is a MEMBER-OWNED workspace
(`workspaces.owned_by` non-empty, migration `0048`) carries one extra Data
field, `workspace_owner`, **whenever the actor is not that owner** — i.e.
whenever an admin acts on a member's workspace for support or offboarding. That
includes the approval write-backs (`workspace.egress.approve`,
`workspace.egress.deny`, `workspace.requirement.write`), which are the most
common cross-user path of all: deciding an approval is owner-OR-admin, so an
admin choosing `always` on a member's run durably rewrites that member's
workspace. One rule stamps them all, in `internal/api/workspace_owner.go` —
`auditWorkspaceData` for a handler that holds the request,
`auditWorkspaceDataFor` for a writer that holds the acting principal as a
string — and it is what makes cross-user admin access QUERYABLE rather than
merely present in the log: filter `?actor=<admin>` and keep the rows where
`workspace_owner` is set and differs.

The deliberate exception is two EMIT SITES, not two actions: the `wardynd`-actor
emits that settle a governed run — `reconcileWorkspaceRun`'s `workspace.scan` and
`reconcileRecordRun`'s `workspace.record`, both in `internal/api/workspace_run.go`
— have no acting human to compare the owner against, so they never stamp. The
human who launched that run is recorded on the run, not on these events. The
`workspace.scan` ACTION is not exempt: emitted from `scanAttachedSources`
(`internal/api/source_scan.go`) it carries the requesting human as actor and
stamps `workspace_owner` like every other row above.

The actor is never rewritten to match. An admin acting on a member's workspace
is recorded as the ADMIN — there is no impersonation anywhere on this path, and
`TestWorkspaceOwner_NoImpersonation` (`internal/api/workspace_reassign_test.go`)
pins it across the write surface. An operator-owned workspace (`owned_by=""`,
every pre-0.6 row) never stamps the field, so an admin-only deployment's audit
Data is unchanged.

## Sessions, SSH & UI relay

| Action | When | Data fields | Where | Stable? |
|---|---|---|---|---|
| `session.attach` | A human attaches to a run's tmux session (console or CLI) | `cols`, `error`, `lane`, `reason`, `rows`, `sandbox_ref`, `transport` | `internal/api/attach.go` | internal |
| `session.detach` | A session detaches | `read_only`, `reason`, `transport` | `internal/api/attach.go` | internal |
| `session.takeover` | A second viewer takes over a held session (the write-lane single-holder rule) | `held_since`, `previous_holder`, `previous_source`, `taken_over` | `internal/api/attach.go` | internal |
| `session.recording` | A recording is attached to / detached from a session | — | `internal/api/attach.go:603` | internal |
| `recording.upload` | A sandbox uploads an asciinema-cast chunk for a run's recording session — audited on both outcomes, like every sibling recording lane, since a full store or an over-cap upload is exactly how a long session's provenance gets lost | `error` (failure only) | `internal/api/recording.go:120` | internal |
| `ssh.auth` | Every SSH-gateway connection attempt, success or failure — `docs/SSH.md` names this one **stable** and documents it as the residual-#19 correlate | `override`, `reason` | `internal/api/sshgateway.go:309` (failure), `internal/api/sshgateway.go:345` (success); documented `docs/SSH.md:251,303,308` | **stable** (documented) |
| `ssh.exec` | A command executed over the SSH gateway (argv + exit code only — no content, per D15) | `argv`, `error`, `exit` | `internal/api/sshgateway_channels.go:632`; documented `docs/SSH.md:346` | **stable** (documented) |
| `ssh.sftp` | An sftp transfer over the SSH gateway (byte count only — no payload/filenames, per D15) | `bytes`, `error` | `internal/api/sshgateway_channels.go`; documented `docs/SSH.md:346` | **stable** (documented) |
| `ssh.forward` | An `ssh -L` port-forward session | `bytes`, `error`, `port` | `internal/api/sshgateway_channels.go`; documented `docs/SSH.md:347` | **stable** (documented) |
| `ssh_key.add` | A human registers an SSH public key (`POST /me/ssh-keys`) | `name` | `internal/api/sshkeys.go:144` | internal |
| `ssh_key.delete` | A human removes a registered SSH public key | — | `internal/api/sshkeys.go:177` | internal |
| `ui.auth` | A UI-sandbox relay session is authorized or denied (see the D14/D27 residuals on this channel) | `app`, `host`, `port`, `reason` | `internal/api/uigateway.go:288` | internal |
| `ui.open` | A UI-sandbox relay session is opened | `app`, `duration_sec`, `port` | `internal/api/uigateway.go:652` | internal |
| `ui.close` | A UI-sandbox relay session closes | `app`, `duration_sec`, `port` | `internal/api/uigateway.go:657` | internal |
| `ui.start` | The relay starts (or fails to start) the sandbox-side app process | `app`, `launcher`, `port`, `reason` | `internal/api/uigateway.go:763` | internal |

## Secrets, credentials & identity

| Action | When | Data fields | Where | Stable? |
|---|---|---|---|---|
| `secret.write` | An admin writes a platform secret | — | `internal/api/secrets.go:170` | internal |
| `secret.delete` | An admin deletes a platform secret | — | `internal/api/secrets.go:185` | internal |
| `secret.read` | A registered secret is resolved for injection into a run (or fails to resolve) | `grant_id`, `host`, `jti`, `purpose`, `reason`, `source` | `internal/api/injection.go:118` | internal |
| `secret.rekey` | `wardynd -rotate-age-key` re-encrypted the whole store to a new age identity. SUCCESS only, and written by the one-shot maintenance process rather than the serving daemon (`actor_type` `system`, `actor` `wardyn/rotate-age-key`); an aborted rotation commits nothing and emits nothing. `target` is the store name (`pg`). Deliberately carries NO secret names — the sibling rows above each name one secret because each event IS one secret, whereas a single event listing the whole inventory would hand every configured sink the full set of names at once. `public_recipient` is the new age recipient (public by construction: it is what ciphertext is encrypted *to*), which is what lets an operator confirm which key the store now answers to | `public_recipient`, `secrets` | `cmd/wardynd/rekey.go:177` | internal |
| `credential.mint` | A credential is minted against an `APPROVED` approval (invariant 2 — mint IS the approval, same transaction). `lease` marks a mint the human did NOT individually decide: a `git_pat` whose approval carried `decision_scope=run` re-mints for the rest of the run under that one decision (the B2 per-run lease), and `decision_scope` beside it is the RAW stored value the lease turned on. Absent on every ordinary mint — a lease widens what one approval authorized, so it is stamped rather than left inferable | `approval_id`, `decision_scope`, `grant_id`, `host`, `jti`, `kind`, `lease`, `reason`, `scope` | `internal/broker/broker.go:888` (`mintEvent`), `internal/api/internal.go:453` | internal |
| `credential.revoke` | A minted credential is revoked (kill-switch, run stop) | — | `internal/broker/broker.go:849` | internal |
| `identity.mint` | A per-run SPIFFE identity (JWT-SVID) is minted | — | `internal/identity/embedded/embedded.go:187` | internal |
| `identity.renew` | A run's identity JWT is renewed | `expires_at`, `prev_jti`, `reason`, `run_state` | `internal/api/internal.go:698` | internal |
| `identity.revoke` | A run's identity is revoked at teardown | — | `internal/identity/embedded/embedded.go:263` | internal |
| `approval.decide` | A human decides a pending approval (approve/deny, any scope) | `approval_id`, `decision`, `reason` | `internal/approval/approval.go:135` | internal |
| `approval.expire` | The approval sweeper expires an undecided approval past its TTL | — | `internal/approval/approval.go:238` | internal |
| `harness.credential.captured` | A harness login flow (Claude Code subscription, etc.) captures a credential | `captured`, `provider`, `source` | `internal/api/harnesscred.go:620`, `internal/api/ssotoken.go:113` | internal |
| `harness.credential.disconnected` | A harness credential is disconnected | `captured`, `provider` | `internal/api/harnesscred.go:644` | internal |
| `harness.login.started` | A harness SSO login flow starts | `egress`, `provider`, `sso_start_url` | `internal/api/harnesscred.go:465` | internal |
| `token.create` | A signed-in human mints a per-user API token for themselves (`POST /me/tokens`). The actor is that human and `Target` is the token id — never the credential, which is returned once and stored only as a hash | `name`, `role` | `internal/api/apitokens.go:216` | internal |
| `token.revoke` | A per-user API token is revoked, by its owner (`DELETE /me/tokens/{id}`) or by an admin revoking anyone's (`DELETE /tokens/{id}`). `principal` names the token's OWNER, which is the point on the admin lane: the actor is the admin, the subject is someone else  A bulk sweep via `POST /sessions/revoke` records only the aggregate `tokens_revoked` on its `session.revoke` row — no per-token event. | `name`, `principal` | `internal/api/apitokens.go:288` | internal |
| `session.revoke` | An admin revokes active OIDC console sessions — `target` is the revoked `sub`, or `*` for a revoke-all (`POST /api/v1/sessions/revoke`, D16). Also revokes every unrevoked per-user API token the target holds — a `wdn_` bearer is that human's session in another form, so "revoke a human now" covers both ("all" covers every token in the deployment); `tokens_revoked` counts THIS call only (a retry after a failure counts just the remainder), and outcome `failure` marks a partial application — sessions revoked, token sweep errored mid-way | `scope` (`sub` or `all`), `sub` (sub-scoped only), `tokens_revoked` | `internal/api/sessions.go:63,68,78,83` | internal |

## Policy, capability & authorization

| Action | When | Data fields | Where | Stable? |
|---|---|---|---|---|
| `policy.create` | A named egress/run policy is created | `min_confinement_class`, `name` | `internal/api/policies.go:241` | internal |
| `policy.update` | A named policy is updated | `min_confinement_class`, `name` | `internal/api/policies.go:276` | internal |
| `policy.delete` | A named policy is deleted | — | `internal/api/policies.go:297` | internal |
| `policy.inline` | A one-off (inline, unsaved) policy is used to launch a run | `eligible_grants`, `min_confinement_class`, `workspace_mounts` | `internal/api/inline_policy.go:147` | internal |
| `capability.grant.created` | `POST /permissions/grants` (new) | `capability`, `effect`, `subject`, `subject_type`, `value` | `internal/api/permissions.go:167` | internal |
| `capability.grant.updated` | `POST /permissions/grants` (upsert on existing key) | `capability`, `effect`, `subject`, `subject_type`, `value` | `internal/api/permissions.go:169` | internal |
| `capability.grant.deleted` | `DELETE /permissions/grants/{id}` | — | `internal/api/permissions.go:199` | internal |
| `capability.enforcement.write` | `PUT /permissions/enforcement` (whole-map replace) | (saved enforcement map) | `internal/api/permissions.go:252`; documented `docs/OPERATIONS.md`'s capability-grants section | internal |
| `approval.second_human.bypass` | `WARDYN_EGRESS_SECOND_HUMAN` is set and the decider was the `admin-token` principal, so the four-eyes rule was BYPASSED (break-glass). A shared token carries no per-human identity to compare against, so it is exempt by design — this event is what keeps that exemption from being silent, and sits beside the `actor_type=system` `approval.decide` the decision itself writes | `reason`, `switch` | `internal/api/approvals.go:488` (`requireSecondHuman`) | internal |
| `authz.denied` | Every member denial that isn't a plain foreign-resource 404 — see `docs/OPERATIONS.md`'s "Every denial that isn't a 404" for the full `reason` vocabulary (`admin_surface`, `not_owner`, `byoi_member`, `capability_workspace`, `capability_egress_host`, `capability_secret`, `grant_pairing_not_eligible`, `second_human_required`) | `dropped`, `host`, `method`, `reason` | multiple sites; documented `docs/OPERATIONS.md:657-750` | **stable** (documented, closed `reason` enum) |
| `auth.failed` | A public-API authentication attempt fails: an `adminAuth` 401 (admin token not configured, missing bearer, or a bearer that doesn't match), OR a presented OIDC session cookie was rejected (tampered/malformed or expired) — whichever reason is more specific wins when both apply on the same request. Actor is always `system` (`wardyn/adminAuth`; no verified caller identity exists at this point). Content-free: `reason` is a closed enum (`admin_token_not_configured`, `missing_bearer_token`, `invalid_admin_token`, `invalid_session`, `expired_session`, `revoked_session`, `session_revocation_unavailable` — the last two only when session revocation is wired), never a user-supplied string; the request path is `Target`, the TCP peer is `SourceIP`. Rate-bound (process-global token bucket, 1/sec with a burst of 5) so a scanner throwing 401s cannot flood the append-only log | `reason` | `internal/api/http.go` (`auditAuthFailed`); session-rejection reason from `internal/auth/oidc/oidc.go`'s `Middleware`/`SessionRejectedFromContext` | internal |
| `egress.*` | The proxy reports an egress decision for a run — the literal suffix is the decision itself: `egress.allow`, `egress.deny`, or `egress.pending` (`egress.Decision`, `internal/egress/egress.go:27-31`). A synthetic `blind` scan decision emits only `llm.scan.blind`, never a duplicate `egress.allow` for the tunnel | `approval_id`, `host`, `method`, `path`, `port`, `rule_source` | `internal/api/internal.go:80` | internal |

## Kernel ground-truth (eBPF/Tetragon stream)

Defined as named constants (not scattered literals) in
`internal/groundtruth/groundtruth.go:44-64` — the `kernel.` prefix is enforced
by the ground-truth ingest endpoint (any action without it is rejected,
fail-closed) and is the field SIEM rules key on alongside `data.stream=ebpf`.
This stream is **detection, not prevention** (see the package doc comment) —
it never blocks, and it is blind inside CC3/Kata microVM guests.

| Action | When | Where | Stable? |
|---|---|---|---|
| `kernel.process.exec` | A process `execve` observed by the kernel sensor | `internal/groundtruth/groundtruth.go:50` | **stable** (const, documented) |
| `kernel.network.connect` | An outbound TCP connect observed by the sensor | `internal/groundtruth/groundtruth.go:52` | **stable** (const, documented) |
| `kernel.file.write` | A write to a sensitive path observed by the sensor | `internal/groundtruth/groundtruth.go:54` | **stable** (const, documented) |
| `kernel.sensor.heartbeat` | Periodic liveness beat from the ingest sidecar (`run_id` NULL) — `/healthz` keys `ebpf_groundtruth` state off this | `internal/groundtruth/groundtruth.go:60` | **stable** (const, documented) |
| `kernel.sensor.blind` | One-time event for a run the host eBPF sensor cannot see into (CC3/Kata) | `internal/groundtruth/groundtruth.go:64` | **stable** (const, documented) |

## Base images, integrations & site config

| Action | When | Data fields | Where | Stable? |
|---|---|---|---|---|
| `base_image.write` | An admin creates/updates a base agent image | `image`, `kind`, `steps` | `internal/api/base_images.go:122` | internal |
| `base_image.delete` | An admin deletes a base image | `detached_from`, `forced` | `internal/api/base_images.go:164` | internal |
| `integration.write` | An admin creates/updates an integration (secrets/egress/config/delivery) | `default_for`, `egress`, `header`, `kind` | `internal/api/setup_integrations.go:251` | internal |
| `integration.delete` | An admin deletes an integration | `credentials`, `egress`, `kind` | `internal/api/setup_integrations.go:292` | internal |
| `site_config.write` | `PUT /site-config` (full-document replace) | `egress_redirects_count`, `scm_hosts_count`, `upstream_proxy_configured` | `internal/api/site_config.go:355` | internal |
| `site_config.test_proxy` | The site-config "test upstream proxy" probe runs | `custom_target`, `elapsed_ms`, `intercepted`, `state`, `target_host` | `internal/api/site_config_probe.go:796` | internal |
| `site_config.test_redirect` | The site-config "test egress redirect" probe runs | `elapsed_ms`, `from_host`, `state`, `to_host` | `internal/api/site_config_probe.go:872` | internal |
| `site_config.test_probe` | An egress-redirect probe run's finalize step (via `finalizeRunTail`; `reclaimProbeRun`'s doc comment explains why the audited name must be this endpoint's own, never `run.compose`) | — | `internal/api/site_config_probe.go:476` | internal |

## System/reaper sources (no HTTP caller)

These are emitted by background sweepers, not request handlers — `ActorType`
is `types.ActorSystem` and `Actor` is a fixed component name
(`wardyn/lifecycle-reaper`, `wardyn/approval-sweeper`,
`wardyn/recording-sweeper`).

| Action | When | Data fields | Where | Stable? |
|---|---|---|---|---|
| `recording.retention.sweep` | The recordings age-based retention sweep runs (the retention knob `docs/ENV.md:47` names — see `docs/OPERATIONS.md`'s audit-retention paragraph for the asymmetry with the audit log itself, which has no such knob) | — | `cmd/wardynd/adapters.go:642` | internal |

## Notes on completeness

- This table is hand-curated from a source grep, not generated by a build
  step. A newly-added action literal will not appear here until this file is
  updated by hand — there is no CI gate enforcing it (unlike, for example,
  the closed-enum DB `CHECK` constraints `internal/db`'s
  `TestClosedEnumChecksMatchConstants` pins).
- `Data` field lists were read from the `mustJSON(map[string]any{...})` or
  `Data:` literal at the cited call site; a field marked here can still be
  `omitempty`'d away on a given event (e.g. `error` only appears on a
  `failure` outcome).
- Two related documents cover ground already: `docs/OPERATIONS.md`'s "Every
  denial that isn't a 404" owns `authz.denied`'s `reason` vocabulary in full,
  and `docs/SSH.md`'s "Audit actions" owns the four `ssh.*` actions — both are
  referenced above rather than restated.
