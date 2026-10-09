/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The Run panel (#1922): what this run is called, then what it runs. Run
// details come first and are always on screen — a title is required, and it is
// the one thing that makes a run findable a week from now.
import * as React from "react";
import type { SetupModelProvider } from "../../../lib/types";
import { Input } from "../../ui/input";
import { Textarea } from "../../ui/textarea";
import { Field } from "../../wardyn/form-primitives";
import { SectionLabel } from "../../wardyn/primitives";
import { RAIL_PROVIDER } from "../../wardyn/copy";
import { NEW_RUN_FLOW } from "../../wardyn/copy/new-run-flow";
import { useModelAccessDoor } from "../../wardyn/model-access-context";
import { ISSUE_LINE_ID, ISSUE_TARGET, type LaunchIssue } from "./new-run-launch-gates";
import { ModelProviderSection } from "./new-run-rail-credentials";
import type { RunRailProps } from "./new-run-rail-types";
import { WhatToRunStep, type WhatToRunStepProps } from "./step-bodies";

// The run's model provider: the picker, what the person provides for the one
// picked, and the door to connect it. Selection is the screen's; this renders
// and asks.
export function RunProviderPicker({ modelProvider }: { modelProvider: NonNullable<RunRailProps["modelProvider"]> }) {
  const door = useModelAccessDoor();
  const wrapper = React.useRef<HTMLDivElement>(null);
  // Where focus goes when the door closes. The control that opened it unmounts
  // the moment a sign-in completes, so the door is handed this wrapper, which
  // outlives both outcomes and passes focus on: back to that control after a
  // cancellation, to the picker after a completed sign-in.
  const opened = React.useRef<{ opener: Element | null; done: boolean } | null>(null);
  const onSignIn = (p: SetupModelProvider) => {
    const state = { opener: document.activeElement, done: false };
    opened.current = state;
    door.openDoor({ for: { provider: p.id }, returnTo: wrapper.current, onSignedIn: () => void (state.done = true) });
  };
  const onFocus = (e: React.FocusEvent<HTMLDivElement>) => {
    if (e.target !== e.currentTarget) return;
    const from = opened.current;
    opened.current = null;
    const opener = from && !from.done && from.opener instanceof HTMLElement && from.opener.isConnected ? from.opener : null;
    (opener ?? e.currentTarget.querySelector<HTMLElement>(`#${ISSUE_TARGET.PROVIDER_PICKER}`))?.focus();
  };
  return (
    <div id={ISSUE_TARGET.PROVIDER} ref={wrapper} tabIndex={-1} role="group" aria-label={RAIL_PROVIDER.LABEL} onFocus={onFocus}>
      <ModelProviderSection
        candidates={modelProvider.candidates}
        access={modelProvider.access}
        selectedId={modelProvider.selectedId}
        onChange={modelProvider.onChange}
        changeNote={modelProvider.changeNote}
        onSignIn={onSignIn}
        gate={modelProvider.gate}
        harnessLabel={modelProvider.harnessLabel}
      />
    </div>
  );
}

export interface RunPanelProps extends Omit<WhatToRunStepProps, "provider" | "taskInvalid" | "taskDescribedBy"> {
  setTitleUserEdited: (edited: boolean) => void;
  knownTitles: string[];
  /** The picker's own props, or undefined when no provider serves this run
   *  shape (a shell command, or none for this agent). */
  modelProvider: RunRailProps["modelProvider"];
  /** Every reason Launch is held, and the one the line above Launch names. */
  issues: LaunchIssue[];
  shown: LaunchIssue | null;
}

export function RunPanel({ setTitleUserEdited, knownTitles, modelProvider, issues, shown, ...what }: RunPanelProps) {
  const { state, patch } = what;
  const detailsId = React.useId();
  const owns = (focus: string) => issues.some((i) => i.focus === focus);
  const describedBy = (focus: string) => (shown?.focus === focus ? ISSUE_LINE_ID : undefined);
  const hasPicker = !!modelProvider && (modelProvider.candidates.length > 0 || !!modelProvider.gate);
  return (
    <div className="space-y-4">
      <div role="group" aria-labelledby={detailsId} className="space-y-3 border-b border-border pb-4">
        <div id={detailsId}>
          <SectionLabel>{NEW_RUN_FLOW.RUN_DETAILS}</SectionLabel>
        </div>
        <p className="text-xs text-muted-foreground">{NEW_RUN_FLOW.RUN_DETAILS_NOTE}</p>
        <Field label="Title" htmlFor={ISSUE_TARGET.TITLE} required>
          <Input
            id={ISSUE_TARGET.TITLE}
            required
            // Rulebook §8: default focus lands on the first required field.
            autoFocus
            // NO Enter-to-launch: this input carries the datalist below, and
            // Chrome dispatches keydown Enter when a suggestion is picked,
            // which would LAUNCH the run. Launch is the rail's button only.
            maxLength={200}
            // Native datalist: existing titles are offered as you type, so
            // joining a family is a pick, not an exact retype.
            list="nr-known-titles"
            placeholder="Refactor the payments module"
            value={state.title}
            // #1197 L2: any edit — including clearing it — turns off the
            // task-derived default for the rest of this session.
            onChange={(e) => {
              setTitleUserEdited(true);
              patch({ title: e.target.value });
            }}
            aria-invalid={owns(ISSUE_TARGET.TITLE) || undefined}
            aria-describedby={describedBy(ISSUE_TARGET.TITLE)}
          />
        </Field>
        <datalist id="nr-known-titles">
          {knownTitles.map((t) => (
            <option key={t} value={t} />
          ))}
        </datalist>
        <Field label="Description" htmlFor="nr-description" hint="Optional. Why this run exists — for whoever reads it later.">
          <Textarea
            id="nr-description"
            rows={2}
            maxLength={2000}
            placeholder="Ticket 4412 — the refund path double-charges on retry."
            value={state.description}
            onChange={(e) => patch({ description: e.target.value })}
          />
        </Field>
      </div>

      <WhatToRunStep
        {...what}
        provider={hasPicker && modelProvider ? <RunProviderPicker modelProvider={modelProvider} /> : null}
        taskInvalid={owns(ISSUE_TARGET.TASK)}
        taskDescribedBy={describedBy(ISSUE_TARGET.TASK)}
      />
    </div>
  );
}
