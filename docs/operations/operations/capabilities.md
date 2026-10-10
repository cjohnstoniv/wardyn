# Capabilities: what one member, or one group, may do

The ten capability kinds, the precedence that resolves them, and the group snapshot's
ceiling. One sentence is the doctrine: a capability bounds what the MEMBER chose, never what
the ADMIN pre-authorized.

## Capabilities: what one member, or one group, may do

- The role split above is deployment-wide.
- A **capability grant** is per-human: a row naming a *subject*, a *kind*, a *value*, and an effect of `allow` or `deny` (`capability_grants`, migration 0042), with a per-kind **enforcement switch** beside it (`capability_enforcement`).
- One sentence is the doctrine, and every rule below follows from it: **a capability bounds what the MEMBER chose, never what the ADMIN pre-authorized.**
- So a stored policy, a workspace's own requirements, the hosts a workspace scan seeded, the model provider's own egress, and the grants `applyWorkspaceRequirements` re-adds at launch are all left untouched no matter what a member holds.

- **The ten kinds** — a closed set, written down once in Go (`capabilityKinds`, [`internal/api/capabilities.go`](../../internal/api/capabilities.go)) rather than as a schema CHECK:

| Kind | Value | Direction | What it bounds, and where |
|---|---|---|---|
| `egress_host` | a host, or a `*.suffix` wildcard | narrows | which host a member may **decide** an `egress_domain` approval for (`authorizeUserDecision`, [`internal/api/approvals.go`](../../internal/api/approvals.go)), and which hosts survive on a member's own `inline_policy` allowlist (`narrowUserInlinePolicy`, [`internal/api/inline_policy_bounds.go`](../../internal/api/inline_policy_bounds.go)) |
| `secret` | exact secret name | narrows | which stored secret a member's own `inline_policy` grant may reference — both refs of an `ssh_key` grant, key and `known_hosts` — and which names `GET /secrets` lists back to them (`handleListSecrets`, [`internal/api/secrets.go`](../../internal/api/secrets.go)) |
| `workspace` | workspace uuid | narrows | which onboarded workspace a member may name on `POST /runs`/preflight (`denyUserRequest`, [`internal/api/runs_create_validate.go`](../../internal/api/runs_create_validate.go)) |
| `image` | exact image ref | **widens** | which custom sandbox image a member may launch at all — without a grant, none (same seam) |
| `agent` | exact `--agent` string | narrows | which agent/harness a member may launch (same seam). Deliberately NOT constrained to the harness catalog, at the gate or at the grant write: `WARDYN_AGENT_IMAGES` custom agents are supported, so a catalog check would make an operator's own entry unwriteable |
| `workspace_provider` | exact git provider row id | narrows | which git provider row a member's repositories may come from; see [`workspace_provider`](#workspace_provider) below |
| `model_provider` | exact model provider id | narrows | which model provider a person's run may use; see [`model_provider`](#model_provider) below |
| `feature` | `ssh_key`, `api_token` or `custom_component` | narrows | whether a member may add an SSH key, mint an API token or define a custom component at all; see [`feature`](#feature) below |
| `policy` | stored policy uuid | narrows | which stored policy a member may select for their own run (`policy_id` on `POST /runs`/preflight, `denyUserRequest`, same seam). |
| | | | Only the choice: the selected row is still bounded by the member's ceiling, and a run that names no policy is not gated. |
| | | | Checked before the row is read, so an ungranted id is refused whether or not it exists |
| `component` | org component uuid | narrows | which org component (an admin-written row) a person may attach to their own run (`componentAttachRefusal`, [`internal/api/components_authz.go`](../../internal/api/components_authz.go)). |
| | | | An org component's id is restricted ("Available to") from its create, so nobody may attach it until an allow row names it — a wildcard allow does not. |
| | | | A component a person defines themselves is the `feature` value `custom_component`'s question, not this kind's |

- `*` as a value matches everything of that kind, spelled the same way for all ten.
- 0.8 retired one more, `integration`, with the AI integrations it bounded: a run's `integration_id` is refused with a `422` for everyone, so there is nothing left for it to gate.
- Its stored grant rows are inert, and a new one is refused.
- `egress_host` values are matched by `entryCoversAny` ([`internal/api/artifact_redirect.go`](../../internal/api/artifact_redirect.go)):
  - the *same* matcher that decides whether one allowlist entry covers a host, deliberately not a second one, because two host matchers that disagree is how a deny gets bypassed by a port suffix.
- Every other kind is an exact compare.
- A **deny** on `egress_host` asks that matcher in BOTH directions and bites whenever the two sets intersect: `deny secret.example.com` stops a member asking for `*.example.com`, `deny *.corp` stops one asking for `evil.corp`.
- An allow still has to COVER the request outright — a half-overlapping grant permits nothing — and `*.example.com` never covers `example.com`.
- Grant values are shape-checked at write time by the same validator every `allowed_domains` ingest uses, so a value that could never match is a `400` rather than a row that quietly does nothing.

- `devcontainer_repo` is **not** a kind and stays unconditionally admin-only: it hands attacker-authored build configuration to the image builder.

- **Precedence — the order is the design** (`capAllowed`, same file):

1. **admin, admin token, and local mode are exempt.** A capability bounds a member; the admin tier is the one writing the grants.
2. Any matching **deny** ⇒ refused.
3. Any matching **allow** ⇒ permitted.
4. The kind is **not enforced** ⇒ permitted.
5. Otherwise ⇒ refused.

- There is no user-over-group precedence: a deny anywhere wins, because "Bob's user allow overrode the group deny" is a breach report.
- Deny sits *above* the enforcement switch on purpose, which makes deny rows the adoption on-ramp: blacklist one host for one contractor without flipping the whole deployment fail-closed.
- A store error is never permission — the request answers `500`.

- **The widening kind reads the same rows the other way.**
- For `image` (`capGranted`) an unenforced kind is *refused*, not permitted, because 0.5 refused it too.
- Both directions obey "an upgrade with no configuration changes nothing".
- So `image` needs *both* the switch on and an exact-ref grant; the other nine need only the absence of a deny until you enforce them (a value an admin restricted, as a new organisation component is, also needs an allow row naming it).
- That rule is also why `agent` narrows rather than widens:
  - launching an agent is something every member could already do,
  - so a widening kind would refuse every member run on every deployment that has not enforced it — i.e. all of them on upgrade day.

- **Default posture: an absent enforcement row is off.**
- A deployment upgraded from 0.5 with no rows written behaves byte-for-byte as before.
- Turning `workspace` on with no grants written locks every member out of every workspace at once; write the grants (or the targeted denies) first, then flip the switch.

- **Subjects, and the group snapshot's ceiling.**
- A grant's `subject_type` is
  - `user` (matches **either** the lowercased OIDC `sub` **or** the email — an admin writing a grant shouldn't have to guess which the IdP made authoritative; a deny on either identity hits),
  - `group` (the login-time union of the ID token's `roles` and `groups` claims, lowercased and deduped — so Entra App Roles are grantable for free),
  - `user_type` (everyone of one user type, named by the type's id — 0.8),
  - or `all` (every signed-in human).

- A person holds exactly one user type, stamped at sign-in,
  - so a `user_type` row is one more subject in the union above:
    - a type **allow** is one more way in, and a type **deny** is a wall no user or group allow lifts for anyone of that type.
- A security admin of that type is bound by it like anyone else; only a super admin is exempt.
- The type must exist when the row is written (`400` otherwise), and a type cannot be deleted while any grant, governance assignment or drive grant names it.
- For the governance ceiling and user drives the type is a **tier**, not a union: `user > group > user_type > all`, so a group assignment overrides the type's profile and the type's profile overrides `all`.
- An API token carries no type yet and answers as the built-in `standard` type.
- A session whose type was deleted after sign-in is refused (`403`, `user_type_unknown`) wherever a control names a type, never resolved without it.

- A `group` subject must be **printable ASCII**, and the write is refused with that reason when it is not — the same rule a console role mapping already gets.
- The snapshot a group grant is matched against carries printable ASCII only.
- So a subject outside that set is a row that can never match anyone: a deny that protects nothing while the Permissions screen renders it as active.
- The check runs on what you typed, *before* lowercasing.
- So a look-alike character that collapses onto one of your ASCII group names (Unicode case folding maps KELVIN SIGN to `k`) is refused rather than quietly stored as the real group.
- The same refusal guards a governance assignment's subject, for the same reason.

- Group membership is a **snapshot taken at login**, carried in the session cookie.
- Grants are read from the database per request, so a new grant takes effect on the very next request but a *directory* change does not until the human signs in again.
- The snapshot is capped at 2048 payload bytes (`maxSessionGroupsBytes`, [`internal/auth/oidc/derive.go`](../../internal/auth/oidc/derive.go)) so the signed cookie stays under the ~4096 bytes a browser silently drops entirely.
- Groups are sorted and dropped **from the end**, so the same human loses the same groups every login instead of a coin flip.
- That is roughly 100 typical group names — past that, grant the user directly, or prefer Entra App Roles on the much smaller `roles` claim.
- `groups_snapshot_stale` on `GET /me/capabilities` reports the "can't tell yet" state distinctly from "holds no groups", because the two must not read the same — but as of 0.7 it is never a *pre-upgrade cookie* that produces it.
- The session payload carries a codec version and `decodeSession` requires an exact match (`SessionCodecVersion = 1`, [`internal/auth/oidc/session_codec.go`](../../internal/auth/oidc/session_codec.go)),
  - so a cookie minted before 0.7 — which has no `v` key at all — is not a stale-groups session;
  - it is not a session, and the human is bounced to sign in.
- See "[Upgrades](../OPERATIONS.md#upgrades)" below.

- **A DENY is never allowed to evaporate with the snapshot.**
- A group that fell off the 2048-byte cut — or one your directory names with a character the snapshot cannot carry, or a pre-0.7 API token whose completeness was never recorded —
  - has none of its group rows in the scan.
- For an ALLOW that costs the caller access, which is the safe direction.
- For a DENY it would hand back exactly what the row forbade,
  - so the resolver (`capScan`, [`internal/api/capabilities.go`](../../internal/api/capabilities.go)) checks whether **any** group-subject deny row of that kind could cover the value, and refuses when one could —
  - the same scoping the ceiling refusal gets: a deployment with no group deny rows behaves byte-for-byte as it did before.
- The refusal reads as an ordinary capability denial, with a server log line naming the unanswerable snapshot; signing in again (or re-minting the token) resolves it for good.
- Where a deny has to bite with no store read at all, write it against the **user** (either identity).

- **Re-mint pre-0.7 API tokens.**
- A token minted before 0.7 recorded nothing about whether its group snapshot was complete, and that unknown is read as "incomplete" — the fail-closed choice.
- So every request such a token makes takes the extra check above, on every capability it touches, for the whole life of the token.
- It is correct but it is not free, and it is the one lasting cost of the upgrade:
  - re-minting moves those callers (CI jobs, scripts, the headless `wardyn run` lane) back onto the ordinary indexed path and removes the standing possibility of a group-completeness refusal they cannot themselves resolve.
- `GET /api/v1/tokens` lists the deployment's tokens with their `last_used_at`, so the dead ones can be revoked rather than re-minted.

- **A third cause of a partial snapshot: the IdP's own overage.**
- Entra ID stops sending the `groups` (or `roles`) claim altogether once a human is in more groups than the token limit —
  - **200** for a JWT, 150 for SAML —
- and sends a `_claim_names` / `_claim_sources` pointer to Microsoft Graph in its place.
- Wardyn does not dereference that pointer.
- It marks the snapshot **truncated** (`sessionGroups`, [`internal/auth/oidc/derive.go`](../../internal/auth/oidc/derive.go)), which reads downstream exactly like a group that fell off the byte cap:
  - the ceiling resolver treats it as unanswerable rather than as "asked, there were none".
- Without that, such a login would arrive complete-and-empty and quietly shed every group-tier grant and governance assignment.
- Where members legitimately sit in that many groups, the answers that do not depend on the size of the claim are Entra App Roles (the much smaller `roles` claim) and user-subject grants.

- **Every workaround that merely SHRINKS the group claim trades a detected failure for an undetected one.**
- An overage is loud: the claim is absent, the snapshot is marked truncated, and the resolver refuses rather than guessing.
- A FILTERED claim is silent.
- Set `groupMembershipClaims: "ApplicationGroup"` — the "Groups assigned to the application" option, which Microsoft recommends for exactly this limit — and the token carries a smaller list that is *complete by the IdP's account*:
  - no `_claim_names`, no truncation bit, nothing downstream to refuse.
- A governance assignment or a group DENY row keyed on a group that is no longer emitted simply stops applying.
- That is the evaporation the truncation bit exists to prevent, with the detector switched off, and **Wardyn cannot tell the two claims apart**.
- A filtered claim and a full one are identical in the token.

- What that option drops is **nested membership**: "nested groups are not included and the user must be a direct member of the group assigned to the application" ([Configure group claims for applications](https://learn.microsoft.com/en-us/entra/identity/hybrid/connect/how-to-connect-fed-group-claims)).
- The same rule governs group-based **App Role** assignment.
- Nested group memberships are not supported for group-based assignment to an application, so a role assigned to a group reaches its direct members only ([Manage users and groups assignment](https://learn.microsoft.com/en-us/entra/identity/enterprise-apps/assign-user-or-group-access-portal)).
- App Roles are a smaller claim, not automatically a safer one.

- **So re-key before you change the claim, not after:**

1. List what resolves by group today: `GET /permissions` for every `subject_type=group` grant — **deny rows first**, since those are the ones whose loss WIDENS somebody — and `GET /governance` for every group-tier assignment.
2. Re-point each one at something the new claim will still carry:
   - a group the member is a **direct** member of and that is assigned to the application, or the member themselves (`subject_type=user`, or a user-tier assignment).
3. Then change the claim configuration.
4. Verify with a real login, not by reading the IdP's UI: have an affected member sign in again and read `GET /me/capabilities`, whose `session_groups` is the snapshot their token actually produced. Every group your re-keyed rows name must appear in it. `POST /governance/preview` with that exact list says which profile now resolves for them.

- If the groups cannot be flattened and the rows cannot be re-keyed, user subjects are the only shape in this release that a claim-configuration change cannot break without telling you ([`threatmodel/THREAT-MODEL.md`](../../threatmodel/THREAT-MODEL.md) §5).

- An overage also blocks one **role derivation** it must not be allowed to decide.
- The role map is keyed on the very claims the IdP withheld, so an overage login matches nothing — and "nothing matched" is then an absence of evidence, not a fact.
- Falling through to `WARDYN_OIDC_DEFAULT_ROLE=admin` would hand that human the top tier on the strength of a claim nobody read, promoting exactly the member the hidden claim was going to wall.
- Such a login is **denied** (`auth_error=claims_overage`), with a server log line naming the claim and the var.
- The check is narrow, so the ordinary posture is untouched:
  - a login whose claims genuinely matched is served as-is (a hidden claim can only ever *narrow* a highest-wins match),
  - and so is a fallthrough to `user`, the narrowest tier there is — a human in 200+ groups still signs in.
- Only a default WIDER than `user` is refused.
- The remedy is the operator's, and retrying will not clear it: carry the tier on Entra App Roles, map the human's email directly, or stop defaulting unmatched humans to `admin`.

- **What a capability deliberately does not reach.**
- `always`-scope decisions stay on the admin-or-`security_admin` gate even for a member granted the host — a grant must never promote a member's decision into durable workspace config.
- `GET /workspaces` is not narrowed: visibility is not capability, the launch gate is what refuses.
- Machine lanes (`/internal/*`, ground-truth ingest, attach tickets) are untouched.
- And where the operator ceiling sets `allow_all_egress` the allowlist is not the gate at all, so `egress_host` narrowing does nothing there — the operator's own posture, not a switch that failed.

- **What a person is offered.**
- The per-person lists the console's pickers read hold only what the caller may use, decided by the same resolver the launch doors refuse with: the `harnesses` (`agent`) of `GET /setup/status`,
  - and the Azure DevOps rows of `GET /me/scm-access` and `/setup/status`'s `scm_access` (`workspace_provider`).
- A refused row is dropped whole, so it reads exactly as a resource the deployment does not have.
- If the grant tables cannot be read, those lists come back empty rather than unfiltered.
- Admins are exempt, as at every door; a `security_admin` is bounded like a member.

- **Managing them** (the seven `/permissions` rows are `securityOps` — admin or `security_admin`; the `/access` rows are `operatorOnly`; `GET /me/capabilities` is member-safe):

| Route | Does |
|---|---|
| `GET /permissions` | the whole grant table plus every enforcement switch, one call |
| `POST /permissions/grants` | upsert one grant on its natural key (`201` new, `200` updated) |
| `DELETE /permissions/grants/{id}` | remove one grant |
| `PUT /permissions/enforcement` | replace the whole switch map — an omitted kind means *off* |
| `GET /permissions/availability/{kind}/{value}` | one resource's "Available to": `restricted`, and `allowed_by`, the allow rows naming it |
| `PUT /permissions/availability/{kind}/{value}` | `{"restricted": true}` turns on "Only…" for one resource, `false` turns it back to Everyone |
| `GET /permissions/explain?subject_type=&subject=&kinds=` | the Explain grid (K4), one named subject's state for every kind; see [`GET /permissions/explain`](#get-permissionsexplain) below |
| `GET /access` | the merged role-mapping table (chart + console rows, with collision/shadow provenance) plus the same before/after/changes posture the write guards below evaluate |
| `POST /access/mappings` | upsert one console role mapping on its natural key (`value`) — `201` new, `200` updated; refused on a chart/operator-allowlist collision, an unmatched-outcome flip without `acknowledge_access_change`, or a write that would remove the caller's own admin access |
| `DELETE /access/mappings/{id}` | remove one console role mapping — same flip/lockout guards as the write above |
| `POST /access/preview` | dry-run `roles`/`groups`/email (or the caller's own session) through the SAME derivation a real login would use — no write |
| `GET /me/capabilities` | member-safe: the caller's OWN grants, the switches, their session groups, `groups_snapshot_stale`, and `kinds_version` (a number that goes up whenever the set of capability kinds changes) |

- `PUT /permissions/enforcement` replaces the **whole** map, so an omitted kind is an enforced kind switched off: re-fetch `GET /permissions` immediately before writing, or a stale admin tab can silently disable a control two admins both believe is on.
- `GET /permissions`'s `ETag` header (a content hash of the enforcement map alone, not the grant table) can be sent back as this `PUT`'s `If-Match`: a document that changed underneath a stale tab is refused `412`.
- `If-Match` is optional, and the write is audited either way.

- **"Available to" (0.8).**
- `workspace`, `image`, `agent` and `workspace_provider` values can each be restricted one at a time (migration `0081_capability_restrictions`).
- A restricted value counts as enforced whatever its kind's switch says, and only a caller holding an allow row that names the value itself gets it: a `*` allow lists nobody, and a deny still wins.
- So the "Only…" list is the allow rows for that value, written through `POST /permissions/grants` for a person, a group or a user type.
- Security admins are bound like anyone; only the admin tier is exempt.
- On `image`, the one widening kind, the restriction also switches that one image on for the people listed while the kind stays off for every other image.
- Turning "Only…" on with no allow row naming the value is refused `400`, since the resource would then be available to nobody.
- `egress_host` and `secret` values can't be restricted (`400`).
- The value is the rest of the path, so an image ref's slashes need no escaping.

- Writes are audited as `capability.grant.create` / `.updated` / `.deleted`, `capability.enforcement.write` and `capability.availability.write`.
- Enforcement lives in its own table rather than in SiteConfig because `PUT /site-config` is a full replace: a stale client round-tripping an older document could otherwise silently disable an authorization control.
- There is **no cache** — resolution is two indexed reads per check, so a grant applies immediately; a stale permission cache is a security bug, not a slow page.

### `workspace_provider`

- which git provider row the repositories a member brings in may come from — the row `admitRepoURL` resolves a derived clone URL to ([`internal/api/workspace_providers.go`](../../internal/api/workspace_providers.go)).
- **Six doors**, every one a member can reach: `POST /runs` over the resolved spec's repos and over the legacy `repo` field, and `POST`/`PUT /workspaces`, `POST /workspaces/{id}/scan` and `.../build` — the last three re-point or perform a SERVER-SIDE clone.
- It bounds the PROVIDER, not the repository: admission is URL-prefix matching, not a repo ACL.
- Inert on a deployment with no provider rows,
  - and on a repository whose host no row CLAIMS
    - (a row that claims the host and refuses anyway — disabled, or a base path that did not match — still keys the check)

### `model_provider`

- which model provider (Settings → Model providers, `SiteConfig.ModelProviders`) a person's run may use —
  - the one the request names (`model_provider`, `wardyn run --model-provider`),
  - the one a workspace pins (`llm_cred.provider_ref`),
  - or the agent's default reaching them (`enforceRunModelProvider`, [`internal/api/run_model_provider.go`](../../internal/api/run_model_provider.go); create and Review alike).
- **A workspace pin is gated too**: every model credential is the person's own, so a pin naming a provider they aren't granted refuses the run rather than being exempt.
- Such a person never sees that pin's id (0.8.2, #1018): a workspace read (`GET /workspaces`, `GET /workspaces/{id}`, the update response) answers `llm_cred: {"provider_unavailable": true}` in its place, and the launch refusal names no provider.
- Inert with no model-provider block

### `feature`

- whether a member may add an SSH key (`POST /me/ssh-keys`), mint an API token (`POST /me/tokens`) or define a custom component of their own at all
- One check at each mint door (the token door keeps its user-view `409`; the SSH door stores a capped key, #564).
- Mint only: a key or token that already exists keeps working until it is removed or revoked.
- `custom_component` gates saving a component (`POST`/`PUT /me/components`) and attaching one the person defined, inline or saved, to a run (`componentAttachRefusal`, [`internal/api/components_authz.go`](../../internal/api/components_authz.go)).
  - It is on for everyone from the release that adds it, until a deny row names it. This is the one value that sets aside "an upgrade with no configuration changes nothing".
  - Where `feature` is already enforced, it is off for anyone no allow row (the value or `*`) covers, so write that row before upgrading.
  - A refusal carries the reason `capability_feature` with the target `runs.component`.
  - See [Custom components](custom-components.md#custom-components).
- Any other value is refused at write time (`400`)

### `GET /permissions/explain`

- the Explain grid (K4): for one named `user`, `group` or `user_type` subject, every kind's state —
  - `everyone`,
  - `this_type` (an allow, including one written for `all`),
  - `blocked` (a deny that covers the value),
  - `admins_only` (the widening `image` kind, off or with no allow),
  - or `not_available` (an enforced narrowing kind with no allow, or a restricted value no allow naming it lists this subject)
- at the `*` default plus every specific value a grant names or "Available to" restricts (`restricted: true`).
- Each cell is the resolver's own answer, switch and restriction included, for a person who is exactly that subject:
  - only rows naming that subject or `all` are read,
  - so a user's group and type rows are not included.
- The subject is folded the way a grant's subject is, and a user type that doesn't exist is refused (`400`); `kinds` defaults to every kind
