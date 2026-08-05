# Operating Wardyn

Backup, upgrade, rotation, monitoring, and the scaling constraint — the questions
that arrive after the stack is up.

**Scope: mostly the Compose stack.** The backup/state-store, monitoring, and
corporate-network sections below are written against
[`deploy/compose/`](../deploy/compose/). The Helm chart (`deploy/helm/wardyn`,
[`k8s.enabled`](../deploy/helm/wardyn/README.md)) now runs its own
[Kubernetes runner substrate](#kubernetes-known-gaps-v05) with its own
run/recording state to operate — see that section for what's different there.
"[Multi-user: who can change what](#multi-user-who-can-change-what)" and
"[One replica, by construction](#one-replica-by-construction)" apply to both
substrates identically: authorization and the per-process constraints live in
`internal/api`, above the runner seam.

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

## Multi-user: who can change what

The API authenticates with **either** an OIDC session (human SSO) **or** the
admin bearer token; local mode skips both on a loopback-only bind. That is
authentication. Authorization is a real two-role model: every OIDC session
carries an **admin** or **member** role, derived once at login
(`internal/auth/oidc`'s `deriveRole`) and stamped into the signed session
cookie — a cookie signed before this existed (pre-0.5) decodes as no session,
forcing a re-login that derives one fresh.

| `WARDYN_OIDC_ROLE_MAP` | Signed-in humans | Admin token / local mode |
|---|---|---|
| unset | all **admin** (today's pre-0.5 behavior — opt-in, upgrade-safe) | always **admin** |
| set | mapped by `roles`/`groups`/email claim to **admin** or **member**; no match falls through to `WARDYN_OIDC_DEFAULT_ROLE`, or denies the login when that is also unset | always **admin** |

The admin token and local mode are **always admin** — both are a single
shared credential with no per-human identity to key a role off (the token
*is* the admin), which is the documented ceiling of the whole gate
(`requireOperator`/`isOperator`, `internal/api/http.go`), not an oversight.

**Deriving the role** (`WARDYN_OIDC_ROLE_MAP`, a CSV of `value=role` pairs,
e.g. `Wardyn.Admin=admin,eng-team=member,alice@corp.com=admin`): each entry's
`value` is matched case-insensitively against the ID token's `roles` claim
(an Entra App Role — the priority path, see `.claude/skills/wardyn-k8s-setup`
for the app-registration walkthrough), its `groups` claim, or the signed-in
email. Any match resolving to `admin` wins over one resolving to `member`,
regardless of which claim produced it. `WARDYN_OIDC_OPERATOR_EMAILS` — the
pre-0.5 operator allowlist — is **not replaced**: an email on it is still an
*additional* `admin` match (`LegacyAdminEmails`), so an existing deployment
adopting the role map keeps its current operators as admins with zero
re-configuration. `WARDYN_OIDC_DEFAULT_ROLE` (`admin`/`member`, unset = deny)
covers everyone the map doesn't name.

Both are validated at **boot**, not at first use: a malformed
`WARDYN_OIDC_ROLE_MAP` entry (an invalid role value, a non-ASCII key —
matching is ASCII-only, so it could never match — a duplicate key, or
non-blank input with no valid entry at all) or an invalid
`WARDYN_OIDC_DEFAULT_ROLE` value fails wardynd's boot outright, naming the
var in the error (`buildOptionalFeatures`, `cmd/wardynd/boot_deps.go`) —
never a silent fallback that lets a typo reach a session cookie later. A
signed-in human who matches nothing in a validly-configured map, with no
default role set, is denied at login instead ("no Wardyn role assigned").

**What admin-only still means** — the writes with the widest blast radius stay
gated on the role being exactly `admin` (`requireOperator`): the managed
harness credential, policy create/update/delete, every mutating `/workspaces`
route (including `approved-egress`/`llm-cred`/`requirements`), `PUT
/site-config` and its connectivity probes, secret write/delete, `GET
/metrics`, and bringing a custom sandbox image or devcontainer repo to a run
(`image`/`devcontainer_repo` — `denyMemberCustomImage`, `internal/api/runs_create.go`:
a member's own onboarded-workspace base image is unaffected, since that path
is operator-authored at onboarding time, never the member's own free-text
choice). Launching and killing a run (`POST /runs`, `POST /runs/{id}/kill`)
stay open to any signed-in human — using the product is a member act by
design.

**Ownership scoping — real, not just admin-vs-everyone.** A member reaches
their OWN resources the same way an admin reaches any of them
(`ownsRunOrAdmin`/`getRunAuthorized`, `internal/api/helpers.go`): `GET`/kill/
profile/grants on a run, minting its attach ticket, its recording replay, and
`GET /runs`/`GET /approvals`/`GET /audit` (each scoped to the caller's own
`created_by` rows) all answer a foreign resource with the **byte-identical
404** a truly-missing one gets — never a 403 — so probing another user's run
id learns nothing (no existence oracle). `GET /setup/status` redacts
operator-diagnostic detail (checks, secret names, runner detail) for a member.

**Deciding an approval is kind-restricted, not just owner-restricted**
(`decide()`, `internal/api/approvals.go`): a member may approve or deny an
`egress_domain` approval on a run they own. `credential` and `tool_call`
approvals stay **admin-only regardless of ownership** — the shipped default
policy requires approval on `github_token`, so letting a member self-approve
their own run's credential request would self-mint a real token, and
self-approving a `tool_call` re-opens exactly what the clamp (below) exists to
bound, both under the same authority the operator ceiling is meant to
constrain.

**A member's own policy is clamped, not trusted.** `POST /runs`'
`inline_policy` (and its preflight dry-run) is clamped to the operator's
`DefaultPolicy` ceiling before resolution (`composer.Clamp`,
`internal/composer/clamp.go` — the same clamp bounds an AI-composed policy and
a Record Mode-synthesized one against the same ceiling): confinement raised to
the floor, egress intersected down to the allowlist, `first_use_approval`
raised to the stricter of the two, `llm_inspection` inherited when the ceiling
sets one, resources and `auto_stop_after_sec` capped, grants narrowed to what
the ceiling allows, and `workspace_mounts` dropped entirely — a member's
policy can only ever get MORE restrictive than the operator's default, never
less. An admin's own `inline_policy` is not clamped.

Every member denial above that isn't a plain foreign-resource 404 is audited
under `authz.denied` (reasons include `admin_surface`, `byoi_member`,
`not_owner`) — a 404 on a resource that genuinely doesn't exist stays silent
by design, matching the no-existence-oracle rule.

**What's still not built.** No custom roles beyond admin/member, no
per-resource fine-grained permission model (owner-or-admin only — no
"read-only share" or "co-owner" concept), no tenant/org columns, no
separation of duty among admins — every admin (and the admin token, always)
can rewrite the policy that bounds them (`threatmodel/THREAT-MODEL.md`
residual #14, still open). The SSH gateway has **no admin override** at all
today — SSH authorization is a single `run.created_by == the key's principal`
check, deliberately narrower than the web terminal's `requireOperator` gate;
an admin who needs another human's run uses the web terminal, same as a
member would (`docs/SSH.md`'s Bounds section; `threatmodel/THREAT-MODEL.md`
residual #15). See [ROADMAP.md](../ROADMAP.md) for what's queued.

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

## Kubernetes: known gaps (v0.5)

The `k8s` runner substrate (`deploy/helm/wardyn`, `k8s.enabled=true`,
`internal/runner/k8s`) is a **separate, independent confinement substrate**
from the Docker Compose path (L1/NetworkPolicy-backed vs. Compose's L0
structural one) — most of this document applies to both, but the list below
is what the k8s substrate does NOT do yet, honestly, as of v0.5. Each item is
a real limitation checked against the driver, not a guess:

- **No BYOI or devcontainer builds.** A `wardyn-byoi/`-prefixed image ref is
  refused before any pod is created — ephemeral containers cannot honor the
  selftest-then-task double-exec BYOI needs
  (`internal/runner/k8s/errors.go`'s `errBYOIUnsupported`,
  `internal/runner/k8s/exec.go`). `WARDYN_ENVBUILD` devcontainer builds are
  Docker-only for the same reason and are unaffected by `k8s.enabled` — they
  simply have no k8s equivalent.
- **No `local_dir` / host-path workspace mounts.** A policy with any
  `WorkspaceMounts` entry fails the run closed with a clear error
  (`internal/runner/k8s/sandbox.go`'s `errMountsUnsupported`) — a k8s pod has
  no path back to an arbitrary directory on wardynd's own host the way a
  Docker bind mount does. Git-clone workspaces (`WorkspaceRepos`) are
  unaffected; only a *local directory* source is refused.
- **No `~/.aws` / `~/.claude` host staging.** The same `errMountsUnsupported`
  refusal covers the RESIDENT-COPY credential path (mounting staged
  `~/.claude` or a captured `~/.aws` into the sandbox) — there is no host
  filesystem to stage from in the first place. Use proxy-side injection
  instead: managed-subscription OAuth injection and the Bedrock AWS SSO
  exchange are both substrate-agnostic (they happen at `wardyn-proxy`, never
  by mounting a credential directory into the pod), so they work unchanged
  on k8s.
- **No in-sandbox DNS.** Every sandbox pod is `DNSPolicy: DNSNone` with a
  single nameserver, `127.0.0.1` — nothing listens there, so a DNS query
  fails FAST (connection refused) rather than hanging out a real timeout
  against a resolver a NetworkPolicy would deny anyway
  (`internal/runner/k8s/sandbox.go`). Only the pinned `wardyn-proxy` sidecar
  resolves hostnames, exactly like the Compose substrate's proxy-only egress
  posture — this is parity, not a new gap, but the *mechanism* (a present-but-
  unreachable loopback resolver vs. Compose's no-resolver-configured-at-all)
  is k8s-specific enough to name here.
- **No per-pod PIDs limit.** Kubernetes has no per-container "pids" resource
  the way Docker's `--pids-limit` does — a run's `ResourceLimits.PidsLimit`
  is accepted but not enforced, and wardynd logs a warning naming the run id
  every time it's requested and skipped (`internal/runner/k8s/sandbox.go`).
  **Recommendation**: set the node-level kubelet `podPidsLimit` (or the
  equivalent `SystemReserved`/`KubeReserved` PID accounting for your
  distribution) as a cluster-wide fork-bomb backstop — it is coarser
  (per-node, not per-run) but real, and it is the only lever this substrate
  has today.
- **`DiskMiB` is ignored, with a warning.** Same shape as the PIDs gap: no
  per-container writable-storage quota is wired up on this substrate yet, so
  a requested disk cap is accepted, not enforced, and logged
  (`internal/runner/k8s/sandbox.go`). An `ephemeral-storage` resource request/
  limit at the cluster level is the closest present mitigation.
- **No k8s ground-truth correlator.** The Tetragon host-sensor → ground-truth
  pipeline (`cmd/wardynd/gt_rotator.go`, `wardyn-tetragon-ingest`, the
  `groundtruth` Compose profile) has no k8s-substrate equivalent — it is not
  referenced anywhere under `internal/runner/k8s`. A k8s deployment gets the
  NetworkPolicy-enforced boundary (proven live by the boot-time egress
  canary) but not the independent kernel-level corroboration Compose +
  Tetragon provides.
- **`replicas` stays 1 on k8s exactly as it does everywhere else** — see
  [One replica, by construction](#one-replica-by-construction) above; nothing
  about the k8s substrate changes that story (the masking registry is still
  in-process, per-pod).

None of these are silent: the mount and BYOI gaps fail the run closed with a
named error, the resource-cap gaps log a warning naming exactly what is
unenforced, and the DNS/replica behavior matches the rest of this document.
Closing any of them is unstarted work, not a documented-but-planned
near-term item — see ROADMAP.md for what is actually queued.
