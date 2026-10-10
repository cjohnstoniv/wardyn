# Three roles, and who sets the walls

What a super admin, a security admin and a member may do, and the governance profiles that
bound them. Read this after [who-gets-in.md](who-gets-in.md), which says how a session
derives its role.

## Three roles, and who sets the walls

- **Super admin (the deployer).**
  - Installs the chart, connects the IdP, and owns everything only the chart can say:
    - the issuer and client, the boot role map (`WARDYN_OIDC_ROLE_MAP` — chart rows always win), the operator allowlist (`WARDYN_OIDC_OPERATOR_EMAILS` — also the boot posture floor),
    - the default role, the deployment ceiling (`WARDYN_DEFAULT_POLICY`), trust roots, integrations, base images,
    - and the People page (who gets in, and who else is an admin).
  - The admin bearer token is the break-glass and remains exempt from every console lockout guard.
- **Security admin (`security_admin`).**
  - A mapped tier — never a default, and never derivable from the operator allowlist; you create one by mapping an IdP App Role or group to `security_admin` in the role map (chart or People page).
  - Security admins read the whole workspace inventory AND any workspace in it — the list, the row (projected), its build status (image and log blanked) and its **observed egress**.
  - That is because they decide that workspace's allowed and denied hosts and the observed traffic is the input to that decision.
  - `GET /workspaces/{id}/env-as-code` is NOT in that set (owner-or-super, since F287): its files carry the internal registry coordinate, the site-config artifact redirects and the operator's setup commands, none of which are an egress-decision input.
  - They cannot otherwise WRITE a workspace: renaming, reassigning, deleting, binding credential material and launching a recording all stay with the super admin.
  - Security admins author and assign **governance profiles** (named ceilings bound to users or groups), write the org allow/denylists (capability grants).
  - They decide escalated approvals — egress, credential, tool — on anyone's run.
  - They revoke sessions and API tokens, and verify the audit chain.
  - They can also **stop** any run in the deployment.
  - Killing a foreign run is incident response, and the most time-critical thing this tier does.
  - It is a stop, which is deliberately *not* the same as reaching INTO one:
    - no attach ticket, no shell, no credential material, no host.
  - Inspect-or-stop is the whole of that warrant.
  - (The batch form, the sandbox sweep, stays admin-only: it drives the container runtime across every run at once, which is host reach rather than run reach.)
  - They **promote** a workspace's recorded egress into its allowlist, but they cannot **record** one.
  - Launching a recording session opens an interactive sandbox with open egress, the workspace's directory bind-mounted and its credentials injected, which is reach into a run, credential material and the host.
  - These are the three things this tier is defined never to have.
  - So `POST /workspaces/{id}/record` is admin-only and the console shows a security admin that control disabled beside the promote control it leaves live.
  - They also cannot touch the People page, integrations, site-config writes, base images, or the deploy funnel.
  - They run under a governance profile themselves if one is assigned to them, since only `admin` is exempt from ceiling resolution.
  - A profile can only make the deployer's stored credentials *less* available, never more — and a security admin widening their own egress is an audited act, visible in the log they cannot rewrite.
  - No capability grant can widen anyone to admin; that invariant is what makes delegating `/permissions` safe.
- **A security admin's revocations reach the super admin, deliberately.**
  - "Revoke sessions and API tokens" above is not scoped to members: `POST /api/v1/sessions/revoke` applies no target-role check, so a security admin may cut a *super admin's* sessions and tokens by `sub`.
  - The `{"all":true}` arm logs out **every** principal and revokes **every** live API token in the deployment — CI and automation credentials included — in one audited call.
  - That is the tier working as designed.
  - Incident response is the security admin's job, the two tiers deliberately do not nest (a security admin still cannot reach into a run, and their SSH key and attach ticket still stamp `user`).
  - A revocation only ever *subtracts* reach — it grants the caller nothing.
- What bounds it is that a revocation is not a lockout.
  - The session cutoff is a **timestamp**, not a flag: signing in again mints a session issued after the cutoff, which clears it with no operator action.
  - The **admin bearer token never consults revocations at all**, so the break-glass above survives a `{"all":true}` — a security admin cannot use this to lock the deployer out of undoing it.

> [!WARNING]
> API tokens are the one part that does not self-heal: they are revoked permanently and must be re-minted, so treat `{"all":true}` as an incident lever rather than a routine one.

- Every call is audited as `session.revoke` with its scope and the number of tokens revoked, under the calling security admin's own principal.
- **User (member).**
  - Signs in, runs agents inside the governance profile their group is assigned (or the deployment ceiling if none).
  - The profile is enforced outside the sandbox: inline policies are clamped to it, saved policies are clamped to it on selection (for anyone a profile is assigned to).
  - Authoring no policy at all yields it, and its denied hosts are re-asserted when the run is dispatched.
  - A denied host cannot receive an injected or brokered credential at all.
  - What a member can change is what the profile leaves open; what they can ask for is an escalation on the Approvals page.
- **Governance profiles.**
  - One profile per subject; when several match, the most specific wins (user beats group beats user type beats everyone; priority breaks group ties).
  - The Governance page shows the resolved answer, and `GET /policies/default` returns the ceiling that actually binds the caller.
  - A profile is either **standalone** or **composed** (0.8.6).
  - The walkthrough below covers composed profiles from authoring to rollback; the design record is [`docs/design/0.8/0.8.6-comp.md`](../design/0.8/0.8.6-comp.md).
- *Standalone and composed.*
  - A standalone profile replaces the deployment ceiling for its subjects, exactly as on 0.8.5.
  - A composed profile stores no ceiling of its own.
  - It names a **base**, which is another profile (`base_profile_id`) or, when that is null, the deployment default, and an **overlay** (`overlay`, plus `overlay_limits` for the limits) that can only narrow that base.
  - The ceiling that binds is `ApplyOverlay(effective(base), overlay)`, computed whenever authority is read and never stored, so a change to a base reaches every profile built on it.
  - A composed row's own `ceiling` and `limits` columns are `{}`, and the API adds a read-only `effective: {ceiling, limits}` beside them.
- *Authoring.*
  - A profile names at most one base.
  - A chain is at most three profiles deep, counting the profile itself (a baseline, a division and a team), and a chain cannot loop.
  - A write that would make a cycle or push any existing descendant past three is a `409` (`governance_profile_cycle`, `governance_profile_depth`).
  - An overlay lists only the fields it narrows.
  - An absent field inherits the base unchanged, and a present empty list is a value (`allowed_domains: []` narrows to no domains, `allowed_methods: []` is refused because it would mean every method).
  - The write is strict.
  - Take an overlay that names something its base does not permit:
    - a domain the base's `allowed_domains` does not cover under the proxy's own matcher, a method the base excludes,
    - `allow_all_egress` on a base without it, a grant the base's grants do not dominate.
  - Such an overlay is `400 governance_overlay_invalid`, and so is an overlay whose meet with the base would be empty rather than narrow.
  - A `PUT` that omits `base_profile_id`, `overlay`, `overlay_limits` or `contact` keeps the stored value, so an older client cannot flatten a profile by accident.
  - Only an explicit `null` clears one, and `overlay: null` turns the profile back into a standalone one (the request must then carry a valid `ceiling`).
- *Resolution, and what the meet does.*
  - Resolution reads the chain once and composes from the deployment down.
  - Each field has its own meet, taken after the runtime's own defaults are applied to both sides, so a zero that means "the default" is never read as "smaller":

| `RunPolicySpec` field | Unset means | Meet |
|---|---|---|
| `allowed_domains` | nothing allowed | each overlay entry must be covered by the base's entries; the overlay's entries are the result |
| `denied_domains` | none | union |
| `allow_all_egress` | false | AND (an overlay may only set it false) |
| `first_use_approval` | `always_deny` | the stricter mode |
| `first_use_hold_seconds` | 30 | smaller |
| `max_holds` | 16 | smaller |
| `allowed_methods` | every method | intersection of non-empty sets; empty on one side takes the other; disjoint is unsatisfiable |
| `min_confinement_class` | required | the higher class |
| `eligible_grants` | none | the overlay's grants, each re-checked against the resolved base |
| `auto_stop_after_sec` | 0 or less never reaps | smaller positive |
| `workspace_mounts` | none | intersection by source and target; read-only if either side is |
| `workspace_repos` | none | intersection by identity |
| `llm_inspection` | none | the side that sets it; both set and different is unsatisfiable |
| `ui_apps` | none | intersection by name, port and path (an overlay app that serves a different path than the base's is a widening) |
| `resources` | the deployment's size | smaller, per field |
| `tool_rules` | an unnamed tool is held | per tool named on either side, and `*`: the stricter effect |
| `git_push_any_branch` | false | AND |
| `push_rules.deny_paths`, `push_rules.require_review_paths` | none | union |
| `push_rules.max_inspect_pack_mib` | 32 | smaller |
| `push_rules.hold_seconds` | 120 | smaller |
| `push_rules.max_file_size_mib` | off | smaller positive (off is unbounded) |
| `push_rules.deny_new_executables` | false | OR |
| `azure_devops_capabilities` | the provider row's default | intersection of non-empty lists; disjoint is unsatisfiable |

| `GovernanceLimits` field | Unset means | Meet |
|---|---|---|
| `deny_task_mode_exec`, `deny_interactive`, `deny_ui_apps`, `deny_user_drive` | false | OR |
| `max_concurrent_runs`, `max_ephemeral_disk_mib`, `max_drive_size_mib`, `max_cpu_millis`, `max_memory_mib` | 0 is unlimited | smaller positive |
| `autonomy_rubric` | caps nothing | per field, the lower level; a field set on one side only takes that side |
| `max_end_ahead_sec` | 0 is no limit | smaller positive |
| `default_end_sec` | 0 is `max_end_ahead_sec` | smaller, then clamped to the resulting maximum |
| `max_wait_sec` | 0 is the deployment's approval expiry | smaller |
| `default_wait_sec` | 0 is the deployment's approval expiry | smaller, then clamped to the resulting maximum |
| `allow_no_end`, `user_changes_limits` | false | AND |
| `pause_idle_after_sec` | 0 is pause only runs waiting for a decision | smaller positive |

- *Worked example.*
  - An organisation keeps three profiles.
  - The **baseline** is a composed profile with no base (so the deployment default is its base) and an overlay that sets `allowed_domains` to the package registries and the forge, `first_use_approval` to `always_deny`, and `limits.max_cpu_millis` to 4000.
  - The **division** profile names the baseline as its base and an overlay that drops the forge host from `allowed_domains` and sets `max_concurrent_runs` to 6.
  - The **team** profile names the division and an overlay that sets `allowed_methods` to `GET` and `HEAD`, `denied_domains` to one extra host, and `max_cpu_millis` to 2000.
  - Assigned to the team's group, the team profile binds this:
    - the registries only, the forge dropped, `GET` and `HEAD` only, the extra host denied, 2000 millicores (the smaller of 4000 and 2000), six concurrent runs, and the baseline's `always_deny`.
  - Later the baseline's owner narrows `max_cpu_millis` to 1000: the next read of the division and the team gives 1000, with no write to either.
  - A baseline edit that would leave a descendant empty (it narrows `allowed_methods` to `POST` while the team overlay allows only `GET` and `HEAD`) is refused with `409 governance_overlay_unsatisfiable` naming that descendant.
  - Widening the baseline later widens every field a descendant's overlay leaves unset, as any base edit does.
  - It never switches on an overlay entry the base did not permit, because an overlay is checked against the base at write.
- *A base that moves under an overlay.*
  - A write is strict, but a base edit, or a redeploy that narrows the deployment default, is someone else's act arriving later.
  - At resolve the meet drops what the base no longer covers: the run's `201` lists the drop in `clamp_warnings` (naming only the member's own profile), and an administrator sees it in that profile's `effective.warnings` on `GET /governance`.
  - If nothing satisfies the base and the overlay together (the deployment default narrowed until their `allowed_methods` are disjoint, say), the launch and every live door refuse with `403 governance_overlay_unsatisfiable`, audited, until an administrator fixes it.
  - A chain that cannot be read, or one that loops or runs deeper than three, fails the same doors with a `500` or `503`.
  - Neither case is ever read as the deployment's policy.
  - An administrator sees which profile failed on `GET /governance` (`effective.error`).
- *Every reader sees the composed answer.*
  - Create, preflight and dispatch, and the doors that bind runs already going (attach and SSH, UI apps, revive, the limits re-clamp, end extension, the run policy view and the preview) all resolve through one code path.
  - A source guard fails if any other code reads a raw profile row.
  - A profile edit still reaches an already-running proxy only through the denies re-asserted at revive or restart.
  - A base edit now does so for a whole subtree at once, so one edit has a larger reach and the same delay.
- *What a member sees.*
  - A member sees their own profile's name and contact, and the effective content: `GET /policies/default`, `/me` and denial bodies serve the profile that binds them, never the chain.
  - A base's name, overlay and contact are not disclosed, and a profile with no `contact` falls back to the site's `policy_help` rather than inherit a base's contact, since that would name the base.
  - Only an admin or a `security_admin` can read the graph (`GET /governance`).
  - When you write an example for a member, show the effective result and the profile's own name; do not describe the structure behind it.
- *Deleting and unassigning.*
  - Deleting a profile requires unassigning it first, and a base that still has profiles built on it is a `409` naming them (never a silent widening).
- *Exporting a graph (CLI and SDK).*
  - `wardyn governance get` exports a composed graph and `wardyn governance set` applies it to another install; the Go client's `ApplyGovernance` does the same ([`docs/sdk.md`](../sdk.md), "Composed profile graphs").
  - Profiles are written bases first, every graph reference (`base_profile_id`, an assignment's `profile_id`) is remapped through the target's names to ids, the read-only `effective` view is never written back, and prune deletes descendants before bases.
  - Under second-person approval (four-eyes) a write may return `202`: a child whose base is still pending, and an assignment whose profile is pending, are deferred and reported, not sent with a dangling id.
  - Apply again once the base is approved.
- *Upgrade and rollback.*
  - The 0.8.6 migration only adds nullable columns, so every existing profile is standalone and resolves exactly as on 0.8.5.
  - An older SDK or CLI sees `ceiling: {}` on a composed profile; its re-apply compares equal and sends nothing, and a `PUT` it does send omits the composition fields, which are kept.

> [!IMPORTANT]
> There is no downgrade: a 0.8.5 binary refuses a database the migration has touched, and converting composed profiles to standalone first changes nothing it sees.
> Restore the pre-upgrade dump, which holds no composed profile.

*Residual risks.*

- Running proxies keep their dispatched egress until revive or restart (above); a base edit
  makes that reach wider, not faster.
- A deployment redeploy that narrows `DefaultPolicy` can strand a subtree: its launches are
  refused with a named reason rather than widened. Look at the profile's `effective` and the
  denial stream.
- The overlay and the proxy share one domain matcher. That removes drift, and a matcher bug
  now affects both sides alike; an oracle test over a fixed host corpus bounds it.
- The comparison used to exempt narrowing edits from second-person approval is conservative,
  so some narrowing edits still need a second approver. That is the safe direction.
- Operators resolve no profile; that is a separate predicate from composition.

Stated honestly: profiles narrow by omission — a profile that omits secret grants revokes them for its subjects (the editor warns).

- A member's long-lived API token keeps the group snapshot it was minted with until re-minted.
- Sandbox size is the exception.
  - A profile that omits `resources`, or leaves one of its fields at zero, inherits the deployment's size for that field (the default policy's `resources`, else `WARDYN_SANDBOX_DEFAULT_CPU_MILLIS` / `WARDYN_SANDBOX_DEFAULT_MEMORY_MIB`), so omission never grows a sandbox.
  - A profile that sets a size keeps it, and the profile's `limits.max_cpu_millis` and `limits.max_memory_mib` (0 is unlimited, negatives refused) cap CPU and memory for its assigned members at create, preflight and dispatch; operators are exempt.
- **Limits that reach a running run (0.8.2, #1391, #1392).**
  - Two limits bind past create, keyed on the profile the run was created under as it stands now, so a limit set later reaches runs already going (a deleted profile binds nothing).
  - `deny_interactive` also refuses a terminal attach (`wardyn run attach` and the console terminal) and every SSH-gateway connection into any run under the profile, exec runs and the run's owner included.
  - (A super admin enters only a run of their own or one with no personal owner, so the exemption below reaches no person's run.)
  - The attach is an `authz.denied` row (`governance_profile`, target `runs.attach`), the SSH refusal an `ssh.authenticate` failure naming the profile.
  - The harness sign-in run is exempt, as it is at create.
  - `deny_ui_apps` strips `ui_apps` from a run at create, with a `clamp_warnings` sentence and a `dropped` row at target `runs.ui_apps`, and the UI gateway refuses a session into a run created under the profile.
  - An empty ceiling `ui_apps` is still no opinion, so a profile without the limit behaves as before.
  - A super admin is exempt from both at every door, as at create; a security admin is bound.
  - A limit set later reaches new sessions but does not sever ones already open: a terminal, SSH session or UI-app session opened before the limit was set runs until it ends.
  - `deny_ui_apps` does not close an SSH port forward to the app; `deny_interactive` does.
- **The autonomy rubric (0.8, #77).**
  - A profile may also carry `limits.autonomy_rubric`, nine closed fields — three egress postures (`egress_open`, `egress_reviewed`, `egress_sealed`), three secret postures (`secrets_powerful`, `secrets_baseline`, `secrets_none`) and three enforced confinement classes (`confinement_cc1`, `confinement_cc2`, `confinement_cc3`).
    - Each is unset or one of four autonomy levels:
      - `L0` attended (interactive only, supervised seeding), `L1` gated (adds non-interactive runs, but `tool_approvals` is derived to `hold`),
      - `L2` unattended (adds `auto` approval and `seed_auto_tools`), and `L3` (adds `task_mode=exec`, the door that routes around every other gate, so it is the top rung).
  - Below `L3`, an interactive run with a task must use `interactive_start=agent`: the shell startup form (`interactive_start` unset or `shell`) runs the task at sandbox boot the way exec does, and is refused (`runs.interactive_start`).
  - `resolveRunAutonomy` ([`internal/api/runs_autonomy.go`](../../internal/api/runs_autonomy.go)) grades the run's real posture — egress reach graded on the same union `unionRunEgress` builds, secret power, and the already-enforced confinement class — against the assigned profile's rubric and folds every field the posture matches to its **minimum** level.
  - A nil rubric, or a posture none of the nine fields caps, binds nothing (today's behaviour, unchanged).
  - The same function backs both `POST /runs` and `POST /runs/preflight`, so the level Review shows is the level launch enforces.
  - A run whose declared shape exceeds its resolved level is refused `governance_profile` (see [§ Every denial that isn't a 404](denials.md#every-denial-that-isnt-a-404) for its `target`s).
  - A non-interactive run resolved to exactly `L1` is not refused when its agent has a tool-approval lane (claude-code): it launches with its tool approvals derived to `hold`, and the 201 carries a warning saying so.
  - Any other agent — codex-cli, a BYOA image — has no lane to derive a hold into, so the same run is refused with target `runs.agent`.
  - The resolution — level, posture, and every rubric field that tied at that level (`bound_by`) — rides the create audit row's `autonomy` field and is frozen on `agent_runs.autonomy_level`.
  - **Not in the posture:** the model-provider hosts egress dispatch resolves from global configuration after this gate runs (a Bedrock run's region, for one), and any stored-credential residency.
  - A run's autonomy level is graded on what the run can reach and hold, not on where its model credential lives.

## When everyone is an admin, and what a refused person is told

**The everyone-is-an-admin warning.** With SSO configured, the setup checklist's "Who is an admin" row grades `warn` — holding the console in the People step and showing every admin a banner above every page — on either of two conditions (#491):

- **No role map and no admin list.**
  - A person nobody has mapped derives `admin` when there is **neither** a role map (the chart's `WARDYN_OIDC_ROLE_MAP` or a People-step row) **nor** an admin list (the operator allowlist, `WARDYN_OIDC_OPERATOR_EMAILS`).
  - An admin list alone is enough to clear this: an unmatched person then derives `member`.
- **A role map IS set (chart or People step), but `WARDYN_OIDC_DEFAULT_ROLE=admin`.**
  - Every sign-in the map doesn't match still falls through to `admin` — before #491 this read `ok`, since a role map being set was all the check looked for.
  - Fix by setting `WARDYN_OIDC_DEFAULT_ROLE` to `user` or a user type instead.
  - An admin list alone does not trip this: with no role map, a sign-in the (empty) map doesn't match derives `member` regardless of the default role.

A deployment that hits BOTH conditions (no role map, no admin list, AND `WARDYN_OIDC_DEFAULT_ROLE=admin`) reads the first condition's own sentence — one banner, not two competing ones. Members see neither.

- **Request-access help (`sign_in_help_text`, `sign_in_help_url`).**
  - Two optional SiteConfig fields, edited on the People step ("When someone can't sign in") or through `PUT /site-config`.
  - The sign-in page shows them under Wardyn's own sentence — never instead of it — on the four refusals a person cannot clear alone:
    - no role, an email domain that isn't allowed, too many groups to list, and a missing `email_verified` claim.
  - Timeouts and configuration errors get nothing.
  - **Both are public by design:** the anonymous `/healthz` publishes them, because the reader has, by definition, not signed in — so name your request process, not your internal systems.
  - The text is plain text (at most 1,000 characters; no line breaks, control characters, line/paragraph separators or invisible format characters such as bidi overrides and zero-width spaces; quotes are fine) and is rendered as text, never markup.
  - The link must be an `https://` address with a real host name — no spaces, no `user:pass@`, none of those hidden characters, a query string is fine — and always reads "Request access".
  - A link saved as `http://` before 0.8 keeps working and is published unchanged, but the setup checklist warns about it (row `sign_in_help_url`) until you change it.
  - A save that sends it back unchanged is accepted, and a new `http://` link is refused.
  - That includes an MDM or CLI baseline (`wardyn site-config set`) whose `http://` link differs from the stored one.
  - The whole re-apply is refused with a 400 on every boot (`wardyn-desktop.sh` logs "site-config set failed") until the baseline file names an `https://` link.
  - Every write records both values in the clear on `site_config.write`.
  - A write outside those bounds is refused with a 400 naming the field, and a stored value that no longer passes is dropped from `/healthz` rather than published.
  - Like the provider blocks, a body that does not name a field carries the stored value forward; name it as `""` to clear it.

## UI apps with more than one user: use host mode

- If the UI-sandbox gateway is on and more than one person uses this install, set `WARDYN_UI_SANDBOX_ORIGIN_TEMPLATE` (`uiSandbox.originTemplate` in the chart), e.g. `https://run-{run}.ui.example.com`, with wildcard DNS and a wildcard certificate.
- Without it the gateway runs in path mode: every run's relayed app is served from ONE browser origin, separated only by a path-scoped cookie.

> [!WARNING]
> That cookie decides which session a request carries, but not what a page may read: any relayed page on that origin can script any other page there that is open in the same browser.
> That is the shared-origin residual ([THREAT-MODEL.md](../../threatmodel/THREAT-MODEL.md) §5 #18).

- Two controls keep another user's app out of that origin in your browser.
  - The enter ticket is bound to the browser that minted it, so nobody can push you into their app with a link or a form.
  - A relayed app cannot register a service worker outside its own path ([UI-SANDBOXES.md §3](../UI-SANDBOXES.md#3-open-an-app) and [Bounds](../UI-SANDBOXES.md#bounds)).
- They leave one boundary to the path cookie alone: your own apps can still reach each other.
- Host mode gives every run its own origin, which the browser itself isolates, and it refuses an enter served on any other run's host.
- Path mode is for a single-user or demo install.
- Either mode needs the console and the gateway on **the same site** (one registrable domain, one scheme).
  - The enter binding is a cookie the console's fetch sets on the gateway, and a browser refuses that across sites.
  - Open then fails with an error that says so.
