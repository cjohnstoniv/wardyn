---
name: wardyn-k8s-setup
description: Set up or troubleshoot Wardyn's Kubernetes runner substrate (the Helm chart with k8s.enabled=true) — cluster prereqs (proving the CNI enforces NetworkPolicy), authoring values.yaml, wiring Entra ID App Roles for admin/member RBAC, install/upgrade, and verification. Use when deploying Wardyn to Kubernetes, enabling k8s.enabled, configuring SSO/RBAC on a k8s install, or troubleshooting one (redirect loops, "no Wardyn role assigned", ImagePullBackOff, canary INDETERMINATE, NetworkPolicy not enforcing, SSH connection refused). Triggers on "deploy wardyn to kubernetes", "wardyn k8s setup", "k8s runner substrate", "helm install wardyn", "k8s.enabled", "wardyn Entra SSO", "NetworkPolicy canary", "wardyn RBAC on k8s".
---

# Wardyn k8s setup — cluster prereqs, values, Entra RBAC, install, verify

Goal: get `wardynd` running on Kubernetes with the `k8s` runner substrate
creating confined sandboxes, and admin/member RBAC wired through a real IdP
(Entra ID is the worked example — its traps are why it gets its own step
below). Reuse the shipped chart and substrate; never hand-roll a manifest.

This skill is a dispatcher: it asks the input questions, then sends every
setting to the doc that owns it — don't restate a default from memory.

## Inputs to gather first

- The image reference you'll push and pin (`$REGISTRY/wardynd:$TAG` and the
  matching `wardyn-proxy` tag — build recipe: [chart README, Build and push
  wardynd](../../../deploy/helm/wardyn/README.md#build-and-push-wardynd)).
- Your IdP tenant and app registration; whether the browser and wardynd reach
  the IdP at the same hostname.
- The RuntimeClass names registered in the cluster (if CC2/CC3 pin to one);
  whether SSH into runs is wanted, and the host agents dial; a runs namespace
  with no ambient default-deny NetworkPolicy.

## Ground truth (read these, don't restate from memory)

- Chart defaults + every value's own doc comment:
  [`deploy/helm/wardyn/values.yaml`](../../../deploy/helm/wardyn/values.yaml).
- Worked install commands, the `k8s.enabled` walkthrough and the **honest gap
  list** ([chart README](../../../deploy/helm/wardyn/README.md) — start at
  [Kubernetes runner
  substrate](../../../deploy/helm/wardyn/README.md#kubernetes-runner-substrate-k8senabled),
  gaps in [Known
  gaps](../../../deploy/helm/wardyn/README.md#known-gaps)).
- Every `WARDYN_*` var this touches, defaults and gotchas per row
  (`WARDYN_K8S_*`, `WARDYN_OIDC_*`, `WARDYN_SSH_*`, `WARDYN_TLS_TERMINATED`):
  [docs/ENV.md](../../../docs/ENV.md).
- Multi-user semantics (admin vs member, ownership scoping, the shared
  admin-token ceiling) and the k8s known-gaps detail behind the chart README's
  summary: [docs/OPERATIONS.md](../../../docs/OPERATIONS.md).
- SSH gateway use once the cluster is up (the run's **owner**, or an admin on
  a run with no personal owner — the admin override re-checks the key's role
  every `WARDYN_SSH_ROLE_TTL`; see the troubleshooting table below):
  [docs/SSH.md](../../../docs/SSH.md).

## Recipe

1. **Cluster prereqs — prove the CNI enforces NetworkPolicy before anything
   else.** Platform requirements: [chart README,
   Prerequisites](../../../deploy/helm/wardyn/README.md#prerequisites). The
   substrate's boot-time egress canary (`internal/runner/k8s/canary.go`)
   proves it live — a throwaway pod must reach the apiserver with no policy,
   then be proven UNREACHABLE behind a deny-all `NetworkPolicy`; phase two
   getting through means **wardynd refuses to boot the substrate**,
   fail-closed by design, not a bug to work around. Prove it on a throwaway
   cluster first, using the exact recipe CI uses
   (`.github/workflows/ci.yml`'s `conformance-k8s` job,
   `deploy/kind/conformance-kind-config.yaml`): kind's bundled `kindnet` CNI
   does **not** enforce `NetworkPolicy`, so start kind with
   `networking.disableDefaultCNI: true` and install a real CNI yourself —
   Follow the CNI installation and rollout commands in the
   [`conformance-k8s` job](../../../.github/workflows/ci.yml), which owns the
   pinned version and timeout. Any enforcing CNI works on a real cluster.
   **The opt-out and its cost**: `WARDYN_K8S_ALLOW_UNENFORCED_NETPOL=1`
   (helm: `env.WARDYN_K8S_ALLOW_UNENFORCED_NETPOL`) lets wardynd boot anyway
   on a CNI that doesn't enforce policy — logged loudly at construction, and
   **every sandbox this substrate creates then has UNCONFINED egress**. Only
   ever accept that on a cluster you have some other confinement story for;
   it is never an invisible downgrade (`ClassSupport.NetworkPolicy`/
   `StructuralEgress` both keep reporting `false`, so `/healthz` never reads
   like a genuinely confined install). The ambient-default-deny ack is a
   different, narrower switch ([docs/ENV.md](../../../docs/ENV.md#wardyn_k8s_allow_unenforced_netpol),
   [its own
   row](../../../docs/ENV.md#wardyn_k8s_ack_ambient_default_deny)).

2. **Author `values.yaml`.** Every value below is owned and documented in
   the chart — link through, don't guess:
   - **Runner**: `k8s.enabled=true` + `serviceAccount.automount=true` (the
     chart refuses to render without the automount flip), `k8s.runsNamespace`
     (must pre-exist), `k8s.proxyImage` (required — also what the egress
     canary launches). Semantics: [chart README, Kubernetes runner
     substrate](../../../deploy/helm/wardyn/README.md#kubernetes-runner-substrate-k8senabled).
   - **Images**: `image.repository`/`image.tag` for wardynd — [chart README,
     Values](../../../deploy/helm/wardyn/README.md#values).
   - **`runtimeClasses`**: pin `k8s.runtimeClasses.CC2`/`.CC3` to RuntimeClass
     NAMEs from your input list (operator-chosen, so CC1-only until pinned)
     — [same section](../../../deploy/helm/wardyn/README.md#kubernetes-runner-substrate-k8senabled).
   - **SSO block**: `env.WARDYN_OIDC_ISSUER`,
     `env.WARDYN_OIDC_INTERNAL_ISSUER` (only if the browser and wardynd
     reach the IdP at different hostnames), and `extraEnv` for
     `WARDYN_OIDC_CLIENT_SECRET` (🔒 in docs/ENV.md — never a literal `env`
     value). RBAC itself is `env.WARDYN_OIDC_ROLE_MAP` — see step 3. Owner:
     [chart README, Multi-user](../../../deploy/helm/wardyn/README.md#multi-user-adminmember-rbac).
   - **SSH block**: `ssh.enabled`, `ssh.port`, `ssh.advertiseHost` (required
     when enabled — the chart never guesses a public address): [chart README,
     Values](../../../deploy/helm/wardyn/README.md#values). See [chart README,
     Split SSH
     exposure](../../../deploy/helm/wardyn/README.md#split-ssh-exposure) if
     SSH needs different exposure than HTTP.

3. **Entra ID app registration — admin/member RBAC.** Wardyn has real
   `admin`/`user` roles (`internal/auth/oidc`'s `deriveRole`), and Entra
   App Roles are the recommended way to feed them — a step of its own
   because Entra has traps a generic OIDC provider doesn't (below).
   - **Register the App Roles** on the app registration (Azure Portal →
     App registrations → your app → **App roles** → Create app role, or via
     the manifest editor):
     ```json
     {"appRoles": [
       {"id": "3b1f6e2a-...-generate-a-fresh-guid", "allowedMemberTypes": ["User"],
        "displayName": "Wardyn Admin", "value": "Wardyn.Admin",
        "description": "Full Wardyn control-plane access", "isEnabled": true},
       {"id": "9c2d7f4b-...-generate-a-fresh-guid", "allowedMemberTypes": ["User"],
        "displayName": "Wardyn Member", "value": "Wardyn.Member",
        "description": "Launch and use Wardyn runs", "isEnabled": true}
     ]}
     ```
     (the `appRoles` key merges into the app's existing manifest — the
     snippet above is deliberately just that one key, not a full manifest
     replacement)
     `value` is what lands in the ID token's `roles` claim — what
     `WARDYN_OIDC_ROLE_MAP` matches against, not `displayName`.
   - **Set "Assignment required?" to Yes** on the Enterprise Application
     (Azure Portal → Enterprise applications → your app → Properties). Without
     it, Entra hands the app to any tenant user with NO role assigned — an
     empty `roles` claim, every such login falling through to
     `WARDYN_OIDC_DEFAULT_ROLE` (or denied if that's unset). Then assign each
     role under **Users and groups**.
   - **The role map** — `env.WARDYN_OIDC_ROLE_MAP` maps an App Role `value`
     (or a `groups` entry, or an email) to a Wardyn role. Full semantics
     (CSV shape, case-insensitive ASCII matching, `admin` wins over `user`,
     `security_admin` reachable only through the map, deny-by-default
     fallthrough, boot-time validation): [docs/OPERATIONS.md, "Who decides
     who gets in"](../../../docs/OPERATIONS.md#who-decides-who-gets-in-chart-vs-console-vs-idp)
     and [docs/ENV.md](../../../docs/ENV.md#wardyn_oidc_role_map); a minimal
     values snippet lives in [the chart
     README](../../../deploy/helm/wardyn/README.md#multi-user-adminmember-rbac).
   - **Legacy operator allowlist**: existing `WARDYN_OIDC_OPERATOR_EMAILS`
     entries still work; see [docs/ENV.md](../../../docs/ENV.md#wardyn_oidc_operator_emails).
   - **The `email_verified` trap**: Entra ID tokens typically **omit
     `email_verified` entirely**. `WARDYN_OIDC_EMAIL_DOMAINS` (domain-
     restricted login) fails closed on that — it denies **every** login
     against an Entra tenant. Do not use `WARDYN_OIDC_EMAIL_DOMAINS` with
     Entra; App Roles plus "assignment required" is the tenant-scoping
     mechanism instead.
   - **Groups claims don't nest**: a role or group assigned to a group
     reaches its direct members only — nested memberships never arrive
     ([docs/OPERATIONS.md,
     "Capabilities"](../../../docs/OPERATIONS.md#capabilities-what-one-member-or-one-group-may-do)).
   - **`env.WARDYN_OIDC_ROLE_MAP` is the chart's BOOTSTRAP layer, not the only
     place mappings get edited** — once the install is up, an admin edits
     them live from the console's People step, no redeploy (merged-table
     semantics: the link above). A scripted, worked validation of this path
     against a real Entra tenant lives in `deploy/azure-entra-sso/` — run
     it before trusting any of this on a tenant that matters.

4. **Install / upgrade.**
   ```sh
   kubectl create namespace wardyn
   kubectl create namespace wardyn-runs   # k8s.runsNamespace — must pre-exist

   kubectl create secret generic wardyn-auth -n wardyn \
     --from-literal=admin-token="$(openssl rand -hex 32)"   # fallback if OIDC misconfigures
   kubectl create secret generic wardyn-pg -n wardyn \
     --from-literal=dsn="$PG_DSN" \
     --from-literal=age-key="$(docker run --rm "$REGISTRY/wardynd:$TAG" -gen-age-key)"

   helm upgrade --install wardyn ./deploy/helm/wardyn \
     --namespace wardyn \
     --set postgres.dsn.secretRef.name=wardyn-pg \
     --set secrets.ageKeyFromSecret=true \
     --set auth.adminToken.secretRef.name=wardyn-auth \
     --set serviceAccount.automount=true \
     --set k8s.enabled=true \
     --set k8s.runsNamespace=wardyn-runs \
     --set k8s.proxyImage="$REGISTRY/wardyn-proxy:$TAG" \
     --set k8s.runtimeClasses.CC2="<your gVisor RuntimeClass>" \
     -f my-k8s-values.yaml   # the SSO/SSH/runtimeClasses block from step 2-3
   ```
   `postgres.dsn.secretRef` is a PERSISTENT DSN, so the age identity riding in
   the SAME Secret (`age-key`, via `-gen-age-key`) is required — the chart
   refuses to render without it, and without it wardynd generates a fresh
   identity every boot and cannot decrypt what the previous boot encrypted
   (crash-loops on the SECOND restart). Keep an admin token secret configured
   even with OIDC set up — it is the recovery path if the Entra role map ever
   locks everyone out. Owners: [chart README,
   Values](../../../deploy/helm/wardyn/README.md#values). The CC2 pin above
   is optional hardening: the shipped policy floor is CC1, so a stock
   install renders and runs with no pin; a stronger floor with no matching
   RuntimeClass fails closed at dispatch.

5. **Verify.**
   - `kubectl -n wardyn rollout status deploy/wardyn --timeout=120s` — not
     `ImagePullBackOff` (see the troubleshooting table).
   - Console → setup checks: `k8s_egress_containment` reads **Enforcing ·
     NetworkPolicy**, not Indeterminate or Not enforcing (see step 1).
   - Launch a run through the console or `wardyn run`, confirm it reaches
     `RUNNING`, and attach (web terminal, or SSH if `ssh.enabled` — `ssh
     <run-id>@<advertiseHost> -p <port>`, owner or admin, docs/SSH.md).
   - Sign in via SSO with a member-mapped account and confirm the console
     shows member-scoped nav (no policy/workspace/secret CRUD, no BYOI); sign
     in with an admin-mapped account and confirm the full console.

## Troubleshooting

| Symptom | Cause | Fix |
|---|---|---|
| Login redirect loop (bounces back to the IdP forever) | Secure-flagged cookies over plain HTTP — `WARDYN_TLS_TERMINATED` set while the browser is NOT actually on HTTPS (or the reverse: real TLS termination in front but the flag left unset can also confuse a strict ingress). A `Secure` cookie is silently dropped off HTTPS, so the OIDC state/nonce/PKCE cookies never survive the round trip. | Set `env.WARDYN_TLS_TERMINATED=true` (helm) only when an ingress/LB genuinely terminates TLS in front of wardynd (browser sees `https://`); leave it unset for plain-HTTP access (port-forward, no ingress TLS). |
| "no Wardyn role assigned; ask your operator to map you via WARDYN_OIDC_ROLE_MAP" | `WARDYN_OIDC_ROLE_MAP` is set, the signed-in user's `roles`/`groups`/email matched none of its entries, and `WARDYN_OIDC_DEFAULT_ROLE` is unset (fail-closed default: deny — [ENV.md](../../../docs/ENV.md#wardyn_oidc_default_role)). | Either assign the user an Entra App Role (step 3) and confirm "Assignment required" didn't block them, or set `env.WARDYN_OIDC_DEFAULT_ROLE=user` to admit unmatched users with the `user` role instead of denying. |
| wardynd won't start / crash-loops, logs "parse WARDYN_OIDC_ROLE_MAP" or "invalid WARDYN_OIDC_DEFAULT_ROLE" | Both are validated at **boot**, not first use — a malformed role-map entry or a `WARDYN_OIDC_DEFAULT_ROLE` that isn't `admin`/`user` fails boot outright rather than reaching a session cookie later (semantics: [docs/OPERATIONS.md, "Who decides who gets in"](../../../docs/OPERATIONS.md#who-decides-who-gets-in-chart-vs-console-vs-idp)). | Fix the `env.WARDYN_OIDC_ROLE_MAP` CSV syntax or `env.WARDYN_OIDC_DEFAULT_ROLE` value named in the boot log, then redeploy. |
| `id_token verification failed` / issuer mismatch | `env.WARDYN_OIDC_ISSUER` doesn't byte-match the token's `iss` claim — common with Entra: v1 vs v2 endpoint, or the wrong tenant segment in the URL. | Use Entra's v2 issuer exactly: `https://login.microsoftonline.com/<tenant-id>/v2.0`. If wardynd and the browser reach the IdP at different hostnames, set `env.WARDYN_OIDC_INTERNAL_ISSUER` for wardynd's own server-side calls and leave `WARDYN_OIDC_ISSUER` as the public one. |
| `ImagePullBackOff` | `image.repository`/`image.tag` (or `k8s.proxyImage`) point at a tag that was never pushed, or the cluster can't reach the registry / lacks a pull secret. | Confirm the tag exists in the registry the cluster can reach; for a private registry pass `image.pullSecrets`/`k8s.imagePullSecret` ([chart README, Values](../../../deploy/helm/wardyn/README.md#values)). A wrong image still renders and installs fine — this is a **runtime** symptom, always check `kubectl rollout status`, never assume render success means a working image. |
| Setup check `k8s_egress_containment` reads **Indeterminate** | wardynd reports a `k8s` driver but no canary verdict — either a build too old to compute one, or `setupRunnerInfo`'s own `Capabilities()` call errored before the canary ran. | Upgrade wardynd to a build that reports the canary verdict, and check wardynd's boot logs for the egress-canary result directly. |
| wardynd refuses to boot: `egress canary phase A (no NetworkPolicy) did not confirm baseline apiserver reachability` (W27-S1-6) | Phase A applies no NetworkPolicy of its own — it only proves the cluster is reachable at all. This is indistinguishable from `k8s.runsNamespace` already carrying a default-deny NetworkPolicy from something ELSE (a cluster-wide baseline, another operator's policy), which blocks the canary pod too. | Use a namespace with no ambient default-deny for `k8s.runsNamespace`, or exempt Wardyn's pods from **that policy's own `podSelector`** (a `matchExpressions` entry with `key: wardyn.managed`, `operator: NotIn`, `values: ["true"]`) so it stops selecting them. **Do NOT add a separate allow policy for `wardyn.managed=true`** — allows are additive and both the agent and proxy pods carry that label, so it would widen every sandbox pod's egress and flip the canary's phase B to "not enforcing". |
| Setup check `k8s_egress_containment` reads **Not enforcing** | The boot-time canary proved this cluster's CNI does not enforce `NetworkPolicy`, and the operator accepted that via `WARDYN_K8S_ALLOW_UNENFORCED_NETPOL=1` — every sandbox has unconfined egress. | Unset `env.WARDYN_K8S_ALLOW_UNENFORCED_NETPOL` and install a NetworkPolicy-enforcing CNI (step 1) to restore real confinement. |
| Runner never reaches `RUNNING` / pods stuck `Pending` | Often `k8s.runtimeClasses.CC2`/`.CC3` names a RuntimeClass that doesn't exist in the cluster yet, or the namespace lacks the Pod Security Standard level the substrate's pods need. | Confirm `kubectl get runtimeclass` lists the name you pinned; confirm the runs namespace isn't blocking the substrate's restricted `securityContext` (PSS `restricted` is what CI's own conformance namespace uses — [chart README, Kubernetes runner substrate](../../../deploy/helm/wardyn/README.md#kubernetes-runner-substrate-k8senabled)). |
| wardynd crash-loops at boot with `unknown -runner "k8s" (want "none" or a registered substrate; the docker substrate requires a wardynd built with -tags docker)` | Misleading pre-fix headline (W27-S1-3) — ignore the `-tags docker` framing, it never applies here. The `k8s` substrate IS registered; its CONSTRUCTOR refused to start, most often the boot-time egress canary (`k8s: refusing to boot: ...`) or, on a non-chart install, a missing `k8s.proxyImage` (the chart refuses to render without one). | Read past the headline to the wrapped `-runner "k8s" failed to start: ...` cause; check the canary verdict (`k8s_egress_containment` rows above) and, on a non-chart install, confirm `k8s.proxyImage` is set. |
| SSH connection refused | Either the gateway was never turned on (`WARDYN_SSH_LISTEN` unset — nothing generates a host key until it's on; see [chart README, Values](../../../deploy/helm/wardyn/README.md#values)), or a client is dialing the wrong port/Service. | Set `ssh.enabled=true` plus `ssh.advertiseHost`; confirm `/healthz`'s `ssh.enabled` reads `true`; if SSH is split onto its own Service (LoadBalancer, etc. — see the chart README), confirm that Service's `targetPort` is `ssh`, matching the Deployment's named containerPort. |
| Role mapping added on the People step, but the user's access didn't change | Role derivation runs once, at login, and is stamped into the session cookie — a console row change is never applied to an already-signed-in session. | Tell the person to sign out and back in. Their next login re-derives the role against the now-current merged map. |
| Sign-in redirects with **"your sign-in is too old to verify this change"** while adding/removing a People-step mapping | The acting admin's own session snapshot (`groups`) is nil (a pre-0.6 cookie) or was truncated at the [cookie byte cap](../../../docs/OPERATIONS.md#who-decides-who-gets-in-chart-vs-console-vs-idp), so the server can't re-derive whether THEY currently hold admin from it — refused rather than risk a false lockout claim either way. | Sign out and back in to refresh the snapshot, then retry the write. |
| `POST /access/mappings` refused: **"Email mappings are disabled on this install"** | The value contains `@` and `env.WARDYN_OIDC_ALLOW_EMAIL_MAPPINGS` is unset (see [docs/ENV.md](../../../docs/ENV.md#wardyn_oidc_allow_email_mappings)) — the console steers Entra deployments to an App Role or `groups` key by default. | Map an App Role or `groups` value instead (preferred), or set `env.WARDYN_OIDC_ALLOW_EMAIL_MAPPINGS=true` if this deployment genuinely has no usable claim besides email. |
