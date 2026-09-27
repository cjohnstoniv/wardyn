# Announced failures + disabled-control reasons — canon (#459)

Console a11y sweep: every failure line is announced to a screen reader, and every disabled
control states its reason in visible text, never only in a `title` tooltip. Owner approved the
mock; the strings below are canon, shipped byte-for-byte.

## Decisions

**Q459-1 — the rail's failure lines.** The launch and preflight errors on the New Run rail
(`new-run-rail.tsx`) already show the server's own sentence, unchanged and visible — the gap was
that a screen reader arriving after the fact heard nothing, since both were a bare
`<p className="text-danger">` with no `role="alert"`. Decided: `role="alert"` on both, with a
visually-hidden (`sr-only`) prefix spoken *first* — "Launch failed" / "Preflight failed" — so
"what failed" precedes "why". The prefix is announced only, never rendered as visible text, and
the visible sentence is untouched. A repeated, identical failure re-announces: the region is
keyed on an attempt counter (`errorSeq`/`preflightErrorSeq`, bumped in `use-launch.ts` on every
catch), so a second identical failure remounts the DOM node instead of updating it in place —
an in-place text update on a live region is silent to a screen reader.

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
| `recording.tsx` — search field, recording disabled on this deployment | helper text under | `RECORDINGS.SEARCH_DISABLED_HINT` |
| `recording.tsx` — true-empty library (no runs at all) | `EmptyState` title + body | `RECORDINGS.EMPTY_TITLE` / `RECORDINGS.EMPTY_BODY` |
| `audit.tsx` `AuditScreen` — member's unfiltered feed | `EmptyState` title (already visible; verified, left as-is) | `AUDIT.MEMBER_FEED_TITLE` (`screens/audit-copy.ts`) |
| `sign-in.tsx:394` — disabled SSO button | **not touched** — removed outright by #457 | — |

## Frozen strings

| Key | Status | String |
|---|---|---|
| `RAIL.LAUNCH_ERROR_LABEL` (`wardyn/copy/new-run-rail.ts`) | New — announced only, never visible | "Launch failed" |
| `RAIL.PREFLIGHT_ERROR_LABEL` (`wardyn/copy/new-run-rail.ts`) | New — announced only, never visible | "Preflight failed" |
| `OPERATOR_ONLY_REASON` (`wardyn/copy.ts`) | Reused | "Requires the admin role." |
| `RECORDINGS.SEARCH_DISABLED_HINT` (`recording-copy.ts`) | New | "Session recording is disabled on this deployment — there is nothing to search." |
| `RECORDINGS.EMPTY_TITLE` (`recording-copy.ts`) | Reused (mock naming) | "No recordings yet" |
| `RECORDINGS.EMPTY_BODY` (`recording-copy.ts`) | Changed | "Recordings appear once a run's terminal session is captured." |
| `RECORDING_DISABLED_TITLE` (`wardyn/copy/new-run-rail.ts`) | Reused, unchanged | "Session recording is disabled on this deployment" |
| `AUDIT.MEMBER_FEED_TITLE` (`screens/audit-copy.ts`) | Reused, unchanged | "The full audit feed is admin-only." |

## Out of scope

- `sign-in.tsx:394`'s disabled SSO button is removed outright by branch #457 — not touched here.
- The launch flow / `OPEN_RUN_CTA` logic in `new-run-rail.tsx` is left alone (branch #125 edits
  it); this change touches only the two error regions and the `RunRailProps` fields that feed
  them.
