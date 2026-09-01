# Permissioning — admin surface + member why-denied moments

0.6 pillar 2, from ROADMAP.md: *"RBAC grows past admin/member into a permissioning system: admins
grant users and groups specific Wardyn capabilities (egress hosts, secrets, workspace/base images).
The bare minimum an enterprise POC needs."*

This is the mock round for that surface — the design gate before any console code (owner law: the
mock is UI source of truth; canon strings are app strings). The backend it renders is designed in
full in the 0.6 plan's Workstream A; nothing here is open for re-design, only for drawing.

Static mock: `docs/design/permissioning-mock/index.html` (open it in a browser).
Frozen strings: `ui/src/app/lib/permissions-copy.ts` — the table in §7 below, transcribed. The
implementation stage (A-E) imports that module; it does not retype copy from this document.

---

## 1. What exists today (the thing being grown)

Two roles, derived once at login and stamped into the session cookie: **admin** and **member**
(`internal/auth/oidc`'s `deriveRole`; OPERATIONS.md §"Multi-user: who can change what"). A member
launches and governs only their own runs — nav is **Runs · Approvals**, nothing else. Everything
else is `operatorOnly`. The admin token and local mode are always admin, because both are a single
shared credential with no per-human identity to key a role off.

Where a member's power currently ends, verbatim from the code:

| Seam | Today |
|---|---|
| `authorizeMemberDecision` (approvals.go) | A member may decide `egress_domain` approvals **on their own runs**. Credential and tool_call stay admin-only. |
| `resolveRunPolicy` member branch (inline_policy.go) | A member-authored policy is clamped to the operator ceiling; drops emit a warning. |
| `denyMemberCustomImage` (runs_create_validate.go) | A member may **never** name their own base image. |
| `handleListSecrets` | A member sees the full stored-secret name list. |

0.6 adds a bounding layer above those four seams. It does not move any of them.

## 2. The model, in the terms the screen must teach

**Four capability kinds**, exactly the roadmap's list, closed set:
`egress_host` · `secret` · `workspace` · `image`.

**Grants** are rows: *(who, capability, value, allow|deny)*.
*Who* is a **user** (matched on email **or** sign-in subject — either hits), a **group** (any
`roles`/`groups` claim value from the ID token, so Entra App Roles are grantable for free), or
**all** (every signed-in human).

**Enforcement is per kind**, its own switch, off by default. Absent switch = that kind is not
checked = today's member powers byte-for-byte. An upgrade from 0.5 with zero config behaves exactly
like 0.5.

**Resolution order** (`capAllowed`), and the screen must make it predictable:

1. admin / admin token / local mode → allowed, always. Not bounded, not shown a grant.
2. any matching **deny** → refused. (Deny is checked *before* the enforcement switch.)
3. any matching **allow** → allowed.
4. kind **not enforced** → allowed.
5. otherwise → refused.

Two consequences the copy has to carry, because both surprise people:

- **Deny beats the switch.** A deny bites while the capability is still off. That is deliberate —
  it is the adoption on-ramp: block one host for one contractor without going fail-closed for
  everyone.
- **No user-over-group precedence.** A deny anywhere wins. "Bob's user allow overrode the group
  deny" is a breach report, not a feature.

**`image` widens; the other three narrow.** Members are denied custom images *today*, so an `image`
grant hands power out and enforcing that kind can never lock anyone out. The other three take power
away, and enforcing one with no grants locks members out of it completely. The enforcement control
must read differently for the two directions — see §4.3.

## 3. The doctrine (the line the whole surface is built to protect)

> **A capability bounds what the MEMBER chose, never what the ADMIN pre-authorized.**

A member's *own* typed egress list is intersected with their `egress_host` grants. Egress that came
from the workspace's requirements, a stored policy, or a scan is **not** narrowed — narrowing
admin-authored egress would brick workspace runs at scale. Same shape for secrets. Design this
sentence onto the screen (it is `PERM.DOCTRINE`); it is the single most common misread of the
feature and a support ticket every time it is missing.

## 4. Surfaces to design

### 4.1 Where it lives

A new **admin-only screen, `/permissions`**. Decide and state its home: the six-item sidebar is a
settled surface (Runs · Approvals · Workspaces · Policies · Secrets · Audit) and Settings is the one
home for *connections*, which this is not. A seventh sidebar item beside Policies is the obvious
read — argue it or argue the alternative, don't leave it implied. Member nav is unchanged:
**Runs · Approvals**, no permissions entry, no empty screen behind a hidden route.

### 4.2 The screen's spine

One page, three blocks, in this order:

1. **Header** — title, lead, and the two standing facts: the doctrine sentence (`PERM.DOCTRINE`) and
   the exemption (`PERM.EXEMPT`). These are facts, not alerts — quiet, like the role chip in the
   account menu, not a banner.
2. **Enforcement** — the four kinds as cards or rows, each with its own switch, its state chip
   (`Not enforced` / `Enforced`), and the concrete consequence line for the state it is *currently*
   in (`KIND[k].unenforced` / `KIND[k].enforced`). The consequence sentence is the point of the
   block; the switch is the smaller half.
3. **Grants** — one table, all kinds, filterable by kind, plus the add form.

### 4.3 States to draw (all of them)

| # | State | What it is |
|---|---|---|
| 1 | **Fresh install** | Four kinds off, zero grants. `PERM.DEFAULT_POSTURE`. The most common first view and the one that must not look broken or unfinished. |
| 2 | **Grants, still advisory** | Rows exist, kind off. `PERM.ADVISORY` on the kind, and the deny exception (`PERM.DENY_BEFORE_ENFORCE`) said where a deny row is visible. |
| 3 | **Enforced** | Kind on, rows biting. |
| 4 | **Mixed** | The realistic steady state — e.g. `egress_host` enforced, `secret` advisory, `workspace` off, `image` enforced-to-widen. Draw this one; a screen that only ever renders all-on or all-off hides the layout problem. |
| 5 | **Switch-on confirm, narrowing kind** | `PERM.ENFORCE_ON_TITLE` + `PERM.ENFORCE_ON_BODY(n)` with the affected-member count. |
| 6 | **Switch-on confirm, zero grants** | `PERM.ENFORCE_ON_ZERO` — the lockout guard. This is the top risk in the whole workstream. It must be impossible to walk past. |
| 7 | **Switch-off confirm** | `PERM.ENFORCE_OFF_BODY` — and it says denies still apply, because they do. |
| 8 | **Add-grant form** | Who (user/group/all) → capability → value → effect. The value field's label and hint change per kind (`KIND[k].valueLabel` / `.valueHint`). Wildcards are typed, not a separate control. |
| 9 | **Deny row** | Visually distinct from allow, and distinct from "disabled" — a deny is *active*, and it is the strongest row on the page. |
| 10 | **Duplicate** | `PERM.DUPLICATE`, on an upsert that hit an existing (subject, capability, value). |
| 11 | **Empty grants, kind enforced** | The dangerous cell: enforced with nothing granted. `PERM.EMPTY_TITLE` / `PERM.EMPTY_BODY`. |
| 12 | **Stale group snapshot** | `PERM.SNAPSHOT_STALE` beside a member who signed in pre-0.6, and `PERM.SNAPSHOT_TRUNCATED` where their group list overflowed the session. |

### 4.4 Member why-denied moments

0.6 ships **no member permissions screen**. `GET /me/capabilities` exists so a pane is a small
follow-up, but the member's whole experience of this feature is four inline moments. Design each as
a delta on the screen that already exists — no new member surfaces.

| Moment | Screen | Shape |
|---|---|---|
| **Can't approve this host** | Approvals (`PendingCard`) | Approve/Deny disable exactly as they already do for admin-only kinds, with `DENIED.APPROVE_CHIP` beside them and `DENIED.APPROVE_BODY(host)` as the reason. Reuse the existing disabled-plus-chip pattern; do not invent a second one. Also: `DENIED.ALWAYS_STILL_ADMIN` where the Always scope is offered — a grant does not lift that. |
| **Can't launch here** | New Run, workspace step | The list is **not** narrowed — visibility is not capability. Ungranted rows carry `DENIED.WORKSPACE_CHIP` + `DENIED.WORKSPACE_BODY` and refuse at launch. |
| **Can't bring an image** | New Run, base-image step | `DENIED.IMAGE_BODY`. |
| **Something was dropped** | Preflight / Review warnings | `DENIED.SECRET_DROPPED(n)` and `DENIED.EGRESS_DROPPED(n)`. The run still launches — these ride the warning lane that already exists, and `EGRESS_DROPPED` says outright that workspace-carried hosts are untouched (the doctrine, at the point of confusion). |

Plus two quieter ones: the member's Secrets list gets `DENIED.SECRETS_NARROWED` when `secret` is
enforced, and `DENIED.STALE_GROUPS` appears where a member's session predates group recording.

## 5. Honesty rules (product law — copy may move, never soften)

1. **A grant is amber, never green.** Repo law, and it lands hardest here: an allow row is a widened
   blast radius, not an achievement. `PERM.GRANT_IS_NOT_SUCCESS` is the sentence; amber is the
   styling. No green checks on the grants table, no "✓ Granted" success toast.
2. **Never render enforcement that isn't happening.** A kind that is off says so, in those words
   (`Not enforced`), next to every grant it governs. An advisory grant that *looks* live is the
   worst failure this screen can have — an operator would believe they had bounded a contractor.
3. **Visibility is not capability.** Ungranted workspaces stay listed and annotated. Hiding them
   would make the refusal unexplainable and the grant undiscoverable.
4. **The snapshot ceiling is stated, not hidden.** Group membership is read at sign-in
   (`PERM.SNAPSHOT_BODY`). Grants are per-request. Those are two different latencies and the screen
   says both.
5. **Say what a denial costs, at the moment it happens.** Every member-side string names the
   capability, the value, and the way out ("ask an admin to grant…"). No bare 403, no "contact your
   administrator" with nothing else in it.
6. **A dropped requirement is a warning, not a silent success.** `SECRET_DROPPED` follows the
   existing unmet-requirement grammar — the run starts, and whatever needed it fails there.
7. **Admin exemption is stated once, plainly** (`PERM.EXEMPT`) — not discovered by an admin
   wondering why their own grant does nothing.
8. Existing status vocabulary is settled: `Ready`, `Needs setup`, `Unavailable here`,
   `Incompatible here`, `Checking…`, `Connected`, `Unverified`. Do not fork it, and do not invent a
   synonym for `Not enforced` / `Enforced`.

## 6. Do not design

- No member permissions screen, no member nav entry. Inline moments only in 0.6.
- No per-run or per-approval grant control. Grants are admin-authored on this screen only.
- No user-vs-group precedence UI. There is no precedence — a deny anywhere wins.
- No `devcontainer_repo` kind. It stays unconditionally admin-only (it executes attacker-authored
  build config) and is not a grantable capability.
- No fifth kind, no free-text capability field. The set is closed.
- No changes to the Approvals card layout, the New Run step list, or the six-item sidebar's existing
  entries — only the deltas named in §4.4.
- No caching/staleness UI beyond the group-snapshot lines. Grants resolve per request.

## 7. Canonical strings — FROZEN

Byte-exact source of truth: `ui/src/app/lib/permissions-copy.ts`. Screens import; they never retype.

### 7.1 Per kind — `KIND[kind]`

| kind | `label` | `direction` | `blurb` |
|---|---|---|---|
| `egress_host` | Egress hosts | narrows | Which hosts a member may approve for their own run. |
| `secret` | Secrets | narrows | Which stored secrets a member's run may reference. |
| `workspace` | Workspaces | narrows | Which workspaces a member may launch a run against. |
| `image` | Base images | **widens** | Which base images a member may name on a run of their own. |
| `agent` | Agents | narrows | Which agents a member may launch a run with. *(0.7 ADDITION, not from this round — see below.)* |
| `integration` | Model providers | narrows | Which model provider a member may name on a run of their own. *(0.7 ADDITION.)* |

| kind | `valueLabel` | `valueHint` |
|---|---|---|
| `egress_host` | Host | A host, or *.suffix for a domain and everything under it. Use * for every host. |
| `secret` | Secret name | The exact secret name. Use * for every secret. |
| `workspace` | Workspace | One workspace. Use * for every workspace. |
| `image` | Image ref | The exact image ref, registry and tag included. Use * for every image. |
| `agent` | Agent | The exact agent id, spelled as --agent takes it. Use * for every agent. |
| `integration` | Integration | The exact integration id. Use * for every integration. |

**`unenforced`** — the off-state body, per kind (this is the row that proves the default posture is
0.5 byte-for-byte):

| kind | `unenforced` |
|---|---|
| `egress_host` | Members can approve any host their own run asks for, and add any host to a run they launch. |
| `secret` | Members can reference any stored secret their run's ceiling already allows. |
| `workspace` | Members can launch a run against any workspace. |
| `image` | Members can't name their own base image at all. Runs use what the workspace carries. |
| `agent` | Members can launch a run with any agent this deployment carries. |
| `integration` | Members can name any model provider integration on a run they launch. |

**`enforced`** — the on-state body, per kind:

| kind | `enforced` |
|---|---|
| `egress_host` | A member can only approve hosts granted to them. A host they add to a run they launch is dropped before it starts, with a warning naming it. |
| `secret` | A member's run can only reference secrets granted to them. Ungranted names are dropped before launch, and the Secrets page lists only what they hold. |
| `workspace` | A member can only launch against workspaces granted to them. The rest stay listed — a run against one is refused at launch, with the reason. |
| `image` | A member can name an image granted to them. Every other ref is still refused. |
| `agent` | A member can only launch agents granted to them. A run naming another one is refused at launch, with the reason. |
| `integration` | A member can only name providers granted to them. A workspace's own provider and the site default still apply — a grant bounds what the member chose, never what an admin set up for them. |

**The two 0.7 ADDITIONS** (`agent`, `integration`) land here rather than in a
governance-prompt addendum, the same way `ENFORCE_OFF_TITLE` lands in §7.2
below: governance's own §7.1 declares permissions copy *referenced, never
re-frozen*, so `ui/src/app/lib/permissions-copy.ts` stays the single home for
`KIND` rows and this table stays the single canon of them. Both NARROW, for the
reason `capGranted` documents — a widening kind refuses on `!enforced`, which
would refuse every member run on every deployment that has not enforced it.
`integration`'s `enforced` string carries the doctrine in-line on purpose: the
kind bounds the `integration_id` a member typed and nothing else, so a
workspace's own pin and the operator's site default keep applying no matter what
the member holds.

### 7.2 Admin surface — `PERM`

| Key | String |
|---|---|
| `TITLE` | Permissions |
| `LEAD` | Grant members and groups specific Wardyn capabilities. Each capability is enforced on its own — until you enforce one, nothing about it changes. |
| `DOCTRINE` | A capability bounds what a member chose, never what an admin pre-authorized. Egress a workspace, a stored policy, or a scan already carries is never narrowed by a grant. |
| `EXEMPT` | Admins, the admin token, and local mode are never bounded by these rules. |
| `ENFORCEMENT_TITLE` | Enforcement |
| `ENFORCEMENT_LEAD` | Turn a capability on to start refusing what isn't granted. |
| `CHIP_OFF` | Not enforced |
| `CHIP_ON` | Enforced |
| `DEFAULT_POSTURE` | Nothing is enforced yet. Members have exactly the powers they had before this screen existed, and adding a grant on its own changes nothing. |
| `ADVISORY` | Advisory until enforced. These grants are recorded and shown here, but nothing is refused while this capability is off. |
| `DENY_BEFORE_ENFORCE` | A deny applies even while this capability is not enforced — you can block one host for one person without bounding everyone. |
| `ENFORCE_ON_TITLE(kind)` | Enforce {kind}? |
| `ENFORCE_ON_BODY(n)` | {n} members are bounded by the grants below from their next request. Anything not granted starts being refused. *(singular: "1 member is bounded…")* |
| `ENFORCE_ON_ZERO` | There are no allow grants for this capability. Enforcing it now refuses every member request until you add one. |
| `ENFORCE_OFF_TITLE(kind)` | Stop enforcing {kind}? *(0.6 ADDITION, not from this round: the table froze only one title, so the off-dialog asked "Enforce {kind}?" above a body about going back and a "Stop enforcing" button.)* |
| `ENFORCE_OFF_BODY` | Members go back to the powers they had before this capability was enforced. Denies still apply. |
| `ENFORCE_CONFIRM` | Enforce |
| `ENFORCE_STOP` | Stop enforcing |
| `GRANTS_TITLE` | Grants |
| `COL_WHO` | Who |
| `COL_CAPABILITY` | Capability |
| `COL_VALUE` | Value |
| `COL_EFFECT` | Effect |
| `COL_ADDED` | Added |
| `EFFECT_ALLOW` | Allow |
| `EFFECT_DENY` | Deny |
| `SUBJECT_USER` | User |
| `SUBJECT_GROUP` | Group |
| `SUBJECT_ALL` | Everyone signed in |
| `PRECEDENCE` | A deny always wins — over an allow on the same person, over a group they're in, and over this capability being unenforced. |
| `EMPTY_TITLE` | No grants yet |
| `EMPTY_BODY` | Add a grant, then enforce its capability. Enforcing with nothing granted refuses everything. |
| `REMOVE` | Remove |
| `REMOVE_CONFIRM(who)` | Remove this grant from {who}? They lose it on their next request. |
| `ADD_TITLE` | Add a grant |
| `ADD_CTA` | Add grant |
| `FIELD_WHO` | Who |
| `FIELD_CAPABILITY` | Capability |
| `FIELD_VALUE` | Value |
| `FIELD_EFFECT` | Effect |
| `HINT_USER` | An email address or the sign-in subject id. Either one matches the same person. |
| `HINT_GROUP` | A group or app-role name exactly as your identity provider sends it in the token. |
| `HINT_ALL` | Every signed-in member. Admins are exempt. |
| `DUPLICATE` | That grant already exists — its effect was updated. |
| `SNAPSHOT_TITLE` | Groups are read at sign-in |
| `SNAPSHOT_BODY` | Group membership is recorded once, when a member signs in. A group added in your identity provider reaches Wardyn on their next sign-in. Grants themselves take effect on the next request. |
| `SNAPSHOT_STALE` | This member signed in before Wardyn recorded groups, so group grants can't reach them. Grant their user directly, or ask them to sign in again. |
| `SNAPSHOT_TRUNCATED` | This member is in more groups than fit in their session. Grants on the groups shown work; prefer granting an app role or their user directly. |
| `GRANT_IS_NOT_SUCCESS` | Every allow below is something a member can reach that they otherwise couldn't. |

### 7.3 Member why-denied — `DENIED`

| Key | String |
|---|---|
| `APPROVE_CHIP` | Not a host you're granted |
| `APPROVE_BODY(host)` | {host} isn't in the hosts granted to you, so you can't decide this one. An admin can approve it, or grant you the host. |
| `ALWAYS_STILL_ADMIN` | Always is admin-only, even for a host you're granted. |
| `WORKSPACE_CHIP` | Not granted |
| `WORKSPACE_BODY` | A run against this workspace is refused at launch. Ask an admin to grant it to you. |
| `IMAGE_BODY` | You can't name your own base image. Ask an admin to grant the exact image ref. |
| `SECRET_DROPPED(n)` | {n} secrets aren't granted to you and won't be attached. Whatever needs them will fail at that point. *(singular: "1 secret isn't … Whatever needs it will fail…")* |
| `EGRESS_DROPPED(n)` | {n} hosts you added aren't granted to you and were removed from this run. Hosts this workspace already carries are unaffected. *(singular: "1 host you added isn't … was removed…")* |
| `SECRETS_NARROWED` | Only secrets granted to you are listed. |
| `STALE_GROUPS` | You signed in before Wardyn started recording your groups. If a permission looks missing, sign out and back in. |

## 8. Design system (unchanged, restated for self-containment)

Gemini colorway, neutral monochrome surfaces, teal the only action color; light **and** dark both
required. Inter for UI, JetBrains Mono for anything machine-literal — **secret names, host patterns,
image refs and workspace ids on this screen are all machine-literal**. Radius `0.5rem` base. lucide
icons at 14–16px.

| Token | Light | Dark |
|---|---|---|
| background / card | `#ffffff` / `#ffffff` | `#0a0a0a` / `#111111` |
| surface-2 / muted | `#f4f4f5` | `#1f1f1f` |
| foreground | `#171717` | `#ededed` |
| muted-foreground | `#737373` | `#a3a3a3` |
| border | `#e5e5e5` | near-black equivalent |
| primary (teal) | `#0f766e` | `#14b8a6` |
| success | `#047857` | `#10b981` |
| warning (amber) | `#92400e` | `#f59e0b` |
| danger | `#b91c1c` | `#dc2626` |
| info | `#1d4ed8` | `#3b82f6` |

Allow grants are **warning/amber**. Deny grants are **danger/red**. Nothing on this screen is
success/green — there is no green state in a permissions grant table.

## 9. Where to apply

- `/permissions` — the new admin screen, every state in §4.3.
- Approvals `PendingCard` — the disabled-decide delta (§4.4).
- New Run workspace step and base-image step — the annotation deltas (§4.4).
- Preflight / Review warning lane — the two dropped-requirement lines.
- Secrets (member view) — the narrowed-list line.

Report anything where the layers leak: an enforcement control that implies a grant is already
biting, a grants table that renders green, a member surface that hides a workspace instead of
explaining it, or copy that suggests a grant narrows egress the workspace itself declared.
