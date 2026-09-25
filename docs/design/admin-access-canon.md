# Admin access — frozen strings (#484)

The approved copy from mock packet 3 ("Admin Access Warnings"), byte for byte. Code carries these
strings; this table is where a reviewer checks them. It supersedes the `sso_rbac` wording in
[people-access-prompt.md §7.8](people-access-prompt.md).

## Owner decisions

| Id | Decision |
|---|---|
| Q457-4 | The everyone-is-an-admin warning shows in two places with the same words: a shell banner above every page (admins only) and the existing setup row. Members see nothing. |
| Q457-5 | Warn only when **neither** a role map (chart or People step) **nor** an admin list (the operator allowlist) is set. An admin list alone is ok. |
| Q457-6 | The admin's help shows on exactly four refusals — the ones a person cannot clear alone: `no_role`, `email_domain`, `claims_overage`, `email_verified_absent`. Timeouts, configuration errors and the generic arm get nothing. Wardyn's own sentence always comes first. |
| Q457-7 | Two fields, text and URL. The link's label is fixed: "Request access". |
| Q457-8 | The text limit is **1,000 characters**. This is an owner change from the mock's 280. Counted as characters, not bytes. |
| #489 (2026-09-25) | The sign-in help link is **https:// only**. A new http:// link is refused at save with the scheme error. A link already stored as http:// surfaces as a setup warning. |
| #584 (2026-09-25) | The SSH keys screen shows a chip on a capped key (one added in the user view). |
| Q491-1 | Warn whenever the default role is admin, whether or not a role map is set — not only when neither a role map nor an admin list is set (#484's original condition). Same banner and setup row; the copy shown depends on which condition tripped it. |

## Frozen strings

Reordered 2026-09-25 (owner ruling, #726) so the frozen `String` is the LAST
column — the shape `parseFrozenTables()` (the T-66 shared parser) expects.
Purely structural: every `Id` and `String` cell is byte-identical to before,
only `String` and `Where` traded places.

| Id | Where | String |
|---|---|---|
| `sso_rbac.label` | `/setup/status` row label (`ssoRBACCheck`) | Who is an admin |
| `sso_rbac.warn` | setup row `detail`, warn | Nobody is mapped to a role and no admin list is set, so everyone who signs in is an admin. |
| `sso_rbac.fix` | setup row `fix`, warn | Map people to admin or user on the People step, so only the people you name can change this deployment. |
| `sso_rbac.ok` | setup row `detail`, ok | People are mapped to admin or user, so a person's role comes from their sign-in. |
| `ADMIN_ACCESS_BANNER.TITLE` | shell banner (`access-posture-copy.ts`) | Everyone who signs in is an admin |
| `ADMIN_ACCESS_BANNER.BODY` | shell banner | Nobody is mapped to a role and no admin list is set, so every person your identity provider lets in can change policies, read and write secrets, decide approvals, and open a shell in any running sandbox. |
| `ADMIN_ACCESS_BANNER.ACTION` | shell banner CTA → `/admin/setup?step=people` | Set who is an admin |
| `ADMIN_ACCESS_BANNER.BODY_DEFAULT_ROLE` | shell banner, shown in place of `.BODY` when the cause is `WARDYN_OIDC_DEFAULT_ROLE=admin` rather than "no mapping at all" (#491) | The default role is admin, so anyone your identity provider lets in whom the role map doesn't match can still change policies, read and write secrets, decide approvals, and open a shell in any running sandbox. |
| `sso_rbac.warn_default_role` | `/setup/status` row `detail`, warn, same substitution as the banner body (#491) | A role map is set, but the default role is admin, so a sign-in the map doesn't match is still an admin. |
| `sso_rbac.fix_default_role` | `/setup/status` row `fix`, warn (#491) | Set WARDYN_OIDC_DEFAULT_ROLE to user or a user type (chart: env.WARDYN_OIDC_DEFAULT_ROLE), so a sign-in the role map doesn't match becomes a user, not an admin. |
| `SIGNIN_HELP.TITLE` | People-step card (`access-posture-copy.ts`) | When someone can't sign in |
| `SIGNIN_HELP.LEAD` | People-step card | Wardyn says what happened. You say what to do about it. |
| `SIGNIN_HELP.TEXT_LABEL` | card, text field | What to tell them |
| `SIGNIN_HELP.TEXT_PLACEHOLDER` | card, text field | Ask in #it-helpdesk to be added to Wardyn. |
| `SIGNIN_HELP.TEXT_HINT` | card, text field | Up to 1,000 characters. Anyone who can reach the sign-in page can read it, signed in or not — so name your request process, not your internal systems. |
| `SIGNIN_HELP.COUNTER` | card, shown once typing | {n} / 1000 |
| `SIGNIN_HELP.URL_LABEL` | card, URL field | Link |
| `SIGNIN_HELP.URL_PLACEHOLDER` | card, URL field | https:// |
| `SIGNIN_HELP.URL_HINT` | card, URL field (#489 dropped "http:// or ") | Optional. Must start with https://. It shows as "Request access" — the address itself is public. |
| `SIGNIN_HELP.EMPTY_NOTE` | card, neither field set | Nothing set. People see Wardyn's own sentence and are told to ask their Wardyn admin. |
| `SIGNIN_HELP.PREVIEW_HEADING` | card preview (the no-role sentence + text + link) | What a signed-out person sees |
| `SIGNIN_HELP.APPLIES_NOTE` | card preview | Shown on the four refusals a person can't clear themselves: no role, an email domain that isn't allowed, too many groups to list, and a missing email claim. |
| `SIGNIN_HELP_LINK_LABEL` | sign-in page and card preview (`people-access-copy.ts`; the card re-exports it as `SIGNIN_HELP.LINK_LABEL`) | Request access |
| refusal: too long | `PUT /site-config` 400 (`validateSignInHelp`) | sign_in_help_text: longer than 1,000 characters — it renders under a refusal on the sign-in page |
| refusal: bad URL | `PUT /site-config` 400, scheme failures only, a new http:// link included (#489 dropped "http:// or ") | sign_in_help_url: must be an https:// address — it is shown to people who have not signed in |
| `sign_in_help_url.label` | `/setup/status` row label (`signInHelpHTTPCheck`), the card's own title | When someone can't sign in |
| `sign_in_help_url.warn` | setup row `detail`, warn, never blocking (#489) | The sign-in help link uses http://. Change it to an https:// address so people who can't sign in aren't sent to an unencrypted page. |
| SSH keys chip | SSH keys screen, a key whose `capped` is true (#584) | Member access |
| SSH keys chip tooltip | the chip's `title` (#584) | Added while you were a member, so it keeps member rights. Add a new key to use admin access over SSH. |

## Implementation strings (not in the mock)

These strings cover the card's save mechanics. The mock did not draw them.

| Id | String |
|---|---|
| refusal: control character | sign_in_help_text: contains a line break, control character or invisible formatting character — it renders as one plain paragraph on the sign-in page |
| refusal: malformed URL | sign_in_help_url: must be a plain web address with a real host name — no spaces, sign-in details or hidden characters — it is shown to people who have not signed in |
| `SIGNIN_HELP.SAVE` | Save |
| `SIGNIN_HELP.SAVED_TOAST` | Sign-in help saved. |
| `SIGNIN_HELP.SAVE_ERROR` | Couldn't save the sign-in help. |
| `SIGNIN_HELP.LOAD_FAILED` | Couldn't load the sign-in help. Retry to edit it. |
| `SIGNIN_HELP.RETRY` | Retry |
| `SIGNIN_HELP.SAVED_ELSEWHERE` | Someone else saved this deployment's settings after you opened this card. Their version is loaded now and your text is unchanged — save again to apply it. |
