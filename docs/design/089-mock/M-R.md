# M-R — New Run panels and shared policy document

Status: **owner-approved 2026-10-08 at prototype Version 7 (`1791427781-730a`), the version shipped in 0.8.9**. Tracker #1916. This independently approvable packet gates the rendering of the New Run panels, the shared policy viewer and editor, and the visual portions of the draft work. Nonvisual parser, preview, request-controller and mechanical split work can proceed meanwhile. No product rendering code was changed to prepare this packet.

Baseline: `7b08fd722ca4dcfd9d2d59e6f1cb8ecab54d8dcf`. Read together with `regression-inventory.md` and `canon-inventory.md`. Superseded plan wording is not canon. The prototype must use the console design-system project (see `docs/design/SYNC.md`). The component bundle and prototype are not yet verified, so these frames remain authoring instructions, not screenshots or proof of a remote prototype.

Top three corrections: make the launch draft navigable without losing state; preserve the complete nine-section rail and its final verdict reading order; provide one read-only policy document and explicit YAML editing without mutating inspected policies. Existing tokens, four body type rungs, three elevations and one primary action apply. No new color, radius, ad-hoc size or elevation is proposed.

Reuse `Button`, `Field`, `OptionCard`, `Chip`, `WidgetCard`, `SectionCard`, `SectionLabel`, `Input`, `Textarea`, `Select`, `Dialog`, `AlertDialog`, `CopyButton`, `YamlBlock`, `TierPicker`, `ConfinementChip`, `BarrierStrengthStrip`, `EmptyState`, `ErrorState`, existing workspace/drive controls, existing connection doors, `GitPATSection`, `ADOCapabilitiesSection`, `ADOAccessSummary`, `AdoLaunchNote`, `AdoRunTokenLine`, `ToolRulesSection` and `PushRulesSection`. Hoist the current `permissions.tsx` `Segmented`; preserve its `aria-pressed` and dirty-chip accessible-name behavior. The only new navigation pattern is `PanelNav`, drawn below. Do not reuse the demo runner's `StepList`.

## 1. What it unblocks

| Surface | Result |
|---|---|
| New Run | Four freely navigable panels: Run, Workspace, Access, Policy. The source string and all selections survive panel/view changes, invalid edits and connection doors. Launch is available from every panel with the same gates. |
| Shared read-only viewer | `PolicyDocument` supplies Summary/YAML/JSON views to New Run, Policies, Governance and Run detail. Wrappers keep loading, history, provenance and freshness. Run detail retains the policy the run actually used. |
| Explicit source editor | YAML is the default editable source; explicit JSON editing remains. View changes do not convert the source. Structured edits preserve unrelated YAML comments during the session. |
| Access | Existing supported access controls become relevant to selected workspaces/model/source. Reasons explain each row. There is one editable source for ADO/PAT, and no new generic API/MCP/runtime catalog. |
| Carried forward from M-F | M-F's default/saved 2+ workspace refusal appears in the new panels and still refuses every builder path without dropping attachments. |

## 2. Surfaces

R1 — desktop, representative Run panel at 1280×650. The aside is bounded within the viewport; its sections scroll independently. The decision block and Launch share the same aside budget.

```text
[Runs]                                                          New run
┌ PanelNav: nav “New run” ──────────────────────────────────────────────────────┐
│ [Run · current]  [Workspace]  [Access · 1 issue]  [Policy]                    │
└──────────────────────────────────────────────────────────────────────────────┘
┌ active panel ───────────────────────┐  ┌ What this run can do ───────────────┐
│ Run                                │  │ independently scrolling sections: │
│ [existing harness / command choice]│  │ Ceiling                             │
│ [existing run mode]                │  │ Policy                              │
│ Model provider [existing picker]   │  │ Barrier                             │
│ [existing provider status / door]  │  │ Autonomy                            │
│ Task or Command *                  │  │ Credentials — summary retained      │
│ [primary field; initial focus]     │  │ Startup                             │
│ [existing startup/tool approvals]  │  │ Tool rules                          │
│ ▸ Run details                      │  │ Push rules                          │
│   Title / Description              │  │ Recording                           │
│                                    │  ├ decision block, directly above CTA ┤
│ [Continue to Workspace — outline]  │  │ current verdict/class/warnings      │
└────────────────────────────────────┘  │ check state / error / problem       │
                                        │ [Launch run — sole primary]         │
                                        └─────────────────────────────────────┘
```

The diagram names all nine sections; each retains its existing conditional visibility, so an empty heading is not invented for a shell run. Sections never acquire a replacement “network/access/behavior” inventory. The decision block includes the existing risk/verdict, enforced class, warnings, check status, server errors and one applicable local problem. If its content exceeds the available space, it joins the aside scroll with Launch still reachable; do not clip the reason to preserve a decorative fixed height. The verdict remains directly before Launch in visual and DOM reading order. No duplicate problem sentence appears beside both its owning control and the rail.

R2 — the panel bodies. Panel labels are navigation; Back/Continue never launch.

```text
Workspace                            Access
  Workspace [existing select]          [relevant access rows and current status]
  [extra workspace chips]              Required by workspace-a
  [existing scratch choice]            Selected model provider
  [existing create dialog opener]      Source policy / Added by you
  [mounts / read-only / drive]          [existing ADO / PAT / SSH controls]
                                       [Add access — outline]
  [Back] [Continue to Access]           [Back] [Continue to Policy]

Policy
  [existing Barrier/TierPicker states]
  [Use the default policy] [Reuse a saved policy] [Custom policy]
  [saved picker or existing default loading/unavailable state, where applicable]
  [Run draft] [Source policy]           existing Segmented idiom, controlled
  [Summary] [YAML] [JSON]               shared Segmented, read-only views
  [current policy document / stale or pending facts]
  [Copy YAML] / [Copy JSON]             matching view/action, see copy contract
  [Edit policy] or [Customize for this run]
  [existing templates / tool rules / push rules when editing custom source]
  [Back]                               Policy has no Continue
```

The model-provider picker moves from the rail into Run beside the harness. The Credentials rail still states selected-provider identity, residency and connection status. The Access panel can show “Selected model provider” as a dependency reason, but does not introduce a second picker. Title and Description retain their labels/IDs inside “Run details”; Task/Command receives initial focus.

R3 — narrow layout below the current `lg` breakpoint. The new footer pattern uses `border-t border-border bg-card`. The active panel's end padding reserves the **measured** footer height, shell/reauth stack and safe-area inset, including expanded summary and wrapped errors.

```text
[shell banner / renewal strip — existing behavior]
[Runs] New run
[Run] [Workspace] [Access · 1 issue] [Policy]   wraps; all four remain reachable
Run
  [active panel controls, one column]
  [Back] [Continue to …]
  [reserved space keeps final control and focus ring clear of footer]
┌ footer, one instance of rail/decision content ───────────────────────────┐
│ [What this run can do ▸]                  expanded/collapsed button     │
│   expanded: existing nine sections in bounded .scroll-thin region       │
│ verdict / enforced class / warnings / check state / error / problem     │
│ [Launch run — sole primary]                                             │
└─────────────────────────────────────────────────────────────────────────┘
```

Do not render both desktop and narrow active footers into the accessibility tree. Expansion reveals the existing summary; it does not reveal Launch for the first time. When the virtual keyboard reduces space, focused controls and the decision block remain reachable by scrolling; no overlay hides the current focus ring. Use the existing desktop `lg:max-h-[calc(100vh-5rem)]` budget as the starting constraint; the prototype must demonstrate both 1280×650 cases and narrow 390×844 and 320×568 viewports. These viewport fixtures add no product size tokens. Re-derive both existing 1280×650 tests rather than deleting them.

R4 — explicit editing and confirmations:

```text
Source policy                                         [Done editing — outline]
[YAML] [JSON]                                         source format controls
Spec (YAML) *
[authored source textarea, comments and invalid text retained]
(Valid YAML) or (Invalid YAML — {message})
Line {line}, column {column}                         invalid diagnostic position
Comments are kept while you edit; they are not stored when the policy is saved or the run launches.
[Copy source]                                         exact source, even invalid

Switch to JSON?                                      confirm, sm:max-w-md
JSON does not preserve YAML comments. Switching changes your editable source.
[Keep YAML — ghost] [Switch to JSON — default]

Replace your custom policy?                          confirm, sm:max-w-md
Your existing custom policy will be replaced by this source.
[Keep custom policy — ghost] [Replace custom policy — default]
```

“Done editing” changes presentation and never submits, discards or makes invalid source valid. When custom source is invalid, structured edit controls are disabled, Copy source remains available, and the invalid-source remedy returns to the textarea. Read-only YAML/JSON view switches never open the conversion dialog. Explicit JSON→YAML conversion has no YAML-comment loss and uses the existing format control; conversions require valid source and never operate on a last-valid substitute.

R5 — migrated read surfaces:

```text
New Run / Policy: [Run draft | Source policy] + PolicyDocument
  Run draft = authorized current server preview; provisional/pending/stale context visible.
  Source policy = authorized saved/default source or this session's authored custom source.

Policies: existing default/selected-policy wrappers + PolicyDocument
  Existing create/edit dialog opens the YAML-default editor; Save is still explicit.

Governance: existing profile inspection/editor preview + PolicyDocument
  Existing profile fields, ceiling rules and save flow stay in their wrapper.

Run detail / Policy: existing provenance and change groups + PolicyDocument
  Summary: Barrier → This run used → actual run.confinement_class
  Added at start / Removed at start marks retained
  Hidden mount source + “Only admins can see this.” tooltip retained
  YAML/JSON: existing <redacted> explanatory note above raw content
```

`PolicyDocument` is read-only. Its concrete inputs are `spec`, controlled `view: summary | yaml | json` with `onViewChange`, `redacted`, optional existing `changeMarks`, `facts?: {usedClass?, requestedClass?}`, and optional authored `source: {text, format}`. A completed run passes actual `usedClass`; New Run may pass `requestedClass`. No fake `RunDetail` is built for a draft. Wrappers own source/provenance/history/freshness/loading/error state and whether copy actions are permitted. `YamlBlock` remains the single display emitter; the lazy source parser is not added to `code-block.tsx` or the eager shell.

Prototype fixture routes must cover `/m-r/new-run/{run,workspace,access,policy}`, `/m-r/policy/{default,saved,custom,invalid,stale,limited}`, `/m-r/doors/{provider,ado,leave,replace,convert}`, `/m-r/consumers/{policies,governance,run-detail}` and the states below. These are planned route names, not existing URLs. The shared canvas scenario chooser must use real copy without adding product-visible debug labels.

## 3. Exact strings and homes

New copy is imported directly from lazy files `ui/src/app/components/wardyn/copy/new-run-flow.ts` (`NEW_RUN_FLOW`) and `ui/src/app/components/wardyn/copy/policy-document.ts` (`POLICY_DOCUMENT`). Do not re-export it through the eager `copy.ts` barrel. Existing canon remains in its current homes, listed in `canon-inventory.md`.

### New Run additions

| Proposed key | Exact text | Placement |
|---|---|---|
| `NEW_RUN_FLOW.RUN` | Run | PanelNav and Run h2 |
| `NEW_RUN_FLOW.WORKSPACE` | Workspace | PanelNav and Workspace h2; existing selector name preserved |
| `NEW_RUN_FLOW.ACCESS` | Access | PanelNav and Access h2 |
| `NEW_RUN_FLOW.POLICY` | Policy | PanelNav and Policy h2; existing domain label preserved |
| `NEW_RUN_FLOW.RUN_DETAILS` | Run details | Title/Description disclosure |
| `NEW_RUN_FLOW.BACK` | Back | Ghost panel action |
| `NEW_RUN_FLOW.ISSUE_COUNT(n)` — additional explicit proposal | 1 issue / {n} issues | Textual PanelNav Chip; singular for 1, plural otherwise |
| `NEW_RUN_FLOW.CONTINUE(panel)` | Continue to {panel} | Outline panel action; only next panel name interpolated |
| `NEW_RUN_FLOW.ADD_ACCESS` | Add access | Access dialog opener |
| `NEW_RUN_FLOW.REQUIRED_BY(workspace)` | Required by {workspace} | Dependency reason |
| `NEW_RUN_FLOW.MODEL_PROVIDER` | Selected model provider | Dependency reason |
| Reuse `POLICY_DOCUMENT.SOURCE_POLICY` | Source policy | Dependency reason; one definition shared with the policy context |
| `NEW_RUN_FLOW.ADDED_BY_YOU` | Added by you | Explicit selection reason |
| `NEW_RUN_FLOW.INACTIVE_ACCESS` | Not used by the selected workspaces. | Retained explicit access whose dependency is inactive |
| `NEW_RUN_FLOW.ADO_WORKSPACE` | Select an Azure DevOps workspace to configure this access. | Access remedy points to Workspace |

### Policy document/editor additions and explicit canon amendments

| Proposed/reused key | Exact text | Placement / amendment |
|---|---|---|
| `POLICY_DOCUMENT.RUN_DRAFT` | Run draft | Combined-preview context |
| `POLICY_DOCUMENT.SOURCE_POLICY` | Source policy | Authored/selected-source context |
| `POLICY_DOCUMENT.EDIT` | Edit policy | Explicit custom source edit |
| `POLICY_DOCUMENT.DONE` | Done editing | Leave edit presentation, preserve draft |
| `POLICY_DOCUMENT.CUSTOMIZE` | Customize for this run | Seed Custom from authorized source or safe starter |
| Existing `POLICY_TAB.viewSummary` | Summary | **Kept**; “Pretty” is rejected |
| Existing `POLICY_TAB.viewYaml` | YAML | **Kept**; existing display view |
| `POLICY_DOCUMENT.JSON` | JSON | Added read-only view and explicit editor format option |
| Existing `POLICY_TAB.copyYaml` | Copy YAML | **Kept**; copy payload contract below |
| `POLICY_DOCUMENT.COPY_JSON` | Copy JSON | Added view copy |
| `POLICY_DOCUMENT.COPY_SOURCE` | Copy source | Exact authored source copy |
| `POLICY_DOCUMENT.SPEC_YAML` | Spec (YAML) | **Copy change:** default label in all three editable sources, previously Spec (JSON) |
| Existing editor label / `POLICY_DOCUMENT.SPEC_JSON` | Spec (JSON) | **Retained** for explicit JSON editing; pinned tests/narration amended together |
| `POLICY_DOCUMENT.VALID_YAML` | Valid YAML | Added format-specific validity chip |
| `POLICY_DOCUMENT.INVALID_YAML(message)` | Invalid YAML — {message} | Added error chip; parser supplies message/position |
| Existing validity text / `POLICY_DOCUMENT.VALID_JSON` | Valid JSON | **Retained** in explicit JSON mode |
| Existing validity text / `POLICY_DOCUMENT.INVALID_JSON(message)` | Invalid JSON — {message} | **Retained** in explicit JSON mode |
| `POLICY_DOCUMENT.SOURCE_POSITION(line,column)` | Line {line}, column {column} | **New copy proposal:** separate visible and described diagnostic position; positive one-based integers |
| `POLICY_DOCUMENT.INVALID_GATE` | The policy spec isn't valid YAML or JSON. | **Copy change:** replaces “The policy spec isn't valid JSON.” in the shared gate and its anchors |
| `POLICY_DOCUMENT.REQUESTED_CLASS` | This run requests | New Run Barrier fact; run detail keeps “This run used” |
| `POLICY_DOCUMENT.COMMENTS` | Comments are kept while you edit; they are not stored when the policy is saved or the run launches. | Editor helper |
| `POLICY_DOCUMENT.SAFE_CUSTOM` | Some source settings are hidden. Customization starts from a safe policy. | Redacted source customization |
| `POLICY_DOCUMENT.INVALID_PREVIEW` | This preview is out of date. Fix the policy source to refresh it. | Invalid-source stale viewer/rail |
| `POLICY_DOCUMENT.STALE_PREVIEW` | This preview is out of date. Check again to refresh it. | Aged or externally invalidated preview |
| `POLICY_DOCUMENT.PROVISIONAL` | This preview includes your selections. Launch checks may change it. | New Run combined preview |
| `POLICY_DOCUMENT.PENDING` | Not checked in this preview. | Facts whose launch-only checks have not run |
| `POLICY_DOCUMENT.RATE_LIMIT(seconds)` | Preview limit reached. Try again in {seconds}s. | Preview 429; separate from existing preflight 429 copy |
| `POLICY_DOCUMENT.JSON_TITLE` | Switch to JSON? | Conversion confirmation title |
| `POLICY_DOCUMENT.JSON_BODY` | JSON does not preserve YAML comments. Switching changes your editable source. | Confirmation body |
| `POLICY_DOCUMENT.KEEP_YAML` | Keep YAML | Ghost cancellation |
| `POLICY_DOCUMENT.SWITCH_JSON` | Switch to JSON | Confirm conversion |
| `POLICY_DOCUMENT.REPLACE_TITLE` | Replace your custom policy? | Replacement confirmation title |
| `POLICY_DOCUMENT.REPLACE_BODY` | Your existing custom policy will be replaced by this source. | Confirmation body |
| `POLICY_DOCUMENT.KEEP_CUSTOM` | Keep custom policy | Ghost cancellation |
| `POLICY_DOCUMENT.REPLACE_CUSTOM` | Replace custom policy | Confirm replacement |

The `Spec (JSON)`/validity literals currently live in `policy-panel.tsx`; centralizing those unchanged literals for the shared lazy editor is an address change, not a wording change. Run-detail `Summary`, `YAML`, `Copy YAML`, redaction, provenance, change marks and `SUMMARY.used` remain their existing canon entries. Add the JSON-view amendment to the run-policy-view canon and extend its pin test in V. The YAML default explicitly requires updating the existing three-editor tests and demo 05/06 narration; no silent anchor deletion.

### Strict-parser diagnostics — new written-review and owner-approval proposal

This inventory reads the actual policy-document parser (`ui/src/app/lib/policy-document/index.ts`), with exact `yaml@2.9.1`. These diagnostics are internal and unrendered today. Listing them here proposes their presentation; it does not approve new product copy. See `canon-inventory.md` for the separate parser hash, distinct from the original source baseline.

All fixed messages below live in `ui/src/app/lib/policy-document/index.ts`, in the named symbol. Consumers use the returned diagnostic verbatim rather than duplicating these strings in `POLICY_DOCUMENT` or the eager `copy.ts` barrel. Parse failures use the existing outer `INVALID_YAML(message)` / `INVALID_JSON(message)` templates and the proposed `SOURCE_POSITION(line,column)`. A refused structured operation on valid source shows its message and position beside the originating control without an invalid-source label. A successful strict parse proves a JSON-compatible mapping, not server policy validation, authorization or launch readiness.

<!-- parser-fixed-diagnostics -->
| Source symbol | Byte-exact fixed diagnostic |
|---|---|
| `caught` | `Policy source could not be read.` |
| `documentValue` | `Policy source must be a mapping.` |
| `documentValue` | `Mapping keys must be strings.` |
| `documentValue` | `Merge keys (<<) are not allowed.` |
| `documentValue` | `Aliases are not allowed.` |
| `documentValue` | `Explicit tags are not allowed.` |
| `documentValue` | `Numbers must be finite and within the safe integer range.` |
| `documentValue` | `Only JSON-compatible values are allowed.` |
| `readSource` | `Directives are not allowed.` |
| `readSource` | `MULTIPLE_DOCS: Policy source must contain one document.` |
| `editPolicySource` | `The edit must name a mapping field or a sequence item without gaps.` |
| `editPolicySource` | `The edit must contain only JSON-compatible values and safe numbers.` |
<!-- /parser-fixed-diagnostics -->

Variable diagnostics are data, not additional fixed canon:

- `readSource` returns `${issue.code}: ${issue.message}` from the first library error, otherwise the first warning. Warnings are fatal. Library wording and quoted source/key/tag text belong to the pinned library; retain the complete returned message, without interpreting markup, inventing a friendlier rewrite or turning it into a translation key.
- `caught` returns the thrown `Error.message` from parsing, composition, AST traversal, conversion, path mutation or serialization. It uses the fixed fallback above only for a non-Error throw. Both paths return line 1, column 1; that fallback position does not prove where an unexpected failure originated.
- `failure` returns one-based line/column from `LineCounter` at the offending token/node offset. Preserve the returned integers separately from the message. Directive refusal precedes composition; multiple-document refusal precedes library errors/warnings; the first AST refusal follows those. Do not promise an exhaustive list or infer an AST message when an earlier library diagnostic wins.
- Treat every returned message as potentially sensitive authored-source data. Render only as escaped text, never HTML/Markdown or executable links; retain literal text and line breaks in a wrapping, selectable error region. Do not send messages, source snippets or unknown thrown errors to telemetry, analytics, crash reports, URLs or unrelated logs. Prototype fixtures use synthetic policies without real secret values, private paths or names. Only the authorized current editor may show its diagnostic; clear it with its source on principal change and never reconstruct hidden source from a redacted preview.

### Gate sentences, unchanged except the declared parser sentence

| Gate/home | Exact text or canonical function |
|---|---|
| `new-run-launch-gates.ts` task | An autonomous run needs a task to perform. |
| Same, command | Enter a command to run. |
| Parser amendment above | The policy spec isn't valid YAML or JSON. |
| `RUN.POLICY_GONE`, `copy/run-clone.ts` | That saved policy no longer exists — pick another. |
| Existing saved selection gate | Pick a saved policy, or write a custom one. |
| `RAIL_PROVIDER.NOT_GRANTED(harness)` | You haven't been granted a model provider for {harness} — ask your admin. |
| `RAIL_PROVIDER.DEFAULT_OFF(name,harness)` | {name}, the default for {harness}, is turned off. Choose another model provider to launch. |
| `RAIL_PROVIDER.DEFAULT_OFF_ONLY(name,harness)` | {name}, the default for {harness}, is turned off. Ask your admin. |
| `RAIL_PROVIDER.LAUNCH_HINT` | Choose a model provider to launch. |
| `POLICY_TEMPLATE_COPY.DEFAULT_ONE_WORKSPACE` | The default policy launches with one workspace. Remove the extra workspace, or choose Custom policy to keep them all. |
| M-F `POLICY_TEMPLATE_COPY.SAVED_ONE_WORKSPACE` | A saved policy launches with one workspace. Remove the extra workspace, or choose Custom policy to keep them all. |
| `RAIL_SETUP.BACKEND_BLOCK` | This host can't build the barrier this run needs. |
| `RAIL_MODEL_ACCESS.UNATTENDED_BLOCK(agent)` | No model provider serves {agent} for you, so an autonomous run would fail. Switch Run mode to Interactive, or connect one. |
| `NO_BARRIER.LAUNCH_REASON` | No barrier can be built on this host, so no run can be confined. |

Other existing workspace/provider-pin/quota/ADO/server refusal sentences remain imported from their current canonical sources or server envelope; no new rewrite layer is introduced. Actionable issue rows use those exact sentences and reveal/focus the owning control. Existing `RAIL.LAUNCH_ERROR_LABEL` (“Launch failed”) and `RAIL.PREFLIGHT_ERROR_LABEL` (“Preflight failed”) remain screen-reader prefixes on `role="alert"` nodes remounted by `errorSeq`.

The leave dialog reuses `UNSAVED`/`UNSAVED_GUARD` from the existing guard: “Leave without saving?”, “Your changes on this page haven't been saved. Leaving loses them.”, “Keep editing”, “Discard changes”. No replacement leave copy. Navigation label “New run”, “Runs”, “Launch run”, `RAIL_CHECK` and the nine section headings keep their existing canonical wording.

## 4. All states, keyboard and screen reader

### Navigation, focus, launch and dialogs

| State/transition | Required behavior |
|---|---|
| First entry | Run active; Task/Command autofocus, falling back to the existing harness/startup control when that mode has no task field. Title does not steal initial focus. |
| Direct panel click / Continue / Back | Free navigation, no completion lock. Focus destination h2 (`tabIndex=-1`), then Tab enters controls. No source/selection mutation. |
| PanelNav semantics | `<nav aria-label="New run">`; buttons with `aria-current="step"` only on active panel; neutral selected surface and text `Chip` issue counts. Native Tab/Shift+Tab/Enter/Space, no faux tablist or undocumented arrow-key requirement. |
| Issue activation | Reveal owning panel/disclosure, focus its control; `aria-invalid` and `aria-describedby` point at applicable error. Link styling is `text-info`; repeated gate sentences are not rendered in two places. |
| Continue / last panel | Continue is outline; Back is ghost. Policy has only Back. Enter never launches, including Title, Task/Command, source textarea and panel navigation. |
| Press Launch | Sole primary for the New Run surface; same body/gates from every panel. Immediate disabled state; delayed spinner. Dirty clears only on successful 2xx launch/navigation. |
| Launch/preflight refusal | Existing server sentence plus screen-reader failure prefix, `role="alert"` and new `errorSeq` for repeated failed attempts. A visible refusal remains actionable. |
| Provider door opened in Run | Cancellation returns to its still-mounted opener; successful sign-in returns to persistent provider picker/control. Set `returnTo` on the door; no effect racing Radix focus restoration. |
| Provider door opened by Launch refusal | Return to Launch; only the exact explicitly clicked body can retry once after successful sign-in. Changed body re-checks instead. Preflight-origin doors re-check only. |
| ADO connect / blocked popup / completed / cancelled | Existing dialog, status and recovery vocabulary. Completion invalidates preview/preflight and returns to its opener. **Never autolaunch.** |
| Add access / AddWorkspaceDialog | Existing supported forms in a form-sized dialog (`lg`); headings, labels, validation and focus stay with existing components. Confirm dialogs use `sm:max-w-md`; larger content owns a scroll container. |
| Escape untouched | Leave through the shared request-leave path. Native layer/reauth `defaultPrevented` wins first. |
| Escape dirty / Runs / in-page links / sidebar / browser back | `useRequestLeave` with existing UNSAVED dialog; Keep editing preserves everything and restores opener; Discard performs the requested navigation. |
| Dirty definition | Source, policy mode and run/workspace/access selections count; panel, disclosure and display-view preferences do not. Mode switching preserves current custom source. No persistent draft store is added. |
| Narrow footer expansion | Button announces expanded/collapsed; focus stays on toggle; summary appears before decision block in DOM. Focused panel content and final controls stay outside the overlay's reserved area. |
| Dark/light/reduced motion | Token roles and focus rings preserved; motion reduction removes nonessential motion without losing status meaning. |

### Policy modes and source lifecycle

| State | Required rendering / data rule |
|---|---|
| Initial non-clone | Custom remains preselected; moving Default to the first card does not change launch defaults. |
| Clone of saved/custom/missing source | Preserve existing clone banner, selected saved reference, title/task/harness selections and missing/deleted-policy remedies. A restricted/redacted clone is never editable as raw recovered values. |
| Default loading | Existing `DEFAULT_LOADING`, delayed status/spinner; read-only preview slot. Mode remains selectable; display loading alone does not block server-resolved Launch. |
| Default unavailable | Existing `DEFAULT_UNAVAILABLE` + Retry, no fabricated source. Launch behavior stays gated by existing launch checks, not preview-read availability. |
| Default with profile / member / admin | Existing profile-named hint and default note; member redaction preserved. Admin sees only what their authorized source read exposes. |
| Default with 2+ workspaces | Existing default one-workspace sentence; Launch/Check again/preview builder refuse; all attachments retained. |
| Saved loading / empty list / no choice / source deleted | Existing loading, empty picker and gate canon. Source policy cannot be edited through inspection. |
| Saved selected | Stored spec governs; source view read-only; existing stored-policy note and ADO summary retained in their relevant panels. |
| Saved with 2+ workspaces | Exact M-F saved one-workspace sentence; every reference-mode builder path refuses; no workspace silently dropped. |
| Default↔Saved↔Custom | Existing custom source restored, including invalid text and comments; no automatic conversion or selection loss. |
| Customize, no existing custom replacement | Use authorized unredacted source object serialized as YAML; member/redacted source uses safe starter with SAFE_CUSTOM note. Never feed a redacted marker or resolved server preview into editable state. |
| Customize replaces existing custom | Explicit replacement dialog; cancellation preserves exact custom source; confirmation replaces it with the named selected source or safe starter. |
| Custom valid YAML | One source string; structured operations parse→mutate→serialize back to string. Preserve unrelated fields/comments; no YAML Document object in React state. |
| Custom valid JSON | Explicit JSON source editor with Spec (JSON), matching JSON validity words; pasted JSON goes through the same strict policy parser. |
| Invalid source | Exact source and position/message retained. No preview/preflight/launch of last-valid data. Structured mutation disabled; sticky confinement floor retained. Last preview may remain only with INVALID_PREVIEW and current-copy disabled. |
| YAML-invalid cases | Duplicate keys, extra documents, warnings/tags, directives, aliases/merge keys, nonstring keys, nonfinite/unsafe numbers and nonmapping root produce an invalid state; no source coercion masquerades as valid. |
| Display view changes | Summary→YAML→JSON→Summary changes only view. Source, comments, dirty state and serialized launch body remain identical. |
| Explicit YAML→JSON | Valid source required; conversion confirmation carries the exact comment-loss copy. Keep YAML preserves bytes; Switch to JSON changes authored source and dirty state. |
| Save/Launch | Submit object through existing server contract; comments disappear because they are session source content, as the visible helper states. |

### Invalid-source diagnostic frames and interaction

These written fixtures extend R4 and the planned `/m-r/policy/invalid` scenario; they are not implemented screens or browser evidence. Each invalid-parse fixture must be driveable in the real design prototype in both editor formats where applicable. Its displayed diagnostic comes from the parser result, with the format-specific outer label and separate position above; operation refusals retain valid-source status as specified below. `parsePolicySource` failures expose only `ok`, `line`, `column`, `message`, never `value` or a Document.

| Fixture / transition | Diagnostic and state to demonstrate |
|---|---|
| Duplicate YAML or pasted JSON key | Library `DUPLICATE_KEY` diagnostic; demonstrate nested and escaped JSON-key duplicates. `# heading\na:\n  b: 1\n  b: 2` locates line 4, column 3. |
| Second document, including empty second document | Exact fixed `MULTIPLE_DOCS` diagnostic; `a: 1\n---\nb: 2` locates line 2, column 1. |
| Unknown tag / warning-only result | Library `TAG_RESOLVE_FAILED` diagnostic is invalid even without a library error. Known explicit tags use the fixed tags refusal when AST validation is reached. |
| Directives / aliases / merge keys | `%YAML` and `%TAG` tokens refuse; aliases including unresolved/cyclic references refuse; quoted or JSON `<<` keys refuse. `a:\n  <<: {}` locates line 2, column 3; `a: 1\r\nb: *missing` locates line 2, column 4. Quoted/block-string lookalikes remain literal text. |
| Nonstring key / nonmapping root / empty source | Fixed mapping-key or root-mapping refusal, including scalar, sequence, null and comment-only roots. Empty source is invalid, not an empty policy; `{}` is the valid empty mapping control. |
| Nonfinite / unsafe number | Fixed numbers refusal, including unsafe JSON integers and overflow; `a:\n  n: .nan` locates line 2, column 6. Quoted numeric-looking strings and safe bounds remain valid controls. |
| Malformed syntax / source-bearing message | Actual first library error, including an HTML-looking synthetic tag/key. Text remains literal and selectable; no link, image or markup executes and no diagnostic enters telemetry. |
| Unexpected exception / defensive non-JSON value | Show the actual caught Error message or non-Error fallback; preserve line 1, column 1 without claiming that it is the fault site. The fixed JSON-compatible-values refusal is defensive AST coverage, not a promise that normal core-schema input produces objects. |
| Structured edit path/value/serialization failure | Preserve the exact original source; no replacement `source` is returned. Show the returned operation diagnostic at its originating control, with the position. A refused operation on a still-valid document does not relabel that document invalid or change its body; invalid starting source instead follows the shared invalid-parse state. |
| Invalid → Run → Workspace → Policy → corrected | Preserve exact bytes, comments, format and dirty state through navigation; show the same diagnostic on return. Correction reparses current text and removes only the resolved error; it does not resurrect an older accepted body or automatically Save/Launch. |

For an invalid parse, the source textarea stays enabled and receives `aria-invalid="true"`; `aria-describedby` includes its visible diagnostic, position and existing helper. Use one polite, atomic `role="status"` region for changing local validity, not an assertive launch-failure alert. Stable repeated results do not reannounce. Typing, parser completion and preview responses never move focus or reset the caret. Tab/Shift+Tab still reach Copy source, Done editing, the remedy and navigation; Enter in the source inserts a newline and never submits. Escape follows the existing dirty-leave/dialog rules. An activated invalid-source issue reveals Policy and editing, then focuses the textarea; its description supplies the diagnostic/position without forcing a caret jump, including for fallback line 1, column 1.

Summary is the existing view name (called “Pretty” in superseded planning); do not rename it. While authored source is invalid, its Summary and converted raw view show the invalid-source remedy without a synthesized spec, a success chip or last-valid source facts. The matching raw view may show the exact invalid authored bytes as invalid text. Copy source alone remains enabled and copies those bytes, excluding diagnostic/position text; generated, converted, Summary Copy YAML and Run draft Copy controls are disabled while invalid. View switching never formats, repairs or overwrites the source. Structured fields, insertion/template mutations and format conversions that need a parsed mapping remain disabled, with the focusable Edit policy remedy available outside the disabled group; unrelated run/workspace choices remain navigable.

The rail retains all nine sections and applicable non-source facts, including the sticky confinement floor, but does not display last-valid source facts or an old green check as current. One invalid-source gate issue sits in the decision block directly before Launch and focuses the source; the detailed parser message appears only beside its owning editor, never duplicated in the rail. A previously authorized Run draft preview may stay visible only with `INVALID_PREVIEW` and disabled current-preview Copy; without a previous preview, show the remedy without invented content. Invalid source schedules no preview/preflight request and cannot launch/save a last-valid object. Superseded responses cannot restore freshness. Correcting the source uses the existing settled scheduler and gates; the old preview remains stale until a current authorized response arrives. These same source/Copy/focus rules apply in Policies and Governance editors, with their own explicit Save action and no New Run rail.

### Copy and freshness contract

| Action/state | Exact source of copied or displayed content |
|---|---|
| Copy source | Authored source string byte for byte, including comments and invalid input. Available even when invalid or preview-stale. |
| Matching authored YAML/JSON view | Copy preserves authored text when valid; it does not normalize matching-format source. Summary Copy YAML uses authored YAML when available and valid. |
| Converted/generated copy | Current valid spec serialized to requested format; generated YAML uses `YamlBlock`/its existing emitter. Never claim comments persist in normalized output. |
| Run draft copy | Current authorized server preview, not source draft or previous-body preview. Disable while stale/invalid/unavailable. |
| Representable blocked draft | Preview remains inspectable when task/credentials/runner readiness blocks Launch; launch-only facts use PENDING. |
| Preview pending facts | Task, provider selection, credential liveness, autonomy/tool approvals, runner confinement, drive readiness and dispatch egress use the actual response's pending fields. Do not show requested class as an enforced result. |
| Preview refused / read failure | Existing error semantics and actionable remedy; no unauthorized spec rendered or fallback from another selection/principal. |
| Body changes / out-of-order reads | One 800ms settled scheduler; abort superseded calls and reject late generations. Preview/preflight use the one builder. Comment/whitespace-only equivalent bodies do not refetch. |
| 60-second freshness expiry | Mark stale; do not keep an old check green or begin background polling. STALE_PREVIEW and Check again offer recovery. |
| Preview 429 | Keep last preview visibly stale; exact RATE_LIMIT text; one retry after Retry-After only if still same request/generation. Further retries manual. Existing preflight 429 wording/behavior remains separate. |
| External change | Provider-door completion/close, ADO completion/close, selected source/default/governance refetch, manual retry and stale focus return invalidate. Principal change purges facts. |
| Launch after preview | Server independently resolves/gates; PROVISIONAL states that launch can differ. No atomic protection against an unobserved remote state/cookie change is claimed. |

### Access, workspace and barrier matrix

| State | Required result |
|---|---|
| Workspace selection | Primary, extras, ephemeral scratch, create, mounts, read-only and drive remain. All selected sources participate in authorized access derivation. |
| Drive loading / unavailable / denied / allocated / read-only | Existing `nr-drive` / `nr-drive-reason` states and canonical facts; do not invent a drive root/path or silently drop drive selection. |
| Relevant access loading / unknown / failed | Unknown remains unknown; existing loading/Retry idiom. No inferred “connected” claim from an unanswered fetch. |
| Required access | “Required by {workspace}” or “Selected model provider”; remove by changing the origin selection. Do not delete an independent authored choice when a derived dependency goes away. |
| Source grant / explicit added access | “Source policy” or “Added by you”; existing supported controls. Inactive explicit selection is retained with INACTIVE_ACCESS. |
| Saved/default Access inspection | Read-only with Customize for this run before an authored change. No `PUT /policies/*` merely from viewing, navigating or customizing a run. |
| ADO repo selected | Existing capabilities/defaults/ceiling and connection org facts; one editable ADOCapabilitiesSection. Omitted or empty `azure_devops_capabilities` retains the provider row's default profile. An omitted or empty provider `default_profile` uses the existing default; `capability_ceiling` remains required and nonempty when its block is present. Preserve field presence in authored source without inventing an explicit-none meaning for these lists. |
| No ADO repo selected | ADO_WORKSPACE remedy to Workspace; do not configure unrelated repo access by pretending an ADO workspace exists. Retain explicit custom intent visibly inactive. |
| ADO granted / not connected / expired / blocked / minted PAT / retired org lane | Existing ADO/ADO_PAT copy and narrowing rules, note/summary/token-line IDs retained; no credentials or row IDs exposed through preview. |
| Git PAT | Existing secret pairing, hosts/repos/access/API narrowing and honesty note. Omitted vs empty repositories remain distinct; no automatic widening to source-policy ceiling. |
| SSH / conflicting PAT/SSH lanes | Existing supported SSH controls and conflicts; refuse incompatible lanes, never silently substitute a credential. |
| GitHub/direct/enterprise/non-GitHub | Server owns narrow authorized direct GitHub derivation; Access explains returned facts. No client rule grants GitHub hosts to every repository. Dispatch-only SCM/PAT/SSH/site-config hosts remain pending, not an exposed host list. |
| Barrier decided row / selectable classes / floor refusal / T-9 / unknown probe / no host barrier | Existing TierPicker, NO_BARRIER and confinement canon; selected/requested class remains distinct from actual run class. Remedy goes to Policy control or existing authorized setup destination. |
| Startup / tool rules / push rules / recording unknown/off/on | Existing source controls and conditional rail sections preserved; recording/autonomy unknown states do not get success assertions. |

### Model-provider R1–R9, preserved across picker relocation

| State | Run panel / Credentials rail |
|---|---|
| R1 single eligible candidate | Existing static provider line, no meaningless one-option picker; same provider body value and residency summary. |
| R2 several with admin default | Existing default preselected and OPTION strings, explicit override available. |
| R3 selected but not connected | Existing per-kind warning and door for AWS SSO, custom token, Anthropic key, OpenAI key and Claude subscription; no silent kind. Credential refusal/recovery behavior preserved. |
| R4 residency by kind | Existing proxy/sandbox credential sentences and Bedrock ownership chip; no blanket proxy claim. |
| R5b no grants | Actual server `providers_ungranted` fact names NOT_GRANTED and holds Launch; no guessed absence reason. |
| R5c disabled default | DEFAULT_OFF with alternatives and explicit choice, even one survivor; DEFAULT_OFF_ONLY with none. Disabled provider never selectable. |
| R6 several without default | Placeholder and LAUNCH_HINT until explicit pick; existing workspace-pin rule still owns contradictions. |
| R7 harness switch invalidates provider | Existing CHANGED sentence once; clears on deliberate next choice. |
| R8 provider serves next harness | Preserve choice silently; no false change announcement. |
| R9 no provider block / none serves harness | Existing legacy facts/warning, distinct from R5b. Shell/exec sends no model provider and shows no invented empty credential heading. |

`Segmented` uses ordinary Tab/Shift+Tab between pressed-state buttons; arrow keys are not required. Dirty chips stay `aria-hidden` so option names remain byte-exact. Modal escape/cancel stays quiet and returns focus to the opener. Copy announces only its existing transient result. Panel/view navigation never opens the unsaved dialog or resets source. Repeated data/status reads do not steal focus.

### Tests this touches

`regression-inventory.md` lists every baseline `/runs/new` e2e match, all current New Run unit files, three-editor/run-detail tests, explicit source anchors and preserved test IDs. Add shared `goToNewRunPanel(page, panel)` to `ui/e2e/fixtures.ts`, plus the shared unit helper, so specs select the owning panel without duplicating navigation. Demo specs use existing demo primitives and never import another spec. Update demo 05/06 narration with the explicit YAML label change.

Binary acceptance evidence: default/saved/custom launches and refusal paths; invalid source survives Run→Workspace→Policy; view switch preserves exact text/body/dirty state; redacted Customize seeds safe source; source editing preserves omitted/empty values, ADO policy capabilities and provider defaults retain their existing empty/absent equivalence, and PAT repositories and governance-overlay pointer fields retain their presence distinctions; preview out-of-order/429/freshness; actual `This run used`; all R states, drives/barriers/issues/doors; dirty leave and consumed Escape; dark/light/reduced motion; desktop 1280×650 on every panel (both existing reachability tests re-derived); narrow footer with wrapped errors, shell banner and renewal strip; full UI Playwright independently of `make ci`. Check bundle/size caps unchanged and YAML absent from eager entry. A mock walkthrough does not substitute for product tests.

## 5. Decisions for this independent approval

- **R-D1:** Run/Workspace/Access/Policy are freely navigable panels, using the drawn PanelNav with `aria-current="step"`. No completion lock or demo StepList reuse.
- **R-D2:** Continue is outline, Back ghost, Policy has no Continue; Enter never launches. Launch remains the one affirmative page action from every panel.
- **R-D3:** All nine rail sections remain in their current order and conditional states. Only provider selection moves to Run; the rail keeps the summary. Desktop and narrow decision blocks keep verdict/check/error/problem directly above Launch.
- **R-D4:** Task/Command autofocus; panel navigation focuses h2; issues focus controls; doors carry explicit return targets. ADO never autolaunches.
- **R-D5:** Existing UNSAVED copy and `useRequestLeave` handle Escape, Runs and page links; respect `defaultPrevented`. Navigation/view preference alone is not dirty.
- **R-D6:** Keep Summary canon; add JSON display; approve YAML as default explicit editing format and the declared Spec/validity/gate copy changes across all three editors and narration. JSON remains supported.
- **R-D7:** PolicyDocument stays read-only with real run facts, existing change marks and both redaction renderings. New Run requested class never masquerades as used class. One YAML display emitter remains.
- **R-D8:** The authored source string is the sole editable truth. Copy source preserves exact invalid/commented text; generated copies require current valid data. Comments remain session content and are lost at save/launch/conversion as stated.
- **R-D9:** Explicit YAML→JSON and custom replacement confirmations use the drawn exact copy. Saved/default inspection cannot mutate policies; redacted customization uses safe source.
- **R-D10:** Preview is authorized/provisional, with explicit pending/stale/invalid/429 states; Launch re-resolves independently. Existing gate sentences and announced-failure semantics stay byte-exact except the declared parser amendment.
- **R-D11:** Access composes existing supported catalogs with dependency reasons and each field's existing empty/absent semantics. No new generic API/MCP/runtime foundation, hidden-source exposure or duplicate ADO/PAT editor.
- **R-D12:** Default/saved extra workspaces are refused everywhere using M-F's exact copy. Custom retains every attachment. No automatic dropping or mode switch.
- **R-D13:** Hoist Segmented and remove the Policy-tab duplicate only after its recorded provenance check; preserve all original consumers and dirty-name semantics. `regression-inventory.md` records that provenance for the eventual implementation commit body.
- **R-D14:** Approval identifies this concrete design prototype URL/revision and these decisions, including narrow layout/focus and copy amendments. M-F and M-O remain independent. This textual packet alone does not satisfy the blocked remote-prototype gate.
- **R-D15 — new proposal:** Approve the strict-parser diagnostic inventory and variable-data treatment, `SOURCE_POSITION` copy, invalid-source frames and local announcement/focus/Copy/Summary/rail/preview rules above. This amendment needs independent written review and then the real design-prototype state walkthrough and owner decision before any diagnostic rendering is wired.

Owner approval record: **approved 2026-10-08, M-R Version 7**. Design prototype revision: `1791427781-730a`. Access rows are approved as direction only; their rules are superseded by the owner's 2026-10-07 component decisions, and no backend component design is approved.
