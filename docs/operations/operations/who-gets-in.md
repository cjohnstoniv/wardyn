# Who gets in, and with which role

Who may sign in, which role a session derives, and which surfaces decide that.
This is the access half of what the multi-user section used to carry: the merged role map, the
chart/console/IdP split, and the derivation rules behind a signed cookie.

## Multi-user: who can change what

- The API authenticates with **either** an OIDC session (human SSO) **or** the admin bearer token; local mode skips both on a loopback-only bind.
- That is authentication.
- Authorization is a real three-role model: every OIDC session carries an **admin**, **`security_admin`** or **member** role, derived once at login (`internal/auth/oidc`'s `deriveRole`) and stamped into the signed session cookie.
- A cookie signed before this existed (pre-0.5) decodes as no session, forcing a re-login that derives one fresh.

| The merged map (chart `WARDYN_OIDC_ROLE_MAP` + console People-step rows) | Signed-in humans | Admin token / local mode |
|---|---|---|
| empty | listed in `WARDYN_OIDC_OPERATOR_EMAILS` → **admin**, others → **user**; all **admin** only when the allowlist is also unset (override-only under OIDC — the pre-0.5 behavior) | always **admin** |
| non-empty | mapped by `roles`/`groups`/email claim to **admin**, **`security_admin`** or **user**; no match falls through to `WARDYN_OIDC_DEFAULT_ROLE` (which takes `admin`/`user` only), or denies the login when that is also unset | always **admin** |

- A console-added row keys on this exact same table:
  - adding the deployment's *first* row (with the chart map also unset) or removing its *last* one moves the map from empty to non-empty or back, exactly like setting or clearing `WARDYN_OIDC_ROLE_MAP` itself,
  - which is what `POST`/`DELETE /access/mappings`' posture-flip guard warns an admin about before they trip it (see "Managing them" below).
- The admin token and local mode are **always admin** — a single shared credential with no per-human identity to key a role off (the token *is* the admin).
- That is the documented ceiling of the whole gate (`requireOperator`/`isOperator`, [`internal/api/http.go`](../../internal/api/http.go)), not an oversight.

## Who decides who gets in: chart vs console vs IdP

Three surfaces share this decision, and only one of them is live without a restart.

| | IdP (Entra) | Chart / env (boot-time bootstrap) | Console (Getting Started → People, live) |
|---|---|---|---|
| **What lives here** | People and groups exist here; Entra App Roles and their assignment; the app registration's "Assignment required" switch | `WARDYN_OIDC_ISSUER`/client config; `WARDYN_OIDC_ROLE_MAP` (the bootstrap layer — always wins a duplicate key against a console row); `WARDYN_OIDC_OPERATOR_EMAILS` (top-precedence admin allowlist, also the boot posture floor); `WARDYN_OIDC_DEFAULT_ROLE`; `WARDYN_OIDC_ALLOW_EMAIL_MAPPINGS` | `/access` role mappings (`GET /access`, `POST /access/mappings`, `DELETE /access/mappings/{id}`), a **disjoint union** with the chart map — the console never edits `WARDYN_OIDC_ROLE_MAP` itself, only its own rows |
| **Wins a collision** | "Assignment required" stops an unassigned user **before Wardyn's callback ever sees a `roles` claim** — a gate Wardyn cannot see through or override | The chart entry, always; see [below](#wins-a-collision-chart--env) | Nothing — a console row only ever fills a gap the chart and the allowlist leave open |
| **Takes effect** | Immediately for Entra's own gate | At boot (a `wardynd` restart/upgrade) | At the affected human's **next sign-in** — canonicalized (trimmed, lowercased) on write; never retroactive, so a person already signed in keeps the role stamped into their current session cookie |
| **Recovery path** | n/a | The admin bearer token — one shared credential with no per-human identity to demote, so it is the ONE caller the console's lockout guard (below) never binds | n/a — the console surface is the thing that *can* lock an admin out, not a way back from it |

A few things that don't fit the grid:

- **Boot posture is chart-only.**
  - `validateOperatorPosture` ([`cmd/wardynd/boot_posture.go`](../../cmd/wardynd/boot_posture.go)) refuses to boot OIDC at all unless `WARDYN_OIDC_OPERATOR_EMAILS` is set or `WARDYN_OIDC_ROLE_MAP` is non-empty (override: `WARDYN_ALLOW_OIDC_NO_OPERATOR_LIST`)
  - **console rows do not count toward this floor**: they live in the database, read once per login, never at boot, so the chart alone has to justify running OIDC on this install.
- **The two console writes that can flip everyone's default outcome** — adding the first console row while the chart map is empty, or deleting the last one — are refused (400) without `acknowledge_access_change=true`.
  - That holds only when the write would actually change what an unmatched, non-allowlisted human gets
  - (computed from `HasOperatorEmails()`/`DefaultRole()` on each side of the write, never a raw row count — a shadowed row contributes to neither side).
- **The lockout guard** refuses a write that would leave the ACTING admin no longer admin, checked against their own last-sign-in session snapshot
  - never a live re-check, since a role is a stamped cookie, not a query.
  - A snapshot too stale to reproduce the admin access they demonstrably hold right now (a truncated or pre-0.6 cookie)
    - gets a distinct refusal telling them to sign in again,
    - rather than a false lockout claim on data that can't answer either way.
- **The preview panel** (`POST /access/preview`) runs the identical derivation a real login would, against pasted claims or the caller's own session,
  - so an admin can see "who would this row make an admin" without waiting for that person to sign in
  - nothing it does is saved.
- **Some subjects never sign in.**
  - The callback refuses an identity-provider `sub` that names an identity that is not a person — `admin-token`, the configured `WARDYN_LOCAL_OPERATOR`, or any `local:`/`device:`/`delegate:`/`subject:` name, trimmed and case-folded
    - with the generic sign-in error and an `auth.fail` row (`reserved_principal`);
    - a session, `wdn_` token or SSH key already carrying one is refused on use.
  - Switching a local-mode install to SSO:
    - the default seat (`local:<os-user>`) stays reserved by its prefix, but a custom `WARDYN_LOCAL_OPERATOR` seat stays reserved only while the variable remains set.
    - Unset it, and a person whose `sub` is that name would own the runs local mode created under it.
  - Keep it set.
  - On an Entra ID issuer a `sub` starting with `entra:`, in any case, is refused the same way:
    - that namespace belongs to people set up by object id (see "[Tokens for a person who never signs in](api-tokens.md#tokens-for-a-person-who-never-signs-in)").
- **The same claim values do double duty.**
  - The `roles`/`groups` values a role mapping matches are the exact same login-time snapshot a `/permissions` capability grant's `subject_type=group` matches against (see "Subjects, and the group snapshot's ceiling" below).
  - Two levels reading one snapshot, not two systems that happen to agree.
- **Fail-closed on a wired store error.**
  - If the console's role-mapping store can't be read, a login in progress is **denied** (`auth_error= role_check_unavailable`) rather than silently falling back to the chart-only map.
  - The same code the preview panel surfaces when it can't check a row.

**Deriving the role** (`WARDYN_OIDC_ROLE_MAP`, a CSV of `value=role` pairs, e.g. `Wardyn.Admin=admin,eng-team=user,alice@corp.com=admin`; full semantics in [ENV.md](../ENV.md)):

- Each `value` is matched case-insensitively against the ID token's `roles` claim (an Entra App Role — the priority path; app-registration walkthrough in `.claude/skills/wardyn-k8s-setup`), its `groups` claim, or the signed-in email.
- Matches fold **highest wins** over three ranks — `user` < `security_admin` < `admin` — whichever claim produced them (`roleRank`, [`internal/auth/oidc/derive.go`](../../internal/auth/oidc/derive.go)):
  - a human matching a `security_admin` row and a `user` row is a security admin;
  - one matching an `admin` row anywhere is an admin, exactly as before 0.7.
- `WARDYN_OIDC_OPERATOR_EMAILS` is **not replaced**: an email on it is still an *additional* `admin` match (`LegacyAdminEmails`), so a deployment adopting the role map keeps its current operators with zero re-configuration.
- `WARDYN_OIDC_DEFAULT_ROLE` (`admin`/`user`, unset = deny) covers everyone the map doesn't name
  - `security_admin` is **refused** there and fails boot (`validDefaultRole`, [`cmd/wardynd/boot_deps.go`](../../cmd/wardynd/boot_deps.go)):
  - the role map is the only way to reach that tier, so it is never the tier granted by fallthrough to everyone nobody named.

**A `groups`-keyed row needs the `groups` scope requested, on an IdP that gates that claim behind one.**

- The authorization request is fixed at `openid profile email`;
  - it does not ask for `groups` by default, so an IdP that only sends that claim once a client explicitly requests the scope simply omits it
  - indistinguishable from "this human is in no groups", so a `WARDYN_OIDC_ROLE_MAP` row keyed on a group name decides nothing there.
- `WARDYN_OIDC_EXTRA_SCOPES` (see [ENV.md](../ENV.md)) opts a deployment into requesting `groups` (or any other scope), validated at boot against the provider's own discovery document.

Both are validated at **boot**, not at first use:

- a malformed entry (invalid role value, non-ASCII key — matching is ASCII-only, so it could never match — duplicate key, or non-blank input with no valid entry at all) or an invalid `WARDYN_OIDC_DEFAULT_ROLE`
- either fails wardynd's boot outright, naming the var (`buildOptionalFeatures`, [`cmd/wardynd/boot_deps.go`](../../cmd/wardynd/boot_deps.go))
- never a silent fallback that lets a typo reach a session cookie later.

A signed-in human who matches nothing in a valid map, with no default role set, is denied at login instead ("no Wardyn role assigned").

### Wins a collision: chart / env

- The chart entry, always.
- A console write that would collide with a chart key or an operator-allowlist email is refused outright (400).
- A *later* helm upgrade that introduces one anyway leaves the existing console row inert with a "Shadowed" badge instead of silently dropping it.
