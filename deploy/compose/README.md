# Wardyn compose demo

One of Wardyn's two CI-tested deployment paths (the other is
`deploy/helm/wardyn`). This stack stands up the whole control plane locally:

| Service    | Role |
|------------|------|
| `postgres` | System of record (the only required dependency). |
| `dex`      | OIDC IdP for human SSO. Static demo user `demo@wardyn.local`. The console's SSO button lights up once `WARDYN_OIDC_*` is set (`--profile sso` + this service); without it, use the admin-token path or the CLI. Either way every signed-in user has admin-equivalent powers — **team mode** (per-user RBAC) does not exist yet and is not scheduled; see [ROADMAP.md](../../ROADMAP.md). |
| `wardynd`  | Control plane, **built with `-tags docker`** so the docker runner can launch real governed sandboxes. |

The `wardyn-proxy` image is built (the per-run L2 egress sidecar the runner
launches) but not run as a long-lived service.

## Quick start

> **Note:** this stack is the SUPPORTED single-user **containerized** setup.
> `make setup` launches it by default (Enter at the prompt, or
> `WARDYN_SETUP_MODE=container`), which runs `./scripts/up.sh up` for you;
> choice 2 / `local` is advanced host mode, and `make compose-up` is the
> raw-compose variant.

```sh
./scripts/up.sh up    # doctor preflight, build, mint a secret key, bring up postgres+wardynd, open the UI
```

`scripts/up.sh up` is THE one command for the compose stack: it runs a read-only
preflight (`make doctor`), builds the `wardynd` image, mints/persists a
`WARDYN_AGE_KEY`, auto-picks a confinement policy, starts `postgres` +
`wardynd` in **local mode** (no SSO/Dex, no bearer token), and opens
<http://localhost:8080> in your browser as soon as it's healthy
(`WARDYN_UP_NO_BROWSER=1` to skip) — THEN builds the per-run images (sandbox
proxy + agent images) in the background so first light is fast; a run can't
launch until those finish (skip them with `WARDYN_UP_SKIP_RUN_IMAGES=1`). Run
`make doctor` any time on its own — it's read-only. Tear down with
`make compose-down`.

- **WSL**: run `./scripts/up.sh up` inside your WSL distro's shell; the UI opens
  in the Windows browser automatically.
- **Native Windows**: install WSL2 + Docker Desktop (with WSL integration)
  first, then run `./scripts/up.sh up` inside the WSL distro — `make doctor`
  detects a native Windows shell and blocks with this same guidance.

Want the SSO/Dex + bearer-token demo instead (a scripted governed run against
the full stack, incl. Dex)? The demo's run creation is driven by the admin
token / CLI.

```sh
make agent-images  # build the agent OCI images the demo run launches (claude-code + codex)
make demo          # build images, bring the stack up, create a demo run, show its audit trail
# or:
./scripts/demo.sh             # same (also builds the agent image for $WARDYN_DEMO_AGENT)
./scripts/demo.sh --no-build  # reuse already-built images (run `make agent-images` first)
./scripts/demo.sh down        # tear down (volumes preserved)
```

Then:

- UI / API: <http://localhost:8080>
- Dex discovery: <http://localhost:5556/.well-known/openid-configuration>
- Admin API: `Authorization: Bearer demo-admin-token`

The public API accepts **either** a valid OIDC session cookie **or** the admin
bearer token, so the CLI keeps working with the token while humans use SSO.

Dex (human SSO) is an **opt-in compose profile** (`sso`) — `scripts/up.sh up`
never starts it. `scripts/demo.sh` and `make demo`/`make compose-up` still start
it explicitly (compose always honors an explicitly-named service regardless of
active profiles). To bring Dex up yourself on top of a `scripts/up.sh up` stack:

```sh
docker compose -f deploy/compose/docker-compose.yaml --profile sso up -d dex
```

and set `WARDYN_OIDC_ISSUER=http://localhost:5556` in `deploy/compose/.env`
before restarting `wardynd`.

## No-login local mode (`WARDYN_LOCAL_MODE`)

`scripts/up.sh up` already runs in local mode. To bring the raw stack up by hand
(no demo run) use the Compose file directly — note the extension is `.yaml`, not
`.yml` — and set **`WARDYN_LOCAL_MODE`** to skip SSO/Dex and the bearer token
entirely, so you open the UI on localhost and spawn agents with no login:

```sh
docker compose -f deploy/compose/docker-compose.yaml up   # full stack
WARDYN_LOCAL_MODE=true docker compose -f deploy/compose/docker-compose.yaml up postgres wardynd
# then open http://localhost:8080 — no token, no login
```

Actions are attributed to the local operator (`local:<os-user>`, or set
`WARDYN_LOCAL_OPERATOR`), so the `sub`/`sponsor`/`decided_by`/audit attribution
chain stays meaningful. Sidecar/run-token auth is unaffected. Local mode
**refuses to start on an explicit publicly-routable IP** (it will not serve a
no-auth API on a public IP), but on an **unspecified bind** (`0.0.0.0`, the
`WARDYN_LISTEN` default) it only **warns** — it does not refuse — because that
bind might be host-firewalled or purely a docker-bridge address. For a real
guarantee, bind/publish loopback-only (the Compose default already publishes
`127.0.0.1`) — do not rely on the warning alone, and keep local mode on a
trusted single-dev machine. The `wardyn` CLI also works with no token against a
local-mode daemon.

## ⚠️ Daemon-trust tradeoff (read before running)

The `wardynd` service bind-mounts **`/var/run/docker.sock`**. That grants wardynd
**root-equivalent control of the host Docker daemon** — it can create, inspect,
and destroy *any* container on the host, not just Wardyn's. This is acceptable
**only** for a local, single-tenant demo on a machine you trust.

- **Never** expose this stack to untrusted networks or run it multi-tenant.
- A future production path is **Kubernetes** (`deploy/helm/wardyn`), planned to
  use scoped RBAC and share no host socket **[v0.5+ — planned]** — there is no
  Kubernetes runner driver yet, so the Helm chart cannot launch sandboxes
  today. Right now this Docker/Compose data plane (with its docker.sock
  daemon-trust tradeoff) is the only one that can actually run agents.

See `ARCHITECTURE.md` → "Deployment surface".

## Transport security (TLS)

`wardynd` serves plain HTTP by default (loud startup `WARNING`). For any
non-localhost deploy set `WARDYN_TLS_CERT`+`WARDYN_TLS_KEY` (both or neither —
one alone fails closed at boot), or `WARDYN_TLS_TERMINATED=true` behind a
terminating proxy; see [docs/ENV.md](../../docs/ENV.md). Do NOT set either for
the plain-HTTP localhost demo — the `Secure` cookie flag silently breaks login.

## Confinement classes on plain Docker

The stack defaults to `examples/policies/demo.json`, whose
`min_confinement_class` is **CC1** (hardened runc). Plain Docker hosts without
gVisor cannot enforce **CC2** (the `default.json` policy's requirement), and
Wardyn **fails closed** — it refuses to launch a run it cannot confine
(invariant 5). To use the stricter default policy on a CC2-capable host:

```sh
WARDYN_DEFAULT_POLICY=/examples/policies/default.json make demo
```

## Environment variables

Full reference: [docs/ENV.md](../../docs/ENV.md) — set them via
`deploy/compose/.env` (copy `.env.example`) or the shell environment. One
default is stack-specific: `WARDYN_SUBSCRIPTION_INJECT=off`, because the
distroless compose `wardynd` has no `claude` binary, so proxy-side OAuth
injection would fail-lazily and crash the run's proxy; a run that mounts
`~/.claude` uses those creds directly instead (stage them with
`WARDYN_SUBSCRIPTION_INJECT=off scripts/stage-claude-creds.sh`).

**CI overlay.** [`docker-compose.ci.yaml`](docker-compose.ci.yaml) layers onto
the base stack (`docker compose -f docker-compose.yaml -f docker-compose.ci.yaml`)
to turn on devcontainer image builds for BYOA CI runs (`wardyn run --image
<ref>`): it sets `WARDYN_ENVBUILD=true` and mounts the runner-tools directory
`scripts/ci-run.sh` assembles as `WARDYN_CI_TOOLS_DIR`. See
[`docs/CI.md`](../../docs/CI.md).

## OIDC issuer / hostname note

`wardynd` discovers and exchanges tokens with Dex server-side at `http://dex:5556`
(the compose network alias). The browser is redirected to that same issuer for
login. For the **browser** login leg to resolve `dex`, either run the browser on
the compose host with `127.0.0.1 dex` added to `/etc/hosts`, or simply use the
admin-token CLI path (what `scripts/demo.sh` does) — the headless demo never
needs the browser.

## AI Run Composer (optional)

The New Run wizard's "Describe your task" flow proposes a confined run from a plain-
English prompt. `.env.example` seeds the no-key `fake` backend, so copying it (what
`make setup` does) leaves the composer **on**; point `WARDYN_COMPOSER_CONFIG` at a real
backend for real proposals. Ready-made configs live in
[`../../examples/composer-configs/`](../../examples/composer-configs/): `fake.json` (no
key, deterministic demo), `claude-cli-opus.json` (host-mode + Claude subscription), and
`anthropic-api` / `openai-api` templates (need a key via `wardyn secret set`). Empty
disables it and the compose tab is hidden.

## What the demo proves

The audit trail printed at the end shows the governance chain end to end:

1. `identity.mint` — a per-run SPIFFE identity is minted (`actor_type=system`).
2. `run.create` (`actor_type=human`) — policy resolved, confinement gated.
3. sandbox dispatch — succeeds on a host with the agent image present;
   otherwise the run lands in `FAILED` with the pull error recorded (graceful:
   the run row persists and is queryable, the create call never 500s).

Audit events are simultaneously written to the configured **file sink**
(`/data/audit/audit.log` in the `audit` volume) via the fanout, while Postgres
remains the source of truth.

## eBPF ground-truth tier (optional)

The SECOND of Wardyn's three audit streams (Postgres self-report + PTY replay are
the others) is the eBPF/Tetragon **ground-truth** stream: a privileged host
sensor (`tetragon`) exports kernel events as JSON, and a sidecar
(`wardyn-tetragon-ingest`) correlates each to a Wardyn run (via the
`wardyn.run-id` container label), maps a bounded subset to `kernel.*` audit
events, and POSTs them to wardynd — where they are recorded append-only and fan
to every SIEM sink, exactly like every other event.

This tier is **opt-in** (compose profile `groundtruth`) and **honestly
degradable**: with it OFF, `wardynd`'s `/healthz` reports
`ebpf_groundtruth=unavailable` — never a silent claim that the stream exists.

> **Prerequisite — set a persistent `WARDYN_AGE_KEY` first.** The token below is
> minted by a *second* `wardynd` process started via `docker compose exec`. That
> process must load the SAME age identity as the running server to read its
> signing key. With `WARDYN_AGE_KEY` unset (the demo default) each `wardynd` boots an
> *ephemeral* key, so the exec'd process cannot decrypt the server's keys and now
> **fails closed** (it will not, and must not, silently regenerate them). Export a
> real key (`age-keygen`) into the compose env before seeding:
> `export WARDYN_AGE_KEY=AGE-SECRET-KEY-...` (and keep it set for the stack).

> **Automatic (preferred):** the compose stack wires a wardynd token *rotator* that
> keeps the shared `groundtruth_token` volume file fresh (`WARDYN_GROUNDTRUTH_TOKEN_FILE`),
> and the sidecar re-reads it on a 401 — so you can SKIP the manual seeding below and
> just start the groundtruth profile; ground truth then survives the ~1h token TTL
>. The static-token path below still works but goes permanently blind after ~1h.

```bash
# 1. The stack must already be up with a persistent WARDYN_AGE_KEY (see above).
# 2. (OPTIONAL — the rotator above supersedes this.) Mint a static host-sensor token
#    (aud=wardyn-groundtruth; audit-write-only — it can
#    never mint credentials or decide approvals). The exec'd wardynd inherits
#    WARDYN_AGE_KEY from the container env, so it loads the same keys:
export WARDYN_GROUNDTRUTH_TOKEN=$(docker compose exec -T wardynd \
  /usr/local/bin/wardynd -print-groundtruth-token \
  -dsn "postgres://wardyn:wardyn-dev@postgres:5432/wardyn?sslmode=disable")
# 3. Build + start the tier:
docker compose --profile groundtruth build
docker compose --profile groundtruth up -d tetragon wardyn-tetragon-ingest
# 4. Confirm /healthz flips to healthy once heartbeats arrive:
curl -s localhost:8080/healthz | jq .ebpf_groundtruth
```

Honest limits (by design, not hidden):
- **Detection, not prevention.** It never blocks. Exec of a dynamic linker
  (`ld-linux*`/`ld-musl*`) is FLAGGED (`data.loader=true`) — the documented
  `ld-linux`/`mmap` bypass is made visible, not stopped.
- **Host eBPF is blind inside CC3/Kata guests.** For such runs the sidecar emits
  a one-time `kernel.sensor.blind` event so the gap is visible. Set
  `WARDYN_GROUNDTRUTH_BLIND_RUNS=<run-id>,...` to record it at sidecar boot.
- The token has the identity provider's ~1h TTL. The compose stack keeps it fresh
  AUTOMATICALLY: wardynd's rotator re-mints and rewrites the shared `groundtruth_token`
  file and the sidecar re-reads it on a 401, so ground truth does NOT go blind ~1h in
 . The manual `WARDYN_GROUNDTRUTH_TOKEN` env is a static fallback that cannot
  refresh. NOTE: full end-to-end recovery is verified by unit tests (the rotator mints
  + writes; the sidecar reads + refreshes); the live cross-container recovery under a
  real Tetragon sensor needs a privileged eBPF host to exercise.

See `tetragon-policies/` for the `TracingPolicy` and `internal/groundtruth` for
the mapper.
