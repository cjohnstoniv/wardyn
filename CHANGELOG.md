# Changelog

All notable changes to Wardyn are documented here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/); Wardyn is **pre-alpha**
and does not yet follow semantic versioning (interfaces are not stable).

This file holds `[Unreleased]` and the newest released minor line. Older releases, newest first:
[0.8](docs/releases/0.8.md), [0.7](docs/releases/0.7.md), [0.6](docs/releases/0.6.md),
[0.5](docs/releases/0.5.md) and [0.4](docs/releases/0.4.md). **v0.3.1 and older live in
[CHANGELOG-ARCHIVE.md](CHANGELOG-ARCHIVE.md).**

## [Unreleased]

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

- macOS virtual machines (preview, off by default). A Mac on macOS 27 or later can run up to two macOS VM sandboxes,
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

## [0.9.8] — 2026-10-21

### Before you upgrade

- A member's local folder mount is now refused when its path is, or sits inside, `.codex`, `.azure`, `.gemini`, `.npmrc`, `.pypirc` or `.config/gcloud` (any letter case), as it already was for `.ssh`, `.aws` and the other credential paths. A folder that only contains one of them is still allowed. To mount that work, use a folder outside those paths. The same list applies to an admin-made `host_path` drive, which is checked at every launch: an existing drive whose `host_root` is, or sits inside, one of those paths stops binding at launch until an admin moves its `host_root` out of that path.

### Added

- `GET /governance/credential-delivery` lists, as `unbuildable`, each stored row (a class, or a class under a trust class) whose mode the server cannot build for it, with the modes it can; the field is absent when there are none. The setup check `credential_delivery` warns and names each, for an operator. Nothing is changed: the document is read, and those rows refuse as before.

### Fixed

- A request refused because its host resolved into private address space that `internal_hosts` can lift now names the remedy: `an operator may lift this guard for <host> with site_config.internal_hosts, scoped to <range>`, with the range block (such as `100.64.0.0/10`) and never the address. The brokered git lanes, a CONNECT, a plain HTTP request and the refusal's audit row all carry it, so a self-hosted runner whose corporate resolver answers a git provider host into CGNAT or RFC1918 no longer fails with no way forward. A new `private_ip_refusals` setup check lists such refusals from the last 7 days. Nothing is lifted by default and no reach changes.
- A queued run kill is no longer lost when 100 or more stale kills are waiting ahead of it. Before, it was never re-issued and the run kept running.
- A full runner pool pass no longer fails with a Postgres deadlock when it updates the queue wait of waiting runs while another writer holds some of the same runs (part of #3369).
- An egress approval that expires or is cancelled with nobody deciding it is no longer remembered as a "no" for the rest of the run. The request that sees it is refused as `approval:expired`, and the next request asks again.
- The approval poll a sandbox makes no longer returns the approver's name or note (#2434).
- The AWS CLI's derived role credentials are kept on the sandbox's temporary disk, so a kept Docker sandbox no longer holds them after the run ends.
- A member's local folder mount that is refused now gets one sentence that does not say whether a host path exists or where a link points (#2273).
- A sign-in sandbox no longer keeps the new credential until it is stopped: the AWS caches and the pane's history are cleared once the credential is stored, and the Claude paste sandbox is stopped when the capture is stored.
- A runner no longer hangs at start when the container engine is stuck while it sets aside an earlier registration's runs.
- An allowlist host written with a port or a trailing dot, such as `github.com:443`, is judged by its host, so it is no longer graded as open egress when the host is on the safe list.
- Erasing a person now removes their name from launch presets they wrote and reports the token history it deleted (#3009).
- The docs no longer say a workspace's `byo` base image is used verbatim. `registry`, `custom` and `byo` base images are all wrapped with Wardyn's runner tools by the image builder before a run uses them, and the wrapped image runs on the Docker substrate only.
- A run on a workspace whose base image needs the image builder, on a control plane with none, is still refused `image_builder_unavailable`. The refusal now names the image and what works instead: an admin wires the builder, or sets the workspace's base image to recommended so a command that needs no model runs on the stock agent image (or the workspace's convention image where a builder is wired).
- A background command that includes no harness, on a workspace with no base image of its own, now runs, from the stock agent image (or the workspace's convention image where a builder is wired). It was refused `workspace_seed_no_base_image` with advice to pass `--image` or `--agent`, which a run-mode command cannot carry. It evaluates no model provider and gets no model credential.

### Security

- The console's build tooling moves to vite 6.4.4, which carries the fixed launch-editor (GHSA-587h-7wmj-h5pm, GHSA-r825-w8mm-5pxw, GHSA-698v-4r3x-3vcv). Those flaws are in the development server's open-in-editor route; the built console Wardyn ships does not include it.

## [0.9.7] — 2026-10-20

A small release on top of 0.9.6. An Azure DevOps token that the organisation mints for a run can now be delivered to a person's own runner, off by default, and a credential delivery write that the server could not build is refused when it is made. No migration.

### Security

- **Go 1.26.10.** The toolchain pin moves from go1.26.9, fixing a `crypto/x509` certificate verification advisory (GO-2026-6809). `make govulncheck` reports none.

### Added

- An Azure DevOps connection with `token_mode: minted_pat` can now run on a person's own runner. Before, the run was refused at create. It is off by default and needs all three: a delivery policy that sends `ado_minted_pat` as `runner_resident` to the runner's trust class (a security admin's change, held for a second person under `WARDYN_GOVERNANCE_SECOND_HUMAN`), the connection's `entra.runner_resident` opt-in, and a runner of that trust class owned by a person.
  - The token is held in the memory of the runner's proxy, on the person's own computer, for the run and at most `pat_max_hours`. It is not written to disk or the keystore. Root on that computer can read it.
  - The sandbox holds only an inert placeholder; the proxy adds the token to requests on the way out, so a host that echoes request headers can return it. `entra.runner_resident_sandbox` has no effect in this version: it is stored and audited, no run reads it, and on or off the sandbox sees the same.
  - The proxy fetches the token again at most 15 minutes apart, and each fetch is judged against the policy, the runner's owner, trust class and posture, and the opt-in. A delivery policy may add `require_posture` (`mdm_managed`, `disk_encrypted`, `os_min`) for the class; the class has none of its own. A refusal revokes the run's token at Azure DevOps. If the organisation is unreachable, the proxy stops injecting the token at most 30 minutes after its last fetch.
  - A `bearer` connection and a stored git token for an Azure DevOps host are still refused on a person's own runner.

### Changed

- `PUT /governance/credential-delivery` refuses with `422` and `credential_delivery_invalid` a document that sets a class, or a trust class's override of it, to a mode the server cannot build for that class. The sentence names the class, the mode and the modes it can build. `GET` lists them as `buildable_modes`. A stored document is read back as before, and a save that leaves a stored row as it is is not refused for it.
- The Runner credentials screen disables a delivery mode the server cannot build, labelled "Not available yet".
- The refusal of an API-token mint in the user view now says how to switch to Admin view.
- The Azure DevOps connection editor says what the runner switch does, and that the sandbox switch has no effect in this version. A runner that reports no posture fails every posture requirement, and the refusal says so.

### Before you upgrade

- A credential-delivery write that sets a mode the server can't build is now refused (`credential_delivery_invalid`); a stored document is left as it is.

## [0.9.6] — 2026-10-20

A fix release on top of 0.9.5. A person's own Azure DevOps token now works on their own runner, credential and posture refusals say which policy or field refused, the credential delivery screen lists what each class can be delivered by, runners that cannot reach the server say why, and session recordings no longer store typed input. No migration and no new setting.

### Security

- Session recordings no longer store typed input. Recordings made by earlier versions may contain keystrokes typed into an attached terminal, including at prompts that hide what you type, such as passwords. Treat those recordings as sensitive: restrict who can read them, or delete them.

### Fixed

- An Azure DevOps connection using a person's own token (`own_pat`) now works on that person's own runner. Before, the run failed at dispatch. The token reaches the runner's proxy only; the sandbox holds a placeholder. Organisation-held Azure DevOps tokens (`minted_pat`, `bearer`) are still refused on a person's own runner.
- The AWS sign-in dialog's step list moves to "Waiting for AWS" as soon as the dialog shows a device code. Before, it could stay on "Starting the sign-in sandbox" while the code and the waiting prompt were already on screen.
- A brief server error while the console first loads no longer leaves a signed-in person on the sign-in screen.
- A failed `run.output.finalize` audit row from the recording path now carries `step`, as every other finalize failure row does. No existing key changes.

### Changed

- A refusal of a credential on a person's own runner names the delivery policy that refused it, for example `(delivery policy: refuse)`. A runner of unknown trust class is told it gets organisation delivery only. The reason code and status are unchanged (`placement_credential`, 422).
- `runner_posture_unmet` names every posture field that is not met, with the requirement. For a runner that has not reported its posture, it says that an unreported posture meets no requirement. The reason code and status are unchanged.

- A runner that registered but has never connected to the server says so in the console, with where to find its log on each system.
- `wardyn-runnerd` logs the proxy it reaches the server through, by host and port only, and a connection the proxy refuses is reported as a proxy refusal.
- `wardyn-runnerd setup` warns when proxy variables are set in the shell but will not reach the service it installs. It prints the variable names only.
- `wardyn-runnerd` warns about certificates in its trusted CA file that are expired or not a CA, and about a file that another user owns or others can write. The file loads as before.
- On a single sign-on deployment, the CLI's answer to a 401 points at a personal API token.
- `wardyn-runnerd` warns when its pinned proxy image is a different release than the runner, or the organisation names a different one, since runs may then fail to start.

### Added

- `GET /governance/credential-delivery` returns `buildable_modes` beside the policy: the delivery modes each credential class can use today. The stored policy and `PUT` are unchanged.

### Before you upgrade

- Scripts that match the old refusal text for a local credential refusal, or a single posture field before the colon, must match the new sentences. Reason codes are unchanged.

## [0.9.5] — 2026-10-19

A small fix release on top of 0.9.4 for the sign-in and setup paths: the AWS sign-in dialog shows its code and moves
on as the run starts, sign-in refusals say why, `wardyn setup status` points at the right console page, and failed
teardown audit rows name the step that failed. No migration and no new setting.

### Before you upgrade

- **Two `401` answers carry a new reason.** A session cookie of a person whose identity was deactivated now gets
  `identity_deactivated`. On a single-sign-on deployment with no admin token, a request with no session now gets
  `sign_in_required`. Both used to get `admin_token_not_configured`, which described the deployment's admin token.
  A deployment with neither single sign-on nor an admin token keeps `admin_token_not_configured`. A client that keys
  on the old code for these cases should accept the new ones. The audit rows are unchanged.

### Added

- Reason `sign_in_required` (`401`): a request with neither a session nor a bearer token on a deployment with single
  sign-on and no admin token.
- `run.revoke` failure audit rows list `failed_steps`, each with its `step` and `reason`. `run.output.finalize`
  failure rows carry `step`, plus a plain `reason` where the row had none. No existing key changes.

### Fixed

- The AWS sign-in dialog shows the device code and the verification link as soon as the sign-in reports them, before
  the terminal attaches.
- The sign-in dialog's step list moves on once the sandbox has started (it also reads the run's
  `run.interactive.start` audit row), including when a run-state read fails.
- `wardyn setup status` names the console page for an unmet model-access row (Your account → Your model
  connections) instead of a button label.
- A deactivated person's session and a missing session on a single-sign-on deployment are told why the request was
  refused, not that the admin token is unset.

## [0.9.4] — 2026-10-19

A stability and honesty release on top of 0.9.3: runner and output-recovery fixes, a hardening pass on credential
erasure, stored secrets and member drives, a console polish pass across the run, setup and admin screens, and
corrected credential-claims text. Upgrading from 0.9.3, read "Before you upgrade": it carries every change that can
refuse or reshape something that worked before.

### Before you upgrade

- **Migration `0191_secrets_value_rev`** adds `secrets.value_rev`, an identity column, and rewrites the `secrets` table
  once on the first start, under an exclusive lock. A guarded secret write in flight across the upgrade sees one
  superseded conflict and is retried. The column is an identity, so a split-role install needs no extra grant for it.
- **`WARDYN_APPROVAL_EXPIRY_AFTER` of 0 or less now turns the approval sweeper off**, with a boot warning. It used to
  expire every pending approval. If you set 0 to expire approvals at once, set a positive duration instead.
- **A run blocked on a tool approval nobody answers now stops at its idle limit.** The tool gate's approval poll no
  longer counts as agent activity for the idle clock, so such a run that used to wait indefinitely now ends.
- **A kill on Kubernetes no longer force-deletes the pods.** It uses a grace of at least 1 second and waits for the
  kubelet to confirm before the run is reported gone.
- **`helm uninstall` now leaves the `<fullname>-data` PVC.** The claim carries `helm.sh/resource-policy: keep`, which
  also keeps it when you turn `persistence` off. Delete it with `kubectl delete pvc` when you mean to. A claim made
  before 0.9.4 lacks the annotation: run `kubectl annotate pvc <fullname>-data helm.sh/resource-policy=keep` before you
  turn `persistence` off.
- **A compose `.env` that sets `WARDYN_OIDC_EMAIL_DOMAINS=` empty now clears the allowlist.** It used to fall back to
  `wardyn.local`; now any email domain may sign in, and the `email_verified` requirement that goes with the allowlist
  is lifted. Leave the line out to keep the `wardyn.local` default.
- **An `ssh_key` grant whose key is the member's own secret also reads `known_hosts` from the member's own secrets.**
  Store a `known_hosts` secret of your own.
- **A member's first-use hold wait and hold count above the operator's ceiling are lowered to it**, with a warning, as
  the policy rules already said.
- **Eleven more component environment names are reserved**, and a component that sets one is refused, a stored one at
  its next run, with the name and reason: `SHELLOPTS`, `BASHOPTS`, `PS4`, `PYTHONUSERBASE`, `JAVA_TOOL_OPTIONS`,
  `_JAVA_OPTIONS`, `JDK_JAVA_OPTIONS`, `GIT_EXTERNAL_DIFF`, `GIT_ASKPASS`, `SSH_ASKPASS` and `GIT_TEMPLATE_DIR`.
- **A dotfile directory that differs from a protected one only by case** (`.SSH`, for example) is refused as `.ssh` is.
- **A sign-in name with a character that folds onto an ASCII letter** (U+0130, U+212A or U+017F, for example) gets
  `drive_subject_unsupported` (`422` on resolve, `400` on reclaim, and on the `/me` token), where its home folded onto
  an ASCII person's. An admin's home override still applies. Nothing changes for an existing drive.
- **A member's drive delete can be refused as `member_drive_in_use`** for up to 90 seconds after one of the owner's
  runs ends.
- **`GET /me/runners` is paged**: `limit` and `offset`, 200 by default, with `X-Wardyn-Truncated` when cut. The Go
  client and the console walk every page. An older CLI sees only the first 200 runners; `wardyn runner list` takes
  `--limit` and `--offset` and warns when the list is cut.
- **The egress baseline keeps today's subdomain coverage in 0.9.4.** An internal host suffix on the baseline still
  covers the host and every host under it. The Baseline hosts box now shows what each entry covers, before you save.
  0.10 moves baseline entries to the exact host named, so read that list now.
- **A personal API token can no longer propose or approve a node record** (`node_record_human_only`; the admin token
  was already refused). `wardyn runner node-record` therefore works in local mode only: on a single sign-on deployment
  use the console.
- **Desktop envelope files must be root-owned and not group- or world-writable**: `wardyn.env`, `policy.json` and
  `site-config.json` are checked as `secret.env` is, and a symlink is judged by its target. Readable modes such as
  `0644` are unchanged.
- **A staged `claude` binary built for the other architecture is refused at image build.** Stage with
  `scripts/stage-agent-binary.sh --arch amd64|arm64`.
- **Autonomous Codex task runs now pass `--skip-git-repo-check` and `--dangerously-bypass-approvals-and-sandbox`**, as
  autonomous Claude Code runs already do. Codex's own inner sandbox cannot start under Wardyn's container hardening, so
  these runs ended with no changes and exit 0. Wardyn's confinement is the boundary; Codex's is not in play.
  Interactive Codex and Claude Code runs are unchanged.
- **Scripted `setup.sh` no longer truncates stored secrets without consent.** A headless run keeps the data and prints
  the command; set `WARDYN_FORCE_RESET=1` to opt in. A shell that exports `WARDYN_AGE_KEY`, `WARDYN_AGE_KEY_FILE` or
  `WARDYN_PLATFORM_KEY_FILE` differing from `.env` is refused, and `WARDYN_FORCE_RESET=1` does not override that.
- **A person's credentials erasure deletes more.** It now deletes all of the person's delegated tokens, live or
  expired, and the history of their runs' Azure DevOps tokens that is already revoked or expired; a run token still
  live at Azure DevOps stays for the sweep that retries its revoke. `DELETE /people/{principal}/credentials` and a SCIM
  purge do the same. Erasures done before 0.9.4 gain the audit-subject tombstone only when run again.
- **`helm upgrade` now refuses `ha.enabled=true` without `k8s.enabled=true`.** That set of values crash-looped wardynd
  at boot; a release carrying it now gets the error at render instead.
- **`make setup` refuses a workspaces root that is your home directory or a folder above it**, such as `~/`, `/home`
  or `/`, because the daemon would see everything under it. This covers `WARDYN_WORKSPACES_ROOT`, typed or remembered
  in `deploy/compose/.env`. Point it at a dedicated directory, for example `~/projects`.
- **`make setup` keeps an explicit `WARDYN_LOCAL_MODE=false` (token mode) on a re-run.** A re-run no longer changes
  the mode you set.
- **A site-config CIDR of exactly `fd20:ce::254/128` is now refused at write.** It is a never-lift metadata address.

### Not tested here

These are covered by tests against simulated parts only; check them on your estate.

- Mixed-version runners: a 0.9.3 runner beside a 0.9.4 control plane, and the reverse, on a live Docker engine and on
  Kubernetes.
- The Kubernetes runner's refusal of an oversize CA bundle, and the `ends_unkept` capability, on a live cluster.
- A real `helm upgrade` that drops persistence, with the data claim kept.
- A desktop start under a real MDM profile, and the desktop tarball extracted as root.
- Browsers holding more than 8 requests in flight to one run's app over HTTP/2.
- Bedrock record promotion against an allow-all recording on a live run.
- Real Microsoft Entra ID answers for `invalid_client`, `unauthorized_client` and a disabled app.
- The console's new screens under a live single sign-on session.

### Added

- **`scripts/stage-agent-binary.sh --arch amd64|arm64`** (or `WARDYN_AGENT_ARCH`) stages the agent binary for either
  architecture; the default is unchanged. The corp image guide documents a cross-architecture build that needs neither
  the binfmt nor the buildkit public image.
- **A person's erasure can reach three more scopes**: run templates, runner pools and drives, as opt-in checkboxes in
  the console's erase dialog, unticked by default.
- **`GET /audit?run_id=...&order=desc`** answers a run's newest window newest-first. The console uses it, so a long
  run's cockpit and audit view show its latest events.
- **A queued run's page says it is queued**, why it waits, its place in line and when it gives up.
- **The Baseline hosts box lists what each internal host suffix covers** (admins), one line per entry as typed.
  Review says a typed branch is not checked until the run starts, as the picker already did.
- **Getting started and Settings name every organisation's own-token line** for Azure DevOps, not only the
  first one; a launch missing tokens names the organisations. The policy editor locks a capability only when no
  organisation's ceiling allows it, and hints the organisations that refuse one that others allow.
- **The policy summary shows the largest file, the hold waits and the largest push inspected.**
- **The setup status tier for a legacy enrolled daemon** reads "enrolled with an organisation (legacy)", apart from a
  client-mode runner. The wire value is unchanged.
- **An optional `helo` field on SMTP notification channels**; absent, the identity is `localhost` as before.
- **`k8s.clusterDomain`** in the chart (default `cluster.local`).
- **`wardyn site-config set --clear <field>[,..]`** clears a field the older forms carry forward. The old forms are
  byte-identical on the wire.
- **A caps field `ends_unkept`** that a runner sets when its substrate keeps no ended sandbox, and the container label
  `wardyn.deployment`, so each deployment's sweep leaves another's containers alone. An unlabelled container keeps the
  0.9.3 rule.
- **`pkg/client.RunNodeRecordRequest`**, an alias of the existing request type, and `wardyn audit` help lists the
  `service` actor type.
- **`wardyn-licenses.tar.gz`** (verbatim licence texts and notices) is attached to the release next to the standalone
  `wardyn-*` and `wardyn-runnerd-*` binaries.
- **`wardyn drive set` warns** when a drive file re-creates a legacy drive under id naming, and wardynd warns at boot
  about stored secret pointers the configured external store no longer derives.
- New 409 reasons `workspace_name_taken` and `workspace_update_conflict`. The `git_credential` `422` answer carries an
  additive, optional `cause` (absent means not connected); the console opens Connect Azure DevOps only for a cause a
  sign-in repairs. A bulk record promote answers with an
  additive `X-Wardyn-Record-Skipped-Hosts` header naming the model-provider hosts it skipped.
- **The `delegation.exchange` `outside_scope` audit row carries `groups_truncated: true`** when the person's group
  snapshot was partial.

### Changed

- The console's UI relay queues concurrent requests on its 8 pooled connections, for up to 5 seconds, instead of
  failing them at once with `503`.
- Approval notification retries wait 30 s, 1 m, 2 m and 4 m, each varied by up to 20%, so a rate-limited burst no
  longer retries as one burst.
- A runner's reconnect-limit refusal arrives as a WebSocket close (`1013`) carrying `runner_stream_rate_limited`, and
  an old runner logs a clearer close error instead of an opaque one.
- A claim of a runner registration the 24-hour sweep removed gets one uniform answer that names the expiry and the
  remedy.
- A runner's inventory-hold audit row is written at most once per runner per hour per replica.
- A recording is uploaded in ramped parts (5 minutes first, doubling to the 24-hour cap), so a kill no longer loses
  the first part.
- A bulk record promote skips a requested host that is a model provider's own host instead of refusing the request.
- The Runs list says when it is cut at its page limit.
- Probe error text for `curl` exits 56 and 22 names the cause.
- Release tooling: a resumed `make release-patch` no longer merges commits main gained during the wait; it reports how
  many it left out.
- Dependency updates: Go (chi 5.5.0, go-jose 4.1.6, godbus 5.2.4, moby client 0.6.2, `x/*` modules), UI (Radix,
  lucide-react 1.57.0, React 19.3.1, Playwright 1.64.1), CI actions, base-image digests and Rust 1.99.0. Dependabot now
  watches the compose service images.
- SECURITY.md's report template lists the runner components and deployment shapes and asks for the runner trust class.

### Corrections to earlier notes

- Earlier text claimed credentials never enter the sandbox. Some kinds do: the console, README, SECURITY,
  ARCHITECTURE and threat-model text now names which credential kinds enter the sandbox, and "never" is said only of
  the kinds Wardyn brokers.
- The setup `scm_provider` row follows the PAT broker switch, and the `git_pat` risk rationale names both broker
  postures.
- `wardyn run --file` help now says an `inline_policy` is sent as an overlay.
- Run lifetime: a remote workspace's `-1` is never reaped only under a ceiling with no positive `auto_stop`.
- `WARDYN_OIDC_SESSION_RENEW_CAP`, `WARDYN_OIDC_SESSION_IDLE_TIMEOUT`, the renewal `login_hint` and the browser session
  cookie are documented, with the trade-off of the defaults; the doc also says to keep runner reconnects reliable
  behind a shared ingress by allowing `/api/v1/runners/*/stream` and setting `WARDYN_TRUSTED_PROXIES`.
- With `GOVERN_ADMIN_RUNS` off, a security admin's User view is not a binding bound; the member-mode doc now says so.
- Signing out cancels a console-launched run waiting in a pool queue; the user docs now say so.
- The 0.9.3 notes left out these changes:
  - A device's queued launch is promoted as the device, and keeps the create-door refusal.
  - A later end for a run already past its lifetime limit is refused (`409` `run_end_wait_max_lifetime`), not set in
    the past.
  - A service token minted at or before `sponsor_since` no longer authenticates.
  - A UI session for a run with no service sponsor no longer names the creator as its Service.
  - The sign-in screen puts the SSO button above the "or" divider and the admin-token form.
- The 0.9.3 "Recording defaults to preferred" upgrade note needs a correction: `PUT /api/v1/runner-pool-defaults`
  replaces the whole document, so a write that leaves out `pool_id` or `barrier` clears it. Leaving out `recording`
  keeps it.
- The 0.9.3 registration note links `docs/operations/hybrid-laptops.md`, and the published agent images include
  `rsync`.
- The audit action reference lists the `entry`, `service_identity` and `via` fields on `ui.open` and `ui.close`, and
  the SDK reason table lists `runner_pool_queue_full` and `runner_pool_full`.
- The HA lock order includes the organisation MCP server key. The notify and small-lanes records show the
  accepted owner decisions. Agent threat-model category 13 names the three opt-in four-eyes gates.
- `WARDYN_AZURE_AUTHORITY_HOST` does not enable a sovereign-cloud Key Vault. The auth-failure suppression
  counter covers shared and per-key drops.
- NOTICE lists all ten dependency files. SCIM is disclosed as untested against a live Entra tenant.
- The VEX statement lists the nine images, and the probe script and README are corrected.
- The architecture and threat-model docs describe the snapshot-then-commit mint and the GitHub CLI grant's scope.
  The docs say boot's home-root warning covers `/` and the daemon's own `$HOME`.

### Fixed

Runners and output:

- When wardynd takes over a running sandbox's output after a restart, a runner that reconnects at the same moment no
  longer ends the recording early or writes part of the output twice. Its stream now waits until the takeover has
  caught up to where the runner resumes.
- A runner's version, machine name, architecture and retire capability are written together and only by its current
  session, so a superseded session's late capabilities no longer overwrite the current one.
- A runner's queued-run evidence is ordered by delivery order, not by a timestamp that can tie.
- An orphan teardown and its sweep are bounded, so a stalled engine no longer holds them open.
- When an attach lease is lost, the run's waiting observers are promoted, not stranded.
- A completion watcher that cannot read the run retries once and then leaves it to the lease sweep, instead of tearing
  down a run whose files are kept.
- A credential renewal's retry gaps are clamped so the last attempts land before the held token expires.
- A dead agent no longer reads "still running": a container that exited non-zero or was out of memory reports its own
  terminal status.
- A Docker sweep skips containers labelled for another deployment.
- An SFTP or sync session that ends because the image lacks the server prints the image-contract message.
- The Kubernetes runner refuses an oversize CA bundle with a named error, at the 128 KiB environment limit.
- `ci-run.sh` binds every published host port ephemerally and, in remote mode, kills the run when it stops waiting (a
  timeout, TERM or INT).
- `up.sh reset-all` reaps only this stack's labelled sandboxes and networks.
- The queue sweep reports a failed end, fail or promotion instead of reading as healthy.
- `wardyn-rec` exits 128 plus the signal number (143 on SIGTERM) when the recorded command is signalled, instead of 0
  or 1.
- An exec whose recording choice cannot be read is retried once, then refused; the run's failure audit names the
  cause.
- Ground-truth recording now flags connections to every known cloud metadata endpoint.
- The recorder declares a plain log, so the server stops guessing from its first bytes, and replay refuses every key
  the store would refuse as a `404`.
- Recording erasure tolerates a filesystem that cannot sync a directory, removes a run's staging files, and scans once
  for many runs.
- A transient run read no longer writes a permanent output-capture gap.
- A queued run over the quota leftover gets no near-full warning. A limits sweep re-leases a local run whose end
  moved. `run.ending_soon` reports no kept files for a run the end tears down.
- `wardyn-runnerd` treats any 5xx from register as ambiguous, and `setup --no-service` sees a systemd user
  unit under `XDG_CONFIG_HOME`. The k8s runner setup skill uses the CLI for org tokens and runner claims.
- A blank per-user `OrgURL` on the macOS managed runner falls back to the device profile.
- A proxy revive whose relay is refused keeps the running proxy.
- The drive probe container is labelled and removed after a client-side create timeout.
  `wardyn-tetragon-ingest` exits 2 naming the setting on a non-positive interval or negative buffer.
- livebus logs a repeated listener failure once, and its recovery.

Credentials, secrets and audit:

- An Entra `invalid_client` or `unauthorized_client` answer (an expired or mistyped console client secret, a disabled
  app) no longer deletes people's stored Azure DevOps, Azure Foundry or Azure CLI sign-ins; only `invalid_grant` does.
- Run tokens minted through an Azure DevOps row are still revoked after an admin switches the row's token mode.
- A stranded AWS CLI sign-in hold is resolved on the sidecar's next poll when the eager resolve did not land.
- The launch preflight's Azure DevOps fact grades the organisation the launch would refuse on when several own-token
  rows are enabled; scanning a workspace or source at your `max_concurrent_runs` cap answers `403` with the limit and
  remedy instead of `500`.
- A secret's revision no longer changes when a key rewrap, a store migration or a conversion rewrites its ciphertext
  without changing its value, so those maintenance runs no longer fail a concurrent guarded write.
- Saving a secret whose commit fails is rechecked under the row's lock before the old external value is deleted.
- A principal-key reseal checks the source key first, and the remaining count skips a row whose own key was destroyed.
- The audit spool drains past a line it cannot quarantine without re-recording the lines behind it every tick.
- An audit spool blocked behind a line it cannot move checks, every tenth pass and without recording anything, whether
  its volume is writable again, and drains once it is.
- wardynd logs a failed scan for stored secret pointers the configured external store no longer derives, and logs an
  Entra refusal that names a console client app; both used to pass silently.
- A spooled audit row counts as pending only when its parsed data marks it so; free text in an egress path or a token
  name can no longer stall the drain.
- `wardynd -rotate-age-key` refuses to run while an earlier run's `<key>.new` or `<key>.new.tmp` is on disk.
- Audit targets are cut on a character boundary and invalid UTF-8 is replaced, so a crafted path no longer produces a
  row Postgres refuses. The audit export pages by position, so events written during a long export no longer repeat at
  a page boundary. The recovery rehearsal skips erasure fence markers and accepts plain-log recordings.
- A webhook or syslog sink that stalls no longer hangs shutdown: close is bounded at 10 s and sinks close concurrently.
- Erasing a person's audit fields clears their name and typed reason from the approvals they decided, and an erased
  person's rows stay erased after their identity row is bound to someone else.
- Saving a credential locally over a row that still points into the configured external store removes the old external
  value.
- A person can disconnect their Azure DevOps sign-in after the row is disabled or their access to it is withdrawn.
- Workspace reads project recording results to the reader's tier; `wardyn support-bundle` redacts approval-notify
  webhook URLs; a member's own or shared workspace launch is no longer refused because another member onboarded the
  same repository first.
- A SCIM deactivation suspends even when its first directory read fails.
- A person created by object id no longer writes a denied attach audit row on every sign-in.
- `POST /sessions/revoke` with an unresolved or ambiguous email answers `422` instead of `500`; the reason code and
  sentence are unchanged.
- A Transit KEK refusal of a wrapped key is reported as a refusal, not as a key-service outage. A Vault KV binding
  rewrite keeps the organisation's `custom_metadata`, and a Transit decrypt refusal keeps its wrap-verdict wording.
- AWS SSO refresh no longer remembers a "not spent" answer, so a concurrent spent mark is no longer overwritten.
- The Azure DevOps org check no longer leaks its probe token when masking registration fails or the request goes away.
- An Azure DevOps Server hold timeout is not counted as an AWS SSO timeout. A failed revoke of an unrecorded
  Azure DevOps run token is audited and logged with the documented reason `unrecorded`. The lost-start audit
  note no longer blames a kill for a store fault.
- A failed secret-store list is a `503`, not an invalid policy or a missing redirect secret. `POST /runs` with
  an unlistable secret store answers `503` `store_unavailable`.
- A late decision on an approval that the run's end cancelled answers `approval_run_ended`. The second-human bypass
  failure row names the real refusal class.
- The run's mask-manifest corpus logs why it is not provably covered. SSH key and run lookup store errors are
  logged.
- A notification tier delay past int32 seconds is clamped instead of wrapping.
- A person's `runner_pools` erasure also deletes their cockpit layouts, and the threat model exception list is
  corrected.
- The daily credential tick sweeps expired attach tickets and delegated tokens.
- The audit spool gauge counts a line whose write or sync failed. Cancelled syslog emits count as drops, and
  fan-out emits survive a gone caller. Denial coalescing keeps delegated attempts apart, and the
  webhook drain flushes a batch at a time.
- Error text no longer reaches `5xx` bodies or logs on revive owner checks, Azure grant writes and drive home failures.
- Disconnecting Azure DevOps also forgets the sign-ins held for a disabled row. The audit spool's volume probe
  refuses a volume that cannot take a spool-sized rewrite.
- A sign-in refused by the domain allowlist clears the session cookie on every path.
- GitHub repo owners are trimmed and matched case-insensitively. The minted-credential lookup joins only the
  run's own grants, and a discarded mint logs its run and grant.
- `wardyn-aws-sso` prints its failure marker when setup fails before the login.
- Secret-store reconcile targets the configured external store. Each internal TLS listener gets its own clone
  of the hop TLS config.
- An idle or max-age stop runs the full revocation cascade: proxy config, PATs and resident values.

Workspaces, policy and images:

- Editing a workspace no longer reverts a run claim, record result or build that landed during the edit: the edit is
  redone from the fresh row, and one the workspace still outruns is refused with `409` `workspace_update_conflict`. A
  sources edit keeps a running record session.
- Creating or renaming a workspace onto a taken name answers `409` `workspace_name_taken` instead of `500`.
- A repository clone never deletes or clones over files already at its target, and a target that cannot be created
  skips the clone instead of ending the run.
- A companion-host redirect row (`files.pythonhosted.org`, `sum.golang.org`) no longer overrides the index row in the
  generated pip, cargo or go config.
- A push forge read to a host that speaks HTTP/2 unasked no longer fails the push check.
- Azure DevOps pull-request reviewer, property and attachment writes are no longer refused as unclassifiable.
- A stored policy mount under `/home/agent/drives` or `/home/agent/shared` is dropped and logged instead of failing the
  run at `STARTING`.
- Record promotion no longer offers Bedrock or gateway model-provider hosts, and a governance ceiling that withholds
  the Bedrock lane keeps the AWS CLI portal interception and drops the lane's own SSO injection.
- A workspace's chosen base image that fails to wrap now fails the run on the mount and policy door too.
- The "start a new run for the new limit" notice no longer appears when a pool or deployment lifetime cap cut the end.
- `/setup/status` bounds its image probes and skips the Claude sign-in probe for members.
- The operator sandbox sweep and the periodic sweep serialise per run; `wardynd -print-groundtruth-token` beside a live
  daemon is no longer refused by the single-instance lock.
- Boot reconcile, the hourly orphan sweeps and the run-secret sweep read only the runs they act on, and a member's run
  list uses the `created_by` index.
- A single-replica chart no longer renders a PodDisruptionBudget that blocks a node drain.
- `WARDYN_OIDC_CLIENT_ID` is forwarded by the compose file (default `wardyn`), and the desktop launcher points the
  internal issuer at a configured non-Dex issuer.
- The desktop launcher derives the Docker socket mount from `DOCKER_HOST` instead of exporting the envelope's
  `WARDYN_DOCKER_SOCK` verbatim, so a Colima or Rancher socket is not bind-mounted; the envelope examples single-quote
  the SIEM sinks value because `secret.env` is shell syntax; a failed plain uninstall falls back to the compose project
  label and exits non-zero if that also fails.
- `WARDYN_GIT_APPROVAL_TIMEOUT` reaches the proxy sidecar; `WARDYN_KATA_VERSION` survives the child-environment scrub.
- The full agent image's cache directories are owned by the agent user.
- `install.sh --uninstall` falls back to `docker compose down` and fails loudly if that also fails.
- `up.sh` waits 330 s for the daemon, like setup and the chart, and keeps a hand-edited policy path.
- The attach shell waits for the run's prep to finish, not a fixed 120 s; a hung clone now holds it until you press
  Ctrl-C.
- The site-config seed no longer re-validates the merged document.
- Host proxy detection masks token-only logins and PAC URLs, ignores commented maven and apt examples, and takes the
  last shell export. The `git credential.helper` probe on `/setup/status` is bounded.
- The personal component cap holds under concurrent saves. A failed source-scan launch releases the source
  with the fenced write.
- `POST /runs` warns when a repo is dropped for an unsafe ref or locator. A workspace root whose folder is
  named like a dependency tree is still scanned for git remotes.
- The governance change list sets `X-Wardyn-Truncated` only when the list was really cut. Preset reads redact
  `policy_overlay` wherever `inline_policy` is redacted and keep repository credentials out of both reads.
- `dispatch` reads `llm_inspection.mode` trimmed.
- A `404` from Azure DevOps with user data no longer says the account lacks access. The LLM coverage-gap row records
  the real port and method, and a bracketed IPv6 entry with no port gets an accurate refusal.
- `HostOf` drops a login before the first slash, and the cargo index URL ends in a slash.
- On `POST` and `PUT /drives`, the share-probe `503` and the drive-list `500` now carry
  `user_drive_ceiling_unavailable` and `user_drive_rehome_list_unavailable`, not `user_drive_request_invalid`.
  A user-type create refused by a leftover reference says so.
- A direct credential-delivery save checks `If-Match` under the governance lock. A governance profile delete
  audits the name it removed.
- contentscan masks the corpus before truncating field paths. Derived git remotes are validated.
  Leaving a runner-pool owner also removes their run layouts.
- Setup checks name `substrate_health` for an unreadable k8s runner. `llm_access` residency is always
  `proxy_injected`. Absent CLI sign-in rows answer the unavailable refusal.
- A failed source-scan grant now fails the pending run, and a repo-devcontainer image fallback is reported as
  "not built".
- A push whose trees nest too deep is refused as a `413` ceiling with a nesting remedy, not the `--unshallow` advice.
- The public-API-disabled warning is skipped when SSO is configured. Boot's operator-allowlist posture lines
  name today's role tiers.
- A very large list offset no longer turns into a negative `LIMIT` and a `500` on the runs-by-creator-or-sponsor list.
- `helm upgrade --reuse-values` from a release without `scim.purgeAfter` renders the 720h default instead of `<nil>`.
- The compose groundtruth sidecar now receives `WARDYN_GROUNDTRUTH_FORWARD_UNMAPPED_HOST_EVENTS` (default false). The
  codex-cli image selftest no longer prints `OPENAI_API_KEY` when it is set.
- `make setup` honours a non-default `WARDYN_UP_PORT` for the host wardynd and rebuilds on UI or migration changes.
  `scripts/demo.sh` no longer prints the admin token, and `scripts/up.sh` shows "local mode - no login" only in local
  mode.
- Drive probes and workspace scans now apply the same mount rules as a launch. An `egress_host` grant written as
  `host:port` is stored as its host.
- Directory search no longer offers App Role suggestions when there is no sign-in app. A held role-mapping approval
  that would now change access names the step that resolves it.

Tests and CI:

- Test integrity: 36 guards that could pass vacuously now fail on a planted defect. Audit-DDL probes fail instead of
  skipping when the lane can satisfy them, and the refresh-token fake redeems exactly once under concurrency. The e2e
  harness retry re-picks ports, and the python task graders run isolated and require `Ran N tests` and `OK`.
- The testlive secret-file guard judges the real path and fails closed.
- rc tags no longer move `:latest` on the release build path. The release workflow marks only the CLI and runnerd
  binaries executable.
- `scripts/verify-release.sh` no longer demands `wardyn-runnerd.pkg` of v0.9.3-rc.1 and rc.2 and says why an image
  lookup failed. It checks a tag before 0.9.4 without the licence tarball, which first shipped in 0.9.4.
- `make runnerd-image` no longer stamps a local dev image as a release build, so a local runnerd reports its
  commit-suffixed version. The helm-lint absence checks fail on a render failure.

CLI:

- The approvals list's HOLD column shows the server's hold, not a private 30 s window; `wardyn run logs` no longer
  drops reordered events; `wardyn governance plan` and `set` key assignments as the server canonicalises them.
- SDK: Go names for the first-use and override constants. `GovernanceChange` carries its decision.
  Apply steps carry the server's warnings, printed by `wardyn governance set`.
- A failed apply keeps its held changes in a `PendingApprovalError`. `ListRunnerPoolsPage` reports truncation,
  and `wardyn runner pool list` warns.
- `wardyn workspace --add` keeps an `@` inside ssh URLs. The 401 hint points at the compose quick-start's
  token. `wardyn logs --interval 0` no longer spins.

Console:

- An SSO sign-in that began on a deep link returns to that page. An identity provider's refusal shows a message on the
  sign-in screen. A cold load whose session expired says you were signed out. After an SSO sign-out the console
  reminds you to sign out of your identity provider too, on a shared computer.
- The session-expiry banner follows the role-stamp deadline. A member renewing their sign-in on Getting started is no
  longer told the page is not theirs. The AWS device sign-in pane follows a retried login's new code.
- The admin-minted token list no longer counts tokens of deactivated people; "include revoked" still shows them.
- The Approvals screen asks for the User view's own scope and polls only what is on screen; a held sign-in in the run's
  Approvals tab offers the sign-in door instead of disabled Approve and Deny buttons.
- Revive no longer reports failure after a successful revive, and a revive that timed out reads as "result unknown".
- A failed poll no longer drops a demo sandbox; a failed attach-ticket mint on reconnect retries; a stale deny
  confirmation no longer reopens.
- A never-handed-out Azure DevOps token is no longer shown as the live one; the killed-run list names file secrets it
  cannot revoke; a service identity's sponsor can open its run's terminal.
- The ending-soon warning no longer promises 7 days of kept files; Recordings keeps paging when the newest runs have
  none; the containment panel refreshes while a run is held and offers Retry when a read fails.
- Setup and New Run: a refused harness token no longer re-posts on every chunk, and the paste fallback and Cancel stay
  after a refused auto-capture; an edited egress rule drops its stale test verdict; a proxy save clears the old probe,
  and the corporate-network probes get the launch deadline; a credentialed proxy row shows `user@host` only and never
  saves the masked password; the setup review button and "Switch to user view" obey the same gate as Finish setup; the
  Environment step no longer says a barrier pick is saved in the browser; the record review's Approve list leaves out
  the hosts the server never promotes.
- Governance and Settings: host-list removal buttons are disabled while any removal is in flight; the member approvals
  hint no longer offers "always" and the directory chip follows the picked claim; an edited governance profile folds
  its composed limits into the standalone form; a non-whole token lifetime withholds Save; a switched-off git provider
  row says why Save is withheld; a multi-source workspace's detail header names its composition.
- Copy fixes: the SafetyMeter caption no longer says JSON for a YAML editor; the drives copy describes the multi-drive
  model; the SCIM unfinished-step table and labels name every step; the Permissions copy says `service_attach` binds
  super admins; the Governance precedence line names the user-type tier; the account menu role chip uses the role
  label; Wall is no longer called the default barrier; both workspace delete confirmations name the recorded sessions
  and requirements; the Allowed hosts subtitle no longer claims every row was approved; the welcome note points at the
  sidebar's Setup entry; the no-metadata credential store is no longer titled "Something went wrong"; a pre-created
  retention partition reads "Not started"; the inert-row hint says to delete the old row after re-adding; Recordings
  and the New policy dialog drop implementation terms; the run policy view's disk sentence is reader-neutral.
- Narrow screens: the audit row, tab bars, the Governance tab list, the run Overview pane, copy pills and the
  New Run footer no longer overflow at phone width; the footer is a sticky bar under 640 px.
- Copy and layout pass across run, setup, admin, governance, workspace and settings screens: one word for "Pending",
  sentence case on the Run step timing options, a short caption under each locked setup step, a back link on the runner
  pool form, the Who column on wide Add-a-grant forms, empty states inside cards, a Re-check button on the Settings
  Host card, the Changes tab, and the New policy dialog's "Start from a template" menu.
- A `permissions_missing` Azure DevOps card says an admin must act and offers nothing to press. The Policies
  page names the ceiling policy as the limit on any inline policy.

### Security

- Hardened stored-secret grant resolution, sign-in hold binding, role-map approval serialisation, and the sponsor revoke
  for an email-form directory identity.
- Reserved more process-control environment names from component configuration (shell, Java and git helper hooks).
- Tightened the member drive home, mount ceiling, deny-list and delete rules, queued-run snapshots, and local revive.
- Tightened the Entra and Azure DevOps token cache under high availability, so a deleted sign-in stops being injected
  within its documented window.
- Tightened what run owners and other readers below the security tier see in containment and audit views, and the
  projection of workspace egress writes.
- Hardened policy composition limits, peer text in dial-failure reasons, the git push path-byte budget, the SSH
  gateway's connection accounting, the sign-in return path and the console relay's response headers.
- Hardened the workspace scan advisor's tool lock, the Tetragon label handling, an accepted record's re-check on later
  reuse, desktop envelope checks, and node-record sessions.
- Hardened the desktop tarball, which is built with root-owned entries.
- Hardened CI: the nightly migration-merge check builds and tests only same-repo pull requests (fork pull requests are
  counted and skipped), runs its checks in a step with no token in its environment, and keeps its scratch refs to
  itself; the signed release binaries are built without the shared Go cache; the distinct-UID recording permission
  proofs run as root in a throwaway container and fail if skipped.
- Hardened credential-holding processes (`wardyn-runnerd` now disables core dumps at start, like `wardynd` and
  `wardyn-proxy`) and tightened key handling in the secret store, the attach CLI's error text, and the redaction of
  sandbox-facing error messages.
- Tightened the egress proxy's handling of tool-approval text, brokered git discovery and model-gateway addresses, and
  added the Google Compute Engine IPv6 metadata server to the addresses an `internal_hosts` lift never opens.
- Hardened setup: the chart's `kek.*` renders without a `secretStore` block, and setup probes pass the admin token on
  stdin and send none to `/healthz`. `demo.sh` drops `-e token`. `test-drive` refuses an unknown `--section` and
  prints the token only when it is the demo default.
- Hardened images and build: agent images remove npm's build proxy settings after install, and `agent-full` checks the
  Temurin key fingerprint and pins the repo. Release SBOM tooling installs from a checksum-verified tarball. The image
  download gate covers `wget` and `ADD <url>`, and `.dockerignore` adds secret-bearing local files.

## [0.9.3] — 2026-10-17

Organisation runners in Kubernetes, runner-resident credentials, and laptop runners fit for a locked-down
enterprise. This is the final release of the three 0.9.3 candidates: upgrading from 0.9.2, read this section with the
rc.3, rc.2 and rc.1 notes below, which carry every change and each "Before you upgrade" step.

### Defaults changed by this upgrade

Setup status lists the ones that reach your deployment ("Defaults changed by this upgrade", admins only).

- **Recording defaults to `preferred`.** A run a non-recording laptop runner takes goes ahead unrecorded and says why
  (`runner_declined`). For the old behaviour, set `"recording": "required"` with `PUT /api/v1/runner-pool-defaults`
  (read it first: a write that leaves `barrier` out clears it).
- **A laptop runner records a run only when its organisation keeps recordings.** With `WARDYN_RECORDING_STORE=off`,
  runs are marked `org_off` and nothing is uploaded. For the old behaviour, turn the recording store on.

### Before you upgrade

- Upgrade the organisation before its laptop runners: the proxy image of 0.9.2 refuses a 0.9.3 runner's
  configuration, so an unpinned runner's runs do not start until the organisation serves the new image.
- Migrations `0180` to `0190` run on the first start. On a split-role install, grant the app role what the rc.3 and
  rc.2 notes list, and `DELETE` on `runner_registration_tokens`: minting a runner token now deletes its owner's
  expired tokens, and fails without it.

### Not tested here

These are covered by tests against simulated parts only; check them on your estate.

- A run on a macOS runner sending its decisions to the runner end to end, on a real Mac with your container engine.
  The macOS package ships unsigned.
- Recovery after a control-plane restart, on a laptop fleet.
- The proxy NetworkPolicy's cluster-DNS peer, on a network plugin that selects pods by identity.
- The proxy's memory limit under production load in a real cluster.
- The AWS KMS key service with IRSA or EKS Pod Identity on a live EKS cluster.
- The console's admin and member view switch, and the terminal walks, under a live single sign-on session.
- Two embedded third-party bundles (the Mermaid CLI's browser bundle, and code-server's bundled dev-containers CLI)
  still carry older dependency versions, pending their upstream releases.

### Changed

- On a deployment that signs in with Microsoft Entra ID, the sign-in screen's button reads "Sign in with Microsoft", with "Use your work or school account" under it.
- A run on a laptop runner whose proxy image is older than the runner now fails with the fix in its reason: the
  error names the setting the proxy refused and says to upgrade the organisation before its runners, or to pin a proxy
  image at least as new as the runner, followed by the proxy's own output.
- The `412` the console's expected-principal check answers now carries the reason `expected_principal_mismatch`;
  rc.3 sent `site_config_stale`.
- A queued run under required recording, in an organisation that keeps no session recordings, waits with
  `runner_pool_recording_store_off` instead of `runner_pool_no_runner_free`.

### Corrections to earlier notes

- Since 0.9.0, the launch warning that a run's managed settings were not delivered is built from the runner the run is placed on and covers every harness in the run, so it appears for a secondary Claude Code harness and a runner-placed run, and no longer appears for a run that is refused over its tool rules. Earlier notes did not say so.

### Fixed

- Run output with `source` `recording` no longer reads "some bytes may be missing" in the CLI or `docs/sdk.md`: `incomplete` there means the output cannot be vouched complete, and only `capture_gap` says a capture failed. The wire field is unchanged.
- "Discard mine and reload" on the Providers and Agents screens and the model provider editor now also re-reads who you are signed in as and reloads the page when another tab switched the session to someone else, so the page no longer keeps refusing every save as the previous person.
- The run page's "Not recorded by the runner" note no longer claims the organization's sessions are recorded now.
- A forward-lane request the egress audit cannot record (a full decision spool, or a record too large) now carries
  `X-Wardyn-Reason` on its plain-text refusal, as the other refusal shapes do.
- A runner's reconnect reads only the runs its leftover objects name, not every run in the organisation. Its decision
  sink refuses a session whose spool lock does not free within 10 s instead of stalling every other run's session.
- A clone failure that points at a declined Azure DevOps credential names the repository whose clone failed.
- A kill a runner deferred is asked again once that runner completes its next inventory, and the run's fence lifts when
  the runner confirms it; before, the deferred answer stayed the run's last word until someone killed the run again.
- A run on a runner that was contained while the runner was offline (its runner restarted, for example) can be revived
  again. Before, the runner refused it a lease forever. Now the first revive queues the runner's recovery and answers
  `409` `revive_containment_recovering`, and the next revive, once the runner has reconnected, brings the run back.
  A killed or quarantined run is still never recovered. The new audit action is `runner.recovery.request`.
- A policy apply is audited together with the run's creation, so a refused launch leaves no policy apply row, and a recording upload for a run that no longer exists answers 404.
- A runner's version and last-seen time are recorded when it connects, so the runners list and its runner.connect audit rows show which release each runner runs.

## [0.9.3-rc.3] — 2026-10-17

Organisation runners and resident credentials: an organisation can run sandboxes on its own runner in a Kubernetes
cluster that the control plane never dials into. A laptop runner can now hold four credential classes in its OS
keystore for a run. A security operator sets each runner's trust class, and macOS fleets install runners from a
package and an MDM profile. This section lists the changes since 0.9.3-rc.2; read it with the rc.1 and rc.2 notes
below.

### Defaults changed by this upgrade

Setup status lists the ones that reach your deployment ("Defaults changed by this upgrade", admins only), each with
what to set for the old behaviour.

- **Recording defaults to `preferred`.** A run that a non-recording laptop runner takes goes ahead unrecorded and says
  why (`runner_declined`), where a runner older than 0.9.3 recorded it regardless. It reaches deployments with claimed
  self-hosted runners and a recording store. For the old behaviour, set `"recording": "required"` with
  `PUT /api/v1/runner-pool-defaults`, beside the stored `pool_id` and `barrier` (read them first with `GET`: a write
  that leaves `barrier` out clears it). A run only a non-recording runner could take is then refused instead of
  running unrecorded.
- **A laptop runner records a run only when its organisation keeps recordings.** With `WARDYN_RECORDING_STORE=off`,
  runs are marked `org_off` and nothing is uploaded, where before the runner recorded and its upload failed. It reaches
  deployments with claimed self-hosted runners and the recording store off. For the old behaviour, turn the
  recording store on (`pg` or `fs`); `wardyn-runnerd serve --recording-store` stays the runner's ceiling.

### Before you upgrade

- Upgrade the organisation before its laptop runners. A runner from this release configures its proxy with
  `decision_sink`, which the proxy image of 0.9.3-rc.2 and older refuses, so an unpinned runner's runs do not start
  until the organisation serves the new image; a pinned runner needs an image at least as new as its own release.
- Migration `0181_runner_org_trust` adds `owner_kind`, `trust_class`, `trust_set_by`, `trust_set_at` and
  `registration_token_id` to `runners`, and `owner_kind`, `minted_by_kind`, `minted_by_email` and `break_glass` to
  `runner_registration_tokens`. Existing runners become `person` and `personal`.
- Migration `0182_runner_resident_runs` adds the `runner_resident_runs` table. Migration `0183_runner_retirements` adds
  the `runner_output_watermarks` and `runner_retirements` tables, `runners.retire_capable` and
  `runner_execution_exits.receipt_only`, and lets a `runner_pending_actions` settlement be `abandoned`.
- Migration `0184_run_runner_trust_class` adds `agent_runs.runner_trust_class`. Local runs already on a runner become
  `personal`.
- Migration `0185_runner_pool_org_default_revision` adds `runner_pool_org_defaults.revision`. A `PUT` of the
  organisation's pool default that names a revision that has moved on is refused `409` `runner_pool_stale`. One that
  names no revision writes as before.
- Migration `0186_purge_attribution_backfill` reopens two steps of every purge finished before it, once, so a person
  purged earlier is also cleared as a runner's revoker and as the minter of a token made for someone else. Their
  `person.deprovision` row is not written again.
- Migration `0187_runner_subject_run_proof_unasserted` replaces the subject check on `runner_pending_actions`: a
  quarantine bound to a real run can no longer carry an `asserted` proof. Rows written before it are left as they are.
- Migration `0188_run_renewed_token_jtis` adds `agent_runs.token_jti` and `agent_runs.token_prev_jti`, the two tokens a
  run's proxy last held across a renewal, which a revive now revokes.
- Split-role installs (`WARDYN_PG_MIGRATE_DSN` set) whose app role does not pick up new tables through the
  `ALTER DEFAULT PRIVILEGES` step in Operations: grant the app role `SELECT, INSERT, UPDATE` on `runner_resident_runs`
  (migration `0182_runner_resident_runs`), `runner_output_watermarks` and `runner_retirements` (migration
  `0183_runner_retirements`). Without them a runner cannot be handed a
  `runner_resident` credential, and a finished runner run's output is never retired. Grant it `SELECT, INSERT, UPDATE,
  DELETE` on `run_containment_pending` and `SELECT, INSERT, UPDATE` on `run_node_records` (migration
  `0189_run_containment_pending`), or a Kubernetes runner's state page is refused while it holds a run as containment
  pending. Grant it `SELECT` on `deployment_versions` (migration `0190_deployment_versions`), or setup status leaves out
  the defaults an upgrade changed.
- Upgrading from before 0.9.1, migration `0178_self_host_runner_restriction` now keeps self-hosting open whenever a
  runner is claimed, including one whose registration token an admin minted. A database that already applied it is
  not asked again: if people who are not admins are refused `403 capability_feature` on `POST /me/runners/tokens`,
  write `POST /api/v1/permissions/grants` with
  `{"subject_type": "all", "capability": "feature", "value": "self_host_runner", "effect": "allow"}`.
- A repository URL with a user name, password or token before its http(s) host is now refused (`400`
  `repo_url_credentials`). Workspaces, library sources, policies, presets and templates stored with one now fail
  closed at launch and scan, and must be edited to the plain URL with the credential in a `git_pat` grant. Azure
  DevOps' Clone-button form `https://<org>@dev.azure.com/<org>/...`, `git@host:path` and `ssh://user@host` addresses
  are unaffected; an `ssh://` URL carrying a password is refused too.
- `wardynd` now refuses to start when the default policy file has a `git_pat` host with a port or an IPv6 literal. The
  message names the fix. A policy write refuses the same host.
- An existing `go` redirect now keeps Go's checksum database on: `GOSUMDB` is
  `sum.golang.org <to>/sumdb/sum.golang.org`. A mirror that does not proxy `/sumdb/` fails its runs' module downloads
  until the redirect sets `go_checksum_db_off`.
- A policy's `max_file_size_mib` is bounded at 32, the most a git pack can carry. A stored policy above it is enforced
  at 32, and its next save is refused until the value is lowered.
- A run, or a save of `recording: required` (a pool, or the organisation's defaults), is refused `422`
  `recording_store_off` when the organisation keeps no session recordings and recording is required.
- A kill whose teardown is incomplete still answers `500`, now with reason `internal_error`, and its `errors` field is
  an array of the failed step names instead of an object of step and error text. The text is in the `run.revoke`
  audit row; retry the kill.
- The console's draft saves, the providers Saves among them, can now be refused `412` (`site_config_stale`) before
  anything is written. Reload and retry. Writes without the console's header (the CLI, scripts) are unaffected.
- A runner older than 0.9.3 is refused a run that needs a resident credential (`placement_credential`) and is not
  asked to retire a finished run's output. Upgrade runners after the organisation.
- A runner's state file can now hold compacted retired runs, so a runner moved back to an earlier version refuses it
  after any retirement; downgrading a runner below this release is not supported.
- A run queued before the upgrade whose MCP server takes an organisation credential can still start on your own
  runner; its MCP calls then fail. End it and launch again.
- A port-qualified entry in a ceiling's allowed domains no longer covers a bare host derived for the same name; list
  the bare host as well.
- The component configuration schema no longer has a `string_list` field kind. A schema that uses it is refused.

### Added

- **Organisation runners in Kubernetes.** `wardyn-runnerd serve --substrate k8s` places each run as an agent pod and a
  proxy pod in a dedicated runs namespace and dials the organisation outbound only. The `wardyn-runnerd` image is
  published with each release; install its chart from `deploy/helm/wardyn-runnerd` in the release's source tree. Boot refuses a kubelet below 1.31
  (`kubelet_below_floor`) and starts only when an egress canary proves NetworkPolicy is enforced. One release serves
  one architecture (`WARDYN_RUNNERD_ARCH`).
- A security operator mints an organisation runner's token (`wardyn runner org-token mint`) and claims the runner by
  its fingerprint (`wardyn runner claim-org`); under four-eyes the claimer must differ from the minter. An organisation
  runner has no owner and takes runs only through an organisation runner pool.
- Every runner has a trust class: `personal`, `managed_device`, or `org_cluster` for an organisation runner. A security
  operator changes it with `PUT /runners/{id}/trust`, `wardyn runner trust` or Admin → Runners, held for a second
  person under `WARDYN_GOVERNANCE_SECOND_HUMAN` and audited as `runner.trust.write`. Credential delivery can differ by
  class (`by_trust`), and a revive on a runner whose class changed is refused `409` `runner_trust_changed`.
- The operator guide for organisation runners, docs/operations/k8s-runners.md, with a design note and a setup skill.
- A kind conformance suite runs the Kubernetes runner against kubelets 1.31 and 1.35.
- A laptop runner now holds `env_secret`, `file_secret`, `ssh_key` and `git_pat_helper` values set to
  `runner_resident` in its OS keystore for the run, and deletes each at run end (`runner.resident.erase`). A refusal is
  `runner_posture_unmet` when the confinement floor or the runner's posture misses the requirement, otherwise
  `placement_credential`. Erasure on a computer the developer controls is best-effort. See
  docs/operations/hybrid-laptops.md.
- Every release carries `wardyn-runnerd.pkg`, a universal macOS installer, and a managed-preferences profile template
  (`deploy/runner/macos/wardyn-runnerd.mobileconfig`). `serve` reads the profile's keys as flag defaults. The package
  is unsigned until the release workflow has the Apple Developer ID secrets.
- `wardyn-runnerd serve --upstream-proxy` (`WARDYN_UPSTREAM_PROXY`) chains every run's egress through the laptop's own
  corporate proxy. `setup --no-service` registers and prints the command to run the runner without installing a
  service. A local docker context is carried into the service as `DOCKER_HOST`; a TCP, SSH or TLS context is refused.
- `WARDYN_TRUSTED_PROXIES` (Helm `trustedProxies`) names the reverse proxies whose `X-Forwarded-For` gives the client
  address, so each runner and registration gets its own rate-limit bucket behind a shared ingress. The proxy peer is
  kept in audit as `data.proxy_peer`.
- Console session lifetime settings, each off by default: `WARDYN_OIDC_SESSION_RENEW_CAP`,
  `WARDYN_OIDC_SESSION_IDLE_TIMEOUT`, `WARDYN_OIDC_SILENT_REAUTH`, `WARDYN_OIDC_LOGIN_HINT` and
  `WARDYN_OIDC_BROWSER_SESSION_COOKIE`. See docs/ENV.md.
- Session renewal and the idle timeout count reading the console and typing in its terminal as use: a request sent
  within a minute of the person's own keyboard, pointer or wheel input renews the session and resets the idle clock.
  With renewal on, the expiry warning and open terminals follow a renewal instead of ending at the expiry the page
  loaded with. docs/operations/who-gets-in.md compares the three session shapes; with `WARDYN_OIDC_SESSION_TTL` unset
  a session ends with the identity provider's ID token, about an hour on Entra ID.
- A live run's AWS SSO session is renewed in the background before it expires, so a quiet run no longer reaches the
  sign-in hold while its refresh token still works. A session AWS will not renew still holds for a sign-in. The
  renewal's `harness.credential.refresh` row carries `trigger: renewer`, and its passes are the `aws_sso_renewal`
  sweep. `WARDYN_AWS_SSO_REFRESH_SKEW`, `WARDYN_AWS_SSO_REFRESH_SERVE_FLOOR`, `WARDYN_AWS_SSO_REFRESH_RETRY_DELAY` and
  `WARDYN_AWS_SSO_REFRESH_TIMEOUT` tune the renewal, with today's values as defaults; boot refuses a value outside its
  bounds. See docs/ENV.md.
- An Interactive run is idle-stopped on its pool's idle stop when its policy sets none. The New Run pool list says
  whether a run starts now or waits, and `GET /runner-pools` rows carry each pool's state, revision and limits.
- Console: Admin → Runners shows and edits the trust class; sign-out, sign-in and a mode switch in one tab reach the
  others; a restart of older runs shows its progress; My pools has its own card when runners are off; the user type
  can be chosen in the governance and drive previews; a component can carry a one-line description.
- `resources_applied` reports the process limit, and New Run says when a run's process limit was cut.
- An agent-task template can leave its prompt for use time. A permission allow for a feature this version does not
  know can be staged; it grants nothing.
- `wardyn runner revoke <runner>` revokes one of your own runners by id or name, the command-line twin of My runner.
- `wardyn runner pool add` and `remove` put an organization runner in an organization pool and take it out, by name or id (an admin; a runner serves one pool).
- `wardyn-runnerd register --trusted-ca-file` makes the registration call trust a corporate CA beside the system
  roots; the Kubernetes chart passes it to its register container when `trustedCA.configMap` is set.
- Security admins can set who may use a remote-hosted runner.
- A run a Kubernetes runner deleted but cannot prove stopped shows on run detail as "Containment pending; egress
  bounded until T", then "Egress bound passed; termination unknown". The run keeps its lease hold, its proxy's token
  renewal is refused (`run_containment_pending`), and a security operator can release it with a node record ("node
  powered off" or "no prior holder") from run detail or `wardyn runner node-record`; a run whose node name was
  reused, or that the runner could not tie to one machine, needs two people. Migration `0189_run_containment_pending`
  adds `run_containment_pending` and `run_node_records` and lets a `stop_proxy` action carry a node record instead
  of an outage loss. The runner's machine list and each node record's source id are shown to security operators
  only.
- A Kubernetes run whose proxy was given an address still fenced for an earlier run fails at start with
  `relay_ip_fenced`.
- Governance has a Baseline hosts tab, the console for `PUT /governance/egress-baseline`: the hosts the autonomy
  rubric and the risk grade treat as baseline beside the built-in public set. A run whose allowlist holds only such
  hosts does not grade egress `open`, and an `api_key` to one, a Bedrock provider's credential included, grades
  secrets `baseline`, not `powerful`. Nothing is declared by default, so grading changes only when someone adds a host.
- A governance profile write whose rubric sets any row now says what the rubric can see on this deployment: the
  egress, secrets and confinement grades a run under it can get, and which rows can cap a run and which cap nothing.
- Setup status names, for admins, the defaults an upgrade changed that reach this deployment's own configuration
  ("Defaults changed by this upgrade"), each with what to set for the old behaviour. wardynd records each release it
  boots (migration `0190_deployment_versions`).
- Setup status shows a "Session recording" row for admins: whether recording is on and which store, and when it is off, why and how to turn it on (`env.WARDYN_RECORDING_STORE=pg` or `persistence.enabled=true`). The chart's default records nothing.
- Setup status warns admins when `self_host_runner` is restricted, runners people own are claimed and no permission names it, so
  only admins can mint runner tokens; it gives the grant to write.

### Changed

- An unpinned laptop runner takes the proxy image the organisation answers at every connect, so an upgraded
  organisation's image reaches it without a reinstall. `setup --proxy-image` pins one.
- On a person's own runner, a run is refused `placement_credential` when its MCP server takes a credential the
  organisation shares, or when a harness uses a model provider routed through a gateway.
- On a run several people can enter, one person can hold at most seven of its eight sessions. A personal run keeps the
  run cap alone.
- The runner stream adds a global handshake bucket and a cap of 256 handshakes in progress; every refusal is `429`
  `runner_stream_rate_limited`, naming the limit.
- A person can mint at most ten runner registration tokens an hour; the eleventh is answered 429 with reason
  `runner_token_rate_limited`. Minting also deletes that person's expired, unused tokens.
- The internal credential mint answers a revoked run, an unapproved grant, a missing ref ruleset and an unsupported
  grant kind each under its own reason: `grant_run_revoked`, `grant_not_approved`, `grant_ref_ruleset_required`,
  `grant_kind_unsupported`. `wardyn-git-helper` prints the server's sentence.
- A deny of any GitHub host also withholds the GitHub CLI door, and a withheld Azure CLI door takes its hosts with it.

### Fixed

- Laptop runners: setup writes the resolved engine every time and `DOCKER_HOST` outranks `DOCKER_CONTEXT`; a pull fails
  on a registry error inside the stream; the service unit lands where the user manager looks, uninstall finds it under
  a custom `XDG_CONFIG_HOME`, and a refused authentication names its cause. A runner revoke waits at most 5 s for its
  lock and answers `lock_unavailable` to retry. A trusted-CA file over 1 MiB is refused without being read.
  A macOS service setup removes the environment file an earlier `--no-service` run left, so a later `--no-service`
  run no longer reads its old pins back, and a trusted-CA path holding a line break is refused. A proxy image set in a
  macOS managed profile is a pin, as the flag is, even when it carries a release tag.
- Placement and the queue: a promotion never starts a run past its wait, a failed runner-limit read is retryable, a run
  with no size block is capped below the default, a `policy_overlay` size is the run's ask, and a revoked runner's
  queued run that cannot be ended backs off. The pool doors answer `503` when a runner that might take the run cannot
  report.
- Audit, erasure and leavers: a person named in audit data is sealed beside the target, mask copies are erased under
  every name a person is known by, a leaver purge counts the pools it narrows, and direct governance deletes record
  what they removed. `WARDYN_RUN_OUTPUT_RETENTION_DAYS` is bounded.
- Egress: a gateway is matched by address in any spelling, a spelled port is judged as the proxy matches it, an IPv6
  literal is bracketed in the git broker's `Host` header, a GitHub CLI token minted during shutdown is handed back, and
  a workspace's egress requirement rows are held to the launching ceiling on replay as on launch. A proxy whose
  `upstream_proxy_url` is an address inside `hard_deny` is refused at boot.
- Azure DevOps: a default-profile narrowing now applies to the repository a selected workspace adds, at create,
  preflight and preview, and a package recycle-bin restore needs the manage capability.
- Policies, templates and governance: a governance write refuses at proposal what it would fail at approval, composed
  profiles that no longer compose are named at boot, a member's policy read lists no `llm_inspection` secret names, and
  a GitHub push remote with no owner and repository is refused. A run with a GitHub CLI row is refused when your
  organisation's profile blocks GitHub, the way the built-in GitHub component already was; it used to start and lose
  the CLI without a word. A node record's approvers that chain into one person (a shared email and a shared
  principal) count as one, in any order, and an approver with no principal is refused.
- Audit: the `run.ceiling.reassert` row carries its `note` only when it added a denied host or withheld a credential
  lane or injection host; a row that did none of those no longer reads as a report of action taken.
- Console: QUEUED runs read as Queued; a lost run keeps Revive and Extend; drive sizes under 0.1 GiB read in MiB; a
  stale save shows the saved-elsewhere banner; Review gives each blocker its own sentence; approval reasons are bounded
  at 512 characters in the console and the CLI; lapsed minted tokens read Expired.
- CLI: the derived `--wait` queue limit is halved for a `--timeout` under 2 minutes, and `runner-limits` lists ids.
  `wardyn runner remote list --ids --json` is refused instead of ignoring `--json`, and `wardyn runner trust` names
  only `personal` and `managed_device` as settable.
- Kubernetes: the proxy's NetworkPolicy DNS rule also admits the cluster DNS pods (`k8s-app=kube-dns` in
  `kube-system`), so a network plugin that selects pods by identity can match it; the address range and the
  cloud-metadata exclusion are unchanged. The chart keeps runnerd's state in a directory below its volume, which
  runnerd accepts as private; the volume's own root was refused as not mode 0700. A hung exec is bounded, a placement no node allows fails fast and ignores node churn, the quota check
  also counts a run's Secret and NetworkPolicies, and on an organisation runner a sandbox stop after a proxy stop
  keeps the released IP released.
- Recording: saves are durable and chunked, an upload's outcome row survives a client that hangs up, and a masked
  plain log is cut at the store cap with the cut marked.
- Run output: a laptop run's output is no longer read from a recording it never made when the server's own runner is
  Kubernetes with recording on. It used to come back empty, marked `incomplete` and `capture_gap`. The source now
  follows the runner that served the run, so the laptop's tail is kept and read as `stdout`. A run that is not
  recorded reads its tail, and a recording that yields nothing no longer hides a tail the server holds. A pooled
  run asks the substrate its pool named. With several substrates and no pool (Docker beside Kubernetes under the
  recorder), the substrate the run lands on decides, so a Docker run keeps its tail.
- Runner links: the organisation's relay listener speaks HTTP/1.1 only and holds at most 128 relay streams per runner,
  so one runner cannot fill its own session. A link no longer stalls when a peer opens many streams the runner
  refuses, and a handler panic ends that one link instead of the runner process. A runner that cannot open one run's
  spool still replays the other runs' evidence, and a quarantine that lands mid-replay stops that run's remaining
  backlog; a retired run no longer publishes activity every second. `wardyn-rec` forwards SIGTERM and SIGHUP to the
  recorder, so a terminated recording is still delivered. A runner whose unclaimed registration lapsed, or whose
  registration changed while it connected, is told so in the runner log (`registration_lapsed`) instead of seeing a
  bare refusal; the sentence is sent only after the runner proved its key. Switching runners off now ends a runner
  session that was admitted while the switch was being turned off, and a revoke, switching runners off or
  deactivating the owner ends an admission that was stalled on a dead link instead of waiting out its timeouts.
- Runner recovery: a runner that met a control-plane restart no longer stays offline, looping on `containment-only
  identity refuses this action`. The end the restart queues for a run the runner keeps only for containment is refused
  with a receipt, which closes it, so the runner reconnects with its local state untouched, and the next control-plane
  start sends that run a kill, which stops its agent, instead of the same end again. A control-plane start also clears
  a run whose end the runner already applied, and kills one whose stop an online runner refused. A computer registered
  again (Docker runtime) stops the proxies of the earlier registration's runs at its first start and moves that
  registration's `offline/`, `spools/` and `output/` into `previous/` in the state directory, instead of refusing them
  at every start; decisions still spooled there are kept, not delivered. `setup` on a computer that is already
  registered says the token it was given was not used.
- macOS and other VM-backed engines no longer refuse requests with `audit_spool_unavailable`, because the proxy no
  longer shares a directory with the runner: it sends its decisions to wardyn-runnerd, the spool's only writer, as a
  Kubernetes runner's proxy does (see Before you upgrade for the order). A spool error names its directory (`openat
  <dir>/state.json`), and macOS runners get their state directory as `--state-dir` in the LaunchAgent, as Linux units
  always have.
- A runner whose organisation link stalls no longer waits in silence: a stream dial that is never answered fails after
  30 seconds, as does a Docker engine that never answers `serve` at start or a resumed session's containment, and a
  runner with no session logs `runner still offline, retrying` at most once a minute.
- A refused sandbox create no longer leaves its output sink registered, and an adopted run registers its sink before
  the runner is asked for its output.
- An organization pool's member list (`GET /runner-pools/{id}/members`, the console's pool sheet) now shows the
  organization runners it holds, which it left out.
- Revive loads the new proxy's configuration before removing the old proxy, relayed streams survive one failed run
  read, a run started with your own tools starts no CLI, and an egress hold is reported for as long as the proxy
  holds it.
- Console: Providers sizes a member drive on Kubernetes and saves it, the New Run footer shows no stale all-clear,
  Templates says when group membership could not be checked, and the terminal says when a run is at its session cap. A terminal refused at the
  per-person session cap on an operator-owned or service identity's run now names that cap (seven per person) instead
  of the run's eight.

### Security

- Anonymous runner admission is bounded per replica, and a forwarded address is read only from a trusted proxy.
- `wardyn run ssh` refuses a plain-HTTP server that is not loopback before it sends any request.
- `internal_hosts` refuses a range inside a cloud metadata endpoint, and the model gateway guard refuses those
  addresses.
- Repository URLs carrying credentials are refused and redacted from more reads (see Before you upgrade).
- A revive revokes the tokens the replaced proxy renewed.
- Hardened policy grading and the Azure Resource Manager and Azure DevOps path checks. The Azure Resource Manager door
  also refuses a request that carries any of thirteen path-override headers (`X-Original-URL`, `X-Rewrite-Path`,
  `X-Forwarded-Prefix`, `X-Override-URL` and their variants, in any case and with hyphens or underscores), as it
  already refused the method-override headers.
- Hardened directory-sync leaver handling and secret masking in logs and recordings.
- Hardened the custom-image self-test on runners, the runner keystore, and how a run's MCP harness choice is stored.
- Hardened member drive deletion, console session expiry, runner stream ownership and proxy host matching.
- Hardened certificate errors for configured gateways.

### Corrections to earlier notes

- A webhook audit sink whose `flush_interval` or `retry_base_delay` is zero or negative refuses to boot in
  best-effort mode too, not only when the sink is required.
- `wardyn runner remote propose` can suggest up to 1024 runs at once, but registration accepts at most 64.
- Owner-claimed runner registration shipped in 0.9.0 without a note: `POST /runners/register`, `/runners/tokens`,
  `/me/runners/tokens`, `/me/runners/{id}/claim`, `WARDYN_RUNNER_ORG_URL` and migration `0142_runner_registration`.
- A disk maximum set with no default becomes the soft default.
- Host capacity and the run cap count only this deployment's own substrates.

## [0.9.3-rc.2] — 2026-10-17

Laptop runners, second candidate: session recordings follow the organisation, an Azure DevOps credential follows the
run's own repository, and a dropped runner link no longer stops a run's egress.

### Before you upgrade

- Migration `0180_run_recording` adds `recording` to `runner_pools` and `runner_pool_org_defaults`, and
  `not_recorded` to `agent_runs`. On a split-role install, grant the runtime role what docs/OPERATIONS.md lists.
- A laptop runner now records a run only when its organisation asks. With the organisation's recording store off,
  the run is marked `org_off` and nothing is uploaded, where before the runner recorded anyway and its upload failed
  (`relay_route_refused`). `wardyn-runnerd serve --recording-store off` stays the runner's ceiling. A runner older
  than 0.9.3 keeps recording by its own setting.
- An upload for a run marked `runner_declined` is refused `409` `recording_off` unless it comes from a recording
  runner older than 0.9.3.

### Added

- An organisation or a runner pool can set `recording: required` (default `preferred`). A run that only a
  non-recording runner could take is then refused `runner_recording_off` (422). A run that is not recorded says why
  on its page (`org_off` or `runner_declined`). Console and SSH sessions opened through the organisation are still
  recorded while it keeps a recording store. See docs/operations/hybrid-laptops.md.
- A run that names an Azure DevOps repository but gets no per-person Azure DevOps credential says why: an audit row
  `run.ado_lane.skip` with a closed reason, `WARDYN_ADO_LANE_DECLINED` in the sandbox (printed by a failed clone),
  the console Credentials widget, and `wardyn run get`. Preview reports the same reason.

### Fixed

- A run's own `repo` now gets the per-person Azure DevOps credential, as a workspace repository does. Before, only
  workspace repositories were considered, and a run that named its repository directly got none, silently.
- A dropped link between a laptop runner and its organisation no longer stops the run's egress while the local
  audit spool has room. A full spool holds a request up to 10 s, then refuses it with a retryable `503`
  (`Retry-After: 1`) that names the link when it is down.
- A defaults write that leaves `recording` out keeps the stored setting.

## [0.9.3-rc.1] — 2026-10-16

Laptop runners in a locked-down enterprise: a corporate CA, errors an SDK can read, disk on VM engines, and images for
both architectures.

### Before you upgrade

- `wardynd` now refuses a `WARDYN_TRUSTED_CA_FILE` over 120 KiB at startup and warns above 64 KiB: every sandbox
  receives the bundle in one environment variable, which Linux caps at 128 KiB. Name a file holding only your
  organisation's roots, not a whole system bundle.
- A refusal on the brokered model routes, and a forward-lane egress deny, now answer with a JSON body and an
  `X-Wardyn-Reason` header. Clients keyed on `{"wardyn":…}` fields still find them.
- On a brokered route, or in a tunnel the proxy terminates (an LLM host, Bedrock, the AWS SSO portal), a TLS failure
  to the upstream (`tls_untrusted`) is 403, not 502, so SDKs stop retrying it; other intercepted hosts, and a gateway
  both vendors share (plain text), keep 502.
- On the AWS lane an egress deny is 403, an upstream fault stays 500, and a missing credential is 404 (401 only
  when a refresh fails).
- A runner pool that limits run types now refuses a run with no `--experience` as `runner_pool_run_type_missing`
  (422); `runner_pool_run_type_not_allowed` stays for a type the pool does not allow.

### Added

- `wardyn-runnerd --trusted-ca-file` (`WARDYN_TRUSTED_CA_FILE`, at most 64 KiB): a laptop runner's corporate CA. The
  run's proxy trusts it on create and revive, and the sandbox gets the same roots. `setup --trusted-ca-file` writes it
  into the service files. runnerd reads only the file it is given, never `SSL_CERT_FILE` or `SSL_CERT_DIR` (pass
  `--trusted-ca-file "$SSL_CERT_FILE"` to use yours). See docs/operations/hybrid-laptops.md.
- Errors on `/wardyn/llm/anthropic|openai` and the AWS lane use the provider's own error shape with a fixed reason
  (`egress_denied`, `tls_untrusted`, `upstream_unreachable`, `credential_unavailable`, `upstream_error`,
  `request_not_inspected`, `upstream_protocol_mismatch`) naming the host and the rule; `audit_record_too_large` and
  `audit_spool_unavailable` give a sentence instead. The reasons table is in docs/UI-SANDBOXES.md.
- `wardyn-runnerd setup` and `serve` measure free disk on Docker Desktop, Colima and Rancher/Lima with a probe
  container, and `setup` says when runs on the engine have no disk cap (overlayfs).
- The release checks that every published image has linux/amd64 and linux/arm64, and `wardynd:latest` is now
  multi-arch. An image running under emulation is named with its digest, its platform, whether the digest is an
  index, and the fix (docs/operations/build-images.md covers building the claude-code harness image for both architectures).

### Fixed

- The image-architecture probe is hardened.
- `WARDYN_RUNNER_STATE_DIR` is documented, and the env-doc guard now reads string-literal forms and `deploy/runner/*`.
- CI's base-image mirror restarts dockerd instead of a SIGHUP reload, which reset the host-gateway address.

## [0.9.2] — 2026-10-16

Laptop runners on private-endpoint estates: a self-hosted runner now gets the organisation's `internal_hosts`, so a
run reaches hosts that resolve to private addresses.

### Fixed

- A self-hosted (laptop) runner now receives the organisation's `internal_hosts`. Before, they were dropped for every
  local run, so a host resolving to a private address (PrivateLink, Private Service Connect, a zero-trust client's
  `100.64.0.0/10` range) was denied `builtin:private-ip` on a laptop even though the same declaration worked in the
  cluster. That included the AWS SSO portal a per-person Bedrock run renews through. A declaration still lifts only the
  private-IP guard for its suffix; the egress allowlist decides as before. The organisation's upstream proxy, trusted
  CA, `upstream_proxy_no_proxy` and LLM upstream overrides are still not sent.

## [0.9.1] — 2026-10-14

Self-hosted runners on people's own computers, from an adopter's first rollout: a per-person AWS SSO run now starts on
a laptop runner, `wardyn-runnerd` works with Docker Desktop, setup is one command, and the run page says where a run ran.

### Before you upgrade

- Hosting your own runner now needs the `self_host_runner` permission (migration `0178_self_host_runner_restriction`):
  `POST /me/runners/tokens` (`wardyn runner token`, My runner → Get a token) answers `403 capability_feature` without
  it. An estate where someone registered and claimed a runner for themselves keeps self-hosting open: the migration
  adds an `all` allow, so only people a `feature` deny already names, including a `*` deny, are refused. Narrow or
  remove it afterwards (Permissions → Self-hosted runners). An estate with none, including one whose runners an admin minted for people, starts
  closed: 0.9.0 cannot store the grant ahead of time, so add it right after the upgrade (people asking
  for their own token are refused until then). Existing runners and claims keep working, the admin mint is not asked, and admins are
  never asked.
- Runners registered before 0.9.0 still have no limits after an in-place upgrade and take no run. Find them with
  `wardyn runner list --all --no-limits` (self-hosted) and `wardyn runner remote list --no-limits` (remote-hosted), then
  `wardyn runner remote propose <id>` / `wardyn runner set <id> …`. Setup status now shows a Runner limits warning
  while any remain.
- With AWS SSO proxy-inject on (the default), `WARDYN_AWS_SSO_CONFIG_B64` now travels as a plain environment variable:
  it holds a placeholder `~/.aws` (start URL, region, account id, role name) and no token, so `docker inspect` and a
  Kubernetes pod spec show those values. With proxy-inject off it is still a secret.
- Upgrade the organisation before `wardyn-runnerd`: a 0.9.1 runner cannot register with a 0.9.0 organisation, and a
  0.9.0 organisation drops a 0.9.1 runner that reports an image it cannot run.
- A `runner.json` written by a 0.9.1 `register` carries the proxy image the organisation named, which a 0.9.0
  `wardyn-runnerd` cannot read: going back to 0.9.0 means registering the runner again (delete the state directory's
  `runner.json` and `runner.key`, then register with a new token).

### Added

- `wardyn-runnerd setup`: one idempotent command that registers this computer, creates the proxy bridge network,
  installs itself with a systemd user unit or macOS LaunchAgent, and starts it. With `--claim` and your own
  `WARDYN_TOKEN` it also claims the runner, and with `--pool NAME [--default]` puts it in your pool. Device
  management runs it without `--claim`; the person claims afterwards. `deploy/runner/install.sh` now just runs it.
- `wardyn-runnerd serve --relay-listen` (`WARDYN_RELAY_LISTEN`) and the new `--relay-host-target`
  (`WARDYN_RELAY_HOST_TARGET`): `serve` and `setup` find and prove the run relay's address with a probe container
  on Docker Desktop, Rancher Desktop (dockerd), Colima and other VM-backed engines, so Docker Desktop no longer
  needs a flag. `--relay-listen` loopback is accepted on any engine; the relay never binds an unspecified address.
  `setup` pins the pair it proved on a VM-backed engine, and `serve` now pulls the proxy image at startup.
  Rancher Desktop and macOS are expected to work and not yet verified; its containerd mode is unsupported.
- `wardyn runner claim --pool NAME [--default]` claims and puts the runner in your pool in one step; the console claim
  dialog does the same.
- `wardyn runner list --no-limits`, and an admin-only Runner limits row in setup status.
- The run page shows where a run ran — placement, runner name and pool name — for every run, and `GET /runs/{id}`
  adds `runner_pool_name` (`runner_name` now also names a remote-hosted runner). `run.create` audit rows record
  placement for every run, not only pooled ones, and name the pool and runner.
- `GET /me/runner-pools`: the pools you may use (for an administrator, without other people's user pools); the Go
  SDK gains `ListMyRunnerPools`. `claim --pool`, `setup --claim --pool` and the console claim dialog find your own
  pool by name through it.
- The runner is handed its proxy image at registration: the register answer names the one for the runner's reported
  architecture (`WARDYN_PROXY_IMAGE_AMD64` / `_ARM64`, else `WARDYN_PROXY_IMAGE`), `register` keeps it in
  `runner.json`, and `serve` uses it after `--proxy-image` and `WARDYN_PROXY_IMAGE` and before the release default.
  `setup` with no `--proxy-image` on an estate that cannot reach the public release image uses the organisation's
  answer. The token mint answer carries `proxy_image` unless per-architecture proxy images are set, and the console
  always prints the `serve` command.
- Sign in again on a stored AWS or Claude sign-in, in Your model connections and the New Run provider picker.
- A self-hosted runner reports its Docker engine's CPU architecture (`amd64` or `arm64`) at registration and on each connect (migration `0179_runner_arch`); the Runners tab, My runner and `wardyn runner list` show it.
- Per-architecture images for a mirror that publishes one architecture per tag: a `<key>@arm64` (or `@amd64`) entry in
  `WARDYN_AGENT_IMAGES` is used for a run on a runner of that architecture, and the plain key everywhere else; images
  a run names itself (BYOI, a devcontainer, a workspace base image) are used as given. Setup status adds an
  information row naming the keys with no image for the architectures your runners report.
- When an image's architecture differs from the runner's engine, the runner starts it once, without network or mounts,
  to see whether the engine runs it (Docker Desktop and Rancher Desktop can emulate). One that cannot run is refused
  at sandbox create with `image_arch` and a sentence naming the image, both architectures and the fix; one that runs
  is used, with a warning. Before, the run failed later with `exec format error`.

### Changed

- `wardyn-runnerd serve` exits at startup, with the reason, when this computer cannot run anything: Docker not
  reachable, a relay address no proxy can reach (proved with a probe container), or a proxy network it
  cannot create; a missing `--internal-network` bridge is created. Before, the runner showed Online and refused every
  run.
- `wardyn-runnerd serve --proxy-image` and `setup` default to the release proxy image matching the binary; a
  development build must pass it.
- A refused runner claim says why (registration expired, already claimed, revoked, organisation URL changed, key
  fingerprint). Another person's runner and an unknown id still get one generic sentence.
- `wardyn-runnerd register` says how to claim (the console fingerprint, or your own API token with `wardyn runner
  claim`; the admin token cannot claim), and after a timeout or a 5xx no longer says the token is still usable.

### Fixed

- `wardyn-runnerd`: an explicit `--relay-listen` was refused before it was read on engines without a host bridge
  (Rancher Desktop, Colima), and `setup` on such an engine passed, installed the service, and `serve` then
  crash-looped. `setup` now always proves the relay before it spends the token or installs anything.
- A per-person AWS SSO (`bedrock_sso`) run on someone's own self-hosted runner was refused after Launch
  (`bedrock_role_credentials credential is unavailable on a local runner`). It now runs when AWS SSO proxy-inject is
  on (the default); with proxy-inject off it is refused at preflight and Review, before Launch. A local Codex run with
  an MCP component was refused after Launch the same way; it now runs.
- A run refused for its credentials on the chosen pool now points at the Run step (runner and pool), and Review marks the group that
  needs attention.
- When AWS refuses to renew a sign-in (`invalid_request`, `access_denied`, `invalid_scope`, `unsupported_grant_type`),
  the run refusal names AWS's answer and opens the sign-in door, the console shows the sign-in as needing a new
  sign-in, and the `harness.credential.refresh` audit row carries AWS's `code` and `description`. Before, it read
  "launch again in a moment" with no way to sign in. A session stored without its client registration is no longer
  sent to AWS for renewal.
- An admin-minted runner token for a person's email could never be claimed: the owner is now resolved to the person,
  and an ambiguous or unknown owner is refused `422 runner_owner_invalid`.
- Revoking a runner whose session had already dropped answered `500`; it now answers `200` with `state: revoked`.
- `GET /me/runner-pools` answered `405`.
- `wardyn runner remote propose` on Kubernetes now says which read it could not make (ResourceQuota list, a quota
  that does not set an amount with `k8s.readNodes` off, or node list) instead of only "reports no CPU or memory".
- A provider sign-in bound to a self-hosted runner is refused at sandbox creation (defensive; no path binds one).

## [0.9.0] — 2026-10-13

### Before you upgrade

- Session, API token and SSH key revocation is now ordered by a database sequence instead of by comparing
  clock times; the upgrade adds that sequence and records the order of the API tokens and SSH keys that already
  exist. Sessions signed in before the upgrade keep the time comparison until they expire. During a rolling
  upgrade a revocation written by a replica that has not been upgraded is not ordered against a sign-in that is
  in flight, and tokens or keys it creates keep the time comparison: revoke again once the rollout finishes, or
  upgrade one replica at a time.
- Split-role installs (`WARDYN_PG_MIGRATE_DSN` set) whose app role does not pick up new tables through the
  `ALTER DEFAULT PRIVILEGES` step in Operations: grant the app role `SELECT, INSERT, UPDATE, DELETE` on `runners`,
  `runner_pending_actions`, `provider_connection_changes`, `runner_inventories`, `runner_pool_members`,
  `runner_pool_use_policies`, `run_queue`, `run_templates`, `template_group_admins` and `person_invitations`, and `SELECT, INSERT, UPDATE` on `run_template_revisions`, `template_group_labels`, `credential_delivery_policy`,
  `runner_registration_tokens`, `audit_delivery_cursors`, `runner_spool_cursors`, `runner_executions`,
  `runner_execution_exits`, `runner_inventory_candidates`, `runner_inventory_observations`, `runner_pools`,
  `runner_pool_org_defaults`, `remote_runners` (migration `0173_runner_pool_scope`), `secret_owner_generations`, `service_identities` and
  `api_token_moves`, `SELECT, INSERT` on `run_harnesses` (migration `0164_run_harnesses`), and `SELECT, INSERT, DELETE` on
  `run_launch_intents` (migration `0171_run_launch_intents`). Without the `runner_pools` grants wardynd refuses to start at the runner pool bootstrap.
  Without the `provider_connection_changes` grant every secret write fails once migration 0143's trigger on
  `secrets` is in place.
  Without the `run_harnesses` grant every run that records its harnesses fails to launch.
  Without the template grants (migration `0165_run_templates`) every template route answers `500`.
  Without the `run_launch_intents` grant every run launch fails.
- Credential erase fence (migration 0156): an older wardynd running beside the new one does not check a person's
  erase generation; finish the upgrade before erasing anyone.
- Migration `0145_audit_delivery_cursors` adds the `audit_delivery_cursors` table; nothing changes until a webhook
  audit sink sets `"delivery": "acknowledged"`.
- Migration `0143_provider_connection_changes` adds an empty, secret-free history table for why a model connection
  became unavailable. Removals from before the upgrade have no recorded cause.
- For filtered OIDC group claims, check the new transitive-membership procedure in Operations:
  Wardyn cannot detect groups omitted by the identity provider. Re-key affected grants and
  governance assignments to groups actually present in the claim (#1518).

- Migration `0140_audit_legacy_manifest` records the expected legacy audit partition, so verify detects
  its removal even when it held only hashless rows (#1809).
- Migration `0141_run_placement` records `agent_runs.placement`, `placement_filled`, `runner_id` and
  `evidence_source`. Existing rows retain empty placement and evidence, false `placement_filled` and a
  NULL runner; deleting a runner clears its reference while preserving the run. This records placement
  data without enabling local execution or choosing placement for unresolved runs (#116).
- Migrations `0147_runner_spool_cursors`, `0148_runner_executions` and `0149_runner_inventories` add the
  `runner_spool_cursors`, `runner_executions`, `runner_execution_exits`, `runner_inventories`,
  `runner_inventory_candidates` and `runner_inventory_observations` tables for 0.9 client mode (split-role grants:
  see the first item above). `0150_runner_action_binding` and `0151_runner_revoked_by` change existing tables only.
- Migration `0139_runner_delivery` adds the `runners` and `runner_pending_actions` tables, the
  `credential_delivery_policy` document and `credential_grants.delivery`, for 0.9 client mode and
  credential delivery (split-role grants: see the first item above). Nothing changes until runners are
  enabled.
- Migration `0138_audit_ensure_partitions_high_water` drops and recreates `audit_ensure_partitions`
  (the migrator's role must own the schema, as for 0111/0123); forward-only, rollback is the
  pre-upgrade `pg_dump`. Its EXECUTE grant is re-derived from `audit_append`'s: every role that can
  execute `audit_append` can execute it and PUBLIC cannot. A role granted only `audit_ensure_partitions`
  by hand loses it, and a role holding only `audit_append` gains it; repeat any hand grant after upgrading.
- Run `wardyn setup status --pre-upgrade` with your role settings in the environment (or
  `--role-map` / `--default-role`) and fix everything it lists. It reads only this shell's
  environment: for a chart or Compose deployment, load the env file first (`set -a; . FILE; set +a`)
  or check your values. It exits 0 when clean and 1 while anything remains (#623).
- Remap every `=member` entry (and a `member` default role) to `user` or a user type id, in the
  chart or env and under Getting started → People. 0.9 refuses to boot on one, before migrating (#623).
- Rename the six `WARDYN_MEMBER_*` variables to their `WARDYN_USER_*` names
  (`WARDYN_ALLOW_MEMBER_ENV_SECRET` to `WARDYN_ALLOW_USER_ENV_SECRET`, `WARDYN_MEMBER_MODE` to
  `WARDYN_USER_DESKTOP`); 0.9 refuses to boot while any is set (#623).
- Serve your OIDC issuers over `https://`. An in-cluster internal issuer behind a service mesh may
  set `WARDYN_OIDC_INTERNAL_ISSUER_PLAINTEXT=mesh` (#1970).
- Repoint any dashboard or SIEM rule on `auth.member_mode` to `auth.user_view.set` (#623).
- Migration `0146_audit_retention_drop_actor_type` replaces `audit_retention_drop` with a four-argument form so an
  admin-token drop records its actor type; no data changes (#1978).
- Migration `0153_run_experience` adds `agent_runs.experience`, the run mode a new client chose at launch; existing
  runs keep an empty value and their documented behaviour.
- Migration `0154_run_agent_stopped_at` adds `agent_runs.agent_stopped_at`; existing runs keep it empty.
- Migration `0157_invitations_service_identities` adds the `person_invitations`, `service_identities` and
  `api_token_moves` tables (split-role grants: see the first item above) and lets the audit log record a `service`
  actor. It refuses to run while any principal-keyed table already holds a name starting `service:`, naming the
  first such table; rename or remove those rows first. Widening the audit actor check briefly locks `audit_events`
  and its partitions.
- Migration `0159_run_lifetime_cap` adds `agent_runs.lifetime_cap_at` and `lifetime_cap_source`; existing runs keep
  them empty.
- Migration `0161_run_human_author` adds `agent_runs.human_author`, the principal accountable for a run at create (a
  record of accountability, not proof that a person was present); it stays the same when a service identity's
  sponsor changes. Existing runs keep it empty, and under a second-human switch a service identity's run without it
  is refused for every decider: decide such a run's pending approvals with the admin token, or let the run time out
  and launch it again.
- Config keys that hold a secret word (for example `CLIENT_SECRET`, `SECRET_FILE`, `*_IMAGE_PULL_SECRET`) are now
  refused in templates; move such values to a secret reference. Component config still accepts them.
- Proxy compatibility: a run carrying `push_rule_base` or `harness_credentials` keys needs a proxy that reads
  them. An older proxy refuses them under strict decode, so upgrade proxies together with wardynd.
- Migration `0162_run_queue` adds the QUEUED run state, `admitted_at` and the `run_queue` table; nothing changes
  for existing runs.
- Migration `0165_run_templates` adds `run_templates`, `run_template_revisions`, `template_group_admins` and
  `template_group_labels`; nothing changes for existing data.
- Migration `0168_runner_pool_owner` removes every pool of self-hosted runners made before it (they have no owner),
  with its members, the default slots naming it and any run waiting for it; those runs end `KILLED` with
  `runner_pool_removed` and get no `run.kill` audit row. A pool a finished run is bound to stays as a deleted
  tombstone. An organization default or preferred pool type that named self-hosted runners is cleared to none, never
  switched to an organization pool, and is now refused on write; until a person or an administrator sets a default,
  a launch that names no pool is refused `runner_pool_required`.
- Runner pools are now organization pools and user pools. The former Remote Provided pools become organization pools
  and keep their runners; the former Self-Hosted pools become user pools and keep their owners and runners. Migration
  `0173_runner_pool_scope` replaces `hosting_type` with `scope`, renames a first-boot pool called "Remote Provided" to
  "Organization", and keeps each saved default as the one pool it resolved to.
- A person no longer has a preferred runner type. A launch that names no pool uses the person's default pool, then the
  organization's; otherwise it is refused `runner_pool_required`. A launch's `placement` no longer picks a pool: a
  pool with no runner of that kind refuses the launch, `runner_pool_member_mismatch`. A saved default keeps the pool it
  resolved to; a default for the runner type you did not prefer, which `--placement` could pick, is dropped, and
  you then run `wardyn runner pool default set <pool>` before a launch with no pool is accepted.
- The server's Docker host or Kubernetes cluster is now a remote-hosted runner, with a name and an "Available to
  people's pools" setting (off by default). A person can add it to their own user pools only when an admin makes it
  available to everyone or to listed people, groups or roles. That setting is held for a second human under
  `WARDYN_GOVERNANCE_SECOND_HUMAN`.
- API, SDK and CLI clients move to `scope`; a member `kind` with `runner_id` or `remote_runner_id`; `pool_id` for
  defaults; the routes `/remote-runners`, `/me/remote-runners`, `/runner-pools/{id}/remote-runners/{rid}`,
  `/me/runner-pools/{id}/remote-runners/{rid}` and `/runner-pools/{id}/members`; and the run field
  `remote_runner_id`. The old fields and the `/executors/` routes are refused. Migration `0173` renames a key the
  held changes carry, so every pending pool change waiting for a second human (a use policy set or cleared, a
  runner added or removed) goes stale and must be proposed again.
- `wardyn drive set` now adds every grant in its file and never removes an allocation, so `drive get` then `drive
  set` changes nothing. In 0.8 a file that changed a subject's `drive_id` moved the subject; in 0.9 it adds the new
  drive beside the old one. Remove the old allocation with `DELETE /drives/grants/{id}` or in the console.
- `PUT /api/v1/runner-pool-defaults` replaces the whole document, so a PUT that leaves `barrier` out clears it; use
  `wardyn runner pool org-default set` (it reads first) or GET then PUT.
- Every runner now declares its limits: Runs at once, a total all its runs may hold at once, and what one run may
  hold, each in CPU, memory and disk. A self-hosted runner registered before this release gets Runs at once 2
  (migration `0172_runner_max_runs`) and no other limit (migration `0174_runner_limits`), so it takes no run, and a
  launch that could only go there is refused `422 runner_limits_unset`, until its owner sets them all: `wardyn runner
  set <runner> --max-runs N --total-cpu N --total-memory GB --total-disk GB --per-run-cpu N --per-run-memory GB
  --per-run-disk GB`. Register new runners with this release's `wardyn-runnerd`: it measures the computer and
  proposes half of it, and the server refuses a registration that declares no limits (`runner_limits_required`).
  Runs already going keep running and are counted.
- The server's Docker host or Kubernetes cluster gets its limits measured at the first boot. When an amount cannot
  be measured (often disk: on Docker Desktop, or when `wardynd` runs in a container) none is set, and runs there are
  refused `runner_limits_unset` until an admin sets them all with `wardyn runner set <id> …`. Read them with
  `wardyn runner remote show <id> --json`. A stack brought up with `make setup` is given its limits by setup; any
  other install sets them once: `wardyn runner remote list --no-limits`, then `wardyn runner remote propose <id>` and
  `wardyn runner set <id>` (or Runners → the runner → Edit limits).
- A launch can now wait. One whose pool is running its Runs at once, or whose runners are all at their limits, is
  stored `QUEUED` (`201`, wait reason `runner_pool_at_capacity` or `runner_pool_runners_at_capacity`) in every
  pool. A create that loses a runner's last room to another launch as it is stored is refused
  `422 runner_at_capacity`. API clients and scripts that create runs must handle a waiting run; `max_wait_sec`
  (`wardyn run --max-wait`) bounds the wait.
- Runners and pools on the wire: a runner view carries `kind`, `limits` and `usage`. `PUT /me/runners/{id}` takes
  `{"limits": {…}}`, the whole set, and refuses a bad one `runner_limits_invalid`. A limit that a runner bounds names
  the source `runner`. Each limit of a pool is optional, and one left out is set by the pool's runners; an empty
  `barriers` list allows every barrier they offer.
- A pending runner action can now end `refused` (migration `0175_runner_action_refused` widens the action outcome
  check): the runner answers an action it can never apply, such as a stop or kill naming a container it cannot prove
  it created, instead of failing its session on every reconnect. The action closes and `runner.action.refuse` is
  recorded.
- A repository locator, ref or target that contains whitespace or a control character is now refused where it used
  to be stored and then silently not cloned: in the default policy file at startup, in a stored or inline policy's
  `workspace_repos`, and in a workspace's repository source. A stored workspace with such a target answers 422
  `workspace_seed_source_target_invalid` when a run uses it. Edit the field before upgrading. Folder mounts and
  scratch paths with spaces are unaffected.
- On a deployment that sets `WARDYN_USER_WRITABLE_DENY`, a member's writable workspace on a directory that contains a
  denied path is now refused at onboarding and at run start. A writable root that contains a deny entry, such as the
  shipped examples' `<root>/.git`, can no longer be mounted writable as a whole: mount a project directory under it,
  or mount the root read-only.
- Under an allow-all ceiling, a member's own exact entry for a private-range address (RFC 1918, ULA, 100.64/10) is
  dropped at run create, with a warning, unless the ceiling lists it. This also applies to a stored policy an assigned
  member selects.
- A method proposal that shares no method with the ceiling now gets the ceiling's methods instead of every method.
- With second-person approval on, adding an `llm_inspection` that names a `detector_sidecar_url` waits for a second
  approver.
- A run started before the upgrade keeps its resolved policy on revive (revive does not re-resolve policy).
- `ui_apps: []` in an overlay now means "deny UI apps": the composed profile keeps the base's list and sets
  `deny_ui_apps`. Only a non-empty overlay list that shares no app with its base is refused. A base write that would
  leave such a descendant gets `409 governance_overlay_unsatisfiable`. Emptying a profile's own ceiling `ui_apps`
  waits for a second approver.
- With second-person approval on, renaming a profile in an equal-priority group tie waits for a second approver.
- An acknowledged role-mapping change already pending at upgrade carries no stored effect. Approving it gives `409
  governance_change_stale`, so re-propose it.
- Setting a CPU or memory maximum where a profile sets no size (or removing an authored size under a maximum) waits
  for a second approver when it raises the member cap. An overlay maximum above its base's cap is refused at write. An
  existing composition above its base's cap is lowered at resolve, with a warning.
- A run, record session or workspace scan of a member-owned workspace whose repository URL names a private-range
  address reaches it only where the ceiling or the workspace's approved egress lists the address with no port; an IPv6
  address can be listed only in the ceiling. An operator scanning a library source whose locator is a private-range
  literal now needs that address, with no port, in the deployment ceiling's allowed_domains; otherwise the scan run
  fails as a refused clone, with a log line naming the host. Names, public literals, GitHub hosts and operator-owned
  workspaces are unchanged, and no stored row is rewritten.
- Recording uploads to the Postgres store are now written one at a time per server process, across all runs, with
  session recordings on their own slot.
- One person's uploads wait behind each other before they wait with everyone else's.
- An upload that waits more than 65 s for its turn fails with the existing 500, where it used to queue.
- An upload whose body takes more than 60 s to arrive after the store admits it fails with the existing 500. Through
  the cluster proxy this does not happen, because the proxy has the whole body before it forwards; a relayed upload
  from a runner host needs about 4.5 Mbit/s for a 32 MiB part.
- wardyn-rec tries a refused mid-run part again after 10 minutes; only the final upload is not retried.
- The chart sets GOMEMLIMIT=400MiB for the server by default (`goMemLimit`); keep it at about 80% of
  `resources.limits.memory`. The value must be a quoted string with a unit, and the chart refuses anything else at
  render.
- Approve and deny requests now refuse a reason longer than 512 characters (400) and a body over 16 KiB (413). Clients
  that send longer reasons must shorten them. Existing stored reasons are not changed.
- A sign-in is refused with `auth_error=claims_overage` when a groups or roles claim the token did not carry could
  change the person's user type. A sign-in whose type is settled by the claims the token did carry is unaffected.
- The automatic SCIM purge removes a person only when the record shows that the person's own current suspension caused
  the deactivation. Every other scheduled deactivation is held, and the person stays deactivated.
- The Vault client follows a redirect only to the configured address's own scheme, host and port. With Vault HA, name
  the active node or an endpoint that forwards requests.

### Added

- A workspace image build pins its base image, devcontainer features and the Claude Code version it used; when the
  image is later missing, Wardyn rebuilds it from those pins and fails, naming the input, if one is gone. A build that
  could not be pinned is recorded as not reproducible (#1516).
- Governance → Runner credentials: a security operator sets, per credential class, whether an organisation credential
  stays in the organisation, is stored on a person's own runner (with the device posture the runner must report), or
  is not allowed, held for a second person where that is required.
- `GET /model-providers` also reports `entra_login_configured` and `kinds_available`; a `400` from `PUT
  /model-providers` for an Azure provider may carry `field`, the dotted path of the request field it is about. Azure
  Foundry stays switched off in this release.
- The runner pool list (`GET /runner-pools`) now says, for each pool you may use, whether it is available to you and
  why not, your own active runs against your profile's cap, the pool's limits as your runs get them, its barrier
  options and your runners in it; `?run_type=` narrows it to one run type (`400 runner_pool_invalid` otherwise).
  Preflight and the policy preview answer the same facts. The Go SDK gains `ListRunnerPoolsFor(ctx, runType)`.
  Preflight also answers `barrier_options` for the resolved pool, including `policy_floor` and `access_floor`, and
  `wardyn runner pool list` prints each barrier's reason. A pool's availability can read `placement_denied`,
  `placement_unavailable` or `runner_pool_no_online_runner`.
- Interactive runs: Claude Code now enforces tool rules (Ask first, Block it, and Run it for a named tool) through its
  locked managed settings, so a policy's named tool rules apply to interactive Claude Code runs too. If the rules
  cannot be written exactly, or the runner cannot deliver managed settings (including a krun runner that cannot vouch
  for them), the run is refused, never launched without them. A default of "Run it" or "Block it" is still refused in
  interactive runs. A policy's default for unnamed tools (`*`) does not apply there; Claude Code asks.
- A launch into a runner pool that is running its Runs at once, or whose runners are all at their limits, now waits
  instead of failing. It holds no credential while it waits. The oldest waiting run that can be placed starts first,
  once its owner's access, role and user type are re-checked, and only on a runner its launch could have used: one
  that enforces its barrier, takes its drives and is a kind it can run on. A run that cannot run on your own runner
  waits for the remote-hosted one. A run that has started no longer shows the wait reason. It gives up after 24
  hours (Background) or 1 hour (Interactive). `POST /runs` takes
  `max_wait_sec` (1 or more; a longer wait is cut to 604800 seconds, 7 days, with a warning; `0` is `422
  run_max_wait_invalid`). A waiting run can be cancelled, ends `FAILED queue_timeout`, and is cancelled with its
  pool, or when its owner's sessions are revoked or its service identity is retired. The queue sweep reports in
  sweep health as `run_queue`. A run pinned to one of your runners waits for that runner. A pool
  with no runner online that can take the run is still refused at launch.
  New metrics: `wardyn_runner_pool_queue_depth`, `wardyn_runner_pool_slots_used`,
  `wardyn_runner_pool_queue_wait_seconds`.
- New Run can include more than one harness in a sandbox (an interactive environment, or a command), each on its own
  model provider, started from the image an operator declares for the set (`make agent-image-harnesses` builds one,
  as a local build only). A published install declares none, so it refuses two harnesses (`harness_set_no_image`)
  until an operator adds one. An AWS sign-in model provider
  (`bedrock_sso`) serves only the harness a run starts, or else lists first; on any other harness it is refused with
  `request_field_unavailable`, naming the harness.
- `POST /runs`, `/runs/preflight` and `/runs/policy-preview` accept `policy_overlay`: a document's partial policy laid
  onto your default policy. Only the keys it names are bounded by your ceiling, so the default's operator mounts and
  grant pairings stay. It is refused beside `policy_id` or `inline_policy` (`policy_overlay_conflict`); a malformed
  overlay is `policy_overlay_invalid`. Launches write `policy.overlay.apply` (keys only), and the run's policy view
  names the `overlay` cause.
- GitHub CLI: an organisation's GitHub CLI row gives a run `gh` against its own GitHub repositories, as the
  organisation's GitHub App, through GitHub's REST API only (GraphQL commands such as `gh pr list` are refused with a
  pointer to `gh api repos/{owner}/{repo}/...`). The sandbox holds an inert `GH_TOKEN`; the proxy checks each
  `api.github.com` request against the row's capabilities and the run's repositories and adds a short-lived App token,
  revoked when the run stops (best effort, with expiry as the backstop). A row may not grant `repo_admin`,
  `org_admin`, `packages_write` or `actions_admin`; a run with no GitHub repository is refused with
  `cli_no_github_repo`.
- AWS CLI signed in as you: an AWS CLI component names an IAM Identity Center portal, an account, a permission set and
  a region. Each person signs in once (`POST /api/v1/components/{id}/sign-in`); the sign-in stays with Wardyn, and the
  proxy adds it only to the portal's role-credentials call for the pinned account and role. AWS issues role
  credentials into the sandbox, valid for the permission set's session duration and still valid at AWS after the
  run ends. Erasing a person's credentials removes their sign-in. With
  `WARDYN_AWS_SSO_PROXY_INJECT=off` the component is refused. Changing or deleting the component deletes everyone's
  sign-in for it.
- The `base`, `claude-code`, `codex-cli` and `full` images include `gh` and AWS CLI v2, each checked by digest.
- An organisation component can carry a CLI credential block (Azure, AWS or GitHub CLI), and `GET /me/components`
  lists five CLI cards (available, not turned on, or coming soon) without revealing rows the caller is not granted. A run
  can attach up to 16 components (was 8).
- A `wardyn/v1` document of `kind: Run` holds a draft of one run as its `intent`. `POST /api/v1/templates/import`
  reads it (or a `RunTemplate`) and answers the normalised document or diagnostics, storing nothing.
- New optional request field `shape` (`agent_task`, `environment`, `application`), checked against `experience`,
  `workload` and `tools`. A request that does not fit is `422 run_shape_conflict`.
- Administrators can let a self-hosted runner's runs use an organisation credential without it leaving the
  organisation (`via_org`): the runner forwards only those destinations, and the organisation re-checks and
  injects every request. Set per credential class with `PUT /governance/credential-delivery` (security tier;
  held for a second person when `WARDYN_GOVERNANCE_SECOND_HUMAN` is set); this release delivers header-injected
  API keys and the GitHub App broker. The byte limit (8 GiB a run) and the concurrent-stream limit hold per server
  replica and reset on restart.
- Under `WARDYN_AUDIT_SEAL=full` the `target` column of `person.attach`, `person.token.create`,
  `delegation.exchange`, `session.revoke`, `secret.list` and `auth.signin_unserialized` rows is stored as the
  person's subject, like the actor, when that person has an identity row, and a typed `subject:` string in an actor
  or target is refused on rows this organisation writes; a row a device forwards is shown as the device sent it and
  never resolved to a person. `GET /audit?target=<person>` matches both forms on rows this organisation writes and
  does not list a device-forwarded row that merely names the person's subject; a filter on the raw target string
  still finds it (#1828). A person named in `data` (`sub`, `target`, `secret_owner`), and the target of
  `person.create`, stay clear.
- Personal and shared secrets are named by route. `PUT`/`DELETE /me/secrets/{name}` (and `GET /me/secrets`) store
  your own secret whatever your role or view; `PUT`/`DELETE /shared-secrets/{name}` (and `GET`) store a shared
  service secret, admin only. Neither accepts `?owner=`. `wardyn secret set|delete` take `--personal` or `--shared`,
  and the SDK adds `SetMySecret`, `DeleteMySecret`, `SetSharedSecret` and `DeleteSharedSecret`. `PUT`/`DELETE
  /secrets/{name}` is unchanged and nothing moves between namespaces. The new routes' audit rows carry `scope`
  (`personal` or `shared`) (#1510).
- An erase of a person's credentials advances a per-person erase generation (migration 0156) before it deletes
  anything, and every stored-credential write is fenced to the generation it began under. A write that began before
  the erase is refused with `409` `credential_erased_during_write`, across processes and in store mode; a reconnect
  afterwards works. `credential.erase` reports `write_fence` (`generation` or `locks_only`) (#1511).
- Opt-in acknowledged audit delivery for the webhook sink (`"delivery": "acknowledged"`): it reads the stored trail
  from a durable per-destination checkpoint (starting at the current head on first enable), resumes after outages and
  restarts, and is at-least-once with `id`, `seq` and `row_hash` for deduplication. A redirect or a terminal 4xx halts
  delivery until wardynd restarts or another replica takes over. Retention past the checkpoint is never blocked: it
  records a chained `audit.delivery.reset` event, counts it and resends from the oldest retained row. Adds the
  `wardyn_audit_delivery_*` metrics (#1513).
- AWS KMS can hold the key that wraps stored credentials (`WARDYN_KEK=awskms`): each data key is wrapped by KMS
  Encrypt bound to its row, the key never leaves AWS, and IRSA or EKS Pod Identity is the expected identity (#588). A
  long-lived access key still boots but raises an amber setup row. An AWS KMS platform key, key domains and store mode
  are not supported yet, and boot refuses them together with `awskms`. The Helm chart takes `kek.provider=awskms`
  and `kek.awskms.key` and, like the other key services, needs no age key.
- Runners management API: `GET /runners` (active, revoked or all, with each runner's live online state and the runs on
  it), `GET /runners/{id}`, and the caller-scoped `GET /me/runners`, which shows a person only their own runners and
  abbreviates an unclaimed runner's fingerprint. Security admins can list and revoke unused registration tokens (never
  a token value); minting stays admin only. `runners.enabled` changes only through `PUT /runners/settings` (admin,
  audited `runners.enabled.set`, refused until the organisation URL is HTTPS); turning runners off ends the live
  runner sessions the serving replica holds. `PUT /site-config` now refuses a changed value. Unclaimed runners lapse
  and are swept after 24 hours. CLI: `wardyn runner list`, `wardyn runner tokens list|revoke`. Migration
  `0144_runner_minted_by` records who minted each runner's registration token (#1965).
- Runners can be revoked by an admin or security admin (`DELETE /runners/{id}`) or by their owner
  (`DELETE /me/runners/{id}`, optionally `?reason=not_mine`). Every unfinished run on a revoked runner ends KILLED
  in the same transaction and its credentials are revoked, and a live session on the replica that holds it is
  closed. Who revoked it and why are audited (`runner.revoke`) and shown on the runner views as `revoked_by` and
  `revoke_reason` (migration `0151_runner_revoked_by`). While runners are off, every new sandbox on a runner is
  refused before it reaches the runner.
- Runner pending actions bind the owner, run reference, execution generation and contained loss they were queued
  for; a result is accepted only from the queuing runner, as `success` or `deferred`, and a deferred result keeps
  the run fenced (migration `0150_runner_action_binding`).
- A reconnecting runner's pending actions, state report pages and action results are recorded before it is marked
  ready, and teardowns issued while a runner is offline are queued for it (migration `0149_runner_inventories`).
- `wardyn run --placement remote|local` and `--runner <id>` choose the kind of runner a run's sandbox lives on:
  remote-hosted, or one of your own self-hosted runners. Both are passed to the
  server unchanged, so its placement refusals reach the CLI as they are. `--pool <id>` names the runner pool.
- Model connections say why they became unavailable: never connected, removed because an admin changed the provider's
  address or kind (with the new destination host to review before reconnecting), or the credential store could not be
  read ("could not check"; the credential may still be there). The record is owner-scoped and holds no secret; it is
  cleared on reconnect, removed with the provider or by person erasure, and a provider recreated with a new identity
  inherits no history. Older clients keep the neutral wording.
- `GET /api/v1/setup/status` gains `tier` (`local-only`, `runner`, `org`); `wardyn setup status` prints it, the
  console account menu names it and the top bar shows a chip on local-only installs.
- Run template contract: reusable run setups published per person, organisation or group. A template is read strictly
  (unknown fields, secrets, run state, owner fields, titles and descriptions are refused) through one validator for
  text and structs, and its partial policy is laid onto the source policy rather than replacing it. Adds the
  `template_*` reasons, a declarative configuration schema for custom components, `wardyn template` and SDK methods.
- Run templates are stored. A person keeps a run setup for themselves, an admin publishes one to the organisation
  from the Admin view, and a person granted management of a group's templates publishes to that group. Every
  revision is kept, and an update names the revision it read, so a stale update changes nothing. Names are unique per
  audience; each person and each group holds at most 200 templates and the organisation 500. A template saved as
  `kind: policy` holds only run-wide network, credential and access-rule keys and never writes a stored policy.
- `PUT /api/v1/admin/template-group-admins` grants one known person management of one group's templates and can give
  the group a display label; a name that matches no person, or more than one, is refused. There is no console screen
  for it in this release.
- Erasure gains the `run_setups` scope: a person's own templates, their group grants and their name on shared
  templates. The SCIM purge asks for it. New reasons: `template_name_taken` (409), `template_limit_reached` (409) and
  `template_policy_field_refused` (422).
- The credential delivery policy gains `runner_resident`: an administrator can let a class of organisation credential
  be handed to a self-hosted runner for a run's lifetime, and only to runners whose reported device posture meets the
  class's requirement. The runner keeps the value in the OS keystore (the Secret Service on Linux, which needs a
  session bus; the login Keychain on macOS, written over stdin; values up to about 3 KB). Azure DevOps minted tokens
  can be opted in per connection. `runner_resident` values are not yet delivered to runners in 0.9.0: a run that
  needs one is still refused.
- Preflight (`PreflightResult.Access`) and the policy preview answer an `access` block built from one projection of the
  shared fold: what brings access (the harness, each Git provider's repositories, components, secrets, added hosts),
  each host's verdict against the resolved and clamped policy, every credential with where it is delivered and what is
  left once the run ends, what happens to an unlisted host and who decides it, and the setup still needed. A CLI
  component's row says whether its tool is in the run's image. The site-config SCM host list is never shown.
- The preview's policy now includes the hosts a run's `ssh_key` and `git_pat` grants open at dispatch, for the lanes
  create will actually wire, so a lane create drops (an SSH key for a harness with no SSH lane, a provider row that
  turns a lane off) shows no host and no credential. Preflight's `risk_assessment` can move from LOW to MEDIUM for those
  hosts; nothing newly refuses.
- `wardyn template group-admin list|grant|revoke` manages group template grants, and `wardyn template from-preset
  <name> [--everyone]` publishes a launch preset to the organisation as a template (a preset limited to user types
  needs `--everyone`; its `inline_policy` is laid onto the default policy instead of replacing it).
  `wardyn template import --kind Run|RunTemplate` checks either document kind, and `wardyn run --file <doc>` launches a
  `kind: Run` document, laying a document's `inline_policy` onto the default policy. `wardyn person erase --scope`
  names `run_setups`.
- New Run picks a run's harnesses and the run follows them: the harness that starts is the one chosen, with its own model
  provider; an optional starting prompt becomes its first message; Environment and Application runs launch with no
  harness on the agent base image, and a command may include harnesses. Each harness's agent permission, model provider
  and the caller's own credential for it are checked at launch, Review and the policy preview, and a refusal names the
  harness (`tool`). `GET /setup/status` reports `installed` and `approval_hold` per harness plus `harness_sets`;
  preflight and the preview report the run's `image` and `harness_set`. New Run runs record their harnesses in
  `run_harnesses`. Two or more harnesses in one run wait for a later lane.
- Operators can declare one image for several harnesses under a `WARDYN_AGENT_IMAGES` key that joins sorted harness ids
  with `+`; wardynd refuses to start on a malformed key.
- `GET /api/v1/workspaces/{id}/branches?repo=<repo>` lists one workspace repository's branches for the New Run
  branch picker, through the credential the run would clone with: Azure DevOps uses the person's own token or
  sign-in; GitHub uses the organisation's GitHub App with a one-repository `contents: read` token, revoked best effort after the
  read. A listing that fails answers `200` with `listing: unavailable`. Adds the reasons `workspace_repo_unknown` (422)
  and `branch_list_rate_limited` (429, 30 a minute per person) and the audit action `workspace.branches.list`.
- Runner pool contract: the run-create, preflight and policy-preview requests accept `runner_pool_id`. Preflight and
  the preview return `runner_pools`. `wardyn run --pool` and `wardyn runner pool list|default get|set|clear` are
  added.
- A run now lands in a runner pool: the request's pool, or the person's or the organisation's default; a missing or
  stale default is refused, never guessed, and a user pool only ever runs on the launching person's own runners and
  the remote-hosted runners made available to them. A run is created on, and every retry stays on, the target it
  was admitted against. Migration
  `0160_run_pool_binding` adds the pool binding to `agent_runs`; existing runs keep none. A request with no run type
  (`wardyn run` without `--experience`, or an older client) is refused `runner_pool_run_type_not_allowed` by any
  pool that has limits.
- `wardyn sync <run> <directory>` pushes a local directory over the audited SSH sync subsystem.
  `--pull` opts into receiving files; pulls refuse symlinks, unsafe names and executable modes,
  keep local edits on conflict and never delete files. `--watch` repeats the sync.

- New Run request contracts cover per-component overrides, workspace targets, placement, resources
  and allowed-image choices across create, preflight and preview. Fields whose behavior is not yet
  available are explicitly refused when they change the run’s posture.
- Super admins can unbind an Entra identity after app re-registration with
  `POST /api/v1/admin/identities/{id}/unbind`. It refuses while the old principal has live tokens, SSH keys
  or runs, cuts its sessions and records `identity.unbind` (#1818).
- Policy preview and preflight return per-entry provenance: the source that added, clamped or narrowed
  each resolved policy value. Saved-policy values removed by a ceiling remain hidden.

- **Operator-declared egress baseline.** `PUT /governance/egress-baseline` (security admin or admin; held
  for a second human under `WARDYN_GOVERNANCE_SECOND_HUMAN`) sets exact `baseline_hosts` and which
  `internal_hosts` entries are baseline. Both extend the built-in public set that the egress grade and the
  `api_key` CC3 floor key off, so an allowlist of only declared hosts no longer grades OPEN. Nothing is
  declared by default, so no grade changes on upgrade. Audited as `governance.egress_baseline.write`.
- **Sync over SSH.** A new `wardyn-sync` SSH subsystem runs the sandbox's own `sftp-server` in a validated
  start directory (`WARDYN_SYNC_DIR`, under `/home/agent/`), with its own cap of 2 channels per run, and is
  audited as `ssh.sync.transfer`. The start directory is a start point, not a boundary (#1952).
- `WARDYN_SSH_MAX_SESSIONS_PER_RUN` sets the per-run SSH channel cap (default 4, unchanged), and
  `ssh.sftp.transfer` gains `bytes_in`/`bytes_out`; `bytes` keeps its meaning (#1952).
- **Locked L2.** A governance profile's autonomy rubric accepts `agent_guardrail_locks`. A Claude Code run at
  L2 then launches under managed settings that add `allowManagedHooksOnly` and
  `allowManagedPermissionRulesOnly`, so a repository's own hooks and permission rules cannot answer a tool
  call, and `run.agent_policy.write` records `variant: l2_locked`. The managed-settings drift probe pins it on
  the pinned CLI.
- `wardyn setup status --pre-upgrade [--role-map CSV] [--default-role ROLE]` lists leftover
  `member` entries and removed `WARDYN_MEMBER_*` variables before the upgrade (#623).
- `wardynd -recovery-manifest`, `-recovery-verify` and `-recovery-rehearse` (#1514). The manifest ties a database
  snapshot to the audit spool and its sidecars, the key configuration (fingerprints and row counts only), recordings
  and drive bytes; verify refuses a mismatched pair. The rehearsal runs read-only on a scratch database named by
  `WARDYN_REHEARSE_PG_DSN`, refuses the live database, reads the set only inside its own directory, and proves
  pairing, the audit chain, that the configured keys open every stored row (Vault Transit and Azure Key Vault unwrap
  are not exercised by this release's tests), recordings and drive digests. It writes `recovery.rehearse.begin` and
  `recovery.rehearse` audit rows and exits 0 (pass), 3 (refused), 4 (fail) or 5 (incomplete). See
  docs/operations/recovery-rehearsal.md.
- Runner pool catalogue (migration `0152_runner_pools`). Admins create, rename, switch off and delete pools
  (`/runner-pools`), add remote-hosted runners to organization pools, and set the organization default (`/runner-
  pool-defaults`). A person adds only their own claimed runner to a user pool (`/me/runner-
  pools/{id}/runners/{runner}`) and sets their own defaults (`/me/runner-pool-defaults`). `GET /runner-pools` lists
  only the pools the caller may use, with availability and reason; a pool the caller may not use is a 404 like an
  unknown one. On the first boot after the upgrade, a deployment with remote-hosted runners gets one organization pool,
  named "Organization", holding them as the organization's default; it grants nobody a new runner or launch right.
- Pool-use policy: `PUT|DELETE /runner-pools/{id}/use-policy` (security admin or admin) narrows who may use an
  organization pool, held for a second human under `WARDYN_GOVERNANCE_SECOND_HUMAN` (governance change kind
  `runner_pool_use_policy`). Deleting a pool that has a use policy is refused until the policy is cleared; attaching
  or detaching a remote-hosted runner that a pool holds under a use policy is held for a second human; a leaver's
  identity is
  removed from every use policy (an emptied policy permits nobody); deleting a pool clears the organisation default
  that named it. Audit actions
  `runner_pool.*`, `person.runner_pool_default.set|delete`.
- With `WARDYN_GOVERNANCE_SECOND_HUMAN` on, creating a service identity (`POST /service-identities`) and a
  `PATCH /service-identities/{name}` that changes its sponsor are held for a second security-tier human (governance
  change kind `service_identity`). The PATCH is held whole. A description or suspend edit, unsuspend and retire
  apply directly. An approval applies only while the name is free and the sponsor and type are valid, or while the
  identity and its sponsor are as reviewed; otherwise `409 governance_change_stale`.
- New Run run mode: a request chooses `experience` (background or interactive), with its workload, included tools,
  one startup choice, one starting folder and an explicit "no repositories or drives". The CLI's run-mode flags
  mark requests `mode_contract: "0.9"`, so an unset mode is refused instead of inferred. The console sends the run
  mode (`experience` and `mode_contract: "0.9"`) for every shape the contract can say, so its runs start in pools
  that limit run types; older requests keep their documented behaviour.
- Push rules are carried per source-control entry (provider host and canonical organisation; a remote that cannot be
  read exactly is refused) and tool rules per included harness; a request's rules can only narrow. A background task
  has no idle stop from any source, and the idle reaper skips it. Presets refuse run-mode fields until run templates
  replace them, and the proxy refuses rules carried two ways.
- `wardyn governance plan [file] [--prune]` and `client.PlanGovernance` preview the creates, updates and deletes an
  apply would make, without writing (#1515).
- `GovernanceApplyResult.Steps` and `client.ApplyDrivesResult`: a failed `governance set` or `drive set` lists which
  writes were applied (or held for a second human) before the failure; applies stay sequential and not atomic
  (#1515).
- `client.GetModelProviders` (admin) reads the provider records and connected-people counts; it returns no
  credential value (#1515).
- A self-hosted runner relays its runs' calls to the organisation's internal listener as opaque TLS: the runner
  binds only its Docker host-gateway address and forwards the TLS bytes without terminating them, each relayed
  request is bound to the runner that carried it, only a closed list of internal routes is served, and only the
  person's own credentials can be read through it. Turning runners on now also needs `WARDYN_CONTROL_PLANE_URL` set
  to the internal TLS listener, since a runner's runs reach the organisation only through that relay.
- A background task cannot be entered: its terminal, interactive exec, every SSH channel (shell, exec, sftp, `wardyn
  sync`, port forwarding) and its web apps answer `409 run_background_only` (SSH refuses authentication), for every
  caller including a super admin. Logs, output, audit and the lifecycle controls stay.
- An admin or security admin can invite a person by their identity-provider subject (`POST
  /people/{principal}/invitations`); only that person, signed in, can claim the invitation, and the API token it
  mints is shown once, to them alone. Invitations can be listed and revoked before they are claimed, and each one
  expires.
- Runner pools carry limits per run type: CPU and memory (default and cap), a main-drive cap (`disk_mib`, whole GiB),
  lifetime (default, maximum or unlimited), an idle stop for interactive environments only, allowed barriers and
  Runs at once across the pool (`max_concurrent_runs`); an administrator sets them on an organization pool, a
  person on their own user pool (migration 0158). Each one is optional: a pool only narrows its runners, and a limit
  it leaves out is set by its runners. A run is admitted by its pool's run types and barriers, the pool's CPU,
  memory and lifetime size it, and a launch waits at the pool's Runs at once; the idle stop is stored and audited
  but not yet applied to runs.
- `wardyn-runnerd` ships as a release binary for Linux and macOS (amd64, arm64) with a per-user service: a systemd
  user unit or a LaunchAgent, installed by `deploy/runner/install.sh`, which registers the runner with a single-use
  token and keeps it running; the owner then claims it with `wardyn runner claim` (#1968).
- Governance profiles can deny runs on self-hosted or remote-hosted runners (`deny_placements`); a run's barrier
  and capabilities are now checked against the runner it actually lands on, and an offline runner is refused rather
  than guessed. Create, preflight, policy preview and source scans refuse a denied placement with 403
  `placement_denied`.
- `POST /runs` and `wardyn run` can set a run's end at launch (`ends_after_sec`, `--ends-in`), or No end where every
  limit allows it (`no_end`, `--no-end`). An end past a limit is cut to it and the response names the limit. An
  untouched run whose request says `experience: interactive` (the CLI's run-mode flags, the SDK, and the console
  where it sends the run mode) now ends after 24 hours when no pool, profile or deployment names a lifetime. A
  request that carries no `experience` keeps today's end. `GET /runs?no_end=1` lists live runs with no end.
- A run's CPU and memory are bounded by the strictest of its pool, the person's governance profile, the deployment
  and the per-run limit one of its pool's runners declares: a runner that takes the whole ask, else the largest. A
  larger ask is reduced to a size that runner takes whole, never refused; a runner smaller than the ask no longer
  refuses the run. `POST /runs/preflight` and the policy preview return `limits` and `resources_applied` (requested,
  applied, capped, and the limit that bound it with its name); the create response and the `run.create` audit row
  carry `resources_applied`.
- A run can carry a built-in GitHub component, `components[]{builtin: "github", org, repos, access}`, without a
  workspace. It reaches only the listed repositories, through Wardyn's git broker. Each repository must pass workspace
  admission and the `workspace_provider` capability, and access must be within the caller's ceiling for a
  `github_token` grant, which also sets approval and lifetime. Repository names are lowercased; a repository listed
  twice is refused. Reviving or extending such a run re-checks the owner's `workspace_provider` permission over its
  repositories. Review and the policy preview show it, `run.create` and `run.component.attach` record it, and a
  push-rule override for its organisation applies to it. A built-in Azure DevOps component is still refused with
  `request_field_unavailable`. New refusal
  reasons: `component_builtin_lane_off`, `component_builtin_access_above_ceiling`.
- A run can attach several workspaces, each at its own path, and pick which repositories of a workspace it clones and
  at which branch, tag or commit (a ref git could read as an option is refused). Equal or nested paths across them,
  two model-provider pins, two base images, or two repositories in different Azure DevOps organisations that no one
  workspace carries both of are refused before anything is granted. `start_folder` can name a repository of an
  attached workspace. Paths under `/home/agent/drives` and `/home/agent/shared` are reserved for drives. New reasons:
  `workspace_selection_invalid`, `workspace_repo_unknown`, `run_ref_invalid`.
- A run's harness set credentials each harness through its own model provider. Dispatch authors one lane per harness,
  refuses two credentials for one host, and the injection sinks accept only a harness and provider pair the run was
  launched with. Revive and the owner re-checks keep and re-check every harness's provider. Claude Code's managed
  settings are delivered whenever it is in the set. New sandbox env: `WARDYN_HARNESSES`, `WARDYN_START_HARNESS`.
- New Run console: a Start screen with three paths (AI composer coming soon, Guided setup, YAML or JSON); a Shape step
  that starts from scratch, a saved template or an earlier run and picks the workload; a Run document screen that
  edits the draft as YAML or JSON, checks it with the server and reports every unsupported field instead of dropping
  it; and a Save as template / Save as policy dialog (Only me or a group you manage, the whole draft or a subset).
- Templates screen: `/templates` (User view) and `/admin/templates` (Admin view) list saved starting points with
  who-it-is-for, kind and name filters. Use opens New Run on the pinned revision; Rename and Delete follow who may
  change each row; organisation admins publish a template to the organisation from the Admin view.
- New Run's Harnesses step lists the server's harnesses with their real state: installed, enabled, their model
  provider, and which combinations an image carries. An interactive run starts in at most one harness with an
  optional starting prompt, an autonomous run holds one harness and its task, and an environment or application can
  include optional harnesses.
- New Run's Files step sizes the main drive in GiB against the pool's cap and says what enforces it, picks a branch
  for each repository, and attaches a drive of yours, read-only if you wish. An Environment sandbox opens with your
  drive attached when it can be.
- New Run's Tools and access step lists what your harness, repositories and drives bring, and the hosts and
  credentials the run can reach; hosts read "Requested" until the server has checked them. You finish setup in
  place: choose a model provider, sign in, or add a token. Setup left unfinished is a blocker at Review.
  - Harness setup: each harness's model provider, what happens to a tool with no rule, rules for named tools, and
    MCP servers per harness. Choices your policy or the harness rules out are greyed with the reason.
  - Add tools and access: the CLIs, MCP servers and APIs your organisation grants you, one of your saved secrets (as
    a request header, an environment variable or a file), one more host, a remote MCP server by URL, or a custom
    component.
- New Run: Launch opens a "Name this run" dialog that checks the setup again when it opens; a refusal shows in the
  dialog and keeps what you typed. A launched run lands on a screen that survives a reload and shows Starting, or
  its place in line, when it gives up, and Cancel run while it waits.
- A person's tool rules and default on a New Run combine with the policy's as the stricter of the two for each tool.
  "Run it" is offered where the policy allows the tool and greyed with the reason where it does not; a rule can be an
  exception to the person's own default, never to the policy. Tool rules name one tool exactly (`Bash`, `WebFetch`,
  `mcp__server__tool`); patterns are refused. Codex CLI: "Ask first" and blocks on its own tools are refused with a
  reason, and rules for its MCP tools are kept in its rule set. Rules for a harness without Wardyn's tool gate, and
  rules in an interactive run beyond its own "asks you", are refused. New Run's access facts include a tool policy for
  each included harness.
- A run's main drive is sized from one rule at Review and at launch: `resources.disk_mib` on a request is reduced to
  the strictest of the pool, your governance profile and this server's maximum, and the run says so. A run that asks
  for no size gets the strictest of the pool, profile and server defaults as a soft size; a host that cannot cap
  disk, such as overlay2 over ext4, runs a soft size uncapped. On Kubernetes this adds an ephemeral-storage limit to
  CLI and CI runs that had none. A size the run's policy writes is a requirement that a request may only lower; when
  the run's own pool cannot cap disk, such a run is refused with `main_drive_unenforceable` at Review, the policy
  preview and create. The Policy tab tells a pool default from a server default, and `run.policy.resolve` records
  `disk_mib_basis` and `disk_mib_default_source`.
- Remote MCP servers can be added as components. A harness reaches a server only through the proxy's broker route
  (`/wardyn/mcp/<token>/<server>`), which binds the request to the harness by its own token, adds the server's
  credential, answers a tool call the run's rules block with a JSON-RPC error, and re-originates over the same
  guarded path as the git broker, refusing redirects and any data after the JSON-RPC message. While a server is
  attached, a direct request to its host is refused. Two organisation MCP servers may not share a name (`409
  component_name_conflict`). SSE servers are not supported yet. Claude Code gets the servers in its managed MCP file
  (interactive) or a second `--mcp-config` (autonomous); Codex CLI gets `[mcp_servers]` url tables. Migration
  `0166_run_component_mcp_harnesses`.
- A service identity's sponsor can see and stop its runs, in every runs list view; entering one needs an explicit
  `service_attach` grant, is recorded as the sponsor, and is never available over SSH. A portal's delegated token
  gets none of this. Migration `0167_service_attach_restriction` turns the grant off for everyone until an admin
  names it; an allow written for everyone admits every sponsor.
- The pool list and the preflight and policy-preview `runner_pools` now carry `main_drive` for each pool you may
  use: how the pool's runners enforce, keep and mount the main drive, as the weakest across its targets (`unknown`
  when a runner is offline or has not reported).
- Fleet capacity and a run's recorded sizing now count the 400 MiB proxy envelope of a run that carries an Azure
  route gate; they previously recorded the 256 MiB default while the runner applied 400 MiB (#1806).
- The pool list, preflight and the policy preview say how busy each pool you may use is (`slots`: used, waiting,
  limit, a verdict and the default wait); counts only, and if they cannot be read the answer comes without them. A
  waiting run carries `queue` (its place, reason and deadline) on create, `GET /runs/{id}` and `GET /runs`, and
  `wardyn run get` prints it. The Go SDK gains `RunQueued`, `AgentRun.Queue` and `PoolSlots`. `wardyn
  run --max-wait <duration>` sets how long a run may wait for a slot; with `--wait` the server is told `--timeout`
  minus 60s, so a run that waits too long fails with `queue_timeout` (exit 1) before `--timeout` gives up (exit
  124), and `--wait` prints a waiting run's place as it changes.
- Runs on a self-hosted runner are leased to it: the runner refuses to create or execute a run without the
  organisation's current lease and execution generation; token renewals and end changes extend the lease, a runner's
  new session is leased again, and reviving such a run starts its agent under a fresh generation. A self-hosted
  runner takes only its owner's runs, and a user pool of them is available while one of them is online.
  A barrier, size or idle-pause demand that none of the person's runners meets is refused with the pool's
  `placement_capability` sentence, and asking for another person's runner answers `runner_not_found`. The
  organisation's GitHub App token reaches such a run only through its git broker, once an administrator delivers
  `github_token` `via_org`; by default that grant is refused there. A person's own stored key or PAT is delivered
  as on any run.
  deny_placements is also held at dispatch and revive; managed-harness sign-in runs stay exempt at every door, and
  site-config probe runs at create and dispatch.
- User pools are each person's own: a person creates, edits and deletes their pools
  (`/api/v1/me/runner-pools`, `wardyn runner pool create|update|delete|show`), up to 10, with names unique per
  person, and puts their own claimed runners in them (`wardyn runner pool add|remove <pool-id> <runner-id>`). Only
  the owner sees or uses one; anyone else gets the unknown-id `404`. Administrators list every pool read-only with
  its `owner`, create organization pools only, and the organization default names an organization pool only. A
  new erasure scope, `runner_pools`, also runs in the SCIM purge.
- Console: Admin gains a Runners page (every registered computer with its state, last seen and what it reported,
  revoke, unused registration tokens, a mint for a named person, the runners switch), and Your account gains a My
  runner card to register, claim with the fingerprint read on that computer, and revoke. A run on a registered
  computer names it, says when it is offline (unknown, not offline, when another server copy holds its connection),
  explains a revoked runner's runs, and marks egress decisions recorded while it was offline; a remote run's
  Identity shows what `wardyn sync` moved. API: a claimed runner's view carries `capacity`, `classes` and
  `allowed_roots`; `GET /me/runners`, and so `wardyn runner list`, keeps a runner someone else revoked for seven
  days with `revoked_by_email`; both runner-token mint answers carry `org_url`; runs on a registered computer carry
  `runner_name`, `runner_online` and `runner_last_seen_at`.
- A run can attach several drives: `GET /api/v1/me/drives` lists the drives your grants reach, each with its path,
  and `POST /runs` takes `drives: [{drive_id, read_only?}]`. Your top-ranked drive stays at `/home/agent/drive`; the
  others mount at `/home/agent/drives/<slug>` or `/home/agent/shared/<slug>`, and a drive that cannot mount refuses
  the whole run. `POST /drives/grants` with `additional: true` allocates a drive beside a subject's others; without
  it, allocating still replaces the subject's drive, and concurrent replaces leave one (migration
  `0169_user_drive_grants_per_drive`). New refusals: `drive_selection_conflict` and `drive_invalid` (400);
  `drive_not_found`, `drive_not_in_pool`, `drive_target_conflict` and `drive_share_beside_managed` (422), the first
  three audited as `authz.denied`. `run.drive.mount` is written per drive with `drive_id` and `target`, and
  `drive.grant.write` carries `removed_drive_ids`. Docker's orphan sweep removes leftover drive seed containers.
- New Run: the Files step lists a repository you cannot launch (its workspace is not available to you, or its host
  is not an enabled git provider) disabled with the reason instead of hiding it, and those launch refusals link to
  Files; the Tools step's model-provider picker shows the provider the server has already chosen (the default or the
  only one) with how its credential is delivered, and asks for a choice only when one is needed; Review lists the
  server's preflight warnings in one notice and shows a read-only Recording row under Where it runs.
- An administrator can set the organisation's default barrier (`wardyn runner pool org-default set --barrier CC2`,
  or `PUT /api/v1/runner-pool-defaults` with `barrier`). A launch that names no barrier gets it when it is at or
  above the caller's floor, the pool allows it and its runners offer it; otherwise the strongest barrier offered at
  or above the floor
  applies, as before. The pool list's default says who chose it (`default_source`), `run.create` records
  `confinement_default`, and a run that waits for a pool slot keeps the barrier its create resolved
  (`run.queue.start` records it). A person's own pool defaults cannot set a barrier (400). Server-originated
  launches such as source scans keep the strongest class. New migration `0170_runner_pool_org_default_barrier`.
- Run again: each run keeps the setup it was launched with as a `kind: Run` document, readable only by its creator
  (`GET /api/v1/me/launch-intents`, `GET /api/v1/runs/{id}/launch-intent`), including a run that waits in a pool's
  queue. The document leaves out what is chosen at each launch (title, description, `max_wait_sec`) and where the
  request came from; `policy_overlay` becomes `intent.inline_policy`. A run whose request sets a whole
  `inline_policy`, or whose document the import door refuses, keeps none but still starts, and `run.create` records
  `launch_intent` and `launch_intent_reason`. Erasure's `run_tasks`, `components` and `run_setups` scopes, and so
  the SCIM purge, delete a person's launch intents. Migration `0171_run_launch_intents` adds `run_launch_intents`; a
  split-role install grants the app role `SELECT, INSERT, DELETE` on it.
- New Run: the Tools step offers Sign in to Azure DevOps when you have no Azure DevOps sign-in stored, and Review
  never holds Launch behind an item the Tools step cannot show; an ended sign-in, an own-token row or a missing
  console app is reported when you launch. The policy preview checks only whether a sign-in is stored, by name, and
  never reads it.
- The Go SDK and the CLI now cover invitations and service identities: `wardyn person invite`, `person invitations`,
  `invitation revoke`, `service-identity create|list|get|update|retire`, `service-identity token list|revoke` and
  `token move`. There is no CLI claim and no CLI service-token mint, because both need a signed-in browser session.
- Your account lists the invitations waiting for you and lets you claim them, and lists the service identities you
  sponsor with a mint, a list and a revoke for each one's tokens. A new token is shown once and is held nowhere
  after the dialog closes. Admin > Credentials gains a State column for tokens an admin created for someone else and
  a toggle that adds the revoked ones. A moved token's row names its service identity, and minting a token for a
  service identity whose sponsor is not active is refused like a suspended one.
- `POST /api/v1/workspaces/{id}/build` with `{"refresh": true}` rebuilds a workspace's image from fresh inputs,
  pulling a wrapped base image again, and swaps it in only if no other build replaced it meanwhile. The
  `workspace.image.refresh` audit row names who asked and what changed. A build started from the build door that
  loses that race removes its own image and reports `superseded`.
- New Run step 2 (Run) shows the pools you can use for the run type, each unavailable pool with its reason, the
  pool's capacity, your allowance, waiting, run types and per-run limits (with where each limit comes from), and the
  three barriers with the reason one is unavailable. Schedule For Later and Application / server are marked Coming
  soon.
- `wardyn workspace build <workspace-id>` starts a workspace's image build, rebuilds it from fresh inputs with
  `--refresh`, and prints the current state with `--status` (`--json` for the raw view, including whether the image
  is reproducible). The Go SDK adds `BuildWorkspace` and `GetWorkspaceBuild`.
- Runner limits: every runner, self-hosted or remote-hosted, declares Runs at once (1–64 on a self-hosted runner),
  a total and a per-run CPU, memory and disk. `wardyn-runnerd register` measures the computer and proposes half of
  it (`--max-runs`, `--total-*` and `--per-run-*` override). Only its owner changes a self-hosted runner's (`wardyn
  runner set`, `PUT /me/runners/{id}`, audited `runner.update`), and an admin a remote-hosted runner's (`wardyn
  runner set`, `PUT /remote-runners/{id}`, audited `remote_runner.update`). Placement never passes a limit: a run
  whose runners are all at theirs waits with `runner_pool_runners_at_capacity`, then starts on a runner it could
  have been launched on that has room, at the size it would start with there,
  and a revive onto a full runner is refused `runner_at_capacity`. Runner, remote-runner, pool-member and pool reads
  carry the limits and `usage` (in use, available, reported, `disk_capped`), and a pool row adds up its runners as
  `capacity`. Runs the server launches itself (sign-ins, probes, scans) count on the remote-hosted runner when the
  deployment has exactly one, and are not held to its limits.
  Runners report their machine name, runner views list their pools, and `GET /runner-pools/{id}/members` lists a
  pool's runners. `wardyn runner pool create|update` take `--run-type`, `--barrier`, `--max-runs`,
  `--<run type>-cpu|-memory|-disk|-lifetime DEFAULT:MAX`, `--interactive-idle` and `--limits-file`. Migrations
  `0172_runner_max_runs` and `0174_runner_limits`.
- A user pool can mix your own runners with remote-hosted runners your admin made available to people's pools. A run
  that names no placement goes to one of your own runners when one has room, otherwise to a remote-hosted runner;
  `placement: local` or `placement: remote` (or a `runner_id`) fixes the kind, and a pool with no runner of that
  kind is refused `runner_pool_member_mismatch`. A run with drives goes to the pool's remote-hosted runners. A
  queued run that took your runner's last place keeps it when it starts.
- Azure CLI signed in as you: an Azure CLI component names the subscriptions a run may reach and whether it may
  change them. Each person signs in once with Microsoft Entra (`GET
  /api/v1/components-entra/signin?component=<id>`); the sign-in stays with Wardyn. The sandbox holds an `~/.azure`
  profile and a placeholder token cache; the proxy checks each `management.azure.com` request against the
  component's subscriptions and access (`read` is GET and HEAD only) and adds a short-lived token. No Azure token is
  placed in the sandbox. Resource Manager commands only: Storage, Key Vault and Microsoft Graph are not covered, and
  a resource path with `;`, `?`, `#` or `%` in a name is refused (Azure Backup items and API Management revisions
  among them). It needs the console to sign people in with Microsoft Entra, and that application to hold the
  delegated permission "Azure Service Management · user_impersonation" with admin consent. Changing the component's
  subscriptions, or deleting it, deletes everyone's sign-in for it.
- The `full` and `harnesses` images include the Azure CLI (`az` 2.91.0), from Microsoft's apt repository with its
  signing key checked by fingerprint and the version pinned.
- Runners and pools in the console: Settings, Runners lists every runner with its limits and what is in use; the
  Pools tab lets an admin create, edit and delete organization pools, set who may use one and choose the
  organization default, and shows each pool's runners and limits. Your account shows your own runner with its limits
  and pools, and My pools creates and manages your own user pools and adds your runner to them. `wardyn runner pool
  use-policy get|set|clear` does the same from the CLI, and `wardyn runner pool show` prints who may use a pool and
  the limits it sets.
- `make setup`, `make demo`, `make test-drive` and `scripts/ci-run.sh` give the server's runner its limits once it
  is up: half of what the host has, shown before they are set. Where the server cannot see the disk (Docker Desktop,
  or `wardynd` in a container) setup asks for the free disk at a terminal, or reads `WARDYN_SETUP_RUNNER_DISK_GB`,
  and stops naming the variable when neither is there; `WARDYN_SETUP_RUNNER_LIMITS` sets the whole set. Nothing is
  defaulted. New: `wardyn runner remote propose <id> [--free-disk GB]` prints the limits registration's rule gives a
  remote-hosted runner, as `wardyn runner set` flags, and sets nothing; `wardyn runner remote list --no-limits`
  lists the runners that take no run until they have limits. The kind quickstart reads the cluster's nodes
  (`k8s.readNodes`), so the server measures the cluster itself.
- Migration `0177_member_drives` adds `user_drives.owner_subject` and `delete_started_at` for people's own drives.
  Admin drives keep their global name rule. A person's drive names are unique only within their own drives. A write
  that gives a person's drive a host path, a pre-provisioned claim, a claim-derived home or a slug-named object is
  refused.
- Admins can let people create their own drives with `storage.user_drive.member_create` (off by default). It takes a
  drives-per-person limit of 1 to 20, and on Kubernetes a claim size and storage class. A governance profile can still
  deny it with `deny_member_drives`.
- On Docker, a size or storage class on `member_create` is refused: a Docker volume has no size limit.
- Runners can now report whether they can delete the drives they would create (CanManageDrives): Docker always,
  Kubernetes via a SelfSubjectAccessReview for get/create/delete on claims in the runs namespace, failing closed.
- People can create their own drives from New Run when an admin turns on `storage.user_drive.member_create` (off by
  default). Each person gets a set number of drives. Deleting one deletes its files (`POST /me/drives`, `DELETE
  /me/drives/{id}`).
- Super admins list and delete people's drives with `GET /drives/member`, `DELETE /drives/member/{id}` and `wardyn
  drive member list|delete --yes`. `GET /drives` no longer lists people's drives, and the admin drive, allocation and
  reclaim routes refuse one with `409 user_drive_member_owned`.
- On Kubernetes, member drives need `drives.reclaim.enabled`; without it, creating one is refused.
- A person's own drives appear in their New Run drive list (`GET /me/drives`, `owned: true`) and attach through
  `drives[]`. Only the person who created a drive can see or attach it, never as the legacy `drive.enabled` drive.
  `GET /me/drives` adds a `create` block: whether you can create a drive now and why not, with your count and limit.
- A person's own drive never runs on a self-hosted pool: a run that would land there is refused with
  `drive_not_in_pool` and nothing is minted. One deleted while its run waits or starts fails the run with
  `drive_not_found`.
- An organisation or group template can't include a person's own drive (`template_shared_field_refused`).
- Erasing a person (`POST /people/{principal}/erasure`) has a `member_drives` scope that deletes the drives they
  created, with their files. A drive a run still holds fails the scope, naming it; retry once the run ends. A SCIM
  purge lists those drives in `person.deprovision` (`member_drive_ids`) and in `GET /scim/status`, and deletes none.
- Console: admins can let people create their own drives and set drives per person under Settings -> Storage, and deny
  it per profile with "Can't create drives" in the profile editor.
- Console: the Files step of New Run has "+ Create user drive" for a person's own drive and "Delete drive" for one
  they created.

### Changed

- New Run refuses a harness that no sandbox image on this server carries (`harness_not_installed`). A published install
  maps Claude Code to the agent base image, which does not include the Claude CLI, so New Run refuses Claude Code until
  an operator builds one (`make agent-images`, deploy/images/README.md) and registers it under `claude-code` in
  `WARDYN_AGENT_IMAGES`. A run that names its own image is not refused by this check.
- Under a governance profile whose tool rules hold or deny, a background command that includes a harness is refused
  (`governance_profile`, `runs.tools`).
- Security: with two or more harnesses, the tool-approval credential now reaches only the harness Wardyn starts,
  as a file under `/run/wardyn/secrets/` that its own launch reads; it is no longer container or pod env. A harness
  started by hand, or any other process, is given none, and its tool calls take the strictest of the run's rules,
  unless it reads the started harness's file (see the threat model's same-uid residual). Where the file cannot be
  delivered (a runner without managed files, or a shared Kubernetes drive), a `run.toolgate_credential.write` audit
  row records `delivered: false` with the reason.
- A preset write is now held to the same shape and availability checks as a launch, so a preset no one could launch
  is refused when it is saved. Document error sentences no longer say "template", because they serve Run documents too.
- A held runner-pool use-policy change now records whether the pool had a use policy when it was reviewed, so a
  change reviewed on an open pool no longer applies over a policy a leaver purge emptied (or the reverse); it
  goes stale instead. Held changes proposed before the upgrade must be proposed again.
- Updating a runner pool with a revision that another admin's write just made is refused as stale instead of
  writing a `runner_pool.update` audit row for a change that did not happen.
- Per-run overrides: a narrowing edit on a run with no chosen policy keeps the default's workspace mounts and
  onboarded repositories. The Azure DevOps card narrows within the provider's default profile when the policy
  names no capabilities and the run names its repository itself; a run whose only Azure DevOps repository comes from
  a selected workspace is still refused (`override_refused`). `remove_secrets` accepts only your own stored secrets and refuses any other name the
  same way. A launch audits the additions your egress capability drops. Git component facts show a push rule you
  added for an organisation.
- `wardyn governance plan` now lists the write `set` makes when a composed profile's base was renamed on the
  server.
- `wardyn site-config set` restores a document captured with `get` whatever the stored egress baseline (after a
  reset, or after the security tier moved it): the baseline stays as stored and `set` warns only when the file
  asserted a different one. The SDK's `PutSiteConfig` still sends the baseline and gets the server's refusal;
  `PutSiteConfigResult` is the restore path (#1515).
- A webhook audit sink whose `flush_interval` or `retry_base_delay` is zero or negative is now refused at boot, in
  best-effort mode too (#1513).
- The `wardynd` maintenance modes that open the key service (`-rewrap`, `-rewrap-principal-keys`, `-rotate-age-key`,
  `-migrate-secrets`, `-reconcile`) now use `WARDYN_DAEMON_PROXY_URL` and `WARDYN_TRUSTED_CA_FILE` as a serving boot
  does, for Vault Transit and Azure Key Vault too; the AWS instance-metadata, ECS and EKS Pod Identity credential
  endpoints always bypass the daemon proxy (#588).
- A portal's delegated token can read the output tail of its person's runs (`GET /runs/{id}/output`) and nothing
  recovered from a recording: for output recovered from a recording and an interactive run's last screen it gets the
  same 409 a run with no such row gives (the refusal is audited `delegation_scope`), and a failed run's failure hint
  is served without its quote from the recording. (#1423)
- Docs: `docs/AZURE-DEVOPS.md` explains what an Azure DevOps run gets on a laptop runner: only the person's own
  stored PAT, while `minted_pat` and `bearer` runs placed on a laptop are refused (`placement_credential`).
  `docs/DESKTOP.md` explains why a member-mode laptop still enrols into an SSO-only organisation (#475).
- `scripts/kind-run-output-walk.sh` gains a recording-on mode (`WARDYN_KIND_WALK_RECORDING=pg`) that proves a
  Kubernetes task's printed line reaches `/output` from the recording, marked `source: recording`, with a registered
  secret masked (#1831).
- Docs: the operator manual's multi-user material (people, governance, capabilities, API tokens and related tasks) is
  split into task pages under `docs/operations/`, each linked from `docs/README.md`; every old heading in
  `docs/OPERATIONS.md` keeps a stub that links to its new home.
- HA followers check the sweeper and rotator lifetime locks with a read-only `pg_locks` query on the pool before
  opening a dedicated session, instead of dialling a new session every 15 and 30 seconds.
- CI and the nightly log in to Docker Hub (read-only) when the `DOCKERHUB_USERNAME` and `DOCKERHUB_TOKEN` repository
  secrets are set, including the PostgreSQL service containers; without them, pulls stay anonymous.
- `WARDYN_ORG_URL` (org enrolment of a full daemon) logs a deprecation warning at boot; enrolment is removed in 1.0.
  Use a client-mode runner. DESKTOP.md gains a tier table and the managed-laptops page is rewritten for client-mode
  runners.
- Own-runner (local) eligibility classifies the complete dispatch plan: own credentials are separated from operator,
  brokered and platform-minted material, every unclassified or unknown field refuses, own stored grants are persisted
  owner-only after the owner's namespace is proven, and a self-defined component on a local run needs
  `local_self_defined_components` (default deny). A local run is placed through a user pool of the launcher's
  own; one with no pool is refused `placement_unavailable`.
- Run detail and `wardyn run policy` identify organisation-component egress as
  "Added for an attached component"; direct GitHub clone hosts are attributed to source control.

- `PUT /site-config` carries the egress baseline and the `internal_hosts` baseline marks forward and refuses a
  change to either (400 `site_config_egress_via_own_route`); an unchanged round trip, or one from an older
  client that omits the marks, is accepted.
- A plain `http://` OIDC issuer (public or internal) on a host that is not loopback, while the
  console has a TLS posture, now refuses boot; it was a warning. `WARDYN_OIDC_INTERNAL_ISSUER_PLAINTEXT=mesh`
  opts the internal issuer out with a boot warning; the public issuer has no opt-out. Loopback and
  the Compose demo are unaffected (#1970).
- `WARDYN_KEK_REQUIRED` now states what it guarantees: new credential writes need a key service (`transit`,
  `azurekv` or `awskms`) or an external store, and boot is refused without one. Rows the local key already wraps do
  not stop a boot; `/setup/status` shows `kek_local_rows` with their count until `wardynd -rewrap` moves them
  (#1977).
- Dropping an audit partition with the shared admin token is recorded as a `system` actor (still attested) instead
  of a person; the sweeper's autodrop is unchanged. The name `wardynd` is reserved for the sweeper: a drop requested
  under it is refused (`403 audit_retention_actor_reserved`) and audited (#1978).
- The Codex image supports only its pinned Codex. `OPENAI_BASE_URL` and `OPENAI_API_KEY` are still set but unused by
  Codex and deprecated; a later release removes them (#1979).
- A recording uploaded for a run on a self-hosted runner is audited `evidence_source: runner_asserted`: a local
  run's recording is self-reported (threat model residual #73). Masking continuity across restarts (#1512) is
  pinned by tests for a run with no secrets and for kill and revoke without a masking corpus.
- `WARDYN_RUN_MAX_AGE` is now every new run's lifetime cap, not only a reaper stop. A run launched from New Run or
  `POST /runs` that no profile or pool gives an end now ends at creation plus the max age (a request that says
  `experience: interactive` at the earlier of 24 hours and the max age). `no_end` at launch is refused naming the deployment, and `PATCH /runs/{id}`
  can no longer set No end or extend past the cap, a super admin included (a super admin still skips the profile's
  limits). Until the lease sweep honours the cap, the lease sweep keeps the run at the same moment the reaper stops
  it.
- The template decoder refuses secret-named config keys such as `CLIENT_SECRET`, `JWT_SECRET` and `*_PASSPHRASE`
  (`SECRET_NAME`/`SECRET_REF` stay plain).
- Naming a person by a typed `entra:<tenant>:<object>` (erasure, secret owner) is now refused as ambiguous when that
  tenant and object id also signed in under another issuer, instead of resolving to one principal and leaving the
  other's data.
- The request's `resources` field is now applied. A governance profile's CPU or memory maximum no longer cuts a run
  below the member default (the deployment's own default size, lowered by the profile's own size), and dispatch
  applies the same member cap as preflight (#1949). A run with no size from its request or policy gets the
  deployment's own default size (the default policy's resources where set, else the platform default), operators' runs
  included.
- Two repositories that would clone into the same directory are refused; previously one was silently dropped. An
  explicit `image_default` start folder keeps the image's own directory after a clone.
- A background Claude Code run whose policy (after the ceiling) holds or blocks a tool, or where the person set any
  tool rule or default, now goes through Wardyn's tool gate. Before, the policy's `tool_rules` were consulted only
  when the run asked for `tool_approvals: hold`. Tools nobody names still run without a prompt, and a blocked tool is
  refused at the proxy. The shipped default policy names no tools, so only policies that name tools change behaviour.
- The console runs on React 19.3. Go modules (the Docker API client, go-spiffe, Kubernetes client libraries), the
  base image digests and the console's minor and patch dependencies move to their latest releases.
- A run that waits for a pool slot keeps the barrier its create resolved when the organisation lowers its default
  barrier. When the organisation raises the default while the run waits, the run starts under the stronger class if
  the pool allows it and its runners offer it, and `run.queue.start` records `confinement_default_raised`. The
  `run.create` row of a launch that waits now records the drives and the harness set it attaches, as a direct create
  does.
- Each workspace image build records what it built from and the image it produced. `GET
  /api/v1/workspaces/{id}/build` shows the record, its SHA-256 and whether the build is reproducible while the
  record is current; the `run.build` audit row carries the record and its hash, and a workspace-image cache hit now
  writes a `run.build` row too. Workspace image builds do not share an envbuilder layer cache: each build pushes
  under its own tag and fetches every layer fresh.
- `deny_placements` now holds each kind of runner in a pool, not the pool as a whole: a member denied local launches
  can use a mixed pool's remote-hosted runners, and one denied remote launches only their own runners. Pool lists
  and preflight judge availability the same way. A remote-hosted runner stops taking your runs as soon as its people
  policy no longer admits you. `placement_required` is no longer returned.
- New Run's screens follow the approved design: each screen sits in its own centred column with a numbered step
  rail; Shape's workload choices go straight to Run; Run groups its questions with the pool's capacity and limits
  and a barrier comparison; Review puts the view toggle beside its heading and Save, Check again and Launch in one
  row; the run document's YAML/JSON switch changes format at once (text that does not parse stays in its format).
  The "Recording" line, the "Before you launch" heading and relative waiting times are gone; server warnings show as
  notices above the blockers. The GitHub CLI card says what works today: Actions and the REST API.
- New Run's Files step follows its approved design: a main drive section (size, what happens to it), one card per
  repository, drive and folder with its mount path and access, a starting folder picker and a read-only mount map. A
  workspace's folders show as cards of their own and leave with its repositories. A run starts in the repository you
  name, or in its workspace's first folder when none is named. A drive's mount path is read-only; a path outside
  /home/agent is refused with "Use a path inside /home/agent."
- **Breaking:** New Run at `/runs/new` is the new guided flow: Start, then Shape, Run, Harnesses, Files, Tools and
  access, and Review, with a Name dialog at Launch and a Launched screen. It replaces the old wizard and its rail
  (see Removed). [docs/NEW-RUN.md](docs/NEW-RUN.md) is the guide.
- "Start a run like this one" opens Run again on Review when the run kept its setup. Otherwise it prefills Review
  from the run's row and audit trail, without the title. A template or Run again link that cannot be read shows
  the server's sentence and a way back to an empty draft.
- A SCIM suspension refuses the person's self-hosted runners until they are reactivated. A SCIM purge revokes those
  runners and their unused registration tokens, deletes invitations addressed to the person and clears their name
  from the rest, and clears them as creator, suspender or retirer of service identities. A purge stays pending (`GET
  /scim/status` names the identities) while the person still sponsors a service identity that is not retired: hand
  it to another sponsor or retire it.
- A run that cannot pause a tool call is refused when its policy holds a tool of an MCP server the run binds. That
  covers a background Codex CLI run and any command workload (`task_mode: exec`), at create, preflight and policy
  preview (`request_field_unavailable`). An explicit `*: hold` rule counts, whatever single tools the policy allows;
  a policy with no rule for the tool does not. Before, such a run started and the held tool ran without anyone being
  asked. Tool rules set on a harness inside a background command are refused the same way.
- The union Go coverage floor (`COVER_MIN`) is re-set from 79 to 74, one point under the 0.9.0 measurement as the
  Makefile's rule says. 0.9 added code that only the Postgres suites exercise, and the union does not count those
  suites yet; everything else stays at about 79%.
- Revoking one person's API tokens (admin revoke, SCIM suspension or group removal) reads that person's rows through
  indexes on principal and lower(email) instead of the whole token table (index api_tokens_lower_email_idx, in
  migration 0176_revocation_order).
- A domain entry made only of dots and wildcards with a port (e.g. `*.:443`) is now refused when a policy is written
  or loaded; remove any such entry before upgrading, since a default-policy file holding one stops wardynd at start.

### Removed

- The old New Run wizard and its side rail. `/runs/new` opens the guided flow (see Changed). New Run no longer says
  whether a run is recorded (the Recordings screen still does) or which causes tie for a run's autonomy level
  (`bound_by`); a Bedrock sign-in's residency is the "In the sandbox, short-lived" line on Tools and access, and the
  quota near-full and node-size advisories show among Review's server warnings.
- The `member` role value: `WARDYN_OIDC_ROLE_MAP` entries ending `=member` and
  `WARDYN_OIDC_DEFAULT_ROLE=member` refuse boot with `invalid role "member"`, naming the entry (#623).
- The `auth.member_mode` audit action, dual-emitted beside `auth.user_view.set` since 0.8 (#623).
- The `WARDYN_MEMBER_MODE`, `WARDYN_MEMBER_WORKSPACE_ROOTS`, `WARDYN_MEMBER_WORKSPACE_ROOTS_MAP`,
  `WARDYN_MEMBER_WRITABLE_ROOTS`, `WARDYN_MEMBER_WRITABLE_DENY` and `WARDYN_ALLOW_MEMBER_ENV_SECRET`
  aliases. Boot refuses while one is set, naming its `WARDYN_USER_*` replacement (#623).
- The always-empty `resources` array in preflight, the policy preview and the SDK (`PlacementResources`,
  `ResourceAmounts`); read `resources_applied` instead.

### Fixed

- `WARDYN_RECORDING_RETENTION_DAYS` above 106,751 is now refused at startup; a larger value overflowed and the retention sweep could delete recent recordings. Use 0 to keep recordings forever.
- The governance and drive previews now send the standard user type, so a profile assigned to that type shows in the preview as it applies to a run. A preview for another user type is not offered yet.
- A refused attach or UI-gateway ticket, a UI enter on the wrong host and a refused SSH key are audited at a bounded
  rate, sharing the authentication-failure limit; rows past the burst for an invalid attach or UI-gateway ticket are
  counted in `wardyn_auth_failed_suppressed_total`.
- `wardyn template save -` reads stdin again: `template save` takes `--format`.
- A run a laptop submits with a device credential and no person is refused at launch when a per-person Azure DevOps
  connection covers one of its repositories (#731).
- A relayed runner connection honours read and write deadlines, so a response written to a peer that stopped reading
  is aborted instead of holding the run's stream open.
- Runner spool replay after a reconnect sends each batch of up to 64 decisions before waiting for one
  acknowledgement, and the pass before READY ends at the spool head seen when it started, so a large backlog or a busy
  proxy no longer keeps a runner offline for minutes.
- Erasing an organisation-erased run's local evidence now waits for calls already in flight for that run; a lifecycle
  observation that finishes after the quarantine no longer restores the exit or recreates the spool directory.
- `wardyn sync --pull` refuses a sandbox file name holding a control character, and every path or gateway message it
  reports is quoted when it holds a non-printable character, so sandbox text cannot drive the terminal.
- A held runner pool remote-hosted runner attach or detach is listed to, read by and decided by a super admin only, the tier it
  was written on; a security admin no longer sees or approves it.
- An open sign-in or key door stays in place when a background status refresh briefly cannot read the secret store.
- Review and policy preview report the managed-settings fact that launch delivers when governance holds tool
  approvals.
- Workspace target overlap checks stay fast for long attachment lists.
- `docs/sdk.md` documents the five local-placement refusal reasons.
- Security: every route that erases or deletes a person's records by name resolves the name one way: `POST
  /people/{principal}/erasure` (every scope), `DELETE /people/{principal}/credentials`, `DELETE
  /people/{principal}/ssh-keys` and the SSH arm of `POST /sessions/revoke`, `DELETE /secrets/{name}?owner=` and `GET
  /secrets?owner=`, and a key domain's user subject. Any recorded principal, exact or in another case, wins over an
  email: a sign-in, a person an admin created (including one not yet signed in), an API token's owner or a workspace
  owner. An email is matched only for a name with an `@`, against every email Wardyn holds at once, and one two
  people carry is refused (`422 owner_ambiguous`). Before, a person who signed in claiming another person's subject
  or email and minted an API token could make an erasure of that person reach their own records (audit-seal key,
  governance fields) and leave the victim's. An unreadable directory now refuses (`500`, `reason`
  `identity_directory_unreadable` on the erase routes' failure rows) instead of falling back.
- `audit_personal_fields` clears a governance change by its recorded email only for an email the directory names as
  the erased person alone; a principal is never matched against a recorded email. The audit sealer keys a row by the
  same answer, and a bare `?owner=` value is no longer matched against API-token emails.
- Registered runners: a live run's output keeps flowing after a wardynd restart; attach and exec reach only
  sandboxes the runner created; a damaged output directory is named in the start error; a retired run's settled
  output is released.
- `wardyn run --experience` refuses `--model-provider` where no `--agent` would carry it, instead of dropping it.
- The AWS KMS key service boots when `AWS_CA_BUNDLE` (or a shared-config `ca_bundle`) is set; corporate-CA installs
  no longer fail at startup.
- A runner reporting a run the organisation has no row for (erased, or restored over) no longer blocks its other
  runs: that run is contained by a kill and an asserted quarantine. Past 100 such runs the runner is held not-ready
  and `runner.inventory.hold` is audited. The runner deletes their local evidence 90 days after the quarantine or at
  revocation; the organisation deletes their action rows 90 days after the quarantine succeeded. Migration
  `0155_runner_asserted_subjects` alters `runner_pending_actions` only.
- A runner link survives a cancelled request instead of dropping every stream, and a link whose writes stall now
  fails by keepalive; connection credit is no longer lost for data that arrives after a stream ends; a trailing
  slash on `WARDYN_CONTROL_PLANE_URL` no longer breaks the proxy's internal calls; the runner link's keepalive is no
  longer affected by wall-clock steps.
- SSO sign-in works when the public issuer is https and the internal issuer is plain http (mesh opt-out); IdP
  token and key requests time out after 10s; a sign-in refused for no matching role is logged.
- Record Mode no longer counts a network fault (`builtin:dial-failed`, `builtin:tunnel-failed`) as a policy denial,
  so a lost connection no longer fails a clean replay, raises an open-recording anomaly or adds an unreached host to
  the generated allowlist.
- The egress proxy forwards bytes a client pipelined behind its CONNECT headers to the tunnel instead of dropping
  them; every policy check still runs before the tunnel opens.
- The exec-inspect read that decides whether an agent exited addresses the configured Docker daemon instead of a
  placeholder host, and refuses an unparsable daemon address.
- UI e2e: a later step in the same job no longer empties `flaky.tsv`, so a shard's flaky tests reach the flaky-test
  notifier; `make test-e2e-ui` starts each local run with an empty file.
- The audit screen describes `run.policy.resolve` and `run.upstream_proxy.resolve` in words instead of raw action names.
- Docs: under `helm upgrade --reuse-values`, a chart block added in the new release does not render, so wardynd keeps
  its built-in sandbox sizes; pass `-f values.yaml` to pick up new defaults.
- Paused-run resumes hold the run lock; concurrent notification raises share the run
  budget; revived runs reapply stored Git PAT restrictions (#1819, #1812, #1825).
- SMTP channels without recipients or route targets refuse boot. Key rotation and rewrapping
  report an unmigrated database, and legacy audit splitting works immediately after migration (#1825).
- The runs search field preserves characters typed quickly and follows browser history changes.
- Audit rows that waited under a pending subject key reach the SIEM sinks once the spool drain re-seals
  and stores them, so the sink chain has no gap (#1821).
- `audit.retention.set`, `audit.retention.partition_dropped`, `principal_key.destroyed` and
  `credential.reauth.resolve`, which the database writes itself, are sent to the SIEM sinks after they
  commit (#1822).
- The audit log no longer stops accepting writes, and boot no longer refuses, after the database clock
  steps forward: `audit_ensure_partitions` creates partitions up to the chain's high-water mark, so the next
  boot or daily sweep repairs it without hand-run DDL (#1826).
- The People list shows a person suspended over SCIM before their first sign-in as deactivated, and
  `?state=deactivated` finds them (#1824).
- A person's drawer says when their token list could not be read, with Retry, instead of showing
  an empty list (#1815).
- A full reload of the shared masking registry no longer drops a secret this replica committed after the reload's
  snapshot, so that value stays masked (#1975).
- When the exec output relay is refused because a run's masking corpus cannot be proven whole, the denial is audited
  (`authz.denied`, `mask_state_unavailable`, target `exec.relay`) and a pending output row is kept so the sweeper
  records a capture gap with its reason. A run that failed while starting, before any output was opened, no longer
  writes a spurious `no_tail` capture gap (#1976).
- On a PostgreSQL server shared with another Wardyn install, a lock held in the other database no longer silences
  this install's sweeper leader and ground-truth rotator: the lock check is scoped to the current database.
- The best-effort webhook audit sink no longer follows a 301, 302 or 303 redirect, which turned its POST into a GET
  and counted a 2xx as delivered while the events were lost; 307 and 308 are still followed.
- `WARDYN_MAX_CONCURRENT_RUNS` and the Fleet capacity view no longer count an outage-kept run whose agent was
  already stopped at its end after that run is extended (#1825).
- `LockHeld` no longer waits for a nested lock's whole budget: a busy lock connection reads as a lost lock after
  about two watch intervals, so callers re-take it or refuse (#1828).
- A pending rename of a governance profile now holds its new name like a create does, so a create and a rename (or
  two renames) to one name cannot both be stored (#1828).
- Recording a person's sign-in groups now takes the key-domain assignment lock, so a group assignment's ambiguity
  count cannot race a sign-in (#1828).
- A revive that would start an agent is refused at the concurrent-run cap before any identity is minted or image
  pulled, and approval notifications carry the next later SLA tier still ahead (#1825).
- Registered runners: a late audit record or agent exit for a run that already ended no longer drops the runner's
  session; a teardown queued for an offline runner is audited once instead of every sweep; runs finished on a runner
  record their end time.
- Registered runners: a kill or stop queued while a runner was briefly offline now reaches it when it reconnects;
  verified runners behind one ingress no longer use up each other's connect rate limit, while unauthenticated
  connections from one address stay limited; wardyn-runnerd serve logs why connecting fails and when it is
  connected.
- Runner links: a session whose writes stop moving now ends within the keepalive window even if the runner keeps
  sending PONGs. PING replies, dropped-data credit and the answers to stream opens (refusals and accepts) no longer
  start a goroutine per frame; answers to opens are capped and a peer that overruns the cap ends its session.
  Revoking or switching off runners no longer hangs on such a session.
- On a registered runner, a BYOI run's output no longer ends at its image selftest. The run's output is the task's
  own, and the selftest's output goes to the runner's log.
- SCIM deprovisioning never links a sign-in through an email that names another person, and refuses
  (`owner_ambiguous`) when a leaver's emails name two different people, until an admin resolves it. The refusal
  shows as the denied `person.deprovision` audit row and the 5xx the identity provider receives, not on the SCIM
  status card.
- `wardynd -recovery-manifest` now refuses at `pool_max_conns` below 2 and names the setting, instead of hanging.
- `wardyn sync` and sftp now say when the runner is offline or the sandbox is gone, instead of blaming the sandbox
  image for a missing sftp-server.
- A production runner advertised no capacity, so placement refused every sized run on it.
- SCIM: a user create or update whose suspension cannot be read back answers 5xx so the identity provider retries,
  instead of a 2xx with an empty identity; a PUT that omits `active` no longer reactivates a suspended user.
- Sign-in: the session cookie's group budget counts the JSON escaping of `&`, `<` and `>`, so a group list full of
  them cannot outgrow the cookie.
- Directory: a failed token request no longer switches off App Role suggestions for the rest of the process.
- People: creating a person answers 500 when the directory of known principals cannot be read, instead of skipping
  the collision check.
- Audit: an audit row the sealer refuses is logged by action and a fixed reason word. The webhook sink refuses a URL
  with no scheme or host, or a scheme other than http or https, at start-up, and does not follow an https
  collector's redirect to plain http (the batch counts as a failed delivery).
- AWS KMS: a refused Decrypt answer (wrong key or wrong length) has its plaintext zeroed before the error returns.
  The local-key refusal names `WARDYN_KEK=transit, azurekv or awskms`.
- Setup: the "local-key rows remain" row appears again; it never showed in production because the store arrives
  audit-wrapped.
- Component config schemas refuse data after the single JSON value. Config-key secret detection also catches
  `signing_key`, `encryption_key` and `connection_string`, and `SECRET_FILE`, `SECRET_PATH` and `SECRET_STORE` read
  as plain settings.
- Egress proxy: a host verdict the proxy does not recognise is denied as `policy:evaluator-error` instead of falling
  through.
- SSRF guard: the IANA non-globally-reachable IPv6 blocks (`2001::/32`, `2001:2::/48`, `2001:10::/28`,
  `2001:db8::/32`, `3fff::/20`, `5f00::/16`, `100::/64`) are denied. The AWS IPv6 metadata block `fd00:ec2::/64` and
  `100.100.100.200` are never liftable by `internal_hosts`, and the daemon proxy bypasses `fd00:ec2::254` too.
- Egress denial text: a non-canonical literal IP (`10.1`, `127.1`, a zoned IPv6) gets the "denied unconditionally"
  sentence, not the `internal_hosts` remedy.
- Azure DevOps git refusals name the governing policy only when policy decided them, and a request whose client
  hangs up mid-hold no longer records a denial.
- Proxy shutdown keeps part of its deadline for the audit drain and survives a cancelled caller, so the last
  decision rows reach the control plane; it returns both the HTTP and the sink errors.
- `wardyn run --experience interactive --startup ...` says what the sandbox starts instead of "sandbox is idle";
  `wardyn sync` ends a silent gateway handshake after 15 s and keeps at most 64 KiB of gateway stderr; `--ends-in
  .5d` and `1..5d` are refused instead of being read as a fraction of an hour.
- wardyn-tetragon-ingest no longer posts to `//api/...` when the control-plane URL ends in a slash, and
  wardyn-runnerd exits 0 on SIGTERM.
- A refused template-grant revoke says no grant was removed; SSH key owner refusals end "as the Audit log shows it";
  the runner organisation URL refusal names the runner's rule.
- Recovery verbs: manifest and cast reads refuse FIFOs without hanging, refusals name the verb that refused, source
  ports outside 1 to 65535 are refused, the recorded host is lower case, and `-recovery-rehearse` runs behind the
  boot transport.
- remove_secrets now decides whether the caller is an operator by the operator check itself rather than by an empty
  owner name (defence in depth; no known caller reached the old gap).
- As root on rootful Docker, `wardyn-runnerd` no longer runs a run's egress proxy as root: the proxy keeps its
  image's non-root user, and its spool directory is handed to that user.
- A recording's review no longer files a host that only ends in a model provider's name (for example
  notanthropic.com) as provider traffic; it now shows on the list you approve, as the server already treated it.
- Adding a workspace refuses a folder with no mount path beside another such folder, or beside a folder or
  repository placed at /home/agent/work: both would mount there, and a launch that attached both failed. New
  Run's Files step shows a folder that shares its workspace at the path the server mounts it, and shows a folder
  read-only unless its source is writable.
- A workspace read says where each repository clones to (`clone_target`) even with no git provider configured, so
  New Run's Files step offers the repository's mount path on a stock install instead of an empty one.
- New Run: pressing the workload a draft already has on the Shape step keeps its starter, prompt and task.
- An Azure DevOps git push held for approval no longer records a denied decision when the client hangs up before the
  approval is decided; the hold just ends, as it already did for the Azure DevOps REST lane.
- An Azure CLI component compares subscription ids without regard to case: an uppercase id is accepted, and one id
  in two spellings is refused as listed twice.
- The GitHub and Azure DevOps API path classifiers refuse a dot segment that carries a path parameter, such as `..;`
  or `.;x`, in any percent-encoded spelling; a `;` elsewhere in a segment is still accepted.
- When a governance profile denies Azure Resource Manager, a run with the Azure CLI no longer keeps the Entra
  sign-in host open: the hosts the Azure CLI added for its door are removed with the door, and hosts the run's own
  policy listed are untouched.
- A dispatched stop or kill that names a container the runner cannot prove it created is refused by the runner and
  closed (`runner.action.refuse`), instead of failing the runner's session on every reconnect.
- New Run and the console, keyboard and phone use: closing a dialog returns focus to the control that opened it,
  unless the page has already placed focus elsewhere; "Add another harness" moves focus to the new harness; the
  Files Edit dialog opens with focus in its Mount path step. At 320 px Review's file and harness rows no longer
  clip, and the header's home link keeps its name below 640 px.
- Tools and access no longer shows "Nothing else to set up" when the policy preview failed or has not answered: it
  shows the server's sentence with "Check again". A run with no harness no longer gets a blank harness row there.
  The run document says "1 issue" in the singular.
- New Run, Files: the Subfolder field and the "Starts here" line no longer keep a subfolder typed for an earlier
  start after that start was replaced or removed; the next start begins at its own top, as the run is sent. A drive
  list that fails to load no longer closes an edit in progress.
- Azure Key Vault: a next-page link that spells out the default port (`https://<vault>.vault.azure.net:443/...`) is
  followed as the same vault. Before, storing or checking a secret with more than 25 versions failed with "refusing
  to follow a link off this vault", and so did `-reconcile` past 25 secrets. Links to any other host, port or scheme
  are still refused.
- Run again on an interactive agent run that has no stored launch details (every run from before this upgrade, and
  runs with a whole inline policy) keeps the run's first prompt. Before, the new run started with an empty prompt.
- Reviving an Azure CLI run whose owner's profile now denies `management.azure.com` is refused (409
  `revive_ceiling_denies_azure_cli`) and the run's proxy is left as it was. Before, the revive replaced the proxy
  with a configuration it could not load and the run lost all egress. A revive also loads the new proxy
  configuration before it touches the old proxy (`revive_config_does_not_load`).
- Key domains: deleting a person's own assignment is refused (409 `key_domain_ambiguous_membership`) when their
  groups are assigned to different domains, because their next key would then be refused. The boot and rewrap remedy
  and the offboarding guide now say which call destroys which key: `DELETE /people/{principal}/credentials` destroys
  the credential key only, and the audit-seal key is destroyed only by the irreversible `audit_personal_fields`
  erasure scope.
- An internal guarded delete of a stored credential (the AWS sign-in refresh's) no longer removes a row that was
  replaced while the delete waited.
  Building a run's masker no longer slows with the square of the number of stored credential values.
- A run that is stopped but kept no longer answers 500 to the console's Files and Resources polls or writes a
  `run.files.fail` audit row each time: both reads answer 409 `run_inspect_agent_stopped`. A failed run that had
  waited in a queue no longer shows the queue's wait reason as its status reason. A bulk restart with current limits
  is sent in batches of five, each with a deadline of 3.5 minutes per run (the server's bound on one revive plus a
  margin) and never under 60 seconds. A request that still exceeds its budget is aborted, its per-run results are lost
  and the listing is refreshed.
- Review and policy preview refuse a narrowed github.com `git_pat` exactly when launch would, including a run whose
  only GitHub access is a declared clone beside a GitHub App grant. A model provider or Git API host given as an
  IPv6 literal on port 443 is reached; before, every request to it failed with 502.
- Killing a run now stops its image build, including when another replica serves the kill; the build's audit row is
  still written. A run whose state cannot be read just before its image build starts is failed instead of built, so a
  brief database error fails a pending run.
- A run whose sandbox reference cannot be saved is stopped and failed instead of running untracked.
- A kill recovered by the sweep now settles for a run on an offline or revoked self-hosted runner; its `run.kill`
  row carries `sandbox_stop` = `queued` or `runner_revoked`, and neither says the sandbox is gone.
- A resumed run restarts its quiet clock, so the idle-pause sweep no longer freezes it again straight away.
- On a wrapped (bring-your-own) image with recording on, the task now runs after the image selftest; before, the
  recorder refused to overwrite the selftest's recording, the task did not run and the run ended COMPLETED.
- A session log longer than 64 MiB (the fallback recording used when asciinema is absent) is no longer lost:
  wardyn-rec streams it and uploads its first part with a truncation note. Logs up to 64 MiB still upload complete;
  the rest of a longer log is not uploaded.
- Revoking a self-hosted runner ends the runs still waiting for it even when the revoke's own pass could not finish:
  the queue sweep retries from the runner's revoked state.
- A push refused under push content rules because a ref points at a commit the request does not carry (for example a
  branch created at a commit the forge already has) now names the refs (up to ten, and counts the rest) and no
  longer advises `git fetch --unshallow`, which did not help; it is still refused.
- A credential mint whose value cannot be recorded for output masking now fails without using up its single-use
  approval, and a GitHub token minted for it is revoked. Before, the approval was spent and nothing was delivered.
- SSH: a refusal that names a registered key (not the run's owner, a revoked key, a profile that denies it) is now
  audited at a per-key bounded rate (burst 5, 1/s), so a flood of other failed requests cannot hide it. Refusals
  with no attributable actor share the authentication-failure rate bound (burst 5, 1/s). Drops are counted in
  `wardyn_auth_failed_suppressed_total`; the threat model, SSH guide and audit tables say so.
- New Run: a run of two or more harnesses attached to a workspace that builds its own image (its repo's devcontainer
  or a generated profile) is refused with the existing custom-image message at create, preflight and preview,
  instead of being admitted on the set image and launched on an image that lacks the other CLIs.
- Workspaces: a workspace that is in a run only because it stores the same directory no longer turns another
  workspace's (or a policy's) writable mount read-only through a requirement row it has not enabled. A required or
  enabled row still decides the mount, and an attached workspace's explicit read-only still narrows it.
- Interactive attach: a run holds at most eight live terminal sessions (writers and observers, web and SSH) on each
  replica, and a session's slot is held until its recording is saved. A ninth is refused with
  `attach_session_cap_reached` and audited.
- Interactive attach: a database stall longer than the attach lease no longer evicts a live terminal writer. If
  nobody else took the lapsed lease, the writer takes it back and keeps typing, with input discarded until the lease is
  proven; only another live holder or a take-over ends it. A newly attached or promoted writer is no longer evicted
  by a renewal that lands before it has taken its lease.
- The Azure CLI sign-in's resolve door no longer hands a run its bearer token when the token could not be recorded
  for output masking: it answers 503 `mask_state_unavailable` and audits the refusal, as the other credential doors
  do.
- Review and policy preview show the managed-settings document launch writes in two more cases: an interactive
  Claude Code run whose policy carries tool rules, and a run whose started agent has no document while another
  harness in its set does.

### Security

- Signing out now ends the person's browser sessions on the server, not only in their own browser: a copy of the
  session cookie taken before sign-out is refused afterwards. Sign-out ends every browser session of that person,
  including any open web or CLI terminal opened through an attach ticket; API tokens and SSH keys themselves keep
  working. The cut is recorded as a `session.revoke` audit row (session-only, the person as actor) and is written only
  for a request carrying a live session. If the server cannot record the cut, sign-out answers 503 and the console
  reports it was not confirmed.
- A run waiting in a pool queue is ordered against later revocations by the revocation sequence of the credential that
  launched it, the same order the credential itself follows.
- Push rules count a branch the push updates only at the tip the forge reports for it, which must equal the push's
  stated old value.
- A sign-in is refused with `auth_error=claims_overage` when a groups or roles claim the token did not carry could
  change the person's user type. A sign-in whose type is settled by the claims the token did carry is unaffected.
- The brokered model routes (`/wardyn/llm/*`) apply a run policy's `allowed_methods` and refuse a path with `.` or
  `..` segments or control characters.
- An egress-host capability grant or deny that names an IPv6 address matches that whole address.
- `wardyn setup vault` stages the Kata archive in a private temporary directory before unpacking it, and on Docker
  each run's proxy listener binds to its own per-run network address.
- Erasure acts on a derived entra:<tenant>:<object> name only when the identity directory resolves it to the person
  alone. It fails closed when the name is ambiguous or unknown, and it lists any same-tenant/object principal it
  leaves alone as 'not erased: erase by subject'.
- Runs: a run kept after a control-plane outage keeps counting under the run cap and in fleet capacity until its agent
  is confirmed stopped. Moving a kept run's end later can no longer race the lease's stop of its agent.
- The automatic SCIM purge removes a person only when the record shows that the person's own current suspension caused
  the deactivation. Every other scheduled deactivation is held, and the person stays deactivated.
- The Vault client follows a redirect only to the configured address's own scheme, host and port. With Vault HA, name
  the active node or an endpoint that forwards requests.
- The GitHub ref-ruleset check (`github_ref_ruleset`, `WARDYN_GITHUB_REQUIRE_REF_RULESET`) also samples nested branch
  names, and says that it checks sampled names, not every pattern.
- On `POST /drives/grants` a security admin allocates a drive with the drive's own posture or narrower. A new or
  changed directory name (`home_override`) or writable exception, and turning on a disabled allocation that holds one,
  is a super admin's write. `wardyn drive reclaim` takes `--home-subject` for an allocation keyed by an email or other
  alias.
- On Entra ID a token issued to a portal exchanges only for a person who signed in here once or was set up by object
  id (`subject_token_unbound` otherwise). App Role suggestions are read from the sign-in app, and a refused user-type
  removal names the People mappings that still use the type.
