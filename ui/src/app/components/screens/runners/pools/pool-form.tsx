/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The pool form page (mock M-POOLS v6 poolForm): an organization pool's edit (admin) and a user pool's create and
// edit (mine). The form narrows its runners' limits and never widens them; what a pool takes on its own is the
// run types, the barriers, Runs at once and the per-run amounts and durations. Nothing is checked against the
// runners here: a wider limit is accepted and never takes effect. A save that lost a race (409
// runner_pool_stale) shows the server's sentence and a Reload that re-reads the pool into the form.
import * as React from "react";
import { useNavigate, useParams } from "react-router-dom";
import { toast } from "sonner";
import { HttpError } from "../../../../lib/api/core";
import { runnerPools } from "../../../../lib/api/runner-pools";
import { getErrorMessage } from "../../../../lib/format";
import type { RunnerPool, RunnerPoolMemberView } from "../../../../lib/types/runner-pools";
import { POOL_DRAWER, POOL_FORM, POOLS } from "../../../wardyn/copy/runner-pools";
import { Field } from "../../../wardyn/form-primitives";
import { PageHeader } from "../../../wardyn/page-header";
import { Button } from "../../../ui/button";
import { Checkbox } from "../../../ui/checkbox";
import { Input } from "../../../ui/input";
import { Label } from "../../../ui/label";
import {
  BARRIER_FIELDS,
  fromPool,
  runnersBiggest,
  runsTotal,
  toLimits,
  validate,
  type FormErrors,
  type FormValues,
  type RtValues,
} from "./pool-form-model";
import { RtSection } from "./pool-form-sections";

const STALE = "runner_pool_stale";

function Check({ id, label, checked, onChange }: { id: string; label: string; checked: boolean; onChange: (c: boolean) => void }) {
  return (
    <div className="inline-flex items-center gap-2">
      <Checkbox id={id} checked={checked} onCheckedChange={(c) => onChange(c === true)} />
      <Label htmlFor={id} className="font-normal">
        {label}
      </Label>
    </div>
  );
}

function Group({ label, hint, error, children }: { label: string; hint: string; error?: string; children: React.ReactNode }) {
  const id = React.useId();
  return (
    <div role="group" aria-labelledby={id} className="space-y-2">
      <div className="flex items-center gap-1">
        <span id={id} className="text-sm font-medium">
          {label}
        </span>
        <span aria-hidden className="text-muted-foreground">
          *
        </span>
      </div>
      <div className="flex flex-wrap gap-x-4.5 gap-y-2">{children}</div>
      <p className="text-xs leading-snug text-muted-foreground">{hint}</p>
      {error && (
        <p role="alert" className="text-xs text-danger">
          {error}
        </p>
      )}
    </div>
  );
}

export function PoolFormScreen({ scope }: { scope: "admin" | "mine" }) {
  const { id } = useParams();
  const navigate = useNavigate();
  const mine = scope === "mine";
  const [pool, setPool] = React.useState<RunnerPool | null>(null);
  const [members, setMembers] = React.useState<RunnerPoolMemberView[]>([]);
  const [loaded, setLoaded] = React.useState(!id);
  const [loadFailed, setLoadFailed] = React.useState("");
  const [values, setValues] = React.useState<FormValues>(() => fromPool());
  const [errors, setErrors] = React.useState<FormErrors>({});
  const [top, setTop] = React.useState("");
  const [stale, setStale] = React.useState(false);
  const [saving, setSaving] = React.useState(false);

  const load = React.useCallback(() => {
    if (!id) return Promise.resolve();
    return Promise.all([runnerPools.get(id), runnerPools.members(id)])
      .then(([p, m]) => {
        setPool(p);
        setMembers(m);
        setValues(fromPool(p));
        setErrors({});
        setTop("");
        setStale(false);
        setLoadFailed("");
        setLoaded(true);
      })
      .catch((e: unknown) => {
        setLoadFailed(getErrorMessage(e));
        // After the first load the form is on screen, so a failed re-read (Reload after a stale save) says so there.
        setTop(POOL_FORM.RELOAD_FAILED);
      });
  }, [id]);
  React.useEffect(() => {
    void load();
  }, [load]);

  const back = () => navigate(mine ? "/account#my-runner" : "/admin/runners?tab=pools");
  const set = (patch: Partial<FormValues>) => setValues((v) => ({ ...v, ...patch }));
  const patchRt = (k: "bg" | "it") => (patch: Partial<RtValues>) => setValues((v) => ({ ...v, [k]: { ...v[k], ...patch } }));

  const submit = async (ev: React.FormEvent) => {
    ev.preventDefault();
    setTop("");
    setStale(false);
    const found = validate(values, pool?.limits);
    setErrors(found);
    if (Object.keys(found).length) {
      // The invalid control takes focus once the errors have rendered.
      requestAnimationFrame(() => document.querySelector<HTMLElement>("[aria-invalid=true]")?.focus());
      return;
    }
    const name = values.name;
    const limits = toLimits(values, pool?.limits);
    setSaving(true);
    try {
      if (!pool) await runnerPools.createMine({ name, limits });
      else if (mine) await runnerPools.updateMine(pool.id, { revision: pool.revision, name, limits });
      else await runnerPools.update(pool.id, { revision: pool.revision, name, limits });
      toast.success(pool ? POOL_FORM.SAVED(name) : POOL_FORM.CREATED(name));
      void back();
    } catch (e) {
      setTop(getErrorMessage(e));
      setStale(e instanceof HttpError && e.status === 409 && e.reason === STALE);
    } finally {
      setSaving(false);
    }
  };

  const wrap = mine ? "mx-auto max-w-[900px] px-6 py-8" : "mx-auto max-w-[1200px] px-6 py-6";
  const title = pool ? POOL_FORM.TITLE_EDIT(pool.name) : POOL_FORM.TITLE_NEW;
  const backButton = (
    <Button variant="ghost" size="sm" type="button" onClick={back}>
      {mine ? POOL_FORM.BACK_MINE : POOL_FORM.BACK_POOLS}
    </Button>
  );
  if (!loaded) {
    return (
      <div className={wrap}>
        {loadFailed ? (
          <div className="space-y-3">
            <div role="alert" className="rounded-lg border border-danger bg-danger-subtle p-3 text-body">
              {POOLS.ERROR_BODY}
            </div>
            <div className="flex gap-2">
              <Button variant="outline" size="sm" type="button" onClick={() => void load()}>
                {POOL_FORM.RELOAD}
              </Button>
              {backButton}
            </div>
          </div>
        ) : (
          <div role="status" aria-label={POOLS.LOADING}>
            <span className="sr-only">{POOLS.LOADING}</span>
          </div>
        )}
      </div>
    );
  }

  const biggest = runnersBiggest(members);
  const total = runsTotal(members);
  const topError = top || Object.values(errors)[0];
  return (
    <div className={wrap}>
      <PageHeader title={title} description={<span className="block max-w-[68ch]">{POOL_FORM.LEDE}</span>} />
      <div className="mb-3">{backButton}</div>
      {topError && (
        <div role="alert" id="f-top" className="mb-3 rounded-lg border border-danger bg-danger-subtle p-3 text-body">
          {topError}
          {stale && (
            <>
              {" "}
              <span className="text-muted-foreground">{POOL_FORM.STALE_HINT}</span>{" "}
              <Button variant="outline" size="sm" type="button" onClick={() => void load()}>
                {POOL_FORM.RELOAD}
              </Button>
            </>
          )}
        </div>
      )}
      <form noValidate onSubmit={(e) => void submit(e)} className="max-w-[760px] space-y-4">
        <Field label={POOL_FORM.NAME} htmlFor="f-name" required hint={mine ? POOL_FORM.NAME_HINT_MINE : POOL_FORM.NAME_HINT_ORG}>
          <Input
            id="f-name"
            required
            maxLength={80}
            autoComplete="off"
            value={values.name}
            aria-invalid={errors.name ? true : undefined}
            aria-describedby={errors.name ? "f-name-e" : undefined}
            onChange={(e) => set({ name: e.target.value })}
          />
        </Field>
        {errors.name && (
          <p id="f-name-e" role="alert" className="-mt-2 text-xs text-danger">
            {errors.name}
          </p>
        )}
        <div className="space-y-1">
          <span className="text-sm font-medium">{POOL_FORM.POOL_TYPE}</span>
          <p className="text-sm">
            {mine ? POOLS.USER_POOL : POOLS.ORG_POOL} {mine ? POOL_FORM.TYPE_MINE : POOL_FORM.TYPE_ORG}
          </p>
          {!mine && <p className="text-xs text-muted-foreground">{POOL_DRAWER.ORG_ONLY}</p>}
        </div>
        <Group label={POOL_FORM.RUN_TYPES} hint={POOL_FORM.RUN_TYPES_HINT} error={errors.types}>
          <Check id="f-bg-on" label={POOL_DRAWER.BACKGROUND} checked={values.bgOn} onChange={(c) => set({ bgOn: c })} />
          <Check id="f-it-on" label={POOL_DRAWER.INTERACTIVE} checked={values.itOn} onChange={(c) => set({ itOn: c })} />
        </Group>
        <Group label={POOL_FORM.BARRIERS} hint={POOL_FORM.BARRIERS_HINT} error={errors.barriers}>
          {BARRIER_FIELDS.map((b) => (
            <Check key={b.id} id={`f-${b.id}`} label={b.label} checked={values[b.id]} onChange={(c) => set({ [b.id]: c })} />
          ))}
        </Group>
        <div className="space-y-2">
          <Label htmlFor="f-maxruns">
            {POOL_FORM.MAX_RUNS} <span className="font-normal text-muted-foreground">{POOL_FORM.NARROWER}</span>
          </Label>
          <div className="flex max-w-[260px] items-center gap-1.5">
            <Input
              id="f-maxruns"
              type="number"
              min={1}
              step={1}
              value={values.maxruns}
              aria-invalid={errors.maxruns ? true : undefined}
              aria-describedby={errors.maxruns ? "f-maxruns-e f-maxruns-h" : "f-maxruns-h"}
              onChange={(e) => set({ maxruns: e.target.value })}
            />
            <span className="text-xs text-muted-foreground">{POOL_FORM.RUNS_UNIT}</span>
          </div>
          <p id="f-maxruns-h" className="text-xs text-muted-foreground">
            {total > 0 ? POOL_FORM.MAX_RUNS_HINT_EDIT(total) : POOL_FORM.MAX_RUNS_HINT_NEW}
          </p>
          {errors.maxruns && (
            <p id="f-maxruns-e" role="alert" className="text-xs text-danger">
              {errors.maxruns}
            </p>
          )}
        </div>
        {values.bgOn && <RtSection rt="bg" values={values.bg} errors={errors} biggest={biggest} onPatch={patchRt("bg")} />}
        {values.itOn && <RtSection rt="it" values={values.it} errors={errors} biggest={biggest} onPatch={patchRt("it")} />}
        <div className="flex flex-wrap gap-2">
          <Button type="submit" disabled={saving}>
            {pool ? POOL_FORM.SAVE : POOL_FORM.CREATE}
          </Button>
          <Button type="button" variant="ghost" onClick={back}>
            {POOL_FORM.CANCEL}
          </Button>
        </div>
      </form>
    </div>
  );
}
