# Session continuity — frozen strings (#1995, mock M12)

Owner-approved mock M12, option A. The console module is `ui/src/app/lib/session-continuity-copy.ts`;
`session-continuity-copy.test.ts` parses the table below and compares every row with it byte for
byte. Reused, not restated: the gate's fallback sentence (`SESSION_ENDED_REASON`,
[reauth-in-place-canon.md](reauth-in-place-canon.md)), the "Sign in to continue" dialog, and the
unsaved-changes dialog ([unsaved-guard-canon.md](unsaved-guard-canon.md)), which the six editors that
had no guard now share with no new string.

## Frozen strings

`{run}` and `{time}` stand for the value the console fills in. Rows marked "not drawn" are a
console sentence the board has no frame for; they follow the drawn one beside them.

| Key | Where | String |
|---|---|---|
| `SESSION_ENDED.EXPIRED` | Sign-in notice, 401 reason `expired_session` | Your session reached its time limit, so you were signed out. Sign in again to carry on. |
| `SESSION_ENDED.REVOKED` | Sign-in notice, `revoked_session` (names no cause: SCIM group changes and suspensions also cut sessions) | Your session was ended on the server, so you were signed out. Sign in again to carry on. |
| `SESSION_ENDED.ROLE_STALE` | Sign-in notice, `role_stamp_stale` | Your sign-in is older than this deployment allows for your role. Sign in again to pick up your current access. |
| `SESSION_ENDED.INVALID` | Sign-in notice, `invalid_session` | Wardyn couldn't verify your session, so you were signed out. Sign in again. |
| `SESSION_ENDED.DEACTIVATED` | Sign-in notice, `identity_deactivated`; the Sign in button is quieter | Your access to Wardyn has been turned off. Ask your Wardyn admin. |
| `SESSION_ENDED.ELSEWHERE` | Sign-in notice after another tab signed out | You signed out in another tab. |
| `SESSION_ENDED.OWN` | Sign-in notice after this tab's own Sign out | You signed out of Wardyn. |
| `SESSION_ENDED.OWN_SHARED` | Second line of that notice, single sign-on deployments only | On a shared computer, also sign out of your work account in this browser. Wardyn's sign-out does not end that sign-in. |
| `SESSION_ENDED.RETURN(run)` | Under the SSO button, on a run's page | After you sign in, you go back to Run {run}. |
| `SESSION_MENU.SIGNED_IN_UNTIL_TODAY(time)` | Account menu | Signed in until {time} today |
| `SESSION_MENU.SIGNED_IN_UNTIL_DAY(time, day)` | Account menu, expiry on a later day (not drawn) | Signed in until {time} on {day} |
| `SESSION_MENU.SIGN_OUT_HINT` | Account menu, under Sign out | Ends your Wardyn session in this browser. |
| `TERMINAL_SESSION.RESUMED(time)` | Written into the run terminal when it reconnects after a sign-out | ── Signed in again at {time}. Reconnected; the output above is kept. ── |
| `TERMINAL_SESSION.SIGNED_OUT_NOTE` | Strip under the terminal while the console is signed out | The terminal keeps its scrollback while the dialog is open and reconnects once you are signed in. Keystrokes typed while signed out are not sent. |

## Decisions

- One notice per reason the server already sends (`internal/api/auth_status.go`); a reason this
  build does not know, and a stored token refused with no reason, keep the fallback sentence.
- The server's own 401 messages are API-caller text and are not reused.
- The shared-computer line replaces the old "also sign out of your identity provider" toast: one fact, one place.
- "Signed in until" shows whenever `/me` carries `session_expires_at`; local mode shows neither new menu line.
- The return line names a run's page only; other pages are not drawn, so none is named.
- `{time}` follows the console's existing clock format (`lib/format.ts` `shortTime`).
