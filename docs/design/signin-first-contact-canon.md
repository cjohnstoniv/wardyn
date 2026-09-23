# Sign-in first contact — canonical strings (#457)

Frozen. Implements issue #457, "Sign-in first contact: honest loading state, no jargon" — the
sign-in screen (`ui/src/app/components/screens/sign-in.tsx`) makes no claim about which doors
(admin token, SSO) exist until `/healthz` has actually answered, and no refusal sentence on the
screen names an env var or "an operator" a reader here — sometimes not even signed in — cannot
reach or act on.

A backticked substring inside a frozen string below is plain text in the copy module
(`ui/src/app/lib/sign-in-copy.ts`) — mono styling, where it applies, is a display concern applied
at the call site, not baked into the string.

## Owner decisions

- **Q457-1 — hide both doors while checking.** Before `/healthz` has answered even once, neither
  the admin-token form nor the SSO control renders — not even in a disabled/guessed state. A
  wrong guess for even a moment is worse than an honest "checking".
- **Q457-2 — add the still-checking line after three unanswered reads.** Silence past that point
  reads as a hang, not a wait; `SIGNIN.STILL_CHECKING` replaces `SIGNIN.CHECKING` once three
  consecutive `/healthz` reads have come back with no answer (network error, timeout, or any
  non-2xx — see `health.ts`'s `{}` convention), and reverts the instant a real answer lands.
- **Q457-3 — the token hint names neither the demo token nor an env var.** `SIGNIN.TOKEN_HINT`
  says what belongs in the field and who has it ("your Wardyn admin"), never `WARDYN_ADMIN_TOKEN`
  or the compose demo's `demo-admin-token` (already true since #212; this campaign keeps it that
  way explicitly rather than by omission).

## States

1. **Checking** — no answer yet about which doors exist. Title "Wardyn", `SIGNIN.LEAD`, and only
   a checking row (`SIGNIN.CHECKING`). No token field, no SSO control, no claim either way.
2. **Still checking** — after three unanswered reads of that same question, the row reads
   `SIGNIN.STILL_CHECKING` instead. The screen keeps asking; still no claim.
3. **SSO only** — `/healthz` says `sso:true` and the admin-token form should not render
   (`token_login:false`, whether via `sso_only` or member mode). Just "Sign in with SSO". No
   token form, and no role-source sentence — `SIGNIN.ROLE_SOURCE` (formerly
   `people-access-copy.ts`'s `SIGNIN.ROLE_SOURCE`, "comes from your SSO role assignment…") is
   REMOVED everywhere in this campaign, with its tests, not merely hidden here.
4. **Admin token only** — `sso:false`. Token field (placeholder carries no value — unchanged from
   today, per #212), `SIGNIN.TOKEN_HINT`, the Remember checkbox + hint, Sign in. No disabled SSO
   button and no sentence about SSO — the whole disabled-stub branch (button + its
   `WARDYN_OIDC_*` title + the "isn't configured" paragraph) is deleted; nothing renders for a
   door that does not exist.
5. **Both doors** — token form, an "OR" divider, "Sign in with SSO". The token form's submit is
   the one primary (teal) button on the screen; the SSO link/button is `variant="outline"`.
6. **Refusals name no environment variable.** Every `auth_error=<code>` arm
   (`authErrorMessage` in `sign-in.tsx`) reads `SIGNIN.*` below — none of them names a
   `WARDYN_OIDC_*`/`WARDYN_*` value.
7. **Refusals point at "your Wardyn admin", never "an operator".** A reader on this screen has no
   path to a chart value or an operator roster; the person who can act is named consistently.
8. **The shared default error** (`ui/src/app/components/wardyn/states.tsx`'s `ErrorState`, every
   pane that renders it with no `message` of its own) reads `STATES.ERROR_DEFAULT` — no
   "control plane", no "Please".

## Frozen strings

`ui/src/app/lib/sign-in-copy.ts`'s `SIGNIN` table, transcribed verbatim. "Where" names the
screen/element; "Status" is New (introduced by #457), Changed (existing string reworded), or
Reused (unchanged, listed for completeness because #457 touches its neighbors).

| Key | Status | Where | String |
|---|---|---|---|
| `SIGNIN.LEAD` | Reused | card subhead | This console governs the agents on this host. |
| `SIGNIN.CHECKING` | New | checking row (state 1) | Checking sign-in options… |
| `SIGNIN.STILL_CHECKING` | New | checking row (state 2) | Still checking — Wardyn hasn't answered yet. |
| `SIGNIN.TOKEN_HINT` | Changed | under the token field | Paste the admin token this Wardyn was started with — your Wardyn admin has it. |
| `SIGNIN.TOKEN_REJECTED` | Reused | alert, on a real 401 | That admin token was rejected. Check the value and try again. |
| `SIGNIN.UNREACHABLE_ERROR` | Changed | alert, on a network error | Wardyn isn't answering. Try again. |
| `SIGNIN.EMAIL_DOMAIN` | Changed | `auth_error=email_domain` | This email's domain isn't allowed to sign in here. Ask your Wardyn admin to allow it. |
| `SIGNIN.NO_ROLE` | Changed | `auth_error=no_role` | Your account has no Wardyn role assigned. Ask your Wardyn admin to add you to a role mapping. |
| `SIGNIN.CLAIMS_OVERAGE` | Changed | `auth_error=claims_overage` | Your identity provider sent too many groups to list in a sign-in token, so Wardyn can't tell what access you should have — and won't guess. Ask your Wardyn admin to map your role by App Role or email instead. Trying again won't help. |
| `SIGNIN.EMAIL_VERIFIED_ABSENT` | Changed | `auth_error=email_verified_absent` | Your identity provider doesn't send an email_verified claim at all (common on Entra ID), so Wardyn can't confirm your email on its own. Ask your Wardyn admin to map your role by App Role or group instead. |
| `SIGNIN.ROLE_CHECK_UNAVAILABLE` | Changed | `auth_error=role_check_unavailable` | Couldn't check your access — try again, or ask your Wardyn admin. |
| `SIGNIN.OIDC_CONFIG` | Changed | `auth_error=oidc_config` | Sign-in with your identity provider failed. Try again; if it keeps happening, ask your Wardyn admin to check the sign-in configuration. |
| `SIGNIN.OIDC_TRANSIENT` | Reused | `auth_error=oidc_transient` | Your identity provider didn't respond in time. This is usually temporary — try signing in again. |
| `SIGNIN.AUTH_FAILED` | Changed | `auth_error=<unrecognized>` (default arm) | Sign-in failed. Try again, or ask your Wardyn admin. |
| — | Reused (inline JSX, not a copy-module key) | admin-token label | Admin token |
| — | Reused (inline JSX) | Remember checkbox label | Remember on this device |
| — | Reused (inline JSX) | Remember checkbox hint | (keeps the token after the browser closes) |
| — | Reused (inline JSX) | token form submit | Sign in |
| — | Reused (inline JSX) | SSO control | Sign in with SSO |
| `STATES.ERROR_TITLE` | Reused | `ErrorState` heading, every pane | Something went wrong |
| `STATES.ERROR_DEFAULT` | Changed | `ErrorState` body, no `message` passed | Wardyn isn't answering. Try again. |
| `STATES.RETRY` | Reused | `ErrorState` retry button | Retry |

**Not carried forward:** `SIGNIN.ROLE_SOURCE` ("Your role — admin, security admin or member —
comes from your SSO role assignment. Everyone is an admin only when neither a role map nor the
operator allowlist is set."), formerly `people-access-copy.ts`'s `SIGNIN` export under §7.7 of
docs/design/people-access-prompt.md. Removed everywhere per state 3 above, not conditionally on
`sso_only` as before — deleted from every cell, `sign-in.tsx`, and its tests
(`sign-in.test.tsx`, `e2e/auth.spec.ts`, `e2e/live/sso-roles.spec.ts`).

**`email_unverified`** (`auth_error=email_unverified`) is unchanged and out of scope this round —
it names no env var and no "operator" today, so it needed no rewording: "Your identity provider
reports this email as unverified. Verify your email with your identity provider, then try again."

## Where the copy lives

Everything the sign-in screen renders now lives in `ui/src/app/lib/sign-in-copy.ts`'s `SIGNIN`
export. `NO_ROLE`/`EMAIL_VERIFIED_ABSENT`/`ROLE_CHECK_UNAVAILABLE`/`ROLE_SOURCE` used to be split
out into `people-access-copy.ts` (0.7 SSO Phase 3, §7.7) because that campaign's People-step
editor shared their wording; #457 rewrote all of it for honesty/no-jargon, so one screen's copy
now has one home — not `wardyn/copy.ts`, and not split across two modules with the same `SIGNIN`
name. `STATES` (the shared `ErrorState` default) lives in `ui/src/app/components/wardyn/states.tsx`
itself, colocated with the component it styles.
