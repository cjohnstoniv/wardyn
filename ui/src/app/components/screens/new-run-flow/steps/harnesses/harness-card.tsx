/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// One harness on the Harnesses step: which harness, whether it starts the run,
// and its starting prompt or task. What it shows follows the workload.

import * as React from "react";
import { Button } from "../../../../ui/button";
import { Label } from "../../../../ui/label";
import { Textarea } from "../../../../ui/textarea";
import { NR_HARNESSES } from "../../../../wardyn/copy/new-run/harnesses";
import { NR_SHARED } from "../../../../wardyn/copy/new-run/shared";
import { Switch } from "../../../../wardyn/form-primitives";
import { Chip } from "../../../../wardyn/primitives";
import { harnessHint, unavailableReason, type Roster } from "./roster";

export type HarnessMode = "optional" | "interactive" | "autonomous";

const SELECT_CLASS =
  "border-input bg-input-background dark:bg-input/30 focus-visible:border-ring focus-visible:ring-ring h-9 w-full rounded-md border px-3 text-base outline-none focus-visible:ring-[3px] md:text-sm";

export interface HarnessCardProps {
  index: number;
  id: string;
  roster: Roster;
  mode: HarnessMode;
  /** The other cards' harnesses: each is taken, and a combination is checked against them. */
  others: readonly string[];
  removable: boolean;
  started: boolean;
  /** The starting prompt (Interactive) or the task (Autonomous). */
  text: string;
  onSwap: (to: string) => void;
  onRemove: () => void;
  onStart: (on: boolean) => void;
  onText: (text: string) => void;
}

export function HarnessCard({ index, id, roster, mode, others, removable, started, text, onSwap, onRemove, onStart, onText }: HarnessCardProps) {
  const [flagged, setFlagged] = React.useState(false);
  const selectId = `nr-harness-${index}`;
  const textId = mode === "autonomous" && index === 0 ? "nr-task" : `nr-text-${index}`;
  const known = roster.harnesses.some((h) => h.id === id);
  const name = roster.harnesses.find((h) => h.id === id)?.display ?? id;
  // An Autonomous run holds one harness, so the other cards never constrain this one's choice.
  const against = mode === "autonomous" ? [] : others;
  const taskMissing = mode === "autonomous" && flagged && !text.trim();

  return (
    <section aria-label={NR_HARNESSES.EYEBROW(index + 1)} className="flex flex-col gap-4 rounded-lg border border-border p-4">
      <div className="flex items-start justify-between gap-3">
        <div className="flex flex-col gap-1">
          <span className="label-eyebrow">{NR_HARNESSES.EYEBROW(index + 1)}</span>
          <strong>{name}</strong>
        </div>
        <Button type="button" variant="ghost" disabled={!removable} onClick={onRemove}>{NR_SHARED.REMOVE}</Button>
      </div>
      <div className="flex flex-col gap-2">
        <Label htmlFor={selectId}>{NR_HARNESSES.HARNESS_LABEL}</Label>
        <select id={selectId} className={SELECT_CLASS} value={id} aria-describedby={`${selectId}-hint`} onChange={(e) => onSwap(e.target.value)}>
          {!known && <option value={id}>{id}</option>}
          {roster.harnesses.map((h) => (
            <option key={h.id} value={h.id} disabled={h.id !== id && (others.includes(h.id) || !!unavailableReason(h, against, roster))}>
              {h.display}
            </option>
          ))}
        </select>
        <p id={`${selectId}-hint`} className="text-xs leading-snug text-muted-foreground">{harnessHint(roster, id)}</p>
      </div>
      {mode === "optional" && (
        <div className="flex items-center gap-2 text-sm">
          <Chip>{NR_SHARED.OPTIONAL}</Chip>
          <span>{NR_HARNESSES.OPTIONAL_BEHAVIOUR}</span>
        </div>
      )}
      {mode === "interactive" && (
        <div className="flex flex-col gap-3">
          <div className="flex items-center gap-2 text-sm">
            <Switch checked={started} label={NR_HARNESSES.START_IN_HARNESS} onChange={onStart} />
            <span aria-hidden="true">{NR_HARNESSES.START_IN_HARNESS}</span>
          </div>
          {started && (
            <div className="flex flex-col gap-2">
              <Label htmlFor={textId}>{NR_HARNESSES.STARTING_PROMPT} <Chip>{NR_SHARED.OPTIONAL}</Chip></Label>
              <Textarea id={textId} value={text} placeholder={NR_HARNESSES.STARTING_PROMPT_PLACEHOLDER} aria-describedby={`${textId}-hint`} onChange={(e) => onText(e.target.value)} />
              <p id={`${textId}-hint`} className="text-xs leading-snug text-muted-foreground">{NR_HARNESSES.STARTING_PROMPT_HELP}</p>
            </div>
          )}
        </div>
      )}
      {mode === "autonomous" && (
        <div className="flex flex-col gap-2">
          <Label htmlFor={textId}>{NR_HARNESSES.TASK_LABEL} <Chip>{NR_SHARED.REQUIRED}</Chip></Label>
          <Textarea
            id={textId}
            value={text}
            placeholder={NR_HARNESSES.TASK_PLACEHOLDER}
            aria-invalid={taskMissing || undefined}
            aria-describedby={`${textId}-hint`}
            // The shell marks the field invalid before it focuses it when Continue finds the task missing.
            onFocus={(e) => e.currentTarget.getAttribute("aria-invalid") === "true" && setFlagged(true)}
            onChange={(e) => onText(e.target.value)}
          />
          {taskMissing && <p role="alert" className="text-xs text-danger">{NR_HARNESSES.TASK_ERROR}</p>}
          <p id={`${textId}-hint`} className="text-xs leading-snug text-muted-foreground">{NR_HARNESSES.TASK_HELP}</p>
        </div>
      )}
    </section>
  );
}
