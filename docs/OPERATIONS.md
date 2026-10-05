# Operating Wardyn

Backup, upgrade, rotation, monitoring, and the scaling constraint — the questions
that arrive after the stack is up.

**Scope: mostly the Compose stack.** The backup/state-store, monitoring and
corporate-network sections are written against
[`deploy/compose/`](../deploy/compose/); the Helm chart (`deploy/helm/wardyn`,
[`k8s.enabled`](../deploy/helm/wardyn/README.md)) runs its own
[Kubernetes runner substrate](operations/kubernetes-known-gaps.md) with its own run/recording
state — see [Kubernetes: day-2](#kubernetes-day-2) for the chart's own backup,
restore, upgrade and key-persistence commands.
"[Multi-user: who can change what](#multi-user-who-can-change-what)" and
"[High availability](#high-availability)" apply to both
substrates identically: authorization and the per-process constraints live in
`internal/api`, above the runner seam.

**New here?** Installing for the first time belongs in the front-door docs, not
here — [README.md](../README.md) (single vs. multi-user) or
[TRY-IT.md](TRY-IT.md) (a 10-minute walkthrough); this file starts from a stack
that is already up. Adding the second person to an existing install: "[Second
user, same host](#second-user-same-host)". Deciding who can do what:
"[Multi-user: who can change what](#multi-user-who-can-change-what)".

<details>
<summary>Table of contents</summary>

- [State stores](#state-stores)
- [Monitoring](operations/monitoring.md)
- [Approval notifications](#approval-notifications)
- [Multi-user: who can change what](#multi-user-who-can-change-what)
- [Run lifetime: lease, extend, revive, ends](operations/run-lifetime.md)
- [Exercising member mode as an admin](operations/member-mode.md)
- [Second user, same host](#second-user-same-host)
- [Workspaces: three tiers](#workspaces-three-tiers)
- [Integrations](operations/integrations.md)
- [Network: upstream proxy and egress redirects](#network-upstream-proxy-and-egress-redirects)
- [Toolchain, build images and recommended builds](operations/build-images.md)
- [Secrets from files, age-key rotation and external key stores](operations/secrets-and-keys.md)
- [Renamed in 0.8](#renamed-in-08)
- [Upgrades](#upgrades)
- [Kubernetes: day-2](#kubernetes-day-2)
- [High availability](#high-availability)
- [Kubernetes: known gaps](operations/kubernetes-known-gaps.md)
- Task pages under [`docs/operations/`](operations/): [Managed laptops: hybrid enrolment](operations/hybrid-laptops.md), [Launch presets](operations/launch-presets.md), [Console branding](operations/console-branding.md)

</details>

## State stores

These stores hold recovery state. User drives add each person's files, and the
audit fallback holds events that have not reached Postgres. Losing their only
copy is permanent.

| Store | Where | Holds | If you lose it |
|---|---|---|---|
| Postgres | volume `<project>_postgres_data` | runs, approvals, workspaces, policies, encrypted secrets, the append-only audit log — and, under the default `pg` recording store, the PTY asciicasts too | everything |
| Recordings | volume `${WARDYN_NS:-wardyn}-recordings` (`WARDYN_RECORDING_DIR=/data/recordings`) | PTY asciicasts for Replay — **only with `WARDYN_RECORDING_STORE=fs`**; the shipped default (`pg`) keeps them in Postgres and leaves this volume empty | every session replay it holds; nothing reconstructs them |
| Age key | `WARDYN_AGE_KEY` in `deploy/compose/.env` | the X25519 identity the `local` key-encryption key is derived from — the key that wraps every stored secret's data key | every secret in Postgres becomes undecryptable ciphertext |
| User drives | one object per person, per drive, on a deployment that registered one — a Docker volume or a PVC, both named `wardyn-drive-<drive-slug>-<home>`, or a subdirectory of the share YOU mounted (`host_path` — `<host_root>/<home>`) | each person's own files, written by their own runs at `/home/agent/drive`. Postgres holds the drive rows and the allocations, never the bytes, so `pg_dump` never carried this | that person's work; nothing reconstructs it |
| Audit fallback | `WARDYN_AUDIT_SPOOL` and its `.consumed` / `.quarantine` sidecars; Compose mounts their directory on `<project>_audit` | pending failed-Postgres writes, their replay cursor, and permanently refused events | audit events absent from the database backup |

`postgres_data`, `registry_data` and `audit` carry no explicit `name:` in
`deploy/compose/docker-compose.yaml`, so Docker prefixes them with the compose
project — `compose_postgres_data` by default (the name comes from the
`deploy/compose` directory; `docker compose config | grep '^name:'`). Only
`recordings` is explicitly named
(`${WARDYN_NS:-wardyn}-recordings`), because the docker runner mounts that same
volume into agent containers BY NAME. Reach for the unprefixed form and `docker
volume inspect postgres_data` reports no such volume — a volume-level backup that
ignores that error archives nothing. Back Postgres up with `pg_dump` (below), not
at the volume layer.

**A user-drive volume is not part of the compose project.** `wardyn-drive-*`
volumes are created by the runner through the Docker API, not declared in
`deploy/compose/docker-compose.yaml`, so `docker compose down -v` — and `make
reset`, which runs it — leaves every one of them in place. That is deliberate: a
stack teardown must not delete a person's files. It is also why a backup that
walks the compose volumes misses them entirely; list them with `docker volume ls
--filter label=wardyn.managed=true`.

The optional audit **file sink** (`WARDYN_AUDIT_SINKS`, [ENV.md](ENV.md)) is a
forwarding copy for a SIEM. The Compose `audit` volume also holds the **fallback
spool**, whose pending events are not yet in Postgres, so the volume is not
disposable derived data. Preserve that recovery state as described below.
Ground truth (`tetragon_export`) and the rotator's `groundtruth_token` are
transient — regenerated on start.

### Run output

wardynd keeps the last `WARDYN_RUN_OUTPUT_TAIL_BYTES` (default 64 KiB, between
1 KiB and 1 MiB) of every non-interactive run's combined stdout/stderr, in
memory while the run lives and in Postgres once it ends, so a caller can read
the end of a headless run, during it and after a restart, with
`GET /api/v1/runs/{id}/output?tail=<bytes>` — the run's owner or an admin; anyone
else gets the same `404` as `GET /runs/{id}`. An interactive run keeps no
output of its own (its terminal is the recording's), only the pane snapshot
below when Wardyn stops it gracefully; the managed-harness sign-in run keeps
nothing at all, because its output is a live credential.

- **Not captured on Kubernetes in this release.** The Kubernetes runner runs a
  task under the session recorder, which writes nothing to the container's log,
  so it keeps no output. The read answers `409 run_output_not_captured` ("Output
  isn't captured for Kubernetes runs yet. The run's recording has it."), and the
  run's recording holds the full output. Kubernetes runs from 0.8.5 kept an
  empty row and still read as complete and empty. The rest of this section
  describes the Docker runner, and a Kubernetes runner with recordings off.
- **It is not a recording, and it is stored.** It lives outside the recording
  store and works with `WARDYN_RECORDING_STORE=off`. With
  `WARDYN_RUN_OUTPUT_PERSIST` on (the default) the final tail of each run is
  written once, masked, to the `run_outputs` table when the run ends, so it
  **is in Postgres and in its backups** for `WARDYN_RUN_OUTPUT_RETENTION_DAYS`
  (default `30`; `0` keeps it forever) and any replica serves it, with or
  without a restart. An hourly sweeper on the elected leader deletes rows past
  the window, and a run whose output was erased (below) answers
  `404 run_output_erased`. A deployment that turned recordings off so terminals
  are not kept should decide on these two settings too.
- **Another replica's live tail.** With persistence on, a replica that holds no
  tail for a live run answers the read from `run_output_chunks`, where the
  dispatching replica keeps what its masker has passed (`complete` is false).
- **One finalisation for every way a run ends.** Completion, failure, a kill,
  an idle stop, a lease end, a probe reclaim, boot reconciliation and a failed
  dispatch all end in the same step: wait up to 5 seconds for the runner's copy
  of the output to reach its end (the process exiting is not the end of its
  output), release the bytes the masker was holding back as a possible secret
  prefix, seal the tail, and write the row. A byte that arrives after that is
  dropped. The row says what it is: `complete` is true only for a final capture,
  `incomplete` when the wait ran out, a copy failed or a byte was dropped, and
  `capture_gap` when this process held no tail (for example a run adopted after
  a restart) and could not read the output back from the substrate, so there are
  no bytes to keep. A write that fails is retried with
  capped backoff, the bytes held in memory, and audited as `run.output.finalize`;
  a clean capture writes no audit row.
- **A pane snapshot for an interactive run Wardyn stops.** When Wardyn itself
  stops an interactive run's sandbox into STOPPED (the idle stop, the
  `WARDYN_RUN_MAX_AGE` stop, or a lease end), it first revokes the run's
  credentials, then reads the whole scrollback of the run's `wardyn` tmux
  session as plain text (`tmux capture-pane -p -J -S -`, no escape sequences),
  keeps the last `WARDYN_RUN_OUTPUT_TAIL_BYTES` of it, masked the way a
  recording is, and stores it as a `source: "pane_snapshot"` row. It is bounded
  to 3 seconds: a pane that never finishes, a sandbox with no tmux or no
  session, or a failed read leaves no row and the stop goes on. Each attempt is
  audited as `run.output.snapshot` (the outcome and the byte count, never the
  text). A kill, a failed or reconciled run, and a deployment with persistence
  off take none. **The pane is text the sandbox controls:** the sandbox runs as
  the tmux server's user and can make the pane, or `tmux` itself, print
  anything, so the snapshot shows what the sandbox chose to show. It is stored
  as bytes and meant to be shown as plain text, never interpreted. A snapshot is
  a person's last terminal screen, so it is read on the recording's rule: the
  run's owner or a super admin gets it; a security admin gets
  `409 run_output_interactive`, the answer for a run with no snapshot, and the
  refusal is audited.
- **From the CLI.** `wardyn run output <run-id> [--tail N] [--json] [--raw]`
  reads the same endpoint, so a person without console access can read a run's
  kept output. Piped, it writes the bytes exactly as returned, with no added
  newline. On a terminal it prints ESC and other control characters as escapes
  (such as `\x1b`) unless `--raw` is given, and always for a `pane_snapshot`,
  because a run controls this text. A one-line note on stderr says when the row
  is truncated, incomplete, a capture gap, a pane snapshot, or masked against
  global secrets only; a refusal exits non-zero and names its `run_output_*`
  reason.
- **A restart.** A run that was live across a wardynd restart, or that another
  replica adopted, has its output read back from the substrate when that is
  possible and safe, and ends with a `capture_gap` row when it is not. wardynd
  never re-runs the agent to get it. The two substrates differ:
  - **Kubernetes** with recordings off keeps the agent container's log for as
    long as the pod lives, so the new process re-reads it from its first byte
    into a fresh tail, and a run adopted while still running resumes following
    it. The run keeps all of its output: before the restart, during the handoff
    and after. With recordings on there is no output to re-read (above).
  - **Docker** keeps a log only for an exec-less agent (a krun microVM), which
    is re-read the same way. An exec agent's terminal is a hijacked stream that
    dockerd does not log and that cannot be re-attached after the process that
    held it died, so the run ends with a `capture_gap` row.
  - A sandbox that is already gone, and a runner that cannot read output back,
    also give a `capture_gap` row.
  - **A run with no complete masking manifest gets a capture gap, and nothing
    is read.** That is every run with no manifest (a run dispatched before
    0.8.6, including runs that are alive across the upgrade) and any whose
    manifest is incomplete: the log holds the secrets the run was dispatched
    with, and a process that cannot prove its masking registry holds them would
    persist them verbatim. The check is made before the substrate is touched,
    by any replica, without the watcher lease. A recovery that loses its
    coverage part-way ends as a capture gap too, so a recovered row is never
    masked by the process-wide corpus alone.
  - **The read is bounded** to 10 seconds, inside the finalisation's own
    timeout and after the credential revoke cascade, so a sandbox that writes an
    endless log cannot delay boot or a run's teardown: the row is written
    `incomplete`, with what was read.
  - The hourly sweeper resolves a terminal run's pending row, once its claim is
    five minutes old, the same way: by recovery under the same rules, or to a
    `capture_gap` row.

  Runs that finished before the upgrade have no row and read
  `409 run_output_not_kept`.
- **Settings.** `WARDYN_EXEC_OUTPUT_TAIL=off` collects nothing and refuses
  stored rows too (`409 run_output_off`); the sweeper still deletes them.
  `WARDYN_RUN_OUTPUT_PERSIST=off` keeps the tail in memory only, expired
  `WARDYN_EXEC_OUTPUT_TAIL_TTL` (default `24h`) after the run's last output,
  writes nothing to Postgres, and still serves rows stored earlier until the
  sweeper deletes them. The sweep is the `run_output` row of
  [monitoring](operations/monitoring.md).
- **Storage.** About `WARDYN_RUN_OUTPUT_TAIL_BYTES` × (runs per day) × retention
  days at most: 2,000 runs a day at the default size and 30 days is about
  3.7 GiB of table, before indexes.
- **Retained tail memory.** With persistence on, a tail is held from a run's first
  output until its row commits, never expired by the TTL, so the retained ring
  storage is at most
  (live and kept non-interactive runs) × `WARDYN_RUN_OUTPUT_TAIL_BYTES`: 500
  runs at 1 MiB is about 500 MiB. With persistence off a tail is held until
  `WARDYN_EXEC_OUTPUT_TAIL_TTL` after its last output, finished runs included, so
  the retained ring storage is at most (runs that printed within the TTL) × the
  tail size: 2,000 runs a day at 1 MiB is about 2 GiB. Size the variable against
  the mode you run. This is the ring only, not the whole footprint of a capture.
- **Buffers on top of the ring, per live capture.** Each run being captured also
  owns a masking queue that holds up to 512 KiB of output the masker has not yet
  taken, plus the batch being masked, which can be as large again (two buffers
  alternate, one filling while the other is masked). They are held only while
  output is arriving and are released when the queue drains, but a run printing
  faster than the masker keeps up with can hold about 1 MiB in them whatever the
  tail size is, which at the default 64 KiB tail is many times the ring. With
  persistence on, a run also queues up to 256 KiB for the database, plus the
  batch being written, which can be as large again; it grows toward that only
  while writes to Postgres are failing or slow. Add these
  to the formula for the number of runs printing at once.
- **It can hold secrets, like any log.** Values already in Wardyn's masking registry
  (brokered credentials, `env_secret` grants) are masked as they are written,
  the same way a recording is. Anything else a command prints — a token it read
  from a file, a secret a person pasted into the task — is kept verbatim and
  served to whoever may read the run, and now persisted for the retention
  window. A harness that prints a token it read from a file therefore keeps that
  token for the run's readers.
- **Erasure.** `EraseRunOutputs` deletes a run's rows and writes a tombstone in
  one transaction (nothing at all if it fails). Every write and read of a run's
  output checks the tombstone in its own transaction, so no replica recreates or
  serves an erased run's output, and a replica still holding the run's tail in
  memory drops and zeroes it the next time it touches the run. Bytes already in
  a database backup stay there until it ages out. `DELETE /people/{principal}/credentials`
  erases credentials only and does not call it.
- **The off switch** is `WARDYN_EXEC_OUTPUT_TAIL=off`. Turning recordings off
  does not turn this off; a deployment that disables recordings so terminals are
  not kept should decide on this one too.
- **On Kubernetes** the tail is read from the agent container's log: `get` on
  `pods/log` in the runs namespace, which the chart's k8s-runner Role grants. A
  Role you write yourself (`k8s.rbac.create=false`) needs it too; without it the
  log read is refused, wardynd logs a warning, and the run's tail stays empty.

### Audit fallback recovery

Keep the spool, `<spool>.consumed`, and `<spool>.quarantine` with the database
backup as one recovery set. Quiesce work and stop wardynd while taking the
database dump and copying its durable spool directory, so a drain cannot retire
events between the two snapshots. Preserve ownership and restrictive file modes.
The spool holds failed primary writes until replay succeeds; quarantine holds
rejected events requiring manual triage. Neither is reconstructed from Postgres.

Restore the matching files at `WARDYN_AUDIT_SPOOL` before wardynd starts.
`NewAuditSpool` resumes from the valid `.consumed` cursor, restores the backlog
and quarantine counters, and the drain retries pending events. Quarantine is
not replayed automatically. A missing or mismatched cursor restarts replay from
the beginning and can duplicate events; replay is at-least-once. The cursor
identifies spool bytes, **not a database snapshot**: never pair a newer cursor
with an older database dump, because it can skip events absent from that dump.
Unmatched recovery sets need manual reconciliation.

On ephemeral storage, preserve pending files **before** deleting the container
or pod; once its directory is gone there is nothing to restore. If possible,
recover Postgres and let the backlog drain first, while retaining quarantine.
Use durable spool storage for repeatable backup/restore.

### Back them up

The items to collect on each deployment are listed in
[Recovery set by deployment](#recovery-set-by-deployment); the block below is the
Compose recipe.

```sh
# 0. Quiesce active work, then stop the writer while taking the database
#    and audit-fallback backups as one recovery set.
docker compose -f deploy/compose/docker-compose.yaml stop wardynd

# 1. Postgres (the container name is ${WARDYN_NS:-wardyn}-postgres)
docker exec wardyn-postgres pg_dump -U wardyn wardyn > wardyn-$(date +%F).sql

# 2. Recordings — ONLY under WARDYN_RECORDING_STORE=fs. With the default `pg`
#    store step 1 already captured them; this volume is empty.
#    (`recordings` is the ONE explicitly-named volume, so no project prefix —
#     see docker-compose.yaml)
docker run --rm -v wardyn-recordings:/from -v "$PWD":/to alpine \
  tar czf /to/recordings-$(date +%F).tar.gz -C /from .

# 3. The age key — copy WARDYN_AGE_KEY out of deploy/compose/.env into your
#    secret manager. A Postgres dump without it is unreadable ciphertext.

# 4. User drives — one object per person, and step 1's dump does NOT contain
#    them. Wardyn-managed Docker volumes tar out the same way as step 2, one
#    per person:
#      for v in $(docker volume ls -q --filter label=wardyn.managed=true); do
#        docker run --rm -v "$v":/from -v "$PWD":/to alpine \
#          tar czf "/to/$v-$(date +%F).tar.gz" -C /from .
#      done
#    A `host_path` drive is a subtree of a share you already back up, and a PVC
#    is a snapshot per claim — see "User drives on Docker" and "User drives on
#    Kubernetes" for the per-substrate detail.

# 5. Snapshot/copy the audit-fallback directory, including its .consumed and
#    .quarantine sidecars (see "Audit fallback recovery"). On stock Compose
#    it is /data/audit on <project>_audit, not the unprefixed volume "audit".

# 6. Once both the database and fallback copies are complete:
docker compose -f deploy/compose/docker-compose.yaml start wardynd
```

### Restore them

Order matters, and it is not the backup order reversed — the block below is the
order. Two things it must not lose: the age key in place before wardynd ever
boots against the restored data, and Postgres as the ONLY thing running while the
dump loads.

```sh
# 1. Age key FIRST, before anything boots against the restored data — copy
#    it back into deploy/compose/.env (wherever you stashed it in backup
#    step 3 above).

# 2. Start Postgres ALONE, not the full stack, so nothing else touches the
#    database mid-restore. (Restoring onto a HOST THAT ALREADY HAS DATA?
#    `docker compose -f deploy/compose/docker-compose.yaml down -v` first —
#    pg_dump's plain-SQL output re-creates the schema from scratch and will
#    collide with an existing one. Preserve any current spool/quarantine
#    before down -v deletes the audit volume.)
docker compose -f deploy/compose/docker-compose.yaml up -d postgres

# 3. Restore into it. -v ON_ERROR_STOP=1 makes the FIRST failed statement
#    abort the whole load with a nonzero exit, instead of silently skipping
#    it and leaving a partial database with no error.
docker exec -i wardyn-postgres psql -U wardyn -v ON_ERROR_STOP=1 wardyn < wardyn-<date>.sql

# 4. Recordings — `fs` store only; the default `pg` store already came back
#    in step 3.
docker run --rm -v wardyn-recordings:/to -v "$PWD":/from alpine \
  tar xzf /from/recordings-<date>.tar.gz -C /to

# 5. User drives — backup step 4's tarballs. The dump in step 3 brought back
#    the drive ROWS; it holds none of the BYTES. Skip this and the runner
#    creates a fresh EMPTY volume on the drive's first use, silently.
#    Re-create each volume WITH ITS LABELS and with NO --opt (see "User drives
#    on Docker": Wardyn refuses to mount a volume that is not local-driver and
#    option-free), then untar into it. Leave wardyn.subject OFF — you need not
#    compute the digest, and a volume without it still mounts.
#      for f in wardyn-drive-*; do          # backup step 4's tarballs
#        v="${f%-????-??-??.tar.gz}"        # the volume each came from
#        docker volume create \
#          --label wardyn.managed=true \
#          --label wardyn.drive=<drive id> \
#          --label wardyn.home=<home> "$v"
#        docker run --rm -v "$v":/to -v "$PWD":/from alpine \
#          tar xzf "/from/$f" -C /to
#      done
#    <drive id> is the `id` on GET /api/v1/drives (the drive row's id, not the
#    volume name); a WRONG id makes Wardyn refuse the volume rather than adopt
#    it. host_path drives need nothing here — you restore that share yourself.

# 5b. Restore the matching audit-fallback directory and sidecars before
#     wardynd starts, preserving ownership/modes ("Audit fallback recovery").

# 6. Now bring up the rest of the stack.
make setup

# 7. Verify — row count first:
docker exec -i wardyn-postgres psql -U wardyn -d wardyn -c "SELECT count(*) FROM audit_events;"
#    Startup already decrypts the persisted signing key and fails closed if
#    the age key does not match. Also verify an application secret, which a
#    row count cannot prove: launch a run against any workspace/policy that
#    depends on a previously-stored secret and confirm it starts without a
#    decrypt error (see docs/operations/secrets-and-keys.md "Rotating the age key"):
wardyn run --agent claude-code --workspace <workspace-id>
#    and, if this deployment allocates user drives, that a drive came back with
#    its bytes rather than as a fresh empty volume — step 5 is the only thing
#    that puts them there:
docker volume ls --filter label=wardyn.drive=<drive id>
#    then launch one run WITH a drive (the console's new-run form, or POST /runs
#    with a `drive` selection — there is no CLI flag for it) and confirm
#    /home/agent/drive holds that person's data rather than an empty directory.
```

> `make reset` runs `compose down -v` after a confirmation prompt
> (`scripts/up.sh` `cmd_reset`): Postgres, recordings and the audit sink all go,
> with no backup counterpart. It leaves `.env` — and so the age key — alone.
> `make compose-down` stops the stack and keeps the volumes.

### Recovery set by deployment

A recovery set is five items, and each deployment keeps them in different places.
Take them together, with the writer stopped ("Back them up" step 0 for Compose;
the equivalents below). This table says what to collect and where it lives; the
commands are the ones in "Back them up" and "Restore them", and in
[Kubernetes: day-2](#kubernetes-day-2) for the chart.

| Deployment | Database | Age identity and keys | Recordings (file store only) | Drives | Audit spool, `.consumed`, `.quarantine` |
|---|---|---|---|---|---|
| **Compose** | The `postgres_data` volume; the dump comes from the `${WARDYN_NS:-wardyn}-postgres` container. | `WARDYN_AGE_KEY` in `deploy/compose/.env`, plus the file `WARDYN_PLATFORM_KEY_FILE` names if you set it. With `WARDYN_KEK=transit` or `azurekv`, the key lives in the external key service (Vault or Azure Key Vault), which you back up separately. | The `${WARDYN_NS:-wardyn}-recordings` volume, only under `WARDYN_RECORDING_STORE=fs`. The default `pg` store is already in the dump. | One Docker volume per person, labelled `wardyn.managed=true`. A `host_path` drive is a share you already back up. | The `audit` volume, `<project>_audit`, mounted at `/data/audit`: `audit-spool.jsonl`, `.consumed` and `.quarantine` (`WARDYN_AUDIT_SPOOL` on the `wardynd` service in `deploy/compose/docker-compose.yaml`). |
| **Managed desktop** | The `postgres_data` volume of the Compose project `wardyn-desktop` (the `compose` helper in `deploy/desktop/wardyn-desktop.sh`). Stop the converge job first (`wardyn.timer` on Linux, the launchd job on macOS), or it restarts the stack under you. | **Not in the set, deliberately.** `age.key` stays on the device ([DESKTOP.md](DESKTOP.md#why-agekey-never-rides-in-an-mdm-payload)), so a desktop restore recovers runs, audit and drives, but not stored secrets. If you set `WARDYN_PLATFORM_KEY_FILE`, keep that file. | The `${WARDYN_NS:-wardyn}-recordings` volume, only under `WARDYN_RECORDING_STORE=fs`. | One Docker volume per person, as on Compose (a drive volume per person). | `/data/audit/audit-spool.jsonl`, `.consumed` and `.quarantine`, on the project's `audit` volume. |
| **Helm** | The Postgres you operate (`postgres.dsn`); the chart renders none. Use that Postgres's own backup. | The Secret holding the `age-key` entry (`secrets.ageKeyFromSecret=true`), `secrets.ageKey`, or an operator-owned Secret named by `secrets.ageKeySecretRef`, or the key wired through `env`/`extraEnv`; the `WARDYN_PLATFORM_KEY_FILE` file if set; or the external key service for Vault Transit or Azure Key Vault. `secrets.allowEphemeralAgeKey=true` makes a backup unrecoverable. | The PVC, with `persistence.enabled=true` (store `fs`). With `persistence.enabled=false` the store is `off`; `env.WARDYN_RECORDING_STORE=pg` puts them in the dump. | One PVC per drive: a snapshot per claim ("User drives on Kubernetes"). | `<persistence.mountPath>/audit-spool.jsonl`, `.consumed` and `.quarantine` on the PVC. With the default `persistence.enabled=false` they sit on a `/tmp` emptyDir, so the precondition is: enable persistence, or drain and copy them before scaling to zero. |

**Rules for every row.**

- **Custody.** Keep the keys apart from the data, in a secret manager, never in
  the same archive as the dump. Everything else here is sensitive in plaintext
  too (a dump holds the audit trail and sealed secrets; recordings and drives
  hold whatever the agent saw), so write mode-restricted tarballs.
- **Order.** Stop the writer, preserve ownership and modes, restore the key
  first, load with `ON_ERROR_STOP=1`. The cursor identifies spool bytes, not a
  database snapshot: never pair a newer cursor with an older dump.
- **Older dump.** Restoring an older dump rolls back `audit_events`. Export the
  live trail first.
- **Drives.** Record the volume to drive id to person map at backup time. After
  a restore, a volume you recreate by hand is adopted by name, so the
  `wardyn.drive=<id>` label is the only check that it belongs to the right drive.
  A volume recreated without that label is adopted unchecked, so set the label when you
  recreate it (`driveVolumeAdoptable`, `internal/runner/docker/driver_volumes.go`).
- **Hybrid desktop.** A device re-enrols with a fresh enrolment token. Never
  restore a revoked device.

**What is not proven.** The commands in the day-2 section were exercised once
against a throwaway kind cluster. No shipped tool rehearses a restore (that is
issue #1514), so none of the above has been validated end to end. Loading a dump
into a scratch database proves the SQL loads, and nothing more: a row count does
not show that the key works, and it does not show that the drive bytes are
intact.

### The audit log can't quietly rot

"Append-only" here is enforced by the database, not by convention. A row-level
Postgres trigger rejects `UPDATE` and `DELETE` on `audit_events`, and a
statement-level guard (migration `0004`) rejects `TRUNCATE` — all three asserted
in `TestPG_AuditAppendOnly_TriggerRejects` (`internal/store/store_pg_test.go`),
so an operator with direct database access cannot rewrite or silently thin the
trail through Wardyn's own schema.

**Those triggers are re-checked on every boot**, because `schema_migrations`
records a *filename*: once a migration has run, an owner who later `DROP`s or
`DISABLE`s one of its triggers leaves a database every later start reports as
fully migrated. `Migrate` now reads `pg_trigger` after the migration loop
(`ensureAuditTriggers`, `internal/db/db.go`). A missing or disabled **hash-chain**
trigger is RESTORED — its migrations are idempotent and replayable, and the boot
log says so at ERROR, because rows written while it was gone are unchained and
the verify sweep will name them. A missing or disabled **append-only** trigger
makes `wardynd` REFUSE TO START: restoring it means replaying the initial schema,
which is a far bigger blast radius than stopping and telling you. Either way the
process no longer continues silently on a table whose guards are gone.

The same read also asks what ELSE is armed on that table, because the shipped
guards being present is not the same as nothing standing beside them. A
**row-level `BEFORE INSERT` trigger Wardyn does not ship** makes `wardynd`
REFUSE TO START, whatever it is called: such a trigger is handed `NEW` and
whatever it returns is what Postgres stores, so it can rewrite any field, choose
`prev_hash`/`row_hash`, or `RETURN NULL` to make the event vanish — and the row
it leaves behind is internally consistent, so the verify sweep below reports the
log **clean**. Any other unexpected trigger (`AFTER`, statement-level, or bound
to another event) cannot alter the stored row, so it is named in the boot log at
ERROR rather than refused — a deployment may legitimately hang a replication or
notify trigger off this table.

The boot does more than count triggers. After the catalog check, wardynd
appends ONE synthetic audit row inside a transaction it always rolls back and
asserts it came out chained — `row_hash` set, and `prev_hash` equal to the
head read under the same lock. A chain that demonstrably does not chain
REFUSES THE START: the trigger can be present, enabled and correctly named and
still not work (that is the `0057` state `0058` repaired), and a wardynd
serving over it writes a log the verify sweep reports as broken for as long
as it runs. A canary that could not be RUN — the chain lock was busy, or the
statement was cancelled — is reported at ERROR and the boot continues,
because those are bounded and self-clearing. In the split-role posture the
canary runs on BOTH pools: the migrator's, at the end of `Migrate`, and the
app role's, which is the connection every audit row is actually written on.

A trigger you have hardened with `ALTER TABLE … ENABLE ALWAYS TRIGGER`
(`tgenabled='A'`, so it fires even under `session_replication_role = replica` —
the bypass the sweep otherwise only catches after the fact) is left **exactly as
it is**, and that holds across an upgrade, not just across a restart. The boot
check counts `'A'` as firing, never re-creates it as plain `'O'`, and never
refuses over it. `Migrate` reads which triggers are hardened *before* it applies
anything and re-applies `ENABLE ALWAYS` to any the run reverted. That re-apply
runs on **every** exit from the migration run, not only a clean one — a
migration that fails loudly by design (0059's colliding `home_override`,
0060's out-of-set `api_tokens.role`) and a boot whose own deadline expires
mid-run both reach it, the latter on a context the boot cannot cancel, because
by then the trigger-defining files have already committed their
`CREATE TRIGGER` and the hardening would otherwise be unrecoverable: the next
boot reads the reverted `'O'` as the shipped state and finds those files
already recorded applied. Every migration
that redefines an audit trigger ends in `CREATE TRIGGER`, which always yields
`'O'`, so without that the 0.7 upgrade would have quietly stripped the hardening
off a 0.6.x deployment that had installed it. The restore is narrow — a trigger
you never hardened is never promoted to `'A'` on your behalf — and if it cannot
be re-applied the boot log says so at ERROR and names the statement to run.
`'D'` (disabled) and `'R'` (replica-only, which does not fire for ordinary
writes) are correctly read as not in force.

Completeness survives an outage too. When a Postgres write fails, the event is
not dropped: it is fsync'd, one JSON line at a time, to a local append-only spool
(`WARDYN_AUDIT_SPOOL`, default `./data/audit-spool.jsonl`, empty to disable —
`internal/api/auditspool.go`), and a background drain replays it into Postgres
once the store recovers. The spool is per-process by design: the fallback for one
pod's failed write, each `wardynd` draining its own back on recovery (see
[High availability](#high-availability)).

**What the drain does not restore: the off-box hash series.** The chain hashes
are filled by the Postgres write itself (`RETURNING`, `store.InsertAuditEvent`),
so an event whose write failed fans out to the sinks with **no** `prev_hash` or
`row_hash` on it at all — both fields are `omitempty`, so they are simply absent
— and the drain replays it through the RAW store recorder, deliberately not
through the sink fanout (a replay into a still-down store has to be retryable,
not re-spooled), so it is never streamed a second time. The queryable trail heals
completely; the SIEM's head-hash series does not. Across an outage window a SIEM
holds those events unchained, and the rows they become are chained when the drain
replays them, interleaved with whatever else is being written then — so reconcile
that window with `GET /audit/chain/verify` and the `wardyn_audit_spool_lines`
gauge, not with the sink stream.

**One line the store will never accept does not wedge the rest.** A rejection
that cannot resolve — a `CHECK` violation, a payload a column type refuses, a
hand-edited line, an event shape from another binary version — used to sit at the
head of the spool and stop every event behind it from ever replaying, with no
signal but a `wardyn_audit_spool_lines` gauge that stopped falling. After three
consecutive rejections of the same line the drain now tries the lines *behind*
it, and **only if the store accepts one** (proving it is up, and that the problem
is that line) moves the rejected line to `<spool>.quarantine` — fsync'd there
before it leaves the spool, verbatim, so the file is a valid JSONL spool you can
move back onto the spool path once the cause is fixed. A store that is simply
down accepts nothing, so nothing is ever quarantined during an outage. Each move
logs at ERROR with the event's id and action and increments
`wardyn_audit_spool_quarantined_total`: alert on it, because a spool that has
drained back to 0 no longer implies the queryable trail is complete. The counter
is **re-read from the sidecar at startup**, so it survives a restart the way the
condition it reports does — a restart in place does not clear the alert while
the events are still sitting in `<spool>.quarantine`.

**That is as durable as the spool's directory, and no more.** It holds where the
spool has durable storage: compose's `audit` named volume, or a chart install
with `persistence.enabled=true`. It does **not** hold on the chart's shipped
default, where `WARDYN_AUDIT_SPOOL` renders to `/tmp/audit-spool.jsonl` and the
only `/tmp` volume is an `emptyDir`
(`deploy/helm/wardyn/templates/deployment.yaml`, which says so itself: "durable
across a container restart, not a reschedule"). A rolling deploy — which is what
`helm upgrade` does to a Deployment — or a pod reschedule gives the new Pod a new
empty directory, so the counter reads 0 again and the permanently-refused events
it accounted for are gone with it. wardynd says so at every boot rather than
leaving it to be inferred from a counter that silently reset: it WARNs `the audit
spool is on ephemeral storage; a redeploy or reschedule discards un-drained
events AND the quarantine sidecar`, and the remedy is the one that line names —
point `WARDYN_AUDIT_SPOOL` at durable storage (`persistence.enabled=true` on the
chart, the `audit` named volume on compose).

**Two limits of that rule, stated.** First, the probe needs a line BEHIND the
suspect to land, so two or more *adjacent* unacceptable lines still wedge — the
second rejection in a pass is read as "the store is down", which is the right
reading for every other cause of two rejections in a row and the price of never
quarantining during an outage. A run like that is exactly what "an event shape
from another binary version" produces. It is not silent: the held-back line is
logged at WARN by event id every tick, which reads differently from the
drain-deferred line an outage produces, and the spool gauge stays flat. With the
store demonstrably up and that WARN repeating, triage the spool by hand — move
the head lines to `<spool>.quarantine` yourself and let the rest drain.

Second, one drain pass is deadline-bounded (15s, half the tick). The spool lock
is held across the store call, so a call that never returns would otherwise stall
every request whose own audit write falls back to the spool — reachable without
any Wardyn bug since the chain trigger began taking the serializing lock: an
external session that inserted into `audit_events` and left its transaction open
holds it. A pass that times out replays nothing, counts nothing against any line
(a store that never answered has rejected nothing), and retries on the next tick.
**The synchronous side is bounded too, at 5 seconds** (`db.AuditChainLockTimeout`).
Since `0056` the chain trigger takes that lock on *every* insert into
`audit_events`, so one transaction that inserted an audit row and stayed open
holds up every audit write in the process — and nothing in Wardyn has to
misbehave for that: a psql session, a seed script, a paused migration tool will
do. A request-path audit write that cannot get the lock within 5s **fails, and
the event goes to the local spool** to be replayed when the lock clears — the
same degraded path a store outage uses, not a dropped event. The one exception is
a **credential mint**, whose audit row shares the mint's transaction: there the
timeout refuses the mint, because a credential that could not be audited is not
one to issue. The bound is `lock_timeout`, set `LOCAL` on the audit transaction,
so it fires only while WAITING for the lock — a slow-but-progressing insert is
never aborted by it — and it is deliberately shorter than the drain's 15s pass,
so the request path yields before the background drain does.

**Set the two server-side timeouts** on the database Wardyn uses. Wardyn bounds
its own waits, but the *holder* is the actual problem, and only Postgres can end
it: `idle_in_transaction_session_timeout` (a few minutes) reaps the stray open
transaction that causes this, and `statement_timeout` bounds anything else that
runs away. Neither is set by default (`SHOW idle_in_transaction_session_timeout`
returns `0` on a stock server), and Wardyn does not set them for you — they are
cluster policy, and a value that suits your maintenance jobs is not one Wardyn
can guess.

**Scraping `/metrics` is not one of the things that lock stalls**: the spool
gauges are served from counters, not from a read of the spool file, so a scrape
answers in constant time while a pass is stuck on a blocked store. It has to —
those gauges are how you see the outage, and a scrape that waited on the drain
lost the whole response, `wardyn_store_up` included, once per tick for as long
as the condition lasted.

**Recovering a backlog costs what the backlog costs.** A pass replays a bounded
batch and retires it by advancing a read offset; the file is physically compacted
only once the replayed prefix is at least as large as what is left, so each
compaction halves it and a full drain writes at most about twice the backlog
rather than once per batch. The consequence to know is that mid-drain the spool
FILE can still hold lines that have already reached the store — `wc -l` on it is
not the backlog, `wardyn_audit_spool_lines` is — and that an unclean stop
mid-recovery can replay the not-yet-reclaimed prefix, which the trail records as
duplicate events with the same `id`. The spool has always been at-least-once for
this reason (a crash between the store write and the trim); this widens that
window in exchange for not fsyncing the whole backlog once per batch onto the
volume the database is recovering on. Duplicates are the benign direction: `seq`
still identifies every row, and each one verifies.

### The hash chain — what a rewritten row looks like

The triggers above stop `UPDATE`/`DELETE`/`TRUNCATE` *through Wardyn's schema*,
and the role split hardens that against the app role. Neither binds a **table
owner or superuser**, who can `ALTER TABLE … DISABLE TRIGGER` and rewrite a row —
the residual `0007_audit_least_privilege.sql` states plainly. The role-split check
that reports this posture at boot (`AuditDDLProtected`) counts FOUR ways to
bypass, not two: membership in a superuser role, membership in the owner role,
the **`TRIGGER` privilege** on `audit_events`, and — on PostgreSQL 15 and later,
where it is GRANTable to a non-superuser — the **`SET` privilege on the
`session_replication_role` parameter**, which silences every simply-enabled
trigger for the session without touching DDL at all. All four are
**membership** tests, not attribute lookups — `GRANT some_admin_role TO app_role`, the ordinary
managed-Postgres migration shape, leaves `app_role` with `rolsuper = false` while
it can still `SET ROLE` and `ALTER TABLE … DISABLE TRIGGER`, and the chain is
followed to any depth whether or not the role `INHERIT`s. Membership in
`pg_write_all_data` is deliberately **not** counted: it confers
`INSERT`/`UPDATE`/`DELETE`, but the append-only guard is a trigger rather than a
privilege and still refuses both — counting it would understate the posture just
as badly as missing a superuser overstates it. The third is the quiet one — a role granted
`TRIGGER` cannot drop the shipped guards, but it can add a row-level BEFORE
INSERT trigger of its own and rewrite the row on the way in, minting records that
say whatever it likes while every shipped guard is still armed. Name order is
**not** what makes that work: a trigger sorting *after* `audit_events_chain`
(same-event row triggers fire in name order) runs last and can overwrite
`prev_hash`/`row_hash` directly, but one sorting *before* it is easier still —
it rewrites `NEW` and the shipped chain trigger then hashes the forgery for it.
Either way the stored row is self-consistent and the verify sweep reports clean,
which is why the boot check now refuses to start over ANY foreign row-level
BEFORE INSERT trigger on this table. So a deploy that grants `TRIGGER` back is
reported as NOT protected.

**The fourth leg needs no DDL at all.** `SET session_replication_role =
'replica'` silences every trigger left at the ordinary `tgenabled='O'` for the
session, so a role holding nothing but `INSERT` can append a row past all
three guards above — unchained, unrewritten by the chain trigger — checked
only on PostgreSQL 15 and later, where `has_parameter_privilege` exists and
the `SET` privilege on the parameter is itself GRANTable to a non-superuser
(`AuditDDLBypassRoutes`, `internal/db/db_audit_ddl.go`); on an older server only a superuser can set the
GUC, so leg one already covers it. It is why `ENABLE ALWAYS`
(`tgenabled='A'`, which fires regardless of replication role — see "A trigger
you have hardened…" above) is the documented hardening rather than an edge
case. Failing any of the four legs is reported at boot as NOT protected, with
the remediation logged verbatim: `wardynd: WARDYN_PG_MIGRATE_DSN is set but
the app role (WARDYN_PG_DSN) can still reach past audit_events' append-only
guard by the route(s) named here — DDL protection is NOT in effect; connect
wardynd as a distinct role that holds only INSERT/SELECT on audit_events and
none of these`, followed by a `bypass_routes=[…]` attribute naming which of
the four fired (`cmd/wardynd/boot_deps.go`'s `connectAndMigrate`).

**The app role's grant set does not grow to keep the chain working.** `INSERT`
and `SELECT` on `audit_events` is still the whole of it. The chain trigger
allocates the row's `seq` itself (so position and chain link are one decision —
see the serialization paragraph below), which is a privileged operation the
identity default never was, so the trigger runs `SECURITY DEFINER`
(`0057_audit_chain_security_definer.sql`): the sequence read happens as the
*owner*, not as whoever inserted. Nothing is widened for the app role — a trigger
function cannot be called directly — and a split-role deploy needs no new
`GRANT`. Because it runs with elevated rights it resolves no name through a
search_path it does not control: every table and function it touches is
**schema-qualified to the schema Wardyn was migrated into**, read from the
catalog when the migration applies, and its pinned `search_path` ends in
`pg_temp` so the session temporary schema is searched last rather than first
(`0058_audit_chain_schema_qualified.sql`). That is what keeps the chain working
on an install whose objects are not in `public`, and what stops a caller
shadowing `audit_events` with a temp table of their own to choose their row's
`prev_hash`. Migration
`0047_audit_hash_chain.sql` does not close that hole; it makes a single use of it
**visible**. Every row written from `0047` onward carries two hex columns:

```
row_hash = SHA-256( prev_hash || canonical(id, time, run_id, actor_type,
                                           actor, action, target, outcome,
                                           source_ip, data) )
```

`prev_hash` is the previous row's `row_hash`, so the log is a linked list where
each row commits to everything before it. Both are computed **inside Postgres**,
in the `audit_events` `BEFORE INSERT` trigger — not by the writer — so no caller
(the sandbox-facing paths included) can choose them.

**What it gives you.** Edit one row and its stored `row_hash` no longer matches
its contents; delete one and its neighbours' links no longer meet. Either shows up
as an exact `seq` and a reason.

**What it does not give you.** Tamper-**evidence**, not tamper-proofness. Someone
who can rewrite one row can usually rewrite every row after it and re-chain the
lot; a re-chained tail verifies perfectly clean, and truncating the newest rows
leaves a shorter, valid chain (the high-water mark below catches it, unless it is rewritten too). The defence against both is **off-box**: every
event **whose Postgres write succeeded** carries its `prev_hash`/`row_hash` onto
the audit sink stream (`WARDYN_AUDIT_SINKS`), so a SIEM holds head hashes Wardyn
cannot later disown — that comparison, not the sweep, is the control. The
qualifier is load-bearing, because the hashes are computed by the write itself:
an event written while Postgres is down still reaches your sinks, but unchained,
and the drain does not re-stream it — see "Completeness survives an outage too"
above. Signed receipts (a key the
database role cannot reach) are the next rung and are **not built**.

**Verifying.** Operator-invoked, never automatic:

```bash
curl -H "authorization: Bearer $WARDYN_ADMIN_TOKEN" \
     https://wardyn.example.com/api/v1/audit/chain/verify
```

```json
{"ok": true, "checked": 41233, "legacy": 902, "first_seq": 903,
 "head_seq": 42135, "head_hash": "9f2c…"}
```

`wardynd` deliberately does **not** verify at boot — the sweep re-hashes every
chained row. Run it from cron and alert on `ok: false`: a broken chain answers
**200** with `ok: false`, `broken_seq` and `reason` (the sweep succeeded; it
found something), while `5xx` means the sweep could not run. `head_hash` is the
value to diff against your SIEM's copy.

**One sweep at a time.** The audit log cannot be pruned, so this is the endpoint
whose cost only ever rises — and a retrying client or an overlapping cron would
otherwise turn one operator action into several full re-hash passes, each holding
a database connection. A request that arrives while a sweep is running is
refused with **429** and a `Retry-After`; it is not queued. Point your cron at a
single caller and let a 429 mean "the answer you want is already being
computed". There is deliberately no server-side time limit on a sweep — a fixed
one would cap how large a log can be verified at all — so the bound is your
client's: the sweep is walked in pages and stops between them when the caller
goes away.

**A break is permanent.** The sweep stops at the first broken row and the log is
append-only, so every later sweep reports that same `broken_seq` forever — no
repair, no "acknowledge" cursor. `ok: false` is a one-way latch: treat the first
occurrence as the incident and preserve the row range, because the alert will not
clear.

**After the audit log was partitioned (0.8.6): what verify checks beyond the chain.** The sweep runs in one
repeatable-read snapshot and still re-hashes every retained row. It starts from the newest attested retention drop
(a `kind = 'drop'` row in `audit_chain_anchors`): the first retained row's `prev_hash` must equal the tail hash
that drop recorded, and the result reports that drop's last `seq` as `anchor_seq`. A removal with no anchor still
breaks the chain, and its `reason` now says `rows removed without an attested retention drop`. Two checks cover
what a chain cannot say about itself. The newest row must be the one `audit_partition_meta` recorded as the
high-water mark, so rows removed from the **newest** end (which leave a shorter, valid chain) read as `the log was
truncated at its tail`. The next append does not clear that: each new row links to the recorded head, not to the
newest row left in the table (`0130_audit_chain_head_from_meta`), so the chain then breaks at the first row appended
after the removal. And every partition in the expected manifest must still exist unless a drop anchor names
it; a range a `split` anchor names must exist or be accounted for by a later drop. Someone who can rewrite the
table can rewrite the high-water mark and the anchors too: the head hash on your SIEM remains the control that
catches that.

**Verifying an exported audit partition by hand.** A partition that is fully behind the high-water mark is
*closed*: nothing can be appended to it, so its digest is fixed. Export it in the raw archive form, fold it
yourself, and compare with the database and the footer:

```bash
wardyn audit export-partition audit_events_p202610 --raw -o p.ndjson   # security_admin; or GET /audit/export?partition=audit_events_p202610&form=raw
# manifest header: a JSON array of strings, jsonb's own text form (", " between elements);
# the ranges are empty strings for an empty partition
header=$(jq -r 'select(.type=="manifest")
  | (if .row_count == 0 then "" else null end) as $e
  | [.partition] + ([.seq_lo, .seq_hi, .recorded_lo_us, .recorded_hi_us] | map($e // tostring)) + [(.row_count | tostring)]
  | "[" + (map(@json) | join(", ")) + "]"' p.ndjson)
d=$(printf %s "$header" | sha256sum | cut -d' ' -f1)                       # d_0
while read -r h; do d=$(printf %s "$d$h" | sha256sum | cut -d' ' -f1); done \
  < <(jq -r 'select(.type=="row") | (.row_hash // .fold_hash)' p.ndjson)   # d_i = sha256(d_(i-1) || row_hash_i)
echo "fold:   $d"
jq -r 'select(.type=="footer") | "footer: " + .digest' p.ndjson
psql -Atc "SELECT 'db:     ' || audit_partition_digest('audit_events_p202610')"
```

All three must match, and the export must end with its `footer` line (a transfer that stops short is aborted, not
ended cleanly). The fold is over lowercase hex text, in `seq` order: `d_0 = sha256(header)` and
`d_i = sha256(d_(i-1) || row_hash_i)`, which holds in constant memory however large the partition is. A row from
before the chain began has no `row_hash`; the archive carries the hash it folds as `fold_hash`. To check each row,
recompute `row_hash` from the archive's own columns as the formula above gives it: `time_us` is the `time` in
microseconds since the epoch, and `data` is the jsonb text exactly as stored (not re-encoded), embedded in the
array as a JSON value. Only a security operator may export a partition; anyone else gets an empty export, and
`?partition=` with any other filter is refused (`audit_export_partition_filter`). The readable form
(`form=readable`, the default) gives the same rows in the audit feed's own event shape.

**Rows written before the upgrade** keep `NULL` hashes, are reported as `legacy`,
and are never a failure. There is no backfill, on purpose: hashes computed after
the fact by the same process that could have altered the rows prove nothing, and
writing them would mean `UPDATE`-ing the append-only table. The chain starts at
the first row inserted after the migration.

**A hashless row is legacy only BELOW the chain.** `legacy` counts the unhashed
PREFIX. A row with no `row_hash` that sits *after* the chain has started did not
predate the migration — it was written with the chain trigger dropped, disabled,
or bypassed — so the sweep reports it as the break, at its own `seq`, instead of
counting it. Without that rule an actor who dropped the trigger could append rows
the chain neither covered nor mentioned while `ok` stayed `true`. One blind spot
remains, stated plainly: `seq` gaps *below* the first chained row (a rolled-back
insert burns a `seq`) can still hold a hashless forgery that no rule here can
tell from a legacy row — only your off-box copy can.

**Writers are serialized, and the link is correct for a writer at `READ
COMMITTED`.** The chain link and the row's `seq` are allocated together under one
advisory lock held inside the insert trigger (`0056_audit_chain_serialize.sql`,
redefined by `0057`), so a direct `INSERT` from `psql`, a seed script or any
future code path takes its place in line instead of racing a concurrent Wardyn
write between reading the head and writing its own row. Before that, two writers
could chain to the same head and the sweep reported a **tamper that never
happened** — permanently, per the latch above.

**What the lock does not decide is which head you read.** The trigger's head
lookup is an ordinary `SELECT`, running in the CALLER's transaction, so it sees
what that transaction's snapshot sees. Under `READ COMMITTED` — Postgres's
default, and the level every in-tree writer PINS on its own transaction rather
than inheriting — that statement takes a fresh snapshot after the lock is
acquired, so the head it finds is the row the previous writer just committed
and the link is right. A writer whose snapshot was fixed
EARLIER (`REPEATABLE READ` or `SERIALIZABLE`, begun before that commit landed)
still takes its place in line and still gets a correct `seq` — and still chains
onto the stale head its snapshot can see. Two rows then carry the same
`prev_hash`, and the sweep reports *"a row was deleted or reordered"* at the
second of them, permanently, with nothing having been tampered with. **An
external writer to `audit_events` must use `READ COMMITTED`.** Nothing in the
database enforces that: there is no row conflict for Postgres to raise a
serialization failure over, so a `REPEATABLE READ` insert succeeds quietly.

The GUC that decides this is `default_transaction_isolation`, and it is
`USERSET`: any role can set it per session, per role (`ALTER ROLE ... SET`) or
per database (`ALTER DATABASE ... SET`) with no superuser involved. Wardyn's
own writers are unaffected — `store.InsertAuditEvent`, the broker's mint
transaction and the boot chain canary each pin `READ COMMITTED` on their own
transaction, and a transaction-level isolation level overrides the GUC — so
this rule binds writers Wardyn does not know about. wardynd reads the setting
at boot and, when it is anything but `read committed`, logs at ERROR with the
statement to run: `ALTER DATABASE <db> SET default_transaction_isolation =
'read committed'` (or the matching `ALTER ROLE`). It reports rather than
refuses, and it reads the setting on whichever connection ran the migrations:
in a split-DSN deployment that is the MIGRATE role, so a clean line there is
not a promise about the role the serving pool uses.

The costs are honest, and there are two: that isolation rule (and the
`default_transaction_isolation` default it is read from), and a session that
holds a transaction open after inserting into `audit_events` blocks every other
audit append until it commits or rolls back — so do not leave an interactive
`psql` transaction sitting on that table.

**The lever is `default_transaction_isolation`, a `user`-context GUC** —
settable by any role, per role or per database, no superuser involved. wardynd
checks it on every boot (`reportTransactionIsolation`, `internal/db/db.go`)
and, when it reads anything other than `read committed`, logs an ERROR naming
the value and the fix — `ALTER DATABASE <db> SET default_transaction_isolation
= 'read committed'` (or the matching `ALTER ROLE`) — without refusing to
start: Wardyn's own writers pin `READ COMMITTED` on their own transaction
(`store.InsertAuditEvent`'s `Begin`, `internal/store/store.go`; a
transaction-level isolation level overrides the GUC) and are unaffected
either way, so this is a posture to report for an EXTERNAL writer this
package cannot see, not a defect to boot-refuse over (`internal/db/db.go`).

### Retention, erasure and GDPR — a residual, not a solved problem

The append-only guarantee above has one exception since 0.8.6: an attested, digest-checked drop of the oldest
closed monthly partition past a retention window you set ([Audit retention](#audit-retention-the-attested-partition-drop)).
Nothing else deletes an audit row: no size cap, no per-row, per-actor or per-run delete. A deliberate integrity
choice, but it means **retention is forever by default and there is no erasure
lever today**. Concretely:

- `Actor` (`internal/types/types.go`'s `AuditEvent`) is personal data — an OIDC
  `sub` or email, on every human-attributed row, forever.
- `Data` (`json.RawMessage`) can carry a human-typed value verbatim. The
  secret-masking registry only redacts values it minted or that were explicitly
  registered (`internal/secretmask`); a credential a human *pastes* into a
  recorded terminal or types into a run's task field is stored — and replayable —
  in the clear, with no per-value redaction path (same gap for recordings).
- There is no `DELETE`/erasure endpoint for a single audit row, a single actor's
  rows, or a single run's rows. Migration `0001_init.sql`'s row-level trigger and
  `0004_audit_truncate_guard.sql`'s statement-level trigger make this true at the
  database layer, so there is no admin-surface workaround either.
- A held push's complete review-matched path list is kept the same way. Its
  `push_content` approval names ten paths; migration `0085_push_content_paths`
  keeps the rest, one row per approval, bounded at 10,000 paths or 1 MiB of
  path text (`truncated: true` past either), verified against the approval's
  `paths_total` and `paths_digest` before it is written, and refused UPDATE,
  DELETE and TRUNCATE by its own triggers. A run keeps at most 32 such lists.
  The run's audit trail records each as a small chained
  `approval.push_paths.record` row carrying the list's SHA-256
  (`stored_list_digest`); `GET /api/v1/audit/export` inlines the stored list
  into that row as it streams, so the audit log and its SIEM sinks never carry
  a megabyte row, and an exported list that no longer hashes to the chained
  digest was altered in the table (AUDIT-ACTIONS.md). `GET
  /api/v1/approvals/{id}/paths` reads the list back to whoever may see the
  approval — the run's owner, an admin or a `security_admin` — whatever state
  the run ended in. The route answers every
  `push_content` approval: one raised by a previous-release proxy, which sent no
  list, answers with the ten paths its scope names and `truncated: true` when
  more matched.
- **An export that cannot be finished never looks finished.** If
  `GET /api/v1/audit/export` cannot read the audit store (or a held push's path
  list) before it has written anything, it answers `503` with a JSON error
  (`audit_export_read_failed`) and nothing else. If the failure comes after the
  first rows were sent, the `200` is already committed, so wardynd aborts the
  response: the connection is cut without a clean end of stream, and a client
  sees a read error. `curl --fail` exits `18` (partial file) or `56` (receive
  error); a Go client's body read returns `unexpected EOF`. A complete export
  ends cleanly and its format is unchanged. Treat any non-clean end as an
  incomplete export and re-run it. A reverse proxy that buffers whole responses
  can hide the abort from its own client, so export directly or through a proxy
  that streams. A member whose ownership of the requested `run_id` cannot be
  checked gets `503` (`audit_scope_unavailable`), not an empty export; a run
  that is not theirs or does not exist still answers the same empty `200`.

**This is asymmetric with session recordings**, which have the retention lever
audit lacks: `WARDYN_RECORDING_RETENTION_DAYS` (`docs/ENV.md:47`) age-deletes
stored PTY casts, defaulting to keep-forever but operator-settable, and each sweep
that removes anything emits its own `recording.retention.sweep` audit event. Under
a "right to erasure" obligation on data an audit row could contain, the honest
answer is: **you cannot selectively erase it, and the fix that exists for
recordings does not exist here.** The partition drop (0.8.6) is the retention half of the real
fix, and it removes whole months, never one person's rows. The crypto-shredding half, which makes one person's
fields unreadable while the chain still verifies, is [below](#erasing-a-person), for the rows written after
`WARDYN_AUDIT_SEAL=fields` is on.

### Audit retention: the attested partition drop

Since 0.8.6 the audit log is a monthly-partitioned table, and the oldest closed partition can be removed once it is
older than a retention window, through one database function that leaves evidence. Retention is **off by default**
(`WARDYN_AUDIT_RETENTION_DAYS` is `0`, keep forever) and nothing is dropped unless an operator drops it or you turn on
the unattested autodrop.

**The policy.** `WARDYN_AUDIT_RETENTION_DAYS` is recorded at every boot through the database function
`audit_retention_set_policy`; the app role has no write privilege on the table that holds it. The database decides what
takes effect when, from its own clock:

- an **increase** (a longer window, or any window to `0`, forever) applies at once;
- a **decrease** (a shorter window, and `0` to any finite value) takes effect **30 days after the boot that first saw
  it**. The pending value and its date are stored, so a restart never moves the date, and setting the same value at
  every boot for 29 days leaves it where it was. Restating the current value withdraws a pending decrease.
- the boot that records a change writes an `audit.retention.set` audit row.

Set the same value on every replica. `GET /audit/retention` (`wardyn audit retention`) shows the effective and the
pending policy.

**What a drop checks.** `audit_retention_drop(partition, digest, actor)` removes a partition only if it is the oldest
retained one, is closed (nothing can be appended to it any more), is past the effective window, and holds no row of a
run that is still live (a `PENDING`, `STARTING`, `RUNNING` or `WAITING_FOR_CONFIRMATION` run: revocation reads its
`credential.mint` rows, and its provenance is its own audit trail). It then recomputes the partition's digest and
refuses unless it equals the one you supplied. In one transaction it then writes the chained
`audit.retention.partition_dropped` event, the `audit_chain_anchors` row (partition, `seq` range, row count, digest, the
dropped tail's last `row_hash`, who dropped it, and the event's `seq`), takes the partition out of the expected manifest, and detaches and
drops it. A crash leaves all of it or none. The scan of the partition runs before the chain lock is taken, so audit writers wait only for the drop itself; the detach needs a brief exclusive lock on the audit table and waits at most 5 seconds for it, so a long audit read (or a running chain verify) can make a drop fail with `audit_retention_drop_failed` and nothing changed: retry. `GET /audit/chain/verify` then starts from that anchor and still
re-hashes every retained row; a partition removed any other way fails verify.

**Runbook: export, check, drop.**

```bash
wardyn audit retention                                              # which partition is the oldest, and is it droppable now
wardyn audit export-partition audit_events_p202601 --raw -o p.ndjson   # the archive: keep it where your records policy says
tail -n1 p.ndjson                                                   # the footer: its digest
# check the archive (see "Verifying an exported audit partition by hand", above): recompute every row hash and the fold
wardyn audit retention drop audit_events_p202601 --digest <digest from the footer>
curl -fsS -H "Authorization: Bearer $WARDYN_TOKEN" "$WARDYN_URL/api/v1/audit/chain/verify"   # ok, with anchor_seq at the drop
```

`POST /audit/retention/drop` takes `{"partition": "...", "digest": "..."}`. Who may drop: one `security_admin` (or an admin) acting alone, with the 30-day cooldown on decreases as the
brake. The route is on the security tier, and every refusal is a `409` with its own `reason` and an `authz.denied` row (below):
`audit_retention_not_oldest`, `audit_retention_not_closed`, `audit_retention_inside_window` (also while retention is
forever), `audit_retention_live_run`, `audit_retention_digest_mismatch`. An unknown partition is `404`
(`audit_partition_not_found`). `GET /audit/retention` lists every partition with its row count, state (`closed`, `open`
or `future`) and whether a drop would take it now; for one it would refuse, `refusal` is the reason it would refuse with.
The two answers are the same function's, so they cannot disagree. The row counts are exact (a scan of each partition), so
the status call costs as much as the largest partition.

**Pre-0.8.6 history** sits in one legacy partition that ends at the upgrade. It becomes droppable once the upgrade itself is older than
the window, and it is dropped whole; `wardynd -audit-split-legacy` splits it into ranges that drop one at a time (see "Split the pre-0.8.6 audit history", below). An empty partition drops too and records a row count of `0`; verify anchors on the newest drop that removed rows.

**Autodrop (`WARDYN_AUDIT_RETENTION_AUTODROP`, off by default).** With it on, the sweeper leader drops each eligible oldest partition
itself, as the system actor (`wardynd`), computing the digest in the database. It is **unattested**: nobody checked an
export first, and the event says `attested: false`. It still writes the chained event and the anchor and verify still
passes, and it obeys every rule above: nothing inside the window, nothing holding a live run's rows, nothing while the
flag is off. Turn it on only if your records schedule does not require an exported archive.

**Keeping months ahead.** `audit_ensure_partitions(12)` runs at every boot, before the listener, and daily on the sweeper
leader (so with several replicas exactly one runs it). An audit write into a month with no partition fails and waits in
the spool until one exists, so `wardyn_audit_partitions_ahead` (months past the current one that have a partition) is
exported on `/metrics`, and `/setup/status` carries an `audit_partitions` warning below 3. A warning that stays means the leader sweeper is not
running or cannot reach the database.

**Privileges.** `audit_retention_drop`, `audit_retention_set_policy` and the two read helpers are owned by the migrator, run as it, and are granted
to the roles that can `EXECUTE` `audit_append` (a role added later needs the same grant: see the upgrade note), never to `PUBLIC`.
A role with nothing but `CONNECT` gets `permission denied for function`, and the app role gets `permission denied` on a direct `UPDATE`
of the policy or an `INSERT` into the anchors. Boot reports any `PUBLIC` `EXECUTE` on them next to the audit-function posture line.

#### Split the pre-0.8.6 audit history

All the history from before the upgrade sits in one partition, `audit_events_legacy`, so by default none of it can be dropped
until the upgrade itself is older than the window. `wardynd -audit-split-legacy` splits it into one range per month of the
rows' own time, each of which ordinary retention drops on its own schedule. It is one-way and offline: **take a dump first.**

```sh
# 1. Dump the database (pg_dump, or your platform's snapshot). The tool drops the legacy table once its copies are proved.
# 2. Stop every writer: scale the Deployment to zero. The chart's Recreate strategy does this on an upgrade; a manual run needs it.
kubectl -n wardyn scale deploy/wardyn --replicas=0
# 3. Run it once from the same image, on the migrator's DSN (WARDYN_PG_MIGRATE_DSN when you split roles).
wardynd -audit-split-legacy
# 4. Scale back up, then check the chain and the ranges.
curl -fsS -H "Authorization: Bearer $WARDYN_TOKEN" "$WARDYN_URL/api/v1/audit/chain/verify"
wardyn audit retention                                              # the legacy ranges, oldest first, with the same eligibility as any partition
```

It refuses, exiting `3` and naming the reason, and changes nothing, in these cases: another session holds the single-instance
lock (it names the holder); any other client is connected to the database, lock or no lock (a replica started with
`WARDYN_HA` never takes the lock); the connected role does not own the audit tables (`audit_split_not_migrator`:
the app role cannot attach a partition or write an anchor, and the tool never falls back to another connection); the log is
not partitioned yet (`audit_split_not_partitioned`: run `-migrate-only` first); there is no legacy partition left
(`audit_split_no_legacy_partition`: it was already split or dropped) or it is empty; the chain does not verify before the
split (`audit_split_chain_broken`: fix that first); or a table named for a range already exists. It exits `1` when the split
fails, and then the log is exactly as it was: the whole split is one transaction. The time bound is `WARDYN_MIGRATE_TIMEOUT`
(default 5 minutes); raise it for a large log, as the split copies every legacy row once and builds its indexes.

**How the ranges are chosen.** A range is not a month of `time`. A spool replay writes an old `time` late, so a month cut by
`time` would be scattered through the chain and dropping it would remove interior links. Instead the tool walks the rows in
`seq` order, keeps the running maximum of `time`, and starts a new range where that maximum first crosses into a new UTC
month. Each row's new `recorded_at` is the running maximum, never later than the instant before the upgrade, so every range
is an unbroken run of `seq`, the ranges are in order, and each range's largest `recorded_at` is the largest `time` it holds,
which is what its retention eligibility reads. `row_hash` does not cover `recorded_at`, so no hash changes. The ranges are
named `audit_events_legacy_<YYYYMM>` and written to the expected partition manifest, with one `kind='split'` anchor each (its
row count, `seq` range and digest).

**What it proves before it commits.** For every range, the digest `audit_partition_digest` computes over the copy equals the
fold over the source rows, and every column but `recorded_at` matches the source row for row; then the legacy table is
dropped and the chain is verified again inside the same transaction, and it must inspect exactly the rows it inspected
before (the same checked and legacy counts). Matching digests prove the copy; contiguity in `seq` order is what makes a range
safe to remove, and the tool checks it as well. Afterwards the oldest range drops through `audit_retention_drop` like any
partition, and verify then starts from that drop's anchor.

### Erasing a person

`POST /api/v1/people/{principal}/erasure` (security tier; `wardyn person erase`)
erases one person's retained records by explicit scope, in one audited act
(`person.erasure`). The body names the scopes, a non-empty list of:

| Scope | What it erases |
|---|---|
| `credentials` | the person's stored credentials and the key they sit under (the same erase as `DELETE /people/{principal}/credentials`, which erases credentials only and nothing else) |
| `audit_personal_fields` | the person's audit-seal key, every generation: each sealed audit field of theirs reads `[erased]` everywhere it was copied, and the chain still verifies |
| `run_tasks` | the task text of the runs the person created |
| `run_outputs` | the stored output of those runs (404 `run_output_erased` afterwards, on every replica and after a restart) |
| `recordings` | their session recordings. Opt-in: nothing deletes a recording unless this scope is asked for |
| `mask_copies` | the masking manifests of those runs, after their live attaches, SSH shells and relays are fenced |

The scopes run in the order above whatever order the body lists them: the person's live
consumers are fenced first, the data they could still reach next, the keys last. Every scope is
idempotent. A scope that fails stops the run: the answer is `500` `erasure_incomplete` with `done` and
`remaining`, the `person.erasure` row records a `failure` naming each scope's outcome, and a retry with the
same scopes finishes the rest. Erasure is reported complete (`200`, `outcome` success) only when every scope
asked for finished.

Refusals, all before anything is erased: the operator namespace (`erasure_operator_namespace`), an unknown or
empty `scopes` (`erasure_scope_unknown`), a principal that does not resolve (`owner_unresolved`,
`owner_ambiguous`), and the person being the caller for any scope but `credentials`
(`erasure_self_refused`; the admin token, which is no person, may erase anyone, and is audited as every
other bypass is).

**What this does not reach.** Rows written before `WARDYN_AUDIT_SEAL=fields` was turned on, and before 0.8.6,
are plaintext: no key covers them. A sealed field is stored as `seal2.<handle>.<ciphertext>`: the handle is a
random id of the key (migration `0131_principal_key_handles` adds it to `principal_keys`), not derived from the
person, and the erasure clears it from the key table, so neither the
row nor any copy of it (spool, SIEM sink, export, federation push) holds anything in the field that names the
person, in any encoding, and after the erasure nothing maps the handle back to them. A SIEM sink holds ciphertext
for a sealed field, so after `audit_personal_fields` it holds nothing readable either; its copies of the clear
`actor`, `target` and `source_ip` columns are outside this scope, except the `actor` of rows written under
`WARDYN_AUDIT_SEAL=full`, which is a subject id and reads `[erased]` after the erasure. Before turning `full` on, change any SIEM rule keyed on
`actor`: from that moment it sees `subject:<uuid>` for a person, and the setting applies only to rows written
after it is on. A backup restores the wrapped key and so the field until the backup expires or
the wrapping key version is retired. A row waiting in an audit spool under the pending key when the person is
erased is stored as `[erased]` when the spool drains. A pending row the drain cannot open (sealed under another
pending key, or malformed) is moved to the quarantine sidecar `<spool>.quarantine` instead, as any line the store
keeps refusing is: erasure does not reach that file, which stays on the daemon's disk, mode `0600`, still sealed
under the pending key it was written with, until you remove it. See [AUDIT-ACTIONS.md](AUDIT-ACTIONS.md#sealed-fields) for
which fields are sealed and why the rest stay clear.

### Leavers and SCIM

An identity provider can tell Wardyn that a person has left. Wardyn runs a SCIM 2.0 endpoint whose only job is to
**remove access**: it suspends a person (sessions cut, API tokens revoked, SSH keys deleted, every live run killed,
sign-in refused on every replica) and, later or at once, purges them (stored credentials erased, workspaces
handed to the operator, user grants deleted). SCIM never grants anything: a user or group created over SCIM is
stored and gives no access, and roles still come from the sign-in token and the role map. It is off until
`WARDYN_SCIM_TOKEN` is set, and it mounts only on a single-tenant, commercial-cloud Entra issuer.

| Provisioning event | What Wardyn does |
|---|---|
| `PATCH` with `active` false (or `POST` with `active` false) | **Suspend.** The answer is 5xx until every step is confirmed, so the identity provider retries; a restart or a failed teardown resumes the pending steps, including a run already `KILLED` whose teardown failed |
| `PATCH` with `active` true | **Reactivate.** Clears the deactivation and the purge schedule and nothing else: no token, key, run or old session comes back, and the person signs in again |
| `DELETE /Users/{id}` | **Purge now**, 204 once every step is done. Suspends first if the person is not already suspended |
| A suspended person past `WARDYN_SCIM_PURGE_AFTER` | **Purge**, by the sweeper on the elected sweeper leader; it also finishes any suspension or purge whose identity provider stopped retrying |

#### Turn it on

1. Put a bearer of at least 32 bytes (`openssl rand -hex 32`) in a Secret under `scim-token`.
2. On the chart, set `scim.enabled=true` and `scim.tokenSecretRef=<that Secret's name>`; elsewhere set
   `WARDYN_SCIM_TOKEN` (or `WARDYN_SCIM_TOKEN_FILE`). Wardynd refuses to boot without OIDC on a
   single-tenant issuer of the form `https://login.microsoftonline.com/<tenant id>/v2.0`, without TLS (its own,
   or `WARDYN_TLS_TERMINATED` behind your proxy), or with a token that is short, equal to its rotation twin or
   equal to the admin token.
3. In the identity provider's provisioning job, set the tenant URL to `https://<your host><base path>/scim/v2`
   and the secret token to the bearer.
4. Map Entra's `objectId` to SCIM `externalId`. Wardyn matches a leaver by that object id first, then by the
   `entra:<tenant id>:<object id>` person, and uses an email only to widen a removal; without `externalId`
   a suspension can miss the person. `userName` and `emails` may change (renames are accepted as a projection
   update and the old values are kept as removal aliases); `externalId` may not change on a bound identity.
5. Check a test user: suspend them from the provisioning job, then confirm a `scim.user.deactivate` row and a
   `person.deprovision` row in the audit log ([AUDIT-ACTIONS.md](AUDIT-ACTIONS.md)).

#### What a purge removes

| Step | What it does |
|---|---|
| Suspend | Completed first, if it is not already |
| Erase | The person's stored credentials and the masking copies of them, through the same erasure entry point as `POST /people/{principal}/erasure`, with the `credentials` and `mask_copies` scopes. Audit fields, run tasks, run outputs and recordings are not part of a purge: records retention usually outlives the leaver window, so erasing them stays a deliberate `POST /people/{principal}/erasure` |
| Workspaces | With `WARDYN_SCIM_LEAVER_WORKSPACES` set to `reassign` (the default), each workspace the person owns goes to the operator, audited as `workspace.reassign`; `keep` leaves them |
| Grants | The person's user-subject capability grants and governance assignments are deleted by a direct store call, audited in `person.deprovision`, never through the governance apply path. This deletes deny rows, which the governed path would treat as widening; it is safe for the rows keyed by the person's own principals (the sign-in subject and the object-id form) because the identity is a permanent tombstone whose subject can never authenticate again. A row keyed by an email alias is deleted only while no other principal holds that address (another active identity, or a live API token or person row under a different principal: a revoked or expired token, a deactivated person row, and a principal whose identity is deactivated or purged hold nothing); otherwise it stays, because a recycled address's new holder owns it, and `person.deprovision` counts it as `email_rows_kept` |
| Drives | Listed by name in `person.deprovision`, never reclaimed. Reclaim storage with the steps in "Reclaiming a departed person's storage" |

The identity row stays as a tombstone with `purged_at` set. `PATCH active=true` on it is a 400 `invalidValue`
(a denied `scim.user.write` row with reason `purged`), and the sign-in gate refuses it. A re-hired person needs a
new identity, which means a new object id.

#### Rotating the token

1. Set `WARDYN_SCIM_TOKEN_NEXT` (the chart's `scim-token-next` key) to a new value and restart.
2. Point the identity provider at the new value.
3. Move the new value to `WARDYN_SCIM_TOKEN`, unset `WARDYN_SCIM_TOKEN_NEXT`, restart.

Either value is accepted in between, and each audit row names which slot matched, never the bearer.

#### An urgent leaver

Removal is as fast as the identity provider's provisioning cycle, which can be many minutes. For someone who must
lose access now, do not wait for it:

| Need | Use |
|---|---|
| End their console sessions and revoke their API tokens | `POST /api/v1/sessions/revoke` or `wardyn session revoke` with their subject or email. Both are wired by default whenever OIDC is |
| A person with no sign-in since the upgrade and no API token | The session cutoff is all that reaches them; their runs need `POST /api/v1/runs/{id}/kill` until they sign in once, which gives them an identity row the suspension can find |
| Their stored credentials | `POST /api/v1/people/{principal}/erasure` with the `credentials` scope |

#### What this does not cover

- **A stolen SCIM token is a deprovisioning weapon.** It can suspend everyone, and a `DELETE` erases credentials
  at once; the purge delay does not apply to `DELETE`. HTTPS only, per-replica rate limiting, an audit row per
  write and rotation without downtime are the mitigations. Rate limits are per replica, so an HA install's
  effective limit is the per-replica limit times the replica count.
- **Email recycling.** For a person a sign-in has bound, an email alias widens the session cutoff only while no
  other principal holds that address (another active identity, or a live API token or person row under a different
  principal: only a live holder counts, so a revoked or expired token, a deactivated person row and a principal whose identity is deactivated or purged hold nothing), the same rule a purge uses for grants. While another principal holds it, a suspension and a SCIM
  group removal write no cutoff or session cut under the address: the person's own credentials are reached through
  their subject and object-id forms and the identity deactivation, and the other principal's sessions, API tokens
  and SSH keys keep working. Under an email, a suspension revokes API tokens and deletes SSH keys, and a group
  removal revokes API tokens, only where the owning principal is one of the leaver's own forms, never another
  principal's that merely carries the same address. Deactivation never follows an email. A person no sign-in has
  bound is known only by their emails, so for them every alias is cut and every principal an alias resolves to is
  swept as theirs.
- **Role changes that are not SCIM group removals** at the identity provider still lag until the person signs in
  again.
- **Long-lived connections.** A suspension kills every live run, which ends its attach and SSH sessions. Other
  console streams end at their next authentication check or reconnect.
- **A database writer** can clear `deactivated_at`, as it could any other row. The authority epoch only rises, so
  clearing it does not revive a credential minted before the suspension.
- **Other identity providers.** Non-Entra, multi-tenant and sovereign-cloud Entra issuers get no SCIM routes
  (boot refuses a SCIM token on them), but every sign-in on any issuer still passes the deactivation gate.
- **Audit rows written before 0.8.6** stay plaintext and are not reached by a purge; only retention or the
  legacy split removes them ([Erasing a person](#erasing-a-person)).

## Monitoring

Moved to [monitoring.md](operations/monitoring.md).

## Approval notifications

A pending approval waits in the console until someone opens the Approvals page. With
`WARDYN_APPROVAL_NOTIFY` set, wardynd also tells a channel you name that one is waiting. Unset or empty
(the default) means no notification row is written and no worker runs.

**What it guarantees.** Every approval raised while the setting is on gets a durable outbox row in the
same database transaction that creates the approval, at both places an approval can be created (the API
raise paths and the broker's credential path). A crash cannot leave an approval with no notification,
and a raise that loses a dedup race writes nothing. A worker on every replica then delivers each row at
least once, or records a failure. Approvals already pending when you turn it on get no notification.

**Configuration.** One JSON value, read once at boot; a change needs a restart. The value holds URLs
and secrets, so deliver it through `WARDYN_APPROVAL_NOTIFY_FILE` from a secret store where you can (see
[ENV.md](ENV.md)). Wardynd never logs it, and a boot refusal names a channel `id` and the rule it broke,
never a URL, secret or token.

```json
{
  "console_url": "https://wardyn.example.com",
  "channels": [
    {"id": "sec-hook", "type": "webhook", "url": "https://hooks.example.com/wardyn",
     "hmac_secret": "<shared secret>", "bearer_token": "<optional>"},
    {"id": "sec-teams", "type": "teams", "url": "<Teams Workflows trigger URL>"},
    {"id": "sec-slack", "type": "slack", "url": "<Slack incoming-webhook URL>", "redact_requester": true},
    {"id": "mail", "type": "smtp", "host": "smtp.example.com", "port": 587, "from": "wardyn@example.com",
     "to": ["secops@example.com"], "username": "<optional>", "password": "<optional>"}
  ]
}
```

- `id` is `[a-z0-9_-]{1,32}`, unique. It is the metric label and what the outbox stores; rotating a URL
  under the same `id` keeps pending rows deliverable.
- `type` must be one this build implements: `webhook`, `teams`, `slack` or `smtp`. Any other value refuses boot.
- `redact_requester` (optional, per channel, default false) leaves the run owner's principal and email out of that channel's messages, whatever its type.
- HTTPS is required when `hmac_secret` or `bearer_token` is set or the URL carries userinfo or a query.
  Plain HTTP with none of those is allowed for a `webhook`. A `teams` or `slack` URL is itself the
  credential (a Teams workflow signature in the query, a Slack hook path), so it must always be `https://`;
  boot refuses anything else and the error never shows the URL.
- `console_url` is optional and must be `https://` with no userinfo, query or fragment.

**The webhook body, `wardyn.approval.v1`.** A `POST` of JSON with `X-Wardyn-Delivery: <delivery_id>`
and, with `hmac_secret` set, `X-Wardyn-Signature`. Only these fields are ever sent:

| Field | Meaning |
|---|---|
| `schema` | the literal `wardyn.approval.v1` |
| `delivery_id` | the outbox row's id, stable across retries |
| `event`, `tier` | `raised` at tier 0, `escalated` above it |
| `approval.id`, `approval.kind`, `approval.requested_at` | the approval |
| `approval.sla_due_at` | when the next tier is due; omitted when no later tier is scheduled |
| `run.id` | the run that raised it |
| `profile.id`, `profile.name` | the run's governance profile, when it has one |
| `requester.principal`, `requester.email` | the run's owner; omitted on a channel with `redact_requester` |
| `recipients[]` | `{role, email}` for each `notify` target of the tier that resolved to an address |
| `console_url` | `<console_url>/approvals`, when configured |

The request scope (host, tool arguments, push paths), the reason text, the run title and any credential
are never sent: the sandbox agent writes or influences them, and an approval is decided in the console,
signed in, not from the message. Every string field is control-stripped, capped at 256 bytes and passed
through the run's secret masker before encoding.

**Teams and Slack messages.** A `teams` channel posts a Teams Workflows message carrying one Adaptive
Card; a `slack` channel posts a Block Kit incoming-webhook body. Both are built from the same
allowlisted fields as the webhook body and nothing else, so the request scope, the reason and any
credential are absent here too. The title is fixed text, `Approval waiting: <kind>` at tier 0 and
`Approval still waiting (escalation <n>): <kind>` above it, where the kind reads Credential, Network
access, Tool call, Sign-in needed or Push review. Under it come the run's short id, the profile name, the
requester (unless `redact_requester`) and the request time in UTC. The only link is a button to
`<console_url>/approvals`, from config. Requester and profile text is control-stripped, capped at 256
bytes and masked, then made inert: Slack carries it in `plain_text` objects only, and the Adaptive Card
escapes `&`, `<`, `>` and every Markdown character. Any 2xx answer is success and the reply body is never
read (a Teams workflow may answer 202 with none). `hmac_secret` and `bearer_token` are webhook options;
these two types are not signed.

**SMTP mail.** An `smtp` channel takes `host`, `port`, `from`, an optional static `to` (a list of
addresses) and optional `username` and `password`, which are set together; it takes no `url`. It sends a
plain-text message, `Subject: [Wardyn] ` plus the same title as above, with the same allowlisted lines as
the chat bodies and nothing else. It sends over **verified STARTTLS only**: wardynd refuses a relay that
does not advertise STARTTLS (`starttls_missing`), verifies the certificate against the system roots plus
`WARDYN_TRUSTED_CA_FILE` with the server name set to `host` (`tls_verify`), and authenticates only after that, with the mechanism the relay advertises in its AUTH
list: `PLAIN` when offered, else `LOGIN` (what Exchange Online offers); a relay offering neither (for
example XOAUTH2 only) is dead as `auth_unsupported`, and nothing is sent. There is no plaintext fallback and no option to skip verification, and
implicit TLS (port 465) is not supported: use the submission port, usually 587. Recipients are the static
`to` plus each `notify` target of the tier (see below); every address is re-checked at send time and one
that is not a single bare mailbox (a display name, a list, a CR, LF, comma, semicolon, angle bracket or
space, or over 254 bytes) is skipped and never written to a header or the envelope. With no recipient
left the row is dead as `no_recipient` and the relay is not contacted. The `password` has the same
custody as a SIEM bearer token: keep the value in `WARDYN_APPROVAL_NOTIFY_FILE`. Boot refuses a CR or LF
in any smtp field and an address that is not a bare mailbox.

**Verifying the signature.** `X-Wardyn-Signature: t=<unix seconds>,v1=<hex>`, where `v1` is the
HMAC-SHA256 of `<t>.<body>` (the timestamp, a dot, the exact bytes received) keyed with `hmac_secret`.
Reject a `t` more than five minutes old. Test vector: key `whsec_test_vector`, `t` 1700000000, body
`{"schema":"wardyn.approval.v1","delivery_id":"d"}` gives
`v1=b851b43234ba1d1386179e9f8785cf6c37337c6dee0791f8479579e0eb1e097a`.

**Routes, escalation tiers and recipients.** With `routes` absent, every approval goes to every channel
at tier 0. With `routes` set, the first route that matches an approval chooses its channels and tiers:

```json
"routes": [
  {"kinds": ["push_content"],
   "tiers": [{"after": "0s",  "channels": ["sec-hook"]},
             {"after": "30m", "channels": ["sec-hook", "mail"], "notify": ["profile_contact"]}]},
  {"profiles": ["<governance profile id>"],
   "tiers": [{"after": "0s", "channels": ["mail"], "notify": ["run_owner"]}]},
  {"tiers": [{"after": "0s", "channels": ["sec-hook"]}]}
]
```

- A route matches on `kinds` (`credential`, `egress_domain`, `tool_call`, `credential_reauth`,
  `push_content`) and `profiles` (governance profile **ids**, never names, so a rename cannot re-route).
  The profile is the run's leaf profile as bound at dispatch. An absent key matches anything; no match
  means no notification.
- A route has 1 to 5 tiers, and `after` (a duration such as `30m`) is zero or more and strictly
  ascending. Every tier's rows are written when the approval is raised, due `after` past its request
  time, and a tier is sent only while the approval is still pending: if it was decided first, the row is
  cancelled unsent. A config change after a raise does not alter rows already scheduled. Boot logs a
  warning for a tier whose `after` is at or beyond `WARDYN_APPROVAL_EXPIRY_AFTER`, because the approval
  expires before that tier can send.
- `notify` fills `recipients[]` (`{role, email}`) in the body: `run_owner` is the run owner's address,
  `profile_contact` is the leaf profile's contact email after the same re-validation the console applies.
  A target with no address is skipped.
- A channel with `"redact_requester": true` omits `requester` from its body.
- Boot refuses a route that names an unknown channel or kind, a profile that is not a uuid, a tier list
  that is empty, longer than 5 or not strictly ascending, a channel listed twice in one tier, or
  `notify: run_owner` on a channel with `redact_requester` (the owner is the requester).

**At least once.** A crash between a successful send and recording it resends, so a receiver that cares
drops duplicates on `delivery_id`. A row that is retried keeps its `delivery_id`.

**Retries and failure.** Network errors, timeouts, HTTP 408, 429 and 5xx retry after 30 s, 1 m, 2 m and
4 m. A row goes dead on its fifth failure, on any other 4xx or a 3xx (redirects are never followed:
a redirect would hand the body and signature to a host you did not name), or when still unsent an hour
after it came due. A dead row writes one `approval.notify.failed` audit row and increments
`wardyn_approval_notify_failed_total{channel}`; both carry an error class (`http_status:503`, `timeout`,
`tls_verify`, `dial`, `redirect_refused`, `expired`, and for mail `smtp_reply:<code>`, `starttls_missing`,
`auth_unsupported`, `no_recipient`), never a URL, a response body or a relay's reply text. Terminal rows older
than 30 days are deleted, 500 per tick.

**A per-run budget.** One run may create at most 25 tier-0 outbox rows per hour, so an agent cannot bury
the one real approval under a flood of chat messages. A raise over the budget still creates its
approval and shows in the console, but enqueues nothing; it increments
`wardyn_approval_notify_suppressed_total{channel}` and writes one `approval.notify.suppressed` audit row
per run per hour (per replica).

**In the console.** Every PENDING row on `GET /api/v1/approvals` carries `escalation_tier` and
`sla_due_at` when its route has tiers: `escalation_tier` is the highest tier whose `after` has passed
(the first notice is tier 0 and shows nothing), `sla_due_at` is when the next tier is due. Both are read
from the outbox at response time and never stored, so a decided approval carries neither, and a member
sees them only on approvals of runs they own. The Approvals cards show them as an "Escalated · level n"
chip and an "Escalates in" countdown. Settings has a read-only "Approval notifications" card (super
admins) fed by `GET /api/v1/approval-notify/status` (security tier): per channel its `id`, `type`, the
destination **host** only (parsed from the URL, never a path, query or userinfo), `last_success_at`, the
last error class and time, and `failed_last_hour` (rows dead in the last hour). `GET /api/v1/setup/status`
adds a non-blocking `approval_notify` row, present only when the setting is on: `warn` when any row went
dead in the last hour, otherwise `ok`.

**Network policy.** On Kubernetes wardynd's NetworkPolicy is default-deny for egress. Add a rule for each
notification endpoint, and for a corporate proxy if one fronts them, through
`networkPolicy.egress.extra`, exactly as for SIEM sinks. An `smtp` channel needs a rule for its relay's
`host` and `port` too; it dials the relay directly, not through a proxy. The delivery client uses the same
transport as the rest of wardynd, so `WARDYN_TRUSTED_CA_FILE` applies to every channel (and the daemon
proxy setting to the HTTP ones).

## Managed laptops: hybrid enrolment and audit federation

Moved to [hybrid-laptops.md](operations/hybrid-laptops.md).

## Multi-user: who can change what

The API authenticates with **either** an OIDC session (human SSO) **or** the
admin bearer token; local mode skips both on a loopback-only bind. That is
authentication. Authorization is a real three-role model: every OIDC session
carries an **admin**, **`security_admin`** or **member** role, derived once at
login (`internal/auth/oidc`'s `deriveRole`) and stamped into the signed session
cookie — a cookie signed before this existed (pre-0.5) decodes as no session,
forcing a re-login that derives one fresh.

| The merged map (chart `WARDYN_OIDC_ROLE_MAP` + console People-step rows) | Signed-in humans | Admin token / local mode |
|---|---|---|
| empty | listed in `WARDYN_OIDC_OPERATOR_EMAILS` → **admin**, others → **user**; all **admin** only when the allowlist is also unset (override-only under OIDC — the pre-0.5 behavior) | always **admin** |
| non-empty | mapped by `roles`/`groups`/email claim to **admin**, **`security_admin`** or **user**; no match falls through to `WARDYN_OIDC_DEFAULT_ROLE` (which takes `admin`/`user` only), or denies the login when that is also unset | always **admin** |

A console-added row keys on this exact same table: adding the deployment's
*first* row (with the chart map also unset) or removing its *last* one moves the
map from empty to non-empty or back, exactly like setting or clearing
`WARDYN_OIDC_ROLE_MAP` itself — which is what `POST`/`DELETE /access/mappings`'
posture-flip guard warns an admin about before they trip it (see "Managing them"
below).

The admin token and local mode are **always admin** — a single shared credential
with no per-human identity to key a role off (the token *is* the admin). That is
the documented ceiling of the whole gate (`requireOperator`/`isOperator`,
`internal/api/http.go`), not an oversight.

### Who decides who gets in: chart vs console vs IdP

Three surfaces share this decision, and only one of them is live without a
restart.

| | IdP (Entra) | Chart / env (boot-time bootstrap) | Console (Getting Started → People, live) |
|---|---|---|---|
| **What lives here** | People and groups exist here; Entra App Roles and their assignment; the app registration's "Assignment required" switch | `WARDYN_OIDC_ISSUER`/client config; `WARDYN_OIDC_ROLE_MAP` (the bootstrap layer — always wins a duplicate key against a console row); `WARDYN_OIDC_OPERATOR_EMAILS` (top-precedence admin allowlist, also the boot posture floor); `WARDYN_OIDC_DEFAULT_ROLE`; `WARDYN_OIDC_ALLOW_EMAIL_MAPPINGS` | `/access` role mappings (`GET /access`, `POST /access/mappings`, `DELETE /access/mappings/{id}`), a **disjoint union** with the chart map — the console never edits `WARDYN_OIDC_ROLE_MAP` itself, only its own rows |
| **Wins a collision** | "Assignment required" stops an unassigned user **before Wardyn's callback ever sees a `roles` claim** — a gate Wardyn cannot see through or override | The chart entry, always — a console write that would collide with a chart key or an operator-allowlist email is refused outright (400); a *later* helm upgrade that introduces one anyway leaves the existing console row inert with a "Shadowed" badge instead of silently dropping it | Nothing — a console row only ever fills a gap the chart and the allowlist leave open |
| **Takes effect** | Immediately for Entra's own gate | At boot (a `wardynd` restart/upgrade) | At the affected human's **next sign-in** — canonicalized (trimmed, lowercased) on write; never retroactive, so a person already signed in keeps the role stamped into their current session cookie |
| **Recovery path** | n/a | The admin bearer token — one shared credential with no per-human identity to demote, so it is the ONE caller the console's lockout guard (below) never binds | n/a — the console surface is the thing that *can* lock an admin out, not a way back from it |

A few things that don't fit the grid:

- **Boot posture is chart-only.** `validateOperatorPosture`
  (`cmd/wardynd/boot_posture.go`) refuses to boot OIDC at all unless
  `WARDYN_OIDC_OPERATOR_EMAILS` is set or `WARDYN_OIDC_ROLE_MAP` is non-empty
  (override: `WARDYN_ALLOW_OIDC_NO_OPERATOR_LIST`) — **console rows do not
  count toward this floor**: they live in the database, read once per login,
  never at boot, so the chart alone has to justify running OIDC on this install.
- **The two console writes that can flip everyone's default outcome** — adding
  the first console row while the chart map is empty, or deleting the last one —
  are refused (400) without `acknowledge_access_change=true`, and only when the
  write would actually change what an unmatched, non-allowlisted human gets
  (computed from `HasOperatorEmails()`/`DefaultRole()` on each side of the
  write, never a raw row count — a shadowed row contributes to neither side).
- **The lockout guard** refuses a write that would leave the ACTING admin no
  longer admin, checked against their own last-sign-in session snapshot — never
  a live re-check, since a role is a stamped cookie, not a query. A snapshot too
  stale to reproduce the admin access they demonstrably hold right now (a
  truncated or pre-0.6 cookie) gets a distinct refusal telling them to sign in
  again, rather than a false lockout claim on data that can't answer either way.
- **The preview panel** (`POST /access/preview`) runs the identical derivation a
  real login would, against pasted claims or the caller's own session, so an
  admin can see "who would this row make an admin" without waiting for that
  person to sign in — nothing it does is saved.
- **Some subjects never sign in.** The callback refuses an identity-provider
  `sub` that names an identity that is not a person — `admin-token`, the
  configured `WARDYN_LOCAL_OPERATOR`, or any `local:`/`device:`/`delegate:`/`subject:` name, trimmed
  and case-folded — with the generic sign-in error and an `auth.fail` row
  (`reserved_principal`); a session, `wdn_` token or SSH key already carrying
  one is refused on use. Switching a local-mode install to SSO: the default
  seat (`local:<os-user>`) stays reserved by its prefix, but a custom
  `WARDYN_LOCAL_OPERATOR` seat stays reserved only while the variable remains
  set — unset it, and a person whose `sub` is that name would own the runs
  local mode created under it. Keep it set. On an Entra ID issuer a `sub`
  starting with `entra:`, in any case, is refused the same way: that namespace
  belongs to people set up by object id (see "Tokens for a person who never
  signs in").
- **The same claim values do double duty.** The `roles`/`groups` values a role
  mapping matches are the exact same login-time snapshot a `/permissions`
  capability grant's `subject_type=group` matches against (see "Subjects, and
  the group snapshot's ceiling" below) — two levels reading one snapshot, not
  two systems that happen to agree.
- **Fail-closed on a wired store error.** If the console's role-mapping store
  can't be read, a login in progress is **denied** (`auth_error=
  role_check_unavailable`) rather than silently falling back to the chart-only
  map — the same code the preview panel surfaces when it can't check a row.

**Deriving the role** (`WARDYN_OIDC_ROLE_MAP`, a CSV of `value=role` pairs, e.g.
`Wardyn.Admin=admin,eng-team=user,alice@corp.com=admin`; full semantics in
[ENV.md](ENV.md)): each `value` is matched case-insensitively against the ID
token's `roles` claim (an Entra App Role — the priority path; app-registration
walkthrough in `.claude/skills/wardyn-k8s-setup`), its `groups` claim, or the
signed-in email. Matches fold **highest wins** over three ranks — `user` <
`security_admin` < `admin` — whichever claim produced them (`roleRank`,
`internal/auth/oidc/derive.go`): a human matching a `security_admin` row and a
`user` row is a security admin; one matching an `admin` row anywhere is an
admin, exactly as before 0.7. `WARDYN_OIDC_OPERATOR_EMAILS` is **not
replaced**: an email on it is still an *additional* `admin` match
(`LegacyAdminEmails`), so a deployment adopting the role map keeps its current
operators with zero re-configuration. `WARDYN_OIDC_DEFAULT_ROLE`
(`admin`/`user`, unset = deny) covers everyone the map doesn't name —
`security_admin` is **refused** there and fails boot (`validDefaultRole`,
`cmd/wardynd/boot_deps.go`): the role map is the only way to reach that tier, so
it is never the tier granted by fallthrough to everyone nobody named.

**A `groups`-keyed row needs the `groups` scope requested, on an IdP that gates
that claim behind one.** The authorization request is fixed at `openid profile
email`; it does not ask for `groups` by default, so an IdP that only sends
that claim once a client explicitly requests the scope simply omits it —
indistinguishable from "this human is in no groups", so a `WARDYN_OIDC_ROLE_MAP`
row keyed on a group name decides nothing there. `WARDYN_OIDC_EXTRA_SCOPES`
(see [ENV.md](ENV.md)) opts a deployment into requesting `groups` (or any other
scope), validated at boot against the provider's own discovery document.

Both are validated at **boot**, not at first use: a malformed entry (invalid role
value, non-ASCII key — matching is ASCII-only, so it could never match —
duplicate key, or non-blank input with no valid entry at all) or an invalid
`WARDYN_OIDC_DEFAULT_ROLE` fails wardynd's boot outright, naming the var
(`buildOptionalFeatures`, `cmd/wardynd/boot_deps.go`) — never a silent fallback
that lets a typo reach a session cookie later. A signed-in human who matches
nothing in a valid map, with no default role set, is denied at login instead
("no Wardyn role assigned").

**What admin-only still means** — the writes with the widest blast radius stay
gated on the role being exactly `admin` (`requireOperator`). Since 0.7 a SECOND
gate covers part of that surface: `requireSecurityOperator` — admin **or**
`security_admin` — the tier that holds authority over the verdict and over the
org's ceilings, and never reaches into a run, onto credential material, or onto
the host. The two tiers overlap and deliberately do not nest: every gated route
names exactly one of them, and `internal/api/authz_test.go`'s route matrix is
the authoritative per-route classification (it fails on any route it cannot
classify). Status icons in the tables throughout this document: 🟢 open/works ·
🟡 partial or narrowed · ⛔ refused.

| Surface | Gate |
|---|---|
| policy create/update/delete; launch preset writes (`PUT`/`DELETE /presets/{name}`), selectable content like a stored policy; the console branding writes (`PUT`/`DELETE /branding/settings`), org-wide presentation every sign-in page shows; `PUT /site-config` (a full-document replace, integration credential refs included — and the matching `GET` is gated too, see the operator-topology reads below); `GET /metrics`; the `/access` role-mapping routes below — they bound who derives admin at all | ⛔ admin only |
| the operator-topology READS — `GET /site-config`, `GET /sources`, `GET /sources/{id}`, `GET /base-images`: they carry the upstream-proxy secret ref, a `local_dir` source Locator and internal registry refs, so reading them is reading the deployment's own topology | ⛔ admin only |
| the `/workspaces` routes that BIND CREDENTIAL MATERIAL or WRITE THE HOST — `llm-cred`, `requirements`, `env-as-code/write` — plus `reassign` (user administration) | ⛔ admin only |
| the `/workspaces` routes that DECIDE AN EGRESS CEILING — `approved-egress`, `denied-egress`, `promote-egress`: deciding which hosts a workspace's runs may reach is the same authority as deciding an egress approval, and `promote-egress` is that decision in bulk | ⛔ admin or `security_admin` |
| launching a recording session (`POST /workspaces/{id}/record`) — it sat with the egress-decision routes above until 0.7 re-tiered it, because the route does not decide a ceiling: it LAUNCHES a credentialed, host-mounting, open-egress sandbox and stamps the caller as its owner, which is reach into a run and at the host. The egress DECISION stays delegable; only the launch moved | ⛔ admin only |
| the workspace-provider policy — `GET /workspace-providers` and `PUT /workspace-providers`: which git hosts (and which org paths on them) a run may clone from, which credential lanes it may use there, and the ephemeral/drive storage ceilings. Both verbs, because a provider's allowed addresses name the org's forge hosts and org paths — corporate topology, the same reason the `/site-config` reads above are gated. A member is told the provider KIND in a refusal, never the addresses | ⛔ admin only |
| the Azure DevOps organisation check — `POST /workspace-providers/git/{id}/org-check`: on a `minted_pat` row, it uses the caller's own Azure DevOps connection to create and at once revoke two short tokens, to learn whether the app registration holds the token permissions and whether the organisation's maximum token lifespan is on. Beside the rows it checks, and it creates tokens in the caller's name | ⛔ admin only |
| the Azure DevOps token refusal — `GET /workspace-providers/git/{id}/ado-pat-refusal`: the newest launch in the last seven days refused because the organisation restricts who may create tokens, as the refused person's email (from their People record or an API token of theirs; a person with neither is passed over) and the time, or 204 for none. Beside the rows it explains; it names a person other than the caller | ⛔ admin only |
| the agent roster — `GET /agent-providers` and `PUT /agent-providers`: which coding agents this deployment offers, whether each is on, and (0.8) each agent's `default_provider` — the model provider a new run uses unless the person chooses another, which must be enabled for that agent and may be turned off (its runs are then refused, never moved). Since 0.8 a row carries no model credential: model access is a model provider. Both verbs, for the sibling row's reason: the block names the org's model-provider choices. A member is served a narrower document instead — the `enabled` field on `GET /setup/status`'s harness rows | ⛔ admin only |
| the model providers — `GET /model-providers` and `PUT /model-providers` (0.8): which kinds of model credential this deployment supports, where each sends requests (gateway addresses, Bedrock region and data plane), the AWS access portal and account pin a Bedrock SSO provider signs in against, and which agents each may serve. Configuration only — no credential lives on a record. `GET` also answers `connected_people`: per provider id, how many distinct people hold a credential of their own for it (a count, never who; 0 included), which `PUT` refuses. Both verbs, for the agent roster's reason. Removing a provider (or unticking the agent it is the default for) is refused while the roster names it as a default; turning it off is not. A person is served a narrower document instead — `model_providers` on `GET /setup/status`: the providers serving the agents they may launch, each with its kind, the agents it is the default for, and the one host their own credential would be sent to (the host only, never a path, start URL or pin). Members also receive `provider_access`: one row per granted provider (state, action, deadline, and — when they have stored one — `added_at` and `last_used_at` for their own credential, never anyone else's) graded against their OWN credential, whose pin-mismatch action names the pinned account and role, as `model_access`'s already does | ⛔ admin only |
| the two `/site-config` connectivity probes (`POST /site-config/test-proxy`, `/test-redirect`) — non-mutating, and the evidence half of the security admin's job — and the `/permissions` routes below | ⛔ admin or `security_admin` |
| the rest of that tier: `GET`/`DELETE /tokens`, `POST /sessions/revoke`, `GET /audit/chain/verify`, `GET /audit/retention`, `POST /audit/retention/drop`, the `/governance` profile and assignment routes (with the four `/governance/changes` routes that hold and decide their writes, see "Optional: four-eyes on governance writes"), `GET /access/directory/search` | ⛔ admin or `security_admin` |
| `GET /admin/runs/capacity` — the fleet's configured reservations (below, "Fleet capacity"): across every owner, so not a member read. It never execs into a sandbox or calls the runner | ⛔ admin or `security_admin` |
| the approval notification status — `GET /approval-notify/status`: each configured channel's id, type, destination host (never a URL), last delivery, last error class and dead rows in the last hour. It decides nothing, and the security tier is the one that decides approvals, so it is the one that must learn an announcement is not arriving | ⛔ admin or `security_admin` |
| the `/user-types` routes — listing, defining, editing and removing the org's user types (`GET`/`POST /user-types`, `PUT`/`DELETE /user-types/{id}`). Defining a type is the same duty as authoring a profile; deciding who IS a type stays with the admin-only People mappings above. A type is refused removal (`409`) while the chart's role map or default role, or a permission, profile or drive row, still names it, or a live API token carries it, and the built-in `standard` type is never removable | ⛔ admin or `security_admin` |
| the `/key-domains` routes — listing the declared key domains with how many live keys each holds, and setting or removing which domain a user, a group or everyone is assigned to (`GET /key-domains`, `PUT`/`DELETE /key-domains/assignments/{subject_type}/{subject}`). An assignment decides which declared domain's key wraps the subject's NEXT principal key; nothing already written moves. The domains themselves come from `WARDYN_KEY_DOMAINS_FILE`, never from the API. A domain the file does not declare is refused (`422`), as is a group write that would leave people in two domains (`409`) | ⛔ admin or `security_admin` |
| the `/sources` writes — `POST /sources`, `POST /sources/{id}/scan`, `DELETE /sources/{id}`: registering, rescanning, or removing a source touches the same repo/registry topology the operator-topology reads above expose | ⛔ admin only |
| the `/base-images` writes — `POST /base-images`, `DELETE /base-images/{id}`: adding or removing a base image changes what every future onboarded workspace can run | ⛔ admin only |
| `PUT`/`DELETE /integrations/{id}` — editing or removing one integration credential reference outside a full whole-site-config replace | ⛔ admin only |
| `POST /admin/sandboxes/sweep` — force-reaping sandboxes across every workspace, not just the caller's own | ⛔ admin only |
| `GET /admin/runs/proxy-window` and `POST /admin/runs/restart` — listing the runs whose proxy was started by a release older than wardynd N−1, and giving named runs a new proxy on the current release under each OWNER's current profile denies ("Restart with current limits"). Proxy-only: a run lost to a reboot is reported, never started; its owner revives it. Not the security tier: a restart replaces proxies on runs the caller does not own. The runs are restarted one at a time, each audited as `run.revive`, so when a response is cut off part-way those rows say which were. On Kubernetes every run is refused, nothing changed, with `reason` `revive_unsupported` (`runner.ErrReviveUnsupported`): stop it and start a new run | ⛔ admin only |
| `POST /setup/onboarding-complete` — marks first-run setup done for the whole deployment; a distinct route from the model-provider rows below | ⛔ admin only |
| `POST /admin/devices/enrolment-tokens` — minting the single-use token a managed laptop's first boot trades for its device credential: it creates a credential | ⛔ admin only |
| `GET /admin/devices` and `DELETE /admin/devices/{id}` — the enrolled-device inventory and revoking one device: the inventory-then-revoke pair `/tokens` sits on, and like it neither returns credential material nor adds reach | ⛔ admin or `security_admin` |
| `POST /admin/delegates` — registering a portal that may act for the people in one group ([Delegated run management](#delegated-run-management-portals)): it creates a credential | ⛔ admin only |
| `GET /admin/delegates` and `DELETE /admin/delegates/{id}` — the registered-portal inventory and revoking one portal: the device pair's shape, and like it neither returns credential material nor adds reach | ⛔ admin or `security_admin` |
| `DELETE /people/{principal}/credentials` — erasing every credential one person has stored (offboarding, 0.8): it only removes reach and returns a count, never a value | ⛔ admin or `security_admin` |
| `POST /people/{principal}/erasure` — erasing one person's retained records by scope (0.8.6, [Erasing a person](#erasing-a-person)): it only removes records and returns counts, never a record's content; nobody erases themself except their credentials | ⛔ admin or `security_admin` |
| `GET /scim/status` — the Settings SCIM card's read ([Leavers and SCIM](#leavers-and-scim)): who is deactivated, which deprovisioning steps are stuck on what error, and which purged people's drives are still to reclaim; read-only, names and steps, never a credential | ⛔ admin or `security_admin` |
| `DELETE /people/{principal}/ssh-keys` — removing every registered SSH key for a resolved subject or email; returns the removed-key count | ⛔ admin or `security_admin` |
| `GET /people` — the people this deployment knows (0.8.6): everyone who has signed in and everyone set up beforehand, with each one's role (what the role mappings give their email), first and last sign-in, deactivation, and counts of live sessions (0 or 1: sessions are stateless cookies, so this says whether the last sign-in could still hold one), API tokens, SSH keys, stored credentials and runs still going. Paged (`?limit`, default 50, at most 200, and an opaque `cursor`) and filtered by `?q=` (principal or email prefix) and `?state=` (`active` or `deactivated`). It discloses the email of everyone who has signed in, the audience that can already read the audit trail, so it sits on this tier. `wardyn people list` prints it | ⛔ admin or `security_admin` |
| `POST /people` and `GET /people/{principal}/tokens` — setting up a person before their first sign-in, and listing the API tokens an admin once created for them (0.8, [Tokens for a person who never signs in](#tokens-for-a-person-who-never-signs-in)). `POST /people/{principal}/tokens` stays mounted on this tier so a non-admin is refused as ever, and answers every admin `403` `person_token_mint_removed`: no role creates a token that acts as another person (0.8.5) | ⛔ admin or `security_admin` |
| `GET /model-providers/credentials` — the credential inventory (0.8): for each model provider, every person who holds a credential of their own for it, with its state (`stored`, or `expired` past its sign-in's expiry), where it is stored (`pg`, `vaultkv`, `azurekv`), when it was added and when a run last used it (to the minute: a sink stamps a row at most once a minute), plus counts. Each row also carries `email` and `provider_name` (CS-8, both additive and non-secret) so a `security_admin` — who has no route to the model-provider roster or an identity directory — can still read the table well enough to offboard from it. The console's own page is `/admin/credentials`. The erase's companion; read from the rows' metadata, never a value | ⛔ admin or `security_admin` |
| `GET /admin/devices/enrolment-tokens` and `DELETE /admin/devices/enrolment-tokens/{id}` — the enrolment tokens still redeemable and cancelling one before a laptop redeems it: the same pair for tokens, returning neither a token nor its hash | ⛔ admin or `security_admin` |
| `GET /runs/{id}/attach` — the interactive PTY WebSocket's ticket-less fallback lane is admin only; a member attaches their own run only via a minted attach ticket (`POST /runs/{id}/attach/ticket`), a separate owner-or-admin check inside the handler | ⛔ admin only |
| workspace CRUD/scan/build | 🟡 owner-or-admin since 0.6 ("Workspace ownership") |
| `devcontainer_repo` on a run (`denyUserRequest`, `internal/api/runs_create_validate.go`) | ⛔ admin only, never grantable |
| a custom sandbox `image` | 🟡 admin by default; the one power a capability grant can hand a member ("Capabilities") |
| a member's own onboarded-workspace base image | 🟢 never gated — operator-authored at onboarding, not the member's free-text choice |
| the `/drives` routes that NAME A HOST PATH — creating, listing, updating, and removing the **user drive** itself (`GET`/`POST /drives`, `PUT`/`DELETE /drives/{id}`, `mountUserDriveRoutes`, `internal/api/user_drives.go`) | ⛔ admin only, deliberately NOT the security-admin tier: a drive names a host path (`host_root`) or a cluster storage class, and "never the host" is the line between the two admin tiers |
| allocating a drive to people or groups, revoking that allocation, or previewing whose drive resolves — `POST /drives/grants`, `DELETE /drives/grants/{id}`, `POST /drives/preview` (0.8, issue #168) | ⛔ admin or `security_admin`: none of the three names a host path — a security admin's authority over drives is the `DenyUserDrive` door on a governance profile, reached through `/governance` above |
| `POST /drives/{id}/reclaim` — **destroys** one person's drive storage on the substrate (`internal/api/user_drives_reclaim.go`) | ⛔ admin only, and the only irreversible row in this table. It is fenced four ways: super-admin here; a `409` while a run still holds the object or while the object under that name is not this drive's; a `drive.reclaim` audit row on every attempt that reaches the substrate, `409` refusals included; and on Kubernetes wardynd does not even hold the `delete` verb unless the chart's `drives.reclaim.enabled` is set. There is no console button — API and CLI only |
| the user-drive **door** — `DenyUserDrive` on a governance profile (`internal/types/governance.go`) | 🟡 security admin too, through `/governance` — a limit on a profile, not a drive; it refuses the mount, it does not deallocate anything |
| mounting YOUR OWN drive on a run (`drive.enabled`) | 🟢 the person, per run — read-only unless their allocation says otherwise, and the run flag may only narrow that, never widen it |
| signing in to a model provider yourself (0.8) — `POST /model-providers/{id}/sign-in` launches the sign-in sandbox for a `bedrock_sso` or `anthropic_subscription` provider, and `PUT /model-providers/{id}/sign-in` stores the Claude setup-token that sandbox printed. Answers only while a model-provider block exists (409 otherwise). The capture lands in the caller's OWN namespace under that provider's name, never anyone else's | 🟡 any signed-in human, admins included, who may launch one of the agents the provider serves (the `agent` capability; otherwise 404, as if it did not exist) AND is granted the provider (the `model_provider` capability; otherwise 403). The admin token under SSO is a mechanism, not a person: 422. The portal, region and pin are the ADMIN'S, from the provider record — a sign-in can never choose another |
| `POST /runs`, `POST /runs/{id}/kill` | 🟢 any signed-in human — using the product is a member act |

**Documented gaps — routes gated but not yet named above.** None today. The
ten pre-0.7 omissions the F316 completeness check surfaced (the `/sources` and
`/base-images` writes, the two `/integrations/{id}` writes,
`POST /admin/sandboxes/sweep`, `POST /setup/onboarding-complete`, and
`GET /runs/{id}/attach`) each moved into a row above this docs pass;
`docTierUndocumented` (`internal/api/operations_tier_doc_test.go`) is now
empty. It stays a RATCHET, not a closed list: `TestOperationsTierTableMatchesRouteMatrix`'s
completeness check still blocks any new gated route from landing without either
a row above or a filed entry here.

### Who writes the provider policy: console vs CLI/MDM

An Azure DevOps row has no shared credential. Its `workspace_providers` row names how each person
connects — `token_mode` `minted_pat` (Wardyn creates a short-lived token for each run in the person's
name), `bearer` (the person's Entra sign-in) or `own_pat` (a token the person adds themselves) — and an
Azure DevOps Server row is `lanes: ["pat"]` with `credential_source: per_user`, git only. A shared `pat`
or `ssh` lane, and an empty `lanes` on such a row, is a 400 at both write doors. See
[docs/AZURE-DEVOPS.md](AZURE-DEVOPS.md) for the app registration, the row's fields, and what a member
sees.

A deployment carries at most **one enabled** row that signs a person in on the `entra` lane
(`minted_pat` or `bearer`): each person signs in to one Azure DevOps organisation. Both write doors
refuse a second enabled one with a 400 (`git[N].lanes: git[M] already carries the "entra" lane …`); a
disabled second row is accepted, and enabling it later is refused the same way. A document stored
before that rule can still hold two; the setup checklist then warns (row `ado_entra_rows`, not
blocking) until one is disabled. Rows in `own_pat` mode sign nobody in, so any number of them may be
enabled at once. A `minted_pat` row must name the console's own OIDC application and the console must
hold a client secret (`WARDYN_OIDC_CLIENT_SECRET`), or it is refused at write and left unusable at
boot (an error in the daemon log naming `ado_pat_needs_console_app`).

**A disabled row stays closed, and says so.** A launch into a disabled git provider row's organisation
is refused naming the row. Its hosts are left out of `effective_scm_hosts` and out of run egress only
when no enabled row also names the same host: with several organisations on `dev.azure.com`, one off and
another on, the host stays effective. `GET /site-config` also returns `withheld_scm_hosts`, read-only
like `effective_scm_hosts` (a `PUT` ignores it, and it is never stored): one
`{host, provider_id, provider_kind}` entry per host a disabled row claims that `effective_scm_hosts`
omits. A host an enabled row also names is not listed. The list is absent when nothing is withheld.

**The upgrade that retired the shared Azure DevOps credentials (0.8.2).** Migration
`0103_retire_ado_shared_credentials` rewrites the stored rows (a row left with no per-person lane is
turned **off**, and the setup checklist warns with `ado_rows_off` until an admin turns it on), and
`wardynd` deletes the stored shared secrets **once**, at the first start after it:
`git-pat-<host>`, `ssh-key-<host>` and `known-hosts-<host>` for every Azure DevOps host, in the
operator's namespace and every person's. The deletion is irreversible and audited per namespace as
`ado_shared_credential.retire` (`docs/AUDIT-ACTIONS.md`). The `boot_once` row named
`ado_shared_credential_retire` holds the marker; the sweep runs only while its `done_at` is null. A
secret store that cannot answer at that first start **refuses boot** rather than leave a retired
credential in place. A host that a GitHub or other non-Azure DevOps row also names is skipped and
logged, since the name could be that forge's own credential: remove a shared Azure DevOps credential
there by hand.

0.7.2's two provider blocks — `workspace_providers` (which git hosts and org
paths a run may clone from, which credential lanes it may use there, and the
ephemeral/drive storage ceilings) and `agent_providers` (which agents this
deployment offers, the one model-access lane each may use, and whether that
credential is shared or captured per person) — are **policy fields on
`SiteConfig`**, not tables of their own. That is deliberate: `SiteConfig` is the
org→desktop channel MDM already delivers as `/etc/wardyn/site-config.json`
([DESKTOP.md](DESKTOP.md)), so a provider policy reaches a managed laptop with no
new plumbing and no DDL. It also means **two doors write them**, and the grid
below is what tells them apart.

| | Dedicated endpoints (the console's `/admin/providers` screen) | `PUT /site-config` (the CLI / MDM door) |
|---|---|---|
| **Route** | `GET`/`PUT /workspace-providers`, `GET`/`PUT /agent-providers` — both verbs admin-only, for the reason the tier table above gives: a base URL names corporate topology and an `sso_start_url` names the org's IdP | `PUT /site-config`, admin-only, a **full-document replace** of everything except integrations |
| **Writes what** | exactly one block, replaced whole; `{}` is the clear form | the whole document, provider blocks included when the body NAMES them |
| **A block the body does NOT name** | n/a — the route IS the block | **carried forward**, not cleared (`carryForwardUnnamedSiteConfigFields`, `internal/api/site_config.go`). Without this, every 5-minute converge on a laptop whose MDM file predates 0.7.2 would silently delete the org's provider policy |
| **Clearing a block** | `{}` | `{}`. Over raw HTTP an explicit `null` also clears; through `wardyn site-config set` it does **not** — the CLI strict-decodes into the pointer field and re-marshals it ABSENT under `omitempty`, so `null` in a file reads as "unnamed" and carries forward. Use `{}` on both doors and the question never arises |
| **Audit row** | `workspace_provider.write` / `agent_provider.write` — the block's own shape, including `base_urls` in the clear (a provider address is topology, not a credential) and never the `sso_start_url` | `site_config.write`, whose datum carries `git_providers`, `storage_configured`, `agent_providers` and — when the body named `workspace_providers` — `sources_no_longer_admitted`, so an MDM-applied narrowing is reviewable with nobody watching a console |
| **Narrowing is never silent** | the `PUT` response counts the already-onboarded repo sources and library sources the new block refuses; the console renders it on the save toast | the same count, on the response and in `site_config.write` |

**`https://github.com/<org>` bounds HTTPS clones only — SSH is host-level.** An
SSH clone URL carries no path a base URL can be compared against
(`git@github.com:acme/x.git` is not `/acme/x`), so a row scoped to one org
admits an SSH clone of ANY org on that host, with the deployment's
`ssh-key-<host>` secret. That is a documented ceiling of 0.7.2, not an
oversight, and it is never silent: the `/admin/providers` screen says it under the
row's lanes, and run create puts it on the 201 as a warning (with a
`run.provider.admit` audit row) whenever a path-scoped row admits an
SSH repository. **The remedy is the row's own `lanes` list** — drop `ssh` from a
path-scoped row and its addresses bind again, over the one transport that
carries a path. Dot-segment and percent-encoded paths do NOT reach this
question at all: `https://github.com/acme/../evil/repo.git` and
`…/acme%2Fevil/…` are refused outright at every admission door and at both
write doors, because git and the server would read such a path differently.
The one escape that is admitted is an Azure DevOps project or repository name,
which may carry spaces and most punctuation: every door stores such an address
in one spelling — `https://dev.azure.com/acme/Payments Platform/_git/Card Auth
(v2).Service` is stored as `…/Payments%20Platform/_git/Card%20Auth%20(v2).Service`
— and an escape that decodes to a separator, a dot segment or a control
character is still refused. An `azure_devops` row may likewise be scoped to such
a project (`https://tfs.corp.example/Payments Platform`); names compare as
written, case included. Such a row cannot name a project whose name holds
``& ' $ ; | < > " ` `` (site-config values refuse them); scope the row to the
organisation instead. An Azure DevOps Server host takes these names only when
an `azure_devops` provider row names it.

**On the desktop tier this grid has a winner.** `wardyn-desktop.sh` re-applies
`/etc/wardyn/site-config.json` on every converge tick, so on `a′` — where the
developer IS the admin and can open `/admin/providers` — an MDM file that NAMES a
provider block overwrites a local console edit within five minutes, and one that
omits it leaves the edit standing. See
[DESKTOP.md § Posture switches are env vars, never site-config](DESKTOP.md#posture-switches-are-env-vars-never-site-config).

**Ownership scoping — real, not just admin-vs-everyone.** A member reaches their
OWN resources the same way an admin reaches any of them
(`ownsRunOrAdmin`/`getRunAuthorized`, `internal/api/helpers.go`): `GET`/kill/
profile/grants on a run, minting its attach ticket, its recording replay, and
`GET /runs`/`GET /approvals` (each scoped to the caller's own `created_by` rows)
all answer a foreign resource with the **byte-identical 404** a truly-missing one
gets — never a 403, so probing another user's run id learns nothing. `GET /audit`
is **run-scoped, not `created_by`-scoped**: a member must pass `?run_id=` naming
a run they own — no `run_id`, or one they don't own, both return an empty `200`
list, so a member's unfiltered audit feed is always empty by design (the console
reaches it from a run's Audit tab, whose "open full Audit" link carries
`?run_id=`). `GET /setup/status` redacts operator-diagnostic detail (checks,
secret names, runner detail) for a member.

**Workspace ownership (0.6, migration `0048`)** and **Secret ownership (0.7,
migration `0050`)** are the second and third owned nouns after runs.
`workspaces.owned_by` / `secrets.owned_by` hold the creating MEMBER's principal;
`""` — every pre-migration row, and everything an admin writes — means
**operator-owned**, i.e. exactly today's behavior.

- **Workspace CRUD/scan/build are owner-or-admin, not admin-only**
  (`getWorkspaceAuthorized`/`getWorkspaceReadable`, `internal/api/helpers.go`).
  Another member's owned workspace answers the **byte-identical 404** a missing id
  does. An OPERATOR-owned workspace answers a member's mutation with a **403** —
  it is listable and readable by every member, so there is no existence to hide.
  `GET /workspaces` returns the caller's own rows plus the operator-owned ones,
  never another member's.
- **A member's `local_dir` source is bounded by operator-set roots**:
  `WARDYN_USER_WORKSPACE_ROOTS` (and its per-member `_MAP`, which REPLACES the
  shared list for a principal that has an entry) in [ENV.md](ENV.md). Unset = no
  member `local_dir` mounts at all (fail closed); writability needs the separate
  `WARDYN_USER_WRITABLE_ROOTS` minus `WARDYN_USER_WRITABLE_DENY`.
- **Offboarding is `POST /workspaces/{id}/reassign`** (admin-only): returns the
  row to the operator (`owned_by=""`) and audits `workspace.reassign` with the
  departed member in `from_owner`. Idempotent, so a sweep over a departing
  member's ids never fails halfway. The row's `local_dir` sources stop being
  member-authored, so the member root and dotfile gates no longer bound them —
  they become ordinary operator mounts. Treat it like creating the workspace.
- **Offboarding a USER DRIVE is two halves, and only one of them is a product
  action — and the ORDER is not the obvious one.** Run `POST /drives/preview`
  **first, while the allocation still exists**, and write the object name down.
  The preview answers "what does this deployment say about THIS principal" by
  running the ordinary resolver (`resolveUserDriveFor`,
  `internal/api/user_drives_resolve.go`), and the resolver matches on the
  **grant**: delete the allocation first and the preview resolves nothing, so
  the one call that names the object a person is about to lose stops being able
  to name it. Paste the sign-in subject FIRST — on a `hash` drive the name keys
  on the first claim, and the API's `home_subject` says which claim it used (the
  console does not yet show it).

  *Then* delete the allocation (`DELETE /drives/grants/{id}`, admin-only,
  audited `drive.grant.delete`). It stops the mount at that person's next run and
  **deletes no data** — which is why the audit row carries the drive's declared
  `reclaim` intent (`retain` or `delete`), so the log records what the operator
  was told to do about the directory this allocation was the last pointer to.
  What is left behind is one object per person: a Docker named volume, or a
  subdirectory of the share the operator mounted host-side, or a
  PersistentVolumeClaim. A **managed** object (`docker_volume`, `k8s_pvc`)
  carries the drive row's **id** and the person's home name as labels, so a
  departed member's objects stay findable after the row is gone — that is the
  recovery path if you skipped the preview; a share's subdirectory and a static
  claim carry **nothing**, so for those the preview is the only thing that names
  the object at all. Reclaiming it is a deliberate command — `POST
  /drives/{id}/reclaim` (`wardyn drive reclaim`), or the substrate command by
  hand; both stay supported, and "Reclaiming a departed person's storage" below
  is the runbook for both. Deleting the **drive row** itself
  is a `409` while any allocation still points at it (`ON DELETE RESTRICT`), so
  the deallocation is always its own audited event and offboarding can never
  silently widen anything.
- **Secret write/delete moved from admin-only to self-service.** Any signed-in
  human may `PUT`/`DELETE /secrets/{name}` their OWN row
  (`secretOwnerFromRequest`: `""` for an operator, their own principal for a
  member). A member's `DELETE` of another principal's row is structurally
  unreachable (`secretstore.Store.For(owner)` never resolves it) and answers the
  byte-identical 204 a never-set name gets. The six model-credential
  names (`anthropic-api-key`, `openai-api-key`, `bedrock-api-key` and the three
  AWS SigV4 names `aws-access-key-id`/`aws-secret-access-key`/`aws-session-token`)
  are refused (403 `secret_name_reserved`) for every caller, the operator
  included: a model credential is stored on a model provider, by the person it
  belongs to, and wardynd deletes any left from before 0.8.2 at boot
  (`model_credential.retire` in [AUDIT-ACTIONS.md](AUDIT-ACTIONS.md)).
- **`GET /secrets` returns `{names, mine}`.** `mine` is always the queried
  namespace's own rows (reserved names filtered out). `names` keeps its pre-0.7
  meaning for an admin — the operator namespace, or one member's own rows with
  `?owner=<principal>` — and for a member narrows to the operator-owned names an
  eligible grant in the operator's ceiling actually pairs with a host, closing a
  name-enumeration gap.
- **A run resolves its owner's row, falling back to the operator's** — never
  another member's, even when an inline policy names it by hand. The upstream
  (corporate) proxy secret always stays resolved from the operator namespace:
  under a configured upstream the sidecar hands the corp proxy a HOSTNAME rather
  than a pinned address for every host it proxies (all of them, minus
  `upstream_proxy_no_proxy`), so a member-substitutable value there would put a
  member in control of which proxy resolves and dials every one of them.
- **`?owner=<principal>` is admin-only** on `DELETE`/`GET /secrets`, refused
  with a constant 403 for anyone else. **A `PUT` refuses it for everyone (0.8,
  `403`, audited `secret.write` `denied`)**: a credential is set only by the
  person it belongs to, so an admin can remove a person's credentials but never
  set one their runs would use under their name, and pre-provisioning a member's
  key before they sign in is no longer possible. The value names a
  HUMAN and is RESOLVED to the namespace key that person's own writes land in:
  matched case-insensitively against the principals this deployment knows, and
  mapped from the email form through the same (principal, email) pairing
  `POST /sessions/revoke` matches on. A subject is opaque and case-sensitive,
  so the fold cannot be a first-hit scan: an EXACT subject match wins
  outright, and a value that folds case-insensitively onto MORE THAN ONE known
  principal is refused `422` rather than resolved to whichever the directory
  happened to list first — guessing there would write a credential into the
  wrong human's namespace. An email address that pairs to no known
  principal is refused 422 rather than silently creating a namespace its owner
  never reads, and a cross-namespace `DELETE` that removed nothing answers 404
  rather than an idempotent 204.
- **Offboarding a person's credentials is `DELETE /people/{principal}/credentials`**
  (admin or `security_admin`, 0.8); `GET /model-providers/credentials` first lists what
  each person holds per model provider, with when it was added and last used. The erase
  deletes every credential in that person's namespace — keys, tokens and captured
  sign-ins — and answers `{"count": N}`; the
  principal resolves as `?owner=` does. In store mode each value leaves Vault or
  Key Vault before its row, and the answer adds `store`, `purged` and, when Key
  Vault kept soft-deleted copies, `recoverable_days`: the organisation can
  recover them for that long unless its vault operators purge them. It never
  answers success with a credential left behind (`500`, audited
  `credential.erase` `failure` with the count it did delete; run it again), and
  it never erases the operator namespace. A renewal of their AWS session, or
  an Azure DevOps refusal stamp, already in flight finishes first and is erased
  with the rest: the erase waits for the owner's AWS lock, the Azure DevOps
  sign-in's lock and the own-token write lock (for a renewal, at most about 21
  seconds), so a success is final for everything Wardyn itself was writing. That
  coordination lives in one process: on a deployment with more than one replica
  a second one can still interleave, and the erase's own re-list reports only a
  write it can see (tombstones are tracked in #1511). An Azure DevOps sign-in
  configuration that cannot be read refuses the erase (`503`,
  `credential_erase_signin_config_unreadable`, nothing erased). A person who
  reconnects afterwards writes new credentials, which stay. The erase does not
  reach into a running run: one that already holds a credential in memory keeps
  it. A run already going keeps a static key
  (an `api_key` injection is fetched once and cached for the run) until it ends,
  so also stop their runs (`POST /runs/{id}/kill`, the run kill switch).
  **Wardyn cannot revoke anything upstream**, with one exception: it first
  revokes the live Azure DevOps tokens it created for the person's runs
  (`ado_pat.revoke`, reason `offboarding`; one it cannot revoke expires by
  itself). Revoke the person's AWS, Anthropic and Azure DevOps sessions, any
  Azure DevOps token they pasted in themselves, and any gateway token, where
  they were issued, and disable them in the identity provider. A refused erase (a blank principal
  `400`, or one naming nobody or several people `422`) is audited
  `credential.erase` `denied`.
  **The erasure horizon, on the default (local Postgres) store, is your backup
  retention — not the API call.** `DELETE /people/{principal}/credentials`
  removes the live row; it does not, and cannot, reach a `pg_dump` you already
  took, a replica, or Postgres WAL. Until every backup made before the erase
  ages out of your retention window, the value is recoverable from it by
  whoever can read a backup, exactly as it was live (envelope v1 does not
  change this: the same key-encryption key that opened the row in Postgres
  opens the same bytes in a restored dump). With `WARDYN_KEK=transit`, the
  backup is only as erased as the KEK: rotating the Transit key past the old
  wrap (`wardynd -rewrap`, then raising `min_decryption_version` — "Key
  service: Vault Transit") is what actually forecloses an old backup, the same
  way `-rotate-age-key` does for the local key. In store mode (Vault, Azure Key
  Vault) the value itself never reaches your Postgres backup at all — the
  store's own deletion/retention is what governs it, as in "Removing a
  credential, and the erasure horizon" below for Key Vault, or your Vault KV
  engine's own versioning and delete-version policy.
- **Offboarding a person, in full.** The erase removes stored credentials and
  nothing else. In order:
  0. Find them and see what they hold: `GET /people?q=<email or subject>`
     (`wardyn people list --q`) lists each match with its live-session, token,
     SSH-key, credential and running-run counts, which the steps below act on.
  1. Disable the person in the identity provider, so no new sign-in succeeds.
  2. `POST /sessions/revoke` with their subject or email: ends their console
     sessions, refuses a UI-app session at its next re-check (an attach ticket
     minted before the revoke is refused at redemption) and revokes every `wdn_` API token they hold (its
     `tokens_revoked` count is the receipt; see "Per-user API tokens: stop
     sharing the admin token").
  3. Remove their registered SSH keys and end established SSH connections:
     [SSH access revocation](SSH.md#revoking-access-during-an-incident).
  4. Kill their running runs (`POST /runs/{id}/kill`). A run keeps a static
     key it was handed (an `api_key` injection is cached for the run) until it
     ends, whatever the erase does.
  5. Erase their credentials: `DELETE /people/{principal}/credentials`.
  6. Hand back their workspaces (`POST /workspaces/{id}/reassign`) and their
     user drives (the two-halves order above).
  7. Revoke upstream what Wardyn cannot: their AWS, Anthropic and Azure DevOps
     sessions, a token they pasted in themselves and any gateway token.

  One copy outlives all of this in memory: wardynd keeps an Azure DevOps
  sign-in's refresh token in its process-wide masking set, so output quoting it
  is still masked. It is never served or injected from there; it is let go a
  grace period after the credential is replaced, or when wardynd restarts. A
  run's own masking copies go the same grace after the run ends.
- **Dead sign-ins are not kept.** A captured AWS or Azure DevOps sign-in whose
  refresh token the provider refuses for good (`invalid_grant`) is deleted at
  that renewal, and a stored AWS sign-in is deleted by a daily sweep once it can
  no longer be used or renewed (its row's `expires_at`); both audit
  `credential.expired.delete`. A row the sweep cannot delete is kept, audited
  `failure`, and retried the next day. The person is then shown as not
  connected and signs in again. A Conditional Access refusal does not delete
  anything — the sign-in still works once the person is present. An Azure
  DevOps sign-in records no expiry, because Entra publishes none for its
  refresh token: an unused one is kept until the provider refuses it or it is
  erased.
- **Cross-user admin access is queryable.** An admin acting on a member-owned
  workspace stays the ADMIN in the audit actor (no impersonation; delegation
  is recorded as delegation — [Delegated run management](#delegated-run-management-portals)) with
  `workspace_owner` naming the member; `secret.write`/`secret.delete` carry
  `secret_owner` naming the non-"" namespace a write landed in (a member's own
  ordinary write included) — see [AUDIT-ACTIONS.md](AUDIT-ACTIONS.md).
- **Member model access.** A member's model access is their own credential on a
  model provider; no integration row is derived from anyone's convention-named
  secret, and no inline `api_key` grant naming a model vendor's host is admitted
  from a member's own secrets. See
  [USERS.md § Your model connections](USERS.md#your-model-connections).

**Deciding an approval is kind-restricted, not just owner-restricted**
(`decide()`, `internal/api/approvals.go`): a member may approve or deny an
`egress_domain` approval on a run they own. `credential` and `tool_call`
approvals stay **admin-only regardless of ownership** — the shipped default
policy requires approval on `github_token`, so a member self-approving their own
run's credential request would self-mint a real token, and self-approving a
`tool_call` re-opens exactly what the clamp (below) exists to bound.

**Optional: require a SECOND human on egress decisions.** Set
`WARDYN_EGRESS_SECOND_HUMAN=1` and the human who DECIDES an `egress_domain`
approval may not be the human who created the run. Off by default: turning it on
unprompted would deadlock every single-operator deployment. Both verbs are covered
— a self-*deny* is refused too. A refusal is a `403` recorded as `authz.denied`
with `reason: second_human_required`, landing **before** the decision is written,
so a refused decision leaves the approval `PENDING`. Scoped to `egress_domain`
only; `credential`/`tool_call` are already admin-only. A run with an empty
`created_by` (system-created follow-on runs) has no human creator to be the same
as, so the rule cannot apply. **Local mode REFUSES the switch** (`503`) rather
than enforcing it: local mode authenticates nobody, so both the decider and the
run's `created_by` come from the same client-supplied source — the DEV-ONLY
`X-Wardyn-Principal` header, honored there by design — and no request in that
mode can prove a second human decided. Configure SSO to use this switch, or
leave it unset. The refusal is scoped to `egress_domain` decisions, so nothing
else in local mode changes.

**Optional: the same for Azure DevOps capability escalations.** A member may decide
an Azure DevOps capability escalation on a run they own, and ownership is the whole
member rule, so a run's creator can approve their own escalation, admin-class
capabilities included. Set `WARDYN_CAPABILITY_SECOND_HUMAN=1` and the human who
creates the run can neither approve nor deny its Azure DevOps escalation; a
different administrator decides. It is a separate switch, off by default, with
every rule above: the same `403` / `second_human_required`, the approval left
`PENDING`, the `503` in local mode (plus a boot warning), and the `admin-token`
break-glass, whose `approval.second_human.bypass` row names this switch in its
`switch` field. Each switch governs only its own kind. A run's attention state
follows the setting, as it does for egress: it stops naming the creator as the
person who can act.

**Optional: four-eyes on governance writes.** Set `WARDYN_GOVERNANCE_SECOND_HUMAN=1` and no
single administrator can change a governance profile, an assignment, a capability grant, the
enforcement map, a value's availability, a user type's priority, a role mapping or a key-domain
assignment alone. With it on, a
human's write to `POST/PUT/DELETE /governance/profiles`, `POST/DELETE /governance/assignments`,
`POST/DELETE /permissions/grants`, `PUT /permissions/enforcement`, `PUT /permissions/availability/{kind}/*`,
`PUT /user-types/{id}` (when the priority changes), `POST/DELETE /access/mappings` or
`PUT/DELETE /key-domains/assignments/{subject_type}/{subject}`
is decoded and validated exactly as before and then stored as a pending change, answered `202`
with `Location: /api/v1/governance/changes/{id}` and
`{"pending_change": {id, target_kind, op, target_key, state, proposed_by, proposed_at, expires_at, diff}}`.
Nothing is applied. `diff` is rendered by the server, never by a client: the target's current row
and the proposed one (every ceiling passes the same read-redaction as a read of it) plus the changed
field paths; for an assignment, `diff.after` embeds the profile it points at as it stood at the
proposal. A second human approves it with `POST /governance/changes/{id}/approve`, or any
authorised human rejects it with `POST /governance/changes/{id}/reject` (an optional `reason`, at
most 512 characters, no control characters, recorded on the change and its audit row only).
`GET /governance/changes` lists the queue (`?state=` narrows it; pending by default) and
`GET /governance/changes/{id}` reads one. A change nobody decides expires after
`WARDYN_GOVERNANCE_CHANGE_TTL` (default `72h`). Nothing notifies anyone that a change is waiting:
approvers find them through `wardyn governance changes list` or the API.

- **Who may approve.** The approver must pass the predicate of the tier the write was proposed on,
  never a weaker one. Profile, assignment, grant, enforcement, availability and user-type priority
  changes are proposed on the security tier, so a security admin or a super admin approves; a security
  admin may approve a super admin's change. A role mapping is written on the super-admin tier, so only a
  super admin approves one: a security admin who tries is refused `403` `authz.denied` reason
  `admin_surface` (target `governance.change`), and role-mapping changes are neither listed to a security
  admin nor readable by one (`GET /governance/changes/{id}` answers `404`).
  The predicate is evaluated again inside the approval transaction: an API token that was revoked
  after it authenticated, one cut off by a session revocation, or one whose role no longer passes
  is refused and the change stays pending.
- **Distinct human.** An approval is refused (`403`, `authz.denied`, reason `second_human_required`,
  target `governance.change`) when the approver's principal equals the proposer's, or when both
  emails are non-empty and equal once case-folded. The proposer may reject their own change.
- **What applies.** One transaction locks the change, requires it pending and unexpired, compares
  the target (and, for an assignment, the profile it points at) and the deployment default with what
  the proposal reviewed, applies the write, and moves the change to `applied`. A write to the target
  in between (the break-glass included), a rename or ceiling change of the profile an assignment points
  at, or a changed deployment default makes the change `stale` (`409` `governance_change_stale`):
  it never overwrites. The write is re-validated against the current deployment default, so an
  approval cannot apply what a direct write would refuse. Two approvals racing on one change apply it
  once; the other is `409` `governance_change_not_pending`. A failure after the write rolls it back.
- **Exemptions.** Only these apply directly, with no second human: a profile update whose new
  effective profile is no more permissive than the current one (the same resolved comparison
  composition uses, `Leq`), a rename that changes nothing else, an availability `PUT` that leaves the
  restricted bit as it is, and a user type's name or description edit that leaves its priority alone.
  A profile update that changes the contact is held even when it narrows. Everything else is held,
  deletes and every assignment write included: deleting an assignment widens its subjects back to the
  deployment ceiling. No grant, enforcement, availability, priority or role-mapping write has a
  narrowing exemption, because each can widen: deleting a deny grant, lifting a restriction (which
  admits every person with no grant write), turning enforcement on, raising a priority (which moves a
  person matching two types onto the other at their next sign-in).
- **Per target.**
  - *Capability grant* (`POST`/`DELETE /permissions/grants`): the target is the grant's natural key
    (subject type, subject, capability, value); a delete by id is resolved to it at proposal, so a
    pending delete and a pending upsert of one grant collide. Staleness covers the row at that key.
  - *Enforcement* (`PUT /permissions/enforcement`): the whole-map replacement. A stale `If-Match` is
    refused `412` at proposal as for a direct write; the approval then compares the map's ETag, and
    the approval transaction, not the in-process lock a direct write takes, is what serializes it.
  - *Availability* (`PUT /permissions/availability/{kind}/*`): held whenever the stored restricted bit
    changes, in either direction. Staleness covers the bit and the allow rows naming the value, so a
    restriction accepted at proposal is applied only while its list is what it was.
  - *User-type priority* (`PUT /user-types/{id}`): held when the priority changes, whole (a name and a
    priority edit together are one held change). A name or description edit alone applies directly.
  - *Role mapping* (`POST`/`DELETE /access/mappings`): the posture-flip acknowledgement is judged at
    proposal and carried in the payload, and judged again when the change is applied. The lockout guard
    runs when the change is applied, against the claim snapshot of the **approver**; the proposer's own
    facts are not consulted (they may have been demoted since, and the second human is the safeguard
    against them). The `admin-token` is exempt from the guard as it is on a direct write. The tokens
    a demotion strands are revoked after the approval commits, as after a direct write.
- **Break-glass and local mode.** The `admin-token` principal applies a covered write directly and
  approves a change, each writing `governance.change.bypass` beside the target's own row. A deployment
  that wants this gate to bind holds the token out of band. Local mode authenticates nobody, so the
  proposer and approver are both client-supplied: with the switch on it answers every covered write
  and every approve or reject `503` (`governance_second_human_local_mode`).
- **Audit.** `governance.change.propose`, `.approve`, `.reject`, `.expire` and `.bypass`
  ([AUDIT-ACTIONS.md](AUDIT-ACTIONS.md)). On approval the target's own row (`governance.profile.write`,
  `governance.assignment.write`, ...) is written too, its actor the approver, carrying `change_id` and
  `proposed_by`. The proposer, the approver, their emails and a rejection's `reason` are personal fields: the
  `audit_personal_fields` erasure scope clears them from the change rows, and a pending change whose
  proposer is erased expires, so a change with no recorded proposer can never be approved.
- **Residual risks.** The `admin-token` is single-human by design. A database writer can change the
  tables directly: the audit chain then shows a target row with no `propose`/`approve` pair, which is
  detection, not prevention. Each change is reviewed alone: two separately approved changes can compose
  into a widening neither diff shows. Not covered here: the rest of the governance-adjacent writes
  (workspace egress lists, `/policies`, `/site-config`, `/integrations`, the approval `always` scope,
  and user-type create and delete stay single-human until their own lanes).
- **In the console.** The Governance screen's Changes tab lists the pending changes with the server's
  diff, and an approver approves or rejects there (a reason is optional). A covered write made in the console
  that is held shows "Submitted for approval" at the place it was made, never a save. Approve is disabled on
  your own proposal, and the server remains the authority.

**The `admin-token` principal BYPASSES it**, and you should plan around that. A
bare `WARDYN_ADMIN_TOKEN` caller is attributed `system`/`admin-token` because a
shared token carries no per-human identity — there is no second human to compare
it against, and `X-Wardyn-Principal` is ignored off local mode specifically so a
token bearer cannot forge one. Refusing the token instead would lock you out of
your own approval queue the moment SSO breaks, so the bypass is the deliberate
break-glass. It is not silent: every one writes an
`approval.second_human.bypass` audit event beside the `actor_type=system`
`approval.decide`. **For this gate to actually bind, treat the admin token as a
break-glass credential** — configure SSO, and hold the token out of band.

**A `credential` decision carries one scope: `run` — the per-run credential
lease.** Every other scope on a `credential` approval is a `400`, and so is any
scope on a `tool_call`. `run` exists because a `git_pat` installs a *standing*
credential helper git invokes on every operation
(`docs/adoption/corp-network-onboarding-findings.md` B2). Approving with
`decision_scope=run` (`wardyn approval approve <id> --scope run`) makes that one decision
re-mintable for the rest of the run. Three things bound it:
**`git_pat` only** (`github_token` is brokered proxy-side, `ssh_key` is
materialized once and wiped, `api_key` never leaves the broker, so none has the
standing-consumer problem, and `broker.leaseCoversRemint` refuses a lease for
them even under a `run`-scoped decision); **per scope** (if the grant's scope no
longer matches what the human approved, the lease does not carry over); and
**killed by revocation** (a leased re-mint still runs the whole mint transaction,
so the kill-switch cascade ends it the moment the revocation commits).

`credential.mint` carries `lease: true` plus the raw `decision_scope`, so the
audit says which mints were the human's and which the lease's. The comparison is
deliberately **raw**, never `ApprovalScope.Normalize()`d — an empty
`decision_scope` normalizes to `run`, and every credential approval decided
before this feature carries an empty one, so a normalized comparison would have
turned every legacy approval into a standing lease on upgrade. Nothing you
approved before v0.6 leases anything.

**An `egress_domain` decision's *scope* adds a second, narrower gate — and one of
the four scopes is gated on ROLE, not ownership.** A member who owns the run may
choose `once`, `run`, or `until`; each stays inside that run's own proxy cache.
`always` is **admin or `security_admin`, regardless of run ownership**
(`decide()` rule 6, same file; the check is `isSecurityOperator`, in LOCKSTEP
with `authorizeUserDecision`): it writes a durable entry onto the run's
workspace (`approved_egress` on approve, `denied_egress` on deny) — the SAME two
columns the `approved-egress`/`denied-egress` routes write, and those routes sit
on that same `securityOps` tier, so the gate keeps the approval queue from being
a way around them for anyone below it. The
refusal is a `403`, not the ownership checks' `404`: the caller has already proven
the approval exists, is `egress_domain`, and is theirs. It is checked before the
run is confirmed to reference a workspace at all — authorization before
validation, so a member's rejection depends only on role (see
[POLICIES.md](POLICIES.md) "Approval decision scopes" for the
workspace-resolution check this precedes).

**`PUT /workspaces/{id}/denied-egress` is the only way to undo a `deny · always`
decision.** Full-replace, same shape as `approved-egress` (omit a host to un-deny
it — there is no per-host delete). Deny beats allow everywhere the proxy evaluates
policy, so a `deny · always` on a model-provider host, or one a required
integration injects into, permanently breaks that workspace's proxy-side
injection for every future run. `denied-egress`'s validator is deliberately
narrower than `approved-egress`'s (plain host shape only, no model-provider reject
set) so it keeps working as the escape hatch even when the workspace is already
bricked (`handleSetDeniedEgress`, `internal/api/workspaces.go`).

**A member's own policy is clamped, not trusted.** `POST /runs`' `inline_policy`
(and its preflight dry-run) is clamped to the operator's `DefaultPolicy` ceiling
before resolution (`composer.Clamp`, `internal/composer/clamp.go` — the same clamp
bounds an AI-composed policy and a Record Mode-synthesized one): confinement
raised to the floor, egress intersected down to the allowlist, `first_use_approval`
raised to the stricter of the two, `llm_inspection` inherited when the ceiling
sets one, resources and `auto_stop_after_sec` capped, grants narrowed to what the
ceiling allows, and `workspace_mounts` dropped entirely. An admin's own
`inline_policy` is not clamped.

**A first claude-code run parks on nothing the product itself needs — on an
image rebuilt from the 0.7.5 tree.** The claude-code image turns off the CLI's
own fetches — `CLAUDE_CODE_DISABLE_OFFICIAL_MARKETPLACE_AUTOINSTALL=1` (the
plugin-marketplace auto-install, which is what reached `downloads.claude.ai`
and `github.com`), `DISABLE_AUTOUPDATER=1` and
`CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1`; see
[corp-image-authoring.md](adoption/corp-image-authoring.md) "Stop the agent
fetching on its own behalf" — and writes `{"hasCompletedOnboarding": true}` into
the sandbox's `~/.claude.json` before the CLI starts. Without those, the first
Claude Code run anybody launched met the CLI's theme picker and then first-use
approvals for hosts nobody had asked for. The shipped default policy
(`examples/policies/default.json`) is deliberately unchanged: the fix is that the
traffic no longer happens, not that those hosts are now allowed. **This applies
to the claude-code image only** — the `codex-cli` image still reaches for several
hosts of its own at start (see the CHANGELOG's known gaps). **And only to an
image actually carrying the three `ENV` lines**: `agent-claude-code` (where they
were measured) is not a published image — `agent-base` is what ships, and it now
carries the three lines too, so any image built `FROM ghcr.io/cjohnstoniv/agent-base:0.7.6`
inherits them. An image on another base, or an older tag pinned in
`WARDYN_AGENT_IMAGES`, still parks on the CLI's own bootstrap; see
[corp-image-authoring.md](adoption/corp-image-authoring.md) for the rebuild
recipe and the CHANGELOG's Known gaps for the full statement.

What an interactive run still shows on first use is Claude Code's
**workspace-trust** prompt (`Accessing workspace: …` / `1. Yes, I trust this
folder`). That one is a security question — it gates a cloned repo's own project
settings taking effect — and Wardyn does not answer it for you. Pressing Enter
there raises no approvals. A run launched with *"Let it use tools before I
attach"* also parks on Claude Code's own *Bypass Permissions mode* confirmation
(default "No, exit") until someone attaches and chooses Yes — Wardyn does not
answer that one either ([THREAT-MODEL.md](../threatmodel/THREAT-MODEL.md)
§4.7).

**The desktop tier's standing honesty gap: the operator IS the admin.**
[The desktop tier](../deploy/desktop/) (`WARDYN_LOCAL_MODE=true`) has no member
role at all — local-mode callers are *always* admins
(`Server.requireOperator`'s own doc says so), so the unclamped branch above is the
default there. An `inline_policy` the developer submits is bounded by nothing
`WARDYN_DEFAULT_POLICY` sets, and setting one is one ordinary API call. What still
holds: the unclamped spec lands on the audit feed as `policy.inline.apply` before
`run.create` ([AUDIT-ACTIONS.md](AUDIT-ACTIONS.md)), egress still has no route off
the sandbox except `wardyn-proxy`, and the session is still recorded. A governance
control, not a containment boundary against the operator holding the laptop. Full
accounting: [docs/DESKTOP.md](DESKTOP.md) "Tamper posture, stated honestly".

### Four-eyes on governance writes: a walkthrough

The rules are in "Optional: four-eyes on governance writes" above; this is the order an operator
works in. The switch and its TTL are in [ENV.md](ENV.md); the audit rows are in
[AUDIT-ACTIONS.md](AUDIT-ACTIONS.md); the residuals are in
[THREAT-MODEL.md](../threatmodel/THREAT-MODEL.md) §5 "Four-eyes on governance writes".

**1. Before you turn it on.** All four must hold, or the switch deadlocks you or does not bind:

- **SSO is configured.** Local mode authenticates nobody, so with the switch on every covered write
  and every approve or reject answers `503` `governance_second_human_local_mode`.
- **The admin token is held out of band.** It applies a covered write directly and approves a change,
  each audited as `governance.change.bypass`. It is the break-glass and the one way past this gate.
- **Two humans can approve.** Every covered target except role mappings needs a second security admin
  or super admin; role mappings need a second super admin (see step 6).
- **Every CLI and SDK caller is upgraded.** A client built before 0.8.6 decodes the `202` as an empty
  object ([sdk.md](sdk.md) "Old clients"). Nothing applies without approval, but the error it reports
  is confusing.

Then set `WARDYN_GOVERNANCE_SECOND_HUMAN` (and, if `72h` is wrong for your approvers,
`WARDYN_GOVERNANCE_CHANGE_TTL`) in the daemon's environment; on Kubernetes that is the chart's
existing `env` map. Both are read at boot. Turning the switch off later leaves pending changes
approvable and rejectable under the same rules, self-approval included; new writes apply directly.

**2. What is held, and who approves it.** A write the table lists is held when the condition in its
row is true, and approved at the tier it was proposed on.

| Write | Held when | Approver |
|---|---|---|
| Governance profile create, update, delete | the write is not exempt (below) | security admin or super admin |
| Governance assignment upsert, delete | always, deletes included | security admin or super admin |
| Capability grant upsert, delete | always | security admin or super admin |
| Enforcement map replace | always | security admin or super admin |
| Availability set | the stored restricted bit changes, in either direction | security admin or super admin |
| User-type priority update | the priority changes | security admin or super admin |
| Role mapping upsert, delete | always | **super admin only** |
| Key-domain assignment set, delete | always | security admin or super admin |

Exempt, so applied directly: a profile update whose new effective profile is no more permissive than
the current one, a rename or description edit that changes nothing else, an availability `PUT` that
leaves the restricted bit as it is, and a user-type name or description edit that leaves its priority
alone. Nothing else narrows by shape: a delete can widen (a deny grant, an assignment, a restriction),
so no delete is exempt on its own.

**3. Propose, review, decide.** A held write answers `202` with the change. Its `diff` is the
server's: the current row, the proposed row and the changed field paths. The proposer, or anyone
else, then:

```
wardyn governance set ci-governance.json          # held writes print as pending; exit 0
wardyn governance changes list                    # pending by default; --state applied|rejected|expired|stale
wardyn governance changes approve <change-id>     # a different human, at the right tier
wardyn governance changes reject <change-id> --reason "widens egress past the review"
```

`governance set` exits 0 on a pending result and skips `--prune` until every write is decided, so
run it again afterwards. Running the SAME document again before then is safe: a target whose held change is that
very write is reported as pending, with the held change named, and the rest of the file is applied. A
document edited since is refused at that target (`409` `governance_change_pending`, naming the held
change) until the held change is decided; nothing of the edit is stored. The console does the same on the Governance screen's Changes tab, with the
diff in a drawer; Approve is disabled on your own proposal. Nothing notifies an approver that a
change is waiting, so name who looks, and how often, in your own runbook. There is at most one
pending change per target: a second proposal at the same target is `409` `governance_change_pending`,
which names the first and carries it in `pending_change`, with `pending_change_matches` saying
whether it is the very write that was refused (same operation, same payload). A new profile's target is its name while its
create waits, so creating the same name again (by `POST`, or `PUT` at any new id) meets the held create.

**4. Expiry and stale changes.** A change nobody decides within the TTL reads as `expired` and cannot
be approved (`409` `governance_change_not_pending`); propose it again. A change is `stale` (`409`
`governance_change_stale`) when, at approval, the target, the profile an assignment points at or the
deployment default differs from what the proposer's diff showed. Approval never overwrites: a stale
change is dead, and the remedy is to propose the write again against the current state. A direct
write in between (the break-glass included) is the usual cause.

**5. Reading the audit.** Filter `GET /audit` by `action_prefix=governance.change.`. A normal change is
a `governance.change.propose` row (actor the proposer, `Target` the change id) followed by a
`governance.change.approve` row and, beside it, the target's own row (`governance.profile.write`,
`capability.grant.create`, `access.role_mapping.write`, ...). That row's actor is the approver and
it carries `change_id` and `proposed_by`: two named humans for one change. Also look for:

- `governance.change.bypass`: the admin token wrote or approved. Alert on it.
- `governance.change.approve` with outcome `failure`: the approval did not apply (`error` is `stale`,
  `not_pending` or `error`).
- `authz.denied` with reason `second_human_required` or `admin_surface` and target
  `governance.change`: a self-approval, or a security admin at a role-mapping change.
- A target row with **no** `propose`/`approve` pair beside it, switch on: a write that did not come
  through the API. See the threat model's "database is not four-eyed" residual.

**6. One super admin.** Role mappings are written on the super-admin tier, so only a super admin may
approve one, and nobody approves their own. A deployment with a single super admin therefore cannot
change a role mapping while the switch is on, except through the admin token (audited as a bypass).
Add a second super admin before you enable the switch, or accept the token as the path for mapping
changes. The lockout guard is judged against the **approver's** own roles at apply, so the approver
cannot be the person whose mapping change would strip their own admin.

**7. What stays single-human.** Workspace approved and denied egress lists, record-egress promotion,
`/policies`, `/site-config`, `/integrations`, the approval `always` scope, user-type create and
delete, and the SCIM purge's deletion of a person's own assignments and grants. The threat model
lists each with its reason.

### Reclaiming a departed person's storage

Deleting a drive removes its row and deleting an allocation stops the mount;
neither deletes a byte. The storage object one person's allocation resolved to
— a Docker named volume, a PersistentVolumeClaim, or a directory on a share —
outlives both. **Reclaiming it destroys data and nothing undoes it.** On
Kubernetes, Wardyn deletes the claim; what happens to the bytes then follows the
StorageClass's `reclaimPolicy`: `Delete` (the usual default) destroys them,
`Retain` leaves them on the released PersistentVolume until an operator removes it.

There are two supported ways, and both stay supported: the substrate command by
hand (the recipes in "User drives on Docker" and "User drives on Kubernetes"
below), or the product's own verb.

**The verb.** `POST /api/v1/drives/{id}/reclaim`, body
`{"subject_type":"user","subject":"<the person's sign-in subject>"}`, or from
the CLI:

```sh
wardyn drive reclaim <drive-id> --subject <sign-in subject> --yes
```

It answers `deleted` (this call destroyed the storage) or `already_absent`
(nothing answered to the name). **There is no console button**: a destructive
confirmation is a screen, and this one has no approved mock, so the API and the
CLI are the whole surface in 0.8.

**Do it in this order.** Reclaim the storage **first**, then delete the
allocation. A home directory an admin pinned (`home_override`) lives on the
allocation, so once that row is gone the pinned name cannot be recovered from
the database and the object name this verb derives is the drive template's
instead — a different directory. Check the name it reports against
`POST /drives/preview`, which prints the object name for a principal.

**What refuses it, and why each one is there:**

| Refusal | What it means |
| --- | --- |
| `403` | Not a super-admin. Same tier as the rest of `/drives`, for a sharper reason: this one is irreversible |
| `400` | The `subject_type` is `group` or `all`. Those give **every** person they match their own object, so they name no single thing to destroy — reclaim the people one at a time |
| `422` | The drive is a share (`host_path`, `k8s_pvc_static`). Wardyn did not create that object and never deletes it; reclaiming it is a change on the share itself. There is no recursive delete in this product, at any privilege, for any backend |
| `409` | A run still holds the object, a reclaim is already in flight (a claim already `Terminating`), or the object answering to that name is **not this drive's** (the driver re-checks the `wardyn.drive` / `wardyn.home` / `wardyn.subject` labels before issuing any delete, and on Kubernetes binds the delete to the claim it checked: a claim deleted and re-created under the same name in between is refused, never deleted — Docker's volume remove takes no such precondition) |
| `501` | This deployment's runner cannot reclaim at all — use the substrate command |

**On Kubernetes the daemon does not even hold the verb by default.** The chart's
Role carries `persistentvolumeclaims: [get, create]` and adds `delete` only
under `drives.reclaim.enabled` (default `false`, see
[the chart's values](../deploy/helm/wardyn/values.yaml)). Leave it off and every
attempt ends in the apiserver's own `403`, recorded as a failed `drive.reclaim`
row; turn it on only when your offboarding runbook calls the API instead of
running `kubectl delete pvc` by hand. Nothing else changes either way: no run
path, no teardown and no sweep can reach a claim on either setting.

**Every attempt that reaches the substrate is audited**, `409` refusals and
failures included, as `drive.reclaim` — naming the drive, the person, the backend, the object and what became of it
([AUDIT-ACTIONS.md](AUDIT-ACTIONS.md)). That row is deliberately the only
durable record: the drive row and the allocation can both be gone by the time
anyone reads the trail. The `400`, `422` and `501` answers above, and a daemon
that cannot read the allocation, are refused before any object is addressed and
write no `drive.reclaim` row.

### User drives on Docker

A **user drive** is persistent storage an admin registers once and allocates to
people or groups; a member mounts theirs per run at `/home/agent/drive`. On a
Docker deployment there are two backends, and the difference is who owns the
bytes. With `storage.user_drive.disabled` set, all three surfaces answer that one
switch identically — a run is refused 422, `POST /drives/preview` answers the same
422, and `GET /me` reports no allocation at all — so nothing in the console ever
offers a mount the create path refuses (see "Turning drives OFF deployment-wide").

**`docker_volume` — Wardyn allocates.** A per-person named volume
(`wardyn-drive-<drive-slug>-<home>`), created on first use with the `local`
driver and mounted at the reserved target. Nothing to configure. It carries four
labels: `wardyn.managed=true`; `wardyn.drive` = the **drive row's id** (the name
folds the drive's SLUG, which a rename changes, and the id never does — so the
label is the only key that still finds a drive's volumes across one, which is
what the reclaim recipes below select on); `wardyn.home` = that person's directory name; and
`wardyn.subject` = a **digest** of the person
themselves (never their claim — see the restore note below). Reclaim is a
command, never a button — either `wardyn drive reclaim` ("Reclaiming a departed
person's storage" above) or, by hand:

- one person: `docker volume rm wardyn-drive-<drive-slug>-<home>` — `POST /drives/preview`
  prints the object name for a principal — paste the sign-in subject FIRST: on a
  `hash` drive the name keys on the first claim, and the API's
  `home_subject` says which claim it used (the console does not yet show it);
- one drive, everybody: `docker volume ls --filter label=wardyn.drive=<drive id>`
  lists every volume that drive allocated.

**Restoring one by hand: re-create it with its labels, and with no `--opt`.**
Wardyn reuses a volume that already answers to the name, but only when it has
Wardyn's own shape — the `local` driver and **no driver options** — and refuses
to mount anything else rather than adopt it. That refusal is deliberate: a
volume an operator precreated with `--opt type=cifs --opt o=…,password=…` would
otherwise become somebody's drive, on a share credential Wardyn never chose. So
a restore is

```
docker volume create \
  --label wardyn.managed=true \
  --label wardyn.drive=<drive id> \
  --label wardyn.home=<home> \
  wardyn-drive-<drive-slug>-<home>
```

then copy the data in. `wardyn.drive` carries the **drive row's id** (the `id`
on `GET /api/v1/drives`, and the `Target` of that drive's `drive.write` audit
row), not the volume's name — the id is what groups every person's object under
the drive that allocated them. Get it wrong and Wardyn **refuses** the volume
rather than adopting it: a label naming a *different* drive is how two drives
whose home names collided would otherwise hand one member the other's storage.
A volume restored with **no** `wardyn.drive` label at all still mounts (that is
the fall-back this path is for, and every volume created before the label
carried an id has none) — it just no longer answers
`docker volume ls --filter label=wardyn.drive=<drive id>`.

Wardyn also stamps **`wardyn.subject`**, a digest of the person the volume was
allocated to — never their sign-in claim, because `docker volume inspect` echoes
labels to anyone who can reach the daemon. It is the discriminator `wardyn.drive`
cannot be: a volume name carries the drive and the *home* and no person at all
(`DriveObjectName` mints `wardyn-drive-<drive-slug>-<home>`), so one drive whose
home template folded two people onto one directory would produce one volume that
*both* their allocations agree belongs to this drive. Wardyn refuses to mount a
volume stamped for a different person.

A managed drive can no longer be *authored* into that state. The rule is
`ManagedBackendRejectsTemplate` in `internal/types/user_drive.go`, and as of 0.7
it refuses **every** non-`hash` template on a `docker_volume` or `k8s_pvc`
backend — `sub` as well as `email_local` — at **both** enforcement points: the
write boundary, and the run-time resolver that derives the home. It used to name
`email_local` alone, and the resolver keyed on `email_local` alone, so a `sub`
row written by an older binary (or by hand) was refused on write and still
mounted. The folded homes `wardyn.subject` discriminates are therefore rows from
before that widening, or hand-made ones — which is exactly why the label is still
checked rather than assumed away.

You need not compute the digest for a restore (it is a truncated sha256 of the
sign-in subject): **leave `wardyn.subject` off** the `docker volume create`
above and the volume mounts, exactly as a label-less `wardyn.drive` does.

**`host_path` — you already mount the share.** Wardyn binds **one person's
subdirectory** of a tree the *operator* mounted host-side. Wardyn never performs
the share mount, never holds a share credential, and never creates a volume with
`--opt type=cifs`: those options are stored with the volume and echoed by
`docker volume inspect` to anyone who can reach the daemon. The recipe:

1. **Mount the share on the host**, in `fstab` or a systemd mount unit:

   ```
   # SMB — the credential is a root-owned 0600 file, never a mount option in a table
   //nas.corp/wardyn-drives /srv/wardyn-drives cifs credentials=/etc/wardyn/smb.cred,uid=1000,gid=1000,file_mode=0600,dir_mode=0700,vers=3.1.1 0 0
   # or Kerberos instead of a service account: replace credentials= with sec=krb5
   # NFS — export it Wardyn-dedicated and squashed to the sandbox uid
   nas.corp:/export/wardyn-drives /srv/wardyn-drives nfs4 rw,hard,_netdev 0 0
   ```

   The matching NFS export line, on the NAS:
   `/export/wardyn-drives 10.0.0.0/8(rw,all_squash,anonuid=1000,anongid=1000)`.

2. **Make one `0700` subdirectory per person** under the mount point, named the
   way the drive's home template resolves. There are three templates —
   `hash` (a digest of the drive id and the subject), `sub` (the sign-in subject
   claim verbatim) and `email_local` (the part of the email claim before the
   `@`, the usual shape of a corporate home) — and a **share** drive may only
   use `sub` or `email_local`: a hash would name a directory nobody created.
   Per person, a grant's *home override* pins any other name. Wardyn does **not**
   `mkdir` on a share — a missing home is a `422` at run create ("directory
   `<home>` does not exist on the share — ask an admin to create it"), not a
   directory Wardyn invents inside somebody's NAS.

   Two people whose email addresses share the part before the `@` resolve to the
   **same** home under `email_local` — the segment is validated, not proven
   unique. On a share that is a tree you own and can inspect: use `sub`, or a
   per-person home override, where it can happen.

   **A managed drive takes `hash` and nothing else.** As of 0.7 the refusal
   covers **every** non-`hash` template on a `docker_volume` or `k8s_pvc`
   backend — `sub` as well as `email_local`. Registering either answers a `400`
   beginning `invalid drive: home_template "<template>" is not allowed on a
   managed backend`, and the message names `hash` as the single remedy. Two
   reasons, and `sub` fails the second one: Wardyn names a managed object after
   the drive and the home and nothing about the person
   (`wardyn-drive-<drive-slug>-<home>`), so within one drive `email_local`
   allocates two colliding people one volume with write access to each other's
   files whenever the drive is
   writable — and an object *name* is what `docker volume ls` and
   `kubectl get pvc` print with no inspect or describe, so a verbatim `sub`
   publishes the sign-in subject to anyone who can list the daemon or the
   namespace, in the more exposed of the two places the label vocabulary already
   refuses to put it. `hash` is unique and reveals nothing, and it is the
   default; a **share** backend keeps every template, because its tree is one
   you own and navigate by hand. A row written before this rule is refused at
   *run* time too (`drive: this deployment cannot mount your drive (…)`), and
   every managed volume carries a `wardyn.subject` label — a digest of the
   principal, never the claim — that the driver refuses to mount for anybody
   else.

3. **Set the ceiling**: `WARDYN_USER_DRIVE_HOST_ROOTS=/srv/wardyn-drives`
   ([ENV.md](ENV.md)). Unset means **no `host_path` drive may be registered at
   all** — the same fail-closed posture `WARDYN_USER_WORKSPACE_ROOTS` takes,
   one level up: a drive's `host_root` is authored in the database by an admin
   and its subdirectories are bound into *other people's* sandboxes, so the
   allowlist over it lives where a console compromise cannot reach it. The
   driver re-checks the **symlink-resolved real path** against these roots as
   the last thing before the container is created, so a home directory replaced
   by a symlink out of the share after the drive was registered is refused at
   run time too — and it adds two checks the ceiling cannot make, because every
   other drive's tree and every sibling home are inside it as well. The resolved
   directory must be **inside this drive's own `host_root`**, which catches a
   home replaced by a link into *another* `host_path` drive's root (a ceiling
   naming both roots allows either tree, so it cannot tell one drive's from the
   other's); and it must still be **named after the person it resolved for**,
   which catches a home replaced by a link to the home *next to it*. A home
   symlinked deeper inside its own drive's root — homes filed under a year or a
   department — still works, as long as the directory keeps its name; a home
   symlinked onto a *second export* no longer does, even when that export is
   also a configured root. Give the drive the root its homes actually live
   under, or register a second drive for the second export.

   **Two `host_path` drives may not nest.** Registering a drive whose
   `host_root` is inside — or contains — another `host_path` drive's `host_root`
   answers `422`, naming the other drive. Two `host_path` drives may share one
   `host_root` — a read-write and a read-only view of `/srv/homes` is a
   supported shape, and only a NESTED root is refused — and so are sibling
   trees; what is refused is one drive rooted inside a tree whose directories
   another drive's members can rewrite from inside a run. They must agree on
   `home_template`, and a second one that disagrees is refused `409`: a
   share's storage object is `<host_root>/<home>` with no drive component, so
   two different derivation rules over one tree hand two different members the
   same directory (a member whose `sub` is `alice` and a member whose address
   is `alice@corp.example` both derive `alice`).

   The question is asked on the stored strings **and again on the
   symlink-resolved paths**, and either answer refuses — a root that is a link
   into the other drive's tree nests exactly as surely as a literal path does.
   So the refusal **names where each root resolves** whenever that differs from
   what was typed: *host_root "/mnt/teamshare" (resolves to
   "/srv/shares/alice/team") is inside drive "Corp NAS"'s host_root
   "/srv/shares" — …*. Without it an admin reads a refusal about two paths that
   plainly do not nest, and the one fact that explains it — the hop the link
   makes — is the one thing the console form cannot show them. A root that no
   longer resolves on this host falls back to the lexical answer rather than to
   a refusal, so one dead row cannot block every new drive
   (`driveHostRootNesting`, `internal/api`). Two admins creating nested drives at
   the same instant can still both be stored — the gate is a read followed by an
   unconditional write, and the database-level form is 0.7.1.

   **And the drive ceiling must not overlap `WARDYN_USER_WORKSPACE_ROOTS` —
   the member ceiling defeats per-person isolation where they meet.** Per-person
   isolation is the **bind of the subdirectory**: Wardyn hands a run one home out
   of the share and refuses a source that resolved to the root. A member
   workspace is a different surface with a different rule — a member names a
   directory under `WARDYN_USER_WORKSPACE_ROOTS` and binds it **whole**,
   writable where `WARDYN_USER_WRITABLE_ROOTS` allows it, and that path
   consults no drive allocation at all. Point the two ceilings at one tree and a
   member onboards the share as a workspace and mounts **every** person's home.
   Each list is valid on its own, so wardynd compares the pair at boot and
   **WARNs** — it does not refuse, because an operator may have opened a tree to
   both deliberately and a boot refusal would take a running deployment down on
   upgrade (`MountCeilingOverlapWarnings`,
   `internal/runner/user_drive_mount.go`). Three shapes earn the line, each
   behind the prefix `wardynd: mount ceilings overlap — `:

   | Shape | The line says |
   |---|---|
   | The two lists name the same tree | ``WARDYN_USER_WORKSPACE_ROOTS and WARDYN_USER_DRIVE_HOST_ROOTS both name "<p>": a member can onboard that directory as a workspace and bind the WHOLE share, every other person's home included, without a drive allocation. Point the drive ceiling at the share and the member ceiling somewhere else`` |
   | A member root CONTAINS a drive root | ``WARDYN_USER_WORKSPACE_ROOTS contains "<m>", which holds the WARDYN_USER_DRIVE_HOST_ROOTS entry "<d>": a member can onboard that share as a workspace and bind it whole, every other person's home included, without a drive allocation. Point the member ceiling at a tree that does not contain the share`` |
   | A member root is INSIDE a drive root | ``WARDYN_USER_WORKSPACE_ROOTS contains "<m>", which is INSIDE the WARDYN_USER_DRIVE_HOST_ROOTS entry "<d>": member workspaces would be authored inside a share whose directories Wardyn hands out one person at a time. Point the member ceiling outside the share`` |

   Every member ceiling is compared, the shared list **and** each
   `WARDYN_USER_WORKSPACE_ROOTS_MAP` per-principal override — an override
   *replaces* the shared list, so it is a ceiling in its own right. The
   comparison is **lexical**, on the values as configured: boot is not the place
   to touch a share that may not be mounted yet.

   **What the operator gets when a bind is refused.** The member-facing hint
   from a driver-side share refusal carries the drive and the directory and
   never a path; the paths go to the log, on the same run, as
   `wardyn: user drive: this share mount was refused at bind time` with `source`,
   `real_path` and `host_root` attributes (`RefuseUserDriveBind`,
   `internal/runner/user_drive_mount.go`). That split is the rule everywhere on
   this path — see the member's own refusal vocabulary in
   [docs/design/user-drives-prompt.md](design/user-drives-prompt.md) §7.7 and
   §7.9.

**On the Compose stack, wardynd must be able to SEE the root — set two
variables.** The bind's source is resolved by the host daemon (wardynd's
sandboxes are sibling containers), but the ceiling check resolves symlinks and
fails closed on a path it cannot stat, so a `host_path` drive registered from a
containerised wardynd is refused unless the share is visible inside it too.
`docker-compose.yaml` carries both halves already — nothing to hand-edit:

```
WARDYN_USER_DRIVE_HOST_ROOTS=/srv/wardyn-drives   # the ceiling wardynd enforces
WARDYN_USER_DRIVE_HOST_ROOT=/srv/wardyn-drives    # compose binds this one, RO, same path
```

in `deploy/compose/.env` (or the environment `docker compose` is run with).
Unset, both default to nothing exposed — the same opt-in posture
`WARDYN_WORKSPACES_ROOT` and `WARDYN_USER_WORKSPACE_ROOTS` take.

**One root on Compose.** The ceiling is a CSV and may name several roots;
the bind is singular, because compose cannot expand a CSV into volume lines. A
deployment whose ceiling names more than one root adds one more volume line per
extra root in `deploy/compose/docker-compose.yaml`, copied from the
`WARDYN_USER_DRIVE_HOST_ROOT` line — or runs wardynd on the host, or on
Kubernetes, where no bind is involved and the ceiling is the only thing to set.

Read-only is enough for wardynd: it stats the tree and never writes to it. It
does need **search (`x`) permission down to the person's directory**, though,
because the bind-time ceiling check resolves symlinks in *wardynd's own
process* — so a CIFS mount table line like the `dir_mode=0700,uid=1000` one
above works when wardynd runs as root or as uid 1000, and otherwise needs
`dir_mode=0750,gid=<wardynd's gid>` (or the equivalent NFS export mode). A
share wardynd cannot traverse fails every drive on it closed at run create —
with `drive: this deployment cannot mount your drive (drive "<name>" is on a
share this deployment does not allow — ask an admin)` when the **root itself**
is what wardynd cannot resolve, and with `drive: directory <home> does not exist
on the share — ask an admin to create it` when the root resolves but the
person's directory does not stat (a home that was never created, and a home
behind a directory whose permissions hide it, are the same sentence). **Neither
422 names a path**, deliberately: both are read by the MEMBER, so the diagnosis
— the drive's `host_root`, the whole `WARDYN_USER_DRIVE_HOST_ROOTS` list, and
the check's own sentence — goes to wardynd's log instead, as `wardynd: user
drive: a stored share drive's host_root is no longer allowed by this
deployment`, which is where the admin who can act on it is looking
(`driveShareIsBindable`, `internal/api/user_drives_run.go`). The sandbox's own mode comes from
the allocation, not from this line. A `docker_volume` drive needs none of this —
there is no host path to see.

**Why every sandbox is uid 1000, and what that buys.** Every agent image is
`USER agent` (uid 1000), and every agent image pre-creates `/home/agent/drive`
owned by agent — the ones built on a public base do it themselves, the ones
built on a sibling image inherit it — so a fresh managed volume inherits that
ownership by Docker's copy-up. Isolation between people is the **bind of the
subdirectory**, never the uid: a run sees its own home and has no path to the
root or to anyone else's. NFS `AUTH_SYS` trusts the client's uid, which is why
the export above is Wardyn-dedicated and squashed rather than a corporate home
tree. Existing corporate home directories owned by per-user uids are supported
read-only where uid 1000 can read them; where it cannot, Wardyn does **not**
refuse — the directory only has to EXIST for wardynd's own uid
(`driveShareIsBindable`), so the mount succeeds and the agent sees permission
denied at first access.

A **BYOI** image is your own to get right on this one point: a custom base that
never creates `/home/agent/drive` gets a root-owned one from the daemon at mount
time, so a drive you allocated writable is unwritable by uid 1000 on its first
run. `deploy/images/README.md`'s image contract states the one line that fixes
it; wardynd will not chown volume state to compensate.

**gVisor (CC2): if a share bind misbehaves under `runsc`, turn `directfs`
off.** Wardyn does not claim this is required — `runsc`'s own filesystem
guidance ([gvisor.dev](https://gvisor.dev/docs/user_guide/filesystem/)) is the
reference, and whether a given network-backed mount needs direct host-FD access
disabled depends on the share. If a `host_path` drive reads or writes wrongly
under CC2 and works under CC1, this is the first thing to try. It is a
**daemon** setting, not a Wardyn one — add it to the runtime in
`/etc/docker/daemon.json` and restart the daemon:

```json
{ "runtimes": { "runsc": { "path": "/usr/local/bin/runsc", "runtimeArgs": ["--directfs=false"] } } }
```

CC1 (`runc`) and CC3 (Kata) need nothing. Wardyn's own runsc tweaks are
unchanged: this is an operator recipe, and the product does not rewrite your
daemon config.

**A READ-ONLY share loses its RECURSIVE guarantee under `runsc`, and says so in
the log.** A read-only bind's `ro` reaches SUBMOUNTS only when the runtime
declares the OCI `rro` mount option — and gVisor does not (`runsc features`
lists `ro` and `rbind` and no `rro`), while the daemon **refuses the create
outright** for a runtime that does not. So Wardyn asks for it only where it is
declared (`runtimeSupportsRecursiveReadOnly`,
`internal/runner/docker/hardening.go`; `driveBindOptions`,
`internal/runner/docker/driver_mounts.go`): the bind still goes in read-only,
and a submount **under** the person's home — an autofs home, a second export
mounted below the first — can be writable inside the sandbox. wardynd WARNs on
the run it affects, with the drive and the home:

```
wardyn: user drive: this runtime does not support recursively read-only binds,
so a submount under the share's home could be writable inside the sandbox
```

Asking unconditionally is not the alternative: it made every CC2 run with a
read-only drive fail at `ContainerCreate` with the daemon's `rro is not
supported by runtime "runsc"` as the member's failure hint. There is one lever
— run the drives that need the recursive guarantee at **CC1**, where the
daemon's default `runc` declares `rro`. Nothing in the sandbox is affected when
the share carries no submounts.

**What a drive's SIZE means here.** Quoted verbatim, and the same sentence the
console renders:

> Wardyn never enforces a drive's size itself. On Kubernetes the size is the
> volume request and the storage class decides whether it binds — block disks
> do, network-share provisioners do not. On Docker a managed drive has no byte
> cap, the same gap disk_mib has. A share is bounded by its own quota. The
> size you see is the allocation, not a guarantee.

Concretely on Docker: a `docker_volume` drive reports `enforcement: none` —
`--storage-opt size` caps only a container's writable layer, never a volume, and
an XFS project quota needs `CAP_SYS_ADMIN` the control plane must not hold. A
`host_path` drive reports `enforcement: external`: the NAS's own quota binds it,
and Wardyn displays the allocation.

**A real byte cap on Docker: an XFS project quota, run by the operator, on the
host, never inside the control plane.** `CAP_SYS_ADMIN` is what WARDYN must not
hold, not a statement that nothing can enforce a `docker_volume` drive's size —
the recipe below is exactly the case `types.StorageEnforcementFilesystem` was
named and reserved for (`internal/types/user_drive.go`: "NOTHING in v1 reports
this — it is the value the documented operator recipe earns"). Wardyn still
reports `enforcement: none` on the wire; this is an operator ceiling underneath
it, invisible to the product and unaffected by a `wardynd` restart.

1. **The Docker data root must be XFS, mounted with project quotas.** Find it
   with `docker info -f '{{.DockerRootDir}}'`, then confirm with
   `xfs_info <that path>` — the output must list `pquota` or `prjquota`. A
   filesystem created without it needs a remount (`mount -o remount,prjquota
   <mountpoint>`, persisted in `/etc/fstab`) — a host operation, unrelated to
   Wardyn, that does not require restarting the daemon.

2. **Assign a project to the volume's own directory, one per drive per
   person.** Resolve the real path rather than guessing the data root, and
   resolve the XFS mount point rather than assuming it is the data root itself
   (a bind-mounted or LVM-backed data root is not always its own filesystem
   root):

   ```
   VOL=wardyn-drive-<drive-slug>-<home>                    # from the reclaim recipe above
   DIR=$(docker volume inspect -f '{{.Mountpoint}}' "$VOL")
   MOUNT=$(findmnt -T "$DIR" -no TARGET)                    # the XFS filesystem's own mount point
   PROJID=$(( 0x$(echo -n "$VOL" | sha256sum | cut -c1-7) )) # any stable project id, unique per volume
   echo "${PROJID}:${DIR}" >> /etc/projects
   echo "${VOL}:${PROJID}" >> /etc/projid
   xfs_quota -x -c "project -s ${VOL}" "$MOUNT"
   ```

3. **Set the hard limit, and confirm it actually refuses a write:**

   ```
   xfs_quota -x -c "limit -p bhard=20g ${VOL}" "$MOUNT"
   xfs_quota -x -c "report -p" "$MOUNT"
   ```

   A run whose agent then writes past the limit meets the filesystem's own
   `ENOSPC` — the identical error path a genuinely full disk already takes.
   Wardyn adds nothing to it and catches nothing from it; that is the whole
   point of a ceiling that lives below the product rather than in it.

Recreating the volume — a restore, or Wardyn re-minting one after a delete —
does not carry the quota forward: step 2 keys on the volume's directory, which
changes, so re-run it (or script it as a step your own restore/create tooling
runs after Wardyn's). A `host_path` share on an XFS-backed NAS can be capped
the identical way, against the directory the NAS exports; that quota is the
NAS's own, which is already what `enforcement: external` reports.

**And a ceiling bounds what you may ALLOCATE, not what the volume will hold.**
Two numbers can cap a drive, and they are refused and applied in different
places. `storage.user_drive.max_size_mib` on the **Workspace providers** screen
is the deployment's: a drive or an allocation override above it is refused at
the write with **422 `size_mib … exceeds this deployment's drive ceiling`** —
not a 403, because nobody was denied anything, the deployment simply will not
hold it. A governance profile's `max_drive_size_mib` is the **per-principal**
one, and it cannot be refused at a write at all: the profile binding a subject
is resolved from their claims, and a group or `all` allocation names no single
principal. It is CLAMPED when the drive is resolved (`newResolvedDrive`), folded
with the deployment's in one expression — the smaller of the two wins, the
daemon logs which one bit (`bound_by=deployment` or `bound_by=governance_profile`),
and the run, `GET /me` and `POST /drives/preview` all report the clamped number,
so the card cannot offer a size the run will not give. Lowering the deployment
ceiling after drives exist refuses no run and rewrites no row; it clamps from
the next resolve onward.

On Docker that clamp is a number and nothing more — `enforcement: none` on a
managed volume, `external` on a share — so treat the ceiling as governance over
what admins may write down, never as a cap on bytes.

**Turning drives OFF deployment-wide** is `storage.user_drive.disabled` on the
same screen, and it is a different question from a profile's `deny_user_drive`.
The switch is asked FIRST and says *this install offers no drives*: every drive
and allocation write answers **422 "drives are disabled for this deployment"**,
a run asking for its drive is refused 422 in the same family, and no
`authz.denied` row is written, because no profile denied anybody. Every drive
row and every allocation is KEPT — the screen still lists them, above a banner —
so turning it back on restores exactly what was there. **Deletes are deliberately
not refused**: `DELETE /drives/{id}` and `DELETE /drives/grants/{id}` keep
working while the switch is off, so an operator can still tidy up or offboard
somebody without turning drives back on first (the `ON DELETE RESTRICT` between
the two is unchanged, so a drive still cannot be deleted out from under an
allocation). What the switch refuses is every write that CREATES or EDITS one.
The per-profile door is unchanged and still answers 403 with its `authz.denied`
row. **All three read surfaces answer the switch identically**, off one site
(`driveSizeCeilingFor`, `internal/api/user_drives_resolve.go`) so they cannot
drift: a run asking for its drive is refused 422, `POST /drives/preview` answers
the same 422 with the same sentence, and `GET /me` reports **no** `user_drive`
with `user_drive_unavailable: "unavailable"` — never an allocation the create
path would then refuse.

### Capabilities: what one member, or one group, may do

The role split above is deployment-wide. A **capability grant** is per-human: a
row naming a *subject*, a *kind*, a *value*, and an effect of `allow` or `deny`
(`capability_grants`, migration 0042), with a per-kind **enforcement switch**
beside it (`capability_enforcement`). One sentence is the doctrine, and every rule
below follows from it: **a capability bounds what the MEMBER chose, never what the
ADMIN pre-authorized.** So a stored policy, a workspace's own requirements, the
hosts a workspace scan seeded, the model provider's own egress, and the grants
`applyWorkspaceRequirements` re-adds at launch are all left untouched no matter
what a member holds.

**The nine kinds** — a closed set, written down once in Go (`capabilityKinds`,
`internal/api/capabilities.go`) rather than as a schema CHECK:

| Kind | Value | Direction | What it bounds, and where |
|---|---|---|---|
| `egress_host` | a host, or a `*.suffix` wildcard | narrows | which host a member may **decide** an `egress_domain` approval for (`authorizeUserDecision`, `internal/api/approvals.go`), and which hosts survive on a member's own `inline_policy` allowlist (`narrowUserInlinePolicy`, `internal/api/inline_policy.go`) |
| `secret` | exact secret name | narrows | which stored secret a member's own `inline_policy` grant may reference — both refs of an `ssh_key` grant, key and `known_hosts` — and which names `GET /secrets` lists back to them (`handleListSecrets`, `internal/api/secrets.go`) |
| `workspace` | workspace uuid | narrows | which onboarded workspace a member may name on `POST /runs`/preflight (`denyUserRequest`, `internal/api/runs_create_validate.go`) |
| `image` | exact image ref | **widens** | which custom sandbox image a member may launch at all — without a grant, none (same seam) |
| `agent` | exact `--agent` string | narrows | which agent/harness a member may launch (same seam). Deliberately NOT constrained to the harness catalog, at the gate or at the grant write: `WARDYN_AGENT_IMAGES` custom agents are supported, so a catalog check would make an operator's own entry unwriteable |
| `workspace_provider` | exact git provider row id | narrows | which git provider row the repositories a member brings in may come from — the row `admitRepoURL` resolves a derived clone URL to (`internal/api/workspace_providers.go`). **Six doors**, every one a member can reach: `POST /runs` over the resolved spec's repos and over the legacy `repo` field, and `POST`/`PUT /workspaces`, `POST /workspaces/{id}/scan` and `.../build` — the last three re-point or perform a SERVER-SIDE clone. It bounds the PROVIDER, not the repository: admission is URL-prefix matching, not a repo ACL. Inert on a deployment with no provider rows, and on a repository whose host no row CLAIMS (a row that claims the host and refuses anyway — disabled, or a base path that did not match — still keys the check) |
| `model_provider` | exact model provider id | narrows | which model provider (Settings → Model providers, `SiteConfig.ModelProviders`) a person's run may use — the one the request names (`model_provider`, `wardyn run --model-provider`), the one a workspace pins (`llm_cred.provider_ref`), or the agent's default reaching them (`enforceRunModelProvider`, `internal/api/run_model_provider.go`; create and Review alike). **A workspace pin is gated too**: every model credential is the person's own, so a pin naming a provider they aren't granted refuses the run rather than being exempt. Such a person never sees that pin's id (0.8.2, #1018): a workspace read (`GET /workspaces`, `GET /workspaces/{id}`, the update response) answers `llm_cred: {"provider_unavailable": true}` in its place, and the launch refusal names no provider. Inert with no model-provider block |
| `feature` | `ssh_key` or `api_token` | narrows | whether a member may add an SSH key (`POST /me/ssh-keys`) or mint an API token (`POST /me/tokens`) at all — one check at each mint door (the token door keeps its user-view `409`; the SSH door stores a capped key, #564). Mint only: a key or token that already exists keeps working until it is removed or revoked. Any other value is refused at write time (`400`) |
| `policy` | stored policy uuid | narrows | which stored policy a member may select for their own run (`policy_id` on `POST /runs`/preflight, `denyUserRequest`, same seam). Only the choice: the selected row is still bounded by the member's ceiling, and a run that names no policy is not gated. Checked before the row is read, so an ungranted id is refused whether or not it exists |

`*` as a value matches everything of that kind, spelled the same way for all
nine. 0.8 retired a tenth, `integration`, with the AI integrations it bounded: a
run's `integration_id` is refused with a `422` for everyone, so there is nothing
left for it to gate. Its stored grant rows are inert, and a new one is refused. `egress_host` values are matched by `entryCoversAny`
(`internal/api/artifact_redirect.go`) — the *same* matcher that decides whether
one allowlist entry covers a host, deliberately not a second one, because two
host matchers that disagree is how a deny gets bypassed by a port suffix. Every
other kind is an exact compare. A **deny** on `egress_host` asks that matcher in
BOTH directions and bites whenever the two sets intersect: `deny
secret.example.com` stops a member asking for `*.example.com`, `deny *.corp`
stops one asking for `evil.corp`. An allow still has to COVER the request
outright — a half-overlapping grant permits nothing — and `*.example.com` never
covers `example.com`. Grant values are shape-checked at write time by the same
validator every `allowed_domains` ingest uses, so a value that could never match
is a `400` rather than a row that quietly does nothing.

`devcontainer_repo` is **not** a kind and stays unconditionally admin-only: it
hands attacker-authored build configuration to the image builder.

**Precedence — the order is the design** (`capAllowed`, same file):

1. **admin, admin token, and local mode are exempt.** A capability bounds a
   member; the admin tier is the one writing the grants.
2. Any matching **deny** ⇒ refused.
3. Any matching **allow** ⇒ permitted.
4. The kind is **not enforced** ⇒ permitted.
5. Otherwise ⇒ refused.

There is no user-over-group precedence: a deny anywhere wins, because "Bob's user
allow overrode the group deny" is a breach report. Deny sits *above* the
enforcement switch on purpose, which makes deny rows the adoption on-ramp:
blacklist one host for one contractor without flipping the whole deployment
fail-closed. A store error is never permission — the request answers `500`.

**The widening kind reads the same rows the other way.** For `image`
(`capGranted`) an unenforced kind is *refused*, not permitted, because 0.5 refused
it too. Both directions obey "an upgrade with no configuration changes nothing".
So `image` needs *both* the switch on and an exact-ref grant; the other five need
only the absence of a deny until you enforce them. That rule is also why `agent`
narrows rather than widens: launching an agent is something every member could
already do, so a widening kind would
refuse every member run on every deployment that has not enforced it — i.e. all of
them on upgrade day.

**Default posture: an absent enforcement row is off.** A deployment upgraded from
0.5 with no rows written behaves byte-for-byte as before. Turning `workspace` on
with no grants written locks every member out of every workspace at once; write
the grants (or the targeted denies) first, then flip the switch.

**Subjects, and the group snapshot's ceiling.** A grant's `subject_type` is `user`
(matches **either** the lowercased OIDC `sub` **or** the email — an admin writing
a grant shouldn't have to guess which the IdP made authoritative; a deny on either
identity hits), `group` (the login-time union of the ID token's `roles` and
`groups` claims, lowercased and deduped — so Entra App Roles are grantable for
free), `user_type` (everyone of one user type, named by the type's id — 0.8), or
`all` (every signed-in human).

A person holds exactly one user type, stamped at sign-in, so a `user_type` row is
one more subject in the union above: a type **allow** is one more way in, and a
type **deny** is a wall no user or group allow lifts for anyone of that type. A
security admin of that type is bound by it like anyone else; only a super admin
is exempt. The type must exist when the row is written (`400` otherwise), and a
type cannot be deleted while any grant, governance assignment or drive grant
names it. For the governance ceiling and user drives the type is a **tier**,
not a union: `user > group > user_type > all`, so a group assignment overrides
the type's profile and the type's profile overrides `all`. An API token carries
no type yet and answers as the built-in `standard` type. A session whose type
was deleted after sign-in is refused (`403`, `user_type_unknown`) wherever a
control names a type, never resolved without it.

A `group` subject must be **printable ASCII**, and the write is refused with that
reason when it is not — the same rule a console role mapping already gets. The
snapshot a group grant is matched against carries printable ASCII only, so a
subject outside that set is a row that can never match anyone: a deny that
protects nothing while the Permissions screen renders it as active. The check runs
on what you typed, *before* lowercasing, so a look-alike character that collapses
onto one of your ASCII group names (Unicode case folding maps KELVIN SIGN to `k`)
is refused rather than quietly stored as the real group. The same refusal guards a
governance assignment's subject, for the same reason.

Group membership is a **snapshot taken at login**, carried in the session cookie;
grants are read from the database per request, so a new grant takes effect on the
very next request but a *directory* change does not until the human signs in
again. The snapshot is capped at 2048 payload bytes (`maxSessionGroupsBytes`,
`internal/auth/oidc/derive.go`) so the signed cookie stays under the ~4096 bytes a
browser silently drops entirely; groups are sorted and dropped **from the end**,
so the same human loses the same groups every login instead of a coin flip. That
is roughly 100 typical group names — past that, grant the user directly, or prefer
Entra App Roles on the much smaller `roles` claim. `groups_snapshot_stale` on
`GET /me/capabilities` reports the "can't tell yet" state distinctly from "holds
no groups", because the two must not read the same — but as of 0.7 it is never a
*pre-upgrade cookie* that produces it. The session payload carries a codec
version and `decodeSession` requires an exact match
(`SessionCodecVersion = 1`, `internal/auth/oidc/session_codec.go`), so a cookie
minted before 0.7 — which has no `v` key at all — is not a stale-groups session;
it is not a session, and the human is bounced to sign in. See "Upgrades" below.

**A DENY is never allowed to evaporate with the snapshot.** A group that fell off
the 2048-byte cut — or one your directory names with a character the snapshot
cannot carry, or a pre-0.7 API token
whose completeness was never recorded — has none of its group rows in the scan. For an ALLOW that costs the caller access, which is the safe direction. For
a DENY it would hand back exactly what the row forbade, so the resolver
(`capScan`, `internal/api/capabilities.go`) checks whether **any** group-subject
deny row of that kind could cover the value, and refuses when one could — the
same scoping the ceiling refusal gets: a deployment with no group deny rows
behaves byte-for-byte as it did before. The refusal reads as an ordinary
capability denial, with a server log line naming the unanswerable snapshot;
signing in again (or re-minting the token) resolves it for good. Where a deny has
to bite with no store read at all, write it against the **user** (either
identity).

**Re-mint pre-0.7 API tokens.** A token minted before 0.7 recorded nothing about
whether its group snapshot was complete, and that unknown is read as
"incomplete" — the fail-closed choice. So every request such a token makes takes
the extra check above, on every capability it touches, for the whole life of the
token. It is correct but it is not free, and it is the one lasting cost of the
upgrade: re-minting moves those callers (CI jobs, scripts, the headless `wardyn
run` lane) back onto the ordinary indexed path and removes the standing
possibility of a group-completeness refusal they cannot themselves resolve.
`GET /api/v1/tokens` lists the deployment's tokens with their `last_used_at`, so
the dead ones can be revoked rather than re-minted.

**A third cause of a partial snapshot: the IdP's own overage.** Entra ID stops
sending the `groups` (or `roles`) claim altogether once a human is in more groups
than the token limit — **200** for a JWT, 150 for SAML — and sends a `_claim_names` /
`_claim_sources` pointer to Microsoft Graph in its place. Wardyn does not
dereference that pointer; it marks the snapshot **truncated** (`sessionGroups`,
`internal/auth/oidc/derive.go`), which reads downstream exactly like a group that
fell off the byte cap: the ceiling resolver treats it as unanswerable rather than
as "asked, there were none". Without that, such a login would arrive
complete-and-empty and quietly shed every group-tier grant and governance
assignment. Where members legitimately sit in that many groups, the answers that do not
depend on the size of the claim are Entra App Roles (the much smaller `roles`
claim) and user-subject grants.

**Every workaround that merely SHRINKS the group claim trades a detected failure
for an undetected one.** An overage is loud: the claim is absent, the snapshot is
marked truncated, and the resolver refuses rather than guessing. A FILTERED claim
is silent. Set `groupMembershipClaims: "ApplicationGroup"` — the "Groups assigned
to the application" option, which Microsoft recommends for exactly this limit —
and the token carries a smaller list that is *complete by the IdP's account*: no
`_claim_names`, no truncation bit, nothing downstream to refuse. A governance
assignment or a group DENY row keyed on a group that is no longer emitted simply
stops applying. That is the evaporation the truncation bit exists to prevent,
with the detector switched off, and **Wardyn cannot tell the two claims apart** —
a filtered claim and a full one are identical in the token.

What that option drops is **nested membership**: "nested groups are not included
and the user must be a direct member of the group assigned to the application"
([Configure group claims for
applications](https://learn.microsoft.com/en-us/entra/identity/hybrid/connect/how-to-connect-fed-group-claims)).
The same rule governs group-based **App Role** assignment — nested group
memberships are not supported for group-based assignment to an application, so a
role assigned to a group reaches its direct members only ([Manage users and
groups
assignment](https://learn.microsoft.com/en-us/entra/identity/enterprise-apps/assign-user-or-group-access-portal)).
App Roles are a smaller claim, not automatically a safer one.

**So re-key before you change the claim, not after:**

1. List what resolves by group today: `GET /permissions` for every
   `subject_type=group` grant — **deny rows first**, since those are the ones
   whose loss WIDENS somebody — and `GET /governance` for every group-tier
   assignment.
2. Re-point each one at something the new claim will still carry: a group the
   member is a **direct** member of and that is assigned to the application, or
   the member themselves (`subject_type=user`, or a user-tier assignment).
3. Then change the claim configuration.
4. Verify with a real login, not by reading the IdP's UI: have an affected member
   sign in again and read `GET /me/capabilities`, whose `session_groups` is the
   snapshot their token actually produced. Every group your re-keyed rows name
   must appear in it. `POST /governance/preview` with that exact list says which
   profile now resolves for them.

If the groups cannot be flattened and the rows cannot be re-keyed, user subjects
are the only shape in this release that a claim-configuration change cannot break
without telling you (`threatmodel/THREAT-MODEL.md` §5).

An overage also blocks one **role derivation** it must not be allowed to decide.
The role map is keyed on the very claims the IdP withheld, so an overage login
matches nothing — and "nothing matched" is then an absence of evidence, not a
fact. Falling through to `WARDYN_OIDC_DEFAULT_ROLE=admin` would hand that human
the top tier on the strength of a claim nobody read, promoting exactly the member
the hidden claim was going to wall. Such a login is **denied**
(`auth_error=claims_overage`), with a server log line naming the claim and the
var. The check is narrow, so the ordinary posture is untouched: a login whose
claims genuinely matched is served as-is (a hidden claim can only ever *narrow* a
highest-wins match), and so is a fallthrough to `user`, the narrowest tier
there is — a human in 200+ groups still signs in. Only a default WIDER than
`user` is refused. The remedy is the operator's, and retrying will not clear
it: carry the tier on Entra App Roles, map the human's email directly, or stop
defaulting unmatched humans to `admin`.

**What a capability deliberately does not reach.** `always`-scope decisions stay
on the admin-or-`security_admin` gate even for a member granted the host — a grant must never promote a
member's decision into durable workspace config. `GET /workspaces` is not
narrowed: visibility is not capability, the launch gate is what refuses. Machine
lanes (`/internal/*`, ground-truth ingest, attach tickets) are untouched. And
where the operator ceiling sets `allow_all_egress` the allowlist is not the gate
at all, so `egress_host` narrowing does nothing there — the operator's own
posture, not a switch that failed.

**What a person is offered.** The per-person lists the console's pickers read
hold only what the caller may use, decided by the same resolver the launch doors
refuse with: the `harnesses` (`agent`) of `GET /setup/status`, and the Azure
DevOps rows of
`GET /me/scm-access` and `/setup/status`'s `scm_access` (`workspace_provider`).
A refused row is dropped whole, so it reads exactly as a resource the deployment
does not have. If the grant tables cannot be read, those lists come back empty
rather than unfiltered. Admins are exempt, as at every door; a `security_admin`
is bounded like a member.

**Managing them** (the seven `/permissions` rows are `securityOps` — admin or
`security_admin`; the `/access` rows are `operatorOnly`; `GET /me/capabilities`
is member-safe):

| Route | Does |
|---|---|
| `GET /permissions` | the whole grant table plus every enforcement switch, one call |
| `POST /permissions/grants` | upsert one grant on its natural key (`201` new, `200` updated) |
| `DELETE /permissions/grants/{id}` | remove one grant |
| `PUT /permissions/enforcement` | replace the whole switch map — an omitted kind means *off* |
| `GET /permissions/availability/{kind}/{value}` | one resource's "Available to": `restricted`, and `allowed_by`, the allow rows naming it |
| `PUT /permissions/availability/{kind}/{value}` | `{"restricted": true}` turns on "Only…" for one resource, `false` turns it back to Everyone |
| `GET /permissions/explain?subject_type=&subject=&kinds=` | the Explain grid (K4): for one named `user`, `group` or `user_type` subject, every kind's state — `everyone`, `this_type` (an allow, including one written for `all`), `blocked` (a deny that covers the value), `admins_only` (the widening `image` kind, off or with no allow), or `not_available` (an enforced narrowing kind with no allow, or a restricted value no allow naming it lists this subject) — at the `*` default plus every specific value a grant names or "Available to" restricts (`restricted: true`). Each cell is the resolver's own answer, switch and restriction included, for a person who is exactly that subject: only rows naming that subject or `all` are read, so a user's group and type rows are not included. The subject is folded the way a grant's subject is, and a user type that doesn't exist is refused (`400`); `kinds` defaults to every kind |
| `GET /access` | the merged role-mapping table (chart + console rows, with collision/shadow provenance) plus the same before/after/changes posture the write guards below evaluate |
| `POST /access/mappings` | upsert one console role mapping on its natural key (`value`) — `201` new, `200` updated; refused on a chart/operator-allowlist collision, an unmatched-outcome flip without `acknowledge_access_change`, or a write that would remove the caller's own admin access |
| `DELETE /access/mappings/{id}` | remove one console role mapping — same flip/lockout guards as the write above |
| `POST /access/preview` | dry-run `roles`/`groups`/email (or the caller's own session) through the SAME derivation a real login would use — no write |
| `GET /me/capabilities` | member-safe: the caller's OWN grants, the switches, their session groups, `groups_snapshot_stale`, and `kinds_version` (a number that goes up whenever the set of capability kinds changes) |

`PUT /permissions/enforcement` replaces the **whole** map, so an omitted kind is
an enforced kind switched off: re-fetch `GET /permissions` immediately before
writing, or a stale admin tab can silently disable a control two admins both
believe is on. `GET /permissions`'s `ETag` header (a content hash of the
enforcement map alone, not the grant table) can be sent back as this `PUT`'s
`If-Match`: a document that changed underneath a stale tab is refused `412`.
`If-Match` is optional, and the write is audited either way.

**"Available to" (0.8).** `workspace`, `image`, `agent` and
`workspace_provider` values can each be restricted one at a time (migration
`0081_capability_restrictions`). A restricted value counts as enforced whatever
its kind's switch says, and only a caller holding an allow row that names the
value itself gets it: a `*` allow lists nobody, and a deny still wins. So the
"Only…" list is the allow rows for that value, written through
`POST /permissions/grants` for a person, a group or a user type. Security admins
are bound like anyone; only the admin tier is exempt. On `image`, the one
widening kind, the restriction also switches that one image on for the people
listed while the kind stays off for every other image. Turning "Only…" on with
no allow row naming the value is refused `400`, since the resource would then be
available to nobody. `egress_host` and `secret` values can't be restricted
(`400`). The value is the rest of the path, so an image ref's slashes need no
escaping.

Writes are audited as `capability.grant.create` / `.updated` / `.deleted`,
`capability.enforcement.write` and `capability.availability.write`. Enforcement lives in its own table rather than in
SiteConfig because `PUT /site-config` is a full replace: a stale client
round-tripping an older document could otherwise silently disable an authorization
control. There is **no cache** — resolution is two indexed reads per check, so a
grant applies immediately; a stale permission cache is a security bug, not a slow
page.

### Per-user API tokens: stop sharing the admin token

`WARDYN_ADMIN_TOKEN` is one string, deployment-wide admin, attributable to nobody.
A **per-user API token** is the replacement: a human mints one for their own
automation, it carries *their* identity and *their* role, and it is revocable on
its own.

| Call | Who | What |
|---|---|---|
| `POST /api/v1/me/tokens` | any signed-in human | mint one for yourself — the response is the **only** time the plaintext exists |
| `GET /api/v1/me/tokens` | any signed-in human | your own tokens, revoked ones included |
| `DELETE /api/v1/me/tokens/{id}` | any signed-in human | revoke one of your own |
| `GET /api/v1/tokens` | admin or `security_admin` | every token in the deployment |
| `DELETE /api/v1/tokens/{id}` | admin or `security_admin` | revoke anyone's |

Revoking a human (`POST /api/v1/sessions/revoke`, `wardyn session revoke`) also
revokes their API tokens, removes their registered SSH keys, and refuses their
UI-app sessions at the next re-check, whether the revoke names the subject or
the email. The `all` arm
applies all three actions deployment-wide, including the calling admin's own
credentials. Plan to re-mint tokens and register SSH keys again after a global
revoke.

To end only a person's browser sessions, send `"sessions_only": true` with
`sub`: a session-only cut is stamped and nothing else changes. Their browser
sessions end on every instance; their API tokens, SSH keys and portal-delegated
tokens keep working, and an API token can still approve a governance change:
no token-borne credential is refused by the cut; an event stream a token opened
before the cut closes at its next keepalive and reconnects. It is refused
(`sessions_revoke_param_invalid`) with `all`.
The audit row is the usual `session.revoke`, with `sessions_only` set and both
counts `0`. The People drawer's "Sign out everywhere" sends it.

**No `role` parameter on `POST /me/tokens`.** A token always mints at the
caller's own current role; there is no deliberately-downgraded mint. Still
open at 0.8.

**Deleting one API token leaves its registered SSH keys in place.** Use session
revocation to remove the person's tokens and keys together, or
`DELETE /api/v1/people/{principal}/ssh-keys` (admin or `security_admin`) to remove
only their keys and receive `{"count": N}`. A deleted key cannot authenticate
again or open a new channel on an established SSH connection. Existing channels
continue until they close or their run is torn down. Follow
[SSH access revocation](SSH.md#revoking-access-during-an-incident) for the full
offboarding sequence, including stored credentials and affected-run teardown.

**Name them by either identity.** `--sub` takes the OIDC `sub` **or** the email.
The session cutoff and token sweep match an exact subject or a case-insensitive
email. SSH-key removal resolves the stored principal, giving an exact known
subject precedence over an email alias; an ambiguous name or unresolved email cannot
be reported as completed key removal. Session revocation also stamps the resolved
subject's cutoff, because SSH keys carry a subject without an email. SSH registration
and access check the cutoff so a registration in flight cannot outlive the revoke.

A complete revoke answers `204`. A `500` may follow a successful session cutoff
if token revocation, SSH-principal resolution, canonical-subject cutoff or key
deletion then fails. Both
credential operations are attempted, and the `session.revoke` audit records
`tokens_revoked`, `ssh_keys_deleted` and outcome `failure` for partial work.
Resolve the reported failure and retry; each count describes that call only.
Sessions remain stateless signed cookies, so the audit cannot count active
browser sessions or prove that a person has no already-open SSH channels.

Use one as an ordinary bearer: `Authorization: Bearer wdn_…`. Downstream it is
indistinguishable from that human's console session — run ownership, the
admin/member gate and capability grants all resolve to the owning human — so a
member's token reaches exactly the routes their session reaches, and no more. A
token is **never** the admin identity: minting one requires a verified SSO human,
so neither the admin token nor local mode can mint one, and a token cannot mint a
second API token.

Only `hex(sha256(token))` is stored, so a lost token is re-minted, never
recovered, and a database reader (a reporting role, a hot standby, a `pg_dump` in
a backup bucket) cannot lift a usable credential off a row. `last_used_at` is best
effort and is the signal for "which of these are dead"; revoke those.

**Both halves are stamps re-checked at login.** A token carries the role AND
the group snapshot its owner held when they minted it, and every request it
authenticates republishes them, so downstream it is that human as they were at
mint time, or at their most recent sign-in since — whichever is later.

Their next successful sign-in **re-stamps the role, the group snapshot, and the
snapshot's own completeness bit** on every unrevoked token they hold — the same
`OnLogin` hook that has re-stamped their SSH keys since 0.6, now widened to
carry groups too — so a demotion, or a group membership change, reaches
outstanding tokens at that human's own next login rather than immediately. A token's stamp is
timed: `api_tokens.identity_stamped_at` is set at mint and again at every one of those re-stamps.
By default nothing ages a stamp out on its own, so a human who is demoted in the identity provider
and never signs in again keeps the role and groups their tokens were minted with. Set
`WARDYN_ROLE_STAMP_TTL` (default off) to bound that: a token whose stamp is older than the TTL is
refused with `401` and the reason `role_stamp_stale` (an `authz.denied` audit row) until its owner
signs in again, which re-stamps it with their current role. A console session older than the TTL is
sent back through sign-in, so a stamp you can tune is also the age of the longest role a session
can carry (a session already ends at the ID token's own expiry, so the TTL only matters when it is
shorter than that). Wardyn keeps no identity-provider token and cannot re-derive a role on a timer;
re-login is the only refresh. Turning the TTL on asks every token holder to sign in once, because
the backfill dated existing stamps at mint. A login never revives a revoked token, and a stamp is
one statement, so a failed re-stamp leaves that person's tokens stale, never half-updated.
What ends a token is its **expiry** (below) or **explicit revocation**; neither waits for that
next login, and neither needs the TTL.

**A token can expire.** `api_tokens.expires_at` is nullable: NULL means the
token never expires, which is every token minted before 0.8.6. `POST /me/tokens`
takes an optional `ttl_seconds`. Omitted or zero gets `WARDYN_API_TOKEN_MAX_TTL`
when you have set one (see [ENV.md](ENV.md)) and no expiry when you have not; a
value above that cap is clamped to it, and the `token.create` audit row records
the clamp (`ttl_clamped_from_seconds`); a negative value is a `400` and mints
nothing. The console's mint form sends no TTL, so on a deployment with a cap it
gets the cap. An expired token is refused as a revoked one is, the same `401`
with the same body, so expiry is no way to learn that a token once existed. Both
lists show `expires_at`. The cap applies to tokens minted after it is set; it
never shortens one already minted, so an operator who wants those gone revokes
them. There is no default cap: turning one on is what starts failing CI tokens
on its own schedule. Console sessions are unaffected and keep their own cookie
expiry.

A demotion made on the People page is now one of those explicit revocations:
when a role-mapping write or delete takes a tier away from a value, Wardyn
revokes the outstanding tokens of every principal whose own derivation that
edit demotes and whose stamp still carries what was lost, and reports the
number as `tokens_revoked` in the response and the audit row. It is scoped to
that demotion — a promotion, an unrelated value, and a member-stamped
credential naming the same group are all left alone — and a token whose group
snapshot is missing or partial cannot be re-derived, so an elevated stamp in
that state is revoked rather than assumed safe.

**A token carries its holder's user type too** (`api_tokens.user_type`,
stamped at mint and re-stamped with the role at the next sign-in), and a
type change made on the People page revokes rather than waits: when a
role-mapping write or delete changes the user type a value derives, Wardyn
revokes every live token still carrying the old type that names the value
(by principal, email or group) or whose group snapshot is missing or
partial, and counts them in the same `tokens_revoked`. On the first type
assignment to a value that derived Standard user before, that last arm is
every Standard-user token whose snapshot is missing or partial — every token
minted before 0.7 whose holder has not signed in since, and every
truncated-snapshot token — whether or not its holder has anything to do with
the value; `stale_token_snapshots` counts only the tokens that name the value,
so `tokens_revoked` can exceed it. The holder mints a new token after signing
in. A type change made in `WARDYN_OIDC_ROLE_MAP`
has no People-page edit to act on, so it reaches a token only at its
holder's next sign-in — revoke explicitly when that is too late. A user type
a live token still carries cannot be deleted (`409`, naming the count).
The type arm only compares the edited value's own before/after type against
a token's stamp, so a holder whose effective type shifts because a
different, higher-priority group is the one actually edited — or because
the edited value's own prior derivation was empty rather than `standard` —
keeps a stale stamp until that holder's next sign-in or an explicit revoke,
the same as a `WARDYN_OIDC_ROLE_MAP` edit above.

That matters most for the tier 0.7 added. A human demoted out of `security_admin`
keeps, through any token they minted while they held it, exactly what the tier
governs: profile authoring and assignment, capability-grant writes, session and
token revocation, escalated approval decisions on anyone's run, workspace
`approved-egress`/`denied-egress` writes, and audit-chain verify. What it does not
gain is anything the tier itself never had — a token reaches no shell, no attach
ticket on a foreign run, and no capability grant widens it to admin.

**So revoke it, and check that you named the right person.**

```sh
# Everything live in the deployment, with owner, name and last_used_at:
curl -H "Authorization: Bearer $TOKEN" $WARDYN/api/v1/tokens

# One token:
curl -X DELETE -H "Authorization: Bearer $TOKEN" $WARDYN/api/v1/tokens/<id>

# A whole human — sessions AND every unrevoked token they hold, in one call.
# "sub" takes EITHER identity: the OIDC subject or the email. Use the one you
# actually know; on an IdP whose sub is an opaque per-app id (Entra), that is
# the email.
curl -X POST -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' \
  -d '{"sub":"alice@corp.com"}' $WARDYN/api/v1/sessions/revoke

# Only her browser sessions; her API tokens and SSH keys keep working:
curl -X POST -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' \
  -d '{"sub":"alice@corp.com","sessions_only":true}' $WARDYN/api/v1/sessions/revoke
```

A revoked token is never re-stamped — it keeps whatever role it carried when it
was revoked, so the trail still says what that credential actually was.

The `session.revoke` audit row carries `tokens_revoked`. That count is the
receipt: a **zero** against a human you believe holds tokens means the identifier
matched nobody, not that there was nothing to revoke — sessions are stateless, so
that half cannot be counted, and only this half can tell you. Both
`token.create` and `token.revoke` are audited
([`docs/AUDIT-ACTIONS.md`](AUDIT-ACTIONS.md)); the revoke row names the token's
owner. Offboarding a person means revoking their tokens explicitly — a demoted
or departed human who never signs in again is not caught by the login-time
re-stamp, and the row outlives their access to your IdP either way. It is
published as a residual (`threatmodel/THREAT-MODEL.md` §5, "A per-user API
token's role AND group snapshot are bounded-stale, not frozen").

### Tokens for a person who never signs in

**No one can create a token that acts as another person (0.8.5).** An admin or
`security_admin` can still set a person up before their first sign-in, but
cannot mint a token for them: `POST /api/v1/people/{principal}/tokens` answers
every caller `403` with reason `person_token_mint_removed` ("No one can create
a token that acts as another person. They sign in and create their own."),
whether or not the person exists. No role, flag or environment variable turns
it back on. A token an admin created for someone else acted as that person
while the admin held its plaintext, which is the reach this closes.

For people who never open the console, the interim path is that the person
signs in once and creates their own token (`POST /api/v1/me/tokens`, or the
console; see [CI.md](CI.md)). It is never the deployment's admin token. A
trusted front-end that acts for people who ARE signed in to it uses
[delegation](#delegated-run-management-portals) instead: it never holds a
long-lived token for anyone.

| Call | What |
|---|---|
| `POST /api/v1/people` `{"principal":"<sub>","email":"<email>"}` | create the person, or confirm the one already there (`201` / `200`) |
| `POST /api/v1/people` `{"tenant_id":"<tid>","object_id":"<oid>","email":"<email>"}` | Entra ID only: the same, keyed by the tenant and object id (see below); the principal is `entra:<tid>:<oid>` |
| `POST /api/v1/people/{principal}/tokens` | refused: `403` `person_token_mint_removed`, one `person.token.create` audit row with outcome `denied` |
| `GET /api/v1/people/{principal}/tokens` | that person's tokens, revoked ones included |
| `GET /api/v1/tokens?minted_for_others=true` | every live token an admin created for someone else before this change (metadata only, never a plaintext); also on the console's Admin > Credentials page |

**Tokens already minted keep working until you revoke them.** Nothing is
revoked by the upgrade. List them with `GET /api/v1/tokens?minted_for_others=true`
(each row carries `minted_by`, the admin who created it), and revoke one with
`DELETE /api/v1/tokens/{id}`, or all of a person's with `POST /sessions/revoke`
for their `sub`. The revoke's `token.revoke` audit row carries `minted_by`. The
owner sees `minted_by` on their own `GET /me/tokens`. At the person's sign-in
the usual login re-stamp applies, with one difference: if their real role
differs from the token's stamp, a token an admin created for them is **revoked**
instead of re-stamped, otherwise whoever kept the plaintext would hold a higher
tier's credential. When the role is unchanged, the token is re-stamped with the
person's real groups at that sign-in and keeps working. Until then its groups
are unknown, so every group-tier ceiling, drive allocation or deny grant fails
closed for it, as for a truncated session.

**Keying rule: a person is their identity provider's `sub`.** A sign-in resolves
to exactly the id_token's `sub`, case-sensitive, and nothing else. So `principal`
must be that string exactly. A person's first sign-in attaches to the row by
subject equality alone and stamps `first_signed_in_at`. The email is your
assertion. Before a first sign-in it is the only input role derivation has
(an email-keyed role mapping, else the default role), and it **never attaches
anyone**. Someone else who signs in with the same email has a different subject,
lands on a different principal and reaches none of this person's runs, tokens,
secrets or drive. The real person signing in under a subject you mistyped also
attaches to nothing: revoke the orphaned tokens and create the person again.

**On Entra ID, key a person who has never signed in by object id.** Entra's
`sub` is pairwise: it is different for every app registration and unknown until
the person's first sign-in. So on a deployment whose issuer is Entra ID
(`login.microsoftonline.com`, `.us`, `login.partner.microsoftonline.cn` or
`sts.windows.net`), create them with `tenant_id` and `object_id` instead of
`principal`. Both are GUIDs; find them as described in
[deploy/azure-entra-sso/README.md](../deploy/azure-entra-sso/README.md#pre-creating-a-person-by-object-id).
Their principal is `entra:<tenant_id>:<object_id>`, which is what you pass as
`{principal}` to list their tokens. Wardyn records this deployment's
issuer with them. A sign-in becomes this person only when its issuer, `tid`
and `oid` claims all equal the recorded ones exactly, and then on every
sign-in, so re-registering the app does not orphan them. Nothing else attaches
them: not the email, not the object id under another tenant or issuer, and not
a `sub` spelling their principal. A sign-in whose `sub` starts with `entra:`,
in any case, is refused (no real Entra `sub` has a colon). Each such sign-in
writes a `person.attach` audit row naming the person and the pairwise `sub`.

Set up by object id only someone who has **never signed in**. Someone who has
already signed in is known under their pairwise `sub`, and an object-id record
never re-keys them. If you give their email, `POST /people` refuses the record
with `409`, because the email already names their subject. Without an email
Wardyn cannot tell at create time (it does not record a sign-in's `oid`), so
the check happens at sign-in instead. A sign-in that matches the record but
whose `sub` already names someone here keeps that `sub`: a person record, or an
API token, SSH key, run, workspace or stored secret they own (a secret includes
the credential a sign-in captures for them). It does not attach, and it
writes a `person.attach` row with outcome `denied` and `reason:"sub_known"`
naming both. The record then stays unused. Confirm such a person by `principal`
(the `sub` on one of their tokens or runs) instead. Setting up an Entra person by email alone
is not supported. On an Entra issuer the plain form refuses a `principal` in
the `entra:` namespace (`422`, `person_principal_reserved`), since no sign-in
can become it. On any other issuer the object-id form is refused `422`, and
sign-in keys people by `sub` exactly as before.

`POST /people` answers `409` rather than create an ambiguous identity. That
happens when the email already names another known subject, when the subject is
already known under a different email, when the subject differs from a known
one only by case, or when the subject is another person's email. It answers
`422` for the reserved subjects `admin-token`, the local-mode operator,
`local:…`, `device:…`, `delegate:…` and `subject:…`, in any case — the same set a sign-in is refused for
(see "Some subjects never sign in").

Revocation of any such token is immediate either way: `DELETE /api/v1/tokens/{id}`,
or the person's own `DELETE /api/v1/me/tokens/{id}`.

### Delegated run management (portals)

A trusted front-end — a portal — can create, list, extend, stop and open runs
for the person signed in to it, without holding that person's API token. The
portal trades the person's own live identity-provider token for a short
delegated token (RFC 8693 token exchange). The rule is **no impersonation;
delegation is recorded as delegation**: the person owns and is the actor of
everything the token does, and every audit row names the portal beside them.

**Register a portal** (super admin only). The portal must sign people in
against the same identity provider and issuer as Wardyn, with a group claim in
its tokens.

| Call | What |
|---|---|
| `POST /api/v1/admin/delegates` `{"name":"…","idp_client_id":"<the portal's client id>","group":"<group>"}` | register a portal; the `credential` (`wdp_…`) is in this response only, the row keeps its hash |
| `GET /api/v1/admin/delegates` | every portal, revoked ones included (admin or `security_admin`) |
| `DELETE /api/v1/admin/delegates/{id}` | revoke it (admin or `security_admin`) |

The portal acts only for people in its `group`, matched against the group
claim of the person's own token (canonicalized the way a sign-in snapshot is).
`idp_client_id` cannot be Wardyn's own client id.

**Exchange.** `POST /api/v1/token`, form-encoded, the portal's id and
credential as HTTP Basic:

```
grant_type=urn:ietf:params:oauth:grant-type:token-exchange
subject_token=<the person's token>
subject_token_type=urn:ietf:params:oauth:token-type:access_token   (or …:id_token, …:jwt)
```

The subject token must verify against Wardyn's issuer and key set, be
unexpired, carry an `iat`, and be either an access token for Wardyn (`aud`
holds Wardyn's client id) that the portal requested (`azp` is the portal's
client id), or a token issued to the portal itself (`aud` is exactly the
portal's client id). The person is then admitted exactly as a sign-in would
admit them — reserved subjects, the email-domain gate, role and user-type
derivation from the token's own claims. On success the answer is
`{"access_token":"wdg_…","token_type":"Bearer","expires_in":600,…}`: ten
minutes, no refresh token. A portal that needs longer exchanges the person's
live token again. `actor_token` is refused (the authenticated portal is the
actor), and a delegated token cannot itself be exchanged.

| Answer | Why |
|---|---|
| `401 invalid_client` | no, wrong or revoked portal credential (`auth.fail`, actor `wardyn/delegation`) |
| `400 invalid_grant` | the subject token did not verify, was not issued to or for the portal, or its person was refused as a sign-in would be, or their sessions were revoked after it was issued |
| `403 access_denied` | the person is not in the portal's group |

**What a delegated token can do.** Exactly: `POST /runs`, `POST
/runs/preflight`, `GET /runs`, `GET /runs/{id}`, `GET /runs/{id}/events` (the
lifecycle stream, at most 32 open per person across every portal and their own
clients), `PATCH /runs/{id}` (end and wait), `POST /runs/{id}/kill`, `POST /runs/{id}/attach-ticket` (also
`/attach/ticket`, and the UI-gateway ticket), and `GET /me`. Every other
route answers `403` with reason `delegation_scope` and an `authz.denied` row —
including secrets, API tokens, SSH keys, approving or denying the person's own
held egress, revive, and every admin route. Setting a secret and adding an SSH
key also refuse a delegated request in their own handlers, so a later change to
the allow-list cannot open them. The person is always treated at
**user** reach, whatever their own role: an admin acting through a portal
reaches only their own runs. Ownership, secrets, drives and the governance
ceiling all resolve on the person.

**What is recorded.** Each exchange writes `delegation.exchange` (actor
`delegate:<id>`, target the person). Every row a delegated request writes —
the API's, the identity provider's, the attach and UI-gateway rows of a
ticket it minted — has the person as actor and `data.via =
{"delegate":"<portal id>","grant":"<token id>"}`. A run it launches carries
`created_via` (the portal id) on the run row and in the API.

**Revocation.** Revoking the portal ends every delegated token it holds on
their next request. `POST /api/v1/sessions/revoke` for the person ends theirs
the same way and refuses new exchanges of tokens issued before it. A disable
done only at the identity provider takes effect at the next exchange, so at
most ten minutes. An open `GET /runs/{id}/events` stream re-checks its token at
each keepalive (about every 15 seconds) and ends when the portal has been revoked
or the token has expired, rather than at its five-minute hold. A UI-app session
a portal opened (through an attach ticket) is a credential derived from that
grant and is bounded by it: redemption and every 30-second re-check ask whether
the portal is registered and the grant unexpired, and the session cookie is
capped at the grant's expiry, so it lasts at most about ten minutes. Revoking the
portal ends it at the next re-check. Open WebSocket streams are the exception: a
terminal or relayed socket already established keeps working until it closes or
the run ends. Runs a portal launched keep running after it is revoked: they are
the person's runs.

### Three roles, and who sets the walls

**Super admin (the deployer).** Installs the chart, connects the IdP, and owns
everything only the chart can say: the issuer and client, the boot role map
(`WARDYN_OIDC_ROLE_MAP` — chart rows always win), the operator allowlist
(`WARDYN_OIDC_OPERATOR_EMAILS` — also the boot posture floor), the default role,
the deployment ceiling (`WARDYN_DEFAULT_POLICY`), trust roots, integrations, base
images, and the People page (who gets in, and who else is an admin). The admin
bearer token is the break-glass and remains exempt from every console lockout
guard.

**Security admin (`security_admin`).** A mapped tier — never a default, and never
derivable from the operator allowlist; you create one by mapping an IdP App Role
or group to `security_admin` in the role map (chart or People page). Security
admins read the whole workspace inventory AND any workspace in it — the list,
the row (projected), its build status (image and log blanked) and its
**observed egress** — because they decide that workspace's allowed and denied
hosts and the observed traffic is the input to that decision.
`GET /workspaces/{id}/env-as-code` is NOT in that set (owner-or-super, since
F287): its files carry the internal registry coordinate, the site-config
artifact redirects and the operator's setup commands, none of which are an
egress-decision input. They cannot otherwise WRITE a workspace: renaming,
reassigning, deleting, binding credential material and launching a recording all
stay with the super admin. Security
admins author and assign **governance profiles** (named ceilings bound to users or
groups), write the org allow/denylists (capability grants), decide escalated
approvals — egress, credential, tool — on anyone's run, revoke sessions and API
tokens, and verify the audit chain. They can also **stop** any run in the
deployment — killing a foreign run is incident response, and the most
time-critical thing this tier does — which is deliberately *not* the same as
reaching INTO one: no attach ticket, no shell, no credential material, no host.
Inspect-or-stop is the whole of that warrant. (The batch form, the sandbox
sweep, stays admin-only: it drives the container runtime across every run at
once, which is host reach rather than run reach.) They **promote** a workspace's recorded
egress into its allowlist, but they cannot **record** one: launching a recording
session opens an interactive sandbox with open egress, the workspace's directory
bind-mounted and its credentials injected, which is reach into a run, credential
material and the host — the three things this tier is defined never to have — so
`POST /workspaces/{id}/record` is admin-only and the console shows a security
admin that control disabled beside the promote control it leaves live. They also
cannot touch the People page, integrations, site-config writes, base images, or
the deploy funnel — and they run
under a governance profile themselves if one is assigned to them, since only
`admin` is exempt from ceiling resolution. A profile can only make the deployer's
stored credentials *less* available, never more — and a security admin widening
their own egress is an audited act, visible in the log they cannot rewrite. No
capability grant can widen anyone to admin; that invariant is what makes
delegating `/permissions` safe.

**A security admin's revocations reach the super admin, deliberately.** "Revoke
sessions and API tokens" above is not scoped to members: `POST
/api/v1/sessions/revoke` applies no target-role check, so a security admin may
cut a *super admin's* sessions and tokens by `sub`, and the `{"all":true}` arm
logs out **every** principal and revokes **every** live API token in the
deployment — CI and automation credentials included — in one audited call. That
is the tier working as designed. Incident response is the security admin's job,
the two tiers deliberately do not nest (a security admin still cannot reach into
a run, and their SSH key and attach ticket still stamp `user`), and a
revocation only ever *subtracts* reach — it grants the caller nothing.

What bounds it is that a revocation is not a lockout. The session cutoff is a
**timestamp**, not a flag: signing in again mints a session issued after the
cutoff, which clears it with no operator action. The **admin bearer token never
consults revocations at all**, so the break-glass above survives a
`{"all":true}` — a security admin cannot use this to lock the deployer out of
undoing it. API tokens are the one part that does not self-heal: they are
revoked permanently and must be re-minted, so treat `{"all":true}` as an
incident lever rather than a routine one. Every call is audited as
`session.revoke` with its scope and the number of tokens revoked, under the
calling security admin's own principal.

**User (member).** Signs in, runs agents inside the governance profile their group
is assigned (or the deployment ceiling if none). The profile is enforced outside
the sandbox: inline policies are clamped to it, saved policies are clamped to it
on selection (for anyone a profile is assigned to), authoring no policy at all
yields it, and its denied hosts are re-asserted when the run is dispatched — a
denied host cannot receive an injected or brokered credential at all. What a
member can change is what the profile leaves open; what they can ask for is an
escalation on the Approvals page.

**Governance profiles.** One profile per subject; when several match, the most
specific wins (user beats group beats user type beats everyone; priority breaks group ties) — the
Governance page shows the resolved answer, and `GET /policies/default` returns the
ceiling that actually binds the caller. A profile is either **standalone** or **composed**
(0.8.6). The walkthrough below covers composed profiles from authoring to rollback; the
design record is `docs/design/0.8/0.8.6-comp.md`.

*Standalone and composed.* A standalone profile replaces the deployment ceiling for its
subjects, exactly as on 0.8.5. A composed profile stores no ceiling of its own. It names a
**base**, which is another profile (`base_profile_id`) or, when that is null, the deployment
default, and an **overlay** (`overlay`, plus `overlay_limits` for the limits) that can only narrow
that base. The ceiling that binds is `ApplyOverlay(effective(base), overlay)`, computed whenever
authority is read and never stored, so a change to a base reaches every profile built on it. A
composed row's own `ceiling` and `limits` columns are `{}`, and the API adds a read-only
`effective: {ceiling, limits}` beside them.

*Authoring.* A profile names at most one base. A chain is at most three profiles deep, counting
the profile itself (a baseline, a division and a team), and a chain cannot loop; a write that would
make a cycle or push any existing descendant past three is a `409`
(`governance_profile_cycle`, `governance_profile_depth`). An overlay lists only the fields it
narrows; an absent field inherits the base unchanged, and a present empty list is a value
(`allowed_domains: []` narrows to no domains, `allowed_methods: []` is refused because it would
mean every method). The write is strict: an overlay that names something its base does not permit
(a domain the base's `allowed_domains` does not cover under the proxy's own matcher, a method the
base excludes, `allow_all_egress` on a base without it, a grant the base's grants do not dominate)
is `400 governance_overlay_invalid`, and so is an overlay whose meet with the base would be empty
rather than narrow. A `PUT` that omits `base_profile_id`, `overlay`, `overlay_limits` or
`contact` keeps the stored value, so an older client cannot flatten a profile by accident; only an
explicit `null` clears one, and `overlay: null` turns the profile back into a standalone one (the
request must then carry a valid `ceiling`).

*Resolution, and what the meet does.* Resolution reads the chain once and composes from the
deployment down. Each field has its own meet, taken after the runtime's own defaults are applied
to both sides, so a zero that means "the default" is never read as "smaller":

| `RunPolicySpec` field | Unset means | Meet |
|---|---|---|
| `allowed_domains` | nothing allowed | each overlay entry must be covered by the base's entries; the overlay's entries are the result |
| `denied_domains` | none | union |
| `allow_all_egress` | false | AND (an overlay may only set it false) |
| `first_use_approval` | `always_deny` | the stricter mode |
| `first_use_hold_seconds` | 30 | smaller |
| `max_holds` | 16 | smaller |
| `allowed_methods` | every method | intersection of non-empty sets; empty on one side takes the other; disjoint is unsatisfiable |
| `min_confinement_class` | required | the higher class |
| `eligible_grants` | none | the overlay's grants, each re-checked against the resolved base |
| `auto_stop_after_sec` | 0 or less never reaps | smaller positive |
| `workspace_mounts` | none | intersection by source and target; read-only if either side is |
| `workspace_repos` | none | intersection by identity |
| `llm_inspection` | none | the side that sets it; both set and different is unsatisfiable |
| `ui_apps` | none | intersection by name, port and path (an overlay app that serves a different path than the base's is a widening) |
| `resources` | the deployment's size | smaller, per field |
| `tool_rules` | an unnamed tool is held | per tool named on either side, and `*`: the stricter effect |
| `git_push_any_branch` | false | AND |
| `push_rules.deny_paths`, `push_rules.require_review_paths` | none | union |
| `push_rules.max_inspect_pack_mib` | 32 | smaller |
| `push_rules.hold_seconds` | 120 | smaller |
| `push_rules.max_file_size_mib` | off | smaller positive (off is unbounded) |
| `push_rules.deny_new_executables` | false | OR |
| `azure_devops_capabilities` | the provider row's default | intersection of non-empty lists; disjoint is unsatisfiable |

| `GovernanceLimits` field | Unset means | Meet |
|---|---|---|
| `deny_task_mode_exec`, `deny_interactive`, `deny_ui_apps`, `deny_user_drive` | false | OR |
| `max_concurrent_runs`, `max_ephemeral_disk_mib`, `max_drive_size_mib`, `max_cpu_millis`, `max_memory_mib` | 0 is unlimited | smaller positive |
| `autonomy_rubric` | caps nothing | per field, the lower level; a field set on one side only takes that side |
| `max_end_ahead_sec` | 0 is no limit | smaller positive |
| `default_end_sec` | 0 is `max_end_ahead_sec` | smaller, then clamped to the resulting maximum |
| `max_wait_sec` | 0 is the deployment's approval expiry | smaller |
| `default_wait_sec` | 0 is the deployment's approval expiry | smaller, then clamped to the resulting maximum |
| `allow_no_end`, `user_changes_limits` | false | AND |
| `pause_idle_after_sec` | 0 is pause only runs waiting for a decision | smaller positive |

*Worked example.* An organisation keeps three profiles. The **baseline** is a composed profile
with no base (so the deployment default is its base) and an overlay that sets `allowed_domains` to
the package registries and the forge, `first_use_approval` to `always_deny`, and `limits.max_cpu_millis` to
4000. The **division** profile names the baseline as its base and an overlay that drops the forge
host from `allowed_domains` and sets `max_concurrent_runs` to 6. The **team** profile names the
division and an overlay that sets `allowed_methods` to `GET` and `HEAD`, `denied_domains` to one
extra host, and `max_cpu_millis` to 2000. Assigned to the team's group, the team profile binds
this: the registries only, the forge dropped, `GET` and `HEAD` only, the extra host denied,
2000 millicores (the smaller of 4000 and 2000), six concurrent runs, and the baseline's
`always_deny`. Later the baseline's owner narrows `max_cpu_millis` to 1000: the next read of
the division and the team gives 1000, with no write to either. A baseline edit that would leave
a descendant empty (it narrows `allowed_methods` to `POST` while the team overlay allows only `GET` and `HEAD`) is refused with
`409 governance_overlay_unsatisfiable` naming that descendant. Widening the baseline later widens every field a descendant's overlay leaves unset, as any
base edit does. It never switches on an overlay entry the base did not permit, because an overlay
is checked against the base at write.

*A base that moves under an overlay.* A write is strict, but a base edit, or a redeploy that
narrows the deployment default, is someone else's act arriving later. At resolve the meet drops
what the base no longer covers: the run's `201` lists the drop in `clamp_warnings` (naming only the member's own profile), and an administrator sees it in that profile's `effective.warnings` on `GET /governance`. If nothing
satisfies the base and the overlay together (the deployment default narrowed until their
`allowed_methods` are disjoint, say), the launch and every live door refuse with `403
governance_overlay_unsatisfiable`, audited, until an administrator fixes it. A chain that cannot
be read, or one that loops or runs deeper than three, fails the same doors with a `500` or `503`.
Neither case is ever read as the deployment's policy. An administrator sees which profile failed on `GET /governance`
(`effective.error`).

*Every reader sees the composed answer.* Create, preflight and dispatch, and the doors that bind
runs already going (attach and SSH, UI apps, revive, the limits re-clamp, end extension, the run
policy view and the preview) all resolve through one code path, and a source guard fails if any
other code reads a raw profile row. A profile edit still reaches an already-running proxy only
through the denies re-asserted at revive or restart; a base edit now does so for a whole
subtree at once, so one edit has a larger reach and the same delay.

*What a member sees.* A member sees their own profile's name and contact, and the effective
content: `GET /policies/default`, `/me` and denial bodies serve the profile that binds them,
never the chain. A base's name, overlay and contact are not disclosed, and a profile with no
`contact` falls back to the site's `policy_help` rather than inherit a base's contact, since that
would name the base. Only an admin or a `security_admin` can read the graph (`GET /governance`). When you write
an example for a member, show the effective result and the profile's own name; do not describe
the structure behind it.

*Deleting and unassigning.* Deleting a profile requires unassigning it first, and a base that
still has profiles built on it is a `409` naming them (never a silent widening).

*Exporting a graph (CLI and SDK).* `wardyn governance get` exports a composed graph and
`wardyn governance set` applies it to another install; the Go client's `ApplyGovernance` does the
same (`docs/sdk.md`, "Composed profile graphs"). Profiles are written bases first, every graph
reference (`base_profile_id`, an assignment's `profile_id`) is remapped through the target's
names to ids, the read-only `effective` view is never written back, and prune deletes
descendants before bases. Under second-person approval (four-eyes) a write may return `202`:
a child whose base is still pending, and an assignment whose profile is pending, are
deferred and reported, not sent with a dangling id; apply again once the base is approved.

*Upgrade and rollback.* The 0.8.6 migration only adds nullable columns, so every existing
profile is standalone and resolves exactly as on 0.8.5. An older SDK or CLI sees `ceiling: {}`
on a composed profile; its re-apply compares equal and sends nothing, and a `PUT` it does send
omits the composition fields, which are kept. There is no downgrade: a 0.8.5 binary refuses a database the migration
has touched, and converting composed profiles to standalone first changes nothing it sees. Restore the
pre-upgrade dump, which holds no composed profile.

*Residual risks.*

- Running proxies keep their dispatched egress until revive or restart (above); a base edit
  makes that reach wider, not faster.
- A deployment redeploy that narrows `DefaultPolicy` can strand a subtree: its launches are
  refused with a named reason rather than widened. Look at the profile's `effective` and the
  denial stream.
- The overlay and the proxy share one domain matcher. That removes drift, and a matcher bug
  now affects both sides alike; an oracle test over a fixed host corpus bounds it.
- The comparison used to exempt narrowing edits from second-person approval is conservative,
  so some narrowing edits still need a second approver. That is the safe direction.
- Operators resolve no profile; that is a separate predicate from composition.

Stated honestly: profiles narrow by omission — a profile that omits
secret grants revokes them for its subjects (the editor warns); a member's
long-lived API token keeps the group snapshot it was minted with until re-minted.
Sandbox size is the exception: a profile that omits `resources`, or leaves one of
its fields at zero, inherits the deployment's size for that field (the default
policy's `resources`, else `WARDYN_SANDBOX_DEFAULT_CPU_MILLIS` /
`WARDYN_SANDBOX_DEFAULT_MEMORY_MIB`), so omission never grows a sandbox. A profile
that sets a size keeps it, and the profile's `limits.max_cpu_millis` and
`limits.max_memory_mib` (0 is unlimited, negatives refused) cap CPU and memory
for its assigned members at create, preflight and dispatch; operators are exempt.

**Limits that reach a running run (0.8.2, #1391, #1392).** Two limits bind past
create, keyed on the profile the run was created under as it stands now, so a
limit set later reaches runs already going (a deleted profile binds nothing).
`deny_interactive` also refuses a terminal attach (`wardyn run attach` and the
console terminal) and every SSH-gateway connection into any run under the
profile, exec runs and the run's owner included (a super admin enters only a
run of their own or one with no personal owner, so the exemption below reaches no
person's run); the attach is an `authz.denied`
row (`governance_profile`, target `runs.attach`), the SSH refusal an
`ssh.authenticate` failure naming the profile. The harness sign-in run is exempt,
as it is at create. `deny_ui_apps` strips `ui_apps` from a run at create, with a
`clamp_warnings` sentence and a `dropped` row at target `runs.ui_apps`, and the UI
gateway refuses a session into a run created under the profile. An empty ceiling
`ui_apps` is still no opinion, so a profile without the limit behaves as before.
A super admin is exempt from both at every door, as at create; a security admin
is bound. A limit set later reaches new sessions but does not sever ones already
open: a terminal, SSH session or UI-app session opened before the limit was set
runs until it ends. `deny_ui_apps` does not close an SSH port forward to the app;
`deny_interactive` does.

**The autonomy rubric (0.8, #77).** A profile may also carry `limits.autonomy_rubric`,
nine closed fields — three egress postures (`egress_open`, `egress_reviewed`,
`egress_sealed`), three secret postures (`secrets_powerful`, `secrets_baseline`,
`secrets_none`) and three enforced confinement classes (`confinement_cc1`,
`confinement_cc2`, `confinement_cc3`) — each unset or one of four autonomy levels:
`L0` attended (interactive only, supervised seeding), `L1` gated (adds
non-interactive runs, but `tool_approvals` is derived to `hold`), `L2` unattended
(adds `auto` approval and `seed_auto_tools`), and `L3` (adds `task_mode=exec`, the
door that routes around every other gate, so it is the top rung). Below `L3`, an
interactive run with a task must use `interactive_start=agent`: the shell startup
form (`interactive_start` unset or `shell`) runs the task at sandbox boot the way
exec does, and is refused (`runs.interactive_start`). `resolveRunAutonomy`
(`internal/api/runs_autonomy.go`) grades the run's real posture — egress reach
graded on the same union `unionRunEgress` builds, secret power, and the
already-enforced confinement class — against the assigned profile's rubric and
folds every field the posture matches to its **minimum** level; a nil rubric, or a
posture none of the nine fields caps, binds nothing (today's behaviour, unchanged).
The same function backs both `POST /runs` and `POST /runs/preflight`, so the level
Review shows is the level launch enforces. A run whose declared shape exceeds its
resolved level is refused `governance_profile` (see
[§ Every denial that isn't a 404](#every-denial-that-isnt-a-404) for its `target`s). A
non-interactive run resolved to exactly `L1` is not refused when its agent has a
tool-approval lane (claude-code): it launches with its tool approvals derived to
`hold`, and the 201 carries a warning saying so. Any other agent — codex-cli, a
BYOA image — has no lane to derive a hold into, so the same run is refused with
target `runs.agent`.
The resolution — level, posture, and every rubric field that tied at that level
(`bound_by`) — rides the create audit row's `autonomy` field and is frozen on
`agent_runs.autonomy_level`. **Not in the posture:** the model-provider hosts
egress dispatch resolves from global configuration after this gate runs (a Bedrock
run's region, for one), and any stored-credential residency — a run's autonomy
level is graded on what the run can reach and hold, not on where its model
credential lives.

### When everyone is an admin, and what a refused person is told

**The everyone-is-an-admin warning.** With SSO configured, the setup checklist's
"Who is an admin" row grades `warn` — holding the console in the People step and
showing every admin a banner above every page — on either of two conditions
(#491):

- **No role map and no admin list.** A person nobody has mapped derives `admin`
  when there is **neither** a role map (the chart's `WARDYN_OIDC_ROLE_MAP` or a
  People-step row) **nor** an admin list (the operator allowlist,
  `WARDYN_OIDC_OPERATOR_EMAILS`). An admin list alone is enough to clear this:
  an unmatched person then derives `member`.
- **A role map IS set (chart or People step), but `WARDYN_OIDC_DEFAULT_ROLE=admin`.**
  Every sign-in the map doesn't match still falls through to `admin` — before
  #491 this read `ok`, since a role map being set was all the check looked for.
  Fix by setting `WARDYN_OIDC_DEFAULT_ROLE` to `user` or a user type instead.
  An admin list alone does not trip this: with no role map, a sign-in the
  (empty) map doesn't match derives `member` regardless of the default role.

A deployment that hits BOTH conditions (no role map, no admin list, AND
`WARDYN_OIDC_DEFAULT_ROLE=admin`) reads the first condition's own sentence —
one banner, not two competing ones. Members see neither.

**Request-access help (`sign_in_help_text`, `sign_in_help_url`).** Two optional
SiteConfig fields, edited on the People step ("When someone can't sign in") or
through `PUT /site-config`. The sign-in page shows them under Wardyn's own
sentence — never instead of it — on the four refusals a person cannot clear
alone: no role, an email domain that isn't allowed, too many groups to list, and
a missing `email_verified` claim. Timeouts and configuration errors get nothing.
**Both are public by design:** the anonymous `/healthz` publishes them, because
the reader has, by definition, not signed in — so name your request process, not
your internal systems. The text is plain text (at most 1,000 characters; no
line breaks, control characters, line/paragraph separators or invisible format
characters such as bidi overrides and zero-width spaces; quotes are fine) and is
rendered as text, never markup. The link must be an `https://` address with a
real host name — no spaces, no `user:pass@`, none of those hidden characters, a
query string is fine — and always reads "Request access". A link saved as
`http://` before 0.8 keeps working and is published unchanged, but the setup
checklist warns about it (row `sign_in_help_url`) until you change it; a save
that sends it back unchanged is accepted, and a new `http://` link is refused.
That includes an MDM or CLI baseline (`wardyn site-config set`) whose `http://`
link differs from the stored one: the whole re-apply is refused with a 400 on
every boot (`wardyn-desktop.sh` logs "site-config set failed") until the
baseline file names an `https://` link.
Every write records both values in the clear on `site_config.write`. A write outside those bounds is refused with a 400 naming the
field, and a stored value that no longer passes is dropped from `/healthz`
rather than published. Like the provider blocks, a body that does not name a
field carries the stored value forward; name it as `""` to clear it.

### UI apps with more than one user: use host mode

If the UI-sandbox gateway is on and more than one person uses this install, set
`WARDYN_UI_SANDBOX_ORIGIN_TEMPLATE` (`uiSandbox.originTemplate` in the chart),
e.g. `https://run-{run}.ui.example.com`, with wildcard DNS and a wildcard
certificate. Without it the gateway runs in path mode: every run's relayed app
is served from ONE browser origin, separated only by a path-scoped cookie. That
cookie decides which session a request carries, but not what a page may read:
any relayed page on that origin can script any other page there that is open
in the same browser. That is the shared-origin residual
([THREAT-MODEL.md](../threatmodel/THREAT-MODEL.md) §5 #18).

Two controls keep another user's app out of that origin in your browser: the
enter ticket is bound to the browser that minted it, so nobody can push you
into their app with a link or a form, and a relayed app cannot register a
service worker outside its own path
([UI-SANDBOXES.md §3](UI-SANDBOXES.md#3-open-an-app) and
[Bounds](UI-SANDBOXES.md#bounds)). They leave one boundary to the path cookie alone: your
own apps can still reach each other. Host mode gives every run its own origin,
which the browser itself isolates, and it refuses an enter served on any other
run's host. Path mode is for a single-user or demo install.

Either mode needs the console and the gateway on **the same site** (one
registrable domain, one scheme): the enter binding is a cookie the console's
fetch sets on the gateway, and a browser refuses that across sites. Open then
fails with an error that says so.

### Every denial that isn't a 404

(This section is the source of record for `authz.denied`'s `reason` values;
[`docs/AUDIT-ACTIONS.md`](AUDIT-ACTIONS.md) is the vocabulary reference for every
*other* audit `action` and points back here for this one.)

Every member denial that isn't a plain foreign-resource 404 is audited under
`authz.denied`, whose `reason` field is the whole vocabulary.

Three of the reasons below are NOT member denials at all: 0.7.4 added a RUN-TOKEN
tier (`run_terminal`, `run_not_found`; 0.8 adds `run_kept`), raised by `internalAuth`'s liveness
gate against a sandbox sidecar's own run token rather than against a person. They
live in this table because the action, the shape and the `reason` field are the
same one an operator greps; the `actor_type` (`agent`) is what tells them apart.

One FIELD rides beside the reason since 0.7.4: `user_view: true` (renamed in 0.8
from `member_mode` — see [Renamed in 0.8](#renamed-in-08); pre-0.8 rows keep
`member_mode`), on every ADMIN-TIER `403` below — the two `requireOperator` /
`requireSecurityOperator` chokepoints and the in-handler refusals that raise the
same two reasons — when the refused caller is an admin exercising
[the User view](operations/member-mode.md). It is a marker, not a
reason — the `reason`, the status code and the body are unchanged, and the key
is absent entirely for an ordinary member. A burst of denials carrying it is an
admin walking the member path, not an incident.

**A ceiling refusal names the policy that caused it.** A refusal made by the resolved
ceiling (`governance_profile` and `run_quota`, plus the Record Mode and provider sign-in
launches that answer `record_ceiling_limit`) carries a `policy` object beside `error` and
`reason`: `source` (`profile` or `deployment`), the leaf profile's `name`, and the contact
it published (`owner`, `email`, `request_url`, `request_text`, each present only when set
and still valid). A member bound by the deployment is named as `deployment` with the
site config's `policy_help`. An operator is never refused by a ceiling, so never gets one.
The `error` text and the status are unchanged. The key is never on a hidden door (the
`404` twin of a missing resource) and never on a decision whose wire reason was rewritten,
so those stay byte-identical to a missing resource. An `authz.denied` row never carries
the owner or email, only the `reason` and `target` it always had.

- `GET /me` carries `governance_contact`: the caller's own `policy` object, or `null` for
  an operator, when the ceiling cannot be resolved, and on a stale group snapshot. It
  never fails the read, and for a member bound by the deployment it is `{"source":
  "deployment"}` without a site-config read.
- `GET /runs/{id}` carries `policy`: the profile the run was launched under, else the
  deployment's `policy_help`, else the key is absent. A store failure omits it.
- `wardyn` prints one `governed by …, to request a change: …` line after the error when the
  refusal carries a `policy`. Exit codes are unchanged, and the Go SDK exposes it as
  `APIError.Policy`.

| `reason` | Raised when | Shape |
|---|---|---|
| `admin_surface` | a member requested an admin-only route (`requireOperator`) | ⛔ `403` |
| `admin_view` | an admin in the user view launched a run (`POST /runs` or `POST /runs/preflight`) after the type the view looks through was deleted. Not audited on its own — the cause row is `user_view_type_deleted`, which the launch response answered; see that row for the shape and the marker | ⛔ `409` |
| `security_admin_surface` | a member requested a route on the SECURITY tier (`requireSecurityOperator` — admin or `security_admin`), and also raised in-handler by `resolveAlwaysTarget` for `decision_scope=always` on a route that lives on the member group — the same predicate on a route a member may legally reach. The `403` body is byte-identical to `admin_surface`'s on purpose, so a refusal never maps which tier a route sits on; only this reason distinguishes them, which is what lets a rule tell "a member hit an admin route" from "a member hit a security-tier route" | ⛔ `403` |
| `not_owner` | a member reached a run/approval/recording, or a member-OWNED workspace (`owned_by`, migration 0048), that exists but isn't theirs | ⛔ `404` (byte-identical to missing) |
| `attach_ticket_foreign_run` | a caller who is not the run's owner — **including a `security_admin`** — asked to mint a PTY attach ticket for a run they did not create. Its own reason rather than `not_owner` so an auditor can see the security tier refused a foreign shell without inferring it from the path (`internal/api/attach_ticket.go`) | ⛔ `404` (byte-identical to missing) |
| `run_owner_only` | 0.8.5 (#1476): a **super admin** asked for interactive entry (attach-ticket mint or consume, the cookie attach lane, a UI app, take-over) to a run that is not theirs and has a personal owner. A `403` with the body `{"error":"only the person who started this run can open it interactively","reason":"run_owner_only"}`, not the `404` above, because the admin can already see the run. A run with no personal owner (operator-owned service or local runs) stays enterable. Kill, approve, policy, grants, revoke, audit, revive, resume and end are unchanged | ⛔ `403` |
| `recording_governed` | 0.8.6: with `WARDYN_GOVERN_ADMIN_RUNS` on, an admin whose runs are governed (an SSO admin, an admin-role personal token) asked for Record Mode (`POST /workspaces/{id}/record`). Refused first, before the ceiling read and the import-step claim, at target `workspaces.record`; the body says Record Mode is refused for admins whose runs are governed and that the operator can allow it by setting `WARDYN_GOVERN_ADMIN_RUNS_EXEMPT` to `recording`. That setting lifts it for that lane, and each recording is then marked `governance_exempt` on `run.record.start`. The admin token and local mode are never refused. See [Constrained-admin mode](operations/member-mode.md#constrained-admin-mode) | ⛔ `403` |
| `key_domain_unknown` | 0.8.6: `PUT /key-domains/assignments/{subject_type}/{subject}` named a key domain the deployment's key domains file (`WARDYN_KEY_DOMAINS_FILE`, chart `kek.domains`) does not declare. The body names the declared domains; nothing was written. Domains come from the file alone, so the API can only choose among them | ⛔ `422` |
| `key_domain_ambiguous_membership` | 0.8.6: a `PUT` of a group key-domain assignment would leave people who last signed in with that group and another group assigned to a different domain, and who have no assignment of their own, so their next principal key would be refused by name. The body counts them; nothing was written. Assign each of them to one domain as a user first, or give both groups the same domain. The same reason refuses any group write while someone whose last sign-in lost groups (a Microsoft Entra group overage always does) has no user assignment, since once any group is assigned that person is refused by name and signing in again does not clear it; the body names the first of them. A person whose identity is deactivated or purged is never counted, and the same reason refuses a `DELETE` of a user assignment for a person whose last sign-in lost groups while any group is assigned | ⛔ `409` |
| `byoi_user` | a member named a `devcontainer_repo`, or an `image` they hold no grant for | ⛔ `403` |
| `capability_workspace` | `workspace_id`: a member named a workspace they aren't granted (`403`). Launching: an `inline_policy` `workspace_repos` entry for an ungranted workspace was dropped — the run still launches | ⛔ `403`, or 🟡 a drop |
| `capability_egress_host` | deciding: the approval's host isn't granted (`403`). Launching: member-authored allowlist entries were dropped from an `inline_policy` — the run still launches | ⛔ `403`, or 🟡 a drop |
| `capability_secret` | a member's `inline_policy` grant referenced a secret they aren't granted — dropped, not rejected | 🟡 drop |
| `capability_agent` | `agent`: a member named an agent they aren't granted (`denyUserRequest`, `internal/api/runs_create_validate.go`) | ⛔ `403` |
| `capability_workspace_provider` | a member's work would come from a git provider row they aren't granted — the row `admitRepoURL` resolves the repository's derived clone URL to (`internal/api/workspace_providers.go`). Six doors: `POST /runs` over the resolved spec's repos and over the legacy `repo` field (target `runs.workspace_provider`), and `POST /workspaces`, `PUT /workspaces/{id}`, `POST /workspaces/{id}/scan` and `POST /workspaces/{id}/build` (target `workspaces.source_provider`). The body names the provider KIND and nothing else — never a base URL, never the row id, because `GET /workspace-providers` is a security-tier door for exactly that reason. Silent on a deployment with no provider rows, and on a repository whose host no row CLAIMS (including one still admitted through the legacy `scm_hosts` list): there is no row for a grant to name | ⛔ `403` |
| `capability_model_provider` | a member's run would use a model provider they aren't granted — the one they named (`model_provider`), the one the workspace pins, or, when no single granted provider is left, the ones serving the agent (`enforceRunModelProvider`, `internal/api/run_model_provider.go`; target `runs.model_provider`), and on revive/restart/extend as the owner (the run's recorded provider, `internal/api/run_owner_authority.go`). Review answers the same refusal as create. Since 0.8.2 (#1018) a provider the member NAMED in the request is answered exactly as an id no provider has — the `422` `model_provider_unavailable` "there is no model provider by that name" sentence, whatever the provider's state — because provider ids are guessable; only this row records the true reason. A provider the workspace's pin names, which a workspace read hides from them, is refused `403` with the sentence that names no provider, and the row carries `provider`. The key door (`PUT` and `DELETE /model-providers/{id}/credential`, target `model_provider.credential`; a `DELETE` by a person who still holds a key for the provider is not refused) and the sign-in door (`/model-providers/{id}/sign-in`, target `model_provider.sign_in`) likewise answer the `404` an unknown id gets (`denyProviderAsMissing`, `internal/api/model_provider_credentials.go`). Only the unnamed case — the one provider serving the agent, or none granted — answers `403`, with the one sentence that names no provider (the row carries `provider` when exactly one serves) | ⛔ `403` when no provider was named; otherwise byte-identical to an unknown id (`422` at create and Review, `404` at the key and sign-in doors) |
| `capability_feature` | a member tried to add an SSH key (target `me.ssh_keys`) or mint an API token (target `me.tokens`) and that feature is not available to them. Checked before the key or token is validated or stored | ⛔ `403` |
| `capability_policy` | `policy_id`: a member selected a stored policy they aren't granted (`denyUserRequest`, target `runs.policy`, on `POST /runs` and preflight alike), and on revive/restart/extend as the owner (`internal/api/run_owner_authority.go`) | ⛔ `403` |
| `governance_profile` | the member's assigned governance profile refuses this run SHAPE. One cause per emitted `target`: `task_mode=exec` below autonomy level L3 (`runs.task_mode`), a non-interactive run below autonomy level L1 (`runs.interactive`), `seed_auto_tools` below autonomy level L2 (`runs.seed_auto_tools`), an agent with no tool-approval lane — BYOA (`agent` unset) or any agent other than `claude-code` — at a resolved level of exactly L1, where an unattended run's tool calls would otherwise be derived to `hold` (`runs.agent`), — 0.7 — `drive.enabled` under a profile carrying `DenyUserDrive` (`runs.drive`, `denyUserDrive`), and — 0.8 — an interactive run's shell startup command (a task with `interactive_start` unset or `shell`) below autonomy level L3 (`runs.interactive_start`, `resolveRunAutonomy`) or under a profile carrying `deny_task_mode_exec` (`runs.interactive_start`, `denyUserGovernance`), since it runs at sandbox boot unattended the way exec does, and — 0.8.2 — a terminal attach into a run whose profile carries `deny_interactive` (`runs.attach`) or a UI-gateway session into one whose profile carries `deny_ui_apps` (`runs.ui_apps`, which is also the target of the `dropped` row when that limit strips `ui_apps` at create; `internal/api/governance_run_doors.go`). A profile refuses the shape, never the person: the same member launches fine without the refused field | ⛔ `403` |
| `governance_overlay_unsatisfiable` | the governance profile that binds this person, or the run's own, is composed (0.8.6) and nothing satisfies it together with the profile or deployment default it builds on — the deployment default narrowed until an overlay's `allowed_methods` are disjoint with it, or an overlay and base that name different `llm_inspection` modes — so the launch (`governance.ceiling`, on create and preflight alike) and every live door refuse rather than guess: a terminal attach (`runs.attach`), a UI-gateway session (`runs.ui_apps`), a revive and an end extension (the `owner_profile_*` refusals' `403` sibling). A base or a chain that cannot be read is a `500` or `503`, never this reason, and is never read as the deployment's policy. The sentence names the person's own profile and never a base; an administrator sees which profile failed on `GET /governance` (`effective.error`). The SAME value is the `409` a profile write returns when a base change would leave a profile built on it in this state | ⛔ `403` |
| `grant_pairing_not_eligible` | a member's `inline_policy` paired a stored secret with a host the operator never eligible-listed (`filterUserGrants`) — dropped. Also covers the `env_secret` **admin-only** drop (`dropAdminOnlyEnvSecretGrants`), which fires for every non-operator on every route a run policy arrives by — inline body, selected stored row, or the deployment default — whatever the caller's governance assignment, since that rule is a role check plus `WARDYN_ALLOW_USER_ENV_SECRET` rather than a ceiling check | 🟡 drop |
| `groups_snapshot_stale` | the resolver cannot answer this caller's group tier — their login-time group snapshot is missing or was truncated at sign-in, and the deployment assigns governance profiles by group — so every ceiling-bounded seam refuses. Decided by one rule, `selectByTier` (`internal/api/select_by_tier.go`), and emitted ONCE per request at each of its two entrances: `ceilingWithUnusableGroups` (`internal/api/governance.go`) at target `governance.ceiling`, and `driveWithUnusableGroups` (`internal/api/user_drives_resolve.go`) at target `runs.drive`. The ceiling is memoized per request and the drive resolver is asked once, so the count still means denials rather than resolves. A deployment that assigns governance profiles by group emits the first; one that allocates user drives by group emits the second; one that does both emits both, for the same member, because they are two separate refusals the member meets at two separate doors. The remedy is the caller's own and is in the refusal body — sign in again, or re-mint the API token | ⛔ `403` |
| `second_human_required` | `WARDYN_EGRESS_SECOND_HUMAN` is set and the caller deciding an `egress_domain` approval, or `WARDYN_CAPABILITY_SECOND_HUMAN` is set and the caller deciding an Azure DevOps capability escalation, is the run's own `created_by` (`requireSecondHuman`) — a different human must decide it. Also `WARDYN_GOVERNANCE_SECOND_HUMAN` (target `governance.change`, a new target and not a new reason): the approver of a held governance change is its proposer, by principal or by case-folded email | ⛔ `403` |
| `model_provider_unavailable` | #987: at create and Review alike, and at the admin record door (`POST /workspaces/{id}/record`, the same writer) (`enforceRunModelProvider`, `internal/api/run_model_provider.go`; target `runs.model_provider`), the run's model provider cannot credential it: no provider by that name, it is off, it does not serve the agent, several serve it and none is chosen or the default, the caller has no usable credential of their own for it (`remedy` `model_credential`, the one case a sign-in or a stored key repairs), or a policy grant would set a model-credential variable beside it. The row carries `provider` and `kind` when the refusal names one; the 422 body keeps its `provider`, `kind` and `reason` fields. A provider the member is not granted is `capability_model_provider` instead, one row, never both | ⛔ `422` |
| `run_terminal` | 0.7.4: a RUN TOKEN, not a member — the run whose token authenticated an `/internal/*` call has gone terminal (`internalAuth`'s liveness gate). Token verification cannot catch this: the revoke cascade is best-effort, so a killed run whose revocation write failed still presents a token that verifies. `actor_type` is `agent`, the target is the request path, and the terminal state the run was found in rides beside the reason as its own `run_state` datum — the reason itself stays a closed value, because that is what a SIEM rule is written against. The three tail-upload doors — `/internal/recordings/`, `/internal/scan-results/`, `/internal/sso-token/` — are exempt for five minutes after the run went terminal, because those uploads race the watcher that ends it | ⛔ `403` |
| `run_not_found` | 0.7.4: the same gate, when the run the token names has no row at all | ⛔ `403` |
| `run_kept` | 0.8 (#1176): the same gate, when the run the token names is still `RUNNING` but kept — ended by its lease, or lost to a reboot or an outage. Its proxy is stopped on purpose and its identity is not revoked (a revive mints a fresh token under it), so the token the stopped proxy still holds would otherwise verify until it lapses. The kept reason rides beside the reason as `lost_reason`. A kept run later killed or torn down is refused as `run_terminal` instead. The three tail-upload doors are exempt for five minutes after the run was kept. Token renew refuses the same runs on its own path (`identity.renew`, `run_lost:<lost_reason>`) | ⛔ `403` |
| `user_type_unknown` | 0.8: the user type stamped on the caller's session no longer exists (it was deleted after they signed in). Every control that names a type refuses rather than resolving without it — the capability resolvers, the governance ceiling and the drive resolver — at target `user_type`, with the missing id as the `user_type` datum. Written once per request, however many of those controls refuse it, and not for a display read (`GET /me`). The body is the sentence `Your user type no longer exists…`, whose remedy is an admin's (give the person another type) and then the person's (sign in again) | ⛔ `403` |
| `delegation_scope` | 0.8 (#1142): a portal's delegated token asked for a route outside the delegation allow-list ([Delegated run management](#delegated-run-management-portals)), or reached `PUT /secrets/{name}` or `POST /me/ssh-keys`, which refuse a delegated request themselves whatever the allow-list says (0.8.2, #1234). The row's actor is the person and its `data.via` names the portal | ⛔ `403` |
| `role_stamp_stale` | 0.8.6: `WARDYN_ROLE_STAMP_TTL` is set and the `wdn_` API token presented carries a role and group stamp (`api_tokens.identity_stamped_at`) older than it, or never stamped. Checked by `apiTokenAuth` after the token resolves and before it counts as used; target `api_token`, and the row's actor is the token's owner. The body is `this token's role is out of date: its owner must sign in again to refresh it`; the owner's next sign-in re-stamps the token and it works again. A revoked token is not this refusal: it stays an ordinary `401` | ⛔ `401` |
| `event_stream_cap` | 0.8.2 (#1407): the caller already holds 32 open `GET /runs/{id}/events` streams, the most one principal may (`maxRunEventStreams`, `internal/api/run_events.go`; target the run id). A portal's streams count against its person, and every admin-token caller is one principal. Not audited — a caller who IS authorized and hit a limit, like `run_quota` | ⛔ `422` |
| `mask_state_unavailable` | 0.8.6: a door that relays or persists a run's output — the recording upload (`PUT /internal/recordings/{runID}` and its parts, target `recordings.upload`), the live attach (`GET /runs/{id}/attach`, target `runs.attach`), the SSH shell (target `ssh.shell`, a channel error, not an HTTP status) and the live output read (`GET /runs/{id}/output`, target `runs.output`) — cannot prove the run's masking corpus complete on this server, so it refuses instead of passing bytes through. Since the shared masking registry the same reason also answers an injection or capture route (targets `injection.resolve` and `credential.capture`) whose value could not be committed to the masking registry: the value is not handed out. The run has no complete, unfenced masking manifest in Postgres (`run_mask_manifest`): it was dispatched before 0.8.6, its dispatch never finished committing it, its person is being erased, or Postgres did not answer. The exec relay (`task_mode=exec` output tail) refuses by keeping nothing. The row's `data.mask_scope` is `globals_only`. An attach, shell or upload already in flight ends at the next beat (about two seconds) when the run stops being covered, an attach with close status `1013`. Not hidden: the caller can already see the run | ⛔ `503` |
| `audit_export_partition_filter` | 0.8.6: `GET /audit/export?partition=` carried another filter (`run_id`, `since`, `until`, `action`, `action_prefix`, `actor`, `actor_type`, `outcome` or `origin`). A partition export always covers the whole partition, so its footer digest can be checked against `audit_partition_digest`; remove the other parameters. Input shape rather than a denial, so it is not audited | ⛔ `400` |
| `audit_retention_not_oldest` | 0.8.6: `POST /audit/retention/drop` named a partition that is not the oldest retained one. Only the oldest partition can be dropped, so a drop never removes an interior link of the chain. Target `audit.retention`, `partition` beside it | ⛔ `409` |
| `audit_retention_not_closed` | 0.8.6: the same door, when the oldest partition can still receive rows (the high-water mark has not reached its upper bound) | ⛔ `409` |
| `audit_retention_inside_window` | 0.8.6: the same door, when the partition ended less than the effective retention window ago, or retention is set to forever (`WARDYN_AUDIT_RETENTION_DAYS` is `0`). A decrease is not effective until 30 days after the boot that first saw it | ⛔ `409` |
| `audit_retention_live_run` | 0.8.6: the same door, when the partition holds audit rows of a run that is still live (`PENDING`, `STARTING`, `RUNNING` or `WAITING_FOR_CONFIRMATION`). Drop it once the run has ended | ⛔ `409` |
| `audit_retention_digest_mismatch` | 0.8.6: the same door, when the digest supplied is not the one the database computes for the partition. Export the partition again, check the archive, and submit the digest from its footer | ⛔ `409` |
| `user_view_type_deleted` | 0.8: an admin in the user view made a request after the user type the view looks through was deleted. The request is refused — never answered as the admin, because its tier was already read as `user` — and the session's view is turned off on the cookie, so the next request is in the Admin view. The body is `The <type> user type was removed, so you're back in the Admin view…`; `POST /runs` and `POST /runs/preflight` answer `409` with `reason` `admin_view` instead. The row carries `user_view: true` and the deleted `user_type`. `GET /me` is never refused: it drops back and says so (`user_view_dropped`) | ⛔ `403` |
| `user_view_preview` | 0.8.6, with `WARDYN_GOVERN_ADMIN_RUNS` on: an admin or security admin whose User view looks through a user type other than their own stamped type sent a request that is not a `GET`, `HEAD` or `OPTIONS` (`POST /runs`, `POST /runs/preflight`, a workspace create, any write). The view is a read-only preview. The body names the remedy: switch the view to your own type to make changes or launch. `POST /me/view`, `POST /auth/logout` and `POST /policies/grade` are still served. The row carries `user_view: true`, `viewed_user_type` and `stamped_user_type` | ⛔ `409` |

The drop rows are why `POST /runs` mostly *narrows* rather than refuses: a member
whose whole allowlist is ungranted gets a run with no member-authored egress, not
a `403`, because the run's admin-authored egress is still there. A drop is never
silent: it comes back as a **warning on the launch response itself** (a console
toast, `wardyn run`'s stderr), appears the same way in a preflight/Review dry-run
*before* launch, and is recorded as an audit event at launch — one event per
reason with the affected values beside it, not one per dropped host. A preflight
dry-run writes no **drop** rows — a drop is not a denial, and it is recorded at
launch. A dry run that is **refused** does audit, though: every gate preflight
reproduces is the real gate, so a refused door writes its own `authz.denied` row
from inside the shared path (`refuse`, `internal/api/refusal.go`), with **`run_id` NULL**,
because there is no run. A dry run that passes writes nothing at all. That is
deliberate rather than suppressed: the row records that this principal was
refused this capability, which is true whether or not they went on to launch, and
a gate that audits at one door and not at the identical door one handler over is
the drift the shared path exists to prevent.

`run_id` NULL does not mark a dry run, because a launch refused before its run
row exists carries it too. Every row written while serving the preflight request
carries **`dry_run: true`** instead, stamped from the request context by
`audit.DryRunRecorder` (`internal/audit/dryrun.go`) so no door's detail can set or
clear it; a launch row never carries it. Review re-resolves on every edit, so a
member editing against a closed door would otherwise write a row per keystroke.
`audit.DenialCoalescer` (`internal/audit/coalesce.go`) writes the first refusal
for an actor, target and `reason` in full and counts identical repeats for ten
minutes, then appends one **`preflight.denial.coalesce`** row (`count` including
the first row, `suppressed`, `first_at`, `last_at`). Nothing already written is
changed, a window with no repeat writes no summary, and a launch refusal is never
coalesced. The windows live in the replica that served the request, so a
multi-replica deployment writes one summary per replica, and open windows are
flushed on graceful shutdown. Rows written before 0.8.6 carry no marker
(`handlePreflightRun`, `internal/api/preflight.go`).

A 404 on a resource that genuinely doesn't exist stays silent by design. One
exception: the `always`-scope 403 above returns before `decide()` reaches any
audit call, so it is a bare 403 with no audit trail at all.

**What's still not built.** No custom roles: the tier set is the three fixed ones
(admin, `security_admin`, member — see "Three roles, and who sets the walls"), and
a capability grant only narrows or widens what a member may reach, it can never
mint a tier. Only the nine kinds above are grantable; there is no general
per-resource permission model (a run is still owner-or-admin only — no "read-only
share" or "co-owner" concept), no tenant/org columns, and no separation of duty
among super admins — every admin (and the admin token, always) can rewrite the
policy that bounds them
(`threatmodel/THREAT-MODEL.md` residual #14, still open).

The SSH gateway's admin override is a **bounded-stale stamp**, not a live role
check, and since 0.8.5 it reaches only runs with no personal owner
(`operator_owned`; a fresh admin key on a person's run is refused
`run_owner_only`): since migration `0043` a key authorizes when `run.created_by == the key's
principal` OR (the run is operator-owned AND the key's `role` column reads `admin` AND its `role_checked_at`
(migration `0046`) is no older than `WARDYN_SSH_ROLE_TTL` (default `24h`)). The
stamp is written at `POST /me/ssh-keys` time from the registering session's role
and RE-stamped — both columns — on every OIDC login for that principal, across
every key they hold. The gateway never reads the role live at connect time (SSH
carries no session for `requireOperator`), so a demoted admin's key loses the
override at their next login (re-stamped `role=user`) or once `role_checked_at`
ages past the TTL — whichever comes first; deleting the key (`DELETE
/me/ssh-keys/{fingerprint}`, self-service) and re-registering is the immediate
lever. Strictly weaker than the web terminal's live `requireOperator` gate, but no
longer unboundedly so. The same TTL is why **an admin upgrading from 0.5 (or pre-`0046`)
does not get the override on the key they already have until it is refreshed**:
`0043` backfills every pre-existing row as `member` (fail-closed; `0074` renames it `user`) and `0046`
backfills `role_checked_at` as `NULL`, which `sshAuth` treats as infinitely
stale. A member's key never satisfies the override, and neither does a key an
admin registered while in the user view, which is stored capped (migration
`0070_ssh_key_view_capped`; `docs/SSH.md`'s Bounds section;
`threatmodel/THREAT-MODEL.md` residual #15). See
[ROADMAP.md](../ROADMAP.md) for what's queued.

**None of this governance is a paid tier.** The admin/member split, the capability
grants, the approval broker and the append-only audit log all ship in the
Apache-2.0 build — no license key, no "Premium" gate, no entitlement check
anywhere in the tree — and the gating is completeness-tested:
`internal/api/authz_test.go` walks every route the router registers and fails the
build if any one is missing from its `routeMatrix`, so a new route must be
classified admin/member/owner/anonymous/internal before it can ship;
`internal/api/rbac_test.go` then proves the widest admin-gated routes really do
403 a member. What Wardyn gives up is *breadth* — a deliberate two-tier split, not
per-user roles or multi-org depth — not the governance itself.

## The policy a run got

`GET /api/v1/runs/{id}/policy`, `wardyn run policy <run-id>` and the SDK's
`GetRunPolicy` answer "what was this run allowed to do?" after the run has
started, which the saved policy cannot: it is overwritten in place, and an
inline or default policy leaves no row at all. The answer comes from what
dispatch recorded in the append-only audit log, so it is what the sandbox's proxy
enforces, not a re-derivation:

- **The policy itself** is the `run.policy.resolve` envelope, as a normal policy
  document, with restart denies (`run.revive` `denied_added`) folded in.
- **Where it started** is the `policy_source` datum on the run's `run.create`
  row: the saved policy (id, and its name and content as they read at launch), an
  inline policy, the default, or the governance profile the run's creator was
  bound to. It is recorded already redacted, because the run's creator can read
  that row through `GET /audit`.
- **What changed at launch** is each difference between the two, given a cause:
  `workspace`, `source_control`, `mirror`, `model_access`, `git_broker`,
  `profile`, `org_disk`, `restart`, `limits` or `launch`. The launch audit rows
  (`run.egress.add`, `run.requirement.*`, `run.artifact.redirect`,
  `run.bedrock.configure`, `run.egress.confine`, `run.ceiling.reassert`,
  `run.revive`) name the cause first; the rest are derived; anything nothing
  names is `launch`. `limits` is only ever claimed for a member the governance
  bound applied to.
- **Whether the saved policy has moved** (`stored_policy_now`) compares the
  saved policy today with its recorded launch content. A run from before the
  record existed can only say the policy was updated after launch, which a rename
  alone also does.

**Who can read it.** Whoever can read the run (`GET /runs/{id}`): its creator or
an admin. Anyone else gets the run's own `404` and a `not_owner` audit row. A
portal's delegated token is refused `403` `delegation_scope`: the route is not on
the delegation list. Below the security admin tier the policy's mount sources
read `<redacted>` and grant secret names are dropped (`redacted: true`), the same
rule as reading a policy; `llm_inspection` secret values are never returned. The
result is a policy document you can reuse as is only from the security admin tier
up; below it, fill in the hidden values first.

**What it does not cover:** hosts approved while the run was running (Approvals),
credentials the run was handed (Credentials), folders added from a workspace or a
drive, and Azure DevOps access that came from the connection's defaults. A run
that has not reached sandbox setup answers `state: "not_yet"`, and one that ended
before it `"never"`; the CLI prints the sentence and exits `1` so a redirect never
writes an empty file. A run from before `policy_source` existed answers
`complete: false`: its changes list only what the launch rows state.

## Run lifetime: lease, extend, revive, ends

Moved to [run-lifetime.md](operations/run-lifetime.md).

## Exercising member mode as an admin

Moved to [member-mode.md](operations/member-mode.md).

## Second user, same host

> This recipe gives a second person their own SSO identity instead of the shared
> admin token. What that identity *can do* is exactly the **admin/member** model
> in [Multi-user: who can change what](#multi-user-who-can-change-what) above.
> Under OIDC, `WARDYN_OIDC_OPERATOR_EMAILS` is the boot-required allowlist — an
> empty one **refuses to boot** unless `WARDYN_ALLOW_OIDC_NO_OPERATOR_LIST=true`,
> which, absent a role map, instead makes every signed-in human admin
> (`validateOperatorPosture`, `cmd/wardynd/boot_posture.go`). The admin token is always an admin and cannot be
> demoted ([ROADMAP.md](../ROADMAP.md)).

A **remote** second person is a dead end regardless: both the bundled Dex and
wardynd publish loopback-only (`127.0.0.1:PORT`,
`deploy/compose/docker-compose.yaml`) by design. What *does* work is two people on
the same box, each with their own identity, and takes setup `make setup` does not
do for you:

> **Read this before you hand someone a shell on this box.** "Loopback-only"
> bounds the network, not the *host*: every local user reaches `127.0.0.1`, and
> two of the published ports are not behind any Wardyn identity at all.
>
> - **Postgres, `127.0.0.1:${WARDYN_PG_PORT:-5432}`, password `wardyn-dev`** —
>   a literal published in this repository and written into every stack
>   `deploy/compose/docker-compose.yaml` starts. It is Wardyn's whole system of
>   record: `psql -h 127.0.0.1 -U wardyn wardyn` from the second person's own
>   shell reads and REWRITES every run, every policy decision and the
>   append-only audit log, under no Wardyn role and leaving no Wardyn audit
>   entry. The admin/member split below is enforced by wardynd, so anything that
>   goes around wardynd is not subject to it.
> - **The devcontainer-build registry, `127.0.0.1:${WARDYN_REGISTRY_PORT:-5010}`,
>   with no authentication** — any local user can push a layer that a later
>   `WARDYN_ENVBUILD_PUSHED_REF` run pulls and executes.
>
> Both are governed by host access, so a second person you do not trust with the
> database is a second person you do not put on this box. Repoint them
> (`WARDYN_PG_PORT` / `WARDYN_REGISTRY_PORT`) and firewall the loopback ports if
> your host has more users than that — Wardyn does not do it for you. This is
> `threatmodel/THREAT-MODEL.md` residual #23 (the shipped default deployment
> collapses the audited insider into the trusted operator) seen from the
> operator's side.

1. **Turn local mode off.** The containerized `make setup` path writes
   `WARDYN_LOCAL_MODE=true` into `deploy/compose/.env` (see the
   `WARDYN_ADMIN_TOKEN` row in [ENV.md](ENV.md)); left in place alongside a
   configured OIDC issuer, wardynd **refuses to boot** rather than silently
   winning over OIDC (`resolveLocalMode`, `cmd/wardynd/boot_flags.go`: an
   explicit `-local-mode` with `-oidc-issuer` also set is refused unless
   `WARDYN_ALLOW_LOCAL_MODE_WITH_OIDC=true` overrides it — and if you do override
   it, no login is ever required, for anyone, regardless of what you configure
   below). A `make setup` re-run refuses outright on this combination
   (`refuse_local_mode_with_oidc`, `scripts/up.sh`), rather than proceeding into
   a boot that would then fail. In
   `deploy/compose/.env`:

   ```sh
   WARDYN_LOCAL_MODE=false
   ```

2. **Turn Dex on, and choose who's an admin.** Dex is the `sso` compose profile;
   every other OIDC var already defaults to match the bundled
   `deploy/compose/dex.yaml` client (defaults in the `wardynd` service's
   `environment` block in `docker-compose.yaml`), so the two you set are the
   issuer and the operator allowlist. With OIDC configured an **empty** `WARDYN_OIDC_OPERATOR_EMAILS`
   refuses to boot, so list yourself: everyone listed is an **admin**, everyone
   else who can sign in is a **member** (add `WARDYN_OIDC_ROLE_MAP` to derive
   admin/member from SSO roles/groups instead).

   ```sh
   echo 'WARDYN_OIDC_ISSUER=http://localhost:5556'        >> deploy/compose/.env
   echo 'WARDYN_OIDC_OPERATOR_EMAILS=demo@wardyn.local'   >> deploy/compose/.env   # one of the two Dex identities; your own email needs a staticPasswords entry in dex.yaml first
   docker compose -f deploy/compose/docker-compose.yaml --profile sso up -d dex wardynd
   ```

   Not a soft gate: wardynd runs synchronous OIDC discovery against the issuer at
   boot and **exits nonzero if it fails** (`cmd/wardynd/boot_deps.go`) — an
   unreachable Dex refuses the whole boot, not just SSO. Compose's `depends_on:
   dex: condition: service_healthy` sequences this for the command above; it only
   bites if you later restart wardynd alone while Dex is down.

3. **Give a third person their own login** (0.7.4 already ships
   `demo@wardyn.local` and `member@wardyn.local` below — this recipe adds a
   third identity). For the bundled Dex, `staticPasswords` in
   `deploy/compose/dex.yaml` is the authentication list —
   `enablePasswordDB: true` with no external connector means an email absent from
   it has no password to authenticate with, full stop. (Dex authenticates, the
   operator list authorizes.) Mint a bcrypt hash (any bcrypt tool at the same
   cost works):

   ```sh
   htpasswd -bnBC 10 "" 'their-password' | tr -d ':\n'
   ```

   and add an entry alongside the two existing users:

   ```yaml
   staticPasswords:
     - email: "demo@wardyn.local"
       hash: "$2a$10$SDMtAYUgJDDzcanSySsoBuLPINvmRvxVpqg3WU9jfThQABkwBvaiK"
       username: "demo"
       userID: "demo-0001"
     - email: "member@wardyn.local"
       hash: "$2a$10$SDMtAYUgJDDzcanSySsoBuLPINvmRvxVpqg3WU9jfThQABkwBvaiK"
       username: "member"
       userID: "member-0001"
     - email: "reviewer2@wardyn.local"   # not in the operator list ⇒ a member;
       hash: "<paste the whole generated hash>"  # domain must clear WARDYN_OIDC_EMAIL_DOMAINS
       username: "reviewer2"
       userID: "reviewer2-0001"
   ```

   then reload it — `up -d` does not notice a bind-mounted file's *content*
   changing, only `restart` does:

   ```sh
   docker compose -f deploy/compose/docker-compose.yaml restart dex
   ```

4. **Each person signs in on their own.** The browser is redirected to Dex
   directly for the login leg, so both `:8080` (wardynd/UI) and `:5556` (Dex)
   must be reachable from each browser — trivial at a shared console, an `ssh -L
   8080:localhost:8080 -L 5556:localhost:5556 <host>` tunnel per person otherwise
   (a tunnel to the existing loopback bind, not a change to Wardyn's network
   posture). Each clicks **Sign in with SSO**.

`WARDYN_OIDC_EMAIL_DOMAINS` is a separate knob with a different failure mode: an
**unset** value is not "deny all", it fails **open** — any account the IdP
authenticates gets a session, and without the domains list the `email_verified`
claim is not checked at all unless `WARDYN_OIDC_REQUIRE_EMAIL_VERIFIED` is on
(the domain check lives inside the domains branch — `AllowedEmailDomains`,
`internal/auth/oidc/oidc.go`). Compose already pins it
to `wardyn.local` (`docker-compose.yaml`), so this stack is fail-closed as
shipped; re-point it when you swap Dex for a corporate IdP.

`WARDYN_OIDC_REQUIRE_EMAIL_VERIFIED=true` applies the `email_verified` check
without a domains list (default off): a missing claim counts as unverified and is
refused, exactly as below, so on an IdP that never sends it (Entra) it denies every
login.

With the domains list set, `email_verified` **absent** from the id_token and
`email_verified: false` are two different denials, logged and coded separately
(`auth_error=email_verified_absent` vs `email_unverified`). Entra ID tokens
typically omit the claim entirely rather than sending it false — every login
against such a tenant with the domains list set is denied, by design, with no
opt-in flag to relax it. On such an IdP prefer `WARDYN_OIDC_ROLE_MAP` against the
signed `roles`/`groups` claims (plus the app registration's "assignment required"
setting) instead of the domains list.

`WARDYN_OIDC_CLIENT_SECRET` is optional: PKCE S256 is sent on every login
regardless, so a **public client** registration (a SPA/native-app client type
with no secret — some IdPs refuse to issue one for a confidential client) works
the same as a confidential one. Leave it unset for that shape; nothing else in
the OIDC config changes. **Exception (0.8.2):** an Azure DevOps row with
`token_mode: minted_pat` needs a confidential app. The row must name Wardyn's own
sign-in app and this secret must be set, or saving the row is a 400 and the row
is unusable (`ado_pat_needs_console_app`). On a public-client registration, move
the console redirect URI to the **Web** platform, add a client secret and set this
variable; `AADSTS700025` means the redirect is still on a public-client platform.

## Launch presets

Moved to [launch-presets.md](operations/launch-presets.md).

## Console branding

Moved to [console-branding.md](operations/console-branding.md).

## Workspaces: three tiers

A workspace is not one unit of configuration. Wardyn splits it into three:

1. **Source** (tier 1) — a repo or local directory configured ONCE, in a
   shared library: its own requirements contract, its own scan profile and
   status, deduplicated by canonical identity (locator + ref). `GET/POST
   /api/v1/sources`, `GET/DELETE /api/v1/sources/{id}`, `POST
   /api/v1/sources/{id}/scan` (`mountLibraryRoutes`,
   `internal/api/sources.go`) — there is no separate
   `/sources/{id}/requirements` route, and no `PUT` either. A source's own
   contract is authored by re-`POST`ing an existing identity to
   `POST /api/v1/sources` with a new requirements body, which APPLIES it
   (WSPIPE-8); the write-once `handleUpdateSource` that `PUT` used to reach is
   gone, not stubbed.
2. **Base image** (tier 2) — a shared catalog row: registry, custom, or BYO.
   "Recommended" is never a catalog kind — it is a per-workspace DERIVED
   build, excluded by a database CHECK constraint, not by convention.
   `GET/POST /api/v1/base-images`, `DELETE /api/v1/base-images/{id}` — there
   is no `GET` by id (`handleGetBaseImage` went with its route).
3. **Workspace** (tier 3) — an ordered list of attachments (library sources, or
   inline ephemeral scratch dirs) plus an optional catalog image. Attachment
   order is load-bearing: `attachments[0]` is the primary, the same rule a
   single `sources[0]` carried before the split. The floor is one attachment —
   an ephemeral scratch dir, seeded structurally so the invalid empty state
   cannot be built.

`wardyn source list|create|scan|rm` manages tier 1 from the CLI; `wardyn
workspace create --attach SOURCE-ID[@target][:ro|:rw]` composes a workspace from
already-configured sources. Deleting a source or image that workspaces still use
answers `409` naming every workspace attaching it (`handleDeleteSource` /
`handleDeleteBaseImage`); `?force=1` detaches them instead — for an image that
means "fall back to the derived recommended build", for a source it un-mounts
code, which is why the refusal is the default.

### The effective contract is one pure fold

A workspace's effective requirements come from `FoldWorkspaceContract`
(`internal/types/workspace_contract.go`) over its attachments, the attached
sources' own contracts, and the workspace's own overlay rows — computed at the
store's hydrate pass, never stored duplicated. Precedence, in order:

- each attachment contributes its source's contract, in attachment order;
- a `write:<path>` row is DROPPED unless `<path>` is that source's own locator —
  a shared source cannot declare write access to a path it doesn't own and
  silently widen a sibling mount in every workspace that attaches it;
- when two attachments contribute the same key the merge is fail-closed: the
  LEVEL takes the strongest contributor (required beats optional), the
  PROVENANCE the weakest (`scan_seeded` beats `operator_set`), so a row that came
  from reading untrusted repo content never auto-grants a credential even when
  another contributor declared the same key as a direct operator act;
- the workspace's own overlay rows replace the merged value outright — an overlay
  cannot REMOVE a key (a per-attachment override does that, next).

With no attachments, or all-ephemeral ones, the fold is exactly the overlay —
which makes migration `0031` provably behavior-identical for every workspace that
predates it.

### Per-attachment overrides are modeled but not yet operator-reachable

`WorkspaceAttachment.Overrides` lets a workspace disable (`off`) or re-lane
(`optional`/`required`) a requirement key ONE of its attached sources declares —
mount a repo read-only to read its code without inheriting its build secrets, say
— and the fold above honors it. **There is no write path to it yet**: no field on
the workspace write endpoints, no CLI flag, no wizard control sets `Overrides`,
and every attachment Wardyn builds today carries `SourceID`/`Target`/`Writable`
only. Modeled and folded, not operator-reachable, until a write surface ships.

### The requirements contract: Required or Optional, nothing else

Every control a source or workspace can carry — a secret by name, an egress host,
write access to a directory, a named integration — is a row with ONE axis:
**Required** rides along with every run that attaches the workspace, **Optional**
is a per-run opt-in. `PUT /api/v1/workspaces/{id}/requirements` writes the
workspace's own overlay rows (`handleSetWorkspaceRequirements`,
`internal/api/workspace_requirements.go`); the workspace detail page is the
surface that writes them. Launch and preflight both read
`effectiveRequirements(ws)` — the same fold — so Review can never predict
something launch won't do. A `scan_seeded` requirement can never auto-grant a
secret on its own: the scanner reads untrusted repo content, so only an operator's
direct declaration attaches a credential (the fail-closed provenance rule above).

## Integrations

Moved to [integrations.md](operations/integrations.md).

## Network: upstream proxy and egress redirects

One more piece of operator-wide config lives in Postgres alongside everything in
**State stores** above: `SiteConfig` (`GET`/`PUT /api/v1/site-config`, `wardyn
site-config get|set`) — the corporate upstream proxy and the list of outbound
redirects every run's egress inherits. Unconfigured is a valid, common state.
Because it lives in Postgres, `make reset` / `make reset-all` take it with the
volume; `wardyn site-config get > corp-baseline.json` before a reset and `wardyn
site-config set corp-baseline.json` after is the round-trip — the document
carries secret **names**, never values, so it is safe to keep beside the repo.
Because values never round-trip, `set` re-attaches the *names* unconditionally
even when a named secret was never restored into the fresh store: `set` prints a
warning naming every such dangling ref, and the setup checklist's "Site config"
row grades `warn` (never the plain `info` of a fully-live config) while one
remains.

A captured document carries `onboarding_completed_at` whenever the install it
came from had finished the Getting Started funnel, and `set` forwards it
verbatim — deliberately: no client strips it, so the same file works through
`curl` and as the MDM-delivered `/etc/wardyn/site-config.json`. The server owns
that mark, so it is ignored on the write and the STORED one (none on a fresh
install; this install's own once its operator has finished the funnel) is
carried forward: applying a captured baseline restores corporate network config,
never the funnel's completion state. `PUT` never refuses a body over this field
— it cannot be set, cleared or moved through that endpoint whatever it says, so
a refusal would only have broken the recovery flows (`internal/api/site_config.go`,
`handlePutSiteConfig`). When the file's copy is dropped — a captured baseline
applied after re-onboarding, or the MDM file re-applied to a laptop that has
since finished its own funnel — the response says so
(`onboarding_completed_at_ignored`) and `set` prints it as a warning.

**A SiteConfig write reaches only runs dispatched after it lands.** The fields
below that shape a run's own egress — `internal_hosts`, `upstream_proxy_no_proxy`,
`trusted_ca_pem`, the egress redirects — are compiled into the sidecar's proxy
config at dispatch and read once at sidecar startup; a run already running
keeps the config it started with for the rest of its life, however many times
you fix the SiteConfig underneath it. `PUT /site-config`'s response says so
(`applies_from: "next_dispatch"`), **and since 0.7.2 the console repeats it on
save** — BOTH halves of the Network step. The upstream-proxy saves
(`ui/src/app/components/screens/setup/corp-network-proxy.tsx:570,589,604`) and the
egress-redirect saves (`corp-network-egress.tsx:430`, the one chokepoint every
add, edit and remove passes through) carry it as the toast's description, so the
operator who just changed a field reads the lifetime at the moment they change it
rather than in this document afterwards. Both matter for the same reason: the
redirects are one of the compiled-at-dispatch fields listed above, so a save that
said nothing about its lifetime was the one most likely to be acted on twice.
Every OTHER console write of site config still repeats nothing — the toast is not
a general rule you can rely on elsewhere. A change made through
`PUT /site-config` or `wardyn site-config set` has the response field and
nothing else. Live
sidecar reload is deliberately out of scope: a running sandbox's egress
posture must not change under it with no audit row to show why. A run already
refused on a field you just corrected stays refused: kill it and start a new
one — a killed run cannot resume, and the new run reads the corrected config
from the start.

### Upstream proxy: plain URL vs. secret

- `upstream_proxy_url` — a plain URL (`http://proxy.corp.internal:8080`), stored
  and read back in the clear. A proxy address is topology, not a credential, and
  a mistyped one has to be readable to debug.
- `upstream_proxy_secret_ref` — the *name* of a secret holding the proxy URL, for
  a proxy that needs an embedded credential
  (`http://user:pass@proxy.corp.internal:8080`).

**An `upstream_proxy_url` carrying a `user:pass@` is rejected server-side,
always** (`validateSiteConfig`, `internal/api/site_config.go` — `PUT
/site-config` 400s). A guarantee, not a UI courtesy: even a client that skips its
own check cannot persist a credential in the clear this way. Put a credentialed
proxy URL in a secret instead:

```sh
wardyn secret set upstream-proxy-url          # paste the full, credentialed URL
# then reference it by name in the applied site config:
#   "upstream_proxy_secret_ref": "upstream-proxy-url"
```

Both paths are checked by the **proxy sidecar's own loader**
(`proxy.ValidUpstreamProxyURL` over `parseUpstreamProxy`), not by a second copy
of the rule: an `http://` scheme, a host, and a port in 1-65535. The plain
`upstream_proxy_url` is checked at the write — a port the sidecar refuses used
to save with `200 OK` and then `os.Exit(1)` the egress sidecar of every
dispatched run at container start. A URL held in a secret is checked at
DISPATCH instead, because no write-time validator can see inside the secret: it
is dropped there with an audited reason (`unloadable-upstream-url`, a
`run.upstream_proxy.resolve` failure) and the run falls back to direct egress
rather than being delivered.

If both fields are set, `upstream_proxy_url` wins — harmless mid-migration
from one to the other, but don't rely on it; clear whichever you're not using.

**`http://` only — an `https://` upstream proxy URL is rejected server-side,
always** (`validateSiteConfig`, same file). The hop from wardyn-proxy to your
corporate proxy is a plaintext CONNECT + `Proxy-Authorization` header; an
`https://` URL would need a TLS wrap the sidecar doesn't do, or would leak that
Basic credential in cleartext. Dispatch applies the same gate
(`resolveUpstreamProxyURL`, `internal/api/runs_bedrock.go`), and a secret
referenced via `upstream_proxy_secret_ref` carries the same restriction — store
the plain `http://` proxy URL in the secret even when it embeds a credential.

### Upstream proxy: the bypass list (`upstream_proxy_no_proxy`)

With an upstream configured, **every** forward dial is `CONNECT`ed through it,
and the sidecar resolves the name for its own private/reserved-IP guard before it does.
So on an estate whose endpoints are private (a VPC endpoint / PrivateLink, an
in-cluster service, a corp mirror on RFC 6598) a name that resolves here is refused
`builtin:private-ip` before the corp proxy is asked, and one this proxy cannot
resolve reaches a corporate forward proxy that will not `CONNECT` to an internal
address and times out. Neither is reachable: `internal_hosts` lifts the guard,
`upstream_proxy_no_proxy` is the bypass that moves the dial to this sidecar, and
the estate needs both.
`upstream_proxy_no_proxy` is the bypass — the operator-hop equivalent of the
`NO_PROXY` the sandbox already honours internally, spelled the same way:

```jsonc
"upstream_proxy_url": "http://proxy.corp.internal:8080",
"upstream_proxy_no_proxy": [
  "vpce.amazonaws.com",     // host or domain suffix (a leading "." is fine)
  "mirror.corp.internal",
  "100.64.0.0/10"           // CIDR, matched against a literal-IP destination
]
```

Suffix entries match names; CIDR entries match only a destination written as
an IP literal, never a hostname that resolves into the range — the same rule as
Go's `NO_PROXY` (`bypassUpstream`, `internal/egress/proxy/egress_target.go`).
So on an estate where AWS resolves into CGNAT, `100.64.0.0/10` bypasses
nothing for `portal.sso.<region>.amazonaws.com`; list the name or its suffix.

Wildcards are refused at write time: "bypass everything" is spelled by clearing
`upstream_proxy_url`, not by one character in a list. An entry that is neither a
CIDR nor a host is a 400 too, because the proxy drops what it cannot compile and
a typo would otherwise mean "still proxied" — silently, at run time.

**It changes which hop dials; the guard binds both columns.** A bypassed dial
falls straight through to the same unconditional private/reserved-IP guard an
unproxied dial faces, and a dial that goes THROUGH the corp proxy is vetted too:
the sidecar resolves the name for the guard before it hands the hostname over.
Either way the destination still needs its `allowed_domains` entry, and
`internal_hosts` is the only field that lifts the guard — the bypass never
does. The bypass is the other half of the same configuration: it decides which
hop takes the dial, and on a private-endpoint estate the corp proxy cannot make
it, so the two fields are set together:

| | Without `internal_hosts` | With `internal_hosts` |
|---|---|---|
| **No bypass** | the guard resolves the name and refuses `builtin:private-ip` before the corp proxy is asked (a name this proxy cannot resolve at all still goes to the corp proxy) | the guard lifts and the HOSTNAME is handed to the corp proxy (`rule_source: site-config:internal-host`); whether that proxy will `CONNECT` to a private address is the estate's own routing — on the private-endpoint estates this section is written for it will not, which is why the bypass exists |
| **Bypassed** | dialled directly, then refused `builtin:private-ip` | **reaches the endpoint** dialled directly (`rule_source: site-config:internal-host`) |

The left column is the safety property, not a rough edge: neither bypassing a
host nor proxying it lifts the guard — only `internal_hosts` does. A refusal
there names its own cause in the `X-Wardyn-Egress-Detail` response header and
points at `internal_hosts`. The right column is not a promise of reachability:
lifting the guard is necessary, never sufficient, because the dial still has to
leave whichever hop takes it. That is why the bottom-right cell — bypass AND
lift — is the working private-endpoint configuration, and why the recipes below
set both fields.

### Phase B: the SSO/Bedrock MITM lane and the upstream proxy

Phase B (`WARDYN_AWS_SSO_PROXY_INJECT=on`, the default — see [ENV.md](ENV.md) and "Turning the
lane off" below) terminates and re-originates `portal.sso.<region>.amazonaws.com` inside the
`wardyn-proxy` sidecar to inject a captured AWS SSO session on the wire. That re-origination is a
forward dial like any other in this section, not a separate lane with its own rules: it is governed
by `upstream_proxy_url`, `upstream_proxy_no_proxy` and `internal_hosts` exactly as above, and on a
private-endpoint estate it needs the same bypass-plus-lift configuration a VPC-endpoint Bedrock
deployment already does (see "Bedrock on a private endpoint" below).

**The invariant, stated once:** the sandbox's dials — and the sidecar's forward dials on the
sandbox's behalf, MITM re-origination included — follow `SiteConfig.upstream_proxy_url`; wardynd's
own dials follow `WARDYN_DAEMON_PROXY_URL` ("wardynd behind a corporate proxy", next); every
outbound path belongs to exactly one of those two, so a new path that dials must
say which. See
[docs/adoption/aws-sso-mitm-upstream-proxy.md](adoption/aws-sso-mitm-upstream-proxy.md) for
what the code does today.

**A TLS-intercepting corporate proxy needs its CA on both sides of this lane, asymmetrically.**
Every sandbox image bakes `corp-ca.pem` at build (`install_mitm_ca`, "Corporate TLS-inspection root"
below), but the `wardyn-proxy` image carries a corporate CA only if one was staged at its own build —
otherwise it trusts one only through `WARDYN_TRUSTED_CA_FILE` ([ENV.md](ENV.md)). Unset, the
re-origination's re-dial fails `x509: certificate signed by unknown authority`, filed as the same
bare `builtin:dial-failed` as every other dial failure in this section.

### wardynd behind a corporate proxy

Everything above this point in this section — `upstream_proxy_url`, `upstream_proxy_no_proxy`,
`SiteConfig`, the Phase B MITM re-origination just above — is the **sandbox's** egress hop: it
governs what a run's own outbound traffic sees, compiled into the `wardyn-proxy` sidecar's config at
dispatch. It has nothing to do with **wardynd's own** outbound calls: OIDC discovery/JWKS at boot,
the audit webhook sink, GitHub App token minting, AWS SSO `CreateToken` renewal, and Entra directory
sync. Those five calls all ride the process's shared
`http.DefaultTransport`, and Go's `net/http` honors the standard `HTTP_PROXY` / `HTTPS_PROXY` /
`NO_PROXY` variables **process-wide** — including inside the Kubernetes client, so a mistyped
`NO_PROXY` on a k8s deployment can take the control plane's own API access down with it. That is why
those three variables are documented as unsupported for wardynd's runtime environment (see `docs/ENV.md`'s
`HTTP_PROXY` row) rather than a supported knob.

`WARDYN_DAEMON_PROXY_URL` (+ `WARDYN_DAEMON_NO_PROXY`) is the supported, scoped replacement: it sets
`http.DefaultTransport.Proxy` directly at boot (`installDaemonProxy`, beside the same-shaped
`WARDYN_TRUSTED_CA_FILE` trust-tier knob), so it reaches exactly wardynd's five outbound consumers above
and nothing else — the Kubernetes client builds its own transport (unaffected) and the Docker client
speaks a unix socket (unaffected). Unset leaves the transport untouched, byte-identical to today
(`ProxyFromEnvironment` still applies if you set the standard variables yourself — unsupported, not
rejected).

**The bypass list defends itself.** Wardynd auto-appends three hosts to `WARDYN_DAEMON_NO_PROXY` before
applying it, because getting this wrong is exactly the outage this knob exists to prevent:
`KUBERNETES_SERVICE_HOST` (the in-cluster API server address), the `WARDYN_AWS_SSO_ENDPOINT_OVERRIDE`
host when one is configured (a kind Service in a test walk must never be dialed through a corporate
proxy), and the `WARDYN_OIDC_INTERNAL_ISSUER` host when one is configured (a cluster-internal issuer
wardynd itself dials at boot, before SiteConfig or any other runtime read exists). Every boot that sets
a proxy logs one line naming the proxy host (never any embedded
credential — `WARDYN_DAEMON_PROXY_URL` refuses to start if the URL carries `user:pass@`) and the
effective bypass list:

```
grep 'daemon egress proxy configured' <logs>
```

A malformed `WARDYN_DAEMON_PROXY_URL` (not `http://`/`https://`, no host, or an embedded credential)
refuses boot rather than silently falling back to direct — same posture as `WARDYN_TRUSTED_CA_FILE`.

### Control-plane to proxy TLS

Every run's `wardyn-proxy` sidecar calls `wardynd` for everything it does on the
run's behalf, and one of those calls — `GET /api/v1/internal/injection/{grant}` —
answers with a credential **value**. Since 0.7.12 that hop is TLS on every install
shape except a loopback-only local one, and the proxy trusts exactly one root for
it.

- **wardynd's end.** On first boot `wardynd` mints an internal CA (ECDSA P-256, ten
  years) and stores it in the secret store as `wardyn-internal-ca`, beside its
  signing key: age-encrypted in Postgres, in every backup that carries the
  signing key, reserved from the secrets API and from every grant. At each boot
  it signs a serving certificate for the host of `WARDYN_CONTROL_PLANE_URL` — the
  exact name every proxy dials — and serves `/api/v1/internal/*` and `/healthz`
  on `WARDYN_INTERNAL_LISTEN` (default `:8443`, TLS 1.3 only). A bind failure
  ends the daemon. The console listener (`WARDYN_LISTEN`) is unchanged.
- **The proxy's end.** Dispatch puts the CA's public certificate in each run's
  sealed proxy config (`control_plane_ca_pem`: written to the proxy's stdin by
  the docker driver, the k8s driver's per-run Secret, staged by a
  nonroot init container into an owner-only file the sidecar reads via
  `-config` — where the per-run MITM CA already travels). The proxy trusts
  that certificate and nothing else
  for every control-plane call: the resolve, mints, token renewal, decisions,
  approvals and uploads. Not the system roots, and not `WARDYN_TRUSTED_CA_FILE`:
  that bundle is for egress, because a TLS-inspecting box sits between the proxy
  and the internet, never between the proxy and `wardynd`. A wrong CA, a wrong
  name, or an https URL with no CA fails the call closed — at startup, the proxy
  does not start.
- **"Local", precisely.** `http://` is accepted only when the URL's host is
  `localhost`, an address in `127.0.0.0/8`, or `::1` — matched literally, with no
  DNS lookup. `wardynd` applies the rule at boot and the proxy at start (one
  function, `hoptls.CheckURL`); anything else is refused with the fix in the
  message. `host.docker.internal` is not local: those bytes cross a bridge or the
  Docker Desktop VM boundary.
- **Per install shape.** Nothing to configure on any of them:

  | Install | `WARDYN_CONTROL_PLANE_URL` | Notes |
  |---|---|---|
  | Helm (`k8s.enabled`) | `https://<release>.<namespace>.svc.cluster.local:8443` | Service port `internal` (`service.internalPort`); the chart's NetworkPolicy grants it to run proxies |
  | Compose, Desktop, m′ | `https://wardynd:8443` | on `wardyn-internal`; never published to the host |
  | Host mode (`scripts/run-host.sh`) | `https://host.docker.internal:8443` | `wardynd` binds `:8443` on the host |

- **Checking it.** `/healthz` reports `"proxy_hop_tls": true`, and `wardynd`
  logs `proxy-facing TLS listener (internal CA)` with the address at boot. A local
  install reports `false` and logs a warning naming the loopback URL.
- **Rotation.** The CA is replaced at boot once less than a year of its validity
  remains. Runs dispatched under the old CA then fail closed on their next
  control-plane call and must be relaunched. To rotate early, stop `wardynd`,
  delete the row (`DELETE FROM secrets WHERE owned_by = '' AND name =
  'wardyn-internal-ca';`) and start it again.
- **The ground-truth ingest rides the same hop.** `wardyn-tetragon-ingest` posts
  its audit-write-only bearer (`aud=wardyn-groundtruth`) to
  `WARDYN_CONTROL_PLANE_URL` = `https://wardynd:8443` and trusts wardynd's
  internal CA alone. `wardynd` publishes that CA's public certificate at boot as
  `control-plane-ca.pem` beside `WARDYN_GROUNDTRUTH_TOKEN_FILE` (compose: the
  shared `groundtruth_token` volume), and the ingest reads it from
  `WARDYN_CONTROL_PLANE_CA_FILE`. The ingest applies `hoptls.CheckURL` too, and
  refuses to start on a non-loopback `http://` URL or an `https://` URL with no
  readable CA file; a server its CA did not sign gets no request. It reads the
  CA once, so restart it after a CA rotation.
- **The console listener refuses the internal surface.** While the internal
  listener runs, `WARDYN_LISTEN` answers every `/api/v1/internal/*` request
  with the same `404` the internal listener gives a console route (under
  `WARDYN_BASE_PATH` too), so a run token or the ground-truth bearer is only
  ever accepted over the pinned hop, and an Ingress or reverse proxy in front
  of the console exposes none of that surface. A local install (loopback
  `http://` control plane) has no internal listener and keeps serving it on
  the console. Every shipped caller dials `WARDYN_CONTROL_PLANE_URL`, the
  internal listener: each run's proxy (both runners hand it that URL in its
  sealed config, and brokered sidecar uploads go through the proxy) and the
  ingest. A proxy dispatched before 0.7.12 still dials `http://wardynd:8080`
  and gets `404` on every call after an upgrade (on the chart it has no route
  back at all: the runs namespace is never granted the `http` port). Such a
  run cannot be restarted on either runner. On Docker it has no stored proxy
  config (migration 0093 records one only for runs dispatched from then on), so
  `POST /api/v1/admin/runs/restart` answers it `ok:false` with reason
  `revive_config_not_stored`, and a single revive is a `409` (see
  [Run lifetime](operations/run-lifetime.md), "Upgrading to this release"). On
  Kubernetes the substrate implements no `runner.ProxyReviver` (the agent pod
  pins the proxy pod's IP), so the restart refuses it with
  `runner.ErrReviveUnsupported` and reason `revive_unsupported`. On both, stop
  such runs (before upgrading, or after with `POST /api/v1/runs/{id}/kill`) and
  start a new run instead. The restart still replaces the proxy of a run whose
  config a supported release stored. The proxy authenticates
  to `wardynd` with its run token (bearer, not mTLS —
  `threatmodel/THREAT-MODEL.md` B6).

### Serving the console under a sub-path

To put Wardyn behind a reverse proxy at a sub-path next to another application
(`https://host.example.com/wardyn/`), set `WARDYN_BASE_PATH=/wardyn` (Helm:
`basePath: /wardyn`). Unset, everything stays at the host root exactly as before.

- **The proxy forwards the path unchanged.** `wardynd` mounts the console, the
  API, `/auth/login`, `/auth/callback`, `/healthz`, `/readyz` and `/metrics`
  under the prefix and answers 404 for everything outside it, so a rule that
  strips the prefix breaks every request. nginx: `location /wardyn { proxy_pass
  http://wardynd:8080; }` (not `location /wardyn/` — `wardynd` itself 404s
  `/wardynx/…`, so the bare-prefix location matching the daemon's own segment
  check is what makes `https://host.example.com/wardyn` reach the console) —
  no trailing slash or URI on `proxy_pass`. WebSocket upgrades (the terminal)
  need the usual `Upgrade`/`Connection` headers.
- **One bundle, any prefix.** The console is built with relative asset URLs;
  `wardynd` writes the base into the `index.html` it serves (an attribute, not an
  inline script — the CSP is unchanged) and the console builds every API, sign-in,
  terminal and recording URL from it. A refresh on a deep link works.
- **Cookies** — the session, the sign-in cookies and the Azure DevOps sign-in's.
  Over plain HTTP they are scoped to `Path=/wardyn`. A session issued before the
  console moved under a base path (a same-host migration) carries `Path=/` and
  stays valid until it expires; signing out clears both. Under TLS (see
  "Console cookies under TLS" below) they are `__Host-` cookies, which the
  browser only accepts at `Path=/`, so the neighbouring application on the same
  host receives them too. That gives it nothing new: it shares the console's
  origin, so its pages can already call the console's API with them attached.
- **SSO.** Register and set `WARDYN_OIDC_REDIRECT_URL` under the base
  (`https://host.example.com/wardyn/auth/callback`); boot refuses one outside it,
  naming both variables. The Azure DevOps callback is derived from it and carries
  the base itself.
- **Health checks** move with the prefix: `/wardyn/healthz`, `/wardyn/readyz`.
  The chart's probes follow `basePath` (or an `env.WARDYN_BASE_PATH`); a compose
  or external health check has to be edited by hand.
- **What stays at the root.** The proxy-facing TLS listener
  (`WARDYN_INTERNAL_LISTEN`, the default `https` `WARDYN_CONTROL_PLANE_URL`) is not
  behind your reverse proxy, so runs keep dialling it at the root. A plain
  `http://` loopback control-plane URL reaches the console listener instead, so
  it has to end in the base (`http://127.0.0.1:8080/wardyn`); boot refuses one
  that does not. The CLI's `WARDYN_URL` and `wardyn-tetragon-ingest`'s
  control-plane URL point at the console listener too: include the base in them.
- **UI sandboxes.** The shared-origin gateway (no
  `WARDYN_UI_SANDBOX_ORIGIN_TEMPLATE`) serves its enter and relay routes under
  the same prefix on its own listener, and `/healthz`'s
  `ui_sandbox.enter_url_template` and `ui_sandbox.enter_post_url` both include it
  — proxy that origin with the prefix too. Per-run origins (host mode) are
  separate hosts and are unchanged.
- **Not detected.** A proxy that strips the prefix is not refused at boot —
  nothing in a request says it was stripped; it shows up as 404s on every page.

### Console cookies under TLS

The console's cookies are the session (`wardyn_session`), the sign-in's one-time
cookies (`wardyn_oidc_state`, `wardyn_oidc_nonce`, `wardyn_oidc_pkce`,
`wardyn_oidc_widened`) and the Azure DevOps sign-in's (`wardyn_ado_state`,
`wardyn_ado_nonce`, `wardyn_ado_pkce`). With secure cookies on (TLS served
directly, or `WARDYN_TLS_TERMINATED`), each one is written as a `__Host-` cookie
(`__Host-wardyn_session`, and so on): `Secure`, `Path=/` and no `Domain`, also
under `WARDYN_BASE_PATH`. A browser refuses to store a `__Host-` cookie that
breaks any of those rules, so no other host under the console's registrable
domain can plant one. That includes a UI-sandbox app relayed on a sibling host,
whose page script can set `Domain=` cookies the console receives. In this
posture the console never reads the unprefixed names, so a planted
`wardyn_session` is ignored rather than taken as a sign-in.

- **Plain HTTP keeps the plain names.** A browser refuses `__Host-` and `Secure`
  cookies from a plain-HTTP origin, so with secure cookies off the console uses
  the unprefixed names, scoped to `WARDYN_BASE_PATH` when it is set. Any host
  under the same registrable domain can then plant them: its page can sign the
  browser into the console as another account, or seed the sign-in's state.
  Keep plain HTTP to a loopback or single-host install with nothing else served
  under its domain. Anything other people reach belongs behind TLS.
- **The prefix does not separate a host from itself.** Cookies ignore ports. A
  path-mode UI-sandbox gateway on the console's own hostname (another port)
  receives the console's cookies. The relay strips every `wardyn_*` cookie,
  `__Host-` spellings included, before the app sees the request, and drops any
  the app tries to set. The relayed page's own script still runs on that
  hostname and can set a host-only `__Host-wardyn_session` there. Only a gateway
  on a hostname of its own is out of that reach.

The threat model's residual 18 carries both bounds.

### Corporate TLS-inspection root

A TLS-inspecting upstream proxy — one that terminates and re-signs TLS with its
own internal CA rather than relaying CONNECT bytes — is unusable with the
published images out of the box: `wardynd`'s own outbound TLS (OIDC discovery, the
GitHub App transport, the audit webhook sink), the `wardyn-proxy` sidecar's
forwarding transport, and every sandbox's own TLS clients on a passthrough CONNECT
tunnel all fail certificate verification against a root none of them trust.

`WARDYN_TRUSTED_CA_FILE` closes that gap: a PATH to a PEM bundle of additional
trusted roots, additive to the system roots, read once at `wardynd` boot
([ENV.md](ENV.md)). The file must hold certificates only — a private key or CSR
exported alongside the root is refused at boot (`loadTrustedCA`) — and the bundle
handed to the sidecar and the sandboxes is rebuilt from the parsed certificates,
never copied from the raw file. It reaches all three processes: `wardynd` mutates
the shared `http.DefaultTransport`; the proxy sidecar and every sandbox get the
SAME bundle forwarded per run (`runner.ProxyConfig.TrustedCAPEM`,
`installSandboxTrustedCA`). Unset is byte-identical to today. A file that does not
exist, or whose content has no parseable certificate, refuses boot naming the var.

Delivery per install path: compose points it at a file under the
`WARDYN_MANAGED_DIR:/etc/wardyn:ro` mount; the desktop tier ships the PEM
alongside `policy.json` in the same MDM payload ([DESKTOP.md](DESKTOP.md)); the
Helm chart's `trustedCA` value bakes the PEM into a ConfigMap and wires the env
var (`--set-file trustedCA=corp-ca.pem`, chart README's "Corporate CA trust").

**Named ceiling, accepted rather than solved**: with the knob set, a run's sandbox
env carries the corporate PEM even when Wardyn's own TLS-MITM is off for that run,
so a BYOI base image missing every system CA-bundle path (`agent-run-lib.sh`'s
`install_mitm_ca` warns "proxy-CA-only" in that shape) loses public trust for its
OpenSSL-shaped clients (curl, Python, Ruby); the published images are unaffected.
See [docs/adoption/corp-image-authoring.md](adoption/corp-image-authoring.md) and
[docs/ENVBUILD.md](ENVBUILD.md#base-image-trust-byoi). Not a `SiteConfig` field:
boot-time operator posture, never a live API write, never agent-reachable —
`threatmodel/THREAT-MODEL.md` residual #28.

### Egress redirects: two tiers

`egress_redirects` is a list of `{from, to, token_secret_ref,
token_integration_ref, ecosystem}` entries. Each substitutes a public/upstream URL
or host for a corporate-internal one in every run's egress, with an optional token
injected proxy-side as a Bearer credential for `to`'s host (the sandbox never
holds it). The egress entry a redirect adds is scoped to `to`'s **port** — the
one `to` spells, else the default of the scheme `to` spells (`80` for an explicit
`http://`, `443` otherwise) — the same port its TLS termination and token
injection use — so a `to` on a literal IP is never trusted on some other port of
that address; reach
the mirror on a different port by naming that port in `to`. It replaced the old `artifact_overrides` map (one entry per package
ecosystem) because a corporate estate redirects container registries and internal
appliances too — the shape generalized to "a list of From → To pairs over any
URL, host, or IP".

The token comes from either of two places, mutually exclusive — a row setting both
is rejected. `token_secret_ref` names a bare secret directly.
`token_integration_ref` instead names an **Integration** (above) to take the token
from: the integration owns the system and its credential, the redirect owns
rerouting a public endpoint to it. Pointing at an integration carries more than
its secret name — the header and format of that row's `proxy_header` delivery come
with it, so a feed authenticating with something other than `Authorization:
Bearer` (the bare-secret path's hardcoded shape) finally can. Which secret that is
follows the delivery, not the role name: whatever the row calls it, its
`proxy_header` secret is the credential this redirect presents. There is no UI
control for picking an integration here yet; the seam is usable today via `PUT
/site-config` and `wardyn site-config set`.

What you get depends on whether `ecosystem` is set:

| `ecosystem` | Egress substituted | Token injected | Per-tool config file |
|---|---|---|---|
| `npm` \| `pip` \| `cargo` \| `maven` \| `go` \| `nuget` | 🟢 yes | 🟢 yes | 🟢 yes |
| empty (**network only**) | 🟢 yes | 🟢 yes | ⛔ no |

An ecosystem row gets the per-tool config file `EmitArtifactConfig`
(`internal/workspacescan/gen.go`) writes at workspace-import time — `.npmrc`,
`.config/pip/pip.conf`, `.cargo/config.toml`, `.m2/settings.xml`,
`GOPROXY`/`GOSUMDB`, or `.nuget/NuGet/NuGet.Config` — on top of the egress
substitution and token injection every redirect gets.

A network-only row (empty `ecosystem`: a container registry, an internal
appliance, a bare host or IP) gets the network half only: host substituted into
the run's egress allowlist, token injected proxy-side, but **no config file is
written** — there is no `.npmrc` equivalent for an arbitrary host. That is a real
cost: the workspace still needs telling to pull from the mirror itself (`docker
login` against the internal registry, an appliance client's own config), or a run
reaches an allowed, credentialed host that nothing in the sandbox asks for.

**A network-only row also has a third effect the two columns above don't
show: it denies its own `from` host outright, in EVERY run, not only a run
this redirect otherwise covers.** `appendNetworkRedirectDenials`
(`internal/api/workspace_egress.go`) appends every network-only row's `from`
to `policy.DeniedDomains` unconditionally on every dispatch
(`internal/api/runs_dispatch.go`), unlike the substitution and the token plan,
both of which are scoped to a run that actually reaches one of the redirect's
public hosts. Deny beats `allow_all_egress`, so this closes the public route
even for a run the redirect's substitution never touches — the intended
GAP-EGRESS-4 protection against an allow-all Record session reaching the
public host the redirect was configured to steer away from — but it also means
a redirect an operator scoped narrowly still costs every OTHER run that public
host, with neither the `to` host nor the token to show for it. The UI
labels these rows `network only` so the gap stays visible.

**A `to` that is a literal IP** — the normal shape of a private endpoint — is
trusted as an egress target for the runs the redirect covers, on every path the
proxy vets (the opaque tunnel, the TLS-terminated token-injection path, and the
git/PAT brokers alike), and shows in the audit trail as `rule_source:
site-config:egress-redirect` rather than a generic policy allow. The trust comes
from the exact allowlist entry the substitution writes, so it is scoped to those
runs, to that address, and to **one port**: `substituteArtifactEgress` writes
`net.JoinHostPort(hostrules.HostOf(r.To), redirectPort(r.To))`, a
PORT-QUALIFIED entry, and `Policy.AllowsLiteralIP` matches it on that port only
— the one `to` spells, else the default of the scheme `to` spells (`80` for an
explicit `http://`, `443` otherwise), which is the SAME port the redirect's
TLS-MITM/token-injection half is scoped to. A bare address would have matched
EVERY port instead, so a `to` of `https://10.40.2.11:8443/` used to trust
`10.40.2.11:22` and `:5432` as well — a mirror reached on some other port needs
that port in the `to`, exactly as the token injection has always required. A run
the redirect does not cover is refused, and a `denied_domains` entry still wins. `test-redirect`
understands the shape too
(`redirectProbeTo`): it dials the `to` address while presenting the `from`
hostname for TLS, because a private endpoint's certificate names the public host
— probing the address directly failed verification and reported a correct
configuration as broken. It speaks the protocol the stored `to` names, never the
one `from` happens to be spelled with: only `to` knows whether the mirror serves
TLS or cleartext on that port — and it dials the port `to` names, which is the
one `to` spells, else the default of the scheme `to` spells (`80` for an
explicit `http://`, `443` otherwise). A `to` whose port is not a decimal
1-65535 is refused at `PUT /site-config` rather than silently read as `443` by
one reader and rejected outright by another.

### Git push confinement and content rules

Two independent controls sit on the brokered git lanes (`github_token`,
`git_pat`), and an operator tuning one must not assume it moves the other.

- **WHERE a push may land.** Branch-namespace confinement — ONE var,
  `WARDYN_GIT_BROKER_ENFORCE_BRANCH_NS` (#203 folds the former, standalone
  `WARDYN_GIT_PAT_BROKER_ENFORCE_BRANCH_NS` into its `{app,pat}` scope): App
  lane on by default, `git_pat` lane (the `pat` scope) off by default — see
  [docs/ENV.md](ENV.md) for the row.
- **WHAT a push may touch.** `push_rules` (`deny_paths`,
  `max_inspect_pack_mib`) — a policy field, not an env var, set per run. An
  operator ceiling that sets `push_rules` is a floor, not a cap: an unset
  proposal inherits it wholesale, `deny_paths` is unioned with the ceiling's,
  and `max_inspect_pack_mib` is capped only when the ceiling's value is
  non-zero. See [docs/POLICIES.md](POLICIES.md#push_rules--pushrulesspec) for
  the field reference, the pattern language, and what the inspector can and
  cannot see.

The residuals an operator should plan for:

- **A push over the inspection ceiling is refused, not held** — the remedy is
  raising `max_inspect_pack_mib` (bounded 0..64 at write time), never a
  console approval, because holding would ask a person to approve a push
  nobody inspected.
- **`ssh_key` is ungovernable by construction.** git's own SSH transport has
  no broker seam, so `push_rules` cannot be enforced on it; a policy that sets
  `push_rules` while `ssh_key` is the run's only git-capable grant is legal
  but graded a medium-risk item on the Review rail rather than blocked.
- **On `git_pat`, only WHERE is behind a switch.** Branch-namespace
  confinement on the `git_pat` lane is off by default and needs the `pat`
  scope of `WARDYN_GIT_BROKER_ENFORCE_BRANCH_NS`; `push_rules` applies to
  every `git_pat` push whatever that scope is set to. Neither changes the PAT
  itself: it keeps whatever scope the operator issued it with.
- **A first push to a new branch enumerates the whole new tree.** Under
  branch-namespace confinement the pushed commit's parent stays on the forge,
  so the pack holds nothing to diff against: every root file is named whether
  changed or not, and every untouched directory, symlink or submodule arrives
  as an opaque entry. When the forge is GitHub, a matched entry the pack does
  not carry is compared with the parent commit's trees through GitHub's REST
  API (the run's own credential for the lane, trees only), and a legitimate
  rename, restore or directory move passes — only an add, change, move or
  restore under a denied path is refused. That comparison cannot run on a
  `git_pat` grant to a non-GitHub forge, when no parent counts, or when a
  read fails, times out or needs more than 64 reads — there, any matched
  entry the pack does not carry refuses the push, including every root file.
  So `deny_paths: ["Makefile"]` on a GitLab PAT refuses every push to a repo
  that has a Makefile. See "What the rules see, and what they do not" in
  [docs/POLICIES.md](POLICIES.md#push_rules--pushrulesspec).

### Internal hosts

The proxy's unconditional private/loopback/link-local/metadata/CGNAT/NAT64 IP
guard (`isBlockedIP`/`VetHost`, `internal/egress/proxy/policy.go`) denies a
literal or resolved private-range address *regardless of policy* — the SSRF/
DNS-rebinding defense L2 exists for. That also made an in-cluster service name,
`registry.corp.internal`, or any genuinely internal hostname on RFC1918/CGNAT
space unreachable even when the policy allowlist named it.

`internal_hosts` is a list of `{host_suffix, cidrs}` entries on the same
`SiteConfig` document: each declares a hostname (matched by label suffix —
`host_suffix` itself, or any host ending in `.`+`host_suffix`) whose
resolved/literal address is *lifted* out of the private-IP guard, scoped to
`cidrs` — or, when `cidrs` is empty, to the full RFC1918 + `fc00::/7` +
100.64.0.0/10 liftable range, still bounded by `host_suffix` alone.

**Leave `cidrs` empty — that is the default, and it is the right one.**
`host_suffix` is already the control; a `cidrs` list assembled from what you
can see is usually wrong in a way that looks exactly like a correct
configuration. A laptop's corporate resolver answers a private-endpoint name
into CGNAT space; inside the VPC the SAME name resolves to the interface
endpoint's own ENI address, in the VPC's RFC1918 range. A `cidrs` list drawn
from what the operator's own machine sees excludes the range the SANDBOX
actually resolves into, and the resulting 403 is indistinguishable from never
having declared the entry at all — this cost one deployment two failed runs
before the mismatch was found. Tighten `cidrs` only once you have evidence of
what the sandbox itself resolves, never from what your own machine resolves.
The 403 says the same thing to whoever hits it next:

> Leave `cidrs` empty unless you know the addresses the sandbox resolves. What
> your own machine sees for a private endpoint is usually not what the
> cluster sees.

Loopback, link-local, the metadata address, other reserved ranges, and
NAT64-embedded smuggling stay denied unconditionally regardless of `cidrs` —
no entry can ever lift those. Every declared CIDR (once you have that
evidence) must still lie entirely inside the liftable set; `PUT /site-config`
400s one that doesn't (`0.0.0.0/0`, `169.254.0.0/16`, `127.0.0.0/8` are the
obvious mistakes it catches).

This lifts ONE thing: the SSRF builtin. The run's own policy allowlist
(`allowed_domains`) still has to name the host separately. Two more exclusions
apply automatically: an address on the proxy's own network interfaces, and the
resolved control-plane (`wardynd`) host — the sidecar shares its Docker network
with Postgres/Dex/the registry container. On Kubernetes the sidecar's interface
carries only the pod's own address and the control plane's neighbours are
ClusterIP Services off that interface, so only the resolved `wardynd` address is
excluded there — this IS the case where you, the cluster operator, have real
sandbox-side evidence: use a WORKLOAD-SPECIFIC suffix (one Service's own
name, never the bare `svc.cluster.local`, which would reach every Service in
the cluster) and narrow `cidrs` to that workload's own Pod range — ClusterIPs
come from ONE cluster-wide range, so a Service-range CIDR lifts the whole
cluster, control plane included. The lift
applies wherever the proxy resolves a hostname for a direct dial — the sandbox's
CONNECT/plain-HTTP path, the MITM path, and the `git_pat` PAT-broker lane (its
forge host is grant-derived), so a declared suffix covering a self-hosted forge
lets the brokered PAT reach it. A lifted decision's audit `rule_source` reads
`site-config:internal-host` instead of the default `policy:allowed`.

**Not every `builtin:private-ip` refusal is liftable, and the 403 now says so.**
`internal_hosts` lifts exactly one class — RFC1918/ULA/CGNAT private space. A host
that resolves to a loopback address, to link-local space (including the
`169.254.169.254` metadata address), to multicast or unspecified space, to a
NAT64- or IPv4-compatible-embedded blocked address, or to any other reserved
range is refused **unconditionally**: no `internal_hosts` entry, no
`allowed_domains` entry and no `egress_redirects` target reaches it, and a new
run behaves identically. The refusal's body names the class and prescribes
nothing, because there is nothing in site config to change — an agent resolving
a name into that space is either misconfigured or probing the host's own
metadata service.

**The guard's memory of a refusal lasts one run.** Once a hostname and port
have been refused `builtin:private-ip` for a run, that run keeps refusing it for the
rest of its life even if the name later resolves to a public address — the
remedy is the same one above (declare it under `internal_hosts`), and a fresh
run re-resolves the name from scratch.

```json
{
  "internal_hosts": [
    { "host_suffix": "registry.corp.internal" }
  ]
}
```

Add `cidrs` only for the Kubernetes in-cluster case above, once you know the
ranges the SANDBOX resolves into — and scope the suffix to the one workload,
never to the whole cluster:

```json
{
  "internal_hosts": [
    { "host_suffix": "svc-a.apps.svc.cluster.local", "cidrs": ["10.244.3.0/24"] }
  ]
}
```

(`10.244.3.0/24` stands for that workload's Pod range; a Service-range CIDR
such as `10.96.0.0/12` would lift every ClusterIP in the cluster.)

### Bedrock on a private endpoint

Two topologies, told apart by which hostname the endpoint's TLS certificate
names. Get it wrong and the handshake fails on an SNI/cert mismatch — the SNI
presented to the endpoint is the hostname the sandbox dialled (its own
end-to-end TLS on a `bedrock_sso` run, or the proxy's re-dial on a
bearer-injection run), and the dispatch-layer wiring cannot see a TLS failure.

- **Private DNS enabled — a cert for the *public* host (the common shape).**
  Leave the Bedrock provider's `bedrock.base_url` **unset** (Settings → Model
  providers). The sandbox keeps dialling
  `bedrock-runtime.<region>.amazonaws.com`, so the SNI stays the public host the
  cert names; the estate's private resolver answers that name into 100.64.
  Reach it by listing the public host in `upstream_proxy_no_proxy` (skip the
  corp proxy) and in `internal_hosts` — leave `cidrs` empty (the default: the
  CGNAT range is in the liftable set), or name `100.64.0.0/10` only if you have
  confirmed the sandbox resolves into it (lift the guard). Nothing about the
  private address enters the TLS layer.
- **A cert for the endpoint's own name.** Only when the endpoint's cert
  actually covers its `…vpce.amazonaws.com` name (private DNS disabled, or a
  cert issued for it) set the provider's `bedrock.base_url` to that hostname — then
  SNI and cert agree.

Pointing `bedrock.base_url` at the `vpce` hostname against a public-host
cert is the trap: the sandbox presents the `vpce` name, the endpoint answers
with the public-host cert, the handshake fails. The composed dispatch test
proves the env vars propagate, not that TLS validates.

**The endpoint must not land on a wardyn-proxy sidecar's own subnet.** Every
run's proxy refuses to dial any address on the subnet(s) it is itself attached
to (`onOwnSubnetOrControlPlane`, `internal/egress/proxy/egress_target.go` — a
deliberate SSRF invariant that `internal_hosts` cannot lift, on purpose), and
that covers EVERY subnet the sidecar's own interfaces sit on — both the fixed
control-plane network below and the per-run network it shares with that run's
agent. A PrivateLink endpoint that happens to resolve onto either would have
every model call on the affected run(s) denied there instead, with the SDK
misreading the proxy's denial page as a malformed Bedrock response — one
dispatch at a time, never a clean failure. Nothing checks this at boot: verify
by hand that a PrivateLink endpoint's address falls inside neither the
control-plane network's subnet (`WARDYN_INTERNAL_NETWORK`, `wardyn-internal` by
default), nor Docker's default address pools (`172.17.0.0/16` through
`172.31.0.0/16`, and `192.168.0.0/16` — set `default-address-pools` in that
daemon's `daemon.json` away from the endpoint's range, or use an endpoint
outside them), nor a Kubernetes cluster's pod CIDR.

**The control plane is a second service.** Profile-id and
application-inference-profile models call `bedrock.<region>.amazonaws.com`
(`ListInferenceProfiles`/`GetInferenceProfile`), which the provider's `bedrock.base_url`
deliberately does **not** re-point (a PrivateLink endpoint is per-service). On a
fully-private estate that host also resolves into 100.64 and needs its **own**
endpoint plus the same bypass + lift — list `bedrock.<region>.amazonaws.com`
(or a shared `amazonaws.com` suffix) in both fields too, or a profile-id model
fails on a control-plane call the data-plane override never touches.

**A literal-IP data-plane host** needs a **CIDR** `upstream_proxy_no_proxy`
entry — the suffix form matches hostnames only, and `internal_hosts` never
admits a bare IP (an exact `allowed_domains` entry does, per the redirect
literal-IP note above). Prefer the hostname shape.

**`wardynd`'s own egress is a separate channel.** `upstream_proxy_no_proxy`,
`internal_hosts` and a provider's `bedrock.base_url` govern the **sandbox** proxy;
`wardynd`'s own control-plane calls — OIDC discovery, JWKS, the Entra directory
connector, and `oidc.<region>.amazonaws.com` to renew
a captured AWS SSO session at dispatch — go out over its process HTTP client,
which carries no SSRF guard, so a private (100.64) issuer or Graph host is
dialled directly and boots fine. What that client *does* honour is the
process's own `HTTPS_PROXY`/`NO_PROXY` (the published images do not set them at
runtime): if you run `wardynd` behind the corporate proxy, add the private
issuer/Graph ranges to the process `NO_PROXY`, or the corp proxy — which
cannot reach an internal address — fails discovery at boot, and none of the
site-config fields above can fix it. For a split-horizon issuer (public URL,
internal resolution) use `WARDYN_OIDC_INTERNAL_ISSUER`.

### AWS SSO per person

A `bedrock_sso` model provider (Settings → Model providers) means **each person
signs in to AWS themselves**, and their runs use their own session. There is no
shared AWS session: since 0.8 the agent roster carries no model credential, and
the upgrade converted a `per_user` or `shared` `bedrock_sso` roster row into such
a provider (migration `0100_model_provider_conversion`; see the CHANGELOG).

**How somebody signs in.** They open Getting Started (or the New Run screen) and
choose the provider's sign-in. Wardyn launches a throwaway container-login
sandbox: default-deny egress pinned to the AWS SSO endpoints, a 30-minute idle
cap, no workspace, no repo, no credential mounts, never recorded. The sandbox
prints a device code; they finish the sign-in in their own browser. The route
(`POST /model-providers/{id}/sign-in`, no body) admits anyone who may launch an
agent the provider serves and is granted the provider — admins included, each
capturing their OWN session.

**Launch with a lapsed session.** `POST /runs` refuses a run whose own session
for the chosen provider is missing, or expired with no refresh token that could
renew it, BEFORE any run exists (`422`, with `"reason":"model_credential"`).
Neither Review nor create spends a one-use refresh token; dispatch renews an
expired session whose refresh token still lives, and a renewal AWS refuses fails
the run naming the sign-in. Launch is never pre-checked on the console's cached
status — the server is the gate.

**What the sign-in sandbox is, and what it is not.** It is the AWS CLI and
nothing else: no LLM harness, no repo, no mounts. Its run is labelled `harness
login` server-side, and the run page names it, so opening it from `/runs` is not
a mystery box. Typing `claude` or `codex` in it answers *"This is the AWS
sign-in sandbox, not a coding agent. Start a Claude Code run from New run."* and
exits non-zero, rather than the `command not found` that reads like a broken run.

**The sandbox signs itself in.** ONE chained command does the whole capture —
`aws sso login --sso-session wardyn --no-browser --use-device-code && wardyn-aws-sso`
— and both halves matter: `aws sso login` alone leaves the session in
`~/.aws/sso/cache`, where it dies with the container, and `wardyn-aws-sso` is
what uploads it through the brokered endpoint. Since 0.7.5 the IMAGE runs that
command, not the console: `agent-run --idle` creates a `wardyn` tmux session on
`signin-pane.sh` BEFORE its own workspace prep, the pane prints
*"wardyn: sign-in running — AWS sign-in sandbox. Finish the device-code step in
your browser. Nothing else runs here."*, waits up to five minutes for prep, and
runs the pair at most ONCE. Every attach path joins that one session — Wardyn's
sign-in pane, `wardyn run attach`, an SSH attach, and **the Runs list** — because
attaching is `tmux new-session -A -s wardyn`, attach-or-create. So the path that
used to hand out a bare prompt now shows the sign-in already in progress.
If prep never finishes it runs nothing and prints *"wardyn: workspace
preparation did not finish — stop this run and start a new sign-in."* If the
session could not be started at all (no `tmux` in a derived image), an attach
lands on a plain shell that prints *"AWS sign-in sandbox — nothing else runs
here. The sign-in did not start on its own; run: `<the chained command>`"*.

It runs once on purpose. A retry would mint a SECOND live device code while the
person may still be entering the first, and a refusal that is deterministic (a
wrong-account pin) cannot be fixed by signing in again. When the pair does not
complete, the pane says so and names the command:
*"wardyn: sign-in did not complete — start a new sign-in from Getting Started,
or run: …"*. When it does, it says *"wardyn: sign-in command finished — this
pane is now a plain shell."* and hands the pane over as a shell, with the
scrollback intact for somebody attaching late.

**An unpinned multi-account sign-in asks a question in the pane.** If the
provider pins `sso_account_id` and `sso_role_name`, the sign-in is fully
unattended once the browser step is done. If it does NOT, and the person's SSO
session reaches more than one account (or more than one role in the chosen
account), the helper asks WHICH ONE in the sign-in terminal and allows three
tries. Anyone with a WRITABLE attach can answer — the console's sign-in pane,
`wardyn run attach`, an SSH attach, or the Runs list when they hold the terminal.
A read-only viewer cannot; the prompt itself has no deadline, so it waits until
a writable attach answers or the sandbox's own 30-minute idle cap ends the run.
Pin the account and the role on the provider and the question never comes up. While the sandbox
waits on the answer, the sign-in panel's own copy already narrates the sign-in as done
(`CAPTURE_HANDOFF`) — the browser step finished — so the person reads "click or tab into the
terminal, type the number, press Enter" rather than a claim that Wardyn is still waiting on them
externally.

**Signing in from Getting Started is still the path to prefer** — it watches for
the helper's success marker, corroborates the capture with the server, and shuts
the sandbox down when it lands. The Runs-list path shows the same sign-in; it
just has no console around it — with one difference worth stating: nothing
server-side stops a login run when the capture lands (the shutdown is the
console pane's own kill), so a sandbox opened from `/runs` stays up until the
reaper's 30-minute idle cap. The run page says so.

**Version skew (console newer than the image).** The aws-sso image tag is
version-locked on the ghcr default, but an operator `WARDYN_AGENT_IMAGES` pin —
what a private-registry estate uses — can pair a 0.7.5 console with a pre-0.7.5
image that does not self-run. The sign-in pane covers it: it waits 12 seconds
after attaching and, ONLY if the sandbox has not announced itself (no
`wardyn: sign-in running`, no device URL, no success or refusal marker), types
the chained command itself, exactly as 0.7.4 did. The first thing the new image
prints is that announcement, before its own prep wait, so on a current image the
console never types and a second sign-in is never started over a running one.
If you pin agent images, pull the 0.7.6 aws-sso image at the same upgrade.

**The launch answers before the sandbox is up.** Since 0.7.4 `POST
/model-providers/{id}/sign-in` returns `{run_id, state: "PENDING"}` as soon as the run
row exists and the launch is stamped; the pane then polls the run and attaches
once it is RUNNING. The reason is that a **first start may need to pull the
image**: that pull, plus (on Kubernetes) the network-policy canary, can exceed
the console's own request deadline. It is not specific to an upgrade — a first
install pulls too, and a host that already has the image pulls nothing — which
is why the console's own waiting copy hedges the same way. The synchronous
version answered so late that the console
reported the control plane unreachable over a launch that was working — and
dropped the run id, leaving a sandbox alive to its 30-minute idle cap with
nothing able to name it. Cancel now kills it from the first second. A launch
that fails AFTER that answer fails the RUN (a `FAILED` state and a
`failure_hint` the pane renders), never a silent PENDING.

**The access portal is the admin's, not theirs.** The launch uses the
provider's `sso_start_url`, region and pin; the sign-in route takes no body.
That is a security property, not a convenience: the capture is bound to the
portal the launch was seeded with, so a caller-chosen URL would let anyone bind
their capture to an identity provider and account of their choosing.

**Which AWS account and role a sign-in may capture — pin it.** A person's SSO
session commonly reaches more than one AWS account, and `ListAccounts` returns
them in AWS's order, not yours. Set `sso_account_id` and `sso_role_name` on the
provider, beside `sso_start_url`: **the sign-in proposes, the provider
disposes.** Both together or neither. The pin is enforced at every door, and
each fails CLOSED:

- **Sign-in** — the in-sandbox helper verifies the pin against the SSO portal
  and prints `wardyn: aws sso credential rejected: …` on the login terminal
  rather than uploading. It never falls back to the first account.
- **Capture** — the upload is bound to the provider's values AS THEY READ AT
  LAUNCH (stamped on the run's own `harness.login.start` row), and lands only
  while the provider is still the one it was launched for. A blob that
  disagrees — or, on an unpinned launch, that names an account the provider's
  model ARN does not live in — is refused with 400 and a
  `harness.credential.refuse` audit row carrying a `reason` from a fixed
  vocabulary ([AUDIT-ACTIONS.md](AUDIT-ACTIONS.md)). Refused, never rewritten.
- **Run** — `POST /runs` (422) and dispatch (the run goes FAILED, no sandbox,
  no credential authored) compare the stored session against the provider's
  CURRENT pin and refuse, naming both pairs, when they disagree. Signing in
  again replaces the stored session.

**What an unpinned provider leaves open, stated plainly.** With no pin, the ROLE
is whatever the sign-in chose — and so is the account, unless the provider's
model is a full ARN, which constrains the account only. The pin is the fix; both
halves of it.

**What the control plane holds.** One age-encrypted blob per person per
provider, in that person's own secret namespace under the provider's name — the
SSO access token, its refresh token, the client registration, and the
account/role the session mints role credentials for. Reads never fall back: a
person with no capture of their own resolves as *not signed in*, never as
anyone else's session. Wardyn renews the access token control-plane side at
dispatch while the client registration lives (see "wardynd's own egress"
above), so a one-hour token does not mean an hourly sign-in.

**What it never holds.** Anybody's AWS console password, their MFA, or their
browser session. A sign-in that Wardyn cannot renew surfaces as "sign in again",
never as a run that silently borrows somebody else's credential.

**The admin token is not a person.** With OIDC configured, a sign-in attempted
with the shared `WARDYN_ADMIN_TOKEN` bearer is refused (422): every capture made
with it would land in one namespace and overwrite the last one. A real console
sign-in or a `wdn_` API token, which carries its owner's own identity, is the
way in.

**Blast radius.** A compromised sandbox reaches THAT person's SSO session and
the role credentials it mints, not the organisation's. The
`harness.credential.capture` and `harness.credential.refresh` audit rows carry
`owner` (the capture also names `model_provider`), so "whose credential" is
answerable from the trail, and `harness.credential.refuse` says which captures were turned away and
why.

**Revoking a session.** A person removes their own with
`DELETE /model-providers/{id}/credential`; a security admin erases a named
person's credentials with `DELETE /people/{principal}/credentials`. Revoking the
session at the IdP ends it too: IAM Identity Center is the system of record, so
terminating their Identity Center session (or removing their assignment) stops
the refresh token redeeming and their console reads *sign in again* — the
offboarding step. The session also dies with its OIDC client registration.

### One live sign-in sandbox per person

Starting a sign-in closes that person's previous one. Wardyn kills the caller's other non-terminal
sign-in runs for the same agent before it creates the new run, and the superseded run's `run.kill`
audit row carries `reason = superseded_by_new_login` and `superseded_for = <the person>`; it is a
normal, successful kill, so a `run.kill` with `outcome=failure` still means a teardown or revocation
step failed and the run may not be contained. The key is the run's `created_by`, which is not
always a distinguishable PERSON: on an install with no OIDC, the shared admin-token bearer and a
local-mode caller's principal are one shared credential, so two humans using that credential
supersede each other's sign-ins (`docs/AUDIT-ACTIONS.md`'s `run.kill` row).

Superseding keeps an old browser tab from winning: the sandbox runs `aws sso login` itself, so a
sign-in nobody is watching still completes when the person approves it in an old tab. Its
(legitimate) capture would land after the one they just made, and the console would report *"The
sandbox reported a capture the server does not have"* for a sign-in that had, in fact, worked.

Consequences worth knowing:

- **Retrying is safe, including under a concurrency cap.** The supersede runs before the new run is
  created, so a member whose governance profile allows one concurrent run is ordinarily not refused
  by their own abandoned sign-in — the supersede is best-effort (a lookup error skips it, and a
  losing CAS race is abandoned after three tries), so a concurrency-limited member can rarely still
  meet the cap.
- **A closed sandbox's upload is refused.** A killed sign-in run's credential upload is refused with
  `harness.credential.refuse` / `reason = run_killed`, even inside the five-minute grace a terminal
  run otherwise has for its own tail uploads. Credential revocation alone is best-effort; this is the
  belt.
- **The old sandbox's teardown is detached from the launch request.** The state change is still
  synchronous — the launch POST claims the KILLED transition (and frees the concurrency slot it held)
  before it answers, so the superseded run already reads `KILLED` by the time the caller sees a
  response — but the sandbox teardown, the run identity's revocation and the `run.kill` audit row now
  run on a goroutine detached from the request, up to about 30 seconds per superseded run, and on
  Kubernetes it waits for the pod to actually go away. None of that holds the sign-in POST open: a
  client that gives up (a closed tab, a proxy timeout) has already gotten its answer either way.
  Nothing is lost and nothing is stuck — start the sign-in again.
- **Two sign-ins started at once leave one.** A double-click, or the console and a `wdn_` token
  driving the route for the same person, are serialized by a per-person lock
  (`lockLoginSupersede`) held across both supersede passes and the new run's insert, so the second
  launch sees the first and ends the caller's older sign-in. A lock not taken within about 5 seconds
  answers 503 and starts nothing; the person signs in again. A pass is unserialized
  whenever the pool cannot spare two connections at that moment (see the `WARDYN_PG_DSN` row in
  [ENV.md](ENV.md)): on every call at `pool_max_conns=2`, and transiently on a busy larger pool.
  Each such pass proceeds and writes an `auth.signin_unserialized` audit
  row ([AUDIT-ACTIONS.md](AUDIT-ACTIONS.md#auth)). Unserialized, two launches racing across replicas
  with clock skew can leave both sandboxes alive, and the next sign-in clears it.
- **A sandbox superseded mid-upload does not win.** A capture takes the same per-person lock as a
  launch and re-reads the run's state before it stores, so a capture from a sandbox the person's next
  sign-in replaced is refused (`harness.credential.refuse` / `reason = run_killed`) instead of
  overwriting the newer session. A capture that cannot get the lock within about 5 seconds is refused
  with `reason = signin_busy` and stores nothing. Whoever is watching the old sandbox sees "this
  sign-in sandbox was closed — a newer sign-in for you replaced it…" and finishes in the new one. At
  an unserialized pass (every call at `pool_max_conns=2`) there is no lock, so a supersede landing between the re-read and the write can
  still lose to the old capture; the next sign-in replaces it.

### What the sign-in pane's waiting messages mean

The pane polls the run while the sandbox comes up and says which of four states it is in:

| On screen | What Wardyn knows |
|---|---|
| "Starting the sign-in sandbox…" | Reads are healthy, OR have been failing for under 10 seconds (a blip); the sandbox is not up yet; less than a minute has passed since launch. |
| "Still starting — Wardyn can read the sign-in sandbox, it just isn't up yet…" | Reads are healthy, past a minute since launch, and the run carries no `status_detail` (a pre-0.7.6 daemon, or a Docker warm image with nothing to report). Nothing on this path can *prove* a pull is what it is waiting on, which is why the sentence is hedged. |
| the substrate's own reason (e.g. "Waiting for a machine with room for this sandbox.", "Downloading the image…") | Reads are healthy and the run's `status_detail` names a non-terminal reason ("What a starting run is waiting on", above) — since 0.7.6 this REPLACES the generic "Still starting" hedge; the clock budget is unchanged, only the sentence is more honest. |
| the substrate's own reason, Cancel only, no clock | `status_detail`'s reason is TERMINAL (`ImagePullBackOff`, `CrashLoopBackOff`, …) — the wait ends in seconds, not after five minutes, because trying again gets the same answer until the cluster or the image changes. |
| "Wardyn can't read the sign-in sandbox right now — still trying…" | The console's reads of the run have been failing for at least 10 seconds (a daemon restart, an ingress 5xx, a roster edit that made the read a 403). The sandbox itself may be perfectly fine. |
| "Wardyn stopped being able to read the sign-in sandbox…" | Reads have been failing for at least five minutes AND at least 15 consecutive polls. The wait ends; the run id is kept, so Cancel still tears the sandbox down. |

The wait is graded on BOTH the clock and a poll-count floor, not on poll ticks alone: the clock
(five minutes of failing reads) says the outage is real, and the 15-failure floor — kept from the
old tick budget — says it is not one hidden-tab poll pretending to be one (a backgrounded tab skips
ticks entirely, so a single failed read after ten minutes away must not immediately read as
unreadable). A healthy wait with no reason to report is never ended by the pane, however long the
pull takes; a healthy wait carrying a TERMINAL reason ends on the reason instead — what otherwise
bounds it is the server, below.

### What a starting run is waiting on

A run sits in `STARTING` for the whole of `CreateSandbox`, and there is no sandbox reference until it
returns, so the runner reports what the substrate is doing while it waits: every poll of the proxy pod
and of the agent pod computes one line and, when that line CHANGES, writes it to
`agent_runs.status_detail`. The console renders it on the run header, on the Runs board row, in
the run page's terminal pane while the run is Pending or Starting, and in the sign-in pane (below).

The line is the substrate's own words, in the shape `<component>: <Reason>[: <message>]`:

| line | what it means | does waiting fix it? |
|---|---|---|
| `agent: ContainerCreating` | the kubelet has the pod and is getting a container ready — which includes pulling the image | yes |
| `agent: PodInitializing` | as above, init containers | yes |
| `pod: Unschedulable: <scheduler's message>` | no node will take the pod (a taint, a full cluster, an unbound claim) — read the message | yes, if the cluster changes |
| `pod: Pending` | the pod exists and nothing has claimed it yet | yes |
| `image: Building` | the control plane is building this run's sandbox image (a wrapped base image, a devcontainer, a workspace image), up to 30 minutes. Written only when a build actually starts, never on a cache hit, and carries no image or repo reference. Shown while the run is `PENDING` only | yes, up to the 30-minute build bound |
| `image: Pulling: <ref>` | the image is downloading now. Docker: the host does not have it. Kubernetes (since 0.8, #807): the kubelet's latest Event for the agent container is `Pulling` | yes |
| `agent: ImagePullBackOff: <registry's message>` | the registry refused or the tag does not exist | **no** |
| `agent: ErrImagePull: <registry's message>` | as above, first failure | **no** |
| `agent: InvalidImageName: <message>` | the reference does not parse | **no** |
| `agent: CreateContainerError: <message>` | the image exists; the kubelet would not make a container from it | **no** |
| `agent: CreateContainerConfigError: <message>` | usually a missing Secret or ConfigMap key | **no** |
| `agent: CrashLoopBackOff: <message>` | the container starts and exits, repeatedly | **no** |

The six "no" reasons are terminal: the sign-in pane ends its wait on them in seconds rather than
after five minutes, and offers only Cancel, because trying again gets the same answer until somebody
changes the cluster or the image. They are the list in `internal/runner/waiting.go`
(`TerminalWaitingReasons`), which the Kubernetes poll loops, the control plane's read projection and
the console's mirror all read from.

`image: Building` is the one line the control plane writes itself rather than a substrate reporting
it. The build runs before dispatch, so the line is true only while the run is `PENDING`: the API
shows it there, and blanks it on a `STARTING` run, where a warm Docker start never overwrites it and a
finished build must not narrate the sandbox start. It is written outside the start-wait accounting, so
a long build never appears in `wardyn_run_start_wait_seconds`.

After a daemon restart the last stored line stays on the row. A run that was `PENDING` or `STARTING`
has no `sandbox_ref` yet, and `finalizeUndispatchedRuns` reaps such a run only after
`undispatchedGrace` (twice the 30-minute image-build bound, so about an hour after the restart). Until then a run
whose build or start died with the daemon can still read `image: Building` (or the last substrate wait)
as if it were current. It is not: kill the run and launch it again.

`status_detail` is display-only, never interpreted, and never cleared by a write: the API blanks it
at READ for any run that is not `STARTING` (for `image: Building`, any run that is not
`PENDING`) — except a run that FAILED on one of the terminal reasons, where the reason IS the
failure. The last reason therefore survives on the row for a `SELECT`
postmortem without the console ever narrating a finished run's old wait. A run read from a pre-0.7.6
daemon, or a run that started before this upgrade, simply carries no reason.

### Fleet capacity

`GET /api/v1/admin/runs/capacity` (admin or `security_admin`) returns what the fleet's runs were
configured to reserve, summed from the values each run recorded at dispatch. The response says
`basis: "configured_reservations"`: these are not measurements, and the endpoint never execs into a
sandbox or calls the runner. One query reads the non-terminal rows.

| Row | Counted as holding |
|---|---|
| `STARTING`, `RUNNING`, `WAITING_FOR_CONFIRMATION` with no `lost_at` | yes, paused runs included (a paused sandbox keeps its pods) |
| unschedulable: `STARTING` with the reason `Unschedulable` | yes, and listed under `unschedulable` (at most 20, oldest first, with `unschedulable_total`) |
| kept after an outage, before its end (its agent still runs; only its proxy was stopped) | yes, its agent only (no proxy); also counted as `kept` |
| kept otherwise: after its end, a reboot, or an outage past its end (`lost_at` set, agent stopped) | no; counted as `kept` |
| `PENDING` | no; appears only in `states` |
| terminal | no |

Kubernetes rows sum requests and report limits beside them; Docker rows report caps, and a proxy
with no CPU cap adds nothing to the CPU sum (`proxy_cpu_uncapped` counts them). `by_runner` keeps
the two kinds apart, and `totals` carries the deployment's own kind. A holding row that recorded no
reservation (a run from before it was recorded, a failed best-effort write, or a `STARTING` run
between its create and the dispatch write) is counted in `unknown` and adds to no sum. `by_owner`
lists the top 50 owners by held CPU, with `by_owner_truncated` when more exist.

Residual: a run whose teardown failed after it reached a terminal state is not counted, though its
pods may still exist; `POST /admin/sandboxes/sweep` reaps those.

### The start deadlines

The pane will wait; the **runner** will not wait forever, and these are the bounds an operator sizes:

- `WARDYN_SANDBOX_START_TIMEOUT` (default **3 minutes**) is one absolute deadline for the whole sandbox
  start, counted from the moment the proxy pod is created and spent across BOTH pods: the proxy's
  scheduling, image pull, config-staging init container and Ready, then the agent's pull and Running. It
  is not restarted when the agent pod is created, and a change in the reason a pod is stuck never resets
  it. A first pull of the `aws-sso` image was measured at **131 seconds** on a reporting estate, which is
  most of the default; raise it for a slower registry. Before 0.8.6 this was a fixed 3 minutes for the
  agent plus a separate fixed 90 seconds for the proxy's IP.
- `WARDYN_SANDBOX_CAPACITY_WAIT` (default **15 minutes**) is how long a pod the scheduler cannot place
  for lack of room (`Unschedulable`, for example `Insufficient cpu`) may wait. The run stays `STARTING`
  with "Waiting for a machine with room for this sandbox." and the poll backs off to every few seconds.
  That time is counted apart: it spends the capacity wait, not the start timeout, so a run that waited ten
  minutes for room still has its full start budget to pull an image. The longest a run can sit in
  `STARTING` is therefore the two added together (18 minutes by default). `0` turns the wait off: an
  unplaceable run fails at the start timeout, which is what 0.8.5 did (at 90 seconds for the proxy).
- Terminal states never wait: `ImagePullBackOff`, `CrashLoopBackOff`, a failed init and the like fail the
  run at once, inside a capacity wait as anywhere else.
- The **boot egress canary** keeps its own fixed budget (`canaryWaitTimeout`, 3 minutes a phase,
  `internal/runner/k8s/canary.go`) and neither setting moves it: both phases must fit inside the chart's
  450 second startup probe.
- While a run waits, wardynd holds the run's watcher lease with a heartbeat for as long as the sandbox
  create blocks, so another replica's sweep does not adopt a run that is still being set up.
- `GET /api/v1/setup/status` reports both values (`runner.sandbox_start`, Kubernetes only) and the
  checklist shows them as the `sandbox_start` row. The console's "taking longer than expected" bound for a
  starting run is the two added together plus 90 seconds, read from there.

**Upgrading:** a run that cannot be placed now waits up to 15 minutes where 0.8.5 failed it at 90 seconds.
Set `WARDYN_SANDBOX_CAPACITY_WAIT` to `0` to keep failing fast.

A pull slower than the start timeout fails the run honestly: the run carries a `failure_hint` naming the
deadline and the pod's Pending state, and the pane shows that sentence rather than a guess.

**A first pull after an upgrade does not fail a run.** Every image tag changes at a version bump, so
the first start on each node after an upgrade re-pulls; that is a two-minute wait, not a fault. The
Docker substrate asserts a download outright, because `ensureImage` has just checked and the host does
not have the image. Since 0.8 (#807) Kubernetes does too, while the kubelet's `Pulling` Event is the
latest thing it has said about the agent container (below). Without that Event, or with the Events read
refused, all Wardyn has is `ContainerCreating`, which the kubelet reports for a pull and for everything
else it does before a container runs, so the console says a first start *can* take a couple of minutes
while the image downloads.

**The one chart change, and why.** Every other reason comes from `pods: get`, which the chart already
grants. `Pulling` does not: on Kubernetes it lives only in the pod's Events. 0.8 (#807) grants
`events: list` in the namespaced k8s-runner Role: `list` only (no `get`, no `watch`), never in the
ClusterRole, and `make helm-lint` fails a render that widens either. The runner lists at most once a
second, only while the agent container is `ContainerCreating`, field-selected on the pod's kind, name
and UID. Under `k8s.allowRunsInReleaseNamespace=true` that verb reads every co-tenant workload's Events
in that namespace: a `fieldSelector` is a client convenience, not something RBAC can enforce. That is
accepted as part of the same namespace blast radius the render-time fail in
`deploy/helm/wardyn/templates/rbac.yaml` already names for that setting (exec into and delete any pod
there, create or delete any Secret there). Nothing an Event says is surfaced: the image ref comes from
the pod spec, and the Event only chooses between two sentences Wardyn wrote. If you write the Role
yourself (`k8s.rbac.create=false`), add `events: list` on upgrade. Without it the read fails closed:
it is switched off for the rest of that start, the create goes on, and a pull reads as
`agent: ContainerCreating`, as before.

**The fix for a slow registry is to pre-pull, not to wait longer.** Get the agent and `aws-sso`
images onto every node at upgrade time — a DaemonSet that pulls the new tags, or the node cache of
whatever registry mirror the cluster uses. Then the first sign-in after an upgrade is a warm start.

### Telling an estate-side failure from a Wardyn one

If people report *"Wardyn stopped being able to read the sign-in sandbox"*, the reads were failing,
and the cause is almost always in front of Wardyn (an ingress/WAF 5xx or 429 against a 2-second poll,
or a `wardynd` pod rolling during the upgrade that triggered the pull). Run both of these while it is
happening — one shows what the console's poll sees, the other what the sandbox is actually doing:

```sh
# What the pane's poll sees. Same route, same cadence. Watch the status codes.
while :; do
  curl -s -o /dev/null -w '%{http_code} %{time_total}s\n' \
    -H "Cookie: $WARDYN_SESSION" "$WARDYN_URL/api/v1/runs/$RUN_ID"
  sleep 2
done

# What the sandbox is doing, on the cluster side.
kubectl -n "$WARDYN_NS" get pods -l wardyn.run-id="$RUN_ID" -w
kubectl -n "$WARDYN_NS" describe pod -l wardyn.run-id="$RUN_ID" | sed -n '/Events/,$p'
```

A steady stream of `200`s with a Pending pod is a slow pull (pre-pull, above). A stream of `502`/`503`/
`429`, or `200`s that take tens of seconds, is the ingress or the daemon — no console change fixes
that. `describe pod` is also where `ImagePullBackOff`/`ErrImagePull` shows up, which the runner treats
as terminal and reports on the run.

### Testing AWS SSO without an AWS tenant

Everything above is unfalsifiable without an AWS tenant — which is why, before
0.7.4, "a member signs in and their run gets THEIR OWN credentials" was tested
nowhere. It is testable now, on a throwaway kind cluster, with no AWS account
and no real credential anywhere in the loop.

**The cluster.** `make agent-images` (the overlay loads this tree's
`wardyn/agent-aws-sso:local` login image and refuses to start without it), then
`WARDYN_QUICKSTART_HTTP_PORT=8280 WARDYN_QUICKSTART_SSH_PORT=2322
make kind-quickstart`, then `make kind-sso` (see `deploy/kind/sso/README.md`).
The overlay adds Dex with one static principal per role path —
`admin@wardyn.local`, `member@wardyn.local` and four more, password `password` — plus
`wardyn-awsssofake`: an unsigned fake of both AWS IAM Identity Center services
(`sso-oidc` and the `sso` portal) on one in-cluster Service, and a
bedrock-runtime stub on a second (`wardyn-awsssofake-bedrock`, port 8091), so a
model call takes the SigV4 passthrough real Bedrock gets rather than the portal's
terminated tunnel. `make kind-sso-down` removes the overlay; the cluster itself
belongs to `make kind-down`.

**The knobs.** `WARDYN_AWS_SSO_ENDPOINT_OVERRIDE=<url>` re-points both SSO
services at that Service, moving five things together: the containerized login
sandbox's `AWS_ENDPOINT_URL_SSO`/`_SSO_OIDC`, the captured-credential sandbox's
same pair, the SSO egress allow-list entries, the login flow's own
`device.sso.<region>` entry, and the dispatch-time `CreateToken` URL. It is
**refused unless `WARDYN_ALLOW_TEST_ENDPOINTS=true`** is also set, and every
boot carrying it logs a warning opening `TEST HATCH ACTIVE`. Unset — every real
deployment — nothing changes. It is not a Bedrock provider's `bedrock.base_url` (a different
service, and a supported production posture), and it is not the global
`AWS_ENDPOINT_URL`. Read [THREAT-MODEL.md](../threatmodel/THREAT-MODEL.md)
residual #45 before setting either var anywhere that holds a real credential:
an operator who sets both has pointed a real sign-in at a server that can hand
back credentials of its choosing, and Wardyn cannot tell that server from AWS.

The same acknowledgement unlocks one more thing: a plain `http://`
`bedrock.base_url` on a model provider. This is the SigV4 passthrough
lane of a `bedrock_sso` provider — no per-run TLS-MITM terminates for it — so
without the acknowledgement the provider save is refused on rule 1 (`must be
https://`) before the SSO hatch is even reached. That relaxation is rule 1 and nothing else: an embedded
credential, an empty host, a metadata literal, the public host itself, a query
or a fragment all still refuse the save exactly as they do in production. The
walk itself no longer needs it: it serves the fake over HTTPS under a throwaway
CA it mints each run and installs
as the chart's `trustedCA`, so wardynd, every run's proxy and every sandbox
trust the fake the way they would trust a corporate CA.

**Reaching the fake from a sandbox — the step that fails first if you skip it.**
The fake is addressed by its **Service** name, never a pod IP, and site-config
`internal_hosts` must lift it **before the first sign-in**. The sandbox's egress
goes through the proxy sidecar, which denies any host resolving to a private
address unless an `internal_hosts` rule lifts it — and the lift refuses the
proxy's own interface subnets. A pod IP is on the pod CIDR, which IS that
subnet, so a pod IP can never be lifted however it is declared; a ClusterIP is
on the Service CIDR, which can. Scope the rule to the Service CIDR your cluster
actually uses (read it off the apiserver's `--service-cluster-ip-range`, do not
assume `10.96.0.0/16`; `scripts/kind-sso-walk.sh` reads it off the apiserver
itself; a set `WARDYN_KIND_SSO_SERVICE_CIDR` wins over that read, which is also
how you rescue a failed one).
The same entry covers the Bedrock stub, because it is the same Service.

**The walk.** `WARDYN_TEST_K8S=1 scripts/kind-sso-walk.sh` does all of the
above and then drives `ui/e2e/walk/sso-member.spec.ts`. **It resets the cluster
first**: it restarts the quickstart's Postgres, which has no volume, so every
run, workspace, secret and captured session on that cluster is gone — this is a
throwaway cluster by design, never one you keep state on. Then both principals sign in
through Dex, the admin declares the `per_user` lane and pins the account/role,
the member completes the containerized `aws sso login` from their own seat, and
`/setup/status` reads `model_access.state: "live"` for the member and
`not_configured` for the admin at the same moment. The closing assertion is the
one that is not Wardyn asserting about itself: the fake's own `/_seen` reports
which account and role real botocore asked it to mint, and that the Bedrock stub
was hit.

**What the second spec adds (0.7.5).** The walk now runs TWO spec files in one
invocation and against one cluster —
`./scripts/run-ui-e2e.sh sso-member sso-member-recovery` — in that order,
because the second inherits the state the first leaves: a member who is
already signed in, and a roster pin that already contradicts nothing. It
covers the paths a member who is already `live` cannot reach from their own
seat, and three things 0.7.4's walk did not touch at all:

- **the org standard set in the CONSOLE, not by an API PUT.** An admin drives
  the Agents tab once — mechanism, per-person credential source, and the three
  org settings the row carries (SSO start URL, pinned account, pinned role) —
  and the walk then proves a MEMBER is bound by all of it: New Run offers only
  the enabled agent, and the member's own sign-in pane has no start-URL field
  at all, because the organization's portal wins. That last one is the proof
  an org setting is ENFORCED on a member rather than merely saved.
- **the sign-in sandbox signing itself in, reached from the RUNS LIST.** The
  member presses the CTA and immediately leaves the console. Opening the run
  from `/runs` joins the same tmux session the image already started, shows
  the sandbox's own banner and the device-code URL, and the capture completes
  with the test typing nothing at all. The absence of a keystroke is the
  assertion: before 0.7.5 the console typed the command and a Runs-list attach
  got a bare prompt.
- **cancel, retry and supersede.** A sign-in cancelled while it is still
  starting leaves no stored credential, the retry replaces the blob (proven by
  the stored capture's `source_run_id` MOVING to the second run — "it reaches
  live" proves nothing about a member who was already live), and starting a
  third sign-in over an abandoned one kills the orphan — the ordinary case
  leaves exactly one live sandbox per person; a rare timestamp race across
  replicas can leave two (see "One live sign-in sandbox per person" below).

It also holds a sign-in in `STARTING` for 65 seconds on purpose — by tainting
the kind node so nothing the run needs can schedule — and asserts that the
console says the start is SLOW, and never that Wardyn cannot read the sandbox,
with the pane's own 2-second poll answering 200 throughout. That is the live
twin of the Go characterization test, and it is the datum that tells an
operator whether an "unreadable" they saw was estate-side.

The 65 seconds are not arbitrary and neither is what they hold up. A sandbox
creates its PROXY pod first and waits for that pod's IP for **90 seconds**
before the agent pod exists at all, so under a taint it is the proxy pod that
sits `Pending`, and 90 seconds is the point at which the run itself fails.
The hold is therefore five seconds past the 60 at which the slow-start
sentence appears, and the taint comes off the moment the assertion is made —
about 25 seconds of margin. A walk that is killed mid-case would leave the
node unschedulable, so the walk script clears that taint before its own
restarts and again on exit.

**Run it on images built from the tip you are judging.** The console is baked
into the daemon image, so a cluster loaded before a console change judges the
OLD screens with the NEW assertions — which is exactly how one 0.7.4 walk went
red with a correct tree. `WARDYN_KIND_SSO_REBUILD=1 WARDYN_TEST_K8S=1
scripts/kind-sso-walk.sh` rebuilds all five images the walk judges (`wardynd`,
`wardyn-proxy`, `agent-aws-sso`, `agent-claude-code`, the SSO fake) from the
working tree and reloads them first. It retags the two `:local` agent images
on that Docker daemon — a compose stack sharing that daemon adopts them for
new runs, which the walk warns about once. Either way the walk writes an
`images.txt` into its evidence directory naming the tree's HEAD and each
image's content digest and build time (and, since 0.7.6, a `MANIFEST.json` with the host and node
digests compared per image, the dirty paths, the fake's TTL knobs and the kill-switch posture read
back off the Deployment), so "which tip did this prove?" is
answerable afterwards rather than remembered.

**It is still a manual proof, not a CI job** — no workflow runs it, so a green
result is evidence only for the tip somebody actually ran it on.

**And here is what a green walk still does NOT prove.** Naming these is the
point of the walk, not a caveat on it:

- **No real AWS tenant.** Every SSO and Bedrock endpoint is an unsigned fake
  on the cluster. It confirms which account and role real botocore asked it to
  mint; it cannot confirm that AWS would have minted them, that the role's
  policy permits Bedrock, or that a real IAM Identity Center behaves as this
  fake does at any edge. The real-tenant walk is a separate, owner-gated
  exercise.
- **No genuinely cold image pull.** Images reach the node by `kind load` and
  the sandbox pod's pull policy is `IfNotPresent`, so nothing is ever fetched
  from a registry on any walk. The hold above is manufactured with a node
  taint, which reproduces a pod that cannot SCHEDULE — not the network
  conditions of a slow registry, and not an `ImagePullBackOff`, which is
  terminal rather than slow. What it does prove is the half an operator
  actually asked about: that a start which is merely slow is narrated as slow
  and never as unreadable.
- **The device-code URL is a bare path.** The on-cluster fake is served by a
  handler with no public base URL of its own, so it answers
  `/verify?user_code=…` and the sandbox prints that. Nothing here exercises a
  real portal's absolute URL, or a human opening one.
- **No real IdP.** Dex with two static passwords stands in for the estate's
  Entra tenant, so group-to-role mapping, conditional access and token
  lifetimes are all out of scope here.
- **One person at a time.** Both principals are driven serially by one
  browser; nothing here exercises two members signing in at once, or a member
  signing in while another's run is dispatching.
- **The device-code step is pre-approved.** The fake approves every device
  code permanently, so the walk never exercises a human being slow, a code
  expiring before anyone attaches, or a browser leg that fails.

### Where people are told about model access

From 0.7.6 an actionable model-access state reaches a person on every console screen, not only on
Getting Started. The console reads `model_access` from `GET /setup/status` once per session and then
every five minutes (a hidden tab skips its ticks and a returning one refreshes at once), and renders a
banner for the three states a person can act on — `not_configured`, `expired_signin`, `expiring` —
plus `shared_expired`, which a member cannot. The banner carries the sign-in itself: the same AWS SSO
login pane Settings mounts, in a dialog, on whatever screen they were on.

Two suppressions are deliberate. On Getting Started the page IS the door. On Settings and the
Providers screen the banner is withheld for an ADMIN only, because those pages already mount the same
pane for the same states; a member keeps the banner there, because the Settings card's AWS button is
admin-only and would otherwise strand them on the page they were sent to.

`not_configured` (the first-run state) and, for a non-admin, `shared_expired` carry a "Not now" that
hides the banner for that person in that browser session. A lapse — `expired_signin` or `expiring` —
cannot be dismissed.

Under a `shared` row the one credential is the admin's, and a member whose runs depend on it is told
when it dies ("Your admin's model credential expired — ask them to reconnect it") with no button,
because nobody but an admin can repair it. The ADMIN reading the same state sees the blast radius
named — "The shared AWS sign-in no longer works — every Claude Code run needs it" — and gets the
sign-in, which is the path `authorizeHarnessLogin` has always admitted for an operator. Wardyn has no
way for that member to notify the admin; that gap is listed in the CHANGELOG.

### A run refused for a model credential

When an `AgentProviders` row says HOW an agent reaches its model and the credential that lane needs is
dead at dispatch, the run is failed with the server's own sentence — and from 0.7.6 that refusal is
also machine-readable: the `run.create` failure row it already wrote carries `reason:
model_credential` and the run's DECLARED `mechanism`. The console grades that ending `credential` and
renders the sentence with the AWS sign-in beside it, in a dialog, on the run page.

The button is offered only where a sign-in the reader can complete would repair the state: the
refused run's declared lane must still be the deployment's Claude Code lane, the reader's own
`model_access` must be actionable, and the reader must be the person who created the run — an admin
reading somebody else's failed run is shown the sentence alone, because their sign-in repairs nothing
for that run. A refusal whose renewal merely did not complete ("launch again in a moment") grades
`live` and gets no button either, correctly: nothing is wrong with that credential. In the Admin view
(`/admin/runs/:id`) the button never renders, even on the admin's own run; the admin's own per-user
run carries **Open in user view** in its place, since the Admin monitor carries no credential door of
its own (M-7).

Signing in from there does not restart anything. The run stays FAILED; relaunch is the run header's
"Start a run like this one".

**The refusal sentences' destination.** The refusals that name a destination — the dispatch refusal,
its create-time 422 twin, the stored-AWS-identity refusal and the spent-renewal sentence — now name a
door the reader can open. Under a `per_user` row that is the console's Getting started page or the
model-access banner every screen carries; under a `shared` row it stays Settings → Model provider,
which is the admin's own page. The CLI prints the same sentence, and Getting started is the
destination that is true for its reader too.

### A run is holding for a sign-in

A run on the captured-AWS-SSO Bedrock lane can have its credential lapse **while it is working**.
Before 0.7.6 that was terminal: the agent's next model call failed and a mid-task context was lost.
Now the proxy **parks** the sandbox's next credential exchange while the credential's owner signs in
again.

**What the operator sees.**

- The run stays RUNNING. Its header chip reads *Waiting for your AWS sign-in* (plus a count when
  something else is pending too), and the cockpit's approvals strip carries one row, *AWS sign-in
  needed*, whose single button opens the sign-in dialog. There is no Approve and no Deny: the request
  is answered by signing in, and the API refuses a decision on it with `409` — to the security tier
  and to the run's own owner or an admin. A caller who does not own the run gets the same
  `404 approval not found` every other kind gives them, byte for byte, so the refusal cannot be
  used to ask whether a UUID is somebody else's sign-in request.
- The audit trail carries `credential.reauth.request` at the raise — with `owner`,
  `credential_source` and a `reason` from a closed set (`spent` the refresh token is gone at AWS,
  `unavailable` renewal failed transiently with nothing left to serve, `not_found` there is no stored
  session for that namespace) — and `credential.reauth.resolve` when a sign-in answers it, naming
  `resolved_by` and the `capture_run_id` it landed from.
- `/metrics` carries `wardyn_credential_reauth_total{outcome=requested|resolved|expired|cancelled|timeout}`
  and `wardyn_credential_reauth_wait_seconds` (sum/count — the average time a request stayed open).
  **Each label is counted at its own transition**: `requested` at the raise, `resolved` at the sign-in
  that answered it, `expired` where the 24 h sweeper ages a row out, `cancelled` where a terminal run
  cancels one, `timeout` where the daemon ingests the sidecar's `credential:reauth-timeout` decision.
  That decision row is written for a spent BUDGET and nothing else: a hold ended by a proxy
  shutdown, by a killed run (which leaves its own `approval.cancel` row) or by a request
  answered with anything but an approval refuses the sandbox with the same modelled 401 but is
  neither counted nor logged as a timeout, so `timeout` always means "the owner had the whole
  window".
- **`timeout` is the label to alert on**, and the one to tune `WARDYN_CREDENTIAL_REAUTH_TIMEOUT`
  against: it means a sandbox's model call was FAILED because nobody signed in inside the budget. A
  rising `timeout` beside a `wait_seconds` average near the budget says people are only just making
  it — lengthen the budget, or make the request more visible. A rising `timeout` with a LOW
  `wait_seconds` says the opposite: the agent's SDK is giving up before the hold does, and the knob
  should come DOWN below that SDK's own patience so the call fails fast instead of late.
- A series that is mostly `expired` is a deployment whose people never see the request at all — check
  that the console is reachable and that the roster names real principals. Mostly `cancelled` means
  the runs are ending (killed, or finishing) before anyone answers.
- **A hold expiry is deliberately NOT counted on `wardyn_egress_denies_total`.** Policy allowed the
  host and allowed the request; what ran out was a person's time, and that series is the one whose
  HELP promises "denial by policy" and which operators page on.

**What the operator can change.** `WARDYN_CREDENTIAL_REAUTH_TIMEOUT` (proxy sidecar, default `600s`,
clamped `[10s, 1800s]`) is how long ONE hold waits. Lower it if your agent's SDK gives up before the
hold does — on expiry the call fails with the AWS `UnauthorizedException` it would have got anyway.
Both container runners forward it from wardynd's environment into every proxy sidecar, and the
compose stack forwards it from the operator's shell into wardynd. A docker-gated measurement against
the reference agent's own SDK (`wardyn/agent-claude-code`) found it still waiting on a parked
credential exchange at eleven minutes — the test's own ceiling, not the SDK's — having made 28
`GetRoleCredentials` calls in that window, roughly every 30 s. So the 600 s default is the binding
constraint, not that SDK; a less patient SDK is what the "lower it" advice above is for.

**The PENDING row outlives the hold, deliberately.** When the budget ends, the model call fails and
the row stays PENDING — the sign-in is still wanted, and the next run needs it too. So a PENDING
`credential_reauth` row is evidence that a sign-in was **asked for**; it is not evidence that a
request is still parked. The 24-hour approval sweeper (`WARDYN_APPROVAL_EXPIRY_AFTER`) or the run's
own terminal cascade closes it. Read the pair of audit rows, or the `credential:reauth-timeout`
decision row, to tell the three apart.

**Bounds.** One hold per REQUEST, however many of the sandbox's concurrent calls discover the lapse —
the hold belongs to the request rather than to whichever call opened it, so a client that gives up
does not end it and a client that arrives later joins it instead of starting a second one;
at most eight such workflows per run, after which the run is refused rather than asked again. A hold
ends within one poll of a 401/403/410 on the approval read (which is what a killed run answers before
its CANCELLED row is readable), after three consecutive 404s, and at its budget — never later.

### Turning the lane off

`WARDYN_AWS_SSO_PROXY_INJECT=off` restores the pre-0.7.6 behaviour: the SSO access token is written
into the sandbox's token cache, `portal.sso` is not TLS-MITM'd, no injection grant is authored, and a
lapsed session fails the run's model call as it used to. It is also the sanctioned stopgap for the
corporate-proxy interaction described in ["Phase B: the SSO/Bedrock MITM lane and the upstream
proxy"](#phase-b-the-ssobedrock-mitm-lane-and-the-upstream-proxy) above — reachable now as a named
Helm value and a Compose env line, not only through the raw env passthrough.

It applies to **new dispatches only**. The placeholder cache, the injection grant and the MITM entry
are all authored at dispatch, so a run that is already running keeps the lane it was authored with
until it ends — including a run that is currently HELD, which keeps holding to its budget and can
still be resolved by a sign-in. After flipping the switch, relaunch the runs that matter or wait them
out; do not expect a running sandbox to change lane under you. The default is `on` — set the
environment variable to `off` to roll back; a run already dispatched under `on` is unaffected by a
later flip either direction.

**A downgrade to 0.7.5 with `credential_reauth` rows present is UNSUPPORTED.** Migration `0064` is
additive (it widens a CHECK), so the upgrade needs nothing; the old CHECK would refuse the rows on
the way back. The upgrade runbook's `pg_dump` is the rollback.

### Version combinations

`claimSingleInstance` excludes a second daemon (outside HA mode). It does not exclude an old sidecar image, an old
browser bundle or an old CLI, so:

| combination | behaviour |
|---|---|
| 0.7.5 proxy sidecar, 0.7.6 daemon | No hold. The 423 is an unrecognised status, the re-resolve fails closed, and the run's model call fails as it did in 0.7.5. The approval row is still raised and still visible. |
| 0.7.6 proxy sidecar, 0.7.5 daemon | No 423 is ever answered, so the hold never opens. Byte-identical to 0.7.5. |
| 0.7.5 console, 0.7.6 daemon | The row renders through `WIRE_TO_COPY`'s fallback (the raw kind string in the chip) and the screen does not crash; the Approve/Deny pair is offered and the server answers 409. Tell people on an old bundle to reload. |
| 0.7.5 CLI reading a `credential_reauth` row | The kind is a plain string on the wire; the 0.7.5 `approvals list` prints it verbatim. |

### Internal model gateway

Point a provider's model calls at an internal endpoint instead of
`api.anthropic.com`/`api.openai.com`: an Anthropic or OpenAI provider's
`base_url` (Settings → Model providers) re-points the proxy's own brokered
`/wardyn/llm/anthropic` / `/wardyn/llm/openai` route at the gateway, and a
`custom_endpoint` provider is addressed by its `base_url` alone. A Claude
subscription provider's `base_url` re-points the sign-in token too: every person
who connects to it has their own Claude OAuth token sent to that gateway on
every model call, so setting it is a trust decision for the people connecting.
The address is
validated when the provider is saved (`https://` only, RFC1918/CGNAT literal
allowed, loopback/link-local/metadata/multicast/NAT64 refused, must not equal
the public host) and forwarded to the proxy sidecar per run. No
`SiteConfig.InternalHosts` declaration is needed for the gateway itself **on that
brokered route**: only the proxy's own `/wardyn/llm/*` handler resolves and dials
it, per request, with its own refusal for the same disallowed address kinds
(`Proxy.vetTrustedHost`, reached only via `Proxy.gatewayTarget`). That relaxed vet
is scoped to the brokered route alone — a sandbox naming the gateway host on an
ordinary CONNECT/plain-HTTP request is treated like any other host: policy
(`allowed_domains`) plus the unconditional private-IP guard apply unchanged, so a
private-address gateway named by hostname stays unreachable that way without its
own `SiteConfig.InternalHosts` declaration. Prefer a hostname gateway: one
configured by IP literal must be listed by that literal in `allowed_domains`, and
an exact literal-IP allowlist entry is honoured before the private-IP guard
(`Policy.AllowsLiteralIP`) — the gateway box then becomes reachable from the
sandbox on every port over a plain CONNECT (no credential rides that path;
injection happens only on the brokered route).

The operator MUST add the gateway host (exact) to the policy's `allowed_domains`
— the credential grant the proxy injects still needs an exact egress-allowlist
entry, exactly as the public host does. **Behind a corporate
upstream proxy, the gateway must be reachable FROM that upstream** — with
`upstream_proxy_url`/`upstream_proxy_secret_ref` also configured, every forward
dial (the gateway included) is CONNECTed through the corp proxy by the transport,
never dialled directly. A gateway the corp proxy cannot reach — an internal one,
typically — is what `upstream_proxy_no_proxy` is for: list its host there and the
gateway is dialled directly instead, then admitted by `internal_hosts` like any
other internal address.

Two invariants carry over unchanged: the `egress_redirects` lane above still
points the AGENT'S OWN configuration (its `ANTHROPIC_BASE_URL`/`OPENAI_BASE_URL`
env, or an equivalent harness setting) at a gateway independently of Wardyn's
injection lane; and an `egress_redirects` row can never be pointed *at* the
gateway or the public provider host to swap an artifact-registry token onto model
traffic — `planArtifactRedirect` refuses that redirect outright (audited
`run.artifact.redirect`, `warn`) rather than letting `buildInjector`'s
last-write-wins host map silently collide the two.

### Upgrading from `artifact_overrides`

A site-config document saved before this shipped used `artifact_overrides:
{"npm": {"base_url": "..."}, ...}`. Migration `0030` rewrites the one stored row
automatically on the first boot after upgrade — nothing to do for what's already
in Postgres.

`PUT /site-config` (and so `wardyn site-config set`) still accepts a legacy
`artifact_overrides` body **for one release**, folding it into `egress_redirects`
before validating: `set` replaces the *whole* document, so an operator
re-applying a file saved before this release would otherwise silently wipe the
proxy and every redirect rather than just fail to update them. A body that sets
both fields is rejected (400) rather than guessed at. `wardyn site-config get`
after upgrading no longer returns `artifact_overrides` — re-save the file then.

A PUT that does not MENTION a field added after v0.6.6 (`internal_hosts`,
`upstream_proxy_no_proxy`) leaves the stored value alone rather than clearing
it, so an older SDK/CLI's get-edit-apply round trip can no longer erase a
field its struct has no name for
(`carryForwardUnnamedSiteConfigFields`, `internal/api/site_config.go`). To
clear one deliberately, send it explicitly as `[]` or `null` — the body
naming a field is what decides it.

### Integrations are not part of this round-trip

`integrations` — the rows behind **Settings**' Model provider card and behind the
git credential lanes on the Workspace Providers screen (which is where the
retired Git host card's lanes moved in 0.7.2), plus generic rows stored under an
earlier release — lives on the SAME `SiteConfig`
document `GET`/`PUT /site-config` reads and writes, but does not travel through
this door. `PUT /site-config` 400s outright on a body carrying a non-empty
`integrations` ("integrations are managed through their own endpoints, not PUT
/site-config") and always carries the STORED integrations forward onto whatever it
persists, regardless of what the body sent (`handlePutSiteConfig`,
`internal/api/site_config.go`). Same whole-document-replace reason as above: an
older client that `get`s a config saved before `integrations` existed, then
`set`s it back unmodified, would otherwise silently delete every stored
integration.

The practical edge: once any integrations are stored, a fresh `wardyn site-config
get > corp-baseline.json` captures them too, and the client strips them back out
on the way in (`PutSiteConfig`, `pkg/client/families.go`) so the `set` half does
not 400 on its own capture. That strip also means **`set` never restores an
integration** — the ones in the file are dropped, the stored ones carried forward
untouched. `wardyn site-config set` prints a warning naming how many it dropped.
Manage integrations through their own routes (`GET /api/v1/integrations`,
`PUT`/`DELETE /api/v1/integrations/{id}`).

`set` also decodes the file strictly (`DisallowUnknownFields`, the same
validator the server runs): on a whole-document replace a typo'd key would leave
the real setting out of the body and delete it, so a misspelled field fails on the
host, before anything is sent.

**Optional `If-Match`.** `GET /site-config` returns an `ETag` (a content hash of
the document); a `PUT` carrying it back as `If-Match` is refused `412` if the
document changed underneath — two admins editing the same config, or a stale
`corp-baseline.json` applied after someone else's `PUT` landed. Omitting
`If-Match` works exactly as before, and `wardyn site-config set` today sends
none. On `412`, re-`GET`, re-apply the change on top, retry.

### Testing it: two probes, not a courtesy button

Wardyn otherwise has no test-connection buttons: it cannot dial a stored
credential, so a green tick would mean "we wrote it down" while reading as "we
checked". `POST /api/v1/site-config/test-proxy` and `POST
/api/v1/site-config/test-redirect` are the deliberate exception, on the same
footing as the GitHub ref-ruleset check ([TRY-IT.md](TRY-IT.md)): the check is
real. Each launches a throwaway, one-shot confined sandbox, makes an actual
outbound request through it — the same path a real run's egress takes — and tears
it down. Both are **admin-only** (a member 403s) and **audited**
(`site_config.proxy.test` / `site_config.redirect.test`); the audit row carries
the host(s) probed and the outcome, never the proxy URL, which may legitimately
carry a credential.

```sh
curl -s -X POST http://localhost:8080/api/v1/site-config/test-proxy \
  -H "Authorization: Bearer $WARDYN_ADMIN_TOKEN"

curl -s -X POST http://localhost:8080/api/v1/site-config/test-redirect \
  -H "Authorization: Bearer $WARDYN_ADMIN_TOKEN" \
  -H 'Content-Type: application/json' \
  -d '{"from":"https://registry.npmjs.org/"}'
```

`test-proxy` needs no body and runs whether or not an upstream proxy is
configured — with one it proves the chain works, without one it proves direct
egress works, and it says which path it took. It goes out through the sandbox's
normal egress, which dispatch already chains to the configured upstream. It
dispatches the published `agent-base` image (a plain curl task) at the STRONGEST
confinement class this host's runner actually advertises — never the operator's
configured floor, because the question is whether egress works, not whether the
floor is enforceable (an admin floor above what this host's runner advertises —
CC2 with no RuntimeClass registered, say — otherwise fails the probe before it
reaches the network, reading as a proxy problem it is not; see
`not_run` below and the setup checklist's confinement-floor warning row).

It also accepts an optional `{"url": "https://…"}`:

```sh
curl -s -X POST http://localhost:8080/api/v1/site-config/test-proxy \
  -H "Authorization: Bearer $WARDYN_ADMIN_TOKEN" \
  -H 'Content-Type: application/json' \
  -d '{"url":"https://intranet.corp.internal/health"}'
```

That is for hosts with no public internet — internal-only or air-gapped
deployments, where none of the default targets will ever answer. A custom target
proves strictly less, and the response says so: Wardyn cannot match a known
payload, so it only reports that the request completed. The URL is validated the
way any stored site-config URL is (http(s) only, a real host, no shell
metacharacters) and rejected before a sandbox launches; it is not stored and
changes nothing about later runs.

Two details of *how* it decides, both of which change the answer on a corporate
network:

- **It does not use github.com.** Plenty of organisations block GitHub outright,
  and a false "no internet" from a working network is worse than no check at all.
  The targets are the endpoints Windows (NCSI) and Firefox use for their own
  connectivity detection. Several are tried; one blocked endpoint does not fail
  the probe.
- **It checks the response body, not just the exit code.** A corporate block page
  or captive portal is a perfectly well-formed HTTP 200, so an exit-code-only
  probe reports success while egress is firmly shut. Each target publishes a
  fixed payload; the probe matches it. A reply that arrives but does not match is
  reported as blocked, naming interception as the cause.

`test-redirect` takes `{"from": "..."}` naming an entry already in the stored
`egress_redirects` (404 if it names none — `from` only ever *selects* a stored
row). **It never dials a caller-supplied target**: the probe target always comes
from the stored row, never the request body, or the endpoint would be an SSRF
gadget with a friendly label. It runs two fetches in one sandbox — the mirror
(`to`) through the normal path, then the public endpoint (`from`) again with the
proxy deliberately bypassed — to catch a redirect that's configured but not
enforced.

Both return `200` with `{"state", "detail", "elapsed_ms"}`; `test-proxy` adds
three qualifiers so a client never has to string-match `detail`: `via`
(`proxy`/`direct`), `intercepted` (`blocked`'s captive-portal flavor — something
answered, just not with the published payload), and `custom` (a caller-named URL
with no known payload, so `reached` is the weaker "request completed" claim).
Both endpoints may also carry `warning` (below).

| `state` | Means |
|---|---|
| 🟢 `reached` | The path works — proxy or mirror reachable, and for a redirect, the public host is correctly *blocked* when dialed directly. |
| ⛔ `blocked` | Could not reach the proxy or the mirror. `detail` names the real cause — DNS failure, connection refused, TLS failure, timeout, or curl's own exit code — never a generic "failed". It also carries the one case where the mirror answered but the direct dial of the public host produced no connection fact at all to read (a `from` curl cannot dial — a space, a path, an unsupported scheme): `detail` then says the redirect was NOT tested, and the setup gate stays held, because an untested redirect must never render as `reached`. |
| ⛔ `bypass` | **The one that looks fine but isn't.** The mirror answers, but the public host it's supposed to replace is *also* still reachable, directly, from a sandbox. The redirect is configured but not enforced: a run can silently pull from the internet instead of the mirror, and every other signal — the row is filled in, the mirror answers — looks exactly like a working redirect. "Reachable" means the public host **answered** — any HTTP status, a 403 included, or a TLS-level reply — not that the fetch succeeded: a host that answers `403` is one the confinement class did not block, and so is one that merely **accepted** the TCP connection and then stalled (the probe reads curl's own `num_connects`, because a timeout alone cannot tell an accepted-then-tarpitted dial from one that never left the sandbox). `test-redirect` only. |
| 🟡 `no_runner` | No runner is configured; there's nothing to launch a probe with. Not an error, and not a guess. |
| 🟡 `not_run` | A runner IS configured, but the throwaway sandbox never got to running the probe — an image pull failure, or a confinement class this host can't enforce. Distinct from `blocked`: `blocked` means the probe DID run — usually observing a real network fact, and otherwise saying in `detail` that nothing was learned; `not_run` means nothing was learned either way. Setup's gate treats it the same as `no_runner` (unlocks Next with a neutral note, never a click-past). |
| 🟡 `timed_out` | The probe sandbox started and the task launched, but the run never reported completion within the wait budget (90s) — provably **not** a network verdict, unlike `blocked`. `detail` names the sandbox agent's own observed status at the deadline and `WARDYN_CONTROL_PLANE_URL` to check. Usual cause: the run's recording upload hanging against an unreachable control plane — see "Recording upload path on Kubernetes" below. |

The recorder's upload bound ships inside the agent images: an image pinned through
`WARDYN_AGENT_IMAGES` must be rebuilt from 0.6.6 or later (or use the published
`agent-base:0.6.6` or a newer tag — prefer the current release's, since a
pre-0.7 image also lacks `/home/agent/drive` and silently breaks writable
drives), or its recorder keeps the pre-0.6.6 60s upload tail. A probe
is bounded well under two minutes and reclaims (kills) its sandbox if the run
doesn't finish in time, so a wedged probe can never hold one open.

**`warning` (both endpoints, `omitempty`).** Set alongside a `reached` verdict
when the probe's OWN session recording never reached the control plane even
though egress worked — the "probe passes, recordings silently vanish" case.
Present only when a `RecordingStore` is configured and the runner advertises
session recording; absent (never an empty string) otherwise.

**Recording upload path on Kubernetes.** Every exec-mode run's task is wrapped by
`wardyn-rec`, which PUTs the recording to the proxy pod in parts while the run
goes on and flushes the rest at exit
(`http://wardyn-proxy:3128/wardyn/v1/recordings/<runID>`, then `…/parts/<n>`), which forwards it to
`WARDYN_CONTROL_PLANE_URL` — the chart points this at the control plane's
in-cluster Service FQDN, on the internal TLS port ([Control-plane to proxy
TLS](#control-plane-to-proxy-tls)). Delivery failure is deliberately non-fatal to the task
but bounded (`cmd/wardyn-rec/main.go`'s upload client timeout), so it cannot hold
a finished task's exit longer. A cluster-wide baseline default-deny NetworkPolicy
or a mesh authorization policy can drop the proxy-pod → control-plane hop even
with the ambient-deny ack in place (`WARDYN_K8S_ACK_AMBIENT_DEFAULT_DENY`,
"Kubernetes: known gaps" below) — Wardyn's own per-run NetworkPolicy allows are
additive only within the namespaced policy model and cannot override a
platform-applied deny elsewhere. The probe's `warning` field and `timed_out`
state are how you find out.

### Directory autocomplete: the one path where the daemon dials out

Everything above this line is **sandbox** egress — what a run may reach, brokered
by the proxy sidecar. `WARDYN_DIRECTORY_PROVIDER=entra` opts into something
different in kind, and it is written here so it is never discovered as a surprise
in a firewall log: **wardynd itself** makes outbound HTTPS calls to
`login.microsoftonline.com:443` (the app token) and `graph.microsoft.com:443`
(the search), from the control-plane process. Not sandbox egress. Not proxied by
the egress sidecar, not covered by a run policy's `allowed_domains`, and not
subject to the proxy's IP guard. It is the same class as the daemon's existing
server-side IdP calls — OIDC discovery, the token exchange, the JWKS fetch — and
an egress-restricted control plane has to permit those two hosts explicitly or
every search fails.

**What enabling it grants, plainly: read of the WHOLE directory.** Not a scoped
slice — the connector authenticates as an application and can enumerate users and
groups tenant-wide (`User.Read.All` + `Group.Read.All`; App Roles too where
`Application.Read.All` was consented). That is a real expansion of Wardyn's
minimal-reach posture, so:

- It is **default OFF.** Unset, there is no connector, no token, no Graph call,
  and every "who" field is the free-text input it has always been.
- The consent is performed by a **tenant admin in Entra**, never by Wardyn —
  application permissions cannot be self-granted, and an operator who does not
  want this simply does not consent.
- **Who can read it through Wardyn:** `GET /api/v1/access/directory/search` is on
  the `securityOps` tier — admins and security admins. A member gets 403. That
  disclosure is deliberate and bounded: a security admin assigns governance to
  these very people, and this is a read that changes nothing.
- **What is retained: nothing.** Suggestions live in a 60-second in-memory LRU in
  the daemon process and are never written to Postgres, never to the audit log.
  Individual searches are deliberately **not** audited — one row per keystroke
  would make the append-only log a record of every name an admin ever typed.
  Connector **failures** are audited (`directory.search.fail`), with the
  provider, the failing operation and the upstream status — never the query.
- **Retracting it is one variable.** Unset `WARDYN_DIRECTORY_PROVIDER` and
  restart: the connector is gone, the endpoint answers `503
  {"code":"directory_unconfigured"}`, and the console degrades every picker back
  to a plain text input with no error. Revoking the admin consent in Entra
  retracts it from the other side.

Credentials default to the OIDC app registration with the tenant derived from the
issuer, so the common case is the one variable above. A **public** OIDC client
(PKCE, no secret) and a deployment with no OIDC at all can both do neither, and
either one is a **boot refusal** naming what to set rather than a 503 an admin
discovers by typing — see `WARDYN_DIRECTORY_CLIENT_SECRET` in [ENV.md](ENV.md).

## Toolchain-fidelity environment

Moved, with the sections that followed it here, to [build-images.md](operations/build-images.md).

## Secrets from files (Vault Agent / CSI)

Moved, with the sections that followed it here, to [secrets-and-keys.md](operations/secrets-and-keys.md).

## Renamed in 0.8

The non-admin tier's name changed from `member` to `user` across 0.8's tier-rename
work (#608), and 0.8 follows it with a matching server rename sweep (#617) —
mechanical, no behaviour change, and never a wire alias: an integration built
against the old names gets a `404`/`400` on 0.8, not a warning. History is not
rewritten — an audit row written before 0.8 keeps its pre-0.8 action and field
names forever; only what the server emits GOING FORWARD changed.

| Surface | Before | After |
|---|---|---|
| The toggle ("view as member"/the user view) | `POST /me/member-mode {"enabled":bool}` | `POST /me/view {"view":"user"\|"admin","user_type":"…"}` |
| `/me` fields | `member_mode`, `member_mode_no_credential`, `member_preview_available` | `user_view`, `user_view_no_credential`, `user_preview_available` |
| Audit action | `auth.member_mode` | `auth.user_view.set` — **dual-emitted** alongside `auth.member_mode` (identical `Data`) for one minor (0.8.x, OD-18), so a dashboard or SIEM rule still filtering on the old name keeps seeing rows; the compat row is removed in 0.9 |
| `authz.denied` datum | `member_mode: true` | `user_view: true` — a clean rename, not dual-emitted (it lives inside `authz.denied`'s own row, which is not itself renamed) |
| `authz.denied` reason | `byoi_member` | `byoi_user` |
| Go: `runner` package | `MemberMountPolicy`, `SandboxSpec.MemberMountRoots`, `ParseMemberMountPolicy`, `ValidateMemberMount`, `ValidateMemberMountSource`, `deniedMemberSegment`, `memberCeilingRoots`, `validateMemberSource` | `UserMountPolicy`, `SandboxSpec.UserMountRoots`, `ParseUserMountPolicy`, `ValidateUserMount`, `ValidateUserMountSource`, `deniedUserSegment`, `userCeilingRoots`, `validateUserSource` |
| Go: `internal/auth/oidc` | `SetMemberMode` | `SetUserView` (grew a `typeID` param the same release, #835/UT-13) |
| Go: `internal/api` | `auditMemberPolicyDrops`, `authorizeMemberDecision`, `boundMemberSpec`, `denyMemberCapability`, `denyMemberDrive`, `denyMemberGovernance`, `denyMemberRequest`, `denyMemberRunQuota`, `denyMemberSeededImage`, `denyMemberWorkspaceProviders`, `filterMemberGrants`, `handleSetMemberMode`, `memberDropsIntegration`, `memberEnvSecretIsAdminOnly`, `memberModeRequest`, `memberModelAccess`, `memberMountAllowed`, `memberMountPosture`, `memberPreviewApplies`, `memberSafeCapabilities`, `memberSafeIntegration`, `memberSafeIntegrations`, `memberSourcesAllowed`, `memberVisibleOperatorSecretNames`, `narrowMemberInlinePolicy`, `redactSetupStatusForMember`, `redactSpecForMember` | `auditUserPolicyDrops`, `authorizeUserDecision`, `boundUserSpec`, `denyUserCapability`, `denyUserDrive`, `denyUserGovernance`, `denyUserRequest`, `denyUserRunQuota`, `denyUserSeededImage`, `denyUserWorkspaceProviders`, `filterUserGrants`, `handleSetUserView`, `userDropsIntegration`, `userEnvSecretIsAdminOnly`, `userViewRequest`, `userModelAccess`, `userMountAllowed`, `userMountPosture`, `userPreviewApplies`, `userSafeCapabilities`, `userSafeIntegration`, `userSafeIntegrations`, `userSourcesAllowed`, `userVisibleOperatorSecretNames`, `narrowUserInlinePolicy`, `redactSetupStatusForUser`, `redactSpecForUser` |
| CLI: approvals | `approvals list\|get`, `approve <id>`, `deny <id>` | `wardyn approval list\|get\|approve\|deny` — clean break, no alias, 0.8.4; `--reason`, `--scope`, `--until` unchanged |
| CLI: run logs | `logs <run-id>` | `wardyn run logs <run-id>` — clean break, no alias, 0.8.4 |
| CLI: sessions | `sessions list\|revoke` | `wardyn session list\|revoke` — clean break, no alias, 0.8.4; `revoke` still takes exactly one of `--sub` or `--all` |
| CLI: upsert verb | `drive apply`, `governance apply`, `preset apply` | `wardyn drive set`, `wardyn governance set`, `wardyn preset set` — clean break, no alias, 0.8.4; `set` is the one upsert verb, as it already is on `policy`, `secret` and `site-config` |

`denyMemberField` — the old shared helper this table's first cut of the sweep
named — does not appear in the 0.8 column: it is not renamed but RETIRED, folded
into `refuse`/`authz.Deny` (`internal/api/refusal.go`) by #736 (every refusal
through one emitter). Every site that called it (the `byoi_user` image/devcontainer doors, the four
`governance_profile` shape refusals, the `workspaces.llm_cred` admin-surface arm,
`harness_login_not_per_user`) now calls `s.refuse(w, r, authz.Deny(...))`
directly, and the `authz.denied` marker moved with it from the now-deleted
`authzDeniedDatum` (`internal/api/membermode.go`) into `internal/authz.Datum`.

**Not renamed in this pass** — each is a separate, later issue, so the old name
is still correct until its own PR lands:
- The People/Getting-Started copy, and the rest of this
  file's own "view as member" prose ([Exercising member mode as an
  admin](operations/member-mode.md)) — #620, the docs pass.
- The console's remaining "member" copy — #618.
- `oidc.LegacyRoleMember`/`oidc.LegacyRoleMemberWarning` and the `member`
  role-map value itself, which keep working and warn through 0.8.x by design
  (see "A chart that still says `=member`" in the CHANGELOG's #608 entry) —
  removed in 0.9, not renamed now.
- The `classMember` route-classification identifier (`internal/api`'s authz
  matrix) — a tier classification, not this feature; out of scope.

## Upgrades

Migrations are **forward-only**. `internal/db` records each applied filename in
`schema_migrations` and applies anything new on boot, under an advisory lock so
concurrent starts do not race. There are no `down` migrations and no downgrade
path — a rollback to an older wardynd against a migrated database is unsupported,
and wardynd itself refuses it: a boot that finds a `schema_migrations` row it does
not ship stops before writing anything, naming the newest unknown file. That covers
`helm rollback` and a pinned older image, not only `install.sh`. Restore the dump.
`WARDYN_ALLOW_UNKNOWN_MIGRATIONS=true` is the break-glass past the refusal; it does
not make the older binary understand the newer schema. One name is a known
exception, not a downgrade: 0.7.12 databases record `0065_secret_envelope_v1.sql`
(this tree ships the byte-identical file as `0069_secret_envelope_v1.sql`, freeing
0065-0068 for migrations added after the 0.7 branch point), and `internal/db`'s
`retiredMigrations` table accepts that row — the supported 0.7.12 -> 0.8 upgrade
boots normally.

**Upgrading from 0.7.11 or earlier converts every stored secret, once, and it
cannot be undone without the backup.** `0069_secret_envelope_v1` adds the envelope columns, and
the first boot of 0.7.12 or later re-seals every existing (pre-envelope,
age-encrypted) row of
`secrets` as envelope v1 — before it reads its own boot keys, which live in the
same table. It runs as one transaction under its own advisory lock
(`db.SecretConvertLockKey`): a second replica starting at the same moment waits,
then finds nothing left to convert, and every later boot converts nothing. After
it commits, an older wardynd can read none of these rows. There is **no rolling
upgrade across this release**:

```sh
# 0. Take the Postgres dump (see Backup) AND confirm you hold the age key. The
#    dump plus that key is the ONLY way back to an older wardynd afterwards.
# 1. Stop EVERY older replica — one-instance locking cannot see it under
#    WARDYN_HA. An older binary still running keeps writing
#    pre-envelope payloads, which the new version refuses by name ("an older
#    wardynd is still writing"). A NEW name it wrote is converted at the next
#    restart;
#    a name it REPLACED is overwritten in place and must be set again.
# 2. Start the new version with the SAME WARDYN_AGE_KEY. The log says how many it converted:
#    INFO wardynd: converted stored secrets to envelope v1; … secrets=7
```

- **A row the key cannot decrypt stops the boot**, naming it —
  `v0 conversion ABORTED after 2 of 9 rows (nothing committed …): (owned_by="", name="github-app-key") does not decrypt with WARDYN_AGE_KEY`.
  Nothing was converted and the older binary still reads the store. Set the key
  that row was written with, or delete that one row if it is dead, and start again.
- **`WARDYN_AGE_KEY` unset now refuses to start** while any row is sealed under an
  age key (pre-envelope or `local:`), instead of minting an ephemeral key that
  would strand them all.
- **Starting 0.7.11 or earlier over a converted database fails closed** with a
  line that looks like an age-key problem but is not one:
  `load secret "wardyn-signing-key": pg secretstore: decrypt wardyn-signing-key: age decrypt: failed to read header: parsing age header:`
  followed by `file is empty` or by `unexpected intro: "…"`. Either ending means
  the row is not an age payload at all: it is envelope v1, which that binary
  cannot read with any key. The row is not empty ("file is empty" is age's
  wording for "no line break found"), and the quoted bytes are the start of its
  AES-GCM ciphertext. The older binary changes no stored secret before it
  exits. **Do not rotate or replace `WARDYN_AGE_KEY`.** Start 0.7.12 or later
  again with the same key, or restore the pre-upgrade dump (step 0) before you
  run the older version. A wrong key reads differently:
  `age decrypt: no identity matched any of the recipients`.
  Over a database 0.8.6 or later migrated, the older binary stops earlier, at its
  boot audit check: `migrate: db: the audit chain canary could not append a row`,
  ending in `audit_events: rows are appended only through audit_append()`. The
  audit conversion is one-way (see "Stopped-writer upgrade"); the meaning and the
  remedy are the same: start the newer release again, or restore the pre-upgrade
  dump before you run the older version.

**Upgrading to 0.8 signs every SSO human out, once, under TLS (#1258).** With
secure cookies on (TLS served directly, or `WARDYN_TLS_TERMINATED`), the session
cookie is now `__Host-wardyn_session`, and the old `wardyn_session` is never
read, not even as a fallback, so every human re-authenticates at their next
request. The old cookie is left to expire. A plain-HTTP install keeps the old
names and signs nobody out. Admin-token and API-token auth are unaffected.

**Upgrading to 0.7 signs every SSO human out, once.** The session payload gained
a codec version and `decodeSession` requires an exact match
(`SessionCodecVersion`, `internal/auth/oidc/session_codec.go`), so every cookie
minted by an earlier release decodes as no session and the human re-authenticates
at their next request. Nothing is lost but the login: grants, group snapshots and
governance assignments are all read from the database. **Admin-token and
API-token auth are unaffected** — neither carries a session cookie.

```sh
git pull && make compose-build          # rebuild wardynd at the new revision
docker compose -f deploy/compose/docker-compose.yaml up -d wardynd
```

Take the Postgres dump above **before** the restart; that dump is the only
rollback you have. Agent images are built separately — `make agent-images`
rebuilds them.

**0.7 needs the agent images rebuilt, or an allocated drive is unwritable.**
`/home/agent/drive` is the reserved in-container mount point a **user drive**
lands on (`runner.DriveTarget`, `internal/runner/mount.go`), and every image
under `deploy/images` now pre-creates it owned by `agent` — the ones on a public
base do it themselves, the ones on a sibling image inherit it — so a fresh
managed volume takes that ownership through Docker's copy-up. **No pre-0.7 image
has the directory**: 0.6.6's base image created `/home/agent/work` and nothing
else, so the daemon conjures a **root-owned** one at mount time and a drive you
allocated *writable* is unwritable by uid 1000 on its very first run. Nothing
else goes wrong — the image builds, the container starts, the mount succeeds —
so the only symptom is the agent failing to write to its own drive, and wardynd
cannot repair it (fixing that ownership would mean chowning volume state, which
the control plane must never do). Rebuild with `make agent-images-core`, and
re-pin anything listed in `WARDYN_AGENT_IMAGES` at the rebuilt tag; a **BYOI**
image is yours to fix, one `mkdir` (see "User drives on Docker" and
`deploy/images/README.md`'s image contract). `TestAgentImagesPreCreateDriveDir`
holds the rule for every image in this tree. A deployment that registers no
drive is unaffected.

**0.7 refuses `/home/agent/drive` as an AUTHORED mount target, and a row stored
before this release still names it.** The reserved target — the whole subtree,
so `/home/agent/drive/shared` too — is refused to every policy
`workspace_mounts[].target`, every `workspace_repos[].target` and every
workspace `local_dir` source target (`ValidateAuthoredTarget`,
`internal/runner/mount.go`, the authored-target arm of the same validator every
mount target runs). Before 0.7 the rule was only the allowed-prefix one
(`/home/agent`, `/work`, `/workspace`), which admits it. What a stored row does
next splits by where it lives. A stored **workspace** source IS re-validated,
at run-create — `seedRequestWorkspace` re-runs `ValidateAuthoredTarget` over
every `ws.Sources` target (`internal/api/runs_create.go:94-115`) — and is
refused `422` with `workspace <id> source target: target /home/agent/drive is
reserved for the user drive`. A stored **policy**'s `workspace_mounts` or
`workspace_repos` row is never re-validated on read — `validatePolicySpec`
runs on the two **write** paths only — so it survives the upgrade and reaches
dispatch intact; `buildRunMounts` re-checks the reserved target there and
**drops** the mount rather than failing the run, logging `wardynd: stored
policy binds the reserved user-drive target; dropping that mount` at WARN
(`internal/api/runs_dispatch_mounts.go:96-104`) — the run starts one bind
short. `buildRepoRecords` makes the same drop for a stored policy's
`workspace_repos[].target`, folded into its destination validation
(`internal/api/runs_scm.go:166`), but with **no** distinct log line for that
arm. That WARN is the *only* run-time signal a stored policy row produces —
no HTTP error, nothing the member sees — and `buildRunMounts`' drop means
dispatch never reaches the driver-level `docker: denied workspace mount
"<source>" -> "<target>": target /home/agent/drive is reserved for the user
drive` refusal (the `ValidateAuthoredTarget` check in `Driver.agentMounts`,
`internal/runner/docker/driver_mounts.go`) for this
case at all; that check now guards only a path a stored policy row can no
longer take. Find both shapes before the upgrade window rather than in
somebody's run or wardynd's log:

```sh
for path in policies workspaces; do
  curl -fsS "$WARDYN_URL/api/v1/$path" -H "Authorization: Bearer $WARDYN_ADMIN_TOKEN" \
    | grep -o '"target":"/home/agent/drive[^"]*"' || true
done
```

Re-target each hit anywhere else under `/home/agent`, `/work` or `/workspace`
and write the policy or workspace back. Nothing migrates them for you, on
purpose: a mount target is an operator's authored decision, and silently moving
a bind is the outcome this refusal exists to prevent.

On Helm, a **mixed-version rollout repeats that logout** for as long as both
versions serve: a human who lands on an old replica is signed in, and the next
request routed to a new one bounces them. It costs logins, not containment — the
old binary never accepts a cookie the new one refuses, only the reverse — but
plan the window. `--wait` (below) is what keeps it short.

**On Helm, a downgrade past 0.6 also stalls the rollout, before migrations ever
matter.** The chart's readiness probe targets `/readyz`, which the 0.6 images
introduced. The empty default `image.tag` resolves to `.Chart.AppVersion`
(the chart's own `Chart.yaml`, always the shipped release's version), so a
stock install is fine; an `image.tag`
explicitly **pinned** at or below `0.5.0`
serves only `/healthz`, so the probe 404s forever, the pod never joins the
Service's endpoints, and `helm upgrade`/`rollout status` hangs NotReady with
nothing crashed and nothing logged. Pin the probe back for such an image with
`--set readinessProbe.path=/healthz`, accepting that version's ceiling (a dead
Postgres reads healthy again). CI does not catch this — `helm-install-test` and
the kind quickstart both build `wardynd` from source.

### Upgrading a one-line install

The recipe above assumes a checkout. An install created by `curl … | sh` has
none — its compose file and `.env` live in `~/.wardyn` (or `$WARDYN_HOME`), and
the installer's own closing banner says only *"re-run this installer at the new
version"*. That is the mechanism, and it is genuinely all of it: re-running
fetches the new release's compose file and bumps the image pins in `.env`
(`WARDYN_AGENT_IMAGES` is **merged**, so an image you added by hand survives),
while leaving your age key and your ports alone — it refuses outright rather
than continue if `WARDYN_AGE_KEY` is missing, and it re-mints the admin token
only when the existing one is empty or a placeholder. What it does **not** do is
take the dump for you, and the forward-only rule is the same one:

```sh
WARDYN_VERSION="${WARDYN_VERSION:?set this to the release you are moving TO}"
cd ~/.wardyn                                             # or $WARDYN_HOME
docker exec wardyn-postgres pg_dump -U wardyn wardyn > wardyn-$(date +%F).sql
docker compose down                                      # keeps the Postgres volume
curl -fsSL "https://github.com/cjohnstoniv/wardyn/releases/download/v${WARDYN_VERSION}/install.sh" | sh
curl -fsS http://127.0.0.1:8080/healthz                  # or your WARDYN_UP_PORT
```

That dump is your only rollback, for the reason at the top of this section:
there are no `down` migrations, so re-running an OLDER installer against a
database a newer wardynd has already migrated is unsupported — and `install.sh`
refuses it: a downgrade is a refusal that writes nothing, so restore the dump
onto the older version instead. `wardyn --version` says what CLI you have and `/healthz`'s `version` field
says what the control plane is serving; check both before moving backwards.

The desktop tier is different again: its upgrade is an MDM rewrite of the two
image digests in `wardyn.env`, not a re-run of anything
([DESKTOP.md](DESKTOP.md)).

### Splitting the migrator and app roles (`WARDYN_PG_MIGRATE_DSN`)

Single-DSN mode logs a NOTICE at every boot: wardynd's own role owns
`audit_events`, so `DROP TRIGGER`, `ALTER TABLE … DISABLE TRIGGER` and
`DROP TABLE` bypass the append-only guard. `WARDYN_PG_MIGRATE_DSN` is the fix —
migrations run as an owner/migrator role, wardynd connects as a non-owner app
role — and this is how to adopt it on a database that already exists.

**Which role becomes which is the whole procedure, and it only works one way.**
The role you have TODAY already owns every table, function and trigger, so it
becomes the **migrator**. The role you create is the **app** role. Doing it the
other way round — pointing `WARDYN_PG_MIGRATE_DSN` at a fresh "migrator" that
owns nothing — fails on the first migration that touches an existing object,
because PostgreSQL requires ownership for `ALTER TABLE` and for
`CREATE OR REPLACE FUNCTION`. That is not hypothetical on a 0.6 → 0.7 upgrade. Every 0.6.x release ships
through `0049`, so this path applies `0050`–`0062`, and most of it is exactly
this shape: `0050` (secrets), `0052` and `0060` (api_tokens, created back in
`0045`), `0055` (workspaces) and `0062`, `0063`, `0064`, `0065`, `0072`, `0073` (approvals and
`agent_runs`, both created in `0001`) are
`ALTER TABLE` on tables an earlier release created — `0050` also drops and
re-adds a primary key, `0060`, `0062` and `0064` each drop and re-add a CHECK
(`0062` widens `approvals.state` with `CANCELLED`, `0064` widens
`approvals.kind` with `credential_reauth`), `0063` adds the
`agent_runs.status_detail` column, `0065` adds `agent_runs.autonomy_level`, `0072` adds the
run-limit columns (`ends_at`, `wait_budget_sec`, `run_limits`, `governance_profile_id`), `0073` the
lease columns (`lost_at`, `lost_reason`, `ending_soon_for`, `ending_soon_sec`) — and `0056`, `0057` and `0058` are three successive
`CREATE OR REPLACE`s of the chain function `0047` created, each re-creating its
trigger on `audit_events`. (`0053` alters `role_mappings`, which `0051` CREATES
two migrations earlier in the same run, so it is not an instance of the hazard.)
The same shape recurs one release later: `0067` adds `user_drives.object_scheme`,
and `user_drives` itself was `0054`'s table — created inside the already-shipped
0.7 line, not this upgrade's own batch — so an install carried forward from a
released 0.7.x hits the identical ownership requirement on its next upgrade —
as does `0069`, which adds the envelope columns to `secrets` (`0001`'s table),
and `0070`, which adds `ssh_public_keys.capped` (`0033`'s table). `0074` does
too: it renames the stored `member` tier to `user`, re-adding the role CHECK on
`api_tokens` (`0045`'s table), moving the role default there and on
`ssh_public_keys` (`0033`'s, whose `0070` cap it re-creates), and altering
`role_mappings` (`0051`'s). So does `0075`, which re-adds the `approvals.kind`
CHECK (`0001`'s table) with `push_content`, and `0076`, which adds `agent_runs.model_provider_id`.
0.8's user types add three more: `0079` re-adds the subject-type CHECKs on
`capability_grants` (`0042`'s table), `governance_assignments` (`0052`'s) and
`user_drive_grants` (`0054`'s), `0080` adds `agent_runs.user_type`, and `0082` adds
`api_tokens.user_type` with its CHECK. The long-holds runs add six more on `agent_runs`:
`0083` adds `token_renewed_at` and `0084` adds `proxy_release`, and `0088`
(`0088_agent_runs_containment_error`) adds `containment_error` and `containment_error_at`;
`0095` adds `end_tightened_at`, `0096` adds `disk_mib` and `0097` adds the pause columns
(`paused_at`, `paused_reason`, `active_at`).
`0089` adds `agent_runs.operator_owned`.
`0090` adds `api_tokens.minted_by` beside its new `people` table.
`0092` adds `agent_runs.ended_at`.
`0094` adds `attach_tickets.via_delegate`/`via_grant` and `agent_runs.created_via`.
`0106` adds `attach_tickets.authorized_at`/`email` (`0026`'s table).
`0111` converts `audit_events` (`0001`'s table) to a partitioned table: it adds `recorded_at`, drops the
identity and the primary key, renames the table and re-creates its triggers, and `0112` is the
`CREATE OR REPLACE` of `0047`'s chain function that the partitions need.
`0114` adds `api_tokens.expires_at` (`0045`'s table).
`0115` adds `api_tokens.identity_stamped_at` (`0045`'s table), backfilled to `created_at`.
`0116` adds `governance_profiles.contact` (`0052`'s table).
`0117` (`0117_agent_runs_sizing`) adds the dispatch-time sizing columns on `agent_runs` (`runner_kind`, the agent CPU/memory
request and limit columns, `proxy_cpu_millis` and `proxy_memory_mib`); at dispatch, before the sandbox
is created, each run records the values its driver applied, and a run that predates it reads all NULL.
`0125` adds `governance_profiles.base_profile_id`, `overlay` and `overlay_limits` with five CHECKs (`0052`'s table).
`0127` (`0127_deprovision_jobs`) adds `people.deactivated_at` (`0090`'s table), beside its new `deprovision_jobs` table.
`0130` (`0130_audit_chain_head_from_meta`) is a later `CREATE OR REPLACE` of `0047`'s chain function, which
links each new row to the recorded head.
`0085` is named for its `CREATE OR REPLACE FUNCTION push_content_paths_immutable()`,
but it is not an instance of the hazard: it creates that function and the
`push_content_paths` table in the same file, so the migrator owns both from the start.
`0087` adds `agent_runs.preset` and `agent_runs.preset_version` beside its new
`launch_presets` table.
`scripts/test-claims-match-code.sh` derives that list from the migration bodies,
so a new `ALTER TABLE` landing undocumented fails there rather than here. The
failure is loud and the boot is refused — but **it is not a rollback, and it does
not leave the database where it found it.**

**Per-migration atomicity bounds ONE migration, not the sequence.**
`applyMigration` wraps each file in its own transaction and records it in
`schema_migrations` inside that same transaction, and `migrateOn` returns on the
first error (`internal/db/db.go`). So a failure at migration *N* leaves `0…N-1`
**committed and recorded** and only *N* rolled back: the database is
**half-upgraded**, and wardynd's refusal to boot is a refusal to serve that
state, not a repair of it. In the scenario above, `0050` and `0051` commit before
`0052` fails on `api_tokens` and `schema_migrations` is left at `0051`; a failure
at `0060` instead leaves `0050`–`0059` applied. Migrations are forward-only with
no `down` path, so **restoring the pre-upgrade dump is the only supported
recovery** — putting the older binary back does not undo the migrations that
already committed, and it does not boot over them: `migrateOn` lists the migrations
`schema_migrations` records that the binary does not ship (`unknownAppliedMigrations`,
`internal/db/db.go`) and refuses to boot, naming the newest one. With
`WARDYN_ALLOW_UNKNOWN_MIGRATIONS` set it carries on over a schema whose one-way
conversions it cannot read: over a 0.8.6 schema it stops at the audit chain canary,
whose direct insert the chain trigger refuses, and over an older one it fails at the
first write the schema no longer supports (the first secret write, for the scenario
above). The dump is the only way back. Take it before the upgrade, not after the
refusal.

The permission error itself has no way forward except giving the migrator
ownership; do that on the restored database, not on the half-upgraded one.

Run this as the role you have today, the one in `WARDYN_PG_DSN`:

```sql
-- 1. The new LEAST-PRIVILEGE app role. Migrations create no roles by design
--    (0007_audit_least_privilege.sql: "deploy/infra territory").
CREATE ROLE wardyn_app LOGIN PASSWORD '…';
GRANT USAGE ON SCHEMA public TO wardyn_app;

-- 2. Full DML everywhere it needs it …
GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA public TO wardyn_app;

-- 3. … except audit_events, which is INSERT + SELECT and nothing else. This is
--    the point of the split: no UPDATE, no DELETE, no TRUNCATE — and no TRIGGER,
--    which would let the app role add its own BEFORE INSERT trigger that fires
--    after the shipped one (name order) and overwrite the hashes on the way in.
REVOKE UPDATE, DELETE, TRUNCATE, TRIGGER, REFERENCES ON audit_events FROM wardyn_app;

-- 3b. 0.8.6 and later: nothing writes audit_events directly. Every row goes in
--     through the SECURITY DEFINER function audit_append, so the app role gets
--     EXECUTE on it and loses INSERT. The migration that converts the table
--     (0111) does this once for every role that held INSERT at the upgrade;
--     run it yourself for a role you create after that. The same GRANT ALL
--     TABLES in step 2 also reached the table's partitions and its bookkeeping
--     tables, so take their write privileges back too.
REVOKE INSERT ON audit_events FROM wardyn_app;
REVOKE INSERT, UPDATE, DELETE, TRUNCATE, TRIGGER, REFERENCES
  ON audit_partition_meta, audit_chain_anchors, audit_events_legacy FROM wardyn_app;
DO $$
DECLARE p regclass;
BEGIN
  FOR p IN SELECT inhrelid::regclass FROM pg_inherits WHERE inhparent = 'audit_events'::regclass LOOP
    EXECUTE format('REVOKE INSERT, UPDATE, DELETE, TRUNCATE, TRIGGER, REFERENCES ON %s FROM wardyn_app', p);
  END LOOP;
END $$;
GRANT EXECUTE ON FUNCTION
  audit_append(uuid, timestamptz, uuid, text, text, text, text, text, text, jsonb),
  audit_ensure_partitions(integer),
  audit_partition_digest(text),
  audit_retention_drop(text, text, text),
  audit_retention_set_policy(integer),
  audit_retention_partitions(text, boolean),
  audit_retention_window() TO wardyn_app;

-- 4. Every FUTURE migration creates its tables as the MIGRATOR, and a new table
--    grants the app role nothing. Without this line the next upgrade boots an
--    app role that cannot read its own new tables. The second line does the same
--    for the functions a later release adds. A new audit partition is the one
--    table this does NOT hand write access: audit_ensure_partitions takes
--    every privilege but SELECT back as it creates each month.
ALTER DEFAULT PRIVILEGES IN SCHEMA public
  GRANT SELECT, INSERT, UPDATE, DELETE ON TABLES TO wardyn_app;
ALTER DEFAULT PRIVILEGES IN SCHEMA public
  GRANT EXECUTE ON FUNCTIONS TO wardyn_app;
```

Then set **`WARDYN_PG_MIGRATE_DSN` to the DSN you were already using** and point
`WARDYN_PG_DSN` at `wardyn_app`, and restart. Do **not** grant `wardyn_app`
membership in the owner role and do not make it a superuser: either one hands
back every privilege the split just removed, and the boot check below is written
to catch exactly that.

**wardynd verifies the claim rather than asserting it.** On the next boot it
queries whether the app role can reach past the guard by any of FOUR routes —
membership in a superuser role, membership in `audit_events`'s owner role, the
`TRIGGER` privilege on the table, or (PostgreSQL 15+) the `SET` privilege on
the `session_replication_role` parameter, held directly or through a role it
can `SET ROLE` into (`db.AuditDDLBypassRoutes`) — and logs one of two lines:

- `migrations applied via WARDYN_PG_MIGRATE_DSN … app role is a verified
  non-owner of audit_events — the append-only guard is DDL-protected`
- `WARDYN_PG_MIGRATE_DSN is set but the app role … still owns audit_events or is
  a superuser — DDL protection is NOT in effect`

The second line now carries a `bypass_routes` field naming the route(s) that
fired, because the remedy differs per route: the first two are closed by
connecting as a different role, the third by a `REVOKE TRIGGER ON
audit_events`, and the fourth by `REVOKE SET ON PARAMETER
session_replication_role` — which no amount of role-swapping reaches.

The second line means the split did not take; the deployment is no worse off than
single-DSN mode, and no better.

**Why a role with SELECT and EXECUTE can write to a hash-chained table at all.** Since
0.8.6 no role inserts into `audit_events`. Every row goes in through `audit_append`, a
`SECURITY DEFINER` function that runs as its owner — the migrator, which also owns the
table — so allocating `seq` and `recorded_at`, reading the chain head and advancing the
high-water mark are the owner's acts, not the caller's. Before `0057` a split deployment
hit `permission denied for sequence audit_events_seq_seq` on **every** audit insert,
which pushed every write to the spool and refused every credential mint. Keep the
migrator as the owner of the table, the sequence and the functions; that pairing is what
makes the posture work. wardynd checks it at boot: a serving role that cannot `EXECUTE`
`audit_append` refuses to start and names the `GRANT`, and a function any role may execute
is reported on a `WARN` line with the `REVOKE`.

**Single-DSN installs keep one honest gap.** The one role owns the table, so the INSERT
privilege cannot be taken from it; the chain trigger refuses any row `audit_append` did
not allocate, which stops a mistaken or out-of-tree writer, not an owner who edits the
trigger. The boot log says so on a `NOTICE` line ("app role can INSERT directly").

## Kubernetes: day-2

The four sections above — backup, restore, the age key, upgrades — are written
against compose and none transfers verbatim to the chart; this section is the k8s
form of the same four questions. Every command below was run once against the
throwaway cluster [`deploy/kind/quickstart.sh`](../deploy/kind/quickstart.sh)
builds (`make kind-quickstart`), and the outputs shown are that run's; substitute
your own release, namespace and Postgres. That cluster is demo-grade (a single
Postgres pod with no PVC) — a place to rehearse, not a template.

### `helm upgrade`, and why `--wait` is not optional

Migrations are forward-only, applied on boot, no `down` path — wardynd on k8s runs
the identical `internal/db` code, so **take the dump before the upgrade**, through
the Postgres you run or through `kubectl exec` if it lives in the cluster:

```sh
kubectl -n wardyn exec deploy/postgres -- \
  pg_dump -U wardyn wardyn > wardyn-$(date +%F).sql
```

Then upgrade — passing the install's values, from the file you keep them in:

```sh
helm -n wardyn upgrade wardyn ./deploy/helm/wardyn \
  -f your-values.yaml --set image.tag=<new-tag> --wait --timeout 5m
```

Keeping the values in a version-controlled file makes what is deployed reviewable,
and it steps around two Helm sharp edges.

**The first: `helm upgrade` reuses the previous release's values *only while you
pass no `--set`/`-f` at all*.** Add a single `--set` and Helm resets everything
else to chart defaults — dropping exactly the values a Wardyn install cannot run
without (`auth.adminToken.*`, `k8s.proxyImage`, `serviceAccount.automount`,
`secrets.ageKeyFromSecret`). The chart catches that rather than render a crippled
install, so such an upgrade fails at render time naming the missing one:

```console
$ helm -n wardyn upgrade wardyn ./deploy/helm/wardyn --set image.tag=<new-tag> --dry-run
Error: UPGRADE FAILED: execution error at (wardyn/templates/secret.yaml):
wardyn: the public API would 401 every request. Set auth.adminToken.secretRef.name
(external Secret), auth.adminToken.value (inline demo), env.WARDYN_ADMIN_TOKEN, or
env.WARDYN_OIDC_ISSUER for SSO — [...]
```

(Helm prints a `templates/secret.yaml:<line>:<col>` location alongside that
message; the line moves whenever the template does, so match on the message.) A
refusal is the good case, and dropping `secrets.ageKeyFromSecret` earns one too:
on an external-DSN install the chart refuses any render with no age identity
wired, rather than letting the reset render cleanly and take the pod down at boot
(see [the age key](#the-age-key-is-a-secret-and-the-default-loses-your-secrets-on-boot-2)
below).

**The second, and why `--reuse-values` is not the fix for the first: it makes
the PREVIOUS release's values win over the new chart's, so a default the new
version CHANGED silently keeps its old value.** `--reuse-values` layers the
previous release's coalesced values *over* the new chart's `values.yaml` — it
does not replace it. So a block the new version merely ADDED is not missing from
the map the templates read: it arrives with the new chart's defaults, and no
nil-dereference follows from its being new. (A `helm template` of a faithfully
reconstructed `0.6.6` values map against the `0.7` chart renders byte-identical
objects to the same map against `0.6.6`, because `trustedCA` and `userDrives`
are purely additive.)

What `--reuse-values` really costs you is the other direction. Every key the
previous release's map *does* carry wins — including the keys it carries only
because they were that chart's defaults, never because you chose them. So the
day a Wardyn release CHANGES a default (rather than adding one), a
`--reuse-values` upgrade silently keeps the old value, with nothing at render
time to say so: a hardened NetworkPolicy port list, a probe path, a security
context. That has not bitten anyone yet — every `values.yaml` change from `0.5`
through `0.7` is additive, which is exactly why the reused-map render above is
byte-identical — and it is a property of the changes so far, not a promise. Use
`--reset-then-reuse-values` instead (Helm ≥ 3.14: starts from the NEW chart's
defaults and layers only your explicit overrides on top), or better, pass `-f
your-values.yaml` as above and keep that file the source of truth.

(The `| default dict` guards in `templates/networkpolicy.yaml` and
`templates/rbac.yaml` are **null**-robustness, not `--reuse-values`
robustness: they cover a key that is PRESENT and explicitly `null`, which is
what `--set uiSandbox=null` or an operator clearing a block by hand produces.
That is the case the chart's own render checks exercise.)

The new pod applies only the migration files `schema_migrations` does not already
record. For the `0.5` → `0.6` upgrade this recipe serves, that is
`0042_capability_grants` and `0043_ssh_key_role`; re-running the same version
applies nothing and the count is `0`:

```console
$ kubectl -n wardyn get pods -l app.kubernetes.io/name=wardyn
NAME                      READY   STATUS    RESTARTS   AGE
wardyn-66c8f746c4-2b5mq   1/1     Running   0          25s

$ kubectl -n wardyn logs deploy/wardyn | grep -c "applied migration"
2
```

A non-zero count is the expected shape of a version bump, not a warning. It is
`0` only when the schema was already current.

**`--wait` (or `--atomic`) is the load-bearing flag, not a courtesy.** Without it
`helm upgrade` reports on the API objects it wrote, not on whether anything came
up: a deliberately broken upgrade on this cluster printed `STATUS: deployed` and
exited `0` while its only pod sat in `CrashLoopBackOff`, and `helm history` later
recorded that same revision as a clean `Upgrade complete`. Helm's release status
is not a health signal. Re-check `/healthz` after every upgrade — the
quickstart's own probe asserts `.runner == "k8s"` rather than accepting any
`200`, for the same reason.

There is still no rollback. `helm rollback` restores the previous *manifest*,
which is the wrong half: the schema stays migrated, and the older wardynd it
reinstates is the unsupported combination named above. Use it for a bad *config*
change (a wrong env var, a wrong image tag within one schema generation). For a
bad *release*, the dump is the rollback.

### Stopped-writer upgrade

Some migrations convert existing rows and must not run beside a writer; 0.8.6's audit conversion is the first.
**Take a dump first; the audit conversion is one-way.** Upgrading needs Postgres 13 or newer:
`0107_pg13_floor` refuses an older server by name and records nothing, so the database stays as the previous
release left it.

The default path is already a stopped-writer path. The chart's `Recreate` strategy tears the old pod down, which
releases its single-instance lock, before the new pod starts and migrates; compose does the same with
`docker compose down` first. If you want the migration to finish before any new wardynd serves, run it alone with
`wardynd -migrate-only`: it takes the single-instance lock on one connection, runs the same migration on that
connection, and exits without serving.

```sh
# 1. Stop every writer. Under the chart, this is the step that matters:
kubectl -n wardyn scale deploy/wardyn --replicas=0
# 2. Run the migration once, from the NEW image, with the DSN the Deployment uses
#    (WARDYN_PG_MIGRATE_DSN when you split roles). Any Job that runs
#    `wardynd -migrate-only` in the new image works; wait for it to complete.
# 3. Upgrade the release; the new pod finds nothing left to migrate.
helm -n wardyn upgrade wardyn ./deploy/helm/wardyn -f your-values.yaml --set image.tag=<new-tag> --wait --timeout 5m
```

`-migrate-only` exits `0` after migrating, `1` when the migration fails (or no database is configured), and `3`
when it refused to start. It refuses, naming the reason, in two cases:

- another session holds the single-instance lock: a serving wardynd, or another `-migrate-only`; and
- any other client is connected to the database, whether or not a lock is held. The lock is not enough on its
  own, because a replica started with `WARDYN_HA` never takes it. A migrator role sees other
  roles' sessions by user and application name, so the check holds under a split `WARDYN_PG_MIGRATE_DSN`.

A wardynd that boots while `-migrate-only` runs fails its own single-instance claim and exits, so a Job cannot be
raced by a pod the scheduler restarts. The migration's time bound is `WARDYN_MIGRATE_TIMEOUT` (default 5 minutes),
and the chart's startup probe window follows it: 30 seconds to connect, the timeout, and 120 seconds of slack.
Raise it in `env` or `extraEnv` as `h`, `m` and `s` units only.

**Under `ha.enabled`, no replica holds the lock**, so nothing the lock does stops writers there:
`kubectl scale --replicas=0` is the only pre-step that does. `-migrate-only` will still refuse while any replica
is connected, but it is the scale-down that makes the run possible.

#### What the audit conversion does (`0111_audit_partitioned`, `0112_audit_chain_partitioned`)

`audit_events` becomes a table range-partitioned by month on a new, server-assigned `recorded_at`. Every
existing row keeps its place in the chain, its hash and its `seq`, and lands in one partition,
`audit_events_legacy`, whose upper bound is the moment of the conversion. The conversion is one transaction and
it commits with the append-only, chain and TRUNCATE guards already armed, so a crash at any point leaves the
table guarded. `audit_ensure_partitions(12)` then creates the current month and twelve more, at every boot, before
the listener; an insert into a month that does not exist fails and waits in the audit spool until it does.

- **0.8.5 writers are refused.** A 0.8.5 binary inserts directly with no `seq` or `recorded_at`; the chain trigger
  refuses that row, and the converted schema is one a 0.8.5 binary will not start against (it does not ship
  `0111` or `0112`). The chart's `Recreate` strategy is what keeps one from writing during the upgrade.
- **The history stays together.** Every row from before the conversion is in `audit_events_legacy`, one
  partition that ends at the moment of the conversion.
- **An index you built out of band** on `audit_events` (beyond the ones Wardyn ships) stays on
  `audit_events_legacy`; the partitioned table starts with Wardyn's own set. Re-create yours on the parent if you
  need it on the months to come.
- **Grants.** Every role that could `INSERT` into `audit_events` before the upgrade is granted `EXECUTE` on
  `audit_append` and `audit_ensure_partitions` in the same transaction, found from the table's ACL rather than by
  name, and only then loses `INSERT`. The later migrations of this release (`0119`, `0123`) grant their functions
  to the roles that hold `EXECUTE` on `audit_append`. Roles that could `SELECT` keep it. A role you add later needs the
  `GRANT EXECUTE` line in the recipe above.
- **Time.** The cost is one scan of the existing rows to prove the legacy bound, one index build on them, and
  the lock waits. Measured on Postgres 13 and 17 in a container on a shared development machine, over rows
  with the audit log's columns, bulk-loaded without chain hashes (the scan and the index build do not read them);
  a `psql` timing of the `0111` transaction, with the twelve months of partitions inside it, and `0112` adding
  under 10 ms:

  | Rows before the conversion | Postgres 13 | Postgres 17 |
  |---:|---:|---:|
  | 100 000 | 0.3 s | 0.1 s |
  | 1 000 000 | 0.7 s | 0.4 s |
  | 3 000 000 | 1.8 s | 1.0 s |
  | 12 000 000 | 6.2 s | not measured |

  Scale the figures to your own row count; they are an order of magnitude, not a promise. The chart's startup
  probe window is derived from `WARDYN_MIGRATE_TIMEOUT` (above): if your table is larger than the figures cover,
  raise the timeout rather than letting the probe kill a migration that is making progress.

### Backup: what `pg_dump` carries here, and what it does not

The chart renders no database. `postgres.dsn` points at a Postgres you operate,
so the backup is your Postgres's own backup story — Wardyn adds no mechanism.
The differences from the Compose recipe are:

- **Recordings are NOT in the dump on a stock chart install.** The chart pins
  the recording store itself (`deploy/helm/wardyn/values.yaml`, the
  `persistence` block) — `fs` on the PVC, or `off` — the *opposite* of wardynd's
  own `pg` default the compose recipe relies on to sweep asciicasts up with the
  database. With `persistence.enabled=false` (the shipped default) the store is
  `off` and replay is off, so there is nothing to lose. Turn
  `persistence` on and every asciicast lives on that PVC alone: `pg_dump` will
  not carry them, and the PVC needs its own snapshot. Setting
  `env.WARDYN_RECORDING_STORE=pg` instead puts them back in the dump.
- **The age key is a Secret, not a `.env` line.** See below; still the item that
  makes the difference between a restorable dump and a file of undecryptable
  ciphertext.
- **Pending audit fallback is a backup target.** `WARDYN_AUDIT_SPOOL` renders to
  `/tmp/audit-spool.jsonl` on a stock install and onto the PVC beside the
  recordings once `persistence` is on (`templates/deployment.yaml`), so turning
  persistence on includes the spool and its sidecars in that volume's snapshot.
  Undrained events and quarantined lines can be absent from `pg_dump`; preserve
  them with the matching database backup and restore them before startup. Follow
  [Audit fallback recovery](#audit-fallback-recovery), including its cursor and
  snapshot-consistency limits. On the default `emptyDir`, scaling to zero or
  replacing the pod loses these files: drain or preserve them first.

### Restore: rehearse into a scratch database first

Two steps are Wardyn's, and both are cheap:

**1. Preserve any current fallback state, then stop the control plane.** On the
default ephemeral spool, copy any pending spool/sidecars and quarantine before
scaling to zero; deleting the pod discards them. Nothing may run against the
database mid-restore. The compose recipe's
"start Postgres alone" becomes a scale-to-zero, which on a chart install is the
whole control plane:

```console
$ kubectl -n wardyn scale deployment/wardyn --replicas=0
deployment.apps/wardyn scaled
$ kubectl -n wardyn rollout status deployment/wardyn --timeout=60s
deployment "wardyn" successfully rolled out
```

**2. The age key must already be in place** — the same "age key FIRST" ordering as
compose (next section for what happens when it is not).

Then restore the way your Postgres restores, keeping `-v ON_ERROR_STOP=1` —
without it `psql` walks past a failed statement and still exits `0`, leaving a
half-loaded database that looks clean. Before doing that to real data, **rehearse
the dump into a scratch database** — it proves the file loads, costs nothing, and
touches no live row:

```console
$ kubectl -n wardyn exec deploy/postgres -- createdb -U wardyn wardyn_restorecheck
$ kubectl -n wardyn exec -i deploy/postgres -- \
    psql -U wardyn -v ON_ERROR_STOP=1 -q wardyn_restorecheck < wardyn-2026-08-20.sql
$ echo $?
0
$ kubectl -n wardyn exec deploy/postgres -- psql -U wardyn -d wardyn_restorecheck \
    -c "SELECT count(*) AS audit_events FROM audit_events;" \
    -c "SELECT count(*) AS migrations FROM schema_migrations;" \
    -c "SELECT name FROM secrets ORDER BY name;"
 audit_events
--------------
            9
(1 row)

 migrations
------------
         43
(1 row)

        name
---------------------
 wardyn-signing-key
 wardyn-ssh-host-key
(2 rows)

$ kubectl -n wardyn exec deploy/postgres -- dropdb -U wardyn wardyn_restorecheck
```

Read that last query closely: `secrets` is where the control plane's own keys live
— the signing key and, with `ssh.enabled`, the gateway host key — so those two
rows returning is the difference between a restored database and a restored
*install*. Present is not decryptable, though, and no `SELECT` tells you which you
have. On k8s the control plane answers that the moment you scale back to one,
because it reads its own signing key out of that table before it serves anything:
a key that does not match the restored ciphertext is a failed rollout, not a
surprise at first use — the next section.

### The age key is a Secret, and the default loses your secrets on boot 2

Two supported wirings, and the chart refuses both ways of getting it wrong
(`deploy/helm/wardyn/templates/secret.yaml`):

| `postgres.dsn` mode | age key value | What injects `WARDYN_AGE_KEY` |
|---|---|---|
| inline (`dsn.value`) | `secrets.ageKey` | 🟢 the Secret the chart creates |
| external (`dsn.secretRef.name`) | an `age-key` entry in **that** Secret | 🟢 `secrets.ageKeyFromSecret=true` |
| external | `secrets.ageKey` | ⛔ **render fails** — it would be silently dropped |
| external | none wired at all | ⛔ **render fails** — unless `secrets.allowEphemeralAgeKey=true` |

`secrets.ageKey` defaults to empty and `ageKeyFromSecret` to `false`, so an
external-DSN install wiring neither would get **no** stable identity: wardynd
mints an ephemeral one per boot. That install works perfectly once; its second
boot cannot decrypt what its first wrote, and because the control plane readies
its stored secrets during startup, before it loads its own keys
(`convertSecretStore`, `cmd/wardynd/secret_store.go`, then `loadOrCreateSecret`,
`cmd/wardynd/boot_keys.go`), it fails closed there, before serving — a
`CrashLoopBackOff`, not a degraded pod. Hence the
fourth row: the chart stops the install at render.

```console
$ helm template wardyn ./deploy/helm/wardyn \
    --set postgres.dsn.secretRef.name=wardyn-db --set auth.adminToken.value=t
Error: execution error at (wardyn/templates/secret.yaml): wardyn:
postgres.dsn.secretRef.name="wardyn-db" is a PERSISTENT Postgres, but no age
identity is wired, so wardynd generates an ephemeral one at every boot. [...]
Throwaway install where losing every stored secret on restart is fine:
secrets.allowEphemeralAgeKey=true renders anyway.
```

Taking that escape hatch — `--set secrets.allowEphemeralAgeKey=true` — renders the
broken install, which then `CrashLoopBackOff`s on boot 2 with:

```console
$ kubectl -n wardyn logs -l app.kubernetes.io/name=wardyn --tail=2
WARN wardynd: generated ephemeral age identity; secrets are LOST on restart. Persist one with `wardynd -gen-age-key` + set WARDYN_AGE_KEY public_recipient=age1qgu93czj2ksk2g3j4x3rq52kyaw5xkjetd7g38cn63gdl2az4eqsyztpgs
ERROR wardynd: fatal err="refusing to start: WARDYN_AGE_KEY is unset, but […] stored secrets are sealed under an age key — an ephemeral key would make every one unreadable; […] delete them (DELETE FROM secrets WHERE enc_version=0 OR kek_id LIKE 'local:%' OR kek_id LIKE 'local/%') and boot with a persistent key from `wardynd -gen-age-key`"
```

That is the correct behaviour — the boot refuses rather than mint a fresh key
over rows no key it holds can open, which would strand them permanently instead
of loudly. But it is unrecoverable from inside the cluster:
[Rotating the age key](operations/secrets-and-keys.md#rotating-the-age-key) re-encrypts a store you can still
*read*, and the key that reads this one is exactly what is missing. When that key
was a Secret you deleted, the fix is "put the original Secret back", never
"generate a new one". Back the Secret up off-cluster, wherever the DSN Secret is
backed up, and treat deleting it as equivalent to deleting the database.

When the key was ephemeral there is no original to put back: it lived only in
the first pod's memory, so every row it sealed is lost. Delete those rows, wire a
persistent key (`wardynd -gen-age-key`), and start again; Wardyn's own boot keys
are among the rows and are minted afresh, and every stored secret has to be set
again:

```sh
psql "$WARDYN_PG_DSN" -c "DELETE FROM secrets WHERE enc_version=0 OR kek_id LIKE 'local:%' OR kek_id LIKE 'local/%'"
```

If `WARDYN_PLATFORM_KEY_FILE` was set, keep that file: the boot-key rows under it
are not lost — change `OR kek_id LIKE 'local/%'` to `OR kek_id LIKE 'local/cred:%'`
in the statement, so it keeps the `local/platform:` rows.

**Rotating it on k8s** uses the same runbook
([Rotating the age key](operations/secrets-and-keys.md#rotating-the-age-key)), with two differences.

First, "stop the daemon" is a scale-to-zero — the Deployment *is* the daemon:

```sh
kubectl -n wardyn scale deploy/wardyn --replicas=0
kubectl -n wardyn wait --for=delete pod -l app.kubernetes.io/name=wardyn --timeout=2m
```

Second, **run the rotation from outside the cluster, not in a Pod.**
`-rotate-age-key` needs only `WARDYN_PG_DSN`, `WARDYN_AGE_KEY` and a writable
path for the key file — port-forward Postgres and run the same `wardynd` binary
on your workstation, exactly as the compose runbook does. A one-shot Pod is the
awkward path: the wardynd image is distroless with no shell
(`deploy/compose/Dockerfile.wardynd`), so there is nothing in it to seed the key
file with or copy the result back out, and the `.bak` would die with the Pod.

```sh
kubectl -n wardyn port-forward svc/<your-postgres> 15432:5432 &
WARDYN_PG_DSN='postgres://…@127.0.0.1:15432/wardyn?sslmode=disable' \
  WARDYN_AGE_KEY="$(kubectl -n wardyn get secret <name> -o jsonpath='{.data.age-key}' | base64 -d)" \
  ./bin/wardynd -rotate-age-key ~/.wardyn/age.key
```

Then write the new value into the Secret the Deployment reads
(`secrets.ageKey`, or the `age-key` entry of the external-DSN Secret — see the
table above) and scale back up. Keep the old Secret value **and** the Postgres
backup until the rotated deployment is confirmed working; on k8s those are the
rollback, since the Secret, not `<key-file>.bak`, is what the chart reads.

### The SSH host key survives restarts — because the age key does

The SSH gateway (`ssh.enabled`) carries no host key in the chart or in a volume.
wardynd generates an ed25519 key on first boot and persists it into the secret
store under `wardyn-ssh-host-key` (`loadOrCreateSSHHostKey`,
`cmd/wardynd/boot_keys.go`), through the same `loadOrCreateSecret` path as the signing
key. So the fingerprint a client pins is stable across pod churn with no operator
action — the same value survived a rolling `helm upgrade` and a full
scale-to-zero-and-back on the quickstart cluster:

```console
$ curl -s http://127.0.0.1:8080/healthz | jq -c .ssh
{"advertise_addr":"127.0.0.1:2222","enabled":true,"host_key_fingerprint":"SHA256:JEFfrvMMhOTqAMpkJNEqFz3H0hcgoox4swhRkIYIan8"}

$ kubectl -n wardyn logs deploy/wardyn | grep "ssh gateway listening"
INFO wardynd: ssh gateway listening listen=:2222 advertise=127.0.0.1:2222 host_key_fingerprint=SHA256:JEFfrvMMhOTqAMpkJNEqFz3H0hcgoox4swhRkIYIan8
```

`/healthz` is anonymous, so that fingerprint is publishable to the people who
will connect — see [SSH.md](SSH.md).

The dependency runs one way: **the host key is exactly as stable as the age
key.** Persist the age key and clients never see a fingerprint change. Lose it
and the pod crash-loops on the previous section's error before the gateway ever
listens, so clients get a connection refused, never a silently different host key
— strictly better than the man-in-the-middle warning a re-minted key would
produce, and why `loadOrCreateSecret`'s fail-closed branch matters here.

### The UI-sandbox gateway: a per-run origin is the production default

If `uiSandbox.enabled` is on ([deploy/helm/wardyn/README.md](../deploy/helm/wardyn/README.md#ui-sandbox-gateway)),
set `uiSandbox.originTemplate` (`WARDYN_UI_SANDBOX_ORIGIN_TEMPLATE`) too — the
documented production default, not an optional extra. Leaving it unset puts every
run's relayed app on the SAME browser origin, separated only by a path-scoped
cookie; that shared-origin mode is a published residual
([THREAT-MODEL.md](../threatmodel/THREAT-MODEL.md) §5 #18), tolerable for a
single-tenant demo cluster but not for a multi-tenant or production install.
Setting the template needs wildcard DNS and a wildcard certificate for the
gateway's hostname (e.g. `*.ui.example.com`) — the one-time cost that buys every
run its own origin, with an enter on any other host refused outright. Full
recipe, including the wildcard Ingress, in
[docs/UI-SANDBOXES.md §4](UI-SANDBOXES.md#4-deployment) and the chart section
linked above.

### User drives on Kubernetes

A **user drive** is per-person storage a run mounts at `/home/agent/drive`. On
this substrate it is always a PersistentVolumeClaim — a pod cannot bind a host
path, and Pod Security Standards forbids `hostPath` at Baseline and Restricted
alike, so no drive backend offers one. As on Docker, `storage.user_drive.disabled`
is answered identically by all three surfaces — launch 422, `POST /drives/preview`
the same 422, and `GET /me` no allocation — so the console never offers a claim
this deployment will not bind (see "Turning drives OFF deployment-wide").

**Two backends, two lifecycles.** A **managed** drive (`k8s_pvc`) is one claim
per person, named `wardyn-drive-<drive-slug>-<home>` (`<drive-slug>` = the
drive's name lowercased, every run of characters outside `a-z0-9` folded to one
`-`, at most 40 characters), created by wardynd on the first run that mounts
it — `accessModes: [ReadWriteOnce]`, the allocation as
`resources.requests.storage`, and the drive's own storage class when it has one
(empty = the cluster default). A **share** (`k8s_pvc_static`) is a claim an admin
provisioned — typically over an NFS/SMB export — and wardynd only ever looks it
up by name. A missing one fails the run with *"your drive's volume is not
provisioned on this cluster"* rather than being invented as an empty volume where
somebody's files were meant to be.

**`home_template` on a `k8s_pvc_static` share.** The claim you provision is
yours, but its NAME is minted by Wardyn exactly as for a managed drive, so
`hash` is allowed here and is the recommended template: run the preview
endpoint for a member to get the exact claim name to pre-create, and nothing
about that member's identity is published in it. `email_local` is refused on
this backend for the same reason it is refused on a managed one — two
addresses that share the part before the `@` would be allocated ONE claim.
`sub` remains available for operators who need to recognise the claims they
pre-provision by sight. Stamping `wardyn.subject=<the preview's subject
digest>` on a claim you pre-create makes the runner refuse to bind it for
anyone else.

**RBAC is two verbs.** `drives.enabled=true` (renamed from `userDrives.enabled`
in 0.8) adds exactly
`persistentvolumeclaims: ["get","create"]` to the namespaced runner Role
(`deploy/helm/wardyn/templates/rbac.yaml`): `get` because a claim is always
resolved by name first, and is all a share ever needs; `create` for a managed
drive's first use. Leave it on for **any** drive at all. With it off, EVERY
drive's run fails at dispatch — the lookup is the first call a drive makes and a
share makes no other — and the run's failure hint names the switch. Both the Get
and the Create map their 403 onto that one refusal, because the apiserver's own
"cannot get resource" text names nothing an operator can flip. A 403 has a second
cause that no status code distinguishes from the first and that takes the
opposite remedy — a namespace `ResourceQuota` refusing the claim — so wardynd
picks which of the two the hint names, on the `exceeded quota` substring the
quota admission plugin always emits. A hint naming `ResourceQuota` means the
quota, and RBAC is not the problem.

**The failure hint does not quote the apiserver, and the daemon log does.** A raw
403 reads `User "system:serviceaccount:<ns>:<sa>" cannot get resource ...`, and a
run's failure hint is read by the member whose run failed — so the hint carries
the claim name and one remedy, and nothing that names this cluster. The
apiserver's own sentence goes to the daemon log instead, with the verb, the
claim, the namespace, the drive id and the refusal verbatim; grep it for `the
apiserver refused a drive claim`. The claim name appears in both halves, so a
member's report of a failed run joins to the full text without anybody having
been handed the runs namespace or the runner's ServiceAccount name.

**Renaming a drive orphans its claims, and Wardyn will not clean that up.** A
claim's name folds the drive's NAME into a slug
(`wardyn-drive-<drive-slug>-<home>`), so renaming a drive in the console changes
the name every FUTURE claim is
created under. The claims already provisioned keep their old names, keep the
member data in them, and are never looked up again — the next run for each
person provisions a fresh, empty claim under the new name. Nothing deletes the
old ones, on purpose: no run path, teardown or sweep can reach a claim — the
only delete in the product is the operator's explicit reclaim — and a rename
must never be able to destroy storage. The `wardyn.drive` label carries the drive's row **id**
rather than its name precisely so the orphans stay findable:

```sh
kubectl -n <runsNamespace> get pvc -l wardyn.drive=<drive-id>
```

Everything that comes back under a name that is not
`wardyn-drive-<new drive-slug>-*` predates the rename. Move the data (`kubectl
cp`, or a snapshot restore into the new claim) and reclaim the old claim with
the `delete pvc` above. The cheap
alternative is not renaming a drive that has claims.

**The API refuses the edit; the console has no way to confirm it.** An
identity-affecting `PUT /api/v1/drives/{id}` on a drive that already has
allocations answers **`409`** (`driveRehomeGuard`), naming what changes and how
many allocations move: *"this drive is allocated to N subjects and this change
re-homes them: name "old" → "new". … re-send as PUT
/drives/{id}?confirm=rehome."* Five fields count as identity-affecting —
`backend`, `home_template`, `host_root`, a `name` that folds to a **different
slug** (a purely cosmetic rename that folds to the same slug is not refused,
and neither is any edit to a drive nothing is allocated from — and on an
`object_scheme: id` drive a rename never counts at all, because the drive's
name plays no part in that scheme's minted name), and `object_scheme` itself
(see "Legacy rows keep their old object name, permanently" below).

Confirming is an API action, deliberately:

```sh
curl -fsS -X PUT "$WARDYN_URL/api/v1/drives/<drive-id>?confirm=rehome" \
  -H "Authorization: Bearer $WARDYN_ADMIN_TOKEN" \
  -H 'Content-Type: application/json' --data @drive.json
```

The console PUTs with no query parameter and renders the `409` as a save
refusal, so an admin cannot click past this — which is the point: plan the data
move (the `kubectl cp` / snapshot restore above) first, then confirm. The
resulting `drive.write` audit row carries **`rehomed: true`**, so an auditor can
tell a storage re-point from a cosmetic edit after the fact
([AUDIT-ACTIONS.md](AUDIT-ACTIONS.md)). Treat the rename field as an operator
action with a runbook, not a label edit.

**An existing claim's SHAPE is reused as it is, and logged rather than
enforced.** A managed claim is looked up by name and mounted whatever its spec
says. If its shape disagrees with the drive row — a different storage class, a
different `requests.storage`, an access mode that is not `ReadWriteOnce` —
wardynd logs one warning naming the claim and every disagreement, and mounts it
anyway. That is deliberate: the claim is the member's data, a PVC request cannot
be shrunk, and refusing the run would mean an admin editing an allocation in the
console breaks every existing member's runs. Grep the daemon log for `disagrees
with the drive` when a console size and a pod's actual volume do not match.

**A LOWERED CEILING IS DRIFT, and drift is a warning.** A managed claim's
`requests.storage` is the size the drive resolved to on the run that first
provisioned it. Lower `storage.user_drive.max_size_mib` (or a profile's
`max_drive_size_mib`) afterwards and the resolver clamps the number from the
next run onward — so the claim now asks for more than the drive says, and that
disagreement is reported by the same warning as any other: *"disagrees with the
drive"*, naming the claim and `request is 10Gi, the drive's allocation is 2048
MiB`. Nothing shrinks and nothing is refused. **A PVC request cannot be reduced
in place**, no run path may delete a claim (the only delete is the operator's
explicit reclaim), and refusing the run would mean an admin editing a ceiling breaks every existing member's runs — so the
product's answer to a lowered ceiling is a smaller number on the next
allocation, plus this warning on the claims that predate it. To actually reclaim
the space, plan the data move (the `kubectl cp` / snapshot recipes above) and
re-provision.

**Two states are refusals, not warnings.** A claim that is **Terminating** fails
the run outright: a pod mounting a claim under deletion never schedules, and
re-creating it under the same name would undo the reclaim somebody is in the
middle of. So does a claim whose IDENTITY labels are not this run's — a managed
claim whose `wardyn.drive` or `wardyn.home` names a different pair, or whose
`wardyn.subject` names a different person (that third label is checked only when
it is PRESENT, so claims stamped before it existed still mount), or a share
whose claim turns out to carry `wardyn.managed=true` (i.e. it is one person's
managed drive, not an admin's share).

**Legacy rows keep their old object name, permanently — and only they carry
this collision.** A drive's `object_scheme` (migration `0067`) decides which
half of the minted name carries the drive: `slug` — every drive registered
before `0067` shipped, forever, since neither substrate can rename a storage
object and Wardyn will not copy bytes between an old object and a new one to
"fix" a row in place — folds the drive's NAME to a DNS-1123 fragment
(`types.DriveSlug`) at a VARIABLE offset before `<home>`, and that is the
collision the object name cannot rule out: `wardyn-drive-<drive-slug>-<home>`
joins two variable-width fields with the separator both of them admit, so
drive `eng` + home `us-bob` and drive `eng-us` + home `bob` resolve to the
same claim name. Wardyn holds no `delete` verb and cannot repair the
collision, so it refuses the run rather than mount one member's private drive
inside another member's agent. The fix on a `slug` drive is to rename one of
the two drives (see the rename caveat above) or to give the colliding people
distinct home names. Every drive registered from `0067` onward is minted
`id` instead — `wardyn-drive-<drive-id-hex>-<home>` — and the id is
always exactly 32 lowercase hex characters, so `<home>` starts at a FIXED
offset no drive name or home override can move; this whole collision does not
exist for an `id`-scheme drive, by construction rather than by convention.
`GET /api/v1/drives/{id}` reports which scheme a drive is on.

Both refusals also cover the loser of a create race. Two first runs can collide
inside the lookup→create window, and the loser's create comes back
`AlreadyExists`; it re-reads the claim that won rather than mounting on the
strength of the name, so the identity and Terminating answers are the same ones,
one moment later. If the winning claim has been deleted again by the time the
loser looks — a reclaim landing mid-dispatch — the run is refused with *"your
drive's volume claim was deleted while your run was starting"*, and starting it
again is the whole remedy: nothing re-creates a claim somebody is reclaiming.

**Restoring a managed claim by hand: it must carry the labels.** Unlike Docker, a
label-less claim is FOREIGN here (`driveClaimIdentity`): a claim you create
yourself under a member's name — from a snapshot, or to move data after a
rename — needs `wardyn.managed=true`, `wardyn.drive=<drive-id>` and
`wardyn.home=<home>` (leave `wardyn.subject` off), or every run on it fails as
*"drive: your drive's volume is not the one allocated to you — ask an admin"*.
**The member's hint carries no evidence, and the daemon log carries all of it**:
the run's failure hint is read by the person whose run failed, and the label
comparison names a claim, a namespace and — for `wardyn.subject` — a digest of
*another* person. Grep the daemon log for `a drive claim is not this run's` for
the claim, the namespace, the deciding label and both values; the unprovisioned
share is the same split, under `a share drive's claim is not provisioned`.

**`ReadWriteOnce` binds a volume to one NODE — not to one pod, and nothing
schedules around it.** A managed drive is provisioned RWO, which permits any
number of pods to mount it *as long as they land on the same node*
([Kubernetes: access modes](https://kubernetes.io/docs/concepts/storage/persistent-volumes/#access-modes)
— for one-pod-at-a-time you need `ReadWriteOncePod`). kube-scheduler enforces
only that stricter mode: its `volumerestrictions` plugin has an
`ErrReasonReadWriteOncePodConflict` and **no ReadWriteOnce equivalent**. So a
person's second concurrent run is not co-located and is not held back — it is
scheduled like any other pod, and then one of three things happens:

- **Same node** (or a bound PV whose node affinity pins the scheduler there, which
  is what a topology-aware CSI provisioner sets): it mounts, and two sandboxes
  write one home concurrently. Correctness is then the agents' problem, not
  Kubernetes'.
- **Different node:** the pod is *scheduled* and stalls in `ContainerCreating`
  while the attach-detach controller waits for a detach that is not coming. The
  evidence is a `FailedAttachVolume` **warning event on the pod**, naming the
  pod(s) already using the volume — `kubectl -n <runsNamespace> describe pod
  <pod>` is where to read it. wardynd's failure hint does **not** carry this: the
  hint is read from the `PodScheduled` condition, which is `True` here, so the
  run reports the bare dispatch-wait timeout.
- **Node affinity excludes every candidate:** the pod stays Pending with
  `node(s) didn't match PersistentVolume's node affinity`, which *does* land in
  `PodScheduled` and therefore *does* reach the run's failure hint.

`0/N nodes are available: pod has unbound immediate PersistentVolumeClaims` is a
**different failure and does not describe any of the above** — the scheduler
emits it in PreFilter for claims that never bound at all: a storage class with no
provisioner, or no default class on the cluster for a drive that names none.

If members routinely run several sandboxes at once, provision the drive's
class as `ReadWriteMany` storage and pre-create the claims as a
`k8s_pvc_static` share; Wardyn's managed backend does not offer RWX, because a
concurrently-written shared home is a data-loss shape, not a feature.

**Backup.** A drive is *not* in `pg_dump` — the database holds the drive rows and
the allocations, never the bytes. Back the volumes up the way the cluster already
backs up claims: a `VolumeSnapshotClass` snapshot per claim, or
`kubectl -n <ns> cp <pod>:/home/agent/drive <dest>` from a pod that mounts one. A
share is backed up by whoever owns the export, not by Wardyn.

**Offboarding — the reclaim command.** Deleting the allocation in the console is
the product-side half and it deletes no data. Reclaiming the storage is one
deliberate command, by hand:

```sh
kubectl -n <runsNamespace> delete pvc wardyn-drive-<drive-slug>-<home>
```

or, when `drives.reclaim.enabled` is set, through the product's own verb
(`wardyn drive reclaim <drive-id> --subject <sign-in subject> --yes`, super-admin
only, audited, refused while a pod still mounts the claim) — see "Reclaiming a
departed person's storage" above. **With that value left at its default `false`
wardynd holds no `delete` verb on claims at all**, so the by-hand command is the
only path and nothing in the deployment can destroy a claim by accident.

The drive's `when a person leaves` column records the intent (`retain` or
`delete`) so the log says what the operator was told to do; the console's drive
preview prints the object name for a principal — paste the sign-in subject
FIRST: on a `hash` drive the name keys on the first claim, and the API's
`home_subject` says which claim it used (the console does not yet show it). The
claim carries `wardyn.managed`, `wardyn.drive` (the drive's
row **id**, not its name, so the claims a rename orphans stay findable with the
`get pvc -l wardyn.drive=<drive-id>` above) and `wardyn.home` labels — the same
pair the Docker driver stamps on a managed volume — and, deliberately, **no
`wardyn.run-id`**, so the per-run teardown sweep (a `DeleteCollection` selecting
on exactly that label) cannot reach it. That pair is also what the driver checks
before it mounts anything: see the two refusals above.

**Ownership: fsGroup is a MANAGED-claim field, and a share never gets it.** A
pod carrying a **managed** (`k8s_pvc`) drive carries `fsGroup: 1000` (a GROUP id
— it happens to equal the uid every agent image runs as, but this field can
never make a volume user-owned) with `fsGroupChangePolicy: OnRootMismatch`;
`Always` would recursively chown a large drive on every single run. That is
correct by construction for a managed claim: it is provisioned **empty**, it
belongs to **one** principal, and a root-owned volume root is unwritable for uid
1000 — while the control plane must never chown volume state itself.

A pod carrying a **share** (`k8s_pvc_static`) drive carries **no `fsGroup` at
all**, deliberately. The tempting justification for setting it — that the
kubelet does not apply fsGroup to an NFS-type volume — is **false** for the
upstream CSI NFS driver (`kubernetes-csi/csi-driver-nfs`), which ships
`fsGroupPolicy: File`: File means Kubernetes may use fsGroup to change
permissions and ownership of the volume *regardless of fstype or access mode*.
(`ReadWriteOnceWithFSType`, the policy that really is limited to block storage,
is only the DEFAULT for a driver that declares none.) `OnRootMismatch` narrows
**when**, never **what** — the first run whose export root is not already gid
1000 walks the volume and re-owns what it finds, which on a share is **other
people's files**. So the gate is on the drive's kind, not on a backend list
(`applyDriveToPod`, `internal/runner/k8s/drives.go`, over
`types.DriveBackend.Kind`), and a backend this binary does not recognise reads
as a share and gets no fsGroup either — the fail-closed direction here.

A share's ownership is therefore **the export's own uid/gid mapping and nothing
else** — a Wardyn-dedicated export with `all_squash,anonuid=1000,anongid=1000`,
or per-user `0700` subdirectories. That recipe is the mechanism, not a fallback
for when fsGroup does not fire. Expect a read-only mount where an existing
corporate home is owned by a different uid.

**gVisor wants `directfs` off for a drive, and today only the NODE FLAG
delivers it.** The Wall (CC2) and Vault (CC3) tiers run the agent pod under a
RuntimeClass; when its handler is `runsc`, gVisor's `directfs` has the gofer
donate a file descriptor per mount point to the sandbox, which then operates on
the file directly. That is right for a block PVC and wrong for a network-backed
export — a `k8s_pvc_static` share over NFS/SMB.

**Turn it off per node.** `--directfs=false` in the runsc shim's own config
(`/etc/containerd/runsc.toml`, or the `runtimeArgs` a node image bakes in), on
the nodes that run drive pods. This is the whole remedy; there is no working
per-pod alternative to weigh it against.

> ⚠️ **The per-mount annotation wardynd stamps is currently inert — do not rely
> on it.** wardynd sets `dev.gvisor.spec.mount.drive.directfs: "off"` on every
> drive pod whose resolved handler is `runsc`, and runsc **discards it**. A
> gVisor mount hint is only kept when it carries `share`, `source` *and* `type`
> alongside the option; a hint missing any of them is dropped with *"ignoring
> mount annotations for … because of missing required field(s)"*
> ([`runsc/boot/mount_hints.go`](https://github.com/google/gvisor/blob/release-20260824.0/runsc/boot/mount_hints.go),
> `NewPodMountHints`). Nor is the name in the key what binds a hint to a mount:
> `FindMount` matches on the mount's **source path**, which for a CSI-provisioned
> claim is a per-pod path the kubelet generates and no static annotation can name
> in advance. Completing the annotation is a code change, not a configuration
> one, and this document will not claim it works until it does. Separately, and
> upstream of all of that, containerd forwards `dev.gvisor.*` annotations at all
> only where the node's runsc runtime section carries
> `pod_annotations = ["dev.gvisor.*"]` in `/etc/containerd/config.toml`. See
> [gVisor's containerd configuration guide](https://gvisor.dev/docs/user_guide/containerd/configuration/).

The companion caveat is CACHING, and it cuts the other way. runsc serves bind
mounts `shared` by default (`--file-access-mounts=shared`), revalidating against
the host because it cannot assume exclusive access. An operator who has set
`--file-access-mounts=exclusive` for throughput must **not** do so on nodes that
run drive pods over a share other writers touch: exclusive mode caches
aggressively, and a file another writer changes is not seen. A managed
(`k8s_pvc`) drive is **not** exempt from this. RWO makes the volume exclusive to
a NODE, not to a pod — see the `ReadWriteOnce` paragraph above — so two
concurrent runs by the same person on that node are two sandboxes caching one
home aggressively and not seeing each other's writes. Exclusive mode is safe for
managed drives only where a member cannot have two runs on the same node at
once. See [gVisor's filesystem guide](https://gvisor.dev/docs/user_guide/filesystem/).

**Size is an allocation, not a limit**, and the product says so in one frozen
sentence: *"Wardyn never enforces a drive's size itself. On Kubernetes the size
is the volume request and the storage class decides whether it binds — block
disks do, network-share provisioners do not. On Docker a managed drive has no
byte cap, the same gap disk_mib has. A share is bounded by its own quota. The
size you see is the allocation, not a guarantee."* That is the `enforcement`
vocabulary this feature introduces — `filesystem` (the filesystem itself
refuses the write, an XFS project quota), `request` (a scheduling request; a
block storage class binds it, a network-share provisioner accepts it and
enforces nothing), `external` (something outside Wardyn binds it, such as a
NAS's own quota), `none` (nothing binds it), and, since 0.7.2, `eviction` (the
kubelet measures the pod's usage periodically and evicts it once it exceeds
the limit — the write itself is never refused; since 0.7.5 that metered usage
includes the agent's `/tmp` and workdir writes, which land in
`emptyDir` volumes the kubelet meters; what the agent writes anywhere else — the rest of `$HOME`
including the toolchain caches, and any authored target outside the workdir — still does not, see
the `DiskMiB` gap below): a managed claim is `request`,
a share is `external`. It is the same honesty the `DiskMiB` gap below is
written with, and the two vocabularies **have now converged on one**:
`disk_mib` reports `filesystem` on Docker when the storage driver can enforce
a per-container quota, `eviction` on Kubernetes, and `none` on Docker when the
driver cannot enforce a quota at all — which covers two different outcomes
under one word: a driver that takes no size option (`vfs`, `fuse-overlayfs`)
runs the request UNCAPPED with a warning, while overlay2 over a non-xfs
backing filesystem is handed the option anyway and the daemon REFUSES the
create, so that run fails closed instead.

## High availability

`ha.enabled` runs two or more wardynd replicas on Kubernetes so that the loss of a
node does not stop the control plane. It is one documented switch: the chart sets
`WARDYN_HA=true`, the daemon skips the single-instance lock, and both refuse every
configuration that would make a second replica unsafe. It is a node-failure
posture, not a way to scale a busy deployment past one replica's capacity, and it
is **not zero-downtime upgrades**: the Deployment strategy stays `Recreate`, so an
upgrade stops the old replicas before it starts the new ones.

### Turning it on

```
helm upgrade --install wardyn ./deploy/helm/wardyn \
  --set ha.enabled=true --set replicas=2 \
  --set env.WARDYN_RECORDING_STORE=pg \
  --set k8s.enabled=true ... # the Kubernetes runner and its other required values
```

What the switch needs, and refuses to render or boot without:

- **The Kubernetes runner.** The Docker driver's sandbox tracking maps (`agentExecs`,
  `pending`, `mainProc`, `creating` in `internal/runner/docker/driver.go`) are
  per-process by design, and a teardown served by a replica that did not create
  the sandbox cannot see them, so a container can survive the kill it was supposed
  to die from. HA is **supported on Kubernetes only**. `WARDYN_HA=true` on any other
  runner exits non-zero at boot, naming the reason; compose pins `container_name`
  and rejects `--scale` outright.
- **A recording store every replica reads.** `WARDYN_RECORDING_STORE` is `pg` or
  `off`. The chart derives `fs` from `persistence.enabled` and `off` without it, so
  an HA install sets `env.WARDYN_RECORDING_STORE=pg` to record. The chart reads the
  key from both `env` and `extraEnv` (`extraEnv` renders after `env` and wins), and
  refuses `fs` from either; it also refuses `persistence.enabled=true`, whose
  ReadWriteOnce volume exists for the per-pod `fs` store.
- **An audit spool on the per-pod `tmp` emptyDir.** The chart renders
  `WARDYN_AUDIT_SPOOL=/tmp/audit-spool.jsonl` and refuses a `WARDYN_AUDIT_SPOOL`
  set in `env` or `extraEnv` anywhere outside `/tmp`.
- **A database pool of at least 3 connections.** The sweeper leader election holds
  one connection for the process lifetime; below that every replica would sweep
  with no election and no fencing. `WARDYN_HA=true` with `pool_max_conns` under 3
  in `WARDYN_PG_DSN` exits non-zero at boot, naming the value and the minimum.
- **A PodDisruptionBudget** (`minAvailable: 1`) and a preferred **pod anti-affinity**
  across nodes (soft, so a one-node cluster still schedules every replica; an
  `affinity.podAntiAffinity` of your own replaces it).

`allowMultiReplica` and the `-allow-multi-instance` flag are **removed**, a
documented clean break: a values file that still sets `allowMultiReplica=true` is
refused at render with a pointer to `ha.enabled`, and the flag is refused at boot
with a pointer to `WARDYN_HA`. An install that set them was running an unsupported
topology. `replicas > 1` without `ha.enabled` is refused too. The chart's refusals
are render-time only (`kubectl scale` bypasses them), which is why the boot
refusal exists: a Deployment edited by hand to `WARDYN_HA=true` on the Docker
runner, or with the `fs` store, does not start. Without `WARDYN_HA` boot is
unchanged: one instance, the single-instance lock taken, and a second wardynd
against the same database refuses to start.

`GET /setup/status` carries three rows while `WARDYN_HA` is on: `ha_mode`,
`recording_store_shared` and `mask_registry_shared`. The last fails when this
replica's `LISTEN` connection is down or its mask-sync cursor is behind the
committed generation after one read, and while it fails the replica refuses
recording uploads and new attaches and replaces live output with a placeholder.

### What the replicas share

Everything here is in Postgres, so a request served by any replica sees the same
state.

- **the secret-masking registry** (`internal/secretmask`). A value a run was
  given is masked, verbatim, in every artefact Wardyn persists or relays for that
  run, whichever replica serves the request and whether or not any replica
  restarted. (Until 0.8.6 the registry was an in-memory map that failed open: a
  recording uploaded to a replica other than the one that handled the run's
  injection, or after a restart, was persisted with live credentials in it and a
  `success` audit event. That is why a second replica was never supported.)
  **A registry miss fails closed for runs dispatched by 0.8.6.**
  Dispatch commits each run's *masking manifest* to Postgres before the sandbox
  can see a value: the exact bytes of every rendering the run received (its
  workspace and inspection secrets and its Azure DevOps run token in all three
  renderings, and every run token minted later), sealed under the run owner's
  per-subject `cred` key (tables `run_mask_manifest` and `run_mask_values`;
  destroying that key leaves the rows undecryptable). A restarted or second
  wardynd loads it, so a value rotated after dispatch is still masked. Five doors
  — the recording upload, the live attach, the exec relay, the SSH shell and the
  live output read — answer `503` `mask_state_unavailable` for a run whose manifest
  they cannot prove complete (see the reason table above), and audit rows of such
  a run carry `"mask_scope":"globals_only"`. **After any restart, a run that
  predates 0.8.6 has no manifest and is refused at those doors** (attach, SSH
  shell, exec output and recording upload) until it ends; runs dispatched by 0.8.6
  survive restarts. **Since 0.8.6 the registry itself is in Postgres.** A value
  registered at *injection* time (a minted GitHub token, an injected API key) is
  committed to `mask_values` (migration `0124_mask_values`) before the call
  returns, and so is each credential a person's AWS SSO or Azure DevOps sign-in
  registers process-wide (its access and refresh tokens, current and retired);
  each is sealed under its owner's `cred` key, and the in-memory maps are every
  replica's cache of that table. This holds at `replicas: 1` too: there is no
  in-memory-only mode, and wardynd refuses to start when it cannot read the table.
  A registration that cannot be committed fails the call that would hand the
  value out (`503` `mask_state_unavailable`). Replicas read the table by a
  generation cursor (`mask_gen`, taken in the registering transaction, so commit
  order is generation order and a reader has read every generation below the one
  it holds); a consumer about to mask a chunk waits for a read that began after
  the chunk arrived, at most one read per 50 ms per replica. Neither a live
  session's recording (web terminal and SSH shell) nor a batch run's output tail
  holds its source to that rate: the relay, or the runner's copy of the agent's
  output, hands the bytes to the masker without waiting, and everything that
  arrived during one read is masked together after the next. The terminal and
  the agent run at bandwidth, and the recording or tail lags them by about two
  reads. Up to 512 KiB per session or run waits for the masker; past that the
  relay or copy stops reading the sandbox until it drains. `NOTIFY` on
  `wardyn_mask` is only a hint (each replica holds one connection of its own, outside
  `pool_max_conns`, listening for it), so a missed notification costs nothing. With
  Postgres unreachable a live chunk (for a recording, the batch) is replaced by
  `<secret-hidden>`, a recording upload answers `503` and a new attach is refused. An eviction (a deleted
  credential, a value retired past the grace, a run's purge, an erasure) is a
  tombstone whose ciphertext is gone in the same statement; tombstones are deleted
  an hour later, and a replica away longer reloads the table. The elected sweeper
  leader (`SweeperLeaderLockKey`) deletes a terminal run's `per_run` rows and
  manifest after `api.RunSecretGrace`; every replica's own run-secret sweep drops
  only its cache. A credential with no owner (the operator namespace) has no
  subject key and stays in the registering process; none exists today. A split
  migrator and app role install grants the app role `SELECT, INSERT, UPDATE,
  DELETE` on `mask_values`, `SELECT, UPDATE` on `mask_gen` and `DELETE` on
  `run_mask_manifest`. **SSH exec, SFTP and direct-tcpip were never masked**
  (`sshgateway_channels.go`), so none of them is covered by any of this.

Six OTHER pieces are now Postgres-backed, so they survive a crash and no longer
break under a second replica: single-use **attach tickets**, delete-on-read
**compose results**, and the **lifecycle reaper** (migration 0026 + a
`pg_try_advisory_lock` around the reap tick, which skips a tick it does not win);
**run watchers** (migration 0027 — each run's completion watcher is still an
in-process goroutine blocked on `Runner.Wait`
(`internal/api/runs_dispatch.go`), but it refreshes a Postgres lease every 30s and
every replica sweeps for stale leases every 60s, so a run orphaned by a pod that
never comes back is adopted by any live replica within **90–150 s** instead of
stranding forever — real latency, not instant, and slower than the same-pod
restart case `ReconcileOnBoot` handles alone); **session recordings** (migration
0028 — the process default is the Postgres-backed `pg` store, readable from any
replica; `WARDYN_RECORDING_STORE=fs` still selects the old per-pod directory); and
the **ground-truth token rotator** (`cmd/wardynd/gt_rotator.go`, leader-elected via
a Postgres advisory lock — a standby takes over within one ~30s backoff).

Six in-process locks are Postgres advisory locks too (`internal/db/locks.go`),
so the work they serialise holds across replicas: the **per-run operation lock**
(a revive holds it from its claim to its settle, and the lease sweep's
re-assertion of a kept run's stop and the watcher's reboot mark take it, so a
sweep that read a run's lost mark before a revive cannot stop the proxy that
revive just started, #1480; a sweep never waits for it, it skips a locked run
until the next pass), the **site-config** and **capability-enforcement** writers,
the **audit chain verify sweep**, and the **AWS SSO** and **Azure DevOps sign-in
refresh** single-flights, which keep one rotating refresh token from being
redeemed twice. They are taken in one order (run operation, run-token mint,
AWS SSO, Azure DevOps sign-in, the document locks, the audit sweep last), each
nested lock shares its caller's connection, and none proceeds unlocked: a lock
that cannot be taken answers `503` `lock_unavailable` and is retried. A refresh
that loses its lock (a failover) is cancelled, and its final write is a
compare-and-set on the stored credential, so it cannot overwrite a newer one;
a refresh already in flight can still race the new holder at the authority
across a failover (residual). The connection budget is in
[ENV.md](ENV.md) beside `pool_max_conns`.

Since 0.8.6 the rest of a run's live state is in Postgres too (migration
`0129_ha_live_state`), so a request served by any replica sees the same run:

- **the exec output tail** — the dispatching replica's masker writes each chunk it
  passes to `run_output_chunks` as well as to its memory ring, in batches off the
  runner's path (the bytes are masked before they are inserted; the table holds
  about one tail per run and the final `run_outputs` row's transaction deletes
  them). Any replica answers `GET /runs/{id}/output` for a live run from them. If
  the replica that held the tail dies, they are what is left: the run's row is
  written from them as an `incomplete` `capture_gap` when the substrate cannot be
  re-read (what the dead replica's masker still withheld is the gap), audited
  `run.output.finalize` with `from_chunks`. An erased run's chunks are deleted with
  its output and every later write is refused by the same tombstone.
- **the attach writer slot** — a lease row per run (`run_attach_leases`, six seconds,
  renewed every two). A client is the writer only while its holder id is the row's
  holder; input and resize check it at least once a second and fail closed when
  Postgres does not answer. A take-over served by any replica replaces the lease
  with a reservation for the taker, tells the holder's replica to displace the
  client, and promotes the taker's own queued client in place, if it has one; no
  one else's queued client gets the slot. The lease lapses with its replica, and
  `GET /runs/{id}/attach/holder` names a holder on another replica. The attach-mode
  frame a client receives still names only a holder in the same replica.
- **run events and kills** — each replica announces its run events and kills over
  `NOTIFY wardyn_live` (one dedicated connection per replica, outside
  `pool_max_conns`). A notice is a hint: the events stream re-reads the run every
  beat, and a kill whose notice is lost still ends the run `KILLED`, because
  dispatch's STARTING to RUNNING compare fails and tears the sandbox down.
- **the Azure DevOps run token** — `ado_run_pat_state` holds each `minted_pat`
  run's current token sealed under the run owner's `cred` key (AAD: the run id and
  the rendering), the capabilities it was built from and the pause mark. Every mint
  and revoke for a run takes its token lock, so two replicas resolving at once
  create one token, and a pause's revoke cannot interleave with a mint. The count of
  a person's sign-in ends (`ado_signin_ends`) is shared. A person's erasure deletes
  the rows (the `mask_copies` scope) and the manifest fence stops a write after it;
  state that cannot be read or written is a `503`, never a guess. A split migrator
  and app role install grants the app role `SELECT, INSERT, DELETE` on
  `run_output_chunks`, `SELECT, INSERT, UPDATE, DELETE` on `run_attach_leases` and
  `ado_run_pat_state`, and `SELECT, INSERT, UPDATE` on `ado_signin_ends`.

### What stays per replica

These are per-process by design, and under HA they are **per replica**. A cap is
therefore the per-replica cap times the replica count (cap × N):

- **SSH and UI connection counts**, the **rate limiters** and the auth-failure
  coalescer (`internal/api/server.go`): each replica admits its own cap, so N
  replicas admit up to N times it.
- **Debounce caches**: the decision-ingest `lastTouch` debounce (`shouldTouch`,
  `internal/api/internal.go`) and the console's re-assert debounce. N replicas can do
  up to N times the `TouchRun` writes the 30 s debounce was sized for. Load, not
  correctness.
- **The `/metrics` counters** (`internal/api/metrics.go`): a scrape reports one
  replica's slice of the fleet, not the fleet. Sweep gauges come from the shared
  record, so a follower reports the leader's ticks.
- **The audit spool**, a local append-only file per pod (`internal/api/auditspool.go`),
  the fallback for a failed Postgres write; each replica drains its own back into
  the database. **On `emptyDir` it is lost with the node**: a node failure loses
  whatever that pod had spooled and not yet drained. The chart keeps it on `emptyDir`
  because a persistent volume cannot be shared between replicas.
- **The image build tracker** (Docker only), and the run-secret sweeper's local cache
  (it drops this replica's own copy of a terminal run's values; the durable rows are
  the leader's pass).
- **The age identity, when `WARDYN_AGE_KEY` is unset**: each process mints its own
  ephemeral one at boot (`buildSecretStore`, `cmd/wardynd`), so a secret written by
  one replica cannot be decrypted by another. Set the same `WARDYN_AGE_KEY` (or a key
  service) on every replica; the chart refuses an ephemeral key with an external DSN.

### Residual risks

- **SSH exec, SFTP and direct-tcpip were never masked** (`sshgateway_channels.go`), so
  nothing above covers them. Masking is verbatim only: an encoded or narrated value
  is not caught.
- **Runs that predate 0.8.6 have no masking manifest.** After any restart they are
  refused at the five doors (recording upload, live attach, exec relay, SSH shell,
  live output read) until they end. Runs dispatched by 0.8.6 survive restarts.
- **Masking now depends on Postgres.** With it unreachable, uploads and new attaches
  are refused and live chunks are replaced by the placeholder. That trades
  availability for never persisting a credential.
- **On a Postgres failover, advisory locks can overlap briefly.** Leader actions are
  idempotent and epoch-fenced and the refresh write is a compare-and-set, but a
  refresh already in flight on the old holder can still race the new holder at the
  authority and lose the credential; the person signs in again.
- **An owner's key that does not unwrap fences that owner only.** If a person's
  principal key cannot be unwrapped because its `wrapped_key` is corrupted under the
  local key or Azure Key Vault, a read of the shared registry logs
  `maskstore: the owner's key does not open` at Error, naming the owner and key
  version, then handles each of that owner's live values as a destroyed key: their
  runs are fenced (attach, SSH shell and recording upload answer `503`
  `mask_state_unavailable`) and the rows are tombstoned. Other people's values are
  unaffected and `mask_registry_shared` stays healthy. The remedy is to erase that
  person's credentials through the API; they then store their credentials again and
  their future runs are covered. The fenced runs stay fenced. Only a destroyed generation, or a wrap that
  provably does not open under its own key (the local key, or Key Vault), fences.
  Every other key failure fails the read instead, so consumers fail closed and
  nothing is fenced or tombstoned:
  - A transient failure (Postgres or the KEK service unreachable): the read fails and
    retries, and consumers fail closed until it succeeds.
  - A key this replica cannot reach (its own `WARDYN_KEY_DOMAINS_FILE` or KEK
    configuration lags, or the key service refuses its access with a 401 or 403):
    that replica fails its read, and heals once the configuration or the access is
    fixed.
  - A key or mount the service no longer holds (deleted, soft-deleted or renamed), or
    a Key Vault version that is disabled: every replica fails its read until the key
    is restored or that person's credentials are erased through the API.
  - Under Vault Transit, a wrap that does not open: a version retired by
    `min_decryption_version`, a `WARDYN_VAULT_ADDR` that names another Vault with the
    same mount and key name, or a corrupted `wrapped_key`. A retired version comes back
    when the floor is lowered, and Vault's answer for a corrupted wrap is the one it
    gives another Vault's wrap, so none of these is proof about the row. Every replica
    reading through that Vault fails its read until the floor is lowered and
    `-rewrap-principal-keys` is run, the address is fixed, or that person's
    credentials are erased through the API.
  - Any other answer, including a Vault DR secondary's `472`, a performance
    standby's `473` and an Enterprise `412`: every replica fails its read closed
    until the service answers.
- **A compromised wardynd process still sees every value it masks.** Shredding a
  person's copies is complete only after backups expire or the wrapping key version
  is retired.
- **The elected leader is one replica.** The approval, recording, credential, pause and
  always-egress sweepers run there; if it dies, another takes over within one retry
  interval (about 15 seconds).

## Kubernetes: known gaps

Moved to [kubernetes-known-gaps.md](operations/kubernetes-known-gaps.md).

