/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Step 5, Tools and access. The server's `access` block (the policy preview's,
// for the draft as it stands) says what the earlier choices bring and what the
// run can reach; this step shows it, finishes setup in place, and leaves
// anything unfinished to Review as a blocker.

import * as React from "react";
import { Button } from "../../../../ui/button";
import { useModelAccessDoor, useShellSetupStatus } from "../../../../wardyn/model-access-context";
import { NR_SHARED } from "../../../../wardyn/copy/new-run/shared";
import { NR_TOOLS } from "../../../../wardyn/copy/new-run/tools";
import type { AccessFacts } from "../../../../../lib/types/new-run-access";
import type { HarnessSetFact } from "../../../../../lib/types/new-run-contract";
import { intentToWire } from "../../draft/intent-to-wire";
import { useDraftIdentity, useIntentChecks } from "../../draft/use-intent-checks";
import { useLocalIssues, useNewRunDraft, type NewRunScreenProps } from "../../new-run-route";
import { stepIssues } from "../../lenses/local-issues";
import { needsSetup } from "../../lenses/tools-lens";
import { flowSteps, StepShell } from "../step-shell";
import { AddTools } from "./catalogue";
import { AlreadyIncluded } from "./already-included";
import { HarnessSetup, providerNameFor } from "./harness-setup";
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
  const last = React.useRef<{ access: AccessFacts; harnessSet?: HarnessSetFact } | undefined>(undefined);
  const current = checks.preview.result?.access;
  if (current) last.current = { access: current, harnessSet: checks.preview.result?.harness_set };
  return { access: current ?? last.current?.access, harnessSet: last.current?.harnessSet, answered: checks.preview.current && !!current, recheck: checks.invalidateChecks };
}

export function ToolsStep({ go }: NewRunScreenProps) {
  const draft = useNewRunDraft();
  const { access, harnessSet, answered, recheck } = useAccess();
  const { status } = useShellSetupStatus();
  const names = React.useMemo(() => new Map((status?.harnesses ?? []).map((h) => [h.id, h.display])), [status?.harnesses]);
  const providers = new Map((access?.harnesses ?? []).flatMap((f) => {
    const name = providerNameFor(f.harness, draft.intent, harnessSet, status);
    return name ? [[f.harness, name] as const] : [];
  }));
  // Items finished here wait for the server's next answer: only that answer settles them.
  const [finished, setFinished] = React.useState<{ ids: ReadonlySet<string>; base: AccessFacts | undefined }>();
  const waiting = !!finished && (!answered || access === finished.base);
  const setup = access?.setup ?? [];
  const missing = setup.filter(needsSetup);
  const settled = !finished || waiting ? new Set<string>() : new Set(setup.filter((i) => i.status === "satisfied" && finished.ids.has(i.id)).map((i) => i.id));
  const items = [...missing, ...setup.filter((i) => settled.has(i.id))];
  const pending = waiting ? finished.ids : new Set<string>();
  const finish = (id: string) => {
    setFinished((old) => ({ ids: new Set(old?.ids).add(id), base: access }));
    recheck();
  };

  return (
    <StepShell
      eyebrow={NR_SHARED.EYEBROW(5)}
      title={NR_TOOLS.TITLE}
      lede={NR_TOOLS.LEDE}
      progress={{ label: NR_SHARED.PROGRESS_ARIA, steps: flowSteps(STEP_LABELS, "tools") }}
      primary={{ label: NR_TOOLS.REVIEW_RUN, onActivate: () => go({ screen: "review", view: "pretty" }) }}
      issues={stepIssues("tools", draft.intent)}
      stickyFooter
      // The prototype's bar: Back, how many items still need setup, then Review run. Footer extras stand in for Back, so Back is passed here.
      footerExtras={{
        start: <Button type="button" variant="ghost" onClick={() => go({ screen: "step", step: "files" })}>{NR_SHARED.BACK}</Button>,
        end: (
          <span aria-live="polite" className="self-center text-sm text-muted-foreground">
            {access && !waiting && (missing.length === 0 ? NR_TOOLS.STICKY_DONE : missing.length === 1 ? NR_TOOLS.STICKY_ONE : NR_TOOLS.STICKY_MANY(missing.length))}
          </span>
        ),
      }}
    >
      <AlreadyIncluded access={access} answered={answered} intent={draft.intent} names={names} providers={providers} />
      <HarnessSetup access={access} harnessSet={harnessSet} intent={draft.intent} answered={answered} names={names} onSignedIn={recheck} />
      <AddTools access={access} answered={answered} intent={draft.intent} />
      <Totals access={access} answered={answered} names={names} providers={providers} />
      <SetupNeeded items={items} pending={pending} done={settled} intent={draft.intent} names={names} onDone={finish} />
    </StepShell>
  );
}
