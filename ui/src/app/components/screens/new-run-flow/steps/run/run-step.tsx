/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Step 2, Run: how the agent runs, what an application runs, and where. The pool and its barrier come from
// the server's pool list for the run type; the draft keeps a pool or a barrier only once the person chose one.

import * as React from "react";
import { NR_RUN } from "../../../../wardyn/copy/new-run/run";
import { NR_SHARED } from "../../../../wardyn/copy/new-run/shared";
import { isVMKind } from "../../../../../lib/types/machine-kind";
import { useNewRunDispatch, useNewRunDraft, type NewRunScreenProps } from "../../new-run-route";
import type { RunIntent } from "../../draft/run-intent";
import { harnessIds } from "../../lenses/lens-keys";
import { stepIssues } from "../../lenses/local-issues";
import { keepRunning, mayKeepRunning, setHealth, setPort, setRestart } from "../../lenses/service-lens";
import { addApp, appsOf, chooseAppKind, commandOf, editApp, isServer, removeApp, setCommand } from "../../lenses/apps-lens";
import type { MachineKind } from "../../../../../lib/types/machine-kind";
import { dropMinimum, minimumFill, startAtMinimum } from "../../lenses/limits-lens";
import { chooseBarrier, chooseMacOSBase, chooseMachineKind, chooseMode, choosePool } from "../../lenses/run-lens";
import { modeOf, type RunMode } from "../../lenses/shape-lens";
import { flowSteps, StepShell } from "../step-shell";
import { BarrierSection } from "./barrier-section";
import { CommandSection } from "./command-section";
import { MachineKindSection } from "./machine-kind-section";
import { useMacOSOffer } from "./macos-offer";
import { ModeSection } from "./mode-section";
import { ledeOf, runTypeOf, selectedPoolId, usablePools } from "./pool-model";
import { PoolSection } from "./pool-section";
import { ServiceSection } from "./service-section";
import { useRunPreview } from "./use-run-preview";
import { QuestionBlock } from "./question-block";
import { loadPools, usePools, type LoadPools } from "./pool-source";
import { TimingSection } from "./timing-section";

const STEP_LABELS = {
  shape: NR_SHARED.STEP_SHAPE, run: NR_SHARED.STEP_RUN, harnesses: NR_SHARED.STEP_HARNESSES, files: NR_SHARED.STEP_FILES, tools: NR_SHARED.STEP_TOOLS,
};
const TITLES = { agent_task: NR_RUN.TITLE_AGENT, environment: NR_RUN.TITLE_ENV, application: NR_RUN.TITLE_APP } as const;
const titleOf = (intent: RunIntent) => (isServer(intent) ? NR_RUN.TITLE_APP_SERVER : intent.shape ? TITLES[intent.shape] : NR_RUN.TITLE_AGENT);

export function RunStep({ go, deps }: NewRunScreenProps & { deps?: { load: LoadPools } }) {
  const draft = useNewRunDraft();
  const { intent } = draft;
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
  const runType = runTypeOf(intent);
  const { pauses, inboundRefused } = useRunPreview(intent);

  const pools = usePools(deps?.load ?? loadPools, runType);
  const list = pools.status === "ready" ? pools.list : undefined;
  const chosen = list && usablePools(list).find((p) => p.id === selectedPoolId(intent, list));
  // A VM is its own barrier: the container barriers give way to the VM's fixed line.
  const vm = isVMKind(intent.machine_kind);
  // A macOS VM starts its CPU and memory ask at the base's minimum; leaving macOS takes that back.
  const offer = useMacOSOffer();
  const minimum = offer?.ok ? { cpuMillis: offer.minCPUs * 1000, memoryMib: offer.minMemoryMiB } : undefined;
  const fill = draft.guided.macosFill;
  const chooseKind = (kind: MachineKind) => {
    const onMac = intent.machine_kind === "macos_vm";
    if (kind === "macos_vm" && !onMac && minimum) dispatch({ type: "guided", patch: { macosFill: minimumFill(intent, minimum) } });
    else if (kind !== "macos_vm" && fill) dispatch({ type: "guided", patch: { macosFill: undefined } });
    edit((i) => {
      const leaving = i.machine_kind === "macos_vm" && kind !== "macos_vm" ? dropMinimum(i, fill) : i;
      const next = chooseMachineKind(leaving, kind);
      return kind === "macos_vm" && minimum ? startAtMinimum(next, minimum) : next;
    });
  };

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
      {agent && modeOf(intent) === "autonomous" && <TimingSection schedule={draft.schedule} onSchedule={(schedule) => dispatch({ type: "schedule", schedule })} />}
      {intent.shape === "application" && (
        <CommandSection
          kind={isServer(intent) ? "server" : "automation"}
          command={commandOf(intent) ?? ""}
          apps={appsOf(intent)}
          keptRunning={!!intent.service}
          inboundRefused={inboundRefused}
          onKind={(kind) => edit((i) => chooseAppKind(i, kind))}
          onCommand={(command) => edit((i) => setCommand(i, command))}
          onApps={{ edit: (n, patch) => edit((i) => editApp(i, n, patch)), add: () => edit(addApp), remove: (n) => edit((i) => removeApp(i, n)) }}
        >
          {mayKeepRunning(intent) && (
            <ServiceSection
              service={intent.service}
              onKeepRunning={(on) => edit((i) => keepRunning(i, on))}
              onRestart={(restart) => edit((i) => setRestart(i, restart))}
              onPort={(port) => edit((i) => setPort(i, port))}
              onHealth={(patch) => edit((i) => setHealth(i, patch))}
              pauses={pauses}
            />
          )}
        </CommandSection>
      )}
      <QuestionBlock label={NR_RUN.WHERE_LABEL}>
        <MachineKindSection kind={intent.machine_kind ?? "container"} agents={harnessIds(intent)} macosBase={intent.macos_base} onChoose={chooseKind} onChooseBase={(base) => edit((i) => chooseMacOSBase(i, base))} />
        <PoolSection state={pools} runType={runType} intent={intent} onChoose={(id) => edit((i) => choosePool(i, id))} />
        {!vm && <BarrierSection pool={chosen} chosen={intent.confinement_class} onChoose={(cc) => edit((i) => chooseBarrier(i, cc))} />}
      </QuestionBlock>
    </StepShell>
  );
}
