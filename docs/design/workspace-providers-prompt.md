# Workspace providers — the admin's org policy over git hosts, storage and agents; the member's inline moments

This is the mock round for the Workspace Providers surfaces of 0.7.2 — the design gate before any
console code (owner law: the mock is UI source of truth; canon strings are app strings). The model
is decided in `~/.claude/plans/crispy-wondering-raven.md` (v17, FINAL; §3 D1–D11, §4, §5, §5c, §6,
§9) and nothing here is open for re-design, only for drawing. **Every string in §7.2–§7.7 is a
DRAFT the owner freezes at this gate**; seven drawing-level calls are Q1–Q7 in §9 (the plan's), with
the ones this round surfaced after them, and an Adjudication section at the end for owner answers.

One round covers every surface at once (the drives precedent: one prompt + one static mock, strings
byte-exact, Q-numbered owner calls, Adjudication at the end):

1. the **admin's** `/providers` screen (a new screen, SUPER-only, no nav item) — three tabs, **Git ·
   Storage · Agents**, the funnel step `providers` and the Settings card render one shared CARD that links into it,
2. the **security admin's** door — two numeric limit rows in the governance profile editor
   (`MaxEphemeralDiskMiB`, `MaxDriveSizeMiB`; the strings are U2's, drawn here so the state exists),
3. the **member's** inline moments (no new screen — the Getting Started `Model access` chip in its
   six states, the New Run agent picker, the run-detail "Effective policy" widget, the workspace
   row's "not an enabled provider" state, and the refusals), and
4. **canon frozen here, shipped later** — the server-composed refusals each backend lane carries in
   ONE Go constants block (§7.1's second table, §7.4), the drives rows that land through OTHER prompt docs (§2.8),
   and the field-report strings the M2 sitting adjudicates (§7.6, staging).

Static mock: `docs/design/workspace-providers-mock/index.html` (Git · Storage · card · workspace row),
`agents.html` (Agents tab · member chip · agent picker · effective policy), `canon.html` (refusals ·
field-report strings · the two drives states) — open any in a browser; they share `mock.css`.
Frozen strings: §7 below. No TS copy module exists yet. The implementation stage creates
`ui/src/app/lib/workspace-providers-copy.ts` **from §7 verbatim**; it does not retype copy from this
document, and every product string in the mock matches §7 byte-for-byte. Its test,
`workspace-providers-copy.test.ts`, clones `user-drives-copy.test.ts`'s `parseFrozenTables()` over
§7.2–§7.7 of this file — which is why every table from §7.2 on is exactly two columns, `Key` and
`String`, and why §7.1 is not: it holds the reused canon AND every admin-facing / run-time string
the server composes this round, unparsed and checked against the Go source instead (the drives
test's second `describe`, §7.1). **§7.6 is a STAGING table** (field-report strings owned by other lanes): U1's clone either excludes it
(`/^### 7\.[2-57]\b/`) or the rows have moved to their homes before U1 lands — see §7.6.

---

## 1. What exists today (the thing being grown)

The symbols are the durable half; the line numbers are not. They were checked once against
`release/0.7 @ 87624b49` (v0.7.1, the tree the plan cites) — resolve the symbol, re-locate the line
at implementation.

**Every mechanism this needs exists as a separate seam, and no object names which are on.** Git
credential lanes per host are `GitHostCard` (`settings/connection-cards.tsx`, one host at a time;
secrets `git-pat-<slug>` / `ssh-key-<slug>` / `github-app-*`); "provider rows" are derived
client-side from `SiteConfig.ScmHosts` + secret names (`lib/scm-provider.ts`, `brandFor`) — the file
itself says Wardyn stores no such thing and exposes no provider API. The never-resident PAT broker,
the GitHub App broker, ADO's SSH-over-443 endpoint and egress bundle, and the org-level upstream
proxy that MDM delivers in `/etc/wardyn/site-config.json` all ship. Nothing admits a repo URL against
an org allowlist: any host with a credential is clonable.

**Storage is two unconnected halves.** User drives (0.7.0) give per-person reusable disks with
user/group/all allocation and per-tier overrides (`/drives`, `user-drives-prompt.md`). The ephemeral
scratch a run gets when it mounts no drive is `Resources.DiskMiB` — on Docker a three-way (enforced
on xfs+pquota/btrfs/zfs, fails closed on overlay2-over-anything-else, warns-and-runs-uncapped on
vfs/fuse-overlayfs, `runner/docker/hardening.go`); **on Kubernetes warned and ignored**
(`runner/k8s/sandbox.go`). No admin default or ceiling for scratch exists anywhere, and no org
ceiling bounds a drive's size: an allocation override can exceed anything.

**Agents are an image map with no auth semantics.** `WARDYN_AGENT_IMAGES` says which agents exist;
the harness catalog (`internal/api/harness.go`) says which model providers each CAN use; the New Run
agent picker is two hard-coded literals (`new-run-screen.tsx`, `Claude Code` / `Codex CLI`) over the
`WizardAgent` union whose own comment says to read a catalog endpoint once one ships. Model
credentials are admin-global: `resolveBedrockAuth` reads the operator namespace unconditionally, one
admin's captured SSO session credentials every member's runs, and when it lapses dispatch falls
through to the Anthropic API-key lane the admin never configured. The member's Getting Started
chip (`member-getting-started.tsx`, `MODEL_ACCESS_PROVIDED_CHIP`) reads `llm_ready` — "a provider
is configured", not "it works".

**Governance profiles ship.** The profile editor's Limits section (`governance/profile-editor.tsx`)
renders `LimitRow`s — a `Switch` over a boolean — for `DenyTaskModeExec`, `DenyInteractive`,
`DenyUserDrive`. No numeric limit row exists yet.

**The setup funnel's ORDER rule.** `setup/steps.ts` records why `corp_network` earned a step: later
steps look broken without it. Providers pass the same test — with `azure_devops` disabled,
onboarding an ADO repo on the Workspaces step is refused by admission, and the Workspaces step looks
broken when the missing thing is a policy row one step earlier — so this round draws a **step**, in phase "Your work", before `workspaces` (Q1 asks whether it
should sit in Essentials instead). The step's BODY is the card (§7.5): its footer Next is already
the step's one `default` button, so the forms and their teal live on `/providers` — the drives
round's shape.

**Settings summarises and links.** Five cards — Host · Model provider · Git host · SSH keys · Drives
(`settings/settings-screen.tsx`). Where a card's subject has a real home elsewhere it delegates
through a two-line link button. The Git host card retires in this round (§2.1, Q4); a Providers card
takes its place.

**How a member is refused today.** A capability door is a 403 composed server-side (`denyMemberField`,
`runs_create_validate.go`), audited as `authz.denied`; "authorised and simply nothing there" is a 422
with no audit. Both bodies are `writeError` text — a lowercase-opening clause naming the wire field —
and **the console renders them verbatim** (the New Run rail's `launch.warnings` rule). §7.4's server-composed
tables are written in that shape for that reason.

**The 201 warnings and the Review rail.** New Run's Review rail already renders the composer's
clamp tightenings one line each, or "No adjustments." (`new-run-rail.tsx`); after launch the same
warnings were a sonner toast (`run-warnings.ts`) and gone, and run detail rendered only `policy_id`
(`run-detail/widgets/identity.tsx`). A member cannot tell "the ceiling narrowed this" from "the
product ignored my input" (§4c C5).

### 1.1 What this round REUSES rather than builds

- **The git credential lanes are `GitHostCard`'s, unchanged** — `Lane`, `SecretLane`, `HostSummary`
  (`connection-cards.tsx`, module-private today, EXPORTED, not re-typed) render INSIDE the provider
  row; their titles and hints ("Personal access token", "SSH key", "GitHub App", "Access token",
  "Private key", "App ID", "Private key (PEM)", the store note) are byte-for-byte the card's.
- **The lane honesty is `CAPABILITY.brokerLine / gitPatLine / sshKeyLine`** (`wardyn/copy.ts`), the
  tooltips `LANE_META` (`scm-provider.ts`) already carries under the chip labels `App · brokered` /
  `PAT · in-sandbox` / `SSH · resident` — the permitted-lanes checkboxes render those three chips.
- **The tab's honesty line is `S.GIT_FOOTER`** (`connection-cards.tsx`), quoted in §7.1 and rendered
  verbatim under the Git tab. No `HONESTY` key exists in this module (§5 #1).
- **The kind labels are `brandFor`'s** (`scm-provider.ts`): "GitHub" and "Azure DevOps" — frozen in
  §7.2 as `KIND_*` because a provider row exists whether or not a secret does.
- **The subject vocabulary is `/permissions`'s** — `PERM.SUBJECT_* / HINT_* / REMOVE`; a
  `workspace_provider` capability grant is authored on `/permissions` with the kind's six `KindCopy`
  strings (M2 sheet, `permissions-copy.ts`) — nothing here.
- **The enforcement vocabulary is the drives round's** — `DRIVES.ENFORCEMENT_*`
  (`user-drives-copy.ts`), total over `StorageEnforcement`; the fifth word `eviction` gets its ONE
  row in `user-drives-prompt.md` §7.2 with U1 (§2.8), never here.
- **The drives card is `UserDrivesCard`** (`setup/user-drives-card.tsx`) — the Storage tab's link out
  to `/drives`; no drives table is embedded (Q7).
- **The tab control is `Segmented`** (`permissions.tsx`); the switch is `Switch`; every field is
  `Field` (`form-primitives.tsx`); `EmptyState`, `ErrorState`, `TableSkeleton`, `PageHeader`, `Chip`,
  `OperatorOnlyHint` (`OPERATOR_ONLY_REASON`, "Requires the admin role.") — the shipped primitives;
  nothing new in `wardyn/`.
- **The sign-in is `HarnessLoginPane`**, unchanged — the AWS flow ("Connect an AWS SSO session via
  container login", `harness-login-pane.tsx`) is what a member's "Sign in to AWS" opens.
- **Refusals ride today's paths** — the 403/422 render, the launch toast, the run's `failure_hint`.
- **The Review rail's list is the effective-policy widget's list** — the same server warnings
  (`clamp_warnings`, persisted on the `run.create` audit datum), the rail's "No adjustments." when
  empty.

## 2. The model this round mocks

### 2.1 A git provider

A **git provider** is one row on `SiteConfig.WorkspaceProviders.git`: `{id, kind, disabled?,
base_urls[], lanes[]}` — org policy, MDM-deliverable, SUPER-edited, zero DDL (plan D1). Two kinds
in v1, `github` and `azure_devops` (D2); the zero value of `disabled` is enabled; `base_urls` are
HTTPS prefixes (`https://github.com/<org>`, `https://dev.azure.com/<org>`,
`https://<org>.visualstudio.com`, a GHES/ADO-Server host over HTTPS only); `lanes` is a subset of
`app` / `pat` / `ssh`, empty meaning every lane the kind supports. **The tab renders one row per
kind, always.** A kind has three states the row must tell apart: **absent** (no row — its host
follows the legacy `scm_hosts` list, admitted if listed there), **present and on** (its base URLs
admit; a repo outside them is refused), **present and off** (the admin said "off": its host is
refused — it never falls through to legacy). A row is removed by PUTting the document without it
(`PERM.REMOVE` on the row, confirmed); there is no delete route.

**Zero rows = legacy open mode**, decided in one Go predicate (`providersConfigured`): any host with
a stored credential is clonable, exactly as today, and the tab says so in a banner
(`LEGACY_OPEN_*`, Q6). An upgraded 0.7.1 install is byte-identical until an admin adds a row.

**The existing credential lanes move INTO the row** (D2). `GitHostCard` retires (Q4): its free-text
`Host` field could store `git-pat-<slug>` for a host no provider admits — a credential that clones
nothing. The three `Lane`s render unchanged under the row's own host; the Secrets step keeps
`ModelProviderCard` only and keeps its id and label (Q5). A GitLab / Bitbucket PAT (no v1 kind) is
rotated through the Secrets page until 0.8's kinds land — the legacy banner says so.

**Admission** (D3) runs on the derived clone URL at every place a repo URL reaches a clone or a
mint — eight sites, the workspace Build step and the legacy `--repo` field included. A member is
additionally gated by the new `workspace_provider` capability (deny-first, like the other six).
Providers mint nothing; they VETO a lane: a grant for a lane the row does not permit is dropped
with a warning on the 201 (`ADMIT_LANE_DROPPED`, §7.4), never silently.

### 2.2 An agent provider (the Agents tab)

An **agent provider** is one row on `SiteConfig.AgentProviders.agents`, shaped byte-for-byte on
§2.1 (D11): `{id, disabled?, mechanism, credential_source, sso_start_url?}`. `id` is a
harness-catalog id (`claude-code`, `codex-cli`, `none`) or a `WARDYN_AGENT_IMAGES` key — for a
custom image the ONLY valid mechanism is `none`, because no code path can wire a model credential
into it. `mechanism` is ONE of today's lanes — `anthropic_subscription`, `anthropic_api_key`,
`openai_api_key`, `bedrock_bearer`, `bedrock_sso`, `bedrock_env`, `bedrock_aws_dir`, `none` — folded
to the catalog's four coarse values for validation, so an impossible pair (Codex CLI + an Anthropic
lane) is refused with the catalog's EXISTING verbatim reason (§7.1). `credential_source` is
`shared` (today: one credential, captured by an admin, backing everyone) or `per_user` (each
person signs in to AWS themselves) — **`per_user` is permitted for `bedrock_sso` only** in 0.7.2,
and then `sso_start_url` is REQUIRED and admin-owned: a member's login run ignores any start URL in
the request and uses this one.

**Block absent ⇒ byte-for-byte today.** Block present ⇒ run create refuses an agent with no enabled
row (`AGENT_NOT_ENABLED`, §7.7; the record launcher too), and dispatch refuses a model run whose
declared mechanism is dead **without substituting another** (`LLM_MECHANISM_DEAD`, §7.7 — the C2
fix). A renewable AWS SSO session is refreshed control-plane-side at dispatch (C1); nothing is
refreshed for a setup-token, a host `~/.aws` mount, or a browser session — each says "sign in again".

**The member-safe carrier is `SetupStatus.harnesses`** (three new per-row fields `enabled`,
`mechanism`, `credential_source`; the row count is unchanged and the server never filters the
list). The New Run picker reads it: a disabled row renders **disabled with `AGENTS.UNAVAILABLE`,
never hidden**; while the roster is absent or the fetch failed the picker keeps today's two
literals and claims NOTHING unavailable (§2.7).

### 2.3 A storage provider

`SiteConfig.WorkspaceProviders.storage` is two optional halves (D4, plan §6.0b): `ephemeral:
{default_disk_mib, max_disk_mib}` and `user_drive: {disabled?, max_size_mib}`. nil = legacy for
that half. Per-tier overrides ride `GovernanceLimits` — `MaxEphemeralDiskMiB`, `MaxDriveSizeMiB`
(0 = unlimited), user/group/all = user/team/org — as two numeric rows in the profile editor (U2).

**Ephemeral.** `default_disk_mib` FILLS a run that requests no `disk_mib`; `max_disk_mib` and the
profile's `MaxEphemeralDiskMiB` CLAMP a non-zero request to `min()` at ONE site, dispatch — a run
with no request and no default stays unbounded, so the maximum binds requests, not silence. On
Kubernetes the number becomes the agent container's `ephemeral-storage` LIMIT (with a 256Mi
request floor); over it the kubelet EVICTS the pod — the write is never refused — and the run fails
naming the limit. That is a fifth honest enforcement word, **`eviction`**, surfaced on
`/setup/status`'s runner block as `EphemeralDiskEnforcement` and rendered under the field through
`DRIVES.ENFORCEMENT_*`. On Docker `default_disk_mib` means exactly what `disk_mib` means today —
enforced on a quota-capable driver (`filesystem`); **an org FILL never fails a run closed**: on
overlay2-over-ext4 (Docker Desktop, WSL2, stock Ubuntu, the desktop tier MDM delivers to) a filled
value runs uncapped with the word `none` and a run warning, while a POLICY-authored `disk_mib`
still fails the create there as today. The tab says so under all three disk fields
(`DOCKER_UNCAPPED_WARN`) whenever the authoring daemon's driver is not `filesystem`.

**Drives.** `max_size_mib` is refused **422 at the admin write boundary** (drive size, allocation
override) and clamped at resolve for the per-principal governance ceiling; on external backends
(`host_path`, `k8s_pvc_static`) it clamps a DISPLAYED number that binds nothing, which
`MAX_DRIVE_HINT` and `CEILING` say. `user_drive.disabled` is the ORG switch, checked before the
per-profile door: `/drives` shows a banner, drive and grant writes answer 422 "drives are disabled
for this deployment" (never 403 — nobody was denied by a profile), a run carrying `drive.enabled`
answers 422 in the `REFUSED_BACKEND` family. **The drives honesty sentence does not change**
(`DRIVES.HONESTY`, frozen in seven places); this round adds ONE sentence beside the ceiling field
only, `CEILING`.

### 2.4 Lanes and residency

Nothing about where a credential lives changes. The three chips and their tooltips are the
product's honesty canon, imported: `App · brokered` (`CAPABILITY.brokerLine` — the token stays in
Wardyn), `PAT · in-sandbox` (`gitPatLine` — handed to git inside the sandbox), `SSH · resident`
(`sshKeyLine` — a private key file the sandbox can read). `S.GIT_FOOTER` under the tab owns the
split so no lane overclaims. The `app` lane is **unavailable on Azure DevOps** (the App broker mints
repository-scoped GitHub tokens on github.com only) — drawn disabled WITH its reason, never hidden
(Q3). The `ssh` lane is available on `github.com` / `dev.azure.com` (SSH-over-443); port-22 SSH to a
self-hosted GHES/ADO Server is a documented ceiling and its checkbox is disabled with the same
shape. On the Agents tab, credential residency is the model card's footer (`S.MODEL_FOOTER`) —
Bedrock SSO signs inside the sandbox; the plan drops the three refresh fields from the sandbox
cache so the control plane is the one refresher.

### 2.5 What the console refuses, and what it only warns about

- **A base URL that is not an HTTPS prefix — REFUSED (400).** Drawn pre-attempt: `aria-invalid` +
  `BASE_URL_INVALID` under the field, a client mirror of the server rule checked BEFORE a
  credential is written (the `hostError` shape, `scm-provider.ts`); the server's own 400s
  (`PROVIDERS_400_*`, §7.1) are drawn post-attempt under `SAVE_REFUSED_TITLE`, verbatim.
- **A lane the kind cannot carry — never offered and refused:** the checkbox is disabled with
  `LANE_APP_UNAVAILABLE`; the API path's 400 (`PROVIDERS_400_LANE`) is drawn post-attempt.
- **`default_disk_mib` above `max_disk_mib` — REFUSED (400)** ("you wrote this wrong"), post-attempt.
- **Someone saved since you loaded — REFUSED (412).** The console sends the GET's `ETag` as
  `If-Match` on every PUT; a stale one renders `SAVED_ELSEWHERE_*` — reload, never overwrite.
- **Narrowing is never silent.** The PUT answers `sources_no_longer_admitted`; the save toast names
  the count (`SAVED_NARROWED`), and every affected workspace row renders `CARD_NOT_ADMITTED` from
  the server's per-source `admitted` flag (the `user_drive_unavailable` precedent).
- **A repo outside every enabled provider — REFUSED at admission.** An OPERATOR's 422 lists the
  allowed addresses (`ADMIT_OPERATOR`, §7.1); a MEMBER's names only the kind and never a base URL
  (`ADMIT_MEMBER`, §7.4 — base URLs are corporate topology, SUPER-readable only). A lane the row does not
  permit is a **warning** on the 201, not a refusal (`ADMIT_LANE_DROPPED`); an unclaimed legacy
  host admits with a warning (`ADMIT_LEGACY_HOST`) — both §7.1.
- **An agent with no enabled row — REFUSED (4xx) at run create**, `AGENT_NOT_ENABLED`; the picker
  already rendered it unavailable. **A dead declared mechanism — REFUSED at dispatch**,
  `LLM_MECHANISM_DEAD`, and at create/Review as a 422 for the same run; the proxy's `brokered:llm`
  detail explains itself and says it is below policy (`LLM_UNAVAILABLE_*`, §7.1).
- **A drive above the ceiling — REFUSED (422)** at the admin write, `DRIVE_CEILING`; **drives off —
  REFUSED (422)**, `DRIVE_DISABLED` (both §7.1).
- **A size the host cannot enforce — never refused, only stated** (`DOCKER_UNCAPPED_WARN`, and the
  enforcement gloss on every number).

### 2.6 The member's inline moments

The 0.6 doctrine holds: **no member providers screen, inline moments only.** Member nav is
unchanged; `/providers` answers a member with `OperatorOnlyHint`.

- **Getting Started, "What's set up for you"** — the `Model access` chip stops reading `llm_ready`
  and reads the per-principal `SetupStatus.ModelAccess{state, mechanism, action}`: success tone
  ONLY for `live`; warning for the rest, with the action as the chip's own line under the row
  (§7.7). `Sign in to AWS` opens `HarnessLoginPane` in place, against the admin's start URL. Under
  `shared` the chip is today's `MODEL_ACCESS_PROVIDED_CHIP` when the operator's credential is live,
  and `MODEL_ACCESS_SHARED_EXPIRED` with its action when it is not — a green chip over a dead
  credential is the state this round removes.
- **New Run, the Agent picker** — read from `harnesses`; a disabled row is a disabled item with
  `UNAVAILABLE` as its reason line (the Workspace card's "the reason rides the row" shape, drawn as
  the item's own sub-line because the item is never selectable). **Roster unknown = today's two
  literals, nothing claimed unavailable.** The JSON policy field gains one hint when
  `min_confinement_class` names no class (`FLOOR_UNPARSEABLE`) — precedence is unchanged.
- **Launch** — the 201 warnings render INLINE under the launch control instead of a toast, under the
  existing title "Run launched with a warning".
- **Run detail** — a new widget "Effective policy" beside the identity rail's Policy row: one line
  per tightening from the persisted `clamp_warnings`, `No adjustments.` when empty.
- **The workspace row** — `CARD_NOT_ADMITTED` on `/workspaces` and in the New Run Workspace
  `<Select>` (one line, the selection's own rule), from the server's `admitted` flag.
- **Refusals** — `ADMIT_MEMBER`, `AGENT_NOT_ENABLED`, `LLM_MECHANISM_DEAD` rendered verbatim on
  the launch path; `DRIVE_*` 422s on the drive path.

### 2.7 States

- **Populated** — rows for both kinds, one on and one on with a narrower lane set; the Storage tab
  filled; the Agents tab with a `per_user` row.
- **Legacy open mode (zero rows)** — the `EmptyState` banner carrying the action that fills it.
- **Row absent / row on / row off** — three row states, the last dimmed and collapsed, never removed.
- **Base URL invalid** — pre-attempt.
- **Lane unavailable** — `app` on Azure DevOps, disabled with its reason (Q3's losing variant: absent).
- **Fetch failed** — distinct from empty; the providers already saved keep binding every run.
- **Write refusals** — the 400s post-attempt under `SAVE_REFUSED_TITLE`; the unreachable-server arm.
- **Saved elsewhere** — the 412.
- **Saved, narrowed** — the toast with the count; the workspace row's state.
- **Member** — nothing; **security admin** — `OperatorOnlyHint`, and the two limit rows on
  `/governance`.
- **Agents** — enabled with `shared`; enabled with `per_user` (start URL shown); disabled row; a
  custom-image row (mechanism `none` only); an impossible pair disabled with the catalog's reason;
  the admin's own chip in each state.
- **Member chip** — `live` · `expiring` · `expired_signin` · `not_configured` · `shared` live ·
  `shared_expired` (`expired_renewable` renders as `live`: nothing for the person to do).
- **Agent picker** — roster unknown; roster arrived with a disabled row; the unparseable-floor hint.
- **Effective policy** — with tightenings; with none.
- **Drives** — the re-home confirm dialog; the managed-backend editor (drives-mock State 3b).
- **No "managed by your org" read-only state**: on desktop m′ the developer is never an operator,
  and on a′ the MDM file re-applies over console edits every tick with no server fact recording it —
  documented in `DESKTOP.md`, not badged.

### 2.8 Canon frozen here, shipped later

- **Member DOOR refusals** (`PROVIDER_MEMBER`, §7.4) are frozen the way `DRIVE_MEMBER`'s are in
  `user-drives-prompt.md` §7.7 — parsed into the module and byte-checked against the Go literal.
  **Every admin-facing refusal and run-time detail** is frozen in §7.1's second table the way the
  drives round's host-root 400s are — unparsed, rendered from the wire, checked against the Go
  source one-way, and never carried by the module. Each lives in ONE Go constants block per lane
  (A1 `PROVIDERS_400.*`, A3 `ADMIT.*`, S2 `DRIVES.*`, G1 `INJECT.REQUIRE_TLS`, C2/C3 the agent
  refusals, B-β the two egress sentences) so the post-gate swap is a one-file diff.
- **`DRIVES.ENFORCEMENT_EVICTION`** — "Size limited by eviction — over it, the run is stopped, not
  the write" (DRAFT) — lands as ONE new row in `user-drives-prompt.md` §7.2 with U1 (its copy test
  moves 140 → 141). This document renders the word through `DRIVES.ENFORCEMENT_*` and freezes no
  enforcement string.
- **The re-home dialog's one row** lands in `user-drives-prompt.md` §7.4 with U3 (141 → 142); it is
  drafted in §7.4 here so the state can be drawn.
- **The six numeric limit keys** (`LIMIT_EPHEMERAL_*`, `LIMIT_DRIVE_SIZE_*`, `LIMIT_CONCURRENT_*`)
  land in `governance-prompt.md` §7.2 with U2 (90 → 96); drafted in §7.6 so the security-admin
  state has words.
- **The `workspace_provider` `KindCopy`** (six strings) is the M2 sheet's and lands in
  `permissions-copy.ts` with A2 — not drawn here (the permissions screen is unchanged in shape).

## 3. Do not design (out of scope this round)

- **No nav item.** Entry is the funnel step, the Settings card. **No second Workspaces-header
  button** — the header's `actions` slot already holds the drives button (Q2).
- **No GitLab / Bitbucket kind.** Closed set, two kinds (`PLUGGABILITY.md` says so); their PATs live
  on the Secrets page until 0.8.
- **No team-shared drives** (D5, 0.8), no drives table on the Storage tab (Q7 — the move
  `steps.ts` forbids), no change to `/drives` beyond the off-banner.
- **No cross-mechanism fallback opt-in** (0.8), **no per-user bearer/API keys** (0.8), **no
  background credential renewer** (dispatch-time refresh only), **no mid-run renewal** (0.8).
- **No "managed by your org" badge** (§2.7).
- **No `email_local` on a managed drive** — drives-mock State 3b draws the editor's mirror rule; it
  is not a new control.
- **No re-record** of demo videos; the Secrets step keeps its id `integrations` and label (Q5).
- **No dry-run endpoint** for narrowing — the count comes back on the PUT.

## 4. Design system

Same token block and CSS idioms as `docs/design/user-drives-mock/index.html` (`--background` /
`--card` / `--surface-2` / `--foreground` / `--muted-foreground` / `--border` / `--border-strong` /
`--primary` / `--success` / `--warning` / `--danger` / `--info`, light and dark, Inter + JetBrains
Mono, `chip`/`btn`/`sw`/`seg`/`dlg`/`note`/`card` idioms; the four rungs, no ad-hoc sizes —
`CONSOLE-RULES.md` §3), lifted into one shared `mock.css` because the mock is three files.

**Teal is in budget on `/providers`** — a full screen, so exactly **one** `default` button at a
time: **Save providers** (each tab is one form over one document; the button sits under the active
tab); in the legacy-open empty state it is the banner's own action. Everything else is `outline` or
`ghost`; **Remove** (a row) is `outline` with a confirm — it deletes no secret. The member's **Sign in to AWS** is the one teal on Getting Started's summary card when it is the first thing
not done (the page's existing colour budget); **on the Agents tab the admin's own chip action is
`outline`** — that tab's teal is Save providers. **Zero teal on the funnel step** (its footer Next is
the step's one affirmative and its body is the card, the drives round's shape), **on the Settings
card, and on the security admin's door.**

Colour, stated per rule:

- **A lane chip keeps its own tone** — `App · brokered` success, `PAT · in-sandbox` info,
  `SSH · resident` warning (`LANE_META`, the residency honesty) — never recoloured by the checkbox.
- **Off is neutral** — a disabled row, an absent row, an unavailable lane, a disabled agent are facts
  with the word on them, never red.
- **Amber and red carry genuine risk and error only:** write refusals, the 412, the narrowed-sources
  toast, `CARD_NOT_ADMITTED`, every member refusal, fetch-failed, a dead credential's chip.
- **`Model access` is success ONLY for `live`** — `expiring` is warning (an action exists), every
  other state warning; **never neutral for a dead credential**, and never green over one.
- **Mono is for literals only:** base URLs, hosts, wire values (`bedrock_sso`, `per_user`), secret
  names, env var names, the `disk_mib` field, MiB numbers in inputs. A provider KIND and an agent's
  display name are plain.
- **The kind column is a kind label over the row's hosts in mono** — the drives round's
  KIND-over-wire two-glyph rule.

## 5. Hard canon constraints

1. **`S.GIT_FOOTER` is the honesty line, imported** — rendered verbatim under the Git tab; this
   module freezes no `HONESTY` key and no second wording of where a credential goes.
2. **No member-facing string names a base URL, a host list, a secret name, or another person.**
   `ADMIT_MEMBER` names the kind; `ADMIT_OPERATOR` lists addresses for operators only. The
   `DRIVE_CLAIM_*` replacements carry no namespace and no subject digest.
3. **Every string a member is refused with is composed server-side** and rendered verbatim. §7.4's
   and §7.7's server tables are in the `writeError` shape and are complete for the doors 0.7.2 adds.
4. **One mechanism, no substitution.** No string on the Agents tab, the chip, or the refusal
   implies a fallback; `LLM_MECHANISM_DEAD` says "does not substitute" in so many words.
5. **The enforcement words are `DRIVES.ENFORCEMENT_*`** — referenced; the fifth lands in the
   drives prompt with U1. **The drives honesty sentence is untouched**; `CEILING` sits beside the
   ceiling field and nowhere else.
6. **The door's strings live in `governance-prompt.md` §7.2 and `governance-copy.ts`** (U2) — §7.6
   stages them; the profile editor never imports this module.
7. **No nav item; `MEMBER_NAV_PATHS` unchanged; every `/providers*` route and both endpoints are
   SUPER.** A security admin sees `OperatorOnlyHint` and the two limit rows.
8. **The picker never hides a row.** Disabled with `UNAVAILABLE`, or — roster unknown — today's
   literals with nothing claimed.
9. **Pluralisation is the inline ternary** `PERM.ENFORCE_ON_BODY` uses — `SAVED_NARROWED`,
   `CARD_PROVIDERS`, `CARD_AGENTS`, `STEP_BADGE_READY`, `STEP_BADGE_READY_AGENTS`,
   `TRUSTED_CA_COUNT` — never a second helper.
10. **This module exports no name `user-drives-copy.ts` or `governance-copy.ts` exports.** Its
    namespaces are `PROVIDERS`, `AGENTS` and `PROVIDER_MEMBER` (§7.4's member doors, the
    `DRIVE_MEMBER` twin). It carries **no copy** of the admin-facing and run-time strings the server
    composes (§7.1's second table): the drives test's negative `carries no copy of the admin-facing
    server-composed refusals` is inherited with this round's substrings.
11. **Step id `providers`, label `Providers`, heading `What runs can be built from`**; the Secrets
    step keeps `integrations` / "Secrets" (Q5 — renaming an id breaks `episodesFor`).

**Assertion sites this round's copy is already pinned to** — a change here that is not reflected in
these is a broken test, not a free edit:

- `ui/src/app/lib/workspace-providers-copy.test.ts` (U1's clone) — this doc's own `doc.size` is
  **125** over §7.2–§7.7 (95 with §7.6 excluded, after U3 moved `REHOME_TITLE` and
  `DRIVES_OFF_BANNER` to `user-drives-prompt.md`); the pin moves only with a row.
- `ui/src/app/lib/user-drives-copy.test.ts:109` — `doc.size` 140; U1's `ENFORCEMENT_EVICTION` row
  moved it to 141, U3's re-home dialog row + the two `NR_*` unavailable sentences to 144, and the
  `DRIVES_OFF_BANNER` move from this module to 145 — each in the commit that ships the key.
- `ui/src/app/lib/governance-copy.test.ts:172` — `doc.size` 90 → 96 with U2's six keys.
- `ui/src/app/lib/scm-provider.test.ts:111-113` — `LANE_META` tooltips ARE `CAPABILITY.*`.
- `ui/src/app/components/screens/setup/setup-screen.test.tsx:606-628` — the Secrets-step Git host
  radiogroup, the `Ready · 1 connected` badge (`ai.length` only after U1), the `next:` walk.
- `ui/src/app/components/screens/settings/settings-screen.test.tsx` — card order.
- `internal/api/setup_test.go:157` — `len(Harnesses) == len(harnessCatalog)`; the roster is never
  filtered.
- `docs/OPERATIONS.md` — the two "Git host card" sentences (E2), the tier-table rows (A1/C3).

Implementation is not done when it builds and unit tests pass: `scripts/run-ui-e2e.sh providers`
(and `agents`) before calling any of it done.

## 6. Where the model lives on the page

**New screen `/providers`** (SUPER-only; no nav entry — reached from the funnel step and the
Settings card). Top to bottom:

1. **Header** — `TITLE`, `LEAD`.
2. **Tabs** — `Segmented`: `GIT_TITLE` · `STORAGE_TAB` · `AGENTS_TITLE`.
3. **Git** — `GIT_LEAD`; the legacy banner when zero rows; else one row per kind: kind label + the
   row's hosts (mono) + `Switch` `FIELD_ENABLED` + `PERM.REMOVE`; body: `FIELD_BASE_URLS` textarea
   with `BASE_URLS_HINT`, `FIELD_LANES` three chip-checkboxes with `LANES_HINT` (the unavailable one
   disabled with its reason), then the credential `Lane`s inline; `S.GIT_FOOTER` as the plain note
   under the tab; `SAVE_CTA` teal.
4. **Storage** — `EPHEMERAL_TITLE` / `EPHEMERAL_LEAD`; `FIELD_DEFAULT_DISK` + `FIELD_MAX_DISK`, the
   enforcement gloss under them, `DOCKER_UNCAPPED_WARN` when the driver cannot enforce;
   `DRIVE_CEILING_TITLE`; `FIELD_DRIVES_ENABLED` switch, `FIELD_MAX_DRIVE` with `MAX_DRIVE_HINT` and
   `CEILING` as the plain note; then `UserDrivesCard` as the link out; `SAVE_CTA` teal.
5. **Agents** — `AGENTS_TITLE` / `AGENTS_LEAD`; one row per catalog id + one per image-map key: display
   name, `Switch` `FIELD_ENABLED` (reused from `PROVIDERS`); body: `FIELD_MECHANISM` radio grouped by vendor over the
   existing lane titles (impossible pairs disabled with the catalog reason), `FIELD_SOURCE`
   `Segmented` `SOURCE_SHARED` / `SOURCE_PER_USER` (the second disabled off `bedrock_sso` with
   `PER_USER_UNAVAILABLE`), `FIELD_SSO_START_URL` when `per_user`, and the signed-in admin's own
   `Model access` chip with its action (`SIGN_IN_AWS`, `outline` here, opens `HarnessLoginPane` in
   place);
   `S.MODEL_FOOTER` as the plain note; `SAVE_CTA` teal.

**The funnel step** `providers` in "Your work" before `workspaces`: heading `STEP_HEADING`, the
CARD (§7.5) as its body — zero teal: the footer Next is the step's one affirmative, and the card's
link goes to `/providers`, where the forms and the one `SAVE_CTA` live (the drives round's shape,
`user-drives-prompt.md` §4) — badge `Optional` → `Skipped` → `STEP_BADGE_READY(n)`.

**The card**, in two places from one component (`setup/providers-card.tsx`): the Settings page
(third card, replacing Git host: Host · Model provider · **Providers** · SSH keys · Drives) and the
funnel step's body. Title `TITLE`, `CARD_LEAD`, `CARD_SUMMARY` or `CARD_EMPTY`, the two-line
link button `CARD_OPEN`. SUPER only.

**The door**, on `/governance`: two `LimitNumberRow`s after "Deny mounting a user drive" (U2).

**Member surfaces, inline, in place:** `member-getting-started.tsx` (the chip + action line +
login pane), `new-run-screen.tsx` (the picker, the floor hint, the inline 201 warnings),
`run-detail/widgets/effective-policy.tsx`, `workspaces.tsx` + the Workspace `<Select>`
(`CARD_NOT_ADMITTED`), `add-workspace-dialog.tsx` (its repo hint), the 403/422 render path.

## 7. Canonical strings — DRAFT, frozen by the owner at this gate

Throughout §7, a backticked substring inside a string (a URL, a wire value, a secret or env name,
a field) renders `font-mono` in the console and in the mock; the frozen string itself is plain text
(the parser strips backticks). A `{placeholder}` is substituted by the caller; `{n}` keys are the
inline ternary (§5 #9). **Every row from §7.2 on is DRAFT** — the heading says so once rather than
each row, because a marker in the key cell would break the parser's clone; the owner freezes by
deleting the word from the headings.

**Server-refusal shape.** Server-composed rows (§7.4's second half, §7.7's last block) are
`writeError` bodies in the in-tree convention — a lowercase-opening clause naming the wire field or
the thing refused — and the console renders them verbatim under its own heading.

### 7.1 Reused canon — referenced, never re-frozen

| Key | Lives in | String |
|---|---|---|
| `S.GIT_FOOTER` | `settings/connection-cards.tsx` | Only the GitHub App lane keeps its token outside the sandbox — a PAT or SSH key enters it for the clone, then is wiped. Public repos clone with no credential at all. |
| `S.MODEL_FOOTER` | `settings/connection-cards.tsx` | The egress proxy injects these on the wire, so keys never enter the sandbox — except Bedrock's SSO lane, where AWS credentials sign inside it. |
| `S.STORE_NOTE` | `settings/connection-cards.tsx` | Wardyn stores this — it doesn't dial the provider to check it. |
| `Lane` titles / hints (git) | `settings/connection-cards.tsx` | Personal access token / The simplest lane — stored once; a per-run helper hands it to git inside the sandbox at clone time. · SSH key / A per-run copy is written inside the sandbox for the clone, then shredded. · GitHub App / Repo-scoped tokens brokered at the proxy — the token never enters the sandbox. |
| `Lane` titles / hints (model) | `settings/connection-cards.tsx` | Claude subscription / Sign in through a throwaway sandbox — the token never touches disk. · API key / ANTHROPIC_API_KEY or OPENAI_API_KEY. · AWS Bedrock / A bearer key, or an SSO device-code sign-in. |
| `SecretLane` labels | `settings/connection-cards.tsx` | Access token · Private key · App ID · Private key (PEM) · Connected · Disconnect |
| `LANE_META.*.label` | `lib/scm-provider.ts` | App · brokered / PAT · in-sandbox / SSH · resident |
| `CAPABILITY.brokerLine` | `wardyn/copy.ts` | The run works through a short-lived, scoped credential — your stored key stays in Wardyn. |
| `CAPABILITY.gitPatLine` | `wardyn/copy.ts` | A git access token is handed to git inside the sandbox — the process running there can read it. |
| `CAPABILITY.sshKeyLine` | `wardyn/copy.ts` | A private SSH key is written to disk in the sandbox — the process running there can read it. |
| `OPERATOR_ONLY_REASON` | `wardyn/copy.ts` | Requires the admin role. |
| `PERM.REMOVE` | `permissions-copy.ts` | Remove |
| `PERM.SUBJECT_USER / SUBJECT_GROUP / SUBJECT_ALL` | `permissions-copy.ts` | User / Group / Everyone signed in |
| `PEOPLE.CANCEL` | `people-access-copy.ts` | Cancel |
| `ACCESS_STATE.FETCH_FAILED_RETRY` | `people-access-copy.ts` | Retry |
| `DRIVES.TITLE` / `CARD_LEAD` / `CARD_OPEN` | `user-drives-copy.ts` | User drives / Persistent storage people can mount into a run — separate from any workspace. / Manage drives |
| `DRIVES.ENFORCEMENT_FILESYSTEM / _REQUEST / _EXTERNAL / _NONE` | `user-drives-copy.ts` | Size enforced by the filesystem / Size requested; the storage class decides / Size bounded by the share's own quota / Size shown, not enforced |
| **`DRIVES.ENFORCEMENT_EVICTION`** | `user-drives-prompt.md` §7.2 (appended by U1) → `user-drives-copy.ts` | Size limited by eviction — over it, the run is stopped, not the write |
| `DRIVES.HONESTY` | `user-drives-copy.ts` | Wardyn never enforces a drive's size itself. … (unchanged; rendered on `/drives`, not here) |
| `DRIVES.SAVE_CTA` / `SAVE_REFUSED_TITLE` | `user-drives-copy.ts` | Save drive / This drive can't be saved as written |
| `DRIVES.HOME_HASH / HOME_SUB / HOME_EMAIL_LOCAL` + `_HINT`s, `HOME_HINT`, `HOME_RULE` | `user-drives-copy.ts` | the editor's directory-name options (drives-mock State 3b renders them) |
| `GOV.LIMITS_TITLE` / `GOV.LIMITS_LEAD` | `governance-copy.ts` | Limits / Some of what a run can do routes around the ceiling entirely. Deny it here instead. |
| `GOV.LIMIT_EXEC_LABEL / LIMIT_INTERACTIVE_LABEL / LIMIT_DRIVE_LABEL` | `governance-copy.ts` | Deny exec runs / Deny interactive runs / Deny mounting a user drive |
| `MEMBER_GETTING_STARTED.SETUP_SUMMARY_TITLE / _HELPER` | `wardyn/copy.ts` | What's set up for you / Your admin configured the barrier, network and shared credentials. Your runs inherit them. |
| `MEMBER_GETTING_STARTED.BARRIER_CHIP(label)` / `MODEL_ACCESS_PROVIDED_CHIP` / `MODEL_ACCESS_OWN_CHIP` / `SIGNIN_SSO_CHIP` | `wardyn/copy.ts` | Barrier · {label} / Model access · Provided by your admin / Model access · Your key / Sign-in · SSO |
| `MEMBER.GS_CHIP(name)` | `governance-copy.ts` | Governance · {name} |
| `HarnessLoginPane` AWS flow title | `settings/harness-login-pane.tsx` | Connect an AWS SSO session via container login |
| New Run agent picker literals | `new-run-screen.tsx` | Agent · Claude Code · Codex CLI |
| `IdentityWidget` labels | `run-detail/widgets/identity.tsx` | Identity · Run · Image · Policy · Runner · Sandbox · Started |
| Workspaces page header | `workspaces.tsx` | Workspaces · A repo or directory a run can attach. Runs can only attach what's listed here. · Add workspace |
| Setup step labels (existing) | `setup/steps.ts` | Environment · People · Network · Secrets · Workspaces · Review; badge words Optional · Skipped |
| Settings page title + Host card | `settings-screen.tsx` | Settings · Host · The barriers this machine can build, and what every run inherits by default. |
| Harness catalog display names | `internal/api/harness.go` | Claude Code · Codex CLI · Your own tools |

**Composed by the server today — rendered verbatim, never keyed.** These exist and are reused
unchanged; the lanes that raise them do not reword them.

| Source | Emitted by | String |
|---|---|---|
| Impossible agent × mechanism pair (400, reused for `agent_providers`) | `harnessCatalog` reasons, `internal/api/harness.go` | Codex CLI speaks the OpenAI API only — an Anthropic key can't drive it. Not a setting. · Codex CLI speaks the OpenAI API only — a Claude login can't drive it. Not a setting. · Codex CLI speaks the OpenAI API only — Bedrock can't drive it. Not a setting. · Claude Code speaks the Anthropic API only — an OpenAI key can't drive it. Not a setting. |
| Ephemeral clamp warning (201 warning / effective policy) | `composer.Clamp`, `internal/composer/clamp.go` | resources capped to operator maximum |
| The other clamp lines the widget renders | `composer.Clamp` | confinement raised from "{req}" to operator minimum "{min}" · dropped {n} egress domain(s) not in operator allowlist: {list} · allow_all_egress disabled: operator policy does not permit allow-all egress · … (every `warns = append` literal in that file, unchanged) |
| Re-home 409 (the dialog's body) | `driveRehomeGuard`, `internal/api/user_drives.go` | this drive is allocated to {n subjects} and this change re-homes {them}: {…}. Every allocated person's storage object is derived from these fields, so their next run mounts a different object and the one holding their work is left behind with nothing in Wardyn naming it. — its closing "Confirming is an API action…" sentence is rewritten by S2 once the dialog exists (`user-drives-prompt.md` §7.1) |
| Managed backend refuses a claim template (400; drives-mock State 3b) | `ValidateUserDrive` | home_template "{template}" is not allowed on a managed backend — … (unchanged, `user-drives-prompt.md` §7.1) |
| Agent required (400) | `agentRequirementError`, `runs_create_validate.go` | agent is required |
| Today's `brokered:llm` 404 detail — REPLACED by §7.7's | `proxyLLMRequest`, `internal/egress/proxy/llm_routes.go` | no LLM credential is brokered for {host} |


**Composed by the server for THIS round — DRAFT, frozen here, rendered verbatim, never keyed** (the
drives round's host-root precedent: an ADMIN-facing refusal, a 201 warning, a run's failure hint or
a proxy detail is the Go side's literal, and the module carries no copy). `Emitted by` names the
Go function or constants block; `String` is the literal with its format verbs as `{placeholders}`
(`%q` written as `"{x}"`); the status is the one the boundary answers with. **U1's clone inherits the
drives test's second `describe` unchanged in shape** — `goStringLiterals()` over `GO_DIRS` (this
round adds `internal/egress/proxy` for the last four rows), `goFuncNames()` for the `Emitted by`
column, `goFixedRuns` with its 30-character fixed floor, verbs as wildcards, one-way (every row must
exist in Go; the Go side's own marker is still the open ponytail) — plus the negative test over
`PROVIDERS` / `AGENTS` / `PROVIDER_MEMBER` with these substrings: `is not a`, `may not exceed`,
`is not unique`, `outside every enabled provider`, `was not wired`, `legacy scm_hosts list`,
`drive ceiling`, `disabled for this deployment`, `requires TLS`, `names no agent`, `must be none`,
`per_user is available`, `sso_start_url is required`, `brokered LLM route`, `below policy`.

| Source | Emitted by | String |
|---|---|---|
| Base URL shape (400, both write doors) | A1 `PROVIDERS_400.*`, `validateWorkspaceProviders` | base_urls[{i}]: must be an https URL with a host and no credentials, query or fragment |
| Base URL not the kind's host (400) | `validateWorkspaceProviders` | base_urls[{i}]: "{url}" is not a {kind} host |
| Lane the kind cannot carry (400) | `validateWorkspaceProviders` | lanes: "{lane}" is not available for {kind} ({reason}) |
| Ephemeral default above maximum (400) | `validateWorkspaceProviders` | storage.ephemeral: default_disk_mib may not exceed max_disk_mib |
| Duplicate row id (400) | `validateWorkspaceProviders` | id "{id}" is not unique |
| Stale `If-Match` (412; M2 sheet) | `handlePutWorkspaceProviders` | providers changed since you loaded them — reload and retry |
| Repo outside every enabled provider — OPERATOR (422; when a legacy host was refused by a kind-wide claim, the list is the CLAIMING row's) | A3 `ADMIT.*`, beside `providerFor` | repository {repo} is outside every enabled provider's allowed addresses ({addresses}) |
| Lane veto (201 warning + audit) | `ADMIT.*` | the {lane} lane is not permitted for {kind}; the {grant} grant was not wired |
| Unclaimed legacy host (201 warning) | `ADMIT.*` | host {host} is admitted through the legacy scm_hosts list; enable a provider for it before 0.8 |
| Drives off — every drive and grant write (422; also `REFUSED_BACKEND`'s `{reason}` for a run carrying `drive.enabled`) | S2 `DRIVES.*`, `decodeUserDriveRequest` / `handleUpsertUserDriveGrant` | drives are disabled for this deployment |
| Drive size or override above the provider ceiling (422) | `decodeUserDriveRequest` / `handleUpsertUserDriveGrant` | size_mib {size} exceeds this deployment's drive ceiling ({max} MiB) |
| Static claim not provisioned (run `failure_hint`; REPLACES the namespace-bearing `errDriveClaimNotProvisioned`) | `internal/runner/k8s/drives.go` | drive: your drive's volume is not provisioned on this cluster — ask an admin |
| Claim identity mismatch (run `failure_hint`; REPLACES the digest-bearing `driveClaimIdentity`) | `internal/runner/k8s/drives.go` | drive: your drive's volume is not the one allocated to you — ask an admin |
| Unknown agent id (400) | C3 `agent_providers` block, `validateAgentProviders` | agents: "{id}" names no agent this deployment can run — a catalog id or a WARDYN_AGENT_IMAGES key |
| Custom image with a model mechanism (400) | `validateAgentProviders` | agents: "{id}" is not in the agent catalog, so its mechanism must be none — Wardyn wires no model credential into a custom image ({mechanism} would show a live credential that binds nothing) |
| `per_user` off `bedrock_sso` (400) | `validateAgentProviders` | agents: "{id}": credential_source per_user is available for bedrock_sso only, not {mechanism} |
| `per_user` without a start URL (400) | `validateAgentProviders` | agents: "{id}": sso_start_url is required when mechanism is bedrock_sso and credential_source is per_user |
| Duplicate agent id (400) | `validateAgentProviders` | agents: id "{id}" is not unique |
| Injection rule met over plain HTTP (403, rule source `policy:require-tls`) | G1 `INJECT.REQUIRE_TLS`, the `plain_lane.go` deny arm | credential injection for {host} requires TLS: this rule sets require_tls and the request was plain HTTP |
| No credential behind a brokered LLM route (404 detail; REPLACES today's) | C2 `Config.LLMUnavailableDetail`, `proxyLLMRequest` | no credential is configured for this brokered LLM route |
| Half-configured Bedrock (404 detail) | `Config.LLMUnavailableDetail` | Bedrock is configured but its credential expired at {ts}; reconnect it |
| The fixed clause appended to either detail | `proxyLLMRequest` | this is below policy: it cannot be approved, and no policy edit changes it. |

The impossible agent × mechanism 400 reuses the catalog's four reasons above, unchanged.
`ADMIT_MEMBER`, `AGENT_NOT_ENABLED` and `LLM_MECHANISM_DEAD` are NOT here: a member meets them at a
door, so they are keyed (§7.4, `PROVIDER_MEMBER`) the way `DRIVE_MEMBER`'s refusals are.

### 7.2 `PROVIDERS` — the screen, and the Git tab (every row DRAFT)

| Key | String |
|---|---|
| `TITLE` | Workspace providers |
| `LEAD` | Where work can come from, and how big it can get. Enable a git provider to bound which repositories a run may clone; set the storage ceilings every run and every drive is held to. |
| `GIT_TITLE` | Git providers |
| `GIT_LEAD` | With no provider rows, any host with a stored credential can be cloned. Add a row to bound a host to the addresses you list; a row turned off refuses its host. |
| `STORAGE_TAB` | Storage |
| `KIND_GITHUB` | GitHub |
| `KIND_AZURE_DEVOPS` | Azure DevOps |
| `FIELD_ENABLED` | Enabled |
| `ROW_ABSENT_HINT` | Not configured. Its host follows the legacy list, if listed there — every address on it, no bound. |
| `ROW_DISABLED_HINT` | Off: this host is refused. Turn it on to admit the addresses below again. |
| `ROW_DISABLED_CHIP` | Off |
| `ADD_ROW_CTA` | Add provider |
| `REMOVE_CONFIRM_TITLE(kind)` | Remove the {kind} row? |
| `REMOVE_CONFIRM_BODY` | Its host goes back to the legacy list — admitted if listed there, with no address bound. Stored credentials stay. |
| `FIELD_BASE_URLS` | Allowed addresses |
| `BASE_URLS_HINT` | One per line, over HTTPS. A repository is admitted when its URL starts with one of these. |
| `BASE_URL_INVALID` | Must be an `https://` URL with a host; an organisation path where the host is shared (`github.com/<org>`, `dev.azure.com/<org>`) — no port, no credentials, no trailing wildcard. |
| `BASE_URLS_REQUIRED` | Name at least one address. A row with none admits nothing and is refused at save. |
| `FIELD_LANES` | Permitted lanes |
| `LANES_HINT` | Which credential a run may use for this provider. Turning one off does not delete its stored secret. |
| `LANE_APP_UNAVAILABLE` | Not available: the App broker mints repository-scoped GitHub tokens and has no Azure DevOps equivalent. |
| `LANE_SSH_UNAVAILABLE` | Not available: SSH over port 443 is offered for `github.com` and `dev.azure.com` only — a self-hosted host clones over HTTPS. |
| `SSH_HOST_LEVEL_HINT` | SSH clones are admitted for the whole host: an SSH URL carries no org path to bound. Drop SSH here to keep this row's addresses binding. |
| `LANES_NEED_ADDRESS` | Add an allowed address first — a credential is stored under its host. |
| `LEGACY_OPEN_TITLE` | No git provider rows |
| `LEGACY_OPEN_BODY` | Runs clone whatever host has a credential stored, as they do today. Add a provider to bound that to addresses you name. |
| `LEGACY_OPEN_OTHER_HOSTS` | A GitLab or Bitbucket token has no provider row yet — store and rotate it on the Secrets page. |
| `SAVED_ELSEWHERE_TITLE` | Someone else saved providers since you loaded this page |
| `SAVED_ELSEWHERE_BODY` | Reload to see their version before saving yours. |
| `SAVED_TOAST` | Providers saved. |
| `SAVED_NARROWED(n)` | {n} onboarded source is now outside every enabled provider — runs can't clone it until an admin widens the addresses or turns its host on. / {n} onboarded sources are now outside every enabled provider — runs can't clone them until an admin widens the addresses or turns their host on. |
| `SAVE_CTA` | Save providers |
| `SAVE_ERROR` | Couldn't save these providers. |
| `SAVE_REFUSED_TITLE` | These providers can't be saved as written |
| `ADD_WORKSPACE_REPO_HINT` | Cloned into the sandbox when a run starts. Private repos use the credential stored under Settings → Providers for their host. |

**0.7.4 CORRECTION (Appendix A F4-F11, Q12 option (b), owner default):** `BASE_URL_INVALID` used to
claim "at least one path segment" — `display.tsx`'s own `baseURLError` deliberately does NOT require
one on a self-hosted host (a bare `https://git.corp.example` GHES-style host is valid, §5.1); only
`dev.azure.com` requires the organisation segment. Reworded to the rule the mirror actually checks.

**0.7.4 CORRECTION (Appendix A F4-F8, reclassified Low copy):** `DEFAULT_DISK_HINT` / `MAX_DISK_HINT`
/ `MAX_DRIVE_HINT` (§7.3) used to say "0 means…" — `numberField` (`storage-tab.tsx`) renders 0 as an
EMPTY field, so the control was correct (0 IS "unbounded"/"no ceiling" on the wire,
`runs_dispatch_ceiling.go`/`workspace_providers.go`) but the hint named a value the field can't
display. "Leave blank" is what an operator can actually do; the REJECTED alternative was a raw-text
state rewrite of the number fields (out of scope for a copy fix).

`TITLE` is one string for four places — the screen heading, the Settings card's title, the funnel
step's summary card, and the tier row — the way `DRIVES.TITLE` serves its four. `BASE_URL_INVALID`
is the client mirror rendered pre-attempt under an `aria-invalid` textarea; the server's own 400s
(§7.4) head every post-attempt refusal under `SAVE_REFUSED_TITLE`. The three lane checkboxes render
`LANE_META.*.label` with `CAPABILITY.*` as the tooltip; an unavailable lane is disabled with its
`LANE_*_UNAVAILABLE` reason as the chip's sub-line (Q3). `S.GIT_FOOTER` renders once, as the plain
note under the tab, and is not keyed here (§5 #1). `SAVED_TOAST` is the transient confirmation;
`SAVED_NARROWED` is the one toast worth reading twice, so it ALSO stays on the page as an amber
note until the next save (Q8). `ADD_WORKSPACE_REPO_HINT` replaces the Add-workspace dialog's hint
that names the retired card. `REMOVE_CONFIRM_TITLE`'s `{kind}` is `KIND_*`, and it is the confirm dialog's title over
`REMOVE_CONFIRM_BODY` — two keys because Radix's `AlertDialog` renders a title and a description, and a
console that splits one frozen sentence at its question mark reflows a canon edit into the wrong slot.
`ROW_DISABLED_CHIP` is the off row's neutral chip (the `AGENT_ROW_DISABLED_CHIP` precedent) — its own key,
not `ROW_DISABLED_HINT` sliced at the colon.
`BASE_URLS_REQUIRED` is the ZERO-address arm of the same pre-attempt mirror (the server's own
`git[i].base_urls: name at least one address` heads the post-attempt refusal): a present row with no
address is `aria-invalid` and withholds `SAVE_CTA`, because an empty list is a guaranteed 400 and
there is nothing honest to send. `LANES_NEED_ADDRESS` renders in place of a secret name when the
row's first address names no host: the credential lanes are keyed by THAT host, so with none there is
no name to store under — every lane is disabled with this reason rather than defaulted to
`github.com`, which wrote an Azure DevOps PAT into `git-pat-github-com`.

### 7.3 `PROVIDERS` — the Storage tab (every row DRAFT)

| Key | String |
|---|---|
| `EPHEMERAL_TITLE` | Ephemeral scratch |
| `EPHEMERAL_LEAD` | The writable layer a run gets when it mounts no drive. It is wiped when the sandbox exits. |
| `FIELD_DEFAULT_DISK` | Default size (MiB) |
| `DEFAULT_DISK_HINT` | Fills a run that asks for no size. Leave blank for such a run to run unbounded — the maximum below binds requests, not silence. |
| `FIELD_MAX_DISK` | Maximum size (MiB) |
| `MAX_DISK_HINT` | A run asking for more is clamped to this, not refused. Leave blank for no ceiling. |
| `DOCKER_UNCAPPED_WARN` | This host's storage driver cannot enforce a size. A number filled or clamped from here runs uncapped, with a warning on the run; a size a policy or a profile writes still fails the run at create on this host. |
| `DRIVE_CEILING_TITLE` | Drive ceiling |
| `FIELD_DRIVES_ENABLED` | User drives |
| `DRIVES_ENABLED_HINT` | Off means this deployment offers no drives: nothing is mounted and every drive write is refused. Existing drives and allocations are kept. |
| `FIELD_MAX_DRIVE` | Largest drive (MiB) |
| `MAX_DRIVE_HINT` | An allocation or override above this is refused at write and clamped at resolve. Leave blank for no ceiling. |
| `CEILING` | A ceiling bounds what an admin may allocate. It does not bound what the volume will hold. |

The enforcement word under the two disk fields is `DRIVES.ENFORCEMENT_*` (§7.1) read from
`/setup/status`'s runner block — `eviction` on Kubernetes, `filesystem` or `none` on Docker;
`DOCKER_UNCAPPED_WARN` renders under all three disk fields (the two here and the profile editor's
`MaxEphemeralDiskMiB` row) whenever the word is not `filesystem`, because any of the three can
leave a run with a non-zero `disk_mib` on such a host, and it reads the AUTHORING daemon's driver,
never a laptop's. `CEILING` renders once, as the plain note under `FIELD_MAX_DRIVE`; the drives
honesty sentence stays on `/drives`. **Q9 resolved (0.7.2, U3): `DRIVES_OFF_BANNER` moved to
`DRIVES.DRIVES_OFF_BANNER` (`user-drives-prompt.md` §7.2)** — it renders on `/drives` itself, the
same screen that already owns every other string it stands beside, rather than pulling a screen's
banner text from a module for a page it never opens. The `UserDrivesCard` link out follows the form.

### 7.4 Write refusals and states — console, then server-composed (every row DRAFT)

| Key | String |
|---|---|
| `FETCH_FAILED_TITLE` | Couldn't load workspace providers |
| `FETCH_FAILED_BODY` | Something went wrong reaching the server. The providers already saved still bound every run — this page just can't show them right now. |
| `CARD_NOT_ADMITTED` | Not an enabled git provider — runs can't clone this until an admin enables its host. |

`FETCH_FAILED_*` is distinct from the legacy banner (a confident empty state would be a false claim);
Retry is `ACCESS_STATE.FETCH_FAILED_RETRY`. `SAVE_ERROR` (§7.2) is the unreachable-server arm;
`SAVE_REFUSED_TITLE` heads every 400 below. `CARD_NOT_ADMITTED` renders on the workspace row and
as the Workspace `<Select>`'s reason line, from the server's `admitted` flag (U3). **`REHOME_TITLE`
moved to `DRIVES.REHOME_TITLE`** (`user-drives-prompt.md` §7.4, 0.7.2, U3) rather than being copied
here too — the re-home confirm dialog it heads lives on `/drives`, the body is the server's 409
verbatim (§7.1), Cancel is `PEOPLE.CANCEL`, and the confirm is `DRIVES.SAVE_CTA` painted
`destructive` — the action IS saving the drive, and it strands work.

**Server-composed member doors — `PROVIDER_MEMBER` (parsed; the `DRIVE_MEMBER` precedent: keyed in
the module AND byte-checked against the Go literal, `user-drives-copy.test.ts`'s §7.7 check). A
member meets these on the launch path; the console renders them verbatim under its own heading:**

| Key | String |
|---|---|
| `ADMIT_MEMBER` | this repository's host is not an enabled git provider — ask an admin |
| `AGENT_NOT_ENABLED(id)` | agent: "{id}" is not an enabled agent on this deployment — ask an admin |
| `LLM_MECHANISM_DEAD(mechanism, ts)` | this run's model access is configured as {mechanism}, and that credential expired at {ts} and could not be renewed — sign in again under Settings → Model provider. Wardyn does not substitute a different model provider. |

`ADMIT_MEMBER` is the 403 / 422 body a member meets at every admission site (the capability door
audits `authz.denied`; the admission miss is a 422 with no audit) — it names the kind and NEVER a
base URL. `AGENT_NOT_ENABLED` is run create's (and the record launcher's) refusal of an agent with
no enabled row (C3, `internal/api/runs_create_validate.go`). `LLM_MECHANISM_DEAD` is C2's dispatch
refusal (`enforceConfiguredLLMMechanism`, `internal/api/runs_dispatch_llm.go`) and the same 422 at
create and Review; `{mechanism}` is the declared lane in words — the plan's instance is "Amazon
Bedrock (captured AWS SSO session)". Its remedy clause names Settings → Model provider, the ADMIN's
path; under `per_user` the member's remedy is `SIGN_IN_AWS` on Getting Started — **Q11**. Every
OTHER server-composed string this round adds — the admin write refusals, the operator's 422, the
two 201 warnings, the drive 422s and failure hints, the proxy's `brokered:llm` detail, the injection
rule's 403 — is in §7.1's second table, unparsed.

### 7.5 `PROVIDERS` — the card, the step, and the entry points (every row DRAFT)

| Key | String |
|---|---|
| `CARD_LEAD` | Which git hosts a run may clone, and the storage ceilings it works inside. |
| `CARD_EMPTY` | No providers enabled. |
| `CARD_PROVIDERS(n)` | {n} git provider / {n} git providers |
| `CARD_AGENTS(n)` | {n} agent / {n} agents |
| `CARD_SUMMARY(providers, agents)` | {providers} · {agents} |
| `CARD_OPEN` | Manage providers |
| `STEP_LABEL` | Providers |
| `STEP_HEADING` | What runs can be built from |
| `STEP_BADGE_READY(n)` | Ready · {n} provider / Ready · {n} providers |
| `STEP_BADGE_READY_AGENTS(n)` | Ready · {n} agent / Ready · {n} agents |

One component in two homes (`setup/providers-card.tsx`): the Settings page's third card (replacing
Git host) and the funnel step's body (zero teal on both). Title `TITLE`, `CARD_LEAD` over `CARD_SUMMARY(
CARD_PROVIDERS(n), CARD_AGENTS(m))` or `CARD_EMPTY`, the two-line link button with `CARD_OPEN` as
its label. It counts ENABLED rows, never hosts. `STEP_LABEL` / `STEP_HEADING` are the `steps.ts`
contract's; the badge ladder is `Optional` → `Skipped` (the orchestrator's visited override) →
`STEP_BADGE_READY`. The plan carries two done rules for the one step — `enabledGitProviders > 0`
(§9.1) and `≥ 1 enabled agent row` (§5c.4) — so both badge shapes are frozen and **Q10** asks
which counts, or whether the badge names both.

### 7.6 Field-report and follow-up strings — STAGING for the M2 sitting (every row DRAFT)

**Parsed by nothing today.** Each row moves to the prompt doc and copy module of the lane that
ships it (named per block); U1's clone of `parseFrozenTables` excludes this section until then.
Rows whose text the plan gives are transcribed verbatim; rows the plan names without text are
drafted here and flagged in the round notes. Strings that are edits to an EXISTING Go literal
(F055 `builtin:resolve-failed`, F065 metric HELP, F048 `require_inspectable_llm`) are NOT drafted
blind — the sitting needs the current literal in hand.

**B-γ → `wardyn/copy.ts` (B1, B4, B4b, F107, F144):**

| Key | String |
|---|---|
| `SHELL_UNKNOWN_BODY` | We couldn't confirm who you are. Nothing here is hidden from you on purpose — reload, or sign in again. |
| `APPROVAL_STATE_CANCELLED` | Cancelled |
| `APPROVAL_CANCELLED_BODY` | The run ended before anyone decided this. Nothing was approved and nothing was denied. |
| `RUN_CLONE_CTA` | Start a run like this one |
| `RUN_CLONE_NOTE` | Prefilled from this run — task, agent, barrier, policy and what it attached. Credentials and approvals are minted fresh. |
| `RUN_CLONE_CEILING_NOTE` | Your ceiling applies again at launch — anything this run had above it is narrowed, with the reason. |
| `SIGNOUT_FAILED_TITLE` | Couldn't sign you out |
| `SIGNOUT_FAILED_BODY` | Your session is still live. Try again, or close every tab on this site. |
| `TERMINAL_ESCAPE_HINT(chord)` | {chord} moves focus out of the terminal. |

**U1 → `corp-network-step` / `wardyn/copy.ts` (B2, F16, F22):**

| Key | String |
|---|---|
| `SITE_SAVE_NOTE` | Saved. This applies to runs started from now — a run already going keeps the network settings it started with. |
| `CONFINEMENT_NETPOL_ENFORCING` | **RETIRED (U-06, 0.7.3 F6)** — NetworkPolicy: enforcing |
| `CONFINEMENT_NETPOL_NOT_ENFORCING` | **RETIRED (U-06, 0.7.3 F6)** — NetworkPolicy: not enforcing |
| `CONFINEMENT_NETPOL_INDETERMINATE` | **RETIRED (U-06, 0.7.3 F6)** — NetworkPolicy: indeterminate |
| `TRUSTED_CA_COUNT(n)` | {n} trusted CA certificate / {n} trusted CA certificates |

The three `CONFINEMENT_NETPOL_*` rows above lost their only consumer in 0.7.3
F6 (the global header's `NetworkPolicy: enforcing` chip) — retired here so a
canon table that still lists them doesn't invite the next lane to re-add the
chip. The netpol verdict now lives on the setup Environment step alone.

**B-β → `internal/egress/proxy/policy.go` (server-composed; B2/B6 suffix, B7 remedy — quoted
verbatim by `docs/OPERATIONS.md` and asserted by the ops guard):**

| Key | String |
|---|---|
| `EGRESS_DENIAL_SUFFIX` | Site config is read at run start, so change it and start a new run — this one will keep being refused. |
| `SITE_INTERNAL_HOSTS_CIDR_HINT` | Leave `cidrs` empty unless you know the addresses the sandbox resolves. What your own machine sees for a private endpoint is usually not what the cluster sees. |

**U2 → `governance-prompt.md` §7.2 / `governance-copy.ts` (the three numeric limit rows; the
existing `LIMIT_QUOTA_LABEL` is the chip, not a field label):**

| Key | String |
|---|---|
| `LIMIT_EPHEMERAL_LABEL` | Largest ephemeral scratch (MiB) |
| `LIMIT_EPHEMERAL_HINT` | Binds a run's requested scratch size, not a run that requests none. 0 means no limit under this profile. |
| `LIMIT_DRIVE_SIZE_LABEL` | Largest drive (MiB) |
| `LIMIT_DRIVE_SIZE_HINT` | Clamps the drive size a person under this profile resolves to. On a share it bounds the number shown, not the share. 0 means no limit. |
| `LIMIT_CONCURRENT_LABEL` | Concurrent runs |
| `LIMIT_CONCURRENT_HINT` | How many runs a person under this profile may have going at once. 0 means no limit. |

**A2 → `permissions-copy.ts` `KIND.workspace_provider` (the six `KindCopy` strings; direction
`narrows` — a member could already launch against any onboarded repo, so the unenforced default
stays allowed):**

| Key | String |
|---|---|
| `KIND_WP_LABEL` | Workspace providers |
| `KIND_WP_BLURB` | Which git providers a member may clone from. |
| `KIND_WP_VALUE_LABEL` | Provider |
| `KIND_WP_VALUE_HINT` | A provider row's id. Use * for every provider. |
| `KIND_WP_UNENFORCED` | Members can clone from any enabled provider. |
| `KIND_WP_ENFORCED` | A member can only clone from providers granted to them. A repository on any other provider is refused at launch, with the reason. |

**U3 → `user-drives-prompt.md` §7.6 / `user-drives-copy.ts` (F139/F052 — two new sentences for
`/me.user_drive_unavailable`'s four values; `groups_snapshot_stale` reuses `MEMBER.DENIED_STALE_
GROUPS` verbatim, and `unmountable` renders `NR_UNAVAILABLE` too — NOT `REFUSED_BACKEND`, whose
`{reason}` is launch-time-only prose `driveUnavailableReason` discards before `/me` ever sees it):**

| Key | String |
|---|---|
| `NR_UNAVAILABLE` | Your drive couldn't be checked, so it stays unmounted for this run. Try again, or ask an admin. |
| `NR_GOVERNANCE_UNAVAILABLE` | Your governance profile couldn't be resolved, so your drive stays unmounted for this run — sign in again. |

### 7.7 `AGENTS` — the Agents tab, the member chip, the picker, and the agent refusals (every row DRAFT)

| Key | String |
|---|---|
| `AGENTS_TITLE` | Agents |
| `AGENTS_LEAD` | Which coding agents this Wardyn offers, how each one reaches its model, and whether that credential is one for everyone or one per person. |
| `AGENT_ROW_DISABLED_HINT` | Off: runs naming this agent are refused, and it shows as unavailable in New run. |
| `FIELD_MECHANISM` | Model access |
| `MECHANISM_HINT` | One lane per agent. A run whose lane is not working is refused — Wardyn never substitutes another provider. |
| `MECHANISM_NONE` | None — the image brings its own |
| `MECHANISM_NONE_HINT` | Wardyn wires no model credential. The only choice for an agent outside the catalog. |
| `MECHANISM_BEDROCK_BEARER` | Bearer key |
| `MECHANISM_BEDROCK_SSO` | SSO sign-in |
| `MECHANISM_BEDROCK_ENV` | Daemon environment |
| `MECHANISM_BEDROCK_AWS_DIR` | Host `~/.aws` |
| `FIELD_SOURCE` | Credential |
| `SOURCE_SHARED` | Shared |
| `SOURCE_SHARED_HINT` | One credential, captured by an admin, backs every run. |
| `SOURCE_PER_USER` | Per person |
| `SOURCE_PER_USER_HINT` | Each person signs in to AWS themselves. Their runs use their own session; an expiry affects one person. |
| `PER_USER_UNAVAILABLE` | Not available: only an AWS SSO sign-in is captured per person in this release. |
| `FIELD_SSO_START_URL` | AWS access portal start URL |
| `SSO_START_URL_HINT` | Everyone signs in against this portal. A sign-in never chooses another. |
| `SSO_START_URL_MANAGED` | Your admin set this organization's access portal. Your sign-in uses it — there is nothing to enter here. |
| `ADMIN_OWN_CHIP_NOTE` | This is your own sign-in — the same one a member makes. Under a shared credential it is the one everyone uses. |
| `UNAVAILABLE` | Not enabled by your admin |
| `MODEL_ACCESS_LIVE` | Model access · Your AWS sign-in |
| `MODEL_ACCESS_EXPIRING` | Model access · Expiring |
| `MODEL_ACCESS_EXPIRING_ACTION(ts)` | Sign in again before {ts} |
| `MODEL_ACCESS_EXPIRED` | Model access · Signed out |
| `MODEL_ACCESS_NOT_CONFIGURED` | Model access · Not signed in |
| `MODEL_ACCESS_SHARED_EXPIRED` | Model access · Your admin's credential expired |
| `MODEL_ACCESS_SHARED_EXPIRED_ACTION` | Your admin's model credential expired — ask them to reconnect it |
| `MODEL_ACCESS_NOT_APPLICABLE` | Model access · Not applicable |
| `SIGN_IN_AWS` | Sign in to AWS |
| `FLOOR_UNPARSEABLE(value)` | "{value}" isn't a barrier class, so this policy sets no floor — the barrier above is what launches. |
| `EFFECTIVE_TITLE` | Effective policy |
| `EFFECTIVE_LEAD` | What launch narrowed, one line each. Your policy is what you wrote; this is what ran. |
| `EFFECTIVE_NONE` | No adjustments. |
| `LAUNCH_WARNING_TITLE` | Run launched with a warning |
| `OPEN_RUN_CTA` | Open run |
| `AGENT_ROW_DISABLED_CHIP` | Off |

The seven lifecycle states and what each renders: `live` → `MODEL_ACCESS_LIVE`, success, no action
(`expired_renewable` folds in — dispatch renews it); `expiring` → `MODEL_ACCESS_EXPIRING` +
`_ACTION(ts)`, warning, `SIGN_IN_AWS`; `expired_signin` → `MODEL_ACCESS_EXPIRED`, warning,
`SIGN_IN_AWS`; `not_configured` → `MODEL_ACCESS_NOT_CONFIGURED`, warning, `SIGN_IN_AWS`; `shared`
live → `MEMBER_GETTING_STARTED.MODEL_ACCESS_PROVIDED_CHIP` (reused), success; `shared_expired` →
`MODEL_ACCESS_SHARED_EXPIRED` + `_ACTION`, warning, no button (nothing the member can do);
`not_applicable` → `MODEL_ACCESS_NOT_APPLICABLE`, neutral, no action — the caller is a mechanism
(the shared admin token under a per_user row), not a person, so there is no sign-in for it to
complete. The action line renders under the chip row, in the member's own words, and `SIGN_IN_AWS`
opens `HarnessLoginPane` in place. The admin's own chip on the Agents tab renders the same seven.
`MECHANISM_*` labels name Bedrock's four sub-lanes under the card's "AWS Bedrock" lane title; the
Anthropic and OpenAI lanes reuse the model card's titles (§7.1). `UNAVAILABLE` is the picker item's
sub-line (the plan's fragment, sentence-cased). `FLOOR_UNPARSEABLE` renders under the JSON policy
field only when a parse succeeds and `min_confinement_class` names no class; precedence is unchanged.
`AGENTS_TITLE` / `AGENTS_LEAD` head the tab; the row's switch reuses `PROVIDERS.FIELD_ENABLED` (one word, one key), and `AGENT_ROW_DISABLED_CHIP` is the off row's neutral chip — its own key, not `AGENT_ROW_DISABLED_HINT` sliced at the colon. `EFFECTIVE_*` head the run-detail widget; its lines are the server's clamp warnings (§7.1) and its
empty arm is `EFFECTIVE_NONE`, which the New Run rail's preflight block renders too — one spelling, both sites.
`LAUNCH_WARNING_TITLE` heads the 201's advisory `warnings[]` inline in that rail, and `OPEN_RUN_CTA` is the
primary button the screen becomes while they are on screen: a run that launched WITH a warning is never
navigated away from on a timer — the member opens it when they have read them.
`SSO_START_URL_MANAGED` replaces the login pane's start-URL FIELD whenever the sign-in runs under a
`per_user` row (the member's Getting Started button, and the admin's own sign-in on the Agents tab):
the server signs in against the row's stored `sso_start_url` and ignores a typed one, so the field
was a control with no effect. The admin's ordinary Settings sign-in — no row, or `shared` — still
asks for the portal, because nothing is stored to use.

## 8. Where to apply (once implemented, out of scope this round)

- **`/providers`** — `screens/providers/{providers-screen, git-tab, storage-tab, agents-tab,
  display}.tsx`; `lib/api/providers.ts`, `lib/api/agent-providers.ts`;
  `lib/workspace-providers-copy.ts` + test (§7.2–§7.5, §7.7).
- **Funnel step + Settings** — `setup/steps.ts` (`providers`, §7.5), `setup/providers-card.tsx`,
  `settings-screen.tsx` (card order), `integrations-step.tsx` (`GitHostCard` retired).
- **`/governance` profile editor** — two `LimitNumberRow`s (U2; §7.6's six keys land in
  `governance-prompt.md`).
- **Member Getting Started** — the chip states + action + `HarnessLoginPane` (§7.7).
- **New Run** — the picker from `harnesses`, `FLOOR_UNPARSEABLE`, inline 201 warnings.
- **Run detail** — `run-detail/widgets/effective-policy.tsx` + one `widget-registry.ts` line.
- **Workspaces** — `CARD_NOT_ADMITTED` on the row and the `<Select>`; `add-workspace-dialog.tsx`'s
  hint.
- **Drives** — `REHOME_TITLE` dialog (U3, drives prompt §7.4); the managed-backend editor state
  (already gated in `drive-editor.tsx`; the drawn state pins the words).
- **Server** — the constants blocks named in §7.1's second table and §7.4, one per lane; `docs/OPERATIONS.md`
  quotes `SITE_INTERNAL_HOSTS_CIDR_HINT` and the two "Git host card" sentences change.

## 9. Owner question list

**Decided by the plan, drawn for the record (no answer needed):** Q1 — answered in substance by
§9.1's ORDER test (an ADO onboarding refused on the Workspaces step is explained by this one);
drawn in "Your work", with Essentials left as the alternative the plan itself names. Q2 — §9.1: no
second Workspaces-header button (the slot holds the drives button). Q3 — §9.2: `app` on Azure
DevOps disabled WITH its reason, never hidden (the absent variant is drawn as the losing one). Q4 —
§9.1: retire `GitHostCard` now; `LEGACY_OPEN_OTHER_HOSTS` carries the GitLab/Bitbucket case to the
Secrets page. Q5 — §9.7: the Secrets step keeps id `integrations` and label "Secrets" (renaming the
id breaks `episodesFor`). Q7 — §9.1: the Storage tab links out through `UserDrivesCard`, never
embeds the drives table. Also by rule: `S.GIT_FOOTER` is not re-frozen (§5 #1); the picker never
hides a row (§5 #8).

**Open for the owner: Q6, Q8–Q14.**

**Q6.** Legacy-open banner wording: `LEGACY_OPEN_TITLE` / `_BODY` as drafted (+
`LEGACY_OPEN_OTHER_HOSTS` under it). The banner's action is `ADD_ROW_CTA` (teal, the state's one
affirmative); an alternative is no action and the two absent rows' own "Add provider" buttons.

**Surfaced by this round:**

**Q8.** `SAVED_NARROWED`: **(a) a toast only** (the plan's word); **(b) a toast AND an amber note
that stays until the next save** — a count of newly-refused sources is worth reading twice
(CONSOLE-RULES §9). **Recommend (b)**; drawn as (b).

**Q9.** `DRIVES_OFF_BANNER`'s home: **(a) this module** (keyed here, rendered on `/drives`) — the
switch it describes is here; **(b) `user-drives-prompt.md` §7.4** with U1. Either way the string is
the same; (b) moves one row and one count. **Resolved (b), 0.7.2, landed by U3 in §7.2 (not §7.4 —
DRIVES's own screen-banner rows sit beside `SAVE_CTA` there, not the write-refusal table):** the
banner renders on the screen that already owns every other string beside it, and a member of this
module with zero consumers is worse than one row moved.

**Q10.** The step's done rule and badge: the plan carries both `enabledGitProviders > 0` /
`Ready · N providers` (§9.1) and `≥ 1 enabled agent row` / `Ready · N agents` (§5c.4) for the one
step. **(a) git rows decide, agents ride the card summary; (b) either tab decides, the badge names
the first that is ready; (c) both, `Ready · N providers · M agents`.** **Recommend (a)** — one done
rule per step is the funnel's contract; drawn as (a) with (c) as a variant.

**Q11.** `LLM_MECHANISM_DEAD`'s remedy clause names "Settings → Model provider" (the admin's path);
under `per_user` the member's remedy is `SIGN_IN_AWS`. **(a) the sentence takes `{remedy}`**
("sign in again under Settings → Model provider" / "sign in to AWS again from Getting started");
**(b) one sentence, the member's render prefixes the chip's action line.** **Recommend (a).**

**Q12.** `BASE_URL_INVALID` says "at least one path segment" while the model admits a bare
`https://github.com` GHES-style host and makes the org path optional for `github` (plan §5.1).
**(a) keep the sentence and require a path on `github.com` / `dev.azure.com` only; (b) reword** —
"…with a host; an organisation path where the host is shared (`github.com/<org>`,
`dev.azure.com/<org>`) — no port, no credentials, no trailing wildcard." **Resolved (b), 0.7.4** —
landed in §7.2 by lane `ui-providers-people` (F4-F11), shipped together with F4-F8's storage-hint
reword (same §7.2/§7.3 canon doc, one parity red).

**Q13.** The managed-backend directory-name options (drives-mock State 3b): **(a) disabled with
`HOME_HINT` saying why** (what `drive-editor.tsx` does today; F049's proposal); **(b) not offered**
(the plan's word "stops offering", U3). **Recommend (a)** — the drives round's own Q3 rule
(disabled-with-reason over absent); (b) is drawn as the losing variant.

**Q14.** The two unminted `user_drive_unavailable` sentences (F139/F052) are staged in §7.6 and drawn
on `canon.html` as bare lines; D9 puts them in this sitting while the M1 lane row omits them. Do they
get a proper Workspace-card state in this round — in place of the checkbox, the drives mock's 7d
shape — or with U3's?

## Adjudication

### Owner answers

_(empty — the owner fills this at the gate)_

### Round notes (author, 2026-09-11)

0. **Every §7.2–§7.7 row is DRAFT**; the word sits in each heading rather than in each row because
   a marker in the key cell would break `parseFrozenTables`' clone. Strings the plan gives are
   verbatim; the ones this round drafted are listed in the M1 report under "Why" and repeated here:
   `STORAGE_TAB`, `ROW_ABSENT_HINT`, `ROW_DISABLED_HINT` (×2), `ADD_ROW_CTA`, `REMOVE_CONFIRM`,
   `LANE_SSH_UNAVAILABLE`, `LEGACY_OPEN_OTHER_HOSTS`, `SAVED_TOAST`, `SAVED_NARROWED`,
   `SAVE_ERROR`, `ADD_WORKSPACE_REPO_HINT`, `DEFAULT_DISK_HINT`, `DOCKER_UNCAPPED_WARN`,
   `FIELD_DRIVES_ENABLED`, `DRIVES_ENABLED_HINT`, `DRIVES_OFF_BANNER`, `FETCH_FAILED_*`,
   `REHOME_TITLE`, the `CARD_PROVIDERS/AGENTS/SUMMARY` trio, `STEP_BADGE_READY_AGENTS`, the
   `PROVIDERS_400_*` / `ADMIT_*` / `DRIVE_*` / `AGENT_400_*` KEY NAMES (text is the plan's),
   `AGENT_400_UNKNOWN`, `AGENT_400_CUSTOM_MECHANISM`, `AGENT_400_PER_USER`,
   `AGENT_400_SSO_START_URL`, `AGENT_NOT_ENABLED`, every `AGENTS.*` label and hint except
   `UNAVAILABLE`, `SIGN_IN_AWS` and the two given actions, the six `MODEL_ACCESS_*` chip labels,
   `FLOOR_UNPARSEABLE`, `EFFECTIVE_*`, `RUN_CLONE_CEILING_NOTE`, `SIGNOUT_FAILED_*`,
   `TERMINAL_ESCAPE_HINT`, the six `LIMIT_*` rows, `NR_UNAVAILABLE`, `NR_GOVERNANCE_UNAVAILABLE`.
1. **Server rows are keyed flat** (`ADMIT_MEMBER`, `DRIVE_DISABLED`) rather than with the plan's
   Go-side dots (`ADMIT.MEMBER`, `DRIVES.DISABLED`), so the parser's `splitKey` resolves them and no
   `DRIVES` namespace shadows the drives module (§5 #10). The Go constants keep the plan's names.
2. **§7.6 is staging** — six lanes' strings in one place for the M2 sitting, parsed by nothing;
   each row leaves with its lane. Three Go-literal edits (F055/F065/F048) are named, not drafted.
3. **The mock is three pages + one stylesheet** (index · agents · canon) so no file passes the
   1000-line gate; the drives mock's token block is `mock.css` verbatim.
4. **`UNAVAILABLE` is sentence-cased** from the plan's fragment "not enabled by your admin"
   (CONSOLE-RULES §10); the bytes are the owner's to fix.
5. **The re-home dialog reuses `DRIVES.SAVE_CTA` as its destructive confirm** so the drives prompt
   gains exactly the ONE row the plan counts (141 → 142).
8. **Review round (2026-09-12), applied:** the funnel step's body is the CARD (its footer Next is
   the step's one teal; the forms and `SAVE_CTA` live on `/providers`) — a departure from plan §9.1's
   "a full `/providers` page shared by both", forced by CONSOLE-RULES §6; the admin's own chip
   action on the Agents tab is `outline`; every admin-facing and run-time server string moved out
   of the parsed range into §7.1's second table (Go-checked, unparsed), leaving three member doors
   keyed in `PROVIDER_MEMBER`; the two step badges pluralise; `CARD_OPEN` lost its period.
7. **Four keys were prefixed to keep every §7.2–§7.7 key unique** — the plan names the Agents tab's
   `TITLE` / `LEAD` / `FIELD_ENABLED`; the cloned test's one lookup across namespaces assumes no
   collisions, so they are `AGENTS_TITLE` / `AGENTS_LEAD`, the switch reuses `PROVIDERS.FIELD_ENABLED`,
   and the agent row's off-hint is `AGENT_ROW_DISABLED_HINT`.
6. **Drives-mock State 3b is drawn here**, not inserted into the drives mock (M1 writes only its
   own two paths); U3 may move the block when it lands.

Not drawn, deliberately: a nav item, a Workspaces-header button, a GitLab/Bitbucket row, a drives
table on the Storage tab, a "managed by your org" badge, a cross-mechanism fallback toggle, a
background-renewal setting, the `workspace_provider` permissions form (unchanged in shape).
