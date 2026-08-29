# Wardyn compose demo

One of Wardyn's two CI-tested deployment paths (the other is
`deploy/helm/wardyn`). This stack stands up the whole control plane locally:

| Service    | Role |
|------------|------|
| `postgres` | System of record (the only required dependency). |
| `dex`      | OIDC IdP for human SSO. Static demo user `demo@wardyn.local`. The console's SSO button lights up once `WARDYN_OIDC_*` is set (`--profile sso` + this service); without it, use the admin-token path or the CLI. Every signed-in user has admin-equivalent powers unless `WARDYN_OIDC_OPERATOR_EMAILS` (the legacy allowlist — required at boot under OIDC) or `WARDYN_OIDC_ROLE_MAP` (the real **admin/member** RBAC) is set, which makes an unlisted signer-in a **member** (with a role map, an unmatched signer-in follows `WARDYN_OIDC_DEFAULT_ROLE` or is denied login): 403 on the harness-credential/policy/workspace/site-config writes, secret writes/deletes, admin-only approval decisions, and bringing a custom image — reading and launching/killing stay open, and a member is owner-scoped (sees only their own runs/approvals/audit; may decide `egress_domain` approvals on runs they own; their `inline_policy` is clamped to your ceiling). That admin/member model with owner scoping **shipped in v0.5**; a fully packaged **team mode** (SAML/SCIM) is not built yet — see [docs/OPERATIONS.md](../../docs/OPERATIONS.md#multi-user-who-can-change-what) and [ROADMAP.md](../../ROADMAP.md). Members of this stack: [`../../docs/MEMBERS.md`](../../docs/MEMBERS.md). |
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

and set `WARDYN_OIDC_ISSUER=http://localhost:5556` **and**
`WARDYN_OIDC_OPERATOR_EMAILS=you@example.com` (or
`WARDYN_ALLOW_OIDC_NO_OPERATOR_LIST=true`) in `deploy/compose/.env` before
restarting `wardynd` — the issuer alone now REFUSES TO BOOT.

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
- The production path is **Kubernetes** (`deploy/helm/wardyn`): the k8s runner
  substrate (`k8s.enabled`, shipped v0.5) uses scoped RBAC and shares no host
  socket. This Docker/Compose data plane (with its docker.sock daemon-trust
  tradeoff) remains the primary local/demo path.

See `ARCHITECTURE.md` → "Deployment surface".

## Transport security (TLS)

`wardynd` serves plain HTTP by default (loud startup `WARNING` on loopback or
the unspecified bind; a specific non-loopback bind is **refused at boot** unless
a TLS posture or `WARDYN_ALLOW_PLAINTEXT_LISTEN=true` is set). For any
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
`WARDYN_SUBSCRIPTION_INJECT=off scripts/stage-claude-creds.sh`). This flag
covers ONLY that resident-mount path — the separate Wardyn-managed lane (a
connected managed setup-token, no `~/.claude` mount) still injects proxy-side
and still MITMs `api.anthropic.com` regardless of this setting.

**CI overlay.** [`docker-compose.ci.yaml`](docker-compose.ci.yaml) layers onto
the base stack (`docker compose -f docker-compose.yaml -f docker-compose.ci.yaml`)
to turn on devcontainer image builds for BYOA CI runs (`wardyn run --image
<ref>`): it sets `WARDYN_ENVBUILD=true` and mounts the runner-tools directory
`scripts/ci-run.sh` assembles as `WARDYN_CI_TOOLS_DIR`. See
[`docs/CI.md`](../../docs/CI.md).

## OIDC issuer / hostname note

The browser talks to the PUBLIC issuer `http://localhost:5556`; `wardynd`
reaches Dex server-side at `http://dex:5556` (`WARDYN_OIDC_INTERNAL_ISSUER`,
the compose network alias) and rewrites discovery/token/JWKS URLs between the
two — so SSO works in the browser with **no `/etc/hosts` edit**. The
admin-token CLI path (what `scripts/demo.sh` does) never needs the browser at
all.

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
> just start the groundtruth profile; ground truth then survives the ~1h token
> TTL. The static-token path below still works but goes permanently blind after ~1h.

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
- **The kernel sensor itself is host-wide.** `tetragon` is a privileged HOST
  sensor: its `TracingPolicy` (`tetragon-policies/wardyn-groundtruth.yaml`) has
  no namespace/cgroup/binary selector, so it observes every process on the
  box, not only Wardyn's — a plain compose deployment has no stable per-
  container identity a static policy file can select on ahead of a container
  even existing. By DEFAULT the ingest sidecar drops any exec/connect/write
  event it cannot correlate to a `wardyn.managed=true` agent container
  (`gatedMapper`, `cmd/wardyn-tetragon-ingest/correlator.go`) before it reaches
  this audit log / SIEM fanout, so other containers' and the bare host's
  activity is NOT forwarded by default. Set
  `WARDYN_GROUNDTRUTH_FORWARD_UNMAPPED_HOST_EVENTS=true` on the sidecar to
  opt back into forwarding those unmapped events (`run_id` NULL,
  `correlation=unmapped`) for full-host detection coverage.
- **Host eBPF is blind inside CC3/Kata guests.** For such runs the sidecar emits
  a one-time `kernel.sensor.blind` event so the gap is visible. Set
  `WARDYN_GROUNDTRUTH_BLIND_RUNS=<run-id>,...` to record it at sidecar boot.
- **`kernel.network.connect` needs a kernel that reports container egress.**
  On WSL2 + Docker Desktop (kernel `*-microsoft-standard-WSL2`, Tetragon
  v1.1.2) the `tcp_connect` kprobe delivers NO event for a connect that leaves
  a container over its veth — only host-netns and container-loopback connects
  appear. Measured, not inferred: ~120 real container connects produced zero
  events while `tetragon_ringbuf_perf_event_lost_total` stayed 0, and it
  persists with the file kprobe removed entirely. Nothing in Wardyn can fix
  that from user space, so it is GATED rather than papered over: `/healthz`
  reports `ebpf_groundtruth.state="partial"` with
  `missing_kinds:["kernel.network.connect"]`, and every capture/profile carries
  the matching caveat. Run the ground-truth tier on a normal Linux kernel for
  connect coverage; on WSL2, treat `partial` as expected, not as a bug.
- **The shipped policy is loud, and the export keeps only ~50 s.** The
  host-wide `security_file_permission` kprobe dominates the export (~96% of
  lines, thousands/s on an idle box), so Tetragon's 10 MB × 5 rotation holds
  well under a minute of history. An ingest stall longer than that loses ground
  truth outright. Raise `--export-file-max-size-mb`/`--export-file-max-backups`
  (or narrow the kprobe) on a busy host.
- **Correlation covers containers seen in the last 15 minutes.** The sidecar
  binds kernel events to runs from a `docker events` stream plus a
  `docker ps -a` reconcile, and keeps a mapping for `containerRetention`
  (15 min) after the container is gone, because the tail runs behind. An event
  arriving later than that correlates to nothing and is dropped — counted as
  `dropped_unmapped` on the heartbeat and named in `/healthz`'s `idle` reason,
  so a broken correlation can never again read the same as a blind sensor.
- The token has the identity provider's ~1h TTL. The compose stack keeps it fresh
  AUTOMATICALLY: wardynd's rotator re-mints and rewrites the shared `groundtruth_token`
  file and the sidecar re-reads it on a 401, so ground truth does NOT go blind
  ~1h in. The manual `WARDYN_GROUNDTRUTH_TOKEN` env is a static fallback that cannot
  refresh. NOTE: full end-to-end recovery is verified by unit tests (the rotator mints
  + writes; the sidecar reads + refreshes); the live cross-container recovery under a
  real Tetragon sensor needs a privileged eBPF host to exercise.

See `tetragon-policies/` for the `TracingPolicy` and `internal/groundtruth` for
the mapper.
