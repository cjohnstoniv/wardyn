# M-O — recording-recovered Kubernetes output (#1831)

Status: **packet ready for review; Claude Design prototype and owner approval pending**. This independently approvable packet covers lane O's Output-tab rendering. Durable recording/mask erasure work and nonvisual output capture proceed in their lanes; this packet grants no visual approval.

Baseline: `7b08fd722ca4dcfd9d2d59e6f1cb8ecab54d8dcf`. Authority: `docs/design/CONSOLE-RULES.md`, `docs/design/SYNC.md`, `.design-sync/NOTES.md`, approved 088 C4 and existing M8 Output canon. See `design-access.md` for the unverified remote-project status. No product rendering code changed to prepare this artifact.

Top three corrections: show recovered output when available; state that full delivery is unverified; preserve erased/expired/masking refusal states. Reuse `Chip`, `CopyButton`, existing Output `Notice`, `EmptyState`, `ErrorState`, and the current focusable output block. No new color, size, radius or elevation. This packet does not restyle the existing output block.

## 1. What it unblocks

For recording-on Kubernetes runs, the server may recover output from the available recording. The Output tab identifies that source, keeps `incomplete: true`, and explains the delivery limit. A final recovered row can have `complete: true` to mean capture has finished; that must never imply the recovered output contains everything the run produced.

Lane O depends on the recording/mask erasure fences and M-O's owner-approved remote prototype for its visual portion. M-F and M-R approvals are independent.

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

O2 — still capturing (`complete: false`) uses the same source and notice, with the existing `live · refreshing` chip and polling. O3 — a final empty recovered body keeps the recovery notice and an empty output region; it omits “This run printed nothing.” because recovered bytes cannot prove that claim. An absent recording goes to its actual server refusal, not this recovered-success frame.

O4 — output unavailable (off, not kept, expired, erased, mask refusal, read error) uses the existing refusal surfaces and their actual reason. Recording and last-screen arms remain distinct from recovered command output.

Prototype routes to supply: `/m-o/recovered-final`, `/m-o/recovered-live`, `/m-o/recovered-truncated`, `/m-o/recovered-mask-gap`, `/m-o/loading-saving`, `/m-o/refusals`, `/m-o/interactive`. Driveable fixture controls switch true server-state combinations; a route cannot toggle `incomplete` off for a recording-recovered result. Freeze the real Claude Design URL/revision after remote creation.

## 3. Exact strings and homes

All Output copy lives in `ui/src/app/components/wardyn/copy/run-output.ts` (`RUN_OUTPUT`). Existing keys keep their names and text. Only two additions are proposed.

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
| `captureGap` — reused | Some of this run's output is missing: the server capturing it restarted, and the rest couldn't be recovered. | Existing capture-gap warning when true |
| `incomplete` — reused for other sources | This capture may be missing its last lines. | Direct capture/pane incomplete state; recording uses the stronger `recordingRecovered` statement instead of duplicating this warning |
| `truncated` — reused | Showing the end only — earlier output wasn't kept. | Existing tail-limit notice when true |
| `erasedTitle` — reused | This run's output was erased | Erasure refusal |
| `erasedDesc` — reused | It was removed at a person's erasure request and can't be restored. | Erasure refusal |
| `expiredTitle` — reused | This run's output has been deleted | Retention refusal |
| `expiredDesc` — reused | This deployment deletes kept output after a set number of days. | Retention refusal |
| `interactiveLink` — reused | Open the Recording tab → | Only existing refusal arms where recording navigation is appropriate; `text-info` |

Every other loading, off, no-output, interactive and masking sentence remains sourced directly from `RUN_OUTPUT` / existing `RUN_COCKPIT.loadError`; the packet does not amend them. The old `notCapturedTitle` / `notCapturedDesc` are not deleted merely because the new server path makes one case recoverable: inspect source history and older-server/reason coverage before any later deletion.

## 4. All states, keyboard and screen reader

| State | Required result | Interaction / announcement |
|---|---|---|
| Initial read <1 second | Existing empty busy body. | `aria-busy`; no flash of loading text. |
| Initial read ≥1 second | Existing delayed Loading output status. | One `role="status"` announcement; reduced-motion spinner treatment. |
| Recording source, live capture | From recording + live chip + recovery warning; `incomplete: true`; refresh every existing 4 seconds while `complete: false`. | Output text is not a live region; repeated polling does not reread the transcript or move focus. |
| Recording source, final capture | From recording + final chip/time + recovery warning; `incomplete: true`. | Stop polling when capture is complete; no automatic focus transfer. |
| Recording source, final, empty body | Keep provenance and warning with an empty output region only when the API supplied a recovered result; do not show “This run printed nothing.” for this source. | Never manufacture a successful empty result from a refusal. |
| Truncated recording | Recovery notice and existing truncated notice both render. | Both words are available before the output region. |
| Capture gap | Recovery notice and actual gap notice both render. | Do not suppress one independent limit behind another. |
| Global-only masking | Existing mask warning plus recovery notice. | No source switch or success claim bypasses masking. |
| Mask unavailable | Existing mask refusal, no recovered-output fallback. | Existing refusal semantics; no unsafe bytes exposed. |
| Just ended, row not yet kept | Existing Saving state inside the 60-second window, polling at 4 seconds. | Existing title/description, no false permanent no-output statement. |
| Output off | Existing off state. | No action that pretends the current run's output can be created. |
| Recording on but no usable recovery | Actual not-kept/not-captured/server reason, as appropriate. | Do not promise that a recording exists from the deployment setting alone. |
| Retention expired | Existing deleted state. | No reconstruction from a remaining recording after output retention denial. |
| Person-requested erasure | Existing erased state. | No recovery after durable recording or mask erasure fence; no Copy control containing old text. |
| Read fails / 5xx / other refusal | Existing `ErrorState` and Retry, or the existing mapped reason. | Retry is outline; its focus remains stable. |
| Not the viewer's accessible run | Server authorization/refusal controls the result. | No client fallback discloses recording bytes or membership facts. |
| Direct command output | Existing Command output state/copy, including actual incomplete flags. | Existing region name and Copy behavior. |
| Interactive last screen | Existing Last screen of this session caption and final-only semantics. | Region retains its source-specific accessible name. |
| Interactive, no screen, recording off/on/unknown, live/stopped/other terminal | Preserve the complete approved 088 C4 state matrix. | Existing recording link appears only under its current conditions. |
| Narrow / 1280×650 / dark / light / reduced motion | Header and warnings wrap without clipping; output remains bounded with `.scroll-thin`; semantic tokens preserve contrast. | Copy and output region remain keyboard reachable; reduced motion does not hide loading meaning. |

Tab order remains tab strip → Copy → output region → existing next page control. The block stays `tabIndex={0}`, `role="region"`, with `aria-label="From recording"` for this source. Arrow keys/Page Up/Page Down scroll the focused block using the browser; no keyboard trap. Copy returns exactly the displayed output string, without provenance or warnings appended, and uses the existing polite “Copied” result. Enter/Space activate Copy. The output remains a single React text node, never parsed HTML/Markdown/terminal escape sequences.

Preserve test IDs `run-output-text` and `run-output-refusal`; add coverage to the existing Output unit/e2e families for source discrimination, permanent `incomplete: true`, final-versus-complete distinction, notice combinations, erasure/retention precedence, Copy payload and focus. Backend lane O must supply real recording-on Kubernetes regression evidence; a mocked prototype does not establish that capture works.

## 5. Decisions for this independent approval

- **O-D1:** New source label is exactly “From recording”; it never borrows “Command output” or “Last screen of this session”.
- **O-D2:** `incomplete` remains true for recovered recording output. The exact recovery sentence is always visible and replaces the generic incomplete sentence for this source; true mask/gap/truncation notices remain.
- **O-D3:** `final` means capture stopped updating. It does not claim full delivery. No new “complete” UI word is added. Empty recovered bytes do not show the direct-output sentence “This run printed nothing.”
- **O-D4:** Existing off/erased/expired/mask/authorization refusals win over recovery; no client-side fallback reconstructs denied output.
- **O-D5:** The approved M8/088 output structure, interactive-state matrix, IDs, Copy contract and focusable plain-text region remain.
- **O-D6:** Approval identifies M-O's concrete Claude Design URL/revision and these decisions. It does not approve M-F/M-R. The packet and local source inventory alone do not satisfy the remote prototype gate.

Owner approval record: **pending**. Claude Design prototype URL/revision: **not created or verified yet**.
