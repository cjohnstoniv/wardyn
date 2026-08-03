# Operating Wardyn

Backup, upgrade, rotation, monitoring, and the scaling constraint — the questions
that arrive after the stack is up.

**Scope: the Compose stack.** Everything here is written against
[`deploy/compose/`](../deploy/compose/). The Helm chart deploys the control plane
but [cannot create sandboxes yet](../deploy/helm/wardyn/README.md) (v0.5+), so it
has no run/recording state to operate.

## State stores

Three stores hold data that exists nowhere else. Lose any of them and the loss is
permanent.

| Store | Where | Holds | If you lose it |
|---|---|---|---|
| Postgres | volume `postgres_data` | runs, approvals, workspaces, policies, encrypted secrets, the append-only audit log — and, under the default `pg` recording store, the PTY asciicasts too | everything |
| Recordings | volume `${WARDYN_NS:-wardyn}-recordings` (`WARDYN_RECORDING_DIR=/data/recordings`) | PTY asciicasts for Replay — **only with `WARDYN_RECORDING_STORE=fs`**; the shipped default (`pg`) keeps them in Postgres and leaves this volume empty | every session replay it holds; nothing reconstructs them |
| Age key | `WARDYN_AGE_KEY` in `deploy/compose/.env` | the X25519 identity every stored secret is encrypted to | every secret in Postgres becomes undecryptable ciphertext |

The `audit` volume is **derived**, not primary: it is the optional file sink
(`WARDYN_AUDIT_SINKS`, see [ENV.md](ENV.md)). Postgres is the source of truth for
the audit log — `deploy/helm/wardyn/values.yaml` says the same thing about
`persistence`. Restoring Postgres restores the audit trail; the file sink is a
forwarding copy for a SIEM.

Ground truth (`tetragon_export`) and the rotator's `groundtruth_token` are
transient by design — both are regenerated on start.

### Back them up

```sh
# 1. Postgres (the container name is ${WARDYN_NS:-wardyn}-postgres)
docker exec wardyn-postgres pg_dump -U wardyn wardyn > wardyn-$(date +%F).sql

# 2. Recordings — ONLY under WARDYN_RECORDING_STORE=fs. With the default `pg`
#    store step 1 already captured them; this volume is empty.
#    (the volume is named, not project-prefixed — see docker-compose.yaml)
docker run --rm -v wardyn-recordings:/from -v "$PWD":/to alpine \
  tar czf /to/recordings-$(date +%F).tar.gz -C /from .

# 3. The age key — copy WARDYN_AGE_KEY out of deploy/compose/.env into your
#    secret manager. A Postgres dump without it is unreadable ciphertext.
```

Restore is the same three in reverse: `psql -U wardyn wardyn < dump.sql` into a
fresh volume, untar into the recordings volume (`fs` store only), put
`WARDYN_AGE_KEY` back in `.env`, then `make setup`.

> `make reset` runs `compose down -v` after a confirmation prompt
> (`scripts/up.sh` `cmd_reset`): Postgres, recordings and the audit sink all go,
> with no backup counterpart. It leaves `.env` — and so the age key — alone.
> `make compose-down` stops the stack and keeps the volumes.

## Monitoring

`GET /metrics` (admin bearer required, next to the unauthenticated `/healthz`)
serves Prometheus text exposition — stdlib-only, no client library. Counters:
runs by terminal state, approval decisions by outcome, egress denies, credential
mints; plus sandbox launch-latency sum/count. Scrape it with any Prometheus
`authorization` config carrying the admin token. `/healthz` stays the
liveness/component surface (identity, runner classes, eBPF ground-truth state);
`/metrics` is the trend surface. Audit sinks (`WARDYN_AUDIT_SINKS`,
[ENV.md](ENV.md)) are the event stream for SIEMs — metrics deliberately carry no
per-run detail.

## Who can change what

The API authenticates with **either** an OIDC session (human SSO) **or** the
admin bearer token; local mode skips both on a loopback-only bind. That is
authentication. Authorization is one optional list:

| `WARDYN_OIDC_OPERATOR_EMAILS` | Signed-in humans | Admin token / local mode |
|---|---|---|
| unset, no OIDC | all admin-equivalent | admin-equivalent |
| unset, OIDC configured | **refuses to boot** (override: `WARDYN_ALLOW_OIDC_NO_OPERATOR_LIST=true` ⇒ all admin-equivalent) | admin-equivalent |
| set | listed = **operator**; everyone else = **viewer** | always operator |

A viewer reads everything and is refused (403) on the writes with the widest
blast radius: the managed harness credential (`POST /setup/harness-login`,
`PUT`/`DELETE /setup/harness-credential/{provider}`), policy create/update/delete,
every mutating `/workspaces` route (including the `approved-egress`, `llm-cred`
and `setup-commands` writes that widen what a run may do), `PUT /site-config`,
secret write/delete (`PUT`/`DELETE /secrets/{name}` — the name-only list stays
readable), deciding an approval (`POST /approvals/{id}/approve|deny` — the
queue stays readable), and attaching to a running sandbox — BOTH lanes to a
live PTY, the ticket mint (`POST /runs/{id}/attach-ticket`) and the WebSocket
itself (`GET /runs/{id}/attach`). Gating only the mint would buy nothing: the
socket falls back to session-cookie auth when no ticket is presented, and a
browser sends that cookie on a same-origin handshake automatically. Worth stating
plainly: a viewer's own run that trips an approval blocks until an operator
decides it — that is the tier working as intended, not a bug.

Match is on the full address, case-insensitive (ASCII only — a session email
containing any non-ASCII rune never matches, fail closed); entries are
comma-separated. The address comes from the IdP's `email` claim, which is only
forced to be *verified* when `WARDYN_OIDC_EMAIL_DOMAINS` is also set — set both
(wardynd warns at boot if you don't). A signed-in human whose session carries no
email is a viewer. The claim is captured at sign-in and rides the session cookie,
so an IdP-side address change is stale until the session expires (~1h); removing
an address from the list itself takes effect on restart regardless — the
direction that matters.

**This is not RBAC.** Launching and killing a run (`POST /runs`, `POST
/runs/{id}/kill`) stay open to any signed-in human — using the product is a
viewer act by design — and the admin token cannot be demoted, because it is
one shared credential with no human behind it. Real roles and owner scoping
are v0.5+ ([ROADMAP.md](../ROADMAP.md); `threatmodel/THREAT-MODEL.md`
residual #14). Wardyn's operator boundary is still "everyone with a login is
trusted staff".

## The age key has no rotation path

The secret store binds **one** age identity for both encryption and decryption
(`internal/secretstore/pg`), and nothing re-encrypts stored secrets under a new
key — there is no `wardyn secret rotate`. Changing `WARDYN_AGE_KEY` does not
migrate anything; it strands every existing ciphertext. To move keys today you
must re-enter every secret (`wardyn secret set …`, `wardyn subscription connect`)
against the new identity.

The "set a persistent key or lose your secrets on restart" warning is already in
[`deploy/compose/README.md`](../deploy/compose/README.md),
[TRY-IT.md](TRY-IT.md), and both installer scripts. What those do not say, and
this does: **back the key up off-host, because you cannot rotate out of a
compromise without re-entering every secret.**

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

## One replica, by construction

`replicas` is not a scaling knob, and no shipped topology runs more than one
today: compose pins `container_name` (`--scale wardynd=N` is rejected outright)
and the Helm chart pins `replicas: 1`. wardynd still keeps one piece of state
per-process:

- **the audit spool** — a local append-only file per pod
  (`internal/api/auditspool.go`). Per-process *by design*: it is the fallback
  for a failed Postgres write, and each pod drains its own back into the
  database once it recovers.

Six pieces that used to be on this list — the actual reason a second replica
used to silently drop requests — are now Postgres-backed, so they survive a
crash and no longer break under a second replica: single-use **attach
tickets**, delete-on-read **compose results**, and the **lifecycle reaper**
(migration 0026 + a `pg_try_advisory_lock` around the reap tick, which now
skips a tick it does not win instead of racing another pod to stop the same
runs); **run watchers** (migration 0027 — each run's completion watcher is
still an in-process goroutine blocked on `Runner.Wait`
(`internal/api/runs_dispatch.go`), but it now refreshes a Postgres lease every
30s, and every replica sweeps for stale leases every 60s, so a run orphaned by
a pod that never comes back is adopted by any live replica within roughly
65-150s instead of stranding forever — real latency, not instant, and slower
than the same-pod restart case `ReconcileOnBoot` handled alone); **session
recordings** (migration 0028 — the process default is now the Postgres-backed
`pg` store, readable from any replica; `WARDYN_RECORDING_STORE=fs` still
selects the old per-pod directory); and the **ground-truth token rotator**
(`cmd/wardynd/gt_rotator.go`, leader-elected via a Postgres advisory lock — a
standby takes over within one ~30s backoff of the leader's session ending).

None of that makes `replicas > 1` a supported configuration — it removes the
reasons a second replica used to be actively unsafe, not the requirement to
stay at one. Keep `replicas: 1`; going beyond it has not been tested or
released.
