# Wardyn Helm Chart

This chart deploys `wardynd` (the control plane) to a Kubernetes cluster, connecting to a Postgres database for state persistence and audit logging.

> **Published automatically — but only for a released version.** CI
> ([.github/workflows/publish-image.yml](../../../.github/workflows/publish-image.yml))
> builds and pushes `ghcr.io/cjohnstoniv/wardynd` on every push to `main`
> (`:latest`, `:sha-<commit>`) and on every `vX.Y.Z` release tag (the bare
> semver, e.g. `0.4.4` — matching this chart's default `image.tag`,
> `.Chart.AppVersion`; see [RELEASING.md](../../../RELEASING.md)). The
> chart's defaults resolve to a real image once the version in
> `Chart.yaml`'s `appVersion` has actually been released; for an
> unreleased/main-tip build, override `image.tag` to `latest` or
> `sha-<commit>`. Building your own (below) is still useful for a fork, a
> private registry, or a local change CI has not published yet — a wrong or
> stale `image.*` still renders fine and then `ImagePullBackOff`s forever,
> so double-check it either way.

> **[v0.5+ — planned] Kubernetes data plane.** There is no Kubernetes runner
> driver yet. This chart stands up `wardynd` and its dependencies, but
> **cannot create sandboxes** yet. Use Docker Compose (`deploy/compose/`) for
> a working agent run today.

## What it renders

`helm install wardyn ./deploy/helm/wardyn` (plus the required auth flag from
[Installation](#installation)) renders:

- **Deployment** (`wardynd`) — non-root (uid 65532), read-only root FS, all
  capabilities dropped, `RuntimeDefault` seccomp; liveness/readiness/startup
  probes on `/healthz`; `WARDYN_PG_DSN` and `WARDYN_ADMIN_TOKEN` sourced from
  Secrets.
- **Service** (ClusterIP) fronting the HTTP port (API + UI + `/healthz`).
- **ServiceAccount** (dedicated identity; token auto-mount off on the pod, so it
  also holds when you bring your own ServiceAccount).
- **Secret** — only in the inline/demo modes (DSN and/or admin token, see
  below); skipped for whichever credential you supply as an external Secret.
- **NetworkPolicy** — default-deny ingress/egress (Wardyn's L0 egress posture),
  re-opening only DNS, Postgres egress, and HTTP ingress from this namespace.

## Prerequisites

- Kubernetes 1.20+ with a CNI that enforces NetworkPolicy (the chart renders a
  portable `networking.k8s.io/v1` policy — no specific CNI required)
- Postgres 12+ (external or managed)
- A wardynd image: the chart's default pulls the CI-published one for a
  released version (see the callout at the top), or **build and push your
  own** (see below) for a fork, a private registry, or an unreleased change.

## Build and push wardynd

CI publishes `ghcr.io/cjohnstoniv/wardynd` for you on `main` and release tags
(see the callout above) — this section is only for a fork, a private
registry, or a local/unreleased change. From the repo root, with `REGISTRY`
set to a registry your cluster can pull from (`ghcr.io/<you>`, an ECR/GAR
host, a local registry — anything):

```bash
REGISTRY=ghcr.io/<you>          # your registry, not this repo's
TAG=$(git describe --tags --always --dirty)

docker build -f deploy/compose/Dockerfile.wardynd -t "$REGISTRY/wardynd:$TAG" .
docker push "$REGISTRY/wardynd:$TAG"
```

`Dockerfile.wardynd` is the same build the compose stack uses — it builds
wardynd with `-tags docker` onto `distroless:nonroot` (uid 65532), which is what
this chart's `podSecurityContext` already assumes.

If your registry is private, create a pull secret and pass it as
`image.pullSecrets` (a list of `{name: ...}`):

```bash
kubectl create secret docker-registry regcred -n wardyn \
  --docker-server="$REGISTRY" --docker-username=<user> --docker-password=<token>
# ... then add: --set image.pullSecrets[0].name=regcred
```

## Installation

If you built your own image above, point `image.repository`/`image.tag` at
what you just pushed (omit both to use the chart's defaults, which resolve
for a released version — see the callout at the top):

```bash
kubectl create secret generic wardyn-auth -n wardyn \
  --from-literal=admin-token="$(openssl rand -hex 32)"

helm install wardyn ./deploy/helm/wardyn \
  --namespace wardyn \
  --create-namespace \
  --set image.repository="$REGISTRY/wardynd" \
  --set image.tag="$TAG" \
  --set postgres.dsn.secretRef.name=wardyn-pg \
  --set auth.adminToken.secretRef.name=wardyn-auth
```

The image defaults `WARDYN_DEFAULT_POLICY=/examples/policies/default.json`
(baked into `Dockerfile.wardynd` — images older than that fix crash-loop on
boot with `open examples/policies/default.json: no such file or directory`;
on one of those, add
`--set env.WARDYN_DEFAULT_POLICY=/examples/policies/default.json`). To use a
different bundled policy, set `env.WARDYN_DEFAULT_POLICY` to any file under
`/examples/policies/` (`demo.json`, ...).

The chart **refuses to render** without an admin token or an OIDC issuer: an
install with neither brings up a pod that passes its `/healthz` probe and 401s
every API route.

Verify the pod is actually running (not `ImagePullBackOff` — the failure mode
when the image is wrong or absent):

```bash
kubectl -n wardyn rollout status deploy/wardyn --timeout=120s
```

## Database (DSN) — two modes

The two snippets below isolate the DSN wiring; both still need the
`--set image.repository=... --set image.tag=...` and `--set auth.adminToken.*`
flags from [Installation](#installation) — without the first the pod cannot
pull, without the second the chart will not render.

**1. External Secret (recommended).** Create the Secret out-of-band, then point
the chart at it (the default `postgres.dsn.secretRef.name` is `wardyn-postgres-dsn`):

```bash
kubectl create secret generic wardyn-pg \
  --from-literal=dsn="postgres://user:pass@postgres-host:5432/wardyn?sslmode=require" \
  -n wardyn
helm install wardyn ./deploy/helm/wardyn -n wardyn \
  --set postgres.dsn.secretRef.name=wardyn-pg \
  --set auth.adminToken.secretRef.name=wardyn-auth
```

The DSN never appears in the rendered manifests or Helm release history.

**2. Inline (demo only).** Clear `secretRef.name` and pass the DSN; the chart
creates `<release>-secrets`. The DSN lands base64'd in the release — laptop demos only:

```bash
helm install wardyn ./deploy/helm/wardyn -n wardyn \
  --set postgres.dsn.secretRef.name="" \
  --set postgres.dsn.value="postgres://wardyn:wardyn-dev@db:5432/wardyn?sslmode=disable" \
  --set auth.adminToken.secretRef.name=wardyn-auth
```

## Values

See `values.yaml` for all options. Key settings:

- `image.repository` / `image.tag`: wardynd container image. The defaults
  resolve to a real image once `Chart.yaml`'s `appVersion` has been released
  (see the callout at the top) — override both for a locally built image or
  an unreleased commit. `image.tag` empty => `.Chart.AppVersion`.
- `image.pullSecrets`: list of `{name: ...}` pull secrets for a private registry
- `postgres.dsn.secretRef.name`: existing Secret holding the DSN under `postgres.dsn.key` (empty => inline mode)
- `postgres.dsn.value`: inline DSN (inline mode only)
- `auth.adminToken.secretRef.name` / `auth.adminToken.value`: admin bearer token,
  external Secret or inline demo. **One of these (or `env.WARDYN_OIDC_ISSUER`)
  is required** — the chart fails the render otherwise. `env.WARDYN_OIDC_ISSUER`
  alone renders fine but is NOT enough to boot: also set
  `env.WARDYN_OIDC_OPERATOR_EMAILS`, or the pod crash-loops — wardynd refuses to
  start with OIDC configured and an empty operator list (override with
  `env.WARDYN_ALLOW_OIDC_NO_OPERATOR_LIST=true` if every signed-in human should
  really be admin-equivalent).
- `secrets.ageKey` / `secrets.ageKeyFromSecret`: secret-store age identity (empty
  => wardynd self-generates an ephemeral key). `ageKey` is inline-mode only;
  with an external DSN Secret, put `age-key` in it and set `ageKeyFromSecret=true`.
  **Set one of these against any real (non-inline) Postgres**, even for a quick
  trial: an ephemeral key does not survive a pod restart, and wardynd's own
  first-boot secret-store entries (e.g. its internal signing key) are written
  under whatever key that first boot generated — the NEXT boot generates a
  different one, can no longer decrypt them, and the pod crash-loops forever.
- `env`: extra `WARDYN_*` env (OIDC issuer, TLS, default policy). Renders as a
  literal in the pod spec — **not for secrets**. `WARDYN_DEFAULT_POLICY` in
  particular is **required in practice**: the image's own default is a
  relative path that does not resolve inside it (see
  [Installation](#installation)).
- `extraEnv`: raw `EnvVar` entries (so `valueFrom.secretKeyRef` works) for the
  secret-bearing variables docs/ENV.md marks 🔒: `WARDYN_OIDC_CLIENT_SECRET`,
  `WARDYN_COMPOSER_API_KEY`, and `WARDYN_AUDIT_SINKS` (its JSON carries the SIEM
  `bearer_token`).
- `persistence.enabled`: also decides `WARDYN_RECORDING_DIR` —
  `<mountPath>/recordings` when on, empty (replay disabled) when off. wardynd's
  own default writes to the read-only root FS and would crash-loop the pod.
  The chart pins `WARDYN_RECORDING_STORE=fs` — not because `pg` is unsafe here
  (its casts are already readable from any replica), but because this chart is
  single-replica by policy (see `replicas` below), so there is no HA reason to
  force the unbounded-by-default `pg` store on every install. Without the pin,
  wardynd's own process default (`pg`) would win instead, ignoring
  `WARDYN_RECORDING_DIR` and persisting every PTY asciicast into the
  control-plane database with `WARDYN_RECORDING_RETENTION_DAYS` defaulting to
  keep-forever. An operator who wants that anyway can opt in with one key:
  `env.WARDYN_RECORDING_STORE=pg` (leave `persistence` off — the `pg` store
  needs no PVC).
- `networkPolicy.*`: default-deny policy knobs (Postgres port, ingress sources, extra egress)
- `replicas`: **leave at 1 — the chart refuses anything higher.** A render with
  `replicas > 1` fails with an explicit message unless you also set
  `allowMultiReplica=true`. The pin is a safety control: wardynd's
  secret-masking registry is in-memory, per-process, and fails OPEN, so a
  session recording uploaded to a replica that did not handle that run's
  credential injection is persisted verbatim — live credentials in cleartext,
  with a `success` audit event. The per-process defects that used to make a
  second replica drop *requests* — attach tickets, compose-result uploads, run
  watchers, session recordings, and the ground-truth token rotator — are closed
  at the code level (Postgres-backed state, leases, and leader election); the
  masking registry is not, and neither are the other per-process items
  enumerated in [docs/OPERATIONS.md](../../../docs/OPERATIONS.md) ("One replica,
  by construction"). `allowMultiReplica` is an acceptance of that, not a fix.
- `allowMultiReplica`: override for the refusal above. Default `false`.

Where this chart is headed: [ROADMAP.md](../../../ROADMAP.md).
