# Operating Wardyn

Backup, upgrade, rotation, monitoring, and the scaling constraint — the questions
that arrive after the stack is up.

**Scope: mostly the Compose stack.** The backup/state-store, monitoring and
corporate-network sections are written against
[`deploy/compose/`](../deploy/compose/); the Helm chart (`deploy/helm/wardyn`,
[`k8s.enabled`](../deploy/helm/wardyn/README.md)) runs its own
[Kubernetes runner substrate](#kubernetes-known-gaps) with its own run/recording
state — see [Kubernetes: day-2](#kubernetes-day-2) for the chart's own backup,
restore, upgrade and key-persistence commands.
"[Multi-user: who can change what](#multi-user-who-can-change-what)" and
"[One replica, by construction](#one-replica-by-construction)" apply to both
substrates identically: authorization and the per-process constraints live in
`internal/api`, above the runner seam.

## State stores

Three stores hold data that exists nowhere else on every deployment, and a fourth
appears the moment you register a **user drive**. Lose any of them and the loss is
permanent.

| Store | Where | Holds | If you lose it |
|---|---|---|---|
| Postgres | volume `<project>_postgres_data` | runs, approvals, workspaces, policies, encrypted secrets, the append-only audit log — and, under the default `pg` recording store, the PTY asciicasts too | everything |
| Recordings | volume `${WARDYN_NS:-wardyn}-recordings` (`WARDYN_RECORDING_DIR=/data/recordings`) | PTY asciicasts for Replay — **only with `WARDYN_RECORDING_STORE=fs`**; the shipped default (`pg`) keeps them in Postgres and leaves this volume empty | every session replay it holds; nothing reconstructs them |
| Age key | `WARDYN_AGE_KEY` in `deploy/compose/.env` | the X25519 identity every stored secret is encrypted to | every secret in Postgres becomes undecryptable ciphertext |
| User drives | one object per person, per drive, on a deployment that registered one — a Docker volume or a PVC, both named `wardyn-drive-<drive-slug>-<home>`, or a subdirectory of the share YOU mounted (`host_path` — `<host_root>/<home>`) | each person's own files, written by their own runs at `/home/agent/drive`. Postgres holds the drive rows and the allocations, never the bytes, so `pg_dump` never carried this | that person's work; nothing reconstructs it |

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

The `audit` volume is **derived**, not primary: the optional file sink
(`WARDYN_AUDIT_SINKS`, [ENV.md](ENV.md)). Postgres is the source of truth for the
audit log (`deploy/helm/wardyn/values.yaml` says the same about `persistence`);
the file sink is a forwarding copy for a SIEM. Ground truth (`tetragon_export`)
and the rotator's `groundtruth_token` are transient — regenerated on start.

### Back them up

```sh
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
#    collide with an existing one.)
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

# 6. Now bring up the rest of the stack.
make setup

# 7. Verify — row count first:
docker exec -i wardyn-postgres psql -U wardyn -d wardyn -c "SELECT count(*) FROM audit_events;"
#    then prove the age key actually decrypts what came back, which a row
#    count alone can't: launch a run against any workspace/policy that
#    depends on a previously-stored secret and confirm it starts instead of
#    failing closed with a decrypt error (see "Rotating the age key" — the
#    wrong key fails exactly here, not at boot):
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
[One replica, by construction](#one-replica-by-construction)).

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
(`internal/db/db.go:295-333`); on an older server only a superuser can set the
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
leaves a shorter, valid chain. The defence against both is **off-box**: every
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
checks it on every boot (`reportTransactionIsolation`, `internal/db/db.go:815-829`)
and, when it reads anything other than `read committed`, logs an ERROR naming
the value and the fix — `ALTER DATABASE <db> SET default_transaction_isolation
= 'read committed'` (or the matching `ALTER ROLE`) — without refusing to
start: Wardyn's own writers pin `READ COMMITTED` on their own transaction
(`store.InsertAuditEvent`'s `Begin`, `internal/store/store.go:530`; a
transaction-level isolation level overrides the GUC) and are unaffected
either way, so this is a posture to report for an EXTERNAL writer this
package cannot see, not a defect to boot-refuse over (`internal/db/db.go:803-814`).

### Retention, erasure and GDPR — a residual, not a solved problem

The append-only guarantee above is unconditional: no time window, size cap, or
admin-invoked delete path anywhere in `audit_events`. A deliberate integrity
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

**This is asymmetric with session recordings**, which have the retention lever
audit lacks: `WARDYN_RECORDING_RETENTION_DAYS` (`docs/ENV.md:47`) age-deletes
stored PTY casts, defaulting to keep-forever but operator-settable, and each sweep
that removes anything emits its own `recording.retention.sweep` audit event. Under
a "right to erasure" obligation on data an audit row could contain, the honest
answer is: **you cannot selectively erase it, and the fix that exists for
recordings does not exist here.** A time-partitioned audit table with an attested,
operator-invoked partition-drop (or crypto-shredding) is the shape of a real fix;
nothing in that direction is built.

## Monitoring

`GET /metrics` (admin bearer required, next to the unauthenticated `/healthz`)
serves Prometheus text exposition — stdlib-only, no client library. Counters: runs
by terminal state, approval decisions by outcome, egress denies, credential mints;
plus sandbox launch-latency sum/count. Two gauges sit beside them, because every
counter only moves on success — a dead store and an idle cluster otherwise scrape
identically: `wardyn_store_up` (1 when Postgres answers the same bounded ping
`/readyz` makes) and `wardyn_audit_spool_lines` (audit events waiting in the local
JSONL fallback spool — a value that never returns to 0 means the drain loop is not
working; it is the count of events still to replay, which mid-drain can be lower
than the line count of the file on disk). Beside them, `wardyn_audit_spool_quarantined_total` counts events the
store permanently refused and the drain moved aside (see the spool paragraph
above): non-zero means the trail is missing those events even though the spool
drained.

Two counters cover the authentication lane, where a failure otherwise leaves no
trace at all. `wardyn_auth_failed_suppressed_total` counts `auth.failed` audit
rows the rate limiter dropped — the trail is capped at roughly one row per
second, so past a small burst it stops describing the volume it is bounding and
**a credential-stuffing run reads quieter than a handful of typos**. Alert on
this series, not on the audit row count: flat rows with this climbing is the
attack. Both the public lane and the INTERNAL lane (the sandbox's run token and
the host sensor's token) feed that one limiter and that one counter, so a
process inside a sandbox brute-forcing run tokens is visible on this series
without being able to flood the append-only log; the `auth.failed` row's actor
(`wardyn/adminAuth` vs `wardyn/internalAuth` / `wardyn/internalAuthGroundtruth`
/ `wardyn/internalApproval`) is what tells the two incidents apart. `wardyn_auth_store_errors_total` counts requests an authentication lane
could not decide because its store read failed and answered `500` — a state with
no audit row (there is no authenticated principal to attribute one to) and no
client-visible cause.

That second counter exists because **`wardyn_store_up` cannot answer for it**.
The gauge is a *ping*: it says the pool is reachable, and a reachable pool still
fails individual queries — one table denying a read, one statement timing out.
So it can scrape `1` throughout an outage that is 500ing every token-authenticated
request, which is worse than no signal, because it argues against the operator's
own evidence. Read `wardyn_store_up` as reachability and the two counters above
as whether the work is actually succeeding. Scrape with any Prometheus
`authorization` config carrying the admin token.

**On Kubernetes the scrape must also be let through the NetworkPolicy.** The
Helm chart renders a default-deny policy whose only ingress peer is *this
namespace*, so a Prometheus in a `monitoring` namespace is denied before it
reaches `/metrics` — and a scrape a NetworkPolicy dropped looks exactly like a
target that is down. `networkPolicy.ingress.from` opens it, but that value
**REPLACES** the same-namespace default rather than adding to it, so list every
peer that must reach wardynd:

```yaml
networkPolicy:
  ingress:
    from:
      - namespaceSelector:
          matchLabels: {kubernetes.io/metadata.name: ingress-nginx}
      - namespaceSelector:
          matchLabels: {kubernetes.io/metadata.name: monitoring}
```

`deploy/helm/wardyn/ci/all-on-values.yaml` carries exactly that pair, beside the
`prometheus.io/scrape` pod annotation it advertises.

`/healthz` stays the liveness/component surface (identity, runner classes,
eBPF ground-truth state); `/metrics` is the trend surface. Audit sinks
(`WARDYN_AUDIT_SINKS`, [ENV.md](ENV.md)) are the event stream for SIEMs — metrics
carry no per-run detail.

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
| empty | listed in `WARDYN_OIDC_OPERATOR_EMAILS` → **admin**, others → **member**; all **admin** only when the allowlist is also unset (override-only under OIDC — the pre-0.5 behavior) | always **admin** |
| non-empty | mapped by `roles`/`groups`/email claim to **admin**, **`security_admin`** or **member**; no match falls through to `WARDYN_OIDC_DEFAULT_ROLE` (which takes `admin`/`member` only), or denies the login when that is also unset | always **admin** |

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
  (`cmd/wardynd/boot_deps.go`) refuses to boot OIDC at all unless
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
`Wardyn.Admin=admin,eng-team=member,alice@corp.com=admin`; full semantics in
[ENV.md](ENV.md)): each `value` is matched case-insensitively against the ID
token's `roles` claim (an Entra App Role — the priority path; app-registration
walkthrough in `.claude/skills/wardyn-k8s-setup`), its `groups` claim, or the
signed-in email. Matches fold **highest wins** over three ranks — `member` <
`security_admin` < `admin` — whichever claim produced them (`roleRank`,
`internal/auth/oidc/derive.go`): a human matching a `security_admin` row and a
`member` row is a security admin; one matching an `admin` row anywhere is an
admin, exactly as before 0.7. `WARDYN_OIDC_OPERATOR_EMAILS` is **not
replaced**: an email on it is still an *additional* `admin` match
(`LegacyAdminEmails`), so a deployment adopting the role map keeps its current
operators with zero re-configuration. `WARDYN_OIDC_DEFAULT_ROLE`
(`admin`/`member`, unset = deny) covers everyone the map doesn't name —
`security_admin` is **refused** there and fails boot (`validDefaultRole`,
`cmd/wardynd/boot_deps.go`): the role map is the only way to reach that tier, so
it is never the tier granted by fallthrough to everyone nobody named.

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
| managed harness credential; policy create/update/delete; `PUT /site-config` (a full-document replace, integration credential refs included — and the matching `GET` is gated too, see the operator-topology reads below); `GET /metrics`; the `/access` role-mapping routes below — they bound who derives admin at all | ⛔ admin only |
| the operator-topology READS — `GET /site-config`, `GET /sources`, `GET /sources/{id}`, `GET /base-images`: they carry the upstream-proxy secret ref, a `local_dir` source Locator and internal registry refs, so reading them is reading the deployment's own topology | ⛔ admin only |
| the `/workspaces` routes that BIND CREDENTIAL MATERIAL or WRITE THE HOST — `llm-cred`, `requirements`, `env-as-code/write` — plus `reassign` (user administration) | ⛔ admin only |
| the `/workspaces` routes that DECIDE AN EGRESS CEILING — `approved-egress`, `denied-egress`, `promote-egress`: deciding which hosts a workspace's runs may reach is the same authority as deciding an egress approval, and `promote-egress` is that decision in bulk | ⛔ admin or `security_admin` |
| launching a recording session (`POST /workspaces/{id}/record`) — it sat with the egress-decision routes above until 0.7 re-tiered it, because the route does not decide a ceiling: it LAUNCHES a credentialed, host-mounting, open-egress sandbox and stamps the caller as its owner, which is reach into a run and at the host. The egress DECISION stays delegable; only the launch moved | ⛔ admin only |
| the two `/site-config` connectivity probes (`POST /site-config/test-proxy`, `/test-redirect`) — non-mutating, and the evidence half of the security admin's job — and the `/permissions` routes below | ⛔ admin or `security_admin` |
| the rest of that tier: `GET`/`DELETE /tokens`, `POST /sessions/revoke`, `GET /audit/chain/verify`, the `/governance` profile and assignment routes, `GET /access/directory/search` | ⛔ admin or `security_admin` |
| workspace CRUD/scan/build | 🟡 owner-or-admin since 0.6 ("Workspace ownership") |
| `devcontainer_repo` on a run (`denyMemberRequest`, `internal/api/runs_create_validate.go`) | ⛔ admin only, never grantable |
| a custom sandbox `image` | 🟡 admin by default; the one power a capability grant can hand a member ("Capabilities") |
| a member's own onboarded-workspace base image | 🟢 never gated — operator-authored at onboarding, not the member's free-text choice |
| the `/drives` routes — registering a **user drive**, allocating it to people or groups, previewing whose drive resolves (`mountUserDriveRoutes`, `internal/api/user_drives.go`) | ⛔ admin only, deliberately NOT the security-admin tier: a drive names a host path (`host_root`) or a cluster storage class, and "never the host" is the line between the two admin tiers |
| the user-drive **door** — `DenyUserDrive` on a governance profile (`internal/types/governance.go`) | 🟡 security admin too, through `/governance` — a limit on a profile, not a drive; it refuses the mount, it does not deallocate anything |
| mounting YOUR OWN drive on a run (`drive.enabled`) | 🟢 the person, per run — read-only unless their allocation says otherwise, and the run flag may only narrow that, never widen it |
| `POST /runs`, `POST /runs/{id}/kill` | 🟢 any signed-in human — using the product is a member act |

**Documented gaps — routes gated but not yet named above.** Ten further gated
routes have no covering row in the tier table — a pre-0.7 omission the F316
completeness check surfaced, not something this wave caused: the `/sources`
writes (`POST /sources`, `POST /sources/{id}/scan`, `DELETE /sources/{id}`);
the `/base-images` writes (`POST /base-images`, `DELETE /base-images/{id}`);
the integration writes (`PUT /integrations/{id}`, `DELETE /integrations/{id}`,
implied today only inside the `PUT /site-config` row above); `POST
/admin/sandboxes/sweep`; `POST /setup/onboarding-complete` (the setup family
above is named only as "managed harness credential", which this route is
not); and `GET /runs/{id}/attach`. Each is named, with its reason, in
`docTierUndocumented` (`internal/api/operations_tier_doc_test.go`), and
`TestOperationsTierTableMatchesRouteMatrix`'s completeness check blocks any
new gated route from joining that list unnoticed — a route sits there only
until a docs pass moves it into `docTierRows` with the token that covers it.

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
`""` — every pre-migration row, and everything an admin writes without `?owner=`
— means **operator-owned**, i.e. exactly today's behavior.

- **Workspace CRUD/scan/build are owner-or-admin, not admin-only**
  (`getWorkspaceAuthorized`/`getWorkspaceReadable`, `internal/api/helpers.go`).
  Another member's owned workspace answers the **byte-identical 404** a missing id
  does. An OPERATOR-owned workspace answers a member's mutation with a **403** —
  it is listable and readable by every member, so there is no existence to hide.
  `GET /workspaces` returns the caller's own rows plus the operator-owned ones,
  never another member's.
- **A member's `local_dir` source is bounded by operator-set roots**:
  `WARDYN_MEMBER_WORKSPACE_ROOTS` (and its per-member `_MAP`, which REPLACES the
  shared list for a principal that has an entry) in [ENV.md](ENV.md). Unset = no
  member `local_dir` mounts at all (fail closed); writability needs the separate
  `WARDYN_MEMBER_WRITABLE_ROOTS` minus `WARDYN_MEMBER_WRITABLE_DENY`.
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
  the object at all. Reclaiming it is a deliberate operator command, one per
  substrate, and Wardyn holds no `delete` verb that could do it by accident: the
  recipes are "User drives on Docker" and "User drives on Kubernetes" in this
  document, and are not repeated here. Deleting the **drive row** itself
  is a `409` while any allocation still points at it (`ON DELETE RESTRICT`), so
  the deallocation is always its own audited event and offboarding can never
  silently widen anything.
- **Secret write/delete moved from admin-only to self-service.** Any signed-in
  human may `PUT`/`DELETE /secrets/{name}` their OWN row
  (`secretOwnerFromRequest`: `""` for an operator, their own principal for a
  member). A member's `DELETE` of another principal's row is structurally
  unreachable (`secretstore.Store.For(owner)` never resolves it) and answers the
  byte-identical 204 a never-set name gets. The four Bedrock/SigV4 names
  (`aws-access-key-id`/`aws-secret-access-key`/`aws-session-token`/
  `bedrock-api-key`) stay refused (403) for every non-operator PUT: Bedrock always
  resolves from the operator namespace, so a member row under one of those names
  would read as configured in setup while dispatch never uses it.
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
- **`?owner=<principal>` is admin-only** on `PUT`/`DELETE`/`GET /secrets` (an
  admin's cross-write lands in the NAMED member's namespace, never the
  operator's), refused with a constant 403 for anyone else. The value names a
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
- **Cross-user admin access is queryable.** An admin acting on a member-owned
  workspace stays the ADMIN in the audit actor (no impersonation) with
  `workspace_owner` naming the member; `secret.write`/`secret.delete` carry
  `secret_owner` naming the non-"" namespace a write landed in (a member's own
  ordinary write included) — see [AUDIT-ACTIONS.md](AUDIT-ACTIONS.md).
- **Member model access.** A member's own secret under the provider convention
  name (`anthropic-api-key`/`openai-api-key`) synthesises the legacy
  `anthropic_api_key`/`openai_api_key` integration row exactly as the operator's
  does (`resolveIntegrationRef`), so `GET /integrations` lists it, a run
  selecting it resolves model access, and no false "no model access" warning
  fires. `filterMemberGrants` admits the matching hand-authored inline `api_key`
  grant with no operator eligible-grant pairing when ALL hold: the host is a
  model-provider host (the anthropic.com/openai.com convention, or a configured
  internal gateway) that the run's own already-clamped egress allows, and the
  member OWNS a secret by that exact name (a names-only
  `Store.For(<member>).List`, never a value read). Every other grant kind, and
  any pairing failing one of those, stays ceiling-paired exactly as before. See
  [MEMBERS.md § Your model key](MEMBERS.md#your-model-key).

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
`decision_scope=run` (`wardyn approve <id> --scope run`) makes that one decision
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
with `authorizeMemberDecision`): it writes a durable entry onto the run's
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

**The desktop tier's standing honesty gap: the operator IS the admin.**
[The desktop tier](../deploy/desktop/) (`WARDYN_LOCAL_MODE=true`) has no member
role at all — local-mode callers are *always* admins
(`Server.requireOperator`'s own doc says so), so the unclamped branch above is the
default there. An `inline_policy` the developer submits is bounded by nothing
`WARDYN_DEFAULT_POLICY` sets, and setting one is one ordinary API call. What still
holds: the unclamped spec lands on the audit feed as `policy.inline` before
`run.create` ([AUDIT-ACTIONS.md](AUDIT-ACTIONS.md)), egress still has no route off
the sandbox except `wardyn-proxy`, and the session is still recorded. A governance
control, not a containment boundary against the operator holding the laptop. Full
accounting: [docs/DESKTOP.md](DESKTOP.md) "Tamper posture, stated honestly".

### User drives on Docker

A **user drive** is persistent storage an admin registers once and allocates to
people or groups; a member mounts theirs per run at `/home/agent/drive`. On a
Docker deployment there are two backends, and the difference is who owns the
bytes.

**`docker_volume` — Wardyn allocates.** A per-person named volume
(`wardyn-drive-<drive-slug>-<home>`), created on first use with the `local`
driver and mounted at the reserved target. Nothing to configure. It carries four
labels: `wardyn.managed=true`; `wardyn.drive` = the **drive row's id** (the name
folds the drive's SLUG, which a rename changes, and the id never does — so the
label is the only key that still finds a drive's volumes across one, which is
what the reclaim recipes below select on); `wardyn.home` = that person's directory name; and
`wardyn.subject` = a **digest** of the person
themselves (never their claim — see the restore note below). Reclaim is a
command, not a button:

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
   all** — the same fail-closed posture `WARDYN_MEMBER_WORKSPACE_ROOTS` takes,
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

   **And the drive ceiling must not overlap `WARDYN_MEMBER_WORKSPACE_ROOTS` —
   the member ceiling defeats per-person isolation where they meet.** Per-person
   isolation is the **bind of the subdirectory**: Wardyn hands a run one home out
   of the share and refuses a source that resolved to the root. A member
   workspace is a different surface with a different rule — a member names a
   directory under `WARDYN_MEMBER_WORKSPACE_ROOTS` and binds it **whole**,
   writable where `WARDYN_MEMBER_WRITABLE_ROOTS` allows it, and that path
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
   | The two lists name the same tree | ``WARDYN_MEMBER_WORKSPACE_ROOTS and WARDYN_USER_DRIVE_HOST_ROOTS both name "<p>": a member can onboard that directory as a workspace and bind the WHOLE share, every other person's home included, without a drive allocation. Point the drive ceiling at the share and the member ceiling somewhere else`` |
   | A member root CONTAINS a drive root | ``WARDYN_MEMBER_WORKSPACE_ROOTS contains "<m>", which holds the WARDYN_USER_DRIVE_HOST_ROOTS entry "<d>": a member can onboard that share as a workspace and bind it whole, every other person's home included, without a drive allocation. Point the member ceiling at a tree that does not contain the share`` |
   | A member root is INSIDE a drive root | ``WARDYN_MEMBER_WORKSPACE_ROOTS contains "<m>", which is INSIDE the WARDYN_USER_DRIVE_HOST_ROOTS entry "<d>": member workspaces would be authored inside a share whose directories Wardyn hands out one person at a time. Point the member ceiling outside the share`` |

   Every member ceiling is compared, the shared list **and** each
   `WARDYN_MEMBER_WORKSPACE_ROOTS_MAP` per-principal override — an override
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
`WARDYN_WORKSPACES_ROOT` and `WARDYN_MEMBER_WORKSPACE_ROOTS` take.

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

### Capabilities: what one member, or one group, may do

The role split above is deployment-wide. A **capability grant** is per-human: a
row naming a *subject*, a *kind*, a *value*, and an effect of `allow` or `deny`
(`capability_grants`, migration 0042), with a per-kind **enforcement switch**
beside it (`capability_enforcement`). One sentence is the doctrine, and every rule
below follows from it: **a capability bounds what the MEMBER chose, never what the
ADMIN pre-authorized.** So a stored policy, a workspace's own requirements, the
hosts a workspace scan seeded, the model provider's own egress, and the grant
`foldRunIntegration`/`applyWorkspaceRequirements` re-add at launch are all left
untouched no matter what a member holds.

**The six kinds** — a closed set, written down once in Go (`capabilityKinds`,
`internal/api/capabilities.go`) rather than as a schema CHECK:

| Kind | Value | Direction | What it bounds, and where |
|---|---|---|---|
| `egress_host` | a host, or a `*.suffix` wildcard | narrows | which host a member may **decide** an `egress_domain` approval for (`authorizeMemberDecision`, `internal/api/approvals.go`), and which hosts survive on a member's own `inline_policy` allowlist (`narrowMemberInlinePolicy`, `internal/api/inline_policy.go`) |
| `secret` | exact secret name | narrows | which stored secret a member's own `inline_policy` grant may reference — both refs of an `ssh_key` grant, key and `known_hosts` — and which names `GET /secrets` lists back to them (`handleListSecrets`, `internal/api/secrets.go`) |
| `workspace` | workspace uuid | narrows | which onboarded workspace a member may name on `POST /runs`/preflight (`denyMemberRequest`, `internal/api/runs_create_validate.go`) |
| `image` | exact image ref | **widens** | which custom sandbox image a member may launch at all — without a grant, none (same seam) |
| `agent` | exact `--agent` string | narrows | which agent/harness a member may launch (same seam). Deliberately NOT constrained to the harness catalog, at the gate or at the grant write: `WARDYN_AGENT_IMAGES` custom agents are supported, so a catalog check would make an operator's own entry unwriteable |
| `integration` | exact integration id | narrows | which AI-provider integration a member may name on a run (`integration_id`, same seam) — and nothing else. **Tier 1 only**: a workspace's own `LLMCred` pin and your `DefaultFor: agent_runs` site default are operator-authored and are never gated, or one `all` deny row would strip the deployment's model access |

`*` as a value matches everything of that kind, spelled the same way for all
six. `egress_host` values are matched by `entryCoversAny`
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
and `integration` narrow rather than widen: launching an agent, or naming a
provider, is something every member could already do, so a widening kind would
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
free), or `all` (every signed-in human).

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
highest-wins match), and so is a fallthrough to `member`, the narrowest tier
there is — a human in 200+ groups still signs in. Only a default WIDER than
`member` is refused. The remedy is the operator's, and retrying will not clear
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

**Managing them** (the four `/permissions` rows are `securityOps` — admin or
`security_admin`; the `/access` rows are `operatorOnly`; `GET /me/capabilities`
is member-safe):

| Route | Does |
|---|---|
| `GET /permissions` | the whole grant table plus every enforcement switch, one call |
| `POST /permissions/grants` | upsert one grant on its natural key (`201` new, `200` updated) |
| `DELETE /permissions/grants/{id}` | remove one grant |
| `PUT /permissions/enforcement` | replace the whole switch map — an omitted kind means *off* |
| `GET /access` | the merged role-mapping table (chart + console rows, with collision/shadow provenance) plus the same before/after/changes posture the write guards below evaluate |
| `POST /access/mappings` | upsert one console role mapping on its natural key (`value`) — `201` new, `200` updated; refused on a chart/operator-allowlist collision, an unmatched-outcome flip without `acknowledge_access_change`, or a write that would remove the caller's own admin access |
| `DELETE /access/mappings/{id}` | remove one console role mapping — same flip/lockout guards as the write above |
| `POST /access/preview` | dry-run `roles`/`groups`/email (or the caller's own session) through the SAME derivation a real login would use — no write |
| `GET /me/capabilities` | member-safe: the caller's OWN grants, the switches, their session groups, and `groups_snapshot_stale` |

`PUT /permissions/enforcement` replaces the **whole** map, so an omitted kind is
an enforced kind switched off: re-fetch `GET /permissions` immediately before
writing, or a stale admin tab can silently disable a control two admins both
believe is on. `GET /permissions`'s `ETag` header (a content hash of the
enforcement map alone, not the grant table) can be sent back as this `PUT`'s
`If-Match`: a document that changed underneath a stale tab is refused `412`.
`If-Match` is optional, and the write is audited either way.

Writes are audited as `capability.grant.created` / `.updated` / `.deleted` and
`capability.enforcement.write`. Enforcement lives in its own table rather than in
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

Revoking a human (`POST /api/v1/sessions/revoke`, `wardyn sessions revoke`) also
revokes every unrevoked token that principal holds — a token is their session in
another form. The `all` arm is deployment-wide for tokens too: EVERY live token
goes, the calling admin's own included — plan to re-mint after a global revoke.

**Name them by either identity.** `--sub` takes the OIDC `sub` **or** the email,
and both halves of the revoke honour both — the session cutoff and the token
sweep — so you do not have to know which one your IdP made authoritative. This
matters on Entra, where the `sub` is an opaque per-app identifier that appears
nowhere a responder would naturally read it; the email is matched
case-insensitively, the `sub` exactly. It is the same rule a
`subject_type=user` capability grant already follows.

What the API cannot tell you is whether the name matched anybody. Sessions are
stateless signed cookies with no row to count, so a target that names nobody is
indistinguishable from one whose sessions have already expired, and both answer
`204`. The `session.revoke` audit row carries `tokens_revoked` for the half that
*is* countable — a zero there, against a human you believe holds tokens, is the
signal that the identifier was wrong.

Use one as an ordinary bearer: `Authorization: Bearer wdn_…`. Downstream it is
indistinguishable from that human's console session — run ownership, the
admin/member gate and capability grants all resolve to the owning human — so a
member's token reaches exactly the routes their session reaches, and no more. A
token is **never** the admin identity: minting one requires a verified SSO human,
so neither the admin token nor local mode can mint one, and a token cannot mint a
successor.

Only `hex(sha256(token))` is stored, so a lost token is re-minted, never
recovered, and a database reader (a reporting role, a hot standby, a `pg_dump` in
a backup bucket) cannot lift a usable credential off a row. `last_used_at` is best
effort and is the signal for "which of these are dead"; revoke those.

**The role is a stamp re-checked at login; the GROUP SNAPSHOT is not checked at
all.** A token carries the role AND the group snapshot its owner held when they
minted it, and every request it authenticates republishes them, so downstream it
is that human as they were at mint time.

The two halves age differently, and only one of them ages. Their next successful
sign-in **re-stamps the role** on every unrevoked token they hold — the same
`OnLogin` hook that has re-stamped their SSH keys since 0.6 — so a demotion does
reach outstanding tokens, at that human's own next login rather than
immediately. **The group snapshot is never refreshed**, by that hook or anything
else. And nothing ages either half out on its own: `api_tokens` has
`created_at`, `last_used_at` and `revoked_at` and **no expiry column**, there is
no TTL on the stamp the way `WARDYN_SSH_ROLE_TTL` bounds an SSH key, and a human
who is demoted and never signs in again keeps the role their tokens were minted
with indefinitely. **Explicit revocation is the only thing that ends it on your
schedule.**

A demotion made on the People page is now one of those explicit revocations:
when a role-mapping write or delete takes a tier away from a value, Wardyn
revokes the outstanding tokens of every principal whose own derivation that
edit demotes and whose stamp still carries what was lost, and reports the
number as `tokens_revoked` in the response and the audit row. It is scoped to
that demotion — a promotion, an unrelated value, and a member-stamped
credential naming the same group are all left alone — and a token whose group
snapshot is missing or partial cannot be re-derived, so an elevated stamp in
that state is revoked rather than assumed safe.

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
```

A revoked token is never re-stamped — it keeps whatever role it carried when it
was revoked, so the trail still says what that credential actually was.

The `session.revoke` audit row carries `tokens_revoked`. That count is the
receipt: a **zero** against a human you believe holds tokens means the identifier
matched nobody, not that there was nothing to revoke — sessions are stateless, so
that half cannot be counted, and only this half can tell you. Both
`token.create` and `token.revoke` are audited
([`docs/AUDIT-ACTIONS.md`](AUDIT-ACTIONS.md)); the revoke row names the token's
owner. Offboarding a person means revoking their tokens explicitly — the row
outlives their access to your IdP, and it is published as a residual
(`threatmodel/THREAT-MODEL.md` §5, "A per-user API token's role and group
snapshot are frozen at mint").

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
a run, and their SSH key and attach ticket still stamp `member`), and a
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
specific wins (user beats group beats everyone; priority breaks group ties) — the
Governance page shows the resolved answer, and `GET /policies/default` returns the
ceiling that actually binds the caller. A profile replaces the deployment ceiling
for its subjects; deleting one requires unassigning it first (never a silent
widening). Stated honestly: profiles narrow by omission — a profile that omits
secret grants revokes them for its subjects (the editor warns); a member's
long-lived API token keeps the group snapshot it was minted with until re-minted.

### Every denial that isn't a 404

(This section is the source of record for `authz.denied`'s `reason` values;
[`docs/AUDIT-ACTIONS.md`](AUDIT-ACTIONS.md) is the vocabulary reference for every
*other* audit `action` and points back here for this one.)

Every member denial that isn't a plain foreign-resource 404 is audited under
`authz.denied`, whose `reason` field is the whole vocabulary.

| `reason` | Raised when | Shape |
|---|---|---|
| `admin_surface` | a member requested an admin-only route (`requireOperator`) | ⛔ `403` |
| `security_admin_surface` | a member requested a route on the SECURITY tier (`requireSecurityOperator` — admin or `security_admin`). The `403` body is byte-identical to `admin_surface`'s on purpose, so a refusal never maps which tier a route sits on; only this reason distinguishes them, which is what lets a rule tell "a member hit an admin route" from "a member hit a security-tier route" | ⛔ `403` |
| `not_owner` | a member reached a run/approval/recording, or a member-OWNED workspace (`owned_by`, migration 0048), that exists but isn't theirs | ⛔ `404` (byte-identical to missing) |
| `attach_ticket_foreign_run` | a caller who is not the super admin — **including a `security_admin`** — asked to mint a PTY attach ticket for a run they did not create. Its own reason rather than `not_owner` so an auditor can see the security tier refused a foreign shell without inferring it from the path (`internal/api/attach_ticket.go`) | ⛔ `404` (byte-identical to missing) |
| `byoi_member` | a member named a `devcontainer_repo`, or an `image` they hold no grant for | ⛔ `403` |
| `capability_workspace` | `workspace_id`: a member named a workspace they aren't granted (`403`). Launching: an `inline_policy` `workspace_repos` entry for an ungranted workspace was dropped — the run still launches | ⛔ `403`, or 🟡 a drop |
| `capability_egress_host` | deciding: the approval's host isn't granted (`403`). Launching: member-authored allowlist entries were dropped from an `inline_policy` — the run still launches | ⛔ `403`, or 🟡 a drop |
| `capability_secret` | a member's `inline_policy` grant referenced a secret they aren't granted — dropped, not rejected | 🟡 drop |
| `capability_agent` | `agent`: a member named an agent they aren't granted (`denyMemberRequest`, `internal/api/runs_create_validate.go`) | ⛔ `403` |
| `capability_integration` | `integration_id`: a member named a model-provider integration they aren't granted (same seam). Tier 1 only — a workspace's own pin and the site default are never gated | ⛔ `403` |
| `governance_profile` | the member's assigned governance profile refuses this run SHAPE. Five causes, one per emitted `target`: `task_mode=exec` (`runs.task_mode`), an interactive run (`runs.interactive`), `seed_auto_tools` (`runs.seed_auto_tools`), codex-cli under hold-deriving rules (`runs.agent`), and — 0.7 — `drive.enabled` under a profile carrying `DenyUserDrive` (`runs.drive`, `denyMemberDrive`). A profile refuses the shape, never the person: the same member launches fine without the refused field | ⛔ `403` |
| `grant_pairing_not_eligible` | a member's `inline_policy` paired a stored secret with a host the operator never eligible-listed (`filterMemberGrants`) — dropped. Also covers the `env_secret` **admin-only** drop (`dropAdminOnlyEnvSecretGrants`), which fires for every non-operator on every route a run policy arrives by — inline body, selected stored row, or the deployment default — whatever the caller's governance assignment, since that rule is a role check plus `WARDYN_ALLOW_MEMBER_ENV_SECRET` rather than a ceiling check | 🟡 drop |
| `groups_snapshot_stale` | the resolver cannot answer this caller's group tier — their login-time group snapshot is missing or was truncated at sign-in, and the deployment assigns governance profiles by group — so every ceiling-bounded seam refuses. Emitted ONCE per request at each site that decides it, and there are two: `ceilingWithUnusableGroups` (`internal/api/governance.go`) at target `governance.ceiling`, and `driveWithUnusableGroups` (`internal/api/user_drives_resolve.go`) at target `runs.drive`. The ceiling is memoized per request and the drive resolver is asked once, so the count still means denials rather than resolves. A deployment that assigns governance profiles by group emits the first; one that allocates user drives by group emits the second; one that does both emits both, for the same member, because they are two separate refusals the member meets at two separate doors. The remedy is the caller's own and is in the refusal body — sign in again, or re-mint the API token | ⛔ `403` |
| `second_human_required` | `WARDYN_EGRESS_SECOND_HUMAN` is set and the caller deciding an `egress_domain` approval is the run's own `created_by` (`requireSecondHuman`) — a different human must decide it | ⛔ `403` |

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
from inside the shared path (`denyMemberField`) — one row per refused door per
call, with **`run_id` NULL**, because there is no run. A dry run that passes
writes nothing at all. That is deliberate rather than suppressed: the row records
that this principal was refused this capability, which is true whether or not
they went on to launch, and a gate that audits at one door and not at the
identical door one handler over is the drift the shared path exists to prevent.
What it costs is that Review re-resolves on every edit, so a member editing
against a closed door can write a row per keystroke — the NULL `run_id` is what
tells those apart from the denials that actually bounded a run
(`handlePreflightRun`, `internal/api/preflight.go`).

A 404 on a resource that genuinely doesn't exist stays silent by design. One
exception: the `always`-scope 403 above returns before `decide()` reaches any
audit call, so it is a bare 403 with no audit trail at all.

**What's still not built.** No custom roles: the tier set is the three fixed ones
(admin, `security_admin`, member — see "Three roles, and who sets the walls"), and
a capability grant only narrows or widens what a member may reach, it can never
mint a tier. Only the six kinds above are grantable; there is no general
per-resource permission model (a run is still owner-or-admin only — no "read-only
share" or "co-owner" concept), no tenant/org columns, and no separation of duty
among super admins — every admin (and the admin token, always) can rewrite the
policy that bounds them
(`threatmodel/THREAT-MODEL.md` residual #14, still open).

The SSH gateway's admin override is a **bounded-stale stamp**, not a live role
check: since migration `0043` a key authorizes when `run.created_by == the key's
principal` OR the key's `role` column reads `admin` AND its `role_checked_at`
(migration `0046`) is no older than `WARDYN_SSH_ROLE_TTL` (default `24h`). The
stamp is written at `POST /me/ssh-keys` time from the registering session's role
and RE-stamped — both columns — on every OIDC login for that principal, across
every key they hold. The gateway never reads the role live at connect time (SSH
carries no session for `requireOperator`), so a demoted admin's key loses the
override at their next login (re-stamped `role=member`) or once `role_checked_at`
ages past the TTL — whichever comes first; deleting the key (`DELETE
/me/ssh-keys/{fingerprint}`, self-service) and re-registering is the immediate
lever. Strictly weaker than the web terminal's live `requireOperator` gate, but no
longer unboundedly so. The same TTL is why **an admin upgrading from 0.5 (or pre-`0046`)
does not get the override on the key they already have until it is refreshed**:
`0043` backfills every pre-existing row as `member` (fail-closed) and `0046`
backfills `role_checked_at` as `NULL`, which `sshAuth` treats as infinitely
stale. A member's key never satisfies the override (`docs/SSH.md`'s Bounds
section; `threatmodel/THREAT-MODEL.md` residual #15). See
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

## Second user, same host

> This recipe gives a second person their own SSO identity instead of the shared
> admin token. What that identity *can do* is exactly the **admin/member** model
> in [Multi-user: who can change what](#multi-user-who-can-change-what) above.
> Under OIDC, `WARDYN_OIDC_OPERATOR_EMAILS` is the boot-required allowlist — an
> empty one **refuses to boot** unless `WARDYN_ALLOW_OIDC_NO_OPERATOR_LIST=true`,
> which, absent a role map, instead makes every signed-in human admin
> (`cmd/wardynd/boot_deps.go`). The admin token is always an admin and cannot be
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
   below). A `make setup` re-run warns about this combination first. In
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
   echo 'WARDYN_OIDC_OPERATOR_EMAILS=you@wardyn.local'    >> deploy/compose/.env
   docker compose -f deploy/compose/docker-compose.yaml --profile sso up -d dex wardynd
   ```

   Not a soft gate: wardynd runs synchronous OIDC discovery against the issuer at
   boot and **exits nonzero if it fails** (`cmd/wardynd/boot_deps.go`) — an
   unreachable Dex refuses the whole boot, not just SSO. Compose's `depends_on:
   dex: condition: service_healthy` sequences this for the command above; it only
   bites if you later restart wardynd alone while Dex is down.

3. **Give the second person their own login.** For the bundled Dex,
   `staticPasswords` in `deploy/compose/dex.yaml` is the authentication list —
   `enablePasswordDB: true` with no external connector means an email absent from
   it has no password to authenticate with, full stop. (Dex authenticates, the
   operator list authorizes.) Mint a bcrypt hash (any bcrypt tool at the same
   cost works):

   ```sh
   htpasswd -bnBC 10 "" 'their-password' | tr -d ':\n'
   ```

   and add an entry alongside the demo user:

   ```yaml
   staticPasswords:
     - email: "demo@wardyn.local"
       hash: "$2a$10$SDMtAYUgJDDzcanSySsoBuLPINvmRvxVpqg3WU9jfThQABkwBvaiK"
       username: "demo"
       userID: "demo-0001"
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
empty value is not "deny all", it fails **open** — any account the IdP
authenticates gets a session, and without the domains list the `email_verified`
claim is not checked at all (both checks live inside the domains branch —
`AllowedEmailDomains`, `internal/auth/oidc/oidc.go`). The bundled Dex's
hand-curated `staticPasswords` makes that moot here; set the domain(s) for real
once you point this at a corporate IdP.

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
the OIDC config changes.

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

An integration is a **connection** — secrets plus egress — to any named external
system Wardyn talks to on a run's behalf, never an installer (what a run has
installed is what its image carries). `types.Integration`
(`internal/types/workspace.go`) is ONE base shape extended by `kind`:

- `secrets[]` — each a role, a store REF (a name, never a value), and its
  **delivery**: `proxy_header` (the header + format the proxy presents on the
  wire, so the sandbox never holds the credential). A secret on a closed kind may
  declare NO delivery, meaning that kind's own hand-written transport carries it
  (`github_app`'s brokered halves, `git_host`'s clone credentials, Bedrock's AWS
  env). Those lanes are why the type also models `resident_file`/`resident_env` —
  but an operator may not DECLARE one: there is no generic lane materializing a
  named secret into a sandbox path or env var, so a write naming one is refused.
  At most one `proxy_header` secret per row (the proxy injects one credential
  header per host), and every such secret targets the row's whole egress list.
- `egress[]` — where the system lives; why a host is reachable for a granted run
  instead of being hand-listed in every workspace.
- `config{}` — non-secret knobs, key-validated per closed kind (bedrock ⇒
  `region`/`model`/`auth_lane`, `github_app` ⇒ `app_id`/`installation_id`/`host`,
  `anthropic_subscription` ⇒ `lane`); an unknown key on a closed kind 400s by
  name.

`kind` is the ONE field that says what this connects to, and the closed set is the
ONLY writable set: `anthropic_api_key`, `anthropic_subscription`, `bedrock`,
`openai_api_key`, `github_app`, `git_host`. Each has behavior in code
(`capabilitiesFor`), so a new one is a code change, and a write naming anything
else 400s with the accepted list. Two carry-overs from 0.5: generic kinds — an
open slug (`"jira"`, `"artifactory"`, …) validated for shape only — are no longer
writable, though a row stored under an earlier release still loads, still sits in
`SiteConfig`, and is still injected by `internal/api/integrations_run.go`; and
`azure_openai` is gone as a kind.

**Settings** (account menu) is the one surface for these — a Model provider card
and a Git host card, each a radio group over concrete lanes; the standalone
`/integrations` page is deleted and redirects there. Rows are also DERIVED from
what already exists (stored secret names, site config, setup status), so an
operator who never opens Settings keeps identical run behavior. Host proxy and
Egress redirection are deliberately NOT here: that is network topology, configured
under **Network** (below) on the same `SiteConfig` document.

### Wardyn does not dial the provider

There is no "Test" action; `POST /api/v1/integrations/{id}/test` was removed in
0.5. Settings states what is STORED and says so plainly — "Wardyn stores this, it
doesn't dial the provider to check it". A real run is the real test, and it fails
loudly with an audit trail if the credential is wrong.

### Nothing is ambient

Configuring an integration grants nothing by itself. A run gets one only when a
workspace's requirements name it by key — `integration:<id>`, alongside
`secret:`/`egress:`/`write:` — and that workspace is what the run attaches
(`applyIntegrationRequirement`, `internal/api/integrations_run.go`). Once granted,
its hosts join the run's egress allowlist unconditionally, even under
`allow_all_egress` (the proxy's credential injector does not honor allow-all, so
the exact-host entry has to be there regardless), and a header-delivering
integration authors one `api_key` grant per host through the ordinary proxy-side
injection path. An operator with fifty integrations configured and a workspace
that names none of them gets a run whose spec is byte-identical to having none —
true for this `integration:<id>` fold, but not for **model access**: absent a
more specific binding, an AI-provider integration marked `DefaultFor: agent_runs`
still folds into the run, even one with no workspace at all
(`resolveRunIntegration`, `internal/api/llmcred.go`; see "Model access resolves"
below).

That fold degrades silently by design — a workspace may state an
`integration:<id>` requirement before the integration exists, and a missing one
must never brick a run — so the create-run preflight checklist carries an explicit
row instead (`setupWorkspaceIntegrationItems`, `internal/api/compose_setup.go`):
"no integration named `<id>` is configured, add it under Integrations", or "turned
off", or "names no hosts", stated as config state (amber, not the destructive red
reserved for a missing credential) with the requiring workspace named. Optional
requirements are never rowed there.

### A header credential needs a bare exact host

An integration's `egress` entries may carry a leading `*.` wildcard or a `:port`
qualifier UNLESS one of its secrets delivers `proxy_header`. Write-time validation
(`validateIntegrationHosts`, `internal/api/integrations_write.go`) then requires
every host to be a bare exact hostname, because proxy-side injection resolves
through `Policy.AllowedExactHost`, which consults the exact-host set only. A
wildcard would open the path and silently never present the credential; a
port-qualified host makes the injector refuse to build a rule, a hard proxy
startup failure. Both are rejected at write time, by name. Neither restriction
applies to an integration that delivers no credential header (a data store on
`db.corp.internal:5432`, egress only, is exactly the shape this is for).

### Model access resolves — it does not default to none

A Claude run's model access is not configured per run. It resolves, in order
(`resolveRunIntegration`, `internal/api/llmcred.go`):

1. an explicit integration named on the run (`integration_id`);
2. else the primary workspace's `LLMCred.IntegrationRef` binding;
3. else the operator's `DefaultFor: agent_runs` integration — the one
   stored integration marked as the site-wide default for agent runs, of
   any AI-provider kind.

A workspace binding that names something — even something stale or
miscategorized — is the operator's SPECIFIC choice and does not cascade to the
site-wide default; that would be a credential surprise, not a convenience. Launch
and preflight resolve this identically (`foldRunIntegration`), so Review cannot
preview access the run won't get.

**When none of the three tiers resolves, that is not the same as no access.**
Below the Integration system, dispatch's own transport resolution
(`resolveLLMTransport`, `internal/api/runs_dispatch_llm.go`) still credentials
the run from whatever GLOBAL provider config exists, independent of any
integration or workspace binding: a Wardyn-managed subscription connected via
`wardyn subscription connect` (`managedInjectReady`,
`internal/api/harnesscred.go` — checks that the run's agent is `claude-code` and
a captured token exists, never that any integration names it) injects
proxy-side, and a global Bedrock config
(`WARDYN_BEDROCK_REGION`+`WARDYN_BEDROCK_MODEL`, [ENV.md](ENV.md)) still
credentials Bedrock calls when no workspace/integration selection overrides it
(`resolveBedrockAuth`, `internal/api/runs_bedrock.go` — a selection wins only the
fields it sets). The `agent == "claude-code"` gate means the
managed-subscription fallback is not universal: a `codex-cli` run with a
connected managed subscription and no integration gets no model access via this
lane. Full transport precedence (subscription → Bedrock → api-key) once a run
reaches dispatch: [TRY-IT.md](TRY-IT.md) → "Model auth: three ways".

### What an admin can put a fence around

Six things a member chooses on their own run each carry a permission on the
Permissions page: the **hosts** they may add or approve, the **secrets** they may
reference, the **workspaces** they may launch against, the **base images** they
may name, the **agents** they may run, and the **model providers** they may name
("Capabilities: what one member, or one group, may do" above has the kind table).
Five of the six *narrow* — until you enforce one, members keep exactly the powers
they had, and a deny bites even before you do; base images are the one that
*widens*, so a grant is what makes an image nameable at all.

A permission always bounds what the **member** chose and never what you
pre-authorized, which is the whole answer to "can I fence a model provider": the
`integration` kind gates the integration a member names on the run, and nothing
else. The provider a workspace is pinned to and your site-wide `DefaultFor:
agent_runs` default are yours, so they still reach every run, granted or not —
gating them would let one `all` deny row strip the deployment's model access. What
bounds those is the assigned governance profile's egress: its denied hosts are
re-asserted at dispatch and withhold every credential lane that would reach one.

Governance profiles also carry the limits that are not choices at all —
`max_concurrent_runs`, and the two launch modes a profile can refuse outright —
and, like every profile field, they bind only the people a profile is assigned to.

## Network: upstream proxy and egress redirects

One more piece of operator-wide config lives in Postgres alongside everything in
**State stores** above: `SiteConfig` (`GET`/`PUT /api/v1/site-config`, `wardyn
site-config get|apply`) — the corporate upstream proxy and the list of outbound
redirects every run's egress inherits. Unconfigured is a valid, common state.
Because it lives in Postgres, `make reset` / `make reset-all` take it with the
volume; `wardyn site-config get > corp-baseline.json` before a reset and `wardyn
site-config apply corp-baseline.json` after is the round-trip — the document
carries secret **names**, never values, so it is safe to keep beside the repo.
Because values never round-trip, `apply` re-attaches the *names* unconditionally
even when a named secret was never restored into the fresh store: `apply` prints a
warning naming every such dangling ref, and the setup checklist's "Site config"
row grades `warn` (never the plain `info` of a fully-live config) while one
remains.

A captured document carries `onboarding_completed_at` whenever the install it
came from had finished the Getting Started funnel, and `apply` forwards it
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
(`onboarding_completed_at_ignored`) and `apply` prints it as a warning.

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
/site-config` and `wardyn site-config apply`.

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
`cidrs` (or the full RFC1918 + `fc00::/7` + 100.64.0.0/10 range when `cidrs` is
empty). Loopback, link-local, the metadata address, other reserved ranges, and
NAT64-embedded smuggling stay denied unconditionally — an entry can never lift
those, whatever `cidrs` says. Every declared CIDR must lie entirely inside that
liftable set; `PUT /site-config` 400s one that doesn't (`0.0.0.0/0`,
`169.254.0.0/16`, `127.0.0.0/8` are the obvious mistakes it catches).

This lifts ONE thing: the SSRF builtin. The run's own policy allowlist
(`allowed_domains`) still has to name the host separately. Two more exclusions
apply automatically: an address on the proxy's own network interfaces, and the
resolved control-plane (`wardynd`) host — the sidecar shares its Docker network
with Postgres/Dex/the registry container. On Kubernetes the sidecar's interface
carries only the pod's own address and the control plane's neighbours are
ClusterIP Services off that interface, so only the resolved `wardynd` address is
excluded there — declare tight `cidrs` (the workload namespace's pod/Service
ranges) and never a suffix matching the control-plane namespace's DNS zone (a bare
`svc.cluster.local` suffix with empty `cidrs` would reach every Service). The lift
applies wherever the proxy resolves a hostname for a direct dial — the sandbox's
CONNECT/plain-HTTP path, the MITM path, and the `git_pat` PAT-broker lane (its
forge host is grant-derived), so a declared suffix covering a self-hosted forge
lets the brokered PAT reach it. A lifted decision's audit `rule_source` reads
`site-config:internal-host` instead of the default `policy:allowed`.

```json
{
  "internal_hosts": [
    { "host_suffix": "registry.corp.internal", "cidrs": ["10.40.0.0/16"] }
  ]
}
```

### Bedrock on a private endpoint

Two topologies, told apart by which hostname the endpoint's TLS certificate
names. Get it wrong and the handshake fails on an SNI/cert mismatch — the SNI
presented to the endpoint is the hostname the sandbox dialled (its own
end-to-end TLS on a resident-credential run, or the proxy's re-dial on a
bearer-injection run), and the dispatch-layer wiring cannot see a TLS failure.

- **Private DNS enabled — a cert for the *public* host (the common shape).**
  Leave `WARDYN_BEDROCK_BASE_URL` **unset**. The sandbox keeps dialling
  `bedrock-runtime.<region>.amazonaws.com`, so the SNI stays the public host the
  cert names; the estate's private resolver answers that name into 100.64.
  Reach it by listing the public host in `upstream_proxy_no_proxy` (skip the
  corp proxy) and in `internal_hosts` with a `100.64.0.0/10` cidr (lift the
  guard). Nothing about the private address enters the TLS layer.
- **A cert for the endpoint's own name.** Only when the endpoint's cert
  actually covers its `…vpce.amazonaws.com` name (private DNS disabled, or a
  cert issued for it) set `WARDYN_BEDROCK_BASE_URL` to that hostname — then SNI
  and cert agree.

Pointing `WARDYN_BEDROCK_BASE_URL` at the `vpce` hostname against a public-host
cert is the trap: the sandbox presents the `vpce` name, the endpoint answers
with the public-host cert, the handshake fails. The composed dispatch test
proves the env vars propagate, not that TLS validates.

**The control plane is a second service.** Profile-id and
application-inference-profile models call `bedrock.<region>.amazonaws.com`
(`ListInferenceProfiles`/`GetInferenceProfile`), which `WARDYN_BEDROCK_BASE_URL`
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
`internal_hosts` and `WARDYN_BEDROCK_BASE_URL` govern the **sandbox** proxy;
`wardynd`'s own control-plane calls — OIDC discovery, JWKS, the Entra directory
connector, STS for a SigV4 Bedrock run — go out over its process HTTP client,
which carries no SSRF guard, so a private (100.64) issuer or Graph host is
dialled directly and boots fine. What that client *does* honour is the
process's own `HTTPS_PROXY`/`NO_PROXY` (the published images do not set them at
runtime): if you run `wardynd` behind the corporate proxy, add the private
issuer/Graph/STS ranges to the process `NO_PROXY`, or the corp proxy — which
cannot reach an internal address — fails discovery at boot, and none of the
site-config fields above can fix it. For a split-horizon issuer (public URL,
internal resolution) use `WARDYN_OIDC_INTERNAL_ISSUER`.

### Internal model gateway

Point every run's model calls at an internal endpoint instead of
`api.anthropic.com`/`api.openai.com`. **Shipped for the api-key lane**:
`WARDYN_ANTHROPIC_BASE_URL` / `WARDYN_OPENAI_BASE_URL` ([ENV.md](ENV.md)) re-point
the proxy's own brokered `/wardyn/llm/anthropic` / `/wardyn/llm/openai` route at
the gateway — validated once at boot (`https://` only, RFC1918/CGNAT literal
allowed, loopback/link-local/metadata/multicast/NAT64 refused, must not equal the
public host) and forwarded to the proxy sidecar per run. No
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
entry, exactly as the public host does; wardynd warns at boot when the default
policy's `allowed_domains` omits a configured gateway's host. **Behind a corporate
upstream proxy, the gateway must be reachable FROM that upstream** — with
`upstream_proxy_url`/`upstream_proxy_secret_ref` also configured, every forward
dial (the gateway included) is CONNECTed through the corp proxy by the transport,
never dialled directly. A gateway the corp proxy cannot reach — an internal one,
typically — is what `upstream_proxy_no_proxy` is for: list its host there and the
gateway is dialled directly instead, then admitted by `internal_hosts` like any
other internal address.

**Scope: the api-key lane only.** A subscription or Wardyn-managed-token run
still talks to `api.anthropic.com` directly — the published agent images
unconditionally `unset ANTHROPIC_BASE_URL` whenever a resident/managed credential
is detected, and the harness-login (`claude setup-token`) lane is public too.
Routing those lanes through a gateway needs an image change (teaching `agent-run`
to honor an explicit operator-set base URL) — a named gap, tracked in ROADMAP.md.

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

`PUT /site-config` (and so `wardyn site-config apply`) still accepts a legacy
`artifact_overrides` body **for one release**, folding it into `egress_redirects`
before validating: `apply` replaces the *whole* document, so an operator
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

`integrations` — the rows behind **Settings**' Model provider and Git host cards,
plus generic rows stored under an earlier release — lives on the SAME `SiteConfig`
document `GET`/`PUT /site-config` reads and writes, but does not travel through
this door. `PUT /site-config` 400s outright on a body carrying a non-empty
`integrations` ("integrations are managed through their own endpoints, not PUT
/site-config") and always carries the STORED integrations forward onto whatever it
persists, regardless of what the body sent (`handlePutSiteConfig`,
`internal/api/site_config.go`). Same whole-document-replace reason as above: an
older client that `get`s a config saved before `integrations` existed, then
`apply`s it back unmodified, would otherwise silently delete every stored
integration.

The practical edge: once any integrations are stored, a fresh `wardyn site-config
get > corp-baseline.json` captures them too, and the client strips them back out
on the way in (`PutSiteConfig`, `pkg/client/families.go`) so the `apply` half does
not 400 on its own capture. That strip also means **`apply` never restores an
integration** — the ones in the file are dropped, the stored ones carried forward
untouched. `wardyn site-config apply` prints a warning naming how many it dropped.
Manage integrations through their own routes (`GET /api/v1/integrations`,
`PUT`/`DELETE /api/v1/integrations/{id}`).

`apply` also decodes the file strictly (`DisallowUnknownFields`, the same
validator the server runs): on a whole-document replace a typo'd key would leave
the real setting out of the body and delete it, so a misspelled field fails on the
host, before anything is sent.

**Optional `If-Match`.** `GET /site-config` returns an `ETag` (a content hash of
the document); a `PUT` carrying it back as `If-Match` is refused `412` if the
document changed underneath — two admins editing the same config, or a stale
`corp-baseline.json` applied after someone else's `PUT` landed. Omitting
`If-Match` works exactly as before, and `wardyn site-config apply` today sends
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
(`site_config.test_proxy` / `site_config.test_redirect`); the audit row carries
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
floor is enforceable (a CC2 floor with no RuntimeClass registered otherwise fails
the probe before it reaches the network, reading as a proxy problem it is not; see
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
`WARDYN_AGENT_IMAGES` must be rebuilt from 0.6.6 (or use the published
`agent-base:0.6.6`), or its recorder keeps the pre-0.6.6 60s upload tail. A probe
is bounded well under two minutes and reclaims (kills) its sandbox if the run
doesn't finish in time, so a wedged probe can never hold one open.

**`warning` (both endpoints, `omitempty`).** Set alongside a `reached` verdict
when the probe's OWN session recording never reached the control plane even
though egress worked — the "probe passes, recordings silently vanish" case.
Present only when a `RecordingStore` is configured and the runner advertises
session recording; absent (never an empty string) otherwise.

**Recording upload path on Kubernetes.** Every exec-mode run's task is wrapped by
`wardyn-rec`, which PUTs the finished recording to the proxy pod
(`http://wardyn-proxy:3128/wardyn/v1/recordings/<runID>`), which forwards it to
`WARDYN_CONTROL_PLANE_URL` — the chart points this at the control plane's
in-cluster Service FQDN. Delivery failure is deliberately non-fatal to the task
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
  Connector **failures** are audited (`directory.search_failed`), with the
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

Dispatch used to set `GOTMPDIR`/`GOCACHE` and the Maven/Gradle JVM proxy sysprops
(`MAVEN_OPTS`/`GRADLE_OPTS`) on every run, on every image. It no longer does: a
workspace run gets exactly the groups its attached sources' scans detected — the
Go group only when a scan found Go, the JVM group only when it found
Maven/Gradle, the union across every attached source (`buildBaseSandboxEnv`,
`internal/api/runs_dispatch.go`). A run with no workspace attached at all —
ad-hoc, a bare `--image` override, scan, or login runs — keeps the full set:
nothing was scanned and nothing declared, so "unknown" must not silently break
those lanes. A workspace whose OWN base image is registry/custom/BYO is not this
lane: the workspace stays attached (`req.Image` is set from it without leaving
`wsRefs`, `internal/api/runs_create.go`), so `runToolchainNeeds` still narrows to
what that workspace's scan found — only a workspace with no attachment at all, or
one lacking a decodable scan profile, falls back to the full set.

`GOTMPDIR` needs the directory to exist, and unlike `GOCACHE` the go tool refuses
to create it — `go test` compiles and EXECS its test binaries there, and the
sandbox mounts `/tmp` noexec, so the first Go command in an image that never
pre-baked the directory failed with `stat ...: no such file or directory`. Nothing
toolchain-specific is baked into any image for this; two runtime guards create the
directory from the env var alone:

- `agent-run`'s session prep (`make_toolchain_dirs`,
  `deploy/images/common/agent-run-lib.sh`), a no-op when `GOTMPDIR` is unset;
- the attach shell's exec wrapper (`internal/runner/docker/session.go`), which
  runs the same `mkdir -p "$GOTMPDIR"` guard before the prompt renders — session
  prep was measured taking 18s to reach its own mkdir while an attach shell opens
  instantly, so a fast first command would otherwise lose the race.

## Recommended builds on compose

"Recommended — built for this workspace" (a devcontainer build via
`internal/envbuild`; mechanism in [ENVBUILD.md](ENVBUILD.md)) works out of the box
on the compose stack. Four things ship pre-wired:

- a loopback OCI registry sidecar (`WARDYN_ENVBUILD_PUSHED_REF` defaults to
  `127.0.0.1:5010/wardyn/devcontainers`, the host-side pull ref — Docker exempts
  `127.0.0.1` registries from TLS, and it is not reachable off-host; wardynd
  pushes via the in-network `WARDYN_ENVBUILD_CACHE_REPO`,
  `registry:5000/wardyn/devcontainers`);
- builds default ON (`WARDYN_ENVBUILD` defaults to `true` on compose; the
  bare-binary/host-mode default is still off);
- the build context is delivered as a tar streamed into the build container, not
  a bind mount by path (a path staged in the control plane's own filesystem is
  invisible to the host Docker daemon, and built an empty workspace);
- the build container's capability drop grants exactly the file-ownership set an
  image builder needs instead of dropping everything, which otherwise breaks
  rootfs extraction for any featureful build.

### Every generated image carries the claude-code CLI as standard tooling; nothing bakes codex-cli

Every Wardyn-GENERATED recommended image bakes `claude-code` as a real layer —
unconditionally, the way it carries git or curl, regardless of which integrations
the workspace names. Mechanically: a `.devcontainer/Dockerfile` the emitted
`devcontainer.json` points `build.dockerfile` at, carrying a checksum-verified
native install (architecture-detected, sha256-checked against the release
manifest) that runs as root, before every devcontainer feature, inside the
hardened build container (`genStandardTools` folded into `GenerateDevcontainer`,
`internal/workspacescan/gen.go`; proven with a gated integration test that runs
the built image and checks `claude --version`). codex-cli is not in the standard
set — no Wardyn-verified native-download contract, and its npm lane would need a
Node runtime the bake stage doesn't carry: the image is built without it, never
with a guessed URL. A devcontainer's own `onCreateCommand`/`postCreateCommand`
cannot be used either way: envbuilder runs lifecycle commands AFTER the image is
pushed, so they never reach the delivered image.

**This only applies to Wardyn's OWN generated devcontainer.** When the workspace's
primary source is a repo carrying its own devcontainer file (and it is
HTTPS-cloneable — an SSH source falls through to the generated path, since the
image builder has no SSH-clone wiring), `resolveWorkspaceImage`
(`internal/api/workspace_run.go`) builds that devcontainer AS-IS via
`ImageBuilder.BuildDevcontainer` — cloned and built verbatim, never injected into.
An agent CLI is present there only if the repo's own devcontainer installs it; an
agent run on an image without one fails at the CLI, visibly, rather than being
silently patched.

## Rotating the age key

The secret store binds **one** age identity for both encryption and decryption
(`internal/secretstore/pg`), so simply changing `WARDYN_AGE_KEY` migrates nothing
— it strands every existing ciphertext, and wardynd then fails closed on the
first decrypt rather than starting.

`wardynd -rotate-age-key <key-file>` is the supported rotation, a **maintenance
mode, not a server start**: it mints a new identity, re-encrypts every row of the
`secrets` table from the current key to the new one in ONE transaction, replaces
the key file, writes a `secret.rekey` audit event, and exits. It never opens a
listener and never dispatches a run. Three properties:

- **The daemon must be stopped.** A serving wardynd holds the OLD identity in
  memory for the life of the process; after a rotation it decrypts nothing and
  would write any newly-stored secret under the retired key. A Postgres advisory
  lock (`db.SecretRekeyLockKey`) refuses a second concurrent *rotation*, but it
  cannot see a serving daemon, so stopping it is **your** step, not one the tool
  enforces.
- **All-or-nothing.** The whole re-encryption runs in ONE transaction. A row the
  current key cannot decrypt aborts everything with an error naming that secret
  and how far it got (`rekey ABORTED after 3 of 9 rows …`), and nothing is
  committed — every secret is still readable with the old key. There is no
  half-rotated state to diagnose.
- **The CLI never sees the key.** `wardyn` has no rotation surface at all; this
  is a `wardynd` flag, run by whoever has shell access to the key file.

The key file is a **bare `AGE-SECRET-KEY-…` line** (`#` comment lines allowed, so
`age-keygen` output works as-is) — *not* an env file. It must already hold the
identity `WARDYN_AGE_KEY` names, or the rotation is refused: this file is
replaced, and pointing the flag at `deploy/compose/.env` would overwrite it.

```sh
# 0. Take the Postgres dump above FIRST. It is the only rollback for the data
#    half; the .bak below is only the rollback for the key half.

# 1. Stop the daemon. Nothing may be writing secrets during the rotation.
docker compose -f deploy/compose/docker-compose.yaml stop wardynd

# 2. Put the CURRENT key in a key file, if it is not already in one.
#    (Compose keeps it as a WARDYN_AGE_KEY= line in deploy/compose/.env.)
umask 077
grep -E '^WARDYN_AGE_KEY=' deploy/compose/.env | cut -d= -f2- > ~/.wardyn/age.key

# 3. Rotate. The old key still comes in via WARDYN_AGE_KEY; the new one is
#    generated here and lands in the key file.
WARDYN_PG_DSN='postgres://…' WARDYN_AGE_KEY="$(cat ~/.wardyn/age.key)" \
  ./bin/wardynd -rotate-age-key ~/.wardyn/age.key
# INFO wardynd: age key rotated; … secrets=7 key_file=/home/you/.wardyn/age.key
#      public_recipient=age1… rollback_copy=/home/you/.wardyn/age.key.bak

# 4. Put the NEW key back where the deployment reads it from, then restart.
#    Compose: rewrite the .env line. Helm: update the Secret's age-key entry.
docker compose -f deploy/compose/docker-compose.yaml up -d wardynd
```

Verify the same way the restore runbook does — a row count proves nothing about
decryptability, so launch a run against a workspace that depends on a stored
secret and confirm it starts. The audit trail records the rotation itself:

```sh
docker exec -i wardyn-postgres psql -U wardyn -d wardyn \
  -c "SELECT time, data FROM audit_events WHERE action='secret.rekey' ORDER BY time DESC LIMIT 1;"
```

**Rollback.** The previous key file is kept as `<key-file>.bak`, `0600`, until
*you* delete it. Restoring it is only half an undo: the database is already
re-encrypted, so `.bak` is usable **only** together with the Postgres dump from
step 0. Once the rotated deployment is confirmed working, delete `.bak` — leaving
it leaves a second copy of a retired master key on disk.

If a step after the commit fails (the key file could not be replaced), the error
says so and names `<key-file>.new`, which holds the new identity and at that point
is the **only** key that reads the store. Save it before doing anything else.

Whatever you do, **back the key up off-host.** Rotation re-encrypts what is there;
it cannot recover a key you have already lost.

## Upgrades

Migrations are **forward-only**. `internal/db` records each applied filename in
`schema_migrations` and applies anything new on boot, under an advisory lock so
concurrent starts do not race. There are no `down` migrations and no downgrade
path — a rollback to an older wardynd against a migrated database is unsupported.

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
drive` refusal (`internal/runner/docker/driver_mounts.go:120-123`) for this
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
(`0.6.0` from this release on), so a stock install is fine; an `image.tag`
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
WARDYN_VERSION=0.6.6                                     # the release you are moving TO
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
through `0049`, so this path applies `0050`–`0061`, and most of it is exactly
this shape: `0050` (secrets), `0052` and `0060` (api_tokens, created back in
`0045`) and `0055` (workspaces) are `ALTER TABLE` on tables an earlier release
created — `0050` also drops and re-adds a primary key, `0060` drops and re-adds
a CHECK — and `0056`, `0057` and `0058` are three successive
`CREATE OR REPLACE`s of the chain function `0047` created, each re-creating its
trigger on `audit_events`. (`0053` alters `role_mappings`, which `0051` CREATES
two migrations earlier in the same run, so it is not an instance of the hazard.)
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
already committed, and **it boots anyway**: `migrateOn`'s apply loop iterates
only the binary's own embedded migration files and skips any name
`isMigrationApplied` already finds recorded (`internal/db/db.go:425-436`), so
nothing there refuses a schema newer than the binary. It then fails at the
first write the older schema no longer supports, not at boot — on the scenario
above that is the first secret write: `0050` moves the `secrets` primary key
from `(name)` to `(owned_by, name)` (`internal/db/migrations/0050_secret_owned_by.sql:19-24`),
and the older binary's `INSERT … ON CONFLICT (name)`
(`internal/secretstore/pg/pg.go:78` at `v0.6.6`; the same statement on this
branch already reads `ON CONFLICT (owned_by, name)`, `internal/secretstore/pg/pg.go:94`)
names a constraint that no longer exists, which Postgres refuses as
`SQLSTATE 42P10` ("no unique or exclusion constraint matching the ON CONFLICT
specification") on every secret upsert. Take the dump before the upgrade, not
after the refusal.

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

-- 4. Every FUTURE migration creates its tables as the MIGRATOR, and a new table
--    grants the app role nothing. Without this line the next upgrade boots an
--    app role that cannot read its own new tables.
ALTER DEFAULT PRIVILEGES IN SCHEMA public
  GRANT SELECT, INSERT, UPDATE, DELETE ON TABLES TO wardyn_app;
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

**Why an INSERT+SELECT-only role can write to a hash-chained table at all.** The
chain trigger function is `SECURITY DEFINER` and runs as its owner — the
migrator, which also owns `audit_events` — so allocating `seq` and reading the
chain head are the owner's acts, not the caller's (`0057`). Before that, a split
deployment upgrading past `0056` hit `permission denied for sequence
audit_events_seq_seq` on **every** audit insert, which pushed every write to the
spool and refused every credential mint. Keep the migrator as the owner of both
the table and that function; that pairing is what makes the posture work.

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

### Backup: what `pg_dump` carries here, and what it does not

The chart renders no database. `postgres.dsn` points at a Postgres you operate,
so the backup is your Postgres's own backup story — Wardyn adds no mechanism.
What it adds is three corrections to the compose recipe:

- **Recordings are NOT in the dump on a stock chart install.** The chart pins
  `WARDYN_RECORDING_STORE=fs` (`deploy/helm/wardyn/values.yaml`, the
  `persistence` block) — the *opposite* of wardynd's own `pg` default the compose
  recipe relies on to sweep asciicasts up with the database. With
  `persistence.enabled=false` (the shipped default) `WARDYN_RECORDING_DIR`
  renders empty and replay is off, so there is nothing to lose. Turn
  `persistence` on and every asciicast lives on that PVC alone: `pg_dump` will
  not carry them, and the PVC needs its own snapshot. Setting
  `env.WARDYN_RECORDING_STORE=pg` instead puts them back in the dump.
- **The age key is a Secret, not a `.env` line.** See below; still the item that
  makes the difference between a restorable dump and a file of undecryptable
  ciphertext.
- **The audit spool is not a backup target.** `WARDYN_AUDIT_SPOOL` renders to
  `/tmp/audit-spool.jsonl` on a stock install and onto the PVC beside the
  recordings once `persistence` is on (`templates/deployment.yaml`), so turning
  persistence on sweeps the spool up too — neither needs restoring. Derived by
  design (`internal/api/auditspool.go`): the fallback for a failed Postgres write,
  draining back into the database. Postgres remains the source of truth for the
  audit log on both substrates. The one file beside it that is NOT derived is
  `<spool>.quarantine`: it holds events the store permanently refused, which are
  by definition absent from the database, so keep it until you have re-fed or
  triaged its lines.

### Restore: rehearse into a scratch database first

Two steps are Wardyn's, and both are cheap:

**1. Nothing may run against the database mid-restore.** The compose recipe's
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
boot cannot decrypt what its first wrote, and because the control plane loads its
own keys during startup (`loadOrCreateSecret`, `cmd/wardynd/main.go`) it fails
closed there, before serving — a `CrashLoopBackOff`, not a degraded pod. Hence the
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
ERROR wardynd: fatal err="load secret \"wardyn-signing-key\": pg secretstore: decrypt wardyn-signing-key: age decrypt: no identity matched any of the recipients"
```

That is the correct behaviour — `loadOrCreateSecret` fails closed on a decrypt
error rather than minting a fresh key over the existing one, which would strand
the old ciphertext permanently instead of loudly. But it is unrecoverable from
inside the cluster: [Rotating the age key](#rotating-the-age-key) re-encrypts a
store you can still *read*, and the key that reads this one is exactly what is
missing. The fix is always "put the original Secret back", never "generate a new
one". Back the Secret up off-cluster, wherever the DSN Secret is backed up, and
treat deleting it as equivalent to deleting the database.

**Rotating it on k8s** uses the same runbook
([Rotating the age key](#rotating-the-age-key)), with two differences.

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
`cmd/wardynd/main.go`), through the same `loadOrCreateSecret` path as the signing
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
alike, so no drive backend offers one.

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

**RBAC is two verbs.** `userDrives.enabled=true` adds exactly
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
old ones, on purpose: Wardyn holds no `delete` verb, and a rename must never be
able to destroy storage. The `wardyn.drive` label carries the drive's row **id**
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
/drives/{id}?confirm=rehome."* Four fields count as identity-affecting —
`backend`, `home_template`, `host_root`, and a `name` that folds to a
**different slug** (a purely cosmetic rename that folds to the same slug is not
refused, and neither is any edit to a drive nothing is allocated from).

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

**Two states are refusals, not warnings.** A claim that is **Terminating** fails
the run outright: a pod mounting a claim under deletion never schedules, and
re-creating it under the same name would undo the reclaim somebody is in the
middle of. So does a claim whose IDENTITY labels are not this run's — a managed
claim whose `wardyn.drive` or `wardyn.home` names a different pair, or whose
`wardyn.subject` names a different person (that third label is checked only when
it is PRESENT, so claims stamped before it existed still mount), or a share
whose claim turns out to carry `wardyn.managed=true` (i.e. it is one person's
managed drive, not an admin's share). That one is the collision the object name
cannot rule out: `wardyn-drive-<drive-slug>-<home>` joins two variable-width
fields with the separator both of them admit, so drive `eng` + home `us-bob` and
drive
`eng-us` + home `bob` resolve to the same claim name. Wardyn holds no `delete`
verb and cannot repair the collision, so it refuses the run rather than mount
one member's private drive inside another member's agent. The fix is to rename
one of the two drives (see the rename caveat above) or to give the colliding
people distinct home names.

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
*"belongs to a different drive or a different person"*.

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
deliberate operator command, and Wardyn holds no `delete` verb that could do it
by accident:

```sh
kubectl -n <runsNamespace> delete pvc wardyn-drive-<drive-slug>-<home>
```

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
vocabulary this feature introduces (`filesystem` / `request` / `external` /
`none`): a managed claim is `request`, a share is `external`. It is the same
honesty the `DiskMiB` gap below is written with, and the two will converge on one
vocabulary.

## One replica, by construction

`replicas` is not a scaling knob and it is not modesty — **the pin is a safety
control.** No shipped topology runs more than one: compose pins `container_name`
(`--scale wardynd=N` is rejected outright) and the Helm chart both defaults
`replicas: 1` **and refuses to render above it**
(`deploy/helm/wardyn/templates/deployment.yaml`; `allowMultiReplica=true` is the
documented override, an acceptance of everything below, not a fix).

wardynd keeps this state per-process. The first entry is why the pin is a control
rather than a preference:

- **the secret-masking registry** (`internal/secretmask`) — an in-memory
  `map[runID][][]byte`, never persisted, and it **fails open**. Secrets are
  registered by the request that mints or injects them (`Broker.mint` on the mint
  route, `handleInternalInjection` on the proxy's injection call; the captured
  AWS SSO token registers process-*globally* via `AddGlobal`), so they land on
  whichever replica the run's proxy happened to dial. The session-recording
  upload (`POST /runs/{id}/recording`) and the live-attach relay are DIFFERENT
  requests that may land anywhere, and both pass the stream through unmasked when
  the run's snapshot is empty (`buildMaskingBody`, `liveMaskWriter`). Two
  replicas is therefore enough to persist an asciicast containing live
  credentials in cleartext — with a `success` audit event, because nothing in the
  path can tell "no secrets for this run" from "not my run". There is no
  cross-replica fix short of moving the registry into shared storage, which has
  not been built. **This is not bounded to two replicas either.** A single
  `wardynd` process restarting mid-run (upgrade, crash-restart, OOM) wipes the
  same in-memory map, so a run whose secrets were registered before the restart
  and whose cast uploads after it hits the identical empty-snapshot fail-open —
  with `replicas: 1` throughout. The pin removes the *cross-replica* case, not
  this one. The map does not grow without bound: a background sweeper evicts a
  run's entry once that run has been terminal for an hour (`api.RunSecretGrace`),
  late enough for the finalize audit and the cast upload to still see it.
- **the audit spool** — a local append-only file per pod
  (`internal/api/auditspool.go`). Per-process *by design*: the fallback for a
  failed Postgres write, each pod draining its own back into the database.
- **the age identity, when `WARDYN_AGE_KEY` is unset** — each process mints its
  own ephemeral one at boot (`buildSecretStore`, `cmd/wardynd`), so a secret
  written by one pod cannot be decrypted by any other. Fails closed (a decrypt
  error, never a wrong plaintext) and surfaces on `Get`, not at boot, so the pod
  starts healthy and the failure appears at first use. Setting the key removes
  this one entirely.
- **the docker driver's sandbox tracking maps** (`agentExecs`, `pending`,
  `mainProc`, `creating` in `internal/runner/docker/driver.go`) — the process that
  created a sandbox is the only one that can observe its agent exec (`Wait`), and
  `creating` is the in-memory tombstone that makes the exec-less (krun)
  create/teardown handshake atomic. A teardown handled by a pod that did not
  create the sandbox has neither, so a container can survive the kill it was
  supposed to die from.
- **the `/metrics` counters** (`internal/api/metrics.go`) — per-process, so a
  scrape reports one pod's slice of the fleet, not the fleet.
- **the decision-ingest `lastTouch` debounce** (`shouldTouch`,
  `internal/api/internal.go`) — per-process, so N pods can do up to N× the
  `TouchRun` writes the 30s debounce was sized for. Load, not correctness.

Six OTHER pieces are now Postgres-backed, so they survive a crash and no longer
break under a second replica: single-use **attach tickets**, delete-on-read
**compose results**, and the **lifecycle reaper** (migration 0026 + a
`pg_try_advisory_lock` around the reap tick, which skips a tick it does not win);
**run watchers** (migration 0027 — each run's completion watcher is still an
in-process goroutine blocked on `Runner.Wait`
(`internal/api/runs_dispatch.go`), but it refreshes a Postgres lease every 30s and
every replica sweeps for stale leases every 60s, so a run orphaned by a pod that
never comes back is adopted by any live replica within roughly 65-150s instead of
stranding forever — real latency, not instant, and slower than the same-pod
restart case `ReconcileOnBoot` handles alone); **session recordings** (migration
0028 — the process default is the Postgres-backed `pg` store, readable from any
replica; `WARDYN_RECORDING_STORE=fs` still selects the old per-pod directory); and
the **ground-truth token rotator** (`cmd/wardynd/gt_rotator.go`, leader-elected via
a Postgres advisory lock — a standby takes over within one ~30s backoff).

None of that makes `replicas > 1` supported. It closed the six reasons a second
replica used to drop *requests*; it did not touch the list above, and the masking
registry is a worse failure than any of the six — those lost work, this one
persists secrets. Keep `replicas: 1`. Going beyond it has not been built, tested,
or released, and the chart will not render it without `allowMultiReplica=true`.

## Kubernetes: known gaps

The `k8s` runner substrate (`deploy/helm/wardyn`, `k8s.enabled=true`,
`internal/runner/k8s`) is a **separate, independent confinement substrate** from
the Docker Compose path (L1/NetworkPolicy-backed vs. Compose's L0 structural one)
— most of this document applies to both, but the list below is what the k8s
substrate does NOT do yet. Each item is a real limitation checked against the
driver, not a guess:

- ⛔ **No BYOI or devcontainer builds.** A `wardyn-byoi/`-prefixed image ref is
  refused before any pod is created — ephemeral containers cannot honor the
  selftest-then-task double-exec BYOI needs (`internal/runner/k8s/errors.go`'s
  `errBYOIUnsupported`, `internal/runner/k8s/exec.go`). `WARDYN_ENVBUILD`
  devcontainer builds are Docker-only for the same reason, unaffected by
  `k8s.enabled`.
- ⛔ **No `local_dir` / host-path workspace mounts.** A policy with any
  `WorkspaceMounts` entry fails the run closed with a clear error
  (`internal/runner/k8s/sandbox.go`'s `errMountsUnsupported`) — a k8s pod has no
  path back to an arbitrary directory on wardynd's own host. Git-clone workspaces
  (`WorkspaceRepos`) are unaffected; only a *local directory* source is refused.
- ⛔ **No `~/.aws` / `~/.claude` host staging.** The same `errMountsUnsupported`
  refusal covers the RESIDENT-COPY credential path — there is no host filesystem
  to stage from. Use proxy-side injection instead: managed-subscription OAuth
  injection and the Bedrock AWS SSO exchange are substrate-agnostic (they happen
  at `wardyn-proxy`), so they work unchanged on k8s.
- 🟡 **No in-sandbox DNS.** Every sandbox pod is `DNSPolicy: DNSNone` with a
  single nameserver, `127.0.0.1` — nothing listens there, so a DNS query fails
  FAST (connection refused) rather than hanging out a real timeout
  (`internal/runner/k8s/sandbox.go`). Only the pinned `wardyn-proxy` sidecar
  resolves hostnames, matching the Compose substrate's proxy-only egress posture —
  parity, not a new gap, but the *mechanism* (a present-but-unreachable loopback
  resolver vs. Compose's no-resolver-at-all) is k8s-specific.
- 🟡 **No per-pod PIDs limit.** Kubernetes has no per-container "pids" resource
  the way Docker's `--pids-limit` does — a run's `ResourceLimits.PidsLimit` is
  accepted but not enforced, and wardynd logs a warning naming the run id each
  time (`internal/runner/k8s/sandbox.go`). **Recommendation**: set the node-level
  kubelet `podPidsLimit` (or your distribution's
  `SystemReserved`/`KubeReserved` PID accounting) as a cluster-wide fork-bomb
  backstop — coarser but real, and the only lever this substrate has today.
- 🟡 **`DiskMiB` is ignored, with a warning.** Same shape as the PIDs gap: no
  per-container writable-storage quota is wired up yet, so a requested disk cap
  is accepted, not enforced, and logged (`internal/runner/k8s/sandbox.go`). A
  cluster-level `ephemeral-storage` request/limit is the closest mitigation.
  **The honest wording for a size Wardyn does not enforce is now settled, and
  `DiskMiB` should adopt it.** User drives introduced an `enforcement`
  vocabulary — `types.StorageEnforcement`, one of `filesystem` (a quota binds
  it), `request` (a volume request; the storage class decides), `external`
  (somebody else's quota binds it) or `none` — and one frozen sentence the
  console and the docs both render verbatim:

  > Wardyn never enforces a drive's size itself. On Kubernetes the size is the volume request and the storage class decides whether it binds — block disks do, network-share provisioners do not. On Docker a managed drive has no byte cap, the same gap disk_mib has. A share is bounded by its own quota. The size you see is the allocation, not a guarantee.

  That sentence names this gap by its policy field, on purpose. A drive is
  `request` on a managed claim and `external` on a share; `disk_mib` is `none`
  on both substrates today, and the two will converge on the one vocabulary
  rather than on two ways of saying "accepted, not enforced".
- 🟡 **No k8s ground-truth correlator.** The Tetragon host-sensor → ground-truth
  pipeline (`cmd/wardynd/gt_rotator.go`, `wardyn-tetragon-ingest`, the
  `groundtruth` Compose profile) has no k8s-substrate equivalent — it is not
  referenced anywhere under `internal/runner/k8s`. A k8s deployment gets the
  NetworkPolicy-enforced boundary (proven live by the boot-time egress canary)
  but not the independent kernel-level corroboration Compose + Tetragon provides.
- ⛔ **A pre-existing default-deny NetworkPolicy in `k8s.runsNamespace` refuses
  boot outright, with no override — unless the canary pod actually ran and could
  not connect.** The boot-time egress canary's phase A applies no NetworkPolicy of
  its own; it only proves the cluster is reachable before phase B proves Wardyn's
  deny-all rule takes effect. If the namespace already carries a default-deny
  policy from something else, phase A's pod is blocked too and wardynd refuses to
  boot with an INDETERMINATE verdict indistinguishable from a genuinely broken
  cluster (`internal/runner/k8s/canary.go`). **Fix**: give `k8s.runsNamespace` a
  namespace with no ambient default-deny, or exempt Wardyn's pods from *that
  policy's own* `podSelector` (a `matchExpressions` entry with `key:
  wardyn.managed`, `operator: NotIn`, `values: ["true"]`). **Do not instead add a
  separate allow policy for `wardyn.managed=true`**: NetworkPolicy allows are
  additive and both the agent and proxy pods carry that label, so such a policy
  widens every sandbox pod's egress past Wardyn's per-run deny+proxy-only policy
  (`internal/runner/k8s/sandbox.go`) and flips the canary's phase B to "CNI does
  not enforce" — which in turn invites `WARDYN_K8S_ALLOW_UNENFORCED_NETPOL=1` and
  fully unconfined runs. If the ambient default-deny is expected — a managed,
  multi-tenant cluster where a platform team applies the baseline — and the canary
  pod DID reach Running with its own connect exiting exactly 1 (not "never reached
  Running", not any other exit code), `WARDYN_K8S_ACK_AMBIENT_DEFAULT_DENY=1`
  acknowledges that shape and boots anyway. It is never proof of enforcement
  (phase B is skipped); the setup checklist's `k8s_egress_containment` row grades
  this `warn` ("acknowledged, not proven"), never `ok`. The exempt-the-podSelector
  fix is still the way to get REAL proof.
- 🟡 **`replicas` stays 1 on k8s exactly as everywhere else** — see
  [One replica, by construction](#one-replica-by-construction); the masking
  registry is still in-process, per-pod.

None of these are silent: the mount and BYOI gaps fail the run closed with a named
error, the resource-cap gaps log a warning naming exactly what is unenforced.
Closing any of them is unstarted work, not a documented-but-planned near-term item
— see ROADMAP.md for what is actually queued.
