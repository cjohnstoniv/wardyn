# Regression inventory for M-F / M-R / M-O

Generated from baseline `7b08fd722ca4dcfd9d2d59e6f1cb8ecab54d8dcf` on 2026-10-07. This is an implementation/check inventory, not evidence that the redesigned UI or a prototype has passed. No existing tests were removed or modified.

## Relocated and preserved anchors

| ID / accessible name | Destination | Contract |
|---|---|---|
| nr-title; Title | Run → Run details | Preserve input ID/name; disclosure opens before issue focus. |
| nr-task; Task / Command | Run | Primary autofocus by mode; Enter never launches. |
| nr-model-provider; Model provider | Run | RAIL_PROVIDER.LABEL unchanged; persistent door return target. |
| nr-workspace; Workspace | Workspace | Same selector ID and accessible name. |
| Ephemeral scratch — no repo | Workspace | Same selector option. |
| nr-workspace-extras; Remove {name} | Workspace | Retain attached workspaces in every mode. |
| nr-drive; nr-drive-reason | Workspace | Retain drive controls, reasons and states. |
| ado-access-summary | Access; Policies existing summary | Preserve ID and explicit-capability summary semantics. |
| ado-run-token-line; ado-launch-note | Access | Preserve IDs, minted-token note, status/alert roles and connect behavior. |
| ado-capability-card | Run detail Approvals | Preserved there; not incorrectly moved from a New Run mount that does not exist. |
| git-pat-block; git-pat-honesty | Access | Same narrowing controls and honesty text; one editable source. |
| Saved policy | Policy | Same picker accessible name. |
| Use the default policy; Reuse a saved policy; Custom policy | Policy | All three labels and pressed semantics preserved. |
| run-spec-additions | Policy / combined run preview | Retain source-versus-derived explanation and marker where applicable. |
| Spec (JSON) | Policy / explicit JSON; Policies; Governance | Preserve when JSON explicitly selected. YAML default is a declared copy amendment. |
| Spec (YAML) | Same three editors | New default label, tested explicitly alongside JSON. |
| preflight-check-state; preflight-result | Rail decision block, desktop/narrow | Same IDs and meaningful states; one active DOM instance. |
| Launch run | Rail decision block, every panel | Same accessible name; two 1280×650 pins are re-derived. |
| tab-dirty-chip-{value} | Hoisted Segmented consumers | Keep aria-hidden so option accessible names do not change. |
| run-output-text; run-output-refusal | Run detail Output, M-O | Preserve source region, Copy and mapped refusal semantics. |

## Shared navigation helper

Add `goToNewRunPanel(page, panel)` in `ui/e2e/fixtures.ts`, using PanelNav’s accessible buttons and verifying the target h2/current step. Add the equivalent shared unit helper for full-screen tests. Preserve semantic assertions after navigation. Specs must not import other specs; demo flows use `demo/{stage,overlay,narrator,funnel,task,sweep}.ts`.

Both current `new-run.spec.ts` 1280×650 Launch reachability cases are re-derived for every panel with the member ceiling/tool-rule/warning and credential variants. Add narrow layout with expanded/collapsed rail, wrapped refusals, shell banner, renewal strip, safe-area padding, dark/light and reduced motion. A hidden duplicate Launch control does not pass reachability or focus checks.

## Baseline New Run e2e census (39 files)

Command: `rg -l "runs/new|New run" ui/e2e -g "*.spec.ts"` (exit 0). Grouped by location; files can cover more than one area.

### Console and doors

- `ui/e2e/admin-run-monitor.spec.ts`
- `ui/e2e/ado-connect.spec.ts`
- `ui/e2e/ado-launch-door.spec.ts`
- `ui/e2e/agents.spec.ts`
- `ui/e2e/available-to-person.spec.ts`
- `ui/e2e/available-to.spec.ts`
- `ui/e2e/confinement-posture.spec.ts`
- `ui/e2e/drives.spec.ts`
- `ui/e2e/governance-member.spec.ts`
- `ui/e2e/governance.spec.ts`
- `ui/e2e/member-console.spec.ts`
- `ui/e2e/model-access-banner.spec.ts`
- `ui/e2e/navigation.spec.ts`
- `ui/e2e/new-run-provider-picker.spec.ts`
- `ui/e2e/new-run.spec.ts`
- `ui/e2e/one-door.spec.ts`
- `ui/e2e/policy-ado-capabilities.spec.ts`
- `ui/e2e/refusal-doors.spec.ts`
- `ui/e2e/runs-detail.spec.ts`
- `ui/e2e/runs-header.spec.ts`
- `ui/e2e/runs.spec.ts`
- `ui/e2e/setup-gate.spec.ts`
- `ui/e2e/view-routing.spec.ts`
- `ui/e2e/view-switch.spec.ts`

### Walks

- `ui/e2e/walk/sso-concurrency.spec.ts`
- `ui/e2e/walk/sso-member-recovery.spec.ts`
- `ui/e2e/walk/sso-member.spec.ts`
- `ui/e2e/walk/sso-reauth-hold.spec.ts`
- `ui/e2e/walk/terminal-wheel.spec.ts`

### Demos and narration

- `ui/e2e/demo/00-meet-wardyn.spec.ts`
- `ui/e2e/demo/04c-who-may-do-what.spec.ts`
- `ui/e2e/demo/04d-your-drive.spec.ts`
- `ui/e2e/demo/05-your-first-policy.spec.ts`
- `ui/e2e/demo/06-your-first-run.spec.ts`
- `ui/e2e/demo/07-interactive-runs.spec.ts`
- `ui/e2e/demo/08-autonomous-agent.spec.ts`
- `ui/e2e/demo/10-approvals-and-egress.spec.ts`
- `ui/e2e/demo/12b-admin-operations.spec.ts`
- `ui/e2e/demo/walkthrough.spec.ts`

## Baseline New Run unit census (27 files)

Command: `rg --files ui/src/app/components/screens/new-run -g "*test*"` (exit 0). Preserve their semantic coverage through mechanical splits and navigation updates.

- `ui/src/app/components/screens/new-run/agent-picker.test.tsx`
- `ui/src/app/components/screens/new-run/model-provider-lane.test.ts`
- `ui/src/app/components/screens/new-run/new-run-launch-panel.test.tsx`
- `ui/src/app/components/screens/new-run/new-run-rail-alerts.test.tsx`
- `ui/src/app/components/screens/new-run/new-run-rail-launch-door.test.tsx`
- `ui/src/app/components/screens/new-run/new-run-rail-policy-remedy.test.tsx`
- `ui/src/app/components/screens/new-run/new-run-rail-preflight-door.test.tsx`
- `ui/src/app/components/screens/new-run/new-run-rail-provider-door.test.tsx`
- `ui/src/app/components/screens/new-run/new-run-rail-provider.test.tsx`
- `ui/src/app/components/screens/new-run/new-run-rail-push.test.tsx`
- `ui/src/app/components/screens/new-run/new-run-rail-quota.test.tsx`
- `ui/src/app/components/screens/new-run/new-run-rail.test.tsx`
- `ui/src/app/components/screens/new-run/new-run-screen-barrier.test.tsx`
- `ui/src/app/components/screens/new-run/new-run-screen-default-policy.test.tsx`
- `ui/src/app/components/screens/new-run/new-run-screen-form.test.tsx`
- `ui/src/app/components/screens/new-run/new-run-screen-launch.test.tsx`
- `ui/src/app/components/screens/new-run/new-run-screen-model-provider.test.tsx`
- `ui/src/app/components/screens/new-run/new-run-screen-saved-policy.test.tsx`
- `ui/src/app/components/screens/new-run/new-run-screen-setup-rows.test.tsx`
- `ui/src/app/components/screens/new-run/policy-lane.test.ts`
- `ui/src/app/components/screens/new-run/use-ado-launch-door.test.ts`
- `ui/src/app/components/screens/new-run/use-launch.test.tsx`
- `ui/src/app/components/screens/new-run/wizard-spec-command-agent.test.ts`
- `ui/src/app/components/screens/new-run/wizard-spec-run.test.ts`
- `ui/src/app/components/screens/new-run/wizard-spec.test.ts`
- `ui/src/app/components/screens/new-run/wizard-types.test.ts`
- `ui/src/app/components/screens/new-run/workspace-card.test.tsx`

## Other directly affected families

- `ui/src/app/components/wardyn/policy-panel.test.tsx`
- `ui/src/app/components/screens/policies.test.tsx`
- `ui/src/app/components/screens/policies-availability.test.tsx`
- `ui/src/app/components/screens/governance/profile-editor.test.tsx`
- `ui/src/app/components/screens/run-detail/policy-tab.test.tsx`
- `ui/src/app/components/screens/run-detail/policy-tab-copy.test.ts`
- `ui/src/app/components/wardyn/policy-ado-capabilities.test.tsx`
- `ui/src/app/components/wardyn/ado-access-summary.test.tsx`
- `ui/src/app/components/wardyn/ado-run-token-line.test.tsx`
- `ui/src/app/components/wardyn/reauth-layer.test.tsx`
- `ui/src/app/App.reauth.test.tsx`
- `ui/src/app/lib/api/run-sign-in.test.ts`
- `ui/src/app/components/screens/run-detail/output-tab.test.tsx`
- `ui/src/app/bundle-split.test.ts`
- `ui/e2e/policies.spec.ts`
- `ui/e2e/reauth-in-place.spec.ts`
- `ui/e2e/run-output.spec.ts`

## Existing Spec (JSON) test anchors (20 files)

Command: `rg -l -F "Spec (JSON)" ui/e2e ui/src -g "*.spec.ts" -g "*.test.ts" -g "*.test.tsx"` (exit 0). YAML default is an explicit amendment; retain the JSON anchors as explicit-JSON cases and update demo 05/06 narration.

- `ui/e2e/available-to.spec.ts`
- `ui/e2e/demo/00-meet-wardyn.spec.ts`
- `ui/e2e/demo/04c-who-may-do-what.spec.ts`
- `ui/e2e/demo/04d-your-drive.spec.ts`
- `ui/e2e/demo/05-your-first-policy.spec.ts`
- `ui/e2e/demo/07-interactive-runs.spec.ts`
- `ui/e2e/demo/08-autonomous-agent.spec.ts`
- `ui/e2e/demo/10-approvals-and-egress.spec.ts`
- `ui/e2e/demo/12b-admin-operations.spec.ts`
- `ui/e2e/demo/walkthrough.spec.ts`
- `ui/e2e/governance.spec.ts`
- `ui/e2e/new-run.spec.ts`
- `ui/e2e/policies.spec.ts`
- `ui/e2e/policy-ado-capabilities.spec.ts`
- `ui/src/app/components/screens/governance/profile-compose.test.tsx`
- `ui/src/app/components/screens/governance/profile-editor.test.tsx`
- `ui/src/app/components/screens/new-run/new-run-screen-default-policy.test.tsx`
- `ui/src/app/components/screens/new-run/new-run-screen-form.test.tsx`
- `ui/src/app/components/screens/new-run/new-run-screen-launch.test.tsx`
- `ui/src/app/components/wardyn/policy-panel.test.tsx`

## Existing invalid-JSON sentence sites

Command: `rg -l -F "isn't valid JSON" ui/src ui/e2e` (exit 0). Replace only the declared source gate with the approved YAML-or-JSON sentence and update matching anchors.

- `ui/e2e/new-run.spec.ts`
- `ui/src/app/components/screens/new-run/new-run-launch-gates.ts`
- `ui/src/app/components/screens/new-run/new-run-screen-default-policy.test.tsx`
- `ui/src/app/components/screens/new-run/use-launch.ts`

## Segmented / Policy-tab provenance

`git log -S "function Segmented" -- ui/src/app/components/screens/permissions.tsx` identifies `4b29ac93e30b3fb08721bbf6493e5f217c88f2d5`. Its commit body explains the reviewed Permissions page and canonical-copy constraints. Current Segmented comments document real consumers and the #460 dirty chip: the chip is visual only and must not change the option’s accessible name. Hoisting must preserve this behavior.

`git log -S "onMode(m)" -- ui/src/app/components/screens/run-detail/policy-tab.tsx` identifies `9671137178cb3779a985a0e13759eb4d23ea09e9`. Its commit body introduced read-only policy provenance/change groups, Summary/YAML and Copy YAML with member Hidden rendering. Deleting its local switcher is justified only by replacing it with the existing shared Segmented while retaining those contracts.

The future implementation commit must record these two introducing commits in its body. This packet deletes no code.

## Required completion evidence

Appropriate targeted Vitest, three-editor/source tests, real production bundle split assertion, unchanged file-size cap, `scripts/lane-preflight.sh`, full `make ci`, full `go test ./...` (including cmd guards) and `scripts/run-ui-e2e.sh` separately. Real PostgreSQL and kind cases, including recording-on Kubernetes output, run separately. Report actual commands, environment, exit codes and skips; this inventory does not mark them run.
