# Wardyn Helm Chart

The ONE blessed Kubernetes deployment path for Wardyn governance control plane.

This chart deploys `wardynd` (the control plane) to a Kubernetes cluster, connecting to a Postgres database for state persistence and audit logging.

> **[v0.5 — planned] Kubernetes data plane.** There is no Kubernetes runner
> driver yet. This chart stands up `wardynd` and its dependencies, but
> **cannot create sandboxes** yet. Use Docker Compose (`deploy/compose/`) for
> a working agent run today.

## What it renders

`helm install wardyn ./deploy/helm/wardyn` renders:

- **Deployment** (`wardynd`) — non-root (uid 65532), read-only root FS, all
  capabilities dropped, `RuntimeDefault` seccomp; liveness/readiness/startup
  probes on `/healthz`; `WARDYN_PG_DSN` sourced from a Secret.
- **Service** (ClusterIP) fronting the HTTP port (API + UI + `/healthz`).
- **ServiceAccount** (dedicated identity; token auto-mount off by default).
- **Secret** — only in the inline/demo DSN mode (see below); skipped when you
  reference an external Secret.
- **NetworkPolicy** — default-deny ingress/egress (Wardyn's L0 egress posture),
  re-opening only DNS, Postgres egress, and HTTP ingress.

## Features

- Single control plane deployment
- Postgres backend for all state and audit logs
- Default-deny NetworkPolicy (L3/L4); Cilium `toFQDN` integration for egress-by-domain
- Full RBAC support **[v0.5 — planned]**
- Per-run identity with SPIFFE integration **[v0.5 — planned]** (blocked on the Kubernetes runner above)

## Prerequisites

- Kubernetes 1.20+
- Postgres 12+ (external or managed)
- Cilium (recommended for NetworkPolicy toFQDN support)

## Installation

```bash
helm install wardyn ./deploy/helm/wardyn \
  --namespace wardyn \
  --create-namespace \
  --set postgres.dsn.secretRef.name=wardyn-pg
```

## Database (DSN) — two modes

**1. External Secret (recommended).** Create the Secret out-of-band, then point
the chart at it (the default `postgres.dsn.secretRef.name` is `wardyn-postgres-dsn`):

```bash
kubectl create secret generic wardyn-pg \
  --from-literal=dsn="postgres://user:pass@postgres-host:5432/wardyn?sslmode=require" \
  -n wardyn
helm install wardyn ./deploy/helm/wardyn -n wardyn --set postgres.dsn.secretRef.name=wardyn-pg
```

The DSN never appears in the rendered manifests or Helm release history.

**2. Inline (demo only).** Clear `secretRef.name` and pass the DSN; the chart
creates `<release>-secrets`. The DSN lands base64'd in the release — laptop demos only:

```bash
helm install wardyn ./deploy/helm/wardyn -n wardyn \
  --set postgres.dsn.secretRef.name="" \
  --set postgres.dsn.value="postgres://wardyn:wardyn-dev@db:5432/wardyn?sslmode=disable"
```

## Values

See `values.yaml` for all options. Key settings:

- `image.repository` / `image.tag`: wardynd container image
- `postgres.dsn.secretRef.name`: existing Secret holding the DSN under `postgres.dsn.key` (empty => inline mode)
- `postgres.dsn.value`: inline DSN (inline mode only)
- `secrets.ageKey` / `secrets.ageKeyFromSecret`: secret-store age identity (empty => wardynd self-generates an ephemeral key)
- `env`: extra `WARDYN_*` env (OIDC, TLS, audit sinks, default policy)
- `networkPolicy.*`: default-deny policy knobs (Postgres port, ingress sources, extra egress)
- `replicas`: number of wardynd replicas

## Roadmap

This chart is the v0.5 blessed path. Future releases will add:

- CRD support for RunPolicy, AgentRun, ApprovalRequest
- Cilium NetworkPolicy automation
- SPIRE sidecar injection for per-run identities
- Observability (metrics, tracing) via OpenTelemetry
