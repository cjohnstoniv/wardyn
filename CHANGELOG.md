# Changelog

All notable changes to Wardyn are documented here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/); Wardyn is **pre-alpha**
and does not yet follow semantic versioning (interfaces are not stable).

This file holds `[Unreleased]` and the newest released minor line. Older releases, newest first:
[0.9](docs/releases/0.9.md), [0.8](docs/releases/0.8.md), [0.7](docs/releases/0.7.md),
[0.6](docs/releases/0.6.md), [0.5](docs/releases/0.5.md) and [0.4](docs/releases/0.4.md). **v0.3.1 and older live in
[CHANGELOG-ARCHIVE.md](CHANGELOG-ARCHIVE.md).**

## [Unreleased]

### Added

- On a Kubernetes runner, the owner of a run lost to an outage can copy work out of it until the run's end. The run is kept with its agent running and no network. Its owner, and nobody else, can open the terminal, SSH or the Files panel's download, each where the run's policy allows it; nothing can be uploaded through the Files panel, the command route and UI apps stay closed, and the run still cannot be revived. GET /runs/{id} carries kept_copy_out: true while the door is open.
- The run.lost audit row carries cause: control_plane_outage when the lapse sweep judged the lapse an outage, and cause: runner_link_lost when it was marked because a runner disconnected. A new run.lapse_sweep.hold row is written once per outage in which an unattended run was kept, per server process.
- WARDYN_ORPHAN_SWEEP_HOLD (off by default, at most 24 hours) keeps a server Docker sandbox that has no run row, for example after a database restore, instead of deleting it. Its proxy and agent are stopped and only its files are kept. Such a sandbox has no record, cannot be ended or opened from the console, and is copied out only with host access.

### Changed

- After the database or the server has been unreachable, the lapse sweep keeps the server's own Docker runs where it used to end the unattended ones. A run whose token lapsed while the server could not have renewed it is marked lost to an outage, its proxy is stopped, and its agent and files are kept, for unattended runs too; revive brings it back. This applies for 25 minutes after the database is reachable again or the server starts (WARDYN_LAPSE_RECOVERY_GRACE), and while none of the server's runs has renewed for 20 minutes or at least a quarter of them, and at least five, lapsed within 30 minutes (WARDYN_LAPSE_MASS_SHARE, WARDYN_LAPSE_MASS_MIN). Outside those, a proxy that dies on its own is ended as before.
- Server Kubernetes runs are still ended after a long outage. Such an end is recorded on the run.lost audit row (kept: false) in place of run.reconcile, and sets no failure hint on the run.
- Terminal writers' leases: a replica renews all the leases it holds in one database statement per tick, up to 500 a statement, and a lease found lapsed is taken back on its own, so many attached writers cost one round trip per 500, and one writer's recovery no longer holds up the other renewals.
- Terminal input: concurrent checks of the same writer's lease share one database read, and a person's presence stamp no longer holds a keystroke.

## [0.10.3] — 2026-11-23

### Added

- An issuer portal's person can read their own non-interactive run's command output on a runner that does not capture it, such as Kubernetes: `GET /runs/{id}/output` recovers it from the run's recording, masked, with no input and no attach session. An interactive run's output and its pane snapshot stay closed to every portal.
- Audit action `run.output.refuse`: a portal's read of `GET /runs/{id}/output` that ends with 409 `run_output_not_captured`, `run_output_not_kept` or `run_output_interactive`, or the retention window's 410 `run_output_expired`, records the same reason and status the answer carried.

### Changed

- An issuer portal's person runs in the portal's pinned unit, and any unit under it, without holding a membership there. With no `run_in`, a run now lands in the pin unit unless the person holds a home membership in the pin unit or a unit under it. So after upgrading, a person who joined a unit as a workaround may see runs land in the pin unit instead, and a membership outside the pin no longer refuses a run with no `run_in`. A `run_in` outside the pin is still refused. A strict workspace, or an organisation component pinned to a unit, still needs an actual membership, at launch and at every extension, resume and revive. Console portals and console sign-ins are unchanged. See [where a portal's person runs](docs/operations/api-tokens.md#where-a-portals-person-runs).
- `GET /runs/{id}/output` no longer writes an `authz.denied` row when it withholds the pane snapshot or recording-derived output from a portal. A console security admin reading another person's pane snapshot or recording output is still audited `not_owner`.

### Fixed

- The API tokens page now says that a provider token issued before a suspension stays refused after the person is reactivated: its exchange answers 400 `invalid_grant`, audited `session_revoked`.
- An SSH helper whose shell was still opening when the run's owner joined now sees why the session closed, instead of a silent close (#4184).

## [0.10.2] — 2026-11-22

### Added

- A portal can sign its people in against its own identity provider: register it with an `issuer` (and an `internal_issuer` when Wardyn must reach the provider at another address). Wardyn verifies the provider's tokens itself and admits a person by the portal's group or by `portal_managed_admission`, and by one more claim it names when you set one. It mints a grant that acts as a `user` in the unit the portal is pinned to and every unit under it, so a portal pinned to a parent unit reaches its child units. The grant lasts at most `grant_ttl_seconds` (300 to 600) and ends at the token's own signed end if that comes first, so read `expires_in` (it can be as low as 5 seconds). These people are never console sign-ins. Issuer portals stay off until an admin calls `POST /admin/portal-issuers/activate`, once, after every older wardynd is stopped ([issuer portals](docs/operations/api-tokens.md#portals-with-their-own-identity-issuer)). Retiring an unused issuer binding is `DELETE /admin/portal-issuers/{k}`, and changing an issuer portal's admission rule is `PUT /admin/delegates/{id}/admission`.
- Migration `0294_portal_issuer` adds columns to `delegates` and `principal_identities`, the tables `portal_issuers`, `console_issuers`, `portal_grants` and `portal_person_portals`, and a trigger that refuses a portal person's deactivation deadline, reactivation or purge written by anything but the portal's own store writes. It refuses to run, naming the count, while a `delegates` row with no issuer has an empty `scope_group`.
- Split-role installs (`WARDYN_PG_MIGRATE_DSN` set): this release's migrations grant `SELECT, INSERT` on `console_issuers`, and
  `SELECT, INSERT, UPDATE, DELETE` on `portal_issuers`, `portal_grants` and `portal_person_portals`, to every role that was granted `UPDATE` on
  `delegates`, other than that table's owner. If the app role is not one of those roles or a member of one, and does not
  pick up new tables through the `ALTER DEFAULT PRIVILEGES` step in Operations, grant it the same on each table as soon as
  the migration has run. Until then a server with console sign-in configured does not start.
- A `custom_endpoint` model provider's `oidc` block can carry a `service_grant`: a client-credentials grant that lists the service identities allowed to use it. An admin writes its client secret with `PUT /model-providers/{id}/service-grant/secret` and removes the grant with `DELETE /model-providers/{id}/service-grant`; a removed grant's secret is deleted by a background clean-up, which `POST /service-grant-cleanups/{uid}/retry` retries. A write that adds a grant answers 409 `service_grants_not_activated` until the same switch-on as issuer portals. Migration `0295_provider_service_grant` adds the table that records each server's last token request for a grant.
- Split-role installs (`WARDYN_PG_MIGRATE_DSN` set): this release's migrations grant `SELECT, INSERT, UPDATE, DELETE` on
  `provider_service_grant_status` to every role that was granted `UPDATE` on `delegates`, other than that table's owner. If
  the app role is not one of those roles or a member of one, and does not pick up new tables through the `ALTER DEFAULT
  PRIVILEGES` step in Operations, grant it the same on the table as soon as the migration has run. Until then no service
  grant's token request outcome is recorded. They also grant `SELECT, INSERT, UPDATE` on `drive_legacy_releases` to every
  role that was granted UPDATE on the user drives table, other than its owner. An app role outside those needs the
  same grant on that table, or the release of Docker drives created before the upgrade fails.
- `WARDYN_DAEMON_PROXY_ALWAYS` (flag `-daemon-proxy-always`) names hosts to which wardynd sends a service grant's token request, and the discovery and key fetches of an issuer portal registration with no internal issuer address, through its proxy even when `WARDYN_DAEMON_NO_PROXY` covers them; a proxied portal fetch connects to the address wardynd checked and trusts only the new `WARDYN_PORTAL_PROXIED_ISSUER_CA_FILE`. No other call changes.
- A super admin can allocate a team drive to an issuer portal, and a run started through that portal can mount it. Migration `0297_portal_team_drives` adds the `portal` subject type to drive allocations and `agent_runs.portal_drives`; it creates no table, so the split-role grants above are unchanged.
- `WARDYN_PORTAL_INTERNAL_ISSUER_HOSTS` lists the hosts an issuer portal's `internal_issuer` may name, and `WARDYN_PORTAL_INTERNAL_ISSUER_CA_FILE` adds a CA bundle for those hosts only. A failed refresh of an issuer's discovery or keys counts on `wardyn_portal_issuer_refresh_failures_total`.
- The audit log has rows for these: `portal_issuers.activate`, `portal_issuers.activate_override`, `portal_issuers.old_client_seen`, `portal_issuer.retire`, `delegate.admission.change`, and `model_provider.service_grant.mint`, `.secret_write` and `.secret_delete`. A portal person's first exchange writes one `person.create` row, which carries no email. Suspending, reactivating and purging a portal person write `person.suspend`, `person.reactivate`, `person.purge` and `identity.deactivate` rows. Lifecycle denials include a security admin trying to schedule a purge and a purge that loses to reactivation. A portal's browser-app or desktop connection into a sandbox writes a `ui.dispatch` row when it closes; a terminal does not.
- `GET /runs?service=1` returns only the runs started with Keep it running, and the Runs page's Services segment uses it, so the segment no longer depends on which page of runs was loaded.
- `POST /api/v1/admin/drives/legacy-docker/release` (administrators) releases Docker drives created before the upgrade, which 0.10.2 holds until released; the call is audited as `drive.legacy_docker.release`.
- An administrator can suspend, reactivate and permanently purge a person an issuer portal introduced. `GET /people/portal-person?issuer=` and `person_id=` find them by issuer and provider id, and `GET /people/{principal}/portals` lists the portals they used, each with its unit and when it was revoked; neither answer carries a purge date. `POST /people/{principal}/suspend` deactivates them: their grants go with the deactivation, their API tokens and SSH keys are swept and every run of theirs is killed. A suspension that has not finished answers 503 `portal_lifecycle_incomplete`: send it again, or leave it to the sweep, which retries pending steps every 15 minutes whether or not SCIM is set up. A security admin can suspend, but an optional `purge_after_seconds` (up to ten years, which schedules the purge) needs a super admin, as do `POST /people/{principal}/reactivate`, which clears the suspension and any purge date, and `POST /people/{principal}/purge`. A purge needs a suspended person, cannot be undone, and makes every portal refuse that person from then on. The answers report `principal`, `state`, `deactivated_at`, `purge_after` and `purged_at`. `GET /admin/portal-lifecycle/status` lists the people who are deactivated and the steps that are stuck, and `GET /scim/status` lists only the people of its own SCIM issuer and tenant ([the steps](docs/operations/api-tokens.md#suspend-reactivate-and-purge-a-portals-person)).
- The People page finds a portal person by exact issuer and person ID. Their drawer lists the portals they used, with each portal's assigned unit; a revoked portal stays listed and is marked.
- The Changes list names held portal admission changes and portal issuer retirements.
- An issuer portal can set `ui_session_renewal` so a person's browser-app session lasts as long as the portal keeps admitting them, and at most `WARDYN_UI_SANDBOX_SESSION_TTL` after the ticket was redeemed. The session follows the grants the same portal minted for that person while one of them was live. A new request is refused once none of them is live, the portal is revoked, or the person or the portal's unit refuses; a stream already open ends at the end it was admitted with. Desktops (the sign-in desktop included), terminals and a helper's support sessions never renew, and a console portal cannot set it.

### Changed

- The server and its subcommands name their database sessions in `application_name`: `wardynd/<version>/<replica>` for the server and `wardynd-<tool>/<version>` for a subcommand. The issuer portal switch-on reads these labels to tell a 0.10.2 server from an older one.
- A person-erasure `credentials` count, `delegated_deleted`, now includes issuer-portal grants, and the erasure deletes the record of which portals a portal person used.
- A run's policy view marks an allow entry that Wardyn opens only some destinations of as Partly inspected, and the answer's `host_marks.partial` lists those entries. A Tunnel only entry says Wardyn leaves the TLS connection intact, though a corporate upstream proxy that inspects traffic can still break an app that pins its certificate.
- The Mac recovery steps copy a `mac_disk` image with `cp -c` on the same APFS volume, and with `rsync --sparse` otherwise.
- A macOS VM run whose create fails after the Mac was asked now keeps its kept work folder (unmarked and attachable) instead of dropping it; delete it from the drive list if it is not wanted.
- An issuer portal's requests are held to the authority check that admitted them. Wardyn must start a local operation or begin sending a remote start within 25 seconds of that check; a remote runner can receive and execute it later. A write that never started passes no bytes. The request is then refused with a retryable 503, so an app that needs longer than 25 seconds to start from cold can be refused even when nothing was revoked, and a retry may find it started. A write that began in time can still be interrupted with an unknown byte count, a terminal that needed a resume can answer 502 `attach_resume_failed`, and an app's launcher may never become ready. Each portal request goes out on a connection of its own, never a shared one. On a runner reached over a network link, a start the portal sends after its time is refused at the link, and no runner change is needed. The bound ends where Wardyn hands the start to the link: a start handed over in time can still be delivered and run later on a busy runner. A terminal attach with a portal ticket answers 409 `attach_local_unavailable` with `detail` `issuer_portal` on the local route, and goes through the organisation.
- Revoking an issuer portal, or suspending or purging a person, now reaches that person's open connections. A new request is checked afresh every time, with no cached pass. An open browser app or terminal is re-checked every 30 seconds, an event stream every 15 seconds and a desktop every 30 seconds, each check limited to 5, 5 and 10 seconds, and the connection is cut at the first check that fails or at the end it was admitted with: at most about 35, 20, 35 and 40 seconds after the event. Those are the design limits, not measured times. The person sees the connection drop with no clean close, and is refused when they reconnect. An event stream whose opening check fails answers 403 `revoked`, or 503 `revocation_unavailable` when it cannot check; an opening read that gets no answer within 5 seconds is a 503 too; and a ticket for a run that no longer exists is refused 403 `ui_gateway_ticket_invalid` where it answered 503.
- On the link between wardynd and a runner, opening a stream while 256 are held is refused at once, and a stream that was aborted counts against the 256 until its reset is sent.
- A model provider's service grant records each token request. A successful token request means the provider issued a token; a `model_provider.service_grant.mint` success row without `superseded` records a committed publication, and nothing says anything received it. A publication that does not commit is settled afterwards, or spooled when the database is down, so the database holds every mint row except one whose publication and settle both failed, or whose row could not be prepared (the token is then not published). Each server sends sink copies best effort from a queue of 64, in queue order and not commit order. A server sends at most 16 token requests, counting the rows not yet recorded, and a request past that sends nothing and is refused. The [operations guide](docs/OPERATIONS.md#a-service-grants-token-request-timing-recording-and-limits) has the timing, the logged outcomes and the limits.
- A drive delete whose reply from the runner is empty or not one Wardyn knows is no longer taken as done: it answers 503 `member_drive_delete_failed`, "Wardyn could not confirm that deletion finished.", and the drive is kept and held until a later delete confirms it. Every server retries the release record of `POST /api/v1/admin/drives/legacy-docker/release` once a minute, and a repeated call answers `{released: 0, replayed: [...]}` once it has delivered the earlier call's record, on any runner target.
- `WARDYN_DAEMON_PROXY_ALWAYS` takes exact host names: boot refuses an IP address, a numeric name such as `127.1`, `localhost`, a host wardynd always reaches directly, a bad entry, or a list with no usable proxy, in every mode that installs the daemon's transport. A refused portal fetch through the proxy logs its `detail` as `name not resolved`, `request failed`, `address refused` or `host not comparable` as well as the proxy details listed in the issuer portal guide.
- A discovery or key fetch that fails a rule is enforced before it is logged or reported, and the rule's cause stays the one reported through later failed retries and outages until a refresh passes. While a discovery or key address is refused, every unavailable answer names its cause.
- An audit export that retention interrupts, because a partition was dropped while it ran, now ends as incomplete (503 before the first byte, an aborted transfer after) instead of looking finished; run it again.
- The image published for each commit is checked before it is signed: the version command runs for the two amd64 CLIs inside the image and must report the commit, and every regular file copied out of each architecture's image (the daemon and the host CLIs) must contain the revision text. Those files are read, never run, on the publishing runner. `:sha-<short>` is the only name pushed first. `:latest` and the other names move only after the check and the signature.

### Fixed

- An answer to a help request on the run page shows at once, even when the next status read fails, and a late accept no longer brings back a grant that has since ended or been replaced.
- A sign-in whose token names a signing key the issuer portal has not seen, while the portal's keys cannot be fetched, now answers with the same `Retry-After` as the portal's other key outages, where it sent none.
- `wardyn run output` on a terminal now shows the DEL character as `\x7f`, as the help text promised.
- `wardynd -recovery-rehearse` now reports a key fingerprint failure first when the platform key file differs from the one recorded in the set's manifest, or is unset. Manifests from older versions skip that comparison.
- `make agent-image-claude-code-multiarch` refuses `AWS_CLI_INSTALL=staged`, since one staged zip is one architecture. Build each architecture separately, or leave it unset.
- Images published per commit report `<version>+<commit>` in `wardyn --version` and in `/healthz`. Release images and tagged builds still report the bare version.
- On an Intel Mac, the runner package no longer sets up the virtual machine ledger, which needs Apple silicon, so the install no longer fails there. On Apple silicon a failure there still fails the install.
- The release build's check that every macOS binary loads on macOS 26 now also refuses a minimum of 26.0.1 or any other 26.0 patch.
- `wardyn-runnerd doctor` now fails a start the runner refused before any stage, and keeps that failure until the runner next connects, instead of reporting it as stopped.
- The Runs page's Services segment is read by the server, so it no longer shows an empty or partial list when the loaded page of runs holds few service runs. A segment that cannot show them all says so and offers to look further back.
- The approval notification card says "No deliveries marked failed in the past hour" when none failed.
- The boundary review shows a change's Data out value and lock, and says "No existing policies would change" when none would.
- A service with no health reading for 10 minutes reads Health unknown.
- The audit retention status stops showing a decrease as pending once its date has passed.
- A notification that names a person is dismissed on sign-out, and a late one from a request started before it is not shown.
- A run past its lifetime cap whose files are kept no longer offers Extend and revive: the banner shows the retention date and says the run cannot be revived, and a request to move its end is refused 409 `run_end_wait_max_lifetime`, naming that date.
- An archived approval that was cancelled because its run was lost now says so, instead of a fixed sentence.
- A run's header shows its end time from `ended_at`, so a late teardown no longer moves it.
- A Mac kept machine is no longer expired and deleted on a stale or cached idle report. A Mac on 0.10.2 rechecks its own deadline before an expiry delete, and one that refuses because its deadline is later makes the machine take that deadline instead of being asked every minute.
- A resumed kept machine whose run ended without its stop being reported, because of an older runner or a lost report, no longer stays in use. After the run has been over for ten minutes, Wardyn can return a resumed machine to kept when its Mac lists it as idle, even if the stop report was lost.
- An expired kept machine on a Mac that is away stays kept until the Mac returns, and a resume of it is refused as expired (410 `kept_machine_expired`).
- A SCIM group delete now removes every member who joined while it ran, cutting their sessions and revoking the API tokens that carry the group before it answers 204. A member add that reaches a group being deleted answers 404, where it answered 500, and a PATCH whose earlier member operation failed answers that failure first.
- The people list no longer reports the day a SCIM user was provisioned as their first sign-in. Migration `0296_identity_first_login` adds the column that records a person's first sign-in when it happens, and a SCIM user whose first sign-in cannot be told from their provisioning now shows no date rather than a wrong one. It adds no table, so split-role installs need no new grant.
- A SCIM group `PATCH` that changes `externalId` no longer adds members before it answers 400; the removals in the same request still apply.
- A SCIM group filter that selects more than 100 groups answers 400 `tooMany` and names `externalId` as the narrower filter, where it answered 200 with the first 100 as if they were all.
- The Settings SCIM card's token slot counts a group member removal as an update, and a backlog of the sweeper's own rows no longer hides it.
- `wardyn run logs --follow` no longer misses an audit event the server stored late, and keeps its place when audit retention drops the oldest rows: each poll reads the trail newest first and stops at the first event it has printed.
- A service whose sponsor is no longer live gets its `run.ending_soon` warning before the lease's half-life, not only after the refused renewal, when a warning is due before the half-life; a service outside its warning window gets no early warning for that reason alone.
- Cancelling a person's waiting runs no longer turns a run that timed out in the queue into a killed one, and does not count it as cancelled.
- `audit.receipt.create` counts in `redacted` only the events the receipt carries, so an event withheld for the next page of a run's receipt is no longer counted.
- A delayed clean-up no longer deletes a kept folder that a later run used. When a run that asked to keep its work folder ends without starting (cancelled or timed out in the queue, or its runner pool removed), the statement that ends it also begins the delete of its folder, so no other run is offered the folder before the clean-up has deleted it or, when the runner has it in use or the person has a run starting, cleared that mark and kept it. A folder whose delete the person had already begun is left to that delete: if it fails the folder is the person's, and the clean-up of the run does not delete it. A run that asks for the folder as its source run ends is refused it as one still held, and is not left holding a folder it cannot mount.

### Before you upgrade

- A `delegates` row with no issuer and an empty `scope_group` stops migration `0294`. Set a group on each, or revoke the portal and delete the row.
- Issuer portals and model-provider service grants are off until an admin switches them on. Stop every wardynd older than 0.10.2 first: an older server does not know the new tables.
- `wardyn sync --git` now reads the git bundle it is about to send, in a repository of its own. It refuses the branch if a commit in it adds a path (even one later removed or renamed away), or its tip holds one, that matches one of your organisation's `WARDYN_SYNC_DENY` patterns. If your patterns name build output or other paths that were ever committed, such as `node_modules/`, `*.log` or `dist/`, every branch that ever committed one is refused. The error names the first path and pattern, and the working-tree sync is unaffected. Rewrite the branch history or change the patterns. A refused history does not stop `--pull-commits`, and under `--watch` it is a warning. A branch that moves while its bundle is made is bundled once more, and a second move refuses the send. A shallow clone is refused with an error until you run `git fetch --unshallow`, and a SHA-256 repository is checked as one. With no patterns set, Wardyn skips the history scan and its shallow-clone check.
- Update each Mac runner to 0.10.2. A Mac on an older release is sent no expiry delete for a kept machine and removes expired ones in its own sweep only.

## [0.10.1] — 2026-11-21

### Added

- A super admin can ask to help inside someone else's running run, in the console or with `wardyn run support`, and the run's owner decides which doors the helper may use (terminal, commands, files, port forwarding, web apps or desktop) and for 15, 30 or 60 minutes. Help cannot be extended: more time is a new request. The feature is off unless `WARDYN_SUPPORT_SESSIONS` is on. Requests, answers and ends are audited as `support.request`, `support.accept`, `support.decline` and `support.end`. The grant ends even when its `support.end` row is not written: a row lost because wardynd stopped, or because the audit sink and spool refused it, right after the end is not retried. Migration `0293_run_support_grants` adds the grant record.
- A minted Azure DevOps token can be delivered `via_org` to a runner, an organisation's cluster runner included: the organisation keeps the token and runs the gate, and the runner never holds it. An admin turns it on per connection (`runner_via_org`, 'Allow on runners via the server') and in the credential delivery policy, which may require a runner posture for it. The runner and the proxy image must be 0.10.1 (any release candidate) or later. A request through the organisation's proxy waits at most 9 minutes in all for approvals and sign-in. The switch is off by default, and choosing your own pasted token for the connection clears it in the console; the API refuses `own_pat` with the switch on.
- A super admin who holds an open help grant sees, on that run's page, the Desktop and Attach entry points the owner's doors open: Open desktop and the Files panel, SSH, Sync and the run's web apps. A lane the owner did not allow says so in one line, and a tile with no open lane is not drawn. A click after help has ended says that help is no longer open, and the terminal ends with "Help on this run has ended." once the page has read the help as ended; a connection that closes before that reads "This terminal connection ended. Reconnect to check access." Such a request is refused with `support_grant_ended`; a super admin who never helped on the run gets `run_owner_only`.
- `WARDYN_SSH_CHANNELS` (flag `-ssh-channels`) lists which kinds of SSH channel the gateway serves: `shell`, `exec`, `sftp` and `forward`. Unset or empty serves all four, as before. A kind left out is refused with one sentence and an `ssh.channel.reject` row (reason `channel_off`). `sftp` also covers `wardyn sync`, which runs the same sftp-server, and a run with a local folder to sync is refused at create (`local_folder_unsupported`) while `sftp` is off. `exec` also covers `POST /runs/{id}/exec`, the same command over HTTP, which answers `409 run_exec_off`. A client can still start a shell, run sftp-server and relay ports through a command while `exec` is on, so leaving out `shell`, `sftp` or `forward` alone does not stop them; only leaving out `exec` narrows those. `/healthz` lists the served kinds as `ssh.channels`. `wardyn run ssh`, `exec`, `forward` and `editor`, and `wardyn sync`, stop before they dial, with one line, when they need a kind that is off. Boot refuses an unknown name.
- An audit row for a call through an AI gateway or MCP gateway route carries `gateway_request_id` in its data: the request id the gateway or its provider put on its answer, so their own log can be joined to the row. It is read from the answer's `request-id`, `x-request-id` or `x-amzn-requestid` header only, and left out when it is longer than 128 characters, holds anything but printable ASCII or has no letter or digit, or echoes a value the sandbox sent in a header. It is absent on opaque tunnels. On a gateway route, and on every MCP call of a run that has one, the proxy no longer forwards the sandbox's own `request-id`, `x-request-id` and `x-amzn-requestid` headers, so the gateway makes its own. Needs the new proxy image. Upgrade the control plane before runners: a control plane rolled back below this release refuses a runner's spooled row that carries the field, and that row is lost.
- The Changes queue shows when a proposed change widens access, and why a narrower change still needs approval. For supported change kinds, the person deciding can see the change's impact, its risk and the underlying diff.
- A `custom_endpoint` model provider with `identity_binding: none` can set `oidc.require_id_token: false` to sign in against an identity service that never grants `openid`. No identity token is requested or checked, no issuer or subject is recorded, and the capture audit row says `id_token: not_required`. The authority must still publish its discovery document with a usable `jwks_uri`. Changing the setting deletes the provider's stored sign-ins. It is set through the site-config API or a site-config file sent with `wardyn site-config set`; the console shows no control for it.

### Changed

- The sandbox sweep writes its `sandbox.sweep` row for a computer folder record it cannot read once while the record stays unreadable, not on every pass; each server process writes its own, and a new row follows once the record has read or the sandbox has been stopped.
- When the organisation refuses a runner's count of the decisions its full spool dropped, the `runner.event.refuse` row carries `count` and `drop_end` if it moved the run's overflow mark.
- When a CLI sign-in can no longer renew, `wardyn` says whether it was signed out, reached its lifetime or went unused past its idle lifetime. A revoke, a cut, a deactivation or a reuse still reads only that the sign-in ended.
- A paused run's requests through the organisation (`via_org`) are refused with 409 `run_paused` until the run is resumed, for Azure DevOps, GitHub and `api_key` credentials; GitHub and `api_key` requests were served on a paused run before.
- Withdrawing an Azure DevOps token's `via_org` delivery revokes the run's tokens; a write that withdraws it also restarts this server replica's organisation proxy for the run, and a later Azure DevOps request is refused.
- When a run is idle-paused, its open requests through the organisation (`via_org`) are ended when the server's check, made about every 5 seconds, sees the pause, or after two reads of the run in a row fail, so it can take longer than 5 seconds; then the organisation's proxy for the run is released; resuming the run builds it again on its next request. A run paused while it waits for an approval keeps its open requests, so the request the approval is for can still go through.
- The "Authorized, not issued" demo now uses an approval-gated git token. git waits while a person approves, the proxy attaches the token once on the way out, and a second ask after the minute is refused. The approval appears under the demo terminal with the same wording as the Approvals page.
- While a run's proxy cannot reach the organisation, it stops adding a Git credential through Wardyn's Git broker (a GitHub App token, a `git_pat` through the PAT broker, or the GitHub CLI's token) once it has gone 30 minutes without renewing its run token. While the proxy is still renewing, a refused Git request has it try the organisation again within about 30 seconds, and Git requests are served again once that renewal lands, as header credentials already recover. A proxy that has stopped renewing, because its renewal was refused for good or failed for its token's whole lifetime, is not woken by a Git request. Header credentials, and Azure DevOps Git on a runner, already stopped within 30 minutes. A run's proxy outside a Kubernetes runner now renews its run token at least every 15 minutes instead of every 30, so it writes about four `identity.renew` audit rows an hour instead of two. Needs the new proxy image.
- Kubernetes runners at 0.10.1 or later now apply a live egress edit (`PATCH /runs/{id}/egress`, which only removes or denies hosts) in place, as wardynd's own cluster does: the edit's hosts join the proxy's deny list and are refused at once. Such an edit used to answer `409 egress_edit_unsupported`. An edit that would take away a credential lane, a `via_org` route, an MCP server or an intercepted host, or grow the run's deny list past 1,024 hosts, still answers `409 egress_edit_unsupported`, since it needs a new proxy or exceeds the list. If the edit may have reached the proxy but is not confirmed, the run is stopped when the runner takes the stop; while the runner is away the edit answers `503 egress_edit_containment_unresolved`, and the proxy keeps its last list until the stop lands or it lapses, an hour after its last renew. When the runner is offline, has not reported what it supports since wardynd restarted, or cannot reach the run's proxy pod, nothing changes (`502 revive_proxy_kept_current`, naming the runner). On a runner the list is held in the proxy's memory, so the proxy's exit ends the run. A run whose proxy pod an older runner started is still refused.
- An inbound app request refused for its rate (`429 inbound_rate_limited`) now names the bucket that refused it, `run` or `caller`, in the `limit` field of its `inbound.deny` row. Workload events from one runner now draw on an allowance on each replica that refills at 10 events a second and holds up to 6000 events; full reports and single events share it. An event past it is refused (`runner.event.refuse`, code `rate_limited`). A busy runner can lose the end of a full report, and a later full report sends the current resources again. A missed deletion can stay outstanding when the namespace's final list is too large or is dropped: split such a runner's namespaces across runners.
- A run whose policy record is gone (pruned, or never written) and which was restarted or had an egress edit, so its current policy generation is 2 or later, now answers as a run whose current generation's record cannot be read. Output, web apps and bringing the computer folder back answer 503 `data_out_unreadable` (a security admin's output read and the desktop app are exempt, as before); commands and SSH transfers are refused as blocked. Before, it read as `allowed`. Its rule can no longer be read, so the doors refuse rather than allow, and its computer folder is kept until a restart or egress edit, which sets its rule to `blocked`; a run whose current record was never written refuses restarts and edits (503) until it is killed or ended. A run never restarted or edited (generation 1) is not affected by this: it keeps the rule the upgrade gives it (see Fixed). A store without the `data_out` column is unchanged.
- Kubernetes: a person's kill whose pods are all still stopping when its wait ends, with no other kill step failed, now answers 202 with `teardown: confirming`, not 500. Wardyn confirms the stop in the background and writes a `run.kill.confirm` row when that confirmation finishes on the same server process; a kill made while one runs on the same server adds none. If wardynd stops before the confirmation finishes, it writes no `run.kill.confirm` row. At startup Wardyn retries eligible stops that have a saved sandbox reference; a `sandbox.orphan.sweep` failure row means the cleanup was still unconfirmed at that attempt. A kill whose two waits outlast its 30-second budget, a run whose stop no later read can prove (a vanished pod, a reused or changed node, a different execution, a pod the pod garbage collector marked, a pod with no node to check, no read of the recorded pod UID, or a container no check can match), a run whose pods are both past their delete bounds, and an older runner still answer 500.
- The SSH guide is split. The cluster, 443-only and internals topics are now under docs/operations/, and scripted access for other tools has its own page; the old headings in docs/SSH.md point to them.
- docs/TEST-GAPS.md is no longer tracked: `make test-gaps` writes it to test/reports/go/TEST-GAPS.md (gitignored), and `scripts/release-commit.sh --test-gaps` is removed.
- A save that adds or changes an `api_key` grant with `requires_approval` is refused for a policy, a ceiling, a boundary, a preset, a template and a governance overlay, as such grants already are for `env_secret` and `file_secret`: no run could use such a key. Anything saved earlier still loads, runs and can be edited while that grant is unchanged, and its key is still never injected. The default policy file loads with a warning in the log.
- Review's Network row says when IP addresses are refused under allow-all egress and lists the run's host rules in words, one line for each host with a rule (the first eight, then a count), and the run's Policy tab has a Host rules row, so the rules no longer show as raw JSON under Other settings.

### Removed

- The `artifact_overrides` site-config field, the `ArtifactOverride` type in the Go SDK, and the `site_config_artifact_override_invalid` reason. Migration `0030` still rewrites an old stored row.
- The `--run` flag of `wardyn audit`; pass the run id as the argument.
- The `task_key` key in the body of `POST /workspaces/{id}/record`; send `name`. The response's `task_key` (the session key) is unchanged.

### Fixed

- A paused run that was stopped or killed while it waited on a request is no longer thawed when that request later closes; before, a sandbox the stop could not remove could run again under a run already marked ended. A stop or kill that lands while such a resume is already under way is not covered yet.
- A run's `data_out` rule is now kept on the run itself, so audit retention removing the run's policy records no longer reopens output and replay reads, web apps, SSH (file transfer, commands, port forwards and `wardyn sync` down) or computer-folder bring-back. Migration `0292_agent_runs_data_out` fills it on existing runs from the policy records retention has kept. A run restarted or edited since it started (policy generation 2 or later) whose current generation's record is gone is left without a rule and its rule reads as unreadable (see Changed). Any other existing run left with no policy record reads as `blocked` when it was created before the newest audit record retention removed, or within a day after it, and is not still queued, pending or starting with no sandbox: only a security admin reads its output and a super admin replays it. Any other run with no policy record, one that has not started among them, reads as `allowed` as before.
- The organisation now writes at most 32 `runner.event.refuse` rows an hour for a runner whose events it refuses for good, then one row marked `suppressed`, counted across all replicas and restarts, and the summary of the rest is written at the runner's next refusal however long it stays quiet; once a replica has seen the window full, it stops sending that runner's `rate_limited` refusals until the window ends, so the summary leaves those out. Before, each replica kept its own count, a restart reset it, and a summary owed for more than a day was dropped.
- With more than one server replica, the replica connected to a runner now checks whether its sandbox is still there. Replicas that could not reach the runner used to take turns at it, so a run whose container was gone took longer to end as failed the more replicas there were.
- When the organisation refuses a runner's count of the decisions its full spool dropped, the run's cleanup on the runner now still completes. The runner used to keep that run's spool, output and exit for good.
- A held egress or push request that rejoins its pending approval now has both its request and its rejoin timed by the database's clock, so a server whose clock differs from the database's or from another replica's no longer loses the rejoin or starts the hold at the wrong moment.
- An approval request for one MCP call that carries a field the call does not have, such as a host, is now refused. Before, the field was dropped and the request was stored without it.
- Approving a network request whose run cannot be read now answers `503 run_unreadable` at every check, and the approval stays pending so it can be retried. Two of the three checks answered `500` before.
- The egress proxy now removes the headers an upstream's `Connection` header names even when that header also says `close`. Before, a response carrying `Connection: close, X-Foo` reached the sandbox with `X-Foo` still on it. Answers from the Kubernetes API door and from the control plane, and a response whose header block is over 64 KiB, still arrive as before. Needs the new proxy image.
- A sign-in the database stamps after a run asked for one now answers that request, whichever server took the sign-in and however its clock differs from the database's, for Azure DevOps, Azure, the Azure CLI and a custom endpoint's own sign-in. If the database could not stamp the sign-in, or an older release stored it, the request waits for a fresh sign-in. Before, a server behind its database could leave the request waiting after the person had signed in, and a server ahead of it could let a sign-in made just before the request answer it. Pending requests now also expire by the database's clock. How long they are held and how long they waited, and whether they fall in or out of a rule's dry-run history, also use the database's clock while it can be read; when that read fails, the server's own clock is used.
- The run page no longer says a killed run's sandbox was stopped and removed while Wardyn is still confirming the stop, or when the kill does not record a finished stop. It says the stop is being confirmed and hides Kill again for up to two minutes from when the page first sees each kill attempt; reopening the page starts that wait again. If no confirmation lands in that time, it says teardown is not confirmed and offers Kill again. A confirmation that names a failed step says so.
- Bytes moving on a request sent through the organisation (`via_org`) as a plain request, such as a git clone or push through `/wardyn/gh/` or `/wardyn/git/`, now count as the run in use, as a tunnel's bytes already did. Once a minute, if bytes moved, the proxy reports that the run was active; when the server records that activity, the transfer counts against idle pause, so a run whose only work is such a transfer is no longer idle-paused under it. A failed report is not retried. A runner's proxy keeps the old behaviour until the runner is upgraded.
- A run on a runner (your own, or your organisation's self-hosted one) whose key or `custom_endpoint` model provider sends your organisation's run identity is now refused naming that provider, as an MCP server or Bedrock provider already is, in place of `ProxyConfig.LLMUpstreams: credential variant has no permitted local rule`. This holds at Review, preflight, create and dispatch, and a run with an identity MCP server beside a provider with a base URL now names the server too. Its `run.dispatch` audit row's `field` reads `ProxyConfig.GatewayIdentity`.
- `make notices` (the third-party notices check) no longer fails when go-licenses cannot reach the licence host of a module whose recorded licence link has its version as a path segment (`…/v0.9.1/…`): it keeps that link and warns. A new module, a licence change or a version change still fails the check.
- Kubernetes runners keep each run's lease deadline across a runner restart. After a restart, for a run whose organisation cannot be reached, the runner schedules the cleanup of its proxy at the saved deadline, no sooner than 10 minutes and no later than one hour after the restart. Before, every such run got a fresh hour. A run saved before the upgrade has no saved deadline and still gets the hour. The proxy enforces its own lease, so it can stop sooner, and the runner retries the cleanup until it can confirm the stop.
- A plain proxy request (absolute-form, not CONNECT) to a host with a character outside ASCII now gets the same refusal as a tunnel to it: a 403 with the non-ASCII host detail and an audit row whose `rule_source` is `builtin:non-ascii-host`. The 0.10.0 release answered such a request with a 400 and wrote no row, though its notes describe the 403 and the row; the request was refused and not sent either way. Needs the new proxy image.
- The 0.10.0 upgrade note on connect health read too strongly. `/healthz` lists `kernel.network.connect` under `missing_kinds` while the overall sensor state is `partial`; `idle` and `degraded` take precedence and then it lists nothing. It judges from event counts alone, so if the supported hook has recorded connects, the counts cannot show that another hook's connects were dropped. The sensor-health section of the monitoring guide now says so; the check itself is unchanged.
- The runner offline guide no longer says every `refused` action result means the runner changed nothing. That holds for a reference the runner does not own or a container it cannot prove it created. A teardown the runner could not confirm is also reported `refused`, but it may already have been attempted: the runner keeps a `teardown_hold` for the run and retries it. Behaviour is unchanged.
- The warning that Wardyn could not confirm a person's Azure DevOps tokens were revoked no longer begins "The credentials are erased". The same text is on an erase that failed before their stored credentials were erased, so it could say they were gone when they remained. It now begins "Wardyn could not confirm that every token it created for this person's runs was revoked", still with `tokens_revocation_failed` and the remedy, and says nothing of whether the credentials were erased.
- The 0.10.0 note that an erase whose Azure DevOps token revoke does not finish "erases nothing" holds for a person's stored credentials only. A `POST /people/{principal}/erasure` with several scopes runs `credentials` last, so scopes before it, such as run outputs, tasks and recordings, may already be erased when it answers `503` `credential_erase_revoke_unfinished`, and stay erased. Running it again is another attempt, which revokes the token and erases the credentials once the revoke finishes. The Azure DevOps guide now says so.
- A retry sent right after a first-use approval is decided now starts a fresh status check after a floor of at most a quarter of a second, instead of being refused again with `approval:pending` for up to two seconds. The check shares the request's 5 s budget. A request that joins a check already in progress can still see the earlier pending state, and the next retry starts a new check. Needs the new proxy image.
- When two identical host calls wait on one approval that a person decides `once`, a request whose ask was answered only after the other request had used that decision now gets the same answer: it is refused as declined, or it asks again when the other request used the `once` approval. Before, it could be refused with "Wardyn could not ask a person about it", and its audit row named no approval. Needs the new proxy image.
- A refused `local_models` entry is now identified by its host and port, or as an unreadable URL, and the error and the warning omit credentials, paths and queries. Before, the refusal repeated the entry's whole URL, so a user name, password, path or query written in it reached the log.
- A newly created container or pod sandbox maps a custom DNS host name in `WARDYN_SANDBOX_PROXY_URL` to its run's proxy. Before, that name could fail to resolve. An IP address is left as it is, and existing sandboxes keep their host mappings.
- `wardyn sync` asked for another pass after Ctrl-C now stops, where an empty folder used to report a finished pass.
- New Run's add-token dialog asks again before replacing a secret when the save would land in a different namespace (your own or the shared one) than the one you confirmed.
- When the organisation and its HTTPS proxy use distinct DNS names, `wardyn doctor` identifies which one presented an untrusted certificate and gives trust guidance for that host.
- New Kubernetes warm-pool placeholders exit and restart if `sleep` fails, where they used to spin and count as Ready. Existing placeholders keep their old command until they are replaced.

### Corrections to the 0.10.0 notes

- With `WARDYN_LOCAL_TRUST_FORWARDER` on, non-loopback peers must match `WARDYN_LOCAL_TRUSTED_PEERS`. When it is empty, Wardyn uses the gateway and its own addresses found at startup. Traffic from other addresses gets 403; list the intended peer explicitly if it is not found. The 0.10.0 notes said only the listed peers are trusted.
- Commands try to start in `$HOME`; SFTP uses the sandbox account's home directory. If that directory cannot be entered, they keep the image's working directory. Use an absolute path to choose a location. The 0.10.0 notes said they start in your home directory.
- The old Docker run network is permitted only on an older engine, with `WARDYN_ALLOW_HOST_REACHABLE_RUN_NETWORK=1`. The setting has no effect on Docker Engine 28 or later, which the 0.10.0 notes did not say.
- A decline decided `once` refuses the requests already waiting on that approval; a later request asks again. The 0.10.0 notes said it refuses only the waiting request.

- Correction to an earlier release note on SSH gateway audit rows. An sftp transfer or port forward whose process ends with a known nonzero exit is a failure, unless the client or its channel closing ended the transfer first; that close is a success. A transfer whose exit was never read while its client was still connected is a failure that carries no exit status.
- Correction to an earlier release note on secret store clean-ups. A clean-up of a replaced value, or of a value whose row was not written, that is cut short by a shutdown may leave a value behind, or may already have removed it; it is not certain to leave the value in place. `wardynd -reconcile` lists remaining values that no row uses, and the shutdown warning now says so. A value kept because its save did commit is not listed.
- Correction to an earlier release note on a `once` egress approval that a failed HTTPS connection did not spend. A retry that arrives before the approval is restored may ask again. The approval is still given back at most once and only for the window that note names.
- Correction to an earlier release note on a Linux VM guest's plain HTTP requests through its relay. Only a request target that neither starts with `/` nor is a full URL with a host is answered `400` and not forwarded, such as `OPTIONS *`. A full URL with a host is forwarded, and `CONNECT` is answered `405`.

### Security

- `agent-base` and `agent-codex-cli` no longer include `python3-pkg-resources` and the `python3-jaraco.context` chain that came with Debian's `asciinema`, so a full-severity scan no longer reports CVE-2026-23949 on them. `asciinema` is now Debian's package with that unused dependency removed and a `+wardyn1` version suffix, and the GPL source offer says so.
- SCIM requests that name a user by id now answer not found for a user from a different identity provider issuer or tenant, and group requests ignore such a user named as a member. The tenant is checked only when SCIM is configured with one and the user's row records one; otherwise the issuer alone decides. Before, a SCIM token that knew such a user's id could read, change, suspend or remove the user, and a group removal naming them could cut their browser sessions and revoke affected API tokens. The sweeper no longer resumes a pending suspension or purges a user from another identity provider or tenant. Someone already suspended stays suspended. If removal had already started, some cleanup may still be unfinished.
- A session recording upload now masks a secret that was handed to its run while the upload was waiting or being read. Before, such a secret could be stored unmasked in the recording.
- The before-and-after comparison in a governance tree dry run now holds the governance lock through its rollback and uses the same boundary and depth limit for both sides. In 0.10.0, a concurrent edit could make a unit admin's preview include membership names, policy differences and live run ids from outside their unit.

### Before you upgrade

- Three old inputs are refused. A site-config body that still has `artifact_overrides` answers 400 `site_config_request_invalid`, and `wardyn site-config set` refuses such a file before sending it; rewrite it as `egress_redirects`, or save the output of `wardyn site-config get`. `wardyn audit --run <id>` is refused; write `wardyn audit <id>`. `POST /workspaces/{id}/record` answers 400 `invalid_request_body` to a body with `task_key`, which `wardyn record task` and the Go SDK's `RecordWorkspaceTask` of 0.10.0 still send: upgrade them together with the server, or send `{"name": ...}`.
- Existing runs left with no policy record that were created before the newest audit record retention removed, or within a day after it, now read their `data_out` as `blocked`, unless they are still queued, pending or starting with no sandbox, or were restarted or edited since they started (policy generation 2 or later), which answer 503 `data_out_unreadable` instead: only a security admin can read their output and only a super admin replay them, and their SSH file transfer, commands, port forwards, `wardyn sync` down, web apps and computer-folder bring-back are refused. Runs that failed or were killed before they started are among them; they hold no output. A run still running is among them only when its policy record never reached the audit log, and it then does not copy its computer folder back when it ends. On an install whose retention has removed nothing, no run is set to `blocked`.
- Stop every server of an older release before the first server of this release starts. An older server left running after migration `0292` keeps no `data_out` on the runs it starts or revives, so retention can later reopen their content, and a rule it tightens on a revive is not read. The Helm chart already stops the old servers first; this is for servers you run by hand.
- Migration `0292` reads the policy record of every run the audit log still holds and fills each run's `data_out` from it, so an install with many runs in its retained audit history can need more than the default `WARDYN_MIGRATE_TIMEOUT` of 5 minutes. A timeout rolls it back and the next boot starts it again; raise the timeout before you upgrade (see "Before you upgrade: a large recording store" in docs/OPERATIONS.md for how).
- Split-role installs (`WARDYN_PG_MIGRATE_DSN` set): this release's migrations grant `SELECT, INSERT, UPDATE, DELETE` on
  `run_support_grants` to every role that was granted `DELETE` on `run_attach_leases`, and `SELECT, INSERT, UPDATE` on
  `runner_event_refusal_windows` to every role that was granted `UPDATE` on `runner_spool_cursors`, in each case other than
  that table's owner. If the app role is not one of those roles or a member of one, and does not pick up new tables
  through the `ALTER DEFAULT PRIVILEGES` step in Operations, grant it the same on each table as soon as the migration has
  run. Until then no support request or answer is recorded, though approving and denying approval holds works as before,
  and no event the organisation refuses for good is recorded or acknowledged, so a runner keeps resending it.
  An upgrade from before 0.10.0-rc.22 also needs that release's grants. On an install whose app role does not pick up
  new tables through the `ALTER DEFAULT PRIVILEGES` step in Operations, grant the app role `SELECT, INSERT, UPDATE` on
  `mcp_call_sends`. Without it, no `approve_each` MCP call that carries an operation key is asked about or sent, and
  erasing any person's run tasks (`run_tasks`) fails. A call without a key is unaffected only in a run with no
  `correlation_id`; in a run that has one, every `approve_each` call needs the grant. Grant it `SELECT, INSERT,
  UPDATE, DELETE` on `principal_groups` too, unless it holds `DELETE` on `key_domain_login_groups`, itself or through
  a role it is a member of: the migration grants the table to those roles. Without it every SSO sign-in and portal
  token exchange is refused, and so are a SCIM removal from a group and erasing a person's credentials or purging
  them, and a running run that a person owns and that carries a component gets no credential, no gateway token for an
  organisation MCP server and no gateway client certificate (`503`). The admin token still signs in. An upgrade from
  before 0.10.0-rc.21 also needs the 0.10.0-rc.21 grants: `SELECT, INSERT, UPDATE` on `principal_identity_unbinds` and
  `run_teardown_holds`. An upgrade from before 0.10.0-rc.18 also needs the 0.10.0-rc.18 grant: `SELECT, INSERT,
  UPDATE` on `run_proxy_starts`. An upgrade from before 0.10.0-rc.17 also needs the 0.10.0-rc.16 grants: `SELECT,
  INSERT, UPDATE, DELETE` on `governance_unit_deciders` and `governance_envelopes`. An upgrade from before
  0.10.0-rc.14 also needs every grant the 0.10.0-rc.14 notes list: `SELECT, INSERT, UPDATE, DELETE` on
  `run_create_keys`, `wall_presets`, `kept_machines`, `workloads`, `workload_bindings`, `workload_run_ends`,
  `macos_vm_disks`, `aws_sso_refused_tokens`, `cli_sign_ins`, `ui_desktop_closed`, `desktop_signins`,
  `desktop_signin_shares`, `desktop_signin_runs`, `approval_rules`, `governance_restrictions`,
  `governance_restriction_members`, `governance_unit_admins`, `oidc_session_sids` and `desktop_vm_sign_ins`, `SELECT,
  INSERT, UPDATE` on `groundtruth_sensors`, `runner_health`, `launch_preset_version_floor`, `cli_sessions` and
  `governance_boundary` (and on `run_harnesses` if it turns on `WARDYN_MULTI_AGENT_TASKS`), and `SELECT, INSERT` on
  `audit_checkpoints` and `run_model_usage`.

## [0.10.0] — 2026-11-19

Developer sandboxes. A run can be a Linux VM with its own desktop, or a macOS VM, which is a preview, off by default
and not retested on a real Mac for this release. Runs can use folders from your computer, local model servers and
services, and desktop controls decide what crosses in and out. Organisations set walls through governance units, with
deciders for held requests, per-host method and path rules, and a No network preset. Images can be held to their
signatures, and the audit log is signed with receipts you can check offline.

This is the first final release of the 0.10 line, after 26 release candidates. Upgrading from 0.9.x, read the
candidates' sections below, newest first: they list every change in detail, and each "Before you upgrade" step
applies to you. 0.10 cannot be rolled back by redeploying 0.9.x, so take a database backup first. Docker runs need
Docker Engine 28 or later. Upgrade the runners, the proxy image and the Linux VM guest image too, in the order the
candidates' notes give. The 0.10.0-rc.22 notes' first "Split-role installs" bullet names the grant for every table
the 0.10 migrations add. Some settings now stop wardynd starting, each named in its
candidate's "Before you upgrade" notes, among them a `WARDYN_ADMIN_TOKEN` shorter than 32 bytes off loopback (rc.14),
`WARDYN_AWS_SSO_PROXY_INJECT=off` (rc.1) and a 0.9 setting name that was renamed (rc.1).

### Defaults changed by this upgrade

Setup status does not list these nine yet, so read them here. Each entry names the candidate that first shipped it;
its notes have the detail.

- **Copying out of a desktop is blocked by default** (rc.1). Paste into the desktop still works; copy out of the
  `novnc` desktop app is dropped and audited as `ui.clipboard.transfer`. It reaches every deployment with desktop
  runs. For the old behaviour, set `desktop.copy_out` to `allowed` on the policy, and on the ceiling of any member who
  should have it.
- **A member policy with no `desktop` setting gets the ceiling's desktop controls** (rc.12). It reaches member
  policies that leave `desktop` out. To keep the narrower defaults, set `desktop` on the policy.
- **Four hardening switches are on** (rc.1): `WARDYN_EGRESS_HOST_PORT_GRANTS_INERT`,
  `WARDYN_GOVERNANCE_UNIT_PUT_EXISTING_ONLY`, `WARDYN_CAPABILITY_GRANTS_VERIFIED_EMAIL` and
  `WARDYN_DOCKER_PROXY_SANDBOX_PEERS_ONLY`. On Entra ID, an allow grant matched on an email usually stops granting,
  because the token carries no `email_verified`. Boot logs each switch's state. For the old behaviour, set a switch to
  `false`.
- **`WARDYN_LOCAL_TRUST_FORWARDER` trusts only the peers in `WARDYN_LOCAL_TRUSTED_PEERS`** (rc.1), not any
  non-loopback peer. It reaches local-mode installs whose published-port traffic reaches `wardynd` from another
  address, which now get a 403. Set `WARDYN_LOCAL_TRUSTED_PEERS` to that address.
- **The compose services drop `NET_RAW`, and the desktop tier no longer publishes ports 5432, 5010 and 8081 on the
  host** (rc.1). For the old behaviour, add `cap_add: [NET_RAW]` to a custom image that needs raw sockets, and publish
  the ports in your own override.
- **Commands and sftp sessions over the SSH gateway start in your home directory** (rc.1), not the image's working
  directory, so a relative path resolves from `$HOME`. It reaches scripts that use `ssh`, `scp` or `sftp` into a run.
  Use an absolute path, such as `/home/agent/work/...`, to keep the old place.
- **A Docker run's network has no address on the Docker host** (rc.14), so Docker runs need Docker Engine 28 or later
  and an updated runner. On an older engine a run is refused. For the old network, set
  `WARDYN_ALLOW_HOST_REACHABLE_RUN_NETWORK=1`.
- **Model token counting is on** (rc.14). wardynd stores each run's model token counts, and the proxies it starts
  count them. For the old behaviour, set `WARDYN_MODEL_USAGE=off` on wardynd and on each runner on another machine.
- **wardynd signs the audit log** (rc.14). About once a minute while anything is logged, it writes an
  `audit.checkpoint.create` row and sends it to your audit sinks, so a test or alert that counts exact audit rows in
  an idle window sees them. For the old behaviour, set `WARDYN_AUDIT_CHECKPOINT_INTERVAL=0`.

### Added

- A host rule hold can be approved for just the held request: send `decision_scope` `"once"`, or choose **Just this request** in the console. The next request with that host, method and path is held and asked about again, and a decline decided `once` refuses only the waiting request. Under an `egress_domain` deciders rule whose `max_scope` is below `run`, such as `once`, the listed deciders can now decide a hold, which is then decided `once`. Before, no one could. Only a hold raised by a proxy of this release offers `once`: its `requested_scope` carries `accepts_once`. A hold from an earlier proxy is decided for the run as before, and the deciders save warning now says so.

## [0.10.0-rc.26] — 2026-11-19

### Security

- A run's requests through the organisation (`via_org`), such as a `git push` through `/wardyn/gh/`, now follow the run's current network rules on every server. Before, the organisation's copy of the run's proxy was rebuilt only after a network edit, and only on the server that handled the edit. A restart that applied a new wall or `deny_all`, or an edit handled by another server, left it on the old rules until the run had sent nothing for 10 minutes. Now a new request on any server is served under the new rules at once. The server that applied the change ends the run's open requests at once, and any other server ends them within about 5 seconds.
- On a Kubernetes runner that restarts while cut off from the server, a run's network access now ends at its scheduled end, or when its run token would lapse if that is sooner, as the runner last heard them from the server, but no sooner than 10 minutes after the restart, so the server's renewed lease can still arrive. Before, the restart gave each such run a fresh hour, so it could keep network access up to about 65 minutes past its scheduled end. The runner must be at this release: a run it already held when it restarts to upgrade gets the old fresh hour that one time.
- Wardyn refuses an MCP call approval request that carries a field the call's scope does not have, such as a stray host (`mcp_call_scope_invalid`). Before, it dropped the field and stored the rest, so the request a person approved could lack part of what was sent for approval.
- While the proxy cannot reach Wardyn, it no longer adds a credential that states its own expiry, such as a cloud sign-in, an Azure DevOps token or a kubectl token, after that expiry. Before, it kept adding the last value it had for up to 15 minutes past the expiry. Requests that need it are refused as credential unavailable until Wardyn answers again. A stored key, which states no expiry, is still added for up to 15 minutes past its 10-minute lease; set `WARDYN_PROXY_STALE_CREDENTIAL_MAX` (for example `0` or `5m`) on wardynd, or on `wardyn-runnerd` for runs on a runner's computer, to shorten that. Both the proxy and wardynd need this version.
- An address written in square brackets with no port, such as `[::1]` or `[::ffff:169.254.169.254]`, could get past the check that refuses literal IP addresses such as loopback and the cloud metadata address, so a review mode could ask a person to approve it, and with a corporate upstream proxy the request was handed to that proxy. It is now refused like the same address written without brackets.
- Azure's WireServer address `168.63.129.16` is now refused like the `169.254.169.254` metadata address, on every port including its host agent plugin port 32526: a sandbox can no longer reach it by address or by a host name that resolves to it, and on Kubernetes the proxy pod's NetworkPolicy excludes it too.
- Resuming a paused run now checks its owner again first. If the owner was deactivated or suspended while the run was paused, or its service identity was suspended, retired or given a new sponsor, or its sponsor was deactivated or suspended, the run is ended instead of resumed (`identity_deactivated` or `service_identity_inactive`). Revoking the owner's or sponsor's sign-in sessions does not end it. If the owner's governance profile was deleted, no longer holds them, or now denies the run's placement or macOS base, or the model provider the run was launched with was withdrawn, or an organisation component the run uses was deleted or pinned to another unit, the run stays paused. A capability withdrawn from the owner keeps the run paused only when the owner resumes it from the console or API themselves; a resume by a decided approval, by a super admin, or by a keystroke in an SSH or in-sandbox UI session does not check capabilities. Resume in the console (`POST /runs/{id}/resume`) says why a run stays paused, while an attach or SSH session that would resume it gets a plain error. Stop it and start a new run under the current rules. If the check cannot be answered, the resume answers `503` and can be tried again. Before, a resume thawed the run without these checks. A paused run's token still renews, because the proxy needs it to wait on a decision.
- Suspending or retiring a service identity now ends its live runs, and so does a SCIM suspension of its sponsor, the way a SCIM suspension ends the person's own runs. Before, those runs kept running until someone killed them. Each ended run has a `run.kill` audit row whose reason names the event: `service_identity_suspended`, `service_identity_retired` or `service_sponsor_inactive`. If a suspension applied through an approval cannot end every run, its `service_identity.update` row carries `runs_end_failed` and the runs still live, the approval's answer sets `runs_end_failed` (`pkg/client`'s `RunsEndFailed`), and the console says so.

### Added

- Saving an organisation component whose host serves a model on this deployment still succeeds, and the save now warns the admin, in the console and as `warnings[]` in the API, that the port does not count and to give the component a host name no model uses; until then, people who attach it are refused. Before, only a person using the component was told. `pkg/client`'s `ComponentSaved` gains `Warnings`.
- An admin can set `WARDYN_START_AFTER_READBACK=on` so a run's agent first starts only after its proxy confirms the policy it loaded. Until then the run shows `STARTING` and takes no attach or exec; a run whose proxy does not confirm in time fails with its sandbox stopped and no agent started. The sandbox's own startup (the CA install, an interactive run's workspace preparation) still runs first. An interactive run with a task is refused while it is on. A revive, restart or live egress edit is not held. Off by default; any other value stops wardynd at boot.
- Saving a unit's deciders rules with an `egress_domain` rule whose `max_scope` is below `run` now warns the admin, in the console and as `warnings[]` in the API, that host rule holds, which are decided for the rest of the run, cannot then be decided by anyone, security included, and wait until they time out; the fix is `max_scope` `run` for `egress_domain`. A save held for approval carries the same warning. The save still succeeds and the decision rule itself is unchanged.

### Fixed

- A command sent to a run (`wardyn run exec` or an SSH exec) in the first seconds after the run reads `RUNNING` now waits, up to 15 seconds, for the run's CA files (`/tmp/wardyn/ca-bundle.pem` and `mitm-ca.pem`) instead of failing its first TLS call. If they never appear, the command does not run and a sentence says why. A run with no CA, or a macOS guest, does not wait. Both files are now written whole and renamed into place, so a program never reads half of one.
- The People screen's copy button now gives `wardyn person invite`; it gave a command that does not exist.
- A governance unit whose base allows all egress can now lock itself to its own list by setting `allow_all_egress: false` with `allowed_domains`, including hosts the base's own `allowed_domains` does not name. Before, each such host was refused (`governance_overlay_invalid`), though the base already reached it. A host on a port other than 80 or 443, or an IP address, still needs the base's own entry, and the base's `denied_domains` still apply. A unit already saved this way now keeps a host its base's list no longer names; before, that host was dropped with a warning. The base's denied hosts still apply.

### Before you upgrade

- A run started before this upgrade, or on a runner still at an earlier release, can no longer bring its computer folder back early (`computer_folder_agent_unstoppable`); the folder still comes back when the run ends. Runs started after the upgrade, on an upgraded runner, are not affected.

## [0.10.0-rc.25] — 2026-11-18

### Security

- An Azure DevOps `minted_pat` run on the cluster now has its proxy fetch the run's token from Wardyn again at least every 10 minutes while the run uses Azure DevOps, as `bearer` and `own_pat` runs do. Before, disabling or deleting the connection, narrowing its ceiling below a capability the run holds, switching its token mode, or a boundary or restriction cap that leaves such a capability out usually reached such a run only when its token was renewed, up to `pat_max_hours` (default 8) later, and the run kept working against Azure DevOps until then. Now such a run's Azure DevOps requests are refused from at most 10 minutes after the change, and its tokens are revoked at the first refused one (`drift`). A deleted connection's tokens cannot be revoked: they are recorded as `ado_pat.revoke.failed` and stop working at their own expiry, and the run is no longer given them. If Wardyn is unreachable, the run's proxy stops adding the token at most 25 minutes after its last fetch. Runs on a runner are unchanged (15 minutes). Contrary to the 0.10.0-rc.17 notes, a capability a run already holds is not checked again at every request, only at the run's credential resolves; the boundary page now says so.

## [0.10.0-rc.24] — 2026-11-18

### Changed

- The `component_host_serves_model` refusal now says that the port does not count, and to give the component a host name no model uses.
- Claude Code's tool search stays off, as it was in 2.1.231, for a run whose agent reaches its model through a gateway base URL, and for a run on a container whose agent reaches its model through an Anthropic API key, a custom endpoint, a local model or a Claude subscription provider with a gateway base URL, and for a run with no model provider: Wardyn sets `ENABLE_TOOL_SEARCH=false` in the sandbox env. On an image that sets no Anthropic base URL, such as one built from agent-base, tool search is now turned off too; 2.1.231 left it on there. A Claude subscription provider with no gateway, Bedrock, Foundry, Vertex, the Claude sign-in task and a virtual machine run with no base URL keep Claude Code's default. To turn it back on, give the run a component config entry that sets `ENABLE_TOOL_SEARCH` to `true` (or `auto`); a value set that way is kept.

### Fixed

- A workspace or invitation write that matched no row, when the read that follows to explain the miss also fails for another reason, is no longer reported as a missing workspace or invitation. The `workspace.requirement.write`, `workspace.egress.approve` and `workspace.egress.deny` audit rows say `Store error`, not `Workspace not found`, and the server log keeps the store's own error. A workspace edit, an egress list saved with `If-Match`, and `DELETE /api/v1/invitations/{id}` answer `500` `internal_error`, which a retry may clear, where they answered `404`.
- The SSH gateway no longer waits up to 5 seconds for a forwarded port's error output after the client closed that port's channel while the connection stayed up; a closed channel cannot carry it.
- A forwarded port whose client closed its channel while the sandbox was still sending now ends at once, on every runner. Its `ssh.forward` row is written then. On Kubernetes and VM runners it no longer holds one of the run's SSH channels or keeps the run from idling until the SSH connection ends.
- `wardyn sync` now reports when each pass starts, so a pass the session ended before it finished is recorded as a failure in its `ssh.sync.transfer` row ("the session ended before its sync pass finished"), not as a success. This needs both this CLI and this server: an older CLI against this server, or this CLI against an older server, records as before. Stopping `wardyn sync --watch` with Ctrl-C in the middle of a pass now records that pass as failed.
- macOS VM: edit-base's Trust the list's CAs asks only for a CA the base does not trust yet, and once for a certificate listed twice. When every CA is already trusted, it opens nothing and says so.
- A CONNECT that went ahead on a `once` approval given back after a failed first try now names that approval in `approval_id` on its own `builtin:dial-failed` or `builtin:tunnel-failed` row, without the not-spent note. Before, that row read null. When an approval is spent or given back has not changed.
- A run on a runner (your own, or your organisation's self-hosted one) that carries an MCP server or Bedrock provider sending your organisation's run identity is now refused at Review, preflight and create (`placement_credential`). The refusal names every such server or provider and says they run only on your organisation's remote-hosted runners. The refusal at dispatch names them too, in place of `ProxyConfig.GatewayIdentity`.
- Bedrock SSO sign-in now shows its link and code when AWS IAM Identity Center sends the newer sign-in page (`signin.aws`) and a separate code, including on newer regions. The console also shows the whole link when the terminal splits it across rows, and the run page's "Open the verification page" button is back for that page. Look-alike hosts are still refused.

### Security

- `agent-claude-code` now pins Claude Code 2.1.312. The earlier pin, 2.1.231, runs on its own embedded runtime, whose
  Brotli decoder a crafted stream can make write out of bounds (CVE-2026-111497); 2.1.312 carries the fixed runtime. A
  crafted Brotli response from a site a Claude Code run may reach could run code as the agent, inside its sandbox and
  its egress policy. Rebuild locally built `agent-claude-code` images.
- A run on Kubernetes keeps its `data_out` rule after a live egress edit, including after a restart with current limits. Before, the edit let content out until the run ended through every door the rule guards: run output and recording replay, web apps, the Files panel, desktop copy out and download, SSH file transfer, commands and port forwards, `wardyn sync` down, and bringing the computer folder back. A later restart kept that unless a restriction set the rule.
- When a run's current policy generation record cannot be read, every door its `data_out` rule guards now refuses. Output, web apps and bringing the computer folder back answer 503 `data_out_unreadable`; commands and SSH transfers are refused as blocked. A restart or egress edit answers 503 until the record can be read. Before, the rule read as allowed and content could leave. A run with a computer folder that syncs back keeps its sandbox at its end, so the folder can be brought back once the record reads, until the keep expires. A run with no recorded policy (pruned) is unchanged.

## [0.10.0-rc.23] — 2026-11-17

### Added

- `deploy/macos-guest/mdm/inspection-ca.mobileconfig.template`, a `com.apple.security.root` profile for one Mac's inspection CA, and a "Managing macOS VMs with MDM" section in the base image page: what an MDM vendor needs, why only MDM delivery gives TLS trust, what the runner package and profile do with no sudo, and the open question of many clones of one enrolled guest.
- macOS VMs: an opt-in inspection CA per Mac (`macos_vm_inspection_ca`). Trusted once in `macos-vm edit-base`, it lets apps that use only the system trust store reach Inspected hosts. Each such run records `inspection_ca` on its machine. The runner refuses a runnerd.json value other than 1, 0, true, false, on, off, yes or no. A managed value outside that list leaves the CA unused and keeps its key, unless a drive root is or holds the VM directory.

### Changed

- A `once` egress approval is no longer spent by an HTTPS connection that got nothing back from the far side. That is a failed dial, a configured upstream proxy's refusal of the CONNECT, or an upstream that, after the sandbox's TLS hello, closed or reset without a reply or answered in plaintext. The next request to that host within 60 seconds uses it, once per approval, and the failure's audit row carries the `approval_id` and says so. A TLS alert, a sandbox that closed first, and plain HTTP requests still spend it. Needs the new proxy image.
- VM runs (`linux_vm`, `macos_vm`) accept `host_rules`. The guest's requests to a rule host are judged as a container's are, and the `host_rules_need_inspected_vm` refusal is gone.
- `POST /api/v1/sources/{id}/scan` answers a folder scan whose result could not be saved as a server fault, `500` `internal_error`, which a retry may clear, and `404` `source_not_found` when the source was deleted while the scan ran. It answered `422` `source_scan_failed`, which reads as the folder failing its scan.
- `POST /api/v1/workspaces/{id}/scan` answers the same fault as a `500`, where it answered `422`. It skips a folder deleted while it was scanned, as it skips an attachment that no longer exists, instead of ending the scan.
- A `source.scan` row for an upload that is not scan facts, a `workspace.requirement.write` row for a run that could not be read, and a `run.record.start` row for a launch the server failed now name the step that failed, never the error's text; the server log keeps the cause.
- A `source.scan` or `workspace.scan` failure row for a scan the server could not launch or save, or whose folder's runner it could not read, also names the step, never the error's text.
- The failure rows of a run's launch, dispatch and end (`run.create`, `run.dispatch`, `run.build` for a launch's own image build, `run.exec`, `run.fail`, `run.complete`, `run.selftest`, `run.containment.reassert`, `run.artifact.redirect`), of an attach, a file or resource read, an exec, forward or sftp start through the SSH gateway and a desktop pause (`session.attach`, `run.files.fail`, `run.resources.fail`, `ssh.exec`, `ssh.forward`, `ssh.sftp.transfer`, `desktop.sign_in.pause`, `run.command.exec`) and of a credential renewal or sweep (`harness.credential.refresh`, `credential.expired.delete`, `ado_pat.revoke.failed`, and the kubectl `secret.read` failure), as do the `run.revive` and `run.resume` failure rows, the `session.recording.write`, `recording.upload`, `session.revoke` and `ssh_key.delete` failure rows, and the per-step failure keys of the `run.revoke`, `run.kill`, `run.ended`, `run.lost`, `run.pause`, `run.reconcile`, `run.policy.unconfirmed` and a workspace-image `run.build` row (`teardown_error`, `runner_error`, `identity_error`, `broker_error`, `proxy_config_error`, `end_error`, `lost_error` and the like) now name the step that failed instead of the error's text; the server log keeps the cause. A refusal the server worded itself, such as a cancelled attach, an agent policy it refused or a run whose lock another operation held, keeps its sentence, as does a runner too old for the BYOI selftest, which the run's failure hint also says. A launch's image-build failure, including a base image that could not be wrapped, still shows its cause beside the run's failed state. The site-configuration probe's verdict for a run that failed at launch reads the run's own failure hint, so it still says why, and a run whose agent never started in its sandbox now carries the driver's reason in that hint.
- In a `run.record.start` row, a record launch refusal the route answers in its own words keeps that sentence; every other failure, a run-cap or host-capacity refusal included, names the step.
- A `workspace.requirement.write` row for a requirements write the store failed says `Requirements at their key cap`, `Workspace not found` or `Store error`, never the store's text.
- A `workspace.egress.approve` or `workspace.egress.deny` row for an `always` decision whose egress list write the store failed, written either when the decision is made or when the boot re-apply replays it, now says `Egress list at its cap or edited since the decision`, `Workspace not found` or `Store error`, never the store's text. The server log keeps the cause of a `Store error`.
- A second identical MCP call under the same operation key (its own, or the one send once makes) that waits at the same time as the first no longer asks a person: once one is approved and sent, the other is told so. Needs the new proxy image.
- While a component a run carries keeps its MCP servers from a gateway token, the sandbox gets `403` `gateway_component_cut` instead of a `502` `credential_unavailable` that SDKs retry. Needs the new proxy image.
- `POST /api/v1/workloads/{name}/preview` refuses a `?owner=` naming anyone but the caller (`400` `workload_invalid`), as the apply does.
- `PUT /api/v1/workloads/{name}` refuses a `?owner=` naming anyone but the caller (`400` `workload_invalid`); it used to be ignored.
- A file download whose asker was erased reads `a person since erased` in the console. Its card offers Approve and Deny only to the admin token: no person can decide it.
- A workload launch refused for a policy pin mismatch now says why in the console: its policy resolves to a launch policy digest other than the one it is pinned to, so nothing launches until the pin or the policy changes.
- A governance profile's limits show a `Confinement floors` chip when the profile sets at least one confinement floor.
- A macOS VM run's inspection CA copy also names the hosts only the organisation's sealed host rules name, so apps that use only the system trust store reach them. The run's Policy tab and `wardyn run policy` mark such a host Inspected, since the proxy opens it. Needs the new runner and server.

### Fixed

- A CONNECT tunnel that ended before any reply could leave a `builtin:tunnel-failed` audit row naming the wrong side. When the sandbox sent its first bytes and closed, and the upstream hung up in answer, the row could say the upstream closed or reset without answering. When the upstream closed or reset first and the sandbox closed in answer, it could say the client closed before any reply. Where a reset was involved, the row's `refusal` was wrong with it. The same row could also say the client closed when the upstream reset while the proxy was still writing the client's bytes. The row now names the side whose stream ended first, and a write that fails names the side it wrote to. Needs the new proxy image.
- On the Docker runner, a sandbox reaches its proxy as soon as a live egress edit or a revive answers. Every proxy of a run now takes the same MAC address on the run's network, so a gVisor sandbox no longer loses egress for up to a minute after its proxy is replaced. Update the runner to get it. A run started on an earlier runner waits once more, at its first replace after the update. When the proxy's create or start fails with the address, the runner logs a warning and tries once more without one. A run whose proxy was made that way keeps the wait at that replace and at its next one.
- An approved MCP call with an operation key whose body the content scan blocks is refused before its send is recorded, so its key no longer reads as possibly sent. Needs the new proxy image.
- An SSO sign-in now always replaces the groups recorded at the person's last sign-in, also after the database's clock stepped back.
- A VM guest's plain HTTP request whose target does not start with `/`, such as `OPTIONS *`, is answered `400` and not forwarded to the proxy.
- A command run through the SSH gateway (an `ssh` command, an sftp session, a forwarded port or a folder sync) no longer loses the end of its error output, which is usually the line that says why it failed: the gateway now waits for all of it, for up to 5 seconds after the command's output ends, before it sends the exit status and closes the channel.
- SSH gateway audit: an sftp transfer or port forward whose process failed is now recorded as a failure that names its exit, also when the client closed its channel or connection after the process's output had ended and before the gateway read the exit; before, that close made the row a success. On the Docker runner a command whose output had already ended when its client closed now reports its own exit, so `ssh.exec` records that exit and a folder sync that ended cleanly is no longer recorded as a failure. Two cases keep the earlier outcome: on the Docker runner, a close that arrives before Docker has recorded the exit; and on a remote runner, a close that reaches the runner before the gateway asks it for the exit. A transfer or forward its client closed while the process still ran stays a success.

### Security

- A member's workspace mount and a folder pass refuse the macOS VM directory, `macos-vm.noindex`, and anything under it, as they refuse a credential folder. It holds every run's disk and the guest agent's and the inspection CA's keys. A pass of the folder above it leaves it out. A container bind, or a share drive's bind, also refuses a folder that is, is under or contains that directory or the runner's admission and socket directories (`/Users/Shared/Wardyn/macos-vm-admission` and `/Users/Shared/Wardyn/sock`), such as the home or `~/Library`, by where it really is: a directory linked to another volume is held there too. The refusal names neither where the folder resolves nor the runner's directory.
- A folder pass of a folder such as `~/Library` or `~/.config` now leaves out `Keychains`, `gh` and `gcloud` under it, as a deeper pass already did.
- The send record of an MCP call refuses an approval whose operation key starts `call:` but is not the key made from that call (`404`).
- The audit feed no longer serves a workload's digest, launch policy digest or pin below a super admin; an offline receipt verifier older than this release refuses a receipt that holds such a withheld workload row, so verify new receipts with this release's verifier.
- The control plane refuses a raised MCP call whose operation key starts `call:` but is not the key made from that call (`400` `mcp_call_scope_invalid`).

### Before you upgrade

- An organisation whose runners or proxy image predate this release: a VM run with `host_rules` now passes create and fails at its proxy's start, because the older proxy refuses the config. Upgrade the runners and the proxy image first.

### Before you downgrade

- Going back to an earlier runner: remove `macos_vm_inspection_ca` from runnerd.json first. An earlier runner refuses a file with a key it does not know.
- Turn `macos_vm_inspection_ca` off and start this runner once first, with Docker running: that deletes the CA's key, and its log then says `this Mac's inspection CA was removed`. An earlier runner leaves the key in the VM directory and does not keep it away from a drive root.

## [0.10.0-rc.22] — 2026-11-16

### Added

- `wardyn policy check --within <policy-id|template-id|file> -f <candidate>` tells a pipeline whether a candidate policy is no broader than another. It exits 1 and lists each field that is wider. It fails a candidate it cannot show to be no wider.
- `wardyn policy check --within` also takes a run template's id. The server works out the policy a run of the template would start from for you and checks the candidate against that, so a template saved with `record save --template` can be the evaluated side.
- A governance profile can raise a run's minimum confinement class by what the run does: `confinement_floors` in its limits sets a minimum class for a run on an image its launcher chose, a run that carries a component its launcher defined, and a run whose `data_out` lets content leave. A requested class below it is refused. Review says which floor raised the class, and the `run.create` audit row records it as `confinement_raised_by`.
- docs/RUNTIME-CONTRACT.md: what each runtime (Docker, Kubernetes, Kubernetes with agent-sandbox, Linux VM, macOS VM) holds for each contract promise, with the test that checks each claim where one exists, or the page that states it, and the agent-sandbox swap proof.
- An `approve_each` MCP call can now carry an operation key, as the `_meta` member `wardyn.dev/operation-key`: 1 to 128 ASCII letters, digits or `. _ : / -`, not starting with `call:`. The run proxy never sends it to the server. Wardyn records each keyed call before sending it, in the run owner's namespace within the run's correlation ID, else within the run, and the record survives a revive or a replaced proxy. For 24 hours after the send, a later call with that key is not asked about or sent again: the same call is answered with the approval it was sent under, when, and whether the server's answer was recorded; another call with that key is refused. After 24 hours a call with that key is new and needs an approval again. A call whose send cannot be recorded is not sent. A call without a key behaves as before. Erasing a person's run tasks overwrites the send records of their runs, so none keeps the correlation ID or the key; a call one of them answered is asked about again. Erasing a person's audit fields removes the operation key from the approvals raised in their runs. The run proxy's own calls to the server are refused with `409` `operation_already_sent` or `operation_key_reused`, the agent's MCP client gets a tool error that says why, and an answered repeat is audited as `brokered:mcp:approval-sent`.
- A run with a `correlation_id` sends each MCP call an `approve_each` rule asks about at most once per occurrence within that id, for every harness. A call without its own operation key gets one made from its server, tool and arguments and which occurrence of it this is in the run, so a retry in a new run with the same id within 24 hours of the earlier send is answered with that send instead of asking a person again. After 24 hours, or within one run until its proxy is revived or restarted, a repeat is asked about as before. A run with a `correlation_id` and an `approve_each` rule fails at start on a proxy or runner older than this release.
- A workload can be pinned to a launch policy digest: `PUT /api/v1/workloads/{name}/policy-pin` sets the pin and `DELETE` clears it. A later launch whose policy resolves to another digest, for example after a stored policy, a workspace or component it uses, or the owner's limits changed, is refused before its run starts. A workload preview's `launch.policy_digest` is the digest to pin. docs/operations/workloads.md says what the digest covers and lists the pin's limits. A refused launch records `workload_policy_pin_mismatch`; a pin that is not 64 lowercase hex characters is refused `workload_policy_pin_invalid`; setting or clearing a pin writes a `workload.policy_pin.set` audit row.
- `wardyn workload pin NAME DIGEST` and `wardyn workload unpin NAME` set and clear a workload's policy pin, and `wardyn apply --dry-run` prints the launch policy digest each launch would resolve to.
- A run a workload launches, through the API, the CLI or a `WardynWorkload` resource, now carries the SPIFFE ID `spiffe://<trust domain>/workload/<owner>/<name>/run/<run id>` for its whole life, with the owner escaped as [The claims a gateway receives](docs/operations/ai-gateway.md#the-claims-a-gateway-receives) says: in its run token, in its gateway token's `act.sub` and in its gateway client certificate. A gateway can admit every run of one workload by matching the ID up to and including `/run/`. The gateway token also carries `wardyn_workload` (`<owner>/<name>`), and the `gateway_identity.mint` rows with `first` set carry `workload`. Any other run keeps `spiffe://<trust domain>/agent-run/<run id>`.
- Three pages. docs/operations/delegation.md sets standing, session and one-time consent side by side: who may give each, and when a running run loses it. docs/operations/orchestrators.md shows one governed run per workflow step. The page shows that a retry keeping the step's `correlation_id` makes no second request to a person and no second send for an `approve_each` MCP call within 24 hours of the send. It also shows how a client names the operation with its own key. docs/GLOSSARY.md tells apart the gateways, proxies and brokers in and around Wardyn.

### Changed

- Erasing a person's audit fields (`audit_personal_fields`) now also puts `erased` in place of their name as who asked for a file download. While such a download waits, no person can decide it (the admin token still can, as the audited break-glass), and the run's end cancels it. The name `erased` is now reserved, so no person can sign in with it or use a token under it.

### Fixed

- Undoing a database change that failed partway no longer ignores the operation's time limit, which let it wait without limit on a connection that stopped answering. Such an undo now ends within 5 seconds, or sooner when the operation's own time limit comes first. A connection that has not answered by then is closed, and the database undoes the change when it sees the connection gone. Before, such a connection could hold the operation, and every lock it held, such as a run's lock, past every time limit it had.
- When two servers retried the stop of a run that was ended past its end while another operation held it, one of them could record the run's changes as given up at the end of its keep after the other had handed the run back to its normal end, whose copy back may have brought them home. The give-up is now made only while the run is still waiting on that stop. Affects 0.10.0 prereleases only.
- A sandbox that a forced stop had already stopped, while the run's changes could not be given up because the server could not read the folder's record or settings, was stopped again with a warning on every watcher tick until the run's keep ran out. Each server now waits 2 minutes before the next stop, doubling up to 10 minutes; the run's credentials are still revoked on every tick. Affects 0.10.0 prereleases only.
- SSH gateway audit: an `ssh.exec` or `ssh.forward` start that was cut because its SSH channel or connection closed, other than by a server stop, now records that close as its `error` ("the SSH channel or its connection closed"), not the message that another operation holds the run; and an sftp transfer or port forward that a server stop cut while it ran is now recorded as a failure, not a success.
- An operator's scan of a folder that the server could not follow on its host (a link loop, more than 255 links, or a name on its way that is missing or could not be read), while a member's workspace attaches it, is now refused for that reason, and the refusal says to fix the folder's path on the host or remove it from every member workspace that attaches it. It was refused as though that member's roots no longer held the folder. Nothing is read either way.
- A library scan (`POST /api/v1/sources/{id}/scan`) that cannot read the workspace list now records a failed `source.scan` audit row, as the workspace scan does. In both rows `detail` names the read that failed, not the database's error text.
- A killed run whose computer folder record can no longer be read, but still holds changes kept for a later copy back (a server upgraded with such a run), is no longer cleared with no record of them: once the sandbox's keep has run out, Wardyn gives those changes up and writes their given_up row, saying the record could not be read.
- A run that was ended past its end while another operation held its lock, on a runner no server could reach, kept showing its changes to your computer as kept after its keep had run out, for as long as that lock was held. Its copy back is now recorded as given up once the keep has run out, with its audit row, without waiting for the lock. Affects 0.10.0 prereleases only.
- A run whose computer folder record cannot be read now still shows on its page that the changes not yet back were given up, with their count when one was kept, and says that the rest of the record could not be read. Before, such a run showed only that its sandbox copy may no longer be available. The `GET /runs/{id}` `computer_folder` answer carries `record_unreadable` for such a record.
- When Wardyn cannot tear down the sandbox of a run it killed because a desktop sign-in was stuck, its background pass now reports the failure on each try instead of counting the pass as healthy. A teardown queued for a runner that is offline now counts as done, as the kill's own teardown counts it, and the sign-in is closed; before, it was logged as a failure and tried again on every pass.
- More decisions that read a run's times now read the database's clock, not the server's own, so servers whose clocks differ no longer disagree about them: whether a revoked run token's late reports are taken, whether a capped run's decisions count as activity, which write a late Bedrock report's hint takes, whether a run's kept output has expired, and when a late Kubernetes kernel event loses its run. When that clock cannot be read, a request answers an error and can be tried again, and a hint or activity write is skipped until the next report. The life of a minted Azure DevOps token, and how long a runner may read a credential handed to it, are now counted from the database's clock too, or from the server's own when that clock cannot be read. Among the decisions this release leaves on a server's own clock, by design, are these two. The check made before a run's lease is sent to its runner reads the clock of the server holding the runner's session, because the runner counts the run's end and its token's stamp from the time that server gives it. Whether a queued run has waited too long is judged on the servers' clocks, because a server's clock stamped the end of its wait from the time the run was queued, the same time its owner's right to start it is checked from.
- Two ends of one desktop sign-in no longer run at once: while one runs, another is refused and sends nothing to the agent, so an end that finished late no longer continues an agent that a newer sign-in on the same run had stopped. An end whose claim cannot be saved is refused and sends nothing; end the sign-in again.
- Ending a desktop sign-in whose desktop closed but whose end could not be saved now says so, and asks you to end it again, instead of saying the desktop could not be confirmed closed.
- Ending a desktop sign-in while it was still starting could close it before the agent was stopped, which left the agent stopped with no sign-in open, or before the person's display had started, which then opened with no sign-in. Such an end is now refused with the new reason `desktop_signin_starting` until the display is up, for at most 6 minutes after the sign-in began; ask for the end again.
- On the Docker runner, a daemon that stops answering can no longer hold, without end, the signal that stops or resumes the agent in a desktop sign-in, or the removal of a run's hold on its drive volumes when the run's start fails: each signal and each removal now gives up after 30 seconds.
- A run whose computer folder record cannot be read no longer keeps its sandbox for good. Once the sandbox's keep has run out, Wardyn gives up any copy back still owed, writes its given_up row saying the record could not be read, and removes the sandbox. Inside the keep nothing changes: a sandbox kept there stays, with the changes it holds.
- When wardynd stops, it now closes each open SSH gateway connection, one still in its handshake included, and waits, within the time it already gives its background work, for the audit record that each connection and each running shell, command, file transfer or port forward writes as it ends. Before, the exit could cut such a record off mid-write. A record still being written when that wait ends is cut.
- `wardyn logout`, and a `wardyn login` that moves the sign-in to the keychain, no longer remove a sign-in file they cannot read or that is damaged, and a `wardyn login` whose sign-in goes to the file no longer replaces one it cannot read. The file may hold other organisations' sign-ins, and before, they were lost with it. `wardyn logout` now fails with the file's error and leaves it as it is; `wardyn login` says the previous sign-in is still stored there, or, signing in to the file, fails and says to make the file readable. A damaged file is still replaced by a new sign-in.
- When the CLI profile cannot be read, `wardyn login` and the sign-in of `wardyn runner setup` now say when the sign-in they replace, or one in the sign-in file, could not be read and so was not signed out, and, when the new sign-in goes to the sign-in file, that the keychain was not read, so an earlier sign-in it may hold is signed out by the next `wardyn login` that can reach the keychain. Before, they said nothing.
- `wardyn login` now signs out every earlier sign-in this computer holds for the organisation in the store it signs in to and, when it signs in to the keychain, in the sign-in file. It says when the keychain item it replaces cannot be read, and says so for the sign-in file when the CLI profile names that file or could not be read. Before, a sign-in that a store move could not remove, or that a damaged CLI profile hid, stayed signed in until it ended on its own.
- When wardynd stops, it now also waits for the clean-up of a credential save that was still running, or that started, while it stopped, until the end of the time it already gives its background work, and a save that could leave such a clean-up, started once the secret store is closing, is refused before it writes the secret. Before, a clean-up that began after wardynd had looked for running ones was cut at once.
- Kubernetes with keep on (experimental): a kept run's proxy replace, stop and end now name the run's policy generation, as on Docker. After a server loses a run's lock, a slower network edit or revive no longer restarts the kept run's proxy with an older version of its rules. A slower stop or end made for an older generation leaves a newer revive's proxy, its config Secret and its running agent as they are. Before, the older rules applied at once, and where the newer edit had narrowed the run's network they let more through until the run was next edited, revived or stopped. This needs every server replica at this release.
- Folder sync on a filesystem without hard links (such as exFAT): a pull whose copy fails no longer removes a copy someone rewrote in place, even to the same length, or an empty one. Both stay, and the refusal says where. A failed copy an editor renamed to a backup name is no longer called its partial copy.
- Kubernetes keep and revive: when a newer revive of a kept run overtakes an older proxy replace or end of the same run, the older one no longer changes the run's EndpointSlice after the newer revive pointed it at its proxy. Before, the older one could empty it, or a slower replace could point it at a proxy pod the newer revive had removed, and the revived agent was left with no network. A proxy replace that fails before its new proxy pod is created now deletes the config Secret it wrote; before, that Secret stayed until the run's teardown.
- Folder changes kept for a copy back that will never run, a killed run's among them, are now given up once their keep runs out, as owed changes are, also while the run's runner is away. Before, a runner that never came back kept such a run's sandbox, and the runner's retirement, open for good.
- When a folder's runner, the site configuration or a person's own Azure DevOps token cannot be read, the refusal of a run or of a policy save no longer carries the failed read's error text; the server log has it. The status and reason are unchanged.
- A run's output that waited on a recording that cannot be read now ends as a capture gap (`recording_unavailable` in its `run.output.finalize` audit row) on a try made 30 minutes or more after the run ended; before, it stayed pending for good. A later read of the recording that succeeds replaces it. One recording that cannot be read no longer holds back the recovery of other runs' output in the same sweep, and no longer fails that sweep once its run is 30 minutes past its end.
- Console, Approvals: a request you decided stays disabled until a new read of its list has landed, or for 30 seconds when none does. Before, a refresh that failed or was overtaken freed the card, which could then show live buttons for a request already decided.
- `deploy/kind/quickstart.sh`: when the Calico daemonset or the Calico controllers do not roll out in 180 seconds, it now prints `FAIL:` with the name of the one that did not. It then prints the `kube-system` pods, the description of that workload's pods and the `kube-system` events, and stops. Each of those commands waits at most 30 seconds.

### Security

- Withdrawing a `component` capability grant now reaches a running run that carries the component: at the run's next credential mint or redeem, every credential on it, and every gateway token for an organisation MCP server, is refused `capability_component`. A run that carries an organisation component deleted since launch is refused `component_gone` the same way. A run that carries a component its owner defined is refused `capability_component` the same way once the `custom_component` feature is denied to its owner. A grant held through a group counts by the groups of the owner's last SSO sign-in, or, for a run a portal launched, their last token exchange through that portal. Each sign-in and exchange now records those groups, a SCIM group removal narrows them, and a sign-in or exchange whose record cannot be written is refused. Each record keeps when it was issued, a portal's from its token, so an older token cannot put back a group a newer one dropped. An exchange refused because the person is no longer in the portal's group still removes the groups its token no longer carries; one refused before that, at sign-in admission, changes nothing. The component's hosts stay open until the run ends or a live egress edit denies them. Before, a running run kept a withdrawn component's credentials until it ended.

### Before you upgrade

- Split-role installs (`WARDYN_PG_MIGRATE_DSN` set) whose app role does not pick up new tables through the `ALTER
  DEFAULT PRIVILEGES` step in Operations: grant the app role `SELECT, INSERT, UPDATE` on `mcp_call_sends`. Without it,
  no `approve_each` MCP call that carries an operation key is asked about or sent, and erasing any person's run tasks
  (`run_tasks`) fails. A call without a key is unaffected only in a run with no `correlation_id`; in a run that has one, every `approve_each` call needs the grant. Grant it
  `SELECT, INSERT, UPDATE, DELETE` on `principal_groups` too, unless it holds `DELETE` on `key_domain_login_groups`,
  itself or through a role it is a member of: the migration grants the table to those roles. An upgrade from before
  0.10.0-rc.21 also needs the 0.10.0-rc.21 grants: `SELECT, INSERT, UPDATE` on `principal_identity_unbinds` and
  `run_teardown_holds`. An
  upgrade from before 0.10.0-rc.18 also needs the 0.10.0-rc.18 grant: `SELECT, INSERT, UPDATE` on `run_proxy_starts`.
  An upgrade from before 0.10.0-rc.17 also needs the 0.10.0-rc.16 grants: `SELECT, INSERT, UPDATE, DELETE` on
  `governance_unit_deciders` and `governance_envelopes`. An upgrade from
  before 0.10.0-rc.14 also needs every grant the 0.10.0-rc.14 notes list: `SELECT, INSERT, UPDATE, DELETE` on
  `run_create_keys`, `wall_presets`, `kept_machines`, `workloads`, `workload_bindings`, `workload_run_ends`,
  `macos_vm_disks`, `aws_sso_refused_tokens`, `cli_sign_ins`, `ui_desktop_closed`, `desktop_signins`,
  `desktop_signin_shares`, `desktop_signin_runs`, `approval_rules`, `governance_restrictions`,
  `governance_restriction_members`, `governance_unit_admins`, `oidc_session_sids` and `desktop_vm_sign_ins`,
  `SELECT, INSERT, UPDATE` on `groundtruth_sensors`, `runner_health`, `launch_preset_version_floor`, `cli_sessions`
  and `governance_boundary` (and on `run_harnesses` if it turns on `WARDYN_MULTI_AGENT_TASKS`), and `SELECT, INSERT`
  on `audit_checkpoints` and `run_model_usage`.

- Split-role installs (`WARDYN_PG_MIGRATE_DSN` set): the migration grants `SELECT, INSERT, UPDATE, DELETE` on `principal_groups` to every role that was granted `DELETE` on `key_domain_login_groups`, other than that table's owner. If the app role is not one of those roles or a member of one, and does not pick up new tables through the `ALTER DEFAULT PRIVILEGES` step in Operations, grant it the same on `principal_groups` as soon as the migration has run. Until then every SSO sign-in and portal token exchange is refused, and so are a SCIM removal from a group and erasing a person's credentials or purging them. A running run that a person owns and that carries a component gets no credential, no gateway token for an organisation MCP server and no gateway client certificate (`503`) until the grant is made. The admin token still signs in.

- When the server cannot read its workspace list, starting a run (`POST /runs`, and its preflight and preview, which share its checks), saving a policy, and starting a workspace's record session now answer `500` with reason `workspace_sources_list_unavailable` where they answered `422`, the class the folder scan doors already answer for the same fault. A run that attaches a workspace by id answered that fault `422` `workspace_seed_unreadable`, and now answers the same `500`. When such a workspace cannot be read for a reason other than its not being found, the run now answers `500` `workspace_seed_unreadable`; an unknown workspace still answers `422`. These answers no longer carry the store's error, which is in the server log. A client that read one of those `422`s as a request to change should retry instead.

- An operation key needs this release's run proxy. In a run with no `correlation_id`, an older proxy drops the key, and asks about and sends each call as before. A run with a `correlation_id` and an `approve_each` rule does not start on an older proxy or runner.

- The name `erased`, in any letter case, is now reserved: an identity with that name can no longer sign in or use a token, and Wardyn refuses to erase records or credentials under it; in local mode an `X-Wardyn-Principal` header naming it is ignored, and the request counts as the local operator's. Before you upgrade, erase what you need to under that name, then give that person another name at your identity provider.

- Upgrade every wardynd replica together. A replica of an earlier release answers `401` to the run token of a run a workload launched, and going back to an earlier release does the same to every such run. A run that was running or waiting in a queue at the upgrade keeps its `agent-run` ID. A gateway rule that admits runs by `spiffe://<trust domain>/agent-run/` no longer admits a workload's new runs: add the workload's prefix, `spiffe://<trust domain>/workload/<owner>/<name>/run/`, and keep the old rule until the older runs end.

- Run this release's `wardyn-proxy` image. Check `WARDYN_PROXY_IMAGE`, `WARDYN_PROXY_IMAGE_AMD64` and `WARDYN_PROXY_IMAGE_ARM64` on wardynd, and any runner that keeps its own image (`WARDYN_PROXY_IMAGE_PINNED`). An earlier proxy refuses the gateway client certificate of a run a workload launched and sends that run's gateway calls with no certificate, so a gateway that requires one refuses them.

- Runs a portal started before the upgrade: Wardyn has no record of their owners' groups through that portal until each owner's next token exchange through it. Until then, such a run that carries a component keeps it only through a grant to its owner, their user type or everyone, and only while nothing denies it. Otherwise it gets no credential and no gateway token for an organisation MCP server (`403` `capability_component`).

- This release adds database migrations, 0288 to 0290, all forward only. Going back needs a database restore, so take a database backup first.

### Known issues

- In a run with a `correlation_id`, a second identical `approve_each` MCP call that waits at the same time as the first may ask a person, and that approval then sends nothing.
- A workload's launch policy digest is comparable within one Wardyn version. An upgrade can change the digest of the same policy, and each pinned workload's next launch is then refused until it is pinned again. After an upgrade, preview each pinned workload with its `restart_key` changed to read its new digest.
- Only a server of this release or later holds a workload's launch to its policy pin. While an upgrade rolls out, or after a downgrade, a pinned workload can launch without the check.
- Deleting a workload deletes its policy pin, and a workload applied again under that name starts unpinned. For a custom resource, whoever can delete and re-create the resource can drop the pin this way without the service identity's token. Pin again after the first launch, and watch `workload.delete`.
- macOS VM bases with `/Library/Wardyn/carry-before-login`: not yet known on a real Mac are whether a restart started inside the guest keeps the VM running, whether macOS then keeps `agent` at the login window until the carry is done, and whether the carried settings keep Setup Assistant's Apple Account pane closed. If runs on a test base fail as stopped while booting, remove the two items from its list.
- On a Kubernetes install with keep on, the pool's keep chip and the ending-soon notice say files are kept for every interactive run. Only runs with a kept work folder and a `claude-code` or `codex-cli` agent are kept.
- If a step on a server holds the lock of a run whose end Wardyn recorded under that lock and does not let go, that run's sandbox is still not stopped, and a copy back it owes is neither run nor given up, until that step ends or that server restarts. Its network is still cut off where the sandbox can be cut, but the 7-day keep and the stop times in the 0.10.0-rc.20 notes do not apply to it. The steps Wardyn takes under a run's lock now have time limits, so this needs a call that runs past its limit or was given none. Such a hold is counted in `wardyn_lock_holds_over_limit_total` once it passes 10 minutes, and logged at WARN with the functions that took the lock; the lock is not freed. A run whose end was a forced stop is not such a run: its stop is retried without waiting for the lock, and its keep applies.
- When a run ends and the server cannot read the folder's settings or the run's rule for letting files out at that moment, the sandbox is still stopped without copying anything back when its network cut cannot be confirmed, such as on a virtual machine, on Kubernetes in server mode, or when the cut fails at that moment. The same happens when the run itself cannot be read then. The run records that the changes were not brought back once the sandbox has stopped, or later if the server still cannot read them then. This applies to a computer folder set to sync both ways through your own runner. The agent's changes in the sandbox's copy of that folder are lost; the folder on your computer is not changed.
- A forced stop of an ended run, made when its end comes due and Wardyn cannot take it, because another operation holds the run's lock or the server is already ending as many runs as it ends at once, and that is still so 5 minutes past the end of the run's keep grace, or 5 minutes past the time the run ended or was lost for a run that cannot be kept, still stops the sandbox when its network cut cannot be confirmed or the sandbox cannot be kept: a virtual machine, Kubernetes in server mode, a Kubernetes runner, a Docker runner older than 0.10.0-rc.20, or a runner that is not connected. Changes not yet copied back are then lost, and the run records "Not brought back: the run was ended past its end while another operation held it or too many runs were ending at once", unless the server stopped before writing that record. When the server could not read the folder's settings, its record or the run's rule at that moment, the run records that the changes were not brought back once the sandbox has stopped, or later if the server still cannot read them then. A failed stop is retried on the next watcher tick, normally within a minute.
- A copy back that finds the folder busy on a sandbox that cannot be held (a virtual machine, or an agent that cannot be stopped alone) stops the sandbox at that visit and records "not brought back". The same happens when the last pass may still be running on an older runner and the sandbox's agent cannot be stopped alone. Changes the running pass had not finished copying are lost.
- Changes not yet copied back are lost when the laptop's runner is offline at the end of the run and the sandbox cannot be held: a virtual machine, an agent that cannot be stopped alone, or a sandbox on that same offline laptop. On Kubernetes in server mode, they are lost when the sandbox is stopped (see the server-mode line below).
- If the server stops in the moment between a run's end and the start of its access revoke, or the database cannot be written at that moment and the server stops before it can be written again, and the run holds no sandbox, that revoke is not retried. Until the server stops, it tries about once a minute to record the revoke as owed, and the revoke is retried once that record lands.
- A copy back whose result cannot be written to the guarded folder record stops the sandbox, so changes it left behind are lost.
- The first failure of a copy back that settles with an error only, such as a failed file listing before anything was counted, still stops the sandbox, and changes it did not list can be lost.
- With an older runner, the next copy back waits about an hour after one the runner did not answer, and the two can still overlap.
- Copy backs to a virtual machine are not guarded on an older runner, so one can run beside a stuck pass.
- A generation bump that fails with a store error, or fails after a folder record could not be read, stops the sandbox without a copy back, so its changes are not brought back. The run records why when the store takes that record; while the store error lasts, only a server log line says so.
- On Windows, a file replace falls back to a plain rename, with no write-through, when the folder's path cannot be resolved.
- On a Kubernetes runner that restarts while cut off from the server, a run can keep network access past its scheduled end. Its proxy stops serving at most about 65 minutes after the server records the end, or earlier when a renewal is refused for good.
- A Kubernetes sandbox template cannot set where a sandbox runs or its runtime: a template that sets them is refused, because the network enforcement check covers only Wardyn's own placement.
- On Kubernetes, a narrowed proxy restarts on its launch policy if its deny list is lost: a node's disk wiped under the pod, or someone with exec rights deleting it. It allows the denied hosts until the next edit. One restarted over about 30 minutes in cannot renew, so its proxy is stopped and its run marked lost or ended within about 66 minutes of its last renewal.
- The fix for overlapping network edits on Docker applies only when the server, the runner and the proxy image are all at 0.10.0-rc.19 or later, and the check that keeps a late edit or stop from removing a newer edit's proxy needs every server replica and the runner at 0.10.0-rc.20 or later. A live network edit is refused with an update hint when a part is known to be older; a part whose version can't be read (a digest-pinned or custom image, a runner that reports no version, or a runner's own proxy image) is not checked. During a rolling upgrade, update every server replica before using live network edits: a replica still on rc.19 starts proxies without that check. A revive or admin restart whose policy narrows because org policy or the ceiling changed mid-run can still race a stale revive on an older runner or proxy image.
- After a lost database lock, a slower attempt can still stop a newer revive's proxy or sandbox in four cases: when that attempt is ending the run; when it is a background sweep that marks the run lost or stops a kept run's proxy again (about every minute); when the newer revive came from a server still on 0.10.0-rc.19; or when the run's runner was not connected, even briefly, as that attempt's stop began. Outside a rare race inside the stop itself, it no longer happens on Docker, with the server and a connected runner at 0.10.0-rc.20, when a failed revive or network edit marks the run lost or stops a kept run's proxy again. The run then has no network (it never widens), and its policy can still read as enforced until it is revived, or until a background sweep, within about 66 minutes, marks it lost or ends a run that cannot be kept.
- Kubernetes with keep on (experimental): the proxy generation check that Docker has now covers a kept run's proxy replace, stop and end, once every server replica runs 0.10.0-rc.22 or later. While a replica still runs an earlier release, after a server loses a run's lock a slower network edit or revive can still restart the kept run's proxy with an older version of its rules, which then apply at once; if the newer edit narrowed the run's network, the older rules let more through until the run is next edited or revived, or is stopped. With every replica on this release, a slower end after a lost lock can still stop the sandbox a newer revive had just started in the cases of the line above on a lost database lock, such as an attempt that is ending the run: the person's session ends, and the files in the run's work folder are kept.
- If a headless run that is not on a laptop runner loses its database lock while a revive or live egress edit of it is failing, and another server revives the same run at that moment, the failing attempt can end the newer revive's run: the run stops and its sandbox is removed. Start a new run.
- If the server stops right after restarting a revived run's agent, or cannot record that the agent restarted, later network edits of that run are refused until it is revived from its page.
- If a run's lost mark cannot be written after its database lock was lost, the server tries to stop its proxy anyway. On Docker with the server and the runner at 0.10.0-rc.20 or later, that stop leaves a newer revive's proxy alone; it can still stop one started by a server still on 0.10.0-rc.19 during a rolling upgrade, or one on a runner that was not connected, even briefly, as the stop began, and, in a rare race inside the stop itself, a newer revive's agent.
- During a rolling upgrade from 0.10.0-rc.19, on Docker, a late network edit can still remove the proxy that a newer edit of the same run just started, when either edit came from a server still on rc.19. The run then has no network (it never widens), but its policy reads as enforced until a background sweep, within about 66 minutes, marks the run lost (revive it from its page) or ends a run that cannot be kept (start a new run).
- A delayed end whose lock was lost can also remove a sandbox that another server was keeping to copy changes back; those changes are lost.
- While laptop runners are still on an older release, a network edit that one had already started when the server was upgraded can briefly give a cut-off run its network back. Wardyn cuts it again: within about a minute of the edit for a run that is still kept. For an ended run, it cuts it within about 13 minutes of the edit, or about 19 minutes after the run's end if that is later, when up to 100 runs end at once on one server; about 8 s more for each run beyond that.
- When the server comes back from an outage, a virtual machine run that had already ended before it, and whose copy back had not finished, is stopped at the server's first visit to it if more than 6 minutes have passed since it ended: within about 2 minutes of the restart when few runs need a stop, and up to about 65 minutes when up to 100 runs need one at once on one server; about 39 s more for each run beyond that. Its changes not yet copied back are lost, and the run's page says they were not brought back.
- On Kubernetes in server mode, an ended run's sandbox, with its agent stopped, is stopped within about 13 minutes of the run's end (about 16 if its folder is being copied back at that moment), or up to about 70 minutes when up to 100 runs end at once on one server; about 39 s more for each run beyond that; changes not yet copied back are lost. These times do not cover a server whose own sandboxes run on Kubernetes and on another substrate at once, or a run whose lock another step holds for longer than a copy back (see the line on a held lock).
- The times in these notes and in the 0.10.0-rc.20 notes for cutting off and stopping ended runs assume that the machine running the sandbox answers each request within its time limit. If that machine stays connected but answers late, only its own runs are delayed, by up to twice its delay. A sandbox whose machine never answers is not cut off or stopped by the server until it does.
- If the server stops or restarts after a run has ended and before its first copy back has started, a background sweep can stop the sandbox without copying anything back when the sandbox cannot be held for that copy back: a virtual machine, a sandbox on Kubernetes in server mode, or a sandbox whose agent could not be cut off from the network at that moment. This applies to a computer folder set to sync both ways through your own runner, on a run that lets files out of the sandbox. The agent's changes in the sandbox's copy of that folder are lost; the folder on your computer is not changed. The run now records that the changes were not brought back; if that record cannot be written then, a later sweep tries again. A folder that copies in only is not affected.
- On a Kubernetes runner or a Docker runner older than 0.10.0-rc.20, and on any runner whose network cut fails at that moment, a copy back that does not finish in one pass is given up and the sandbox stopped; those changes are lost. On a runner that is connected, a background sweep still stops a sandbox that is waiting for its next copy back when it cannot cut off the sandbox's network at that visit; those changes are lost too. On Kubernetes in server mode, the sandbox is stopped at the times given above, and changes not yet copied back are lost.
- Kubernetes: a No network run's proxy pod reaches Wardyn through a pod-selector NetworkPolicy peer behind a Service address. A CNI that matches NetworkPolicy before Service translation blocks it, and the run fails about a minute after it starts, with a hint that names this. A run Wardyn keeps is marked lost, or stays lost, without the hint. Use a CNI that matches after translation.
- Kubernetes: where Wardyn reads the proxy as older than 0.10.0-rc.18, from the server's proxy image tag or, on a runner, from the runner's reported version, nothing waits for it to confirm the policy it loaded, so the failure above does not happen. On such a CNI the run goes on until its run token lapses, about 65 minutes later, and the older token-lapse handling then ends it or marks it lost, without the hint that names this cause. A run that cannot be kept gets only the hint "run token lapsed: the control plane was unreachable past its renew window", which does not name the cause. A version Wardyn cannot read, such as a proxy image pinned by digest only, is still waited for, so that run fails as above.

### Corrections to earlier notes

- A forced stop happens when a run's end comes due and Wardyn cannot take it, because another operation holds the run's lock or the server is already ending as many runs as it ends at once, and that is still so 5 minutes past the end of the run's keep grace, or 5 minutes past the time the run ended or was lost for a run that cannot be kept. Wardyn then ends the run without the lock, whatever the holder does later. The earlier notes said only that the run's lock had been held that long.
- A step on a server that does not let go of a run's lock keeps the sandbox from being stopped only for a run whose end Wardyn recorded under that lock. A run whose end was a forced stop is not such a run: its stop is retried without waiting for the lock, and its keep applies. The earlier notes did not say which runs this covers.

## [0.10.0-rc.21] — 2026-11-15

### Added

- `/metrics` has a new counter, `wardyn_lock_holds_over_limit_total`: holds of a run's lock that ran past 10 minutes. Each such hold is also logged at WARN with the functions that took the lock, and again with its length when it is released. The watchdog never frees the lock or cancels the holder's work.
- Helm: `uiSandbox.ingress` renders the UI-sandbox gateway's Ingress: the `advertiseURL` host and the wildcard host `originTemplate` needs (`run-{run}.ui.example.com` gives `*.ui.example.com`), to the `ui` port. DNS and the certificate stay yours.
- Helm: with `uiSandbox.enabled`, no SSO, no `uiSandbox.originTemplate` and no `uiSandbox.acceptSharedOrigin`, `helm install` and `helm upgrade` now print a warning: every run's apps share one browser origin, so a page one run serves can reach another run's app in the same person's browser. It names both settings, and the chart renders as before.
- macOS VM bases: a second opt-in switch, `/Library/Wardyn/carry-before-login`, beside `/Library/Wardyn/setup-assistant-carry`, which alone never turns it on. The base then rests with auto-login off: at each boot the guest agent runs the per-host settings carry with nobody at the console, writes auto-login back only after it, and restarts the guest once. The boot after that restart writes `/Library/Wardyn/run/login-hold.status`, and a run starts one restart later. Off by default; not yet tried on a real Mac.
- An `otlp_metrics` block in `WARDYN_AUDIT_SINKS` makes each wardynd replica push its `/metrics` series to `<endpoint>/v1/metrics` as OTLP/HTTP JSON every `interval` (default `60s`). It is separate from the `otlp` sink and sends no audit row. Off by default; the Prometheus scrape is unchanged. A failed push counts in `wardyn_otlp_metrics_push_failures_total`. See [monitoring](docs/operations/monitoring.md#opentelemetry-metrics).
- `POST /api/v1/runs/{id}/exec` (SDK `ExecRun`) now takes a background run (`experience: background`), so a pipeline step can run commands in its own headless run. Before, it answered `409 run_exec_unavailable`. A background run still has no SSH access, so `wardyn run exec` still refuses it, and a command does not keep the run going. Each command and its exit code are audited (`run.command.exec`); its output is neither masked nor recorded.
- Kubernetes warm pools (experimental): idle placeholder pods per pool hold room on a node with the pool's image pulled, and a run that fits a pool prefers that node. It is a preference: the run's pods are still made fresh and checked before start, and no time saving is measured. At most 20 placeholders per pool and 50 in all, at low priority and set not to preempt other pods. The pools and the claim are Wardyn's own and work beside the agent-sandbox controller; the upstream `SandboxWarmPool` and `SandboxClaim` are not built, and no run is handed a pod that already ran. Settings: `k8s.warmPools`, `WARDYN_K8S_WARM_POOLS`.
- Kubernetes keep and revive on the agent-sandbox backend (experimental, off by default): with `k8s.agentSandbox.keep.enabled` (`WARDYN_K8S_AGENT_SANDBOX_KEEP`), an interactive `claude-code` or `codex-cli` run whose work folder is a kept drive is kept when its time runs out, with no compute, and after an extend it can be revived. This is keep and revive, not a freeze: Kubernetes has no pause button, and running programs, memory, open connections and the agent's conversation are lost. Only the work folder stays, and its storage is still used. Such a run reaches its proxy through its own Service, so restart with current limits works on it while it runs; if its proxy pod is deleted, its agent keeps running with no network. Turning keep on gives wardynd Service and EndpointSlice rights in the runs namespace, and the chart's admission policy limits what it creates there to each run's own Service. Pause in place stays Docker-only.
- A model gateway or organisation MCP server that asks for a client certificate now gets one naming the run: an X.509 certificate whose one URI SAN is the run's SPIFFE ID, signed by a new CA (`wardyn-gateway-client-ca`) and checked against the bundle at `/.well-known/wardyn-experimental/gateway-client-ca.pem`. It comes with run identity, with no setting of its own. When there is none to send, or the gateway names only other CAs, the proxy sends none, so a TLS front that only asks lets the call through. The identity token is still sent, and the gateway must still check it on every call.

### Changed

- Helm chart: a base path from `basePath`, `env` or `extraEnv` that wardynd refuses at boot, or one that refers to another variable as `$(NAME)`, now fails the render. The kubelet expands `$(NAME)` in the container's environment but not in the probe paths, so wardynd served under one prefix while the probes asked under another. Set the final literal path.
- `wardyn-runnerd` chart: an empty `apiServer.ports`, an entry in it that is not a port number, and a `relay.servicePort` that is not one now fail the render. Before, an empty list rendered an egress rule that let runnerd reach every port, and an empty entry every TCP port, at any address while `apiServer.peers` was empty. The install notes now name the configured ports.
- Helm chart: a `networkPolicy.postgres.port` that is not a port number, and a `k8s.apiServer.ports` entry that is not one, now fail the render. Before, an empty value rendered an egress rule that let wardynd reach every TCP port, at any address for Postgres, and for the API server while `k8s.apiServer.to` was empty.
- Helm chart: a `WARDYN_BASE_PATH` or `WARDYN_MIGRATE_TIMEOUT` set in `extraEnv` through `valueFrom` now fails the render, because the chart sets the probes from both. Set a literal value, or `basePath` for the base path. Before, the chart ignored such an entry when it set the probes.
- Helm chart: with `basePath` set and a `WARDYN_BASE_PATH` in `extraEnv`, the chart now writes only the `extraEnv` entry, where before it also wrote its own from `basePath`. A release that set both before this chart can lose `WARDYN_BASE_PATH` from the Deployment on a `helm upgrade` that leaves one entry where there were two, because the upgrade removes the entry by name. That is the first upgrade to this chart, or an upgrade on an earlier chart that drops one of the two settings, so setting the base path in one place first does not avoid it. After such an upgrade, check that the Deployment still sets `WARDYN_BASE_PATH`. Without it wardynd serves at the root while the probes ask under the base path, and the pod is restarted until the variable is set again.
- Helm chart: wardynd's container env now carries each name once. Where a chart setting, `env` and `extraEnv` name the same variable, the chart writes only the entry it wrote last before: `extraEnv` over `env`, either over a chart setting, and the `k8s.*`, `ssh.*`, `uiSandbox.*` and `metrics.listener.*` entries over both; a name listed twice in `extraEnv` keeps its last entry. Before, the chart wrote every copy, and a later upgrade that dropped one copy could remove the variable from the Deployment. A release whose last deployed manifest lists a name twice can lose it the same way on its first `helm upgrade` to this chart. Helm works from that stored manifest, so the live Deployment can list the name once and still lose it. Before upgrading, run `helm -n <namespace> get manifest <release> | awk '/^---/{d=0} /^kind: Deployment$/{d=1} d' | grep -E '^ {12}- name: [A-Z_][A-Z0-9_]*$' | sort | uniq -d`. The command finds upper-case names only. If you set a lower-case name such as `no_proxy` in both `env` and `extraEnv`, or twice in `extraEnv`, treat it as printed. If it prints a name, run your `helm upgrade` command with `--dry-run` to check that it renders, then `kubectl -n <namespace> delete deployment <deployment>`, then `helm upgrade`: Helm creates the Deployment from this chart, with each name once. wardynd is down from the delete until the new pod is ready. After the upgrade, check the value of each name the command printed, because the pod may have been running another copy's value.
- `GET /runs/{id}` and `POST /runs/{id}/computer-folder/bring-back` no longer send `given_up_audited` in a computer folder's pass records; it was the server's own audit bookkeeping.
- A live egress edit (`PATCH /api/v1/runs/{id}/egress`) now has one deadline on every substrate: counted from the server's own time limits, and not counting a stalled database read, it answers within 8 minutes, including for a run with a computer folder. If the edit runs out of time while it stops the run, the rest of the run's end, such as copying the folder back and stopping the sandbox, finishes after the answer. An ingress in front of the server needs a proxy read timeout of at least 530 seconds for it.
- The macOS downloads are now built with Go 1.27, so they need macOS 13 or later; rc.20's ran on macOS 12.
- Go SDK: `DeleteMyDrive` now returns a `*MyDriveDeleteUnfinishedError` when the server took the delete of a disk drive on a Mac without removing its files (deferred while the Mac is offline, or record only once the Mac is no longer registered); before, it returned `nil`, as for a finished delete. The new `DeleteMyDriveResult` returns the server's `reason` and `message`.
- The proxy no longer sends a call to the host and port of a model gateway or MCP server with run identity on when no identity route's path prefix there covers the path, or the path has a dot segment or a backslash. This holds for a plain proxy request, a request in a tunnel the proxy terminates, the brokered model route and the MCP broker. The Git brokers, the push check's reads of the forge and the GitHub, Azure, Kubernetes and git_pat API doors and the AWS door on a host AWS serves never carry the identity, so they send such a host and port nothing at all. A tunnel the proxy does not terminate is not read, as before. The 400 answer names the route and says to remove any dot segments or backslashes, or, for a path outside the routes, to widen the intended route's path prefix or give the other service its own host and port. Before, most such calls were sent without the identity. Affects 0.10.0 prereleases only.
- A plain proxy request whose host name has a character outside ASCII is answered 400 and not sent. A client sends an international name in its `xn--` form.
- The folder-only runner now opens every folder from the top of the drive down to each of its roots when it starts, and refuses to start when the runner's user cannot read one of them. The error names that folder: make it readable, or choose another root with `--folder-root`.
- `PUT /api/v1/policies/{id}` now requires `expected_updated_at`, the `updated_at` the caller read. A `PUT` without it is refused `400` with reason `policy_revision_required` and writes nothing. The console, `wardyn policy set <id> -f` and the Go client's `UpdatePolicy` (`PolicyRequest.ExpectedUpdatedAt`) send it. `wardyn policy set <id> -f` refuses a file with no `updated_at` before it sends anything: start from `wardyn policy get <id>`, or pass `--overwrite`.
- `POST /api/v1/runs/{id}/exec` answers a governance profile's refusal as `403 governance_unit` at target `runs.exec`, with its own error naming the limit: `deny_interactive` on any run, and now `deny_task_mode_exec` on a background run. Before, `deny_interactive` was recorded at target `runs.attach` with the terminal's error. Its `409 run_exec_unavailable` errors no longer say "ssh unavailable". On a runner of this release, a Docker sandbox whose runtime runs only the task (libkrun) refuses any command beside the task before a `docker exec` is made, so this route answers `501 run_inspect_exec_stream_unsupported` instead of `502 run_exec_failed`, and every other feature that runs a command in such a sandbox (SSH, the Files panel and the rest) gets the refusal it gives a runner with no command streaming.
- The `/healthz` sensor reason for a sensor that saw no kernel events now starts with a capital letter, like its other reasons: "No kernel events observed", before "no kernel events observed". The reason for a sensor that saw kernel events but tied none to a run likewise reads "Kernel events observed but none correlated to a run". A check that matches the reason text exactly needs the new case.
- `PUT /secrets/{name}`, `/me/secrets/{name}` and `/shared-secrets/{name}` take `If-None-Match: *` to store a name only if it is new: a name the namespace already holds answers `409` with reason `secret_exists` and nothing is stored. Without the header the PUT replaces the value as before.
- The boot now names, in a warning and in the boot canary's refusal, an operator's trigger on the audit tables whose function sets no search_path, instead of saying the chain trigger is not working.

### Fixed

- Saving a role mapping that could not revoke every API token holding the old role now says so in the console, with the remedy, beside the count of tokens it did revoke.
- Deleting a role mapping that could not revoke every API token holding the old role now says so: `DELETE /access/mappings/{id}` answers `200` with `tokens_revocation_failed` and the remedy in `detail` instead of `204`, approving a held role-mapping change adds the same two fields to the change it answers, and the console shows the remedy.
- An Azure DevOps token whose revoke Azure DevOps refused, or whose revoke is not confirmed yet, is no longer shown as revoked: the Settings card and the run page say it may still work and until when, and read it as expired once that time has passed. Disconnecting answers `200` with `tokens_revocation_failed` and a remedy when a token it tried to revoke may still work, and the console shows it.
- The Output tab reads a run's output again a few times after the server cannot prove the run's secrets masked, and offers Retry, instead of stopping until the page is reloaded.
- Rebuild on a workspace now asks for a fresh build and says what the server did: started, nothing to rebuild, or not rebuilt with the server's reason.
- A refused recording launch now shows the server's sentence for a `422` and for a `503` other than no runner; only the no-runner answer says `-runner none` and disables the form, so a failed read can be retried.
- A custom base image whose steps run to several kilobytes of text that does not compress is now saved instead of failing with a server error; a custom image saved again with the same base image and steps still shares the one catalogue row.
- A desktop sign-in whose begin was cancelled while it was being saved or while the agent was being stopped, for example by a dropped connection, could stay open until its 30-minute window passed: a new sign-in on the run was refused, and the agent stayed stopped if the stop had landed. The begin now closes the sign-in at once, resuming the agent first where it may have been stopped; if the agent cannot be resumed, the sign-in stays open and the sweep retries it.
- When the agent's stop at the start of a desktop sign-in answered an error, the sign-in closed without resuming the agent, so an agent whose stop had landed anyway could stay stopped. The begin now resumes the agent after any such error and then checks that the agent is running: the close's `agent_resumed` reports that check, and an agent still stopped keeps the sign-in open while Wardyn retries, then ends the run if the agent still cannot be resumed.
- When a desktop sign-in's end could not prove the person's display closed, or could not resume the agent, the agent stayed stopped while Wardyn retried with no limit. The end now also checks that the agent is running after resuming it, and if the end still cannot finish after 3 failed retries, Wardyn ends the run.
- With more than one server replica, a desktop's screen recording could stay paused after a desktop sign-in ended, when the replica that held the pause missed the other replica's notice that it had ended. Each open recorded desktop connection now reads whether the run's sign-in is still open and resumes the recording once it has closed, and a new connection reads it before it opens. A desktop opened on one replica while a sign-in begun on another is open now starts paused.
- In store mode (Vault or Azure Key Vault), saving your own model provider key, a pasted Claude sign-in, the AWS SSO renewal's write check, and the note that Azure DevOps refused your own token now also record a `secret.write` failure with reason `row` when the save may have reached the store without its row being confirmed; before, that row was missing. That note's `ado_pat.own.store` row is now written then too; before, it could be lost for good. An AWS SSO renewal whose first save landed while its reply was lost is now recorded as a success, not as `superseded`.
- The run output sweep now reaches every run that still owes its output or its macOS VM agent recording, even when more than 200 others fail on every pass; before, those 200 held the sweep and newer runs were never reached.
- Helm chart: the liveness, readiness and startup probes and the ServiceMonitor path now follow a `WARDYN_BASE_PATH` set in `extraEnv`. Before, they followed only `basePath` or `env`, so with a different base path in `extraEnv` the probes failed and the pod was restarted.
- A run's output that waited on its recording now ends as a capture gap, with the reason in its `run.output.finalize` audit row, when the recording was erased (`recording_erased`) or a read of it ran over its 10-second budget on a try made 30 minutes or more after the run ended (`recording_over_budget`); before, it stayed pending for good. A later read of the recording that succeeds replaces it.
- Desktop Files panel uploads: a failed upload says nothing was kept only when the desktop confirmed it removed the partial file, which it now also does when writing or renaming the file fails; otherwise it says the upload failed and some bytes may remain. A failed upload's status reads Upload failed instead of Not sent. An upload that fails while the browser has stopped sending now answers without waiting for it; before, the answer waited until the browser sent more or gave up.
- A desktop file whose name ends in a space now lists and downloads under its own name; before, the listing dropped the space and the download asked for a different file.
- A desktop download that fails after it has begun is now cut off, so the browser does not save a short file and the Files panel does not mark it downloaded; before, it could end as a clean, shorter file. A download whose stream fails before its first byte now answers as a failed download; before, it could be saved as an empty file. A reviewed download whose approval was used without the file reaching this browser now says the approval was used, instead of Downloaded at the approval time.
- When a forced end keeps a run's sandbox for its copy back, the Folders from your computer card now shows the reason the server recorded, as kept for its copy back, instead of saying the changes could not come back.
- Saving a credential in store mode (Vault or Azure Key Vault) now answers as soon as its row is written. Removing the value the save replaced, or a value whose row was not written, runs after the save, under the credential's own lock, and gives up after six times `WARDYN_SECRET_STORE_TIMEOUT`, as the save does; before, it ran inside the save and held every lock the save's caller held, with no time limit at all for a value whose row was not written. A local save over a credential still kept in Vault or Key Vault removes the old copy the same way. A credential's first save, or a save that moves it to a new Key Vault secret name, whose commit was cut off now fails at once, saying it is unknown whether its row was written, instead of waiting to check; the value stays if the row points to it.
- When the transaction that writes a credential's row fails or runs out of time partway, undoing it no longer waits past the save's own time limit: once the save's time is up, the connection is closed, and the database undoes the save when it sees the connection gone. Before, a connection that stopped answering could hold the save, and every lock its caller held, with no time limit.
- When wardynd stops, it now waits for a credential save's clean-up of a value in Vault or Azure Key Vault that it replaced or did not record, within the time it already gives its background work; a key rotation, or a start that fails right after saving a key, waits for it too. A clean-up still running when that wait ends is cut; the value is kept, and `wardynd -reconcile` lists it. Clean-ups running at once are capped at a quarter of the database pool's connections (rounded down, at least one): past that, a save skips its clean-up with a warning, and the value is kept and listed the same way.
- On a Docker runner, ending a Linux VM run whose AWS credential caches could not be wiped was reported as finished even when one of the VM's helper containers could not be removed. That end now reports the removal failure, as it does when the wipe succeeds.
- On a Mac runner, a stop, kill or end of a macOS VM run whose VM helper process outlived its kill was reported as done. It now fails with that reason; the run's disk and VM slot stay held as before, and stopping the run again finishes it once the helper has exited.
- On a Mac runner, an update of a macOS base or kept machine whose new copy failed its file check, and whose swap back to the old copy failed too, deleted the old copy. The old copy is now moved aside, next to the base or kept machine, and the error names where it is. Deleting that kept machine, or its expiry, deletes the moved-aside copy with it, so move it back first to keep it.
- On a Mac runner, two tools updates running at once that downloaded the same tool could push a copy other than the one whose checksum was checked, or fail. The second now waits for the first's download and reuses the checked file.
- On a Docker runner, a Linux VM run ended because its audit spool was full, and whose AWS credential caches could not be wiped, kept being ended again on every pass, with a warning logged each minute. That end now counts as finished.
- On a Mac runner, when the runner refused a kill or end of a macOS VM run because it could not see the VM stop, the server kept only the refusal, and never learned when the runner's later try finished it. The run page now shows the kill or end as not finished on the runner, with the time the runner began holding it, until the runner reports that it finished, and then the time it finished. `GET /runs/{id}/containment` returns it as `teardown_hold`. Once the runner reports that its teardown hold has cleared, the server sends the refused kill again on its next reconcile pass, so the run's teardown goes on. A migration (`0286_run_teardown_holds`) adds one table; split-role installs need the grant under Before you upgrade.
- A workload apply whose record of its new run landed but answered an error, as when the caller went away while it was saved, ended that new run though the workload pointed at it; the run it replaced was ended too, and the service stayed down until its spec changed or it was restarted. The outcome is now read back: a run the workload took keeps running, and one whose outcome cannot be read is left to the workload sweep, which ends it only if the workload did not take it.
- The workload sweep now reaches a run whose end is due, such as the run of a deleted workload whose end was cut short, when 100 or more older owed ends are waiting for their queued replacements to start or fail on every pass; before, those 100 filled every pass and the newer run kept running until they cleared or another limit ended it.
- A workload applied with a changed spec while its previous launch was still starting now launches the changed spec at once, and the earlier launch's run is ended with `workload_replaced`, also when the changed spec's launch is refused; before, the changed spec was stored but not launched until the workload was applied again or restarted, or, for a custom resource, until the runner next reported it.
- A run created just as the person who saved one of its components erased their components, or deleted that component, could keep a copy of it in the run's record if the server stopped at that moment. The copy is now checked in the same database transaction that writes it, and the create fails instead.
- File recording store: retention (`WARDYN_RECORDING_RETENTION_DAYS`) could delete a recording saved again under the same name just as the sweep reached it. The sweep now checks the file's age again, under the recording's lock, before it deletes it.
- File recording store: an upload for a run whose recordings were erased is now refused before any of it is written to disk. Before, it was written to a staging file first, and a crash during the upload could leave that file until retention removed it.
- A recording upload whose save failed before reading any of it, and whose body then stalled, could wait for the rest of the body with no time limit before it was answered. It is now answered within 60 seconds of the failure, the upload's read window.
- A credential value registered for masking a run's output at the moment the run's masking values were erased (`mask_copies`) or purged by retention could stay in the server's memory until it restarted; the stored copy was erased as before. That registration is now refused and leaves no copy. When the server cannot read at that moment whether the run's values were erased or purged, the registration is refused too, and the value is dropped once the server's next read of the masking values, or the next registration of the same value, finds that they were.
- With `WARDYN_AUDIT_SEAL=full`, an audit row written before an identity row was unbound (`POST /api/v1/admin/identities/{id}/unbind`) read as whoever the row was bound to next, and erasing that person after the unbind did not erase it. An unbind now records the principal it released and the database time of the unbind. A row written at or before that time reads as that person, or `[erased]` once their audit personal fields are erased; erasing the row's later holder no longer erases it, and that erasure lists the person under `not_erased`. Erasing their audit personal fields clears the person from that record, and an unbind of a person already erased records no name. An unbind made before this change left no record, so rows from before it read as the next person the record names, or as before when it names none; a row an earlier erasure had turned to `[erased]` still reads `[erased]`. A row that waited to be written because its person could not be looked up, and is written after that person's unbind, is stored under the identity row that was theirs when it happened, so it reads and is erased the same way instead of keeping their name. A migration (`0283_principal_identity_unbinds`) adds one table; split-role installs need the grant under Before you upgrade.
- The Go SDK now sends an empty `requirements` map on `CreateSource` as `{}`, so re-adding an existing source with one clears its contract, while leaving `requirements` unset keeps it. Before, the empty map was dropped and the server kept the old contract while answering 200.
- A governance document's empty `deciders` section now stays in the document when the SDK encodes it, so `wardyn governance set --prune` of a file written from it still clears every unit's rules. Before, the section was dropped and the prune cleared nothing.
- `ApplyGovernance` no longer changes the document it is given, so the same document can be applied again after a success or a failure part way. Before, a second apply sent memberships naming ids the server did not hold.
- `wardyn governance plan` and `set` now refuse a document with two units of one name or one id. Before, the apply matched entries to whichever of them came last.
- `wardyn governance set --under --prune` now reads the subtree again just before it prunes and stops, pruning nothing, when a unit it read inside has been moved out by then. Before, it pruned from its first read alone. Each prune write now also names the subtree, and the server refuses one whose unit is no longer inside it (409 `governance_unit_not_under`); a server without that check ignores the name. A held prune write made with `--under` is refused as stale when a unit above it changes before it is decided; propose it again.
- `wardyn person erase`, the Go SDK and the console's erase dialog now show the principals an erasure left alone (`not_erased`). Before, all three dropped them, `--json` too.
- Erasing a person's credentials (`DELETE /people/{principal}/credentials`, the `credentials` scope of `POST /people/{principal}/erasure`, or a SCIM purge) no longer reads as complete when it could not revoke an Azure DevOps token Wardyn created for their runs. When Azure DevOps refuses the revoke for good, the erase goes on, and the answer and the audit row carry `tokens_revocation_failed` and a `detail` saying that token works until it expires; `wardyn person erase` prints it as a warning and exits `1`, the Go SDK reads it with `ErasePersonResult.TokensNotRevoked`, the console's erase dialog shows it, and a credentials erase that fails later keeps the two fields on its failure row. When the revoke does not finish, the erase now erases nothing and answers `503` `credential_erase_revoke_unfinished`, so running it again can still revoke the token; a SCIM purge keeps that step pending, as it does any failed step. Before, both cases erased the sign-in a later revoke needs. `wardyn governance changes approve` now prints the remedy when an approved role-mapping change could not revoke every API token, and the Go SDK's `GovernanceChange` carries `tokens_revocation_failed` and `detail` on that answer.
- An erase of a person's credentials run again, after one that failed or answered `503`, now still carries `tokens_revocation_failed` and its `detail` while a token of their runs whose revoke Azure DevOps refused has not expired, on both erase routes and a SCIM purge's retry; before, the retry answered as complete because the refused token was no longer listed as live. The same holds for a token whose revoke Azure DevOps refused at a disconnect, pause or run end. An erase now leaves a refused Azure DevOps token's record in place while the token can still work, and counts it in `live_left`; after it expires, the Azure DevOps token sweep deletes it. A SCIM purge's `person.deprovision` row now carries `live_left` too. A migration (`0287_ado_signin_ends_erased_at`) adds one column to `ado_signin_ends`; no grant changes.
- Two requests at the last free slot can no longer both pass a cap: a run's 4096 approvals, or a person's 20 live API tokens or 20 SSH keys. The second gets the cap's usual refusal. A run's notification budget of 25 tier-0 rows an hour is now counted per route (with no routes, over all its approvals), so one route's notifications no longer hold back another's. With more than one notification channel, a raise whose tier-0 rows would not all fit in its route's budget now sends none, as a raise past the budget already did. A tier 0 of more than 25 channels is the one exception: a raise on it still reaches all of them when the run has no tier-0 notification rows on that route from the past hour. A launch preset created again while its delete is still finishing gets a version above the deleted one.
- A syslog collector that accepts the connection but stops reading no longer piles up blocked writes in `wardynd`. One write waits on it at a time; while that write is blocked, each later event waits out the write timeout and is then dropped and counted in `wardyn_audit_sink_drops_total{sink="syslog"}`, as a timed-out event already was.
- When the audit `file` sink cannot rotate its log, for example because the log's directory is not writable, it now refuses the event, counts it in `wardyn_audit_sink_drops_total{sink="file"}`, logs the error and tries the rotation again on a later event. Before, it reported the event written and kept appending past `max_bytes`.
- A macOS VM helper log that fails to rotate once now writes again as soon as a rotation works; only the lines written while it could not rotate are lost. Before, one failed rotation left the log closed, and every later line was lost until the helper restarted.
- At startup wardynd now counts the audit spool's backlog and its quarantine file through a fixed buffer instead of reading each file into memory, so a large backlog no longer needs memory of its size before the drain starts.
- The audit spool now syncs its directory at startup, after a compaction replaces the spool file and after it moves a line to `<spool>.quarantine`, so on a filesystem that supports a directory sync a host crash no longer undoes those files' names once that sync has succeeded. If the sync fails during a move, the line stays in the spool and the move is tried again; a failed sync at startup or after a compaction is logged. A spool directory that wardynd has just created is not itself synced into its parent directory.
- Linux VM sandboxes: a run with more than 64 folders and drives is now refused before anything starts. Before, the folder servers past the 64th could stay behind after the run ended or failed to start.
- On Kubernetes, the runner's relay now lets at most 64 connections wait to be matched to a run, and gives each 10 seconds in all for that lookup and its first byte. Before, a Kubernetes API server that did not answer let such connections pile up with no limit.
- macOS VM admission now treats an entry file with a negative disk reservation as damage, which holds a VM slot until it is removed, as an unreadable entry does, and its sums of entries' memory and disk stop at their largest value instead of wrapping. Before, an entry file another local account wrote could make the Mac look as if it had more free disk or memory than it has.
- With `WARDYN_PG_MIGRATE_DSN` set, wardynd now refuses to start when the schema `WARDYN_PG_DSN` reaches records different applied migrations from the schema the migrate role just migrated, for example a restore a newer release migrated, and the refusal names `WARDYN_PG_DSN`. Before, only the migrate role's schema was checked. The app role needs `SELECT` on `schema_migrations`, which the role split in the operations guide grants.
- With `WARDYN_HA`, two replicas starting together for the first time after the upgrade that removed the shared Azure DevOps credentials could delete a person's own Azure DevOps token saved in between. A replica now waits for another one's removal to finish and then skips it.
- Cancel run on a scheduled run, or on one waiting for your sign-in, no longer ends the run when it has started since the page last read it: the console asks `POST /runs/{id}/kill?not_started=true`, the server refuses a started run with `run_kill_started` and ends nothing, and the console says the run started and points to End run. An open Cancel confirm on the Runs list now closes, and says so, when the run starts.
- `wardyn device enrol-token`, `wardyn runner token` and `wardyn runner org-token mint` now fail when the one-time token cannot be written to standard output, and the error names the token's id, when it expires and how to revoke it. Before, they exited 0 with the token stored on the server and held by nobody.
- `wardyn audit export-partition` now fails, and keeps no file at `-o`, when the answer is not a whole export that starts with its manifest line and ends with its footer line, such as the empty answer the server gives a caller who is not a security operator. Before, it wrote what it got and said so.
- `wardyn folder bring-back` now reads a success answer of up to 4 MiB. Before, it read 64 KiB, so a pass with many refused files whose names hold `&`, `<` or `>` lost its receipt after the run had already stopped.
- `wardyn workspace list` and `get`, `wardyn template import` and `wardyn audit verify` now show control characters, line breaks, tabs and text-direction marks in text from another person or a file as escapes, so that text cannot change the screen or add a row.
- `wardyn runner pool create` and `update` now refuse a lifetime or idle value that is not a whole number of seconds. Before, the fraction was dropped after the default was checked against the maximum, so `--background-lifetime 1.9s:1.1s` was saved as one second for both.
- `wardyn window` now exits with the code of the refusal's class (2 for 401 or 403, 3 for another 4xx, 4 for a 5xx) when the server refuses. Before, it printed the server's sentence and exited 1.
- `wardyn run logs` now prints every event of a run when more than 1000 of them were written within 10 seconds. Before, it stopped after 1000.
- `wardyn runner list` now refuses a negative `--offset`. Before, `--no-limits --offset=-1` crashed.
- A long-running `wardyn` command whose CLI sign-in is signed out, or replaced by another `wardyn login` on the same computer, now stops using it when it next renews its access token, instead of carrying on under the new sign-in, which may be another person's, or with none. Its later requests are refused on the computer and not sent; `wardyn run --wait`, `wardyn run wait-ready` and `wardyn apply --wait` stop at their next status poll and exit 1. While the stored sign-in cannot be read, a request is refused in the same way, and a later one goes through once it can be read. A renewal of the same sign-in by another command carries on as before.
- `wardyn runner pool create --json --runner` now prints the pool as one JSON value on standard output and nothing after it; the lines saying which runners were added, refused or held for approval go to standard error, and a refusal still fails the command. Before, they followed the JSON on standard output.
- The Host name that `wardyn run ssh --config` prints and `wardyn run editor` saves, and that the console's SSH card shows, is now `wardyn-` and the run's whole id, not its first eight characters. Before, two runs whose ids started alike replaced each other's saved block, and the older name then reached the newer run. A block already saved under a short name stays in `~/.wardyn/ssh_config` unchanged; running `wardyn run editor` again saves the run under its new name.
- `wardyn sync --pull-commits` now fetches into the branch `wardyn/sync-` and the run's whole id, not its first eight characters, so two runs synced into one repository no longer overwrite each other's branch. `wardyn sync` also uses the run id in its standard lower-case form however it was typed, so `urn:uuid:` or upper-case spellings of one run share its saved sync state and branch. A sync state saved earlier under another spelling is moved to the standard one the next time that spelling is used. A branch fetched earlier under a short name is left as it is.
- A `wardyn` request that is waiting for another command to finish renewing the CLI sign-in now stops when its own deadline or cancellation arrives, such as the one `wardyn run --wait` or `wardyn run wait-ready --timeout` sets, instead of waiting out the 10-second lock wait and then going out with no sign-in. `wardyn run wait-ready --timeout` now counts from before the command reads its sign-in, so that wait is inside it too; before, it could add up to 20 seconds.
- A component saved from an editor opened before someone else saved it no longer writes back what that save changed, such as a removed host or a removed plain-HTTP delivery. The console now sends the version it opened as `expected_version` on `PUT /components/{id}` and `PUT /me/components/{id}`, and the server refuses a save the component has moved past with `409` `component_version_conflict`, before it writes anything or removes a CLI sign-in. Closing the editor reads the list again. A `PUT` without `expected_version` replaces the component as before.
- Editing one of your own saved MCP server components in the console no longer removes its server address and transport.
- A granted organisation MCP server is now listed under MCP in New run's Add tools and access, not under APIs. A remote server's address is shown as its scheme, host and port only.
- Components saved with Save to reuse are now offered in New run's Add tools and access, marked "Saved to your components", so a later run can add them.
- A recording whose later part could not be read is no longer served as a shorter, complete recording: its replay now fails part-way, and a run's output is not recovered from it while that part cannot be read; the recovery stays owed and is tried again later, as for a recording whose first part cannot be read.
- A UI app is now refused at most 30 seconds after its run ended or was kept after an outage; before, its requests kept reaching the app for as long as the server's connection to the app stayed open. A stream that is already open is not re-checked by this change.
- The recorder now also retries its last upload when the server answers 500, within the same 30-second retry window, so a short run's recording is not lost to one failed write of the recording store.
- A desktop clipboard paste is now audited as relayed (`ui.clipboard.transfer` `success`) only once it was written to the desktop; a paste in a message the relay refused, or whose write to the desktop failed, is no longer recorded as relayed.
- A confined workspace replay is no longer finalized as clean while the server that finalizes it still holds one of its audit events in the audit spool after a failed write; the replay stays recording until that spool has drained, and a later read of the workspace finalizes it.
- A desktop recording that ends because the relay refused a pixel format is now marked as stopped there: its replay shows where the screen recording stopped and why, its `desktop.recording.write` audit row carries `stopped_ms`, and the stored recording names the refused format (`stop_reason`). Before, it just ended, with no mark.
- A confined workspace replay already finalized as clean is judged again on the next `GET /workspaces/{id}` when an egress decision of its run was stored after it was finalized, such as one that waited in another server's audit spool. A confined replay whose run is missing an event that an audit spool set aside, because the store kept refusing it, now ends as failed with a hint naming the quarantine file instead of clean: the spool writes an `audit.spool.quarantine` row on the event's run before it sets the event aside, writes it again without the event's action if the store refuses it, and keeps the event only when the store refuses that too. A confined replay whose egress proxy reported decisions it could not deliver now ends as failed too, instead of clean.
- The egress proxy's last reports of a run (its decisions, DNS lookups and model token counts) are now kept for five minutes after the run ends, as its recording and scan uploads already were, even once the run's token is revoked; before, the server refused them, and a decision made just before the end was lost. A confined workspace replay is judged again on the next `GET /workspaces/{id}` when such a decision lands, and one whose run had a report refused at that door with a 403 now ends as failed instead of clean. An open workspace page keeps reading a replay stamped clean for 10 minutes after it finished, so it shows the verdict the server takes back.
- A confined workspace replay now also ends as failed when the server did not keep one of its egress proxy's reports at the decision door: one answered with a 503 because the server could not read the run, or the pending actions of the runner the report was relayed through, with a 400 because the report did not decode, or with a 401 because the run's token had expired, in the last 65 minutes or, for an older one, while the run was not over. The server notes such a report as `run.report.drop`, at most once per run in 65 minutes in each server process. The decision door now answers a decision it could neither store nor queue to store later with a retryable 503 instead of a 202, so the egress proxy counts it as dropped. `GET /workspaces` now judges a clean confined replay again before it lists it, as `GET /workspaces/{id}` does. The egress proxy now also reports the decisions it counted as dropped after its last report while it stopped; on a runner whose proxy still writes the run's spool itself (wardyn-runnerd 0.9.3-rc.2 or older), it now counts the decisions it could no longer write as it stopped, those of requests still open included, into one last summary, and the replay ends as failed. That summary also counts each decision of a request it had sent that the spool had no room for, which the spool counted before as a request refused because it was full, and each deny or hold of a request it had not sent that the spool could neither store nor count as refused. A confined replay also ends as failed when the run's runner reported egress decisions its spool lost (`builtin:spool-lost`). When the egress proxy stops and tells wardyn-runnerd what it could not pass on, that report now also counts each hold it dropped, and each other record it dropped that had no place reserved in the spool, such as a deny of a request it had not sent or a DNS lookup, from the start of the run until the proxy closes. It also ends as failed when the run's spool was full, so that some of its requests were refused before the policy judged them, or a decision was not recorded (`builtin:spool-full`). A Bedrock failure hint on an egress report that arrives after its run ended, through the runner or at the decision door, is written without reopening the five minutes in which the run's last uploads are accepted.
- On a runner, the egress proxy now serves only once wardyn-runnerd's decision sink has accepted it, so a run whose proxy cannot reach that sink now fails to start, and a Docker runner's replacement of a run's proxy fails the same way. Before, such a proxy refused every request without recording it, and a deny it wrote meanwhile, for a request whose target was too long to record, was lost without being counted, so a confined workspace replay of a run whose proxy had been replaced could be judged clean. When the egress proxy stops, it now also ends the connections still open, CONNECT tunnels included, and on a runner it waits, within its stop deadline, for the requests it is still serving before it closes its link to wardyn-runnerd, so a decision they write before that deadline is no longer dropped uncounted because the link had already closed.
- When a Docker run starts, or its egress proxy is replaced on a revive, and the proxy is still running but not yet listening when the wait for it ends, the start or the replacement now fails with a reason that says so and, when it is known, names what the proxy may be waiting for: its first run-token renewal with the organisation, on a replacement, or wardyn-runnerd's decision sink. Before, the reason said the proxy had exited at config load.
- The desktop replay no longer shows a reviewed download's request as a downloaded file. Its playhead now moves with the arrow keys, Home and End, and Play at the end starts again from the beginning.
- After a provider that uses AWS sign-in is moved to another region, a run started on the old region is refused the next time it asks for its AWS session, with no sign-in request; before, it was held for its owner's sign-in and then served the session from that sign-in for the new region. A GitHub App lookup still in flight when the App's credentials rotated no longer leaves the old App's installation cached for the new App. With front-channel logout on, an older sign-in that records last no longer shortens how long the identity provider's logout can find the session, and a logout no longer deletes the record a newer sign-in under the same provider session wrote.
- A stored AWS sign-in for another region than its provider's, which a sign-in never stores (one can remain after a secret store is restored apart from the database), now reads as not signed in: a run on it is refused before it starts, and the provider's sign-in status asks for a new sign-in; before, the run started and was refused the first time it asked for its AWS session. Moving a provider that uses AWS sign-in to another region or pinning it to another account or role, deleting it or changing its kind now also ends, as expired, every sign-in request still pending for it that no sign-in can answer any more when the change is saved, and one raised while the change was being saved; before, such a request stayed pending until its run ended or the request aged out.
- The run document editor now reads a document once to put the server's problems on their lines, where before it read the whole document again for each one. The run and policy document editors now check a mapping for a repeated key by reading each key once, where before they compared each key with every earlier one; a document that has a repeated key and another fault is still checked the old way. With thousands of keys, the page could stop responding while they did.
- `wardyn doctor` now holds the organisation's certificate to its address when `WARDYN_URL` is an IP address, and sends no token when the certificate does not cover it; before, a certificate from a trusted CA passed whatever name it carried, and the token went to whatever answered. It also trusts the CAs `wardyn ca add` stored for the organisation's host, as every other command does, and names a stored `wardyn login` sign-in as the credential it checked, with `wardyn login` as the fix when that sign-in is refused. `wardyn-runnerd setup --claim` now claims through the trust and route its registration used, `wardyn runner setup` says it could not confirm the connection, instead of blaming the runner, when it cannot read the runner back, and rerunning local setup keeps the log of a wardynd it leaves running.
- A starting run could mount another person's drive storage if an administrator reclaimed the run's drive object and another person's first run then made one under the same name. Only two drives created before migration `0067` whose names collide, or a `home_override` given to a second person, name one object for two people. The Kubernetes runner now reads each drive claim again before the agent pod is made and once its main container runs, and refuses a claim that is gone, being deleted or replaced. The Docker runner holds each drive volume from its check to the run's teardown, so a reclaim in between answers that it is in use. It refuses a volume whose labels changed before the hold. `POST /drives/{id}/reclaim` answers `409` while one of that person's runs is starting. A container under a run's drive seed name that is not that run's is no longer removed; the run is refused.
- A person's bring-back of a folder from their computer, for a container run on a runner, no longer loses the folder's changes when the server shuts down after the run has ended and before the copy back starts. The agent is still stopped, the sandbox is kept, and Wardyn tries the copy back again on its next check. Before, the folder's record said the agent could not be stopped, and the sandbox was stopped with the changes still in it. This does not cover Kubernetes in server mode: the sandbox is kept at the end the same way, but a background sweep can stop it before that copy back.
- An environment build now fails when `WARDYN_ENVBUILD_BUILD_MEMORY_MB` or `WARDYN_ENVBUILD_MAX_CONTEXT_MB` is too large to count in bytes, or `WARDYN_ENVBUILD_BUILD_CPUS` is NaN, infinite or a positive value under 0.000000001; before, such a value was accepted: an over-large size or a positive CPU value under 0.000000001 removed the limit, and one too small to represent fell back to the default.
- An environment build whose wait for the build container fails now ends the log follow at once and tries to remove the container before returning; before, it waited for the log stream, up to the build's time limit (30 minutes by default).
- The env-as-code export (`GET /workspaces/{id}/env-as-code` and `POST /workspaces/{id}/env-as-code/write`) now points pip and NuGet at the operator's artifact mirrors when the project is opened outside Wardyn: its `devcontainer.json` sets `PIP_INDEX_URL` to the pip mirror, and the NuGet config is written as `NuGet.Config` at the project root. Before, those settings sat in files in the project where pip and NuGet do not read them. Maven does not read the project's `.m2/settings.xml` by itself; the exported `AGENTS.md` now says how to pass it. The write keeps a file the project already has under the same name in another letter case, such as `nuget.config`, and reports the exported one as skipped instead of writing it beside it. A `.config/pip/pip.conf` or `.nuget/NuGet/NuGet.Config` that an earlier export wrote was never read and can be deleted. A `devcontainer.json` from an earlier export is kept as it is; delete it before writing again to get the new setting.
- On the Governance screen, a profile built on the deployment ceiling now shows the Default unit's values as what it inherits. Before, it showed the signed-in person's own ceiling, which their own profile can narrow, while the saved profile inherits the Default unit's. A unit admin who opens a unit whose parent is outside their scope now sees that parent's values as inherited, instead of none.
- `GET /governance/changes` now applies its 500-row bound to the pending changes the caller may see, after leaving out the ones they may not, and `GET /governance/requests` returns every pending request that has not expired. Before, an older pending change or request could be left out behind 500 newer ones the caller could not see, and the console then said nothing was waiting. The Governance Changes tab now says when the server cut that list at 500 changes.
- A bring-your-own base image is now wrapped from the image ID its pre-wrap check inspected, so a tag moved in between cannot swap in an image that was never checked; the repository digest is still recorded when the host has one. A Linux VM run no longer starts its guest image in a host container to learn its architecture: the image's metadata decides, and an image that is not `linux/amd64` is refused before any container is made from it. The VM conversion now exports the inspected image by its ID, and conversions an earlier release cached are not reused. On its Docker engine, wardynd now removes a workspace image that a rescan, refresh, edit or delete replaced, unless a container still uses it or a run launch on the same server has selected it and not yet created its sandbox; a refresh whose image could not be recorded keeps its image.
- On Approvals, an approved image request under Your requests now says who approved it and when. It no longer says the image can be launched now, which was wrong once an admin deleted the grant.
- `GET /governance/requests` answers `500` when an approved elevation's membership cannot be read. Before, it listed the request with no end, and Your requests showed it as ended at its approval. An approved elevation whose membership a unit admin made permanent now reads as approved, with who approved it and when, instead of ended.
- A run could stay frozen while it read as not paused, when the server stopped after an idle or waiting pause froze the run and before it marked the run paused. Resume then answered that the run was not paused and thawed nothing. Wardyn now records each pause before it freezes the run, so the pause sweep sets such a run right, and Resume, or opening a terminal, SSH or the desktop on the run, thaws it first.
- A pause or resume whose call to the container runtime was cut at its time limit, and took effect only after the pause sweep had checked the run, could leave the run frozen while it read as not paused, or running while it read as paused. The pause sweep now checks such a run again on every pass until at least two minutes after the cut, on whichever server runs the sweep, so a call that takes effect in that time is set right, even across a server restart.
- A run event stream that cannot deliver, such as to an HTTP/2 client that never opens its flow-control window, now gives up after 30 seconds and frees its place under the limit of 32 open streams per caller; before, it held that place until the connection closed. The image check's registry token cache now holds at most 1,024 entries and drops expired ones, instead of growing with every repository checked. The `run_secret` sweep's success on `/setup/status` and `wardyn_sweep_last_tick_seconds` now comes only from the replica that holds the sweeper lock, so a leader that fails every pass reads stale instead of healthy.
- The `run_watcher` sweep's success on `/setup/status` and `wardyn_sweep_last_tick_seconds` now comes only from the replica that holds the sweeper lock, as `run_secret`'s does, so a leader whose tick fails, or that stops ticking, reads stale instead of healthy; before, any other replica's tick kept it healthy.
- `wardyn attach` on a run of your own runner now waits up to 2 seconds for the runner's last output to reach your terminal when the session ends without the organisation stopping it, instead of ending as soon as the organisation's connection closes; before, output the runner had already sent could go unshown. When that wait runs out and nothing else has ended the command first, it exits with an error that says the output may be incomplete.
- `wardyn attach` on a run of your own runner now detaches with exit status 0 when the runner reports that the shell exited or the run ended and the organisation did not stop the session; before, the command could exit with an error ending in `failed to read frame header: EOF`.
- The documented recovery for a lost ephemeral age key now lets wardynd start when per-person keys were wrapped under that key: its statement, in the boot refusal and in OPERATIONS.md, also marks those keys destroyed, as an erasure does, and deletes the credentials sealed under them. Before, those keys stayed live and wardynd still refused to start. Rows and per-person keys under a key service or a declared key domain are left as they are. Sealed audit fields under those keys then read `[erased]`, but audit rows still show the people they name.
- macOS disk drives: a run, or `wardyn-runnerd drive open`, could format an existing disk drive whose image had come to start with 4 KiB of zeros, and erase what it held. The guest formats a disk that starts that way, which is meant only for a new drive. The runner now writes a mark beside each disk image it makes, and lets a guest see an image that starts with 4 KiB of zeros only while that image's mark is there; the mark goes once a run or a drive open has had the drive mounted. Any other image that starts with 4 KiB of zeros, and any image shorter than 4 KiB, is refused before the VM starts, with a message that says Wardyn did not format it and how to start again with an empty drive. A drive that an earlier runner made and no run has formatted yet holds no files and is refused the same way: delete it and create a new one. The guest agent also no longer formats a disk it could not read 4 KiB of; run the tools update once to get it. Not yet tried on a real Mac.
- macOS VMs: the check that a file in the runner's folders belongs to the runner's own user passed every file, so it never refused one. A lock file, an existing disk image, a drive opened on the Mac, and a kept machine's disk, auxiliary storage and machine id file are now refused when another user owns them or their owner cannot be read. A file left by a `sudo` command must be given back to the runner's user before it can be used. Not yet tried on a real Mac.
- Folder sync, in `wardyn sync --pull` and in a runner's pass back, no longer writes over a file on the laptop that was saved after the pass last checked it, in the moment before the pass put the sandbox's version in place. On Linux and macOS the new version now goes in by a swap: a save found in what it displaced is put back, and the file is listed as a conflict, with the sandbox's copy kept beside it on a runner's pass back. A file deleted on the laptop in that moment stays deleted and is listed as a conflict. If a save cannot be put back, it is kept under a name of its own and the pass reports the name. A file that appears on the laptop in that moment is never written over. With `--pull-deletes`, a sandbox delete now moves the laptop file aside before it looks at it, so a save made after the pass checked the file is put back and listed as a conflict instead of deleted. On Windows, and on a filesystem that does not offer the swap, a save in that moment can still be replaced by a pull.
- Folder sync, on a laptop filesystem without hard links: when a pass copies a file into a name on the laptop and the copy fails part-way, it now removes only its own partial copy, never a file that replaced it at that name in the meantime. A file that replaces it at that name while the copy is made keeps its own modification time and is no longer recorded as in step with the sandbox.
- A run's or a governance unit's model usage no longer fails to load, and `wardyn_model_tokens_total` no longer turns negative, when token counts add up past 9,223,372,036,854,775,807. A model line whose counts add up past that, or would take a total of the view past it, now shows its calls as not counted, reason `count_out_of_range`, and a count that would take a token series past it is left out of that series.
- When output arrives after a run's final output row is written, the mark that labels the row incomplete is now retried, for as long as the server stays up, until it is written or the output is erased; before, one failed write left the row labelled complete for good.
- When a server takes over the output of a run whose first server stopped, a failed read of the output that server had already saved no longer deletes it; the run's output stays pending and a later pass writes it.
- When database errors kept a run's pending output record and then its final output row from being written, and the server then stopped, the output kept from the run is now recovered into its output row, marked incomplete, and the retention sweep deletes that row as usual; before, the output stayed readable past `WARDYN_RUN_OUTPUT_RETENTION_DAYS`. Output an earlier version left this way is recovered by the run output sweep when its run ended inside that window, and is then kept for a full window from its recovery, as a recording-derived row is; when its run ended longer ago, it is deleted and no longer served.
- Turning one enforcement switch on or off on the Permissions screen could switch back another one that a second admin had changed after the page loaded. That save is now refused; the page reloads the switches, and the change can be confirmed again.
- A run starting from a runner pool's queue whose start briefly stopped renewing its lease is no longer failed by the stale-start sweep when the renewal lands before the sweep acts on it; only a start whose lease is still stale at that moment is failed.
- An audit receipt for an event whose signed checkpoint no longer matches the stored rows, such as a checkpoint that also covered a month that audit retention then dropped, is now refused with `409` `audit_receipt_proof_unavailable` and audited as a failure. Before, the server returned a receipt that `wardyn audit verify` rejects.
- The audit checkpoint signer now resumes when audit retention dropped rows it had not signed yet: the next checkpoint starts at the first retained row, which must chain to the drop's recorded tail. Before, it reported a chain break and stopped signing, again after every restart.
- A desktop recording that just ended when the relay refused something other than a pixel format (a message or a connection setup it cannot follow, a WebSocket frame it cannot read, or a control message on a view-only desktop) is now marked as stopped there too: its replay shows where the screen recording stopped and why, its `desktop.recording.write` audit row carries `stopped_ms`, and the stored recording says in fixed words what the relay refused and whether the browser or the desktop sent it (`stop_reason`). On a container desktop, the relay now also refuses a frame from the browser that breaks the WebSocket protocol. Most of these it used to pass on to the desktop, and when the desktop closed the connection on one, the recording got no mark.
- A run's Recording tab now lists a desktop's saved screen recordings when its terminal recordings could not be listed, keeps the recordings it already listed when a later read fails, and lists a saved screen recording even when the desktop's pause and resume audit rows fill the first 1000 rows. When a list of the run's recordings could not be read, or filled its 1000-row page, the tab says some may be missing, with Try again after a failed read.
- The Recordings page no longer lets a page that answers late land after Refresh; before, it could add rows from the older list and make the next Load more skip runs.
- `wardynd -rotate-age-key` no longer deletes the new key file (`<key-file>.new`) when the rewrap returns an error. If the commit had landed and only its reply was lost, that file was the only key that could open the rewrapped secrets. The error now prints the new key's fingerprint: a row whose `kek_id` ends in it proves the rotation landed, and no message or runbook step says to delete that file. A failed commit in `-rotate-age-key` or `-rewrap` now says its outcome is unknown, not that nothing was committed.
- When a commit returns an error, `wardynd -rewrap` and `-rewrap-principal-keys` now record their `secret.rewrap` audit event, and `-migrate-secrets` its `secret.migrate` event, with reason `unknown` instead of `aborted`, and the boot's conversion of pre-envelope secrets no longer says nothing was committed. A commit error does not say whether the commit took effect.
- `wardynd -rewrap` and the key rotation steps in the secrets and keys guide now say that an older key version is retirable only after a `-rewrap` that started once every wardynd process running at the rotation had exited; the Transit and Key Vault steps restart every replica before that run. A write that sealed its row before the rotation could land under the older version after the run reported 0 rows; that row was then refused until the version was enabled again.
- A failed pick from the Ends or Waits menu in a run's header now stays on screen under the menus, which stay there to try again; before, the error showed only in a passing notification.
- On a runner, a command sent with `POST /runs/{id}/exec` just as the runner's connection dropped was answered as failed, yet could still start on the runner when it reconnected. A reconnect that would deliver such a request now starts a new session instead of resuming, so the request is dropped. An exec, terminal or desktop connection cancelled just as the runner answered no longer holds stream slots on that connection until the runner reconnects.
- On a Docker runner, a proxy stop, end or proxy replacement refused because the run's proxy belongs to a newer policy generation no longer makes the runner stop that newer proxy afterwards.
- A run scheduled for later no longer starts at once when scheduled runs are turned off while its Name dialog is open: the Run step, Review and the dialog say it is held, with the reason, Launch waits, and the run starts now only when you choose Run now. A start time that the clocks skip or repeat on a daylight-saving change is now refused, in the console and by `POST /runs` (`start_at_invalid` with `detail` `clock_change`), instead of being moved to another time; an RFC 3339 `start_at` with its UTC offset still names one occurrence of a repeated hour.
- `GET /api/v1/scim/status` now reports a purge delay under one second as `purge_after_seconds: 1` instead of 0, which the console showed as off while the purge still ran.
- Go SDK: the new `WorkloadHistoryPage` also returns the server's `X-Wardyn-Truncated` signal, which `WorkloadHistory` drops; with no limit given, a history longer than 200 events comes back as one page of 200 events with no sign that more exist.
- Drives, People's drives: the confirm for deleting a disk drive on a Mac now says that the disk stays on its Mac if the Mac is no longer registered, and after such a delete it says that only the record was removed; before, it said the drive's files were deleted and closed. The confirm can no longer be closed while its delete is in flight. The confirm a person gets for deleting their own disk drive on a Mac, in the Add dialog of a new run, now says the same before the click.
- `wardynd -reconcile` run beside a serving wardynd could report a secret that was being saved or deleted at that moment as a pointer without a value or a value without a pointer, and exit with an error. It now checks each such finding again once any save or delete of that secret under way has finished, and reports it only if it still holds.
- `wardyn sync --git` into a sandbox directory with no repository now fails when setting up the repository fails, and the next `--git` carries on setting it up, as it does after a send cut off partway; before, a failed step was reported as sent and never retried, and a cut send left a `.git` the next send took for a finished repository. Commits made on a branch in such a repository in the sandbox stay on it, and `--pull-commits` brings back those on its `HEAD`, as for any repository. An empty `.git` directory now gets the new repository; a `.git` that is a link to nothing, or a directory that is not a repository, is left as it is and refused. Before, git set a repository up where the link pointed, or used one in a directory above when there was one. An SFTP session whose `sftp-server` exits nonzero, and an `ssh -L` forward whose relay exits nonzero (such as a forward to a closed port), are now audited as `failure` with the exit in `error`, as `wardyn sync` sessions already were, instead of `success`. The organisation audit export no longer fails on a held push's `approval.push_paths.record` row forwarded by an enrolled device: that row's path list stays on the device, and the export marks it `paths_unavailable` `held_on_device`.
- A Kubernetes kernel event that loses its run because it arrived more than 15 minutes after the run ended is now masked with that run's secrets, while the server still holds them. Before, a secret registered only for that run could be stored in plain text in the event's command line.
- A registered secret that contains a space is now masked in a kernel exec event's command line. Before, the sensor split the command line into words first, and the words of the secret were stored in plain text. The server now splits the command line only after it has read the shared list of registered secrets and loaded the secrets the run was started with; when it cannot do both, it stores `<secret-hidden>` in place of the arguments.
- The kernel sensor now records only the probes it understands: a connect from a connect hook, and a write from `security_file_permission` that the kernel allowed to a sensitive path. Before, a read or open of a sensitive file in your own Tetragon policy was recorded as a write, a write the kernel refused as one that happened, and any probe that carried a socket as a connect.
- After someone signs out on a console page, the next sign-in there, or one in another tab while that page shows the sign-in screen, now loads that page fresh at Runs, so nothing the first person's page had started carries on into the next person's console. Before, a model-access status read the first person started could answer on the next person's page and show the first person's providers and their sign-in state. A status read that answers after any other sign-in change is now read again.
- A token sign-in, at the sign-in screen after an SSO sign-out or in the sign-in prompt after a session ends, no longer carries the earlier session's expiry, so the expiry warning no longer appears for the token session. A read the first session started that is refused after the next sign-in no longer opens the sign-in prompt or holds the new session's saves.
- These now send every later request in the name of the person who started them: saving a new policy, model provider or image together with its Available to list; the first Add under Only these; saving a drive with its team groups; Restart with current limits; sending several files to a run's desktop; removing an allowed host; approving a caught host; Extend and revive; a template save sent again with what needs setup; renaming a template; Switch to user view at the end of setup; a run layout saved behind another or just after a drag; and opening a shared app. If someone else has signed in by then, in this tab or another, the server refuses those requests instead of making them as the new person; what was already done stays. A save that waits while the page checks who is signed in after a change in another tab now names the person who made it, not whoever is confirmed when it leaves.
- Signing out of the console now closes its notices that a run needs you in the tab you signed out in, and a notice that was still being prepared when you signed out is no longer shown afterwards; before, they stayed up on the sign-in page. Another open tab of the console keeps showing its page, with any such notices, along with its sign-in prompt.
- Saved run views are now kept separately for each person at each console address, in this browser; views saved before this change are removed the first time the Runs page loads, and are not carried over.
- The desktop viewer now stops when it can't read what the desktop sent, shows the reason and offers Try again; before, it hid the reason and reconnected again and again.
- `wardyn drive member delete` now says before you confirm that a Mac disk image may stay: the outcome is `deferred` while the Mac can't be reached and `record_only` when the Mac is no longer registered.
- A download from a desktop's Files panel and an audit partition export now release the downloaded file's browser link on the next tick, as recording downloads already did, instead of right after the click.
- A local runner's spool records that the server refuses now keep the run's replay receipt to the same last 64 records as the records it takes; before, each refused record added one more entry to the receipt, and the server rewrote the whole receipt for every one.
- `wardyn needs-you` now returns when the person hands control back: the sandbox's read of its request carries `handed_back_at`, and still not who held control or decided.
- Handing back a takeover now wakes a run that was paused for being idle while the person had control, and the pause sweep wakes it if that did not run. A hand back that lands while the sweep is pausing the run leaves it running.
- A takeover whose desktop did not open now says the person has control and keeps Hand back, with an Open the desktop button to try again, instead of saying the takeover failed.
- After a hand back, a new takeover request on the same run page now shows its Take over instead of the finished banner.
- A copy offer already showing in a run's terminal when the page learns that the run keeps its content in now closes, and its Copy copies nothing. Before, it still copied under the line saying copying out of this terminal is off. When the page cannot read the run's `data_out` rule, the terminal now keeps copying off and says it could not read the rule; before, a failed read left copying on.
- Your account, Your API tokens: without the API token feature, the card now hides its Mint button and keeps listing your tokens with their Revoke buttons; with no token to list, the card stays hidden. Before, the whole card disappeared, though the server still lists and revokes those tokens.
- Admin > Credentials, Tokens an admin created for someone else: a token is no longer counted as one that still works, and reads Refused, when authentication refuses it because its owner is deactivated, because a revocation of its owner's credentials came after it was made, because its owner's name is reserved, or, with `WARDYN_ROLE_STAMP_TTL` set, because its role stamp is older than that. The list asks the same check authentication runs on a person's token. `GET /api/v1/tokens?minted_for_others=true` now leaves out every such token, as it already did a deactivated owner's; with `&include_revoked=true` they are listed with `owner_refused: true`.
- A finished run's page now keeps refreshing while the changes in its Folders from your computer card are kept for their copy back or were given up with the sandbox's stop not yet confirmed, and for up to two minutes while that stop is confirmed, so the card shows the outcome without a reload.
- When the server's setup status cannot be read, New Run no longer says your organisation doesn't allow folders from personal computers, and no longer leaves Add a harness disabled with no reason; it says it couldn't check, with Try again; the run document's Load example no longer says to choose a harness.
- A model call sent to a gateway with run identity on as a plain proxy request, not through a tunnel, now carries the run's identity and call id like the gateway's other calls, or is not sent when the identity cannot be had. Affects 0.10.0 prereleases only.
- The UI relay no longer passes the common caller-address and front-proxy sign-in headers, such as `X-Real-IP` or any `X-Forwarded-*`, to the app in the sandbox. An inbound app still gets `X-Wardyn-Caller`.
- With `push_rules.max_file_size_mib` set, an Azure DevOps REST push that renames a file, or makes any change other than a delete without carrying its new content, is now refused (`403`, `brokered:git:push-rules`), because the size of that content cannot be checked. It used to be forwarded with no size check.
- macOS VM runs: a guest agent that restarts after the run's setup now refuses each command with "the guest agent restarted and lost the run's environment; end this run and start a new one", as a Linux VM's agent does. Before, such a command ran without the run's proxy settings, CA or secrets. Run the tools update once to get the new guest agent. Not yet tried on a real Mac.
- Force-deleting a library source or a base image now names every workspace it detached in its audit row, and a source delete also in its response, including one that attached while the delete ran, and a forced delete that fails leaves every workspace as it was. A source or workspace whose scan or recording stopped before its run was written no longer stays busy for good: a later cleanup pass frees it and marks that recording failed. The owner of a workspace that attaches a shared source no longer reads, on the workspace or in its setup checklist, the secret names and internal hosts an administrator set on that source, unless a scan of the source found them or the workspace lists them itself.
- `wardyn walls set -f` and `wardyn walls presets set -f` now refuse a file that holds more than one YAML document, or text after the first that does not parse. Before, they read only the first document and saved it as the whole walls, without the limits after it.
- Most API routes, `PUT /api/v1/me/walls` among them, now refuse a JSON request body that holds a second value, or other text, after the first value, or that writes a key twice in one object. Before, they read the first value and ignored the rest, and kept only the last of a repeated key, so a body of `{}` followed by the intended walls replaced the stored walls with none. The `--default-policy` file wardynd reads at start is held to the same rules.
- `wardyn template save --name` and `--description` now work on a document that has its own name or description: the flag's value replaces the document's. Before, the server refused the save because the two differed. `--description ""` clears the description.
- A template with a key written twice in one object, such as `denied_domains` twice under `inline_policy` or `intent` twice, is now refused when it is saved, as its import already was. Before, the save kept only the last value. The Go SDK refuses a repeated key when it reads a template's intent.
- `wardyn governance deciders set --file` and the `--limits-file` of `wardyn runner pool create` and `update` now refuse a file that holds a second value, or other text, after the first. Before, they sent the first value, so `{"rules":[]}` followed by the intended rules cleared the unit's rules. These commands, `wardyn template save` and the other commands that read a JSON file the same way now also refuse a key written twice in one object.
- A JSON request body, a template document, the `--default-policy` file, the policy files of `wardyn policy set`, `wardyn policy render` and `wardyn run --policy-file`, and the other JSON files the CLI reads are now refused when they write one field twice in two letter cases, such as `denied_domains` and `Denied_Domains` (or `DENIED_DOMAINſ`, with a long s). Before, both keys were read into that one field and the last was kept, or two objects were merged, so the second spelling could empty a deny list. A key in another case on its own is still read: `PUT /api/v1/site-config` and `PUT /api/v1/runner-pool-defaults` now save it instead of keeping the stored value, and a change to a run's end or wait now reads it instead of answering that neither field was set.
- The approve and deny routes, the decision log the egress proxy sends, template import, the governance dry run, `PUT /api/v1/branding/settings` and other routes that read a request body on their own now refuse a second value after the first, and a key written twice, as the rest of the API does. An approve or deny with no body is still accepted.
- The device audit ingest route and the sensor's ground-truth route now count a batch's rows before decoding them, and answer a batch over the row limit with 413 at once, before its rows' fields are read. A body that is not one well-formed JSON value, or that writes a key twice, is still answered with 400 first. Before, the whole batch was decoded first, so a megabyte of empty rows took hundreds of megabytes of memory before it was refused.
- A Kubernetes runner started with `--upstream-proxy` (or `WARDYN_UPSTREAM_PROXY`, or `upstream_proxy` in `runnerd.json`) now chains its runs' egress through that proxy, as a Docker runner does; before, it checked the setting at startup and every run's egress went out direct. The run's proxy also refuses a non-standard spelling of an IP address, such as `134744072` for `8.8.8.8`, when that address is on `denied_domains`, denied by a live egress edit, or in a Kubernetes runner's in-cluster hard deny. Before, such a spelling passed those denies, so under `allow_all_egress` without `refuse_ip_literals`, or once approved, it was sent to a configured upstream proxy as written, and an upstream proxy that reads it as an address could connect to the denied one.
- A request sent through the organisation (`via_org`) to a model host that `allowed_models` checks no longer waits for good on an upload that stops arriving: its body now has the 5-minute read bound a request through the run's own proxy has, so the request ends and a run end or idle release no longer waits on it past that bound. Git push inspection also reads a commit's or annotated tag's headers in place, so a pushed object made of millions of short header lines no longer takes several times its own size in proxy memory.
- The run's proxy now refuses, on every run and whatever the policy allows, a host that writes an address in a non-standard spelling: all numbers, such as `134744072` or `0x8080808` for `8.8.8.8`, `010.0.0.1`, `008.008.008.008` or `0x`; or an IPv6 address with a zone. Its audit row's `rule_source` is `builtin:ip-spelling`, and the 403 says: "this host is written as numbers, as an IP address in a non-standard form, or with a colon or a percent sign, which the proxy refuses on every run whatever the policy allows, because programs differ on which address such a spelling names. Write an address in standard form: four decimal numbers from 0 to 255 with no leading zeros, or an IPv6 address with no zone." Before, under `allow_all_egress`, such a spelling of an address nothing blocked was allowed past the host rules, the MCP server and local model refusals and the other checks keyed on the standard spelling, and was sent as written to a configured upstream proxy. An IP address configured for a local model, a kubectl door, a git PAT API grant, an Azure gate, an Azure DevOps grant or a `via_org` route now also matches every standard spelling of it, such as `::ffff:1.2.3.4` for `1.2.3.4`. A `via_org` grant stored in another spelling than the request's is refused by the organisation instead of going out direct.
- The run's proxy now refuses, on every run and whatever the policy allows, a host with a character outside ASCII, such as `ｅｘａｍｐｌｅ.com`, `８.８.８.８` or `8.8.8.8。`. Its audit row's `rule_source` is `builtin:non-ascii-host`, and the 403 says: "this host has a character outside ASCII, which the proxy refuses on every run whatever the policy allows, because programs differ on which name or address such a host names. Send the name in its ASCII form: an internationalised name as its xn-- form, an IP address in standard form." Before, such a host was checked as written while programs map it differently: under `allow_all_egress`, `ｅｘａｍｐｌｅ.com` passed a `denied_domains` entry for `example.com`, which a program that maps the name reads. Upgrade cost: a client that sends a raw non-ASCII name is refused, where it used to be passed on; send the name's `xn--` form, which the lists match.
- A destination written as an all-numeric IP address in a non-standard spelling, such as `134744072`, `0x8080808` or `010.0.0.1`, is now refused where it is written: in `allowed_domains` or `denied_domains`, a host rule's `host`, an MCP server's URL, or a live egress edit. An MCP server's URL now also refuses a host with a character outside ASCII, such as `bücher.de`, or with a percent sign, such as `1.2.3.4%eth0`, which the other places already refused; every one of them already refused an address written with a colon in a non-standard spelling, such as `::ffff:010.0.0.1`, or with a zone, such as `2001:db8::1%eth0`. The proxy now refuses every request in such a spelling, and the host in its standard spelling is another key, so such an entry matches nothing; a deny written that way never denied the host in its standard spelling. Upgrade cost: a run whose stored policy or MCP server holds an entry in any of these spellings, or whose live egress edit denied one, no longer starts, and its revive is refused. The proxy logs a warning naming each such entry, and the refusal names each with its standard form, or its ASCII form (`xn--bcher-kva.de`), such as `config: policy.denied_domains[0]: "134744072" writes an IP address in a non-standard spelling, which the proxy refuses in every request, so the entry matches none; its standard form is 8.8.8.8`. Rewrite the entry in that form. A run already going keeps its proxy, but the control plane's own loads of its stored config are refused too: a `via_org` stream, a live egress edit, and a later end for the run. A service run's lease is then not renewed, so the run ends at its lease. On a Kubernetes server, a proxy that restarts runs the image its pod names: if that is a tag since moved to this release, the proxy refuses its config or its live deny list, does not come back, and the run has no network.
- Every other host a run's proxy config names is now held to the same rule. A write refuses such a spelling in a git PAT grant's host, an `api_key` grant's host, a kubectl component's server, a model provider's base URL (a local model's and Bedrock's included), and the site's `internal_hosts`, upstream proxy URL, `upstream_proxy_no_proxy`, egress redirects and `scm_hosts`. A stored one stops the run at proxy load, and the refusal names the config field, the entry and its standard or ASCII form, such as `config: pat_grants["134744072"]: "134744072" writes an IP address in a non-standard spelling, which programs read differently, so the proxy does not dial it; its standard form is 8.8.8.8`. An upstream proxy URL held in a secret, which no write sees, stops the run the same way: the run's proxy is given it and refuses it, where an upstream proxy URL refused for any other reason is still left out and the run goes direct. Two entries of one config table that name one host, such as `git.example.com` and `git.example.com.` among the git PAT grants, or two local model URLs for one host and port, now stop the run at load too, naming both. Before, a model table kept one of them, and which one could differ from one start to the next; the git PAT tables held both, each found only under its own spelling. A policy write now refuses two git PAT grants for one host written two ways. The git broker now finds a run's Azure DevOps grant under every standard spelling of its host's address, such as `::ffff:1.2.3.11` for `1.2.3.11`, where it refused the request as a host not granted to the run, and holds a push that names the host in any spelling to that host's own push rules; before, a push naming `ado.corp.example.` was held to the run's base rules alone. Upgrade cost: a run whose stored grants, model providers, kubectl components or site settings hold such a host, or two such entries, no longer starts, and its revive is refused; a site-wide value stops every run that carries it, an upstream proxy URL held in a secret included. `wardynd` does not start when its default policy file or its site config seed file holds such a host, or when its default policy file holds two git PAT grants for one host written two ways, and the error names the file, the field and each entry. A write of a workspace's approved or denied egress, an `egress:` requirement, an approval rule's hosts or the egress baseline refuses such a spelling too, such as `010.0.0.1`; each replaces its whole list, so one that still holds such a host is refused until the host is left out. So is a workspace's repository source whose URL names such a host, such as `https://010.0.0.1/org/app.git`, and an approval decided `always` for such a host, which can be decided `run` or `once` instead; neither a recording nor a workspace's observed egress offers one any more. A runner started with such an upstream proxy refuses it at startup. A live egress edit on a run whose stored config does not load now answers `409 revive_config_does_not_load` with the load error, which names the entry, as a revive does; before, it answered `503 revive_config_unreadable`, and only the server log named the entry.
- A request sent through the organisation (`via_org`), to a model host or to the git broker, whose upload has stopped arriving now ends as soon as a run end, a release or an idle release cancels it, instead of holding its inspection memory and the run's connection until the upload's 5-minute read bound runs out.
- Git push inspection now refuses a commit with more than 1024 parents with a 413: "gitpack: the push exceeds an inspection ceiling: a commit with more than 1024 parents", and the remedy "merge fewer branches in one commit". Before, a 32 MiB commit made of parent lines took more than four times its size in proxy memory to read.
- `GET /drives` now orders allocations that tie on tier, priority and subject, such as one subject's several drives at one priority, by allocation id. Before, reading it page by page, as `wardyn drive get` does, could return one such allocation twice and leave out another. `wardyn drive get` also refuses a read in which an allocation comes back twice or the allocation count changes between pages, which an allocation added, removed or re-prioritised during the read can cause.
- New Run: a run document, a Run again draft or a template that states its run mode only in the older fields (`agent`, `task`, `interactive`, `interactive_start`, `task_mode`) now opens in Guided setup and Review in the run-mode fields when they can say the same run, so it can be launched; before, those fields were refused at launch and no Guided step could remove them. A run document or Run again draft with no `interactive` is read as a background run, as `wardyn run --file` reads it; a template with no `interactive` has chosen no run mode. A template that leaves its task, or its agent and task, for the person opens the same way, and a `needs_setup` entry that names an older field is met by the run-mode field that replaces it. Anything else is kept as written, to be changed in the document.
- New Run: a template that leaves its run mode to the person but names its agent or task in the older fields (`agent`, `task`, `interactive_start`, `task_mode`) is now put in the run-mode fields when the person chooses its shape or run mode in Guided setup, when they can say the same run; before, those fields were refused at launch once Guided setup had named the run mode. Such an agent task that gives a task starts as Autonomous on the Run step. A task in an environment is kept as written, since only the person can say whether it is a first prompt or a command.
- New Run: switching an agent task between Autonomous and Interactive on the Run step now keeps the task and the first message. The task becomes the first message of the harness it belongs to, which Interactive starts in, and that message becomes the task again on the way back; a harness the draft already starts in keeps its first message. Before, each switch cleared it, and choosing Interactive started the first harness in place of the one the draft started in. Text Interactive has no place for (a task written before any harness is in the draft, such as one from the Runs composer, a task on a virtual machine, another harness's own task, or a task for Your own tools, which starts no CLI) is never put in the run: a switch back to Autonomous gives it back, and the harness it belongs to, or the first one started when it was written before any harness, takes it as its first message when it is switched on, or added while the run starts in none. Your own tools takes no first message, and no harness is switched on on a virtual machine. On a virtual machine, choosing Interactive now starts no harness; before, it started one, which the server refuses. Choosing a virtual machine for an Interactive run that starts in a harness now holds that harness's first message the same way; before, it was cleared with the start.
- New Run: an agent-task template for a virtual machine that starts in a harness and names no `workload` now opens without that start, which the server refuses on a virtual machine, and its first message is held beside the draft as text Interactive has no place for is: a switch to Autonomous gives it back. Before, the start stayed in the draft until the machine kind or the run mode was changed, and when the template named no run mode its first message was cleared on the Run step. Swapping a harness on its card for another now moves the text held for it to the new harness, when none is held for that one.
- New Run: Save as template now says when a virtual machine draft was opened without the harness start its template or run names, since that start and its first message are not saved with the new template; before, the new template lacked them with no word.
- New Run: an agent-task template for a virtual machine that starts in a harness, names no `workload` and is already Autonomous now opens with that start's first message as its task instead of holding it. Before, the start stayed in the draft, which the server refuses on a background run, and switching the run mode cleared its first message.
- New Run: a background run in the older run-mode fields whose apps are all inbound is now put in the run-mode fields when it can say the same run, as the server accepts an inbound app on a background run; before, it kept the older fields and was refused at launch.
- The Codex image's launcher now says, on a run whose policy binds Codex to its own sandbox, that `/etc/codex/requirements.toml` is delivered; before, it said on every run that Codex reads no managed-policy file.
- A source scan that could not read an entry of its folder or clone, read a file only up to its 1 MiB limit, or cut its list of secret names, suggested hosts or secret files at its cap now says so in the API: its profile has `incomplete` and low confidence, and its `required_secrets_omitted` counts the secret names past the 100-name cap. Only such a scan keeps the scan-found requirements an earlier scan added. A scan that stopped at one of its other limits, such as the number of files or lines it reads, has low confidence and, as before, replaces the scan-found requirements with what it found. Before, an unreadable entry, a file past the limit or a cut list still gave high confidence, and the scan replaced the scan-found requirements with only what it found.
- In a `.env` template such as `.env.example`, a value of `""` or `''`, or a comment alone after `=`, now marks the name required, as an empty value does. Before, it counted as a default and the name was optional.
- When a scan finds two secret names that are stored under one name, such as `SESSION_SECRET` and `session_secret`, the requirement is now required when either one is. Before, the name later in sort order decided.
- A build heap given in kibibytes, such as `-Xmx1048576k`, now reads as 1024 MiB. Before, the number was capped before its unit was applied, so it read as 256 MiB.
- The folder-only runner now opens each root when it starts, and each shared folder when it checks it, one folder at a time without following a link, and keeps that folder open until its browse or pass ends. A root or shared folder renamed or replaced by a link after its check is not followed. Inside a pass, the runner and `wardyn sync` reach each file through its folder opened the same way, and never read or write a file through a link, with three exceptions that still use a path or a link: on Windows, a file brought back is moved into place by its folder's path; if the pass's own new file is replaced by a link before its modified time is set, that time is set on the link's target; and `wardyn sync --git` and `--pull-commits` run git on the folder by its path.
- A run waiting in a runner pool's queue for the one kind of runner it needs (one that enforces its barrier, takes its drives or folders, or has room for its size) no longer holds its owner's later runs back: a later run another of the owner's runners can take now starts. Only a wait no run of that owner could get past, such as no runner online with room or no recording store where recording is required, still holds them.
- A run that has waited in a runner pool's queue since a 0.9 release, before a run's size was stored with it, no longer keeps the runs behind it waiting while the runner it was judged for cannot take it at the size it joined with; it keeps its place and the next run that fits starts.
- A run scheduled for later no longer refuses the revive of a kept run into a pool with a free slot; only a waiting run that is due does.
- A run promoted from a runner pool's queue now starts before the queue records the waits of the runs it passed over, and that record gives up at the pass's time limit, so a slow record no longer leaves the promoted run to be failed as a lost start.
- A promoted run sent back to wait for its owner's sign-in after its pool was deleted now ends with `runner_pool_removed` instead of waiting in the deleted pool until its deadline.
- Erasing a person's run tasks while a scheduled run of theirs was starting and found their model sign-in refused could put that run back in its queue with its original task, title, description and correlation id, storing the erased text again. Such a run is no longer put back; it ends as failed. A run put back while the erasure ran is now cancelled with the person's other waiting runs. A run of theirs that had left its queue, and whose start the server had not yet written when the erasure ran, still started: with no task, or with the erased task when its start was written partway through the erasure. It now ends as failed. A migration (`0279_agent_runs_tasks_erased`) adds one nullable column to `agent_runs`; no row or grant changes.
- A No network run no longer has an upstream proxy hop. Its proxy wrote an `egress.allow` row for the upstream proxy (`builtin:upstream-proxy`) at start, the one allow in its audit, and `run.upstream_proxy.resolve` said `in_effect: true`. Now the server resolves no upstream for it, and `run.upstream_proxy.resolve` says `in_effect: false` with reason `no-network`. A proxy of this release also drops a runner's own `--upstream-proxy` for such a run and writes no such row; an older runner's proxy still writes it.
- A run on a person's runner no longer records `run.upstream_proxy.resolve` with `in_effect: true` at dispatch. The organisation's upstream proxy is never sent to a person's runner, so that row, written at dispatch and now also at a revive on that runner, says `in_effect: false` with reason `not-sent-to-runner` (reason `no-network` for a No network run). A destination such a run reaches through the organisation (a `via_org` grant) goes out through an organisation-side proxy, which still records its own row with `in_effect: true` under the same run when the site's upstream resolves.
- A model provider key save or sign-in that answers after its door was closed no longer closes or completes the door opened since, and a cancelled Azure DevOps sign-in no longer closes the next attempt's sign-in window. The Azure DevOps personal access token, SSH key and wall preset dialogs, and the confirm for deleting a wall preset, now stay open until their save or delete answers, with their fields read-only.
- Your walls, branding, the Git and Storage settings, the agent settings, the key domain assignment dialog, the dialog that saves a custom component for reuse and the user type editor are now read-only while their save is in flight; before, a change made during the save was dropped without a word.
- On People, a search or state filter change now clears the list until its first page answers, so Load more no longer mixes the old list into the new one. Signed-in CLIs counts down the minutes on the admin access chip and drops the chip when that access ends. The SCIM card shows the purge delay exactly, in the largest whole unit (for example 36 hours); before, it was rounded to whole days, so a delay of 1 hour read as 1 day. On Audit, a read for a run filter that has since changed no longer marks the new list as loaded, live, failed or not updating.
- The Permissions form for adding a grant, and on Governance the deciders, the assignment form, the profile editor and the platform envelope form, are now read-only while their save is in flight; before, a change made during the save was dropped. A Governance change's drawer now stays open while its approval or rejection is out, and an access request's kind can no longer be switched while it is being sent, so the answer no longer closes the drawer opened since or goes unseen. After Discard mine and reload, the Git, Storage and agent settings stay read-only until the reload answers, so an edit made meanwhile is no longer replaced by it.
- On Approvals, a slow read of your own access requests no longer shows a request you withdrew as still waiting, and the next poll waits for a read still out instead of starting another. A workspace's page now shows that a recording has ended even when the server answers slower than the page polls. On the admin Drives screen, a page of people's drives that answers after a delete no longer shows a drive twice or makes Show more skip one. A synthesized profile that answers after another recorded session was opened no longer replaces the policy that Save policy creates. The model provider editor is now read-only while its save is out and, after Discard mine and reload, until it closes; before, a new provider whose list of people was refused lost what was typed during the save. On Governance, Save profile is unavailable while the Deciders section holds a change that was not saved or sent for approval, and that section is read-only while the profile save is out; before, the profile save closed the editor and dropped the change.
- On Governance, Save profile now says beside it why it is unavailable while the Deciders section holds a change that was not saved. On Approvals, a poll read that answers after a decision no longer puts the decided request back on Pending or Waiting on you, and a slower read of the decided history no longer drops a request that a newer read showed. In the model provider editor, Cancel during Discard mine and reload no longer closes another provider opened before the reload answered.
- The Secrets page's Add secret dialog no longer suggests `anthropic-api-key` or `openai-api-key`, as a name to pick or as the example name; the server has refused to store both since model access moved to Settings → Model providers, and the dialog now says where a model key goes: Settings → Model providers in the Admin view, and Settings → Your model connections in the User view where the deployment has model providers set up.
- Deleting a secret on the Secrets page now names the person this tab was signed in as when Delete was chosen. If someone else has signed in on the browser since, the server refuses the delete instead of removing that person's secret of the same name, and the refusal is shown; nothing is removed and nothing is sent again.
- A Delete or Rotate chosen on a Secrets page row now acts on the namespace the row was listed from, your own secrets or the administrators' shared ones, through the route that names it. Before, it went to whichever namespace the session's role picked when the request arrived, so the same person signing in again with a different role could delete or replace the other namespace's secret of the same name.
- Add secret on the Secrets page stays off until the tab knows who is signed in and the list of your secrets has loaded, and says why when the list fails. Before, a failed list, or a list read before the tab knew whose it was, let a save replace an existing secret without asking first.
- Add secret on the Secrets page, and the proxy setup's Save of a proxy address that carries a password, no longer replace a secret someone else stored under the same name after the list was read: the write is create-only, and a name the server already holds shows the overwrite warning and stores nothing until the person chooses to overwrite it.
- A Git credential saved or disconnected on Workspace providers, and the proxy setup's secret, now go to the administrators' shared namespace through the route that names it, and a token added from New Run's setup list goes to the shared namespace for an administrator or to the person's own, by the same kind of route, once the tab knows who is signed in. Before, each went to whichever namespace the session's role picked when the request arrived. Their saves are also create-only unless they replace a stored secret on purpose: a name the server already holds shows the overwrite warning and stores nothing until the person chooses to overwrite it. The proxy setup's Save stays off while its secret is being stored, so a double click sends one write.
- The Model provider only policy template no longer grants an API key stored as `anthropic-api-key` or `openai-api-key`, and the policy editor's grant example names `npm-token` instead; the server cannot store either name and drops such a grant when a run starts.
- Leaving an Open App link's page while it is still opening no longer lets that earlier attempt take the tab to the app later, or show its error on the page you moved to.
- The desktop Files panel now offers Try again, and keeps checking a paused run, when the connection drops or a read times out.
- The Add secret dialog: a save started before the dialog was closed and reopened no longer clears, closes or changes the new draft, and its failure is shown by name; while a save is in progress nothing closes the dialog and its fields cannot be changed, so nothing typed then is lost; Enter in the Name field saves; and closing the dialog with typed text asks first.
- Workloads: restarting a person's workload as someone else no longer says it restarted. The server records that restart for the owner's next apply and starts no run, and the console now shows the server's sentence; a restart that starts no run for another reason says so.
- A failed change to a run's end, a failed Extend, Revive, Extend and revive, End run or Resume from the run's banner, and a failed token revoke under Admin → Credentials now stay on screen with the action there to try again; before, the error showed only in a passing notification, and a failed revoke closed its dialog.
- `wardyn workload restart` no longer prints that the workload restarted when the server started no run: it prints the server's sentence for that answer, or that no new run has started.
- Smaller console fixes: repeated clicks on Download approved fetch the file once; the Approvals screen asks again on its next refresh when its check for Waiting on you failed, instead of leaving that tab out until the screen is reopened; copying the managed-install command, which carries no token, no longer counts as copying the token before Done; the kept-machines card reads the list again every 10 seconds while a machine is being prepared, used by a run or deleted; the runner pool editor's Back and Cancel ask before discarding unsaved edits; and changing the replay speed keeps the replay's place and whether it was playing.
- An ended run's sandbox that is kept to copy changes back to your computer is no longer deleted by the run's automatic retirement on its runner: before the copy back has run, while a pass is under way, or while it waits to resume. Retirement now waits until the copy back has finished, has been given up, or its sandbox has been stopped, and a deletion queued before the sandbox was kept is held back until then if the runner has not received it yet. Affects 0.10.0 prereleases only.
- When the server, as it starts, tears down a sandbox it was keeping to copy changes back, because it could not confirm that the sandbox's agent was cut off, the run now records the copy back as given up, with the count and up to 50 names of the changes not brought back when a pass had counted them, and writes its audit row. Before, the run went on showing the copy back as still owed. Affects 0.10.0 prereleases only.
- When a run that was ended past its end while another operation held it has its kept sandbox stopped later, because the server could not confirm that the sandbox's agent was cut off, the run now records the copy back to your computer as given up, with the count and up to 50 names of the changes not brought back when a pass had counted them, and writes its audit row. When the server cannot read the run's folder state, it records the give-up once the sandbox's keep has run out. Before, the run went on showing the copy back as still owed. Affects 0.10.0 prereleases only.
- When a run's end stops a sandbox that was kept, or still due, to copy changes back to your computer, and the copy back did not run or could not be recorded because the server could not read or write the run's folder state, or, for a kept Docker sandbox the server runs itself, because it could no longer stop the sandbox's agent and network first, the run now records the copy back as given up, with the count and up to 50 names of the changes not brought back when a pass had counted them, and writes its audit row. When the server cannot read the run's folder state, it records the give-up once the sandbox's keep has run out. Before, the run could go on showing the copy back as still owed, or record it with no count. Affects 0.10.0 prereleases only.
- A queue pass now reads a runner pool's runners once for one person's waiting runs, not once for each run it judges, so many runs of one person that each wait for a runner of their own (one they name or, for a run that needs one kind of runner, that one kind) no longer use up the pass's time on those reads while another person's run further back waits. Runs the person queued under another unit or sign-in identity, or that ask for another placement or named runner, are read once more each.
- On Approvals, switching between the User and Admin view while a read of the pending queue or the decided history is out, or while a decision is being sent, no longer draws the list of the view left, or that read's failure, as the new view's; the new view's list shows once its own read answers.
- On Approvals, a request whose decision is being sent, or was just made, now has its buttons off in both the User and Admin view and on every tab until the list has been read again; before, switching the view, or leaving Waiting on you and coming back, showed it with Approve and Deny available, and a second click got the server's already-decided answer. When the server answers that a request was already decided, Approvals, a run's Approvals tab and the run page's live approvals strip now read their list again at once, so the request leaves the screen without waiting for the next poll and a reason dialog open on it closes.
- A runner reconnecting no longer removes the sandbox of an ended run that is kept so its changes can still be copied back to the folder on your computer, nor one whose end is still copying them back. Before, the sweep Wardyn runs when a runner reconnects removed every ended run's sandbox, and those changes were lost. Affects 0.10.0 prereleases only.
- `wardyn apply --json` and `wardyn preset set` now print the workloads or presets that were stored before a later one failed, and still exit with the error.
- `wardyn governance plan` names a restriction it would delete (`restriction "name"`) instead of an empty assignment line.
- `wardyn-runnerd drive open` says the changes were written back only after the throwaway VM confirmed it had written them; when it does not confirm, the folder is kept and the command fails.
- A laptop's audit forwarding no longer stops at a row made by a service identity. The organisation answered it with 400 `invalid_row`, and every later row waited behind it; it now stores the row, with its claimed actor type `service`, as it does a human, agent or system row.
- After every organisation server runs this release, restart `wardynd` on each laptop whose audit forwarding had already stopped at a service identity's row; it does not start again by itself. Such a laptop's log says forwarding is halted until wardynd restarts, and its `/healthz` `org_federation.lag` stays above 0. After the restart the laptop sends again from the first row the organisation has not acknowledged. This fix needs no change on the laptop besides that restart.
- A run lost to an outage, whose end could not be read from the database, now has its agent stopped where the substrate cannot stop its proxy alone. Where it cannot keep a stopped sandbox either, as on Kubernetes in server mode, the run is torn down, and its `run.lost` row says `kept: false` with the read's error in `lost_error`. Before, nothing was stopped and the stop was only tried again: the run stayed kept, with its containment recorded as unresolved.
- `wardyn login` now stores a new sign-in, and records which store holds it, before it removes the sign-in it replaces from the other store, so a keychain that refuses the write leaves the previous sign-in in use. `wardyn logout` now reports that the organisation was not told when the stored sign-in cannot be read, instead of a clean sign-out; the stored item is still removed. If the sign-in it replaces cannot be removed, `wardyn login` says where it is still stored. `wardyn login` also says when the sign-in it replaces could not be read, and so was not signed out. When the CLI profile cannot be read, `wardyn login` now signs out the sign-in it replaces in the same store; before, that sign-in was not signed out.
- Two wardyn commands that update the CLI profile at the same time no longer drop each other's change, such as the store a sign-in was just saved in. Requests the CLI sends with a stored sign-in, or to an organisation whose certificate authority was added with `wardyn ca add`, now stop waiting when no response has started within 30 seconds of the request being sent.
- When a run that was ended past its end while another operation held it still owed changes to your computer and its sandbox could not be stopped, the run went on showing those changes as kept, with a give-up time that had passed. Once the sandbox's keep has run out, the run now records the copy back as given up, with the count and up to 50 names of the changes not brought back when a pass had counted them, and writes its audit row, while Wardyn keeps trying to stop the sandbox. Affects 0.10.0 prereleases only.
- When the server's setup status cannot be read, the rest of the console no longer reads that as a host with no barrier or as Wardyn not being reachable. The notice across the top says "Couldn't check this host's barrier.", with Re-check for those who can fix it, and an empty Runs board's barrier and model provider rows say "Couldn't check." Settings, Your account and the Getting started pages say they couldn't load setup status, and the welcome screen says "Couldn't check". The control plane banner now reads "Control plane not available", not "unreachable", since it also shows for an error answer, and a model provider sign-in whose check with the server failed says "Wardyn couldn't verify this sign-in with the server — try again."
- A server whose clock is ahead of or behind the database's no longer moves a run's times or most of the decisions that read them. A new run's start, end and lifetime cap are now stamped on the database's clock, and under an elevation the cap is the earlier of the run's maximum and the elevation's end on that clock. Ending a run at its end, keeping or force-stopping it, marking it lost, reviving it, changing its end or wait, renewing its run token, a queued run's cap check as it starts, the waits of several background sweeps and other checks of a run's times now read the database's clock too, so servers whose clocks differ no longer disagree about them. When that clock cannot be read, the decision waits: a request answers an error and can be tried again, and a background pass tries again on its next turn. A revive that failed and whose lost mark cannot then be stamped still has its access revoked and its proxy asked to stop. A queued run whose start cannot read the clock goes back to its queue, and fails only if that cannot be written either, with a failure row that says the clock could not be read. The warning that the end you asked for was cut, and the `run.create` audit row's `end`, now give the end as stored, so an end your elevation cut reads as cut by it, and one that ends at or before the elevation's end does not.
- The console no longer says Wardyn isn't answering when Wardyn answered with a failure. The Sign in to continue dialog, the sign-in screen, creating an API token, the units on the elevation request form, Your requests and a person's tokens now show the server's own sentence when it gives one, and otherwise say what Wardyn couldn't do, such as "Wardyn couldn't check your session." or "Wardyn couldn't load your units."; opening an app says "Wardyn couldn't open this app." Each adds a prompt to try again only after a server error (a 5xx answer). Where an answer's body cannot be read, creating a token, the units, Your requests, a person's tokens and opening an app say "Wardyn's answer couldn't be read." Before, the sign-in screen could also show a bare status text, a raw response body or an empty alert.
- New Run's Add a caller menu now asks again for your groups and the active service identities you sponsor the next time it opens after a read of either failed; before, a read that failed was not asked again, so a passing outage kept them hidden. A list that loaded is kept. The run title in the Name dialog and the fingerprint when you claim a runner, which show a Required mark, now also carry the browser's own required attribute.
- A run token renewal for a run that ended before the renewal was recorded is now refused as for an ended run, `403` `run_terminal`, and no new token is given. Before, a run that ended between the renewal's read of it and its record of the renewal got a new token.
- When a run's access revoke could not finish and the record that it is still owed could not be written either, as when the database cannot be written at that moment, the server now writes that record again about once a minute until it lands or the server stops, and the revoke is then retried even when the run holds no sandbox. Before, it was retried only while the run held a sandbox. Each step of a revoke now has a time limit of 10 seconds, so a step that hangs no longer leaves the steps after it no time.
- A revive whose new proxy could not be started, on a run that keeps its old proxy or is lost again, no longer holds the run's lock while the database does not answer. The step that drops the unused proxy start now gives up after 15 seconds and runs once. Before, it waited as long as the database took, and on a run that kept its old proxy it then ran a second time for up to 30 seconds more, while every other action on the run waited. A start it could not drop stays the run's pending policy readback until the run's next revive or egress edit.
- When a run ended and the server could not read the folder's settings or the run's rule for letting files out at that moment, the sandbox was stopped without copying anything back. A sandbox whose network cut is confirmed (in the server's own Docker, or on a runner whose network cut is confirmed) is now kept for its copy back, which runs once those reads answer, for up to 7 days after the run ends, or for the ended-run grace if that is longer. A sandbox on a runner that is not connected at that moment is left as it is until a later check. A run with no computer folder is kept the same way while the folder's settings cannot be read, and is stopped once they can. Affects 0.10.0 prereleases only.
- A computer folder's copy back that had no time left to start when its run ended, because the end had used up its time limit before it or the server was shutting down, was recorded as finished with an error, and the sandbox was stopped with the changes in it. The sandbox is now kept, with its agent stopped, and the copy back is tried again on Wardyn's next check. A virtual machine, or a sandbox whose agent cannot be stopped alone, is still stopped, and the run records that the changes were not brought back. On Kubernetes in server mode, a background sweep can still stop the kept sandbox before that copy back. Affects 0.10.0 prereleases only.

### Security

- Deleting a namespace binding did not stop a custom resource's launch that was already under way: a launch waiting in the background, the sweep's retry of a lapsed one or a restart could still start a run as the bound service after the delete answered. Such a launch now reads the binding again before it starts a run and is refused `workload_binding_inactive`, and a run whose binding is deleted while it starts is ended with the new `run.kill` reason `workload_binding_deleted` and never becomes the workload's. One still waiting in its pool's queue no longer starts: it ends `FAILED` with `workload_binding_inactive`, and a run it was to replace that is still running keeps running. A run queued before the upgrade finished can still start; end it by hand if its binding is deleted. Runs started before the delete keep running. This adds a database migration (0280, forward only), so take a database backup first.
- A person removed from a group in the identity provider could keep an API token holding that group if they minted it, or claimed it from an invitation, while the removal ran; that mint or claim is now refused. When the person was added back and removed again while an earlier removal was still finishing, the earlier removal could mark the later one done before it revoked anything; each removal now finishes only its own steps. A person reactivated while the leaver sweeper was resuming their earlier suspension now stays active; before, the sweeper suspended them again, revoked their tokens, ended their runs and scheduled their purge.
- When the identity provider reactivated a person while their suspension was still finishing, in the request that suspended them or in the leaver sweeper's later pass, that suspension could still end runs, revoke API tokens, delete SSH keys and cut sessions the person started or added after the reactivation. A suspension step that finds the person active again now leaves what the database recorded after its deactivation and writes no new session cut for a form it already cut. Migration `0281` records when each run, API token and SSH key row was created, by the database's clock.
- An enrolled laptop could make audit rows it forwarded count as this organisation's own. Laptop audit forwarding takes any action, with data and a time of the laptop's choosing. A forwarded sensor heartbeat was taken as the sensor's own by `/healthz`, the sensor counters on the metrics endpoint and the sensor notes on a record-mode capture whenever fewer than two node sensors had beaten in the last day, so a missing or stopped sensor could show as healthy. A forwarded key domain assignment row counted in the Key domain changes setup check and could be named as the latest change. A forwarded governance row naming a unit appeared in that unit's audit feed for its unit admins, where it could read as a security admin's write. These reads now take only the organisation's own rows. With fewer than two live node sensors, the sensor's state now comes from its own latest heartbeat record from the last 7 days, which only the sensor's ingest writes, and the audit heartbeat is read only when there is none. That read stops after half a second, so `/healthz` answers inside its Kubernetes probe timeouts even when the read is slow, and each server makes it at most once every 30 seconds. When it fails or runs out of time, `/healthz` reports the sensor unavailable with the reason "Sensor heartbeat could not be read", and the audit page and a record-mode capture's note say the heartbeat could not be read, instead of saying no sensor has ever reported. Forwarded rows are still accepted and stored with that laptop's audit rows.
- A host sensor whose Tetragon or export had stopped kept reading healthy for as long as its ingest process kept sending heartbeats, because each heartbeat carries the sensor's lifetime counts. `healthy` now needs the kernel event counts on those heartbeats to have risen in the last 15 minutes, as kept on the sensor's own heartbeat record, so every server reads the same. Otherwise the sensor reads idle, with the reason "No new kernel events in the last 15 minutes" on `/healthz` and the audit page (with two or more node sensors the reason counts the idle nodes) and its own note on a record-mode capture. With no rise on record yet, as after upgrading or for a new node, it reads idle with "Whether new kernel events still arrive is not yet known" until its counts next rise. A host or node that runs nothing the sensor counts (process starts, network connections, writes to credential paths) for 15 minutes reads idle the same way. A sensor that stops can still read healthy for up to 15 minutes after the last heartbeat that showed higher counts. When the heartbeats before that one were lost, as across a control plane restart, it can come up to 15 minutes after the sensor's last kernel event. With no sensor heartbeat on record, the capture note and the audit page now say so, instead of saying none was ever observed, since audit retention can drop old heartbeats.
- The reason `/healthz` gives anonymous callers for a sensor that saw kernel events but tied none to a run no longer includes how many it dropped, a count the anonymous view otherwise withholds. The count stays on `wardyn_groundtruth_dropped_unmapped_total` on the operator-gated metrics endpoint.
- With `WARDYN_PG_MIGRATE_DSN` set, the boot check of whether the app role can reach past `audit_events`' append-only guard now reads what every role it can `SET ROLE` into holds, counting the roles reachable from the role its connection signed in as, as well as from its current role, and it also counts: the `TRIGGER` privilege held by such a role whose privileges the app role does not inherit; `TRIGGER` on, or ownership of, any partition of the table or its bookkeeping tables; ownership of a function in the table's schema or run by its triggers; the `CREATE` privilege on that schema; a `SECURITY DEFINER` function there without a `search_path` that ends in `pg_temp`; and membership in `pg_execute_server_program`, `pg_read_server_files`, `pg_write_server_files` or, before PostgreSQL 16, a role with `CREATEROLE`. Before, a role holding any of these was logged as DDL-protected. PostgreSQL 14 and earlier grant `CREATE` on schema `public` to every role, and a database brought to a later version by `pg_upgrade` or by a dump and restore can keep that grant, so a deployment there that has not revoked it now logs that DDL protection is not in effect. The check only logs: it gates nothing, and the boot goes on whichever way it comes out. Migration `0285` also takes the install's schema off the `search_path` of the audit functions and of the two functions that order sign-in revocations, wherever the migrator owns them, so a function a role creates in that schema is no longer run by `audit_append` or the other audit functions. `CREATE` on that schema is still reported, because migrations look names up there.
- The CA certificates `wardyn ca add` stores are now trusted only for the CLI's connection to that organisation's host. Before, a certificate signed by one of them that also named another host was accepted on the CLI's connection to that other host, such as an `https://` proxy; that connection now uses the system's trusted roots only.
- A recorded desktop now refuses every pixel format but the one its replay reads: 32 bits per pixel, depth 24, little-endian true colour, a maximum of 255 and shifts 0, 8 and 16 for red, green and blue. Before, a viewer that set 8 or 16 bits per pixel could make the desktop recording keep the desktop's clipboard text, which recordings leave out, and a viewer that set another 32-bit layout could make the replay show wrong colours.
- Wardyn's binaries, in its downloads and its container images, now build with Go 1.27.4. Before, the container images built with Go 1.27.3, which the `net/http` advisory GO-2026-6859 affects, and the downloads built with Go 1.26.11.
- macOS kept machines: a command sent to a run just as it stopped could start after the stop scrub had finished, and write back what the scrub removed, such as the AWS CLI's sign-in cache, while the machine's record said it was scrubbed. The guest agent now refuses a command that has not started by the time the stop scrub begins, and the scrub waits for one that is already starting. Run the tools update once to get the new guest agent. Not yet tried on a real Mac.
- macOS kept machines: a program of a run still running when a normal stop scrubbed the disk, such as its harness, a command or an app on the desktop, could write back what the scrub removed, and the machine's record still said it was scrubbed. A normal stop now asks macOS to sign `agent` out and ends every program of `agent` before the scrub, without asking any to save, and looks again after it. If one will not end, or the check immediately after the scrub finds one running, the machine shows `scrubbed: false` and is scrubbed when it next starts. Run the tools update once to get the new guest agent. Not yet tried on a real Mac.
- Runner posture: a Linux runner whose root file system spans an encrypted and an unencrypted disk, such as one LVM volume over both, no longer reports its disk as encrypted. A posture report from a runner's earlier connection that is handled after its current connection began no longer replaces the current posture. Either could let a credential that requires an encrypted disk be delivered to a runner that does not have one.
- The one-line installer's upgrade rewrote `.env` through a fixed name, `.env.tmp`. An `.env.tmp` already there gave `.env` its permissions, so a leftover file others could read could leave `.env` readable by them, and a symlink at that name had the store key and the admin token written to wherever it pointed. Each rewrite now goes through a new file made by `mktemp`, readable by its owner only, and an existing `.env.tmp` is left untouched. The next upgrade leaves `.env` readable by its owner only. `scripts/up.sh` and `scripts/setup.sh` rewrote `deploy/compose/.env` through `.env.tmp` too, and followed a symlink at that name; they now use a new `mktemp` file as well.
- `wardyn support-bundle`: a flag such as `--password value`, `--api-key value` or `--requirepass value`, given in a Compose `command` list or in a shell string, kept its value in `compose-config.redacted.yaml`, and so did `--api-key=value`. When a flag's name contains `key`, `pass`, or one of the words the bundle already treats as secret in a setting name (such as `token`, `secret` or `auth`), the list entry after it is now redacted, so is a list entry that joins it to its value with `=`, and a shell string that carries such a flag with a value is redacted whole.
- The proxy now drops the headers a request's `Connection` header names as soon as the request arrives, before any check reads them. The checks used to read those headers while the forward dropped them, so a check could judge a different request from the one sent. On an Azure DevOps host with no `host_rules` entry, a `POST` to a repository's `refs` carrying `X-HTTP-Method-Override: PATCH` and `Connection: X-HTTP-Method-Override` was judged as an update of the run's own branch named in its `filter`. The `POST` was then sent with the run's credential, and could move the refs its body named, such as `main`; push rules other than `deny_all` did not check it.
- On Azure DevOps, when a push moves a branch other than forward from the tip it states, what it does not carry is no longer compared with the commits it builds on for `require_review_paths`, `deny_paths`, `max_file_size_mib` and `deny_new_executables`, even when the push also carries a commit built on that tip. A path those rules could match is held or refused instead, as for any move other than forward. Before, such a commit made the move read as forward, whether no ref in the push pointed at it or another ref in the push set it. What the push left out was then compared with an older commit, so content the branch's tip had changed at a `require_review_paths` path could go back to an older version without a hold. On GitHub and Azure DevOps, a commit the push carries that is not part of the history of any ref it sets is no longer used to find the commits the push builds on. The Azure DevOps comparison came in 0.10.0-rc.10.
- On GitHub and Azure DevOps, a push that moves an existing branch must now leave nothing at a `deny_paths` or `require_review_paths` path that the branch's tip did not hold there, whether the push carries commits or moves the branch to a commit the forge already holds. Anything else there is refused or held. Before, a merge naming an older commit of the branch as a parent, a second branch the same push created on such a commit, or a move that carried no commits brought an older version back without a hold, and a stock git client could send each. A hold now names every such path, also when the push changes another review path or one of its branches cannot be compared. This also refuses or holds a merge that brings a change at such a path into an existing branch, from the default branch or anywhere else; a branch the push creates may still take it. Under these two rules a push that deletes a branch is refused or held, because a branch created again has no tip to compare with, and a request that names one ref twice is refused. `max_file_size_mib` and `deny_new_executables` still compare with any commit the push builds on.
- A held governance change, or an elevation or image request, now belongs only to the sign-in that proposed it: that sign-in alone can read it without being one of its approvers, write its note or withdraw it. Before, a different sign-in showing the same email could do so even when sign-in did not check that email: an administrator outside the change's approvers could read, note and withdraw a held change, and any signed-in member could list and withdraw another person's request. Nobody could approve through it. A second sign-in with the same email still counts as the proposer when sign-in vouches for emails (`WARDYN_OIDC_EMAIL_DOMAINS` or `WARDYN_OIDC_REQUIRE_EMAIL_VERIFIED` is set, or `WARDYN_OIDC_ALLOW_EMAIL_MAPPINGS` is on), and the same email still stops that sign-in from approving the change. Affects 0.10.0 prereleases only.
- With `WARDYN_GOVERNANCE_SECOND_HUMAN` on, approving a held change of a service identity's sponsor could undo a suspension, a resume or a description edit made after the change was proposed, when the held `PATCH` also named that field: one that named `"suspended": false` while the identity was active cleared a later suspension. The held change now applies only while the identity's suspension and description are as they were when it was proposed; otherwise the approval answers `409` `governance_change_stale` and the identity is left as it is. A change held before the upgrade for an identity that has a description or is suspended answers the same; propose it again.
- A request made with a service identity's token that was already authenticated when the identity was suspended, retired or given a new sponsor could still store a model provider credential for the identity or create a run as it; a credential stored that way after a retirement stayed, though the retirement had erased the identity's credentials. Such a write now either lands before the suspension, retirement or sponsor change (a retirement's erase then removes a credential stored that way), or is refused: a model provider credential write or a `POST /runs` made that way answers `403` `identity_deactivated`.
- A save from an older copy of a stored policy no longer overwrites a newer save. Before, a second tab or a second admin saving the policy as they first opened it put back an allowed host that had since been removed. `PUT /api/v1/policies/{id}` now answers `409` with reason `policy_stale`, writing nothing, when the policy was saved after the `updated_at` the caller sent as `expected_updated_at`. The console keeps the refused edit on screen to copy, and **Discard mine and reload** loads the saved policy. `wardyn policy set <id> -f` sends the `updated_at` from a file that `wardyn policy get` printed, and `--overwrite` replaces the policy as it is now.
- Content inspection: with `inspect_forward_egress` on, the remote MCP broker now scans each POST body it forwards, before it adds the server's credential. A body the policy blocks is not sent: each request in it is answered with a JSON-RPC error, and the refusal is recorded as a `scan:blocked` decision. A forwarded body's findings or skips ride on its decision. A custom detector in Wardyn's own format must now answer with exactly one JSON object holding a `findings` field: any other 200 answer, such as `null`, `{}` or an error object, is a sidecar error (`sidecar_error`) instead of a clean scan, so `on_scanner_error=block` refuses the request in `block` mode. `{"findings":null}` is still a clean answer. A finding's field path now masks an email address, US Social Security number, Luhn-valid card number, IBAN or E.164 phone number used as a JSON key, whether or not `detect_pii` is on.
- A folder scan now opens a folder inside a member workspace root when it checks it against those roots, one folder at a time from the top of the drive without following a link, and reads it only through that open handle. This holds for a member's scan of their own workspace and for an operator's scan, from the library or from an operator's own workspace. Before, a member's scan read the folder again by its path after the check, and an operator's scan read it by its path unchecked. So a folder, or a folder above it, that was or became a link could put a folder outside the member's roots into the scan's results, which every workspace that attaches the folder shows: its repository remotes, host names, secret names and secret-file paths, not secret values. An operator's scan of such a folder must now resolve inside every member root that holds it; a folder reached through a link that leaves that root is refused and is onboarded by its real path instead. Every folder scan now also lists and reads the files inside the folder without following a link at any folder on the way. The server needs read access to each folder from the top of the drive down to a folder inside a member root; where it can only pass through one, the scan is refused as outside the member's roots, and the server log says why.
- A folder scan on the server is now held to every member whose workspace attaches the folder, since what it finds shows in each of those workspaces. Before, which folders got the member check was decided from today's roots and the folder's spelling. So three kinds were still read by their path, following links, or checked against one member only. One was a folder whose member's roots were narrowed or removed after onboarding. Another was a folder named another way, such as through a link to a member root or a folder above it, an operator's link into a root, or another letter case on a Mac. The third was a folder inside one person's root, scanned by a member whose wider roots hold where a link in it leads. Now a folder named another way is named from the first member root on its way, found by identity rather than by name, and must resolve inside every member root that holds that name. A member's workspace that attaches a folder none of that member's roots holds any more blocks every scan of it (`workspace_source_mount_not_allowed`), naming that workspace and its owner to an operator. It clears once the folder is removed from that workspace, the workspace is deleted or reassigned to the operator, or that member's roots cover the folder again.

### Before you upgrade

- This release adds database migrations, starting at 0278, all forward only. Going back needs a database restore, so take a database backup first.
- Split-role installs (`WARDYN_PG_MIGRATE_DSN` set) whose app role does not pick up new tables through the `ALTER
  DEFAULT PRIVILEGES` step in Operations: grant the app role `SELECT, INSERT, UPDATE` on
  `principal_identity_unbinds` and `run_teardown_holds`. Without the first, unbinding an identity fails, and so do
  erasing a person or their credentials, a SCIM purge, and reading any audit row that names a person by subject
  (`WARDYN_AUDIT_SEAL=full`). Without the second, the server cannot queue a kill, end or proxy stop for an offline
  runner of any kind, retires no finished run from its runner, and fails `GET /runs/{id}/containment`. A runner whose
  state page makes the server queue a proxy stop, and a Mac runner that holds or has held a kill or end, have their
  state pages refused, so they do not come back online. An
  upgrade from before 0.10.0-rc.18 also needs the 0.10.0-rc.18 grant: `SELECT, INSERT, UPDATE` on `run_proxy_starts`.
  An upgrade from before 0.10.0-rc.17 also needs the 0.10.0-rc.16 grants: `SELECT, INSERT, UPDATE, DELETE` on
  `governance_unit_deciders` and `governance_envelopes`. An upgrade from
  before 0.10.0-rc.14 also needs every grant the 0.10.0-rc.14 notes list: `SELECT, INSERT, UPDATE, DELETE` on
  `run_create_keys`, `wall_presets`, `kept_machines`, `workloads`, `workload_bindings`, `workload_run_ends`,
  `macos_vm_disks`, `aws_sso_refused_tokens`, `cli_sign_ins`, `ui_desktop_closed`, `desktop_signins`,
  `desktop_signin_shares`, `desktop_signin_runs`, `approval_rules`, `governance_restrictions`,
  `governance_restriction_members`, `governance_unit_admins`, `oidc_session_sids` and `desktop_vm_sign_ins`,
  `SELECT, INSERT, UPDATE` on `groundtruth_sensors`, `runner_health`, `launch_preset_version_floor`, `cli_sessions`
  and `governance_boundary` (and on `run_harnesses` if it turns on `WARDYN_MULTI_AGENT_TASKS`), and `SELECT, INSERT`
  on `audit_checkpoints` and `run_model_usage`.
- If your own Tetragon policy hooks `security_file_permission`, export its access mask as an `int` argument and its return value as an `int` (`return: true` with a `returnArg`). A write is no longer recorded without both, and `/healthz` does not report its absence. The Compose policy has both.
- A connect is now recorded only from a hook on `tcp_connect`, `security_socket_connect`, `__sys_connect`, `__x64_sys_connect` or `sys_connect`. If your own policy hooks another function that carries a socket, that hook's connects are no longer recorded. `/healthz` shows the loss only when your policy hooks none of the five: it then reports `partial` and lists `kernel.network.connect` under `missing_kinds`, once every live sensor has recorded an event. If your policy also hooks one of the five, `/healthz` shows nothing.
- Upgrade the kernel sensor before the server or with it. In Compose the sensor is its own image, `wardyn/wardyn-tetragon-ingest`, built from source, so rebuild it too; in Helm the sensor DaemonSet and the server roll out separately. A sensor older than the server still splits the command line into words before it sends it; the server joins the words back with single spaces before it masks them, so only a registered secret whose white space is single spaces between words is sure to be masked: one that contains a tab, a line break, a non-breaking space or two spaces in a row, or that starts or ends with a space, can still be stored as words until that sensor is upgraded. A server older than the sensor stores the sensor's command line as one `arguments` field beside an `argv` that holds only the binary, and drops it from an event that loses its run.
- An older `wardyn` CLI or Go SDK can no longer save a change to a stored policy. Its `PUT /api/v1/policies/{id}` carries no `expected_updated_at`, so this server refuses it `400` `policy_revision_required`. Creating, reading and deleting policies still work. Update the CLI, and anything built on the SDK, when you update the server. A script that calls the route itself must send the `updated_at` it read as `expected_updated_at`.
- Update the server before the CLI. A CLI from this release sends `expected_updated_at` when it replaces a policy, or creates one from a file that carries `updated_at`, which an older server refuses as an unknown field.
- A trigger of your own on `audit_events` or one of its bookkeeping tables, such as a notify or replication trigger, whose function sets no `search_path`: that function now runs with `pg_catalog, pg_temp`, the path migration `0285` gives the audit functions that fire it. Before, the install's schema was on that path. Before upgrading, name every table, sequence and function it uses with its schema, or give it its own `search_path` that ends in `pg_temp`. Otherwise a bare name is not found and the audit write that fired the trigger fails; the server writes one audit row as a check when it starts, so it then refuses to start.
- A folder scan of a folder inside a member workspace root, a member's scan of their own workspace or an operator's scan from the library or from an operator's own workspace, now opens every folder from the top of the drive down to that folder without following a link. An operator's folder there that is reached through a link leaving the root that holds it is refused: onboard it by its real path instead. In `scripts/setup.sh` host mode, where the server runs as you, and with rootless Docker, the server's user needs read access to each folder on the way. Where that user can only pass through one of them (execute without read), the scan is refused as outside the member's roots and the server log says why. Before upgrading, give that user read access to every folder on the way to the member workspace roots and inside them, or move the roots to where it has that access.
- If you narrowed or removed member workspace roots after members onboarded folders, a scan of such a folder by an operator or by another member is now refused (`workspace_source_mount_not_allowed`). An operator reads which member's workspace still attaches it. Before upgrading, remove those folders from those workspaces, delete or reassign the workspaces, or set the members' roots to cover the folders again.
- A folder an operator onboarded through another name of a member root is now checked as that root's folder. Another name is a link to the root or a folder above it, or another letter case on a Mac. If a link in the folder then leads out of the root, the scan is now refused where it used to read the folder by its path. So is a folder reached through a link whose target runs through the root and then has a `..`. Onboard such a folder by its real path instead.

### Known issues

- macOS VM bases with `/Library/Wardyn/carry-before-login`: not yet known on a real Mac are whether a restart started inside the guest keeps the VM running, whether macOS then keeps `agent` at the login window until the carry is done, and whether the carried settings keep Setup Assistant's Apple Account pane closed. If runs on a test base fail as stopped while booting, remove the two items from its list.
- On a Kubernetes install with keep on, the pool's keep chip and the ending-soon notice say files are kept for every interactive run. Only runs with a kept work folder and a `claude-code` or `codex-cli` agent are kept.
- A forced stop of an ended run, once its lock has been held 5 minutes past the end of its keep grace, still stops the sandbox when its network cut cannot be confirmed or the sandbox cannot be kept: a virtual machine, Kubernetes in server mode, a Kubernetes runner, a Docker runner older than 0.10.0-rc.20, or a runner that is not connected. Changes not yet copied back are then lost, and the run records "Not brought back: the run was ended past its end while another operation held it or too many runs were ending at once", unless the server stopped before writing that record. When the server could not read the folder's settings, its record or the run's rule at that moment, the run records that the changes were not brought back once the sandbox has stopped, or later if the server still cannot read them then. A failed stop is retried on the next watcher tick, normally within a minute.
- A copy back that finds the folder busy on a sandbox that cannot be held (a virtual machine, or an agent that cannot be stopped alone) stops the sandbox at that visit and records "not brought back". The same happens when the last pass may still be running on an older runner and the sandbox's agent cannot be stopped alone. Changes the running pass had not finished copying are lost.
- Changes not yet copied back are lost when the laptop's runner is offline at the end of the run and the sandbox cannot be held: a virtual machine, an agent that cannot be stopped alone, or a sandbox on that same offline laptop. On Kubernetes in server mode, they are lost when the sandbox is stopped (see the server-mode line below).
- If the server stops in the moment between a run's end and the start of its access revoke, or the database cannot be written at that moment and the server stops before it can be written again, and the run holds no sandbox, that revoke is not retried. Until the server stops, it tries about once a minute to record the revoke as owed, and the revoke is retried once that record lands.
- A copy back whose result cannot be written to the guarded folder record stops the sandbox, so changes it left behind are lost.
- The first failure of a copy back that settles with an error only, such as a failed file listing before anything was counted, still stops the sandbox, and changes it did not list can be lost.
- With an older runner, the next copy back waits about an hour after one the runner did not answer, and the two can still overlap.
- Copy backs to a virtual machine are not guarded on an older runner, so one can run beside a stuck pass.
- A generation bump that fails with a store error, or fails after a folder record could not be read, stops the sandbox without a copy back, so its changes are not brought back. The run records why when the store takes that record; while the store error lasts, only a server log line says so.
- On Windows, a file replace falls back to a plain rename, with no write-through, when the folder's path cannot be resolved.
- On a Kubernetes runner that restarts while cut off from the server, a run can keep network access past its scheduled end. Its proxy stops serving at most about 65 minutes after the server records the end, or earlier when a renewal is refused for good.
- A Kubernetes sandbox template cannot set where a sandbox runs or its runtime: a template that sets them is refused, because the network enforcement check covers only Wardyn's own placement.
- On Kubernetes, a narrowed proxy restarts on its launch policy if its deny list is lost: a node's disk wiped under the pod, or someone with exec rights deleting it. It allows the denied hosts until the next edit. One restarted over about 30 minutes in cannot renew, so its proxy is stopped and its run marked lost or ended within about 66 minutes of its last renewal.
- The fix for overlapping network edits on Docker applies only when the server, the runner and the proxy image are all at 0.10.0-rc.19 or later, and the check that keeps a late edit or stop from removing a newer edit's proxy needs every server replica and the runner at 0.10.0-rc.20 or later. A live network edit is refused with an update hint when a part is known to be older; a part whose version can't be read (a digest-pinned or custom image, a runner that reports no version, or a runner's own proxy image) is not checked. During a rolling upgrade, update every server replica before using live network edits: a replica still on rc.19 starts proxies without that check. A revive or admin restart whose policy narrows because org policy or the ceiling changed mid-run can still race a stale revive on an older runner or proxy image.
- After a lost database lock, a slower attempt can still stop a newer revive's proxy or sandbox in four cases: when that attempt is ending the run; when it is a background sweep that marks the run lost or stops a kept run's proxy again (about every minute); when the newer revive came from a server still on 0.10.0-rc.19; or when the run's runner was not connected, even briefly, as that attempt's stop began. Outside a rare race inside the stop itself, it no longer happens on Docker, with the server and a connected runner at 0.10.0-rc.20, when a failed revive or network edit marks the run lost or stops a kept run's proxy again. The run then has no network (it never widens), and its policy can still read as enforced until it is revived, or until a background sweep, within about 66 minutes, marks it lost or ends a run that cannot be kept.
- Kubernetes with keep on (experimental): after a server loses a run's lock, a slower network edit or revive can still restart the kept run's proxy with an older version of its rules, which then apply at once; if the newer edit narrowed the run's network, the older rules let more through until the run is next edited or revived, or is stopped. On such an install, after a lost lock any slower end, not only the cases in the line above on a lost database lock, can also stop the sandbox a newer revive had just started: the person's session ends, and the files in the run's work folder are kept.
- If a headless run that is not on a laptop runner loses its database lock while a revive or live egress edit of it is failing, and another server revives the same run at that moment, the failing attempt can end the newer revive's run: the run stops and its sandbox is removed. Start a new run.
- If the server stops right after restarting a revived run's agent, or cannot record that the agent restarted, later network edits of that run are refused until it is revived from its page.
- If a run's lost mark cannot be written after its database lock was lost, the server tries to stop its proxy anyway. On Docker with the server and the runner at 0.10.0-rc.20 or later, that stop leaves a newer revive's proxy alone; it can still stop one started by a server still on 0.10.0-rc.19 during a rolling upgrade, or one on a runner that was not connected, even briefly, as the stop began, and, in a rare race inside the stop itself, a newer revive's agent.
- During a rolling upgrade from 0.10.0-rc.19, on Docker, a late network edit can still remove the proxy that a newer edit of the same run just started, when either edit came from a server still on rc.19. The run then has no network (it never widens), but its policy reads as enforced until a background sweep, within about 66 minutes, marks the run lost (revive it from its page) or ends a run that cannot be kept (start a new run).
- A delayed end whose lock was lost can also remove a sandbox that another server was keeping to copy changes back; those changes are lost.
- While laptop runners are still on an older release, a network edit that one had already started when the server was upgraded can briefly give a cut-off run its network back. Wardyn cuts it again: within about a minute of the edit for a run that is still kept. For an ended run, it cuts it within about 13 minutes of the edit, or about 19 minutes after the run's end if that is later, when up to 100 runs end at once on one server; about 8 s more for each run beyond that.
- When the server comes back from an outage, a virtual machine run that had already ended before it, and whose copy back had not finished, is stopped at the server's first visit to it if more than 6 minutes have passed since it ended: within about 2 minutes of the restart when few runs need a stop, and up to about 65 minutes when up to 100 runs need one at once on one server; about 39 s more for each run beyond that. Its changes not yet copied back are lost, and the run's page says they were not brought back.
- On Kubernetes in server mode, an ended run's sandbox, with its agent stopped, is stopped within about 13 minutes of the run's end (about 16 if its folder is being copied back at that moment), or up to about 70 minutes when up to 100 runs end at once on one server; about 39 s more for each run beyond that; changes not yet copied back are lost. These times do not cover a server whose own sandboxes run on Kubernetes and on another substrate at once, or a run whose lock another step holds for longer than a copy back (see the line on a held lock).
- The times in these notes and in the 0.10.0-rc.20 notes for cutting off and stopping ended runs assume that the machine running the sandbox answers each request within its time limit. If that machine stays connected but answers late, only its own runs are delayed, by up to twice its delay. A sandbox whose machine never answers is not cut off or stopped by the server until it does.
- If the server stops or restarts after a run has ended and before its first copy back has started, a background sweep can stop the sandbox without copying anything back when the sandbox cannot be held for that copy back: a virtual machine, a sandbox on Kubernetes in server mode, or a sandbox whose agent could not be cut off from the network at that moment. This applies to a computer folder set to sync both ways through your own runner, on a run that lets files out of the sandbox. The agent's changes in the sandbox's copy of that folder are lost; the folder on your computer is not changed. The run now records that the changes were not brought back; if that record cannot be written then, a later sweep tries again. A folder that copies in only is not affected.
- On a Kubernetes runner or a Docker runner older than 0.10.0-rc.20, and on any runner whose network cut fails at that moment, a copy back that does not finish in one pass is given up and the sandbox stopped; those changes are lost. On a runner that is connected, a background sweep still stops a sandbox that is waiting for its next copy back when it cannot cut off the sandbox's network at that visit; those changes are lost too. On Kubernetes in server mode, the sandbox is stopped at the times given above, and changes not yet copied back are lost.
- Kubernetes: a No network run's proxy pod reaches Wardyn through a pod-selector NetworkPolicy peer behind a Service address. A CNI that matches NetworkPolicy before Service translation blocks it, and the run fails about a minute after it starts, with a hint that names this. A run Wardyn keeps is marked lost, or stays lost, without the hint. Use a CNI that matches after translation.
- Kubernetes: where Wardyn reads the proxy as older than 0.10.0-rc.18, from the server's proxy image tag or, on a runner, from the runner's reported version, nothing waits for it to confirm the policy it loaded, so the failure above does not happen. On such a CNI the run goes on until its run token lapses, about 65 minutes later, and the older token-lapse handling then ends it or marks it lost, without the hint that names this cause. A run that cannot be kept gets only the hint "run token lapsed: the control plane was unreachable past its renew window", which does not name the cause. A version Wardyn cannot read, such as a proxy image pinned by digest only, is still waited for, so that run fails as above.
- If a step on a server holds the lock of a run whose end is already recorded and does not let go, that run's sandbox is still not stopped, and a copy back it owes is neither run nor given up, until that step ends or that server restarts. Its network is still cut off where the sandbox can be cut, but the 7-day keep and the stop times in the 0.10.0-rc.20 notes do not apply to it. The steps Wardyn takes under a run's lock now have time limits, so this needs a call that runs past its limit or was given none. Such a hold is counted in `wardyn_lock_holds_over_limit_total` once it passes 10 minutes, and logged at WARN with the functions that took the lock; the lock is not freed.
- When a run ends and the server cannot read the folder's settings or the run's rule for letting files out at that moment, the sandbox is still stopped without copying anything back when its network cut cannot be confirmed, such as on a virtual machine, on Kubernetes in server mode, or when the cut fails at that moment. The same happens when the run itself cannot be read then. The run records that the changes were not brought back once the sandbox has stopped, or later if the server still cannot read them then. This applies to a computer folder set to sync both ways through your own runner. The agent's changes in the sandbox's copy of that folder are lost; the folder on your computer is not changed.

### Corrections to the rc.20 notes

- On Kubernetes, a live egress edit closes the open connections through the proxy only when it adds a host the proxy's deny list did not already hold. Connections to the proxy's DNS listener, which forwards nothing, stay open. The rc.20 notes said open connections through the proxy drop on every edit.
- An ingress in front of the server that serves live egress edits needs a proxy read timeout of at least 530 seconds, as the chart's README says. The rc.20 notes said at least 540 seconds.

## [0.10.0-rc.20] — 2026-11-12

### Added

- macOS VM bases: an opt-in switch, `/Library/Wardyn/setup-assistant-carry`, copies the agent user's per-host settings into each run's new machine identity at boot, when the guest agent starts, so a Setup Assistant pane finished once in `edit-base` may stay closed in runs. Off by default; not yet tried on a real Mac.
- On Kubernetes, a live egress edit now narrows the running proxy in place: the edit's hosts join its deny list, open connections through the proxy drop, and the answer waits for its policy readback. An edit that is not confirmed ends the run. Older runs, runs on a registered runner and edits that need a new proxy are refused as before (`409 egress_edit_unsupported`).
- On Kubernetes, when the proxy does not confirm an in-place narrowing and the edit stops the run, the run's failure hint says "The run's proxy did not confirm the narrowed policy after the edit, so the run was stopped", or, while that stop is not confirmed, "The run's proxy did not confirm the narrowed policy after the edit; stopping it is not confirmed yet and is retried". These hints name no proxy image as a cause. When the proxy's own report or a background check stops the run first, the hint keeps the general words, which name an older proxy image as one possible cause.

### Changed

- A hosted run whose network could not be cut off when it ended is now stopped, instead of kept for its folder's copy back; changes not yet copied back are lost.
- On a runner, a sandbox whose copy back was cut short after its agent was stopped and its network cut is now kept, so the copy back can resume.
- Revives and live network edits of a container run on a Docker laptop runner older than 0.10.0-rc.20 are refused with `409 revive_runner_update_needed`, naming the machine to update.
- `WARDYN_ENDED_RUN_GRACE` also sets how long a sandbox is kept for its copy back, never less than 7 days.
- A revive of a run that has reached its maximum age is refused with its own reason, `revive_max_age`, and says to start a new run instead of asking to extend it.
- A live egress edit that the run's substrate cannot apply now says what to do: "This run's network can't be changed while it runs on this substrate. Start a new run with the narrower policy." On a runner that must be updated first, it says to update the runner.
- macOS VM windows say that paste from your Mac does not reach the VM, and the edit window says Type code types only the one-time code.
- Ending a run with a Both ways folder now says that the folder does not come back, and names `wardyn folder bring-back`. The folder docs no longer point at a Bring back control the console does not have.
- Docs: corrected the default end of Interactive runs, the main-drive limit on some drivers, the status of signed checkpoints and receipts, the image-builder fallback warning, and the gateway presets.
- Kubernetes: where the egress canary shows NetworkPolicy enforced, a No network run's proxy pod now reaches only Wardyn. Its NetworkPolicy has no DNS rule and no route out of the cluster, and on a server Wardyn's internal address is pinned in the proxy pod's hosts file. A server without `WARDYN_K8S_CONTROL_PLANE_NAMESPACE` and `WARDYN_K8S_CONTROL_PLANE_POD_LABELS` (the chart sets both) refuses No network runs. Under `WARDYN_K8S_ALLOW_UNENFORCED_NETPOL` or `WARDYN_K8S_ACK_AMBIENT_DEFAULT_DENY`, No network stays proxy-level, and the run's `run.dispatch` audit row says so.
- A live egress edit of a No network run is refused with `409 egress_edit_no_network`, on every substrate.

### Fixed

- An ended run whose sandbox is kept is now cut off the network by a loop on every server. Counted from the run's end, it is cut normally within about 7 minutes, and within about 19 minutes when up to 100 runs end at once on one server (with 4 cut workers per server); about 8 s more for each run beyond that. For 10 runs it is about 8 minutes. When a cut fails twice, or the sandbox has no cut of its own (a virtual machine, or Kubernetes in server mode), the sandbox is stopped: normally within about 13 minutes of the run's end, and up to about 70 minutes, whatever their mix (with 4 cut and 4 stop workers per server), when up to 100 runs end at once on one server; about 39 s more for each run beyond that. For 10 runs it is about 18 minutes.
- A sandbox kept to copy changes back, with its network cut (in the server's own Docker, or on a runner whose network cut is confirmed), is kept for 7 days after the run ends, or for the ended-run grace if that is longer. This does not cover Kubernetes in server mode (see Known issues). Past that, the copy back is given up, the run records the files not brought back, and the sandbox is stopped.
- A run's Folders from your computer card now says what became of the sandbox's copy after the run ends: kept for now, given up, stopped and deleted, or not yet confirmed as stopped.
- A failed revoke of an ended run's access is retried in the background, also after a server restart, until it succeeds.
- A forced stop no longer stops a sandbox whose changes are still owed when its network cut is confirmed: in the server's own Docker (not a virtual machine), or on a Docker runner at this release. It is kept, and the copy back runs once the other operation lets go.
- A forced stop that could not read the folder's settings or the run's rule for letting files out stopped the sandbox without copying anything back. A sandbox that can be kept, with its network cut confirmed, is now kept for its copy back. Affects 0.10.0 prereleases only.
- Changes not yet copied back are no longer lost when only the laptop's runner is offline at the end of the run, if the sandbox's agent can be stopped alone and its network cut (in the server's own Docker, or on a runner that is connected): the sandbox is kept, and the copy back is tried again when the laptop is back, for up to 7 days. This does not cover Kubernetes in server mode.
- A busy folder on a sandbox kept with its network cut (in the server's own Docker, or on a runner) is now given up after the 7-day keep.
- A run whose sandbox cannot be kept, ending while another step holds the run, is now forced to stop about 5 minutes after its end, not after its keep grace, once the server knows that its sandbox cannot be kept; until the server knows that, the run waits for its keep grace as before. A run whose end is already recorded is not forced: see the line on a held lock under Known issues.
- A server that stops during a forced stop no longer leaves the run without its end record or with approvals still open.
- A copy back's record keeps its outcome when the runner's reason is long.
- A copy back that ended before the server took its stream reports its own reason, not "no such stream".
- The server sends each copy back its deadline, so the runner ends it in time.
- A copy back's failure to move its record before the pass now records why on the run and in its audit row.
- On Docker, with the server and the runner at 0.10.0-rc.20 or later, a late network edit no longer removes the proxy that a newer edit of the same run started. A stop made after a lost database lock, while a failed revive or network edit marks the run lost or stops a kept run's proxy again, no longer stops a newer revive's proxy, unless the run's runner was not connected, even briefly, as that stop began.
- An undo of a network edit that did nothing, because a newer edit was undone first, now cuts the run off instead of reporting the edit as kept.
- Stopping a local tmux sandbox with a request that had already run out of time now reports that nothing was stopped.
- On Kubernetes in server mode, an ended run's sandbox could stay up, with its network live, until the background sweep reached it, which on a server with many stored runs could take an hour or more. It is now stopped within about 13 minutes of the run's end (about 16 if its folder is being copied back at that moment), or up to about 70 minutes when up to 100 runs end at once on one server; about 39 s more for each run beyond that. These times do not cover a server whose own sandboxes run on Kubernetes and on another substrate at once, or a run whose lock another step holds for longer than a copy back (see Known issues).
- A request through a grant delivered `via_org` that arrived after 10 minutes without one could be cut off, or answered 503, while the server set the route up again. The server no longer cuts off or refuses a request for this reason.
- macOS VM runs: the guest agent no longer asks Setup Assistant to quit at run start. On macOS 26.6.2 that signed the agent user out and left the run at the login window. The setup-assistant-quit switch is no longer read; remove it from your base.
- Pausing, resuming and settling a paused run no longer wait without limit on a container runtime that stops answering: Wardyn now stops waiting for each freeze or thaw call after 15 seconds. The pause sweep later puts a sandbox cut off that way back in line with the run's state, and a resume is tried again on a later input, after a short backoff.
- Whether a run lost to an outage is still inside its end, and whether a kept run whose lock is held has passed its 5-minute hold-off, is now checked against the database's clock rather than each server's own, so servers whose clocks differ no longer disagree about it. When the database cannot say whether a lost run is inside its end, Wardyn first tries to cut its proxy and retries the stop on the next sweep.

### Known issues

- A forced stop of an ended run, once its lock has been held 5 minutes past the end of its keep grace, still stops the sandbox when its network cut cannot be confirmed or the sandbox cannot be kept: a virtual machine, Kubernetes in server mode, a Kubernetes runner, a Docker runner older than 0.10.0-rc.20, or a runner that is not connected. Changes not yet copied back are then lost, and the run records "Not brought back: the run was ended past its end while another operation held it or too many runs were ending at once", unless the server could not read the folder's settings, its record or the run's rule at that moment, or stopped before writing that record. A failed stop is retried on the next watcher tick, normally within a minute.
- A copy back that finds the folder busy on a sandbox that cannot be held (a virtual machine, or an agent that cannot be stopped alone) stops the sandbox at that visit and records "not brought back". The same happens when the last pass may still be running on an older runner and the sandbox's agent cannot be stopped alone. Changes the running pass had not finished copying are lost.
- Changes not yet copied back are lost when the laptop's runner is offline at the end of the run and the sandbox cannot be held: a virtual machine, an agent that cannot be stopped alone, or a sandbox on that same offline laptop. On Kubernetes in server mode, they are lost when the sandbox is stopped (see the server-mode line below).
- If the server stops in the moment between a run's end and the start of its access revoke, or the database cannot be written at that moment, and the run holds no sandbox, that revoke is not retried.
- A copy back whose result cannot be written to the guarded folder record stops the sandbox, so changes it left behind are lost.
- The first failure of a copy back that settles with an error only, such as a failed file listing before anything was counted, still stops the sandbox, and changes it did not list can be lost.
- With an older runner, the next copy back waits about an hour after one the runner did not answer, and the two can still overlap.
- Copy backs to a virtual machine are not guarded on an older runner, so one can run beside a stuck pass.
- A generation bump that fails with a store error, or fails after a folder record could not be read, stops the sandbox without a copy back, so its changes are not brought back. The run records why when the store takes that record; while the store error lasts, only a server log line says so.
- On Windows, a file replace falls back to a plain rename, with no write-through, when the folder's path cannot be resolved.
- On a Kubernetes runner that restarts while cut off from the server, a run can keep network access past its scheduled end. Its proxy stops serving at most about 65 minutes after the server records the end, or earlier when a renewal is refused for good.
- A Kubernetes sandbox template cannot set where a sandbox runs or its runtime: a template that sets them is refused, because the network enforcement check covers only Wardyn's own placement.
- On Kubernetes, a narrowed proxy restarts on its launch policy if its deny list is lost: a node's disk wiped under the pod, or someone with exec rights deleting it. It allows the denied hosts until the next edit. One restarted over about 30 minutes in cannot renew, so its proxy is stopped and its run marked lost or ended within about 66 minutes of its last renewal.
- The fix for overlapping network edits on Docker applies only when the server, the runner and the proxy image are all at 0.10.0-rc.19 or later, and the check that keeps a late edit or stop from removing a newer edit's proxy needs every server replica and the runner at 0.10.0-rc.20 or later. A live network edit is refused with an update hint when a part is known to be older; a part whose version can't be read (a digest-pinned or custom image, a runner that reports no version, or a runner's own proxy image) is not checked. During a rolling upgrade, update every server replica before using live network edits: a replica still on rc.19 starts proxies without that check. A revive or admin restart whose policy narrows because org policy or the ceiling changed mid-run can still race a stale revive on an older runner or proxy image.
- After a lost database lock, a slower attempt can still stop a newer revive's proxy or sandbox in four cases: when that attempt is ending the run; when it is a background sweep that marks the run lost or stops a kept run's proxy again (about every minute); when the newer revive came from a server still on 0.10.0-rc.19; or when the run's runner was not connected, even briefly, as that attempt's stop began. Outside a rare race inside the stop itself, it no longer happens on Docker, with the server and a connected runner at 0.10.0-rc.20, when a failed revive or network edit marks the run lost or stops a kept run's proxy again. The run then has no network (it never widens), and its policy can still read as enforced until it is revived, or until a background sweep, within about 66 minutes, marks it lost or ends a run that cannot be kept.
- If a headless run that is not on a laptop runner loses its database lock while a revive or live egress edit of it is failing, and another server revives the same run at that moment, the failing attempt can end the newer revive's run: the run stops and its sandbox is removed. Start a new run.
- If the server stops right after restarting a revived run's agent, or cannot record that the agent restarted, later network edits of that run are refused until it is revived from its page.
- If a run's lost mark cannot be written after its database lock was lost, the server tries to stop its proxy anyway. On Docker with the server and the runner at 0.10.0-rc.20 or later, that stop leaves a newer revive's proxy alone; it can still stop one started by a server still on 0.10.0-rc.19 during a rolling upgrade, or one on a runner that was not connected, even briefly, as the stop began, and, in a rare race inside the stop itself, a newer revive's agent.
- During a rolling upgrade from 0.10.0-rc.19, on Docker, a late network edit can still remove the proxy that a newer edit of the same run just started, when either edit came from a server still on rc.19. The run then has no network (it never widens), but its policy reads as enforced until a background sweep, within about 66 minutes, marks the run lost (revive it from its page) or ends a run that cannot be kept (start a new run).
- A delayed end whose lock was lost can also remove a sandbox that another server was keeping to copy changes back; those changes are lost.
- While laptop runners are still on an older release, a network edit that one had already started when the server was upgraded can briefly give a cut-off run its network back. Wardyn cuts it again: within about a minute of the edit for a run that is still kept. For an ended run, it cuts it within about 13 minutes of the edit, or about 19 minutes after the run's end if that is later, when up to 100 runs end at once on one server; about 8 s more for each run beyond that.
- When the server comes back from an outage, a virtual machine run that had already ended before it, and whose copy back had not finished, is stopped at the server's first visit to it if more than 6 minutes have passed since it ended: within about 2 minutes of the restart when few runs need a stop, and up to about 65 minutes when up to 100 runs need one at once on one server; about 39 s more for each run beyond that. Its changes not yet copied back are lost, and the run's page says they were not brought back.
- On Kubernetes in server mode, an ended run's sandbox, with its agent stopped, is stopped within about 13 minutes of the run's end (about 16 if its folder is being copied back at that moment), or up to about 70 minutes when up to 100 runs end at once on one server; about 39 s more for each run beyond that; changes not yet copied back are lost. These times do not cover a server whose own sandboxes run on Kubernetes and on another substrate at once, or a run whose lock another step holds for longer than a copy back (see the line on a held lock below).
- The times in these notes for cutting off and stopping ended runs assume that the machine running the sandbox answers each request within its time limit. If that machine stays connected but answers late, only its own runs are delayed, by up to twice its delay. A sandbox whose machine never answers is not cut off or stopped by the server until it does.
- If a step on a server holds the lock of a run whose end is already recorded and does not let go, that run's sandbox is not stopped, and a copy back it owes is neither run nor given up, until that step ends or that server restarts. Its network is still cut off where the sandbox can be cut, but the 7-day keep and the stop times in these notes do not apply to it.
- If the server stops or restarts after a run has ended and before its first copy back has started, a background sweep can stop the sandbox without copying anything back when the sandbox cannot be held for that copy back: a virtual machine, a sandbox on Kubernetes in server mode, or a sandbox whose agent could not be cut off from the network at that moment. This applies to a computer folder set to sync both ways through your own runner, on a run that lets files out of the sandbox. The agent's changes in the sandbox's copy of that folder are lost; the folder on your computer is not changed. The run's folder record may not show that the changes were not brought back. A folder that copies in only is not affected.
- On a Kubernetes runner or a Docker runner older than 0.10.0-rc.20, and on any runner whose network cut fails at that moment, a copy back that does not finish in one pass is given up and the sandbox stopped; those changes are lost. On a runner that is connected, a background sweep still stops a sandbox that is waiting for its next copy back when it cannot cut off the sandbox's network at that visit; those changes are lost too. On Kubernetes in server mode, the sandbox is stopped at the times given above, and changes not yet copied back are lost.
- When a run ends and the server cannot read the folder's settings or the run's rule for letting files out at that moment, the sandbox can be stopped without copying anything back, and the run may not record that the changes were not brought back. This applies to a computer folder set to sync both ways through your own runner. The agent's changes in the sandbox's copy of that folder are lost; the folder on your computer is not changed. It applies to every kind of sandbox.
- Kubernetes: a No network run's proxy pod reaches Wardyn through a pod-selector NetworkPolicy peer behind a Service address. A CNI that matches NetworkPolicy before Service translation blocks it, and the run fails about a minute after it starts, with a hint that names this. A run Wardyn keeps is marked lost, or stays lost, without the hint. Use a CNI that matches after translation.
- Kubernetes: where Wardyn reads the proxy as older than 0.10.0-rc.18, from the server's proxy image tag or, on a runner, from the runner's reported version, nothing waits for it to confirm the policy it loaded, so the failure above does not happen. On such a CNI the run goes on until its run token lapses, about 65 minutes later, and the older token-lapse handling then ends it or marks it lost, without the hint that names this cause. A run that cannot be kept gets only the hint "run token lapsed: the control plane was unreachable past its renew window", which does not name the cause. A version Wardyn cannot read, such as a proxy image pinned by digest only, is still waited for, so that run fails as above.

### Before you upgrade

- This release adds a database migration (0277, forward only). Going back needs a database restore, so take a database backup first.
- Update the server first, then every laptop runner.
- If an ingress in front of the server serves live egress edits (`PATCH /api/v1/runs/{id}/egress`), set its proxy read timeout to at least 540 seconds, as the chart's README says; a shorter one can answer 504 while the edit goes on.
- After the upgrade, each server's background pass revisits older ended runs that still list a sandbox and owe no copy back, up to 50 per pass, normally once a minute. It retries their stop and access revoke, writing an `identity.revoke` row, even when the sandbox has already gone. The containment loop also visits them, as below. Until that history is done, the times above can be longer.
- On the first start after the upgrade, every older ended run that still lists a sandbox is worked through at once. With about 200 of them on one server, a run that ends just then is cut off the network within about 31 minutes of its end, and stopped, if it needs a stop, within about 2 hours 15 minutes. The older runs themselves are done within about 2 hours 10 minutes of the start. Beyond 200, each run adds about 8 s to the cut and about 39 s to the stop.

## [0.10.0-rc.19] — 2026-11-11

### Changed

- A live network edit or revive on Docker now fails if the new proxy can't renew its token with the server within 10 seconds: a run that can be kept is lost until revived, and any other run ends. Creates are unchanged.
- A live edit or revive that loses its database lock while it is replacing the proxy now stops instead of finishing the replace, and a headless run that is not on a laptop runner then ends.

### Fixed

- **Go 1.26.11.** The toolchain pin moves from go1.26.10, fixing a `net/http` advisory (GO-2026-6859).
- New Run: a main drive size typed over the pool's cap now shows "{size} is over the {cap} cap. The run gets {cap}." under the Size field as you type, and the Files summary shows the size the run gets, as Review does.
- `POST /api/v1/runs/{id}/exec` on a background run answered "no SSH access". A background run (`experience: background`) still answers `409 run_exec_unavailable`, and the message now says to create the run with `experience: interactive` to use exec.
- A live egress edit (`PATCH /runs/{id}/egress`) that the run's substrate cannot apply, such as a Kubernetes run whose proxy cannot be replaced, now answers `egress_edit_unsupported`, and its audit row says the same. Before, it answered the revive reason `revive_unsupported`.
- A No network launch of a run that calls a model is now refused for No network before any model sign-in check, so an expired sign-in no longer asks to be reconnected first and then refuses the launch anyway.
- A No network launch that includes a model harness beside "none" (your own tools), such as "none" plus `codex-cli`, now answers `422 no_network_needs_egress` naming the harness's model, at create, Review and the preview. Before, create accepted it when the deployment had no model provider for that harness, and the harness could then reach no model. A launch with no model harness at all is not affected.
- Files changed in a computer folder could be lost when a run ended while they were still being copied back: a periodic sandbox sweep could stop the sandbox part way through the copy. The sandbox is now left running until the copy back finishes. Affects 0.10.0 prereleases only.
- A "both ways" computer folder's changes were dropped when its run was stopped for being idle or for reaching its maximum age. They now come back before the sandbox stops, which can add up to 2 minutes to the stop. Affects 0.10.0 prereleases only.
- A kept run's teardown at the end of its grace could stop its sandbox while another step was still using the run, and a revive that reached its final step after the grace ran out could still bring the run back. The teardown now waits for that step and is retried on the next pass, stopping the sandbox without bringing a computer folder back once it has waited 5 minutes, and such a revive is refused. Affects 0.10.0 prereleases only.
- An `AAAA` lookup for `wardyn-proxy`, which a sandbox on Kubernetes or Docker makes because its `/etc/hosts` holds only the proxy's IPv4 address, no longer writes a refused `run.dns.query` row: the proxy answers it with an empty `NOERROR` and records nothing. A lookup for any other name, and an `A` or any other type for `wardyn-proxy`, is still refused and recorded.
- A kept run whose lock was held 5 minutes past its end could lose the race with a copy back that started after the forced stop read the run, so the stop and the copy back both acted on it. The forced stop now wins or loses the run's one terminal claim, and when it wins and the stop of the sandbox fails, that stop is retried on the next watcher tick, normally within a minute. Affects 0.10.0 prereleases only.
- A copy back that found the folder busy, because a pass back was still running on the sandbox, stopped the sandbox and dropped a copy that was still going. A sandbox that can be held is now kept with a Resume until the pass finishes, and the busy visit no longer counts as a stalled pass. Affects 0.10.0 prereleases only.
- A laptop runner runs one folder pass for a run at a time and answers busy to a second. It syncs each file it writes on the laptop to disk, and saves a file changed on both sides as a laptop conflict copy before it sends the laptop's version. Update the runner to get this.
- `wardyn sync` warns once on stderr when the laptop's filesystem cannot sync a folder, since a power loss could then drop a file it just wrote.
- Where the database accepts the lock connection's keepalive settings, a lock held by a server that vanished without closing its connection is now freed in about a minute, not about two hours. A refused setting is logged once, and the lock may then stay held longer.
- On Kubernetes in server mode, a torn-down run whose proxy failed to start now has its sandbox stopped on the first pass. Before, its network could stay open past the cut-off until a background sweep, at most about two hours. Affects 0.10.0 prereleases only.
- A replacement network proxy (started when a run's proxy is revived or its egress is edited) now waits for its first token renewal before it serves, so a proxy started late, after its token was retired or revoked, never serves a byte; it exits if that renewal is refused or does not land. A renewal made while the run is busy, such as during a live edit, is answered at once, and the runner's updated lease is retried once a minute until the run is free, while the token is valid. This needs both the runner and the proxy image to be at 0.10.0-rc.19 or later; with either one older, a replacement proxy serves at once as before. A live network edit is refused, with a hint to update, when the runner or the proxy image is known to be older than 0.10.0-rc.19; a version that cannot be read is not checked.
- Two revives or live egress edits of one run on two servers, after one lost the run's lock, could let the slower one start the run's proxy again from the config before the faster one's edit, reopening a host the edit had removed. A revive now stores its config in the same write as its policy generation, before the new proxy starts, and that write lands only while the generation it read before its config is still the newest. A revive that loses that race, or that lost the lock before its replace, replaces nothing. When its policy record is undone the run keeps its current proxy. When that undo fails, recovery attempts to mark the run lost and stop its proxy. An edit whose config cannot be stored now changes nothing, where it used to stop the new proxy and lose the run. Affects 0.10.0 prereleases only.
- A revive, live egress edit or failed proxy start's containment whose run lock was lost could mark the run lost and stop the proxy of a newer revive that had taken the run over on another server. That lost mark now lands only while the attempt's policy generation is still the run's newest, and a miss stops nothing; a store error still triggers an attempt to stop the proxy. A revive of a lost or ended run whose lock was lost before its replace now gives the run back its lost mark, instead of leaving it looking live with its agent stopped. A policy generation write that answered an error is read back, and the revive tries to abandon one it wrote all the same.

### Known issues

- A forced stop of an ended run, once its lock has been held 5 minutes past the end of its keep grace, stops the sandbox. Changes not yet copied back to the laptop are lost, and the run records "not brought back". The one exception is a sandbox in the server's own Docker (not a virtual machine): if its folder record could not be read or its copy back may still be running, it is kept with its network proxy stopped, and "not brought back" may not be recorded. A sandbox on a runner, a virtual machine, or Kubernetes in server mode is always stopped. A failed stop is retried on the next watcher tick, normally within a minute.
- A copy back that finds the folder busy on a sandbox that cannot be held (a virtual machine, or an agent that cannot be stopped alone) stops the sandbox at that visit and records "not brought back". The same happens when the last pass may still be running on an older runner and the sandbox's agent cannot be stopped alone. Changes the running pass had not finished copying are lost.
- Changes not yet copied back are lost when the laptop's runner is offline at the end of the run.
- The cut-off loop for a kept run, the 7-day keep and give-up, and the "kept / deleted" view are not in this release. A revoke that fails at the end of an ordinary run has no durable retry once its sandbox stops. A forced stop keeps retrying its revoke on each watcher pass, normally within a minute, until it succeeds.
- A copy back whose result cannot be written to the guarded folder record stops the sandbox, so changes it left behind are lost.
- The first failure of a copy back that settles with an error only, such as a failed file listing before anything was counted, still stops the sandbox, and changes it did not list can be lost.
- With an older runner, the next copy back waits about an hour after one the runner did not answer, and the two can still overlap.
- Copy backs to a virtual machine are not guarded on an older runner, so one can run beside a stuck pass.
- A busy folder on a sandbox that can be held (one in the server's own Docker, or one on a runner or Kubernetes whose agent can be stopped alone) has no limit of its own: it is retried on every sweep and never given up.
- A generation bump that fails with a store error, or fails after a folder record could not be read, stops the sandbox without a copy back and records nothing on the run (only a server log line), so its changes are not brought back.
- A run whose sandbox cannot be kept, ending while another step holds the run, can stay running past its end until that step lets go, or until a sweep forces its stop after the keep grace and another 5 minutes.
- On Windows, a file replace falls back to a plain rename, with no write-through, when the folder's path cannot be resolved.
- On a Kubernetes runner that restarts while cut off from the server, a run can keep network access past its scheduled end. Its proxy stops serving at most about an hour after the server records the end, or earlier when a renewal is refused for good.
- A Kubernetes sandbox template cannot set where a sandbox runs or its runtime: a template that sets them is refused, because the network enforcement check covers only Wardyn's own placement.
- On Kubernetes, a live network edit of a running run is not supported: it is refused with `409 egress_edit_unsupported` and nothing changes. Start a new run with the narrower policy.
- The fix for overlapping network edits on Docker applies only when the server, the runner and the proxy image are all at 0.10.0-rc.19 or later. A live network edit is refused with an update hint when a part is known to be older; a part whose version can't be read (a digest-pinned or custom image, a runner that reports no version, or a runner's own proxy image) is not checked. During a rolling upgrade, update every server replica before using live network edits. A revive or admin restart whose policy narrows because org policy or the ceiling changed mid-run can still race a stale revive on an older runner or proxy image.
- After a lost database lock, a slower attempt can still stop a newer revive's proxy or sandbox. The run then has no network (it never widens), and its policy can still read as enforced until it is revived, or until a background sweep, within about 66 minutes, marks it lost or ends a run that cannot be kept.
- If a headless run that is not on a laptop runner loses its database lock while a revive or live egress edit of it is failing, and another server revives the same run at that moment, the failing attempt can end the newer revive's run: the run stops and its sandbox is removed. Start a new run.
- In a rare race between two servers, when two network edits or revives of the same run both fail, the run's displayed network policy can show a narrowing that never took effect: its proxy keeps its earlier, wider allowlist until it is next replaced. The run's network is never wider than it was before.
- If the server stops right after restarting a revived run's agent, or cannot record that the agent restarted, later network edits of that run are refused until it is revived from its page.
- If a run's lost mark cannot be written after its database lock was lost, the server tries to stop its proxy anyway, even when a newer revive has taken the run over.
- On Docker, a late network edit can remove the proxy that a newer edit of the same run just started. The run then has no network (it never widens), but its policy reads as enforced until a background sweep, within about 66 minutes, marks the run lost (revive it from its page) or ends a run that cannot be kept (start a new run).

### Before you upgrade

- This release adds a database migration (0276, forward only). Once it has run, the previous release candidate will not start on that database, as the previous one did to the one before it, so going back needs a database restore. Take a database backup first.
- Update the server first, then every laptop runner. A runner from this release answers busy to a copy back it is still running, which an older server reads as an error and answers by stopping the sandbox. Most folder sync fixes in this release run in the runner, so a laptop with an older runner keeps the older behaviour.

## [0.10.0-rc.18] — 2026-11-10

### Added

- DNS queries from a run's sandbox are recorded as `run.dns.query` audit rows with name, type and outcome (repeats folded, at most 64 rows a second per proxy; rows over that are dropped and counted in the proxy's warning), on Kubernetes, Docker and VM runs. On Kubernetes and Docker the agent's nameserver is now its own proxy, which refuses every query. On Docker the proxy is the nameserver through a read-only `/etc/resolv.conf` the driver mounts from a per-run Docker volume. Covered by CI tests on runc (every pull request) and gVisor (nightly); Kata and krun use the same mount but are untested. It needs Docker Engine 26 or later and Docker API 1.45 or later; on an older engine run with `WARDYN_ALLOW_HOST_REACHABLE_RUN_NETWORK`, or a client pinned to an older API, lookups fail at Docker's resolver as before and write no row. VM runs keep the relay's answers.
- `wardyn record save <run-id> --name <n> --template` saves a synthesized sandbox profile as your own template, which a member may do. Without it, a member's save is refused with a line that names the flag, because stored policies are an administrator's. The template carries exactly the settings the recording decides (allowed domains, open egress, first-use approval, minimum confinement class, eligible grants, auto-stop), empty or off values included, and keeps the allowed methods a narrowed proposal lists. It is bounded by your own ceiling when a run uses it.
- The `otlp` audit sink takes `"traces": true`: each run-scoped audit row is also sent to `<endpoint>/v1/traces` as a span, and joins the caller's trace when the run's correlation id is a valid W3C `traceparent`. Off by default; see [monitoring](docs/operations/monitoring.md#opentelemetry-logs).
- `POST /api/v1/runs/{id}/exec` (SDK `ExecRun`) runs one command in a running run over HTTP, as an SSH exec runs it, and answers its exit code, its output (the first 1 MiB of each stream) and whether it timed out (`timeout_sec`, 60 by default, at most 300). A run whose `data_out` rule is `review` or `blocked`, or cannot be read, refuses it `409 data_out_blocked` before the runner is called, and the refusal is audited. A run whose governance profile denies interactive sessions refuses it too, as SSH does. A command holds one of the run's SSH channels while it runs (`429 run_exec_busy` when they are all in use) and keeps the run from idling out. Each command writes a `run.command.exec` audit row with the command (its first 4 KiB) and the exit code, never the output; a command over 128 KiB is refused `400 run_exec_invalid`. When the timeout passes the command's stream is closed, and the command may keep running until the run ends. An automation can now create, execute, inspect and destroy a run without SSH.
- Strict audit (`WARDYN_AUDIT_STRICT`, flag `-audit-strict`, off by default). With strict mode on, a covered change is refused if its audit intent row cannot be written first. Covered changes are the POST, PUT, PATCH and DELETE routes under `/policies`, `/governance`, `/approvals`, `/permissions` (grants, enforcement and availability), `/drives/grants`, `/wall-presets`, `/templates` and `/me/walls`, less the read-only checks. A refused change answers `503 audit_unavailable`. The handler's own detailed row can still spool.
- New Run, Tools: the Wall preset control lists No network when the server offers that preset, and stacks its four options on a phone. A create refused with `no_network_needs_egress` holds Launch and links to the Tools step.
- Each proxy now reports, once it serves, the SHA-256 of the exact config bytes it loaded, its version and the deployment switches set in its environment (`WARDYN_LLM_SCAN`, `WARDYN_MODEL_USAGE`), and retries until the control plane accepts the report. The Docker and Kubernetes drivers, runners included, return the SHA-256 of the final bytes they handed the proxy on every create and supported revive, and each create also returns the address the agent's proxy is pinned to.
- Experimental: with the agent-sandbox backend on, `k8s.agentSandbox.template` (`WARDYN_K8S_AGENT_SANDBOX_TEMPLATE`) names a platform-owned `SandboxTemplate` in the runs namespace. The agent pod takes only its annotations under `sandbox-template.wardyn.dev/`; claims and warm pools are a written design, not code. Placement comes only from Wardyn's own settings (where the boot canary checks NetworkPolicy enforcement), and a template's own placement fields are refused: node selectors, affinity, tolerations, topology spread, priority class, scheduler and RuntimeClass. Any other pod field is refused too, pod labels and resources included. The refusal comes before the `Sandbox` is created, and boot refuses a template it cannot read or accept. Install only the template's CRD from agent-sandbox v1.0.9, not the extensions controller. The agent-sandbox page says why claims and warm pools wait.
- A fourth built-in wall preset, `no-network` ("No network"), for a run with no network at all: `"wall_preset": "no-network"` on `POST /runs`, `--wall-preset no-network` on the CLI. A launch that would need a host is refused `422` `no_network_needs_egress`, naming each source, at create, Review and the policy preview alike; a harness whose model is reached over the network is refused. The run's proxy refuses every request (`policy:no-network`) while still reaching the control plane, the seal empties the allowlist, and every approval for the run is refused. A local server launched through a package fetcher (npx, uvx, docker run) counts as needing a host unless told not to install (`npx --no-install`, `uvx --offline`); launching its installed program directly needs none.
- A policy switch, `refuse_ip_literals`: under `allow_all_egress`, every IP-address host is refused (`policy:ip-literal`) in any spelling, through `CONNECT` and plain HTTP. An IP address listed exactly in `allowed_domains` is still allowed. An organisation default or a ceiling that sets it holds every run under it.
- A host rule can hold a request for a person: `effect: hold` in `host_rules` raises a `tool_call` approval naming the host, method and path. An approval lets that host, method and path through for the rest of the run, whatever query or body it carries. A decline refuses it for the run. On a host a `via_org` route serves, the answer is kept by the organisation's engine, and once that engine is let go (after 10 idle minutes, for example) the request is asked about again. Approvals and the run page's live approvals strip show such a request on its own card, naming the host, the method and the path. Any other rule set that denies the request still refuses it. See [POLICIES.md](docs/POLICIES.md) → "A host rule hold".
- A running run's egress can now be narrowed at once with `PATCH /api/v1/runs/{id}/egress` (`client.EditRunEgress`): remove allowlist entries, or deny hosts. The edit never adds a host; adding one to a running run is still an approval. A removed host is also denied for the rest of the run. The run's proxy is replaced, and the answer comes only after the new proxy confirms the edited policy it loaded. If it does not confirm, the new proxy is stopped and the answer is `503 egress_edit_unconfirmed` once that stop is confirmed, or `503 egress_edit_containment_unresolved` while it is not (the stop is retried). If another revive replaced the edit's proxy start first, the answer is `503 egress_edit_superseded`: that revive starts from the edited config when the edit stored it, and otherwise the edit was not applied and must be sent again. One edit per run at a time. Runs on runners or proxy images older than 0.10.0-rc.18 are refused before anything changes. Open connections drop when the proxy is replaced.
- Each proxy start now confirms the policy its proxy loaded. The driver reports the SHA-256 of the config bytes it handed the proxy, and the proxy reports the SHA-256 of the bytes it loaded. The start is enforced when the two are equal. The proxy is stopped when the driver sends no hash, when the hashes differ, or when no report arrives within 60 seconds of the driver's answer. The run read shows the result as `policy_readback`. Runners and proxy images older than 0.10.0-rc.18 run as before, unconfirmed. A proxy image named by digest alone, untagged or `:latest` names no release, so the server reads it as current; on a runner, the runner's version decides. A runner built from a source checkout, with a `+<commit>` suffix on its version, reads as the release it was cut from. Upgrade note: move `WARDYN_PROXY_IMAGE`, and any runner's `--proxy-image` pin, with the server. A proxy from before 0.10.0-rc.18 read as current never reports, and its runs fail at the deadline; the failure hint names this as one possible cause. A revive that must confirm, on a substrate that cannot report the hash, is refused before anything changes.
- Per-host method and path rules, `host_rules`: a list of rule sets, each `{"rules": [{host, methods, paths, effect}]}` with `effect` `allow`, `deny` or `hold`. Every set that names a host must allow a request to it; inside a set the first matching rule decides, and no match denies. Layers keep their own sets, so a ceiling's or a restriction's rules apply beside the run's own. Paths are matched case-sensitively, as written after one decode of unreserved escapes; a path with an empty, `.` or `..` segment, a `;` or `%3B`, an encoded `/` or `\`, `%00` or a control character is refused, as is one that matches a denied path only when case is ignored, and a request carrying a method override naming another method or a path, prefix or host override header, or declaring one as a trailer. Both are fixed lists (`X-HTTP-Method-Override`, `X-Method`, `Forwarded`, `X-Original-URL`, `X-Forwarded-Host`, `X-Host` and the like; a header outside them that a host honours is not caught), matched in any spelling of case, `-` and `_` (the git routes `/wardyn/gh/` and `/wardyn/git/` drop those headers and send the request on, and a model provider's `extra_headers` may not name one). The proxy intercepts TLS to a rule host on port 443 and refuses a `CONNECT` it cannot open (`policy:host-rule-needs-inspection`); a refused request is `403` `policy:host-rule`. A VM run with host rules is refused `422` `host_rules_need_inspected_vm`. Requests the brokered routes, the API doors and `via_org` routes send are checked against the rules too, per request. A runner too old to carry host rules is refused a run that has them. Across sets a deny wins, then a hold asks a person, then the request is allowed. A run's policy explanation and provenance list each rule set as its own entry, so a layer's added set shows as its own change.
- The console shows the `refuse_ip_literals` switch in a run's Policy tab, in words and in its change rows, and New Run's Review lists it as a run-document field when the draft sets it. Templates carry `host_rules` and `refuse_ip_literals`. The console has no dedicated display for `host_rules`; policy documents show it as raw JSON under Other settings.

### Changed

- The AI gateway token format is stable as format `1`: within it claims are only added, never removed, renamed or re-typed; tokens stay ES256 with the key's `kid`; the key set's address is stable through 0.11; a format 2 is announced a minor release ahead. See [the claims](docs/operations/ai-gateway.md#the-claims-a-gateway-receives).
- Kubernetes: the NetworkPolicy egress canary now re-runs every 10 minutes in each runs namespace, not only at boot. The first check that shows egress through a deny-all policy refuses new sandboxes in that namespace, and three checks in a row that cannot tell refuse them as not proven; running ones keep running. `/healthz` adds `network_policy_checked_at`, the time of the check that produced the verdict, and reports `network_policy` as `refused` or `not_proven` after a failed re-check. The setup page's egress-containment row shows the verdict and that time. The prewrapped image check follows the same live verdict. Each change of the verdict, either way, writes a `k8s.netpol.update` audit row. A Kubernetes runner reports its verdict again after each re-check that changes it, so wardynd's `/healthz` shows the worst of its own and its connected runners', and a change on a runner writes the same row naming the runner. `/healthz` adds `network_policy_source`, set to `runner` when the verdict it shows is a connected runner's, worse than wardynd's own; the console then names the runner instead of this cluster. A runner's first verdict that is not `enforced` writes a `k8s.netpol.fail` row naming the runner.
- The console follows that check. While its tab is visible, it re-reads the network-confinement posture every 30 seconds and when you return to the tab, and keeps the last answer through an outage. In Admin view, a check that refuses new sandboxes shows a danger band with the check's time. Every role now sees a mark on a run's confinement chip when confinement is refused or not proven.
- A run adds a workspace's clone host only for a repository it clones. A workspace attached for its folders alone (`repos: []`) no longer opens its repositories' hosts, at create, Review and the preview alike. A run that needs a forge it does not clone from keeps it once that host is added to the workspace's approved egress.
- The console names each page in the browser tab and keeps a custom brand name there. Moving to another page puts keyboard focus on its heading, or on the page itself while the heading has not drawn yet, unless the page has placed focus already; a redirect leaves focus where it was.
- Accessibility pass on the console: row buttons, search boxes and table headers have names, form errors are announced and tied to their field, page tabs expose their panel, and keyboard focus returns to a sensible place after a dialog closes.
- Search and filters on Audit, Workspaces, Secrets, Policies and Credentials are kept in the address, so a reload or a shared link keeps them.
- Removing an allowed or denied host from a workspace now asks first and confirms with a toast, and the host list says a change reaches new runs only.
- Approving or denying on the run page shows the same confirmation toast as Approvals; Cancel on a run that has not started asks a plain question; Deny says it needs a reason.
- The console calls the minimum barrier setting "Barrier floor", and more screens share one local-time format. A team drive's Mode reads Writable or Read only only when every allocation is listed and agrees, and Not shown otherwise.
- The Secrets page and the Add and Rotate secret dialogs make no claim about Git token handling until the token broker setting is known, and say a run can deliver a stored value into its sandbox.
- The GitHub API door (`github_capabilities`) refuses a request carrying any method override on the fixed list host rules use, in any case and with `-` or `_`. `X-Method`, `X-MS-HTTP-Method`, `X-Original-Method` and `X-Forwarded-Method` join `X-HTTP-Method-Override`, `X-HTTP-Method` and `X-Method-Override`.

### Fixed

- A run's proxy on a newer runner posted its loaded-config report to an older control plane, which has no route for it, every 30 seconds for the run's life. It now logs once and stops.
- A run whose proxy failed to confirm its policy could have its sandbox stopped before its computer folder's changes were copied back, or while the sandbox was kept so that copy could resume. Its proxy is stopped first now, and the sandbox stays for the copy back.
- An ended run's page, and `wardyn folder`, no longer say a computer folder's sandbox copy was deleted. They now say the copy may no longer be available.
- macOS VM runs could stop at the login window, a regression in rc.17. The rc.17 step that ended Setup Assistant at run start killed it while `agent` could still be signing in, most likely ending the sign-in, and auto-login runs once per boot. The step is now off by default, so the Apple Account pane can stay up in the window. A base can turn it on with the file `/Library/Wardyn/setup-assistant-quit`: the step then waits until the desktop is up (Dock and Finder running as `agent`) and asks Setup Assistant to quit with SIGTERM; it does not force-kill it. A run with a desktop whose guest is not signed in as `agent` 5 minutes after boot now fails with "the Mac guest never reached the agent's desktop: still at the login window" instead of running. Run the tools update once to get the new guest agent. Not yet tried on a real Mac. To check on your Mac: after boot, `stat -f%Su /dev/console` prints `agent`, and the desktop window appears.
- A security admin's Drives page now shows a team drive as one (the Team badge, One host only, and whether members can write), and every reader of `GET /drives` gets the deployment's `team_drives` switch in the answer. Before, the console had to read the switch from a page only an administrator may open, and showed a team drive as a plain one when that read was refused.
- The docs said the gateway gets no person or run identity and that a service has no check from outside the sandbox. Both are corrected: a gateway can receive a signed person and run token, and `health.probe: control_plane` is Wardyn's own check.
- On a macOS or Linux VM run, a guest program's direct requests to a host whose credential your organisation supplies through Wardyn (a `via_org` credential) no longer count as idle activity, so they no longer keep the run from stopping when idle. The runner marks such a request when it forwards it to your organisation, and the server only trusts the mark on a VM run's forwarded request from that run's own runner; a mark the guest sets itself is removed. The record a proxy writes when it starts with an upstream proxy no longer counts as activity either, so the server rebuilding its side of the forward after 10 idle minutes does not restart the idle clock. The runner and the server both need the upgrade. This was a Known issue in rc.17.
- A Kubernetes runner no longer stays offline for good after a run on it ends while it is away, for example a run killed before the control plane restarts. Every reconnect failed with "runner: this substrate cannot keep an ended sandbox". The upgraded runner now turns down that request and connects. The control plane, after a restart, removes a finished run's pod on any runner instead of asking the runner to keep it. Once a Kubernetes runner has connected, the control plane also stops asking it to keep a run's pod, and a run that reaches its end there is stopped and shown as not kept. That last part works only on the control-plane process the runner is connected to. Another process, when you run more than one, or any process before the runner first connects to it, can still ask, and can still show such a run as kept until the process the runner is connected to stops it. Upgrade your Kubernetes runners together with the control plane: an older runner can still get stuck this way. A runner already stuck comes back once it is upgraded.
- An SSH key's private key could reach the sandbox when the control plane's answer to a credential request arrived cut short or over the size limit, instead of being withheld. Such answers are now refused, and so is any credential answer that does not name its kind. Affects 0.10.0 prereleases only.
- A run could not be revived when the person was under a restriction that sets push rules or tool rules and the run had a repository or more than one agent: the revive failed with a config error. The restriction's rules are now applied to each repository's push rules and each agent's tool rules, and the revive goes through. Affects 0.10.0 prereleases only.
- A rule that sets who approves held `kubectl` writes and leaves the run's owner out now stops the owner from approving them when it does not also require a second person; the owner had been able to approve their own run's write.
- The governance writes guide now says that a restriction a person gains after their run started applies when the next start or revive resolves it: a new group restriction needs the owner's own sign-in, and an admin's revive does not find it.
- A run that waited an hour or more in a runner pool's queue failed at start on a self-hosted runner, and on your own executors could be cut off about a minute after starting: its token's renewal stamp still dated from when it joined the queue. The start now stamps it fresh.
- After a host reboot, one failed read of a kept interactive run's record could end the run and remove its sandbox. The check now waits for the next tick.
- Docker runs set up the recording folders with `mkdir` and `chmod` run as root by name, so an image that puts a folder the agent can write at the front of its PATH (the full image does) let the agent run its own file as root; they now run by absolute path with a fixed system PATH.
- Removing a Docker run's agent container left behind the anonymous volumes of an image that declares a `VOLUME`, with whatever the agent wrote in them; they are now removed with the container, and the same goes for the short-lived drive-seed, architecture-check, VM image-export and environment-build file-read containers. A kept (ended, revivable) container and your drives keep theirs.
- A read-only workspace folder was not forced recursively read-only, so a disk mounted inside that folder could be writable in the sandbox; it now is on runtimes that support it, and the runner logs a warning where one does not.
- A Docker run with a disk limit could write past it into a folder its image declares as a `VOLUME`, because a volume is not part of the limited container layer; the run is now refused when a limit set by policy meets such a folder (with a message naming it), and a default or requested limit starts with a warning. Reviving a kept sandbox is checked the same way before it starts, but a sandbox kept from before this release with any disk limit is treated as policy-limited: if such a folder is uncovered, the revive is refused (start a new run) rather than warned.
- New Run: Review's Network line, and the Network row of Effective policy, now read "N hosts listed · other public hosts allowed unless blocked" when egress is open, instead of a bare host count.
- New Run: Review shows "Barrier · The virtual machine" for a Linux or macOS VM, and the Barrier row says The virtual machine, instead of a container barrier chip.
- New Run: a main drive size kept from a template, a run again or a pool change that is over the pool's cap now says "{size} is over the {cap} cap. The run gets {cap}." under the Size field, and the Files summary shows the size the run gets and no longer shows the check. Review shows that size too.
- New Run: when the chosen pool cannot run macOS VMs, "Pools that offer macOS" lists the pools that can, as buttons. Picking one chooses that pool and macOS, with the CPU and memory minimum of the picked pool.
- An open terminal, web-app session or VM desktop session now ends when the API token or CLI sign-in that opened it is revoked, expires or is signed out (a terminal within 30 seconds); before, only a person-level revoke ended it. A token presented directly on the terminal socket is still checked at the handshake only, and terminals and web-app sessions opened before the upgrade keep the old behaviour until they end. A migration (`0274_attach_ticket_credential`) adds two nullable columns to `attach_tickets` and deletes the dead access-token rows earlier CLI refreshes left in `api_tokens`. A terminal or relay session opened with an admin's `cli_admin` step-up is bound to the step-up's own token, so revoking that token ends it.
- Revoking a CLI sign-in's access token, or demoting a person on the People page, now ends their CLI sign-ins, so the next refresh can no longer mint a token with the old user type and groups. They sign in again from the browser.
- A CLI refresh no longer leaves a revoked access-token row behind, so the token lists and the role-mapping edit no longer read one dead row per CLI user per hour.
- Anyone who knew a CLI session id could use up that computer's hourly refresh allowance by sending bad requests; only a request with the right device proof now counts against it.
- `install.sh` with no `WARDYN_VERSION` now installs the release it was downloaded from (the release job stamps it, as it does the CLI installers), and a raw checkout installs the highest version not marked pre-release instead of the first row of the releases list, which could be an older line published later. The Helm install snippets in the README and in `docs/VERIFY.md` take the release GitHub marks Latest.
- `scripts/verify-release.sh` now downloads every release asset and checks each against the signed `SHA256SUMS`; before, it checked two of them, so a swapped binary, package or installer script passed. It also fails on a failed download and on any release asset the list does not name.
- The `:latest` tag of the `wardynd` image no longer moves back to an older commit when an earlier push's checks finish after a newer one's, and the manual image-publish workflow now writes only the old 0.0.x to 0.4.x backfill tags, so that workflow can no longer overwrite a release image tag or `latest` by hand.
- The release build no longer runs a binary built from the source tree in the step that holds the release signing identity or the Apple signing and notary credentials, and builds the vscode, novnc and novnc-signin images on the exact digest of the base image, checked to be signed by this release workflow at this tag.
- One person could hold every authenticated SSH gateway connection, refusing everyone else's SSH access until wardynd restarted, and deleting their keys or revoking their sessions did not free them. One person now holds at most 8 with single sign-on, or all of `WARDYN_SSH_MAX_CONNECTIONS` without it, and `WARDYN_SSH_MAX_CONNECTIONS_PER_PRINCIPAL` sets it. A connection over the share holds no established-connection slot (its refusal work still counts against the handshake and per-source bounds, whose slots are released when its handler returns, after its refusal row is written), is told why if it opens a channel within 5 seconds and is one of the few (one per person, 8 in all) kept waiting, and is recorded as `ssh.connection.reject` and counted in `wardyn_ssh_connections_refused_total` as `principal`, or as `established` when the share equals the whole ceiling (the default without single sign-on). Each connection's key is rechecked every 30 seconds, and the connection is closed when the check finds the key deleted or the session revoked.
- Uploading, downloading or sending for review a desktop file whose name has a space or a non-ASCII letter next to `(`, `)`, `!`, `*` or `'`, such as `report (1).pdf`, used the percent-encoded name: the upload landed under the wrong name and the download answered 404.
- Rotating through more address keys than a per-address rate limit's table holds gave each visit a full burst, so the sustained limit stopped applying (anonymous sign-in and enrolment routes, and the admin token's failed-attempt limit). A full table now keeps its spent entries and charges new addresses to one shared bucket.
- Profile synthesis from a run's audit trail is limited to two at a time per replica and answers `429` `profile_synthesis_busy` beyond that. Finding a run's node for the kernel check now reads its newest kernel events, not the whole trail.
- Saving a workspace's approved or denied egress list from a page read before an "always" decision was made dropped that decision. `PUT /workspaces/{id}/approved-egress` and `denied-egress` now answer `412` `workspace_update_conflict` when `If-Match` names an older `updated_at` than the workspace has; a request with no `If-Match` is unchanged.
- On a Kubernetes cluster, a network-confinement check whose request timed out could leave its test pod or deny-all policy behind. The check now attempts cleanup even when a create request times out.
- The setup page's "Not proven" row for network confinement no longer says the last 3 checks could not run when the checks did run. It now says the canary has not proved confinement and points to the API, pod permissions, the pod network and an existing default-deny NetworkPolicy.
- After signing out, the sign-in page's browser tab keeps its plain Wardyn title instead of the last page's name and the Admin cue.
- A failed SSH agent request from a sandbox no longer shows the sandbox the control plane's address or its response text.
- The console scrolls inside its shell, so a short window or a stack of banners no longer hides the page; long names in dialogs, drawers, tables and the sidebar now shrink and break instead of overflowing.
- When a list or settings card fails to load, the console shows the server's own reason (or a neutral line), and no longer shows a failed read as an empty list.
- New Run: pressing Escape in a text field no longer leaves the page, and leaving macOS drops the CPU and memory minimums a pool pick had set.
- The brokered model routes (`/wardyn/llm/anthropic/`, `/wardyn/llm/openai/`) now refuse a request whose upstream host the run's policy denies, with the same `403` as a refused connection. Before, a local model with no auth configured was still reached after its host was denied, because the broker sends it no credential to withhold.
- A request with no body left to read, such as a `GET` or a request whose body was already checked, could fail when its answer took more than five minutes after a hold or credential step. That covers inspected hosts (Azure DevOps sign-in and approval holds among them), the Azure, AWS and Kubernetes doors, gateway identity tokens and the organisation's git route. It ended with a `502` or an empty answer, and later requests on the same connection failed the same way. Such a request now keeps its connection; a request whose body is still arriving keeps its fresh read time.

### Security

- The `git_pat` broker route (`/wardyn/git/<host>/`) now judges the whole request path, not only its last segment. Before, a forge API path that happened to end in `git-upload-pack` was forwarded with the PAT, so on Gitea or Forgejo a run could write a file to any branch past `api: false`, `access: read` and every push rule. A path that reaches the forge's API mount is now refused, under a deployment prefix too: `api/` or `api/<version>/`, Bitbucket Server's `rest/<name>/<version>/`, Bitbucket Cloud's `!api`, Azure DevOps' `_apis`, and GitLab's `-`; on `gitea` also any `api` before the repository name, and on `generic` also `_api`. Wardyn has no setting for a forge's deployment root, so a GitLab repository under subgroups named `api/v<digits>/` or `api/graphql/` is refused too. A path with an empty segment, or still holding a percent escape after decoding, is refused. A `generic` grant is judged only by these known mounts, so set `forge` or `repos` for another forge. On `bitbucket_server` the path must be `[<context path>/]scm/<project>/<repo>`, and a context path now works with `repos` too. On Azure DevOps Services the Entra sign-in route now reaches git only at `[<project>/]_git/<repository>`, as on a Server. Both git broker routes also stop forwarding any query beyond the refs discovery `service`, and they and the API doors stop forwarding method- and path-override headers (`X-HTTP-Method-Override`, `X-Original-URL`, `X-Rewrite-URL`, `X-Forwarded-Prefix` and the like).

### Known issues

- Computer folders that sync both ways still behave as in rc.17 when a run ends. A run's changes can be lost in these cases:
  - a periodic sandbox sweep stops the sandbox while its changes are still being copied back;
  - a run stopped for being idle or for reaching its maximum age;
  - two copies back running at once for the same folder;
  - a crash during a copy back. After a crash, the laptop's older copy of a file can overwrite the agent's newer edit.
  Copy changes off the sandbox before a run ends if you cannot lose them. The fixes are being prepared for a later release candidate.
- After two copies back of the same folder run at once, or a crash during one, the folder's sync record on the laptop can become unreadable. Every later sync of that folder then fails with `sync state … is unreadable (delete it to start over)`. To recover, first copy anything you need off the sandbox: on the first sync after the record is gone, a file that differs on the laptop and in the sandbox is replaced in the sandbox by the laptop's copy. Then delete the file the message names: `folders/<run id>.json` in the laptop runner's state directory, which by default is `~/.config/wardyn/runner` on Linux, `~/Library/Application Support/wardyn/runner` on macOS and `%AppData%\wardyn\runner` on Windows, or the runner's `state_dir` setting. The next sync then starts over as a first sync.

### Before you upgrade

- The AI gateway identity token's `wardyn_format` claim is now `1`, where it was `experimental-1`. A gateway rule that checks `wardyn_format` = `experimental-1` must accept `1`.
- Kubernetes: each run's proxy pod now sets the namespaced sysctl `net.ipv4.ip_unprivileged_port_start=53` to serve DNS. A cluster whose admission forbids pod sysctls in `k8s.runsNamespace` fails every run at proxy start; allow that sysctl before upgrading. See the [Helm prerequisites](deploy/helm/wardyn/README.md).
- Split-role installs (`WARDYN_PG_MIGRATE_DSN` set) whose app role does not pick up new tables through the `ALTER
  DEFAULT PRIVILEGES` step in Operations: grant the app role `SELECT, INSERT, UPDATE` on
  `run_proxy_starts`. Without it, every run fails at create because its proxy start cannot be recorded. An upgrade
  from before 0.10.0-rc.17 also needs the 0.10.0-rc.16 grants: `SELECT, INSERT, UPDATE, DELETE` on
  `governance_unit_deciders` and `governance_envelopes`. An upgrade from
  before 0.10.0-rc.14 also needs every grant the 0.10.0-rc.14 notes list: `SELECT, INSERT, UPDATE, DELETE` on
  `run_create_keys`, `wall_presets`, `kept_machines`, `workloads`, `workload_bindings`, `workload_run_ends`,
  `macos_vm_disks`, `aws_sso_refused_tokens`, `cli_sign_ins`, `ui_desktop_closed`, `desktop_signins`,
  `desktop_signin_shares`, `desktop_signin_runs`, `approval_rules`, `governance_restrictions`,
  `governance_restriction_members`, `governance_unit_admins`, `oidc_session_sids` and `desktop_vm_sign_ins`,
  `SELECT, INSERT, UPDATE` on `groundtruth_sensors`, `runner_health`, `launch_preset_version_floor`, `cli_sessions`
  and `governance_boundary` (and on `run_harnesses` if it turns on `WARDYN_MULTI_AGENT_TASKS`), and `SELECT, INSERT`
  on `audit_checkpoints` and `run_model_usage`.
- A model provider whose `extra_headers` names a method override or a path, prefix or host override (see `host_rules` under Added) is now refused when it is saved, and while any stored provider carries one, a whole-document `PUT /site-config` is refused `400`. One already stored makes the proxy of each run that uses it refuse its config, so the run does not start. Before upgrading, rename or remove that header in the model provider. A revive loads the config the run was started with, so a run started before the rename is refused `409` `revive_config_does_not_load` on every revive: start a new run instead.

## [0.10.0-rc.17] — 2026-11-09

### Before you upgrade

- Walls that name a port other than 80 or 443 of a host the walls above them name bare no longer reach that host, on any runner. Before upgrading, name that host with that port in the walls above.
- A migration (`0273_governance_elevation`) adds `governance_memberships.expires_at` and allows `elevation` as a run's lifetime-cap source. No existing row changes.
- Unit admins' `GET /audit` now includes governance rows about their units (see Changed).
- A held change to a unit's own walls, locks, mode, name or parent is now decided only by a security admin or an admin of a unit above it, never by an admin granted on that unit itself, and a move needs an admin above the new parent as well. Changes already waiting are decided under this rule. Where a unit's admins are granted only on that unit, these changes now go to security.
- macOS VM runners: upgrade the runner, then run `wardyn-runnerd macos-vm update-tools` once, so the base gets the new guest agent (the small Wardyn program inside the macOS VM). Most macOS VM changes below need both.

### Before you downgrade

- An older server ignores `governance_memberships.expires_at`: an elevation approved before the downgrade becomes a permanent membership, and pending elevation requests are shown to nobody. Delete elevated memberships (`expires_at IS NOT NULL`) before downgrading.
- An older server shows pending image requests to nobody. Grants already approved stay.
- Replace `pr_create`, `pr_vote` and `pr_complete` (Azure DevOps) and `pr_write` and `pr_review` (GitHub) with `pr` in every provider row, policy, unit wall, person's walls, wall preset and GitHub CLI component first. An older release refuses the new Azure DevOps names, and grants nothing for `pr_write` or `pr_review` in a GitHub list, without an error.
- An older release ignores a restriction's allowed domains, push rules, desktop rules, recording floor, tool rules and allowed models: remove them, or restate them as unit walls, first.
- An older release ignores component pins and strict workspace pins: a pinned component is usable from any of a person's contexts again. It also refuses to delete a unit a component is pinned to, as still assigned: unpin components first.
- An older release ignores `data_out`. SSH file transfer, commands and port forwards, `wardyn sync` down, run output, recording replay, web apps and computer folders open again on runs that set it, and its drive, kept-machine and push denies are lifted. Set `deny_interactive` and the matching walls first, or stay on this release.
- An older release ignores the boundary's and restrictions' `azure_devops_capabilities` and `github_capabilities` caps: restate them in each provider row's capability ceiling and each policy first.

### Added

- `wardyn-runnerd macos-vm update-tools` starts a tools update of the macOS VM base now, even when the base looks current. The doctor, `tools.json` and docs text that said to run the tools update again now names it. It refuses while a prepare, tools update or edit-base runs, and says to run `wardyn-runnerd macos-vm prepare` first when the Mac has no ready base. Not yet tried on a real Mac; to check it, run it on the runner and watch `status` and `tools-update.log`.
- On Approvals, a group decider approves or denies an image request under Waiting on you, with the person, the image and its full digest, and what approving grants.
- On Approvals, a person whose default profile offers image requests can ask for one image by digest and follow it under Your requests, one card for elevation and images. Governance › Changes shows an image request with the person, the full digest and what approving writes. Your requests now also lists withdrawn and not-applied elevation requests.
- On Approvals, a person whose unit offers elevation can request it (unit, how long, why, and an optional ticket), follow it under Your requests and withdraw it. A group decider can approve or deny elevation requests under Waiting on you.
- Governance › profile has a Deciders section: who besides security admins decides each kind of hold for runs in that profile. The rules above it show, and a lock above it makes them read only.
- The Approvals screen has a Waiting on you tab for people a unit's deciders rule lets decide other people's holds; pending cards say who decides, a decider's card says who asked, decided rows say on what basis, and the Approve dialog offers no scope wider than the rule allows.
- A new page, Keep run branches from starting pipelines ([docs/operations/ci-trigger-guard.md](docs/operations/ci-trigger-guard.md)): what to set in Azure DevOps and GitHub so a run's pushes and pull requests cannot start a pipeline unreviewed, and the Wardyn settings that hold or refuse them.
- Elevation requests: a person asks for a time-boxed membership in a unit whose deciders rule offers `elevation` (`POST /governance/requests`, kind `elevation`, with a required reason and an optional ticket). The rule's deciders approve it, never the requester; the membership ends at the approved duration, a run launched in the unit has its lifetime cap at that end, and the end is audited as `governance.elevation.expire`. Who may ask is set per rule, by group; a rule can require a ticket matching a pattern and a wait between elevations.
- Image requests: a person whose default unit offers it (an `image` deciders rule) requests one image by digest (`POST /governance/requests`, kind `image`, with a reason). The rule's deciders approve it, never the requester, and approval adds an exact allow grant of that image for that person. The digest rule, restrictions and image admission still apply at every launch.
- `data_out` (`allowed`, `review` or `blocked`) on a run policy, a unit's walls, a restriction and the Wardyn boundary: one rule for content leaving a run.
- `data_out` closes these paths for a run: Files panel downloads, SSH file transfer (`sftp`, `scp`), commands and port forwards, `wardyn sync` down, run output, recording replay, web apps other than the desktop, and a computer folder's bring-back. `review` holds a Files panel download for a person's yes; `blocked` also refuses every push and, for runs under a unit and a kept machine's governed callers, denies drives, member drives and kept machines through the switches `deny_user_drive`, `deny_member_drives` and `deny_kept_machines`; a member's own `data_out: blocked` refuses pushes only. On an HTTP path a refusal answers `403` `data_out_blocked`; a rule that cannot be read keeps the content in (SSH treats it as `blocked`, the other paths answer `503` `data_out_unreadable`). Each refusal writes a `run.data_out.deny` audit row naming the `path` and the `rule`; a web app's refusals write it within the audit rate limit shared with failed authentication. Network egress is outside the rule: narrow it with `allowed_domains`, `denied_domains` and tool rules. Reading the terminal or the screen is not covered either, nor what the desktop app's own server answers over plain HTTP: turn off interactive access for those. A run with no recorded policy carries no rule.
- In the console, a policy, a profile's ceiling and the Wardyn boundary set `data_out`. A restriction's `data_out` is set through the API (`/governance/restrictions`); the console has no restriction editor. A run that keeps its content in says so in its terminal and Files panel, the Output tab prints the rule's sentence, and the Recording tab prints the replay's refusal.
- A restriction can now carry an allowed-domains list, push rules, desktop transfer rules, a recording floor, tool rules and an allowed-models list. Each only narrows, in every unit the person runs in. A restriction that names allowed domains and not `allow_all_egress` turns open egress off for its people. A restriction also binds a run that is already running: a revive meets it again, a credential for a host outside its allowed domains is withheld at the next mint, and an approval cannot admit such a host. Absent, nothing changes.
- With `WARDYN_GOVERNANCE_SECOND_HUMAN=1`, a restriction created with an allowed-domains list, or given its first one, waits for a second person, even though it turns open egress off: there is no earlier list to judge it within, and each run's own list is met with it.
- An organisation component can be pinned to a unit: only runs in that unit or below it may use it. A workspace pin can be strict, so a person's parent context no longer runs in the pinned unit. A person's own CLI sign-in to a component is not pinned.
- `wardyn governance get`, `plan` and `set` take `--under <unit>`: the document covers that unit and the units below it, entries outside it are refused, and `--prune` never reaches past it. The document gains a `deciders` section (each unit's own rules, by unit name); a document without one leaves rules untouched.
- `GET /governance/units/{id}/model-usage` gives a unit admin, or a security admin, the token usage of the runs frozen to a unit and the units below it, by model, over a window of up to 92 days (30 by default). Tokens are shown, not cost.
- The Wardyn boundary and a restriction can cap the Azure DevOps and GitHub capabilities of every run they bind (`azure_devops_capabilities`, `github_capabilities`). A capability the cap does not list is refused at launch, at every request and approval, and at every Azure DevOps credential resolve, whatever a provider row allows. A capability added in a later release is outside the cap until you list it. Absent means no cap, so nothing changes at upgrade.
- New run asks which unit a run runs in when you have more than one, hold an elevation, or can ask for one. An elevated unit's run ends when the elevation ends, and a unit you can only ask for offers Request elevation on Review.
- Governance › Changes shows a held change's reason and ticket. Its proposer can add or edit them from the change or right after the save, and a security admin can show only the changes that only security can decide. Elevation requests read as requests, and a person's own request offers Withdraw request.
- Model providers: a new Your own endpoint provider can start from a gateway preset (LiteLLM, Portkey self-hosted, Portkey hosted or OpenRouter), which fills only the fields it names and records nothing; the editor also takes the extra headers a gateway routes on.
- Governance › Profiles marks platform roots and the profiles inside them, and a Platform envelopes card lets a security admin set, edit and remove a platform's approved groups.
- The denied components and model providers rows offer the stored components and the model providers by name, a profile without a base has both rows, and a runner pool's policy can name a unit, picked from the profiles.
- In the run's Files panel, a download under `desktop.download: review` shows its state (not asked, waiting, approved, denied, expired, released once) and the one action that state allows. Approvals shows a reviewed download as a card (file, size, SHA-256, who asked) and a held kubectl write as a card (cluster, namespace, method, path, query, content type, body, with Copy body). `call_text` on a held kubectl write is its body as text (`""` when it has none); with no text from the server the card turns Approve off.
- Settings > Runners has a Require a managed device switch for `runners.require_managed_device`; only a super admin can change it.
- A change held for approval from the Boundary, Components, Permissions, self-hosted runner, Setup access and user type screens offers Add a note, as Governance does.
- A run killed at its elevation's end says so on the run page and offers its owner Request elevation; anyone else reads "the elevation" on the pill menu, the warning and the ended banner.
- Docs: a new page, [Platform tenancy](docs/operations/platform-tenancy.md), covers the platform envelope, what a unit admin's change applies directly, and recipes. The deciders page lists what each decider kind governs and the console screens.
- Docs: the model gateways page shows how to run Codex CLI through a Your own endpoint provider that serves the Responses API, and the policies page says that a run asking `wait_for_review` under a unit set to `deny_with_review` runs at `deny_with_review`, with a warning.

### Changed

- macOS VM: edit-base no longer asks to trust the signing certificate of the guest agent for code signing, and neither does the tools update; `macos-vm status` and `doctor` no longer mention it. Full Disk Access keeps to the agent's designated requirement (its identifier and the certificate's hash), which needs no trust. "Trust the list's CAs" still trusts the install list's CA items for SSL, and tools mode still allows that trust. A base that already trusts the certificate keeps that trust; to remove it, run `sudo launchctl asuser $(stat -f %u /dev/console) security remove-trusted-cert -d /Library/Wardyn/stage/wardyn-agent-signing/item.cer` in edit-base's admin Terminal. Not yet tried on a real Mac. Retest on your Mac: edit-base's Terminal printout and Trust the list's CAs name only the CA items; with none, the button says "The install list has no certificates."; `macos-vm status` says "guest agent: signed on this Mac"; after one more tools update, the live line still reads "Full Disk Access allowed".
- A person who holds a unit admin grant sees Governance and Audit in the console: the profiles their grant reaches, read only, and their units' audit rows.
- A unit admin no longer decides a held change to the unit they are granted on: a unit's own walls, locks, mode, name and parent are decided from above it.
- Azure DevOps `pr` is now three capabilities: `pr_create` (open, update, comment on, label and abandon a pull request), `pr_vote` (vote, add or remove reviewers, post pull-request statuses, resolve comment threads) and `pr_complete` (complete or set auto-complete, without bypassing a policy). GitHub `pr` is now two: `pr_write` (open, update and comment on a pull request, request reviewers) and `pr_review` (submit, approve, request changes on or dismiss a review). `pr` still means all of them, so nothing changes at upgrade and a list that says `pr` still lets an agent approve. To stop that, name `pr_create` (Azure DevOps) or `pr_write` (GitHub) in its place; on Azure DevOps a vote inside the provider's ceiling is then held for the person.
- Narrowing a profile's denied components or model providers starts from an empty list that adds to what the base denies, instead of a copy of the base's list.
- A unit's deciders rule (set with `PUT /api/v1/governance/units/{id}/deciders`, added in rc.16) may now only narrow the rules above it for the same kind: a rule that names a decider they do not, or a wider scope, second-person, duration, eligible-requester or renew-after setting, is refused with `422` `decider_rule_wider_than_parent` at the direct write, when a held write is proposed and when it is approved. Clearing a rule is never refused. `GET /governance` adds `bounded_by` (every unit whose rule bounds a kind) and `effective` (the rule they leave) beside `from`.
- A hold is decided under every deciders rule for its kind from the run's unit up, not under the nearest unit's rule alone as in rc.16: deciders are the people all of them name, a `second_human` set by any rule holds, and the narrowest `max_scope` and `max_elevation` win. For elevation, who may ask is the groups every rule names, the longest wait between elevations applies, and every ticket pattern on the chain must match. `approval.decide` still names the nearest unit.
- A `group:` decider matches an API token only when `WARDYN_ROLE_STAMP_TTL` is set. A CLI session matches as a console session does.
- A unit admin's `GET /audit` also serves rows with no run, which rc.16 left out: only unit, membership, unit-admin, deciders, envelope and request writes naming one of their units, through a governance allow list that names a security admin only as `security` and a group decider only as `group`. They are not in `GET /audit/export`. An `actor` filter on a unit admin's feed returns run rows only, so it cannot confirm who wrote a governance row.
- Console wording: the network policy text says another host is held or refused for review unless it is always blocked; the Audit view says it lists recorded decisions, not every decision; a refusal at the run's host limit says it is that limit, not hosts waiting.

### Fixed

- Clearing a governance unit's deciders with `{"rules": []}` when the unit already has none still answers 200, but no longer writes a `governance.deciders.set` audit row, because nothing changed. With the second-human switch on it no longer raises a pending change for a second person to approve either.
- `GET /api/v1/runners/settings` now always includes `require_managed_device`, `false` included. It was left out when the requirement was off.
- On a macOS or Linux VM run, a request the guest makes without the run's proxy settings no longer counts as idle activity, even when allowed: macOS's own background requests, and tools that ignore `HTTPS_PROXY` (xcodebuild and Swift Package Manager downloads, Gradle, Maven). The harness's model calls and what goes through the run's proxy settings still count. A request to a host whose credential your organisation supplies through Wardyn (a `via_org` credential) still counts (see Known issues). A Mac runner needs the runner upgrade for this. Not yet tried on a real Mac. Retest on your Mac: leave a macOS VM run idle with macOS's own background traffic flowing, and confirm it idle-stops at its cap.
- A macOS VM run's window no longer stays on Setup Assistant's Apple Account pane: for the first 2 minutes of a run, the guest agent ends Setup Assistant in the agent's session within 2 seconds of it opening, so the pane can show for a moment. The Setup Assistant keys in the base did not stop it: the pane opened on every run. The same step also runs on a drive-open boot, where it finds nothing. Run the tools update once to get the new guest agent. Not yet tried on a real Mac. Retest on your Mac: start two fresh runs with the window open at start: the window should show the desktop within a few seconds.
- macOS VM: edit-base's Open admin Terminal and Trust the list's CAs now write their Terminal scripts to `/Library/Wardyn/edit`, under the root:wheel directory stage 1 makes, instead of `/private/var/run/wardyn-edit`. A stock guest has `/private/var/run` as root:daemon 0775, which the guest agent's root-only write check refuses, so both buttons answered "the Terminal script was not written" on every click. When a button fails, the window now shows the guest's reason instead of "did not reach the guest agent". Run the tools update once to get the new guest agent. Not yet tried on a real Mac. Retest on your Mac: in edit-base, click Open admin Terminal; Terminal should open in the VM at `su - wardyn-admin`. If a button fails, send the window's line.
- macOS VM: Trust the list's CAs now runs each `security add-trusted-cert -d -r trustRoot -p ssl` through `launchctl asuser` for the console user, after `su - wardyn-admin` and `sudo`, so macOS can show its dialog for approving an admin trust on the guest's screen. From the `su` shell alone macOS answered "authorization denied since no user interaction possible". The command edit-base prints for a trust by hand runs the same way. Not yet tried on a real Mac. Retest on your Mac: click Trust the list's CAs, check each subject and SHA-256, and type the code when `su` and `sudo` ask. In macOS's dialog, enter wardyn-admin and the code. The window's live line should then read "certificate <name> trusted". If no dialog shows, send the Terminal's output.
- macOS VM: closing edit-base's window now asks the guest agent to shut the guest down as root (`shutdown -h now`) instead of sending the VM's stop request, which made macOS ask "Are you sure you want to shut down your computer now?". A guest agent without this, or a second close, still sends the stop request; the window says to click Shut Down if macOS asks. Not yet tried on a real Mac. Retest on your Mac: after the tools update, close the edit window; the guest should shut down with no dialog and edit-base should go on to its check.
- macOS VM runs: the guest agent now writes a run's settings file (`session-env.zsh`) and its two status files (`session-env.status`, `desktop-view.status`) to `/Library/Wardyn/run`, root's at 0755 under the root:wheel directory stage 1 makes, instead of `/var/run/wardyn`. A stock guest has `/private/var/run` as root:daemon 0775, which the guest agent's root-only write check refuses, so none of the three was written and the line in `/etc/zshenv` that sources the settings was never added: a Terminal macOS restored at login had none of the run's settings, and the control plane could not tell where they stood. The stop and boot scrubs remove the new directory, and a base item may not write it. The base gets this with its next tools update; until then the window behaves as before. Not yet tried on a real Mac. Retest on your Mac: after the tools update, start a run, open a Terminal that macOS restored in its window, and check the run env is set: `echo $HTTPS_PROXY` prints the run's proxy, and `cat /Library/Wardyn/run/session-env.status` reads `session_env=set`.
- The AWS sign-in steps in the operations guide now say what to do when the person is not already signed in to the AWS portal: its sign-in redirect drops the code from the link, so open the verification link a second time.
- A run refused for a missing model sign-in or key no longer points to a banner on every page that is not there. The refusal, and the Bedrock one for an AWS sign-in that can no longer be renewed, now say to connect in the console's User view, under Settings, in Your model connections.
- On a macOS VM base, the Safari Safe Browsing setting that macOS refuses to write now says it needs Full Disk Access for the guest agent, not the raw `defaults` "Could not write domain" error. The raw error goes to the guest agent's log. Not yet tried on a real Mac; to check it, run the tools update and read the skip reason in `tools.json`.
- On a macOS VM runner, a tools update paused until the Mac is on AC power now shows in `wardyn-runnerd macos-vm status` and `wardyn-runnerd doctor` as "tools update waiting for AC power"; it showed only in `tools-update.log`. Not yet tried on a real Mac; to check it, start a tools update on battery and read `status` and `doctor`.
- The Runs page's "Start a run" workspace picker listed no workspaces, because the page never loaded them. It now loads them when the page opens.
- The Runs list labelled every run on a named workspace "Ephemeral scratch — no repo". It now shows the workspace's name, and Group by Workspace groups by it.
- The New Run page showed an unlimited run lifetime as "596523.24 hours" for a member whose profile sets no maximum end. It now says "No maximum", as it does for the other limits.
- A run's header said "Runs on: Remote-hosted runner" even when the run was on this server's Docker host. It now says "Docker host", or "Kubernetes cluster" for a run there.
- An organisation runner pool whose only members are organisation runners, such as runners in a Kubernetes cluster, now shows as available, with those runners counted in its capacity and offered in its barrier options. Runs could already be placed on them. An organisation pool with both remote-hosted and organisation runners now stays available under a deny of remote placement, as its runs already did.
- Approving an elevation request now answers stale when the unit's elevation rule no longer names a group the person matched when they asked.
- After a revive, a run's recorded policy and its policy view showed the organisation's recording level instead of the run's own; the recording itself was never changed. A revive now starts from the run's recorded policy and is refused (503) when that cannot be read.
- A run that has reached its lifetime cap is refused at once, before the lease sweep ends it: its credential and approval requests, its token renewal and decisions on its pending approvals (`409 approval_run_past_lifetime`). A queued run whose cap passed while it waited ends without starting (`max_lifetime_reached`).
- An inbound web app now obeys the run's `data_out` rule: under `review` or `blocked`, a request with an inbound token is refused as the browser entry is.
- The Audit views labelled a held, denied or refused request "Released by approval". Only a request an approval let through reads that way now; held, denied, expired, stale, refused and host-limit requests each say so.
- A run whose policy allowed a credential's host only by a wildcard, by `allow_all_egress`, or on another port after a layer's list failed when its proxy started. Dispatch and revive now withhold that credential and record it on `run.ceiling.reassert` or `run.revive`; to deliver it, name the host exactly in `allowed_domains`. `wardyn run get` and the run page list the hosts whose credentials were withheld: the run's policy does not allow them exactly, or a wall denies them. A remote MCP server's credential stays with the broker.
- A bare entry in an allowed-domains list (a host with no port) means ports 80 and 443 only. When one wall, restriction, boundary or unit list named a host bare and another named another port of it, such as `pypi.org:22`, the run was given that port; it now gets a port only when every list allows it.
- A unit admin's change that names a port other than 80 or 443 of a host the unit's walls name bare now waits for a second person; naming 80 or 443 applies directly. An overlay that names such a port is refused instead, because the walls do not cover it.
- A workspace's generated environment build now retries a Claude Code download from downloads.claude.ai that times out, is refused, or gets a 408, 429 or 5xx answer, up to 20 retries over five minutes. The exported `.devcontainer/Dockerfile` carries the same retries. The `agent-claude-code` image also retries its Claude Code downloads.

### Corrections to the rc.16 notes

- A `custom_endpoint` provider's `extra_headers` take a name of at most 128 bytes and a value of at most 2048 bytes. The rc.16 notes said 2048 bytes for both.
- A governance change's note: another reader who can see the change gets `403` `governance_change_note_proposer_only`, a change that is no longer pending answers `409` `governance_change_not_pending`, and a change the caller cannot see answers `404`. The rc.16 notes gave `403` for every write but the proposer's.
- Reviewed downloads: the deployment's admin token can still decide a held download, as it can any approval. The rc.16 notes said nobody who created the run or asked decides it, which holds for people but not for the admin token.

### Known issues

- On a macOS or Linux VM run, a guest program's direct requests to a host whose credential your organisation supplies through Wardyn (a `via_org` credential) still count as activity, so they can keep the run from stopping when idle. A fix is planned for the next release candidate.
- A run's data_out limit stops applying on output and replay reads, web apps, SSH (file transfer, commands, port forwards and `wardyn sync` down) and computer-folder bring-back once audit retention removes the run's policy records; the Files panel refuses every transfer instead.

## [0.10.0-rc.16] — 2026-11-08

### Before you upgrade

- Before a downgrade, set any `desktop.download: review` to `blocked`: an older server can resolve `review` to allowed when it meets a unit's rules, so its Files panel could send files with no one's yes.
- Split-role installs (`WARDYN_PG_MIGRATE_DSN` set) whose app role does not pick up new tables through the `ALTER
  DEFAULT PRIVILEGES` step in Operations: grant the app role `SELECT, INSERT, UPDATE, DELETE` on
  `governance_unit_deciders` and `governance_envelopes`. Without the first, `GET /governance` fails and a unit's
  deciders cannot be set; without the second, `GET /governance` fails and an envelope cannot be set. An upgrade from
  before 0.10.0-rc.14 also needs every grant the 0.10.0-rc.14 notes list: `SELECT, INSERT, UPDATE, DELETE` on
  `run_create_keys`, `wall_presets`, `kept_machines`, `workloads`, `workload_bindings`, `workload_run_ends`,
  `macos_vm_disks`, `aws_sso_refused_tokens`, `cli_sign_ins`, `ui_desktop_closed`, `desktop_signins`,
  `desktop_signin_shares`, `desktop_signin_runs`, `approval_rules`, `governance_restrictions`,
  `governance_restriction_members`, `governance_unit_admins`, `oidc_session_sids` and `desktop_vm_sign_ins`,
  `SELECT, INSERT, UPDATE` on `groundtruth_sensors`, `runner_health`, `launch_preset_version_floor`, `cli_sessions`
  and `governance_boundary` (and on `run_harnesses` if it turns on `WARDYN_MULTI_AGENT_TASKS`), and `SELECT, INSERT`
  on `audit_checkpoints` and `run_model_usage`.
- Before you downgrade: an older binary ignores a portal's `run_in_under`, so the portal can start runs in any of the person's contexts.
- A kubectl row with `access: restart` (below) is not understood by an older wardynd or proxy image. After a downgrade an older wardynd still loads the row, because it reads `access` as plain text, but it refuses to attach or save it (`cli.kubectl.access: "restart" is not read or write`). An older proxy image refuses to start on a run config that carries `restart`. Move such rows to `read` or `write` first, and update the proxy image before using `restart`.
- Unit admins now read the audit rows of runs in the units they govern, on `GET /audit` only, with no export, without personal walls, restriction names, secret names or error text. This is on at upgrade. To keep a person out, remove their unit-admin grant.
- Migration `0269_agent_runs_unit_idx` builds its index without `CONCURRENTLY`, so writes to `agent_runs` wait while it builds, and `0271_approval_file_review` re-checks every `approvals` row under a lock that also holds off reads of `approvals`. On an install with a large `agent_runs` or `approvals` table, upgrade in a quiet period.

### Before you downgrade

- An older binary ignores `deny_components` and `deny_model_providers` on unit limits, so denied components and providers attach again.
- An older version ignores platform envelopes, so a held change of an envelope's unit, and a unit admin grant or revoke on it, can again be approved by a unit admin who governs that unit. Changes below the envelope's unit and adds of its approved groups are held again, as before.
- An older wardynd loads a kubectl row with `approve_each` but drops that field without an error, so after a downgrade the row's writes are sent without a person's yes. Turn `approve_each` off and narrow the row's access, or its service account's RBAC, first. Rows with `access: restart` fail to validate on an older binary.
- An older build ignores the `governance_unit_deciders` table, so unit deciders rules stop binding and every hold is decided as before. A unit whose rule left out `owner` has its run's owner deciding egress, Azure DevOps and MCP holds again. Clear the rules you rely on first.

### Added

- Reviewed downloads: `desktop.download` takes `review` (download only; the default stays `blocked`). The Files panel asks with `POST /api/v1/runs/{id}/desktop/files/{name}/review`, which raises a `file_download` approval naming the file's name, size and SHA-256 and who asked, never its content. The security tier decides it, or a unit's `file_download` deciders; nobody who created the run or asked decides it, and a decision is always once. `GET .../files/{name}?approval=<id>` then sends that file once, after hashing it again: a file that changed is refused with `409 file_changed_since_review` and nothing is sent. A reviewed file is at most 256 MiB. `desktop.file.transfer` rows name the `review` step. Strictness is `allowed` < `review` < `blocked`, so a layer below can only make download stricter, and an unknown rule now reads as blocked.
- `PUT /api/v1/governance/units/{id}/deciders` sets a unit's deciders: who besides the security tier may decide each kind of hold (`egress_domain`, `ado_escalation`, `mcp_call`, `kube_call`, `credential`, `push_content`, `elevation`, `image`, `file_download`) for runs launched in that unit. A rule lists `owner`, `unit_admins` or `group:<name>`, and may require a second person (`second_human`) and cap one decision's reach (`max_scope`). `GET /governance` shows each unit's rules and which unit's rule governs each kind. With no rules anywhere, holds are decided as before. A security admin's write follows `WARDYN_GOVERNANCE_SECOND_HUMAN`; a unit admin may set them only for a unit strictly below one they govern, and the write is always held for a second person. A new lock knob, `deciders`, stops units below the locker from setting them; a unit, a move or a boundary write that would put rules under that lock is refused until the rules are cleared. `max_elevation` is stored in one canonical form (`4h` for `240m`). The rules are kept in a new table, `governance_unit_deciders` (migration `0265_governance_unit_deciders`), one row per unit that goes with its unit; the migration runs on upgrade and changes no existing row. Split-role installs need the grant listed under Before you upgrade, or `GET /governance` fails and a unit's deciders cannot be set.
- Approval notification routes accept `notify: unit_admins`, which tells the emails of user-subject unit-admin grants on the run's unit and its ancestors. Group-subject grants are not expanded. A config without it is unchanged.
- Governance: a profile can set `deny_scheduled_runs` to refuse a start time (`start_at`) for its people with `403` `scheduled_runs_denied`, and a run booked earlier ends `FAILED` `scheduled_runs_denied` at its start time. It is off by default, and `WARDYN_SCHEDULED_RUNS` stays the deployment's master switch, so nothing refuses more until a profile turns it on. Operators are exempt.
- Governance: a profile can set `deny_components` (stored component ids) and `deny_model_providers` (model provider uids) to refuse a run that attaches a listed component or would use a listed provider, with `403` `component_denied` or `model_provider_denied` at create, Review and preview, and `FAILED` with the same reason when a queued run starts. Both lists are empty by default, so nothing is denied and no install refuses more on upgrade. The lists compose as a union (deny wins), removing an id is held like any widening, and operators are not exempt.
- Governance: a profile's autonomy rubric has a tenth field, `self_defined_component`, that caps a run carrying a component its launcher defined. It is unset by default, so it caps nothing. It composes with `components.autonomy_cap` as the lower of the two, so a profile can tighten below the site cap and never loosen it.
- Governance changes: the person who proposed a held change may write why it is wanted and a ticket reference with `PUT /governance/changes/{id}/note` (`wardyn governance set --reason --ticket` does it for every change it held). Both are optional free text, shown on the change as `note` and audited as `governance.change.note`. `GET /governance/changes?approver=security` lists the changes only a security admin may decide, and `governance.change.propose` now carries `units` and `changed`, so a relay on the acknowledged audit webhook can route a held change to the people who decide it. Nothing changes for a client that sends no note. The note is kept in two new columns on held changes, `note_reason` and `note_ticket` (migration `0267_governance_change_note`), empty on every existing change; erasing the proposer's personal fields clears them. A note can be written only by the proposer and only while the change is pending: any other write is refused with `403` `governance_change_note_proposer_only`, and a reason over 500 characters, a ticket over 128 or on more than one line, or a control character is refused with `400` `governance_change_note_invalid`.
- A runner pool's use policy can name a unit (`{"subject_type":"unit","subject":"<unit id>"}`, set through the API). It admits a run launched under that unit or a unit below it, judged by the run's own unit: a new run's, a queued start's and a revive's frozen one. A run in no unit and a unit that no longer exists match nothing. No existing policy changes; an older binary matches a unit row to nobody, so it narrows.
- kubectl component: `access: restart` (rows are `read` or `write` today). It reads like `read` and admits one change: the patch `kubectl rollout restart` sends to a named deployment in `apps/v1`, a strategic merge patch of at most 4 KiB that sets only the `kubectl.kubernetes.io/restartedAt` pod-template annotation. Every other write is refused, and the cluster's RBAC still scopes the account. Verified against kubectl v0.32.2. Off unless you set a row to `restart`; `WARDYN_KUBECTL_CLI` stays off by default.
- Unit deciders now decide holds: the nearest unit with a rule for the kind, from the run's unit up, says who decides. `owner` is the run's creator, with the `egress_host` grant for an egress host; `unit_admins` and `group:<name>` never match the run's creator, its service identity's sponsor or its recorded author. A rule's `second_human` and `max_scope` bind every decider, security admins included (the admin token passes `second_human` and is audited), and `always` is refused under any rule. An owner a rule leaves out gets `403 not_a_decider`; a wider scope gets `403 decision_scope_above_rule`; rules that cannot be read refuse with `503 deciders_unreadable`, while a security admin still decides. `approval.decide` records `basis` and `rule`, and pending approvals list their `deciders`. With no rules, decisions and their audit rows are as before.
- `GET /api/v1/approvals?decidable_by=me` lists the pending approvals the caller may decide now, newest first, at most 500 (`X-Wardyn-Truncated: true` when more were decidable): a security admin's are every pending approval the switches let them decide; anyone else's are those on runs they created plus those their unit's deciders rule names them for. A row served to a decider who is not the run's creator carries `requested_by`, and a decider can read the path list of a held push they decide. A hold the caller may not decide is left out whether it exists or not, and a portal's delegated token is refused as for every approvals route. Without a deciders rule the list is what the caller could already decide. The SDK has `ListApprovalsDecidable`.
- `wardyn governance deciders set <unit> --file <f>` sets a unit's deciders from `{"rules": [...]}` (`{"rules": []}` clears them; a held write prints the pending change and exits 0), and `wardyn approval list --waiting-on-me` lists the pending approvals you may decide now, with the run's creator in a `REQUESTED_BY` column. `--waiting-on-me` takes no `--state`, `--run`, `--limit` or `--offset`. The SDK has `PutUnitDeciders`.
- Docs: a new page, [security-admin-setup.md](docs/operations/security-admin-setup.md), is a first-setup checklist for a security admin, and the docs index has a Getting started table.
- Platform envelopes: `PUT` and `DELETE /api/v1/governance/units/{id}/envelope` (and `SetEnvelope`, `ClearEnvelope` in the SDK) name the IdP groups the security tier approves for the unit that roots a platform. Security tier only, held under `WARDYN_GOVERNANCE_SECOND_HUMAN`; the Default unit, a unit on the path of another envelope, and a group already on another envelope are refused. `GET /governance` lists `envelopes`; a unit admin reads each one's setter as `security`, not the security admin's name. By itself an envelope changes no run: none exists until security sets one.
- A portal can be pinned to one governance unit when it is registered (`run_in_under`, a unit id or exact name): a run it launches must resolve to that unit or one below it, or `POST /runs` answers `403` `portal_unit_scope`. The check reads the context the run resolved to, so a default context outside the unit is refused too, and the person's restrictions still apply after the pick. Default: no pin, which changes nothing. The pin is set at registration only and is not checked against any other setting. The pin is kept in a new nullable column, `delegates.run_in_under` (migration `0268_delegate_run_in_under`), NULL on every existing portal. It has no foreign key on purpose: if the unit is deleted, the pin still refuses its runs, and never falls back to unpinned.
- A unit admin's `GET /governance` carries `unit_names`: the name of each unit above their subtrees, by id, such as an envelope's root. Nothing else about those units is served.
- With a platform envelope set on a unit, a unit admin's change to a unit below it applies directly when the new walls stay within the envelope unit's walls and the change stays below that unit. A unit admin's add of a group on the envelope's approved list to that unit or one below it, or a move of the group's membership between two such units, also applies directly. A change that removes a lock, changes a contact, or changes which legacy membership a person is selected by is still held. Every held change of the envelope's unit itself, and every unit admin grant or revoke on it, is decided by a security admin only. People, groups not on the list, and membership deletes keep their present rules. An envelope that cannot be read gives no direct write and leaves the envelope's unit to security admins. With no envelope, nothing changes.
- A unit admin's `GET /audit` serves the rows of runs frozen to a unit in their subtrees, through an allow-listed view: the row's envelope, its target for run, egress and approval rows, and listed data keys holding numbers and codes only. `GET /audit/export` serves a unit admin only the runs they created themselves, and answers a member's empty `200` for anything else. Rows with no run are not served, and a `target` filter without a run, egress or approval `action` or `action_prefix` is refused `400`. A new index on `agent_runs.unit_id` serves the read. The index, `agent_runs_unit_idx` (migration `0269_agent_runs_unit_idx`), covers only runs that have a unit and is built on upgrade, which holds off writes to `agent_runs` while it builds; no row changes and no grant is needed.
- The audit row of a forwarded MCP `tools/call` (`brokered:mcp`) now names the tool in `cause` (`tool "<name>"`, or `tools` with the first 8 names of a batch), never its arguments or the answer.
- A `custom_endpoint` model provider can carry up to 8 `extra_headers` (a name and a value of at most 2048 bytes each), which the run proxy sets on every request to the provider's gateway, after the stored credential and over a sandbox header of the same name. For a gateway that routes on a header, such as Portkey's `x-portkey-provider`. The default is none. Names that could carry a credential or change the request's framing are refused (`Authorization`, `Cookie`, hop-by-hop and `X-Wardyn-*` headers, any name containing key, token, secret, auth, password, cookie or credential, and the provider's own `auth_header`), as is a value shaped like a secret. Changing a provider's headers removes everyone's stored credential for it, as changing its `base_url` does, and the audit row names the headers, never their values. A remote runner that does not name `llm_headers` is refused a run that carries any.
- `llm_inspection.detector_sidecar_format` can be set to `presidio`, so `detector_sidecar_url` can point at an unmodified Presidio analyzer (`/analyze`). Unset, the default, keeps Wardyn's own format. It needs `detector_sidecar_url`, and in `block` mode it needs `block_min_severity` of at least `medium`, because Presidio also reports names, dates, URLs, ids and addresses, which are logged here at low severity. A reply that is not a JSON array counts as a sidecar error.
- An approval row a unit deciders rule governs says how it is decided. A pending row carries the rule's `max_scope` and `second_human` beside `deciders`. A decided row carries `decision_basis` and `decision_rule_unit`, stored with the decision (migration `0270_approvals_decision_basis`). Rows decided before the upgrade carry neither.
- kubectl component: `approve_each: true` on a `restart` or `write` row holds each write the door admits until a person approves that exact request (method, path, query, media type and body), once. The run's owner decides it, as for one MCP call. A held write waits 90 seconds, then is refused with the approval to wait for; the same command run again sends it once approved. Bodies must be JSON, not encoded, at most 32 KiB. Off by default; an older proxy image refuses a config that carries it.
- Operations guide "Who decides a hold" covers unit deciders: the rules, who may write them, how a hold is resolved and the audit fields `basis` and `rule`. The threat model lists five residuals, among them that a security admin decides without the rule while the rules cannot be read.
- A new page, [Model gateways and detectors](docs/operations/model-gateways.md), sets up LiteLLM, Portkey (self-hosted and hosted), OpenRouter and a Presidio analyzer with a `custom_endpoint` provider and `llm_inspection`, and lists the versions a nightly test pins (its first green run is pending). `extra_headers` values are stored in clear and are not secrets; the `model_provider.write` audit row lists their names. The docs also cover `detector_sidecar_format`, the placed-secrets corpus and a walk-through of `approve_each` against `test/gatewayfake`.
- [Brokered operations](docs/operations/brokered-operations.md), a page of recipes: ops commands through an organisation's MCP server with a checklist for the server, read-only database access, and kubectl with one RBAC Role per row, `access: restart` and `approve_each`, and a pool kept to one unit.
- New setting `runners.require_managed_device` (`PUT /api/v1/runners/settings {"require_managed_device": true}`, super admin): a new local run is refused with `runner_trust_required` on a runner whose trust class is `personal`, so only a `managed_device` runner or an organisation's runner takes it. Off by default, so an upgrade changes nothing; runs already started are not affected. It trusts the security operator's `managed_device` mark and checks no device certificate; the new Device posture section in the hybrid-laptops page says what is and is not checked.
- A revive of a run that holds a component or model provider listed in `deny_components` or `deny_model_providers`, on the profile or on a restriction, is refused with the same `component_denied` or `model_provider_denied` reasons as at create.
- `PUT /site-config` refuses a document that names a different `runners.require_managed_device` than the stored one, with `400` `site_config_runners_via_own_route`; change it with `PUT /runners/settings`. An echo of the stored value, or no mention, is carried forward.
- A held kubectl write (`approve_each`) is offered to its run's owner in the console and no longer shown as blocking them on the run page, and `wardyn approval get` shows its cluster, method, path and body hash.
- Migration `0271_approval_file_review` admits the `file_download` approval kind and keeps one pending review of a file per run; no grant is needed.
- The desktop clamp warning names `review` as well as `blocked`.

### Changed

- Docs: OPERATIONS says Codex CLI on a `custom_endpoint` calls the OpenAI Responses API, and AZURE-DEVOPS says a member's run under a unit's `deny_with_review` cannot loosen it to `wait_for_review`.
- The proposer of a held governance change may withdraw it with `POST /governance/changes/{id}/reject`, even when only others may approve it, such as a unit admin's change held for the security tier. The `governance.change.reject` row then carries `withdrawn: true`.
- With `detect_secrets` on, the known-secret check now covers every `env_secret` and `file_secret` value placed in the run, with a component's secrets, not only the secrets named in `workspace_secret_names`. A `block`-mode policy may therefore refuse a prompt that quotes such a value. It checks the whole value only (single-line values, and multi-line values only when pasted verbatim), at least 8 bytes long. Do not store non-secret values, such as a region or a public URL, as an `env_secret`. A revive now rebuilds the proxy with the `file_secret` placeholders as well. Nothing changes with `detect_secrets` off.

### Fixed

- A run's model token usage now counts a call to the OpenAI Responses API that went through a gateway such as LiteLLM: the gateway ends the stream with the chat stream's `data: [DONE]` line, which the meter read as a malformed answer and showed the call as not counted.
- A unit admin whose unit moved out of their scope between a held governance change's pre-check and its decision is now refused with `unit_scope_denied`, as the denials page documents; before, the same case answered `security_admin_surface`.
- A unit admin whose change to the root of a platform envelope is held for the security tier still lists and reads that change and finds it in the duplicate-proposal `409`; peers of the root see none of the three. A unit admin refused on a security-only root now gets `security_admin_surface`, not `unit_scope_denied`.

## [0.10.0-rc.15] — 2026-11-08

### Before you upgrade

- A refused egress request no longer counts as activity for idle auto-stop or idle pause, on every kind of run, with no switch: a run that is only being refused now idles out. Only an allowed request restarts the idle clock. On a macOS VM run the guest's Apple boot hosts never count, allowed or refused, on the control plane and in a runner's proxy (`activity_ignore_hosts`). A runner's proxy keeps the old behaviour until the runner is upgraded.
- A run with an allowed-models list, or with content inspection in `alert` or `block` mode, now treats a model call whose body carries `fallbacks`, `models`, `context_window_fallbacks`, `content_policy_fallbacks`, `router_settings_override` or `preset` as a refusal (`block`, and always under the allowed-models list) or a logged alert (`alert`). Claude Code and the Codex CLI send none of these keys. In `block` mode, a model call whose body is over the 32 MiB scan limit, has a `Content-Encoding`, or is not one JSON object is now refused where an oversize body used to pass with `on_scanner_error: pass`. A run with neither feature is unchanged, and so is a route set to `inspection: gateway`.
- A macOS VM run now denies `mask.icloud.com`, `mask-h2.icloud.com` and `proxy.safebrowsing.apple` quietly, as it already does for the other Apple background hosts: no approval is raised and each denial is recorded. A run whose policy allows one of them by name keeps it. Allow the host in the run's policy to reach it.
- A macOS VM base gets its new settings (below) from the tools update, and the first update after the upgrade is answered by the base's old guest agent, which cannot apply them: `tools.json` records "agent predates this setting; run the tools update again". Run the update twice.
- On a macOS VM runner, `wardyn-runnerd doctor` now has a `guest-agent` stage that fails when the base's last tools update pushed the guest agent signed ad hoc. The tools update after the upgrade signs it again; if the stage still fails, it says why each signer failed.
- The Review step's "Sandbox barrier" row now reads "missing", not "unverified", when the deployment has no runner at all, so it holds Launch where it did not before. A runner that Review merely cannot reach still shows "unverified".
- A macOS VM base that already exists has its guest agent signed again with this Mac's certificate at its next tools update, in the runner's own process, with codesign as the fallback.
- A Codex Model stored on a custom endpoint or OpenAI key provider, which was ignored before, now takes effect for runs with the Codex CLI. With none stored, Codex keeps its own default.
- On a configured model gateway's host, a sandbox's own `x-portkey-*` headers are now dropped, except the header the stored credential rule owns. A run that set one itself no longer reaches the gateway with it.

### Added

- Docs: OPERATIONS has a section on how long an AWS sign-in lasts: the IAM Identity Center session duration, the identity provider's session for that application, and assigning the same people to both applications.
- macOS VM: `edit-base` has an Install Rosetta button, which runs Apple's Rosetta installer in the base once you press it. Rosetta stays off otherwise, and the prep proxy now also allows `configuration.apple.com` for it.
- macOS VM: the guest agent's posture reads FileVault with `fdesetup status`. `edit-base` warns, in its window and in the recheck, when FileVault is on in the guest, and `doctor` shows a `file-vault` line. The base is kept either way.
- Two configuration profile templates for organisations with MDM, in `deploy/macos-guest/mdm/`: Setup Assistant panes to skip (installs by hand or MDM) and Full Disk Access for the guest agent (MDM only).
- macOS VM bases now mark Setup Assistant's panes as seen for the agent account, stop the Fn key raising Dictation, turn Spotlight indexing and diagnostics upload off, and kill Terminal and remove its saved state, so a run's first desktop is clean. On by default. Prepare writes them and every tools update writes them again, through a new guest-agent route that takes no input and adds no allowed exec. Rows that fail or cannot be applied are listed in `tools.json` as `base_settings_skipped` and never fail a prepare or an update. edit-base's finish also cleans Terminal.
- `wardyn-runnerd doctor` on a macOS VM runner has a `guest-agent` stage: it fails when the guest agent went in signed ad hoc, quoting why each signer failed, and passes when this Mac's cert signed it. `macos-vm status` shows the signer and the signing errors, and the base's `tools.json` records them as `agent_signer` and `agent_sign_errors`, with no paths.
- Setup status shows an "Image checks" row when image admission is `audit` or `enforce` and `scope` is `member` (the default), to say that the images runs get from their agent or harness set are not checked. Set `images.admission.scope` to `all` to check them. Nothing changes until you do.

### Changed

- The desktop viewer and the run page no longer show the line about a macOS login window. The macOS VM docs note that the macOS 27 prepare flow creates only the setup account and that nothing beyond macOS 26 is promised as tested.
- A Codex Model stored on a custom endpoint or OpenAI key provider now takes effect: runs with the Codex CLI use it. With none stored, Codex keeps its own default.

### Fixed

- `opaque_tunnels` in a run's model usage now counts a tunnelled connection to a model host whether or not content inspection is on: the proxy marks the connection's allow row (`model_tunnel`), and the count is the larger of the marked rows and the `llm.scan.bypass` rows, so older runners still count.
- Idle auto-stop no longer reads a refused request as the run working. A macOS VM run's Apple background requests (a refused boot host above all) kept resetting the idle clock, so the run never stopped at its cap. Only allowed requests count.
- A runner no longer fails a new run with a spool lock error when something outside Wardyn removes the run's new spool directory just before its lock is made: the directory is made again once. A spool that already held data is never recreated, and the error now says when the directory was removed or replaced while it was opened.
- A run being created no longer has its spool wedged ("spool metadata missing beside existing data") when another run's creation writes the live-activity files into it at the same moment. Those files are no longer counted as existing data.
- When the runner fails to create the sandbox of a run that was keeping a new machine, the dropped machine now has a `machine.create` audit row with outcome `failure` and `reason` `sandbox_not_created`; before, only the earlier success row was there.
- A macOS VM runner now signs the guest agent in its own process with this Mac's signing identity, with no keychain and no trust setting, so the agent can keep Full Disk Access across tools updates. codesign could refuse that self-signed identity ("no identity found"), and the agent then went in signed ad hoc. codesign stays as the fallback, now given the designated requirement and the signing keychain on the search list for the call. Each signed copy must pass `codesign --verify --strict` and read back the designated requirement before it is pushed. This is on by default.
- Review's "Sandbox barrier" row reads the barriers of the runner or pool the run will land on, not the control plane's own runner. A runner Review cannot reach shows the row as unverified, "Checked at the runner when the run starts."
- Review's "Sandbox barrier" row on a deployment with no runner at all now shows as missing, instead of "Checked at the runner when the run starts." Nothing would answer there.

### Security

- A model call can no longer name another model through a gateway's rerouting keys while the allowed-models list is set. Under content inspection the keys are refused in `block` mode and logged in `alert` mode, including on the Responses routes the scan cannot read. A body that cannot be read for the keys is refused in `block` and logged as `unparsable` in `alert`.
- On a configured model gateway's host, the proxy now drops every `x-portkey-*` header the sandbox sends, except the header the stored credential rule owns. Before this, a sandbox could set `x-portkey-custom-host` and send the stored key to a host it chose, through a gateway that honours it.

## [0.10.0-rc.14] — 2026-11-07

### Before you upgrade

- If your walls or a wall preset set `*` to hold or deny, tools the run's policy allows by name are now held or denied too. To keep such a tool open, remove the `*` rule and name the tools you want held or denied instead.
- A `WARDYN_ADMIN_TOKEN` shorter than 32 bytes now stops wardynd starting unless it listens on loopback only (the compose stack's `demo-admin-token` is unchanged). Set a longer one, for example `openssl rand -hex 32`.
- With SSO configured, the UI-sandbox gateway now refuses to start without `WARDYN_UI_SANDBOX_ORIGIN_TEMPLATE`, and the Helm chart refuses to render unless a per-run origin is set, through `uiSandbox.originTemplate` or `WARDYN_UI_SANDBOX_ORIGIN_TEMPLATE` in `env` or `extraEnv`. Set a per-run origin such as `https://run-{run}.ui.example.com`, or accept the shared origin with `WARDYN_ALLOW_UI_SANDBOX_SHARED_ORIGIN=true` (chart: `uiSandbox.acceptSharedOrigin`).
- Docker runs now need Docker Engine 28 or later and an updated runner, because a run's network is now made without an address on the Docker host. On an older engine (Podman reports one) a run is refused unless `WARDYN_ALLOW_HOST_REACHABLE_RUN_NETWORK=1`, which keeps the old network. Runs started before the upgrade keep their old network until they stop or finish; restarting or reviving one is refused unless that setting is on.
- A governance profile's `github_capabilities` now limits its members' runs, and a run's GitHub CLI access is limited to the run's own GitHub capabilities. A run that relied on wider access is now refused or gets only the GitHub capabilities both lists name.
- A file or variable secret that reaches the 8-byte minimum only with its trailing line break is no longer delivered to a run and can no longer be saved: output shows it without the break, which can't be masked. Store a longer value.
- Wrong admin-token attempts are now limited per client address (`admin_token_rate_limited`). Behind a load balancer or reverse proxy, list it in `WARDYN_TRUSTED_PROXIES` so each client counts on its own; otherwise one client's wrong attempts block every admin call.
- Image admission is off by default: no image check runs until a security operator sets site setting `images.admission.mode` to `audit` or `enforce`. A governance profile that lists `required_image_signers` holds its members to those signers even while the site mode is off. Turning `enforce` on refuses reviving runs created before it was on; start a new run.
- Model token counting is on by default and needs no setting. `WARDYN_MODEL_USAGE=off` turns it off: wardynd then stores no count, and the proxies it starts stop counting. The setting reaches a proxy as an environment variable, so a proxy older than 0.10.0 starts as before and does not count. A runner on another machine starts its proxies from its own environment, so set `WARDYN_MODEL_USAGE=off` there too. Runs on a runner older than 0.10.0 are not counted, because that runner cannot read the count rows. To move a runner back below 0.10.0, first stop its runs and keep it connected to the organisation until its spool is delivered: no `spools/*/0*.json` file is left in its state directory. An older runner replays every kept spool, ended runs included, before it reports ready, and stops at a count row it cannot read.
- Image admission reads trusted-root files (`images.admission.trusted_roots[].path`) only from `/etc/wardyn/sigstore`. On compose, put them in a `sigstore` folder of the `WARDYN_MANAGED_DIR` mount; on Kubernetes, mount them there with `extraVolumes` and `extraVolumeMounts`. A saved path anywhere else, or a link that leads out of that folder, is never read: the image check cannot load that root until an admin moves the file there and saves the setting again with its new path.
- Split-role installs (`WARDYN_PG_MIGRATE_DSN` set) whose app role does not pick up new tables through the `ALTER
  DEFAULT PRIVILEGES` step in Operations: grant the app role `SELECT, INSERT, UPDATE, DELETE` on `run_create_keys` and
  `SELECT, INSERT, UPDATE` on `groundtruth_sensors`. Without the first, a `POST /api/v1/runs` that sends an
  `Idempotency-Key` fails; without the second, `/healthz` cannot tell Kubernetes node sensors apart and reads only the
  newest heartbeat. Grant it `SELECT, INSERT` on `audit_checkpoints` too; the migration already does for a role that
  can call `audit_append`. Grant it `SELECT, INSERT` on `run_model_usage` too; without it a run's model token counts
  are not stored. An upgrade from before 0.10.0-rc.12 also
  needs every grant the 0.10.0-rc.12 notes list: `SELECT, INSERT, UPDATE, DELETE` on `wall_presets`, `kept_machines`,
  `workloads`, `workload_bindings`, `workload_run_ends`, `macos_vm_disks`, `aws_sso_refused_tokens`, `cli_sign_ins`,
  `ui_desktop_closed`, `desktop_signins`, `desktop_signin_shares`, `desktop_signin_runs`, `approval_rules`,
  `governance_restrictions`, `governance_restriction_members`, `governance_unit_admins`, `oidc_session_sids` and
  `desktop_vm_sign_ins`, and `SELECT, INSERT, UPDATE` on `runner_health`, `launch_preset_version_floor`,
  `cli_sessions` and `governance_boundary` (and on `run_harnesses` if it turns on `WARDYN_MULTI_AGENT_TASKS`).
- Revoking a portal still leaves the runs it launched their gateway access, as before. To cut it, pass `cut_in_flight=true` when you revoke; on an already revoked portal it cuts it then.
- `POST /api/v1/runs` now honours `Idempotency-Key`. A client that already sends that header gets the first answer
  replayed for 24 hours instead of a new run, and a value outside 1 to 255 visible ASCII characters is refused with
  400 `idempotency_key_invalid`. A retry with a key is safe only once every control-plane replica runs 0.10.0 or
  later; an older replica ignores the key.
- `Client.CreateRun` and `wardyn run` now send an `Idempotency-Key` (a fresh UUID unless you pass one) and still make
  one attempt: they never repeat a create on their own, so a lost answer is returned as the error. Retrying is opt-in
  (`CreateRunOptions.Retry`, or re-running a job with the same `--idempotency-key`) and is safe only once every
  control-plane replica runs 0.10.0 or later; an older replica ignores the key and starts a second run.
- On a remote MCP server where a run's tool rules block a tool, that server's tool lists now leave the blocked tools out, a tool list Wardyn cannot read is answered with an error, and one message from that server larger than 10 MiB ends its response.
- A run whose tool rules mark a remote MCP tool `approve_each` does not start on a proxy image or runner older than this release: its proxy config carries a key they refuse.
- Watching a background macOS VM run's desktop now shows its apps, not a login window. If that is not acceptable, turn `macos_vm.allow_background_watch` off before you upgrade.
- wardynd now signs the audit log by default: about once a minute, while anything is being logged, it writes an `audit.checkpoint.create` row, sends it to your audit sinks, and makes the boot key `wardyn-audit-receipt-key` on first start. A test or alert that counts exact audit rows in an idle window will see these rows. Set `WARDYN_AUDIT_CHECKPOINT_INTERVAL=0` to turn it off; an interval of 1s to 9s is refused at boot. Back up the database and the age key together, as for the other boot keys: a restore that lacks the key row stops signing (`foreign_row`).

### Added

- Runs launched through a portal name the portal and the acting agent to an AI gateway: gateway tokens carry `wardyn_delegate` and, when the portal named an agent, a nested `act`. The run's `created_via_actor` names the agent, and so do the audit rows of a queued launch's start and of an attach ticket the portal minted. On by default with no setting: a portal that names no agent changes nothing but the new `wardyn_delegate` claim.
- Kubernetes installs can now run Wardyn's kernel ground-truth sensor beside your own Tetragon (`groundtruth.k8s.enabled`, off by default); its events are tied to runs and people, and `/healthz` reports each node's sensor.
- Image signature checks at run create (`images.admission`): with it set to `enforce`, a member's image must be signed (Sigstore keyless or a public key) by a signer the organisation trusts for that image, with any attestations it requires, and the run uses the exact digest that was checked. `audit` reports without refusing. Off by default. Governance profiles can require named signers with `required_image_signers`. Ungoverned operators' runs and runs Wardyn launches itself are not checked. See [image signature checks](docs/operations/image-signature-checks.md).
- The run page now shows each run's model token usage (input, output, cache) per model, counted from the provider's own answers; calls Wardyn can't see into are counted and shown as not counted. `GET /api/v1/runs/{id}/model-usage` returns the same, and `wardyn_model_tokens_total` counts tokens by vendor and direction. `WARDYN_MODEL_USAGE=off` turns it off.
- Audit: wardynd signs a checkpoint over the newest audit rows every minute, with a key the database cannot read, and sends each to your audit sinks. `wardyn audit receipt` and `wardyn audit verify` give and check, offline, a receipt that an event is in the log unaltered, and `wardyn audit checkpoints` and `wardyn audit keys` download the signed checkpoints and the public keys to pin. Verify exits 0 when every row checked out, 1 on a failed check, 2 on an unreadable file or bad usage and 3 when some events were withheld from your role. `wardynd -rotate-audit-receipt-key` rotates the key, and `-retire-previous` retires the old one. Three metrics, `wardyn_audit_checkpoint_lag_rows`, `_last_timestamp_seconds` and `_failures_total{reason}`, show signing. On by default; nothing it signs is ever refused.
- Approvals: the run's owner or an administrator approves one MCP call that a tool rule marks `approve_each`, once, and an approval rule never does. `WARDYN_CAPABILITY_SECOND_HUMAN` (off by default) also makes someone other than the run's creator decide it.
- Tool rules can mark a remote MCP tool `approve_each`: each call waits for a person to approve that call with its exact arguments, and Wardyn sends exactly what was approved, once. It works for Codex CLI too. Nothing changes until a rule says `approve_each`.
- Approvals: the console shows a request to approve one MCP call as its own card (server, tool, the server's own tool name, and the arguments in full) and approves that call once, with no scope choice. Arguments and other parameters come from the new `call_text` on the approval, so a number past 2^53 or a long decimal shows and copies as written; `wardyn approval get` prints the same text.
- Tool rules: Add a tool rule, the policy editor and Your walls offer Approve each call (`approve_each`) for one MCP tool of a remote server. It is greyed, with the reason, anywhere it cannot apply.
- Docs: the SSO setup pages now recommend `WARDYN_GOVERN_ADMIN_RUNS`. It stays off by default.
- Docs: a guide to choosing a barrier for an agent that has no shell, uses tools only, or runs code. It also says there is no gateway-only mode in 0.10.x and gives the smallest sandbox for a tool-only agent.
- Settings: Your walls and wall presets now have controls for GitHub and Azure DevOps capabilities, tool rules, paths never pushed, a recording minimum and an idle stop.
- Audit: every audit row of a run written after its policy is resolved now carries `policy_digest`, the SHA-256 of the run's effective policy as recorded in its `run.policy.resolve` row, and `GET /runs/{id}` returns it too. It is on by default and refuses nothing. SIEM parsers that compare a row's `data` object exactly will see the new key; rows written before the run's policy was recorded, rows with no run, and rows a device forwards carry none.
- Audit: a revive now records the run's new policy as `run.policy.generation`, and the run's audit rows carry `policy_generation` beside `policy_digest` (1 for dispatch's policy, 2 after the first revive, and so on). A row the run's proxy sends is stamped with the policy of the run token it presented, so a decision made before a revive and delivered after it keeps its old policy; once a run has had more than one policy, a proxy row no token ties to one carries `policy_digest` `unknown`. The proxy's config gains no key, so a proxy pinned to an older release still starts. On by default; it refuses nothing.
- `wardyn-guest-agent tls-check <host>` inside a macOS VM checks a host the way apps that use only the system trust store do, and exits 1 when TLS fails.
- A portal can name the agent acting for a person. `POST /api/v1/token` accepts an `actor_token` your identity provider issued to the portal, and every audit row the delegated token writes names the agent in `data.via.actor`. On by default with no setting: the agent is recorded, never given access, and a portal that sends no `actor_token` sees no change.
- `POST /api/v1/workloads/{name}/preview` answers what an apply would do: created, updated or unchanged, each changed field, whether a run starts and which run it replaces, and the refusal the apply would meet. It stores nothing. A service identity's token may call it.
- Run identity for an Amazon Bedrock provider people sign in to with SSO. With `WARDYN_AWS_SIGV4_AT_PROXY` on, Wardyn signs each Bedrock request as the person for the regional Bedrock address and sends it to your gateway with the run's identity token. The gateway forwards it to AWS unchanged. Off unless an admin turns it on for the provider.
- Revoking a portal can also cut the gateway access of the runs it launched: `DELETE /api/v1/admin/delegates/{id}?cut_in_flight=true`. Their gateway calls are refused within about half the provider's token lifetime. Off by default. A run's gateway identity is now refreshed before the first call after its half-life instead of behind it, so a refusal of the refresh applies to that call. A refresh that fails but may pass, such as an unreachable control plane, still leaves the held token in use until it expires, and a call waits up to 15 seconds for that refresh first. Once the control plane cuts the run's gateway access, the proxy answers every call to that gateway with 403 and stops asking.
- Helm: `imageAdmission.registryAuthSecret` names an existing `kubernetes.io/dockerconfigjson` Secret that wardynd reads, mounted read-only, so image signature checks can reach private registries. Unset by default: registries are read anonymously.
- `POST /api/v1/runs` honours an `Idempotency-Key` header for 24 hours. A retry with the same key and body gets the
  first answer back with `Idempotent-Replayed: true` instead of a second run. A different body is refused (422
  `idempotency_key_reused`), and a retry while the first is still being answered gets 409
  `idempotency_key_in_flight`. `/healthz` reports `idempotent_run_create`. On by default; a request without the
  header is unchanged.
- The edit-base window has Open admin Terminal, Type code, Trust the list's CAs and Full Disk Access buttons, and
  shows whether each certificate is trusted and whether the guest agent has Full Disk Access, as the guest reads them.
  Its one-time code is now 10 lowercase letters on the same keys on QWERTY, QWERTZ and AZERTY keyboards, so Type code
  types it on any of them; Show code stays for other layouts. Trust the list's CAs also trusts this Mac's guest agent
  signing certificate, for code signing only, while the base does not trust it.

### Changed

- Gateway tokens for a run a portal launched now carry a `wardyn_delegate` claim holding the portal's id. A gateway rule that logs unknown claims will show it; token validators ignore it.
- Docs: one section, [Sharing a folder: the settings](docs/operations/hybrid-laptops.md#sharing-a-folder-the-settings), names the three places a folder share is set and which refusal means which.
- Docs: the macOS VM page is split by reader into a use page, a base image page, a host page and a troubleshooting page, and every old anchor still lands on the entry page. Ingress rate limits and trusted proxies, and the CLI component sign-in, have their own operations pages. The docs index is grouped by reader, and eight figures (run states, run placement, credential mint and shapes, egress checks, nested limits, the Azure DevOps request flow and the macOS VM run) replace Mermaid fences.
- `wardyn run --idempotency-key KEY` keeps a re-run CI job from launching a second run: the same key and request get the first run back, and the CLI says so on stderr. The Go SDK gains `CreateRunWith`, with the same key and an opt-in retry of a lost answer; `Client.CreateRun` and the CLI make one attempt.
- `scripts/ci-run.sh` passes `WARDYN_CI_IDEMPOTENCY_KEY`, when it is set, to the launch as `--idempotency-key`, and the CI examples set it from the job's stable identity, so a re-run job gets the first run back.
- macOS: the runner now signs each guest agent it pushes into a base with a code signing identity made once on that Mac (in the VM directory's `agent-signing/`), and the base trusts that certificate for code signing only, so a Full Disk Access grant can last through tools updates. On by default; if signing fails the agent is pushed signed ad hoc, as before. `macos-vm status` says which. The first tools update after upgrading changes the signature, so grant Full Disk Access once more in edit-base.
- `wardyn diff` and `wardyn apply --dry-run` ask the server what the apply would do, so they now show refusals the apply would meet and whether a run would start. `apply --dry-run` exits 1 when one would be refused, and `diff` exits 2 and names the reason when the launch would be refused. Against a server without the preview route they fall back to the old client-side comparison and say so on stderr.
- macOS: `wardyn-macvm update-tools` (and the base derivation runnerd starts) now fails at once with "A tools update of this macOS base is already running." when another update or a prepare holds the base's update lock, instead of waiting for it.
- `edit-base` no longer prints the setup-code file's path. It points at the window's Type code and Show code buttons. The guest agent now answers three edit-window requests in tools mode only (open an admin Terminal, trust the install list's certificates, open Full Disk Access), each run as the console user.
- A runner whose proxy image cannot be pulled at start now waits and tries again (after about 15 seconds at first, backing off to about every five minutes) instead of exiting and restarting; `wardyn-runnerd doctor` shows the next try in its status line, and the organisation is told why once the runner connects. A stop still exits cleanly.
- An agent no longer sees a remote MCP server's tools that its run's tool rules block: Wardyn leaves them out of the server's tool list, page by page, and still refuses their calls.
- `wardyn-runnerd doctor` on a Mac with a base now says which folder-share setting is missing: a `folder-shares` stage names the drive root or Full Disk Access to set, and is a skip, never a failure.

### Fixed

- Your walls: a tool rule set to `approve_each` no longer turns into `hold` when the form is opened and saved.
- The policy editor now locks Azure DevOps capabilities against the ceiling of your own row, even with one organisation, instead of the first row on the provider.
- A Linux VM run's terminal, commands and agent start now always run inside the VM. If the runner can't tell whether a run is a VM, the request fails and can be retried; if Linux VM sandboxes were turned off (or failed their start-up check) after the VM started, the request is refused, and the run can still be stopped or ended, including when its audit log fills up.
- In focus mode, switching the side panel to another widget after one failed to display now shows the new widget instead of the previous error.
- Run networks no longer carry an address on the Docker host.
- A Mac runner restarted before its new proxy image could be pulled no longer leaves macOS VM helpers holding both VM slots: at start the runner ends the helpers an earlier start left behind that never took their slot, it starts the tools update that replaces an older guest agent in the base, or that installs a changed install list (`macos_vm_base_file`), only once it is serving, trying a refused start again after 10 minutes, and it starts one tools update at a time.
- A window open on your Mac through `wardyn window` now counts as activity, so idle auto-stop no longer stops a macOS VM run while someone works in its window.
- Revoking a personal API token, or ending a CLI session, now closes a `wardyn window` that it opened. An at-start window asked for with an API token or a CLI sign-in before the upgrade does not open; open it again with `wardyn window`.
- Tool rules in your walls or a wall preset now give the same result in any order, and a rule for a named tool takes precedence over `*`. A `*` rule now also applies to tools the run's policy names, unless your walls name that tool themselves, so walls can no longer leave a tool more open than they set.
- The note under Your walls now says which limits a restarted run takes on; other changes need a new run.
- A Linux VM run now ends when its harness exits, even when a background process it started still holds its output open; the stray processes are stopped, and the harness gets a terminal when it asks for one, as in a container.
- Terminals and commands in an interactive Linux VM run now trust the run's TLS-interception certificate: the files the certificate variables name are written when the VM starts, so `curl`, `pip` and `aws` no longer fail on a missing CA file.
- A Linux VM run's harness output is kept on the VM's disk instead of its memory, is capped at 64 MiB with a closing line that says so, and a full memory disk can no longer block pausing the agent.
- After the VM's agent service restarts, a new terminal or command is refused with a reason instead of running without the run's proxy and credentials.
- A Linux VM run no longer silently drops a limit it cannot enforce: a policy disk size or process limit is refused with a plain sentence, and a requested disk size is shown as not enforced.
- Ending a Linux VM run now wipes the AWS sign-in caches inside the guest before it is kept, and reports it when it cannot.
- Starting a sign-in in a browser of your own on a Linux VM run is now refused instead of freezing the VM (pausing the VM's agent to sign in still works), a Linux VM's output recovery is refused with a reason, and a VM run's folder-share cleanup is retried until it finishes.
- A secret delivered to a run as a file or variable, with line breaks inside it (a private key, a kubeconfig), is now hidden in terminal output, live attach and recordings even when the terminal prints its lines with CR LF ends, or only some of its lines.
- The audit row for a clipboard transfer through a desktop window no longer carries a SHA-256 of the text; it records the direction and size only.
- A governance profile's GitHub capabilities now limit members' runs, as documented.
- A terminal opened on a service identity's run now closes at its next check once the person is no longer that identity's sponsor, as a new terminal would be refused.
- An admin reading another person's terminal text through a run's output now leaves a `run.output.read` row in that run's audit, as replaying its recording does.
- Repeated wrong admin tokens from one address are now answered `429` with `Retry-After`, the right token included, until an attempt refills (10 attempts, then one every 10 seconds). A request that presents the right token is never counted.
- Reviving a run now re-checks its runner pool's use policy and people policy, as starting one does: a run whose pool was deleted, or no longer admits the run's owner, is refused.
- Moving a governance profile to a new parent is now held for approval when the move takes it out from under a lock.
- Opening a `wardyn window` that was asked for with a credential Wardyn can no longer check is now refused with a plain sentence (403) instead of an error.
- Settings: saving an unrelated change no longer reorders stored capabilities, and walls errors now point at their field.
- The console's Content-Security-Policy now sets `form-action`, and the console sends `Permissions-Policy` and, under TLS, `Strict-Transport-Security`.
- The Files changed widget now says a run's repository state is unknown when git cannot run in the sandbox, instead of showing no repository.
- macOS VM runs: a Terminal opened in the window, and apps started from the Dock or Finder, now get the run's non-secret settings (the proxy, the CA bundle, the provider sign-in files, the git identity), so Claude Code in the window's Terminal is signed in. A run's secret variables (`env_secret`) are never placed in the window's Terminal or desktop. An app already open when the window shows ready may need a relaunch. While the settings are being applied, `wardyn window --status` and the run page say so, and an at-start window waits for them (up to ten minutes) without holding the run's start; if they could not be applied, the window still opens and the status names the reason and the retry, `agent-run --session-env-retry` in the window's Terminal.
- Watching a macOS VM run's desktop from the browser now shows the agent's desktop instead of a macOS login window ([#3890](https://github.com/cjohnstoniv/wardyn/issues/3890)). It is on by default, with no setting: where the Mac's Screen Sharing does not let the run's account sign in, the view shows the login window as before, and `/var/run/wardyn/desktop-view.status` in the run says why. The Mac's base gets this with its next tools update, which the runner starts on its own after the upgrade.
- Review now refuses an Azure DevOps capability list that reaches past the provider's ceiling (`ado_capabilities_above_ceiling`), with the same sentence launch gave, instead of letting the run fail after the click. It is on by default and refuses nothing launch would not.
- `wardyn diff` now checks every workload in a file and names each one the apply would refuse, where it stopped at the first. `wardyn apply --dry-run --json` leaves out `preflight` when the server's preview carries none.
- After a revive, the effective-policy view, the approval checks, the run's UI apps and the console's clipboard rule read the policy the run's proxy enforces: the revived one once it replaced the proxy, and the one before it when the revive left the old proxy running. This holds for a run with a long audit trail too.
- An approved `approve_each` MCP call reaches the server with the client's own request id text (`1.0` stays `1.0`), and a decline says it covers that request and identical retries.
- Image admission: a registry answer for an image's signatures that does not parse reads as a missing signature (403) instead of "could not be read" (503), and a bundle and a legacy layer count against one limit of 16 per signer. The run's admission record says when its verdict came from the cache.
- A tool list event stream whose first event is shorter than three bytes is no longer held back until more arrives.
- Erasing a person's run tasks now also clears the answers kept for their `POST /api/v1/runs` idempotency keys; a retry after the erasure gets a `201` rebuilt from the run, and each key stays bound.
- Ending a Linux VM run whose VM the runner can no longer reach, because VM sandboxes were turned off, now also removes its sidecars.
- The run page's layout preset buttons (Live, Finished) are now a labelled group and say which one is pressed.
- Settings: an Amazon Bedrock SSO provider's run identity row that is already on can be switched off where the install cannot sign its AWS calls through the proxy.

### Security

- A run's image check is made where the run is created and the run is pinned to the checked digest, so an image re-tagged after the check is not what runs.
- Wardyn's container images now build with Go 1.27.3, and Wardyn uses go-jose 4.1.7, which fix certificate-verification and token-parsing security issues.

### Known issues

- macOS: that the guest agent keeps Full Disk Access through a tools update with the signature made on that Mac is not yet proven on a real Mac. After an update, check `macos-vm status`; if Full Disk Access reads refused, grant it again in edit-base.

## [0.10.0-rc.13] — 2026-11-04

### Before you upgrade

- The Mac window fix is in the Mac's helper, `wardyn-macvm`. On each Mac that opens windows, install the new `wardyn-runnerd.pkg` and run `/usr/local/wardyn/bin/wardyn-runnerd setup` again with the same flags. `wardyn runner upgrade` replaces only `wardyn-runnerd`, so it does not bring the fix; a server-only upgrade leaves the refusal in place.

### Fixed

- A window onto a macOS VM run opens on an unlocked Mac again. rc.12 refused every open with `mac_window_lock_unknown`, because macOS leaves out the session's screen-lock key while the screen is unlocked. A missing key now reads as unlocked; a lock key that reads true, or a lock notification, still refuses and closes an open window. From a report of a run on a real Mac (macOS 26.7); not run again on a real Mac.
- On an interactive macOS VM run (`wardyn run --machine-kind macos_vm --experience interactive --window`, or `desktop_window: "at_start"`), the window now opens once the run's preparation finishes. Before, it was asked for while preparation was still running, refused as preparing, and never asked again. Not yet run on a real Mac.
- The macOS VM page no longer says the console has no Open Mac window button; the run page has one.

## [0.10.0-rc.12] — 2026-11-03

### Before you upgrade

- Endpoint sign-ins made before this release must be made again once; runs ask for it.
- Split-role installs (`WARDYN_PG_MIGRATE_DSN` set) whose app role does not pick up new tables through the `ALTER
  DEFAULT PRIVILEGES` step in Operations: grant the app role `SELECT, INSERT, UPDATE, DELETE` on `wall_presets` and
  `kept_machines` (migrations `0248_wall_presets` and `0250_kept_machines`). Without them the wall preset routes and
  kept machines fail. An upgrade from before 0.10.0-rc.9 also
  needs every grant the 0.10.0-rc.8 notes list: `SELECT, INSERT, UPDATE, DELETE` on `workloads`, `workload_bindings`,
  `workload_run_ends`, `macos_vm_disks`, `aws_sso_refused_tokens`, `cli_sign_ins`, `ui_desktop_closed`,
  `desktop_signins`, `desktop_signin_shares`, `desktop_signin_runs`, `approval_rules`, `governance_restrictions`,
  `governance_restriction_members`, `governance_unit_admins`, `oidc_session_sids` and `desktop_vm_sign_ins`, and
  `SELECT, INSERT, UPDATE` on `runner_health`, `launch_preset_version_floor`, `cli_sessions` and `governance_boundary`
  (and on `run_harnesses` if it turns on `WARDYN_MULTI_AGENT_TASKS`).
- A member policy with no desktop setting now gets the ceiling's desktop controls (for example, copy out opens where the ceiling allows it). Set `desktop` on the policy to keep the narrower defaults.
- Existing `image` capability grants whose value ends in `/*` (for example `registry.example.com/team/*`, which matched nothing until now) now match every image under that path. Review them before upgrading. A stored image value with any other `*`, or a prefix without a registry host, still matches nothing.
- After a downgrade to an older server, a person whose walls use the new settings (tool rules, push rules, recording, auto-stop, or Azure DevOps or GitHub capabilities) gets errors when they create a run, until they run `wardyn walls clear` or save their walls without those settings.
- After a downgrade to an older server, a run that was started with a wall preset keeps running but is no longer held to the preset when it restarts or when an approval is decided, because the older server ignores the preset it was started with.
- An older server does not know `push_rules.deny_all`, so its strict reader refuses walls or presets that name it. Before a downgrade, save your walls without it.
- Upgrade each wardyn-runnerd before wardynd where a governance profile will set `allowed_models`: a runner that does not know that proxy setting is refused every run with a list (`runner_proxy_setting_unsupported`), at create and at a revive's proxy replacement. A run with no list sends an older runner what it sent before.
- A run policy whose github_capabilities leave out code_write now stops git pushes; add code_write where runs should push.

### Added

- An `otlp` audit sink (`WARDYN_AUDIT_SINKS`) sends each audit row to an OpenTelemetry collector as an OTLP/HTTP JSON log record, with the run, actor, target, outcome and, once a row carries one, the correlation id. It is best-effort logs, not lossless: shed events show in `wardyn_audit_sink_drops_total{sink="otlp"}`. A `bearer_token` needs an `https://` endpoint. Spans come later. Not run in CI against a real collector.
- An organisation setting, `images.require_digest` (off by default), refuses a member's run whose image, or the base image of a workspace it attaches or its policy carries (whoever owns the workspace), is not pinned by digest (`image@sha256:<digest>`), even where an exact or prefix image grant covers it. The refusal is `403` `image_digest_required`. A revive or restart checks again the image the run's create checked, under the rule as it is then; a run whose image was not pinned, made while the rule was off or before this release, is refused until a new run starts. Operators are not bound; their unpinned runs are marked `image_digest_exempt` on the run.create and run.revive audit rows. Migration `0249_run_image_input` adds `agent_runs.image_input`, NULL on every existing row; no grant changes. Not run in CI against a real registry.
- `push_rules.deny_all`, in a policy or your walls, refuses every push: git pushes through every git broker (GitHub and Azure DevOps), ref updates and deletes included, and Azure DevOps REST writes to a repository. The built-in Read-only and Sealed wall presets now set it, so they refuse pushes. A run that sets it is refused at start when it holds an `ssh_key`, or a `git_pat` while the git token broker is off (`push_deny_all_ssh_key`, `push_deny_all_resident_pat`).
- Under `push_rules.deny_all` the proxy also refuses a connection, on any port, to a code host Wardyn knows that none of the run's doors serves (`403`, `brokered:git:push-deny-all-forge`): github.com and its API, upload, gist and SSH hosts, hosted Azure DevOps, the site's SCM hosts and Git provider rows, and each git_pat and ssh_key grant's host. A wildcard, `allow_all_egress` or a first-use approval does not reopen one. The policy preview adds `push_deny_all_secrets` when such a run carries an environment or file secret, and the Read-only and Sealed presets say: "A push token you add as a secret could still push to a code host Wardyn doesn't recognize, if this run can reach it." The proxy reads a new config key, `forge_hosts`, so upgrade the proxy image and each wardyn-runnerd before wardynd. A runner too old for it is refused every run with `push_rules.deny_all` (`runner_proxy_setting_unsupported`).
- Wall presets: named walls that only narrow, kept for yourself, a group or the organisation, with three built in (Open, Read-only and Sealed). `GET`, `POST`, `PUT` and `DELETE /api/v1/wall-presets` manage them, with the run templates' authority: your own are yours, a group's template managers save a group's, and an organisation administrator saves the organisation's. Migration `0248_wall_presets` adds the `wall_presets` table and `agent_runs.wall_preset`.
- The run page says, above the terminal, when an interactive macOS VM run is still being set up (`macos_preparing` on `GET /runs/{id}`), even with windows onto the Mac off; and a Linux VM run's built-in desktop gets the Files panel, under the run's desktop rules, with no UI app declared.
- Kept macOS VM machines: keep, resume or fresh-start a person's macOS VM across runs (`wardyn run --keep-machine`, `--resume-machine`, `--fresh`, and `wardyn machines` to list and delete them).
- Kept machines in the console: New run and Review offer keeping the machine or resuming a kept one, the run page's Machine card shows the machine as it is now and flags one that still needs cleanup, Your settings lists your kept machines with their idle days, and an admin sets the site switch, its two limits under `macos_vm` (`kept_machine_idle_days`, 1 to 90 days, and `max_kept_machines_per_person`, 1 to 4), a governance profile's `deny_kept_machines`, and a runner's `macos_vm_kept_disk_limit_gb`. A kept machine's expiry is now the one its Mac wrote at the stop, so the Mac and Wardyn agree on one deadline; a stop that could not write it reports the expiry as pending. A kept machine's stop and next boot scrub what the run wrote to the guest (Wardyn's sections of `~/.aws/config`, the run's CA trust and certificate). The macOS VM page and the SDK reference cover keeping a machine, what stays on its disk, its expiry, deletion, limits and the API.
- Experimental, off by default: an agent-sandbox backend for the Kubernetes substrate (server mode), with its chart switch, admission policies, CI job and operator guide.
- An `image` capability grant may be a registry or path prefix ending in `/*` (for example `registry.example.com/team/*`), so one grant covers every image under it. A deny on an image also covers its other spellings, such as capitals in the host, `:443` and Docker Hub short names.
- An admin can set a model provider's run identity token to live 60 to 600 seconds (`gateway_identity.token_lifetime_seconds`; the default stays 10 minutes), so a killed run's tokens stop working sooner.
- An organisation's remote MCP server can be sent the run's signed identity, so its gateway knows which person, run and runner made each call: set "Token audience" in the server's component editor (`mcp.gateway_audience`). Turn run identity on for a model provider first; a run whose servers and providers share one audience at different addresses is refused.
- `wardyn window <run>` opens a window onto your macOS VM run on your own Mac, `wardyn window <run> --status` shows whether one can open and why your last one closed, and `wardyn run --machine-kind macos_vm --window` asks for it as the VM comes up. A refusal prints the server's sentence and exits non-zero. Built and unit-tested; not yet run on a real Mac.
- People can sign in to a custom model endpoint instead of pasting a token. An admin gives the `custom_endpoint` provider an `oidc` block (issuer, client id, an operator secret for the client secret, scopes, optional authorize and token URLs, and `identity_binding`, which by default requires the same issuer as the console's own sign-in; `none` is an admin opt-in). The provider editor offers Sign in beside Paste a token and asks first, naming the count, when a change would sign everyone out. A person signs in from Your model connections or from a run held for it, can sign in again or disconnect, and a run then calls the endpoint with that person's own short-lived access token, renewed from the stored refresh token. Setup status marks such a provider with `sign_in: "oidc"`. Editing the issuer, client, scopes or binding signs everyone out of that provider, and Paste a token answers `409` on a provider that signs people in.
- `POST /runs` takes an optional `correlation_id` (up to 128 characters of ASCII letters, digits and `. _ : / -`; otherwise `422` `correlation_id_invalid`), stored on the run and returned with it, and stamped as `data.correlation_id` on each of the run's audit events sent to audit sinks; the stored rows and their hash chain are unchanged, and it is masked with the run's other values. `wardyn run --correlation-id` sends it, and a run template leaves it out as a launch-time field. Migration `0254_agent_runs_correlation_id` adds a column; no grant changes.
- A governance profile can limit the models its runs may ask a hosted provider for (`allowed_models`; one model per line in the profile editor, blank for no limit). A narrowing overlay intersects with its base, and a list that shares no model with the base is refused. When a list binds a run, the run's proxy reads the model named in each Anthropic, OpenAI and Bedrock request and refuses one not on the list, or one it cannot read, before anything is forwarded; file and skill uploads are refused while a list is set. Review shows the list and warns, without blocking, when a harness's model or small model is not on it; the run page shows the list the run was held to; Audit shows the gate's refusals (`brokered:allowed-models`). A provider whose calls the proxy cannot read (an AWS sign-in signed in the sandbox) is refused with `allowed_models_unchecked` at Review, create and start. An admin governed under `WARDYN_GOVERN_ADMIN_RUNS` is held to their profile's list. Migration `0255_agent_runs_allowed_models` adds a column; no grant changes.
- A `bedrock_bearer` model provider with `bedrock.base_url` set can send the run's signed identity to its gateway (`gateway_identity`), as a key provider can; the provider editor offers the switch. A `bedrock_sso` provider is refused with the way out: SigV4 signs the Host, so a gateway at its own name would break the signature.
- A model provider that sends run identity can leave prompt checking to its gateway: `gateway_identity.inspection: "gateway"` ("Let this gateway check prompts" in the provider editor; any other value is refused). The route is sealed with it, so a change refuses the next token like any other route edit, and Wardyn still checks the run's own secret values. The run's Policy tab names each provider whose gateway checks prompts, Audit names each prompt left to a gateway (`deferred_to` on `llm.scan` rows), and the setup page says who inspects. Not available on a Bedrock provider, whose calls Wardyn's whole engine scans.
- A `custom_endpoint` provider's `claude-code` entry may name a `fast_model`; a run then gets the harness's small-model variable set to it, with or without a main model. Rows without it send what they sent.
- A service can ask Wardyn to check its port itself: `health.probe: control_plane`, chosen at create and fixed for the run (the New Run service section has the switch once a port is named). The health sweep does a GET of the health path with no credentials and no redirects, or a TCP connect, bounded to 5 seconds. Wardyn's own failures count against the service's `failure_threshold`; a check that cannot run reads unknown, never unhealthy. The reading is stored beside the sandbox's, audited with source `control_plane`, and shown on the run, the workloads view (the worse of the two) and `wardyn run get`. With the check on, renewal needs both readings starting or healthy. With it off nothing changes.
- Wall presets in the CLI and the console: `wardyn walls presets` lists them, with `show <name|id>`, `set <name> -f FILE [--group G | --org]` and `delete <name|id>`, and `wardyn run --wall-preset <name|id>` starts a run under one (also with `--file`). New run's Tools step offers Open, Read-only and Sealed with saved presets under More presets, Review shows the chosen preset and a Pushes row, the run page's Why card names the preset and who chose it, and a Wall presets card in Settings lists, creates, edits, copies and deletes the ones you may write. A run keeps a frozen copy of its preset (`wall_preset` on the run; a personal preset is named to its owner only), so a later edit or delete changes nothing on it; a queued run whose preset changed or went is ended; a preset you cannot see is refused with `wall_preset_unknown`.
- The console can open the window on the Mac. With the site switch `macos_vm.allow_attended_window` on (Settings, macOS virtual machines), New run offers "Open in a window on my Mac" for a macOS VM run (`desktop_window: "at_start"`), and the run page gets an Open Mac window button in place of the printed command, which also says why a window refused at launch did not open. A refusal names the `desktop_window` field, so the console clears that choice and checks the draft again. Review refuses a window a launch would refuse. While the switch is off, neither shows. Not yet run on a real Mac.
- An admin whose own runs stand outside governance now sees, on Review and in the policy preview (`ungoverned`), the governance unit and headline limits a member of their type would be held to, or why there is nothing to compare with.
- Docs: the macOS VM page now covers the attended window (how to open it, the site switch, every refusal, closing, audit rows) and an interactive developer sandbox quickstart.
- Docs: a new page on signing people in to a custom model endpoint, and a new page on checking image signatures in your cluster with Sigstore policy-controller or Kyverno. The second recipe is not run in CI.
- Docs: Windows VMs are listed as not available yet. A Linux desktop VM uses your own image for now.
- Docs: joining a run to a platform team's own kernel sensor on Kubernetes.
- Docs: running confinement class CC3 with Kata on a managed Kubernetes cluster (`k8s.runtimeClasses.CC3`), a partial Azure DevOps template example, that deleting a secret stops masking it, and every flag that goes with `wardyn run --file`.
- Your walls can now also narrow tool rules, push rules, recording, auto-stop and Azure DevOps and GitHub capabilities, and the Settings card keeps them across a save.

### Fixed

- Focus mode on the run page: the keyboard now skips the run controls it covers, starts on Exit focus, and returns to the Focus button when you leave.
- Endpoint sign-in: a changed endpoint now tells the people it signed out why ("destination changed"), and signing in again clears that note, as every other provider sign-in already did.
- Endpoint sign-in: a stored refresh token is used only with the provider address and token URL it was signed in at. A renewal for a provider whose address, authority or token URL moved now answers "sign in" without sending the token.
- A `github_capabilities` list without `code_write`, in your walls or the run's policy, now stops git pushes to GitHub: the git broker refuses them with a sentence saying why (`brokered:git:push-capability`), and the run's GitHub tokens are read-only. An `azure_devops_capabilities` list without `code_write` does the same for an Azure DevOps `git_pat` grant.
- Opening an app from Attach from your terminal now opens its new tab inside your click, and says to allow pop-ups when the browser blocks it. The SSH option is chosen by default when you have a key, and the VS Code Remote-SSH reason no longer says the sandbox has no internet.
- Review no longer greys out Launch with "The setup changed" after you have read it for a minute: it asks the server again when the check ages out and when you press Launch, says "Checking with the server…" while it does, and says the check is more than a minute old if it cannot refresh.
- Your walls: Remove all walls now asks first, since it also removes walls set from the CLI. Barrier at least and When a site is not listed can be set back to "No minimum" and "Your organisation's choice". A typed site keeps only its lower-case host, with a hint for the format, and the clipboard options say VM desktops have no copy and paste yet.
- Small console fixes: Take over, the Desktop tile and Watch desktop now open their new tab inside your click (and close it again if the request fails, or say to allow pop-ups when the browser blocks it); Needs you follows the surface the server chose for a takeover; a paused run's row shows a still ring instead of the pulsing running dot; ending a waiting run names it by title in its toast; destructive admin rows in the Audit log (erasure, deprovision, revoke, retire, delete) read as failed or refused when they were, and a partial erasure says so; the Your walls clipboard hint and the Audit empty state say what they apply to.
- Ending a run uses one verb: the run page button and its confirm both read "End run", matching the notices. A run that was lost (after a reboot, an outage or when it went missing) shows that state instead of "Paused". The macOS VM desktop's copy-and-paste note points to the terminal only, since Files is not there.
- Finish setup now sticks: the console re-reads its setup status right after the install records completion, so the logo or the Admin home no longer sends the admin back into Getting started for up to five minutes.
- Getting started no longer says the wrong thing in several places: an SSO install whose SSO side does not answer shows "SSO · not answering" instead of "isn't configured", a failed workspace list says so with a Retry instead of "No workspaces onboarded yet", Review no longer shows two verdicts when the barrier is ready and only a model provider is missing, a Kubernetes cluster with no barrier is pointed at the chart settings instead of the Docker daemon, and the welcome screen says "Couldn't reach Wardyn" instead of "Checking…" forever.
- A browser desktop of a macOS VM run that had control now says so when a window opens on the Mac: the gateway closes it with code `4006` ("A window is open on the Mac…"), not `4001`, which the viewer read as the session's time limit, and it no longer reconnects view only without a word. Open the desktop again from the run page to watch. The desktop's page also links back to its run when the console's URL (`WARDYN_OIDC_REDIRECT_URL`) is set. The window's refusals for a site switch that is off (`mac_window_site_off`), a runner too old (`mac_window_runner_too_old`) and a runner on another server (`mac_window_not_here`) are reworded. Not yet run on a real Mac.
- Needs you: taking over a Linux VM run, or a macOS VM run whose Mac allows control, now opens its desktop, and a view-only macOS desktop says so. A hold on your own run raises a toast naming the site, both toasts say "Open run" or "Review", and a snoozed request keeps the agent's reason and Decline.
- The run page's VM desktop tile is clearer. It says that on macOS 26 you watch in the browser and work in the window on the Mac, with an Open Mac window button when the site allows one, says when the gateway check failed (with Try again) instead of showing nothing, shows a pause or resume error under its buttons, and no longer shows a View only chip beside Off. Files on a VM says copy and paste aren't available, a macOS disk that couldn't be removed says who to tell, a Watch desktop error wraps instead of being cut off, and the Identity card has a Machine row and separate Started and Started by rows.
- Docs no longer say a gateway run identity token always lives 10 minutes. It lives 60 to 600 seconds, and 600 by default.
- A window onto a macOS VM run no longer opens when the Mac can't tell whether its screen is locked: it refuses with `mac_window_lock_unknown`, and an open window closes as soon as the Mac locks or its lock can no longer be told. Reopening after a lock needs a new `wardyn window`. Not yet run on a real Mac.
- New run: choosing a virtual machine no longer leaves "Start the run in this harness" on. The switch is off and disabled for a Linux or macOS VM, whose terminal opens a shell, and the refusal now names that switch, links to Harnesses and gives the `interactive_start` field. The macOS choice also says the browser desktop is view only on macOS 26.
- A person's Azure DevOps or GitHub capability walls now narrow the provider's default when the run's policy names no capability list. Before, the default applied in full (for example code writes under a read-only wall) and create only warned. A wall with nothing in common with the default refuses the run.
- A macOS VM run's screen no longer locks while it sits idle: the guest tries to turn the screen lock off at the start of each run; if that fails, it logs why and the run starts anyway. Not yet run on a real Mac.
- Review now refuses a macOS VM run that starts its terminal in the agent CLI, or whose profile pauses idle runs, instead of failing at start or showing a pause that never happened. A macOS VM is never paused when idle.
- A macOS VM run whose disk drive is still held by a run that just ended now waits for it, up to 90 seconds, instead of failing at once. 0.10.0-rc.11 refused at once while the earlier run's VM was still shutting down. A run whose drive a live run still holds now also waits up to 90 seconds before it fails, where 0.10.0-rc.11 failed at once.
- Settings now says so when it cannot reach Wardyn ("Couldn't reach Wardyn", with Retry) instead of showing made-up Host facts such as "Can't launch", on both Admin Settings and Your settings. On Admin Settings, Host Re-check now shows that it is working and when it finished, and a Kubernetes cluster with no barrier gets the Kubernetes fix line. Saving two site-config cards one after the other no longer drops the second change with a false "someone else changed these settings", and Custom components asks before you leave with an unsaved edit. The Branding Save button says an empty organisation name is why it is off, a failing approval-notification channel or an Azure DevOps token that needs you now shows a Needs attention pill while collapsed, every card's load failure uses one compact error block, the Settings group names are real headings, and the macOS background-watch switch says who can watch and that a Mac that allows desktop control is never watched.
- Audit reads better. An admin's own actions (adding or erasing a person, signing someone out, creating or revoking a token, invitations, service identities, role mappings, templates, dropping an audit partition) read as sentences instead of raw keys, with the key kept in the row's tooltip. The Actor filter and chip know service identities. The search box has a name, the live chip says "Not updating — retrying" when polling fails, and the page copy says "brokered credential" and "Showing the newest N events" for a feed that is served newest first.
- Run status words are plainer. A paused run reads "Paused" in the list and the run header. The Egress tile shows denies beside allows ("40 allow · 3 deny"), and a failed read of Files or Sandbox keeps the last good value or says "Couldn't load right now." Rows name a macOS VM or Linux VM. End run on an ended run now asks first, as Kill does, and both say "Kill run"; their toasts name the run's title. The Launched screen shows a placeholder while it loads, and a missing terminal recording no longer says "cast".
- The run page's layout now saves as you arrange it and says so ("Saving layout…", "Layout saved"), instead of a separate Save as my default button. A save that fails stays on screen as "Live layout wasn't saved" (or Finished) with Retry, also after Done, where it used to be silent. In Focus mode, Escape and Ctrl+\ now reach the terminal (vim keeps its Escape) while the terminal has focus, and Ctrl+Shift+Backspace, the chord the terminal already teaches, leaves focus from anywhere.
- The policy editor and the Wardyn boundary page now say what is true: the `desktop` field help says an omitted `desktop` under a ceiling takes the ceiling's rules, the `recording` help says the run owner's walls can raise the level, the Policies empty state says a stored policy is clamped to a governance profile's ceiling (it can narrow but never widen), and the boundary and recording words match Governance ("profiles below"). The Fields list no longer points at `docs/POLICIES.md`, a file a packaged install does not have.
- People and tokens copy is truer. A service identity's inbound-token help names the identity (not "you"), a person's own API token can run to 1 year or the server's limit, the token reveal and the People page call the `admin` role "Admin", the People confirms are titled for what they do ("Sign out everywhere?", "Remove all SSH keys?") with toasts that say what happened ("Removed 2 SSH keys for …", "… was already listed."), the Holds cell says "SSH key", the drawer says how to invite someone to create a token, and a lifetime hint is read with its control. Erase below the security tier is titled "Erase someone's credentials".
- Kubernetes teardown now identifies a run by its pod's fixed name, never by a label.
- Setup's Corporate network step no longer loses a proxy URL or secret name you typed while the saved settings were still loading: the proxy editor waits for them, says so when they cannot be read (with Try again), and can no longer save over settings it has not read.
- Opening a window onto a macOS VM run right after the org ended the last one now waits briefly for that window to finish closing, instead of refusing with "This run's window is already open". It still refuses if the close takes longer than the wait.
- With the console on, a `GET` under `/.well-known/` that names no file now answers a plain `404`, and so does the gateway key set's address while run identity is off; before, both served the console shell. A file placed there is still served. Docs: the AI gateway page describes the three TLS hops and warns that a TLS-inspecting proxy on the gateway hop reads the identity header and the bearer.
- `ssh -L`, sftp and Attach from your terminal now work against a Linux or macOS VM run: the guest agent forwards the port to the guest's loopback itself (`forward <port>`), where a container run uses socat, and sftp runs the Mac's own `sftp-server`. A forward or a UI app to a macOS VM's Screen Sharing ports (5900-5909) is refused with a sentence that points to the run's window. Not yet run on a real Mac.
- An interactive macOS VM run now gets its repository clone, every listed harness's files and its managed credentials before the person reaches it; 0.10.0-rc.11 opened its terminal on an empty machine. The run's status detail reads "prep: running" and then "prep: done"; a prep that fails fails the start with the guest's sentence, and a restart during the prep ends the run with a sentence instead of leaving it running with its window refused. Not yet run on a real Mac.
- `wardyn-runnerd drive open`: after a write back the finished folder under `~/Wardyn Drives` is moved to the Trash with one rename, not deleted, and a file the editor changed or made after what was sent was frozen is kept, with a line that says so; a discard removes the folder. The guest copy leaves out extended attributes and the volume's own `.Trashes`, `.Spotlight-V100`, `.fseventsd` and `.TemporaryItems`, which no longer count as unread items. Not yet run on a real Mac.

### Changed

- The run page says "End run" everywhere: the header button, the confirm ("End <title>?") and its note, and the retry and error messages. Signing someone out everywhere now says it ends browser and CLI sessions, and that personal API tokens and SSH keys keep working. The invite command shown for a person is quoted so any subject is safe to paste into a shell.
- A service identity may now call `POST /templates/import`, which only parses and normalises the document it is sent (it stores nothing, grants nothing and reads no stored policy, secret, grant or workload), so a service token can preview a change before applying it, as `wardyn apply --dry-run` does.
- A macOS VM run's desktop is recorded again. At Screen and actions a watched macOS VM desktop goes through the gateway's recorder, as a Linux VM's does, with the same pauses and audit rows; at Terminal it is not recorded. Only a watched screen is kept, and its frames are kept as the guest drew them, unmasked. A window on your own Mac never crosses the gateway, so it is refused at that level (`mac_window_unrecorded`). A macOS VM run under a recording requirement, a locked level, a ceiling or your walls is no longer refused at create, placement or start; Review shows the level the start will resolve. A macOS VM task run's agent output is recorded as a masked cast that leads the run's Recording tab; a run whose cast cannot open is marked `runner_declined` and fails under a requirement, and a restart records it again from the runner's copy or says why it could not. This replaces the 0.10.0-rc.11 line that said a macOS VM run's desktop is no longer recorded. Earlier 0.10.0 prereleases may have stored its screen frames, unmasked, at the Screen and actions level; `desktop.recording.write` on a macOS VM run shows whether they did. Recordings already stored are kept forever by default, unless `WARDYN_RECORDING_RETENTION_DAYS` is set. Not yet run on a real Mac.

## [0.10.0-rc.11] — 2026-11-02

### Added

- A Linux VM run can mount a drive bound to its runner. An older runner and a Kubernetes runner are refused at Review. Do not rely on a file lock across two VMs.
- New Run now says which of your own walls apply to the run, in one line on Review above the setup groups and above the Tools step totals, with a link to change them (opens in a new tab).
- Governance profiles have a new limit, "Deny watching background desktops" (`deny_background_desktop_watch`), and Settings has an "Allow watching background runs" switch for macOS virtual machines. The switch is on by default.
- A governance profile write now warns, without refusing, when its Azure DevOps capability list reaches past a provider row's capability ceiling (a member who asks for one of those is refused at launch). It also warns when a standalone profile leaves out the local-run component limit, or sets desktop, recording, tool rules, auto-stop or LLM inspection weaker than the deployment default.
- macOS VMs: `wardyn-runnerd drive open <name>` opens a disk drive in a throwaway virtual machine meant to run with no network and copies its files to ~/Wardyn Drives/<name>, so this Mac never reads a disk an agent wrote. Press Return to write your changes back as the agent, or Ctrl-C to discard them. Links, devices and set-id files are skipped, and every file is marked as downloaded with no execute permission. Built and unit-tested; not yet run on a real Mac.
- Experimental: an admin can turn on run identity per model provider. Each model call to that provider's gateway then carries a short-lived signed token naming the person, the run and the runner. Off changes nothing a run sends. A run with identity on is refused on an older remote runner rather than calling the gateway unsigned.
- Added: The model provider editor can send each model call's person, run and runner to a gateway you name, for Anthropic API key, OpenAI API key and own-endpoint providers (experimental until 0.11). It shows the issuer and key address to give your gateway (#3861).
- The run page has its own Desktop tile for a VM run. The Open desktop button sits at the top, and sign-ins and the Files panel stay with it.
- A background macOS run you may watch gets Watch desktop from that tile. It is view only, and each refusal shows the server's sentence.
- Saved run-page layouts gain the Desktop tile when they load. Removing it is remembered.
- Added: Every widget on the run page except the Terminal now has an Expand button in its header that shows the same live widget in a large view, and a widget with more to scroll shows its scrollbar and an edge tint (#3940).
- Added: the audit rows for a model call through an AI gateway, and for an MCP call on the same run, now record `gateway_call_id`, the id Wardyn sent as `X-Wardyn-Call-Id`, so a run's audit row can be matched to the gateway's access log line (#3864). With no gateway route nothing is sent or recorded.
- The Audit screen and a run's Audit tab show a gateway call id, with a copy button, on a model call or MCP call row of a run whose gateway has identity on. The same id appears in the gateway's log (#3864).
- Gateway token rows read "Signed a gateway token for <provider>" or "Refused a gateway token for <provider>" and appear under the Credentials filter.
- Added: an external AI gateway can now check Wardyn's run identity token. The setup page lists every claim and how to join a run's audit rows to the gateway's log by call id. The token format and key-set address are experimental until 0.11.
- The signing keys publish at `/.well-known/wardyn-experimental/gateway-jwks.json`, rotated with `wardynd -rotate-gateway-identity-key`; both experimental until 0.11.
- A watch of a background macOS VM run is refused with `desktop_sign_ins_off` when the agent's desktop sign-ins are not allowed.

### Changed

- API: a run list without `include=recording_meta` leaves `has_recording`, `recording_bytes` and `recording_duration_sec` off each run. It used to say `has_recording: false` for a run that has a recording. A single-run read and an opted-in list always carry `has_recording`. In the Go SDK `has_recording` is now a `*bool`.
- A `pass_through` governance unit written with `overlay: {}` and no parent is refused, as the same write with no `overlay` already was. A pass-through unit already under Default still saves.
- `deny_interactive` also refuses a desktop's file transfers and `novnc`: a person watching a `novnc` desktop on a run whose profile denies interactive sessions is refused after upgrade, at entry and at the session's next check.
- Beginning a desktop sign-in or sharing a login from one is refused under `deny_interactive` or `deny_ui_apps`. A refused begin leaves the agent running, and ending a sign-in stays available.
- Opening a desktop's Files panel on a paused run no longer wakes it. Only a transfer the rules allow does.
- A BYOI interactive run with a task on a runner skips its warn-only selftest, as one with no task does, so it shows fewer selftest rows.
- Setup and Claude sign-in no longer accept Wardyn's `agent-base` image as a sign-in image. A one-line install shows Claude sign-in as not ready until you supply a Claude Code image; [the steps](docs/operations/build-images.md#claude-sign-in-image) are in the docs.
- macOS VMs: a run on a view-only base can be watched from the run page with Watch desktop. Keys, pointer moves and paste never reach the guest. The console's viewer sends no input on a view-only desktop; a new `ui.input.drop` audit row records the first dropped key and pointer move each time the desktop socket opens, and a paste is recorded as a denied `ui.clipboard.transfer`. Built and unit-tested; not yet run on a real Mac ([#3922](https://github.com/cjohnstoniv/wardyn/issues/3922)).
- macOS VMs: a background run's desktop can now be watched, view only, from its run page. Typing, clicking, pasting and clipboard copy are disabled, and watching neither wakes a run nor keeps it alive. A screen can show what the run keeps out of its logs. It's on by default; a site switch or the governance limit `deny_background_desktop_watch` turns it off, and an open view ends within a minute. A run started before the upgrade can't be watched. Built and unit-tested; not yet run on a real Mac.
- A run's memory and disk on the sandbox card read in GB from 1 GiB, as MB below that. An egress row no longer says "Recorded offline"; a decision that reached Wardyn a minute or more after the runner made it shows "Reached Wardyn later" and the time it was decided.
- When a member's folder mount is refused for being outside the member workspace roots, the message now names `WARDYN_USER_WORKSPACE_ROOTS` and says an operator sets it.
- macOS VMs: `edit-base` now says on its first line that the edit is of a copy of the VM base, not this Mac, and that the copy takes about 2 minutes to start. Its window lists, in order, the code, the setup questions, who you are signed in as and where Terminal is, Full Disk Access and the certificates; a certificate the base reads as not trusted prints with its subject, SHA-256 and the command. The recheck prints what it read (each certificate, Full Disk Access and the desktop), a guest that cannot answer reads "could not be read" and never "allowed", and `status` prints the last edit's read with its time. Built and unit-tested; not yet run on a real Mac.
- macOS VMs: `wardyn-runnerd macos-vm status` now reads the newest tools-port dial from the run boot, the prepare log and the tools update log, says where each line came from, and says that its number is this Mac's own sum and does not read the organisation's limits. A tools update now stamps the dial it reports, so an older connected dial can no longer out-rank a newer failure. Built and unit-tested; not yet run on a real Mac.
- A background run shows no Take over button, no Attach tile, and no Watch button in the terminal notice. A background run's takeover request shows no Take over in the approval card, the toast or the runs list. Decline and the toast itself stay.

### Fixed
- A macOS VM run's desktop is no longer recorded, as Review already said. Earlier 0.10.0 prereleases may have stored its screen frames, unmasked, at the Screen and actions level; `desktop.recording.write` on a macOS VM run shows whether they did. Recordings already stored are kept forever by default, unless `WARDYN_RECORDING_RETENTION_DAYS` is set.
- Review and the docs now say that console and SSH terminal sessions on a macOS VM run are recorded at this run's recording level, as they always were (`session.recording.write`).

- The membership-move dry run counts a live run whose owner a membership names by a verified alias.
- A BYOI selftest that did not run says so, in the run and in its audit row, on an interactive run with no task.
- A shutdown no longer leaves a sign-in run's stop retrying past its wait on a slow store. The pass under way finishes, no further one starts, and the run's idle cap ends a run left behind.
- A Linux VM managed file with no mode is delivered at the default mode.
- The runner and Azure DevOps token reads in the console stop polling when signed out and no longer stack requests.
- A macOS VM helper logs a refused pairing before closing the connection.
- An interactive run with a task on a runner no longer takes an execution generation it never spends, which refused its revive. A run from before the upgrade stays unrevivable: start a new run.
- A unit admin's membership add checks scope first, so someone outside the unit can't tell which user types exist.
- A Linux VM's kept work folder is refused at Review, naming the folder.
- A service with no end time and no lifetime cap says it runs until stopped, at New Run, in preflight and in the create response.
- Compose passes `WARDYN_MIGRATE_TIMEOUT` on from `.env`.
- Docs: what a policy's `github_token` grant needs before a runner's first run.
- A governance write that clears the UI gateway's caches is no longer undone by a check already in flight: the next request on that replica, warm connections included, meets the change, and the caches clear before the write's success reaches the client. Other replicas keep their existing 30 seconds.
- A run waiting on its runners' limits now names the same limit on the Launched screen and on the run page, and the sentence follows the runners as they change. With several runners it gives an example runner instead of calling one the nearest, and a single runner reads "<runner> is at a limit: <limit>." It names the limit that holds the run: Runs at once, or the CPU, memory or disk total. A launch into a capped pool with room whose runners were full named the pool's limit; it now names the runner's. The wait reason token is unchanged.
- A macOS VM run that asks for a disk drive still being released now waits up to 90 seconds in total for all its drives, and the failure after the wait states the wait. A drive held by a live run still refuses at once.
- A macOS VM run's drives now show at the guest's path under `/Users/agent` in the `run.create`, `drive.member.create` and `run.drive.mount` audit rows and in the mount map the dry runs return, instead of the container path under `/home/agent`.
- The console's terminal reconnect budget is no longer reset by a socket that opens and says nothing: the first terminal byte, or 15 seconds open, resets it. The runner no longer logs an ordinary detach as a "session read failed" warning, and names which side failed when it does warn.
- The governance admin view no longer says the deployment default set a wall that a standalone profile sets itself. A size the profile omits still follows, and is credited to, the deployment default. No resolved value changes.
- A run that Amazon Bedrock refused with any other HTTP 4xx or 5xx now names, in its failure hint, the status and the error class AWS answered with. Wardyn does not record AWS's message; the agent's own output may hold AWS's explanation, and so may the run's recording if the run has one ([#3891](https://github.com/cjohnstoniv/wardyn/issues/3891)).
- Fixed: The Service panel on the run page is no longer clipped on a short tile (#3940).

### Security

- An approval rule no longer approves a host a person denied on that run after the run's proxy is replaced. The approval waits for a person, and a person's later allow undoes the deny.

## [0.10.0-rc.10] — 2026-11-01

### Before you upgrade

- 0.10 can't be rolled back by redeploying 0.9.x. Boot refuses a database that records a migration the binary does not
  ship. Even past that refusal, a 0.9.9 binary can't add a library source, with or without a bound folder, because
  migration `0247` drops the unique constraint its save relies on. This corrects the 0.10.0-rc.9 note. Take a database
  backup first; to go back, restore it ([#3909](https://github.com/cjohnstoniv/wardyn/issues/3909)).
- An upgrade stops the old replicas first, because the chart uses Recreate, so older notes that say "rolling upgrade" mean
  an older server still running while the new one starts ([#3909](https://github.com/cjohnstoniv/wardyn/issues/3909)).
- A large recording store can need a longer migration timeout. Migration `0216_recordings_size_tail` reads every stored
  recording once, and the whole migration run is bounded by `WARDYN_MIGRATE_TIMEOUT` (default 5 minutes). A timeout rolls
  that migration back, and every later boot starts it again and fails the same way until the timeout is long enough. Raise
  it before you upgrade; the chart takes `h`, `m` and `s` units only. Compose does not pass the variable on from `.env`: put
  `WARDYN_MIGRATE_TIMEOUT: 30m` under `services.wardynd.environment` in a `docker-compose.override.yaml`. Helm: use
  `--set env.WARDYN_MIGRATE_TIMEOUT=30m` or an `extraEnv` entry, and give `helm upgrade --timeout` more than the migration
  needs ([#3908](https://github.com/cjohnstoniv/wardyn/issues/3908)).

### Added

- macOS VMs: a run on a view-only base can be watched from the run page with Watch desktop. Keys, pointer moves and
  paste never reach the guest; a new `ui.input.drop` audit row records the first dropped key and pointer move of a
  session. Built and unit-tested; not yet run on a real Mac ([#3922](https://github.com/cjohnstoniv/wardyn/issues/3922)).
- `wardyn run` has `--machine-kind` (`container`, `linux_vm` or `macos_vm`) and `--macos-base`, so the command line can
  start a macOS or Linux VM run. They work beside `--file`, since a Run document cannot carry the kind of sandbox
  ([#3882](https://github.com/cjohnstoniv/wardyn/issues/3882)).

### Changed

- macOS VMs: on macOS 26, apps that use only the system trust store, such as Safari and Electron apps, can't use
  inspected hosts, and behind a TLS-inspecting proxy also fail on tunnel-only hosts until the organisation's root is
  trusted by hand in `edit-base`. Tools that read a CA variable work. `gh` and `kubectl` have not yet been run on a real
  Mac. This qualifies the 0.10.0-rc.9 entry on the base's keychain ([#3923](https://github.com/cjohnstoniv/wardyn/issues/3923)).
- The macOS VM setup window and the edit-base window now have a Show code button, so the one-time code can be read in
  the window instead of with `cat`. It stays hidden until you click, a second click hides it, and it cannot be copied
  from the window. Not yet run on a real Mac ([#3906](https://github.com/cjohnstoniv/wardyn/issues/3906)).
- On a Mac set up with `wardyn-runnerd setup`, `wardyn runner upgrade` now says it replaced only the runner and tells
  you to install the new `wardyn-runnerd.pkg` and rerun `wardyn-runnerd setup`. Upgrading all three files in one step is
  planned for 0.10.x ([#3873](https://github.com/cjohnstoniv/wardyn/issues/3873)). Rerun `setup` only on Macs set up with `setup`: a LaunchAgent of your own that runs
  `/usr/local/wardyn/bin/wardyn-runnerd` needs no rerun ([#3925](https://github.com/cjohnstoniv/wardyn/issues/3925)).
- When a disk drive is still held by a macOS VM run that just ended, starting another run with it, or deleting it, now
  says the drive is still being released and to try again shortly ([#3899](https://github.com/cjohnstoniv/wardyn/issues/3899)).
- When a tools update cannot remove a CA from an existing macOS base, it now says why and that `prepare --reprepare` is
  the way to drop it ([#3879](https://github.com/cjohnstoniv/wardyn/issues/3879)).
- The status strip's Sandbox tile reads "Not available" on a macOS VM run; the guest's numbers are on the Sandbox card
  ([#3924](https://github.com/cjohnstoniv/wardyn/issues/3924)).
- On the Azure DevOps Entra lane, git now prints why a push was refused when its paths could not be worked out, or when
  a review path matches on an unattended run, including what to do next ([#3921](https://github.com/cjohnstoniv/wardyn/issues/3921)).
- A push that removes content from a review path is not held on any forge: deleting a file, renaming it out, or
  replacing a directory or parent at or above the pattern with a file. The forge's own branch policy is what protects
  that file ([#3921](https://github.com/cjohnstoniv/wardyn/issues/3921), [#3928](https://github.com/cjohnstoniv/wardyn/issues/3928)).
- A review-path match on an unattended run stays refused and cannot be approved later. Every macOS VM run is unattended
  ([#3921](https://github.com/cjohnstoniv/wardyn/issues/3921), [#3926](https://github.com/cjohnstoniv/wardyn/issues/3926)).

### Fixed

- On Azure DevOps, `require_review_paths`, `deny_paths`, `max_file_size_mib` and `deny_new_executables` now compare what
  a push does not carry with its base commit through the Git REST API, so a push is held or refused only for what it
  changes. Tested against a fake Azure DevOps server only ([#3921](https://github.com/cjohnstoniv/wardyn/issues/3921)).
- macOS VMs: on a view-only base, the prepare log and `macos-vm status` now say that macOS 26 doesn't keep Screen Sharing
  control, not that setup was wrong. The run page offers "Watch the captured session" only on a run with a recording
  ([#3922](https://github.com/cjohnstoniv/wardyn/issues/3922)).
- macOS VM runs: Files changed reads the VM's work folder (`/Users/agent/work`) instead of reporting "No git repository
  at /home/agent/work". The Overview Output pane shows the run's kept output when there is no recording to replay, live
  on a macOS VM run and on any ended run without a recording. The Sandbox card shows CPU, memory, disk and processes as
  reported by the guest, for the whole VM. Not yet run on a real Mac ([#3924](https://github.com/cjohnstoniv/wardyn/issues/3924)).
- `wardyn-runnerd macos-vm status` lists all three setup steps while macOS waits for setup by hand, and no longer keeps
  showing a `tools_dial` failure from an earlier run after a later prepare connected ([#3925](https://github.com/cjohnstoniv/wardyn/issues/3925)).
- A macOS VM run no longer writes a second `run.complete` audit row with outcome `failure` and a teardown deadline error
  ([#3886](https://github.com/cjohnstoniv/wardyn/issues/3886)).
- A macOS VM run refused for its size when the cap comes from the Default governance unit now names that unit and says
  to assign the person a governance profile that allows at least the CPUs and memory the refusal names ([#3885](https://github.com/cjohnstoniv/wardyn/issues/3885)).
- Setting up a macOS VM by hand with the wrong password for the one-time code now fails within seconds and says the
  password does not match the code, instead of ending in "handshake failed: EOF" ([#3874](https://github.com/cjohnstoniv/wardyn/issues/3874)).
- A single command in the macOS VM guest that waits for a person, such as a macOS permission prompt, now fails the step
  after 30 minutes with a clear message ([#3879](https://github.com/cjohnstoniv/wardyn/issues/3879)).
- A macOS base `ca` item that is not an X.509 v3 CA certificate is now refused with the reason when the list is read,
  before the restore image downloads ([#3876](https://github.com/cjohnstoniv/wardyn/issues/3876)).
- A macOS bases file that cannot be read now shows its base as failed, with the reason, in `macos-vm status` and on the
  runner's page, and no longer hides the other bases ([#3887](https://github.com/cjohnstoniv/wardyn/issues/3887)).
- A macOS VM run that names two disk drives, or a disk drive and a kept work folder, is refused at Review and create
  (`drive_one_disk`) instead of when it starts on the Mac ([#3897](https://github.com/cjohnstoniv/wardyn/issues/3897)). One its Mac refused at start no longer leaves an
  empty kept work folder drive counting against your limit ([#3900](https://github.com/cjohnstoniv/wardyn/issues/3900)).
- Deleting a disk drive on a Mac now removes its `.img.lock` file as well ([#3895](https://github.com/cjohnstoniv/wardyn/issues/3895)). A macOS VM run now mounts an existing
  disk drive even when the size limit was lowered after the drive was made ([#3898](https://github.com/cjohnstoniv/wardyn/issues/3898)).
- `deploy/runner/install.sh --uninstall` on a Mac now also removes the macOS VM helper and guest agent that
  `wardyn-runnerd setup` installed ([#3872](https://github.com/cjohnstoniv/wardyn/issues/3872)).
- A Linux or macOS VM run now reaches its proxy when `WARDYN_SANDBOX_PROXY_URL` is set. The value must be
  `http://host:3128`; any other form or port is refused for VM runs, with a message naming it ([#3888](https://github.com/cjohnstoniv/wardyn/issues/3888)).
- A release dry run on a branch whose name contains a slash no longer fails every image build ([#3877](https://github.com/cjohnstoniv/wardyn/issues/3877)).
- A run that Amazon Bedrock refused with any other HTTP 4xx or 5xx now names, in its failure hint, the status and the
  error class AWS answered with. Wardyn does not record AWS's message; the agent's own output, in the run's
  recording, says what was refused ([#3891](https://github.com/cjohnstoniv/wardyn/issues/3891)).
- A service whose lease renewal was due but refused, or whose authority check went unanswered, now writes a denied
  `run.end.set` audit row with the reason when that starts, then at most hourly. Before, it ended at its lease with
  nothing in the audit trail ([#3760](https://github.com/cjohnstoniv/wardyn/issues/3760)).
- `wardyn-runnerd doctor` judged a folders or Kubernetes runner's drive roots as a Docker runner's, so a root it
  passed could still be refused by `serve`. `runnerd.json` now takes an optional `substrate` key that `doctor` and
  `serve` read when `--substrate` is not given; the flag still wins, and a file without the key means Docker
  ([#3856](https://github.com/cjohnstoniv/wardyn/issues/3856)).
- The UI gateway now warns at boot when it cannot know its own scheme: behind TLS with no advertise URL or origin
  template, or with one written without a scheme. Before, an app that checks Origin against Host refused its own
  WebSocket with no sign why ([#3799](https://github.com/cjohnstoniv/wardyn/issues/3799)).
- Saving a workspace's requirements no longer reads a `write:` path on the server when the workspace binds that
  folder to a person's runner; such a key gets the text-only check
  ([#3857](https://github.com/cjohnstoniv/wardyn/issues/3857)).
- `wardyn workspace create --runner` no longer rebinds a library folder that is bound to another runner. It refuses,
  naming both runners and the folder ([#3858](https://github.com/cjohnstoniv/wardyn/issues/3858)).
- The run page finds its workload chip by run id instead of listing up to 1000 workloads, and the workload drawer hides
  Restart from readers the server would refuse ([#3789](https://github.com/cjohnstoniv/wardyn/issues/3789), [#3794](https://github.com/cjohnstoniv/wardyn/issues/3794)).
- `wardyn apply` and `wardyn delete` exit 2 on a flag, argument or file mistake, not the 1 of a refused launch
  ([#3774](https://github.com/cjohnstoniv/wardyn/issues/3774)).
- An application shape that names a stored policy is read against that policy's UI apps, instead of being refused for
  carrying none ([#3796](https://github.com/cjohnstoniv/wardyn/issues/3796)).
- Erasing a person now ends their workloads' current runs, as deleting the workload does ([#3771](https://github.com/cjohnstoniv/wardyn/issues/3771)).
- A queued service's start records the advisory as the started run stands, on the `run.queue.start` audit row. The run's
  visible advisory can still be the one from when it was queued ([#3764](https://github.com/cjohnstoniv/wardyn/issues/3764)).
- An invalid Workload resource now gets a status saying so, and the Kubernetes status push sends the run's current health,
  not the stored one ([#3779](https://github.com/cjohnstoniv/wardyn/issues/3779), [#3770](https://github.com/cjohnstoniv/wardyn/issues/3770)).
- Walls refusals and the Why card name the run's owner by display name, a saved Runs view keeps its name instead of reading
  Custom, and a Service panel a person removed stays removed after a reload
  ([#3785](https://github.com/cjohnstoniv/wardyn/issues/3785), [#3783](https://github.com/cjohnstoniv/wardyn/issues/3783), [#3777](https://github.com/cjohnstoniv/wardyn/issues/3777)).
- The Templates list has its own Owner column, with Includes under the name ([#3817](https://github.com/cjohnstoniv/wardyn/issues/3817)).
- The docs wrongly called the released macOS runner package signed. It is unsigned until the Apple Developer ID secrets
  are added, and the install steps now tell you to check it against the signed `SHA256SUMS`.
- The one-line install pins `claude-code` to the published `agent-base`, which ships no coding agent. TRY-IT and the README
  now say so, and what a real Claude Code run or the Claude sign-in needs.
- TRY-IT's single-sign-on step now also turns local mode off, as `wardynd` refuses to start with local mode and an issuer
  together.
- The workloads page now says that `wardyn diff` and `wardyn apply --dry-run` are refused for a service identity's token,
  that a queued replacement can end the old run first and that an unchanged apply launches again after a refused one
  ([#3795](https://github.com/cjohnstoniv/wardyn/issues/3795)). It also says an idle binding shows the run's phase until a launch is owed, `NotBound` means no pool binds
  the namespace, and the CRD is applied before the chart upgrade.
- The docs said an inbound token's upgraded connection is re-checked every 30 seconds. It is every 15, and a revoked
  inbound token can still call a UI-sandbox port for up to 30 seconds.

### Security

- A change to a policy, a governance profile or a permission grant now reaches a live inbound caller at once on the
  replica that served the write, and on other replicas within the existing 30 seconds. A successful write under
  `/policies`, `/governance` or `/permissions`, or a revive that changes a run's restrictions, ends the session an
  inbound token holds on that replica, instead of letting it run out its 30 seconds
  ([#3768](https://github.com/cjohnstoniv/wardyn/issues/3768)).
- The console's development toolchain now uses minimatch 11.0.1 (GHSA-g3hv-75wg-m249). Nothing shipped changes.

### Known issues

- macOS VM fixes in this release candidate are built and unit-tested and have not been run on a real Mac unless a line
  says so. Do not rely on a macOS VM run's view-only desktop as a security boundary until a run on a real Mac has proven
  it ([#3922](https://github.com/cjohnstoniv/wardyn/issues/3922)).
- On a macOS 26 base, a macOS VM run's desktop is view only: a person can watch it but not act in it
  ([#3901](https://github.com/cjohnstoniv/wardyn/issues/3901)).
- A macOS VM run's desktop shows the lock screen ([#3890](https://github.com/cjohnstoniv/wardyn/issues/3890)).
- On a Mac runner using Docker Desktop, a macOS VM slot can stay busy for a few minutes after a run if Docker Desktop
  started during that run. It frees when Docker Desktop goes idle; quitting Docker Desktop with no runs going frees it at
  once ([#3871](https://github.com/cjohnstoniv/wardyn/issues/3871)).
- Wardyn's macOS runner package is not yet signed with a Developer ID.

## [0.10.0-rc.9] — 2026-11-01

### Added

- An installed runner can now be given the folders drives and workspace folders may use, with
  `wardyn-runnerd setup --drive-root <dir>` (repeatable). Setup writes them to `runnerd.json` as `drive_root`, so
  the service needs no change; a rerun with `--drive-root` replaces the list and one without keeps it. A managed Mac
  can set them with the `DriveRoot` profile key, and `WARDYN_DRIVE_ROOT` also sets them; a `--drive-root` on `serve`
  wins over all three. `wardyn-runnerd doctor` prints the roots in effect and where they came from. See
  [Drive roots](docs/operations/hybrid-laptops.md#drive-roots-on-a-laptop-runner).
- `wardyn-runnerd macos-vm edit-base` boots a copy of a Mac's macOS base for one attended session, then checks it
  before runs use it: desktop control (a base that had it must keep it), accounts, Remote Login and the launcher's
  self-test. A copy that fails is discarded and runs keep the old base. The runner reports when Standard was last
  edited, and each run records that time, whichever base it uses. A base now keeps its setup account's password on the
  Mac, readable by the Mac's user only, because macOS will not let root change it without the old one. A base prepared
  before this release cannot be edited until `wardyn-runnerd macos-vm prepare --new-image`
  ([#3881](https://github.com/cjohnstoniv/wardyn/issues/3881)). See
  [Editing a base](docs/operations/macos-vms.md#editing-a-base). Built and unit-tested; not yet run on a real Mac.
- A Mac can hold more than one macOS base, each derived from Standard with its own install list, and a run picks one
  with `macos_base`. A derived base's list may not carry a configuration profile; profiles belong to Standard, which
  every other base is derived from. A governance profile's `limits.allowed_macos_bases` limits which a team may pick.
  See [Several bases](docs/operations/macos-vms.md#several-bases). Built and unit-tested; not yet run on a real Mac.
- The console shows a disk drive's Mac in New Run and in a new list of people's drives on the admin Drives screen, with
  delete, and says how a disk drive's delete ended. A Mac's runner drawer now shows its macOS base's guest posture, age
  and last edit, and marks a base past the organisation's maximum age as not offered, as does its card in the runner
  list. An admin sets that maximum in Settings, under macOS virtual machines, as the oldest macOS base in days; empty is
  no maximum.
- New Run offers a macOS base picker when the pool's Macs hold more than one base, and says why a base can't be
  picked; Review names the chosen base. A Mac's runner drawer lists its bases, and a governance profile's macOS bases
  limit has its own label and hint.
- macOS VMs: a run's workload now starts in the guest. Each base carries the standard launcher (task, command and
  self-test modes). A run that asks for several agents, held tool approvals or a service is refused by the launcher
  with a reason. Built and unit-tested; not yet run on a real Mac.
- Preparing a macOS VM base now ends with the launcher's self-test as the agent user. A harness that fails it is
  withheld, and a base that cannot start a run is not published. Built and unit-tested; not yet run on a real Mac.
- macOS VMs: the guest agent trusts the run's proxy certificate in the run's System keychain, for TLS only, before the
  run starts, so tools that use only the keychain trust it too. If macOS refuses, the run goes on and its audit says
  so. Built and unit-tested; not yet run on a real Mac.
- macOS VMs: the runner setting `macos_vm_base_file` names a list of packages, apps and files, each pinned by its
  SHA-256, that prepare and every tools update install into the base as admin. A changed list starts a tools update.
  Built and unit-tested; not yet run on a real Mac.
- macOS VMs: configuration profiles in the install list are staged in the base, approved once in `edit-base`, and read
  back as installed. Built and unit-tested; not yet run on a real Mac.
- macOS VMs: an organisation CA in the install list is trusted in the base for SSL only, then verified. A CA macOS
  will not trust without a person reads `not_trusted`; trust it by hand in `edit-base`. On macOS 26 expect Apple to
  ask a person to approve the trust: the CA then reads `not_trusted` and preparing goes on
  ([#3878](https://github.com/cjohnstoniv/wardyn/issues/3878)). On macOS 26, expect removing or rotating a CA to stop
  the tools update; to drop or replace a CA, change the list and run `wardyn-runnerd macos-vm prepare --new-image`.
  Built and unit-tested; not yet run on a real Mac.
- macOS VMs: a package in the install list may take secret files, such as an EDR licence. A file's value is never in a
  command line, a log, `tools.json` or the caps, and the file is removed after the install. Built and unit-tested; not
  yet run on a real Mac.
- macOS VMs: the runner reports the guest's posture (macOS version, SIP, Gatekeeper, profiles, MDM enrolment, the
  organisation CA) beside the Mac's own. Built and unit-tested; not yet run on a real Mac.
- A Mac's MDM can now attest posture (`MDMManaged`, `DiskEncrypted`) and set `register` and `setup` limits (`MaxRuns`,
  `TotalCPU`, `TotalMemoryGB`, `PerRunCPU`, `PerRunMemoryGB`) from the managed preferences profile, with no posture
  file to push. Posture is read from the device-wide profile only; a user-scoped profile does not attest it.
  `macos-vm prepare` sizes its VM from the per-run keys when the organisation has sent no limit. Built and
  unit-tested; not yet run on a real Mac.
- A Mac now reports when its macOS base was prepared and its build. `macos-vm status` shows the base's age and this
  Mac's macOS, and a new site-config setting, `macos_vm.max_base_age_days`, refuses macOS VM runs on a Mac whose base
  is older (or whose runner reports no age). Built and unit-tested; not yet run on a real Mac.
- `wardyn-runnerd macos-vm prepare --ipsw-url <https URL> --sha256 <hex>` builds the macOS base from an image on your
  own mirror, which must answer HTTP Range requests, and never falls back to Apple. `MacOSVMImageURL` and
  `MacOSVMImageHash` in the managed profile (or `macos_vm_image_url` and `macos_vm_image_sha256` in `runnerd.json`;
  the profile wins) set it for setup too. With `--ipsw-url`, the hash is `--sha256` alone. Built and unit-tested; not
  yet run on a real Mac.

### Changed

- The workloads guide now explains that a workload's policy is laid onto the deployment default, and how to launch on a local runner.
- Denying any Azure DevOps host now withdraws the person's Azure DevOps credential lane: its credentials and the hosts
  it added; hosts your policy allows itself stay reachable without the credential. This holds at dispatch and on a
  revive. A deny on `dev.azure.com` alone used to leave the person's credential on `<org>.visualstudio.com` and the
  `vssps`, `vsrm`, `feeds`, `pkgs` and `almsearch` hosts. `run.ceiling.reassert` and `run.revive` name the lane
  `azure_devops` in `dropped_broker_lanes`.
- A macOS VM that gets less than the base image's minimum size now says what cut it: your own request, the governance
  profile's ceiling or `limits.max_*`, the pool, or the server. It names the setting that raises it. Built and
  unit-tested; not yet run on a real Mac.
- A macOS VM run is not recorded in this release, and says so at Review and when it is created. Where the
  organisation, the pool, a locked organisation level or the run's own ceiling asks for recording, it is refused at
  create and Review instead, and says which of them asked. Built and unit-tested; not yet run on a real Mac.
- A macOS VM run now denies Apple's background boot-time hosts without an approval prompt. The list is deny-only: a
  host your policy allows stays reachable, and nothing on it widens egress. See
  [Apple's boot-time hosts](docs/operations/macos-vms.md#apples-boot-time-hosts). Built and unit-tested; not yet run
  on a real Mac.
- A macOS VM run that mounts a Docker or cluster volume drive, or a drive of no known place, is refused at create and
  Review, before a Mac is asked to start the VM. So is a drive its Mac's runner cannot mount. Built and unit-tested;
  not yet run on a real Mac.
- A macOS VM run can mount an admin or team drive that is a folder on the Mac. It is shared into the guest at the
  container's names under `/Users/agent`, read-only as granted, and a runner that predates this refuses it at create.
  See [Drives](docs/operations/macos-vms.md#drives). Built and unit-tested; not yet run on a real Mac.
- A macOS VM run can mount a person's own disk drive, a disk image kept on one of their Macs (`POST /me/drives` with
  `runner_id`), and keep its work folder as one. Only macOS VM runs on that Mac can use it, and deleting the drive
  deletes the image there, at once or when the Mac reconnects. See [Drives](docs/operations/macos-vms.md#drives).
- A disk drive is in one macOS VM run at a time: a second run on the Mac that asks for it while another holds it is
  refused when that run starts, not at Review. Built and unit-tested; not yet run on a real Mac.
- A disk drive is found in the guest by its size, apart from the boot disk. A macOS VM run attaches one disk drive, and
  a kept work folder counts as one: a person's disk drives all have one size, so a second is refused when the run
  starts.
- A disk drive is formatted Mac OS Extended (journaled), not APFS: an APFS volume made in one macOS VM run was not
  mounted by the next run's VM, so the drive kept nothing between runs.
- A macOS VM run is also refused at create and Review when a second harness has its own starting prompt. It is refused
  too when a tool rule, tool approvals, autonomy level L1 or the policy's inner walls would hold a tool call for
  approval. The launcher still refuses the same shapes as a backstop. Built and unit-tested; not yet run on a real Mac.
- With `WARDYN_AWS_SIGV4_AT_PROXY` on, a run on a person's own runner now re-signs its AWS lane at that runner's
  proxy, instead of being refused. Setup warns of each connected runner whose `wardyn-runnerd` cannot re-sign. This is
  not yet verified end to end on a person's runner.
- Docs: the macOS VM page compares a guest's controls with a managed Mac's. It says a guest is an unmanaged device to
  compliant-device sign-ins, and that the Mac's MDM device certificate is not checked in this release. The
  high-availability page covers running one replica.

### Fixed

- `wardyn drive set` now keeps a drive's second place (`alt_backend`, `alt_host_root`, `alt_runner_id`). Before, it
  dropped it, and re-applying a document cleared one set in the console
  ([#3894](https://github.com/cjohnstoniv/wardyn/issues/3894)).
- Re-running `wardyn-runnerd setup` on a Mac whose runner is running no longer fails with `launchctl bootstrap` exit 5
  and leaves the runner stopped: setup waits for the old LaunchAgent to unload, and on a failure prints the command
  that starts it ([#3892](https://github.com/cjohnstoniv/wardyn/issues/3892)).
- The macOS VM setup window, and the edit-base window, show all three of their steps under the title. Before, only
  the first step showed.
- A process inside a macOS VM could pose as Wardyn's guest agent.
- A run in a macOS VM no longer hangs on its first read of a shared folder. macOS asks a privacy question for
  each network volume, and nobody can answer it inside a run, so the guest agent needs Full Disk Access in
  the base: approve `/usr/local/libexec/wardyn-guest-agent` by hand in `edit-base`. Without it, a run with a
  shared folder now stops before it starts, within 10 seconds, and names that fix
  ([#3889](https://github.com/cjohnstoniv/wardyn/issues/3889)). See
  [Folders](docs/operations/macos-vms.md#folders). Built and unit-tested; not yet run on a real Mac.
- Installing a macOS base's file items no longer follows a symbolic link, or passes through a folder others can write
  to, on the way to an item's destination.
- The error body of a `capability_feature` refusal now carries `feature`, the feature it names (such as `inbound` or
  `custom_component`), and the New Run page reads it to tell an inbound-app refusal from the other feature refusals.
  It used to match the wording of the message, so rewording the message would have turned the notice off (#3769).
- Retrying an AWS model sign-in no longer shows the new sandbox as waiting for AWS before it has shown a code. While
  the retry launched, the old sandbox, still stopping, could put its device code on screen and move the new run's
  progress to the waiting step (#3407).
- Setup's Egress redirection step no longer records a redirect test for a target that is no longer configured. A test
  still running when its redirect was saved with a new To used to land as the redirect's verdict and could open the
  corporate-network gate; it is now dropped, and a test of an unchanged redirect still counts (#3517).
- When a person's Azure DevOps sign-in lacks the permissions Wardyn needs to create tokens, Settings and Getting started
  now agree: both say the administrator needs to finish setting up Azure DevOps, and both keep Connect, so the person
  can sign in again once the permissions are granted. Settings offered no Connect, and Getting started left out the
  administrator step (#3520).
- Settings → Signed-in CLIs now says a file-stored sign-in ends after 7 days at most. It used to say 7 days for every
  such session, though a shorter configured session lifetime ends it sooner; the Ends column shows the real expiry
  (#3402, #3813).
- Saving Settings → Runner credentials no longer fails with a 422 when the stored policy holds an older row with an
  empty posture requirement, such as Git tokens stored on the runner with `require_posture: {}`. Rows you did not
  edit now go back exactly as stored (#3502, #3816).
- On the Runs page, the Agent tasks, Environments and Apps & scripts segments now say how to start one when you have
  none, as Services does. They used to show "No runs match these filters" (#3782).
- On the Approvals page, a decision the server refuses because the run owner's own walls deny the host now turns that
  request's Approve off and shows whose walls block it, as a request the walls already block does. It used to leave
  Approve live with no reason (#3781).
- The Wardyn boundary page tells you to reload when the person signed in changed while a draft was open (#3406).
- A terminal client promoted to writer while its attach was still opening is now shown as the writer. Its opening frame
  could land after the promotion's and leave its terminal read-only until it reconnected (#3403).
- An SSH sign-in refused because its run could not be read now names the key's owner in its `ssh.authenticate` row,
  with reason `run lookup failed` for a store fault and `unknown run` for a missing run. It used to name `unknown` and
  say `unknown run` for both (#3508).
- A UI relay connection now closes its sandbox side before it writes its `ui.close` row, and writes the row off the
  closing path. A stuck audit write held every closing relay connection open.
- A run's second UI app now opens while the first app's idle relay connections fill the run's 8-connection cap: at the
  cap the relay closes idle pooled connections and tries again. It used to answer a `503` at once, for up to 90
  seconds (#3507).
- A macOS VM's plain-HTTP relay refuses CONNECT and drops the relay's own flow and stage headers a guest sends, so a
  guest can no longer raise approval asks under a flow it did not open. A desktop VM's connection to its own proxy,
  the one its `HTTPS_PROXY` names, now reaches the proxy unchanged, so tools that honour the variable, and the run's
  repo clones, work ([#3884](https://github.com/cjohnstoniv/wardyn/issues/3884)). The proxy decides what is sent on it
  as for a container; the relay's own requests use a proxy port of their own, which the guest cannot open. Built and
  unit-tested; not yet run on a real Mac.
- An open UI app, desktop or terminal session now ends at its next re-check once the run's governance profile is
  edited to refuse a new one: `deny_ui_apps` ends an app or desktop session, and `deny_interactive` a desktop or a
  terminal. They used to stay open until they closed or reached their time limit (#3398).
- A browser session, or a recently checked inbound token, calling an inbound app on a run that has ended or been kept
  is now refused with a `not_running` audit row and no `inbound.allow` row. It used to be recorded as allowed before
  the refusal (#3775).
- Page loads of an inbound app from a person's browser session, the run owner's included, no longer count against
  the per-caller inbound rate limit that inbound tokens use. They still count against the run's own limit (#3792).
- An egress approval-pending answer now carries `X-Wardyn-Egress-Reason` on every path: a CONNECT, a plain request, an
  organisation-forwarded request and the sign-in listener. An approval that expired or was cancelled answers
  `approval:expired`, a full host table `approval:host-cap` and a raise the control plane refused `approval:refused`,
  so a sandbox can tell them from `approval:pending`, an approval a person has yet to decide (#3815).
- While the control plane cannot be reached, a minted credential's last-good header now stops at expiry plus the
  15-minute grace. A failed re-check just before that deadline could keep serving it for up to 30 seconds past it,
  the pause between re-checks (#3416).
- A model gateway host both vendors share that is also the run's own model endpoint is now inspected as that endpoint's
  vendor. Under fail-closed inspection its request bodies were refused as uninspected (#3819).
- A run's container `CONNECT` tunnels are now capped at 1,024 open at once, the bound desktop VM tunnels already have.
  The next one is refused with a `503`, `Retry-After`, `X-Wardyn-Host` and `X-Wardyn-Egress-Reason:
  builtin:tunnel-limit`, and a `builtin:tunnel-limit` deny row; it is admitted once a tunnel closes. Before, a process
  could hold any number of tunnels open (#3698). The person's sign-in tunnels count too. A desktop VM's tunnels through
  its network relay count only there, so a VM at its own bound still leaves room for the person to sign in; tunnels
  the guest opens through its `HTTPS_PROXY` count at the proxy too, as a container's do.
- With `WARDYN_FILE_SECRET_PLACEHOLDERS` on, a file named by two `file_secret` grants now holds the placeholder of the
  grant that delivered it. A later grant's placeholder replaced it, so requests to the delivering grant's host lost
  their credential.
- Promoting a recording's hosts into a workspace's egress no longer promotes a model provider's host when the site
  configuration can't be read. The promote now writes nothing and answers `503`, to retry; the provider hosts are read
  once, for both the filter and the hosts a request names.
- Two value-equal approval requests for one run that spell a number differently (`1` and `1.0`) no longer both open a
  pending approval when they race: a request's scope is stored with one spelling per number. A pending request no
  longer stands in for another whose numbers differ past floating-point precision.
- Restarting a `WardynWorkload` custom resource's workload now needs the namespace binding that holds now, as the
  runner's next report of the resource would. A restart after the binding was deleted could still launch the run on
  the deleted binding's pool; it is now refused `workload_binding_inactive`, and the refusal is recorded on the
  workload. The launch uses the binding's own sponsor and standing (#3758).
- A `WardynWorkload` custom resource's launch whose claim lapsed (the replica that took it went away, or the launch
  failed before it recorded a run or a refusal) is now retried by the daemon's 30-second workload sweep, instead of
  waiting up to 10 minutes for the runner's next full report. The retry launches as the bound service under the
  namespace binding that holds now; at most one retry per workload per claim (60 seconds), and a refused launch still
  waits for the resource's next report (#3772).
- A runner's full list of a bound namespace now reads only that namespace's workloads on the binding's pool, in the
  database, instead of every custom-resource workload in the organization on every list (#3773).
- A dropped base image and an unbuilt SSH devcontainer are now recorded in the audit log as a skipped part of the
  build, with the reason. A run that later fails for its own reason is no longer shown as an image failure.
- A secret-store fault while proving that a person holds their own credential, for a run on their own runner, now
  refuses with the retryable 503 `store_unavailable` instead of reading the credential as the organisation's. At
  dispatch the run fails as unreadable rather than refused. The store is read once per person per request, fault or
  not. The same fault on any other check of a person's own secret that admits a run (an inline policy, an override,
  a component, a model provider key) is the same 503, never "unknown secret", a dropped grant or a 500. A
  component's save stays advisory and still lists that secret as missing.
- A run's end no longer loses its settlement when the sandbox stop overruns: the teardown-error audit, the output
  finish, the workspace and record settlement and the cleared sandbox reference each get a budget of their own, so a
  workspace scan is no longer left in `scanning`. A replica that loses the run's lock during that tail now stops it,
  instead of running it beside the replica that took the lock and writing duplicate audit rows.
- `run.ending_soon` no longer promises kept files (`files_kept_sec`) for a run its end tears down at once: one with no
  sandbox recorded, on a runner that cannot keep a sandbox, or with no ended-run grace. Behind the router it reads the
  run's own runner or executor, and leaves `files_kept_sec` out when no fact says whether the end will keep the run.
- A run Wardyn launches itself (a workspace scan or recording, a harness sign-in, a probe) no longer records its
  placement as `remote` on a deployment where runner pools do not place runs: it is left empty there, as every other
  run's is. The 0.9.1 note that `run.create` records the placement of every run holds only where pools place runs.
- On Kubernetes, a run that keeps its work folder beside a shared drive is refused up front, at the preview,
  preflight, create and queue replay, with `drive_share_beside_managed`. It was accepted and then refused at dispatch,
  since the kept work folder is a managed drive the run adds only then.
- Erasing a person no longer clears the author of a launch preset written by a different subject whose name only
  matches after the database's lower-casing, such as a Kelvin sign in place of a K or a non-ASCII case variant. Only
  an ASCII subject is matched case-insensitively, and only on its ASCII letters.
- With second-person approval on, a held unit create or update now shows the approver the unit's mode, own walls or
  pass-through, in its before and after.
- Offboarding no longer stops handing a person's workspaces to the operator when the renamed fallback for a name
  collision is itself taken. The fallback now takes a counter until neither the person nor the operator holds the
  name, and one workspace that cannot be handed over no longer stops the others.
- `wardyn-runnerd setup` now registers through the proxy its connection check used. The check went through the
  settings setup installs for the service, but the registration followed this shell's environment, so a rerun from a
  shell with no proxy could pass the check and then dial the organisation direct.
- A Docker runner whose relay status failed to reach the organisation now sends it again at the next relay check. A
  failed rebuild or send of the runner's capabilities was retried only on a reconnect or a further change of state.
- A folders runner that resumes its session now sends the failure it recorded while the connection was down. Only a
  fresh session sent it, so a stable resumed runner stayed connected without the organisation seeing it.
- A `wardyn-runnerd serve` start that fails on a bad flag now records that error as the last error in the runner's
  `status.json`. The file was rewritten as starting then stopped, which dropped the previous process's last error and
  recorded nothing in its place.
- `wardyn-runnerd doctor` no longer pulls the proxy image if it is removed from the engine between its image check and
  its relay check. The relay check now fails on the missing image instead, as the page says doctor never pulls.
- A Kubernetes run whose pinned proxy image is older than the runner now fails with the cause named: the proxy image is
  older than this runner and refused a setting at config load, so have the organisation name this release's proxy
  image, or run setup with `--proxy-image` set to it. It showed only a generic sidecar failure.
- A Kubernetes runner no longer stops a run's proxy pod created while it lists its namespaces: the list read the pod
  as gone, and the run failed with the proxy's config stage "exited" (exit code 137, `ContainerStatusUnknown`) (#3823).
- New Run Review no longer calls a local MCP server's secret a Kubernetes service account when it shares an
  organisation row with a cluster, and the Tools step's added local server card no longer shows another component's
  command or secret name while a later preview is out (#3409).
- Check again on New Run Review now reads the web app gateway and the drive catalogue again, so a gateway turned on or a
  drive deleted since the page opened clears its blocker (#3410, #3420).
- One "Your walls leave out" line on New Run Review no longer hides the walls' warning for the other host list (#3797).
  The effective-policy card's Network row and the Access group's host count no longer count the hosts your walls take out (#3780).
- New Run checks a service health path's characters and length before launch, as the server does (#3776), and the Tools
  step's harness fields fit a 320 px viewport (#3818).
- A credential refusal from Review's preflight refreshes the model-access banner at once instead of at its next poll.
- A service run's Service panel now takes the head of the cockpit's right column as the saved layout has it, not always the
  column starting at 8, and moves down only the widgets under it (#3784).
- On a run's own Approvals tab, an approval the run owner's walls refuse now closes the way it does on the Approvals page:
  the walls card, Approve off and Deny on. It stayed live with no reason (#3781).
- The lease sweep no longer re-reads a healthy service's limits, sponsor and policy on every pass. It reads the
  owner's authority to extend only once a `run.ending_soon` warning would be due, and the renewal re-checks it as
  before (#3762).
- An image whose own `ENV` sets `WARDYN_SERVICE=1` no longer turns a plain command run into a service that restarts
  in a loop. Dispatch now sets `WARDYN_SERVICE=0` on every run that is no service, and the sandbox's env wins over the
  image's (#3763).
- A workload launch refused `placement_credential` for one of its policy's grants now says that the grant may come
  from the default policy, which a workload's `inline_policy` is laid onto, and that `eligible_grants` (`[]` for none)
  replaces it. The console's last refusal says so in plain words and, for a reader who may see it, adds the server's
  sentence naming the grant.
- With several Kubernetes runners serving one bound namespace, one runner's late full list no longer deletes a workload
  another runner applied since, and a late report of a deleted resource's uid no longer overwrites the recreated
  resource's workload. A prune, or a report of another uid, now counts only from the runner that last applied the
  workload, or from another once that runner has left the pool; a report from a runner session a newer one replaced
  writes nothing. A short-lived duplicate run can last until the reporting runner's delete arrives.
- A runner that was refused events, then went quiet or was revoked, no longer leaves its refusal bookkeeping in the
  organisation's memory for good. It is dropped a day after the window opened, and a summary of rows suppressed after
  the bound is lost if the runner stays silent that long (#3663).
- A run ended `gone` (its runner reported the sandbox gone with no exit code) now shows a failure hint saying so: the
  container, pod or VM was removed, or its host restarted, including when the lease sweep finishes the end. It was
  FAILED with no reason on its page (#3669).
- A runner that resumes its session and keeps dying within seconds of READY with an event unacknowledged, five times in
  a row, now starts a fresh session and logs a warning, instead of resuming for good against an organisation that
  refuses that event. An agent exit or spool record is still replayed and keeps failing until the organisation is
  upgraded (#3660).
- `wardyn secret set` and `wardyn-aws-sso` no longer take standard input on `/dev/null` for a terminal. An unattended
  run with input from `/dev/null` now reads it as piped input, or refuses, instead of offering the interactive prompt
  (#3106).
- `wardyn run --editor` no longer counts an `Include` under a `Host` or `Match` block in `~/.ssh/config` as the one it
  needs. Only an `Include` above the first of those lines counts; otherwise it asks before adding its own at the top
  (#3415).
- `wardyn runner list --no-limits` now filters every page of runners, not only the one it fetched, so a runner with
  no limits set past the first page is listed. `--limit` and `--offset` then cut the filtered list (#3811).
- `wardyn runner install` on a computer that already runs a runner now says it is upgrading and ignores `--pool`,
  `--default` and `--name`, instead of dropping them silently. `--artifact-base` is saved in `cli.json` only after the
  release or runner downloaded from it has been verified, so a mirror that served something refused is no longer
  remembered (#3551).
- `wardyn doctor`'s `runner-exits` stage, when the organisation will not show the runner, now says a revoked runner is
  not shown to its owner and points at the administrator's `runner.exit.park` audit rows. `docs/runner-offline.md`
  no longer says doctor reports a revoked runner's parked receipts (#3614).
- The proxy's decision log now masks the AWS access key id of the role credentials it re-signs with, as it already
  masked their secret key and session token.
- macOS VMs: a harness started in a macOS VM's terminal did not get the policy's managed settings, such as its
  autonomy ceiling, tool rules and server list, or the cloud CLI's managed configuration. They are now written where
  macOS reads them. A managed file with no macOS location stops the run's start with a reason. A run environment value
  that names a Linux path with no place in the guest stops the start the same way. Built and unit-tested; not yet run
  on a real Mac.
- Preparing a macOS VM base no longer logs the guest's tools connection as `tools_dial: transport failure` while it is
  still opening. Stage 2 reads the dial after its first tools request, and logs one still unclassified as `pending`.
  Built and unit-tested; not yet run on a real Mac.
- Preparing a macOS base no longer fails at stage 1 when Screen Sharing was already turned on during setup by hand:
  stage 1 checks that the service is loaded and skips loading it again. A macOS base prepared with Screen Sharing on
  view-only now says in `macos-vm status` how to get control. Built and unit-tested; not yet run on a real Mac.
- During base preparation the guest's DNS lookups are refused instead of answered, so Setup Assistant's Screen Time
  screen no longer waits minutes for a network that is not there. Run-time DNS is unchanged. Built and unit-tested;
  not yet run on a real Mac.
- The macOS setup window comes to the front, shows the three steps to follow, and `macos-vm status` no longer warns,
  wrongly, that the base's guest agent is not this runner's. Built and unit-tested; not yet run on a real Mac.
- Review's preflight now renews an expired AWS sign-in whose refresh token still lives, as launch does, so Review and
  launch give the same answer. The renewal's `harness.credential.refresh` row carries `trigger: preflight`.
- `scripts/ci-run.sh` now kills a run cancelled before its waiting line prints, reading the run id from `run.json`
  first, and its run-id read works with BSD sed (#3519).
- The `git_credential` workspace probe no longer passes when `docker exec` itself fails or the sandbox is not running;
  a refused exec ends it in ERROR (#3521).
- `scripts/verify-release.sh` no longer expects the Windows CLI binaries from a release older than 0.10.0, where they
  first shipped (#3404).
- On a run's Agents panel, an agent whose state has not arrived yet is now shown without a status badge, not as
  Running.
- A member's workspace folder that exists only on their own Mac or Linux computer can now be used there: a `local_dir`
  source names one of the person's runners (`runner_id`, or `wardyn workspace create --runner`). Wardyn checks the
  path's form when the workspace is saved and when a run starts, and places the run only on that runner, which checks
  that the folder exists, resolves links, and holds it to the member's workspace roots, its own `--drive-root`, the
  credential-path list and the writable roots before it binds or shares it. When that runner is later revoked or
  removed, or the workspace passes to someone else, a run with the folder is refused by name
  (`workspace_source_runner_unavailable`). Previously such a folder was refused when the workspace was saved, and no
  workspace folder could run on a person's own runner (`placement_local_path`).

- On a Mac set up with `wardyn-runnerd setup`, the runner now has its macOS VM helper and guest agent beside it, so
  it offers macOS VMs and can prepare a base. Before, it served with macOS VMs off (`no_helper`), and a prepare failed
  at its second stage after the setup by hand. `prepare` now refuses at once when the serving runner has no helper.

### Security

- `WARDYN_USER_WRITABLE_DENY` now matches a member's writable folder in any letter case, so an entry naming `.ssh`
  also refuses a path that spells it `.SSH`, as a case-insensitive disk resolves both to one folder.
- An upgraded connection an inbound token opened, a WebSocket say, is now re-checked every 15 seconds and closed at
  most 45 seconds after the last check that confirmed its token (30 seconds unconfirmed, plus one re-check), even
  while a check is still waiting on a store that does not answer. A hung check used to hold the connection open for good, and a store that kept
  failing kept it open about a minute. The refusal's audit row is now written after the close, so a slow audit write
  no longer delays it.

### Known issues

- On a Mac runner using Docker Desktop, a macOS VM slot can stay busy for a few minutes after a run if Docker Desktop
  started during that run. It frees when Docker Desktop goes idle; quitting Docker Desktop with no runs going frees it at
  once ([#3871](https://github.com/cjohnstoniv/wardyn/issues/3871)).
- If the macOS VM setup is given the wrong one-time code, preparing stops after about two minutes with `handshake failed:
  EOF` instead of saying the code was wrong. Each prepare makes a new code: use the one from that prepare's window
  ([#3874](https://github.com/cjohnstoniv/wardyn/issues/3874)).
- A macOS VM run's desktop shows the lock screen. Stage 1 turns the screen saver off for the base's machine identity,
  and each run starts with a new one, so the setting does not reach the run
  ([#3890](https://github.com/cjohnstoniv/wardyn/issues/3890)).
- On a macOS 26 base, a macOS VM run's desktop is view only: a person can watch it but not act in it
  ([#3901](https://github.com/cjohnstoniv/wardyn/issues/3901)).
- A macOS VM run refused for its size when the cap comes from the Default governance unit says only the minimum it
  needs, not which limit applied. Assign the person a governance profile that allows at least the CPUs and memory the
  refusal names ([#3885](https://github.com/cjohnstoniv/wardyn/issues/3885)).
- Each macOS VM run writes a second `run.complete` audit row with outcome `failure` and a teardown deadline error,
  about 30 seconds after the real one. The run's state and exit code are right
  ([#3886](https://github.com/cjohnstoniv/wardyn/issues/3886)).
- A macOS VM run attaches one disk drive; a second one, or a drive plus a kept work folder, is refused when the run
  starts, not at create; the refused run still creates its kept work folder drive, which can be deleted
  ([#3897](https://github.com/cjohnstoniv/wardyn/issues/3897),
  [#3900](https://github.com/cjohnstoniv/wardyn/issues/3900)).
- Wardyn's macOS runner package is not yet signed with a Developer ID, so each upgrade removes the guest agent's Full
  Disk Access: grant it again in `edit-base` before runs that share folders.

### Before you upgrade

- Macs already set up with `wardyn-runnerd setup` must run setup again after installing this package, so the runner
  gets its macOS VM helper beside it. `wardyn runner upgrade` alone does not do this.
- macOS bases prepared on 0.10.0-rc.8 or an earlier release candidate must be prepared again: run
  `wardyn-runnerd macos-vm prepare` on each Mac. Until then the Mac's runner reports its base as needing an update
  and offers no macOS VMs.
- Migration `0246_user_drives_mac_disk` admits `mac_disk` in `user_drives.backend`'s CHECK and in the member-drive
  rule, and adds `user_drives.home_runner_id`, the Mac a disk drive is kept on. It changes no row and adds no grant.
  A downgrade with a disk drive present is unsupported.
- Migration `0247_sources_runner_id` adds `sources.runner_id`, the runner a library folder is bound to, and makes it
  part of a library source's identity. It changes no row and adds no grant. A downgrade with a bound folder present is
  unsupported.
- Runner owners: a workspace folder bound to your runner mounts only from inside its `--drive-root`, and only on a
  runner updated to this release, which says `bound_folders` in its capabilities. A run with such a folder is refused
  on an older runner (`placement_local_path`) until it is updated.

## [0.10.0-rc.8] — 2026-10-29

### Before you upgrade

- Split-role installs (`WARDYN_PG_MIGRATE_DSN` set) whose app role does not pick up new tables through the `ALTER
  DEFAULT PRIVILEGES` step in Operations: grant the app role `SELECT, INSERT, UPDATE, DELETE` on `workloads` and
  `workload_bindings` (migration `0241_workloads`) and on `workload_run_ends` (migration `0244_workload_run_ends`).
  Without it applying a workload and binding a namespace fail. An
  upgrade from before 0.10.0-rc.3 also needs `SELECT, INSERT, UPDATE, DELETE` on `macos_vm_disks` (migration
  `0236_macos_vm_disks`), and every grant the 0.10.0-rc.1 notes list: `SELECT, INSERT, UPDATE, DELETE` on
  `aws_sso_refused_tokens`, `cli_sign_ins`, `ui_desktop_closed`, `desktop_signins`, `desktop_signin_shares`,
  `desktop_signin_runs`, `approval_rules`, `governance_restrictions`, `governance_restriction_members`,
  `governance_unit_admins`, `oidc_session_sids` and `desktop_vm_sign_ins`. Also grant it `SELECT, INSERT, UPDATE` on
  `runner_health`, `launch_preset_version_floor`, `cli_sessions` and `governance_boundary`. If it turns on
  `WARDYN_MULTI_AGENT_TASKS`, grant it `SELECT, INSERT, UPDATE` on `run_harnesses` as well.
- A service needs the runner tools of this release in its image. An image wrapped or built before it runs the command
  once, with no restart and no health check, and its health reads *Health unknown* after 10 minutes. Wrap or build the
  image again with this release. See [Services](docs/SERVICES.md#images).
- Migration `0239_inbound` restricts the `feature` value `inbound`, admits `inbound` in `api_tokens.kind`'s CHECK and
  adds `caller` and `app` to `attach_tickets`. It inserts the `inbound` restriction row when absent, rewrites no token or ticket, and adds no grant. See
  [OPERATIONS.md](docs/OPERATIONS.md).
- A change to a runner pool's use policy that was held for a second person before the upgrade, on a pool that already
  holds an organization runner, reads as stale when it is approved: the state it was proposed against now includes the
  pool's organization runners. Propose it again. A held change on a pool with no organization runner is unaffected.
- Migrations `0240_run_service`, `0242_runner_pool_member_namespace` and `0245_run_shape` add columns to `agent_runs`
  and `runner_pool_members`; they change no grant.
- On a Kubernetes chart upgrade, apply the `WardynWorkload` CRD again by hand:
  `deploy/helm/wardyn-runnerd/crds/wardyn.dev_wardynworkloads.yaml`. Helm installs a chart's `crds/` once and never
  upgrades it. A runner whose chart lists a namespace under `workloads.namespaces` refuses to start while that
  namespace has no CRD. See [Workloads](docs/operations/workloads.md).

### Added

- The console's left bar leads with Runs, Workloads and Approvals (and Audit and Recordings in the Admin view), with no
  heading; Getting started and Settings now sit under **Setup**. **Your account** is now **Settings**.
- The Runs page filters by what a run was set up as: **All**, **Agent tasks**, **Environments**, **Apps & scripts** or
  **Services**, each with its count. A run keeps the shape it was set up from (`shape` on the run, and on the
  `run.create` audit row when the request names one); a run from before this release, or one whose request fits
  several shapes, has none. New Run's **Environment sandbox** card is now **Environment**.
- **Your API tokens**, under Settings: a person mints and revokes their own API tokens in the console, choosing what
  the token is for under **Use**: **Wardyn API** or **Calling inbound ports**.
- A background command can be kept running as a service. On New Run, an Automation / script has **Keep it running**
  (`wardyn run --service`, or `service` on `POST /runs`): the command is restarted when it exits with an error, after a
  pause that grows to 5 minutes, and checked on a port inside the sandbox by a TCP connect or an HTTP GET. The run list
  and run page show *Starting*, *Healthy*, *Unhealthy* or *Health unknown* and the restart count. The supervisor runs
  inside the sandbox, so health is the sandbox's own report (`source: sandbox` on `run.service.health` and
  `run.service.restart`). A starting or healthy service has its lease renewed within its lifetime cap, as
  `run.end.set` with reason `service_renewed`, but only when a cap applies: the pool's `background.lifetime.max_sec` or
  `WARDYN_RUN_MAX_AGE`. With no cap, or when unhealthy, a service ends at its lease like any run, and New Run says so
  before launch. See [Services](docs/SERVICES.md).
- A port on a background run opens from the run page only when it is callable from outside; the run's owner is always
  one of its callers. See [Services](docs/SERVICES.md#start-a-service).
- Changing a service identity's sponsor stops lease renewal of the services it already runs: they end at their current
  lease end. Relaunch or apply them again under the new sponsor.
- Your walls: limits you set on every run you start, which only narrow what your organisation allows. Set them under
  **Settings**, with `wardyn walls` (`wardyn walls set --file walls.yaml`, `wardyn walls clear`), or through
  `GET`, `PUT` and `DELETE /api/v1/me/walls`. They name the sites a run may reach or never reach, what happens at a site
  not listed, a barrier floor, the clipboard and file directions, and credentials kept out. They are private to you and
  bind every run you start, a super admin's too. Your site list is not applied again at dispatch or revive, so the hosts
  a run's own setup adds stay reachable; your denies, blocked directions and withheld credentials are applied again, at dispatch, at
  revive and at the mint door. An egress approval for a host your walls deny is refused `personal_walls_host`. New reasons: `walls_invalid`,
  `walls_need_person`, `walls_unavailable` and `personal_walls_host`; new audit actions `person.walls.set` and
  `person.walls.delete`. See [Your walls](docs/POLICIES.md#your-walls).
- `overrides.egress.allow_all_egress: false` is accepted on a run request, on any base policy, and turns open egress off
  for that run; it reaches the base's listed hosts, the hosts you add and the hosts its own setup needs. New Run step 5
  offers it as **Only the sites listed**. Before, `false` was refused.
- A run's `ui_apps` entry can now be marked `inbound`, with a `callers` list, so the run's owner and the people, groups and
  service identities it names can call that port through the app gateway on any run, a background run with no terminal
  included. Each admitted call is recorded as `inbound.allow`, and refusals as `inbound.deny` under the `auth.fail` rate bound. It is HTTP and WebSocket only, and it travels the
  same exec lane as any UI app, so docker, Kubernetes and remote runners need no change. Nobody but an operator may declare one until an
  administrator allows the new `inbound` value of the `feature` permission (a run that only inherits an administrator's
  inbound apps is not asked); a governance profile's `deny_ui_apps` or
  `deny_interactive` removes inbound apps. See [UI sandboxes](docs/UI-SANDBOXES.md#inbound-let-named-callers-reach-a-port).
- An **inbound token** (`{"kind":"inbound"}` on `POST /me/tokens`, or on a service identity's token mint) calls the inbound
  apps that name its holder as a caller, sent as `Authorization: Bearer wdn_…` on the gateway. It does nothing on the Wardyn
  API, and the gateway takes no other token. It must expire, in at most 90 days, and token lists mark it `"inbound": true`.
  See [API tokens](docs/operations/api-tokens.md#inbound-tokens).
- `POST /runs/{id}/apps/{app}/ticket` mints the browser ticket that opens one inbound app for the run's owner or a person
  its `callers` names. A named person who is not the owner needs per-run hosts (`WARDYN_UI_SANDBOX_ORIGIN_TEMPLATE`);
  on one shared host it answers `403` `inbound_needs_host_mode`. `/healthz` gains `ui_sandbox.inbound_url_template`.
- A UI-app call to a run whose runner is offline now answers `503` `runner_offline` on the gateway instead of `502`.
- An inbound app's run page has **Copy link for people** and **Copy address for API clients**. The link is
  `/open/{run}/{app}`: it mints the caller's ticket and opens the app in one step.
- New Run warns that a service will be paused while it waits for requests when your admin pauses runs nobody is using;
  a queued service's `201` carries the same warning.
- Workloads: a named run Wardyn keeps declared. `wardyn apply -f FILE` stores a workload and starts its run; an apply that
  changes nothing starts nothing, and a changed spec starts the new run before it ends the old one (`workload.replace`). A
  launch your organisation refuses keeps the old run and is recorded as `last_refusal`. `wardyn diff`, `wardyn delete`,
  `wardyn workload list|get|restart|delete` and `wardyn workload binding create|list|delete` complete the CLI. On a
  Kubernetes organisation runner a `WardynWorkload` resource in a bound namespace launches as its service identity, and its
  status shows the phase and reason. A binding names an organization pool only, and is refused `409`
  `service_identity_inactive` while the identity's sponsor is not active; a restart after the sponsor's access is
  revoked is refused too. See [Workloads](docs/operations/workloads.md).
  - The console has a **Workloads** screen: each workload's state, and a drawer with its spec digest, its history and the
    current run's inbound address. A run started from a workload shows a workload chip in its header, and Runners has a
    **Workload bindings** tab.
  - The API: `PUT`, `GET` and `DELETE /workloads/{name}`, `GET /workloads`, `GET /workloads/{name}/history` and
    `POST /workloads/{name}/restart`. Audit actions: `workload.apply`, `workload.launch`, `workload.launch_refused`,
    `workload.replace`, `workload.restart` and `workload.delete`.
  - Migration `0243_workload_k8s_uid` records the `metadata.uid` of the resource a workload was reported from. A status
    is written only to the resource with that uid, so one deleted and recreated under the same name never gets the old
    one's status. No grant changes.
- An organization runner on Kubernetes can place runs in several runs namespaces. List the extra ones under
  `extraRunsNamespaces` in the runner chart, and add the runner to a pool with
  `wardyn runner pool add POOL RUNNER --runs-namespace NAMESPACE`. The namespace is fixed when the runner joins the pool,
  and a runner serves each named namespace for one pool. See
  [Several runs namespaces](docs/operations/k8s-runners.md#several-runs-namespaces).
- Removing an organization runner from a pool that carries a use policy is held for a super admin when second-human
  governance is on, as a remote-hosted runner's removal already was.
- With several wardynd replicas, a kill, end or stop queued on a replica that does not hold a runner's session now ends
  that session, so the runner reads the action at once and not at its next reconnect. Setup status gains a warning row,
  *Runners with several replicas*, while high availability and runners are both on: a launch, terminal or revive served by
  another replica still finds the runner offline. See [High availability](docs/operations/high-availability.md).

### Changed

- With per-run hosts, every relay session request from a browser must now be same-origin, carry no fetch metadata, or be a
  top-level `GET` or `HEAD` navigation, and an `Origin` it names must be its own; anything else is `403` `ui_gateway_cross_site`.
  This holds for every app, inbound or not, so a page on another run's origin can no longer drive a person's own editor
  session.
- The gateway removes any `X-Wardyn-*` header a client sends before the request reaches the app. On an inbound app it adds
  `X-Wardyn-Caller`, and `X-Wardyn-Caller-Email` when the credential carries an email.
- An upgraded connection an inbound token opened is re-checked at most every 30 seconds and closed once its token is
  revoked, has expired, its owner is suspended or the run's profile denies inbound.

### Fixed

- A macOS guest now gets its address on a macOS VM's network. The relay answered ARP for every address, the guest's own
  included, so macOS's check of its offered address before taking it found it in use, declined it, and asked again
  forever; preparing a base then waited out its hour without reaching the guest. The relay now answers ARP only for its
  gateway, and a guest's DHCP decline is logged as one, with the guest's reason.
- On a Mac that needs setup by hand, the setup window's title now names the account to create, `wardyn-admin`, and
  says its password is the one-time code. If the guest then refuses that sign-in, preparing stops at once and says to
  run prepare again, instead of ending on an SSH error.
- A host removed from a workspace's allowed list is no longer added back when wardynd reapplies earlier "always" approvals
  at start, if the workspace could not be read or its list changed while they were being reapplied.
- Azure Key Vault: deleting and re-adding a secret while the cleanup of its replaced value was still running no longer
  loses the new value. The cleanup now checks, under the secret's lock, that the object it removes is not in use again.
- A runner retries removing a retired run's output after a failed attempt, at its next start and on each reconnect;
  before, that output stayed on disk.
- Folder sync no longer takes a different file of the same name and size for a rename, which kept the old bytes in the
  sandbox. It compares contents before renaming there, and otherwise copies the file.
- `wardyn-vmnet` in listen mode now exits on SIGTERM while a guest's connection is open but idle.
- A run's egress proxy that is told to stop before its run opens now stops; it could wait forever.
- A runner that reconnects at the moment its limits are rechecked no longer has the new session ended by that check.
- New Run: the computer-folder card no longer shows the previous folder's file counts after **Change**.
- Approvals: the Decided tab's load error now clears when the background refresh loads the list.
- `wardyn site-config set` now treats an empty list written as `[ ]` or over several lines as empty, like `[]`.
- wardynd no longer fails its first start after an upgrade whose migrations take longer than 30 seconds. The checks
  after the migration had kept the connect deadline and stopped with "verify the audit append privileges: context
  deadline exceeded".
- Helm: with `basePath` set and `env.WARDYN_BASE_PATH` set to `""`, the probes and ServiceMonitor now follow the
  empty value wardynd serves under, so the pod goes ready.
- A placement refusal that names no field no longer begins with ": ".
- VS Code in the browser could not open its workbench connection through the app gateway: the gateway now gives an app its
  own origin on a request from its own page.

### Security

- The console's `seroval` and `seroval-plugins` move from 1.6.8 to 1.6.12 (GHSA-9wx9-5pv9-3349, GHSA-f86p-h4j2-7qgj).

### Known issues

- `agent-claude-code` stays on Claude Code 2.1.231. Its embedded runtime carries a Brotli decoder flaw
  (CVE-2026-111497): a crafted Brotli response from a site a run reaches could run code as the agent, inside its
  sandbox and its egress policy. Later Claude Code releases fix that runtime. In them, though, a repository's
  `permissions.allow` still lets a tool run that managed settings with `allowManagedPermissionRulesOnly` ask about, and
  Wardyn's strictest attended rule set (L0) depends on those settings. Wardyn keeps the pin until a release passes both.
  Until then, keep a Claude Code run's allowed hosts to sites you trust.
- A dead macOS VM's slot stays held while a later Virtualization process of the same user runs. Quitting that process
  frees it.
- A Kubernetes exec run still records no output and no recording (`run_output_not_captured`).
- The macOS package is unsigned until the release workflow has the Apple Developer ID secrets; Gatekeeper refuses a
  double-click. See [Installing with a macOS package and MDM](docs/operations/hybrid-laptops.md#installing-with-a-macos-package-and-mdm).
- No macOS VM has been booted on a real Mac by this release's tests; the rows listed under
  [Not yet tested on a real Mac](docs/operations/macos-vms.md#not-yet-tested-on-a-real-mac) stay untested.
- A person other than a run's owner can open an inbound app only on per-run hosts; on one shared host the ticket is
  refused `inbound_needs_host_mode`. Opening a code editor through an inbound port on per-run hosts has not been tested
  end to end in this release.

## [0.10.0-rc.7] — 2026-10-29

### Before you upgrade

- Migrations `0237_base_image_prewrapped` (admits `prewrapped` in `base_images.kind`'s CHECK and adds the check's
  columns) and `0238_run_image_arches` (adds a column to `agent_runs`) change no row and no grant. See
  [OPERATIONS.md](docs/OPERATIONS.md).

### Added

- A run's Policy tab now marks each allowed host **Inspected** or **Tunnel only**, with a line saying what each means, and `wardyn run policy` prints the same marks above the YAML. Inspected means Wardyn opens that traffic to add a credential or check it, so the sandbox must trust Wardyn's certificate; Tunnel only passes through unread. Hosts Wardyn opens on a port no allowlist entry names are listed too, as Inspected; under allow-all, every host Wardyn opens is. The mark comes from the run's own proxy config at launch, and `GET /api/v1/runs/{id}/policy` returns it as `host_marks`. A run from before this change, or one restarted since, shows no marks. See [OPERATIONS.md](docs/OPERATIONS.md#the-policy-a-run-got).
- A Kubernetes control plane, which has no image builder, can now run a pre-wrapped image: one built `FROM` agent-base and
  pinned by digest. Add it under Images with *How it runs* → *Pre-wrapped: runs as is*, or `POST /base-images` with
  `kind: "prewrapped"`. The control plane checks it on the cluster first, with one short pod per node architecture and no
  network, and offers it to workspaces and runs only once it is ready; its runs go only to the architectures that passed.
  A refused or cut-short check gets *Check again* (`POST /base-images/{id}/check`), and each verdict is audited as
  `base_image.admit`. A security admin now has an Images page and lists, adds, checks and removes pre-wrapped images only. See
  [deploy/images/README.md](deploy/images/README.md#pre-wrapped-image).
- A macOS VM run now shares its workspace's folders. Each is shared into the guest at `/Users/agent/…` (a workspace's
  `/home/agent/…` path), with the same rules and read-only or writable choice as a container. A folder macOS protects
  (Desktop, Documents, Downloads, iCloud Drive, removable volumes) needs the runner allowed under Privacy & Security →
  Files and Folders first. A whole home folder, /Users or /private/var can't be shared, because each holds files Wardyn
  protects; share a project folder inside it. A runner older than this release can't take such a run: it is refused
  before it starts, naming the runner to update. Sharing is not yet tested on a real Mac; see
  [macOS VM sandboxes](docs/operations/macos-vms.md#not-yet-tested-on-a-real-mac).

### Fixed

- A runner that the organisation sends away or revokes while it waits for its pending work now acts on that. It could take
  it for a dropped connection and log `context canceled`: a revoked runner then kept its state, including the credentials
  delivered to it, until it next got through to the organisation.
- A header credential Wardyn adds at the proxy from a stored secret is now sent without one trailing line break, which a
  secret saved with `echo` or an editor ends in; the request carried an invalid header before. A secret that is under
  8 bytes once that line break is dropped is refused instead (`secret_value_too_short`), since output masking could not
  cover it.

### Security

- With `WARDYN_ENV_SECRET_PLACEHOLDERS`, `WARDYN_FILE_SECRET_PLACEHOLDERS` or `WARDYN_RUNNER_SECRET_PLACEHOLDERS` on, a
  secret whose stored value ends in one line break, as one saved with `echo` or an editor does, now gets a placeholder.
  It went into the sandbox as its value, and only the run's audit log said so.
- The agent images now carry GitHub CLI 2.103.3, built with Go 1.27.3. Earlier images carried 2.102.0, built with
  Go 1.27.1, which image scanners flag for CVE-2026-56857. That flaw affects Windows only, so it can't be reached in
  the Linux images.

## [0.10.0-rc.6] — 2026-10-28

### Added

- With `WARDYN_AWS_SIGV4_AT_PROXY` on, a Bedrock lane through a PrivateLink endpoint is now re-signed at the proxy, so its
  role credentials stay out of the sandbox. Only Bedrock's own services under the name AWS generates for the endpoint qualify;
  your own endpoint service and other services' PrivateLink hosts keep the old path.
- With `WARDYN_AWS_SIGV4_AT_PROXY` on, operator setup now warns when new runs using an AWS sign-in model provider's current
  settings would keep role credentials in the sandbox, and names the host or setting responsible.
- A run's Audit tab now says whether the run was set up to sign its AWS requests at the proxy or to get role credentials in
  the sandbox, and why not at the proxy, for a Bedrock sign-in run and for an AWS CLI component. It is recorded at
  dispatch (`run.bedrock.configure` gains `aws_signing`, and the new `run.aws_cli.configure` row carries the same fields).
- When a policy's allowlist drops a host from a run, New Run's Review step now names the policy the run is bound by
  under the warning, with the way to ask its owner when that policy publishes one. Create and preflight carry it as
  `warning_context`.

### Fixed

- When AWS refuses to renew an AWS sign-in, the run's refusal now says how long after signing in it came, and that a
  refusal that soon after every sign-in can be the IAM Identity Center session duration or the identity provider's
  sign-in session. The `harness.credential.refresh` failure row carries `signed_in_sec`.
- `PUT /governance/units/{id}` now takes a unit as `GET /governance` returns it, so GET, change one field and PUT the
  rest works. It refused the body with `unknown field "id"` (`governance_unit_request_invalid`); the server-set fields
  are now ignored, and a field GET never returns is still refused.
- A macOS VM prepare or start on a Mac in ordinary use is no longer refused for memory macOS can reclaim within
  seconds. The live check counted only free, inactive and speculative pages, which on a 32 GiB Mac in use stayed
  under the VM's memory plus the 2 GiB margin while macOS's own memory pressure reported nearly half of memory free. It
  now takes the larger of that page sum and the share of memory the kernel's memory-pressure level calls free.
- The runner no longer finds its macOS VM directory under a relative `$HOME`. `wardyn-runnerd macos-vm remove`
  and the staging sweep delete under that directory, so a relative one would resolve against the working directory;
  the runner now refuses it and says why.

## [0.10.0-rc.5] — 2026-10-28

### Fixed

- A failed macOS VM start no longer keeps its VM in the helper until the helper exits (#3708). Each failed attempt
  kept one, and a first boot's retries on the auxiliary storage lock can make several.
- After a change of the Mac's clock, a macOS VM run whose helper stopped no longer ends with "The Mac restarted, so
  the macOS VM was lost." (#3709). The runner tells a restart by the boot session the ledger records, as the ledger
  does, so a run whose entry has no session, from an earlier release, says its helper stopped.

### Security

- The agent images now build on Node.js 24.22.1, and `agent-vscode` on code-server 4.141.1, which bundles Node.js
  24.22.1. Earlier images carried Node.js 24.21.0, and `agent-vscode` ran code-server on Node.js 24.20.0. Both
  versions decode Brotli data with a decoder that a crafted stream can make write out of bounds (CVE-2026-111497).
  A Node.js program in a sandbox that decompressed Brotli data from an untrusted source could crash, leak its
  memory or run the sender's code with that program's rights. Rebuild locally built agent images.

## [0.10.0-rc.4] — 2026-10-26

### Before you upgrade

- A slot that 0.10.0-rc.3 recorded for a macOS VM prepare or tools update, and never freed, lacks the boot session
  and start order this release reads, so the upgraded runner frees it only once no macOS VM, helper or Docker engine
  of that user runs (#3700). Keep the runner running and quit Docker Desktop while no macOS VM runs. The runner
  checks about every 15 seconds; start Docker again once `wardyn-runnerd macos-vm status` no longer counts the VM. A
  leaked slot that an earlier release recorded, and an entry file that does not parse, stay until their owner
  removes them, as the shared ledger section of `docs/operations/macos-vms.md` says.
- A Kubernetes run that an earlier release left stopping (#3537) still has its proxy pod, both NetworkPolicies and a
  Secret holding the proxy's config, the run's secret values and its managed files in the runs namespace. Its stop is
  retried when its runner, upgraded to this release, next reconnects, and finished when the pod shows the proxy never
  started; the pod, the policies and the Secret are then removed. Upgrade the runner first. A run still held after
  that keeps its fence; `docs/operations/k8s-runners.md` ("A stop still held after the upgrade") says how it may be
  released.
- A macOS VM restore image in `macos-vm.noindex/cache` that this release has no record of, placed by hand or kept by
  an earlier release after a failed prepare, now stops `macos-vm prepare` (#3706). Prepare names the file and neither
  uses nor deletes it. Delete it, so prepare downloads the image again, or move it out of the cache and pass it with
  `--ipsw <file> --sha256 <hex>`, taking the hash from a source you trust.

### Added

- `wardyn-runnerd macos-vm prepare --ipsw <file> --sha256 <hex>` builds a Mac's base from a restore image downloaded
  elsewhere, for a Mac whose network refuses Apple's pinned update host. `--sha256` is required: prepare copies the
  file into its cache, checks the copy's hash before anything loads or installs it, refuses a mismatch naming both
  hashes, and never changes or deletes the file given. The pinning error now names `--ipsw` (#3701).

### Changed

- A macOS VM's prepare reuses a restore image from its download cache only when a previous prepare's download wrote a
  record of its SHA-256 beside it and the file still matches. A file with no record, or a different hash, stops
  prepare with an error naming the file and what to do; it is never deleted. The record is written only for an image
  Wardyn downloaded over the pinned connection in a download it started, resumed only from its own partial file (the
  one with the marker Wardyn wrote for it), or copied with `--ipsw` after its hash matched. It guards against
  hand-placed or damaged files, not against someone with the runner user's write access (#3706).
- A macOS VM's prepare behind a proxy that inspects TLS now fails at the first range of the restore image, not after
  three retries, when the error is Apple's certificate-pinning refusal of `updates.cdn-apple.com`. The error says the
  host's certificate chain failed Apple's pinning check, that a proxy inspecting TLS is the likely cause and to exempt
  the host from it, and prints the image URL; the requirements table in
  `docs/operations/macos-vms.md` lists the uninspected path (#3701).
- The macOS VM start refusal "this Mac does not have enough free memory right now" now gives the free memory (free,
  inactive and speculative pages) and the memory needed (the VM's plus the margin) in GiB, and names lowering Docker's memory allowance as the
  usual fix; `macos-vm status` and `prepare` on a runner with no `runnerd.json` now say none was found at its path and
  to run `wardyn-runnerd setup` or write `org_url` into it, and use the `--org` an earlier service definition passes
  `serve` (#3702).
- Upgrade note: a hand-installed macOS LaunchAgent does not get `--log-file`, so its log is not rotated until someone
  adds the flag to its `ProgramArguments`. `wardyn runner upgrade` to a newer release writes it; on a runner already on the
  current release, upgrade stops before setup, so run `wardyn-runnerd setup` again or add the flag by hand (#3702).
- A macOS VM's prepare now refuses a base that holds another local account than `wardyn-admin` and `agent`, and names
  the extra one, so an account made by hand during setup no longer ships in every VM cloned from the base (#3687).
- `wardyn-runnerd macos-vm status --json` has a `failure` key when the base has one, and a setup by hand the helper
  refused because it cannot reach the Mac's desktop (over SSH, under sudo, or with no one signed in) is
  `no_desktop`, with the state `failed`; before, only the error text said so. A console that does not know the code
  shows the error text as before.
- A Linux or macOS VM run now refuses a new connection on a granted port other than 80 once it has 1,024 such
  connections open at once, and reports the refusal through its denial report with the reason `tunnel-limit`; the cap
  does not evict existing connections, and lower process or container resource limits can bind first. The relay now
  sends TCP keepalive probes toward both the guest and the proxy, so a peer that vanished is noticed; the
  proxy-to-origin leg is unchanged (#3489).
- The runner inventory's macOS VM capabilities no longer carry the runner's own `refusal` sentence. Read `reason`
  (with `min_cpus` and `min_memory_mib`) instead; the console words the refusal from those.

### Fixed

- A Linux VM whose guest agent never becomes ready before the boot timeout now reports the last error the agent
  gave (for example a refusal) instead of only the context deadline (#3495).
- The host's connection to a desktop VM's guest agent no longer keeps every connection it has ever redialled, and
  reads at most 64 KiB of response headers from the guest; a frame cut off after its header now reads as an unexpected
  end of stream rather than a clean one (#3388).
- `wardyn-runnerd serve --log-file` now bounds the crash log beside it: at start, a `runnerd.crash.log` over 1 MiB moves
  to `runnerd.crash.log.1` and is emptied in place, so a runner that panics at every start no longer grows it without
  limit (#3633).
- A first macOS VM prepare no longer fails right after the install with "Failed to lock auxiliary storage" (#3699). The
  helper now runs the install in a process of its own, whose exit lets go of the installer's VM and its auxiliary
  storage before the first boot, and retries that boot for a few seconds on exactly that refusal.
- A macOS VM that ended without reporting its stop, such as a failed prepare, no longer keeps its slot until Docker
  stops (#3700). The runner frees it once no Virtualization or helper process of the user created after the VM's helper
  remains, other than another macOS VM's own helper, so Docker's Linux VM, started earlier, no longer counts as that
  VM. Which came first is read from the time since the Mac started, so a change of the clock cannot make that VM look
  older than its helper. A process it cannot prove is not that VM still keeps the slot, and a slot recorded by an
  earlier release frees once no macOS VM, helper or Docker engine of that user runs.
- A finished tools update of the macOS VM base no longer keeps one of the Mac's two VM slots until the Mac restarts
  (#3700). The runner now frees its entry once its helper has exited, as it does for a finished prepare.
- A change of the Mac's clock no longer makes the macOS VM ledger take its live VMs for an earlier boot's. The ledger
  told boots apart by `kern.boottime`, which macOS moves with the clock, so a clock change could stop counting a live
  VM, refuse its helper and delete its runner's entries. Each entry now records the boot's session, and only a
  different session means a restart. An entry with none, from an earlier release, is never taken for an earlier
  boot's, and an entry file that does not parse now counts until its owner removes it, whatever its time.
- A Kubernetes run whose proxy container is running but never becomes ready now fails at the start deadline with a line
  naming the readiness probe and the proxy image, and saying to check the proxy's log and connectivity; on a runner it
  adds that the proxy may be waiting for its first run-token renewal with the organisation (#3612).
- The audit trail now records the end of a desktop sign-in or take-over pause that was still open when the run ended,
  after the person had closed the desktop (#3583). Its `desktop.recording.resume` row is written once, marked
  `end_reason` `run_ended`, and the server stops holding the pause. Before, the pause stayed open with no end row.
- A Linux VM run whose agent was paused for a desktop sign-in, then lost to a control-plane outage and its agent
  stopped, now has the pause ended once the stop is confirmed (#3629). Its `desktop.sign_in.pause` row is written once,
  ended at the stop, and a recorded desktop's `desktop.recording.resume` row says `end_reason` `run_ended`. Before, the
  pause stayed open with no row. A pause whose stop failed, or is not yet made, still stands.
- `wardyn-runnerd doctor` on a host whose service definition is still an earlier install's, which passes `--org`, now
  checks the organisation that service uses, as `serve` does, instead of the one in `runnerd.json`.
- `wardyn-runnerd macos-vm status` and the runner no longer lose a failed prepare's reason when a long line in
  `prepare.log` comes before the helper's error; a read that stops early says so (#3690).
- The macOS runner package's install script sets its own `PATH` instead of relying on the installer's (#3646).
- A macOS VM run's disk-removed report is no longer lost when the report prune runs as the report is stored (#3682).
- On macOS, a base-image download that resumes a partial file now continues after the bytes already on disk instead of
  overwriting its start, so a restarted preparation no longer re-downloads the whole image after the check rejects
  it (#3684).
- On macOS, a base preparation whose network relay stops on a virtual network device failure now fails with the relay's
  error, instead of waiting for a guest that has no network and leaving the reason only in `prep-relay.log` (#3643).
- On macOS, a base preparation or a tools update whose VM macOS refuses to start for its VM limit now records the leaked
  VM slot, as a run already did, so the slot is counted at once instead of when a run next hits it (#3659).
- A macOS VM runner's stage-2 network is now removed when the job's VM goes away before its control socket appears, and
  is no longer torn down when handing it to the helper fails after the helper already took it (#3641, #3642).
- The macOS VM ledger setup (`wardyn-macvm ledger-setup`, run by the runner package's postinstall) no longer fails on a
  directory that a login made in the entries directory under an entry's name: the directory moves to a root-only
  quarantine beside the base and the setup finishes (#3675). An entry file with the immutable flag still stops it.
- A Kubernetes runner's proxy now logs `wardyn-proxy: listening` when it has bound its port, after its first run-token
  renewal, not while it is still waiting for that renewal (#3632).
- The server no longer warns every minute that re-asserting a kept run's stop failed while the run's runner is offline
  and the stop is already queued for it (#3703). It warns once, and logs once when the re-assert succeeds after the
  runner reconnects; any other failure still warns on every pass, and the stop is still re-asserted on every pass.
- While the Docker engine is down or restarting, a runner's log no longer repeats `runner containment will retry` every
  second or two (#3704). Each error a run's containment fails with is logged once, and its repeats as a summary line
  about once a minute saying how many went unlogged, even when two errors take turns; a new error, or the run's
  containment succeeding, logs at once. Containment is still tried on every pass, and a failed pass still refuses the runner's ready state as
  before.
- A Kubernetes runner now finishes stopping a run whose proxy pod was deleted after its setup step finished and before
  the proxy started, such as a run cancelled at that moment (#3537). The kubelet reports that pod as failed with the
  proxy never started, which the runner did not accept as proof that it stopped. The pod, both NetworkPolicies and the
  Secret holding the proxy's config, the run's secret values and its managed files stayed in the runs namespace until
  removed by hand.
- A recorded run that ends within its first five minutes, such as a short command on a laptop's Docker runner, uploads
  its whole recording once, at exit. That upload is now retried when it gets no answer or a 502, 503 or 504 (the run's
  proxy cannot record the request yet, or the runner's link to the organisation is down), with new attempts starting
  for up to 30 seconds (an attempt already sending may finish later), so the run keeps its recording (#3705). Before,
  one such refusal lost the whole recording.
- A single run's own read (`wardyn runs get <id> --json`, `GET /api/v1/runs/{id}`) now reports `has_recording`,
  `recording_bytes` and `recording_duration_sec` without `?include=recording_meta` (#3705). Before, it said
  `has_recording: false` for every run, including one whose recording was stored. Run lists report the recording
  fields only with `?include=recording_meta`; without it a listed run says `has_recording: false` whatever is stored.

## [0.10.0-rc.3] — 2026-10-25

### Before you upgrade

- Split-role installs (`WARDYN_PG_MIGRATE_DSN` set) whose app role does not pick up new tables through the `ALTER
  DEFAULT PRIVILEGES` step in Operations: grant the app role `SELECT, INSERT, UPDATE, DELETE` on `macos_vm_disks`
  (migration `0236_macos_vm_disks`). Without it a macOS VM run's page never says whether its disk was removed from the
  Mac. An upgrade from 0.9.x also needs every grant the 0.10.0-rc.1 notes list: `SELECT, INSERT, UPDATE, DELETE` on
  `aws_sso_refused_tokens`, `cli_sign_ins`, `ui_desktop_closed`, `desktop_signins`, `desktop_signin_shares`,
  `desktop_signin_runs`, `approval_rules`, `governance_restrictions`, `governance_restriction_members`,
  `governance_unit_admins`, `oidc_session_sids` and `desktop_vm_sign_ins`. Also grant it `SELECT, INSERT, UPDATE` on
  `runner_health`, `launch_preset_version_floor`, `cli_sessions` and `governance_boundary`. If it turns on
  `WARDYN_MULTI_AGENT_TASKS`, grant it `SELECT, INSERT, UPDATE` on `run_harnesses` as well.

### Added

- A macOS VM run's page says when the run's disk was removed from the Mac, or why it could not be. It needs this
  release's wardyn-runnerd. With an older one the page says "Removing…" for 10 minutes after the run ends, then nothing
  about the disk. A saved "Finished" arrangement of the run page gains the "Attach from your terminal" tile at its
  bottom, once, where the disk line shows.
- macOS VMs run on a Mac on macOS 26 as well as 27. macOS 26 has no unattended guest setup, so on a 26 Mac each
  `wardyn-runnerd macos-vm prepare` stops after the install for setup by hand, once per base, about 10 minutes at the
  Mac; `status` says so beforehand. Started over SSH or with sudo, it refuses at once and says to run it in Terminal
  at the Mac's desktop. On macOS 27 nothing changes. Setup by hand now opens the guest's window on the Mac,
  "Wardyn: set up this macOS VM"; before, its instructions pointed at a window that did not exist, even on 27. The
  helper is built for macOS 26, and the release checks that no binary in the package asks for a newer macOS.

### Fixed

- A macOS VM start that failed after its helper had started deleted the run's directory even when the helper
  outlived its kill. The teardown and the orphan sweep need that directory to end the VM later, so it now stays,
  with the run's capacity held, until the helper has exited.
- A macOS VM run whose policy keeps no desktop sign-ins now starts agent-only. Its guest turns the desktop off and
  sets no desktop password, and the runner took that as the desktop password failure and ended the run.
- A run on a runner could be refused with "no eligible runner" at the moment the runner was busy recording an idle
  signal, though the runner was connected and ready. The runner now reads as connected throughout.
- Stopping a run on a Docker runner no longer stops half-way when the organisation gives up on a slow stop and resets
  the call. The stop finishes on its own time budget, so the run's proxy and network are not left for the clean-up
  sweep.
- A runner upgrade that failed to start and was being rolled back no longer overwrites the kept copy of the old runner
  with the broken one first, so a full disk at that moment cannot leave the computer with no copy of the old runner. A
  rollback also keeps its own copy of the old runner to restore from.
  An upgrade also stops, with the runner untouched, when it cannot tell whether a previous copy is there (for example
  a permissions error), rather than assuming there is none.
- A desktop recording that stopped early, because the desktop sent a message the recorder could not follow, was saved
  as an ordinary success. Its save record now says how far in the recording ends, and wardynd logs a warning.
- A desktop recording's audit chain check passed when the oldest rows of the audit log had been deleted and nothing
  came before the recording's own rows. It now fails there, as the whole-log check does.
- In local mode with the second-admin switch on, the boundary editor no longer says "Submit for approval" for a save
  that is refused. The save is refused in local mode, so the editor now words it as a plain save.
- Over the SSH gateway, a command run in a bring-your-own image whose `HOME` is relative (resolved through `CDPATH`)
  or is `-` no longer starts its output with a stray directory line. The output is the command's alone, as under
  OpenSSH.
- `wardyn-runnerd doctor` finds the organisation address the way the service does: `--org`, the managed `OrgURL`
  setting, then `org_url` in `runnerd.json`. It no longer reads a `WARDYN_RUNNER_ORG_URL` exported in your shell, which
  the service never reads, so a check that passed on that variable no longer hides an address the service would not
  find. The variable is read only from an earlier install's service settings, where its unit passes it to `--org`.
  Its hints name the settings that work.
- An admin's successful check of the audit chain over another person's desktop recording now leaves a `recording.read`
  row in that run's audit, marked `chain_check`. The check reads only the recording's metadata, not its content. A check
  of your own run still leaves nothing, and who may ask, and what a refused caller sees, are unchanged.
- A macOS VM run stuck in one start phase, such as connecting to the VM, now shows the same overdue advice as any
  other start once the macOS start time has passed. Before, the phase row kept its spinner and said nothing. A start
  that failed still shows its failure.
- Choosing Linux again after macOS in New Run no longer drops a CPU or memory value you had set to exactly the Mac
  minimum before choosing macOS. Only the figures that choosing macOS filled in are taken back, and only while they are
  still at the minimum.
- In New Run, a Mac pool that was preparing or at capacity when the page opened offers macOS once it recovers, when
  you move to another step. Before, it stayed unavailable until the page was reloaded, and the draft is kept either way.
- A run on a runner whose sandbox disappears without an exit code now ends. When two reports at least 30 seconds
  apart find the sandbox gone, the run is marked lost (`gone`) and ends FAILED, and its slot frees at once (#3667).
  Before, it stayed running and held the runner's capacity until its token lapsed. Migration
  `0235_run_sandbox_absent_at` adds a column to `agent_runs`; no grant changes.
- An interrupted or failed reinstall of the runner package on a Mac no longer leaves it counting fewer running macOS
  VMs than it has. The installer also stops, with the count untouched, when it cannot read the count's files, where
  it used to start the count empty.
- A brief database error while a runner replays its saved audit events no longer drops one of them from the audit
  log. The event is kept for the next replay.

## [0.10.0-rc.2] — 2026-10-25

### Before you upgrade

- 0.10 can't be rolled back by redeploying 0.9.x: its migrations rename tables and change constraints. Take a database backup first.
- **Upgrade wardyn-runnerd and rebuild the Linux VM guest image (`vm-linux`) to order a desktop sign-in's pause and
  resume.** A run on an older wardyn-runnerd or guest image is not refused: its pause and resume work as before, its
  `desktop.sign_in.pause` row says `fenced` false, and a continue that reaches its guest late can still run the agent
  while the run page says paused. Migration `0231_desktop_vm_pause_generation` adds a column to `desktop_vm_sign_ins`;
  no grant changes.
- During a rolling control-plane upgrade, a desktop sign-in pause can stay paused until it is resumed from an upgraded server.
- A Kubernetes runner (wardyn-runnerd) gets the short-run recording fix below only with a proxy image of this release.
  The Helm chart pins the proxy tag to the runner's. A runner started without Helm and not pinned uses the proxy image
  the organisation named at registration: when that is an older release, its runs keep working as before, without the
  fix, and the runner logs a warning once. Have the organisation name this release's proxy image, or run setup with
  `--proxy-image` set to it.
  A proxy image named by digest or by a tag that is not a release number starts its runs as before, whatever its
  release: this release's proxy gets the fix that way too, and an older one keeps working without it. The runner
  cannot read such an image's release, so it logs no warning for it; name a release tag to have it checked.
- The proxy-ready fix on Docker spans three pieces: wardyn-runnerd, the wardyn-proxy image and the agent images.
  Upgrade the organisation before its runners, as always. A runner that follows an older organisation's proxy image
  keeps starting runs as before and logs once that the image does not announce when it is listening, so the fix does not
  reach those runs until the organisation is upgraded. The agent images wait for the proxy on their own and need no
  runner change.

### Changed

- Changing a runner's per-run CPU or memory now ends its connected session, so the runner reconnects with the new
  pair. Before, it kept the old pair until it next reconnected. A runner offline at the time reads the pair when it
  next connects. Other limit changes leave the session alone.
- On `agent-base`, an exec-mode run (`workload.kind: command`) that clones a repository or names a starting folder
  now gets the same workspace prep a harness run does: the repos are cloned into `/home/agent/work` and the git
  credential helper is ready, where the clone was skipped and the folder left empty unless the run also had a
  starting folder. A run with neither is unchanged.
- A failing retry no longer floods the logs. The organisation retries a runner exit receipt after 5 seconds, doubling
  to 5 minutes, and logs an identical error once per interval with a `repeated` count; a receipt refused on 8 tries in
  a row as a foreign, stale or incomplete incarnation is parked (migration `0232_runner_exit_parked`): kept, audited as
  `runner.exit.park`, no longer retried, and failed by `wardyn doctor`'s new `runner-exits` stage. The runner backs off
  each run's failing replay on its own, without holding up the others, logs its containment and replay retries the
  same way (containment is still tried on every pass), and on macOS writes `~/Library/Logs/wardyn/runnerd.log` itself, rotated
  at 10 MiB with three older files kept (a crash goes to `runnerd.crash.log`); `wardyn runner upgrade` rewrites the LaunchAgent to pass `--log-file`.

### Added

- macOS virtual machines (preview, off by default). A Mac on macOS 27 or later is built to run up to two macOS VM
  sandboxes ([not yet tested on a real Mac](docs/operations/macos-vms.md#not-yet-tested-on-a-real-mac)),
  installed on that Mac from Apple's own restore image. Wardyn does not ship macOS. Turn it on with `WARDYN_MACOS_VMS`
  in the organisation and, on the Mac, `WARDYN_RUNNER_MACOS_VM=1`, the managed preference `MacOSVM` or
  `wardyn-runnerd setup --macos-vm on`. `wardyn-runnerd macos-vm prepare`, `status` (`--json` for one JSON object) and
  `remove` build, show and delete the Mac's macOS base; `prepare --new-image` builds a new one from Apple's newest
  restore image. The runner offers macOS VMs only when its helper's `wardyn-macvm probe` passes, offers as many as the
  Mac's shared ledger and memory leave room for, and ends its newest one when the disk drops below 5 GiB free. Migration
  `0234_macos_vm_kind` lets a run record the `macos_vm` kind. `wardyn-runnerd setup --keep-ipsw` keeps Apple's
  restore image, as `prepare --keep-ipsw` does.

### Fixed

- A unit admin deciding a held membership change no longer sees, in its diff, the other units the person holds
  memberships under (`belongs_by`). Super admins and security admins still see them; the decision's recheck is
  unchanged.
- A headless run on a runner could be marked lost (reboot) when a check landed between its agent's exit and the
  runner's exit receipt; the receipt was then refused and the run never recorded `run.complete`. The runner's exit
  receipt now ends such a run (#3665).
- An SSH key approval's title now says what happens: "Sign Git's SSH sign-in for <host>", where it said "Write an
  SSH key for <host>". The proxy signs the sign-in and no key file enters the sandbox.
- The desktop replay shows where a screen recording stopped, at its size cap or at a stream the recorder could not
  follow. Before, it froze on the last frame with no notice.
- A VM run lost to an outage keeps its desktop sign-in pause, since its agent may still be running. A run lost because
  its lease ended or its agent's container exited ends the pause and its recording hold.
- The image launcher split each `WARDYN_REPOS` clone record with a tab as whitespace, so an empty field moved the
  fields after it up a place. This control plane never sends such a record. A control plane that sends only the
  single-repository variables (`WARDYN_REPO_URL`, `WARDYN_REPO_SLUG`) got no git broker rewrite, and a record with
  an empty middle field lost its branch. Empty fields now stay in place.
- A unit delete, move or rename no longer scans every membership per affected person (migration
  `0233_governance_memberships_subject` adds an index on the membership subject).
- A continue that reaches a Linux VM's guest agent after a later "Pause the agent to sign in", as one delayed past a
  lost run lock can, no longer runs the agent while the person signs in. Each pause takes the run's next pause
  generation, kept in the database (migration `0231_desktop_vm_pause_generation`), and every stop and continue carries
  one to the guest agent, which drops a request out of order and answers `409`: one older than the newest it has
  seen, or a stop after its own generation's continue. The order outlives a restart of the guest agent's service. Each
  pause-to-resume's `desktop.sign_in.pause` row records `fenced`, false when the run's wardyn-runnerd or guest image
  predates it.
- A short command run on a Kubernetes runner (wardyn-runnerd) no longer loses its recording. The run's proxy refuses
  every request until its first token renew succeeds, but its pod read as ready once it listened, so the agent could
  start, finish and have its recording upload refused inside that window. The proxy now listens only once its first
  renew has succeeded, so its pod is ready only once it serves.
- A run on a Kubernetes runner (wardyn-runnerd) whose proxy cannot renew its run token, for example because a
  firewall blocks the organisation, now fails naming the cause: "the run's proxy could not renew its run token with
  the organisation:" and the proxy's last renew error, without the token or the URL's path and query. Before, its
  start waited out the start timeout and said only that the proxy pod never became ready. A proxy that has never
  renewed now gives up about a minute after it starts; a refused renew still ends it at once, with the same sentence.
- `GET /runs/{id}/output` no longer points the run's owner or an operator at a recording that was never received.
  With run output persistence off, an ended run on a runner that does not capture output, and whose recording never
  arrived, answers `409` `run_output_not_kept`.
- On Docker, a run's agent can no longer start before its proxy is listening. The runner now holds the agent back until
  the proxy reports it has bound its port, and fails the run if it does not within 10 seconds. The
  agent images also wait up to 10 seconds for the proxy before they run the task, and carry on with one stderr line if
  it never answers. Before, an exec run's first requests, or a very short run's recording upload, could be refused with
  "connection refused".
- A Linux VM run that ends while its agent is paused to sign in now writes its `desktop.sign_in.pause` audit row, marked
  `ended_paused` with the run's own end time and no `resumed_at`. Before, the pause had no row at all, because a resume
  is refused once the run is not running.
- A desktop recording's replay no longer shows a failed clipboard or file action read as an empty action log. It shows
  the replay's load failure with its retry.
- Retrying a team drive save that stored one group's allocation and failed on another now also removes a group the
  admin took out of the editor since. Before, that group's allocation stayed.
- The setup checklist's model-access row now reports `resident_file` for a run served by an AWS sign-in, whose session
  file is held in the sandbox, instead of `proxy_injected`, so it agrees with the `model_credential` the same
  response carries.
- A runner whose event the organisation refused for good no longer reconnects and drops every second: the refusal is
  acknowledged and audited (`runner.event.refuse`), and a refused action result closes its action (#3648).

## [0.10.0-rc.1] — 2026-10-24

### Before you upgrade

- New, off by default: `storage.user_drive.member_create.keep_main_drive` (#1991). Turned on, a run can keep its work
  folder as its person's own drive; whatever an agent leaves there, a planted script or a harness project file included,
  comes back in the next run that attaches it.
- Upgrade wardyn-runnerd before you rely on a newer proxy feature: a run that sets one (`aws_sigv4`,
  `scrub_responses`, `vm`, `port_rule`, `local_models`, `kube_api`, `canonical_push_repos` or `local_model_relay`) is
  refused on an older wardyn-runnerd.
- **A security admin can no longer add or change a mount source on a governance profile, or see a mount's host path; a super admin can.**
  A security admin reads every `workspace_mounts[].source` as `<redacted>`, on policies, presets, profiles, the
  policy default and held changes. To keep a mount, send it back with that sentinel and its target, as
  `wardyn governance get` and `set` do; an empty source is still refused `400`. Adding a mount, changing its host
  path or making a kept mount writable is refused `403` `admin_surface`, and a sentinel on a target with no stored
  host path is refused `422` `governance_mount_keep_unknown`. A pending profile change that sets a host path is
  shown to and decided by super admins only. The risk assessment and the composed-profile warnings name a mount by
  its target, for every reader.
- Split-role installs (`WARDYN_PG_MIGRATE_DSN` set) whose app role does not pick up new tables through the
  `ALTER DEFAULT PRIVILEGES` step in Operations: grant the app role `SELECT, INSERT, UPDATE, DELETE` on
  `aws_sso_refused_tokens` (migration `0194_aws_sso_refused_tokens`). Without it an AWS SSO session whose renewal
  AWS refused cannot be marked, read or cleared, so replicas do not share that mark. Grant it `SELECT, INSERT, UPDATE`
  on `runner_health` (migration `0206_runner_health`). Without `INSERT` or `UPDATE` a runner's offline summary and
  relay status are not kept; without `SELECT` every one-runner read (`GET /api/v1/runners/{id}`,
  `GET /api/v1/me/runners/{id}`) answers 500. Grant it also
  `SELECT, INSERT, UPDATE` on `launch_preset_version_floor` (migration `0221_launch_preset_version_floor`).
  Without it, creating or deleting a launch preset fails.
- **`wardyn run attach` is now `wardyn attach`.** The old spelling is an unknown command, with no alias. A script
  that calls `wardyn run attach <run-id>` changes to `wardyn attach <run-id>`. `wardyn run ssh` is unchanged.
- Split-role installs (`WARDYN_PG_MIGRATE_DSN` set) whose app role does not pick up new tables through the
  `ALTER DEFAULT PRIVILEGES` step in Operations: grant the app role `SELECT, INSERT, UPDATE` on `cli_sessions` and
  `SELECT, INSERT, UPDATE, DELETE` on `cli_sign_ins` (migration `0199_cli_sign_in`, which also adds `kind` and
  `cli_session_id` to `api_tokens`). Without it every API token read fails, because a token's liveness now reads
  `cli_sessions`.
- Split-role installs (`WARDYN_PG_MIGRATE_DSN` set) that turn on `WARDYN_MULTI_AGENT_TASKS`: grant the app role
  `SELECT, INSERT, UPDATE` on `run_harnesses` (it holds the first two since `0164_run_harnesses`; migration
  `0203_run_harness_task` adds `task`, which person erasure blanks, so it needs `UPDATE`). With the switch off,
  nothing needs the new verb.
- Split-role installs (`WARDYN_PG_MIGRATE_DSN` set) whose app role does not pick up new tables through the
  `ALTER DEFAULT PRIVILEGES` step in Operations: grant the app role `SELECT, INSERT, UPDATE` on
  `governance_boundary` (migration `0200_governance_boundary`). Without it the Wardyn boundary cannot be read or
  written. The same migration's `agent_runs.break_glass` column needs no grant. Grant it `SELECT, INSERT, UPDATE,
  DELETE` on `governance_restrictions` and `governance_restriction_members` (migration `0227_governance_restrictions`).
  Without it restrictions cannot be read or written. Grant it `SELECT, INSERT, UPDATE, DELETE` on
  `governance_unit_admins` (migration `0228_governance_unit_admins`). Without it unit admins cannot be granted, read
  or revoked.
- Split-role installs (`WARDYN_PG_MIGRATE_DSN` set) whose app role does not pick up new tables through the
  `ALTER DEFAULT PRIVILEGES` step in Operations: grant the app role `SELECT, INSERT, UPDATE, DELETE` on
  `ui_desktop_closed` (migration `0219_ui_desktop_closed`). Without it a desktop Close cannot be stored or read, so
  every desktop session is refused and Close fails.
- **Copying out of a desktop is now blocked by default.** The UI gateway's relay filters the clipboard of the
  `novnc` desktop app: paste into the desktop stays allowed, copy out of it is dropped and audited as
  `ui.clipboard.transfer`. Until now both directions synced unfiltered. A `novnc` socket that does not speak RFB is closed.
  Other apps are unchanged. To keep copy out, set `desktop.copy_out` to `allowed` on the policy, and on the ceiling of any
  member who should have it ([`desktop`](docs/POLICIES.md#desktop--desktopcontrols)).
- Split-role installs as above: grant the app role `SELECT, INSERT, UPDATE, DELETE` on `desktop_signins`,
  `desktop_signin_shares` and `desktop_signin_runs` (migration `0220_desktop_signins`). Without it a desktop run starts
  without sign-ins, a desktop sign-in cannot begin, and the sweep that ends one cannot read it.
- **Sign-ins in a Linux VM's desktop are allowed by default** (#2972). Admin view › Providers › Agents gains "Sign-ins in
  a desktop" per agent (`agent_providers.agents[].desktop_sign_ins_off`, absent is Allowed). A VM's guest now mounts an
  empty tmpfs over its keychain and browser profile paths (`/home/agent/.local/share/keyrings`, `.mozilla`,
  `.config/chromium` and the rest in [Linux VM sandboxes](docs/operations/linux-vm-sandboxes.md#sign-ins-in-the-desktop)),
  read-only for an agent set Not allowed, so anything a custom guest image baked there is hidden. A guest with any swap
  or zram on now fails to start. Upgrade wardyn-runnerd and rebuild the guest image together. A sign-in pause stops the
  agent and pauses the desktop recording while it is open.
- Split-role installs as above: grant the app role `SELECT, INSERT, UPDATE, DELETE` on `desktop_vm_sign_ins`
  (migration `0230_desktop_vm_sign_ins`). Without it a VM's run page says nothing about sign-ins and offers no pause.
- Split-role installs as above: grant the app role `SELECT, INSERT, UPDATE, DELETE` on `oidc_session_sids` (migration
  `0229_oidc_session_sids`). Without it, with `WARDYN_OIDC_FRONTCHANNEL_LOGOUT` on, a sign-in's `sid` is not kept, so
  the identity provider's front-channel logout ends none of that person's sessions.
- **Desktop sign-ins are new and off** (`WARDYN_DESKTOP_SIGNINS`, default `off`). Turned on, a run's creator can sign
  in to websites inside the run's desktop while the agent is stopped, and share one site's login with the agent. The
  person's browser is a different uid in the same sandbox, not a sandbox of its own, and after a share the agent holds
  that site's cookies within its egress until the run ends: read [THREAT-MODEL §5.1a](threatmodel/THREAT-MODEL.md)
  before turning it on. Turned on, every run whose policy declares the desktop uses a larger image of its own,
  `agent-novnc-signin` (about 670 MB compressed, against `agent-novnc`'s 400 MB), whether or not a sign-in happens;
  `agent-novnc` itself is unchanged, and with the setting off the sign-in image is never pulled. Self-hosted runners,
  pools and Kubernetes get no sign-ins, and their runs start as before.
- **Governance profiles are now units, and assignments are now memberships.** The routes are
  `/governance/units`, `/governance/memberships` and `/governance/check` (was `/governance/preview`), and the keys
  are `units`, `memberships`, `unit_id` and `parent_id`. The old routes answer 404 or 405 and are not aliased; a
  script or a `wardyn governance` document that names the old routes or keys changes to the new ones. A 0.9
  governance document is refused, and the error names the new key. The audit actions are `governance.unit.*` and
  `governance.membership.*`; rows written before the upgrade keep the old names, so a query over a span that
  crosses the upgrade matches both.
- The same rename reaches what a client reads outside the governance body. Reason codes become
  `governance_unit_*`, `governance_membership_*`, `user_drive_denied_by_unit`, `member_drives_denied_by_unit`,
  `unit_unreadable`, `unit_gone`, `governance_unit_not_found` and `governance_membership_not_found`. The enum value
  `profile` becomes `unit` in a policy's `source`, `cause` and `layer`, in managed-settings `locked_by` and in a
  change's `target_kind`. The fields are `unit_name` and `unit` in the policy envelope, `user_drive_denied_by_unit`
  in `GET /me`, `denied_by_unit` in the drives catalogue, `unit_id` on a run, and `unit_name` in
  `GET /policies/default`. The approval-webhook payload carries `unit {id, name}`. The wardynd flag is
  `-governance-unit-put-existing-only`, and the task is `governance.units.walk`.
- `WARDYN_GOVERNANCE_PROFILE_PUT_EXISTING_ONLY` is now `WARDYN_GOVERNANCE_UNIT_PUT_EXISTING_ONLY`. wardynd refuses
  to start while the old name is set, and names the new one. A notify route keyed `profiles` is now `units`; the
  old key is refused when the config is read.
- **Four hardening switches are now on by default.** Set one to `false` to keep its old behaviour; boot logs each
  one's state.
  - `WARDYN_EGRESS_HOST_PORT_GRANTS_INERT`: re-save each `egress_host` allow grant whose value carries a port through
    `POST /api/v1/permissions/grants`, which stores the host alone, then delete the old row. Boot logs how many
    rows are inert.
  - `WARDYN_GOVERNANCE_UNIT_PUT_EXISTING_ONLY`: switch callers that create a unit by `PUT` to
    `POST /api/v1/governance/units`.
  - `WARDYN_CAPABILITY_GRANTS_VERIFIED_EMAIL`: check that each allow grant matched on an email has a sign-in that
    vouches for it (`WARDYN_OIDC_EMAIL_DOMAINS` or `WARDYN_OIDC_REQUIRE_EMAIL_VERIFIED` set, or
    `WARDYN_OIDC_ALLOW_EMAIL_MAPPINGS` on). An Entra ID token usually carries no `email_verified`, so on Entra
    those grants stop granting; boot warns with how many. Re-grant each to the person's user id (sub) or a group.
  - `WARDYN_DOCKER_PROXY_SANDBOX_PEERS_ONLY`: a caller of a run's proxy from outside the run's own network is now
    refused. A runner that is newer than its organisation keeps the old behaviour (any peer) until the organisation
    is upgraded, and logs that it did.
- Migration `0207_governance_units` renames the governance tables and their columns, keeping every row, grant and
  foreign key, so a split-role install needs no grant change. A governance change proposed before the upgrade reads
  as stale once and is proposed again.
- **`WARDYN_MCP_LOCAL_SERVERS` is off by default.** Turning it on lets an organisation or a person define an MCP
  server that runs inside the run's sandbox. That server runs with the agent's access, and the harness, not
  Wardyn's proxy, applies the run's rules to its tools.
- **With `WARDYN_MCP_LOCAL_SERVERS` off, a rebooted run whose MCP components were erased is no longer revived.**
  Starting the agent again admits the run anew, and the erased row keeps only its harnesses, so it is refused in the
  organisation form ("Ask your admin"). An erased remote server is refused too, which a 0.9 revive accepted. A run
  that is still running keeps its server until it ends.
- **The first restriction changes how admin-token runs are recorded.** With the Wardyn boundary in `hard_limits` and
  bounding something, an admin-token run is a break-glass run once any unit with its own walls exists, and now also
  once any restriction exists. A boundary that bounds nothing, as the fresh-install seed does, asks no reason. A
  deployment that has only pass-through units or none sees no change until someone writes its first restriction.
  From that write on, an admin-token `POST /runs`, `/runs/preflight` or `/runs/policy-preview` without an
  `X-Wardyn-Break-Glass-Reason` header (`wardyn run --break-glass-reason`; at most 512 characters) is refused `400
  break_glass_reason_required`, so a script or CI job that starts runs with the admin token and sends no reason stops
  working. Runs it starts with a reason carry `break_glass: true` and a `run.break_glass.open` audit row holding the
  reason. The boundary still binds them, including its run-length maximum (`limits.max_end_ahead_sec`). Restrictions
  themselves never apply to those runs. Deleting the last restriction (and having no unit with walls) ends this.
- A person covered by a restriction has their stored policy rows clamped to the ceiling, grants included (unlisted
  pairings are dropped).
- A workspace pinned to an owning unit always runs in that unit's walls. A person with no membership on that unit's
  path cannot launch on it (`workspace_unit_not_yours`).
- The Default unit is read-only: `PUT /governance/units/<Default>` answers 409; change `WARDYN_DEFAULT_POLICY`. A
  membership can no longer be added to the Default unit or moved into it: `POST` and `PUT /governance/memberships`
  answer 409 `governance_membership_default_unit`.
- `POST /governance/dry-run` takes a `target` (`boundary`, `unit` or `membership_move`); a body with no target is read
  as a boundary dry run.
- Deleting an assignment that was winning for someone may let a lower one take over. With the second-human switch on,
  that delete is held and names the units that may become active.
- Unit admins: a security admin, or a unit admin of a unit above, can make a person or an IdP group a unit admin. A
  security admin's grant follows `WARDYN_GOVERNANCE_SECOND_HUMAN`, so a single-admin install can create unit admins; a
  unit admin's grant always waits for a second approver. A unit admin's narrowing applies at once and their widening
  always waits for a second approver. Locked settings bind every unit below the lock, narrowings included.
- The governance routes a unit admin can use now answer a member who is not one with 403 `unit_scope_denied`, where they
  used to answer the security-tier refusal.
- Split-role installs (`WARDYN_PG_MIGRATE_DSN` set) whose app role does not pick up new tables through the
  `ALTER DEFAULT PRIVILEGES` step in Operations: grant the app role `SELECT, INSERT, UPDATE, DELETE` on
  `governance_unit_admins` (migration `0228_governance_unit_admins`). Without it unit admins cannot be listed, granted
  or revoked. Migration `0226_governance_change_tree_ops` widens one CHECK and changes no row or grant.
- **Approval rules are off.** `WARDYN_APPROVAL_RULES` (new, default `off`) lets a security admin's
  approval rule approve a held tool call or a first-use egress host on a service identity's runs. Nothing changes until a
  deployment sets it. Set `dry_run` first: matching approvals then get an `approval.policy.would_approve` audit row and
  stay pending. A rule never decides a credential, `credential_reauth` or `push_content` approval, and never counts as a
  second human.
- Split-role installs (`WARDYN_PG_MIGRATE_DSN` set) whose app role does not pick up new tables through the
  `ALTER DEFAULT PRIVILEGES` step in Operations: grant the app role `SELECT, INSERT, UPDATE, DELETE` on
  `approval_rules` (migration `0224_approval_rules`). Without it approval rules cannot be listed, written or deleted,
  and a raise with `WARDYN_APPROVAL_RULES` set writes an `approval.policy.skip` row instead of matching a rule.
- **`WARDYN_AWS_SSO_PROXY_INJECT=off` now stops `wardynd` from starting.** That value used to write the AWS sign-in
  token into the sandbox. The token now stays at the proxy and the sandbox holds a placeholder. `off`, `false` and `0`
  are all refused, in the environment, in the `-aws-sso-proxy-inject` flag and in the Helm value `awsSSOProxyInject`.
  Unset it, or set it to `on`, before you start. Behind a TLS-intercepting corporate proxy that breaks the sign-in
  lane, point `WARDYN_TRUSTED_CA_FILE` at the proxy's CA instead.
- **The Go client's `CreatePolicy` and `UpdatePolicy` now return `PolicySaved`, not `types.RunPolicy`.** `PolicySaved`
  holds the policy as its `RunPolicy` field and adds `Requirements`. Code that assigns the result to a
  `types.RunPolicy` changes to read `.RunPolicy`.
- **Workspace names are now unique per owner, not per install.** Two owners may use the same name (migration
  `0192_workspace_name_per_owner` drops the install-wide unique constraint `workspaces_name_key`). A script that looks
  a workspace up by name alone can now match more than one. Reassigning a workspace to the operator when the operator
  already has one of that name is refused (409 `workspace_name_taken`). A SCIM purge renames such a workspace to
  `name (id-prefix)` and audits `renamed_to`.
- **The compose services drop `NET_RAW`, and the desktop tier no longer publishes ports 5432, 5010 and 8081 on the
  host.** Use `docker exec` or your own override. A custom image on those services that needs raw sockets adds
  `cap_add: [NET_RAW]`. Desktop packages need Docker Compose 2.24.4 or newer.
- **Rebuild the Linux VM guest image (`vm-linux`) with each runner upgrade.** A guest image older than the runner fails
  to boot, because its guest agent does not answer the runner's session handshake. Linux VM runs stay off unless
  `WARDYN_RUNNER_LINUX_VM=1` is set.
- **`WARDYN_ATTACH_LOCAL` is off by default.** Set it to `on` on `wardynd` and `wardyn attach <run-id>` keeps the
  terminal on this computer when the run is on your own runner here and the organisation allows it. The organisation
  still admits, records and audits the session. Left off, attach goes through the organisation as before.
- **`WARDYN_CLI_SIGN_IN` is off by default, and `wardyn login` needs it plus SSO.** Set it to `true` on `wardynd` and
  `wardyn login` signs a CLI in through the console. The CLI keeps a session secret on the laptop, in the keychain or
  a file only its owner can read, that renews an access token of at most an hour (shorter when
  `WARDYN_API_TOKEN_MAX_TTL` is lower). A computer with no browser signs in by pasting a code only when
  `WARDYN_CLI_PASTE_SIGN_IN=true` is also set.
- **`wardynd` refuses to start when a Kubernetes sandbox annotation sits in a container runtime's namespace.** A
  `podAnnotations` key in `WARDYN_K8S_SANDBOX_PLACEMENT` (`k8s.sandbox.podAnnotations` in the chart) that starts
  `io.katacontainers.`, `io.kubernetes.cri-o.`, `io.kubernetes.cri.`, `io.containerd.`, `dev.gvisor.` or `run.oci.`
  is refused, and the boot error names the key. Remove those annotations. To choose the runtime, pin the confinement
  class to a RuntimeClass (`k8s.runtimeClasses`, or `WARDYN_CONFINEMENT_MAP`).
- **`WARDYN_LOCAL_TRUST_FORWARDER` now trusts only the peers in `WARDYN_LOCAL_TRUSTED_PEERS`, not any non-loopback
  peer.** The compose stack ships it on. Unset, the trusted peers are the default route's gateway and `wardynd`'s own
  non-loopback addresses, and `wardynd` refuses to start when it finds neither. An install whose published-port
  traffic reaches `wardynd` from another address now gets a 403 from the local-mode peer check. Set
  `WARDYN_LOCAL_TRUSTED_PEERS` to that address (IPs or CIDRs, comma-separated).
- **With `WARDYN_GIT_PAT_BROKER=off`, a member's run that carries a `git_pat` grant is refused
  (`git_pat_broker_off_resident`).** The broker is on by default. Off, the token would sit in the sandbox, so only an
  administrator's run may hold it. An administrator's run is refused too when `WARDYN_GIT_PAT_BROKER_OFF_REFUSED` is
  on (off by default). Azure DevOps hosts, GitHub-brokered forges and `runner_resident` grants are not affected. Turn
  the broker back on for members.
- **Commands and sftp sessions over the SSH gateway start in your home directory, not the image's working directory
  (#3549).** A script that runs `ssh <run-id> <command>` with a relative path, or copies with `scp` or `sftp` to a
  relative path, now resolves it from `$HOME` (`/home/agent` on Wardyn's images). Use an absolute path, such as
  `/home/agent/work/...`, to keep the old place. `wardyn sync` and `wardyn-sync` still use the directory they name.
- **`wardyn runner install` and `wardyn runner upgrade` check the release's Sigstore bundle (`SHA256SUMS.sigstore.json`) before installing anything.** This is a clean break: a CLI from this release refuses a release published before 0.10.0, because those carry no bundle. A mirror must serve `SHA256SUMS.sigstore.json` beside `SHA256SUMS`.
- During a rolling upgrade, a person may be asked to sign in to AWS again once.
- A person set up by Entra object id who has already signed in keeps that principal, even if items later appear under
  their raw sign-in id (their pairwise `sub`). Before, that next sign-in was refused (`sign_in_refused`) because the
  identity was already bound to the person.
- `WARDYN_RUNNER_SECRET_PLACEHOLDERS` is new and off by default, so nothing changes until you set it to `true`. Then a
  `runner_resident` `env_secret` or `file_secret` grant whose scope names a `host` is no longer delivered to the
  runner: the sandbox holds `wardyn-placeholder:<id>`, and requests to that host go through the organisation's egress,
  which fills the placeholder in the `Authorization` header. Those requests then leave from the organisation's
  network, not the laptop's, so a host only the laptop can reach stops working for that grant. A grant without a
  `host`, a host that is not an exact allowlist entry or already carries a credential, and a value that spans lines
  are delivered to the runner as before. Without `WARDYN_PROXY_RESPONSE_SCRUB`, a host that echoes the value can
  return it to the sandbox. No runner upgrade is needed.

### Added

- `GET` and `PUT /api/v1/governance/recording`, on the security tier, read and set the organisation's recording level
  and whether it is a locked minimum, the Wardyn boundary's recording row (#2967). The `PUT` writes that block and no
  other field, is held for a second human under `WARDYN_GOVERNANCE_SECOND_HUMAN`, and is audited as
  `governance.recording.write`.
- A Linux VM's desktop keeps sign-ins in the machine's memory, gone when it shuts down, and the run page says so (#2972).
  "Pause the agent to sign in" stops the agent's processes inside the guest while the desktop stays live; "Resume the
  agent" continues them, and each pause-to-resume is one `desktop.sign_in.pause` audit row. On a recorded desktop the
  pause holds the screen recording on every replica within 2 seconds, a desktop that connects after a `wardynd`
  restart records nothing while the sign-in is open, and a desktop whose sign-in state cannot be read is refused rather
  than recorded, or, already open, keeps no frames until it is read. The run page says plainly that
  the resumed agent can use those sign-ins and read saved files. An admin can set an agent's "Sign-ins in a desktop" to
  Not allowed: its desktops keep none, and the pause is refused at the server.
- `GET /api/v1/runs/{id}/desktop/sign-ins` reads a VM desktop's sign-in store (`memory` or `none`) and whether its agent
  is paused; `POST` to `.../pause` and `.../resume` stop and continue the agent for the person who may enter the run's
  desktop, while it runs. Reasons: `desktop_sign_ins_off` (`403`, the agent is set Not allowed),
  `desktop_sign_ins_unavailable` (the run has no store, because its runner or guest image predates sign-ins, or the
  store could not be read), `desktop_sign_ins_pause_failed` (the agent keeps running) and
  `desktop_sign_ins_resume_failed` (it is still paused; try again). A failed pause or resume writes a failed
  `desktop.sign_in.pause` row (#2972).
- A Linux VM sandbox on a Docker runner now has a network: a relay (`wardyn-vmnet`, built into the proxy image) on the
  run's internal network hands connections to allowed hosts to the run's proxy and refuses raw IP addresses, outside DNS
  resolvers and IPv6. The reference guest image addresses its NIC for the relay. Kubernetes VM pods are unchanged.
- A Linux VM sandbox gets the same folders a container on its runner would: a member's folder under the runner's roots is shared into the guest over virtiofs at the same path and mode, and a folder synced from another computer arrives through the guest agent. A runner without folder sharing refuses such a run at create (`placement_capability`).
- Approval rules (migration `0224_approval_rules`, off unless `WARDYN_APPROVAL_RULES` is `dry_run` or `on`): a security
  admin's rule approves a held `tool_call` or first-use `egress_domain` approval on a service identity's run, through the
  same decide path and gates a person's approval meets, audited as `policy:<rule_id>` with the rule's version, digest and
  what matched. `/governance/approval-rules` lists, writes, deletes and dry-runs rules; see
  [approval-rules.md](docs/operations/approval-rules.md). A `policy:tool-allow` or `policy:tool-deny` egress row now names
  the `tool_rules` key that answered in `matched_rule`.
- `wardyn attach <run-id>` keeps the terminal on this computer when the run is on your own runner here and the
  organisation allows it (`WARDYN_ATTACH_LOCAL`, off by default); otherwise it goes through the organisation, as
  before. One line on stderr names the path. `--via local|relay|auto` forces a path, and `--take-over` takes the
  terminal from whoever holds it. On Windows the runner lives in WSL2, so attach goes through the organisation.
- CLI sign-in, server half (migration `0199_cli_sign_in`), off unless `WARDYN_CLI_SIGN_IN=true` and SSO is set up.
  `wardyn login` asks for a sign-in, the person approves the computer in the console, and the CLI gets a session
  secret, bound to an Ed25519 device key, that renews an access token of at most an hour (shorter when
  `WARDYN_API_TOKEN_MAX_TTL` is lower). The secret DOES live on the laptop, in the keychain or a file only its owner
  can read; [THREAT-MODEL.md](threatmodel/THREAT-MODEL.md) says what protects
  it and what it does not stop.
  - Sessions last 30 days (`WARDYN_CLI_SESSION_TTL`, up to 90), end after 7 days idle (`WARDYN_CLI_SESSION_IDLE`),
    and at most 7 days in a file (`WARDYN_CLI_FILE_STORE=false` refuses the file). Paste-back sign-in for a computer
    with no browser is off unless `WARDYN_CLI_PASTE_SIGN_IN=true`.
  - Every renewal rotates the secret and re-checks revokes, sessions cuts, deactivation, the role stamp and the
    `api_token` feature. An old secret presented again ends the session. A revoke, Sign out everywhere, a console
    sign-out and a per-computer sign-out end it at once, with its tokens.
  - A renewed token carries the role, groups and role-stamp age of the person's last console sign-in, so a renewal
    never resets the `WARDYN_ROLE_STAMP_TTL` clock.
  - An admin's CLI acts as role `user`; `wardyn login --admin` gives a 1-hour admin token, never renewed.
  - Routes: `POST /api/v1/cli/sign-ins`, `POST /api/v1/cli/sign-ins/{id}/token`, `POST /api/v1/cli/sessions/{id}/refresh`,
    `GET /api/v1/me/cli-sign-ins/{id}` with `/approve` and `/deny`, `GET /api/v1/me/cli-sessions` and
    `DELETE /api/v1/me/cli-sessions/{id}`. `/healthz` reports `cli_sign_in` and `cli_paste_sign_in`.
  - Audit actions `cli.sign_in.request`, `cli.sign_in.approve`, `cli.sign_in.deny`, `cli.session.create`,
    `cli.session.revoke`, `cli.session.refresh_refused` and `cli.session.reuse_detected`. `token.create` rows carry
    `kind`; `session.revoke` rows carry `cli_sessions_ended`.
- The runner's managed-settings lock. A macOS managed profile that sets `lock_security_settings` now outranks a flag for `OrgURL`, `ProxyImage`, `TrustedCAFile`, `OrgCAFile` and `RecordingStore`, and `wardyn-runnerd serve` refuses to start on a flag or environment variable that disagrees, naming the setting. It is off by default, so nothing changes until an administrator sets it.
- Recording is a level: a policy's `recording` is `off`, `terminal` or `screen_actions`, and
  `site_config.recording` sets the organisation's level and can lock it as a minimum. Nothing set records terminal
  sessions, as before. `WARDYN_RECORDING_STORE=off` still records nothing, and keystrokes are never recorded.
- Replay a recorded desktop session on the run's Recording tab: the screen is drawn with noVNC at the playhead, the
  pauses show as gaps on the timeline (with a 1x, 2x or 4x play clock and "Skip the gap" or "Show it on the timeline"),
  the action log jumps to its moment, and the tamper-evident log's verdict for the recording's range is shown beside it
  (#2968). `GET /api/v1/runs/{id}/recording-chain?key=&from=` checks the audit chain over that range for whoever may
  read the recording: `ok: false` names the broken sequence, `400` `recording_chain_key_required` has no `key`, and
  `503` `recording_chain_busy` (with `Retry-After`) means another check is walking the log on that replica. The console
  bundles `@novnc/novnc` 1.7.0, unmodified, under MPL-2.0; [THIRD-PARTY-NOTICES.md](THIRD-PARTY-NOTICES.md) lists it
  and the `pako` code inside it.
- The Wardyn boundary is the outermost layer of every run's policy, with a dry run and break-glass. A security admin
  reads and writes it at `GET`/`PUT /api/v1/governance/boundary` and previews a draft with
  `POST /api/v1/governance/dry-run`, which writes nothing and lists what the boundary would narrow, knob by knob.
  Both `no_limits` and `hard_limits` can be set, and the console shows the dry run before every save. A fresh install
  starts in `hard_limits` with nothing listed and floor `CC1`, which narrows nothing; an upgraded install stays in
  `no_limits`. Break-glass (`wardyn run --break-glass-reason`) is asked of an admin-token run only once the boundary
  bounds something and a unit with its own walls or a restriction exists.
- GET /governance/boundary carries optional `save_held_for_approval`, true when the caller's save would be held for a second admin.
- Restrictions are deny-only lists (domains, run limits, withheld credential kinds) that follow users, groups, user
  types or everyone into every unit they run in. A workspace can be pinned to an owning unit
  (`PUT /workspaces/{id}/owning-unit`, `wardyn workspace set-unit`).
- A person or an IdP group can be granted as a unit admin (`POST /governance/units/{id}/admins`,
  `DELETE /governance/units/{id}/admins/{admin}`, `wardyn governance admins add|remove`). A unit admin governs that
  unit and every unit below it and nothing else: the unit and membership writes, `GET /governance`, the check, the
  dry run and the held changes now check the target against their units, and `GET /me` gains `unit_admin_of`. A unit
  admin's narrowing applies at once and their widening is held for a second approver. A unit's `locked_knobs` fix a
  knob for every unit below (409 `unit_knob_locked`). Audit actions are `governance.unit_admin.grant` and
  `governance.unit_admin.revoke`; see [unit-admins.md](docs/operations/unit-admins.md).
- `locked_knobs` can fix each desktop direction on its own: `desktop.paste_in`, `desktop.copy_out`, `desktop.upload` and
  `desktop.download` (`desktop` alone fixes all four). The boundary's Desktop card gains "Lock for teams below" on each
  row and "Lock the group for teams below", and its dry run lists the drafted locks. A team below a locked direction
  cannot change it (409 `unit_knob_locked`), and the boundary's own locks stay as they are unless the page changes them
  (#2967).
- A run can pick which of its person's units to run in: `run_in` on `POST /runs`, preflight and the policy preview, and
  `wardyn run --run-in`. A unit the person is not in and an unknown unit both answer 403 `unit_not_a_context`.
  `PUT /governance/memberships/{id}` with a `unit_id` moves a membership and
  `POST /governance/memberships/{id}/promote` makes a legacy membership a context; both follow the second-human
  switch. `POST /governance/check` lists the person's contexts with their walls. `POST /governance/dry-run` takes a
  `target` (`boundary`, `unit` or `membership_move`). `wardyn governance check` and `wardyn governance dry-run` follow.
- `POST /governance/check` and `GET /governance` name, for a security admin, in `path_provenance`, the unit on the path
  that changed each knob. A restated value or a clamped-away widening stays the parent's, and a pass-through unit is never named.
- Policy field `harness_inner_walls` (`sandbox_mode`, `approvals`) chooses the harness's own walls inside Wardyn's.
  Omitted, each run keeps what its harness and lane give today. It only adds walls: a looser choice refuses the run
  (`harness_inner_walls_looser`). A run whose policy sets it needs the proxy image from this release.
  - `sandbox_mode` is `off`, `read-only` or `workspace-write`. A mode launches a Codex run inside Codex's own sandbox:
    Wardyn delivers `/etc/codex/requirements.toml`, which the agent cannot rewrite and which binds every `codex` in the
    sandbox to the mode and refuses its bypass flag, and sets `WARDYN_HARNESS_SANDBOX` for the image's `agent-run`. It
    starts at CC3 in both modes and at CC2 only for `workspace-write` on a background run; any other class and mode is
    refused `harness_sandbox_unavailable` at create. A runner that cannot place the file where the agent cannot rewrite
    it (no managed files, krun) fails the run. A mode on a run that is not a Codex run, or whose agent is a custom image
    or an exec run, is refused `harness_inner_walls_unmet`.
  - `approvals: harness` on a seeded interactive run turns `seed_auto_tools` off and says so.
- An `ssh_key` grant clones through the run's proxy, which holds the key and signs SSH sign-in requests only. The key
  file is not placed in the sandbox, but any process there can have a sign-in signed until the agent closes (the
  `claude-code` image closes it before the agent starts). There is no switch. A custom image that mints the key
  itself now gets a 403; use `wardyn-git-helper ssh-exec`.
- `WARDYN_AWS_SIGV4_AT_PROXY` is `off` by default. Set it to `on` and an AWS sign-in lane whose hosts the proxy can
  all re-sign gets a placeholder key pair in the sandbox, and the proxy signs each AWS request with role credentials
  the sandbox never holds. Other lanes keep their old path. New dispatches only.
- `WARDYN_EGRESS_PORT_RULE` is `off` by default. `dry_run` refuses nothing and marks each allow the rule would refuse
  with `port_rule: "dry_run"` on its `egress.allow` row. `on` limits a bare allowlisted host, a wildcard, allow-all
  egress and a first-use approval to ports 80 and 443; another port needs an entry that names it
  (`db.example.com:5432`). The refusal is a 403 with `rule_source` `policy:port`. A run dispatched as a desktop VM
  always has it on. It needs the proxy image from this release.
- `WARDYN_PROXY_RESPONSE_SCRUB` is `false` by default. Set it to `true` and each run's proxy replaces a credential it
  added with `<secret-hidden>` when a response echoes it, in headers and body.
- `WARDYN_DOCKER_PROXY_SANDBOX_PEERS_ONLY` is on by default. Each run's proxy on the Docker substrate serves only
  callers on the run's own network and closes others before reading a byte. Set it to `false` to keep the old
  behaviour (any peer); boot logs it.
- `WARDYN_ENV_SECRET_PLACEHOLDERS` and `WARDYN_FILE_SECRET_PLACEHOLDERS` are `false` by default. Set one to `true` and
  an `env_secret` (or `file_secret`) grant whose scope names a `host` puts `wardyn-placeholder:<id>` in the variable
  (or file), and the run's proxy fills the `Authorization` header to that host. Off, the value is placed in the
  sandbox for the whole run, as before.
- `WARDYN_GIT_PAT_BROKER_OFF_REFUSED` is `false` by default. Set it to `true` and, while `WARDYN_GIT_PAT_BROKER` is
  `off`, an administrator's run that would hold a `git_pat` token in the sandbox is refused too. A member's run is
  refused either way.
- `WARDYN_EGRESS_HOST_PORT_GRANTS_INERT` is on by default. A stored `egress_host` allow grant whose value carries a
  port is inert: `GET /api/v1/permissions` marks it and it grants nothing. Set it to `false` to keep the old behaviour,
  and listing grants warns on each such grant; boot logs it.
- `WARDYN_GOVERNANCE_UNIT_PUT_EXISTING_ONLY` is on by default. A `PUT /api/v1/governance/units/{id}` naming an id no
  unit holds answers `404 governance_unit_not_found`; create with `POST /api/v1/governance/units`. Set it to `false`
  to keep the old behaviour, and each create by PUT is logged; boot logs it.
- `WARDYN_CAPABILITY_GRANTS_VERIFIED_EMAIL` is on by default. An allow grant on a caller's email matches only when
  sign-in vouches for the email; deny grants bind either way. Set it to `false` to keep the old behaviour, and each
  match on an unvouched email is logged at INFO; boot logs it.
- `WARDYN_COMPUTER_FOLDERS` is `false` by default. Set it to `true` and a run can take a folder from its creator's
  own folder-only runner (`wardyn-runnerd serve --substrate folders`). Off, New Run shows "Your computer" as blocked
  and `local_folder.runner_id` is refused with `computer_folders_off`.
- `WARDYN_OIDC_BACKCHANNEL_LOGOUT` is `false` by default. Set it to `true` and `POST /auth/backchannel-logout` accepts
  a logout token your identity provider signed and ends that person's console sessions. Off, the route does not
  exist.
- A `local_model` model provider (#2957) is a model server you run on a laptop's host or an organisation node, such as
  Ollama, a llama.cpp server, LM Studio or vLLM. Add it in Settings › Model providers (Local model server) or through
  `/api/v1/model-providers`. A run reaches it only through its proxy's brokered LLM route: `base_url` is `http://` or
  `https://` with no path or credential and not a loopback address, and `local.models` (1 to 64 exact names, tag
  included, none with a `/` or a `:cloud` or `-cloud` tag) is the allowlist the request's `model` must be on. Other
  methods and paths are refused `403 egress_denied`, a model inventory is filtered to the allowlist, and
  `local.max_context`, `local.max_output` and `local.keep_alive_seconds` cap those request fields when a request
  supplies them (unset caps mean 32768, 32768 and 1800 seconds); a request that omits one keeps the model server's own
  default. A run never pulls, pushes, creates, copies or deletes a model, and the prompt is never rewritten. The
  proxy lifts its private-address guard for the registered host and port alone; every other lane refuses that host.
  With no provider of this kind, nothing changes. Move the proxy image pin with `wardynd`, image first, before adding
  one: an older proxy refuses the `local_models` key it writes.
- A `local_model` provider can set `local.allow_on_laptop_runners` (off by default; the console's local model server
  editor has a switch for it): a run on a person's own Docker runner then uses the model server on that computer, with
  `base_url` on `host.docker.internal` (any other host is refused at write). The runner relays that one port to its
  loopback, only to the proxies of flagged runs and only while one is going, and the run's proxy admits only that
  address and keeps the route table, model allowlist and caps; management routes stay refused (#2957). The person
  allows the port on their own runner with `wardyn-runnerd serve --local-model-port` (`local_model_port` in
  `runnerd.json`, `WARDYN_LOCAL_MODEL_PORT`, or the `LocalModelPort` managed preference); unset allows none, and a run
  on another port is refused at create with `runner_local_model_port_not_allowed`. An older wardyn-runnerd or a
  Kubernetes runner refuses it with `runner_proxy_setting_unsupported` naming `local_model_relay`, and the
  organisation's own runner refuses it as before. On native Linux Docker, bind the model server to `127.0.0.1`: a
  server bound to every address already holds the port on the bridge, and the run is refused at create.

- A runner that could not connect, or could not start, reports why when it next connects: the first and last failure,
  the stage and a redacted reason. The runner's read shows it as `offline_summary`, and each report writes a
  `runner.offline_summary.record` audit row. A refusal at start is shown only once a later start connects.
- A Docker runner checks its relay on every reconnect and every 5 minutes, logs `run relay unreachable` when the relay
  stops answering, and reports it as `relay_status` on the runner's read.
- A runner's `status.json` now records the stage a failed connect or start stopped at (`stage`, `stage_at`) and whether
  the run relay answers (`relay`), so the file shows why the runner is down (Refs #3296).
- A service token reads its own runs' audit, the presets and workspaces it may launch with, and its reach on
  `GET /me`.

### Changed

- Commands and sftp sessions over the SSH gateway now start in your home directory, as OpenSSH does, not in the
  image's working directory, so a bare `mutagen sync create` works without a `.mutagen` link in the sandbox (#3549).
  This is a clean break (see "Before you upgrade").
- A run that needs a proxy setting its runner cannot honour is refused before its sandbox starts, naming the setting
  (reason `runner_proxy_setting_unsupported`).
- The CLI's hint under a `401` follows the answer's reason. On a single-sign-on deployment with no admin token
  (`sign_in_required`) it says to use a personal API token in `WARDYN_TOKEN`, not the compose stack's admin token. For
  a token the organisation does not know (`admin_token_not_configured`) it names where the token came from.
- Under a `hard_limits` boundary that bounds something, admin-token runs are break-glass runs once any restriction
  exists, as for a unit with its own walls (see "Before you upgrade").
- On a phone, the Runs filter bar is a search box, a `Filters (n)` button and a row of removable chips, so the first run
  is on the first screen instead of under five rows of controls (#3041). The button opens a sheet with every other
  control and Save view. From the tablet breakpoint up the bar is unchanged.
- The security tier's audit read no longer shows a drive's host root. Below a super admin, `GET /audit` and both
  exports withhold, per action: a drive's host root and storage class, a source's path, the recovery rehearsal's
  database target, the site config's logo path, internal hosts, upstream proxy URL and egress redirects, a redirect
  probe's target, and each workspace mount's host path in a policy spec, a policy write or delete and a launch preset.
  Each such row names what was withheld in `redacted_fields` and keeps its stored hashes, so the chain still links
  across it, and a partition export's footer counts those rows (`redacted_rows`). A super admin's read is unchanged,
  and so are the operator's sinks.
- Runner settings are in one file, `runnerd.json` in the user config directory (mode 0600), and the service
  definition carries only the binary and that file's path. Setup no longer writes a proxy image the organisation
  named: that image, like the release image, is derived again at each start, so replacing the binary moves the proxy
  with it. A `proxy_image` set in the file, by `--proxy-image` or by hand, is the pin. An earlier install keeps
  working, and running setup again moves its settings into the file.
- A service token's `GET /me` answers 503 `roster_unreadable` when the site configuration cannot be read, as
  `GET /workspaces` does, and 500 when its presets or workspaces cannot be listed.
- An address the console does not know now shows a "Page not found" card at the typed address, with "Back to Runs",
  instead of redirecting to Runs (#2884). Under `/admin` the button returns to the admin Runs list.
- Setup shows the full step rail, 200 px wide, from a 1024 px window up, not an icon-only rail (#2844). The "Host
  status" bar is gone: one "Re-check" sits at the right end of every step's heading row (#2847).
- A disabled filled button (primary, destructive, info) turns muted grey in the dark theme, where opacity alone left it
  reading as live (#2865).
- On a run in focus mode, the approvals that hold the run have their own row under the session, outside the dock panel,
  and stay open and keep their in-flight Approve or Deny across entering and leaving focus. The dock, the request row
  and the strip fit a phone's width (#2912).
- A resolve of an Azure DevOps token served from the token cache no longer reads the stored sign-in, so it writes no
  `ado-refresh` `secret.read` row; the injected token's own `secret.read` row is still written. Each redemption that
  fills the cache reads the sign-in and records `secret.read` with purpose `ado-refresh` (#2140). A
  cached token is now served for at most one hour after the redemption that filled it, whatever lifetime the authority
  gave it, and never within ten minutes of its expiry.

### Fixed

- A run create that needs one AWS SSO credential no longer waits out the renewal of another of the same person's (a
  `bedrock_sso` provider, the AWS CLI session), up to about 21 seconds, before it serves the token it had. Renewals now
  hold a lock per stored credential, not per person (#2137). A renewal the authority refuses (`invalid_grant`) after
  another writer redeemed the same refresh token first runs once more on that writer's pair, which usually serves it
  instead of asking the person to sign in again.
- A desktop recording's gap no longer ends early when two take overs, or a take over and a desktop sign-in, overlap on
  one run: each holds the gap open until its own end. A hand back or a sign-in's end on another replica now resumes the
  recording on the replica that holds it, and a take over's gap still open when the run's last desktop connection
  closes after the run ended gets its `desktop.recording.resume` row.
- A take over keeps the person's screen out of the recording on every replica and after a restart: a desktop opened
  while a take over is open keeps no frames until the hand back, and one whose take over cannot be read is refused.
- Sign-in and probe runs under a boundary (#3446): a member whose profile denies every placement, under a hard-limits
  boundary, can again run the sign-in and probe runs the placement rule always exempted (it answered 500).
- The Approvals page cards and the run's Approvals tab now turn Approve and Deny off, and say why, once the server refuses a person with `capability_egress_host` (an admin withholds that host). Before, they kept live buttons and only showed an error after each click. The live approvals strip already did this.
- A sign-in sandbox whose token is pasted before the sandbox is running now ends once the token is stored. Before, the
  launch could move the run on between the server's read of it and the kill, and the sandbox stayed up until its
  30-minute idle cap. That sandbox never held the pasted token.
- A run's recorded output is no longer lost after the organisation's server restarts when the runner hears of the
  restart only after the server has begun reading that output back. Before, a run writing a lot of output at that
  moment could lose the part not yet read back.
- Building the wardynd image no longer waits without limit on a stalled npm registry (#3472). Installing pnpm and
  fetching the console's packages each get three tries with a time limit, so an unreachable registry fails the build
  in about ten minutes, and a package fetch that stalls fails it in under twenty. The lockfile is still enforced.
- `make setup` no longer stops at the health wait on hosts whose /bin/sh is dash (Debian, Ubuntu, WSL Ubuntu).
- Creating a run checks the workspace grant for every attached workspace, not only `workspace_id`. Preflight and the
  policy preview answer the same.
- Getting started no longer counts a service identity's run as your first run (#2513). `GET /api/v1/runs` takes
  `sponsored=exclude`, which turns off only the addition of the runs of service identities you sponsor to your own list;
  the page sends it, and the Runs list still shows those runs.
- A computer folder's pass back no longer uses up the time a run's teardown has (#3466). On Docker, every pass back
  runs with the agent stopped and no egress: only the folder pass back runs. If the agent cannot be stopped first, no
  pass runs, and the sandbox is stopped with the record. That includes an exec-less (krun) sandbox, whose agent is the
  sandbox itself, and one a lease end has already stopped. A pass that completes, or that has no time to start, is
  followed by the sandbox's stop. A pass that is cut short keeps the sandbox on Docker, and a later sweep resumes it
  from what the folder's runner saved. On a runner, or on Kubernetes, the first pass runs with the agent still up, for
  at most the pass's 2 minutes, and a cut pass stops the sandbox. After two cut passes in a row that save nothing, the
  sandbox is stopped, and the pass's record and its `run.folder.transfer` audit name how many changed files were not
  brought back. `wardyn sync` also saves what a cut pass finished, so its next pass starts after it.
- `wardyn sync --pull-deletes` no longer moves a laptop file to the Trash because its name holds a backslash or a
  control character. Such a laptop file is now refused, with a report row, as not a plain file name instead of pushed,
  and never counts as deleted.
- `wardyn runner upgrade` that rolls back to a 0.9 runner no longer fails putting it back: `--org-ca-file` is passed only
  to a runner that has it. The rollback also puts the old runner's service files back as they were, so it keeps its
  trusted CA file and image pin.
- A run whose desktop is swapped to the sign-in image records that image on the run, not the one it asked for.
- The Recording tab and the take over strip say "Recording off" for a run its policy, its organisation or its runner did
  not record, instead of naming a level. A take over on a run whose runner declined still says it is recorded: the
  console and SSH sessions it opens are.
