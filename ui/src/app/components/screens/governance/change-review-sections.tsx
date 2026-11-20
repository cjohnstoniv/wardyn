/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The sections that tell the person deciding what a change does before the raw diff: the verdict and its sentence, the
// impact, the risk, and the raw diff last. Each takes the change and, for the impact, the server's impact
// result, and holds no layout of the drawer, so any page can place them.
//
// Every string is from CHANGE_REVIEW (change-review-copy.ts) or the canon it reads; this file adds none.
import type { ReactNode } from "react";
import { BOUNDARY_DRY_RUN } from "../../../lib/boundary-copy";
import { alsoChanges, fieldLabel, reviewSentence, showValue, valueAt, changeTarget } from "../../../lib/change-review";
import { CHANGE_REVIEW, entriesLabel } from "../../../lib/change-review-copy";
import type {
  BoundaryChange,
  BoundaryReport,
  BoundaryReportAssign,
} from "../../../lib/api/boundary";
import { absoluteTime, relativeTime } from "../../../lib/format";
import { CHANGES } from "../../../lib/governance-copy";
import type {
  ChangeImpact,
  ChangeReview,
  ChangeVerdict,
  GovernanceChange,
  HeldBecause,
  RiskItem,
  RiskLevel,
} from "../../../lib/types";
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "../../ui/table";
import { Mono } from "../../wardyn/code-block";
import { Chip } from "../../wardyn/primitives";
import { Note } from "./display";

// How many people and live runs the impact lists before it counts the rest.
const SHOWN = 3;

const VERDICT_TONE: Record<ChangeVerdict, "warning" | "info" | "neutral"> = {
  widens: "warning",
  narrows_held: "info",
  not_checked: "neutral",
};
const RISK_TONE: Record<RiskLevel, "success" | "warning" | "danger"> = { low: "success", medium: "warning", high: "danger" };
const LEVELS: RiskLevel[] = ["low", "medium", "high"];

const heldText = (r: HeldBecause | undefined): string => (r ? CHANGE_REVIEW.HELD_BECAUSE[r] ?? r : "");

// The verdict as a chip. An absent review reads as not_checked; a narrowing change that is held names `reason`, or
// the first reason the server gave.
export function VerdictChip({ review, reason }: { review: ChangeReview | undefined; reason?: HeldBecause }) {
  const verdict = review?.verdict ?? "not_checked";
  const label =
    verdict === "narrows_held"
      ? CHANGE_REVIEW.VERDICT.narrows_held(heldText(reason ?? review?.held_because?.[0]))
      : CHANGE_REVIEW.VERDICT[verdict];
  return (
    <Chip tone={VERDICT_TONE[verdict]} className="whitespace-normal text-left" title={label}>
      {label}
    </Chip>
  );
}

// The risk grade before and after, as a chip; a create has no earlier grade.
export function RiskChip({ risk }: { risk: NonNullable<ChangeReview["risk"]> }) {
  const after = CHANGE_REVIEW.RISK_LEVEL[risk.after.overall];
  const label = risk.before
    ? CHANGE_REVIEW.RISK_CHIP(CHANGE_REVIEW.RISK_LEVEL[risk.before.overall], after)
    : CHANGE_REVIEW.RISK_CHIP_NEW(after);
  return <Chip tone={RISK_TONE[risk.after.overall]}>{label}</Chip>;
}

function Heading({ children }: { children: ReactNode }) {
  return <h4 className="text-body font-medium text-foreground">{children}</h4>;
}

// A field by its plain name with the path beside it, or the path alone when the console has no name for it.
function FieldName({ path }: { path: string }) {
  const label = fieldLabel(path);
  return (
    <span className="inline-flex flex-wrap items-baseline gap-x-2">
      {label && <span className="font-medium text-foreground">{label}</span>}
      <Mono className="text-foreground">{path}</Mono>
    </span>
  );
}

// The verdict with the one sentence that says what the change does: the widened fields and what they add, then
// the other fields it changes.
export function VerdictBlock({ change }: { change: GovernanceChange }) {
  const review = change.diff.review;
  const verdict = review?.verdict ?? "not_checked";
  const sentence = reviewSentence(change);
  const also = alsoChanges(change);
  const widens = verdict === "widens" ? (review?.widens ?? []) : [];
  const reasons = verdict === "narrows_held" ? (review?.held_because ?? []) : [];
  return (
    <section data-testid="change-verdict" className="space-y-2">
      <div className="flex flex-wrap items-center gap-2">
        {reasons.length > 0 ? (
          reasons.map((r) => <VerdictChip key={r} review={review} reason={r} />)
        ) : (
          <VerdictChip review={review} />
        )}
      </div>
      {verdict === "not_checked" && <p className="text-body text-foreground">{CHANGE_REVIEW.NOT_CHECKED_NOTE}</p>}
      {widens.length > 0 && (
        <ul className="space-y-1">
          {widens.map((w) => (
            <li key={w.field} className="text-body">
              <FieldName path={w.field} />
              {w.entries && w.entries.length > 0 && (
                <p className="text-xs text-muted-foreground">
                  {entriesLabel(w.field)} <Mono className="break-all text-foreground">{w.entries.join(", ")}</Mono>
                </p>
              )}
            </li>
          ))}
        </ul>
      )}
      {sentence && <p className="text-body text-foreground">{sentence}</p>}
      {also.length > 0 && verdict !== "not_checked" && (
        <div>
          <p className="text-xs text-muted-foreground">{CHANGE_REVIEW.ALSO_CHANGES}</p>
          <ul className="text-body text-foreground">
            {also.map((a) => (
              <li key={a.path}>
                {a.label} {a.before} → {a.after}
              </li>
            ))}
          </ul>
        </div>
      )}
      {verdict !== "not_checked" && <p className="text-xs text-muted-foreground">{CHANGE_REVIEW.RULES_NOTE}</p>}
    </section>
  );
}

function Tile({ n, label }: { n: number; label: string }) {
  return (
    <div className="rounded-lg border border-border px-3 py-2">
      <div className="text-lg font-semibold text-foreground">{n}</div>
      <div className="text-xs text-muted-foreground">{label}</div>
    </div>
  );
}

function Moves({ changes }: { changes: BoundaryChange[] }) {
  return (
    <ul className="flex flex-col gap-0.5 text-xs text-muted-foreground">
      {changes.map((c) => (
        <li key={c.knob}>
          {fieldLabel(c.knob) ?? c.knob}: {showValue(c.knob, c.before)} → {showValue(c.knob, c.after)}
        </li>
      ))}
    </ul>
  );
}

function BoundaryImpact({ report }: { report: BoundaryReport }) {
  const teams = [
    ...(report.deployment.length > 0 ? [{ key: "deployment", name: BOUNDARY_DRY_RUN.DEFAULT_TEAM, changes: report.deployment }] : []),
    ...report.units.map((u) => ({ key: u.id, name: u.name, changes: u.changes })),
  ];
  const policies = report.stored_policies.map((p) => ({ key: p.id, name: p.name, changes: p.changes }));
  const row = (a: BoundaryReportAssign) => ({
    key: a.id,
    name: BOUNDARY_DRY_RUN.ASSIGNMENT_ROW(a.subject_type, a.subject, a.unit_name),
    changes: a.changes,
  });
  const people = report.memberships.filter((a) => a.subject_type === "user").map(row);
  const others = report.memberships.filter((a) => a.subject_type !== "user").map(row);
  const groups = [
    { title: BOUNDARY_DRY_RUN.TEAMS, rows: teams },
    { title: BOUNDARY_DRY_RUN.POLICIES, rows: policies },
    { title: BOUNDARY_DRY_RUN.PEOPLE, rows: people },
    { title: BOUNDARY_DRY_RUN.OTHER_ASSIGNMENTS, rows: others },
  ];
  const none = groups.every((g) => g.rows.length === 0);
  return (
    <div className="space-y-3" data-testid="change-impact-boundary">
      <div className="grid grid-cols-3 gap-2">
        <Tile n={teams.length} label={BOUNDARY_DRY_RUN.TILE_TEAMS(teams.length)} />
        <Tile n={policies.length} label={BOUNDARY_DRY_RUN.TILE_POLICIES(policies.length)} />
        <Tile n={people.length} label={BOUNDARY_DRY_RUN.TILE_PEOPLE(people.length)} />
      </div>
      {none ? (
        <Chip tone="neutral">{BOUNDARY_DRY_RUN.NO_CHANGE}</Chip>
      ) : (
        groups.map(
          (g) =>
            g.rows.length > 0 && (
              <div key={g.title}>
                <p className="text-xs font-medium text-foreground">{g.title}</p>
                <ul className="mt-1 flex flex-col gap-2">
                  {g.rows.map((r) => (
                    <li key={r.key} className="rounded-lg border border-border px-3 py-2">
                      <div className="flex flex-wrap items-center gap-2">
                        <span className="font-medium text-foreground">{r.name}</span>
                        <Chip tone="warning">{BOUNDARY_DRY_RUN.NARROWED}</Chip>
                      </div>
                      <Moves changes={r.changes} />
                    </li>
                  ))}
                </ul>
              </div>
            ),
        )
      )}
    </div>
  );
}

function TreeImpact({ impact }: { impact: ChangeImpact }) {
  const people = impact.people ?? [];
  const units = new Set(people.map((p) => p.unit_id)).size;
  const runs = impact.live_runs ?? [];
  const runsTotal = impact.live_runs_total ?? runs.length;
  return (
    <div className="space-y-3" data-testid="change-impact-tree">
      <div className="grid grid-cols-3 gap-2">
        <Tile n={people.length} label={CHANGE_REVIEW.PEOPLE} />
        <Tile n={units} label={CHANGE_REVIEW.UNITS} />
        <Tile n={runsTotal} label={CHANGE_REVIEW.LIVE_RUNS_WALLS} />
      </div>
      <p className="text-xs text-muted-foreground">
        {CHANGE_REVIEW.STORED_POLICIES}: {CHANGE_REVIEW.NOT_IN_REPORT}
      </p>
      {people.length > 0 && (
        <div data-testid="change-impact-people">
          <p className="text-xs font-medium text-foreground">{CHANGE_REVIEW.WHOSE_WALLS}</p>
          <Table>
            <TableHeader>
              <TableRow className="hover:bg-transparent">
                <TableHead>{CHANGE_REVIEW.COL_PERSON}</TableHead>
                <TableHead>{CHANGE_REVIEW.COL_UNIT}</TableHead>
                <TableHead>{CHANGE_REVIEW.COL_WHAT_CHANGES}</TableHead>
              </TableRow>
            </TableHeader>
            <TableBody>
              {people.slice(0, SHOWN).map((p) => (
                <TableRow key={p.membership_id}>
                  <TableCell className="align-top">{p.subject}</TableCell>
                  <TableCell className="align-top">{p.unit_name}</TableCell>
                  <TableCell className="align-top">
                    {p.error ? <Mono className="text-foreground">{p.error}</Mono> : <Moves changes={p.changes} />}
                  </TableCell>
                </TableRow>
              ))}
            </TableBody>
          </Table>
          {people.length > SHOWN && <p className="mt-1 text-xs text-muted-foreground">{CHANGE_REVIEW.MORE(people.length - SHOWN)}</p>}
        </div>
      )}
    </div>
  );
}

function LiveRuns({ impact }: { impact: ChangeImpact }) {
  const runs = impact.live_runs ?? [];
  if (runs.length === 0) return null;
  const total = Math.max(impact.live_runs_total ?? 0, runs.length);
  return (
    <div data-testid="change-impact-runs">
      <p className="text-xs font-medium text-foreground">{CHANGE_REVIEW.LIVE_RUNS}</p>
      <Table>
        <TableHeader>
          <TableRow className="hover:bg-transparent">
            <TableHead>{CHANGE_REVIEW.COL_RUN}</TableHead>
            <TableHead>{CHANGE_REVIEW.COL_RUN_OWNER}</TableHead>
            <TableHead>{CHANGE_REVIEW.COL_STARTED}</TableHead>
          </TableRow>
        </TableHeader>
        <TableBody>
          {runs.slice(0, SHOWN).map((r) => (
            <TableRow key={r.id}>
              <TableCell className="align-top">{r.title ? r.title : <Mono className="text-foreground">{r.id.slice(0, 8)}</Mono>}</TableCell>
              <TableCell className="align-top">{r.owner}</TableCell>
              <TableCell className="whitespace-nowrap align-top text-muted-foreground" title={absoluteTime(r.started_at)}>
                {relativeTime(r.started_at)}
              </TableCell>
            </TableRow>
          ))}
        </TableBody>
      </Table>
      {total > Math.min(runs.length, SHOWN) && (
        <p className="mt-1 text-xs text-muted-foreground">{CHANGE_REVIEW.MORE(total - Math.min(runs.length, SHOWN))}</p>
      )}
    </div>
  );
}

// What approving the change would reach, from the server's impact result. `impact` is undefined while it loads (the
// section is not drawn yet) and `error` is the server's message when it could not be read, shown for this section
// only.
export function ImpactSection({
  change,
  impact,
  error,
}: {
  change: GovernanceChange;
  impact: ChangeImpact | undefined;
  error?: string | null;
}) {
  if (!impact && !error) return null;
  let body: ReactNode;
  if (error) {
    body = (
      <Note tone="red" role="alert">
        <Mono className="text-inherit">{error}</Mono>
      </Note>
    );
  } else if (impact?.computed === "tree") {
    body = (
      <>
        <TreeImpact impact={impact} />
        <LiveRuns impact={impact} />
      </>
    );
  } else if (impact?.computed === "boundary" && impact.boundary) {
    body = (
      <>
        <BoundaryImpact report={impact.boundary} />
        <LiveRuns impact={impact} />
      </>
    );
  } else {
    body = (
      <p className="text-body text-foreground">
        {impact?.not_computed === "stale"
          ? CHANGE_REVIEW.IMPACT_STALE(changeTarget(change))
          : impact?.not_computed === "refused"
            ? CHANGE_REVIEW.IMPACT_REFUSED
            : CHANGE_REVIEW.IMPACT_NOT_COMPUTED}
      </p>
    );
  }
  return (
    <section data-testid="change-impact" className="space-y-2">
      <div className="flex flex-wrap items-baseline gap-2">
        <Heading>{CHANGE_REVIEW.IMPACT}</Heading>
        {impact && !error && (
          <span className="text-xs text-muted-foreground" title={absoluteTime(impact.as_of)}>
            {CHANGE_REVIEW.AS_OF_NOW}
          </span>
        )}
      </div>
      {body}
    </section>
  );
}

const countByLevel = (items: RiskItem[]) => LEVELS.map((l) => `${CHANGE_REVIEW.RISK_LEVEL[l]} ${items.filter((i) => i.risk_level === l).length}`).join(", ");
const sameItem = (a: RiskItem, b: RiskItem) => a.field === b.field && a.value === b.value && a.risk_level === b.risk_level;

// The risk grade today and after approval, and the items that are new or changed with the rule's reason. A kind
// the server does not grade says so.
export function RiskSection({ change }: { change: GovernanceChange }) {
  const risk = change.diff.review?.risk;
  if (!risk) {
    return (
      <section data-testid="change-risk">
        <p className="text-body text-muted-foreground">{CHANGE_REVIEW.RISK_NOT_GRADED}</p>
      </section>
    );
  }
  const fresh = risk.after.items.filter((a) => !(risk.before?.items ?? []).some((b) => sameItem(a, b)));
  return (
    <section data-testid="change-risk" className="space-y-2">
      <div className="flex flex-wrap items-baseline gap-2">
        <Heading>{CHANGE_REVIEW.RISK}</Heading>
        <RiskChip risk={risk} />
      </div>
      <dl className="grid grid-cols-[auto_1fr] gap-x-4 gap-y-1 text-body">
        {risk.before && (
          <>
            <dt className="text-muted-foreground">{CHANGE_REVIEW.RISK_TODAY}</dt>
            <dd className="text-foreground">{countByLevel(risk.before.items)}</dd>
          </>
        )}
        <dt className="text-muted-foreground">{CHANGE_REVIEW.RISK_AFTER}</dt>
        <dd className="text-foreground">{countByLevel(risk.after.items)}</dd>
      </dl>
      {fresh.length > 0 && (
        <div>
          <p className="text-xs font-medium text-foreground">{CHANGE_REVIEW.RISK_NEW_OR_CHANGED}</p>
          <ul className="mt-1 space-y-2">
            {fresh.map((i) => (
              <li key={`${i.field}:${i.value}`} className="text-body">
                <div className="flex flex-wrap items-center gap-2">
                  <FieldName path={i.field} />
                  <Mono className="break-all text-foreground">{i.value}</Mono>
                  <Chip tone={RISK_TONE[i.risk_level]}>{CHANGE_REVIEW.RISK_LEVEL[i.risk_level]}</Chip>
                </div>
                <p className="text-xs text-muted-foreground">{i.rationale}</p>
              </li>
            ))}
          </ul>
        </div>
      )}
      <p className="text-xs text-muted-foreground">{CHANGE_REVIEW.RISK_RULES_NOTE}</p>
    </section>
  );
}

// One value of a diff cell. Strings read as they are, an absent value as DIFF_UNSET, anything else as JSON.
function cell(v: unknown): ReactNode {
  if (v === undefined || v === null) return <span className="text-muted-foreground">{CHANGES.DIFF_UNSET}</span>;
  return <Mono className="whitespace-pre-wrap break-words text-foreground">{typeof v === "string" ? v : JSON.stringify(v)}</Mono>;
}

// The server's diff exactly as it came, last and closed: each changed field with its plain name beside the path,
// and the full before and after below. `open` is for a page that has nothing else to show.
export function RawDiffSection({ change, open }: { change: GovernanceChange; open?: boolean }) {
  return (
    <details data-testid="change-raw-diff" open={open}>
      <summary className="cursor-pointer text-body font-medium text-foreground">{CHANGE_REVIEW.RAW_DIFF}</summary>
      <div className="mt-2 space-y-4">
        <Table>
          <TableHeader>
            <TableRow className="hover:bg-transparent">
              <TableHead>{CHANGES.DIFF_FIELD}</TableHead>
              <TableHead>{CHANGES.DIFF_BEFORE}</TableHead>
              <TableHead>{CHANGES.DIFF_AFTER}</TableHead>
            </TableRow>
          </TableHeader>
          <TableBody>
            {change.diff.changed.map((path) => (
              <TableRow key={path}>
                <TableCell className="align-top">
                  <FieldName path={path} />
                </TableCell>
                <TableCell className="align-top">{cell(valueAt(change.diff.before, path))}</TableCell>
                <TableCell className="align-top">{cell(valueAt(change.diff.after, path))}</TableCell>
              </TableRow>
            ))}
          </TableBody>
        </Table>
        <details>
          <summary className="cursor-pointer text-body text-muted-foreground">{CHANGE_REVIEW.SHOW_FULL}</summary>
          <div className="mt-2 grid gap-2 sm:grid-cols-2">
            {[change.diff.before, change.diff.after].map((side, i) => (
              <pre key={i} className="scroll-thin max-h-72 overflow-auto rounded-lg bg-muted p-3 font-mono text-xs">
                {side === undefined ? CHANGES.DIFF_UNSET : JSON.stringify(side, null, 2)}
              </pre>
            ))}
          </div>
        </details>
      </div>
    </details>
  );
}
