/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// A runner's limits (mock M-POOLS v6, frames B5, B6 and RN3): Runs at once, the total across all its
// runs, and the most one run may use. A self-hosted runner's are its owner's, prefilled from what the
// computer reports and never above it; a remote-hosted runner's are a super admin's. Per run follows
// total ÷ Runs at once until the person types over it. The server checks every value again and its
// refusal shows as written.
import * as React from "react";
import { Loader2 } from "lucide-react";
import { toast } from "sonner";
import { getErrorMessage } from "../../../lib/format";
import type { RunnerAmounts, RunnerLimits } from "../../../lib/types/runners";
import { LIMITS_DIALOG, RES, RUNNER_V6 } from "../../wardyn/copy/runner-pools";
import { RUNNER } from "../../wardyn/copy/runners";
import { Button } from "../../ui/button";
import { Dialog, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle } from "../../ui/dialog";
import { Input } from "../../ui/input";
import { amountsText, cpus, gb, perRunShare, proposeLimits, toAmounts } from "./resources";

export interface LimitsTarget {
  name: string;
  limits: RunnerLimits | null;
  /** What a self-hosted runner's computer reports; absent for a remote-hosted runner or before it has reported. */
  reported?: RunnerAmounts;
  running: number;
  self: boolean;
}

const FIELDS = ["rn", "tc", "tm", "td", "pc", "pm", "pd"] as const;
type FieldId = (typeof FIELDS)[number];
type Vals = Record<FieldId, string>;
type Errs = Partial<Record<FieldId, string>>;

const fromLimits = (l: RunnerLimits): Vals => ({
  rn: String(l.runs_at_once),
  tc: String(cpus(l.total.cpu_millis)),
  tm: String(gb(l.total.memory_mib)),
  td: String(gb(l.total.disk_mib)),
  pc: String(cpus(l.per_run.cpu_millis)),
  pm: String(gb(l.per_run.memory_mib)),
  pd: String(gb(l.per_run.disk_mib)),
});
const BLANK: Vals = { rn: "", tc: "", tm: "", td: "", pc: "", pm: "", pd: "" };

function initial(t: LimitsTarget): Vals {
  if (t.limits) return fromLimits(t.limits);
  return t.reported ? fromLimits(proposeLimits(t.reported)) : BLANK;
}

const share = (v: Vals) => perRunShare({ cpu: Number(v.tc), mem: Number(v.tm), disk: Number(v.td) }, Number(v.rn));

const PAIRS: { total: FieldId; per: FieldId; reported: (a: RunnerAmounts) => number; unit: (n: number) => string }[] = [
  { total: "tc", per: "pc", reported: (a) => a.cpu_millis / 1000, unit: (n) => RES.CPUS(cpus(n * 1000)) },
  { total: "tm", per: "pm", reported: (a) => a.memory_mib / 1024, unit: (n) => `${gb(n * 1024)} GB memory` },
  { total: "td", per: "pd", reported: (a) => a.disk_mib / 1024, unit: (n) => `${gb(n * 1024)} GB disk` },
];

/** The amount each field will send, in the dialog's units: a field still showing what it loaded is its exact loaded value, not the rounded display. */
function effective(v: Vals, loaded?: LoadedLimits | null): Record<FieldId, number> {
  const was = loaded?.limits;
  const exact: Record<FieldId, number | undefined> = {
    rn: was?.runs_at_once,
    tc: was && was.total.cpu_millis / 1000,
    tm: was && was.total.memory_mib / 1024,
    td: was && was.total.disk_mib / 1024,
    pc: was && was.per_run.cpu_millis / 1000,
    pm: was && was.per_run.memory_mib / 1024,
    pd: was && was.per_run.disk_mib / 1024,
  };
  return Object.fromEntries(FIELDS.map((f) => [f, loaded && v[f] === loaded.vals[f] ? exact[f] : Number(v[f])])) as Record<FieldId, number>;
}

/** The first thing wrong with each field, in the mock's words; what a save would send is what is checked. */
export function validateLimits(v: Vals, reported?: RunnerAmounts, loaded?: LoadedLimits | null): Errs {
  const errs: Errs = {};
  const n = effective(v, loaded);
  if (v.rn === "" || !Number.isInteger(n.rn) || n.rn < 1) errs.rn = LIMITS_DIALOG.ERR_RUNS;
  for (const p of PAIRS) {
    const total = n[p.total];
    const per = n[p.per];
    if (!(total > 0)) errs[p.total] = LIMITS_DIALOG.ERR_ABOVE_ZERO;
    else if (reported && p.reported(reported) > 0 && total > p.reported(reported)) errs[p.total] = LIMITS_DIALOG.ERR_REPORTED(p.unit(p.reported(reported)));
    else if (p.total === "td" && !Number.isInteger(total)) errs.td = LIMITS_DIALOG.ERR_DISK_WHOLE;
    if (!(per > 0)) errs[p.per] = LIMITS_DIALOG.ERR_ABOVE_ZERO;
    else if (!errs[p.total] && per > total) errs[p.per] = LIMITS_DIALOG.ERR_PER_RUN;
    else if (p.per === "pd" && !Number.isInteger(per)) errs.pd = LIMITS_DIALOG.ERR_DISK_WHOLE;
  }
  return errs;
}

const toLimits = (v: Vals): RunnerLimits => ({
  runs_at_once: Number(v.rn),
  total: toAmounts({ cpu: Number(v.tc), mem: Number(v.tm), disk: Number(v.td) }),
  per_run: toAmounts({ cpu: Number(v.pc), mem: Number(v.pm), disk: Number(v.pd) }),
});

/** The limits the dialog opened on, and the values it showed for them. */
export interface LoadedLimits {
  vals: Vals;
  limits: RunnerLimits;
}

/**
 * What a save sends: the fields shown as one-decimal CPUs and GB, except that a field still showing what it
 * loaded goes back exactly as loaded, so an unrelated edit never turns 1250 milli-CPU into 1300.
 */
export function limitsToSave(v: Vals, loaded?: LoadedLimits | null): RunnerLimits {
  const out = toLimits(v);
  if (!loaded) return out;
  const was = loaded.limits;
  const keep = (id: FieldId, now: number, then: number) => (v[id] === loaded.vals[id] ? then : now);
  return {
    runs_at_once: keep("rn", out.runs_at_once, was.runs_at_once),
    total: {
      cpu_millis: keep("tc", out.total.cpu_millis, was.total.cpu_millis),
      memory_mib: keep("tm", out.total.memory_mib, was.total.memory_mib),
      disk_mib: keep("td", out.total.disk_mib, was.total.disk_mib),
    },
    per_run: {
      cpu_millis: keep("pc", out.per_run.cpu_millis, was.per_run.cpu_millis),
      memory_mib: keep("pm", out.per_run.memory_mib, was.per_run.memory_mib),
      disk_mib: keep("pd", out.per_run.disk_mib, was.per_run.disk_mib),
    },
  };
}

function Num({ id, label, vals, errs, reported, onChange }: { id: FieldId; label: React.ReactNode; vals: Vals; errs: Errs; reported?: number; onChange: (id: FieldId, v: string) => void }) {
  const err = errs[id];
  return (
    <div className="min-w-0 space-y-1">
      <label htmlFor={`lm-${id}`} className="block text-xs font-medium text-foreground">
        {label}
      </label>
      <Input
        id={`lm-${id}`}
        type="number"
        min={0}
        step="any"
        inputMode="decimal"
        value={vals[id]}
        aria-invalid={err ? true : undefined}
        aria-describedby={err ? `lm-${id}-e` : undefined}
        onChange={(e) => onChange(id, e.target.value)}
      />
      {reported !== undefined && <span className="block text-meta text-muted-foreground">{LIMITS_DIALOG.REPORTED(reported)}</span>}
      {err && (
        <span id={`lm-${id}-e`} role="alert" className="block text-xs text-danger">
          {err}
        </span>
      )}
    </div>
  );
}

export function LimitsDialog({
  target,
  onOpenChange,
  onSave,
  onSaved,
}: {
  /** The runner whose limits are edited; null when the dialog is closed. */
  target: LimitsTarget | null;
  onOpenChange: (open: boolean) => void;
  onSave: (limits: RunnerLimits) => Promise<unknown>;
  onSaved: () => void;
}) {
  const last = React.useRef(target);
  if (target) last.current = target;
  const t = target ?? last.current;
  const [vals, setVals] = React.useState<Vals>(BLANK);
  const [errs, setErrs] = React.useState<Errs>({});
  const [top, setTop] = React.useState("");
  const [busy, setBusy] = React.useState(false);
  // Per run follows the totals until the person types a per-run value of their own.
  const [auto, setAuto] = React.useState(false);
  const loaded = React.useRef<LoadedLimits | null>(null);
  const open = target !== null;
  React.useEffect(() => {
    if (!target) return;
    const first = initial(target);
    loaded.current = target.limits ? { vals: first, limits: target.limits } : null;
    setVals(first);
    setErrs({});
    setTop("");
    setAuto(!target.limits);
  }, [open]); // eslint-disable-line react-hooks/exhaustive-deps -- a fresh form each time it opens, not on every re-read of the runner
  if (!t) return null;
  const setup = !t.limits;
  const rep = t.self ? t.reported : undefined;

  const change = (id: FieldId, value: string) => {
    const next = { ...vals, [id]: value };
    if (id.startsWith("p")) setAuto(false);
    else if (auto) {
      const c = share(next);
      if (c) Object.assign(next, { pc: String(c.cpu), pm: String(c.mem), pd: String(c.disk) });
    }
    setVals(next);
  };
  const recalc = () => {
    const c = share(vals);
    if (c) setVals({ ...vals, pc: String(c.cpu), pm: String(c.mem), pd: String(c.disk) });
    setAuto(true);
    setErrs({});
    setTop("");
  };
  const submit = async (e: React.FormEvent) => {
    e.preventDefault();
    const found = validateLimits(vals, rep, loaded.current);
    setErrs(found);
    const first = FIELDS.find((f) => found[f]);
    if (first) {
      setTop(found[first] ?? "");
      requestAnimationFrame(() => document.getElementById(`lm-${first}`)?.focus());
      return;
    }
    setBusy(true);
    setTop("");
    try {
      await onSave(limitsToSave(vals, loaded.current));
      toast.success(LIMITS_DIALOG.SAVED(t.name));
      onOpenChange(false);
      onSaved();
    } catch (err) {
      setTop(getErrorMessage(err));
    } finally {
      setBusy(false);
    }
  };
  const c = share(vals);
  const calc = c
    ? LIMITS_DIALOG.CALC(Number(vals.tc), Number(vals.tm), Number(vals.td), Number(vals.rn), c.cpu, c.mem, c.disk)
    : LIMITS_DIALOG.CALC_PENDING;
  const field = (id: FieldId, label: string, reported?: number) => <Num id={id} label={label} vals={vals} errs={errs} reported={reported} onChange={change} />;
  return (
    <Dialog open={open} onOpenChange={(o) => !busy && onOpenChange(o)}>
      <DialogContent className="scroll-thin max-h-[calc(100vh-2rem)] overflow-y-auto sm:max-w-[560px]">
        <form onSubmit={submit} noValidate className="space-y-3">
          <DialogHeader>
            <DialogTitle>{setup ? LIMITS_DIALOG.TITLE_SETUP(t.name) : LIMITS_DIALOG.TITLE(t.name)}</DialogTitle>
            <DialogDescription>{!t.self ? LIMITS_DIALOG.LEDE_REMOTE : setup ? LIMITS_DIALOG.LEDE_SETUP : LIMITS_DIALOG.LEDE_SELF}</DialogDescription>
          </DialogHeader>
          {rep && (
            <p className="text-xs text-muted-foreground">
              {RUNNER_V6.REPORTS(amountsText(rep))} <span>{RUNNER.REPORTED}</span>
            </p>
          )}
          {top && (
            <p role="alert" className="rounded-lg border border-danger bg-danger-subtle p-2.5 text-body text-foreground">
              {top}
            </p>
          )}
          <div className="space-y-1.5">
            <div className="flex items-center gap-1 text-sm font-medium">
              <label htmlFor="lm-rn">{LIMITS_DIALOG.RUNS_AT_ONCE}</label>
              <span aria-hidden className="text-muted-foreground">
                *
              </span>
            </div>
            <div className="max-w-40">
              <Num id="rn" label={<span className="sr-only">{LIMITS_DIALOG.RUNS_AT_ONCE}</span>} vals={vals} errs={errs} onChange={change} />
            </div>
            <p className="text-xs text-muted-foreground">{LIMITS_DIALOG.RUNNING_HINT(t.running)}</p>
          </div>
          <fieldset className="space-y-1.5">
            <legend className="text-sm font-medium">{LIMITS_DIALOG.TOTAL}</legend>
            <div className="grid grid-cols-1 gap-x-3 gap-y-2 sm:grid-cols-3">
              {field("tc", LIMITS_DIALOG.CPU, rep && cpus(rep.cpu_millis))}
              {field("tm", LIMITS_DIALOG.MEMORY, rep && gb(rep.memory_mib))}
              {field("td", LIMITS_DIALOG.DISK, rep && gb(rep.disk_mib))}
            </div>
          </fieldset>
          <fieldset className="space-y-1.5">
            <legend className="text-sm font-medium">{LIMITS_DIALOG.PER_RUN}</legend>
            <div className="grid grid-cols-1 gap-x-3 gap-y-2 sm:grid-cols-3">
              {field("pc", LIMITS_DIALOG.CPU)}
              {field("pm", LIMITS_DIALOG.MEMORY)}
              {field("pd", LIMITS_DIALOG.DISK)}
            </div>
            <p data-calc className="text-xs text-muted-foreground">
              {calc}
            </p>
            <Button type="button" variant="ghost" size="sm" onClick={recalc}>
              {LIMITS_DIALOG.USE_SHARE}
            </Button>
          </fieldset>
          <DialogFooter>
            <Button type="button" variant="ghost" disabled={busy} onClick={() => onOpenChange(false)}>
              {LIMITS_DIALOG.CANCEL}
            </Button>
            <Button type="submit" disabled={busy}>
              {busy && <Loader2 className="size-3.5 animate-spin" />}
              {setup ? LIMITS_DIALOG.SAVE_SETUP : LIMITS_DIALOG.SAVE}
            </Button>
          </DialogFooter>
        </form>
      </DialogContent>
    </Dialog>
  );
}
