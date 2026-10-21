/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Step 3, Harnesses: the server's roster as cards. An Interactive agent run
// starts in at most one harness; an Autonomous one holds one harness and its
// task; an Environment or Application takes optional harnesses and starts none.

import * as React from "react";
import { Button } from "../../../../ui/button";
import { NR_HARNESSES } from "../../../../wardyn/copy/new-run/harnesses";
import { NR_SHARED } from "../../../../wardyn/copy/new-run/shared";
import { Notice } from "../../../../wardyn/notice";
import { Chip } from "../../../../wardyn/primitives";
import type { RunIntent } from "../../draft/run-intent";
import { useNewRunDispatch, useNewRunDraft, type NewRunScreenProps } from "../../new-run-route";
import { addHarness, agentTask, clearStarter, removeHarness, setAgentTask, setStartPrompt, setStarter, swapHarness } from "../../lenses/harness-lens";
import { harnessIds } from "../../lenses/lens-keys";
import { modeOf } from "../../lenses/shape-lens";
import { stepIssues } from "../../lenses/local-issues";
import { flowSteps, StepShell, useAnnounce } from "../step-shell";
import { useLoaded } from "../shape/use-loaded";
import { HarnessCard, type HarnessMode } from "./harness-card";
import { combinationReason, loadRoster, unavailableReason, type Roster } from "./roster";

const STEP_LABELS = {
  shape: NR_SHARED.STEP_SHAPE, run: NR_SHARED.STEP_RUN, harnesses: NR_SHARED.STEP_HARNESSES, files: NR_SHARED.STEP_FILES, tools: NR_SHARED.STEP_TOOLS,
};
const LEDES: Record<HarnessMode, string> = {
  optional: NR_HARNESSES.LEDE_OPTIONAL,
  interactive: NR_HARNESSES.LEDE_INTERACTIVE,
  autonomous: NR_HARNESSES.LEDE_AUTONOMOUS,
};
const EMPTY: Roster = { harnesses: [], sets: [] };
const ADD_ID = "nr-harness";
const UNAVAILABLE_ID = "nr-harness-unavailable";

export function HarnessesStep({ go, deps }: NewRunScreenProps & { deps?: { load: () => Promise<Roster> } }) {
  const draft = useNewRunDraft();
  const dispatch = useNewRunDispatch();
  const announce = useAnnounce();
  const loaded = useLoaded(deps?.load ?? loadRoster);
  const roster = loaded.status === "ready" ? loaded.value : EMPTY;
  const { intent } = draft;
  const ids = harnessIds(intent);
  const mode: HarnessMode = intent.shape !== "agent_task" ? "optional" : intent.experience === "background" ? "autonomous" : "interactive";
  // Several agents side by side only in an Autonomous run, and only where the server has the switch on.
  const multi = mode === "autonomous" && roster.multiAgent === true;
  const edit = (apply: (i: RunIntent) => RunIntent) => dispatch({ type: "edit", apply });

  const taken = mode === "autonomous" ? [] : ids;
  const blocked = roster.harnesses.filter((h) => !ids.includes(h.id)).flatMap((h) => {
    const why = unavailableReason(h, taken, roster);
    return why ? [{ row: h, why }] : [];
  });
  const addable = roster.harnesses.find((h) => !ids.includes(h.id) && !unavailableReason(h, taken, roster));
  const comingSoon = mode === "autonomous" && ids.length > 0 && !multi;
  // A card may pick any harness; the ones that no image carries together are refused on the cards, and Continue holds.
  const combos = multi && loaded.status === "ready" ? ids.map((id) => combinationReason(roster, ids, id)) : [];
  const comboIssues = ids.flatMap((_, index) => (combos[index] ? [{ focus: `nr-harness-${index}` }] : []));
  // The Add button disables once nothing more can be added, so focus follows the new harness's select.
  const focusNew = React.useRef<number | null>(null);
  React.useEffect(() => {
    if (focusNew.current === null) return;
    document.getElementById(`nr-harness-${focusNew.current}`)?.focus();
    focusNew.current = null;
  }, [ids.length]);

  // Make gives a new Agent task one harness that starts the run (App.tsx:1258-1270). The roster's first addable
  // harness stands in for Make's fixed Claude Code (A14e). A template, a rerun or an import keeps what it carries.
  const seed = draft.origin.kind === "scratch" && intent.shape === "agent_task" && ids.length === 0 ? addable?.id : undefined;
  React.useEffect(() => {
    if (!seed) return;
    dispatch({ type: "edit", apply: (i) => (modeOf(i) === "interactive" ? setStarter(addHarness(i, seed), seed) : addHarness(i, seed)) });
  }, [seed, dispatch]);

  return (
    <StepShell
      eyebrow={NR_SHARED.EYEBROW(3)}
      title={NR_HARNESSES.TITLE}
      lede={multi ? NR_HARNESSES.LEDE_AUTONOMOUS_MULTI : LEDES[mode]}
      progress={{ label: NR_SHARED.PROGRESS_ARIA, steps: flowSteps(STEP_LABELS, "harnesses") }}
      back={{ label: NR_SHARED.BACK, onBack: () => go({ screen: "step", step: "run" }) }}
      primary={{ label: NR_SHARED.CONTINUE_TO(NR_SHARED.STEP_FILES), onActivate: () => go({ screen: "step", step: "files" }) }}
      issues={[...comboIssues.slice(0, 1), ...stepIssues("harnesses", intent)]}
    >
      {loaded.status === "error" && <p role="alert" className="text-sm text-danger">{loaded.message}</p>}
      {ids.length === 0 && mode === "optional" && (
        <div className="flex flex-col gap-1 rounded-lg border border-dashed border-border p-4 text-sm">
          <strong>{NR_HARNESSES.EMPTY_TITLE}</strong>
          <span className="text-muted-foreground">{NR_HARNESSES.EMPTY_BODY}</span>
        </div>
      )}
      {/* empty:hidden: with no cards the wrapper would still take a slot (and a second gap) in the step's column. */}
      <div className="flex flex-col gap-3 empty:hidden">
        {ids.map((id, index) => (
          <HarnessCard
            key={id}
            index={index}
            id={id}
            roster={roster}
            mode={mode}
            others={ids.filter((other) => other !== id)}
            removable={!(intent.shape === "agent_task" && ids.length === 1)}
            count={ids.length}
            combination={combos[index]}
            started={intent.startup?.kind === "harness" && intent.startup.tool === id}
            text={mode === "autonomous" ? agentTask(intent, id) : intent.startup?.tool === id ? intent.startup.prompt ?? "" : ""}
            onSwap={(to) => edit((i) => swapHarness(i, id, to))}
            onRemove={() => {
              edit((i) => removeHarness(i, id));
              announce(NR_HARNESSES.REMOVED);
              document.getElementById(ADD_ID)?.focus();
            }}
            onStart={(on) => edit((i) => (on ? setStarter(i, id) : clearStarter(i)))}
            onText={(text) => edit((i) => (mode === "autonomous" ? setAgentTask(i, id, text) : setStartPrompt(i, text)))}
          />
        ))}
      </div>
      {loaded.status === "loading" ? (
        <p role="status" className="text-sm text-muted-foreground">{NR_HARNESSES.LOADING}</p>
      ) : (
        <div className="flex items-center gap-3">
          <Button
            id={ADD_ID}
            type="button"
            variant="outline"
            disabled={comingSoon || !addable}
            aria-describedby={blocked.length ? UNAVAILABLE_ID : undefined}
            onClick={() => {
              if (!addable) return;
              focusNew.current = ids.length;
              edit((i) => addHarness(i, addable.id, multi));
              announce(NR_HARNESSES.ADDED);
            }}
          >
            {ids.length ? NR_HARNESSES.ADD_ANOTHER : NR_HARNESSES.ADD_HARNESS}
          </Button>
          {comingSoon && <Chip>{NR_SHARED.COMING_SOON}</Chip>}
        </div>
      )}
      {multi && ids.length > 1 && <Notice tone="info">{NR_HARNESSES.MULTI_NOTE}</Notice>}
      {blocked.length > 0 && (
        <ul id={UNAVAILABLE_ID} className="flex flex-col gap-1 text-sm">
          {blocked.map(({ row, why }) => (
            <li key={row.id}><strong>{row.display}</strong> <span className="text-muted-foreground">{why}</span></li>
          ))}
        </ul>
      )}
    </StepShell>
  );
}
