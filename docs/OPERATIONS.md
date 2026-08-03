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
and `requirements` writes that widen what a run may do), `PUT /site-config`
and its connectivity probes (`POST /site-config/test-proxy`, `POST
/site-config/test-redirect` — each launches a sandbox and makes a real
outbound request on the operator's behalf), secret write/delete (`PUT`/`DELETE /secrets/{name}` — the name-only list stays
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

## Corporate network: upstream proxy and egress redirects

One more piece of operator-wide config lives in Postgres alongside everything
in **State stores** above: `SiteConfig` (`GET`/`PUT /api/v1/site-config`,
`wardyn site-config get|apply`) — the corporate upstream proxy and the list of
outbound redirects every run's egress inherits. Unconfigured is a valid,
common state: a host with direct internet access needs none of this. Because
it lives in Postgres, `make reset-all` takes it with the volume; `wardyn
site-config get > corp-baseline.json` before a reset and `wardyn site-config
apply corp-baseline.json` after is the round-trip — the document carries
secret **names**, never values, so it's safe to keep beside the repo.

### Upstream proxy: plain URL vs. secret

- `upstream_proxy_url` — a plain URL (`http://proxy.corp.internal:8080`),
  stored and read back in the clear. A proxy address is topology, not a
  credential; forcing it through the write-only secret store meant a
  mistyped URL could never be read back to debug.
- `upstream_proxy_secret_ref` — the *name* of a secret holding the proxy URL,
  for a proxy that needs an embedded credential
  (`http://user:pass@proxy.corp.internal:8080`).

**An `upstream_proxy_url` carrying a `user:pass@` is rejected server-side,
always** (`validateSiteConfig`, `internal/api/site_config.go` — `PUT
/site-config` 400s). This is a guarantee, not a UI courtesy: even a client
that skips its own check cannot persist a credential in the clear this way.
Put a credentialed proxy URL in a secret instead:

```sh
wardyn secret set upstream-proxy-url          # paste the full, credentialed URL
# then reference it by name in the applied site config:
#   "upstream_proxy_secret_ref": "upstream-proxy-url"
```

If both fields are set, `upstream_proxy_url` wins — harmless mid-migration
from one to the other, but don't rely on it; clear whichever you're not using.

### Egress redirects: two tiers

`egress_redirects` is a list of `{from, to, token_secret_ref, ecosystem}`
entries. Each substitutes a public/upstream URL or host for a
corporate-internal one in every run's egress, with an optional token injected
proxy-side as a Bearer credential for `to`'s host (the sandbox never holds
it). This replaced the old `artifact_overrides` map (one entry per package
ecosystem) because a corporate estate redirects container registries and
internal appliances too, not only package managers — the shape generalized
from "one entry per ecosystem" to "a list of From → To pairs over any URL,
host, or IP".

What you get depends on whether `ecosystem` is set:

| `ecosystem` | Egress substituted | Token injected | Per-tool config file |
|---|---|---|---|
| `npm` \| `pip` \| `cargo` \| `maven` \| `go` \| `nuget` | yes | yes | yes |
| empty (**network only**) | yes | yes | no |

An ecosystem row gets the per-tool config file `EmitArtifactConfig`
(`internal/workspacescan/gen.go`) writes at workspace-import time — `.npmrc`,
`.config/pip/pip.conf`, `.cargo/config.toml`, `.m2/settings.xml`,
`GOPROXY`/`GOSUMDB`, or `.nuget/NuGet/NuGet.Config` — on top of the egress
substitution and token injection every redirect gets.

A network-only row (empty `ecosystem`: a container registry, an internal
appliance, a bare host or IP) gets the network half only: its host is
substituted into the run's egress allowlist and its token is injected
proxy-side, exactly like an ecosystem row, but **no config file is written**
— there is no `.npmrc` equivalent for an arbitrary host. That is a real cost,
not a technicality: the workspace still needs to be told to pull from the
mirror itself (`docker login` against the internal registry, an appliance
client's own config), or a run reaches an allowed, credentialed host that
nothing in the sandbox actually asks for. The UI labels these rows `network
only` so the gap stays visible instead of reading like a redirect that does
everything the row above it does.

### Upgrading from `artifact_overrides`

A site-config document saved before this shipped used
`artifact_overrides: {"npm": {"base_url": "..."}, ...}`. Migration `0030`
rewrites the one stored row automatically on the first boot after upgrade —
nothing to do for what's already in Postgres.

`PUT /site-config` (and so `wardyn site-config apply`) still accepts a legacy
`artifact_overrides` body **for one release**, folding it into
`egress_redirects` before validating. This isn't generosity: `apply` replaces
the *whole* document, so an operator re-applying a file they saved before
this release — without the fold — would silently wipe the proxy and every
redirect rather than just fail to update them. A body that sets both fields
is rejected (400) rather than guessed at. `wardyn site-config get` after
upgrading no longer returns `artifact_overrides` at all — re-save the file at
that point.

### Testing it: two probes, not a courtesy button

Wardyn otherwise has no test-connection buttons anywhere: it cannot dial a
stored credential, so a green tick would mean "we wrote it down" while
reading as "we checked" — a false reassurance nobody wants at 3am. `POST
/api/v1/site-config/test-proxy` and `POST /api/v1/site-config/test-redirect`
are the deliberate exception, on the same footing as the GitHub ref-ruleset
check ([TRY-IT.md](TRY-IT.md)): the check is real. Each launches a throwaway,
one-shot confined sandbox, makes an actual outbound request through it — the
same path a real run's egress takes — and tears the sandbox down. Both are
**operator-only** (a viewer 403s, like `PUT /site-config` itself) and
**audited** (`site_config.test_proxy` / `site_config.test_redirect`); the
audit row carries the host(s) probed and the outcome, never the proxy URL,
which may legitimately carry a credential.

```sh
curl -s -X POST http://localhost:8080/api/v1/site-config/test-proxy \
  -H "Authorization: Bearer $WARDYN_ADMIN_TOKEN"

curl -s -X POST http://localhost:8080/api/v1/site-config/test-redirect \
  -H "Authorization: Bearer $WARDYN_ADMIN_TOKEN" \
  -H 'Content-Type: application/json' \
  -d '{"from":"https://registry.npmjs.org/"}'
```

`test-proxy` needs no body, and runs whether or not an upstream proxy is
configured — with one it proves the chain works, without one it proves direct
egress works, and it says which path it took. The question it answers ("can a
sandbox on this host reach the internet?") matters most where nothing is
configured yet. It goes out through the sandbox's normal egress, which
dispatch already chains to the configured upstream, so it proves the path a
real run takes rather than a reconstruction of it.

It also accepts an optional `{"url": "https://…"}`:

```sh
curl -s -X POST http://localhost:8080/api/v1/site-config/test-proxy \
  -H "Authorization: Bearer $WARDYN_ADMIN_TOKEN" \
  -H 'Content-Type: application/json' \
  -d '{"url":"https://intranet.corp.internal/health"}'
```

That exists for hosts with no public internet — internal-only or air-gapped
deployments, where none of the default targets will ever answer. Point it at
something your network *can* reach and the check goes back to proving what it
is for: that egress works, not that the public internet does.

A custom target proves strictly less, and the response says so. Wardyn has no
idea what your endpoint is supposed to return, so it cannot match a known
payload — it only reports that the request completed. The URL is validated the
same way any stored site-config URL is (http(s) only, a real host, no shell
metacharacters) and rejected before a sandbox launches. It is not stored and
changes nothing about later runs; it is a recovery affordance for a failed
check, not configuration.

Two details of *how* it decides, both of which change the answer on a
corporate network:

- **It does not use github.com.** Plenty of organisations block GitHub
  outright, and a false "no internet" from a working network is worse than no
  check at all. The targets are the endpoints Windows (NCSI) and Firefox use
  for their own connectivity detection — blocking those breaks the operating
  system's network indicator, so they are about as close to unblockable as the
  public internet offers. Several are tried; one blocked endpoint does not
  fail the probe.
- **It checks the response body, not just the exit code.** A corporate block
  page or captive portal is a perfectly well-formed HTTP 200, so an
  exit-code-only probe reports success while egress is firmly shut. Each
  target publishes a fixed payload; the probe matches it. A reply that arrives
  but does not match is reported as blocked, naming interception as the cause
  — that state is the whole reason the check exists.

`test-redirect` takes `{"from": "..."}` naming an entry already in the stored
`egress_redirects` (404 if it names none — the request's `from` only ever
*selects* a stored row). **It never dials a caller-supplied target**: the
probe target always comes from the stored row, never the request body, or
the endpoint would be an SSRF gadget with a friendly label. It runs two
fetches in one sandbox — the mirror (`to`) through the normal path, then the
public endpoint (`from`) again with the proxy deliberately bypassed — to
catch a redirect that's configured but not enforced.

Both return `200` with `{"state", "detail", "elapsed_ms"}`:

| `state` | Means |
|---|---|
| `reached` | The path works — proxy or mirror reachable, and for a redirect, the public host is correctly *blocked* when dialed directly. |
| `blocked` | Could not reach the proxy or the mirror. `detail` names the real cause — DNS failure, connection refused, TLS failure, timeout, or curl's own exit code — never a generic "failed". |
| `bypass` | **Read this one carefully — it's the one that looks fine but isn't.** The mirror answers, but the public host it's supposed to replace is *also* still reachable, directly, from a sandbox. The redirect is configured but not enforced: a run can silently pull from the internet instead of the mirror, and every other signal — the row is filled in, the mirror answers — looks exactly like a working redirect. `test-redirect` only. |
| `no_runner` | No runner is configured; there's nothing to launch a probe with. Not an error, and not a guess. |

A probe is bounded well under a minute and reclaims (kills) its sandbox if the
run doesn't finish in time, so a wedged probe can never hold one open.

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

`replicas` is not a scaling knob, and it is not modesty either — **the pin is a
safety control.** No shipped topology runs more than one: compose pins
`container_name` (`--scale wardynd=N` is rejected outright) and the Helm chart
both defaults `replicas: 1` **and refuses to render above it**
(`deploy/helm/wardyn/templates/deployment.yaml`; `allowMultiReplica=true` is the
documented override, and it is an acceptance of everything below, not a fix).

wardynd keeps this state per-process. The first entry is why the pin is a control
rather than a preference:

- **the secret-masking registry** (`internal/secretmask`) — an in-memory
  `map[runID][][]byte`, never persisted, and it **fails open**. Secrets are
  registered by the request that mints or injects them (`Broker.mint` on the
  mint route, `handleInternalInjection` on the proxy's injection call; the
  captured AWS SSO token registers process-*globally* via `AddGlobal`), so they
  land on whichever replica the run's proxy happened to dial. The session-recording upload
  (`POST /runs/{id}/recording`) and the live-attach relay are DIFFERENT requests
  that may land anywhere, and both pass the stream through unmasked when the
  run's snapshot is empty (`buildMaskingBody`, `liveMaskWriter`). Two replicas is
  therefore enough to persist an asciicast containing live credentials in
  cleartext — with a `success` audit event, because nothing in the path can tell
  "no secrets for this run" from "not my run". There is no cross-replica fix
  short of moving the registry into shared storage, which has not been built.
- **the audit spool** — a local append-only file per pod
  (`internal/api/auditspool.go`). Per-process *by design*: it is the fallback
  for a failed Postgres write, and each pod drains its own back into the
  database once it recovers.
- **the age identity, when `WARDYN_AGE_KEY` is unset** — each process then mints
  its own ephemeral one at boot (`buildSecretStore`, `cmd/wardynd`), so a secret
  written by one pod cannot be decrypted by any other. Fails closed (a decrypt
  error, never a wrong plaintext) and surfaces on `Get`, not at boot, so the pod
  starts healthy and the failure appears at first use. Setting the key removes
  this one entirely — which you should be doing anyway for restart durability.
- **the docker driver's sandbox tracking maps** (`agentExecs`, `pending`,
  `mainProc`, `creating` in `internal/runner/docker/driver.go`) — the process
  that created a sandbox is the only one that can observe its agent exec
  (`Wait`), and `creating` is the in-memory tombstone that makes the exec-less
  (krun) create/teardown handshake atomic. A teardown handled by a pod that did
  not create the sandbox has neither, so on that path a container can survive the
  kill it was supposed to die from.
- **the `/metrics` counters** (`internal/api/metrics.go`) — per-process, so a
  scrape reports one pod's slice of the fleet, not the fleet.
- **the decision-ingest `lastTouch` debounce** (`shouldTouch`,
  `internal/api/internal.go`) — per-process, so N pods can do up to N× the
  `TouchRun` writes the 30s debounce was sized for. Load, not correctness.

Six OTHER pieces that used to be on this list — the actual reason a second replica
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

None of that makes `replicas > 1` a supported configuration. It closed the six
reasons a second replica used to drop *requests*; it did not touch the list
above, and the masking registry is a worse failure than any of the six was —
those lost work, this one persists secrets. Keep `replicas: 1`. Going beyond it
has not been built, tested, or released, and the chart will not render it without
`allowMultiReplica=true`.
