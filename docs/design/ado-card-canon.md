# Azure DevOps card canon — issue #458

Mock packet 6a, owner-approved 2026-09-22 with all three recommendations. This is the decision
record for #458; the frozen strings themselves live in `ui/src/app/lib/ado-entra-copy.ts` and
`ui/src/app/lib/approvals-copy.ts`, and are pinned byte-exact against `ado-entra-prompt.md` §10.7
by `ado-entra-copy.test.ts` (that doc is the one the parser reads — this file is the human-readable
decision record, not a second source of truth the parser checks).

## What #458 fixed

Go grades an admin-token or local-mode caller `not_applicable`
(`scmaccess.go:137-139`, `isMechanism := subject == ""`). The Settings Azure DevOps card had no
branch for that state — it returned early only for an absent row (`state === ""`), so an admin who
had just configured a per-user row with the admin token saw a card titled "Azure DevOps" with an
empty body. Two smaller defects rode along: the capability card's consent CTA ("Allow and
continue") named a different act than the page it linked to (`ado-connection.tsx`'s own CTA,
"Connect Azure DevOps"), and landed at the top of a five-card Settings page with no anchor to the
one card it was about; and a single `busy` boolean spun both Approve and Deny on one click instead
of only the pressed button.

## Q458-1 — the not-applicable card: one line

**Decision: one line, no chip, no button.** `ADO.NOT_APPLICABLE_BODY` renders under the card's
title and nothing else — the Connected / Not connected / admin's-token-expired states are
unchanged, and a row that doesn't exist still renders no card at all (the absent-row doctrine every
other Settings card here follows).

| Key | String |
|---|---|
| `ADO.NOT_APPLICABLE_BODY` | This sign-in is an admin token, not a person, so it has no Azure DevOps connection of its own. Each person's own connection carries their runs. |

## Q458-2 — the consent CTA: match the destination

**Decision: "Connect Azure DevOps".** `ADO.REQ_CONSENT_CTA` changes from "Allow and continue" to
"Connect Azure DevOps" — the same label the destination card's own CTA (`ADO.CONNECT_ADO`) already
uses. Both consent-chain doors (the Entra-consent card and the mid-run sign-in card) now link to
`/settings#azure-devops`, the Settings card's own anchor, instead of a bare `/settings`.

| Key | String (unchanged, reused for the match) |
|---|---|
| `ADO.CONNECT_ADO` | Connect Azure DevOps |

## Q458-3 — the landing card takes focus

**Decision: a visible focus ring, no extra scroll.** The Settings Azure DevOps card carries
`id="azure-devops"`. On arrival with that hash, the card (`tabIndex={-1}`) receives `.focus()`
after render — the standard `--ring` focus treatment (`outline-none focus-visible:border-ring
focus-visible:ring-ring focus-visible:ring-[3px]`, the same classes `button.tsx`/`input.tsx` use),
no colour wash, and no scroll beyond whatever the browser's own focus-triggered scroll already
does.

## Also decided this packet (not numbered, folded into the same change)

- **`busy` becomes `"approve" | "deny" | null`.** While deciding, both Approve and Deny disable;
  only the pressed one shows the spinner. Every mount (`screens/approvals.tsx`,
  `wardyn/live-approvals.tsx`, `screens/run-detail.tsx`) tracks which action is in flight, not just
  whether one is.
- **The hold window gets its own timer.** `stillHeld()` was read only at render, so `REQ_HELD`
  survived past its own window until an unrelated re-render happened to catch it up. A timer sized
  to the remaining window now flips it to `REQ_HELD_EXPIRED` on its own, and is cleared on unmount.
- **`REQ_NOT_YOURS_BODY`'s "the run's owner" fallback is now `ADO.REQ_OWNER_FALLBACK`** — a literal
  outside the copy module no longer speaks for the design.
- **The four approve/deny toasts move into canon** — `APPROVALS.TOAST_APPROVED`,
  `TOAST_DENIED`, `TOAST_APPROVE_FAILED`, `TOAST_DENY_FAILED` (`ui/src/app/lib/approvals-copy.ts`,
  not `wardyn/copy.ts` — that module's own `APPROVAL` namespace is unrelated banner/scope copy).
  Every mount site (`screens/approvals.tsx`, `wardyn/live-approvals.tsx`, `screens/run-detail.tsx`)
  reads from it instead of a hand-typed literal. Two of the four (`TOAST_APPROVED`, `TOAST_DENIED`)
  match what the code already said; `TOAST_APPROVE_FAILED`/`TOAST_DENY_FAILED` are the only wording
  change — packet 6a's approval covers all four so the module has one settled home, not two
  matching-by-accident strings plus two changed ones.

| Key | String |
|---|---|
| `APPROVALS.TOAST_APPROVED` | Request approved |
| `APPROVALS.TOAST_DENIED` | Request denied |
| `APPROVALS.TOAST_APPROVE_FAILED` | Couldn't approve this request |
| `APPROVALS.TOAST_DENY_FAILED` | Couldn't deny this request |

## Full string table

See `ado-entra-prompt.md` §10.7 for the byte-exact frozen rows this packet added or changed
(`NOT_APPLICABLE_BODY`, `REQ_OWNER_FALLBACK`, and the `REQ_CONSENT_CTA` row edited in place in
§7.6) — that table, not this one, is what `ado-entra-copy.test.ts` parses and checks against.
