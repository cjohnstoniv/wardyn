# Wardyn Helm Chart

This chart deploys `wardynd` (the control plane) to a Kubernetes cluster, connecting to a Postgres database for state persistence and audit logging.

> **Published automatically — but only for a released version.** CI
> ([.github/workflows/publish-image.yml](../../../.github/workflows/publish-image.yml))
> builds and pushes `ghcr.io/cjohnstoniv/wardynd` on every push to `main`
> (`:latest`, `:sha-<commit>`) and on every `vX.Y.Z` release tag (the bare
> semver, e.g. `0.4.5` — matching this chart's default `image.tag`,
> `.Chart.AppVersion`; see [RELEASING.md](../../../RELEASING.md)). The
> chart's defaults resolve to a real image once the version in
> `Chart.yaml`'s `appVersion` has actually been released; for an
> unreleased/main-tip build, override `image.tag` to `latest` or
> `sha-<commit>`. Building your own (below) is still useful for a fork, a
> private registry, or a local change CI has not published yet — a wrong or
> stale `image.*` still renders fine and then `ImagePullBackOff`s forever,
> so double-check it either way.

> **Kubernetes data plane (`k8s.enabled`, v0.5+).** Off by default — this chart
> stands up `wardynd` on its own either way, but sandboxes need a runner
> substrate. Docker Compose (`deploy/compose/`) is still the primary local
> path; set `k8s.enabled=true` (see [Kubernetes runner substrate](#kubernetes-runner-substrate-k8senabled)
> below) to make wardynd create/manage sandboxes as pods in THIS cluster
> instead (`internal/runner/k8s`, a `-tags k8s` build).

## Quickstart

`make kind-quickstart` runs [`deploy/kind/quickstart.sh`](../../kind/quickstart.sh):
one command, a throwaway [kind](https://kind.sigs.k8s.io/) cluster, and a real
install of this chart with the Kubernetes runner substrate ON (sandboxes are
pods in that cluster). It builds `wardynd`/`wardyn-proxy` locally, `kind
load`s them, installs Calico pinned to exactly what CI's `conformance-k8s`
job pins (the substrate refuses to boot on a CNI that doesn't enforce
NetworkPolicy — this script never works around that refusal), and prints the
URL, admin token, and pod list once `/healthz` answers through the published
NodePort:

```
$ make kind-quickstart
...
Wardyn is up.

  URL:    http://127.0.0.1:8080
  Token:  <printed>
  SSH:    ssh -p 2222 <run-id>@127.0.0.1   (docs/SSH.md)
```

`make kind-down` deletes the cluster. It is demo-grade, not a production
recipe (single-pod Postgres, no PVC, inline admin token) — read on for a real
install.

## What it renders

`helm install wardyn ./deploy/helm/wardyn` (plus the required auth flag from
[Installation](#installation)) renders:

- **Deployment** (`wardynd`) — non-root (uid 65532), read-only root FS, all
  capabilities dropped, `RuntimeDefault` seccomp; liveness/readiness/startup
  probes on `/healthz`; `WARDYN_PG_DSN` and `WARDYN_ADMIN_TOKEN` sourced from
  Secrets.
- **Service** (ClusterIP) fronting the HTTP port (API + UI + `/healthz`), plus
  an SSH port when `ssh.enabled` (same Service, no second object — see
  [Split SSH exposure](#split-ssh-exposure) to expose it differently) and a UI
  port when `uiSandbox.enabled` (which must reach a DIFFERENT hostname — see
  [UI sandbox gateway](#ui-sandbox-gateway)).
- **ServiceAccount** (dedicated identity; token auto-mount off on the pod by
  default, so it also holds when you bring your own ServiceAccount —
  `k8s.enabled` requires flipping this to `true`, see below).
- **Secret** — only in the inline/demo modes (DSN and/or admin token, see
  below); skipped for whichever credential you supply as an external Secret.
- **NetworkPolicy** — default-deny ingress/egress (Wardyn's L0 egress posture),
  re-opening DNS, Postgres egress, HTTP (+ SSH and + the UI-sandbox gateway,
  when enabled) ingress from this
  namespace, and (`k8s.enabled`) API-server egress plus an ingress peer for a
  separate `k8s.runsNamespace`.
- **Role/RoleBinding + ClusterRole/ClusterRoleBinding** (`k8s.enabled` only) —
  least-privilege RBAC for the k8s runner substrate; see
  [Kubernetes runner substrate](#kubernetes-runner-substrate-k8senabled).

## Prerequisites

Platform requirements, in one breath: **Kubernetes 1.20+, Helm 3, a
NetworkPolicy-enforcing CNI, and Postgres 12+.** Everything else the control
plane needs (ServiceAccount, namespaced RBAC, NetworkPolicies, Secrets
wiring) is rendered by this chart. In detail:

- **Kubernetes 1.20+** and **Helm 3**.
- **A CNI that enforces NetworkPolicy** (Calico, Cilium, ...). The chart
  renders portable `networking.k8s.io/v1` policies — no specific CNI required
  — but enforcement is load-bearing: with `k8s.enabled`, a boot-time egress
  canary verifies it and refuses to start the substrate on a non-enforcing
  CNI (kind's default kindnet is the classic case — the conformance CI lane
  pins kind + Calico; `WARDYN_K8S_ALLOW_UNENFORCED_NETPOL=1` downgrades the
  refusal to a loud warning).
- **Postgres 12+** (external or managed).
- A wardynd image: the chart's default pulls the CI-published one for a
  released version (see the callout at the top), or **build and push your
  own** (see below) for a fork, a private registry, or an unreleased change.
- Optional: **RuntimeClasses** delivering gVisor/Kata isolation, pinned via
  `k8s.runtimeClasses.CC2`/`.CC3`, to advertise the stronger confinement
  tiers — CC1 works out of the box. An **OIDC issuer** for SSO and
  admin/member RBAC (see [Multi-user](#multi-user-adminmember-rbac)).

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

**`k8s.enabled` also needs `wardyn-proxy` built and pushed** — the chart
refuses to render without `k8s.proxyImage` set (see below), and the quickstart
a few sections down assumes `$REGISTRY/wardyn-proxy:$TAG` already exists:

```bash
docker build -f deploy/compose/Dockerfile.proxy -t "$REGISTRY/wardyn-proxy:$TAG" .
docker push "$REGISTRY/wardyn-proxy:$TAG"
```

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

## Multi-user (admin/member RBAC)

Wardyn has a real two-role model — every OIDC session carries an **admin** or
**member** role, derived at login (`internal/auth/oidc`'s `deriveRole`).
`env.WARDYN_OIDC_ISSUER` alone only enables SSO (everyone signs in as admin,
today's pre-0.5 behavior); `env.WARDYN_OIDC_ROLE_MAP` is what turns that into
RBAC. Full semantics (ownership scoping, the approval kind-restriction, the
policy clamp, the admin-token ceiling): [docs/OPERATIONS.md's "Multi-user: who
can change what"](../../../docs/OPERATIONS.md#multi-user-who-can-change-what).
A worked, end-to-end setup for Entra ID App Roles specifically (the manifest,
"assignment required", and the `email_verified` trap) lives in the
`wardyn-k8s-setup` Claude Code skill
(`.claude/skills/wardyn-k8s-setup/SKILL.md`).

A minimal end-to-end values snippet — an existing OIDC app registration, two
Entra App Roles already created (`Wardyn.Admin`, `Wardyn.Member`), the legacy
allowlist kept as a safety net:

```yaml
env:
  WARDYN_OIDC_ISSUER: "https://login.microsoftonline.com/<tenant-id>/v2.0"
  WARDYN_OIDC_ROLE_MAP: "Wardyn.Admin=admin,Wardyn.Member=member"
  WARDYN_OIDC_DEFAULT_ROLE: "member"          # unmatched users land here instead of being denied
  WARDYN_OIDC_OPERATOR_EMAILS: "platform@corp.example"  # legacy admin safety net, still honored

extraEnv:
  - name: WARDYN_OIDC_CLIENT_SECRET       # 🔒 secret-bearing var — extraEnv, never a literal `env` value
    valueFrom:
      secretKeyRef: {name: wardyn-oidc, key: client-secret}
```

```bash
helm upgrade --install wardyn ./deploy/helm/wardyn -n wardyn \
  --set postgres.dsn.secretRef.name=wardyn-pg \
  --set auth.adminToken.secretRef.name=wardyn-auth \
  -f rbac-values.yaml   # the snippet above
```

Keep `auth.adminToken` configured even with OIDC set up: the admin token
always authenticates as admin (one shared credential, no per-human identity to
demote), which is the recovery path if a role-map typo ever locks every human
out. Set `env.WARDYN_OIDC_ROLE_MAP` on its own (no `extraEnv`/Entra changes)
against an EXISTING OIDC-only install to turn on RBAC for the first time — it
takes effect on each user's next login (a session signed before the role map
existed carries no role and is never treated as authenticated).

## Kubernetes runner substrate (`k8s.enabled`)

Off by default. Turning it on makes wardynd itself create/manage sandboxes as
pods in this cluster (`internal/runner/k8s`) instead of the Docker Compose
path — a completely separate confinement substrate (L1, NetworkPolicy-backed,
proven live by a boot-time egress canary) from the Compose stack's L0
(structural, no-default-route) one.

```bash
helm install wardyn ./deploy/helm/wardyn -n wardyn \
  --set auth.adminToken.secretRef.name=wardyn-auth \
  --set postgres.dsn.secretRef.name=wardyn-pg \
  --set serviceAccount.automount=true \
  --set k8s.enabled=true \
  --set k8s.proxyImage="$REGISTRY/wardyn-proxy:$TAG"
```

- `k8s.enabled`: turns on the wiring below. **Requires
  `serviceAccount.automount=true`** — the chart refuses to render otherwise
  (the substrate drives the API server directly via client-go, which needs
  the pod's own projected ServiceAccount token; `automount=false` is the
  chart's own default, since a non-k8s wardynd calls no API server at all).
- `k8s.runsNamespace`: namespace every sandbox (Secret/NetworkPolicies/pods)
  is created in. Empty (default) => the release namespace, with nothing extra
  to set up. A DIFFERENT namespace must **already exist** — the chart never
  creates or labels it — and gets its own Role/RoleBinding plus an extra
  NetworkPolicy ingress peer (matched on the namespace's built-in
  `kubernetes.io/metadata.name` label, since an operator-created namespace
  carries no chart labels) so its proxy sidecars can still reach wardynd for
  credential mints, approval checks, and recording uploads.
- `k8s.proxyImage`: the wardyn-proxy sidecar image (`WARDYN_PROXY_IMAGE`) —
  also what the boot-time egress canary launches. **Required — the chart
  refuses to render without it** (like `serviceAccount.automount` above): the
  k8s runner substrate refuses to construct on an empty value
  (`errProxyImageUnset`), which is a boot-time failure, not a per-run one —
  wardynd itself never comes up, it does not boot fine with runs merely
  failing closed. See "Build and push wardynd" above for how to build and
  push it (`deploy/compose/Dockerfile.proxy`).
- `k8s.imagePullSecret`: optional pre-existing Secret name
  (`WARDYN_K8S_IMAGE_PULL_SECRET`) threaded onto every pod the substrate
  creates (agent, proxy, canary) — separate from `image.pullSecrets`, which is
  only for wardynd's own image.
- `k8s.runtimeClasses.CC2` / `.CC3`: pins a Confinement Class to a RuntimeClass
  NAME already registered in the cluster (`WARDYN_CONFINEMENT_MAP`), e.g.
  `--set k8s.runtimeClasses.CC2=gvisor`. Unlike Docker's well-known runtime
  family names, a RuntimeClass object name is operator-chosen and carries no
  platform convention Wardyn can guess — CC2/CC3 stay unadvertised
  (CC1-only) until pinned here to a RuntimeClass whose `.Handler` actually
  delivers that class's isolation.
- `k8s.apiServer.ports`: port(s) the control-plane NetworkPolicy opens so
  wardynd can reach the API server. Defaults to `[443, 6443]` —
  `kubernetes.default.svc`'s Service port (443, what client-go's in-cluster
  config always targets) PLUS the typical kubeadm/kind apiserver backend port
  (6443): on iptables-mode kube-proxy + Calico, the DNAT to that backend port
  happens BEFORE Calico evaluates egress, so `[443]` alone fails this rule
  closed on that (common) combination. Override/extend for a different
  apiserver port or a CNI/dataplane that evaluates pre-DNAT.
- **`k8s.apiServer.to`: empty (any destination) by default — this is a REAL
  WIDENING, not a narrow rule.** Like the Postgres egress rule right above it
  in the rendered NetworkPolicy, an empty `to` allows the ports above to ANY
  destination, because the apiserver is frequently not a selectable pod (a
  managed control plane, or static pods no podSelector/namespaceSelector can
  match) — there is no generically-correct default peer. **Scope this in any
  cluster where "wardynd can reach 443/6443 anywhere" is not an acceptable
  posture** — set it to a raw `NetworkPolicyPeer` list (same shape as
  `networkPolicy.egress.extra`), e.g. an `ipBlock` naming your cluster's
  actual apiserver/load-balancer CIDR.

RBAC ships least-privilege: the namespaced Role covers exactly the verbs the
substrate issues (pods create/get/list/delete/deletecollection;
`pods/ephemeralcontainers` update; `pods/exec` get+create — the exec
subresource's websocket transport issues GET, SPDY issues POST, and the
driver tries websocket first; secrets and networkpolicies
create/delete/deletecollection — deliberately **no** get/list on either,
wardynd never reads one back); the cluster-scoped ClusterRole covers
`runtimeclasses` get only (RuntimeClass is never namespaced, and the driver
only ever resolves one by name).

### Known gaps (v0.5)

The k8s substrate is not yet at parity with the Docker Compose one. Fails
closed with a clear error: **no BYOI/devcontainer image builds**, **no
`local_dir`/host-path workspace mounts** (git-clone workspaces are fine —
only a local-directory source is refused), and therefore **no `~/.aws` /
`~/.claude` host staging** either (use proxy-side subscription/Bedrock
credential injection instead — substrate-agnostic, works unchanged here).
Accepted but not enforced, with a logged warning naming the run: **no per-pod
PIDs limit** (set the node-level kubelet `podPidsLimit` as a cluster-wide
backstop) and **`DiskMiB`** (no writable-storage quota wired up yet). Also:
**no in-sandbox DNS** (a fast-failing loopback-only resolver — only
`wardyn-proxy` resolves hostnames, matching Compose's proxy-only egress),
**no k8s ground-truth correlator** (the Tetragon host-sensor pipeline has no
k8s-substrate equivalent), and **`replicas` stays 1**, same reason as every
other substrate (see [docs/OPERATIONS.md](../../../docs/OPERATIONS.md)'s
"One replica, by construction"). Full detail, including the exact code each
claim above is checked against: `docs/OPERATIONS.md`'s "Kubernetes: known
gaps (v0.5)" section.

What *is* proven, and what the gaps above are measured against: Wardyn ships
exactly two deployment paths — `deploy/compose` and this chart — and both run
sandboxes. CI proves the chart renders (`helm-lint`), boots to a healthy
control plane on a real cluster, AND (the k8s runner substrate,
`internal/runner/k8s`) actually creates a confined sandbox there,
conformance-tested on a NetworkPolicy-enforcing cluster (kind + Calico).

## Split SSH exposure

`ssh.enabled` adds an SSH port to wardynd's EXISTING Service (no second
Service object), so by default SSH shares whatever exposure HTTP has
(`service.type`, `networkPolicy.ingress.from`). To expose SSH differently —
e.g. its own `LoadBalancer` while HTTP stays internal `ClusterIP` — bring your
own minimal Service targeting the same pods:

```yaml
apiVersion: v1
kind: Service
metadata:
  name: wardyn-ssh
spec:
  type: LoadBalancer
  selector:
    app.kubernetes.io/name: wardyn
    app.kubernetes.io/instance: wardyn
  ports:
    - name: ssh
      port: 22
      targetPort: ssh
```

`targetPort: ssh` matches the Deployment's named containerPort
(`ssh.port`, default `2222`) regardless of what port your own Service exposes
it on. See [docs/SSH.md](../../../docs/SSH.md) for the SSH gateway itself
(what it does once traffic reaches it, session semantics, client setup).

## UI sandbox gateway

`uiSandbox.enabled` relays one policy-declared loopback port inside a run's
sandbox — a code editor, a dev server — to a browser
([docs/UI-SANDBOXES.md](../../../docs/UI-SANDBOXES.md)). Like `ssh.*` it adds a
conditional port to the SAME Service/Deployment, and it is off by default.

**The one thing this chart cannot do for you: give it its own hostname.** What
the gateway serves is the sandbox's own HTML and JavaScript. On the console's
origin that code could read the console's session and drive every admin action
the operator can — so `wardynd` refuses to boot when the two *bind addresses*
are equal, and it is on you to keep them apart at the *hostname* level too. A
single ingress hostname routing `/` to the console and something else to the
gateway re-creates exactly the shared origin the second listener exists to
prevent.

```yaml
uiSandbox:
  enabled: true
  port: 8081
  # A DIFFERENT hostname than the console's, with a certificate that covers it.
  advertiseURL: https://wardyn-ui.example.com
  # Optional, strongly preferred: one origin per run (wildcard DNS + wildcard
  # certificate). Unset, every run's apps share one origin, separated only by
  # a path-scoped cookie — published residual #18 in the threat model.
  originTemplate: https://run-{run}.ui.example.com
```

Route it with an Ingress (or its own Service) against the Deployment's named
`ui` containerPort, the same shape as the "Split SSH exposure" recipe above:

```yaml
apiVersion: networking.k8s.io/v1
kind: Ingress
metadata:
  name: wardyn-ui-sandbox
spec:
  rules:
    # Wildcard host: what originTemplate needs. Drop to a single host only if
    # you are accepting the shared-origin residual.
    - host: "*.ui.example.com"
      http:
        paths:
          - path: /
            pathType: Prefix
            backend:
              service:
                name: wardyn
                port:
                  name: ui
  tls:
    - hosts: ["*.ui.example.com"]
      secretName: wardyn-ui-sandbox-tls
```

The gateway relays WebSockets (a browser IDE needs them), so an ingress
controller in front of it must not buffer or strip the `101` upgrade.

Runs still have to declare `ui_apps` in policy
([docs/POLICIES.md](../../../docs/POLICIES.md)) and run an image that ships the
matching `/usr/local/bin/wardyn-ui-<name>` launcher — enabling the gateway on
its own opens nothing.

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
  literal in the pod spec — **not for secrets**. `WARDYN_DEFAULT_POLICY` is
  optional — the image already bakes a working default; see
  [Installation](#installation) for that default and the crash-loop caveat on
  images built before it was baked.
- `extraEnv`: raw `EnvVar` entries (so `valueFrom.secretKeyRef` works) for the
  secret-bearing variables docs/ENV.md marks 🔒: `WARDYN_OIDC_CLIENT_SECRET`,
  and `WARDYN_AUDIT_SINKS` (its JSON carries the SIEM
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
- `k8s.*`: the Kubernetes runner substrate, off by default — see
  [Kubernetes runner substrate](#kubernetes-runner-substrate-k8senabled) above.
- `ssh.*`: SSH access into a running sandbox, off by default — see
  [Split SSH exposure](#split-ssh-exposure) above.
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
