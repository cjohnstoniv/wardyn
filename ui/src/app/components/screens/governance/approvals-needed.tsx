/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Governance -> Approvals needed: how many people must approve each kind of governance change, against the
// deploy-time minimum, with each kind's pool of people who can approve it. The counts are one document: a save
// sends every own row, and the server decides what it accepts, so the only limits here are the number box's own.
// Every edit is a governance change itself: a held save shows the submitted-for-approval note (202), and the
// counts reload once the admin token applies one at once (200).
//
// EVERY user-visible string comes from approvals-needed-copy.ts, or CHANGES for a kind's label.
import * as React from "react";
import { AlertTriangle, Loader2 } from "lucide-react";
import { HttpError, PendingChangeError } from "../../../lib/api/core";
import { governance as api } from "../../../lib/api/governance";
import type { ApprovalCounts, ApprovalCountsKind, ApprovalCountsRule, ApprovalCountsWrite } from "../../../lib/types/approval-counts";
import type { GovernanceChange } from "../../../lib/types";
import { APPROVALS_NEEDED as COPY } from "../../../lib/approvals-needed-copy";
import { getErrorMessage } from "../../../lib/format";
import { CHANGES } from "../../../lib/governance-copy";
import { ACCESS_STATE } from "../../../lib/people-access-copy";
import { useDeferredBusy } from "../../../lib/use-deferred-busy";
import { Button } from "../../ui/button";
import { Input } from "../../ui/input";
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "../../ui/table";
import { INLINE_ACTIONS } from "../../wardyn/form-actions";
import { Switch } from "../../wardyn/form-primitives";
import { EmptyState, TableSkeleton } from "../../wardyn/states";
import { Note } from "./display";
import { SubmittedNote } from "./submitted-note";

// The most approvals a rule can ask for.
const MAX_APPROVALS = 9;

// A rule as the boxes hold it: the count is the text typed, so a half-typed number is not forced to a value.
interface RuleDraft {
  required: string;
  require_security: boolean;
}
interface Draft {
  default: RuleDraft;
  kinds: Record<string, RuleDraft>;
}

const ruleDraft = (r: ApprovalCountsRule): RuleDraft => ({ required: String(r.required), require_security: r.require_security });
const draftFrom = (c: ApprovalCounts): Draft => ({
  default: ruleDraft(c.default),
  kinds: Object.fromEntries(c.kinds.filter((k) => k.own).map((k) => [k.kind, ruleDraft(k)])),
});
const rule = (d: RuleDraft): ApprovalCountsRule => ({ required: Number(d.required), require_security: d.require_security });
// The PUT body: the default and the own rows only, in the order the server listed them.
const bodyFrom = (d: Draft, own: string[]): ApprovalCountsWrite => ({
  default: rule(d.default),
  kinds: own.map((kind) => ({ kind, ...rule(d.kinds[kind]) })),
});

// The count and, for a kind counted in its smallest unit, the unit; then what the status adds.
function Pool({ row }: { row: ApprovalCountsKind }) {
  const { status, count, unit_name: unit, why } = row.pool;
  const label = CHANGES.KIND[row.kind] ?? row.kind;
  return (
    <div className="flex flex-col gap-1">
      <span>{status === "cant_confirm" ? COPY.CANT_CONFIRM : unit ? COPY.IN_SMALLEST_UNIT(count, unit) : count}</span>
      {status === "exact" && <span className="max-w-[48ch] whitespace-normal text-xs text-warning">{COPY.EXACT(count)}</span>}
      {status === "short" && <span className="max-w-[48ch] whitespace-normal text-xs text-danger">{COPY.SHORT(count, label, row.required)}</span>}
      {status === "cant_confirm" && why && (
        <span className="max-w-[48ch] whitespace-normal text-xs text-muted-foreground">
          {why === "too_many_people" ? COPY.CANT_CONFIRM_TOO_MANY : COPY.CANT_CONFIRM_UNCONFIRMED}
        </span>
      )}
    </div>
  );
}

// The sentence with its {env} slot filled by the variable's name, in mono.
function WithEnv({ text }: { text: string }) {
  const [before, after] = text.split(COPY.ENV_SLOT);
  return (
    <>
      {before}
      <code className="font-mono text-xs">WARDYN_GOVERNANCE_MIN_APPROVALS</code>
      {after}
    </>
  );
}

export function ApprovalsNeededTab({ onSubmitted }: { onSubmitted?: () => void }) {
  const [status, setStatus] = React.useState<"loading" | "ready" | "error">("loading");
  const [counts, setCounts] = React.useState<ApprovalCounts | null>(null);
  const [draft, setDraft] = React.useState<Draft | null>(null);
  const [outcome, setOutcome] = React.useState<{ kind: "submitted"; change: GovernanceChange } | { kind: "refused"; message: string; next: boolean } | null>(null);
  const [busy, setBusy] = React.useState(false);
  // The body last held for approval: until a box changes again it is not a new change.
  const [submittedJson, setSubmittedJson] = React.useState<string | null>(null);
  const { showSpinner } = useDeferredBusy(busy);
  const refusedRef = React.useRef<HTMLHeadingElement>(null);

  const load = React.useCallback(() => {
    setStatus("loading");
    api
      .getApprovalCounts()
      .then((c) => {
        setCounts(c);
        setDraft(draftFrom(c));
        setSubmittedJson(null);
        setStatus("ready");
      })
      .catch(() => setStatus("error"));
  }, []);
  React.useEffect(load, [load]);
  React.useEffect(() => {
    if (outcome?.kind === "refused") refusedRef.current?.focus();
  }, [outcome]);

  const ownKinds = React.useMemo(() => (counts?.kinds ?? []).filter((k) => k.own), [counts]);
  const own = React.useMemo(() => ownKinds.map((k) => k.kind), [ownKinds]);
  const body = draft ? bodyFrom(draft, own) : null;
  const json = JSON.stringify(body);
  const dirty = !!counts && !!draft && json !== JSON.stringify(bodyFrom(draftFrom(counts), own)) && json !== submittedJson;
  // A count outside the number box's own bounds cannot be sent; every other refusal is the server's.
  const floor = Math.max(1, counts?.minimum ?? 0);
  const inBounds = (d: RuleDraft) => Number.isInteger(Number(d.required)) && d.required !== "" && Number(d.required) >= floor && Number(d.required) <= MAX_APPROVALS;
  const valid = !!draft && inBounds(draft.default) && own.every((k) => inBounds(draft.kinds[k]));
  // With no minimum set the table can be read and not changed.
  const readOnly = !counts || counts.minimum === 0;

  const edit = (apply: (d: Draft) => Draft) => {
    setOutcome(null);
    setDraft((d) => (d ? apply(d) : d));
  };
  const editRow = (kind: string | null, patch: Partial<RuleDraft>) =>
    edit((d) => (kind === null ? { ...d, default: { ...d.default, ...patch } } : { ...d, kinds: { ...d.kinds, [kind]: { ...d.kinds[kind], ...patch } } }));

  const save = async () => {
    if (!body) return;
    setBusy(true);
    setOutcome(null);
    try {
      await api.putApprovalCounts(body);
      load();
    } catch (e) {
      if (e instanceof PendingChangeError) {
        setSubmittedJson(json);
        setOutcome({ kind: "submitted", change: e.change });
        onSubmitted?.();
      } else {
        setOutcome({ kind: "refused", message: e instanceof HttpError ? e.message : getErrorMessage(e), next: e instanceof HttpError && e.status === 422 });
      }
    } finally {
      setBusy(false);
    }
  };

  const rowInputs = (kind: string | null, d: RuleDraft, label: string) => (
    <>
      <TableCell>
        <Input
          type="number"
          className="w-20"
          min={floor}
          max={MAX_APPROVALS}
          step={1}
          value={d.required}
          disabled={readOnly || busy}
          aria-label={COPY.ROW_FIELD(label, COPY.COL_APPROVALS)}
          onChange={(e) => editRow(kind, { required: e.target.value })}
        />
      </TableCell>
      <TableCell>
        <Switch
          checked={d.require_security}
          disabled={readOnly || busy}
          label={COPY.ROW_FIELD(label, COPY.COL_SECURITY)}
          onChange={(next) => editRow(kind, { require_security: next })}
        />
      </TableCell>
    </>
  );

  return (
    <section className="mt-6 overflow-hidden rounded-xl border border-border bg-card">
      <div className="px-6 pt-5">
        <h2 className="text-sm font-medium text-foreground">{COPY.TITLE}</h2>
        <p className="mt-1 max-w-[82ch] text-body text-muted-foreground">{COPY.LEAD}</p>
      </div>

      {status === "loading" && !counts && (
        <div className="mt-4">
          <TableSkeleton rows={3} cols={5} />
        </div>
      )}

      {status === "error" && (
        <EmptyState
          icon={AlertTriangle}
          title={COPY.FETCH_FAILED_TITLE}
          description={COPY.FETCH_FAILED_BODY}
          action={
            <Button variant="outline" size="sm" onClick={load}>
              {ACCESS_STATE.FETCH_FAILED_RETRY}
            </Button>
          }
        />
      )}

      {counts && draft && status !== "error" && (
        <>
          <div className="mt-4 flex flex-wrap items-center gap-x-4 gap-y-1 border-t border-border px-6 py-4">
            <label htmlFor="approvals-minimum" className="text-sm font-medium text-foreground">
              {COPY.MIN_LABEL}
            </label>
            <Input id="approvals-minimum" type="number" className="w-20" value={counts.minimum} readOnly disabled aria-describedby="approvals-minimum-hint" />
            <p id="approvals-minimum-hint" className="text-xs text-muted-foreground">
              {COPY.MIN_HINT}
            </p>
          </div>
          {counts.minimum === 0 && (
            <div className="px-6">
              <Note>
                <span data-testid="approvals-minimum-zero">
                  <WithEnv text={COPY.MIN_ZERO} />
                </span>
              </Note>
            </div>
          )}

          <Table>
            <TableHeader>
              <TableRow className="hover:bg-transparent">
                <TableHead>{COPY.COL_KIND}</TableHead>
                <TableHead>{COPY.COL_APPROVALS}</TableHead>
                <TableHead>{COPY.COL_SECURITY}</TableHead>
                <TableHead>{COPY.COL_WHO}</TableHead>
                <TableHead>{COPY.COL_NOW}</TableHead>
              </TableRow>
            </TableHeader>
            <TableBody>
              {ownKinds.map((k) => (
                <TableRow key={k.kind} data-testid="approvals-row">
                  <TableCell className="font-medium">{CHANGES.KIND[k.kind] ?? k.kind}</TableCell>
                  {rowInputs(k.kind, draft.kinds[k.kind], CHANGES.KIND[k.kind] ?? k.kind)}
                  <TableCell>{COPY.WHO[k.approvers] ?? k.approvers}</TableCell>
                  <TableCell>
                    <Pool row={k} />
                  </TableCell>
                </TableRow>
              ))}
              <TableRow data-testid="approvals-default-row">
                <TableCell className="font-medium">{COPY.EVERY_OTHER_KIND}</TableCell>
                {rowInputs(null, draft.default, COPY.EVERY_OTHER_KIND)}
                <TableCell className="text-muted-foreground">{counts.default.required === counts.minimum ? COPY.DEFAULT_COUNT : null}</TableCell>
                <TableCell />
              </TableRow>
            </TableBody>
          </Table>

          <div className="flex flex-col gap-2 px-6 py-4">
            <p className="text-xs text-muted-foreground">{COPY.NEVER_SIGNED_IN}</p>
            <p className="text-xs text-muted-foreground">{COPY.NOTE_HELD(counts.highest)}</p>
            <p className="text-xs text-muted-foreground">{COPY.NOTE_REQUESTS}</p>
            {outcome?.kind === "submitted" && <SubmittedNote change={outcome.change} />}
            {outcome?.kind === "refused" && (
              <Note tone="red" role="alert">
                <h3 ref={refusedRef} tabIndex={-1} className="font-semibold outline-none">
                  {COPY.REFUSED_TITLE}
                </h3>
                <span>{outcome.message}</span>
                {outcome.next && <span>{COPY.REFUSED_NEXT}</span>}
              </Note>
            )}
            {!readOnly && (
              <div className={INLINE_ACTIONS}>
                <Button disabled={busy || !dirty || !valid} onClick={() => void save()}>
                  {showSpinner ? <Loader2 className="size-4 animate-spin" /> : null}
                  {COPY.SAVE}
                </Button>
              </div>
            )}
          </div>
        </>
      )}
    </section>
  );
}
