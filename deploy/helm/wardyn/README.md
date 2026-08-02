# Wardyn Helm Chart

This chart deploys `wardynd` (the control plane) to a Kubernetes cluster, connecting to a Postgres database for state persistence and audit logging.

> **YOU MUST BUILD AND PUSH THE IMAGE FIRST — no wardynd image is published
> anywhere.** No workflow in this repo pushes to a registry (see
> [docs/CI.md](../../../docs/CI.md)), so the chart's default
> `image.repository` (`ghcr.io/cjohnstoniv/wardynd`) and its default tag
> (`.Chart.AppVersion`) resolve to an image **that does not exist**.
> `helm install` with the defaults renders fine and then `ImagePullBackOff`s
> forever. Build + push your own and point `image.*` at it — see
> [Build and push wardynd](#build-and-push-wardynd) below, which every
> `helm install` example here assumes you have done.

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
- **A wardynd image you built and pushed yourself** (see below), plus a
  registry your cluster can pull from.

## Build and push wardynd

Nothing publishes this image for you. From the repo root, with `REGISTRY` set to
a registry your cluster can pull from (`ghcr.io/<you>`, an ECR/GAR host, a local
registry — anything):

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

Point `image.repository`/`image.tag` at what you just pushed — the chart's
defaults resolve to an image that does not exist:

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
kubectl create secret generic wardyn-auth --from-literal=token="$(openssl rand -hex 32)" -n wardyn
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

- `image.repository` / `image.tag`: wardynd container image. **Required in
  practice** — the defaults name an unpublished image (see the warning at the
  top). `image.tag` empty => `.Chart.AppVersion`, which no registry carries.
- `image.pullSecrets`: list of `{name: ...}` pull secrets for a private registry
- `postgres.dsn.secretRef.name`: existing Secret holding the DSN under `postgres.dsn.key` (empty => inline mode)
- `postgres.dsn.value`: inline DSN (inline mode only)
- `auth.adminToken.secretRef.name` / `auth.adminToken.value`: admin bearer token,
  external Secret or inline demo. **One of these (or `env.WARDYN_OIDC_ISSUER`)
  is required** — the chart fails the render otherwise.
- `secrets.ageKey` / `secrets.ageKeyFromSecret`: secret-store age identity (empty
  => wardynd self-generates an ephemeral key). `ageKey` is inline-mode only;
  with an external DSN Secret, put `age-key` in it and set `ageKeyFromSecret=true`.
- `env`: extra `WARDYN_*` env (OIDC issuer, TLS, default policy). Renders as a
  literal in the pod spec — **not for secrets**.
- `extraEnv`: raw `EnvVar` entries (so `valueFrom.secretKeyRef` works) for the
  secret-bearing variables docs/ENV.md marks 🔒: `WARDYN_OIDC_CLIENT_SECRET`,
  `WARDYN_COMPOSER_API_KEY`, and `WARDYN_AUDIT_SINKS` (its JSON carries the SIEM
  `bearer_token`).
- `persistence.enabled`: also decides `WARDYN_RECORDING_DIR` —
  `<mountPath>/recordings` when on, empty (replay disabled) when off. wardynd's
  own default writes to the read-only root FS and would crash-loop the pod.
- `networkPolicy.*`: default-deny policy knobs (Postgres port, ingress sources, extra egress)
- `replicas`: **leave at 1.** wardynd is single-writer by construction — attach
  tickets (`internal/api/server.go`), compose-result uploads
  (`internal/api/composeresult.go`) and the ground-truth token rotator
  (`cmd/wardynd/gt_rotator.go`) are per-process, so a second replica drops the
  requests that land on the wrong pod. See [docs/OPERATIONS.md](../../../docs/OPERATIONS.md).

Where this chart is headed: [ROADMAP.md](../../../ROADMAP.md).
