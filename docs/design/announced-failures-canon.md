# Announced failures + disabled-control reasons — canon (#459)

Console a11y sweep: every failure line is announced to a screen reader, and every disabled
control states its reason in visible text, never only in a `title` tooltip. Owner approved the
mock; the strings below are canon, shipped byte-for-byte.

## Decisions

**Q459-1 — the launch and preflight failure lines.** Decided on the old New Run rail: both failure
lines carried `role="alert"`, with a visually-hidden prefix ("Launch failed" / "Preflight failed")
spoken first, so "what failed" preceded "why". That rail is retired. The new flow shows the server's own
sentence in a `role="alert"` region of its Launch dialog (`new-run-flow/launch/name-run-dialog.tsx`)
and does not carry the two prefix labels; they are no longer frozen strings.

**Q459-2 — disabled-control reasons.** A `title` tooltip explains nothing to a keyboard or touch
user, since a disabled control never receives focus or a tap-and-hold. Decided shape, reused
from the existing `OperatorOnlyHint` rulebook precedent (`wardyn/primitives.tsx`): when the
control sits in a row beside other content, the reason rides as trailing meta text beside it;
when it stands alone, the reason is helper text directly under it. A `title` tooltip may remain
only as a duplicate of the now-visible text, never as the sole explanation.

## Sites fixed

| Site | Shape | Reason shown |
|---|---|---|
| `record-pane.tsx` — per-host Approve button (`CaughtHosts`, in a row with the host) | trailing meta | `OperatorOnlyHint` (`OPERATOR_ONLY_REASON`) |
| `record-pane.tsx` — "Approve N selected hosts and replay again" (standing alone) | helper text under | `OPERATOR_ONLY_REASON` |
| `recording.tsx` — true-empty library (no runs at all) | `EmptyState` title + body | `RECORDINGS.EMPTY_TITLE` / `RECORDINGS.EMPTY_BODY` |
| `audit.tsx` `AuditScreen` — member's unfiltered feed | `EmptyState` title (already visible; verified, left as-is) | `AUDIT.MEMBER_FEED_TITLE` (`screens/audit-copy.ts`) |
| `sign-in.tsx:394` — disabled SSO button | **not touched** — removed outright by #457 | — |
| `setup-layout.tsx` — optional step's "Done with this one" (standing alone) (#497) | helper text under | the `refuseNext` reason |
| `setup-layout.tsx` — required step's "Next" (standing alone) (#497) | helper text under, for a plain `refuseNext` refusal only; a `nextGate.blocked` reason already shows in the gate's head/reason row above | the `refuseNext` reason |
| `phase-rail.tsx` — full rail step row (#497) | trailing meta (a line under the step's badge) | the `refuseNext` reason |
| `phase-rail.tsx` — icon-only compact rail (#497) | **exception:** no room for a caption, so the reason joins the button's `sr-only` accessible name, and the `title` stays for a sighted mouse user (at `lg` this is the only rail rendered) | the `refuseNext` reason |

## Frozen strings

| Key | Status | String |
|---|---|---|
| Check error fallback (`checkFailureText`, `new-run-flow/draft/use-intent-checks.ts`; the preflight blocker and the Tools step's failed-preview alert both use it) | Changed (#497) — was "Preflight failed.", spoken twice after the prefix | "No reason was given." |
| `OPERATOR_ONLY_REASON` (`wardyn/copy.ts`) | Reused | "Requires the admin role." |
| `RECORDINGS.EMPTY_TITLE` (`recording-copy.ts`) | Reused (mock naming) | "No recordings yet" |
| `RECORDINGS.EMPTY_BODY` (`recording-copy.ts`) | Changed | "Recordings appear once a run's terminal session is captured." |
| `RECORDING_DISABLED_TITLE` (`wardyn/copy/recording-disabled.ts`) | Reused, unchanged | "Session recording is disabled on this deployment" |
| `AUDIT.MEMBER_FEED_TITLE` (`screens/audit-copy.ts`) | Reused, unchanged | "The full audit feed is admin-only." |

## Out of scope

- `sign-in.tsx:394`'s disabled SSO button is removed outright by branch #457 — not touched here.
- The launch flow / `OPEN_RUN_CTA` logic is left alone (branch #125 edits it).
