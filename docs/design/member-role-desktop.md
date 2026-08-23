<!--
Copyright 2025 The Wardyn Authors
SPDX-License-Identifier: Apache-2.0
-->

# Member-role desktop — ownership + safe local_dir mounts (M0 design)

Status: **DESIGN ONLY** (the gate before any member-role code; the owner reads this
before M1). No code in this milestone. Every boundary below is anchored to the real
code it will change and carries the threat argument for why it exists.

The 0.6 permissioning pillar already ships the enforcement machinery: a
`capability_grants` / `capability_enforcement` pair (migration `0042`), a resolver
(`internal/api/capabilities.go`), and per-seam narrowing of a member's inline policy
(`internal/api/inline_policy.go`, `runs_create_validate.go`). This design does NOT
rebuild any of that. It adds the one thing a *desktop* member deployment needs and the
current model does not have: a member who **owns** workspaces, and can mount their own
local directories into a run **without** an admin authoring the mount — under a hard,
operator-set root prefix that no member input can widen.

---

## Vocabulary, so the threat arguments are unambiguous

- **Operator** — the admin authority. In code, `isOperator(ctx)` returns true
  (`internal/api/http.go:391`): an admin-token / local-mode caller (a shared credential
  with no per-human role), or an OIDC session whose derived role is `oidc.RoleAdmin`.
- **Member** — a signed-in OIDC human whose derived role is `oidc.RoleMember`. Gets
  403 from `requireOperator` (`http.go:364`) on every admin-gated route; may read, may
  launch/own runs, may author a *clamped* inline policy.
- **`operatorOnly`** — the chi sub-group in `routes.go:90` carrying the
  `requireOperator` gate. 34 registrations sit on it (the count is pinned in the
  `routes.go:79-83` maintenance note).
- **Ownership-404** — the pattern `getRunAuthorized` (`helpers.go:213`) /
  `ownsRunOrAdmin` (`helpers.go:201`) use: a non-owner non-admin caller gets the
  **byte-identical** 404 a truly-missing row would, never a 403 — no existence oracle.

---

## (a) OPERATOR authority on a member-mode local daemon

### Topology m′ (member-mode desktop) vs topology a′ (developer=operator)

The sibling **W-DESK** design owns **topology a′**: the developer *is* the operator.
That is today's default — local host mode, `WARDYN_LOCAL_MODE=true`,
`humanOrAdminAuth` bypassed for a loopback peer (`http.go:248-296`), and every
admin-gated action attributed to `local:operator`. The single-dev machine trusts its
own user; there is no member at all. **a′ applies whenever one human owns the box and
its policy.**

This design owns **topology m′**: the developer on the desktop is a **member**, and the
operator authority is *elsewhere* — an org, an MDM, an IdP — not the person at the
keyboard. m′ applies whenever the box is org-managed and the developer must not be able
to reconfigure their own sandbox governance.

The three authority anchors for m′, each contrasted with a′:

| Authority | a′ (developer=operator, today) | m′ (member-mode desktop, this design) |
|---|---|---|
| **Config authority** | The developer's own env / flags to `cmd/wardynd`. | **MDM-managed config** is the operator authority. `WARDYN_LOCAL_MODE` MUST be `false`; `WARDYN_OIDC_*` and the new `WARDYN_MEMBER_WORKSPACE_ROOTS` (section c) are MDM-set and not developer-writable. |
| **Identity → role** | Local mode: no identity, caller *is* admin (`isOperator` true, `http.go:392-394`). | The **org IdP** (an OIDC *variant profile*) authenticates the developer, and `deriveRole` (referenced in `http.go:348-359`) maps them to `oidc.RoleMember`. `WARDYN_OIDC_ROLE_MAP` / the operator-emails allowlist are MDM-set; the developer is on neither, so they derive `RoleMember`. |
| **Admin surface reach** | The developer holds the admin token / is the loopback operator, so the whole `operatorOnly` group is theirs. | The admin surface is reachable **only via an org-held credential** the developer does not possess: the admin bearer (`WARDYN_ADMIN_TOKEN`) is MDM-injected and not developer-readable, and no `RoleAdmin` OIDC session is available to the developer. The developer reaches `operatorOnly` routes only through the member-owned-scoped subset carved out in section (b). |

**Threat argument for m′.** The whole point is that the human with physical/root access
to the desktop must *not* be the governance authority. If m′ let the developer reach any
`operatorOnly` route, they could rewrite `default.json`, add an `allow_all_egress`
policy, grant themselves any secret, or widen the mount root — defeating every clamp the
member path relies on (`composer.Clamp`, `filterMemberGrants`, `narrowMemberInlinePolicy`).
So the load-bearing invariant is: **on a member-mode daemon, `isOperator(ctx)` is false
for the developer's every request.** That already holds *if* three preconditions are
MDM-enforced, and M1 must fail-closed check them at boot:

1. `LocalMode == false` — otherwise `humanOrAdminAuth` bypasses auth and makes the
   loopback developer an admin (`http.go:248`, `isOperator` true at `http.go:392`).
2. The admin token is not disclosed to the developer's session context — it is a
   process credential, never surfaced to the browser UI.
3. OIDC is configured and the developer's derived role is `RoleMember`.

**ponytail:** M1 adds ONE boot guard (a `cmd/wardynd` refuse-or-warn, mirroring the
existing `LocalMode`-on-public-IP refusal noted at `http.go:243-247`) — a
`WARDYN_MEMBER_MODE=true` that *refuses to start* if `LocalMode` is also true or OIDC
is unconfigured. No new auth middleware: the role split in `http.go`/`routes.go` already
does the enforcement; member-mode only asserts the preconditions under which it is real.

---

## (b) MEMBER workspace ownership

### The column: migration `0047` (reserved)

```sql
-- 0047_workspace_owned_by.sql (RESERVED — authored in M2)
ALTER TABLE workspaces ADD COLUMN owned_by TEXT NOT NULL DEFAULT '';
CREATE INDEX IF NOT EXISTS workspaces_owned_by_idx ON workspaces (owned_by);
```

(Migrations `0044`–`0046` belong to sibling lanes; member-role takes `0047` per the M0
task. Highest on this branch today is `0043_ssh_key_role.sql`.)

`owned_by` is the lowercased OIDC `sub` **or** email of the creating member — the same
dual-key identity a `capability_grants` `user` subject already uses
(`0042_capability_grants.sql:8-9`), so an admin who knows either identity can reason
about ownership. `''` (the default) means **operator-owned** — every workspace that
exists today, and every workspace an admin creates, is `owned_by=''` and behaves exactly
as it does now. This is the back-compat story, identical in spirit to
`capability_enforcement`'s "absent row = not enforced" (`0042:59-63`): a 0.5→0.6 upgrade
changes nothing until a member creates their first owned workspace.

### The authorization predicate — reuse the ownership-404 pattern verbatim

Add the workspace twin of `ownsRunOrAdmin` (`helpers.go:201`):

```go
// ownsWorkspaceOrAdmin — the workspace analog of ownsRunOrAdmin.
func (s *Server) ownsWorkspaceOrAdmin(r *http.Request, ws types.Workspace) bool {
    return s.isOperator(r.Context()) || ws.OwnedBy == principalFromRequest(r)
}
```

and the `getWorkspaceAuthorized` twin of `getRunAuthorized` (`helpers.go:213`): load,
then if `!ownsWorkspaceOrAdmin` return the **byte-identical 404** a missing workspace
would (`notFoundIf(w, err, "workspace")` shape, already used at `helpers.go:258`) plus an
`authz.denied` audit with `reason: not_owner`. **No 403 on a foreign owned workspace** —
same no-existence-oracle argument as runs (`helpers.go:208-212`): probing another
member's workspace id must learn nothing.

**Threat argument.** A member enumerating `/workspaces/{id}` must not be able to tell
"exists but not yours" from "does not exist" — otherwise a member learns another
member's workspace ids and can correlate them with audit/run metadata. The run path
already closes this; ownership must close it the same way or it is the weaker door to the
same room.

### Scans and grants bind per owner

- A member's **scan** (`POST /workspaces/{id}/scan`) runs against a workspace they own,
  and the scan run is created with the member as `CreatedBy` (so the scan run itself is
  owner-scoped by the existing run ownership). The scan's `WorkspaceID` linkage
  (`newWorkspaceStepRun`, `workspace_run.go:138`) is the trusted server-side binding — no
  change to how facts upload.
- **`capability_workspace` grants** stay the operator's deployment-wide narrowing knob
  and are unchanged: `capWorkspace` (`capabilities.go:38`) gates the workspace a member
  may *launch a run against* (`denyMemberRequest`, `runs_create_validate.go:248-258`).
  Ownership is a **stronger, additive** relation, not a replacement: an owned workspace
  is one the member both created and (trivially) is granted, because
  `ownsWorkspaceOrAdmin` short-circuits before the capability seam is consulted. An
  operator-owned workspace still needs a `capWorkspace` grant for a member to launch
  against it. **The two never conflict: ownership is checked first and only ever grants;
  the capability seam still governs operator-owned workspaces.**

### Every `operatorOnly` workspace route — which become member-owned-scoped

The workspace block is `routes.go:225-264`. Classified:

| Route | Handler | Disposition under m′ | Why |
|---|---|---|---|
| `POST /workspaces` | `handleCreateWorkspace` | **member-allowed, stamps `owned_by`** | A member creating a workspace is the on-ramp; the created row is owner-stamped. Source is validated as member-safe (section c) — a member `local_dir` must be under a member root. |
| `GET /workspaces` | `handleListWorkspaces` | **member-scoped list** | Returns the member's own owned workspaces ∪ operator-owned ones they hold a `capWorkspace` grant for. Never another member's owned rows. |
| `GET /workspaces/{id}` | `handleGetWorkspace` | **owner-or-admin (404)** | `getWorkspaceAuthorized`. |
| `PUT /workspaces/{id}` | `handleUpdateWorkspace` | **owner-or-admin** | A member edits their own workspace; a foreign one 404s. |
| `DELETE /workspaces/{id}` | `handleDeleteWorkspace` | **owner-or-admin** | Same. |
| `POST /workspaces/{id}/scan` | `handleScanWorkspace` | **owner-or-admin** | A member scans their own source. |
| `GET /workspaces/{id}/build` | `handleGetWorkspaceBuild` | already member-tier read | Stays; scoped by ownership for owned rows. |
| `POST /workspaces/{id}/build` | `handleBuildWorkspace` | **owner-or-admin** | Building a member's own workspace image; the image builder runs Wardyn-generated recipes, not member free-text (contrast `devcontainer_repo`, section d). |
| `GET /workspaces/{id}/observed-egress` | `handleObservedEgress` | **owner-or-admin** | Telemetry about the member's own runs. |
| `GET /workspaces/{id}/env-as-code` | `handleGetEnvAsCode` | **owner-or-admin** | Re-generates from the scanned profile; owner-readable. |
| **`PUT /workspaces/{id}/approved-egress`** | `handleSetApprovedEgress` | **STAYS operator-only** | Promotes scanner suggestions into a permanent allowlist widening — a member widening their own egress ceiling is exactly the escalation the clamp exists to prevent. |
| **`PUT /workspaces/{id}/denied-egress`** | `handleSetDeniedEgress` | **STAYS operator-only** | Revocation / bricking a workspace's egress — an operator control. |
| **`PUT /workspaces/{id}/llm-cred`** | `handleSetWorkspaceLLMCred` | **STAYS operator-only** | Binds model/harness credential MATERIAL to a workspace — credential authority, never member (mirrors the secret-write posture, section d). |
| **`PUT /workspaces/{id}/requirements`** | `handleSetWorkspaceRequirements` | **STAYS operator-only** | The requirements contract folds admin-authored egress/secret grants into every run of the workspace (`applyWorkspaceRequirements`) — a member authoring it would re-add the very grants `narrowMemberInlinePolicy` (`inline_policy.go:209`) drops. |
| **`POST /workspaces/{id}/record`** + `.../promote-egress` | `handleRecordWorkspace`, `handlePromoteRecordEgress` | **STAYS operator-only** | Record Mode launches an OPEN-egress (`AllowAllEgress`) learning sandbox (`workspace_run.go:282`) and promotes observed hosts into `ApprovedEgress` — an egress-widening authority. |
| **`POST /workspaces/{id}/env-as-code/write`** | `handleWriteEnvAsCode` | **STAYS operator-only** | Writes generated files into the host source dir — a host-write authority. |

**Rule the table encodes:** a route becomes member-owned-scoped iff it only ever
*reads*, *builds from Wardyn-generated recipe*, or *edits the member's own onboarding
metadata*. Every route that **widens an egress ceiling, binds credential material, or
writes the host** stays operator-only. A member owning a workspace does not make them the
operator of it.

**ponytail:** `owned_by` gates in the handlers, not a second middleware group. The
`operatorOnly` registration for the demoted routes is *dropped* (they move to the `r`
group) and the ownership check moves *inside* the handler as `getWorkspaceAuthorized` —
exactly how runs already do owner-or-admin inside the handler while sitting on the plain
`r` group (`routes.go:98-117`). No new route group, no chi.Walk matrix churn beyond the
reclassification `authz_test.go` already enumerates.

---

## (c) MEMBER-SAFE `local_dir` — the hard security core

This is why the milestone is design-first. Everything else is CRUD scoping; this is the
one place a member supplies a **host path** that gets bound into a sandbox, and getting
it wrong is a host-filesystem read (or, on a writable mount, write) outside the member's
own tree.

### What exists today (the floor we build on)

`runner.ValidateMountSource` (`mount.go:94`) already: rejects non-absolute / uncleaned
paths (defeats lexical `..`), rejects a host-root/`/etc`/`/proc`/docker-socket deny-list
(`deniedSourcePrefixes`, `mount.go:55`), **and resolves symlinks** — `filepath.EvalSymlinks`
at `mount.go:114`, then **re-runs the deny-list on the real path** (`mount.go:115`),
fail-closed on any resolve error other than not-exist. This runs at policy-write time
*and* again in the docker driver at bind time (`driver.go:573`, the per-mount loop in
`agentMounts`). The bind-time residual TOCTOU is already documented honestly at
`mount.go:127-133`: validate and `ContainerCreate` are not atomic.

Today mounts are **operator-authored only** — the whole point of
`composer.Clamp` dropping `WorkspaceMounts` unconditionally (`clamp.go:288-292`) and of
`buildRunMounts`/`agentMounts`'s "the agent-run entrypoint never sees this surface"
argument (`runs_dispatch_mounts.go:17-27`, `driver.go:532-536`). Member-safe `local_dir`
is the first path where a *non-operator* supplies the source — so it needs a stricter
gate than the operator deny-list, additively.

### The three additive checks

**1. Root-prefix allowlist (`WARDYN_MEMBER_WORKSPACE_ROOTS`).** A new operator/MDM-set
list of absolute host prefixes. A member `local_dir` source is allowed **only** if its
canonicalized real path is within one of these roots. This is the compose file's own
warning (`docker-compose.yaml:412-416` — "DO NOT set it to your home directory. wardynd
would then be able to read ~/.ssh, ~/.aws, ~/.claude…") turned from prose into an
enforced allowlist. Empty list ⇒ **member `local_dir` mounts are unavailable** (fail
closed; a member can still onboard repos and use operator-owned workspaces). Operators
point it at a dedicated projects dir, never `$HOME`.

**2. Canonicalize + re-check within a root AT BIND TIME.** New in `runner`:

```go
// ValidateMemberMountSource — additive to ValidateMountSource, for a MEMBER-owned
// local_dir bind. Runs the full operator deny-list first (reuse), THEN:
//   real := filepath.EvalSymlinks(filepath.Clean(src))   // fail-closed on error
//   - real must be within one of `roots` (real == root || HasPrefix(real, root+"/"))
//   - real (and every path component) must not hit the dotfile deny-list (check 3)
// The within-root test is on the RESOLVED real path, so a symlink INSIDE a root that
// points OUT of every root is refused — the escape a lexical prefix check misses.
func ValidateMemberMountSource(src string, roots []string) error
```

**The exact bind-time site:** `driver.agentMounts`, the per-mount loop at
`driver.go:572-582`, immediately after the existing `runner.ValidateMount(m)` at line
573 and **before** the `mounts = append(mounts, mount.Mount{...})` that hands the bind to
`ContainerCreate`. A member run threads its roots through a new
`SandboxSpec.MemberMountRoots []string` (nil for operator/non-member runs — the driver
then does *exactly* today's behavior, so the change is additive and cannot widen an
operator mount). When non-nil, each mount is additionally run through
`ValidateMemberMountSource(m.Source, roots)` at that same site, fail-closed. This is the
canonical-just-before-bind re-check the residual argument turns on: `EvalSymlinks` here
resolves what the daemon is about to bind, and the within-root assertion sits as close to
the `ContainerCreate` call as the existing deny-list does.

**Why at bind time and not only at create-run:** the same reason `ValidateMount` is
re-run in the driver even though it ran at policy-write (`driver.go:537-542`) — a symlink
that was benign when the workspace was onboarded can be repointed before the run. The
resolved-real-path within-root check has to be the *last* thing before the bind.

**3. Dotfile deny-list — the compose warning as code.** A new suffix/basename deny-list
checked against the resolved real path and its components, refusing a source that IS or
traverses any of: `.ssh`, `.aws`, `.claude`, `.wardyn` (the staged `claude-creds`),
`.gnupg`, `.docker`, `.kube`, `.config/gh`, `.netrc`, `.git-credentials`, and any
`.git/config`. Modeled on `deniedSourcePrefixes` (`mount.go:55`) but **suffix/segment**
matching (these live under a member's home, not at fixed absolute paths). This is
belt-and-suspenders with the root allowlist: an operator who *does* misconfigure a root
to include `$HOME` still cannot let a member mount their own `~/.ssh`.

**Additivity is the invariant.** `composer.Clamp` KEEPS dropping every mount not on this
path (`clamp.go:288-292`) — the member-safe mount is authored on a member's *owned
workspace* through the CRUD path (section b), not through an inline policy or a composer
proposal, both of which still have their mounts dropped. So this widens nothing about the
operator-only mount rule; it opens exactly one new, tightly-bounded authoring surface
(member-owned workspace onboarding) and gates it harder than the operator surface.

### Residual, bounded honestly

A determined member with **local root** on the desktop can still win the TOCTOU rename
race the operator path already documents (`mount.go:127-133`): swap the source for a
symlink between the `EvalSymlinks` at the bind site and the daemon's actual bind. We do
**not** claim to defeat that — it is the same non-atomic window operator mounts have. Two
things bound the blast radius so it stays acceptable:

1. **The roots are operator/MDM-set, not member-set.** The widest a member can aim the
   race is *within the declared roots* — the member cannot enlarge the target set, only
   race within it. An operator who sets a narrow projects dir bounds the damage to that
   dir regardless of the race.
2. **The dotfile deny-list is checked on the resolved real path**, so even a won race
   that lands on `~/.ssh` inside a too-wide root is refused by check 3 unless the attacker
   *also* defeats the deny-list — and the deny-list match is on the post-`EvalSymlinks`
   real path, closing the "symlink to `.ssh`" variant.

The un-bounded residual is a member who is **already local-root on a box whose operator
set a reckless root (e.g. `/`)** — which is an operator misconfiguration the compose
warning and the fail-closed empty default both steer hard against, and which M1's boot
guard can additionally refuse (open question O4). A member with local root and a sane
root prefix is bounded to that prefix minus its dotfiles.

---

## (d) OPERATOR-ONLY, unchanged

No member reach, no ownership scoping — these stay on `operatorOnly` exactly as today:

- **Secret writes/deletes** — `PUT/DELETE /secrets/{name}` (`routes.go:275-276`). Credential
  MATERIAL; the LIST stays names-only (`routes.go:277`). A member's own effective secret
  *references* are still gated by `capSecret` at the inline-policy seam, not by a write.
- **Base images / source library** — `mountLibraryRoutes` (`routes.go:223`), the tier-2
  base-image catalog. Operator-curated build inputs.
- **`devcontainer_repo`** — unconditionally operator-only and **not a capability kind at
  all** (`runs_create_validate.go:207-210`, `capabilities.go:41-45`). It hands
  attacker-authored build configuration to the image builder; not a power to grant one row
  at a time. A member-owned workspace's `base_image` is Wardyn-generated recipe, never a
  member `devcontainer_repo`.

---

## M1–M5 task breakdown

| Milestone | Scope | Key files | Test it owes |
|---|---|---|---|
| **M1** | Member-mode boot preconditions. `WARDYN_MEMBER_MODE` flag; `cmd/wardynd` refuse-or-warn if `LocalMode` true or OIDC unconfigured; assert admin token not surfaced to session ctx. No new middleware. | `cmd/wardynd`, `internal/api/http.go` (assertions only) | Boot refuses on `MEMBER_MODE && LocalMode`; a member session has `isOperator==false`. |
| **M2** | `0047_workspace_owned_by.sql`; `types.Workspace.OwnedBy`; store scan/param plumbing; `ownsWorkspaceOrAdmin` + `getWorkspaceAuthorized`; reclassify the section-(b) routes; owner-scoped `handleListWorkspaces`; stamp `owned_by` in `handleCreateWorkspace`. | `internal/db/migrations/0047_*`, `internal/store/store_workspaces.go`, `internal/api/{helpers,workspaces,routes}.go` | **The adversarial matrix below.** |
| **M3** | `WARDYN_MEMBER_WORKSPACE_ROOTS` config; `runner.ValidateMemberMountSource`; dotfile deny-list; `SandboxSpec.MemberMountRoots`; the bind-time check in `driver.agentMounts` (`driver.go:572`); member-safe source validation in `handleCreateWorkspace`. | `internal/runner/mount.go`, `internal/runner/docker/driver.go`, `internal/runner/types` (SandboxSpec), `internal/api/workspaces.go`, `cmd/wardynd` | symlink-escape / `..` / TOCTOU / dotfile-deny unit tests (below). |
| **M4** | Thread member roots from create-run through dispatch for a member-owned-workspace run; ensure `narrowMemberInlinePolicy` (`inline_policy.go:288`) and `capWorkspace` interplay is right for owned workspaces (ownership short-circuits the seam). | `internal/api/{runs_create,runs_dispatch_mounts,workspace_run}.go` | A member-owned-workspace run binds only within a root; an owned run's mount survives, a foreign one 404s at create. |
| **M5** | UI: member sees "My workspaces"; onboarding a `local_dir` shows the root constraint; admin Permissions screen unchanged (capability grants already there). Docs: OPERATIONS §Multi-user, THREAT-MODEL member-mount section. | `ui/`, `docs/OPERATIONS.md`, `docs/THREAT-MODEL.md` | e2e: member onboards a dir under a root, launches, cannot reach a foreign workspace. |

### `0047` migration shape (M2, restated for the owner)

```sql
ALTER TABLE workspaces ADD COLUMN owned_by TEXT NOT NULL DEFAULT '';
CREATE INDEX IF NOT EXISTS workspaces_owned_by_idx ON workspaces (owned_by);
```

`DEFAULT ''` = operator-owned = today's behavior. No `CHECK` (an identity string, same
rationale as `capability_grants.subject` carrying no check, `0042:17-23`). The
`owned_by_idx` serves the owner-scoped `handleListWorkspaces` read.

---

## Authz matrix (the rows M2/M3 add or change)

| Actor | Route / action | Outcome | Enforced by |
|---|---|---|---|
| member | `POST /workspaces` (repo, or `local_dir` under a root) | 201, `owned_by`=member | `handleCreateWorkspace` stamps owner; `ValidateMemberMountSource` on `local_dir` |
| member | `POST /workspaces` `local_dir` **outside** every root | 400 | member-safe source validation |
| member | `POST /workspaces` `local_dir` = `~/.ssh` (inside a too-wide root) | 400 | dotfile deny-list |
| member | `GET/PUT/DELETE /workspaces/{id}` — **own** | 200 | `getWorkspaceAuthorized` |
| member | `GET/PUT/DELETE /workspaces/{id}` — **another member's owned** | **404** (byte-identical to missing) | `getWorkspaceAuthorized`, no existence oracle |
| member | `GET /workspaces` | own owned ∪ granted operator-owned | owner-scoped list |
| member | `PUT .../approved-egress` \| `.../denied-egress` \| `.../llm-cred` \| `.../requirements` \| `.../record` \| `.../env-as-code/write` | **403** | `requireOperator` (unchanged) |
| member | run against **own** owned workspace | launches, mounts within root | `denyMemberRequest` short-circuited by ownership; bind-time root check |
| member | run against operator-owned workspace **without** `capWorkspace` grant | 403 (kind enforced) / allowed (kind unenforced) | `denyMemberRequest` (`runs_create_validate.go:248`) unchanged |
| admin | any of the above | allowed | `isOperator` true |
| member-mode daemon | developer request | `isOperator==false` always | M1 boot preconditions |

---

## Adversarial test matrix (M2/M3 owe every row RED-before / GREEN-after)

Mount-source tests are unit tests in `internal/runner` (no docker, no daemon — the
existing `mount_test.go` shows the pattern, including the remote-daemon note at
`mount_test.go:95`). Ownership-404 tests mirror `authz_test.go` / the run ownership tests.

1. **symlink escape** — a source *inside* a root that is a symlink to `/etc` (or to a dir
   outside every root): `ValidateMemberMountSource` resolves via `EvalSymlinks` and refuses
   (real path not within a root, and/or deny-list). RED if the check is lexical-only.
2. **`..` escape** — `<root>/../../etc`: refused by the existing uncleaned-path check
   (`mount.go:101`) *and* by within-root on the cleaned real path. Belt-and-suspenders.
3. **TOCTOU rename** — assert the check runs on the resolved real path *at the bind site*
   (`driver.go:572`), not only at create-run; document the residual window as the bounded,
   accepted one (matches `mount.go:127-133`). Test the *ordering* (validate-then-append),
   not the race itself (unwinnable to test deterministically) — i.e. a test that fails if
   `ValidateMemberMountSource` is moved before `EvalSymlinks`-freshness or after the append.
4. **foreign-owner 404-parity** — a member GET/PUT/DELETE on another member's owned
   workspace returns the **byte-identical** status+body a truly-missing id returns; assert
   equality against the missing-id response, and assert the `authz.denied reason=not_owner`
   audit fires only for the *existing* foreign row, never the missing one (mirrors
   `helpers.go:222-227`).
5. **dotfile deny under a wide root** — with a root set to `$HOME`, every entry of the
   dotfile deny-list is refused; with a narrow projects root, a normal project dir is
   allowed.
6. **empty roots = fail closed** — `WARDYN_MEMBER_WORKSPACE_ROOTS` unset ⇒ a member
   `local_dir` onboarding is refused (repos still work); an operator `local_dir` is
   unaffected (nil `MemberMountRoots`, existing path only).
7. **additivity** — an operator run (nil `MemberMountRoots`) binds a legitimate mount
   *outside* the member roots successfully — the member gate never narrows operator mounts.

---

## OPEN QUESTIONS for the owner (verbatim)

- **O1.** `WARDYN_MEMBER_WORKSPACE_ROOTS` — one shared list for all members, or per-member
  roots (e.g. each member restricted to their own `/home/<user>/projects`)? A shared list is
  simplest and matches the single-`WARDYN_WORKSPACES_ROOT` compose model; per-member roots
  need an identity→root mapping and are a bigger config surface. Design above assumes ONE
  shared operator/MDM-set list. Which?
- **O2.** On a member-mode desktop, does the developer authenticate through a full org IdP
  round-trip (real OIDC), or is there a lighter "MDM asserts identity" variant profile? The
  design assumes real OIDC with `deriveRole`→member; if there's an MDM-token identity path,
  where does the role come from and does `isOperator` still see it as a non-admin session?
- **O3.** Should a member's owned `local_dir` mount be **read-only by default with no
  writable opt-in at all** for members (operators keep the `Writable` opt-in), or may a
  member mark their own dir writable? `wireWorkspaceSource` already honors `src.Writable`
  (`workspace_run.go:531-544`, default read-only). A member writing to their own project dir
  is the common case, but a writable bind widens the residual. Default posture?
- **O4.** Should M1's boot guard **refuse** to start member-mode when
  `WARDYN_MEMBER_WORKSPACE_ROOTS` contains `/` or `$HOME` (bounding the section-c residual at
  boot), or only warn (matching the `LocalMode`-on-unspecified-bind *warn* posture at
  `http.go:243-247`)? Refuse is safer; warn matches precedent.
- **O5.** When an admin later needs to act on a member's owned workspace (support,
  offboarding), `isOperator` already grants full access via `ownsWorkspaceOrAdmin` — but do
  we want an **audit distinction** for "admin acted on a member-owned workspace" beyond the
  normal action audit, so cross-user admin access to member data is reviewable? (Runs have no
  such distinction today.)
- **O6.** Offboarding: when a member leaves, their owned workspaces are orphaned
  (`owned_by` points at a gone identity). Do we need a reassign-to-operator (`owned_by=''`)
  admin action in M2, or is admin's existing full access (they can already GET/DELETE any
  owned workspace) sufficient for 0.6?
