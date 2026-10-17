/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import * as React from "react";
import { Button } from "../../../../ui/button";
import { Input } from "../../../../ui/input";
import { Label } from "../../../../ui/label";
import { NR_REVIEW } from "../../../../wardyn/copy/new-run/review";
import { NR_SHARED } from "../../../../wardyn/copy/new-run/shared";
import type { EffectiveLimit } from "../../../../../lib/types/runner-pools";
import { parseCpuMillis, parseMemoryMib, setCpu, setMemory } from "../../lenses/limits-lens";
import { draftSource } from "../provenance";
import { cpuNumber, memoryNumber, resolvedLifetime, resolvedLifetimeText } from "../format";
import { ReviewGroup } from "../review-group";
import type { GroupProps } from "./group-props";

function LimitField({ id, label, text, onText, hint, invalid }: {
  id: string; label: string; text: string; onText: (t: string) => void; hint?: string; invalid: boolean;
}) {
  return (
    <div className="flex flex-col gap-1.5">
      <Label htmlFor={id}>{label}</Label>
      <Input id={id} inputMode="decimal" value={text} aria-invalid={invalid || undefined} onChange={(e) => onText(e.target.value)} />
      {hint && <small className="text-muted-foreground">{hint}</small>}
    </div>
  );
}

const capHint = (limit: EffectiveLimit | undefined, cap: string, by: string | undefined) =>
  limit && !limit.cap.unlimited && by ? NR_REVIEW.LIMIT_UP_TO(cap, by) : undefined;

function LimitsEditor({ cpuMillis, memoryMib, pre, dispatch, close }: Pick<GroupProps, "pre" | "dispatch"> & {
  cpuMillis?: number; memoryMib?: number; close: () => void;
}) {
  const [cpuText, setCpuText] = React.useState(cpuMillis === undefined ? "" : cpuNumber(cpuMillis));
  const [memoryText, setMemoryText] = React.useState(memoryMib === undefined ? "" : String(memoryMib));
  const applied = pre?.resources_applied;
  const limits = pre?.limits;
  const by = (limit: EffectiveLimit | undefined, name: string | undefined) => (limit?.cap_source === "pool" ? pre?.runner_pool?.name : name);
  return (
    <>
      <LimitField id="review-cpu" label={NR_REVIEW.CPU_LABEL} text={cpuText} invalid={parseCpuMillis(cpuText) === undefined}
        hint={capHint(limits?.cpu_millis, cpuNumber(limits?.cpu_millis.cap.value ?? 0), by(limits?.cpu_millis, applied?.cpu_millis.source_name))}
        onText={(t) => {
          setCpuText(t);
          const millis = parseCpuMillis(t);
          if (millis !== undefined) dispatch({ type: "edit", apply: (i) => setCpu(i, millis) });
        }} />
      <LimitField id="review-memory" label={NR_REVIEW.MEMORY_LABEL} text={memoryText} invalid={parseMemoryMib(memoryText) === undefined}
        hint={capHint(limits?.memory_mib, memoryNumber(limits?.memory_mib.cap.value ?? 0), by(limits?.memory_mib, applied?.memory_mib.source_name))}
        onText={(t) => {
          setMemoryText(t);
          const mib = parseMemoryMib(t);
          if (mib !== undefined) dispatch({ type: "edit", apply: (i) => setMemory(i, mib) });
        }} />
      <Button type="button" variant="outline" className="w-fit" onClick={close}>{NR_REVIEW.DONE}</Button>
    </>
  );
}

export function LimitsGroup({ draft, pre, docRows, dispatch }: GroupProps) {
  const applied = pre?.resources_applied;
  const requested = draft.intent.resources;
  const cpuMillis = applied?.cpu_millis.applied ?? requested?.cpu_millis;
  const memoryMib = applied?.memory_mib.applied ?? requested?.memory_mib;
  const lifetime = resolvedLifetimeText(resolvedLifetime(draft.intent, pre?.limits));
  const size = cpuMillis !== undefined && memoryMib !== undefined ? [cpuNumber(cpuMillis), memoryNumber(memoryMib)] : undefined;
  const value = size && (lifetime === undefined
    ? NR_SHARED.JOIN(NR_SHARED.CPU(size[0]), NR_SHARED.MIB(size[1]))
    : NR_REVIEW.LIMITS_VALUE(size[0], size[1], lifetime));
  const source = applied?.cpu_millis.source_name || applied?.memory_mib.source_name;
  return (
    <ReviewGroup
      id="limits"
      label={NR_REVIEW.GROUP_LIMITS}
      value={value}
      detail={NR_REVIEW.LIMITS_DETAIL}
      provenance={source || draftSource(draft, ["resources"], NR_REVIEW.PROV_SELECTED)}
      docRows={docRows}
      editor={(close) => <LimitsEditor cpuMillis={requested?.cpu_millis} memoryMib={requested?.memory_mib} pre={pre} dispatch={dispatch} close={close} />}
    />
  );
}
