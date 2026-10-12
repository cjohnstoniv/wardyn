/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Step 5, Tools and access. The server's `access` block (the policy preview's,
// for the draft as it stands) says what the earlier choices bring and what the
// run can reach; this step shows it, finishes setup in place, and leaves
// anything unfinished to Review as a blocker.

import * as React from "react";
import { useModelAccessDoor, useShellSetupStatus } from "../../../../wardyn/model-access-context";
import { NR_SHARED } from "../../../../wardyn/copy/new-run/shared";
import { NR_TOOLS } from "../../../../wardyn/copy/new-run/tools";
import type { AccessFacts } from "../../../../../lib/types/new-run-access";
import { intentToWire } from "../../draft/intent-to-wire";
import { useDraftIdentity, useIntentChecks } from "../../draft/use-intent-checks";
import { useLocalIssues, useNewRunDraft, type NewRunScreenProps } from "../../new-run-route";
import { stepIssues } from "../../lenses/local-issues";
import { flowSteps, StepShell } from "../step-shell";
import { AddTools } from "./catalogue";
import { AlreadyIncluded } from "./already-included";
import { SetupNeeded } from "./setup-needed";
import { Totals } from "./totals";

const STEP_LABELS = {
  shape: NR_SHARED.STEP_SHAPE, run: NR_SHARED.STEP_RUN, harnesses: NR_SHARED.STEP_HARNESSES, files: NR_SHARED.STEP_FILES, tools: NR_SHARED.STEP_TOOLS,
};
const NO_AUTO_PREFLIGHT = { local: false, backendArm: false, modelArm: false };

/** The `access` block of the preview for this draft, and whether the server has answered exactly this body. */
function useAccess() {
  const draft = useNewRunDraft();
  const issues = useLocalIssues();
  const identity = useDraftIdentity();
  const door = useModelAccessDoor();
  const body = React.useMemo(() => (issues.length ? null : intentToWire(draft.intent).body), [issues.length, draft.intent]);
  const checks = useIntentChecks({
    body, selectionKey: "tools", identity, externalRevision: "0", sourceRefreshPending: false,
    autoCheck: NO_AUTO_PREFLIGHT, doorOpen: door.open, adoDoorOpen: false,
  });
  // While the next answer is out, the last one stays on screen with every host back at Requested.
  const last = React.useRef<AccessFacts>();
  const current = checks.preview.result?.access;
  if (current) last.current = current;
  return { access: current ?? last.current, answered: checks.preview.current && !!current, recheck: checks.invalidateChecks };
}

export function ToolsStep({ go }: NewRunScreenProps) {
  const draft = useNewRunDraft();
  const { access, answered, recheck } = useAccess();
  const { status } = useShellSetupStatus();
  const names = React.useMemo(() => new Map((status?.harnesses ?? []).map((h) => [h.id, h.display])), [status?.harnesses]);
  const [done, setDone] = React.useState<ReadonlySet<string>>(new Set());
  const setup = access?.setup;
  // A new answer settles what was finished: an item still missing comes back as itself.
  React.useEffect(() => setDone(new Set()), [setup]);

  const items = setup ?? [];
  const left = items.filter((i) => !done.has(i.id)).length;
  const finish = (id: string) => {
    setDone((old) => new Set(old).add(id));
    recheck();
  };

  return (
    <StepShell
      eyebrow={NR_SHARED.EYEBROW(5)}
      title={NR_TOOLS.TITLE}
      lede={NR_TOOLS.LEDE}
      progress={{ label: NR_SHARED.PROGRESS_ARIA, steps: flowSteps(STEP_LABELS, "tools") }}
      back={{ label: NR_SHARED.BACK, onBack: () => go({ screen: "step", step: "files" }) }}
      primary={{ label: NR_TOOLS.REVIEW_RUN, onActivate: () => go({ screen: "review", view: "pretty" }) }}
      issues={stepIssues("tools", draft.intent)}
    >
      <AlreadyIncluded access={access} intent={draft.intent} names={names} />
      {/* Harness setup and Add tools and access mount here. */}
      <div data-tools-slot="harness-setup" />
      <AddTools access={access} intent={draft.intent} />
      <Totals access={access} answered={answered} names={names} />
      <SetupNeeded items={items} done={done} intent={draft.intent} names={names} onDone={finish} />
      <p aria-live="polite" className="sticky bottom-0 bg-background py-2 text-sm text-muted-foreground">
        {access && (left === 0 ? NR_TOOLS.STICKY_DONE : left === 1 ? NR_TOOLS.STICKY_ONE : NR_TOOLS.STICKY_MANY(left))}
      </p>
    </StepShell>
  );
}
