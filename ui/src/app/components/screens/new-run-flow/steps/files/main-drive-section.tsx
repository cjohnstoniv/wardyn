/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Main drive: the sandbox's own disk, sized in GiB (0.1 steps) and in whole MiB under 0.1 GiB. The cap, what
// enforces it and what happens to the drive at the run's end are the server's
// facts for the pool the run lands in.

import * as React from "react";
import { Input } from "../../../../ui/input";
import { Label } from "../../../../ui/label";
import { Popover, PopoverContent, PopoverTrigger } from "../../../../ui/popover";
import { Chip } from "../../../../wardyn/primitives";
import { InfoTip } from "../../../../wardyn/info-tip";
import { NR_FILES } from "../../../../wardyn/copy/new-run/files";
import { NR_SHARED } from "../../../../wardyn/copy/new-run/shared";
import type { LimitSource } from "../../../../../lib/types/runner-pools";
import { commitSize, mainDriveMiB, parseSize, setMainDrive, sizeField, type SizeUnit } from "../../lenses/files-lens";
import type { RunIntent } from "../../draft/run-intent";
import { useNewRunDispatch, useNewRunDraft } from "../../new-run-route";
import type { MainDriveFacts, MainDriveView } from "./files-seams";
import { FilesGroup } from "./kind-visuals";
import { sizeWords } from "./size-words";

export const SIZE_INPUT_ID = "nr-main-drive-size";
const UNIT_WORD: Record<SizeUnit, string> = { GiB: NR_SHARED.GIB("").trim(), MiB: NR_SHARED.MIB("").trim() };


const hoursText = (sec: number): string => {
  const hours = Number((sec / 3600).toFixed(2));
  return hours === 1 ? NR_SHARED.HOUR_ONE : NR_SHARED.HOURS(hours);
};

/** The one place a clamp's source becomes a sentence. Governance names no profile on the wire, so it reads as the organisation's. */
function clampText(size: string, source: LimitSource | "policy" | undefined, pool: string | undefined): string {
  if (source === "policy") return NR_FILES.CLAMP_POLICY(size);
  if (source === "deployment") return NR_FILES.CLAMP_SERVER(size);
  if (source === "governance") return NR_FILES.CLAMP_ORG(size);
  return pool ? NR_FILES.CLAMP_POOL(size, pool) : NR_FILES.CLAMP_SERVER(size);
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

/** W1: one chip per enforcement word; the chip itself opens its tooltip on a tap. Unknown says nothing. */
function Enforcement({ facts }: { facts: MainDriveFacts }) {
  if (facts.enforcement === "unknown") return null;
  const { word, tip, label, tone } = ENFORCEMENT[facts.enforcement];
  return (
    <Popover>
      <PopoverTrigger asChild>
        <button type="button" aria-label={label} className="rounded-full outline-none focus-visible:ring-[3px] focus-visible:ring-ring">
          <Chip tone={tone}>{facts.basis === "runner_asserted" ? `${word} ${NR_FILES.REPORTED_BY_RUNNER}` : word}</Chip>
        </button>
      </PopoverTrigger>
      <PopoverContent side="top" align="end" className="w-[250px] text-sm">{tip}</PopoverContent>
    </Popover>
  );
}

/** The size as typed, committed to the draft on blur and on Continue, never past the cap. */
export function useMainDriveSize(view: MainDriveView) {
  const draft = useNewRunDraft();
  const dispatch = useNewRunDispatch();
  const committed = mainDriveMiB(draft.intent);
  const typed = draft.guided.mainDriveText;
  // The field is GiB with one decimal from 0.1 GiB up and whole MiB under it; the unit follows the text it labels.
  const unit: SizeUnit = typed !== "" ? (draft.guided.mainDriveUnit ?? "GiB") : committed !== undefined ? sizeField(committed).unit : "GiB";
  // The field's text. An emptied field stays empty while it is being edited (until blur restores the stored size).
  const [dirty, setDirty] = React.useState(false);
  const text = typed !== "" ? typed : dirty ? "" : committed !== undefined ? sizeField(committed).text : "";

  // A pool's default (else its cap) fills the empty field once; clearing it afterwards is the person's.
  const filled = React.useRef(false);
  // A prefill is shown, never sent: only a size the person typed becomes the request.
  const edited = React.useRef(false);
  // The text and clamp note the field had before it was edited: an emptied field gets them back on blur.
  const before = React.useRef<{ text: string; note?: string }>({ text: "" });
  // `dirty` is the same fact as `edited`, for rendering: true from a keystroke until the next commit stores it.
  // A fill the person never changed follows the pool: another pool's default replaces it.
  const fill = draft.guided.mainDriveFill;
  // A pool with no size to offer clears it, and the next pool that has one fills it again.
  React.useEffect(() => {
    if (committed !== undefined) return;
    const next = view.defaultMib === undefined ? undefined : sizeField(view.defaultMib, "floor");
    const nextText = next?.text ?? "";
    if (!filled.current && typed === "" && nextText !== "") {
      filled.current = true;
      dispatch({ type: "guided", patch: { mainDriveText: nextText, mainDriveFill: nextText, mainDriveUnit: next?.unit } });
    } else if (typed !== "" && typed === fill && (nextText !== typed || (next !== undefined && next.unit !== unit))) {
      if (nextText === "") filled.current = false;
      dispatch({ type: "guided", patch: { mainDriveText: nextText, mainDriveFill: nextText === "" ? undefined : nextText, mainDriveUnit: next?.unit } });
    }
  }, [typed, fill, unit, committed, view.defaultMib, dispatch]);

  const commit = React.useCallback(() => {
    if (!edited.current) return;
    // An emptied field is no new size: the stored one comes back, with its note, as if it had not been touched.
    if (text === "" && committed !== undefined) {
      edited.current = false;
      setDirty(false);
      dispatch({ type: "guided", patch: { mainDriveText: before.current.text, mainDriveClampNote: before.current.note } });
      return;
    }
    const { mib, clamped } = commitSize(text, view.capMib, unit);
    if (mib === undefined) return;
    // The stored size is final: the field now shows it (rounded), and re-reading that text would change it.
    edited.current = false;
    setDirty(false);
    const shown = sizeField(mib, clamped ? "floor" : "round");
    dispatch({ type: "edit", apply: (i) => setMainDrive(i, mib) });
    dispatch({
      type: "guided",
      patch: {
        mainDriveText: shown.text,
        mainDriveUnit: shown.unit,
        mainDriveFill: undefined,
        mainDriveClampNote: clamped ? clampText(sizeWords(mib), view.capSource, view.poolName) : undefined,
      },
    });
  }, [text, unit, committed, view.capMib, view.capSource, view.poolName, dispatch]);

  // What the field means: the stored size once committed, else what its text reads as.
  const mib = !dirty && committed !== undefined ? committed : parseSize(text, unit);
  return {
    text,
    unit,
    mib,
    missing: mib === undefined,
    // The size in words for the summary and the mount map: exact once stored; while it is being typed, the typed
    // text in its own unit, so "500" in MiB never reads "0.5 GiB" before blur.
    words: mib === undefined ? undefined : dirty ? (unit === "MiB" ? NR_SHARED.MIB(text) : NR_SHARED.GIB(text)) : sizeWords(mib),
    note: draft.guided.mainDriveClampNote,
    // The exact stored size, whenever the field's text does not say it.
    exact: !dirty && committed !== undefined && parseSize(text, unit) !== committed ? committed : undefined,
    setText: (next: string) => {
      filled.current = true;
      if (!edited.current) before.current = { text: typed, note: draft.guided.mainDriveClampNote };
      edited.current = true;
      setDirty(true);
      // Emptying a stored size keeps its clamp note: blur restores the size, and the note describes it.
      const keepNote = next === "" && committed !== undefined;
      dispatch({ type: "guided", patch: { mainDriveText: next, mainDriveFill: undefined, ...(keepNote ? {} : { mainDriveClampNote: undefined }) } });
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
    <FilesGroup titleId="nr-main-title" title={NR_FILES.MAIN_TITLE} body={NR_FILES.MAIN_BODY} aside={<span className="text-meta font-semibold uppercase tracking-[0.08em] text-muted-foreground">{NR_SHARED.REQUIRED}</span>}>
      <div className="grid gap-3 px-[15px] py-3.5">
        <div className="grid grid-cols-[70px_125px_minmax(175px,1fr)_auto] items-center gap-2.5 text-sm max-[520px]:grid-cols-[1fr_auto]">
          <Label htmlFor={SIZE_INPUT_ID} className="font-medium text-muted-foreground max-[520px]:col-span-full">{NR_FILES.SIZE_LABEL}</Label>
          {/* At phone width the row stacks: label, the input across the row, the cap, then the chip. */}
          <span className="relative flex items-center max-[520px]:col-span-full">
            <Input
              id={SIZE_INPUT_ID}
              type="number"
              inputMode="decimal"
              min={0}
              step={size.unit === "GiB" ? 0.1 : 1}
              className="h-[42px] pr-10"
              value={size.text}
              aria-invalid={showError || undefined}
              aria-describedby={showError ? errorId : undefined}
              onChange={(e) => size.setText(e.target.value)}
              onBlur={() => { setTouched(true); size.commit(); }}
            />
            <span aria-hidden="true" className="pointer-events-none absolute right-3 text-sm text-muted-foreground">{UNIT_WORD[size.unit]}</span>
          </span>
          <span className="text-muted-foreground max-[520px]:col-span-full">{view.capMib !== undefined && NR_FILES.UP_TO(sizeWords(view.capMib))}</span>
          <span className="max-[520px]:col-span-full">{view.facts && <Enforcement facts={view.facts} />}</span>
        </div>
        {(size.note || size.exact !== undefined) && (
          <p className="text-sm text-muted-foreground">
            {size.note}
            {size.note && size.exact !== undefined ? " " : ""}
            {size.exact !== undefined && <span data-testid="nr-main-drive-exact">{NR_FILES.SIZE_EXACT(size.exact)}</span>}
          </p>
        )}
        {showError && <p id={errorId} className="text-sm text-danger">{NR_FILES.SIZE_ERROR}</p>}
        {life && (
          <div className="grid grid-cols-[150px_1fr] items-start gap-3 border-t border-border pt-[11px] max-[520px]:grid-cols-1">
            <span className="text-sm font-medium text-muted-foreground">{NR_FILES.LIFE_LABEL}</span>
            <strong className="text-sm leading-[1.45] font-semibold">{life}</strong>
          </div>
        )}
        {/* The rule row: checkbox, label and Coming soon in one wrapper, the tooltip beside it, on one line that wraps whole (#2860). */}
        <div className="flex flex-wrap items-center gap-2 border-t border-border pt-[11px] text-sm">
          <span className="flex flex-wrap items-center gap-2 opacity-60">
            <input id="nr-reuse-drive" type="checkbox" disabled className="size-4" />
            <label htmlFor="nr-reuse-drive" className="font-semibold">{NR_FILES.REUSE_LABEL}</label>
            <Chip>{NR_SHARED.COMING_SOON}</Chip>
          </span>
          <InfoTip label={NR_FILES.REUSE_ARIA}>{NR_FILES.REUSE_TIP}</InfoTip>
        </div>
      </div>
    </FilesGroup>
  );
}
