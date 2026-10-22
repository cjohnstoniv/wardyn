# Scheduled runs: Runs list and run page strings

The owner-approved FEAT-2 boards F2SCHED-2 and F2SCHED-3 (approved 2026-10-20, issue #1986). Each row is the board's "Copy not on today's console", byte for byte. `ui/src/app/components/wardyn/copy/scheduled-runs.ts` holds them; `ui/src/app/lib/scheduled-runs-copy.test.ts` keeps this file and that one equal. The start-time picker, the Review row and the Launch line are `new-run-canon.md`'s (`START_*`, `STARTS`, `TIMING_STARTS`).

`{when}` is the start in the zone it was given in, with that zone's name: "Wed 21 Oct, 02:00 (UTC)". `{wait}` is today's short relative style without its "in": "12h". `{deadline}` is the queue's own deadline in the same form.

## 1. Frozen strings

| Key | Source | Frozen string |
|---|---|---|
| `WORD` | deviation (FEAT-2 board F2SCHED-2) | Scheduled |
| `ROW_STARTS(when)` | deviation (FEAT-2 board F2SCHED-2) | starts {when} |
| `PAGE_STARTS(when,wait)` | deviation (FEAT-2 board F2SCHED-2) | Starts {when} · in {wait} |
| `PAGE_NOTE` | deviation (FEAT-2 board F2SCHED-2) | Policy and your access are checked again when it starts. Nothing is set up and no credential is held until then. |
| `CANCEL_RUN` | mock | Cancel run |
| `ROW_DUE(when)` | deviation (FEAT-2 board F2SCHED-3) | was due {when} |
| `SIGN_IN_TITLE` | deviation (FEAT-2 board F2SCHED-3) | Waiting for your sign-in |
| `SIGN_IN_BODY(when,deadline)` | deviation (FEAT-2 board F2SCHED-3) | This run was due to start at {when}. It starts when you sign in again; nothing runs until then. If you don't sign in by {deadline}, the run fails without starting. Cancel it if you no longer need it. |
| `SIGN_IN_TIMED_OUT` | deviation (FEAT-2 board F2SCHED-3) | This run waited too long for your sign-in and was not started. |

## 2. Reused as is

The boards reuse today's strings, so no row above repeats them: "Waiting for sign-in" and "Sign in" (`RUNS_ROW_WORD.WAITING_SIGN_IN`, `RUNS_ROW_ACTION.SIGN_IN`), "Your AWS sign-in for {name} no longer works." (`CONNECTIONS.C6_LINE`), "Sign in again" (`SIGN_IN_AGAIN`), "Running", "Failed", "Wall" and the run header.

### Held by `new-run-canon.md`, recorded here for its source

| Key | Source | Frozen string |
|---|---|---|
| `NR_RUN.START_HELP(n)` | deviation (computed from the server's lead wait; overseer ruling 2026-10-21) | Up to {n} days ahead. Policy and your access are checked again when it starts. |
| `NR_RUN.START_FAR(n)` | deviation (computed from the server's lead wait; overseer ruling 2026-10-21) | Choose a time up to {n} days ahead. |
| `NR_REVIEW.TIMING_CHOOSE` | deviation (derived from board F2SCHED-1's logic, not its copy box; approved 2026-10-20) | Autonomous · Choose a start time |

## 3. Display rules

- `TIMING_CHOOSE` shows in the Review row while the start is missing or not one the server will take. The "too far ahead" check mirrors the server: a start later than 7 days less the run type's lead wait (24 hours for a background run) is too far; `schedule.test.ts` keeps the lead wait equal to the Go constants. `{n}` in `START_HELP` and `START_FAR` is the whole days in that span, "1 day" when it is one; the board draws a fixed 7.
- The word "Scheduled" and the "starts {when}" part show on a run that waits (`QUEUED`) with queue reason `scheduled`. "Waiting for sign-in", "was due {when}" and the sign-in page show on queue reason `owner_sign_in`.
- `SIGN_IN_TIMED_OUT` shows in the row's meta line of a `FAILED` run whose `status_reason` is `queue_sign_in_timeout`, joined with " · " as "started 14m ago" is. Today's "This run waited too long for a free slot and was not started." stays for `queue_timeout`.
- A cancelled scheduled run ends as a cancelled queued run does today. There is no Schedules screen in 0.10.0.
