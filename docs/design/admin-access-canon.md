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

## Frozen strings

| Id | String | Where |
|---|---|---|
| `sso_rbac.label` | Who is an admin | `/setup/status` row label (`ssoRBACCheck`) |
| `sso_rbac.warn` | Nobody is mapped to a role and no admin list is set, so everyone who signs in is an admin. | setup row `detail`, warn |
| `sso_rbac.fix` | Map people to admin or user on the People step, so only the people you name can change this deployment. | setup row `fix`, warn |
| `sso_rbac.ok` | People are mapped to admin or user, so a person's role comes from their sign-in. | setup row `detail`, ok |
| `ADMIN_ACCESS_BANNER.TITLE` | Everyone who signs in is an admin | shell banner (`access-posture-copy.ts`) |
| `ADMIN_ACCESS_BANNER.BODY` | Nobody is mapped to a role and no admin list is set, so every person your identity provider lets in can change policies, read and write secrets, decide approvals, and open a shell in any running sandbox. | shell banner |
| `ADMIN_ACCESS_BANNER.ACTION` | Set who is an admin | shell banner CTA → `/setup?step=people` |
| `SIGNIN_HELP.TITLE` | When someone can't sign in | People-step card (`access-posture-copy.ts`) |
| `SIGNIN_HELP.LEAD` | Wardyn says what happened. You say what to do about it. | People-step card |
| `SIGNIN_HELP.TEXT_LABEL` | What to tell them | card, text field |
| `SIGNIN_HELP.TEXT_PLACEHOLDER` | Ask in #it-helpdesk to be added to Wardyn. | card, text field |
| `SIGNIN_HELP.TEXT_HINT` | Up to 1,000 characters. Anyone who can reach the sign-in page can read it, signed in or not — so name your request process, not your internal systems. | card, text field |
| `SIGNIN_HELP.COUNTER` | {n} / 1000 | card, shown once typing |
| `SIGNIN_HELP.URL_LABEL` | Link | card, URL field |
| `SIGNIN_HELP.URL_PLACEHOLDER` | https:// | card, URL field |
| `SIGNIN_HELP.URL_HINT` | Optional. Must start with http:// or https://. It shows as "Request access" — the address itself is public. | card, URL field |
| `SIGNIN_HELP.EMPTY_NOTE` | Nothing set. People see Wardyn's own sentence and are told to ask their Wardyn admin. | card, neither field set |
| `SIGNIN_HELP.PREVIEW_HEADING` | What a signed-out person sees | card preview (the no-role sentence + text + link) |
| `SIGNIN_HELP.APPLIES_NOTE` | Shown on the four refusals a person can't clear themselves: no role, an email domain that isn't allowed, too many groups to list, and a missing email claim. | card preview |
| `SIGNIN_HELP_LINK_LABEL` | Request access | sign-in page and card preview (`people-access-copy.ts`; the card re-exports it as `SIGNIN_HELP.LINK_LABEL`) |
| refusal: too long | sign_in_help_text: longer than 1,000 characters — it renders under a refusal on the sign-in page | `PUT /site-config` 400 (`validateSignInHelp`) |
| refusal: bad URL | sign_in_help_url: must be an http:// or https:// address — it is shown to people who have not signed in | `PUT /site-config` 400, scheme failures only |

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
