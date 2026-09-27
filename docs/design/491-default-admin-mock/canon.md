# Default-role-admin warning — proposed strings (#491)

Rows to add to `docs/design/admin-access-canon.md`'s frozen-strings table, once approved. No
existing #484 row changes: `ADMIN_ACCESS_BANNER.TITLE` and `.ACTION`, and `sso_rbac.ok`, stay
exactly as frozen. Only `ssoRBACCheck`'s warn condition widens (to fire when
`WARDYN_OIDC_DEFAULT_ROLE=admin`, whether or not a role map is set) and gets a second body/detail/
fix triplet for that cause. Byte for byte, same as the rest of the canon doc.

## Owner decision (if approved)

| Id | Decision |
|---|---|
| Q491-1 | Warn whenever the default role is admin, whether or not a role map is set — not only when neither a role map nor an admin list is set (#484's original condition). Same banner and setup row; the copy shown depends on which condition tripped it. |

## New strings

| Id | String | Where |
|---|---|---|
| `ADMIN_ACCESS_BANNER.BODY_DEFAULT_ROLE` | The default role is admin, so anyone your identity provider lets in whom the role map doesn't match can still change policies, read and write secrets, decide approvals, and open a shell in any running sandbox. | shell banner, shown in place of `.BODY` when the cause is `WARDYN_OIDC_DEFAULT_ROLE=admin` rather than "no mapping at all" |
| `sso_rbac.warn_default_role` | A role map is set, but the default role is admin, so a sign-in the map doesn't match is still an admin. | `/setup/status` row `detail`, warn, same substitution as the banner body |
| `sso_rbac.fix_default_role` | Set WARDYN_OIDC_DEFAULT_ROLE to user or a user type (chart: env.WARDYN_OIDC_DEFAULT_ROLE), so a sign-in the role map doesn't match becomes a user, not an admin. | `/setup/status` row `fix`, warn |

## Why this wording

- **`ADMIN_ACCESS_BANNER.BODY_DEFAULT_ROLE`** — mirrors `.BODY`'s sentence shape and the same four
  stakes (policies, secrets, approvals, shell), so an admin reading either state sees the same kind
  of sentence with only the cause clause swapped ("Nobody is mapped..." vs "The default role is
  admin..."). Names the mechanism ("the role map doesn't match") because that's the whole gap: a
  role map exists, and that alone used to be enough to hide the banner.
- **`sso_rbac.warn_default_role`** — kept to one short clause, matching the terseness of
  `sso_rbac.warn` and `sso_rbac.ok` rather than the banner's longer register (the existing pair
  already splits registers this way: banner spells out the stakes, the setup row states the fact).
  "The map doesn't match" rather than "isn't mapped" — a role map is present here, so the earlier
  phrasing would misdescribe the deployment's actual configuration.
- **`sso_rbac.fix_default_role`** — does not reuse `sso_rbac.fix` ("Map people to admin or user on
  the People step...") because that instruction alone doesn't fix this deployment: people are
  already mapped, and the People step has no control for `WARDYN_OIDC_DEFAULT_ROLE` (it's chart/env
  only). Names the variable and its Helm path, consistent with how `tls_cookie_posture.fix` already
  names `WARDYN_TLS_TERMINATED` — the setup row, not the banner, is where a chart variable is named
  (decision 2 in `index.html`).
