# Operating Wardyn

Backup, upgrade, rotation, monitoring, and the scaling constraint — the questions
that arrive after the stack is up.

**Scope: mostly the Compose stack.** The backup/state-store, monitoring, and
corporate-network sections below are written against
[`deploy/compose/`](../deploy/compose/). The Helm chart (`deploy/helm/wardyn`,
[`k8s.enabled`](../deploy/helm/wardyn/README.md)) now runs its own
[Kubernetes runner substrate](#kubernetes-known-gaps-v05) with its own
run/recording state to operate — see that section for what's different there,
and [Kubernetes: day-2](#kubernetes-day-2) for the chart's own backup, restore,
upgrade and key-persistence commands.
"[Multi-user: who can change what](#multi-user-who-can-change-what)" and
"[One replica, by construction](#one-replica-by-construction)" apply to both
substrates identically: authorization and the per-process constraints live in
`internal/api`, above the runner seam.

## State stores

Three stores hold data that exists nowhere else. Lose any of them and the loss is
permanent.

| Store | Where | Holds | If you lose it |
|---|---|---|---|
| Postgres | volume `<project>_postgres_data` | runs, approvals, workspaces, policies, encrypted secrets, the append-only audit log — and, under the default `pg` recording store, the PTY asciicasts too | everything |
| Recordings | volume `${WARDYN_NS:-wardyn}-recordings` (`WARDYN_RECORDING_DIR=/data/recordings`) | PTY asciicasts for Replay — **only with `WARDYN_RECORDING_STORE=fs`**; the shipped default (`pg`) keeps them in Postgres and leaves this volume empty | every session replay it holds; nothing reconstructs them |
| Age key | `WARDYN_AGE_KEY` in `deploy/compose/.env` | the X25519 identity every stored secret is encrypted to | every secret in Postgres becomes undecryptable ciphertext |

`postgres_data`, `registry_data` and `audit` are declared WITHOUT an explicit
`name:` in `deploy/compose/docker-compose.yaml`, so Docker prefixes them with the
compose project — `compose_postgres_data` by default (the project name comes from
the `deploy/compose` directory, `docker compose config | grep '^name:'`). Only
`recordings` is explicitly named (`${WARDYN_NS:-wardyn}-recordings`), because the
docker runner has to mount that same volume into agent containers BY NAME. Reach
for the unprefixed form and `docker volume inspect postgres_data` reports no such
volume — and a volume-level backup that ignores that error archives nothing. Back
Postgres up with `pg_dump` (below), not at the volume layer.

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
#    (`recordings` is the ONE explicitly-named volume, so no project prefix —
#     see docker-compose.yaml)
docker run --rm -v wardyn-recordings:/from -v "$PWD":/to alpine \
  tar czf /to/recordings-$(date +%F).tar.gz -C /from .

# 3. The age key — copy WARDYN_AGE_KEY out of deploy/compose/.env into your
#    secret manager. A Postgres dump without it is unreadable ciphertext.
```

### Restore them

Order matters, and it is not just the backup order reversed. The age key has
to be in place before wardynd ever boots against the restored data — putting
it in `.env` after `make setup` means the first boot already minted (and
started using) an ephemeral one. And Postgres has to be the ONLY thing
running while the dump loads: bringing up the full stack first races
wardynd's own migrations and health traffic against the restore, and a bare
`psql` with no `-v ON_ERROR_STOP=1` (the earlier form of this recipe) keeps
going past a failed statement and still exits `0` — a lock conflict or schema
mismatch mid-load then leaves a silently half-restored database that looks
like a clean success.

```sh
# 1. Age key FIRST, before anything boots against the restored data — copy
#    it back into deploy/compose/.env (wherever you stashed it in backup
#    step 3 above).

# 2. Start Postgres ALONE, not the full stack, so nothing else touches the
#    database mid-restore. (Restoring onto a HOST THAT ALREADY HAS DATA?
#    `docker compose -f deploy/compose/docker-compose.yaml down -v` first —
#    pg_dump's plain-SQL output re-creates the schema from scratch and will
#    collide with an existing one.)
docker compose -f deploy/compose/docker-compose.yaml up -d postgres

# 3. Restore into it. -v ON_ERROR_STOP=1 makes the FIRST failed statement
#    abort the whole load with a nonzero exit, instead of silently skipping
#    it and leaving a partial database with no error.
docker exec -i wardyn-postgres psql -U wardyn -v ON_ERROR_STOP=1 wardyn < wardyn-<date>.sql

# 4. Recordings — `fs` store only; the default `pg` store already came back
#    in step 3.
docker run --rm -v wardyn-recordings:/to -v "$PWD":/from alpine \
  tar xzf /from/recordings-<date>.tar.gz -C /to

# 5. Now bring up the rest of the stack.
make setup

# 6. Verify — row count first:
docker exec -i wardyn-postgres psql -U wardyn -d wardyn -c "SELECT count(*) FROM audit_events;"
#    then prove the age key actually decrypts what came back, which a row
#    count alone can't: launch a run against any workspace/policy that
#    depends on a previously-stored secret and confirm it starts instead of
#    failing closed with a decrypt error (see "The age key has no rotation
#    path" — the wrong key fails exactly here, not at boot):
wardyn run --agent claude-code --workspace <workspace-id>
```

> `make reset` runs `compose down -v` after a confirmation prompt
> (`scripts/up.sh` `cmd_reset`): Postgres, recordings and the audit sink all go,
> with no backup counterpart. It leaves `.env` — and so the age key — alone.
> `make compose-down` stops the stack and keeps the volumes.

### The audit log can't quietly rot

[Watch — Audit & attach (2:00–2:30)](README.md#v10--audit--attach)

"Append-only" here is enforced by the database, not by convention. A row-level
Postgres trigger rejects `UPDATE` and `DELETE` on `audit_events`, and a
statement-level guard (migration `0004`) rejects `TRUNCATE` — all three are
asserted in `TestPG_AuditAppendOnly_TriggerRejects`
(`internal/store/store_pg_test.go`), so an operator with direct database access
cannot rewrite or silently thin the trail through Wardyn's own schema. The
nearest competitor's own maintenance docs, by contrast, recommend `DELETE FROM
audit_logs` to prune.

Completeness survives an outage too. When a Postgres write fails — the store is
briefly down, a transient error — the event is not dropped: it is fsync'd, one
JSON line at a time, to a local append-only spool (`WARDYN_AUDIT_SPOOL`, default
`./data/audit-spool.jsonl`, empty to disable — `internal/api/auditspool.go`), and
a background drain replays it back into Postgres once the store recovers. Where a
fail-closed audit trades *availability* for integrity — refusing to serve until
it can record — Wardyn keeps both. The spool is per-process by design: it is the
fallback for one pod's failed write, and each `wardynd` drains its own back on
recovery (see [One replica, by construction](#one-replica-by-construction)).

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
| unset | listed in `WARDYN_OIDC_OPERATOR_EMAILS` → **admin**, others → **member**; all **admin** only when the allowlist is also unset (override-only under OIDC — the pre-0.5 behavior) | always **admin** |
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
route (including `approved-egress`/`denied-egress`/`llm-cred`/`requirements`), `PUT
/site-config` and its connectivity probes, secret write/delete, `GET
/metrics`, the permissioning routes below, and bringing a custom devcontainer
repo to a run (`devcontainer_repo` — `denyMemberRequest`,
`internal/api/runs_create_validate.go`). A custom sandbox `image` is admin-only
*by default* and is the one power a capability grant can hand a member (see
"Capabilities" below); a member's own onboarded-workspace base image was never
gated either way, since that path is operator-authored at onboarding time,
never the member's own free-text choice. Launching and killing a run (`POST
/runs`, `POST /runs/{id}/kill`) stay open to any signed-in human — using the
product is a member act by design.

**Ownership scoping — real, not just admin-vs-everyone.** A member reaches
their OWN resources the same way an admin reaches any of them
(`ownsRunOrAdmin`/`getRunAuthorized`, `internal/api/helpers.go`): `GET`/kill/
profile/grants on a run, minting its attach ticket, its recording replay, and
`GET /runs`/`GET /approvals` (each scoped to the caller's own `created_by`
rows) all answer a foreign resource with the **byte-identical
404** a truly-missing one gets — never a 403 — so probing another user's run
id learns nothing (no existence oracle). `GET /audit` is **run-scoped, not
`created_by`-scoped**: a member must pass `?run_id=` naming a run they own —
no `run_id`, or one they don't own, both return an empty `200` list (a
collection endpoint's no-oracle answer), so a member's unfiltered audit feed
is always empty by design. The console reaches it from a run's Audit tab,
whose "open full Audit" link carries `?run_id=`. `GET /setup/status` redacts
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

**An `egress_domain` decision's *scope* adds a second, narrower gate on top
of the kind check above — and one of the four scopes is gated on ROLE, not
ownership.** A member who owns the run may still choose `once`, `run`, or
`until` for their own decision; each stays inside that run's own proxy
cache. `always` is **operator-only regardless of run ownership** (`decide()`
rule 6, same file): it writes a durable entry onto the run's workspace
(`approved_egress` on approve, `denied_egress` on deny) — the SAME two
columns the `approved-egress`/`denied-egress` routes write, both already
`operatorOnly` above, alongside every other mutating `/workspaces` route.
Without this gate a member could reach those same two columns through the
approval queue instead, a back door around that gate. The refusal is a `403`, not the
byte-identical `404` the ownership checks above use: by the time this rule
runs, the caller has already proven the approval exists, is `egress_domain`,
and is theirs, so a `403` discloses nothing new. It is also checked before
the run is confirmed to reference a workspace at all — authorization before
validation, deliberately, so a member's rejection depends only on role,
never on what the run happens to reference (see
[POLICIES.md](POLICIES.md) "Approval decision scopes" for the
workspace-resolution check this precedes).

**`PUT /workspaces/{id}/denied-egress` is the only way to undo a `deny ·
always` decision.** Full-replace, same shape as `approved-egress` (omit a
host to un-deny it — there is no per-host delete). This is not just a
convenience route: deny beats allow everywhere the proxy evaluates policy, so
a `deny · always` on the wrong host — a model-provider host, or a host a
required integration injects a credential into — can permanently break that
workspace's own proxy-side credential injection for every future run
launched against it. `denied-egress`'s validator is deliberately narrower
than `approved-egress`'s own (plain host shape only, no model-provider reject
set) specifically so it keeps working as the escape hatch even when the
workspace is already bricked — see `handleSetDeniedEgress`'s comment
(`internal/api/workspaces.go`) for why copying the other validator here would
close the one door back out.

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

### Capabilities: what one member, or one group, may do

The role split above is deployment-wide. A **capability grant** is per-human:
a row naming a *subject*, a *kind*, a *value*, and an effect of `allow` or
`deny` (`capability_grants`, migration 0042), with a per-kind **enforcement
switch** beside it (`capability_enforcement`). One sentence is the doctrine,
and every rule below follows from it: **a capability bounds what the MEMBER
chose, never what the ADMIN pre-authorized.**

That is why a stored policy, a workspace's own requirements, the hosts a
workspace scan seeded, the model provider's own egress, and the grant
`foldRunIntegration`/`applyWorkspaceRequirements` re-add at launch are all left
untouched no matter what a member does or doesn't hold: narrowing
admin-authored egress would brick workspace runs at scale, and a member who
cannot be trusted with a workspace should not be granted the workspace.

**The four kinds** — a closed set, written down once in Go
(`capabilityKinds`, `internal/api/capabilities.go`) rather than as a schema
CHECK:

| Kind | Value | Direction | What it bounds, and where |
|---|---|---|---|
| `egress_host` | a host, or a `*.suffix` wildcard | narrows | which host a member may **decide** an `egress_domain` approval for (`authorizeMemberDecision`, `internal/api/approvals.go`), and which hosts survive on a member's own `inline_policy` allowlist (`narrowMemberInlinePolicy`, `internal/api/inline_policy.go`) |
| `secret` | exact secret name | narrows | which stored secret a member's own `inline_policy` grant may reference — both refs of an `ssh_key` grant, key and `known_hosts` — and which names `GET /secrets` lists back to them (`handleListSecrets`, `internal/api/secrets.go`) |
| `workspace` | workspace uuid | narrows | which onboarded workspace a member may name on `POST /runs`/preflight (`denyMemberRequest`, `internal/api/runs_create_validate.go`) |
| `image` | exact image ref | **widens** | which custom sandbox image a member may launch at all — without a grant, none (same seam) |

`*` as a value matches everything of that kind, spelled the same way for all
four. `egress_host` values are matched by `entryCoversAny`
(`internal/api/artifact_redirect.go`) — the *same* matcher that already decides
whether one allowlist entry covers a host, deliberately not a second one,
because two host matchers that disagree is how a deny gets bypassed by a port
suffix. Every other kind is an exact compare: a secret name, a uuid, and an
image ref are identifiers where a near-miss must not match.

`devcontainer_repo` is **not** a kind and stays unconditionally admin-only. It
hands attacker-authored build configuration to the image builder, which is not
a power to hand out one row at a time.

**Precedence — the order is the design** (`capAllowed`, same file):

1. **admin, admin token, and local mode are exempt.** A capability bounds a
   member; the admin tier is the one writing the grants.
2. Any matching **deny** ⇒ refused.
3. Any matching **allow** ⇒ permitted.
4. The kind is **not enforced** ⇒ permitted.
5. Otherwise ⇒ refused.

There is no user-over-group precedence: a deny anywhere wins, because "Bob's
user allow overrode the group deny" is a breach report. Deny sits *above* the
enforcement switch on purpose — that makes deny rows the adoption on-ramp: you
can blacklist one host for one contractor without flipping the whole
deployment fail-closed. A store error is never permission: the request answers
`500`, not "allowed".

**The widening kind reads the same rows the other way.** For `image`
(`capGranted`), an unenforced kind is *refused*, not permitted — because 0.5
refused it too. Both directions obey the same rule, "an upgrade with no
configuration changes nothing", and land on opposite defaults only because the
two start from opposite postures. So `image` needs *both* the switch on and an
exact-ref grant; the other three need only the absence of a deny until you
enforce them.

**Default posture: an absent enforcement row is off.** A deployment upgraded
from 0.5 with no rows written behaves byte-for-byte as it did before — this
whole subsection is inert until an admin turns a kind on, one kind at a time.
Turning `workspace` on with no grants written is the way to lock every member
out of every workspace at once; write the grants (or the targeted denies)
first, then flip the switch.

**Subjects, and the group snapshot's ceiling.** A grant's `subject_type` is
`user`, `group`, or `all`:

- `user` matches **either** the lowercased OIDC `sub` **or** the email — an
  admin writing a grant knows one or the other and shouldn't have to guess
  which one the IdP made authoritative. It widens who an allow hits, and in
  the direction that matters a deny on either identity also hits.
- `group` matches the login-time union of the ID token's `roles` and `groups`
  claims (so Entra App Roles are grantable for free), lowercased and
  deduped.
- `all` matches every signed-in human — the baseline for an IdP whose group
  claims aren't usable.

Group membership is a **snapshot taken at login** and carried in the session
cookie; grants themselves are read from the database per request, so a new
grant takes effect on the very next request but a *directory* change does not
until the human signs in again. The snapshot is capped at 2048 bytes of
payload (`maxSessionGroupsBytes`, `internal/auth/oidc/derive.go`) so the
signed cookie stays under the ~4096 bytes a browser will silently drop
entirely; groups are sorted and dropped **from the end**, so the same human
loses the same groups on every login instead of a coin flip. That is roughly
100 typical group names — past that, grant the user directly, or prefer Entra
App Roles, which arrive on the much smaller `roles` claim. A pre-0.6 session
cookie carries no groups field at all and stays valid (no forced re-login);
that state is reported distinctly as `groups_snapshot_stale` on `GET
/me/capabilities`, because "can't tell yet" and "holds no groups" must not
read the same.

**What a capability deliberately does not reach.** `always`-scope decisions
stay operator-only even for a member granted the host — a grant must never
promote a member's decision into durable workspace config. `GET /workspaces`
is not narrowed: visibility is not capability, the launch gate is what
refuses, and hiding the row would only make the refusal unexplainable.
Machine lanes (`/internal/*`, ground-truth ingest, attach tickets) are
untouched. And where the operator ceiling sets `allow_all_egress`, the
allowlist is not the gate at all, so `egress_host` narrowing does nothing
there — that is the operator's own posture, not a switch that failed.

**Managing them** (all `operatorOnly` except the last):

| Route | Does |
|---|---|
| `GET /permissions` | the whole grant table plus every enforcement switch, one call |
| `POST /permissions/grants` | upsert one grant on its natural key (`201` new, `200` updated) |
| `DELETE /permissions/grants/{id}` | remove one grant |
| `PUT /permissions/enforcement` | replace the whole switch map — an omitted kind means *off* |
| `GET /me/capabilities` | member-safe: the caller's OWN grants, the switches, their session groups, and `groups_snapshot_stale` |

Writes are audited as `capability.grant.created` / `.updated` / `.deleted` and
`capability.enforcement.write`. Enforcement lives in its own table rather than
in SiteConfig precisely because `PUT /site-config` is a full replace: a stale
client round-tripping an older document could otherwise silently disable an
authorization control, which is a fail-open nobody would see.

There is **no cache** — resolution is two indexed reads per check, so a
grant applies immediately. A process-local cache would be the same HA blocker
named elsewhere in this document, and a stale permission cache is a security
bug rather than a slow page.

### Every denial that isn't a 404

Every member denial that isn't a plain foreign-resource 404 is audited under
`authz.denied`, whose `reason` field is the whole vocabulary:

| `reason` | Raised when | Shape |
|---|---|---|
| `admin_surface` | a member requested an admin-only route | `403` |
| `not_owner` | a member reached a run/approval/recording that exists but isn't theirs | `404` (byte-identical to missing) |
| `byoi_member` | a member named a `devcontainer_repo`, or an `image` they hold no grant for | `403` |
| `capability_workspace` | a member named a workspace they aren't granted | `403` |
| `capability_egress_host` | deciding: the approval's host isn't granted (`403`). Launching: member-authored allowlist entries were dropped from an `inline_policy` — the run still launches | `403`, or a drop |
| `capability_secret` | a member's `inline_policy` grant referenced a secret they aren't granted — dropped, not rejected | drop |
| `grant_pairing_not_eligible` | a member's `inline_policy` paired a stored secret with a host the operator never eligible-listed (`filterMemberGrants`) — dropped | drop |

The three drop rows are why `POST /runs` mostly *narrows* rather than refuses:
a member whose whole allowlist is ungranted gets a run with no member-authored
egress, not a `403`, because the run's admin-authored egress is still there and
is usually what the task needed. Each drop surfaces as a warning in
preflight/Review before launch **and** as an audit event at launch — one event
per reason, with the affected values beside it, rather than one per dropped
host, so a policy naming twenty ungranted hosts reads as the one authorization
outcome it is. Preflight dry-runs are not audited: Review re-resolves on every
edit, and a stream of denials for a policy nobody launched is
indistinguishable from denials that actually bounded a run.

A 404 on a resource that genuinely doesn't exist stays silent by design,
matching the no-existence-oracle rule. One exception remains: the
`always`-scope 403 above returns before `decide()` reaches any audit call, so
it is a bare 403 with no audit trail at all, unlike every other denial here.

**What's still not built.** No custom roles beyond admin/member — capabilities
narrow (or widen) what a member may reach, they do not add a third role. Only
the four kinds above are grantable; there is no general per-resource permission
model (a run is still owner-or-admin only — no "read-only share" or "co-owner"
concept), no tenant/org columns, no
separation of duty among admins — every admin (and the admin token, always)
can rewrite the policy that bounds them (`threatmodel/THREAT-MODEL.md`
residual #14, still open). The SSH gateway's admin override is a
**registration-time stamp**, not a live role check: since migration `0043` a
key authorizes when `run.created_by == the key's principal` OR the key's
`role` column reads `admin`, and that column is written once, at
`POST /me/ssh-keys` time, from the role the registering session held then.
The gateway never re-reads the human's role now, so a demoted admin's
already-registered key keeps the override until that key is deleted
(`DELETE /me/ssh-keys/{fingerprint}`, self-service) and re-registered —
strictly weaker than the web terminal's live `requireOperator` gate. A
member's key never satisfies the override (`docs/SSH.md`'s Bounds section;
`threatmodel/THREAT-MODEL.md` residual #15). See
[ROADMAP.md](../ROADMAP.md) for what's queued.

**None of this governance is a paid tier.** The admin/member split above, the
capability grants, the approval broker, and the append-only audit log all ship in
the Apache-2.0 build — there is no license key, no "Premium" gate, no entitlement
check anywhere in the tree (`grep -riE
'license.key|premium|enterprise.(only|tier)|entitlement' internal/ cmd/` returns
nothing), and the gating is completeness-tested: `internal/api/authz_test.go`
walks every route the router actually registers and fails the build if any one of
them — all 36 admin-gated routes included — is missing from its `routeMatrix`, so
a new route must be classified admin/member/owner/anonymous/internal before it can
ship; `internal/api/rbac_test.go` then proves each of the 24 widest admin-gated
writes really does 403 a member. Worth stating plainly,
because the field Wardyn is measured against puts exactly these controls behind a
license — Coder bundles audit logging and template RBAC into a 30-day **Premium**
trial, Vault's namespaces and hold-then-resume are Enterprise/HCP, OpenHands gates
RBAC/SSO to Enterprise. What Wardyn gives up is *breadth* — this is a deliberate
two-tier split, not per-user roles or multi-org depth — not the governance itself.
A corporate evaluator used to OSS meaning a crippled trial should read the trade
the other way here.

## Second user, same host

> This recipe gives a second person their own SSO identity instead of the shared
> admin token — and what that identity *can do* is exactly the **admin/member**
> model in [Multi-user: who can change what](#multi-user-who-can-change-what)
> above. Under OIDC, `WARDYN_OIDC_OPERATOR_EMAILS` is the boot-required allowlist
> (an empty one **refuses to boot** unless `WARDYN_ALLOW_OIDC_NO_OPERATOR_LIST=true`,
> which — absent a role map — instead makes every signed-in human admin —
> `cmd/wardynd/boot_deps.go`):
> everyone on it is an **admin**, and `WARDYN_OIDC_ROLE_MAP` (opt-in, on top) can
> also derive **admin**/**member** from SSO roles/groups. Anyone neither listed
> nor mapped to admin is a **member** (or, with a role map set, denied login
> unless `WARDYN_OIDC_DEFAULT_ROLE` catches them) — a member reads and
> launches/kills runs, but is owner-scoped: only their OWN runs/approvals/audit
> are visible (a foreign run id gets the byte-identical 404 a missing one does, so
> there is no existence oracle), they may decide only `egress_domain` approvals on
> runs they own, their `inline_policy` is clamped to your ceiling, and every
> admin-only surface 403s. What remains unbuilt is narrower than "no roles at
> all": no custom roles beyond admin/member and no "read-only share"/co-owner
> concept, and the admin token is always an admin and cannot be demoted
> ([ROADMAP.md](../ROADMAP.md)).

A **remote** second person is a dead end regardless: both the bundled Dex and
wardynd itself publish loopback-only (`127.0.0.1:PORT`,
`deploy/compose/docker-compose.yaml`) by design, so nothing off-host can reach
either. What *does* work is narrower — two people on the same box, each with
their own identity instead of sharing the admin token — and takes explicit
setup `make setup` does not do for you:

1. **Turn local mode off.** The containerized `make setup` path writes
   `WARDYN_LOCAL_MODE=true` into `deploy/compose/.env` (see the
   `WARDYN_ADMIN_TOKEN` row in [ENV.md](ENV.md)); left in place alongside a
   configured OIDC issuer, wardynd now **refuses to boot** rather than
   silently winning over OIDC (`resolveLocalMode`,
   `cmd/wardynd/boot_flags.go`: an explicit `-local-mode` with `-oidc-issuer`
   also set is refused unless `WARDYN_ALLOW_LOCAL_MODE_WITH_OIDC=true`
   explicitly overrides it — no login is ever required, for anyone, if you do
   override it, regardless of what you configure below). A `make setup`
   re-run also warns about exactly this combination before boot gets the
   chance to refuse it. In `deploy/compose/.env`:

   ```sh
   WARDYN_LOCAL_MODE=false
   ```

2. **Turn Dex on, and choose who's an admin.** Dex is the `sso` compose
   profile; every other OIDC var already defaults to match the bundled
   `deploy/compose/dex.yaml` client (client id/secret, redirect URL, email
   domain — the `wardynd` service's `environment` block in
   `docker-compose.yaml` carries those defaults), so the two you set are the
   issuer and the operator allowlist. With OIDC configured, an **empty**
   `WARDYN_OIDC_OPERATOR_EMAILS` refuses to boot (the split has to mean
   something — [Multi-user: who can change what](#multi-user-who-can-change-what)),
   so list yourself: everyone listed is an **admin**, everyone else who can sign
   in is a **member** (add `WARDYN_OIDC_ROLE_MAP` to derive admin/member from SSO
   roles/groups instead).

   ```sh
   echo 'WARDYN_OIDC_ISSUER=http://localhost:5556'        >> deploy/compose/.env
   echo 'WARDYN_OIDC_OPERATOR_EMAILS=you@wardyn.local'    >> deploy/compose/.env
   docker compose -f deploy/compose/docker-compose.yaml --profile sso up -d dex wardynd
   ```

   This is not a soft gate: wardynd runs synchronous OIDC discovery against
   the issuer at boot and **exits nonzero if it fails**
   (`cmd/wardynd/boot_deps.go`) — an unreachable Dex refuses the whole boot,
   not just SSO. Compose's `depends_on: dex: condition: service_healthy`
   already sequences this for the command above; it only bites if you later
   restart wardynd alone while Dex is down.

3. **Give the second person their own login.** For the bundled Dex,
   `staticPasswords` in `deploy/compose/dex.yaml` is the authentication list —
   `enablePasswordDB: true` with no external connector means an email absent
   from it has no password to authenticate with, full stop. (Whether that
   sign-in is an admin or a member is decided by
   `WARDYN_OIDC_OPERATOR_EMAILS`, not here: Dex authenticates, the operator list
   authorizes.) Mint a bcrypt hash (Dex's own recipe; any bcrypt tool at the
   same cost works):

   ```sh
   htpasswd -bnBC 10 "" 'their-password' | tr -d ':\n'
   ```

   and add an entry alongside the demo user:

   ```yaml
   staticPasswords:
     - email: "demo@wardyn.local"
       hash: "$2a$10$SDMtAYUgJDDzcanSySsoBuLPINvmRvxVpqg3WU9jfThQABkwBvaiK"
       username: "demo"
       userID: "demo-0001"
     - email: "reviewer2@wardyn.local"   # not in the operator list ⇒ a member;
       hash: "<paste the whole generated hash>"  # domain must clear WARDYN_OIDC_EMAIL_DOMAINS
       username: "reviewer2"
       userID: "reviewer2-0001"
   ```

   then reload it — `up -d` does not notice a bind-mounted file's *content*
   changing, only `restart` does:

   ```sh
   docker compose -f deploy/compose/docker-compose.yaml restart dex
   ```

4. **Each person signs in on their own.** The browser is redirected to Dex
   directly for the login leg, so both `:8080` (wardynd/UI) and `:5556` (Dex)
   must be reachable from each browser — trivial at a shared console, an
   `ssh -L 8080:localhost:8080 -L 5556:localhost:5556 <host>` tunnel per person
   otherwise (a tunnel to reach the existing loopback bind, not a change to
   Wardyn's own network posture). Each clicks **Sign in with SSO** and
   authenticates as themselves instead of pasting the admin token.

`WARDYN_OIDC_EMAIL_DOMAINS` is a separate knob with a different failure mode: an
empty value is not "deny all", it fails **open** — any account the IdP
authenticates gets a session (a member one unless the address is in the operator
list), and without the domains list the `email_verified` claim is not checked at
all (both checks live inside the domains branch — `AllowedEmailDomains`,
`internal/auth/oidc/oidc.go`). The bundled Dex's hand-curated `staticPasswords`
makes that moot for this recipe; set the domain(s) for real once you point this
at a corporate IdP that isn't hand-curated the same way.

## Workspaces: three tiers

A workspace is not one unit of configuration. Wardyn splits it into three:

1. **Source** (tier 1) — a repo or local directory configured ONCE, in a
   shared library: its own requirements contract, its own scan profile and
   status, deduplicated by canonical identity (locator + ref). `GET/POST
   /api/v1/sources`, `GET/PUT/DELETE /api/v1/sources/{id}`, `POST
   /api/v1/sources/{id}/scan` (`mountLibraryRoutes`,
   `internal/api/sources.go`) — there is no separate
   `/sources/{id}/requirements` route; a source's own contract rides the
   plain `PUT /sources/{id}` body (`handleUpdateSource`).
2. **Base image** (tier 2) — a shared catalog row: registry, custom, or BYO.
   "Recommended" is never a catalog kind — it is a per-workspace DERIVED
   build, excluded by a database CHECK constraint, not by convention.
   `GET/POST /api/v1/base-images`, `GET/DELETE /api/v1/base-images/{id}`.
3. **Workspace** (tier 3) — an ordered list of attachments (library sources,
   or inline ephemeral scratch dirs) plus an optional catalog image.
   Attachment order is load-bearing: `attachments[0]` is the primary, the
   same rule a single `sources[0]` carried before the split. The floor is
   one attachment — an ephemeral scratch dir, seeded structurally so the
   invalid empty state cannot be built.

`wardyn source list|create|scan|rm` manages tier 1 from the CLI;
`wardyn workspace create --attach SOURCE-ID[@target][:ro|:rw]` composes a
workspace from already-configured sources. Deleting a source or image that
workspaces still use answers `409` naming every workspace attaching it
(`handleDeleteSource` / `handleDeleteBaseImage`); `?force=1` detaches them
instead of refusing — for an image that means "fall back to the derived
recommended build", for a source it un-mounts code, which is why the refusal
is the default rather than something silently tolerated.

### The effective contract is one pure fold

A workspace's effective requirements come from `FoldWorkspaceContract`
(`internal/types/workspace_contract.go`) over its attachments, the attached
sources' own contracts, and the workspace's own overlay rows — computed at
the store's hydrate pass, never stored duplicated. Precedence, in order:

- each attachment contributes its source's contract, in attachment order;
- a `write:<path>` row is DROPPED unless `<path>` is that source's own
  locator — a shared source cannot declare write access to a path it
  doesn't own and silently widen a sibling mount in every workspace that
  attaches it;
- when two attachments contribute the same key, the merge is fail-closed:
  the LEVEL takes the strongest contributor (required beats optional), but
  the PROVENANCE takes the weakest (`scan_seeded` beats `operator_set`) — so
  if any contributor's row came from reading untrusted repo content, the
  merged row never auto-grants a credential, even when another contributor
  declared the same key as a direct operator act;
- the workspace's own overlay rows replace the merged value outright — an
  overlay cannot REMOVE a key (a per-attachment override does that, next).

With no attachments, or all-ephemeral ones, the fold is exactly the overlay —
which is what makes migration `0031` (extracting attachments out of the
pre-split embedded columns) provably behavior-identical for every workspace
that predates it.

### Per-attachment overrides are modeled but not yet operator-reachable

`WorkspaceAttachment.Overrides` lets a workspace disable (`off`) or re-lane
(`optional`/`required`) a requirement key ONE of its attached sources
declares — mount a repo read-only to read its code without inheriting its
build secrets, for example — and the fold above honors it. **There is no
write path to it yet**: no field on the workspace write endpoints, no CLI
flag, no wizard control sets `Overrides`. Every attachment Wardyn builds
today carries `SourceID`/`Target`/`Writable` only. Treat it as modeled and
folded, not as something an operator can reach, until a write surface ships.

### The requirements contract: Required or Optional, nothing else

Every control a source or workspace can carry — a secret by name, an egress
host, write access to a directory, a named integration — is a row with ONE
axis: **Required** rides along with every run that attaches the workspace,
**Optional** is a per-run opt-in. `PUT /api/v1/workspaces/{id}/requirements`
writes the workspace's own overlay rows (`handleSetWorkspaceRequirements`,
`internal/api/workspace_requirements.go`). The 7-step workspace wizard that used to write
them from its Requirements and Integrations steps was retired in 0.5 — adding a
workspace is one dialog now — so the workspace detail page is the surface that
writes them. Launch and preflight both read
`effectiveRequirements(ws)` — the same fold — so Review can never predict
something launch won't do. A `scan_seeded` requirement can never auto-grant a
secret on its own — the scanner reads untrusted repo content, so only an
operator's direct declaration attaches a credential (the fail-closed
provenance rule above).

## Integrations

An integration is a **connection** — secrets plus egress — to any named
external system Wardyn talks to on a run's behalf, and never an installer
(what a run has installed is what its image carries). `types.Integration`
(`internal/types/workspace.go`) is ONE base shape extended by `kind`:

- `secrets[]` — each a role, a store REF (a name, never a value), and its
  **delivery**: `proxy_header` (the header + format the proxy presents on the
  wire, so the sandbox never holds the credential). A secret on a closed kind
  may declare NO delivery, which means that kind's own hand-written transport
  carries it (`github_app`'s brokered halves, `git_host`'s clone credentials,
  Bedrock's AWS env). Those hand-written resident lanes are why the type also
  models `resident_file`/`resident_env` — but an operator may not DECLARE one:
  there is no generic lane that materializes a named secret into a sandbox
  path or env var, so a write naming one is refused rather than stored as a
  promise nothing keeps. At most one `proxy_header` secret per row: the proxy
  injects one credential header per host, and every such secret targets the
  row's whole egress list.
- `egress[]` — where the system lives. This is the reason a host is reachable
  for a granted run, instead of being hand-listed in every workspace.
- `config{}` — non-secret knobs, key-validated per closed kind (bedrock ⇒
  `region`/`model`/`auth_lane`, `github_app` ⇒ `app_id`/`installation_id`/
  `host`, `anthropic_subscription` ⇒ `lane`); an unknown key on a closed kind
  400s by name.

`kind` is the ONE field that says what this connects to, and as of 0.5 the
closed set is the ONLY writable set: `anthropic_api_key`,
`anthropic_subscription`, `bedrock`, `openai_api_key`, `github_app`, `git_host`.
Each has behavior in code (`capabilitiesFor`), so a new one is a code change,
and a write naming anything else 400s with the accepted list.

Two things left in 0.5 and are worth knowing if you are upgrading. Generic
kinds — an open slug (`"jira"`, `"artifactory"`, …) validated for shape only,
whose row WAS its whole contract — are no longer writable; a row stored under an
earlier release still loads, still sits in `SiteConfig`, and is still injected by
`internal/api/integrations_run.go`, it simply cannot be edited through the API
any more. `azure_openai` is gone as a kind: its one capability powered the AI Run
Composer, which was also removed, and no agent tool can be pointed at an Azure
deployment.

**Settings** (account menu) is the one surface for these — a Model provider card
and a Git host card, each a radio group over concrete lanes. The standalone
`/integrations` page is deleted and redirects there. Rows are also DERIVED from
what already exists (stored secret names, site config, setup status), so an
operator who never opens Settings keeps identical run behavior. Host proxy and
Egress redirection are deliberately NOT here: that is network topology, its
configuration lives under **Network** (below) on the same
`SiteConfig` document, and this surface neither derives nor displays it.

### Wardyn does not dial the provider

There is no "Test" action. `POST /api/v1/integrations/{id}/test` used to launch
a throwaway confined sandbox and curl the row's own probe URL through the row's
own egress and proxy-side injection; that probe framework was removed in 0.5
along with the catalog page whose rows it verified.

Settings states what is STORED and says so plainly — "Wardyn stores this, it
doesn't dial the provider to check it" — which is the honest claim about a
credential nobody has used yet. A real run is the real test, and it fails loudly
with an audit trail if the credential is wrong.

### Nothing is ambient

Configuring an integration grants nothing by itself. A run gets one only
when a workspace's requirements name it by key — `integration:<id>`,
alongside `secret:`/`egress:`/`write:` — and that workspace is what the run
attaches (`applyIntegrationRequirement`,
`internal/api/integrations_run.go`). Once granted, its hosts join the run's
egress allowlist unconditionally, even under `allow_all_egress` — the
proxy's credential injector does not honor allow-all, so the exact-host
entry has to be there regardless — and a header-delivering integration
authors one `api_key` grant per host through the ordinary proxy-side
injection path. An operator with fifty integrations configured and a
workspace that names none of them gets a run whose spec is byte-identical to
having none at all — true for this `integration:<id>` fold, but not for
**model access** specifically: absent a more specific binding, an
AI-provider integration marked `DefaultFor: agent_runs` still folds into
the run — even one with no workspace at all (`resolveRunIntegration`,
`internal/api/llmcred.go`; see "Model access resolves" below).

That fold degrades silently by design — a workspace may state an
`integration:<id>` requirement before the integration exists, and a missing
one must never brick a run — so the create-run preflight checklist carries
an explicit row for it instead (`setupWorkspaceIntegrationItems`,
`internal/api/compose_setup.go`): "no integration named `<id>` is
configured, add it under Integrations", or "turned off", or "names no
hosts", stated as config state (amber, not the destructive red reserved for
a missing credential) with the requiring workspace named. Optional
requirements are never rowed there — only what a run cannot avoid needing.

### A header credential needs a bare exact host

An integration's `egress` entries may carry a leading `*.` wildcard or a
`:port` qualifier UNLESS one of its secrets delivers `proxy_header` (a
credential presented proxy-side). Write-time validation (`validateIntegrationHosts`,
`internal/api/integrations_write.go`) then requires every host to be a bare
exact hostname, because proxy-side injection resolves through
`Policy.AllowedExactHost`, which consults the exact-host set only. A
wildcard would open the path and silently never present the credential; a
port-qualified host is worse — the injector refuses to build a rule for it,
which is a hard proxy startup failure. Both are rejected at write time, by
name, before either can happen. Neither restriction applies to an
integration that delivers no credential header (a data store reachable on
`db.corp.internal:5432`, egress only, is exactly the shape this is for).

### Model access resolves — it does not default to none

A Claude run's model access is not configured per run. It resolves, in order
(`resolveRunIntegration`, `internal/api/llmcred.go`):

1. an explicit integration named on the run (`integration_id`);
2. else the primary workspace's `LLMCred.IntegrationRef` binding;
3. else the operator's `DefaultFor: agent_runs` integration — the one
   stored integration marked as the site-wide default for agent runs, of
   any AI-provider kind.

A workspace binding that names something — even something stale or
miscategorized — is the operator's SPECIFIC choice and does not cascade to
the site-wide default; that would be a credential surprise, not a
convenience. Launch and preflight resolve this identically
(`foldRunIntegration`), so Review cannot preview access the run won't get.

**When none of the three tiers resolves, that is not the same as no
access.** Below the Integration system, dispatch's own transport resolution
(`resolveLLMTransport`, `internal/api/runs_dispatch_llm.go`) still
credentials the run from whatever GLOBAL provider config exists, independent
of any integration or workspace binding: a Wardyn-managed subscription
connected via `wardyn subscription connect` (`managedInjectReady`,
`internal/api/harnesscred.go` — checks that the run's agent is
`claude-code` and a captured token exists, never that any integration names
it) injects proxy-side, and a global Bedrock config
(`WARDYN_BEDROCK_REGION`+`WARDYN_BEDROCK_MODEL`, see [ENV.md](ENV.md)) still
credentials Bedrock calls when no workspace/integration selection overrides
it (`resolveBedrockAuth`, `internal/api/runs_bedrock.go` — a selection wins
only the fields it sets; the global config is the fallback for the rest).
The `agent == "claude-code"` gate means the managed-subscription fallback is
not universal: a `codex-cli` run with a connected managed subscription and
no integration gets no model access via this lane. See
[TRY-IT.md](TRY-IT.md) → "Model auth: three ways" for the full transport
precedence (subscription → Bedrock → api-key) once a run reaches dispatch.

## Network: upstream proxy and egress redirects

One more piece of operator-wide config lives in Postgres alongside everything
in **State stores** above: `SiteConfig` (`GET`/`PUT /api/v1/site-config`,
`wardyn site-config get|apply`) — the corporate upstream proxy and the list of
outbound redirects every run's egress inherits. Unconfigured is a valid,
common state: a host with direct internet access needs none of this. Because
it lives in Postgres, `make reset` / `make reset-all` take it with the volume; `wardyn
site-config get > corp-baseline.json` before a reset and `wardyn site-config
apply corp-baseline.json` after is the round-trip — the document carries
secret **names**, never values, so it's safe to keep beside the repo. Because
values never round-trip, `apply` re-attaches the *names* unconditionally even
when a named secret was never restored into the fresh store (e.g. `wardyn
secret set` for it was skipped) — `apply` prints a warning naming every such
dangling ref, and the setup checklist's "Site config" row grades `warn` (never
the plain `info` of a fully-live config) for as long as one remains, so a
reset+apply that leaves a credentialed path dead never reads as fully
configured.

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

**`http://` only — an `https://` upstream proxy URL is rejected server-side,
always** (`validateSiteConfig`, same file). The hop from wardyn-proxy to your
corporate proxy is a plaintext CONNECT + `Proxy-Authorization` header; an
`https://` URL would need a TLS wrap the sidecar doesn't do, or would leak
that Basic credential in cleartext. This is the SAME gate dispatch itself
applies (`resolveUpstreamProxyURL`, `internal/api/runs_bedrock.go`) — before
this write-time check existed, an `https://` URL saved clean, displayed as
the live chain, and was silently dropped at dispatch: every run went direct
with no signal anywhere. A secret referenced via `upstream_proxy_secret_ref`
carries the same restriction; store the plain `http://` proxy URL in the
secret even when it embeds a credential.

### Egress redirects: two tiers

`egress_redirects` is a list of `{from, to, token_secret_ref,
token_integration_ref, ecosystem}` entries. Each substitutes a
public/upstream URL or host for a corporate-internal one in every run's
egress, with an optional token injected proxy-side as a Bearer credential
for `to`'s host (the sandbox never holds it). This replaced the old
`artifact_overrides` map (one entry per package ecosystem) because a
corporate estate redirects container registries and internal appliances
too, not only package managers — the shape generalized from "one entry per
ecosystem" to "a list of From → To pairs over any URL, host, or IP".

The token can come from either of two places, mutually exclusive — a row
setting both is rejected. `token_secret_ref` names a bare secret directly.
`token_integration_ref` instead names an **Integration** (see
**Integrations** above) to take the token from — a private registry is
genuinely both a system you authenticate to and sometimes the destination a
public endpoint reroutes to, and this is the seam that keeps the two from
duplicating each other: the integration owns the system and its credential,
the redirect owns rerouting a public endpoint to it. Pointing at an
integration carries more than its secret name — the header and format of that
row's `proxy_header` delivery come with it, so a feed authenticating with
something other than `Authorization: Bearer` (the bare-secret path's hardcoded
shape) finally can. Which secret that is follows the delivery, not the role
name: whatever the row calls it, its `proxy_header` secret is the credential
this redirect presents. There is no UI control for picking an integration here yet; the seam
is usable today via `PUT /site-config` and `wardyn site-config apply`.

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

### Integrations are not part of this round-trip

`integrations` — the rows behind **Settings**' Model provider and Git host
cards, plus generic rows stored under an earlier release — lives on the SAME
`SiteConfig` document `GET`/`PUT /site-config` reads and writes, but it does
not travel through this door. `PUT /site-config` 400s outright on a body
carrying a non-empty `integrations` ("integrations are managed through their
own endpoints, not PUT /site-config") and always carries the STORED
integrations forward onto whatever it persists, regardless of what the body
sent (`handlePutSiteConfig`, `internal/api/site_config.go`). That guard exists
because this is the same whole-document-replace contract as above: an older
client that `get`s a config saved before `integrations` existed, then `apply`s
it back unmodified (the exact round-trip described at the top of this
section), would otherwise silently delete every stored integration.

The practical edge: once any integrations are stored, a fresh `wardyn
site-config get > corp-baseline.json` captures them too, and the client strips
them back out on the way in (`PutSiteConfig`, `pkg/client/families.go`) so the
`apply` half of the round-trip does not 400 on its own capture. That strip is
what keeps the recovery flow working, but it also means **`apply` never
restores an integration** — the ones in the file are dropped, and the stored
ones are carried forward untouched. `wardyn site-config apply` prints a warning
naming how many it dropped, so a restore that did not happen does not read as
one. Manage integrations themselves through their own routes (`GET
/api/v1/integrations`, `PUT`/`DELETE /api/v1/integrations/{id}`), never through
this document.

`apply` also decodes the file strictly (`DisallowUnknownFields`, the same
validator the server runs): because this is a whole-document replace, a typo'd
key is not an ignored line — it would leave the real setting out of the body and
delete it. A misspelled field fails on the host, before anything is sent.

### Testing it: two probes, not a courtesy button

Wardyn otherwise has no test-connection buttons anywhere: it cannot dial a
stored credential, so a green tick would mean "we wrote it down" while
reading as "we checked" — a false reassurance nobody wants at 3am. `POST
/api/v1/site-config/test-proxy` and `POST /api/v1/site-config/test-redirect`
are the deliberate exception, on the same footing as the GitHub ref-ruleset
check ([TRY-IT.md](TRY-IT.md)): the check is real. Each launches a throwaway,
one-shot confined sandbox, makes an actual outbound request through it — the
same path a real run's egress takes — and tears the sandbox down. Both are
**admin-only** (a member 403s, like `PUT /site-config` itself) and
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

Both return `200` with `{"state", "detail", "elapsed_ms"}`; `test-proxy` adds
three qualifiers so a client never has to string-match `detail` to pick a
treatment: `via` (`proxy` or `direct` — which path the probe actually
traversed), `intercepted` (`blocked`'s captive-portal flavor — something
answered, just not with the endpoint's published payload, rendered apart
from a plain connection failure), and `custom` (the probe hit a
caller-named URL with no known payload, so a `reached` here is the weaker
"request completed" claim, never "payloads matched"):

| `state` | Means |
|---|---|
| `reached` | The path works — proxy or mirror reachable, and for a redirect, the public host is correctly *blocked* when dialed directly. |
| `blocked` | Could not reach the proxy or the mirror. `detail` names the real cause — DNS failure, connection refused, TLS failure, timeout, or curl's own exit code — never a generic "failed". |
| `bypass` | **Read this one carefully — it's the one that looks fine but isn't.** The mirror answers, but the public host it's supposed to replace is *also* still reachable, directly, from a sandbox. The redirect is configured but not enforced: a run can silently pull from the internet instead of the mirror, and every other signal — the row is filled in, the mirror answers — looks exactly like a working redirect. `test-redirect` only. |
| `no_runner` | No runner is configured; there's nothing to launch a probe with. Not an error, and not a guess. |

A probe is bounded well under a minute and reclaims (kills) its sandbox if the
run doesn't finish in time, so a wedged probe can never hold one open.

## Toolchain-fidelity environment

Dispatch used to set `GOTMPDIR`/`GOCACHE` and the Maven/Gradle JVM proxy
sysprops (`MAVEN_OPTS`/`GRADLE_OPTS`) on every run, on every image. It no
longer does: a workspace run gets exactly the groups its attached sources'
scans detected — the Go group only when a scan found Go, the JVM group only
when it found Maven/Gradle, the union across every attached source
(`buildBaseSandboxEnv`, `internal/api/runs_dispatch.go`). A run with no
workspace attached at all — ad-hoc, a bare `--image` override, scan, or
login runs — keeps the full set: nothing was scanned and nothing
declared, so "unknown" must not silently break those lanes. A workspace
whose OWN base image is registry/custom/BYO is not this lane: the workspace
stays attached (`req.Image` is set from it without leaving `wsRefs`,
`internal/api/runs_create.go`), so `runToolchainNeeds` still narrows to
what that workspace's scan found — only a workspace with no attachment at
all, or one lacking a decodable scan profile, falls back to the full set.

`GOTMPDIR` needs one more thing besides the env var: the directory has to
exist, and unlike `GOCACHE` the go tool refuses to create it — `go test`
compiles and EXECS its test binaries there, and the sandbox mounts `/tmp`
noexec, so the first Go command in an image that never pre-baked the
directory failed with `stat ...: no such file or directory`. Nothing
toolchain-specific is baked into any image for this; instead two runtime
guards create the directory from the env var alone, so a workspace's actual
requirements — not the image — decide whether it exists:

- `agent-run`'s session prep (`make_toolchain_dirs`,
  `deploy/images/common/agent-run-lib.sh`), a no-op when `GOTMPDIR` is unset;
- the attach shell's exec wrapper (`internal/runner/docker/session.go`),
  which runs the same `mkdir -p "$GOTMPDIR"` guard before the prompt
  renders — session prep was measured taking 18s to reach its own mkdir,
  while an attach shell opens instantly, so an operator typing a fast first
  command could otherwise still lose the race.

## Recommended builds on compose

"Recommended — built for this workspace" (a devcontainer build via
`internal/envbuild`, see [ENVBUILD.md](ENVBUILD.md) for the build mechanism
itself) works out of the box on the compose stack. Four things that used to
need hand-set knobs, or didn't work at all, ship pre-wired:

- a loopback OCI registry sidecar (`WARDYN_ENVBUILD_CACHE_REPO` defaults to
  `127.0.0.1:5010/wardyn/devcontainers` — Docker exempts `127.0.0.1`
  registries from TLS, so no daemon config is needed, and it is not
  reachable off-host);
- builds default ON (`WARDYN_ENVBUILD` defaults to `true` on compose; the
  bare-binary/host-mode default is still off);
- the build context is delivered as a tar streamed into the build
  container, not a bind mount by path — the earlier path was staged in the
  control plane's own filesystem, which the host Docker daemon can't see,
  so it built an empty workspace;
- the build container's capability drop grants exactly the file-ownership
  set an image builder needs instead of dropping everything, which used to
  break rootfs extraction for any featureful build.

### Every generated image carries the claude-code CLI as standard tooling; nothing bakes codex-cli

Every Wardyn-GENERATED recommended image bakes `claude-code` as a real layer —
unconditionally, the way it carries git or curl, regardless of which
integrations the workspace names (integrations are connections — secrets +
egress — and never decide what is installed). Mechanically: a
`.devcontainer/Dockerfile` the emitted `devcontainer.json` points
`build.dockerfile` at, carrying a checksum-verified native install
(architecture-detected, sha256-checked against the release manifest) that
runs as root, before every devcontainer feature, inside the hardened build
container (`genStandardTools` folded into `GenerateDevcontainer`,
`internal/workspacescan/gen.go`; proven with a gated integration test that
runs the built image and checks `claude --version`). codex-cli is not in the
standard set — it has no Wardyn-verified native-download contract, and its
npm lane would need a Node runtime the bake stage doesn't carry: the image
is built without it, never with a guessed URL or a false claim that it's
there. A devcontainer's own `onCreateCommand`/`postCreateCommand` cannot be
used for this either way: envbuilder runs lifecycle commands AFTER the image
is already pushed, so they never reach the delivered image, and they would
run as the base image's unprivileged user besides.

**This only applies to Wardyn's OWN generated devcontainer.** When the
workspace's primary source is a repo carrying its own devcontainer file
(and it is HTTPS-cloneable — an SSH source falls through to the generated
path instead, since the image builder has no SSH-clone wiring),
`resolveWorkspaceImage` (`internal/api/workspace_run.go`) builds that
devcontainer AS-IS via `ImageBuilder.BuildDevcontainer` — cloned and built
verbatim, never injected into. An agent CLI is present in that image only if
the repo's own devcontainer installs it; an agent run on an image without
one fails at the CLI, visibly, rather than being silently patched.

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

**On Helm, a downgrade past 0.6 also stalls the rollout, before migrations ever
matter.** The chart's readiness probe targets `/readyz`, which the 0.6 images
introduced; an `image.tag` at or below `0.5.0` — including the empty default,
which resolves to `.Chart.AppVersion` — serves only `/healthz`, so the probe
404s forever, the pod never joins the Service's endpoints, and
`helm upgrade`/`rollout status` hangs NotReady with nothing crashed and nothing
logged. Pin the probe back for such an image with
`--set readinessProbe.path=/healthz`, accepting that version's ceiling (a dead
Postgres reads healthy again). CI does not catch this — `helm-install-test` and
the kind quickstart both build `wardynd` from source.

## Kubernetes: day-2

The four sections above — backup, restore, the age key, upgrades — are written
against compose, and none of them transfers verbatim to the chart. This section
is the k8s form of the same four questions. Every command below was run once
against the throwaway cluster [`deploy/kind/quickstart.sh`](../deploy/kind/quickstart.sh)
builds (`make kind-quickstart`), and the outputs shown are that run's; substitute
your own release, namespace and Postgres. That cluster is demo-grade (a
single Postgres pod with no PVC) — it is a faithful place to rehearse these
commands, not a template for where to run them.

### `helm upgrade`, and why `--wait` is not optional

The migration rule is the substrate-independent one stated above: forward-only,
applied on boot, no `down` path. wardynd on k8s runs the identical
`internal/db` code, so **take the dump before the upgrade** — through the
Postgres you actually run, or through `kubectl exec` if it lives in the cluster:

```sh
kubectl -n wardyn exec deploy/postgres -- \
  pg_dump -U wardyn wardyn > wardyn-$(date +%F).sql
```

Then upgrade — and pass `--reuse-values` explicitly:

```sh
helm -n wardyn upgrade wardyn ./deploy/helm/wardyn \
  --reuse-values --set image.tag=<new-tag> --wait --timeout 5m
```

It is not redundant, and the reason is a Helm sharp edge worth knowing before
it bites: `helm upgrade` reuses the previous release's values *only while you
pass no `--set`/`-f` at all*. Add a single `--set` and Helm resets everything
else to chart defaults — which drops exactly the values a Wardyn install cannot
run without (`auth.adminToken.*`, `k8s.proxyImage`, `serviceAccount.automount`,
`secrets.ageKeyFromSecret`). The chart is built to catch that rather than
render a crippled install, so the same upgrade fails at render time with a
refusal naming the missing one:

```console
$ helm -n wardyn upgrade wardyn ./deploy/helm/wardyn --set image.tag=<new-tag> --dry-run
Error: UPGRADE FAILED: execution error at (wardyn/templates/secret.yaml:31:4):
wardyn: the public API would 401 every request. Set auth.adminToken.secretRef.name
(external Secret), auth.adminToken.value (inline demo), env.WARDYN_ADMIN_TOKEN, or
env.WARDYN_OIDC_ISSUER for SSO — [...]
```

A refusal is the good case, and dropping `secrets.ageKeyFromSecret` earns one
too: on an external-DSN install the chart refuses any render with no age
identity wired, rather than letting the reset render cleanly and take the pod
down at boot (see
[the age key](#the-age-key-is-a-secret-and-the-default-loses-your-secrets-on-boot-2)
below). `--reuse-values` is what keeps both cases from arising. Better still,
keep the install's values in a file under version control and pass `-f` every
time; then nothing is being reused, and what is deployed is reviewable.

The new pod applies nothing, because `schema_migrations` already records every
file — the forward-only rule at work, visible as an empty count:

```console
$ kubectl -n wardyn get pods -l app.kubernetes.io/name=wardyn
NAME                      READY   STATUS    RESTARTS   AGE
wardyn-66c8f746c4-2b5mq   1/1     Running   0          25s

$ kubectl -n wardyn logs deploy/wardyn | grep -c "applied migration"
0
```

**`--wait` (or `--atomic`) is the load-bearing flag, not a courtesy.** Without
it `helm upgrade` reports on the API objects it wrote, not on whether anything
came up: a deliberately broken upgrade on this cluster printed `STATUS:
deployed` and exited `0` while its only pod sat in `CrashLoopBackOff`, and
`helm history` later recorded that same revision as a clean `Upgrade complete`.
Helm's release status is not a health signal. Re-check `/healthz` after every
upgrade regardless — the quickstart's own probe asserts `.runner == "k8s"`
rather than accepting any `200`, for the same reason.

There is still no rollback. `helm rollback` restores the previous *manifest*,
which is exactly the wrong half: the schema stays migrated, and the older
wardynd it reinstates is the unsupported combination named above. Use it for a
bad *config* change (a wrong env var, a wrong image tag within one schema
generation). For a bad *release*, the dump is the rollback.

### Backup: what `pg_dump` carries here, and what it does not

The chart renders no database. `postgres.dsn` points at a Postgres you operate,
so the backup itself is your Postgres's own backup story — Wardyn adds no
mechanism. What it adds is three corrections to the compose recipe:

- **Recordings are NOT in the dump on a stock chart install.** The chart pins
  `WARDYN_RECORDING_STORE=fs` (`deploy/helm/wardyn/values.yaml`, the
  `persistence` block) — the *opposite* of wardynd's own `pg` default that the
  compose recipe above relies on to sweep asciicasts up with the database. With
  `persistence.enabled=false` (the shipped default) `WARDYN_RECORDING_DIR`
  renders empty and replay is off, so there is nothing to lose. Turn
  `persistence` on and every asciicast lives on that PVC alone: `pg_dump` will
  not carry them, and the PVC needs its own snapshot. Setting
  `env.WARDYN_RECORDING_STORE=pg` instead puts them back in the dump — the
  trade the `persistence` comment in `values.yaml` spells out.
- **The age key is a Secret, not a `.env` line.** See below; it is still the
  item that makes the difference between a restorable dump and a file of
  undecryptable ciphertext.
- **The audit spool is not a backup target.** `WARDYN_AUDIT_SPOOL` renders to
  `/tmp/audit-spool.jsonl` on a stock install and onto the PVC beside the
  recordings once `persistence` is on (`templates/deployment.yaml`) — so turning
  persistence on to capture asciicasts sweeps the spool up too, and neither
  needs restoring. Derived by design (`internal/api/auditspool.go`): it is the
  fallback for a failed Postgres write and drains back into the database.
  Postgres remains the source of truth for the audit log on both substrates.

### Restore: rehearse into a scratch database first

Two steps are Wardyn's, and both are cheap:

**1. Nothing may run against the database mid-restore.** The compose recipe's
"start Postgres alone" becomes a scale-to-zero, which on a chart install is the
whole control plane:

```console
$ kubectl -n wardyn scale deployment/wardyn --replicas=0
deployment.apps/wardyn scaled
$ kubectl -n wardyn rollout status deployment/wardyn --timeout=60s
deployment "wardyn" successfully rolled out
```

**2. The age key must already be in place** — the same "age key FIRST" ordering
as compose, for the same reason (see the next section for what happens when it
is not).

Then restore the way your Postgres restores, keeping `-v ON_ERROR_STOP=1` for
the reason the compose section gives: without it `psql` walks past a failed
statement and still exits `0`, leaving a half-loaded database that looks
clean. Before doing that to real data, **rehearse the dump into a scratch
database** — it proves the file actually loads, costs nothing, and touches no
live row:

```console
$ kubectl -n wardyn exec deploy/postgres -- createdb -U wardyn wardyn_restorecheck
$ kubectl -n wardyn exec -i deploy/postgres -- \
    psql -U wardyn -v ON_ERROR_STOP=1 -q wardyn_restorecheck < wardyn-2026-08-20.sql
$ echo $?
0
$ kubectl -n wardyn exec deploy/postgres -- psql -U wardyn -d wardyn_restorecheck \
    -c "SELECT count(*) AS audit_events FROM audit_events;" \
    -c "SELECT count(*) AS migrations FROM schema_migrations;" \
    -c "SELECT name FROM secrets ORDER BY name;"
 audit_events
--------------
            9
(1 row)

 migrations
------------
         42
(1 row)

        name
---------------------
 wardyn-signing-key
 wardyn-ssh-host-key
(2 rows)

$ kubectl -n wardyn exec deploy/postgres -- dropdb -U wardyn wardyn_restorecheck
```

That last query is the one to read closely. `secrets` is where the control
plane's own keys live — the signing key and, with `ssh.enabled`, the gateway
host key — so those two rows returning is the difference between a restored
database and a restored *install*.

Present is not the same as decryptable, though, and no `SELECT` can tell you
which one you have. Compose answers that by launching something that needs a
stored secret; on k8s the control plane answers it for you the moment you scale
back to one, because it reads its own signing key out of that table before it
serves anything. A key that does not match the restored ciphertext is therefore
a failed rollout, not a surprise at first use — which is the next section.

### The age key is a Secret, and the default loses your secrets on boot 2

Two supported wirings, and the chart refuses both ways of getting it wrong
(`deploy/helm/wardyn/templates/secret.yaml`):

| `postgres.dsn` mode | age key value | What injects `WARDYN_AGE_KEY` |
|---|---|---|
| inline (`dsn.value`) | `secrets.ageKey` | the Secret the chart creates |
| external (`dsn.secretRef.name`) | an `age-key` entry in **that** Secret | `secrets.ageKeyFromSecret=true` |
| external | `secrets.ageKey` | **render fails** — it would be silently dropped |
| external | none wired at all | **render fails** — unless `secrets.allowEphemeralAgeKey=true` |

`secrets.ageKey` defaults to empty and `ageKeyFromSecret` to `false`, so an
external-DSN install wiring neither would get **no** stable identity: wardynd
mints an ephemeral one per boot. That install works perfectly once. Its second
boot cannot decrypt what its first boot wrote, and because the control plane
loads its own keys during startup (`loadOrCreateSecret`, `cmd/wardynd/main.go`)
it fails closed there, before serving — a `CrashLoopBackOff`, not a degraded
pod. That is the fourth row: a guaranteed outage is not a default worth
shipping, so the chart stops the install at render, before it touches a
cluster.

```console
$ helm template wardyn ./deploy/helm/wardyn \
    --set postgres.dsn.secretRef.name=wardyn-db --set auth.adminToken.value=t
Error: execution error at (wardyn/templates/secret.yaml:53:4): wardyn:
postgres.dsn.secretRef.name="wardyn-db" is a PERSISTENT Postgres, but no age
identity is wired, so wardynd generates an ephemeral one at every boot. [...]
Throwaway install where losing every stored secret on restart is fine:
secrets.allowEphemeralAgeKey=true renders anyway.
```

Taking that escape hatch — `--set secrets.allowEphemeralAgeKey=true` — is what
it takes to render the broken install today, and it then fails exactly as
described. This is what flipping `ageKeyFromSecret` off on the quickstart
cluster produced back when the render still permitted it:

```console
$ kubectl -n wardyn get pods -l app.kubernetes.io/name=wardyn
NAME                     READY   STATUS             RESTARTS      AGE
wardyn-794dcd78f-tk6jw   0/1     CrashLoopBackOff   1 (23s ago)   24s

$ kubectl -n wardyn logs -l app.kubernetes.io/name=wardyn --tail=2
WARN wardynd: generated ephemeral age identity; secrets are LOST on restart. Persist one with `wardynd -gen-age-key` + set WARDYN_AGE_KEY public_recipient=age1qgu93czj2ksk2g3j4x3rq52kyaw5xkjetd7g38cn63gdl2az4eqsyztpgs
ERROR wardynd: fatal err="load secret \"wardyn-signing-key\": pg secretstore: decrypt wardyn-signing-key: age decrypt: no identity matched any of the recipients"
```

That is the correct behaviour — `loadOrCreateSecret` fails closed on a decrypt
error rather than minting a fresh key over the existing one, which would strand
the old ciphertext permanently instead of loudly. But it is also unrecoverable
from inside the cluster: there is no rotation path (see
[The age key has no rotation path](#the-age-key-has-no-rotation-path)), so the
fix is always "put the original Secret back", never "generate a new one". Back
the Secret up off-cluster, wherever the DSN Secret is backed up, and treat
deleting it as equivalent to deleting the database.

### The SSH host key survives restarts — because the age key does

The SSH gateway (`ssh.enabled`) does not carry a host key in the chart or in a
volume. wardynd generates an ed25519 key on first boot and persists it into the
secret store under `wardyn-ssh-host-key` (`loadOrCreateSSHHostKey`,
`cmd/wardynd/main.go`), through the same `loadOrCreateSecret` path as the
signing key. So the fingerprint a client pins is stable across pod churn with
no operator action — the same value survived a rolling `helm upgrade` and a
full scale-to-zero-and-back on the quickstart cluster:

```console
$ curl -s http://127.0.0.1:8080/healthz | jq -c .ssh
{"advertise_addr":"127.0.0.1:2222","enabled":true,"host_key_fingerprint":"SHA256:JEFfrvMMhOTqAMpkJNEqFz3H0hcgoox4swhRkIYIan8"}

$ kubectl -n wardyn logs deploy/wardyn | grep "ssh gateway listening"
INFO wardynd: ssh gateway listening listen=:2222 advertise=127.0.0.1:2222 host_key_fingerprint=SHA256:JEFfrvMMhOTqAMpkJNEqFz3H0hcgoox4swhRkIYIan8
```

`/healthz` is anonymous, so that fingerprint is publishable to the people who
will connect — see [SSH.md](SSH.md).

The dependency runs one way and is worth stating plainly: **the host key is
exactly as stable as the age key.** Persist the age key and clients never see a
fingerprint change. Lose it and the pod crash-loops on the previous section's
error before the gateway ever listens, so what clients get is a connection
refused, never a silently different host key — a strictly better failure than
the man-in-the-middle warning a re-minted key would produce, and the reason
`loadOrCreateSecret`'s fail-closed branch matters here specifically.

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
  **This is not bounded to two replicas either.** A single `wardynd` process
  restarting mid-run (upgrade, crash-restart, OOM) wipes the same in-memory
  map, so a run whose secrets were registered before the restart and whose
  cast uploads after it hits the identical empty-snapshot fail-open — with
  `replicas: 1` throughout. The pin removes the *cross-replica* case; it does
  not remove this one. What the map no longer does is grow without bound: a
  background sweeper evicts a run's entry once that run has been terminal for
  an hour (`api.RunSecretGrace`) — late enough for the finalize audit and the
  cast upload, which mask lazily at use time, to still see it. Nothing to
  configure, and it never touches a live run.
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
- **A pre-existing default-deny NetworkPolicy in `k8s.runsNamespace` refuses
  boot outright, with no override.** The boot-time egress canary's phase A
  applies no NetworkPolicy of its own — it only proves the cluster is
  reachable at all before phase B proves Wardyn's deny-all rule takes effect.
  If the namespace already carries a default-deny policy from something else
  (a cluster-wide baseline, another operator's), phase A's pod is blocked too,
  and wardynd refuses to boot with an INDETERMINATE verdict indistinguishable
  from a genuinely broken cluster — even though per-run confinement would work
  fine once Wardyn's own allow-rules are in place
  (`internal/runner/k8s/canary.go`). **Fix**: give `k8s.runsNamespace` a
  namespace with no ambient default-deny, or exempt Wardyn's pods from *that
  policy's own* `podSelector` (a `matchExpressions` entry with
  `key: wardyn.managed`, `operator: NotIn`, `values: ["true"]`) so it stops
  selecting them. **Do not instead add a separate allow policy for
  `wardyn.managed=true`**: NetworkPolicy allows are additive and both the agent
  and proxy pods carry that label, so such a policy widens every sandbox pod's
  egress past Wardyn's per-run deny+proxy-only policy
  (`internal/runner/k8s/sandbox.go`) and flips the canary's phase B to "CNI
  does not enforce" — which in turn invites
  `WARDYN_K8S_ALLOW_UNENFORCED_NETPOL=1` and fully unconfined runs.
- **`replicas` stays 1 on k8s exactly as it does everywhere else** — see
  [One replica, by construction](#one-replica-by-construction) above; nothing
  about the k8s substrate changes that story (the masking registry is still
  in-process, per-pod).

None of these are silent: the mount and BYOI gaps fail the run closed with a
named error, the resource-cap gaps log a warning naming exactly what is
unenforced, and the DNS/replica behavior matches the rest of this document.
Closing any of them is unstarted work, not a documented-but-planned
near-term item — see ROADMAP.md for what is actually queued.
