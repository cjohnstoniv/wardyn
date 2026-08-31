# Azure Entra ID SSO validation runbook

Live-verifies Wardyn's Entra ID App Role / groups-claim RBAC (`internal/auth/oidc`'s
`deriveRole`, `WARDYN_OIDC_ROLE_MAP`, the console's Getting Started → People
step) against a **real, throwaway** Entra tenant — not Dex
(`deploy/kind/sso/`), which proves the console/chart wiring but is not Entra
and cannot exercise its two traps (security defaults blocking device-code
sign-in, and the missing `email_verified` claim). This directory is scripts +
docs only: nothing here runs `az`/`kubectl`/`helm`/`kind`/`docker` on your
behalf. You run each step yourself, in order, under your own supervision.

Read `.claude/skills/wardyn-k8s-setup/SKILL.md` §3 first if you haven't — it's
the general Entra App Role recipe this runbook automates end to end, plus a
troubleshooting table (redirect loops, `no Wardyn role assigned`,
`ImagePullBackOff`) worth keeping open during the walk.

## Prelude — the tenant (manual, ~5 minutes)

1. Create an **Azure Free Account** if you don't have one:
   <https://azure.microsoft.com/free/>. A credit card is required for
   identity verification only — this runbook creates no billable resource,
   nothing here is charged.
2. During or after signup, note the **new tenant's ID** (Entra ID → Overview
   → Tenant ID — a GUID). `01-tenant-prep.sh` below takes it as its one
   argument.
3. **This tenant is throwaway, on purpose.** Step 1 disables Entra's security
   defaults (MFA enforcement + a device-code-flow block) tenant-wide — a
   trade-off that is fine on a tenant with three demo users and nothing else
   in it, and never fine on a tenant with real ones. `teardown.sh` at the end
   deletes every object this runbook creates; the very last line is the
   portal action that deletes the **directory itself** — do that when you're
   done. Don't point any of this at a tenant you or your org depends on.

## Context — facts this runbook leans on

- **Entra ID Free** covers everything here: per-**user** App Role assignment
  is free. Per-**group** App Role assignment needs Entra ID **P1** — not
  scripted anywhere in this runbook; the group path used here (`wardyn-eng` →
  the `groups` claim → the console's People-step mapping) is the free
  alternative and is in fact what the walk is built to demonstrate.
- The `groups` claim itself is free — `groupMembershipClaims: SecurityGroup`
  on the app registration, no P1 required.
- Tenants created **2026-07 or later** ship with **security defaults ON**:
  they force MFA and block the device-code flow. `01-tenant-prep.sh` disables
  them — see the Prelude's trade-off above. Until that runs, use a normal
  interactive browser `az login`, never `--use-device-code`.
- **Entra never emits `email_verified`.** `WARDYN_OIDC_EMAIL_DOMAINS` fails
  *every* login closed against an Entra tenant if set (`docs/ENV.md`'s own
  row says so) — `04-values.sh` never sets it, and neither should you.
- A **cloud-only** user has no `email` claim unless (a) the app registration
  requests the **optional** `email` ID-token claim (`02-app.sh` does this)
  **and** (b) the user object's `mail` attribute is actually populated —
  `mail` is **best-effort writable** on a cloud-only user, not guaranteed.
  The walk asserts this claim explicitly and names the fallback (an
  admin-token sign-in to read the raw ID token) if it's absent.
- Web-platform **redirect URIs match exactly**, including the port — Entra
  does byte comparison, not prefix matching — and Entra only accepts `http://`
  on `localhost` (any other host must be `https://`).
- `az login --tenant <id> --allow-no-subscriptions` works on a
  subscription-less Free tenant; every script here checks it's the active
  session before doing anything.
- Issuer, always: `https://login.microsoftonline.com/<tenantId>/v2.0`
  (`WARDYN_OIDC_ISSUER`). Get the v1/v2 or tenant-segment wrong and you get
  `id_token verification failed` — the SKILL.md troubleshooting table's exact
  entry for this.

## Step 1 — tenant prep

```sh
deploy/azure-entra-sso/01-tenant-prep.sh <tenant-id>
```

Interactive browser `az login --tenant <tenant-id> --allow-no-subscriptions`,
then `az rest PATCH` on
`policies/identitySecurityDefaultsEnforcementPolicy` (`{"isEnabled": false}`),
then a GET to confirm it took. Writes `TENANT_ID` to
`.env.local` (created `chmod 600`, gitignored — see below).

## Step 2 — app, people, values

```sh
deploy/azure-entra-sso/02-app.sh     # app registration, App Roles, SP, secret
deploy/azure-entra-sso/03-people.sh  # 3 users, 2 groups, role assignments
deploy/azure-entra-sso/04-values.sh  # renders values-entra.yaml from .env.local
```

Each script sources `deploy/azure-entra-sso/.env.local` on entry and appends
to it on exit — TENANT_ID, CLIENT_ID/CLIENT_SECRET, the two App Role GUIDs,
the three users' UPN/object-id/password, and the two groups' object ids all
accumulate there across the three scripts. Nothing in this directory's
generated output is ever committed:
`deploy/azure-entra-sso/.gitignore` (this directory's **own** file — the repo
root `.gitignore` is untouched) excludes `.env.local`, `values-entra.yaml`,
and `*.secret`. No script ever echoes a secret to stdout.

`02-app.sh` also writes `.env.local`'s `HTTP_PORT` (default `8480`) — the
port baked into the app registration's redirect URI. It must equal the
`WARDYN_QUICKSTART_HTTP_PORT` used to bring the cluster up in Step 3, or the
redirect URI won't byte-match what the browser is actually on (see Context
above).

## Step 3 — the cluster

State first: this quickstart targets the **default Docker daemon** (no
`DOCKER_HOST` override), not the `wardyn-docker.sock` daemon other Wardyn dev
flows use — `docker ps` **both** daemons before you start, so a stray SSO
validation cluster never lands where a live demo/e2e run expects the other
one.

```sh
WARDYN_QUICKSTART_CLUSTER=wardyn-entra \
WARDYN_QUICKSTART_HTTP_PORT=8480 \
WARDYN_QUICKSTART_SSH_PORT=2422 \
deploy/kind/quickstart.sh
```

`WARDYN_QUICKSTART_CLUSTER` is this lane's one line of `quickstart.sh` itself
(`CLUSTER="${WARDYN_QUICKSTART_CLUSTER:-wardyn-quickstart}"`) — a **named**
cluster (`wardyn-entra`) so it never collides with, and is never mistaken
for, the plain `wardyn-quickstart` cluster another lane may already have up.

**Ports 8480/2422 are deliberately not any port already spoken for
elsewhere in this repo** — avoid re-using any of: `8080`, `8280`, `2322`,
`5557`, `8390`, `8888`, `8890`, `8891`, `9999`, `8088`, `55432` (compose,
Dex-overlay, e2e Postgres, screening-room, and other demo-take ports already
in use across `scripts/`).

Teardown for this step uses the **same** variable:

```sh
WARDYN_QUICKSTART_CLUSTER=wardyn-entra deploy/kind/quickstart.sh --down
```

A bare `make kind-down` targets the chart's *default* cluster name
(`wardyn-quickstart`) — without the override it deletes the wrong cluster (or
reports "not found" and leaves `wardyn-entra` running).

## Step 4 — the overlay

```sh
kubectl --context kind-wardyn-entra -n wardyn create secret generic wardyn-entra-oidc \
  --from-literal=client-secret="${CLIENT_SECRET}" \
  --dry-run=client -o yaml | kubectl --context kind-wardyn-entra apply -f -

helm --kube-context kind-wardyn-entra upgrade wardyn deploy/helm/wardyn \
  -n wardyn --reuse-values \
  -f deploy/azure-entra-sso/values-entra.yaml \
  --set-file defaultPolicy=deploy/kind/sso/default-policy.json
```

`CLIENT_SECRET` comes from `.env.local` (`source deploy/azure-entra-sso/.env.local`
first, or substitute it by hand — never paste it on the command line where
shell history keeps it either way if you can avoid it).

**Every `kubectl`/`helm` line above, and every one in the walk below, carries
`--context`/`--kube-context kind-wardyn-entra` explicitly.** A context-less
command falls back to `kubectl`'s current-context, which may be a *different*
cluster in `~/.kube/config` (a live demo cluster, a real cluster from another
project) — this is the same shared-host discipline `docker ps` above is for,
just at the k8s-context layer instead of the daemon layer.

**The confinement-floor trap (F-10):** the chart's baked-in default policy
floors confinement at `CC2`, but this kind cluster registers no gVisor/Kata
`RuntimeClass` (Fence/`CC1` only, same as the plain quickstart and the Dex
overlay) — every **member** run would be refused at launch, clamped to a
floor the cluster can't actually satisfy. `--set-file
defaultPolicy=deploy/kind/sso/default-policy.json` rides along for exactly
this reason: it sets `min_confinement_class: CC1`, matching what this cluster
can really enforce, without touching `k8s.runtimeClasses`. Skip this flag and
the People-step run in the walk below fails closed with a confinement-floor
refusal, not an auth error — don't mistake it for one.

**`auth.adminToken` is deliberately NOT emptied.** `values-entra.yaml` never
sets `auth.*` at all, and `--reuse-values` carries the quickstart's inline
admin token forward from Step 3 — it is the recovery path if the role map
ever locks every human out (same guidance as the chart README's own
Multi-user section). Never `--set auth.adminToken.secretRef.name=wardyn-auth`
here: no such Secret exists on this install, and the chart would stop
rendering the Secret its own Deployment references (`templates/secret.yaml`
only renders the inline-mode Secret when `secretRef.name` is empty), crash
the render, or point at nothing — leaving no way back to admin at all.

## The walk

Evidence checklist — log each numbered row (pass/fail, timestamp, one-line
observation) to `local/sso-people/FINDINGS.md` as you go.

1. **Browse `http://localhost:8480` — never the `127.0.0.1` URL
   `quickstart.sh` prints.** OIDC state/PKCE cookies are host-scoped, and
   Entra only accepts `http://` on `localhost` specifically (Context above) —
   opening `127.0.0.1` starts the flow from one origin and completes it on
   another, and the ONLY symptom is a bare `400 invalid state parameter` with
   no further explanation. If you see that error, this is the first thing to
   check, not an app-registration bug.
2. **Sign in as `wardyn-admin`** (its UPN/password are in `.env.local`) — the
   **App Role path**: their token's `roles` claim carries `Wardyn.Admin`,
   which the chart's `WARDYN_OIDC_ROLE_MAP: "Wardyn.Admin=admin"` resolves to
   admin. Confirm you land in the forced **Getting Started** flow:
   - **Environment** step — confirm it renders.
   - **People** step — add `<ENG_GROUP_OID>=member` (from `.env.local`) as a
     console-managed mapping, **in the UI**, then use the People step's own
     claims preview on `wardyn-member`'s pending sign-in (or its most recent
     one) to confirm the `groups` claim actually carries that object id — if
     it doesn't, `groupMembershipClaims: SecurityGroup` didn't take on the
     app registration (`02-app.sh`'s PATCH) and step 3 below will fail before
     you get there. **Note:** this deliberately does not trip the console's
     posture-flip guard (the one that warns when a role map goes from
     empty to non-empty mid-session) — the chart's own
     `WARDYN_OIDC_ROLE_MAP` is already non-empty (`Wardyn.Admin=admin`)
     before this UI write, so the guard's precondition never fires; this is
     proven by Go test, not a gap in this walk.
   - **Egress demo**, **Secrets**, **Finish** — walk each screen to
     completion.
3. **Sign in as `wardyn-member`** (new browser profile / incognito — the
   admin session's cookie is still live otherwise) — the **groups-claim
   path**: `roles` carries `Wardyn.Member` (passes the app's "assignment
   required" gate only, matches no `WARDYN_OIDC_ROLE_MAP` entry — the chart
   map has none for it, by design), and `groups` carries the eng group's
   object id, which the **console row you just added** resolves to member.
   Confirm you land in member's own (unforced, since People is admin-scoped)
   Getting Started, and **launch a run** to prove the member path actually
   works end to end, not just authenticates.
4. **`wardyn-outsider` — the two-gate demo.** `wardyn-outsider` has no group
   and no App Role assignment (`03-people.sh`).
   - With `appRoleAssignmentRequired: true` still set (Step 2's `02-app.sh`
     default): sign in as `wardyn-outsider` and confirm Entra itself refuses
     the sign-in with **AADSTS50105** ("the user is not assigned to a role
     for the application") — **before Wardyn's own callback is ever hit**.
     Screenshot it.
   - Toggle the gate off:
     ```sh
     az rest --method PATCH \
       --url "https://graph.microsoft.com/v1.0/servicePrincipals/${SP_OBJECT_ID}" \
       --headers "Content-Type=application/json" \
       --body '{"appRoleAssignmentRequired": false}'
     ```
     Sign in as `wardyn-outsider` again: Entra now admits the sign-in (empty
     `roles`, no matching `groups` entry), and **Wardyn's own gate** denies
     it instead — redirected to `/?auth_error=no_role`. Screenshot it. Two
     independent gates, two independent denials; this is the point of the
     demo. Restore `appRoleAssignmentRequired: true` afterward if you're
     continuing to use this tenant for anything else.
5. **ID-token email-claim assertion.** Confirm the `email` claim actually
   arrived on `wardyn-admin`'s (or `wardyn-member`'s) token — either via the
   app's own console-side claims preview (same view used in step 2's People
   check) or by decoding the raw ID token (`az account get-access-token`
   won't show it; use the browser's network tab on the callback, or a
   `jwt.io`-style decode of what the OIDC library logged). **Named fallback**
   if it's absent: sign in with the recovery admin token
   (`kubectl --context kind-wardyn-entra -n wardyn get secret wardyn-auth -o
   jsonpath='{.data.admin-token}' | base64 -d`) and confirm Wardyn still
   functions without it — `email` is best-effort here (Context above), never
   load-bearing for anything this runbook proves.

## Playwright — what's automated vs. what this runbook is for

`ui/e2e/entra/entra-live.spec.ts` exercises this same login flow against a
real Entra tenant, but it is **env-gated** (`WARDYN_ENTRA_E2E=1` plus the
tenant/app/user parameters this runbook produces) and **not part of `make
ci`** — it drives Microsoft's own hosted login UI, an external dependency CI
cannot depend on being stable, reachable, or unchanged run to run. The
automated suite (`make ci`, `scripts/run-ui-e2e.sh`) carries Wardyn's own
product behavior through seams and Go tests instead
(`internal/auth/oidc/derive_test.go` et al.) — this runbook is what
live-verifies the **IdP half** those seams stub out. The spec file itself is
authored and lands with a later e2e-authoring phase, not this lane; this
README documents its env-var contract now so that phase has something to
implement against. Per the WRITE-ONLY scope of this lane, those two variable
names are **not** added to `docs/ENV.md` — that file's envdoc reverse-ratchet
guard fails on a documented var with no Go reader, since `WARDYN_ENTRA_E2E`
et al. are consumed by the Playwright spec, not by `wardynd`.

## Teardown

```sh
deploy/azure-entra-sso/teardown.sh
```

Deletes the app registration (and its service principal), the 3 users, and
the 2 groups. Prints — does not run — the cluster/Secret teardown (same
`WARDYN_QUICKSTART_CLUSTER=wardyn-entra ... --down` line as Step 3, plus the
`kubectl delete secret wardyn-entra-oidc` line) so an operator supervising a
live run keeps control of when the cluster actually goes away. The very last
step is manual and printed as a pointer, not scripted: **Azure Portal →
Microsoft Entra ID → Manage tenants → (the throwaway tenant) → Delete** — do
this once teardown.sh's object deletions are confirmed, per the Prelude's
promise.
