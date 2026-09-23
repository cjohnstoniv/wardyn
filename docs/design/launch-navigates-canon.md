# Launch navigates (#125) — canon

Design record for issue #125: a launch that answers 2xx navigates straight to the run page, warnings
and all, instead of holding the New Run screen behind an "Open run" button. Owner-approved mock; the
strings below are frozen, byte-for-byte.

## Frozen strings

| Key | Status | Home | Value |
|---|---|---|---|
| `RUN_STATUS.PENDING_NO_DETAIL` | New | `ui/src/app/components/screens/run-status-detail.ts` | `Queued — Wardyn is getting this run ready.` |
| `RUN_DETAIL.LAUNCH_WARNING_TITLE` | Reused | `AGENTS.LAUNCH_WARNING_TITLE` (`ui/src/app/lib/workspace-providers-copy.ts`) | `Run launched with a warning` |
| `RUN_DETAIL.LAUNCH_WARNING_EPHEMERAL` | New | `ui/src/app/components/wardyn/copy/run-cockpit.ts` | `This note goes when you reload. The run's audit trail keeps it.` |
| `RUN_DETAIL.LAUNCH_WARNING_DISMISS` | New | `ui/src/app/components/wardyn/copy/run-cockpit.ts` | `Dismiss` |
| `AGENTS.OPEN_RUN_CTA` | Removed | was `ui/src/app/lib/workspace-providers-copy.ts` | `Open run` |

`RUN_DETAIL.LAUNCH_WARNING_TITLE` is not a second string: the run page imports and renders
`AGENTS.LAUNCH_WARNING_TITLE` directly (`run-detail/launch-warnings-note.tsx`), so the New Run rail's
retired advisory block and the run page's new one can never spell "launched with a warning" two
different ways.

## Behaviour

- `use-launch.ts`'s `launch()` always calls `navigate('/runs/' + id, { state: { launchWarnings } })`
  on a 2xx response, in the same tick — no held screen, no timer. `launchWarnings` is `created.warnings
  ?? []`, so the call shape is identical whether or not the 201 carried any.
- A refusal (4xx/422) never navigates: the form stays exactly as it stood, showing the server's own
  sentence. `isCredentialRefusal` and the 422 → sign-in door path are unchanged.
- `new-run-rail.tsx` drops the `onOpenRun` arm and its inline warnings block — there is no longer a
  state where Launch's slot holds anything but Launch.
- `run-detail.tsx` reads `useLocation().state?.launchWarnings` once, at mount, into local state.
  While non-empty it renders `LaunchWarningsNote` (`run-detail/launch-warnings-note.tsx`) in the rail's
  own advisory-block shape: the (reused) title, the warning text(s), the ephemeral-note line, and a
  Dismiss button that clears the local state. Router state does not survive a reload, so a reloaded
  run page shows nothing — correct, not a bug: the durable record is the `run.create` audit row's own
  clamp warnings, on the Audit tab (see the CHANGELOG's Known-gaps entry).
- `run-detail-summary-header.tsx`'s `statusChip` widens from `STARTING`-only to `STARTING || PENDING`.
  A `PENDING` run whose `status_detail` is still empty renders `RUN_STATUS.PENDING_NO_DETAIL` instead
  of nothing; the moment the server sends a real stage line, `status_detail` stops being empty and the
  ordinary `statusDetailChip`/`statusDetailSentence` derivation takes over, superseding it.
- `runs.ts`'s `createRun` drops the 5-minute `LAUNCH_DEADLINE_MS` and rides `wfetch`'s plain default
  deadline instead — `POST /runs` dispatches asynchronously now (#121), so the call no longer blocks
  through `CreateSandbox` server-side the way that constant's own doc comment describes.
  `preflightRun` is unchanged: it still runs that resolution synchronously and keeps the longer bound.

## Owner decisions

- **Q125-1 — does the launch-advisory note say it won't survive a reload?** Yes. The note carries an
  explicit ephemeral-note line (`RUN_DETAIL.LAUNCH_WARNING_EPHEMERAL`) rather than silently vanishing
  on the next reload with no explanation, and points at the durable record (the audit trail).
- **Q125-2 — what does a PENDING run with no status detail say?** The queued sentence
  (`RUN_STATUS.PENDING_NO_DETAIL`, "Queued — Wardyn is getting this run ready."), not a blank header.
  `STARTING`'s own empty-detail case stays silent (there is no ordinary wait worth naming before the
  pod is even scheduled) — `PENDING` is earlier still, and an all-blank header there reads as "nothing
  is happening" rather than "the request is in flight".
