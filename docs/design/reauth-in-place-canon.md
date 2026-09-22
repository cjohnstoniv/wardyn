# Signed out mid-page — frozen strings (#483)

Mock packet 1, "Signed Out Recovery", approved by the owner. The console module is
`ui/src/app/lib/reauth-copy.ts`; `reauth-copy.test.ts` parses the table below and compares
every row with it byte for byte.

## Frozen strings

| Id | String | Where |
|---|---|---|
| REAUTH_DIALOG.TITLE | Sign in to continue | Dialog title |
| REAUTH_DIALOG.BODY | Your session ended. Sign in again and this page carries on — nothing here has been lost. | Dialog body |
| REAUTH_DIALOG.WAITING | Waiting for you to finish signing in… | Dialog, while the SSO window is open |
| REAUTH_DIALOG.POPUP_BLOCKED | Your browser blocked the sign-in window. | Dialog, SSO popup refused |
| REAUTH_DIALOG.POPUP_FALLBACK | Open it in a new tab | Dialog, link after POPUP_BLOCKED |
| REAUTH_DIALOG.CLOSED_WITHOUT | That window closed before you signed in. | Dialog, SSO window closed with no session |
| REAUTH_DIALOG.UNREACHABLE | Wardyn isn't answering. This page is still here — try again in a moment. | Dialog, daemon unreachable |
| REAUTH_DIALOG.WRITE_DROPPED | Your last save didn't go through. Everything you typed is still here — save again. | Beside the Save of the screen whose save was refused; a toast elsewhere |
| REAUTH_DIALOG.ROLE_CHANGED_BODY | You're signed in, but this page is no longer yours to open. Copy anything you need — Wardyn will take you to Runs. | Dialog body, same person with a narrower role |
| REAUTH_BAR.BODY | You're signed out. This page is read-only until you sign in again. | Bar across the top after "Not now" |
| REAUTH_BAR.CTA | Sign in | Bar button (reopens the dialog); the dialog's token submit |

Reused, not restated: "Copy my changes" and its toast (`PROVIDERS_DRAFT.CONFLICT_COPY`,
`CONFLICT_COPIED_TOAST`), "Not now" (`MODEL_ACCESS_BANNER.NOT_NOW`), "Sign in with SSO"
(`sign-in.tsx` `SSO_SIGN_IN`). The full sign-in screen's notice is `SESSION_ENDED_REASON` in
`lib/api/core.ts`: "You were signed out. Sign in again to continue."

The mock's OTHER_PERSON_* strings are retired by Q457-12.

## Owner decisions

- **Q457-9 — Runs.** A person whose role no longer reaches the page goes to Runs.
- **Q457-10 — warning.** The full sign-in screen's session-ended notice is an amber warning, not an error.
- **Q457-11 — never re-sent.** A request refused by the expiry is never re-sent by the console; the form keeps its values and says the save didn't go through.
- **Q457-12 — reload fresh.** A different person signing in reloads the page fresh as them: no carry-on, no copy offer.
- **Q457-13 — read-only bar.** "Not now" leaves the page read-only under a bar that offers Sign in.
