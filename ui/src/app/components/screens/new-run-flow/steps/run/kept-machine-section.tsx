/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The Run step's "Between runs" group (board KM-1), for an interactive macOS VM on the person's own Mac while kept machines
// are on: delete the machine when the run ends (today's behaviour), keep it for next time with an optional name, or
// resume one kept earlier, optionally starting it fresh. A machine a run holds can't be resumed; at the person's limit
// Keep is disabled. The server's checks at Review and create stand behind every choice.

import * as React from "react";
import { shortDate } from "../../../../../lib/format";
import type { KeptMachine } from "../../../../../lib/types";
import { Checkbox } from "../../../../ui/checkbox";
import { Input } from "../../../../ui/input";
import { Label } from "../../../../ui/label";
import { RadioGroup, RadioGroupItem } from "../../../../ui/radio-group";
import { gbOf, keptCount, KEEP_NEW_RUN as T } from "../../../../wardyn/copy/keep-machine";
import type { RunIntent } from "../../draft/run-intent";
import { shortRunId, useHolderLabels } from "../../../settings/use-holder-labels";
import type { KeptChoice } from "./kept-machine-choice";

const note = "text-xs leading-snug text-muted-foreground";
const DELETE = "delete";
const KEEP = "keep";
const RESUME = "resume:";

/** The radio value of the draft's machine. */
export const valueOf = (machine: RunIntent["machine"]): string => (machine?.keep ? KEEP : machine?.resume ? RESUME + machine.resume : DELETE);

/** A machine a run holds or is making can't be resumed. */
const held = (m: KeptMachine) => m.state === "in_use" || m.state === "creating";

export function KeptMachineSection({ choice, machine, onDelete, onKeep, onName, onResume, onFresh }: {
  choice: KeptChoice;
  machine: RunIntent["machine"];
  onDelete: () => void;
  onKeep: () => void;
  onName: (name: string) => void;
  onResume: (id: string) => void;
  onFresh: (fresh: boolean) => void;
}) {
  const labelId = React.useId();
  const nameId = React.useId();
  const freshId = React.useId();
  const countId = React.useId();
  const labels = useHolderLabels(choice.machines);
  return (
    <div role="group" aria-labelledby={labelId} data-testid="nr-kept-machine" className="flex flex-col gap-2">
      <span id={labelId} className="text-xs font-semibold">{T.LABEL}</span>
      <RadioGroup
        aria-labelledby={labelId}
        value={valueOf(machine)}
        onValueChange={(v) => (v === DELETE ? onDelete() : v === KEEP ? onKeep() : onResume(v.slice(RESUME.length)))}
        className="gap-2"
      >
        <Option value={DELETE} label={T.DELETE} />
        <Option value={KEEP} label={T.KEEP} disabled={choice.atLimit} reasonId={choice.atLimit ? countId : undefined} selected={!!machine?.keep}>
          <p className={note}>{T.KEEP_DETAIL(choice.mac, choice.gb, choice.days)}</p>
          <div className="flex flex-col gap-1.5">
            <Label htmlFor={nameId} className="text-xs">{T.NAME}</Label>
            <Input id={nameId} value={machine?.name ?? ""} aria-describedby={`${nameId}-hint`} className="max-w-72" onChange={(e) => onName(e.target.value)} />
            <p id={`${nameId}-hint`} className={note}>{T.NAME_HINT}</p>
          </div>
          <p className={note}>{T.SECRETS}</p>
        </Option>
        {choice.machines.map((m) => (
          <Option
            key={m.id}
            value={RESUME + m.id}
            label={T.RESUME(m.name)}
            disabled={held(m)}
            meta={held(m) ? T.IN_USE(labels[m.run_id ?? ""] ?? (m.run_id ? shortRunId(m.run_id) : "")) : T.RESUME_META(shortDate(m.last_used_at), m.base, gbOf(m.disk_bytes))}
            warn={!held(m) && !m.scrubbed ? T.NOT_SCRUBBED : undefined}
            selected={machine?.resume === m.id}
          >
            <div className="flex items-center gap-2.5">
              <Checkbox id={freshId} checked={!!machine?.fresh} onCheckedChange={(on) => onFresh(on === true)} />
              <Label htmlFor={freshId}>{T.FRESH}</Label>
            </div>
            <p className={note}>{T.FRESH_HINT(m.name, choice.mac)}</p>
            {machine?.fresh && <p className={note}>{T.FRESH_BASE(m.base)}</p>}
          </Option>
        ))}
      </RadioGroup>
      <p id={countId} className={note}>{keptCount(choice.count, choice.limit)}</p>
    </div>
  );
}

/** One radio row; its detail (the children) shows while it is the chosen one. */
function Option({ value, label, meta, warn, disabled, reasonId, selected, children }: {
  value: string;
  label: string;
  meta?: string;
  warn?: string;
  disabled?: boolean;
  /** The element that says why the option is disabled, when it is not the row's own meta or warning. */
  reasonId?: string;
  selected?: boolean;
  children?: React.ReactNode;
}) {
  const id = React.useId();
  const metaId = `${id}-meta`;
  const describedBy = [meta || warn ? metaId : undefined, reasonId].filter(Boolean).join(" ") || undefined;
  return (
    <div className={`flex flex-col gap-2 rounded-lg border p-3 ${selected ? "border-primary/50 bg-surface-2" : "border-border"}`}>
      <div className="flex items-start gap-2.5">
        <RadioGroupItem id={id} value={value} disabled={disabled} aria-describedby={describedBy} className="mt-0.5" />
        <div className="flex min-w-0 flex-col gap-0.5">
          <Label htmlFor={id} className="text-sm font-medium">{label}</Label>
          {(meta || warn) && (
            <span id={metaId} className="flex flex-col gap-0.5">
              {meta && <span className={note}>{meta}</span>}
              {warn && <span className={note}>{warn}</span>}
            </span>
          )}
        </div>
      </div>
      {selected && children && <div className="ml-7 flex flex-col gap-2">{children}</div>}
    </div>
  );
}
