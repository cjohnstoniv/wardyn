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

Three stores hold data that exists nowhere else. Lose any of them and the loss is
permanent.

| Store | Where | Holds | If you lose it |
|---|---|---|---|
| Postgres | volume `<project>_postgres_data` | runs, approvals, workspaces, policies, encrypted secrets, the append-only audit log — and, under the default `pg` recording store, the PTY asciicasts too | everything |
| Recordings | volume `${WARDYN_NS:-wardyn}-recordings` (`WARDYN_RECORDING_DIR=/data/recordings`) | PTY asciicasts for Replay — **only with `WARDYN_RECORDING_STORE=fs`**; the shipped default (`pg`) keeps them in Postgres and leaves this volume empty | every session replay it holds; nothing reconstructs them |
| Age key | `WARDYN_AGE_KEY` in `deploy/compose/.env` | the X25519 identity every stored secret is encrypted to | every secret in Postgres becomes undecryptable ciphertext |

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

# 5. Now bring up the rest of the stack.
make setup

# 6. Verify — row count first:
docker exec -i wardyn-postgres psql -U wardyn -d wardyn -c "SELECT count(*) FROM audit_events;"
#    then prove the age key actually decrypts what came back, which a row
#    count alone can't: launch a run against any workspace/policy that
#    depends on a previously-stored secret and confirm it starts instead of
#    failing closed with a decrypt error (see "Rotating the age key" — the
#    wrong key fails exactly here, not at boot):
wardyn run --agent claude-code --workspace <workspace-id>
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

A trigger you have hardened with `ALTER TABLE … ENABLE ALWAYS TRIGGER`
(`tgenabled='A'`, so it fires even under `session_replication_role = replica` —
the bypass the sweep otherwise only catches after the fact) is left **exactly as
it is**: the boot check counts `'A'` as firing, never re-creates it as plain
`'O'`, and never refuses over it. `'D'` (disabled) and `'R'` (replica-only, which
does not fire for ordinary writes) are correctly read as not in force.

Completeness survives an outage too. When a Postgres write fails, the event is
not dropped: it is fsync'd, one JSON line at a time, to a local append-only spool
(`WARDYN_AUDIT_SPOOL`, default `./data/audit-spool.jsonl`, empty to disable —
`internal/api/auditspool.go`), and a background drain replays it into Postgres
once the store recovers. The spool is per-process by design: the fallback for one
pod's failed write, each `wardynd` draining its own back on recovery (see
[One replica, by construction](#one-replica-by-construction)).

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
drained back to 0 no longer implies the queryable trail is complete.

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

### The hash chain — what a rewritten row looks like

The triggers above stop `UPDATE`/`DELETE`/`TRUNCATE` *through Wardyn's schema*,
and the role split hardens that against the app role. Neither binds a **table
owner or superuser**, who can `ALTER TABLE … DISABLE TRIGGER` and rewrite a row —
the residual `0007_audit_least_privilege.sql` states plainly. The role-split check
that reports this posture at boot (`AuditDDLProtected`) counts THREE ways to
bypass, not two: superuser, membership in the owner role, and the **`TRIGGER`
privilege** on `audit_events`. The third is the quiet one — a role granted
`TRIGGER` cannot drop the shipped guards, but it can add a BEFORE INSERT trigger
of its own whose name sorts after `audit_events_chain` (same-event row triggers
fire in name order) and overwrite `prev_hash`/`row_hash` on the way in, minting
rows that hash to whatever it says while every shipped guard is still armed. So a
deploy that grants `TRIGGER` back is reported as NOT protected.

**The app role's grant set does not grow to keep the chain working.** `INSERT`
and `SELECT` on `audit_events` is still the whole of it. The chain trigger
allocates the row's `seq` itself (so position and chain link are one decision —
see the serialization paragraph below), which is a privileged operation the
identity default never was, so the trigger runs `SECURITY DEFINER` with a pinned
`search_path` (`0057_audit_chain_security_definer.sql`): the sequence read
happens as the *owner*, not as whoever inserted. Nothing is widened for the app
role — a trigger function cannot be called directly — and a split-role deploy
needs no new `GRANT`. Migration
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
event on an audit sink stream (`WARDYN_AUDIT_SINKS`) carries its
`prev_hash`/`row_hash`, so a SIEM holds head hashes Wardyn cannot later disown —
that comparison, not the sweep, is the control. Signed receipts (a key the
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

**Every writer is serialized, including one that is not Wardyn.** The chain link
and the row's `seq` are allocated together under one advisory lock held inside
the insert trigger (`0056_audit_chain_serialize.sql`), so a direct `INSERT` from
`psql`, a seed script or any future code path takes its place in line rather than
reading the same head as a concurrent Wardyn write. Before that, two writers
could chain to the same head and the sweep reported a **tamper that never
happened** — permanently, per the latch above. The cost is honest: a session
that holds a transaction open after inserting into `audit_events` blocks every
other audit append until it commits or rolls back, so do not leave an interactive
`psql` transaction sitting on that table.

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
working). Beside them, `wardyn_audit_spool_quarantined_total` counts events the
store permanently refused and the drain moved aside (see the spool paragraph
above): non-zero means the trail is missing those events even though the spool
drained. Scrape with any Prometheus `authorization` config carrying the admin
token. `/healthz` stays the liveness/component surface (identity, runner classes,
eBPF ground-truth state); `/metrics` is the trend surface. Audit sinks
(`WARDYN_AUDIT_SINKS`, [ENV.md](ENV.md)) are the event stream for SIEMs — metrics
carry no per-run detail.

## Multi-user: who can change what

The API authenticates with **either** an OIDC session (human SSO) **or** the
admin bearer token; local mode skips both on a loopback-only bind. That is
authentication. Authorization is a real two-role model: every OIDC session
carries an **admin** or **member** role, derived once at login
(`internal/auth/oidc`'s `deriveRole`) and stamped into the signed session
cookie — a cookie signed before this existed (pre-0.5) decodes as no session,
forcing a re-login that derives one fresh.

| The merged map (chart `WARDYN_OIDC_ROLE_MAP` + console People-step rows) | Signed-in humans | Admin token / local mode |
|---|---|---|
| empty | listed in `WARDYN_OIDC_OPERATOR_EMAILS` → **admin**, others → **member**; all **admin** only when the allowlist is also unset (override-only under OIDC — the pre-0.5 behavior) | always **admin** |
| non-empty | mapped by `roles`/`groups`/email claim to **admin** or **member**; no match falls through to `WARDYN_OIDC_DEFAULT_ROLE`, or denies the login when that is also unset | always **admin** |

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
signed-in email. **Any match resolving to `admin` wins** over one resolving to
`member`, whichever claim produced it. `WARDYN_OIDC_OPERATOR_EMAILS` is **not
replaced**: an email on it is still an *additional* `admin` match
(`LegacyAdminEmails`), so a deployment adopting the role map keeps its current
operators with zero re-configuration. `WARDYN_OIDC_DEFAULT_ROLE`
(`admin`/`member`, unset = deny) covers everyone the map doesn't name.

Both are validated at **boot**, not at first use: a malformed entry (invalid role
value, non-ASCII key — matching is ASCII-only, so it could never match —
duplicate key, or non-blank input with no valid entry at all) or an invalid
`WARDYN_OIDC_DEFAULT_ROLE` fails wardynd's boot outright, naming the var
(`buildOptionalFeatures`, `cmd/wardynd/boot_deps.go`) — never a silent fallback
that lets a typo reach a session cookie later. A signed-in human who matches
nothing in a valid map, with no default role set, is denied at login instead
("no Wardyn role assigned").

**What admin-only still means** — the writes with the widest blast radius stay
gated on the role being exactly `admin` (`requireOperator`). Status icons in the
tables throughout this document: 🟢 open/works · 🟡 partial or narrowed · ⛔
refused.

| Surface | Gate |
|---|---|
| managed harness credential; policy create/update/delete; `PUT /site-config` + its connectivity probes; `GET /metrics`; the permissioning routes below | ⛔ admin only |
| the `/workspaces` routes that WIDEN AN EGRESS CEILING, BIND CREDENTIAL MATERIAL or WRITE THE HOST — `approved-egress`, `denied-egress`, `llm-cred`, `requirements`, `record` + `promote-egress`, `env-as-code/write`, `reassign` | ⛔ admin only |
| workspace CRUD/scan/build | 🟡 owner-or-admin since 0.6 ("Workspace ownership") |
| `devcontainer_repo` on a run (`denyMemberRequest`, `internal/api/runs_create_validate.go`) | ⛔ admin only, never grantable |
| a custom sandbox `image` | 🟡 admin by default; the one power a capability grant can hand a member ("Capabilities") |
| a member's own onboarded-workspace base image | 🟢 never gated — operator-authored at onboarding, not the member's free-text choice |
| the `/drives` routes — registering a **user drive**, allocating it to people or groups, previewing whose drive resolves (`mountUserDriveRoutes`, `internal/api/user_drives.go`) | ⛔ admin only, deliberately NOT the security-admin tier: a drive names a host path (`host_root`) or a cluster storage class, and "never the host" is the line between the two admin tiers |
| the user-drive **door** — `DenyUserDrive` on a governance profile (`internal/types/governance.go`) | 🟡 security admin too, through `/governance` — a limit on a profile, not a drive; it refuses the mount, it does not deallocate anything |
| mounting YOUR OWN drive on a run (`drive.enabled`) | 🟢 the person, per run — read-only unless their allocation says otherwise, and the run flag may only narrow that, never widen it |
| `POST /runs`, `POST /runs/{id}/kill` | 🟢 any signed-in human — using the product is a member act |

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
  action.** Deleting the allocation (`DELETE /drives/grants/{id}`, admin-only,
  audited `drive.grant.delete`) stops the mount at that person's next run and
  **deletes no data** — which is why the audit row carries the drive's declared
  `reclaim` intent (`retain` or `delete`), so the log records what the operator
  was told to do about the directory this allocation was the last pointer to.
  What is left behind is one object per person: a Docker named volume, or a
  subdirectory of the share the operator mounted host-side, or a
  PersistentVolumeClaim. A **managed** object (`docker_volume`, `k8s_pvc`)
  carries the drive row's **id** and the person's home name as labels, so a
  departed member's objects stay findable after the row is gone; a share's
  subdirectory and a static claim carry nothing — `POST /drives/preview` is how
  you name those. Reclaiming it is a deliberate operator command, one per
  substrate, and Wardyn holds no `delete` verb that could do it by accident: the
  recipes are "User drives on Docker" and "User drives on Kubernetes" in this
  document, and are not repeated here. `POST /drives/preview` prints the object
  name for a principal — paste the sign-in subject FIRST: on a `hash`/`sub` drive
  the name keys on the first claim, and the API's `home_subject` says which claim
  it used (the console does not yet show it). Deleting the **drive row** itself
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
  under a configured upstream the sidecar skips the SSRF guard for every host it
  proxies (all of them, minus `upstream_proxy_no_proxy`), so a
  member-substitutable value there would be a guard bypass, not a convenience.
- **`?owner=<principal>` is admin-only** on `PUT`/`DELETE`/`GET /secrets` (an
  admin's cross-write lands in the NAMED member's namespace, never the
  operator's), refused with a constant 403 for anyone else.
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
as, so the rule cannot apply. Local mode binds normally — the injected operator IS
a verified human, so `local:alice` deciding `local:alice`'s own run is refused.

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
`always` is **operator-only regardless of run ownership** (`decide()` rule 6,
same file): it writes a durable entry onto the run's workspace (`approved_egress`
on approve, `denied_egress` on deny) — the SAME two columns the
`approved-egress`/`denied-egress` routes write, both already `operatorOnly`, so
without this gate a member could reach them through the approval queue. The
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
(`wardyn-drive-<home>`), created on first use with the `local` driver and
mounted at the reserved target. Nothing to configure. It carries four labels:
`wardyn.managed=true`; `wardyn.drive` = the **drive row's id** (the object name
is per *person*, so the id is the only thing that groups a drive's volumes
together); `wardyn.home` = that person's directory name; and
`wardyn.subject` = a **digest** of the person
themselves (never their claim — see the restore note below). Reclaim is a
command, not a button:

- one person: `docker volume rm wardyn-drive-<home>` — `POST /drives/preview`
  prints the object name for a principal — paste the sign-in subject FIRST: on a
  `hash`/`sub` drive the name keys on the first claim, and the API's
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
  wardyn-drive-<home>
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
cannot be: a volume name carries only the *home*, so one drive whose home
template folded two people onto one directory would produce one volume that
*both* their allocations agree belongs to this drive. Wardyn refuses to mount a
volume stamped for a different person. You need not compute the digest for a
restore (it is a truncated sha256 of the sign-in subject): **leave
`wardyn.subject` off** the `docker volume create` above and the volume mounts,
exactly as a label-less `wardyn.drive` does.

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
   per-person home override, where it can happen. On a **managed** drive there
   is nothing to inspect, so `email_local` is **refused outright** — a
   `docker_volume` or `k8s_pvc` drive registered with it answers a `400`
   beginning `invalid drive: home_template "email_local" is not allowed on a
   managed backend` and going on to name the two templates that do work. Wardyn
   names a managed object after the home and nothing else, so those two people
   would be allocated one volume, with write access to each other's files
   whenever the drive is writable; use `hash` (the default) or `sub`. A row
   written before this rule is refused at *run* time too (`drive: this
   deployment cannot mount your drive (…)`), and every managed volume carries a
   `wardyn.subject` label — a digest of the principal, never the claim — that
   the driver refuses to mount for anybody else.

3. **Set the ceiling**: `WARDYN_USER_DRIVE_HOST_ROOTS=/srv/wardyn-drives`
   ([ENV.md](ENV.md)). Unset means **no `host_path` drive may be registered at
   all** — the same fail-closed posture `WARDYN_MEMBER_WORKSPACE_ROOTS` takes,
   one level up: a drive's `host_root` is authored in the database by an admin
   and its subdirectories are bound into *other people's* sandboxes, so the
   allowlist over it lives where a console compromise cannot reach it. The
   driver re-checks the **symlink-resolved real path** against these roots as
   the last thing before the container is created, so a home directory replaced
   by a symlink out of the share after the drive was registered is refused at
   run time too — and it now also checks that the resolved directory is still
   **named after the person it resolved for**, which is what catches a home
   replaced by a link to the home *next to it* (inside the roots, so the ceiling
   alone would allow it). A home symlinked onto a second export still works, as
   long as that export is also a configured root and the directory keeps its
   name.

   **Two `host_path` drives may not nest.** Registering a drive whose
   `host_root` is inside — or contains — another `host_path` drive's `host_root`
   answers `422`, naming the other drive. Drives on the *same* root are fine
   (one share, two allocations with different home templates), and so are
   sibling trees; what is refused is one drive rooted inside a tree whose
   directories another drive's members can rewrite from inside a run.

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
with `drive: this deployment cannot mount your drive (host_root … could not be
resolved on this host …)` when the **root itself** is what wardynd cannot
resolve, and with `drive: directory <home> does not exist on the share — ask an
admin to create it` when the root resolves but the person's directory does not
stat (a home that was never created, and a home behind a directory whose
permissions hide it, are the same sentence). The sandbox's own mode comes from
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

Group membership is a **snapshot taken at login**, carried in the session cookie;
grants are read from the database per request, so a new grant takes effect on the
very next request but a *directory* change does not until the human signs in
again. The snapshot is capped at 2048 payload bytes (`maxSessionGroupsBytes`,
`internal/auth/oidc/derive.go`) so the signed cookie stays under the ~4096 bytes a
browser silently drops entirely; groups are sorted and dropped **from the end**,
so the same human loses the same groups every login instead of a coin flip. That
is roughly 100 typical group names — past that, grant the user directly, or prefer
Entra App Roles on the much smaller `roles` claim. A pre-0.6 session cookie
carries no groups field at all and stays valid (no forced re-login); that state is
reported distinctly as `groups_snapshot_stale` on `GET /me/capabilities`, because
"can't tell yet" and "holds no groups" must not read the same.

**A DENY is never allowed to evaporate with the snapshot.** A group that fell off
the 2048-byte cut — or a caller still holding a pre-0.6 cookie, or a pre-0.7 API
token whose completeness was never recorded — has none of its group rows in the
scan. For an ALLOW that costs the caller access, which is the safe direction. For
a DENY it would hand back exactly what the row forbade, so the resolver
(`capScan`, `internal/api/capabilities.go`) checks whether **any** group-subject
deny row of that kind could cover the value, and refuses when one could — the
same scoping the ceiling refusal gets: a deployment with no group deny rows
behaves byte-for-byte as it did before. The refusal reads as an ordinary
capability denial, with a server log line naming the unanswerable snapshot;
signing in again (or re-minting the token) resolves it for good. Where a deny has
to bite with no store read at all, write it against the **user** (either
identity).

**A third cause of a partial snapshot: the IdP's own overage.** Entra ID stops
sending the `groups` (or `roles`) claim altogether once a human is in more groups
than the token limit — **200** for a JWT, 150 for SAML — and sends a `_claim_names` /
`_claim_sources` pointer to Microsoft Graph in its place. Wardyn does not
dereference that pointer; it marks the snapshot **truncated** (`sessionGroups`,
`internal/auth/oidc/derive.go`), which reads downstream exactly like a group that
fell off the byte cap: the ceiling resolver treats it as unanswerable rather than
as "asked, there were none". Without that, such a login would arrive
complete-and-empty and quietly shed every group-tier grant and governance
assignment. Where members legitimately sit in that many groups, prefer Entra App
Roles (the much smaller `roles` claim) or user-subject grants — or configure the
group claim to emit only the groups assigned to the application.

**What a capability deliberately does not reach.** `always`-scope decisions stay
operator-only even for a member granted the host — a grant must never promote a
member's decision into durable workspace config. `GET /workspaces` is not
narrowed: visibility is not capability, the launch gate is what refuses. Machine
lanes (`/internal/*`, ground-truth ingest, attach tickets) are untouched. And
where the operator ceiling sets `allow_all_egress` the allowlist is not the gate
at all, so `egress_host` narrowing does nothing there — the operator's own
posture, not a switch that failed.

**Managing them** (all `operatorOnly` except the last):

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
| `GET /api/v1/tokens` | admin | every token in the deployment |
| `DELETE /api/v1/tokens/{id}` | admin | revoke anyone's |

Revoking a human (`POST /api/v1/sessions/revoke`, `wardyn sessions revoke`) also
revokes every unrevoked token that principal holds — a token is their session in
another form. The `all` arm is deployment-wide for tokens too: EVERY live token
goes, the calling admin's own included — plan to re-mint after a global revoke.

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

**The role is a stamp, not a live check.** A token carries the role its owner held
when they minted it, exactly as a registered SSH key does. Demoting a human from
admin to member does **not** reach their outstanding tokens — revoke them with
`DELETE /api/v1/tokens/{id}`, which is also the path for a departed owner's
credential. Both `token.create` and `token.revoke` are audited
([`docs/AUDIT-ACTIONS.md`](AUDIT-ACTIONS.md)); the revoke row names the token's
owner.

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
admins author and assign **governance profiles** (named ceilings bound to users or
groups), write the org allow/denylists (capability grants), decide escalated
approvals — egress, credential, tool — on anyone's run, revoke sessions and API
tokens, and verify the audit chain. They cannot touch the People page,
integrations, site-config writes, base images, or the deploy funnel — and they run
under a governance profile themselves if one is assigned to them, since only
`admin` is exempt from ceiling resolution. A profile can only make the deployer's
stored credentials *less* available, never more — and a security admin widening
their own egress is an audited act, visible in the log they cannot rewrite. No
capability grant can widen anyone to admin; that invariant is what makes
delegating `/permissions` safe.

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
| `admin_surface` | a member requested an admin-only route | ⛔ `403` |
| `not_owner` | a member reached a run/approval/recording, or a member-OWNED workspace (`owned_by`, migration 0048), that exists but isn't theirs | ⛔ `404` (byte-identical to missing) |
| `byoi_member` | a member named a `devcontainer_repo`, or an `image` they hold no grant for | ⛔ `403` |
| `capability_workspace` | `workspace_id`: a member named a workspace they aren't granted (`403`). Launching: an `inline_policy` `workspace_repos` entry for an ungranted workspace was dropped — the run still launches | ⛔ `403`, or 🟡 a drop |
| `capability_egress_host` | deciding: the approval's host isn't granted (`403`). Launching: member-authored allowlist entries were dropped from an `inline_policy` — the run still launches | ⛔ `403`, or 🟡 a drop |
| `capability_secret` | a member's `inline_policy` grant referenced a secret they aren't granted — dropped, not rejected | 🟡 drop |
| `capability_agent` | `agent`: a member named an agent they aren't granted (`denyMemberRequest`, `internal/api/runs_create_validate.go`) | ⛔ `403` |
| `capability_integration` | `integration_id`: a member named a model-provider integration they aren't granted (same seam). Tier 1 only — a workspace's own pin and the site default are never gated | ⛔ `403` |
| `governance_profile` | the member's assigned governance profile refuses this run SHAPE — `task_mode=exec`, an interactive run, `seed_auto_tools`, or codex-cli under hold-deriving rules. A profile refuses the shape, never the person: the same member launches fine without the refused field | ⛔ `403` |
| `grant_pairing_not_eligible` | a member's `inline_policy` paired a stored secret with a host the operator never eligible-listed (`filterMemberGrants`) — dropped | 🟡 drop |
| `second_human_required` | `WARDYN_EGRESS_SECOND_HUMAN` is set and the caller deciding an `egress_domain` approval is the run's own `created_by` (`requireSecondHuman`) — a different human must decide it | ⛔ `403` |

The drop rows are why `POST /runs` mostly *narrows* rather than refuses: a member
whose whole allowlist is ungranted gets a run with no member-authored egress, not
a `403`, because the run's admin-authored egress is still there. A drop is never
silent: it comes back as a **warning on the launch response itself** (a console
toast, `wardyn run`'s stderr), appears the same way in a preflight/Review dry-run
*before* launch, and is recorded as an audit event at launch — one event per
reason with the affected values beside it, not one per dropped host. Preflight
dry-runs are not audited: Review re-resolves on every edit, and a stream of
denials for a policy nobody launched is indistinguishable from denials that
actually bounded a run.

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
   /api/v1/sources`, `GET/PUT/DELETE /api/v1/sources/{id}`, `POST
   /api/v1/sources/{id}/scan` (`mountLibraryRoutes`,
   `internal/api/sources.go`) — there is no separate
   `/sources/{id}/requirements` route; a source's own contract rides the
   plain `PUT /sources/{id}` body (`handleUpdateSource`).
2. **Base image** (tier 2) — a shared catalog row: registry, custom, or BYO.
   "Recommended" is never a catalog kind — it is a per-workspace DERIVED
   build, excluded by a database CHECK constraint, not by convention.
   `GET/POST /api/v1/base-images`, `GET/DELETE /api/v1/base-images/{id}`.
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

With an upstream configured, **every** forward dial is `CONNECT`ed through it —
and a corporate forward proxy will not `CONNECT` to an internal address. So on an
estate whose endpoints are private (a VPC endpoint / PrivateLink, an in-cluster
service, a corp mirror on RFC 6598) every one of them times out.
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

**It changes which hop dials, and nothing else.** A bypassed dial falls straight
through to the same unconditional private/reserved-IP guard an unproxied dial
faces, and still needs its `allowed_domains` entry. So on a private-endpoint
estate the bypass and `internal_hosts` are one configuration in two fields, and
**neither works alone**:

| | Without `internal_hosts` | With `internal_hosts` |
|---|---|---|
| **No bypass** | corp proxy takes the dial, cannot reach an internal address → timeout | same; the guard never even runs |
| **Bypassed** | dialled directly, then refused `builtin:private-ip` | **reaches the endpoint** (`rule_source: site-config:internal-host`) |

The bottom-left cell is the safety property, not a rough edge: bypassing a host
never makes a private address reachable. A refusal there names its own cause in
the `X-Wardyn-Egress-Detail` response header and points at `internal_hosts`.

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
holds it). It replaced the old `artifact_overrides` map (one entry per package
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
reaches an allowed, credentialed host that nothing in the sandbox asks for. The UI
labels these rows `network only` so the gap stays visible.

**A `to` that is a literal IP** — the normal shape of a private endpoint — is
trusted as an egress target for the runs the redirect covers, on every path the
proxy vets (the opaque tunnel, the TLS-terminated token-injection path, and the
git/PAT brokers alike), and shows in the audit trail as `rule_source:
site-config:egress-redirect` rather than a generic policy allow. The trust comes
from the exact allowlist entry the substitution writes, so it is scoped to those
runs and to that address; a run the redirect does not cover is refused, and a
`denied_domains` entry still wins. `test-redirect` understands the shape too
(`redirectProbeTo`): it dials the `to` address while presenting the `from`
hostname for TLS, because a private endpoint's certificate names the public host
— probing the address directly failed verification and reported a correct
configuration as broken. It speaks the protocol the stored `to` names, never the
one `from` happens to be spelled with: only `to` knows whether the mirror serves
TLS or cleartext on that port.

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
| ⛔ `blocked` | Could not reach the proxy or the mirror. `detail` names the real cause — DNS failure, connection refused, TLS failure, timeout, or curl's own exit code — never a generic "failed". |
| ⛔ `bypass` | **The one that looks fine but isn't.** The mirror answers, but the public host it's supposed to replace is *also* still reachable, directly, from a sandbox. The redirect is configured but not enforced: a run can silently pull from the internet instead of the mirror, and every other signal — the row is filled in, the mirror answers — looks exactly like a working redirect. "Reachable" means the public host **answered** — any HTTP status, a 403 included, or a TLS-level reply — not that the fetch succeeded: a host that answers `403` is one the confinement class did not block. `test-redirect` only. |
| 🟡 `no_runner` | No runner is configured; there's nothing to launch a probe with. Not an error, and not a guess. |
| 🟡 `not_run` | A runner IS configured, but the throwaway sandbox never got to running the probe — an image pull failure, or a confinement class this host can't enforce. Distinct from `blocked`: `blocked` means the probe DID run and observed a real network fact; `not_run` means nothing was learned either way. Setup's gate treats it the same as `no_runner` (unlocks Next with a neutral note, never a click-past). |
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

```sh
git pull && make compose-build          # rebuild wardynd at the new revision
docker compose -f deploy/compose/docker-compose.yaml up -d wardynd
```

Take the Postgres dump above **before** the restart; that dump is the only
rollback you have. Agent images are built separately — `make agent-images`
rebuilds them.

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

**The second, and why `--reuse-values` is not the fix for the first:
`--reuse-values` replaces the new chart's `values.yaml` with the previous
release's, so every value block the new version ADDED is absent from the map the
templates read.** That is fatal for any chart that dereferences a new block
without a default — the render dies on a nil map, not on a refusal. This chart is
written not to: each `0.6`-only block (the UI-sandbox gateway, the
readiness-probe path) is read through a `default dict` and its `values.yaml` leaf
default, so a `0.5` values map renders as though you had accepted the new
defaults, with and without `k8s.enabled`. It renders — but only ever with those
defaults: the knobs `0.6` added never appear in the map, so what a fresh install
would have made you choose is chosen for you, silently. Use
`--reset-then-reuse-values` instead (Helm ≥ 3.14: starts from the NEW chart's
defaults and layers the previous release's overrides on top), or better, pass `-f
your-values.yaml` as above.

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

**The console does not warn about this**, and in 0.7 it does not refuse it
either: a rename with grants attached is accepted like any other edit. Treat the
rename field as an operator action with a runbook, not a label edit.

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
claim whose `wardyn.drive` or `wardyn.home` names a different pair, or a share
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

**`ReadWriteOnce` binds a claim to one node.** A managed drive is provisioned
RWO, so a person's second concurrent run schedules onto the node their first run
landed on — or stays Pending and fails at the dispatch wait timeout with the
message below. The pod's failure reads `0/N nodes are available: pod has unbound
immediate PersistentVolumeClaims` or a volume-node-affinity conflict, and wardynd
surfaces the scheduler's own
`PodScheduled` message in the run's failure hint rather than a bare timeout. The
same message is what a claim that never bound at all produces — a storage class
with no provisioner, or no default class on the cluster for a drive that names
none. If members routinely run several sandboxes at once, provision the drive's
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
FIRST: on a `hash`/`sub` drive the name keys on the first claim, and the API's
`home_subject` says which claim it used (the console does not yet show it). The
claim carries `wardyn.managed`, `wardyn.drive` (the drive's
row **id**, not its name, so the claims a rename orphans stay findable with the
`get pvc -l wardyn.drive=<drive-id>` above) and `wardyn.home` labels — the same
pair the Docker driver stamps on a managed volume — and, deliberately, **no
`wardyn.run-id`**, so the per-run teardown sweep (a `DeleteCollection` selecting
on exactly that label) cannot reach it. That pair is also what the driver checks
before it mounts anything: see the two refusals above.

**Ownership, and where fsGroup stops working.** A pod with a drive carries
`fsGroup: 1000` (a GROUP id — it happens to equal the uid every agent image runs as, but this field can never make a volume user-owned) with
`fsGroupChangePolicy: OnRootMismatch` — `Always` would recursively chown a large
drive on every single run. The kubelet applies fsGroup for CSI drivers that
declare `ReadWriteOnceWithFSType` volume ownership, i.e. block storage: the
managed case is correct by construction. It does **not** apply to an NFS-type
volume. A static share is owned by whatever its export says, so map it there — a
Wardyn-dedicated export with `all_squash,anonuid=1000,anongid=1000`, or per-user
`0700` subdirectories — and expect a read-only mount where an existing corporate
home is owned by a different uid.

**gVisor wants `directfs` off for a drive, and the annotation is a request.**
The Wall (CC2) and Vault (CC3) tiers run the agent pod under a RuntimeClass; when
its handler is `runsc`, gVisor's `directfs` has the gofer donate a file
descriptor per mount point to the sandbox, which then operates on the file
directly. That is right for a block PVC and wrong for a network-backed export —
a `k8s_pvc_static` share over NFS/SMB. gVisor takes the override **per mount**,
from a pod annotation keyed by the volume's own name, and wardynd stamps it on
every drive pod whose resolved handler is `runsc`:

```yaml
dev.gvisor.spec.mount.drive.directfs: "off"
```

containerd only forwards it when the node's runsc runtime section allows the
prefix, so on a cluster whose `/etc/containerd/config.toml` does not carry

```toml
pod_annotations = ["dev.gvisor.*"]
```

the annotation is inert and the node-level setting is the one that applies:
`--directfs=false` in the runsc shim's own config (`/etc/containerd/runsc.toml`,
or the `runtimeArgs` a node image bakes in). Either is fine; the annotation is
per-pod and the flag is per-node. See
[gVisor's containerd configuration guide](https://gvisor.dev/docs/user_guide/containerd/configuration/).

The companion caveat is CACHING, and it cuts the other way. runsc serves bind
mounts `shared` by default (`--file-access-mounts=shared`), revalidating against
the host because it cannot assume exclusive access. An operator who has set
`--file-access-mounts=exclusive` for throughput must **not** do so on nodes that
run drive pods over a share other writers touch: exclusive mode caches
aggressively, and a file another writer changes is not seen. A managed
(`k8s_pvc`) drive is exclusive to its pod by construction and is unaffected. See
[gVisor's filesystem guide](https://gvisor.dev/docs/user_guide/filesystem/).

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
