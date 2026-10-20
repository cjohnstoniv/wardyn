/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Policies -> Wardyn boundary (board T2) and its dry run (board T4's report anatomy), over
// GET/PUT /governance/boundary and POST /governance/dry-run. Every save shows the dry run first; the PUT goes
// through the governance-writes path (202 -> SubmittedNote, 412 -> reload). While the server refuses
// hard_limits (409 boundary_hard_limits_gated) the refusal is shown in its own words.
//
// EVERY user-visible string comes from lib/boundary-copy.ts, or is canon reused from governance-copy.ts.
import * as React from "react";
import { Link } from "react-router-dom";
import { AlertTriangle, Loader2 } from "lucide-react";
import { HttpError, PendingChangeError } from "../../../lib/api/core";
import { boundary as boundaryApi, type Boundary, type BoundaryChange, type BoundaryMode, type BoundaryReport, type BoundaryWrite } from "../../../lib/api/boundary";
import type { ConfinementClass } from "../../../lib/types/runs";
import { BOUNDARY, BOUNDARY_DRY_RUN } from "../../../lib/boundary-copy";
import { BASELINE, CHANGES } from "../../../lib/governance-copy";
import { ACCESS_STATE } from "../../../lib/people-access-copy";
import { getErrorMessage } from "../../../lib/format";
import { Button } from "../../ui/button";
import { Label } from "../../ui/label";
import { Textarea } from "../../ui/textarea";
import { Chip } from "../../wardyn/primitives";
import { CC_META } from "../../wardyn/cc-meta";
import { useSecurityOperator } from "../../wardyn/operator-context";
import { SavedElsewhereBanner } from "../../wardyn/saved-elsewhere-banner";
import { EmptyState, TableSkeleton } from "../../wardyn/states";
import { Note } from "../governance/display";
import { SubmittedNote } from "../governance/submitted-note";

const CLASSES: ConfinementClass[] = ["CC1", "CC2", "CC3"];

interface Draft {
  mode: BoundaryMode;
  blocked: string;
  floor: ConfinementClass | "";
}

const lines = (text: string) =>
  text
    .split("\n")
    .map((l) => l.trim())
    .filter((l) => l !== "");

const draftFrom = (b: BoundaryWrite): Draft => ({
  mode: b.mode,
  blocked: (b.spec.denied_domains ?? []).join("\n"),
  floor: b.spec.min_confinement_class ?? "",
});

// The PUT body: the draft's knobs over the stored document, so knobs this page doesn't edit are kept.
const bodyFrom = (d: Draft, stored: BoundaryWrite): BoundaryWrite => {
  const blocked = lines(d.blocked);
  return {
    mode: d.mode,
    spec: { ...stored.spec, denied_domains: blocked.length > 0 ? blocked : undefined, min_confinement_class: d.floor || undefined },
    limits: stored.limits,
    ...(stored.contact ? { contact: stored.contact } : {}),
  };
};

const modeLabel = (m: BoundaryMode) => (m === "hard_limits" ? BOUNDARY.MODE_HARD : BOUNDARY.MODE_ORG);
const floorLabel = (c: ConfinementClass | undefined) => CC_META[c ?? "CC1"].label;

type Outcome = { kind: "saved" | "submitted" } | { kind: "refused"; message: string } | null;
type ReportState = { status: "loading" } | { status: "error" } | { status: "ready"; report: BoundaryReport };

export function BoundaryScreen() {
  const securityOperator = useSecurityOperator();
  const [status, setStatus] = React.useState<"loading" | "ready" | "error">("loading");
  const [stored, setStored] = React.useState<BoundaryWrite>({ mode: "no_limits", spec: {}, limits: {} });
  const [draft, setDraft] = React.useState<Draft>({ mode: "no_limits", blocked: "", floor: "" });
  const [report, setReport] = React.useState<ReportState | null>(null);
  const [outcome, setOutcome] = React.useState<Outcome>(null);
  const [busy, setBusy] = React.useState(false);
  const [savedElsewhere, setSavedElsewhere] = React.useState<string | null>(null);
  const [submittedJson, setSubmittedJson] = React.useState<string | null>(null);
  const etagRef = React.useRef<string | null>(null);

  const adopt = (doc: Boundary) => {
    const b: BoundaryWrite = { mode: doc.mode, spec: doc.spec ?? {}, limits: doc.limits ?? {}, ...(doc.contact ? { contact: doc.contact } : {}) };
    setStored(b);
    setDraft(draftFrom(b));
    etagRef.current = doc.etag ?? null;
    setSubmittedJson(null);
  };

  const load = React.useCallback(() => {
    setStatus("loading");
    boundaryApi
      .get()
      .then((doc) => {
        adopt(doc);
        setStatus("ready");
      })
      .catch(() => setStatus("error"));
  }, []);
  React.useEffect(load, [load]);

  const body = React.useMemo(() => bodyFrom(draft, stored), [draft, stored]);
  const json = JSON.stringify(body);
  const dirty = json !== JSON.stringify(bodyFrom(draftFrom(stored), stored)) && json !== submittedJson;

  const edit = (patch: Partial<Draft>) => {
    setOutcome(null);
    setDraft((d) => ({ ...d, ...patch }));
  };

  // The dry run judges the drafted knobs as hard_limits; a draft in no_limits narrows nothing.
  const runDryRun = React.useCallback(() => {
    setReport({ status: "loading" });
    const knobs = body.mode === "hard_limits" ? { spec: body.spec, limits: body.limits } : { spec: {}, limits: {} };
    boundaryApi
      .dryRun(knobs)
      .then((r) => setReport({ status: "ready", report: r }))
      .catch(() => setReport({ status: "error" }));
  }, [body]);

  const review = () => {
    setOutcome(null);
    runDryRun();
  };

  const submit = async () => {
    setBusy(true);
    setOutcome(null);
    try {
      const doc = await boundaryApi.put(body, etagRef.current);
      adopt(doc);
      setReport(null);
      setSavedElsewhere(null);
      setOutcome({ kind: "saved" });
    } catch (e) {
      if (e instanceof HttpError && e.status === 412) {
        setSavedElsewhere(JSON.stringify(body, null, 2));
        setReport(null);
        load();
      } else if (e instanceof PendingChangeError) {
        setSubmittedJson(json);
        setOutcome({ kind: "submitted" });
      } else {
        setOutcome({ kind: "refused", message: e instanceof HttpError ? e.message : getErrorMessage(e) });
      }
    } finally {
      setBusy(false);
    }
  };

  const disabled = !securityOperator || busy;
  const hard = draft.mode === "hard_limits";
  const chip = hard ? (
    <Chip tone="primary">{BOUNDARY.CHIP_HARD}</Chip>
  ) : (
    <Chip tone="warning">{stored.mode === "no_limits" ? BOUNDARY.MODE_ORG : BOUNDARY.CHIP_ORG_DRAFT}</Chip>
  );

  return (
    <div className="mx-auto max-w-[1100px] px-4 py-6 sm:px-6">
      <nav aria-label="Breadcrumb" className="mb-2 flex items-center gap-1.5 text-xs text-muted-foreground">
        <Link to="/admin/policies" className="hover:text-foreground">
          {BOUNDARY.BREADCRUMB_PARENT}
        </Link>
        <span aria-hidden="true">›</span>
        <span>{BOUNDARY.TITLE}</span>
      </nav>
      <div className="flex flex-wrap items-center gap-2">
        <h1 className="text-[1.75rem] font-bold text-foreground">{BOUNDARY.TITLE}</h1>
        {status === "ready" && <span data-testid="boundary-mode-chip">{chip}</span>}
      </div>
      <p className="mt-1 max-w-[82ch] text-body text-muted-foreground">{BOUNDARY.LEAD}</p>

      {status === "loading" && (
        <div className="mt-4">
          <TableSkeleton rows={3} cols={1} />
        </div>
      )}

      {status === "error" && (
        <EmptyState
          icon={AlertTriangle}
          title={BOUNDARY.FETCH_FAILED_TITLE}
          action={
            <Button variant="outline" size="sm" onClick={load}>
              {ACCESS_STATE.FETCH_FAILED_RETRY}
            </Button>
          }
        />
      )}

      {status === "ready" && report === null && (
        <>
          <Rings />

          <section className="mt-4 rounded-xl border border-border bg-card px-5 py-4">
            <fieldset disabled={disabled}>
              <legend className="text-sm font-medium text-foreground">{BOUNDARY.MODE}</legend>
              <div className="mt-2 grid gap-2 md:grid-cols-2">
                <label className="flex gap-2 rounded-lg border border-border p-3">
                  <input type="radio" name="boundary-mode" checked={hard} onChange={() => edit({ mode: "hard_limits" })} />
                  <span className="flex flex-col gap-1">
                    <span className="flex flex-wrap items-center gap-1.5 font-medium">
                      <span>{BOUNDARY.MODE_HARD}</span>
                      <Chip tone="primary">{BOUNDARY.RECOMMENDED}</Chip>
                      <Chip tone="neutral">{BOUNDARY.NEW_INSTALLS}</Chip>
                    </span>
                    <span className="text-xs text-muted-foreground">{BOUNDARY.MODE_HARD_BODY}</span>
                  </span>
                </label>
                <label className="flex gap-2 rounded-lg border border-border p-3">
                  <input type="radio" name="boundary-mode" checked={!hard} onChange={() => edit({ mode: "no_limits" })} />
                  <span className="flex flex-col gap-1">
                    <span className="flex flex-wrap items-center gap-1.5 font-medium">
                      <span>{BOUNDARY.MODE_ORG}</span>
                      <Chip tone="warning">{BOUNDARY.NEEDS_SECOND_ADMIN}</Chip>
                    </span>
                    <span className="text-xs text-muted-foreground">{BOUNDARY.MODE_ORG_BODY}</span>
                  </span>
                </label>
              </div>
            </fieldset>
          </section>

          {hard ? (
            <section data-testid="boundary-hard-limits" className="mt-4 flex flex-col gap-4 rounded-xl border border-border bg-card px-5 py-4">
              <h2 className="text-sm font-medium text-foreground">{BOUNDARY.HARD_HEADING}</h2>
              <div className="flex flex-col gap-1.5">
                <Label htmlFor="boundary-blocked">{BOUNDARY.BLOCKED_LABEL}</Label>
                <Textarea
                  id="boundary-blocked"
                  rows={5}
                  value={draft.blocked}
                  disabled={disabled}
                  aria-describedby="boundary-blocked-hint"
                  onChange={(e) => edit({ blocked: e.target.value })}
                />
                <p id="boundary-blocked-hint" className="text-xs text-muted-foreground">
                  {lines(draft.blocked).length === 0 ? `${BOUNDARY.BLOCKED_EMPTY} ` : ""}
                  {BOUNDARY.BLOCKED_HINT}
                </p>
              </div>
              <div className="flex flex-col gap-1.5">
                <Label htmlFor="boundary-floor">{BOUNDARY.FLOOR_LABEL}</Label>
                <div className="flex flex-wrap items-center gap-2 text-sm">
                  <select
                    id="boundary-floor"
                    className="rounded-md border border-border bg-input-background px-2 py-1"
                    value={draft.floor || "CC1"}
                    disabled={disabled}
                    onChange={(e) => edit({ floor: e.target.value as ConfinementClass })}
                  >
                    {CLASSES.map((c) => (
                      <option key={c} value={c}>
                        {CC_META[c].label}
                      </option>
                    ))}
                  </select>
                  <span className="text-muted-foreground">{BOUNDARY.FLOOR_TAIL(floorLabel(draft.floor || undefined))}</span>
                </div>
              </div>
              <p className="text-xs text-muted-foreground">
                {BOUNDARY.HARD_FOOT}
                <br />
                {BOUNDARY.ALWAYS_BLOCKED}
              </p>
            </section>
          ) : (
            <section data-testid="boundary-org" className="mt-4 flex flex-col gap-2 rounded-xl border border-dashed border-border px-5 py-4">
              <p className="text-body text-foreground">{BOUNDARY.ORG_PANEL}</p>
              <div className="flex flex-wrap items-center gap-2 text-xs text-muted-foreground">
                <Chip tone="warning">{BOUNDARY.NEEDS_SECOND_ADMIN}</Chip>
                <span>{BOUNDARY.ORG_PANEL_NOTE}</span>
              </div>
            </section>
          )}

          <section className="mt-4 flex flex-col gap-2 rounded-xl border border-border bg-card px-5 py-4 text-sm">
            <h2 className="font-medium text-foreground">{BOUNDARY.BINDS_TITLE}</h2>
            <div className="flex flex-wrap items-center gap-2">
              <Chip tone="warning">{BOUNDARY.BINDS_EVERY}</Chip>
              <span className="text-muted-foreground">{BOUNDARY.BINDS_EVERY_BODY}</span>
            </div>
            <div className="flex flex-wrap items-center gap-2">
              <Chip tone="neutral" className="font-mono">
                {BOUNDARY.BREAK_GLASS_TAG}
              </Chip>
              <span className="text-muted-foreground">{BOUNDARY.BREAK_GLASS_BODY}</span>
            </div>
          </section>

          <div className="mt-4 flex flex-col gap-2">
            {savedElsewhere !== null && <SavedElsewhereBanner reloaded documentText={savedElsewhere} onDiscard={() => setSavedElsewhere(null)} />}
            {outcome?.kind === "saved" && <Note role="status">{BOUNDARY.SAVED}</Note>}
            <div className="flex flex-wrap items-center justify-end gap-2">
              <span className="mr-auto text-xs text-muted-foreground">{BOUNDARY.FOOTER}</span>
              <Button variant="ghost" disabled={disabled || !dirty} onClick={() => edit(draftFrom(stored))}>
                {BOUNDARY.DISCARD}
              </Button>
              <Button disabled={disabled || !dirty} onClick={review}>
                {BOUNDARY.REVIEW}
              </Button>
            </div>
          </div>
        </>
      )}

      {status === "ready" && report !== null && (
        <DryRunReport
          stored={stored}
          body={body}
          state={report}
          outcome={outcome}
          canSubmit={!disabled && dirty}
          busy={busy}
          onRetry={runDryRun}
          onBack={() => {
            setOutcome(null);
            setReport(null);
          }}
          onSubmit={() => void submit()}
        />
      )}
    </div>
  );
}

function Rings() {
  return (
    <div aria-label={BOUNDARY.RINGS_LABEL} role="group" className="mt-4 rounded-xl border border-dashed border-border p-3 text-xs">
      <div className="flex flex-wrap gap-2 text-muted-foreground opacity-70">
        <span className="font-medium">{BOUNDARY.ORG_BOUNDARY}</span>
        <span>{BOUNDARY.NOT_VISIBLE}</span>
      </div>
      <div className="mt-2 rounded-lg border border-border bg-card p-3">
        <div className="flex flex-wrap gap-2">
          <span className="font-medium text-foreground">{BOUNDARY.TITLE}</span>
          <span className="text-muted-foreground">{BOUNDARY.RING_BOUNDARY_SUB}</span>
        </div>
        <div className="mt-2 grid gap-2 md:grid-cols-3">
          {(
            [
              [BOUNDARY.RING_TEAMS, BOUNDARY.RING_TEAMS_SUB],
              [BOUNDARY.RING_RESTRICTIONS, BOUNDARY.RING_RESTRICTIONS_SUB],
              [BOUNDARY.RING_RUN, BOUNDARY.RING_RUN_SUB],
            ] as const
          ).map(([name, sub]) => (
            <div key={name} className="flex flex-col gap-0.5 rounded-md border border-border p-2">
              <span className="font-medium text-foreground">{name}</span>
              <span className="text-muted-foreground">{sub}</span>
            </div>
          ))}
        </div>
      </div>
    </div>
  );
}

// A knob value as the report shows it: a list joined, a barrier by its name, an unset value as "Not set".
function showValue(knob: string, v: unknown): string {
  if (v === null || v === undefined || (Array.isArray(v) && v.length === 0)) return CHANGES.DIFF_UNSET;
  if (knob === "min_confinement_class" && typeof v === "string" && v in CC_META) return CC_META[v as ConfinementClass].label;
  if (Array.isArray(v)) return v.map((x) => (typeof x === "string" ? x : JSON.stringify(x))).join(", ");
  return typeof v === "string" ? v : JSON.stringify(v);
}

function Changes({ changes }: { changes: BoundaryChange[] }) {
  return (
    <ul className="mt-1 flex flex-col gap-0.5 text-xs text-muted-foreground">
      {changes.map((c) => (
        <li key={c.knob}>
          {BOUNDARY_DRY_RUN.KNOB[c.knob] ?? c.knob}: {showValue(c.knob, c.before)} → {showValue(c.knob, c.after)}
        </li>
      ))}
    </ul>
  );
}

function ReportRows({ title, rows }: { title: string; rows: { key: string; name: string; changes: BoundaryChange[] }[] }) {
  if (rows.length === 0) return null;
  return (
    <section className="mt-4">
      <h3 className="text-sm font-medium text-foreground">{title}</h3>
      <ul className="mt-2 flex flex-col gap-2">
        {rows.map((r) => (
          <li key={r.key} className="rounded-lg border border-border px-3 py-2">
            <div className="flex flex-wrap items-center gap-2">
              <span className="font-medium text-foreground">{r.name}</span>
              <Chip tone="warning">{BOUNDARY_DRY_RUN.NARROWED}</Chip>
            </div>
            <Changes changes={r.changes} />
          </li>
        ))}
      </ul>
    </section>
  );
}

function DryRunReport({
  stored,
  body,
  state,
  outcome,
  canSubmit,
  busy,
  onRetry,
  onBack,
  onSubmit,
}: {
  stored: BoundaryWrite;
  body: BoundaryWrite;
  state: ReportState;
  outcome: Outcome;
  canSubmit: boolean;
  busy: boolean;
  onRetry: () => void;
  onBack: () => void;
  onSubmit: () => void;
}) {
  const refusedRef = React.useRef<HTMLHeadingElement>(null);
  React.useEffect(() => {
    if (outcome?.kind === "refused") refusedRef.current?.focus();
  }, [outcome]);

  const changeRows: [string, string, string][] = [
    [BOUNDARY_DRY_RUN.MODE, modeLabel(stored.mode), modeLabel(body.mode)],
    [BOUNDARY.BLOCKED_LABEL, showValue("denied_domains", stored.spec.denied_domains), showValue("denied_domains", body.spec.denied_domains)],
    [BOUNDARY.FLOOR_LABEL, floorLabel(stored.spec.min_confinement_class), floorLabel(body.spec.min_confinement_class)],
  ];

  return (
    <section data-testid="boundary-dry-run" className="mt-4 rounded-xl border border-border bg-card px-5 py-4">
      <h2 className="text-lg font-semibold text-foreground">{BOUNDARY_DRY_RUN.TITLE}</h2>
      <p className="mt-1 max-w-[82ch] text-body text-muted-foreground">{BOUNDARY_DRY_RUN.LEAD}</p>

      <h3 className="mt-4 text-sm font-medium text-foreground">{BOUNDARY_DRY_RUN.CHANGE}</h3>
      <table className="mt-2 w-full text-left text-xs">
        <thead className="text-muted-foreground">
          <tr>
            <th className="py-1 font-medium">{BOUNDARY.TITLE}</th>
            <th className="py-1 font-medium">{BOUNDARY_DRY_RUN.TODAY}</th>
            <th className="py-1 font-medium">{BOUNDARY_DRY_RUN.AFTER}</th>
          </tr>
        </thead>
        <tbody>
          {changeRows.map(([name, today, after]) => (
            <tr key={name} className="border-t border-border">
              <td className="py-1 pr-2 font-medium text-foreground">{name}</td>
              <td className="py-1 pr-2">{today}</td>
              <td className="py-1">{after}</td>
            </tr>
          ))}
        </tbody>
      </table>

      {state.status === "loading" && (
        <div className="mt-4">
          <TableSkeleton rows={2} cols={1} />
        </div>
      )}

      {state.status === "error" && (
        <Note tone="red" role="alert">
          <b className="font-semibold">{BOUNDARY_DRY_RUN.FAILED_TITLE}</b>
          <span>{BOUNDARY_DRY_RUN.FAILED_BODY}</span>
          <Button variant="outline" size="sm" onClick={onRetry}>
            {BOUNDARY_DRY_RUN.RETRY}
          </Button>
        </Note>
      )}

      {state.status === "ready" && <ReportBody report={state.report} />}

      <p className="mt-4 text-xs text-muted-foreground">{BOUNDARY_DRY_RUN.COMPARED}</p>

      <div className="mt-3 flex flex-col gap-2">
        {outcome?.kind === "submitted" && <SubmittedNote />}
        {outcome?.kind === "refused" && (
          <Note tone="red" role="alert">
            <h3 ref={refusedRef} tabIndex={-1} className="font-semibold outline-none">
              {BASELINE.REFUSED_TITLE}
            </h3>
            <span>{outcome.message}</span>
          </Note>
        )}
        <div className="flex flex-wrap items-center justify-end gap-2">
          {body.mode !== stored.mode && <Chip tone="warning">{BOUNDARY.NEEDS_SECOND_ADMIN}</Chip>}
          <Button variant="ghost" onClick={onBack}>
            {BOUNDARY_DRY_RUN.BACK}
          </Button>
          <Button disabled={!canSubmit || state.status !== "ready"} onClick={onSubmit}>
            {busy ? <Loader2 className="size-4 animate-spin" /> : null}
            {BOUNDARY_DRY_RUN.SUBMIT}
          </Button>
        </div>
      </div>
    </section>
  );
}

function ReportBody({ report }: { report: BoundaryReport }) {
  const teams = [
    ...(report.deployment.length > 0 ? [{ key: "deployment", name: BOUNDARY_DRY_RUN.DEFAULT_TEAM, changes: report.deployment }] : []),
    ...report.profiles.map((p) => ({ key: p.id, name: p.name, changes: p.changes })),
  ];
  const policies = report.stored_policies.map((p) => ({ key: p.id, name: p.name, changes: p.changes }));
  const people = report.assignments.map((a) => ({ key: a.id, name: `${a.subject} · ${a.profile_name}`, changes: a.changes }));
  const none = teams.length + policies.length + people.length === 0;
  return (
    <>
      <div className="mt-4 grid grid-cols-3 gap-2" data-testid="boundary-dry-run-tiles">
        {(
          [
            [teams.length, BOUNDARY_DRY_RUN.TILE_TEAMS],
            [policies.length, BOUNDARY_DRY_RUN.TILE_POLICIES],
            [people.length, BOUNDARY_DRY_RUN.TILE_PEOPLE],
          ] as const
        ).map(([n, label]) => (
          <div key={label} className="rounded-lg border border-border px-3 py-2">
            <div className="text-lg font-semibold text-foreground">{n}</div>
            <div className="text-xs text-muted-foreground">{label}</div>
          </div>
        ))}
      </div>
      {none ? (
        <div className="mt-4">
          <Chip tone="neutral">{BOUNDARY_DRY_RUN.NO_CHANGE}</Chip>
        </div>
      ) : (
        <>
          <ReportRows title={BOUNDARY_DRY_RUN.TEAMS} rows={teams} />
          <ReportRows title={BOUNDARY_DRY_RUN.POLICIES} rows={policies} />
          <ReportRows title={BOUNDARY_DRY_RUN.PEOPLE} rows={people} />
        </>
      )}
    </>
  );
}
