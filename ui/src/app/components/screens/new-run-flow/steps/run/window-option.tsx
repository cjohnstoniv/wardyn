/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The Run step's Window group (board WIN-2): one checkbox, what it does, and a cleared choice's notice. The Review
// editor draws the same checkbox without the lines.

import * as React from "react";
import { TriangleAlert } from "lucide-react";
import { Checkbox } from "../../../../ui/checkbox";
import { Label } from "../../../../ui/label";
import { NR_REVIEW } from "../../../../wardyn/copy/new-run/review";
import { NR_WINDOW } from "../../../../wardyn/copy/new-run/window";

const note = "text-xs leading-snug text-muted-foreground";

/** The checkbox alone, labelled W2-02. */
export function WindowCheckbox({ id, chosen, onChoose, describedBy }: { id: string; chosen: boolean; onChoose: (on: boolean) => void; describedBy?: string }) {
  return (
    <div className="flex items-center gap-2.5">
      <Checkbox id={id} checked={chosen} aria-describedby={describedBy} onCheckedChange={(on) => onChoose(on === true)} />
      <Label htmlFor={id}>{NR_WINDOW.OPTION}</Label>
    </div>
  );
}

/** "Window choice cleared." and the server's sentence after it (W2-06). Spans only: Review draws it inside a line. */
export function WindowCleared({ text }: { text: string }) {
  return (
    <span role="status" className="flex items-start gap-2 text-xs leading-snug">
      <TriangleAlert aria-hidden="true" className="mt-px size-3.5 shrink-0 text-warning" />
      <span className="min-w-0">
        <span className="block text-warning">{NR_WINDOW.CLEARED}</span>
        <span className="block text-muted-foreground">{text}</span>
      </span>
    </span>
  );
}

/**
 * The group. `runner` is the pool's one Mac, when it has exactly one; with several (or none known) the hint says the
 * run may land on a Mac that isn't the person's.
 */
export function WindowOption({ chosen, runner, checking, cleared, onChoose }: {
  chosen: boolean;
  runner: string | undefined;
  checking: boolean;
  cleared?: string;
  onChoose: (on: boolean) => void;
}) {
  const id = React.useId();
  return (
    <div role="group" aria-labelledby={`${id}-label`} className="flex flex-col gap-2">
      <span id={`${id}-label`} className="text-xs font-semibold">{NR_WINDOW.GROUP}</span>
      <WindowCheckbox id={id} chosen={chosen} onChoose={onChoose} describedBy={`${id}-help`} />
      <div id={`${id}-help`} className="flex flex-col gap-1.5">
        <p className={note}>{runner ? NR_WINDOW.ONE_MAC(runner) : NR_WINDOW.SEVERAL_MACS}</p>
        {chosen && runner && <p className={note}>{NR_WINDOW.BE_THERE(runner)}</p>}
      </div>
      {chosen && checking && <p role="status" className={note}>{NR_REVIEW.CHECKING}</p>}
      {!chosen && cleared && <WindowCleared text={cleared} />}
    </div>
  );
}
