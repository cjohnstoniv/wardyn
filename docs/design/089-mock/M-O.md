# M-O — recording-recovered Kubernetes output (#1831)

Status: **packet ready for review; design prototype and owner approval pending**. This independently approvable packet covers the Output-tab rendering. Durable recording and mask erasure work and nonvisual output capture proceed separately; this packet grants no visual approval.

Baseline: `7b08fd722ca4dcfd9d2d59e6f1cb8ecab54d8dcf`. Authority: `docs/design/CONSOLE-RULES.md`, `docs/design/SYNC.md`, `.design-sync/NOTES.md`, approved 088 C4 and existing M8 Output canon. No product rendering code changed to prepare this artifact.

Top three corrections: show recovered output when available; distinguish capture gaps from clean empty output; preserve erased/expired/masking refusal states. Reuse `Chip`, `CopyButton`, existing Output `Notice`, `EmptyState`, `ErrorState`, and the current focusable output block. No new color, size, radius or elevation. This packet does not restyle the existing output block.

## 1. What it unblocks

For recording-on Kubernetes runs, the server may recover output from the available recording. The Output tab identifies that source, keeps `incomplete: true`, and explains the delivery limit. A final recovered row can have `complete: true` to mean capture has finished; that must never imply the recovered output contains everything the run produced.

Missing, malformed or uncovered recovery data yields a capture-gap result, not a clean empty result or an invented refusal. The existing `PG.SaveGapRunOutput` can store `source: "stdout"`, empty output and `incomplete: false`; `handleRunOutput` serves that final row as HTTP 200 with `complete: true` and `capture_gap: true`. Gap rendering therefore depends on `capture_gap`, independently of source or `incomplete`. This packet corrects the proposed UI contract; the current product rendering has not been changed.

The Output-tab rendering depends on the recording/mask erasure fences and on M-O's owner-approved prototype for its visual portion. M-F and M-R approvals are independent.

## 2. Surfaces

O1 — recovered recording, final row:

```text
[Overview] [Approvals] [Policy] [Audit] [Recording] [Output]
┌ existing Output card ──────────────────────────────────────────────────────┐
│ OUTPUT  (From recording) (final)                 Captured 14:02:11 [Copy]  │
│ ⚠ Recovered from the available recording. Full output delivery could not  │
│   be verified.                                                            │
│ [additional existing mask / gap / truncation notices when true]            │
│ ┌ focusable existing output block ──────────────────────────────────────┐  │
│ │ $ go test ./...                                                      │  │
│ │ ok   example.com/app/internal/api   4.118s                            │  │
│ └──────────────────────────────────────────────────────────────────────┘  │
│ Also from the CLI: wardyn run output run_example                           │
└───────────────────────────────────────────────────────────────────────────┘
```

O2 — still capturing (`complete: false`) uses the same source and notice, with the existing `live · refreshing` chip and polling. O3 — a final empty recovered body keeps the recovery notice and an empty output region. Neither recording recovery nor any capture gap may show “This run printed nothing.” or “Nothing printed yet.”: available bytes do not establish those claims.

O4 — missing/malformed/uncovered recovery with an empty stored gap row:

```text
[Overview] [Approvals] [Policy] [Audit] [Recording] [Output]
┌ existing Output card ──────────────────────────────────────────────────────┐
│ OUTPUT  (Command output) (final)                 Captured 14:02:11 [Copy]  │
│ ⚠ Some or all of this run's output could not be recovered.                 │
│ ┌ focusable existing output block; empty body ──────────────────────────┐  │
│ │                                                                     │  │
│ └──────────────────────────────────────────────────────────────────────┘  │
│ Also from the CLI: wardyn run output run_example                           │
└───────────────────────────────────────────────────────────────────────────┘
```

The empty-body annotation above describes the drawing; it is not UI copy. The source chip and region name are the actual response's `sourceStdout` label. This result is HTTP 200, final, gap true; incomplete may be false. It has neither a From recording chip nor the recording-recovery sentence. An enabled recording setting alone does not establish that a recording exists. Copy stays available and copies the empty response string; it does not copy the warning or claim that bytes were recovered.

O5 — partial recording plus a gap uses O1 with both recovery and gap notices and the actual returned bytes. O6 — genuine off/not-kept/expired/erased/mask/refusal/read-error responses retain their existing surfaces and actual reasons. Recording and last-screen arms remain distinct from recovered command output.

| Server outcome | Response / source of truth | Required frame |
|---|---|---|
| Available recording successfully decoded, including an empty output body | HTTP 200; `source: "recording"`; `incomplete: true`; other flags are actual returned values | O1/O2/O3; recovery notice always; no clean-empty sentence |
| Missing, malformed or uncovered recovery when recovery is owed and no separate gate denies it | HTTP 200 final row with `capture_gap: true`; keep actual source, output and incomplete flag; an empty `stdout` row with incomplete false is valid | O4 or actual partial-output frame; gap notice regardless of source; no not-kept/not-captured substitution |
| Recovered partial recording with an actual gap | HTTP 200; `source: "recording"`; incomplete true, gap true | O5; both notices; partial bytes remain readable and copyable |
| Policy/persistence off or no recovery owed | Existing actual off/not-kept reason, or existing live response where supported | Existing server-selected path; never synthesize a durable recovery/gap from configuration alone |
| Authorization, erasure, retention, unsafe live mask read, or another genuine refusal/error | Actual server refusal/error retains its precedence | O6; no client reconstruction, success region or stale Copy payload |

An uncovered recovery writes no unsafe bytes and produces the gap result above. An unsafe live read can instead be refused by the masking gate; these are separate server outcomes. Keep the server's gate order. The legacy `run_output_not_captured` branch is not a fallback for failed recovery, and its “The run's recording has it.” sentence must never appear in a gap frame. The wire gap flag has no restart-cause field, so no gap frame may infer a restart.

Prototype routes to supply: `/m-o/recovered-final`, `/m-o/recovered-live`, `/m-o/recovered-truncated`, `/m-o/recovered-mask-gap`, `/m-o/gap-empty-stdout`, `/m-o/gap-partial-recording`, `/m-o/loading-saving`, `/m-o/refusals`, `/m-o/interactive`. The gap route exposes missing/malformed/uncovered fixtures separately. Driveable fixture controls switch true server-state combinations; a route cannot toggle `incomplete` off for a recording-recovered result. Freeze the real prototype URL/revision after it is created.

## 3. Exact strings and homes

All Output copy lives in `ui/src/app/components/wardyn/copy/run-output.ts` (`RUN_OUTPUT`). Two additions and one explicit canon amendment are proposed for owner approval. Other existing keys keep their names and text. These proposals have not changed source code or received visual approval.

| Key | Exact text | Placement |
|---|---|---|
| `sourceRecording` — **addition** | From recording | Source chip and output region's accessible name when `source === "recording"` |
| `recordingRecovered` — **addition** | Recovered from the available recording. Full output delivery could not be verified. | Existing warning `Notice`, immediately before the output; remains whenever recording-recovered output is shown |
| `sourceStdout` — reused | Command output | Direct command-output source |
| `sourcePane` — reused | Last screen of this session | Pane snapshot source |
| `live` — reused | live · refreshing | `complete: false` |
| `final` — reused | final | `complete: true`; state of capture, not a completeness guarantee |
| `capturedAt(t)` — reused | Captured {clockTime(t)} | Final capture time |
| `copyLabel` — reused | Copy output | Existing Copy button's accessible name |
| Copy visible literal — reused | Copy | `run-detail/output-tab.tsx`; existing button text |
| `loading` — reused | Loading output… | After the existing 1-second loading delay |
| `savingTitle` — reused | Saving this run's output… | Existing recent-end waiting state |
| `savingDesc` — reused | It shows here in a few seconds. | Same state |
| `globalsOnly` — reused | Part of this capture was masked without this run's own secrets, so a secret given to this run may appear unmasked. | Existing mask-scope warning when true |
| `captureGap` — **copy amendment** | Some or all of this run's output could not be recovered. | Existing warning whenever `capture_gap: true`, for every source; makes no restart or recording-exists claim |
| `incomplete` — reused for other sources | This capture may be missing its last lines. | Direct capture/pane incomplete state; recording uses the stronger `recordingRecovered` statement instead of duplicating this warning |
| `truncated` — reused | Showing the end only — earlier output wasn't kept. | Existing tail-limit notice when true |
| `erasedTitle` — reused | This run's output was erased | Erasure refusal |
| `erasedDesc` — reused | It was removed at a person's erasure request and can't be restored. | Erasure refusal |
| `expiredTitle` — reused | This run's output has been deleted | Retention refusal |
| `expiredDesc` — reused | This deployment deletes kept output after a set number of days. | Retention refusal |
| `interactiveLink` — reused | Open the Recording tab → | Only existing refusal arms where recording navigation is appropriate; `text-info` |
| `emptyFinal` — reused with restricted condition | This run printed nothing. | Existing direct-output empty state only when neither gap nor recording recovery applies |
| `emptyLive` — reused with restricted condition | Nothing printed yet. | Existing direct-output live empty state only when neither gap nor recording recovery applies |

The current `captureGap` sentence is “Some of this run's output is missing: the server capturing it restarted, and the rest couldn't be recovered.” The proposed replacement above is deliberate: a gap reports unavailable output without identifying the cause. This amendment applies to all gap rows, including existing stdout/pane rows, and must be reviewed in the concrete prototype.

Every other loading, off, interactive and masking sentence remains sourced directly from `RUN_OUTPUT` / existing `RUN_COCKPIT.loadError`. The old `notCapturedTitle` / `notCapturedDesc` are not deleted merely because a server path becomes recoverable: inspect source history and older-server/reason coverage before any later deletion. Their existing legacy refusal coverage is distinct from the new recovery-gap mapping.

## 4. All states, keyboard and screen reader

| State | Required result | Interaction / announcement |
|---|---|---|
| Initial read <1 second | Existing empty busy body. | `aria-busy`; no flash of loading text. |
| Initial read ≥1 second | Existing delayed Loading output status. | One `role="status"` announcement; reduced-motion spinner treatment. |
| Recording source, live capture | From recording + live chip + recovery warning; `incomplete: true`; refresh every existing 4 seconds while `complete: false`. | Output text is not a live region; repeated polling does not reread the transcript or move focus. |
| Recording source, final capture | From recording + final chip/time + recovery warning; `incomplete: true`. | Stop polling when capture is complete; no automatic focus transfer. |
| Recording source, final, empty body | Keep provenance and recovery warning with an empty output region only when the API supplied a recovered result; no clean-empty sentence. | Copy is enabled and copies exactly `""`; region is focusable and described by the warning. |
| Truncated recording | Recovery notice and existing truncated notice both render. | Both words are available before the output region. |
| Empty gap, source stdout, incomplete false | HTTP 200/final; Command output label; cause-neutral gap notice and blank output region; no clean-empty sentence, recovery notice or not-captured frame. | Copy is enabled and copies exactly `""`; region is named Command output and described by the gap notice. |
| Partial gap, source stdout | Actual bytes and source label; gap notice plus only other true flags. | Copy uses exactly the returned partial bytes, never warning text; focus stays stable. |
| Partial gap, source recording | From recording, incomplete true; recovery and gap notices both render. | Actual partial bytes remain readable/copyable; region is described by both notices. |
| Global-only masking | Existing mask warning plus recovery notice. | No source switch or success claim bypasses masking. |
| Unsafe live mask read | Existing mask refusal, no recovered-output fallback. | Existing refusal semantics; no unsafe bytes exposed. Uncovered historical recovery instead produces a gap without unsafe bytes. |
| Just ended, row not yet kept | Existing Saving state inside the 60-second window, polling at 4 seconds. | Existing title/description, no false permanent no-output statement. |
| Policy/output off or persistence off | Preserve actual existing off/not-kept/live response; no recovery owed from a disabled persistence path. | No client fallback or action that pretends durable output can be created. |
| Recording on but missing/malformed/uncovered recovery | Actual HTTP 200 capture-gap result with the returned source and bytes; final capture may have no bytes. | Missing recording is not a policy/erasure/retention/auth refusal; the recording setting alone adds no provenance. |
| Retention expired | Existing deleted state. | No reconstruction from a remaining recording after output retention denial. |
| Person-requested erasure | Existing erased state. | No recovery after durable recording or mask erasure fence; no Copy control containing old text. |
| Read fails / 5xx / other refusal | Existing `ErrorState` and Retry, or the existing mapped reason. | Retry is outline; its focus remains stable. |
| Not the viewer's accessible run | Server authorization/refusal controls the result. | No client fallback discloses recording bytes or membership facts. |
| Direct command output | Existing Command output state/copy, including actual incomplete flags; gap-specific empty suppression applies here too. | Existing region name and Copy behavior. |
| Interactive last screen | Existing Last screen of this session caption and final-only semantics. | Region retains its source-specific accessible name. |
| Interactive, no screen, recording off/on/unknown, live/stopped/other terminal | Preserve the complete approved 088 C4 state matrix. | Existing recording link appears only under its current conditions. |
| Narrow / 1280×650 / dark / light / reduced motion | Header and warnings wrap without clipping; output remains bounded with `.scroll-thin`; semantic tokens preserve contrast. | Copy and output region remain keyboard reachable; reduced motion does not hide loading meaning. |

Tab order for every HTTP-200 output frame remains tab strip → Copy → output region → existing next page control, including empty gaps and empty recovered bodies. The block stays `tabIndex={0}`, `role="region"`, with the source-specific accessible name. Gap/recovery regions use `aria-describedby` referencing their visible warning(s), including when the body is empty; entering the region exposes the limitation without moving focus or repeatedly announcing polling updates. No output/refusal transition steals focus. Arrow keys/Page Up/Page Down scroll the focused block using the browser; no keyboard trap.

Copy remains enabled on HTTP-200 results and returns exactly `o.output`, including the empty string, without provenance, placeholders or warnings appended. Copying an empty gap writes `""` to the clipboard and reports the existing polite “Copied” once for that action; it does not report successful recovery. Partial recording output copies only the returned partial text. Enter/Space activate Copy; focus remains on the button. Refusals have no output Copy control or old text. The output remains a single React text node, never parsed HTML/Markdown/terminal escape sequences.

The following acceptance fixtures pin the mapping for the prototype and later implementation. `empty` means `""`; `partial` means the exact test string `available output\n`. Notice and empty-sentence cells name `RUN_OUTPUT` keys; `none` adds no text. `Copy payload` is exact and Copy is enabled in all these HTTP-200 cases. These are proposed fixture expectations, not evidence that product tests or rendering already changed.

<!-- output-result-fixtures -->
| Fixture | HTTP | Source | Complete | Incomplete | Gap | Output | Source label | Notices | Empty sentence | Copy payload |
|---|---|---|---|---|---|---|---|---|---|---|
| gap-missing | 200 | stdout | true | false | true | empty | sourceStdout | captureGap | none | empty |
| gap-malformed | 200 | stdout | true | false | true | empty | sourceStdout | captureGap | none | empty |
| gap-uncovered | 200 | stdout | true | false | true | empty | sourceStdout | captureGap | none | empty |
| gap-partial-stdout | 200 | stdout | true | false | true | partial | sourceStdout | captureGap | none | partial |
| gap-partial-recording | 200 | recording | true | true | true | partial | sourceRecording | recordingRecovered,captureGap | none | partial |
| recording-partial | 200 | recording | true | true | false | partial | sourceRecording | recordingRecovered | none | partial |
| recording-empty | 200 | recording | true | true | false | empty | sourceRecording | recordingRecovered | none | empty |
| recording-live-empty | 200 | recording | false | true | false | empty | sourceRecording | recordingRecovered | none | empty |
| stdout-clean-empty | 200 | stdout | true | false | false | empty | sourceStdout | none | emptyFinal | empty |
| stdout-live-empty | 200 | stdout | false | false | false | empty | sourceStdout | none | emptyLive | empty |
<!-- /output-result-fixtures -->

Exercise each missing/malformed/uncovered fixture independently; assert HTTP-200 frame (`run-output-text`), absence of `run-output-refusal`, absent clean-empty/recording-exists/restart claims, source-specific accessible name, warning description, stable focus and exact empty Copy payload. For partial recording with/without gap, assert incomplete true, the exact applicable warnings, actual text and exact Copy payload; polling must not move focus or reread it. Independently return each genuine off/not-kept/erased/expired/mask/auth response and verify its established refusal semantics with no successful-output region or stale Copy data; do not turn a refusal into a gap. Keep the clean-empty stdout controls above to show that suppression depends on recovery/gap, not body length alone.

Preserve test IDs `run-output-text` and `run-output-refusal`; add the fixtures above to the existing Output unit/e2e families, alongside final-versus-complete distinction, notice combinations and refusal precedence. These fixture expectations are written proposals; they do not exercise the product. Backend work must supply real recording-on Kubernetes regression evidence; a mocked prototype does not establish that capture works.

## 5. Decisions for this independent approval

- **O-D1:** New source label is exactly “From recording” only for an actual recording-source response. Other results retain their actual source label; a recording setting is not provenance.
- **O-D2:** `incomplete` remains true for recovered recording output. The exact recovery sentence is always visible and replaces the generic incomplete sentence for this source; true mask/gap/truncation notices remain.
- **O-D3:** `final` means capture stopped updating. It does not claim full delivery. No new “complete” UI word is added. Empty recovery and every empty capture gap suppress clean-empty sentences regardless of source/incomplete. Missing/malformed/uncovered recovery maps to the actual HTTP-200 gap result.
- **O-D4:** Existing off/erased/expired/mask/authorization refusals win over recovery; no client-side fallback reconstructs denied output.
- **O-D5:** The approved M8/088 output structure, interactive-state matrix, IDs, enabled exact-byte Copy contract (including empty strings) and focusable plain-text region remain. Gap/recovery regions gain visible-warning descriptions.
- **O-D6:** Approval identifies M-O's concrete design prototype URL/revision and these decisions. It does not approve M-F/M-R. The packet and local source inventory alone do not satisfy the remote prototype gate.
- **O-D7:** Amend `RUN_OUTPUT.captureGap` to exactly “Some or all of this run's output could not be recovered.” for all sources. No restart or recording-exists claim is inferred from the gap flag. This copy amendment is proposed, unimplemented and pending owner approval.

Owner approval record: **pending**. Design prototype URL/revision: **not created or verified yet**.
