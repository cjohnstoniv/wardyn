/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Main drive: the sandbox's own disk, sized in GiB (0.1 steps). The cap, what
// enforces it and what happens to the drive at the run's end are the server's
// facts for the pool the run lands in.

import * as React from "react";
import { Input } from "../../../../ui/input";
import { Label } from "../../../../ui/label";
import { Chip } from "../../../../wardyn/primitives";
import { InfoTip } from "../../../../wardyn/info-tip";
import { NR_FILES } from "../../../../wardyn/copy/new-run/files";
import { NR_SHARED } from "../../../../wardyn/copy/new-run/shared";
import type { LimitSource } from "../../../../../lib/types/runner-pools";
import { commitSize, mainDriveMiB, mibToGibText, parseGiB, setMainDrive } from "../../lenses/files-lens";
import type { RunIntent } from "../../draft/run-intent";
import { useNewRunDispatch, useNewRunDraft } from "../../new-run-route";
import type { MainDriveFacts, MainDriveView } from "./files-seams";
import { FilesGroup } from "./kind-visuals";

export const SIZE_INPUT_ID = "nr-main-drive-size";
const UNIT = NR_SHARED.GIB("").trim();

const hoursText = (sec: number): string => {
  const hours = Number((sec / 3600).toFixed(2));
  return hours === 1 ? NR_SHARED.HOUR_ONE : NR_SHARED.HOURS(hours);
};

/** The one place a clamp's source becomes a sentence. Governance names no profile on the wire, so it reads as the organisation's. */
function clampText(cap: string, source: LimitSource | undefined, pool: string | undefined): string {
  if (source === "deployment") return NR_FILES.CLAMP_SERVER(cap);
  if (source === "governance") return NR_FILES.CLAMP_ORG(cap);
  return pool ? NR_FILES.CLAMP_POOL(cap, pool) : NR_FILES.CLAMP_SERVER(cap);
}

export type Lifecycle = { kind: "deleted" } | { kind: "noEnd" } | { kind: "kept"; grace: string; max?: string } | { kind: "unknown" };

/** W2. An Interactive run's drive outlives its end time only where the server says it keeps it; with no facts yet, nothing is claimed. */
export function lifecycleOf(intent: RunIntent, view: MainDriveView): Lifecycle {
  if (intent.experience !== "interactive") return { kind: "deleted" };
  if (intent.no_end) return { kind: "noEnd" };
  const keep = view.facts?.keep;
  if (!keep) return { kind: "unknown" };
  if (!keep.supported || !keep.grace_sec) return { kind: "deleted" };
  return { kind: "kept", grace: hoursText(keep.grace_sec), max: view.maxLifetimeSec ? hoursText(view.maxLifetimeSec) : undefined };
}

export const lifecycleText = (l: Lifecycle): string | undefined =>
  l.kind === "deleted" ? NR_FILES.LIFE_DELETED
    : l.kind === "noEnd" ? NR_FILES.LIFE_NO_END
      : l.kind === "kept" ? (l.max ? NR_FILES.LIFE_KEPT(l.max, l.grace) : NR_FILES.LIFE_KEPT_UNLIMITED(l.grace))
        : undefined;

const ENFORCEMENT = {
  filesystem: { word: NR_FILES.ENFORCED, tip: NR_FILES.ENFORCED_TIP, label: NR_FILES.ENFORCED, tone: "neutral" },
  eviction: { word: NR_FILES.ENFORCED_BY_STOP, tip: NR_FILES.ENFORCED_BY_STOP_TIP, label: NR_FILES.ENFORCED_BY_STOP, tone: "neutral" },
  none: { word: NR_FILES.NOT_ENFORCED, tip: NR_FILES.NOT_ENFORCED_TIP, label: NR_FILES.NOT_ENFORCED_ARIA, tone: "warning" },
} as const;

/** W1: one chip per enforcement word, with its tooltip; unknown says nothing. */
function Enforcement({ facts }: { facts: MainDriveFacts }) {
  if (facts.enforcement === "unknown") return null;
  const { word, tip, label, tone } = ENFORCEMENT[facts.enforcement];
  return (
    <span className="inline-flex items-center gap-1">
      <Chip tone={tone}>{facts.basis === "runner_asserted" ? `${word} ${NR_FILES.REPORTED_BY_RUNNER}` : word}</Chip>
      <InfoTip label={label}>{tip}</InfoTip>
    </span>
  );
}

/** The size as typed, committed to the draft on blur and on Continue, never past the cap. */
export function useMainDriveSize(view: MainDriveView) {
  const draft = useNewRunDraft();
  const dispatch = useNewRunDispatch();
  const committed = mainDriveMiB(draft.intent);
  const typed = draft.guided.mainDriveText;
  const text = typed !== "" ? typed : committed !== undefined ? mibToGibText(committed) : "";

  // A pool's default (else its cap) fills the empty field once; clearing it afterwards is the person's.
  const filled = React.useRef(false);
  // A prefill is shown, never sent: only a size the person typed becomes the request.
  const edited = React.useRef(false);
  React.useEffect(() => {
    if (!filled.current && typed === "" && committed === undefined && view.defaultMib !== undefined) {
      filled.current = true;
      dispatch({ type: "guided", patch: { mainDriveText: mibToGibText(view.defaultMib, "floor") } });
    }
  }, [typed, committed, view.defaultMib, dispatch]);

  const commit = React.useCallback(() => {
    if (!edited.current) return;
    const { mib, clamped } = commitSize(text, view.capMib);
    if (mib === undefined) return;
    dispatch({ type: "edit", apply: (i) => setMainDrive(i, mib) });
    dispatch({
      type: "guided",
      patch: {
        mainDriveText: mibToGibText(mib, clamped ? "floor" : "round"),
        mainDriveClampNote: clamped ? clampText(mibToGibText(mib, "floor"), view.capSource, view.poolName) : undefined,
      },
    });
  }, [text, view.capMib, view.capSource, view.poolName, dispatch]);

  return {
    text,
    missing: parseGiB(text) === undefined,
    note: draft.guided.mainDriveClampNote,
    setText: (next: string) => {
      filled.current = true;
      edited.current = true;
      dispatch({ type: "guided", patch: { mainDriveText: next, mainDriveClampNote: undefined } });
    },
    commit,
  };
}

export function MainDriveSection({ view, size }: { view: MainDriveView; size: ReturnType<typeof useMainDriveSize> }) {
  const draft = useNewRunDraft();
  const [touched, setTouched] = React.useState(false);
  const errorId = React.useId();
  const showError = size.missing && touched;
  const life = lifecycleText(lifecycleOf(draft.intent, view));

  return (
    <FilesGroup titleId="nr-main-title" title={NR_FILES.MAIN_TITLE} body={NR_FILES.MAIN_BODY} aside={<span className="text-meta font-semibold uppercase tracking-[0.08em] text-warning">{NR_SHARED.REQUIRED}</span>}>
      <div className="grid gap-3 px-[15px] py-3.5">
        <div className="grid grid-cols-[70px_125px_minmax(175px,1fr)_auto] items-center gap-2.5 text-sm max-[520px]:grid-cols-[1fr_auto]">
          <Label htmlFor={SIZE_INPUT_ID} className="font-medium text-muted-foreground max-[520px]:col-span-full">{NR_FILES.SIZE_LABEL}</Label>
          <span className="relative flex items-center">
            <Input
              id={SIZE_INPUT_ID}
              type="number"
              inputMode="decimal"
              min={0}
              step={0.1}
              className="pr-10"
              value={size.text}
              aria-invalid={showError || undefined}
              aria-describedby={showError ? errorId : undefined}
              onChange={(e) => size.setText(e.target.value)}
              onBlur={() => { setTouched(true); size.commit(); }}
            />
            <span aria-hidden="true" className="pointer-events-none absolute right-3 text-sm text-muted-foreground">{UNIT}</span>
          </span>
          <span className="text-muted-foreground">{view.capMib !== undefined && NR_FILES.UP_TO(mibToGibText(view.capMib, "floor"))}</span>
          <span>{view.facts && <Enforcement facts={view.facts} />}</span>
        </div>
        {size.note && <p className="text-sm text-muted-foreground">{size.note}</p>}
        {showError && <p id={errorId} className="text-sm text-danger">{NR_FILES.SIZE_ERROR}</p>}
        {life && (
          <div className="grid grid-cols-[150px_1fr] items-start gap-3 border-t border-border pt-[11px] max-[520px]:grid-cols-1">
            <span className="text-sm font-medium text-muted-foreground">{NR_FILES.LIFE_LABEL}</span>
            <strong className="text-sm leading-[1.45] font-semibold">{life}</strong>
          </div>
        )}
        <div className="flex items-center gap-2 border-t border-border pt-[11px] text-sm opacity-60">
          <label className="flex items-center gap-2 font-semibold">
            <input type="checkbox" disabled />
            {NR_FILES.REUSE_LABEL}
          </label>
          <Chip>{NR_SHARED.COMING_SOON}</Chip>
          <InfoTip label={NR_FILES.REUSE_ARIA}>{NR_FILES.REUSE_TIP}</InfoTip>
        </div>
      </div>
    </FilesGroup>
  );
}
