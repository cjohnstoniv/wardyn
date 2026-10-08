# M-F — 0.8.9 independent console fixes

Status: **packet ready for review; design prototype and owner approval pending**. This packet independently covers #1901, #1906 and #1908. It does not depend on M-R. Implementation-plan authorization is not visual approval. No product rendering was changed to prepare this packet.

Baseline: `7b08fd722ca4dcfd9d2d59e6f1cb8ecab54d8dcf`. Design authority: `docs/design/CONSOLE-RULES.md`, `docs/design/SYNC.md`, `.design-sync/NOTES.md`, and the approved 0.8.8 mock decisions C2/M2/C3. The prototype must be built on the console design-system project (see `docs/design/SYNC.md`); a local project ID is not verification.

Top three corrections: refuse saved-policy workspace loss; use the information token for the Recording link; reconcile sign-in reads without abandoning a still-visible renewal. Reuse `OptionCard`, `Field`, `Button`, `CopyButton`, `DeviceCode`, `Loader2`, the existing waiting strip, the existing renewal strip and their existing status regions. No new color, type size, radius, elevation or copy vocabulary beyond the one declared #1901 sentence.

## 1. What it unblocks

| Fix | Product scope |
|---|---|
| #1901 | Default and saved policy bodies carry one workspace by reference. With two or more attached, refuse Launch, Check again and all shared builder paths; preserve all attachments and show the remedy. Custom policy retains its multi-workspace behavior. |
| #1906 | In the terminal notice, the existing “Open the Recording tab →” control uses `text-info`. The action and words are unchanged. |
| #1908 | Older `/me` reads cannot overwrite the result of a newer sign-in event; a visible renewal keeps checking beyond the old 21-minute quiet-watch bound; waiting device-code reads refresh every 5 seconds and on focus/visibility return without focus theft or repeated announcements. |

## 2. Surfaces

F1 — saved policy with two attached workspaces, at the current New Run layout. M-R later carries this same state into its panels.

```text
Workspace
  [workspace-a]  [workspace-b  ×]                  existing attached-workspace controls

Policy
  [Use the default policy] [Reuse a saved policy ✓] [Custom policy]
  [Saved policy: team-policy]                     existing picker and stored-policy note
  A saved policy launches with one workspace. Remove the extra workspace,
  or choose Custom policy to keep them all.
  [Check again — disabled]

What this run can do                              existing rail, sections unchanged
  …
  [Launch — disabled]
```

F2 — default with two attached workspaces: the same refusal placement, with the existing `DEFAULT_ONE_WORKSPACE` sentence. F3 — switching to Custom leaves both workspace chips in place and clears only the policy-mode workspace hold. Removing an extra workspace instead preserves saved/default selection and clears the hold once at most one remains. Other gates can still hold Launch.

F4 — finished-run Overview terminal notice:

```text
Recording                     existing finished-run chip and notice
  [existing loading / missing / disabled / error sentence]
  Open the Recording tab →    text-info, same action, same accessible name
```

F5 — the existing device-code strip above the unchanged login-sandbox note:

```text
This sign-in is waiting for you
Approve it on the verification page. If the page asks for a code, enter this one.
[ABCD-EFGH]  [Open the verification page]  [Copy code]
Opens sso.example.com
```

F6 — the existing shell renewal strip above any page, including New Run:

```text
Waiting for you to finish signing in…                                    [Cancel]
Your browser blocked the sign-in window. Open it in a new tab             [Cancel]
That window closed before you signed in. [Sign in again]                  [Cancel]
Wardyn isn't answering. This page is still here — try again in a moment.   [Cancel]
```

The four rows are alternative states, not four simultaneous strips. A visible strip remains backed by session reconciliation even after a popup closes, a fallback wait ends, or 21 minutes pass. Cancel removes it and starts/continues the existing bounded, silent reconciliation watch if a sign-in may still land. This temporal difference changes no words. The reconciliation generation advances on known start/retry/cancel, popup close, focus/visibility and observed local auth mutation settlement; coalesce one fresh read through the existing `wfetch`/reauth notification seam. Do not replay writes. Device-code reads are serialized, reset per run/principal and cleaned up on unmount. A remote cookie mutation that this page has not observed remains a residual until a successful fresh read; this packet makes no atomic-cookie protection claim.

Prototype routes to supply: `/m-f/saved-two`, `/m-f/default-two`, `/m-f/custom-two`, `/m-f/terminal-notice`, `/m-f/waiting-sign-in`, `/m-f/renewal`. Each route must expose its error/loading transitions as click-through fixtures. Freeze the prototype's URL and revision in this packet after remote verification.

## 3. Exact strings and homes

Only the first row is new. Every other row is reused byte for byte. Names, codes, hostnames, policy names and formatted times are data.

| Key | Exact text | Home / placement |
|---|---|---|
| `POLICY_TEMPLATE_COPY.SAVED_ONE_WORKSPACE` — **addition** | A saved policy launches with one workspace. Remove the extra workspace, or choose Custom policy to keep them all. | `ui/src/app/components/wardyn/copy/policy-templates.ts`; policy refusal beside disabled Check again; same shared gate outcome for Launch |
| `POLICY_TEMPLATE_COPY.DEFAULT_ONE_WORKSPACE` | The default policy launches with one workspace. Remove the extra workspace, or choose Custom policy to keep them all. | Same file and existing default-policy placement |
| `POLICY_TEMPLATE_COPY.DEFAULT_TITLE` | Use the default policy | Same file; first mode card |
| Saved mode title | Reuse a saved policy | Existing `policy-panel.tsx` literal; preserve its home in F |
| Custom mode title | Custom policy | Existing `policy-panel.tsx` literal; preserve its home in F |
| Terminal-notice link | Open the Recording tab → | Existing `run-detail/terminal-notice.tsx` literal; only `text-primary` → `text-info` |
| `RUN_SIGN_IN.TITLE` | This sign-in is waiting for you | `wardyn/copy/run-sign-in.ts`; waiting strip |
| `RUN_SIGN_IN.BODY` | Approve it on the verification page. If the page asks for a code, enter this one. | Same file |
| `RUN_SIGN_IN.CODE_LABEL` | Verification code | Same file; code accessible name |
| `RUN_SIGN_IN.OPEN` | Open the verification page | Same file; real external link |
| `RUN_SIGN_IN.COPY` | Copy code | Same file; Copy label/name |
| `RUN_SIGN_IN.OPENS(host)` | Opens {host} | Same file; host rendered as a literal |
| `RUN_SIGN_IN.CHECKING` | Checking for a waiting sign-in… | Same file; delayed loading status |
| `RUN_SIGN_IN.NO_LONGER` | This sign-in is no longer waiting. The terminal below shows how it ended. | Same file; only after a waiting answer |
| `RUN_SIGN_IN.READ_FAILED` | Couldn't check whether a sign-in is waiting. The terminal below still shows it. | Same file; existing retained-answer failure |
| `STATES.RETRY` | Retry | Existing states copy; outline button |
| `RUNS_ROW_WORD.WAITING_SIGN_IN` | Waiting for sign-in | `wardyn/copy/runs-landing.ts`; existing row word |
| `RUNS_ROW_ACTION.SIGN_IN` | Sign in | Same file; existing row action |
| `REAUTH_RENEW.CTA` / `SIGN_IN_AGAIN` | Sign in again | `lib/session-renew-copy.ts`; consumed by lazy `lib/reauth-copy.ts` |
| `REAUTH_RENEW.CANCEL` | Cancel | `lib/reauth-copy.ts`; ghost button |
| `REAUTH_RENEW.RENEWED(time)` | Signed in again. Your session now lasts until {time}. | Same file; existing transient confirmation |
| `REAUTH_DIALOG.WAITING` | Waiting for you to finish signing in… | Same file; renewal waiting status |
| `REAUTH_DIALOG.POPUP_BLOCKED` | Your browser blocked the sign-in window. | Same file |
| `REAUTH_DIALOG.POPUP_FALLBACK` | Open it in a new tab | Same file; existing information-colored link |
| `REAUTH_DIALOG.CLOSED_WITHOUT` | That window closed before you signed in. | Same file |
| `REAUTH_DIALOG.UNREACHABLE` | Wardyn isn't answering. This page is still here — try again in a moment. | Same file |
| `REAUTH_DIALOG.ROLE_CHANGED_BODY` | You're signed in, but this page is no longer yours to open. Copy anything you need — Wardyn will take you to Runs. | Same file; existing role-change dialog |
| `REAUTH_EXTRA.GO_TO_RUNS` | Go to Runs | Same file |

The session-expiry banner, login-sandbox explanatory note, general terminal notices and Copy confirmation remain their existing canons; #1908 adds no sentence. The interactive prototype must import them from their current homes instead of retyping an abbreviated version.

## 4. All states, keyboard and screen reader

| State | Visible result and transition | Keyboard / announcement |
|---|---|---|
| F1 saved + 2+ workspaces | Show saved refusal; disable Launch and Check again; builder refuses without a request or dropping an attachment. | Focus stays on the selecting/removing control. Existing mode buttons expose pressed state. Associate the explanation with held controls. |
| F2 default + 2+ workspaces | Show existing default refusal; same blocking behavior. | Same contract; no duplicate announcement of the same sentence in panel and rail. |
| F3 custom + 2+ workspaces | Attachments survive; this hold clears. Other launch gates remain. | Mode selection stays focused; no automatic Launch or panel navigation. |
| Saved/default + 0 or 1 workspace | Existing behavior, including scratch and named workspace selection. | Existing focus order and names. |
| Earlier validation problem also present | Existing priority sentence remains the rail's problem; mode-specific workspace explanation remains by Check again, as the default does today. | Do not create competing assertive alerts for local validation. |
| Saved policy missing, loading or gone | Existing empty/loading/gone behavior coexists with the hold. | Existing refusal and Retry behavior; neither a fetch failure nor mode change erases selections. |
| Member/admin, accessible/inaccessible workspace | Existing capability/refusal behavior still applies; additional attachments never disappear from either role. | No inaccessible resource fact is introduced by this UI. |
| F4 notice loading/idle, error, ready-without-recording, recording disabled | Same notice text and Recording action in each existing arm, using `text-info`. Ready-with-recording still renders the player. | Same tab position and visible focus ring; Enter/Space retains the existing tab-switch action. |
| F5 initial loading | Existing note immediately; delayed checking line after 1 second. | `role="status"`; no focus transfer. |
| F5 initial not-waiting | Existing note, no claim that sign-in ended; retain approved first-two-minutes startup watch. | No announcement until state meaningfully changes. |
| F5 waiting | Keep reading every 5 seconds; focus/visible return triggers an immediate fresh read. | Same answer changes neither live text nor focus. No repeated screen-reader announcement per poll. |
| F5 changed code/link | Update once to the new verified answer and destination host. | One meaningful status update; retain focus on current control/terminal. |
| F5 not-waiting after waiting | Existing `NO_LONGER`; polling stops under existing terminal-state rules. | Announce state change once; no focus theft. |
| F5 failed read / unsafe non-HTTPS URL | Keep any last answer, show existing failure and Retry; existing failure stop/retry behavior. | Retry is outline; restored polling cannot focus the strip. |
| F5 other owner / unresolved identity / no longer running | No unauthorized read; existing strip/row eligibility remains. | No speculative waiting announcement. |
| F6 popup waiting / blocked / closed / server unreachable | Existing strip state; visible renewal reconciliation continues, including beyond the former quiet-watch bound. | Initial activation focuses Cancel once; routine reads never repeat that focus move. |
| F6 sign-in event races older `/me` response | Advance the read generation and reconcile fresh state; the older answer is inert. | The stale read cannot reannounce waiting, restore the banner, or undo a successful focus return. |
| F6 same person and authority, later live expiry | Adopt new `/me`, remove banner/strip, existing renewed toast. | Return focus to `#main-content` once. |
| F6 same person, unchanged expiry | Continue existing waiting/closed state; do not claim success. | No repeated live announcement. |
| F6 other/unresolved person or changed authority | Existing fresh-load or narrowed-role dialog rule at every expiry. | Do not preserve one person's draft for another. Existing dialog focus and Go to Runs remain. |
| F6 a request receives 401 during renewal | Same renewal serves the refusal; no second popup. | Existing held-page and sign-in dialogue semantics. |
| F6 Cancel / Escape inside strip | Cancel visible renewal, close handle if present, return existing banner/dialog; only the cancelled background watch uses the 21-minute redeemable bound. | Escape calls `preventDefault`; New Run's Escape and dirty-leave handler must not also fire. |
| F6 a cancelled sign-in later changes identity/authority | Existing silent watch still reconciles within its bound. | No renewal strip is resurrected; use existing fresh-load/role-change rules. |
| Narrow / 1280×650 / dark / light / reduced motion | Existing strips wrap; no overlap with shell banners or last focusable control; semantic tokens in both themes. | No animation required to understand any state; native focus ring stays visible. |

Device-code strip Tab order remains Open the verification page, Copy code, Retry only on error, then the existing terminal controls. The verification link remains an anchor with `target="_blank" rel="noopener noreferrer"`, with its host nearby. The renewal strip retains its existing Cancel-first focus contract, described status, fallback link or retry button, and Escape handling. Polling intervals are not live countdowns.

Regression evidence required: shared-builder refusal tests for saved and default (Launch/manual check/automatic check), preserving 2+ attachments and custom recovery; `terminal-notice` token regression; controllable pending-read sign-in race; visible renewal past 21 minutes; bounded cancelled watch; 5-second device reads plus focus/visibility refresh; unchanged repeated answers produce neither repeated status announcements nor focus movement. Keep `new-run-screen-form.test.tsx` defaultPrevented tests and 088 sign-in/renewal tests. The full UI Playwright suite runs outside `make ci`.

## 5. Decisions for this independent approval

- **F-D1:** Refuse saved/default multi-workspace requests at the shared builder boundary and show the matching mode sentence. Discarding extra workspaces is rejected because it silently changes the requested run.
- **F-D2:** Add exactly `SAVED_ONE_WORKSPACE`; default wording remains byte-exact. No saved/default mode renames.
- **F-D3:** #1906 changes only the link's semantic color token. Existing link copy and tab navigation remain.
- **F-D4:** #1908 changes read ownership, timing and refresh triggers, with no copy changes. A visible renewal remains measured; the quiet bound belongs only to a cancelled background watch.
- **F-D5:** Duplicate poll answers are silent and do not move focus. Existing owner-only, startup-watch, failure and successful-renewal transitions remain.
- **F-D6:** Approving M-F must identify its concrete design prototype URL/revision and the decisions above. It does not approve M-R or M-O. Packet text alone does not satisfy the remote prototype gate.

Owner approval record: **pending**. Design prototype URL/revision: **not created or verified yet**.
