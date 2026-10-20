/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Step 2, Run: how the agent runs, what an application runs, and where. The pool and its barrier come from
// the server's pool list for the run type; the draft keeps a pool or a barrier only once the person chose one.

import * as React from "react";
import { NR_RUN } from "../../../../wardyn/copy/new-run/run";
import { NR_SHARED } from "../../../../wardyn/copy/new-run/shared";
import type { RunnerPoolRunType } from "../../../../../lib/types/runner-pools";
import { useNewRunDispatch, useNewRunDraft, type NewRunScreenProps } from "../../new-run-route";
import type { RunIntent } from "../../draft/run-intent";
import { stepIssues } from "../../lenses/local-issues";
import { addApp, appsOf, chooseAppKind, commandOf, editApp, isServer, removeApp, setCommand } from "../../lenses/apps-lens";
import { chooseBarrier, chooseMode, choosePool } from "../../lenses/run-lens";
import { modeOf, type RunMode } from "../../lenses/shape-lens";
import { flowSteps, StepShell } from "../step-shell";
import { BarrierSection } from "./barrier-section";
import { CommandSection } from "./command-section";
import { ModeSection } from "./mode-section";
import { ledeOf, selectedPoolId, usablePools } from "./pool-model";
import { PoolSection } from "./pool-section";
import { QuestionBlock } from "./question-block";
import { loadPools, usePools, type LoadPools } from "./pool-source";
import { TimingSection } from "./timing-section";

const STEP_LABELS = {
  shape: NR_SHARED.STEP_SHAPE, run: NR_SHARED.STEP_RUN, harnesses: NR_SHARED.STEP_HARNESSES, files: NR_SHARED.STEP_FILES, tools: NR_SHARED.STEP_TOOLS,
};
const TITLES = { agent_task: NR_RUN.TITLE_AGENT, environment: NR_RUN.TITLE_ENV, application: NR_RUN.TITLE_APP } as const;
const titleOf = (intent: RunIntent) => (isServer(intent) ? NR_RUN.TITLE_APP_SERVER : intent.shape ? TITLES[intent.shape] : NR_RUN.TITLE_AGENT);

export function RunStep({ go, deps }: NewRunScreenProps & { deps?: { load: LoadPools } }) {
  const { intent } = useNewRunDraft();
  const dispatch = useNewRunDispatch();
  const edit = (apply: (i: RunIntent) => RunIntent) => dispatch({ type: "edit", apply });
  const agent = intent.shape === "agent_task";
  // The server never infers a mode, so an agent run that has not chosen one gets one and the draft says so. A task
  // already in the draft (the Runs composer's) is the Autonomous carrier, so the draft is named Autonomous and keeps it.
  const unnamed = agent && intent.experience === undefined;
  const fallback: RunMode = intent.workload?.kind === "agent_task" ? "autonomous" : "interactive";
  React.useEffect(() => {
    if (unnamed) dispatch({ type: "edit", apply: (i) => chooseMode(i, fallback) });
  }, [unnamed, fallback, dispatch]);
  const runType: RunnerPoolRunType | undefined = intent.experience ?? (unnamed ? (fallback === "autonomous" ? "background" : "interactive") : undefined);

  const pools = usePools(deps?.load ?? loadPools, runType);
  const list = pools.status === "ready" ? pools.list : undefined;
  const chosen = list && usablePools(list).find((p) => p.id === selectedPoolId(intent, list));

  return (
    <StepShell
      eyebrow={NR_SHARED.EYEBROW(2)}
      title={titleOf(intent)}
      lede={ledeOf(list)}
      progress={{ label: NR_SHARED.PROGRESS_ARIA, steps: flowSteps(STEP_LABELS, "run") }}
      back={{ label: NR_SHARED.BACK, onBack: () => go({ screen: "step", step: "shape" }) }}
      primary={{ label: NR_SHARED.CONTINUE_TO(NR_SHARED.STEP_HARNESSES), onActivate: () => go({ screen: "step", step: "harnesses" }) }}
      issues={stepIssues("run", intent, { poolDefault: list ? !!list.default?.pool_id : undefined })}
    >
      {agent && <ModeSection mode={modeOf(intent) ?? fallback} onChange={(mode) => edit((i) => chooseMode(i, mode))} />}
      {agent && modeOf(intent) === "autonomous" && <TimingSection />}
      {intent.shape === "application" && (
        <CommandSection
          kind={isServer(intent) ? "server" : "automation"}
          command={commandOf(intent) ?? ""}
          apps={appsOf(intent)}
          onKind={(kind) => edit((i) => chooseAppKind(i, kind))}
          onCommand={(command) => edit((i) => setCommand(i, command))}
          onApps={{ edit: (n, patch) => edit((i) => editApp(i, n, patch)), add: () => edit(addApp), remove: (n) => edit((i) => removeApp(i, n)) }}
        />
      )}
      <QuestionBlock label={NR_RUN.WHERE_LABEL}>
        <PoolSection state={pools} runType={runType} intent={intent} onChoose={(id) => edit((i) => choosePool(i, id))} />
        <BarrierSection pool={chosen} chosen={intent.confinement_class} onChoose={(cc) => edit((i) => chooseBarrier(i, cc))} />
      </QuestionBlock>
    </StepShell>
  );
}
