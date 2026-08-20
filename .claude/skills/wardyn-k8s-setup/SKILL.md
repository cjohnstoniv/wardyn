---
name: wardyn-k8s-setup
description: Set up or troubleshoot Wardyn's Kubernetes runner substrate (the Helm chart with k8s.enabled=true) — cluster prereqs (proving the CNI enforces NetworkPolicy), authoring values.yaml, wiring Entra ID App Roles for admin/member RBAC, install/upgrade, and verification. Use when deploying Wardyn to Kubernetes, enabling k8s.enabled, configuring SSO/RBAC on a k8s install, or troubleshooting one (redirect loops, "no Wardyn role assigned", ImagePullBackOff, canary INDETERMINATE, NetworkPolicy not enforcing, SSH connection refused). Triggers on "deploy wardyn to kubernetes", "wardyn k8s setup", "k8s runner substrate", "helm install wardyn", "k8s.enabled", "wardyn Entra SSO", "NetworkPolicy canary", "wardyn RBAC on k8s".
---

# Wardyn k8s setup — cluster prereqs, values, Entra RBAC, install, verify

Goal: get `wardynd` running on Kubernetes with the `k8s` runner substrate
actually able to create confined sandboxes, and admin/member RBAC wired
through a real IdP (Entra ID is the worked example — the trap it has that a
generic OIDC provider doesn't is exactly why it gets its own section below).
Reuse the shipped chart and substrate; never hand-roll a manifest.

## Ground truth (read these, don't restate from memory)

- Chart defaults + every value's own doc comment: `deploy/helm/wardyn/values.yaml`.
- Worked install commands, the `k8s.enabled` walkthrough, and the **honest
  gap list** (what the k8s substrate does not do yet — BYOI, `local_dir`
  mounts, per-pod PIDs/disk limits, ground-truth correlator): `deploy/helm/wardyn/README.md`.
- Every `WARDYN_*` var this touches, with defaults and gotchas spelled out
  per row (`WARDYN_K8S_*`, `WARDYN_OIDC_*`, `WARDYN_SSH_*`, `WARDYN_TLS_TERMINATED`): `docs/ENV.md`.
- Multi-user semantics (admin vs member, ownership scoping, the shared
  admin-token ceiling) and the k8s known-gaps detail behind the chart
  README's summary: `docs/OPERATIONS.md`.
- SSH gateway setup/use once the cluster is up (owner-only today — no
  operator override; see this skill's troubleshooting table): `docs/SSH.md`.

## Recipe

1. **Cluster prereqs — prove the CNI enforces NetworkPolicy before anything
   else.** The k8s substrate's boot-time egress canary
   (`internal/runner/k8s/canary.go`) runs a two-phase check against the
   cluster itself the moment `wardynd` starts with `k8s.enabled` — a
   throwaway pod first confirms it CAN reach the apiserver with no policy in
   place, then a deny-all `NetworkPolicy` is applied and the same pod is
   proven UNREACHABLE. If phase two still gets through, the CNI does not
   enforce `NetworkPolicy` and **wardynd refuses to boot the substrate** —
   this is fail-closed by design, not a bug to work around. Prove it on a
   throwaway cluster before touching a real one, using the exact recipe CI
   uses (`.github/workflows/ci.yml`'s `conformance-k8s` job,
   `deploy/kind/conformance-kind-config.yaml`): kind's bundled `kindnet` CNI
   does **not** enforce `NetworkPolicy`, so start kind with
   `networking.disableDefaultCNI: true` and install a real CNI yourself —
   Calico is what CI pins:
   ```sh
   kind create cluster --config deploy/kind/conformance-kind-config.yaml
   kubectl apply -f https://raw.githubusercontent.com/projectcalico/calico/v3.28.0/manifests/calico.yaml
   kubectl -n kube-system rollout status daemonset/calico-node --timeout=180s
   ```
   Any NetworkPolicy-enforcing CNI works the same way on a real cluster
   (Calico, Cilium, etc.) — this is just the reproducible local proof.
   **The opt-out and its cost**: `WARDYN_K8S_ALLOW_UNENFORCED_NETPOL=1`
   (helm: `env.WARDYN_K8S_ALLOW_UNENFORCED_NETPOL`) lets wardynd boot anyway
   on a CNI that doesn't enforce policy — logged loudly at construction, and
   **every sandbox this substrate creates then has UNCONFINED egress**. Only
   ever accept that on a cluster you have some other confinement story for;
   it is never an invisible downgrade (`ClassSupport.NetworkPolicy`/
   `StructuralEgress` both keep reporting `false`, so `/healthz` never reads
   like a genuinely confined install).

2. **Author `values.yaml`.**
   - **Runner**: `k8s.enabled=true` and `serviceAccount.automount=true` — the
     chart REFUSES to render `k8s.enabled` without the automount flip (the
     substrate drives the apiserver directly via client-go, which needs the
     pod's own projected token).
   - **Images**: `k8s.proxyImage` (required in practice — empty boots
     wardynd fine but every run then fails closed; also what the egress
     canary itself launches) and `image.repository`/`image.tag` for wardynd.
   - **`runtimeClasses`**: `k8s.runtimeClasses.CC2`/`.CC3` pin a Confinement
     Class to a RuntimeClass NAME already registered in the cluster (e.g.
     `CC2: gvisor`) — unlike Docker's well-known runtime family names, a
     RuntimeClass object name is operator-chosen, so CC2/CC3 stay
     unadvertised (CC1-only) until pinned here.
   - **SSO block**: `env.WARDYN_OIDC_ISSUER`, `env.WARDYN_OIDC_INTERNAL_ISSUER`
     (only if the browser and wardynd reach the IdP at different hostnames),
     and `extraEnv` for `WARDYN_OIDC_CLIENT_SECRET` (🔒 in docs/ENV.md — never
     a literal `env` value). RBAC itself is `env.WARDYN_OIDC_ROLE_MAP` — see
     step 3.
   - **SSH block**: `ssh.enabled`, `ssh.port` (default `2222`), and
     `ssh.advertiseHost` (required when enabled — the chart never guesses a
     public address). See [Split SSH exposure](../../../deploy/helm/wardyn/README.md#split-ssh-exposure)
     if SSH needs different exposure than HTTP (its own `LoadBalancer` while
     HTTP stays `ClusterIP`, for example).

3. **Entra ID app registration — admin/member RBAC.** Wardyn has real
   `admin`/`member` roles (`internal/auth/oidc`'s `deriveRole`), and Entra
   App Roles are the recommended way to feed them — do this as its own step,
   not folded into general SSO setup, because Entra has a trap a generic OIDC
   provider doesn't (below).
   - **Register the App Roles** on the app registration (Azure Portal →
     App registrations → your app → **App roles** → Create app role, or via
     the manifest editor):
     ```json
     {
       "appRoles": [
         {
           "id": "3b1f6e2a-...-generate-a-fresh-guid",
           "allowedMemberTypes": ["User"],
           "displayName": "Wardyn Admin",
           "value": "Wardyn.Admin",
           "description": "Full Wardyn control-plane access",
           "isEnabled": true
         },
         {
           "id": "9c2d7f4b-...-generate-a-fresh-guid",
           "allowedMemberTypes": ["User"],
           "displayName": "Wardyn Member",
           "value": "Wardyn.Member",
           "description": "Launch and use Wardyn runs",
           "isEnabled": true
         }
       ]
     }
     ```
     (the `appRoles` key merges into the app's existing manifest — the
     snippet above is deliberately just that one key, not a full manifest
     replacement)
     `value` is what lands in the ID token's `roles` claim — that string is
     what `WARDYN_OIDC_ROLE_MAP` matches against, not `displayName`.
   - **Set "Assignment required?" to Yes** on the Enterprise Application
     (Azure Portal → Enterprise applications → your app → Properties). Without
     this, Entra hands out the app to any tenant user with NO role assigned —
     the token carries an empty `roles` claim, and every such login falls
     through to `WARDYN_OIDC_DEFAULT_ROLE` (or is denied if that's unset). Turn
     it on, then assign each role to the right users/groups under **Users and
     groups**.
   - **The role-map values** — `env.WARDYN_OIDC_ROLE_MAP` maps the App Role's
     `value` (or a `groups` entry, or an email) to a Wardyn role:
     ```yaml
     env:
       WARDYN_OIDC_ROLE_MAP: "Wardyn.Admin=admin,Wardyn.Member=member"
       # Optional: no match at all -> this role instead of denying the login.
       WARDYN_OIDC_DEFAULT_ROLE: "member"
     ```
     Matching is case-insensitive and ASCII-only; any `admin` match wins over
     a `member` match regardless of which claim produced it; an email on
     `WARDYN_OIDC_OPERATOR_EMAILS` is an ADDITIONAL admin match (see step 3's
     legacy note below). Leaving `WARDYN_OIDC_ROLE_MAP` unset disables role
     derivation entirely — every signed-in human is admin, upgrade-safe but
     not RBAC.
   - **The `email_verified` trap**: Entra ID tokens typically **omit
     `email_verified` entirely**. `WARDYN_OIDC_EMAIL_DOMAINS` (domain-restricted
     login) fails closed on that — it denies **every** login against an Entra
     tenant, since it requires a verified email that never arrives. Do not
     use `WARDYN_OIDC_EMAIL_DOMAINS` with Entra; App Roles plus "assignment
     required" is the tenant-scoping mechanism instead — only assigned users
     ever reach a role, and everyone else denies via `WARDYN_OIDC_DEFAULT_ROLE`
     (or the same fall-through denial with it unset).
   - **Legacy allowlist still works**: `WARDYN_OIDC_OPERATOR_EMAILS` (the
     pre-0.5 operator list) is not replaced by `WARDYN_OIDC_ROLE_MAP` — an
     email on it is still an additional `admin` match, so an existing
     deployment adopting App Roles keeps its current operators as admins with
     zero re-configuration.

4. **Install / upgrade.**
   ```sh
   kubectl create secret generic wardyn-auth -n wardyn \
     --from-literal=admin-token="$(openssl rand -hex 32)"   # fallback if OIDC misconfigures

   helm upgrade --install wardyn ./deploy/helm/wardyn \
     --namespace wardyn --create-namespace \
     --set postgres.dsn.secretRef.name=wardyn-pg \
     --set auth.adminToken.secretRef.name=wardyn-auth \
     --set serviceAccount.automount=true \
     --set k8s.enabled=true \
     --set k8s.proxyImage="$REGISTRY/wardyn-proxy:$TAG" \
     -f my-k8s-values.yaml   # the SSO/SSH/runtimeClasses block from step 2-3
   ```
   Keep an admin token secret configured even with OIDC set up — it is the
   recovery path if the Entra role map ever locks everyone out.

5. **Verify.**
   - `kubectl -n wardyn rollout status deploy/wardyn --timeout=120s` — not
     `ImagePullBackOff` (see the troubleshooting table).
   - Console → setup checks: `k8s_egress_containment` reads **Enforcing ·
     NetworkPolicy**, not Indeterminate or Not enforcing (see step 1).
   - Launch a run through the console or `wardyn run`, confirm it reaches
     `RUNNING`, and attach (web terminal, or SSH if `ssh.enabled` — `ssh
     <run-id>@<advertiseHost> -p <port>`, owner-only, docs/SSH.md).
   - Sign in via SSO with a member-mapped account and confirm the console
     shows member-scoped nav (no policy/workspace/secret CRUD, no BYOI); sign
     in with an admin-mapped account and confirm the full console.

## Troubleshooting

| Symptom | Cause | Fix |
|---|---|---|
| Login redirect loop (bounces back to the IdP forever) | Secure-flagged cookies over plain HTTP — `WARDYN_TLS_TERMINATED=true` set while the browser is NOT actually on HTTPS (or the reverse: real TLS termination in front but the flag left unset can also confuse a strict ingress). A `Secure` cookie is silently dropped by the browser off HTTPS, so the OIDC state/nonce/PKCE cookies never survive the round trip. | Set `env.WARDYN_TLS_TERMINATED=true` (helm) only when an ingress/LB genuinely terminates TLS in front of wardynd (browser sees `https://`); leave it unset for plain-HTTP access (port-forward, no ingress TLS). |
| "no Wardyn role assigned; ask your operator to map you via WARDYN_OIDC_ROLE_MAP" | `WARDYN_OIDC_ROLE_MAP` is set, the signed-in user's `roles`/`groups`/email matched none of its entries, and `WARDYN_OIDC_DEFAULT_ROLE` is unset (fail-closed default: deny). | Either assign the user an Entra App Role (step 3) and confirm "Assignment required" didn't block them, or set `env.WARDYN_OIDC_DEFAULT_ROLE=member` to admit unmatched users as members instead of denying. |
| wardynd won't start / crash-loops, logs "parse WARDYN_OIDC_ROLE_MAP" or "invalid WARDYN_OIDC_DEFAULT_ROLE" | Both are validated at **boot**, not first use: a malformed role-map entry (bad role value, non-ASCII key, duplicate key, or non-blank input with no valid entry) or a `WARDYN_OIDC_DEFAULT_ROLE` that isn't `admin`/`member` fails boot outright rather than reaching a session cookie later. | Fix the `env.WARDYN_OIDC_ROLE_MAP` CSV syntax (`value=admin` or `value=member` pairs only, ASCII keys, no duplicates) or `env.WARDYN_OIDC_DEFAULT_ROLE` value named in the boot log, then redeploy. |
| `id_token verification failed` / issuer mismatch | `env.WARDYN_OIDC_ISSUER` doesn't byte-match the token's `iss` claim — common with Entra: v1 vs v2 endpoint, or the wrong tenant segment in the URL. | Use Entra's v2 issuer exactly: `https://login.microsoftonline.com/<tenant-id>/v2.0`. If wardynd and the browser reach the IdP at different hostnames, set `env.WARDYN_OIDC_INTERNAL_ISSUER` for wardynd's own server-side calls and leave `WARDYN_OIDC_ISSUER` as the public one. |
| `ImagePullBackOff` | `image.repository`/`image.tag` (or `k8s.proxyImage`) point at a tag that was never pushed, or the cluster can't reach the registry / lacks a pull secret. | Confirm the tag exists in the registry the cluster can reach; for a private registry pass `image.pullSecrets`/`k8s.imagePullSecret`. A wrong image still renders and installs fine — this is a **runtime** symptom, always check `kubectl rollout status`, never assume render success means a working image. |
| Setup check `k8s_egress_containment` reads **Indeterminate** | wardynd reports a `k8s` driver but no canary verdict — either a build too old to compute one, or `setupRunnerInfo`'s own `Capabilities()` call errored before the canary ran. | Upgrade wardynd to a build that reports the canary verdict, and check wardynd's boot logs for the egress-canary result directly. |
| wardynd refuses to boot: `egress canary phase A (no NetworkPolicy) did not confirm baseline apiserver reachability` (W27-S1-6) | Phase A applies no NetworkPolicy of its own — it only proves the cluster is reachable at all. This is indistinguishable from `k8s.runsNamespace` already carrying a default-deny NetworkPolicy from something ELSE (a cluster-wide baseline, another operator's policy), which blocks the canary pod too. | Use a namespace with no ambient default-deny for `k8s.runsNamespace`, or add an allow rule for pods labeled `wardyn.managed=true` to the pre-existing policy. |
| Setup check `k8s_egress_containment` reads **Not enforcing** | The boot-time canary proved this cluster's CNI does not enforce `NetworkPolicy`, and the operator accepted that via `WARDYN_K8S_ALLOW_UNENFORCED_NETPOL=1` — every sandbox has unconfined egress. | Unset `env.WARDYN_K8S_ALLOW_UNENFORCED_NETPOL` and install a NetworkPolicy-enforcing CNI (step 1) to restore real confinement. |
| Runner never reaches `RUNNING` / pods stuck `Pending` | Often `k8s.runtimeClasses.CC2`/`.CC3` names a RuntimeClass that doesn't exist in the cluster yet, or the namespace lacks the Pod Security Standard level the substrate's pods need. | Confirm `kubectl get runtimeclass` lists the name you pinned; confirm the runs namespace isn't blocking the substrate's restricted `securityContext` (PSS `restricted` is what CI's own conformance namespace uses). |
| wardynd crash-loops at boot with `unknown -runner "k8s" (want "none" or a registered substrate; the docker substrate requires a wardynd built with -tags docker)` | Misleading pre-fix headline (W27-S1-3) — ignore the `-tags docker` framing, it never applies here. The `k8s` substrate IS registered; its CONSTRUCTOR refused to start, most often the boot-time egress canary (`k8s: refusing to boot: ...`) or a missing `k8s.proxyImage`. | Read past the headline to the wrapped `-runner "k8s" failed to start: ...` cause; check the canary verdict (`k8s_egress_containment` rows above) and confirm `k8s.proxyImage` is set. |
| SSH connection refused | Either the gateway was never turned on (`WARDYN_SSH_LISTEN` unset — `ssh.enabled=false` is the chart's own default, and NOTHING generates a host key until it's on), or a client is dialing the wrong port/Service. | Set `ssh.enabled=true` plus `ssh.advertiseHost`; confirm `/healthz`'s `ssh.enabled` reads `true`; if SSH is split onto its own Service (LoadBalancer, etc. — see the chart README), confirm that Service's `targetPort` is `ssh`, matching the Deployment's named containerPort. |
