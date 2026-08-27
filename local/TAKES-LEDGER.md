# Takes ledger

Seeded 2026-08-24 from `/mnt/c/Users/Chaz/Videos/`. One row per id: what is on disk,
whether it is the take that ships, whether `scripts/verify-demo-take.sh` ever passed on
it, and whether the superseded files have been deleted yet.

EVERY file below predates the 0.6 console (each one films the pre-0.6 sidebar) and the
2026-08-24 series restructure renumbered 03 into 03a-03d — so all of them are SUPERSEDED
and none of them ships. They stay on disk until each id's replacement verifies (a folder
holding two numberings of 03 is where a stale cut gets published by mistake).

## Inventory

| id | files on disk | take status | verify | deleted |
|---|---|---|---|---|
| 01 | `wardyn-01-why-govern-agents-20260824T132656Z*` (2 files) | superseded · pre-0.6 console | unknown | no |
| 02 | `wardyn-02-set-up-the-host-20260824T133349Z*` (2 files) | superseded · pre-0.6 console | unknown | no |
| 02b | — | never shot | — | n/a |
| 02c | — | never shot | — | n/a |
| 03 (old numbering → 03a) | `wardyn-03-what-it-stops-20260824T170823Z*` (3 files) | superseded · pre-0.6 console AND pre-split | unknown | no |
| 03a | — | never shot | — | n/a |
| 03b | — | never shot | — | n/a |
| 03c | — | never shot | — | n/a |
| 03d | — | never shot | — | n/a |
| 04 | `wardyn-04-add-a-workspace-20260824T140200Z*` (2 files) | superseded · pre-0.6 console | unknown | no |
| 04b | — | never shot | — | n/a |
| 04c | — | never shot | — | n/a |
| 05 | `wardyn-05-your-first-policy-20260824T140646Z*` (2 files) | superseded · pre-0.6 console | unknown | no |
| 06 | `wardyn-06-your-first-run-20260824T141043Z*` (2 files) | superseded · pre-0.6 console | unknown | no |
| 07 | `wardyn-07-interactive-runs-20260824T143818Z*` (2 files) | superseded · pre-0.6 console | unknown | no |
| 08 | `wardyn-08-autonomous-agent-20260824T144250Z*` (3 files) | superseded · pre-0.6 console | unknown | no |
| 09 | `wardyn-09-record-a-run-20260824T142609Z*` (3 files) | superseded · pre-0.6 console | unknown | no |
| 10 | `wardyn-10-approvals-and-egress-20260824T143345Z*` (3 files) | superseded · pre-0.6 console | unknown | no |
| 11 | — | never shot | — | n/a |
| 12 | — | never shot | — | n/a |
| 12b | — | never shot | — | n/a |
| 13 | — | never shot | — | n/a |


> **Drift note (2026-08-24):** every take on this list predates the clock fix on `feat/v0.6-take-drift` — the picture
> lags the narration by a growing ~2–3.5% (≈5–6 s by t≈210 s; WSL2 realtime-vs-monotonic skew, `Date.now()` cue stamps vs
> Playwright's monotonic frame pacing). All are superseded for this reason too; `scripts/demo-drift.py` gates future takes.
> Fix MERGED into the series branch as `3246dd55` (drift check lives in `scripts/lib/verify-demo-take-drift.sh`, auto-sourced); old takes grade red on drift by design (Fable ruling: FAIL, not WARN).

## Attempt log (appended by scripts/take-chain.sh)

| when (UTC) | id | attempt | record rc | verify | artifact |
|---|---|---|---|---|---|
| 2026-08-25T01:54:36Z | 01 | 1 | 0 | FAIL | wardyn-01-why-govern-agents-20260825T014748Z-narrated.mp4 |
| 2026-08-25T02:09:15Z | 03a | 1 | 0 | FAIL | wardyn-03a-what-it-stops-20260825T015436Z-ffwd-narrated.mp4 |
| 2026-08-25T02:18:32Z | 03c | 1 | 0 | FAIL | wardyn-03c-authorized-then-issued-20260825T020916Z-ffwd-narrated.mp4 |
| 2026-08-25T02:25:42Z | 03d | 1 | 0 | FAIL | wardyn-03d-the-kinds-that-cant-use-a-header-20260825T021832Z-narrated.mp4 |
| 2026-08-25T02:30:30Z | 04 | 1 | 0 | FAIL | wardyn-04-add-a-workspace-20260825T022543Z-narrated.mp4 |
| 2026-08-25T02:34:36Z | 05 | 1 | 0 | FAIL | wardyn-05-your-first-policy-20260825T023030Z-narrated.mp4 |
| 2026-08-25T02:42:11Z | 09 | 1 | 0 | FAIL | wardyn-09-record-a-run-20260825T023437Z-ffwd-narrated.mp4 |
| 2026-08-25T02:46:51Z | 10 | 1 | 0 | FAIL | wardyn-10-approvals-and-egress-20260825T024212Z-ffwd-narrated.mp4 |
| 2026-08-25T02:56:14Z | 02 | 1 | 0 | FAIL | wardyn-02-set-up-the-host-20260825T024652Z-narrated.mp4 |
| 2026-08-25T03:14:24Z | 03b | 1 | 0 | PASS | wardyn-03b-the-network-three-more-ways-20260825T030748Z-ffwd-narrated.mp4 |
| 2026-08-25T03:14:32Z | 06 | 1 | 1 | not run | wardyn-06-your-first-run-20260825T031425Z.mp4 |
| 2026-08-25T03:14:58Z | 06 | 2 | 1 | not run | wardyn-06-your-first-run-20260825T031452Z.mp4 |
| 2026-08-25T03:15:24Z | 06 | 3 | 1 | not run | wardyn-06-your-first-run-20260825T031518Z.mp4 |
| 2026-08-25T03:15:32Z | 07 | 1 | 1 | not run | wardyn-07-interactive-runs-20260825T031525Z.mp4 |
| 2026-08-25T03:21:09Z | 08 | 1 | 0 | PASS | wardyn-08-autonomous-agent-20260825T031533Z-ffwd-narrated.mp4 |
| 2026-08-25T03:27:55Z | 04 | 1 | 0 | PASS | wardyn-04-add-a-workspace-20260825T032307Z-narrated.mp4 |
| 2026-08-25T03:42:35Z | 03a | 1 | 0 | PASS | wardyn-03a-what-it-stops-20260825T032755Z-ffwd-narrated.mp4 |
| 2026-08-25T03:51:58Z | 03c | 1 | 0 | PASS | wardyn-03c-authorized-then-issued-20260825T034235Z-ffwd-narrated.mp4 |
| 2026-08-25T03:59:14Z | 03d | 1 | 0 | PASS | wardyn-03d-the-kinds-that-cant-use-a-header-20260825T035158Z-narrated.mp4 |
| 2026-08-25T04:07:00Z | 09 | 1 | 0 | PASS | wardyn-09-record-a-run-20260825T035915Z-ffwd-narrated.mp4 |
| 2026-08-25T04:11:40Z | 10 | 1 | 0 | PASS | wardyn-10-approvals-and-egress-20260825T040700Z-ffwd-narrated.mp4 |
| 2026-08-25T04:16:12Z | 06 | 1 | 0 | PASS | wardyn-06-your-first-run-20260825T041141Z-narrated.mp4 |
| 2026-08-25T04:20:51Z | 07 | 1 | 0 | PASS | wardyn-07-interactive-runs-20260825T041612Z-narrated.mp4 |
| 2026-08-25T03:05:00Z | 01 | rp | 0 | PASS | wardyn-01-why-govern-agents-20260825T014748Z-narrated.mp4 |
| 2026-08-25T03:06:30Z | 02 | rp | 0 | PASS | wardyn-02-set-up-the-host-20260825T024652Z-narrated.mp4 |
| 2026-08-25T03:06:00Z | 05 | rp | 0 | PASS | wardyn-05-your-first-policy-20260825T023030Z-narrated.mp4 |

> Sweep 2026-08-25: exactly one verified narrated artifact kept per episode (13 kept, 62 superseded takes/intermediates deleted). 11/12 old takes untouched — superseded but replacement is desk-gated.
> Sweep addendum 2026-08-25: the pre-split monolithic 03 take (wardyn-03-*T170823Z* x3, superseded by the 03a-d split) deleted on owner instruction; directory now holds exactly the 13 verified artifacts.
