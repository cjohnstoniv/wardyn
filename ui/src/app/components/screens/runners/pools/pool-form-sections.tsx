/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The pool form's per-run-type fieldset (mock M-POOLS v6 rtSection / durPair): CPU, memory and disk as a
// Default and a Maximum per run, then Lifetime (and for Interactive an Idle stop) as a number and a unit each,
// with the No maximum / Never choice. Every control carries its own accessible name (section, field, column),
// since the visible column captions repeat across fields.
import type { RunnerAmounts } from "../../../../lib/types/runners";
import { POOL_DRAWER, POOL_FORM } from "../../../wardyn/copy/runner-pools";
import { Checkbox } from "../../../ui/checkbox";
import { DurationField } from "../../../wardyn/duration-field";
import { Input } from "../../../ui/input";
import { cpus, fmtAmount, gb } from "../resources";
import { UNIT_LABEL, UNITS, type FormErrors, type RtKey, type RtValues, type Unit } from "./pool-form-model";

const UNIT_OPTIONS = UNITS.map((u) => ({ value: u, label: UNIT_LABEL[u] }));
const SUB = "text-meta text-muted-foreground";
const ERR = "text-xs text-danger";

type Patch = (patch: Partial<RtValues>) => void;

function Err({ id, children }: { id: string; children?: string }) {
  return children ? (
    <p id={id} role="alert" className={ERR}>
      {children}
    </p>
  ) : null;
}

function AmountField({
  rt,
  k,
  label,
  values,
  error,
  hint,
  onPatch,
}: {
  rt: RtKey;
  k: "cpu" | "mem" | "disk";
  label: string;
  values: RtValues;
  error?: string;
  hint: string | null;
  onPatch: Patch;
}) {
  const title = rt === "bg" ? POOL_DRAWER.BACKGROUND : POOL_DRAWER.INTERACTIVE;
  const id = `f-${rt}-${k}`;
  const input = (suffix: "D" | "C", caption: string) => (
    <div className="space-y-1">
      <span className={`${SUB} block`} aria-hidden>
        {caption}
      </span>
      <Input
        id={`${id}${suffix}`}
        type="number"
        inputMode="decimal"
        min={0}
        step="any"
        aria-label={`${title}, ${label}, ${caption}`}
        aria-invalid={error ? true : undefined}
        aria-describedby={error ? `${id}-e` : undefined}
        value={values[`${k}${suffix}`]}
        onChange={(e) => onPatch({ [`${k}${suffix}`]: e.target.value })}
      />
    </div>
  );
  return (
    <div className="space-y-2">
      <span className="text-sm font-medium" id={`${id}-l`}>
        {label}
      </span>
      <div className="grid grid-cols-[repeat(auto-fit,minmax(120px,1fr))] gap-x-3 gap-y-2 max-[480px]:grid-cols-1" role="group" aria-labelledby={`${id}-l`}>
        {input("D", POOL_FORM.DEFAULT)}
        {input("C", POOL_FORM.MAX_PER_RUN)}
      </div>
      {hint && <p className="text-xs text-muted-foreground">{hint}</p>}
      <Err id={`${id}-e`}>{error}</Err>
    </div>
  );
}

function DurPair({
  rt,
  pre,
  label,
  noLabel,
  values,
  error,
  maxError,
  onPatch,
}: {
  rt: RtKey;
  pre: "life" | "idle";
  label: string;
  noLabel: string;
  values: RtValues;
  error?: string;
  /** The error is about the Maximum field, not the Default. */
  maxError?: string;
  onPatch: Patch;
}) {
  const title = rt === "bg" ? POOL_DRAWER.BACKGROUND : POOL_DRAWER.INTERACTIVE;
  const id = `f-${rt}-${pre}`;
  const unl = values[`${pre}Unl`];
  const durPair = (n: "Dn" | "Mn", u: "Du" | "Mu", caption: string, disabled: boolean) => (
    <DurationField
      id={`${id}${n}`}
      count={values[`${pre}${n}`]}
      unit={values[`${pre}${u}`]}
      units={UNIT_OPTIONS}
      onCount={(raw) => onPatch({ [`${pre}${n}`]: raw })}
      onUnit={(v) => onPatch({ [`${pre}${u}`]: v as Unit })}
      countLabel={`${title}, ${label}, ${caption}`}
      unitLabel={`${title}, ${label}, ${caption}, ${POOL_FORM.UNIT}`}
      disabled={disabled}
      unitDisabled={u === "Mu" && unl}
      invalid={!!error && (n === "Mn") === !!maxError}
      describedBy={error && (n === "Mn") === !!maxError ? `${id}-e` : undefined}
      step="any"
    />
  );
  return (
    <div className="space-y-2">
      <span className="text-sm font-medium" id={`${id}-l`}>
        {label}
      </span>
      <div role="group" aria-labelledby={`${id}-l`} className="grid grid-cols-[repeat(auto-fit,minmax(220px,1fr))] gap-x-3 gap-y-2 max-[480px]:grid-cols-1">
        <div className="space-y-1">
          <span className={`${SUB} block`} aria-hidden>
            {POOL_FORM.DEFAULT}
          </span>
          {durPair("Dn", "Du", POOL_FORM.DEFAULT, false)}
        </div>
        <div className="space-y-1">
          <span className={`${SUB} block`} aria-hidden>
            {POOL_FORM.MAXIMUM}
          </span>
          {durPair("Mn", "Mu", POOL_FORM.MAXIMUM, unl)}
        </div>
      </div>
      <label className="inline-flex items-center gap-2 text-sm font-normal">
        <Checkbox checked={unl} onCheckedChange={(c) => onPatch({ [`${pre}Unl`]: c === true })} />
        {noLabel}
      </label>
      <Err id={`${id}-e`}>{error}</Err>
    </div>
  );
}

/** What the pool's runners allow per run, as the hint under an amount; with no runners chosen the fieldset's note says so once. */
export function runnersHint(biggest: RunnerAmounts & { count: number }, k: "cpu" | "mem" | "disk"): string | null {
  if (!biggest.count) return null;
  const amount = k === "cpu" ? fmtAmount("cpu", cpus(biggest.cpu_millis)) : fmtAmount(k, gb(k === "mem" ? biggest.memory_mib : biggest.disk_mib));
  return POOL_FORM.RUNNERS_ALLOW(amount);
}

export function RtSection({
  rt,
  values,
  errors,
  biggest,
  onPatch,
}: {
  rt: RtKey;
  values: RtValues;
  errors: FormErrors;
  biggest: RunnerAmounts & { count: number };
  onPatch: Patch;
}) {
  const title = rt === "bg" ? POOL_DRAWER.BACKGROUND : POOL_DRAWER.INTERACTIVE;
  const amount = (k: "cpu" | "mem" | "disk", label: string) => (
    <AmountField key={k} rt={rt} k={k} label={label} values={values} error={errors[`${rt}.${k}`]} hint={runnersHint(biggest, k)} onPatch={onPatch} />
  );
  return (
    <fieldset className="mb-3 space-y-4 rounded-xl border border-border bg-card p-4">
      <legend className="label-eyebrow px-1.5">{title}</legend>
      <p data-testid={`f-${rt}-qualifier`} className="text-xs text-muted-foreground">
        {biggest.count ? POOL_FORM.QUALIFIER : `${POOL_FORM.QUALIFIER} ${POOL_FORM.RUNNERS_NOT_CHOSEN}`}
      </p>
      {amount("cpu", POOL_FORM.CPU)}
      {amount("mem", POOL_FORM.MEMORY)}
      {amount("disk", POOL_FORM.DISK)}
      <DurPair rt={rt} pre="life" label={POOL_FORM.LIFETIME} noLabel={POOL_FORM.NO_MAXIMUM} values={values} error={errors[`${rt}.life`]} maxError={errors[`${rt}.life.max`]} onPatch={onPatch} />
      {rt === "bg" ? (
        <div className="-mt-1.5 space-y-1">
          <label className="inline-flex items-center gap-2 text-sm font-normal">
            <Checkbox disabled={!values.lifeUnl} checked={values.lifeNoEnd} onCheckedChange={(c) => onPatch({ lifeNoEnd: c === true })} />
            {POOL_FORM.NO_END}
          </label>
          <p className="text-xs text-muted-foreground">{POOL_FORM.NO_END_HINT}</p>
        </div>
      ) : (
        <DurPair rt={rt} pre="idle" label={POOL_FORM.IDLE} noLabel={POOL_FORM.NEVER} values={values} error={errors[`${rt}.idle`]} maxError={errors[`${rt}.idle.max`]} onPatch={onPatch} />
      )}
    </fieldset>
  );
}
