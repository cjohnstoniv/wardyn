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
| Postgres | volume `postgres_data` | runs, approvals, workspaces, policies, encrypted secrets, the append-only audit log | everything but the recordings |
| Recordings | volume `${WARDYN_NS:-wardyn}-recordings` (`WARDYN_RECORDING_DIR=/data/recordings`) | PTY asciicasts for Replay | every session replay; nothing reconstructs them |
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

# 2. Recordings (the volume is named, not project-prefixed — see docker-compose.yaml)
docker run --rm -v wardyn-recordings:/from -v "$PWD":/to alpine \
  tar czf /to/recordings-$(date +%F).tar.gz -C /from .

# 3. The age key — copy WARDYN_AGE_KEY out of deploy/compose/.env into your
#    secret manager. A Postgres dump without it is unreadable ciphertext.
```

Restore is the same three in reverse: `psql -U wardyn wardyn < dump.sql` into a
fresh volume, untar into the recordings volume, put `WARDYN_AGE_KEY` back in
`.env`, then `make setup`.

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

`replicas` is not a scaling knob. wardynd holds per-process state that a second
replica does not see, so a request landing on the wrong pod fails:

- **attach tickets** — single-use WebSocket tickets live in `Server.attachTix`
  (`internal/api/server.go`), so a ticket minted on pod A is unknown to pod B.
- **compose results** — the in-sandbox proposal upload is parked in a `sync.Map`
  and taken delete-on-read by the waiting launcher (`internal/api/composeresult.go`).
  Uploaded to A, awaited on B, it is never found.
- **the ground-truth token rotator** — every wardynd runs one
  (`cmd/wardynd/gt_rotator.go`) and they all write the same shared token file.

Keep `replicas: 1`. Making those three shared is the prerequisite for anything
else, and it is not on the [roadmap](../ROADMAP.md).
