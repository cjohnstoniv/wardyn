/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The Governance screen's Changes tab: the covered governance writes held
// for approval, one row each. A row opens the change on its own page (change-review-page.tsx), where it is
// read and decided; the helpers that read a request's person, unit and length out of the server's diff are shared
// with that page from here.
//
// Every string is from CHANGES (governance-copy.ts), CHANGE_NOTE (wardyn/copy/change-note.ts) or CHANGE_REVIEW
// (change-review-copy.ts); this file adds none.
import * as React from "react";
import { Inbox } from "lucide-react";
import { useNavigate } from "react-router-dom";
import { governance as api, type GovernanceChangeList } from "../../../lib/api/governance";
import { runnerPools } from "../../../lib/api/runner-pools";
import { absoluteTime, formatDuration, getErrorMessage, relativeTime } from "../../../lib/format";
import { goDurationMs } from "../../../lib/go-duration";
import { changeTarget } from "../../../lib/change-review";
import { CHANGE_REVIEW } from "../../../lib/change-review-copy";
import { CHANGES } from "../../../lib/governance-copy";
import type { GovernanceChange } from "../../../lib/types";
import { Button } from "../../ui/button";
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "../../ui/table";
import { cn } from "../../ui/utils";
import { Mono } from "../../wardyn/code-block";
import { CHANGE_NOTE } from "../../wardyn/copy/change-note";
import { IMG } from "../../wardyn/copy/image-request";
import { changeTargetRef, splitImageRef } from "../elevation/image-ref";
import { usePrincipal, useSecurityOperator } from "../../wardyn/operator-context";
import { EmptyState, TruncatedNote } from "../../wardyn/states";
import { useRovingRadio } from "../../wardyn/use-roving-radio";
import { RiskChip, VerdictChip } from "./change-review-sections";
import { Note } from "./display";

// "Profile · Edit": the kind and the op, each in the table's own vocabulary. One this console predates
// shows its raw name rather than nothing.
export function changeLabel(c: GovernanceChange): string {
  return `${CHANGES.KIND[c.target_kind] ?? c.target_kind} · ${CHANGES.OP[c.op] ?? c.op}`;
}

export { changeTarget };

// The organization runner a held removal takes out of its pool. The payload never reaches the console, so it
// is the one id the diff's runner_ids lose; null for every other change. A pool's key is its id.
function removedOrgRunner(c: GovernanceChange): string | null {
  if (c.target_kind !== "runner_pool_use_policy") return null;
  const ids = (side: unknown) => ((side as { runner_ids?: unknown } | undefined)?.runner_ids ?? []) as string[];
  const after = ids(c.diff.after);
  return ids(c.diff.before).find((id) => !after.includes(id)) ?? null;
}

// The names the removal rows read: each such change's pool and runner. A name that cannot be read (or has
// not arrived) shows its id, so the row is never blank.
function useRemovalNames(changes: GovernanceChange[]): Record<string, { pool: string; runner: string }> {
  const [names, setNames] = React.useState<Record<string, { pool: string; runner: string }>>({});
  const asked = React.useRef(new Set<string>());
  React.useEffect(() => {
    for (const c of changes) {
      const runner = removedOrgRunner(c);
      if (!runner || asked.current.has(c.id)) continue;
      asked.current.add(c.id);
      Promise.all([runnerPools.get(c.target_key), runnerPools.members(c.target_key)])
        .then(([pool, members]) => {
          const m = members.find((x) => x.kind === "self_hosted" && x.id === runner);
          setNames((n) => ({ ...n, [c.id]: { pool: pool.name, runner: m?.name ?? runner } }));
        })
        .catch(() => {});
    }
  }, [changes]);
  return names;
}

// An elevation request's person, unit and length, read from the server's diff (`after`). Null when a field is
// missing or the length is not a Go duration, so the row and the page fall back to the generic ones.
export function elevationOf(c: GovernanceChange): { person: string; unit: string; length: string } | null {
  if (c.target_kind !== "governance_elevation") return null;
  const a = c.diff.after as { unit_name?: unknown; subject?: unknown; duration?: unknown } | undefined;
  if (typeof a?.unit_name !== "string" || typeof a.subject !== "string" || typeof a.duration !== "string") return null;
  if (!a.unit_name || !a.subject) return null;
  const ms = goDurationMs(a.duration);
  return Number.isFinite(ms) ? { person: a.subject, unit: a.unit_name, length: formatDuration(ms, "words") } : null;
}

// An image request's person, the reference and the unit it was asked under, read from the server's diff (`after`).
// Null when a field is missing or the reference has no digest, so the row and the page fall back to the generic ones.
export function imageOf(c: GovernanceChange): { person: string; ref: string; unit: string } | null {
  if (c.target_kind !== "governance_image_request") return null;
  const a = c.diff.after as { image?: unknown; unit_name?: unknown; subject?: unknown } | undefined;
  if (typeof a?.image !== "string" || typeof a.unit_name !== "string" || typeof a.subject !== "string") return null;
  if (!a.unit_name || !a.subject || !splitImageRef(a.image).digest) return null;
  return { person: a.subject, ref: a.image, unit: a.unit_name };
}

// A person asking for something, not proposing a change: the kinds whose proposer's Reject is a withdrawal.
export function isRequest(c: GovernanceChange): boolean {
  return c.target_kind === "governance_elevation" || c.target_kind === "governance_image_request";
}

// Where a row stands for the viewer, the one answer the marker and both filters read. A row with approvals
// follows the viewer's state in them. A row with none is decided by one person (a request, or no minimum in
// force): it needs the viewer unless they proposed it. An unknown viewer (no principal) is never told a row
// needs them while it may be theirs, so such a row with a proposer stands as none.
export function queueStanding(row: GovernanceChange, principal: string): "needs_you" | "waiting" | "none" {
  if (row.approvals) {
    switch (row.approvals.viewer?.state) {
      case "can_approve":
        return "needs_you";
      case "approved":
      case "proposer":
        return "waiting";
      default:
        return "none";
    }
  }
  if (principal) return row.proposed_by === principal ? "waiting" : "needs_you";
  return row.proposed_by ? "none" : "needs_you";
}

type Standing = "all" | "needs_you" | "waiting" | "security";

// The proposer's reason, cut with an ellipsis, then the ticket in mono.
function NoteLine({ note }: { note: NonNullable<GovernanceChange["note"]> }) {
  return (
    <p className="flex items-baseline gap-1.5 text-xs font-normal text-muted-foreground">
      <span className="max-w-[48ch] truncate">{note.reason}</span>
      {note.ticket && (
        <>
          {" "}
          <Mono>{note.ticket}</Mono>
        </>
      )}
    </p>
  );
}

// "Who can decide": the pill group over the queue, the look of the runners screen's filter. "Only security can
// decide" is a security operator's; the other three are every viewer's.
function DeciderFilter({
  value,
  security,
  onChange,
}: {
  value: Standing;
  security: boolean;
  onChange: (value: Standing) => void;
}) {
  const options: { value: Standing; label: string }[] = [
    { value: "all", label: CHANGE_NOTE.FILTER_ALL },
    { value: "needs_you", label: CHANGES.FILTER_NEEDS_YOU },
    { value: "waiting", label: CHANGES.FILTER_WAITING },
    ...(security ? [{ value: "security" as const, label: CHANGE_NOTE.FILTER_SECURITY }] : []),
  ];
  const group = useRovingRadio(
    options.length,
    Math.max(0, options.findIndex((o) => o.value === value)),
    (i) => onChange(options[i].value),
  );
  return (
    <div role="radiogroup" aria-label={CHANGE_NOTE.FILTER_LABEL} className="flex flex-wrap gap-1.5" {...group.containerProps}>
      {options.map((o, i) => {
        const { tabIndex, radioRef } = group.itemProps(i);
        return (
          <button
            key={o.value}
            ref={radioRef}
            tabIndex={tabIndex}
            type="button"
            role="radio"
            aria-checked={value === o.value}
            onClick={() => onChange(o.value)}
            className={cn(
              "rounded-full border px-2.5 py-0.5 text-xs",
              value === o.value ? "border-primary text-foreground" : "border-border text-muted-foreground",
            )}
          >
            {o.label}
          </button>
        );
      })}
    </div>
  );
}

// The Approvals cell: how many have approved of how many are needed (or that one person decides), then the mark.
function ApprovalsCell({ change, standing }: { change: GovernanceChange; standing: "needs_you" | "waiting" | "none" }) {
  const a = change.approvals;
  const mark =
    standing === "needs_you"
      ? CHANGES.NEEDS_YOU
      : standing === "waiting"
        ? a?.viewer?.state === "approved"
          ? CHANGES.YOU_APPROVED
          : CHANGES.YOU_PROPOSED
        : null;
  return (
    <div className="flex flex-col gap-1">
      <span className="text-xs font-medium text-foreground">
        {a ? CHANGES.APPROVED(a.have, a.required) : CHANGES.ONE_PERSON_DECIDES}
      </span>
      {mark && (
        <span data-testid="changes-mark" className={cn("text-xs", standing === "needs_you" ? "font-medium text-primary" : "text-muted-foreground")}>
          {mark}
        </span>
      )}
    </div>
  );
}

const NONE: GovernanceChange[] = [];

export function ChangesTab({
  changes,
  loadError,
}: {
  changes: GovernanceChangeList;
  /** The server's message when the list could not be read; null when it was. */
  loadError: string | null;
  /** The screen still passes it; a change is decided on its own page now, so nothing here reloads the list. */
  onChanged?: () => void;
}) {
  const navigate = useNavigate();
  const open = (c: GovernanceChange) => navigate(`/admin/governance/changes/${encodeURIComponent(c.id)}`);
  const securityOperator = useSecurityOperator();
  const principal = usePrincipal();
  // "Only security can decide" is a request of its own: the list rows do not say who could decide them, so the
  // server answers which ones are security's alone. It is read again whenever the parent reloads the list. "Needs
  // you" and "Waiting on others" read the rows already loaded.
  const [standing, setStanding] = React.useState<Standing>("all");
  const onlySecurity = standing === "security";
  const [filtered, setFiltered] = React.useState<GovernanceChangeList | null>(null);
  const [filterError, setFilterError] = React.useState<string | null>(null);
  React.useEffect(() => {
    if (!onlySecurity) return;
    let live = true;
    api
      .listChanges({ approver: "security" })
      .then((list) => {
        if (!live) return;
        setFiltered(list);
        setFilterError(null);
      })
      .catch((e) => live && setFilterError(getErrorMessage(e)));
    return () => {
      live = false;
    };
  }, [onlySecurity, changes]);
  const chooseFilter = (next: Standing) => {
    setStanding(next);
    setFiltered(null);
    setFilterError(null);
  };
  const base = onlySecurity ? filtered : changes;
  const shown =
    base && (standing === "needs_you" || standing === "waiting")
      ? Object.assign(
          base.filter((c) => queueStanding(c, principal) === standing),
          { truncated: base.truncated },
        )
      : base;
  const shownError = onlySecurity ? filterError : loadError;
  const removalNames = useRemovalNames(shown ?? NONE);
  const hasList = changes.length > 0 || (filtered?.length ?? 0) > 0;
  return (
    <section className="mt-4 overflow-hidden rounded-xl border border-border bg-card">
      {/* The lead describes the list, so it rides with the list; with nothing waiting the empty block
          below says the same thing once (#2907). */}
      {(hasList || onlySecurity) && (
        <div className="flex flex-wrap items-start justify-between gap-3 px-6 pt-5">
          {hasList && <p className="max-w-[82ch] text-body text-muted-foreground">{CHANGES.LEAD}</p>}
          <DeciderFilter value={standing} security={securityOperator} onChange={chooseFilter} />
        </div>
      )}
      {shownError && (
        <div className="px-6 pt-5">
          <Note tone="red" role="alert">
            <Mono className="text-inherit">{shownError}</Mono>
          </Note>
        </div>
      )}
      <div className={shown && shown.length > 0 ? "mt-4" : undefined}>
        {/* The server's header decides, not the row count: exactly 500 rows can be the whole queue. */}
        {shown?.truncated && (
          <div className="px-6">
            <TruncatedNote count={1} cap={1}>
              {CHANGES.TRUNCATED}
            </TruncatedNote>
          </div>
        )}
        {shown === null ? null : shown.length === 0 && onlySecurity ? (
          <div data-testid="changes-empty-filtered">
            <EmptyState
              size="compact"
              icon={Inbox}
              title={CHANGE_NOTE.FILTERED_EMPTY_TITLE}
              description={CHANGE_NOTE.FILTERED_EMPTY_BODY}
              action={
                <Button variant="outline" size="sm" onClick={() => chooseFilter("all")}>
                  {CHANGE_NOTE.FILTERED_EMPTY_ACTION}
                </Button>
              }
            />
          </div>
        ) : shown.length === 0 && standing !== "all" ? (
          <div data-testid="changes-empty-filtered">
            <EmptyState
              size="compact"
              icon={Inbox}
              title={standing === "needs_you" ? CHANGES.FILTER_EMPTY_NEEDS_YOU : CHANGES.FILTER_EMPTY_WAITING}
              action={
                <Button variant="outline" size="sm" onClick={() => chooseFilter("all")}>
                  {CHANGE_NOTE.FILTERED_EMPTY_ACTION}
                </Button>
              }
            />
          </div>
        ) : shown.length === 0 ? (
          // A change reaches this list only by being proposed at a write site, so the one door is the audit
          // trail, where the decided ones are.
          <div data-testid="changes-empty">
            <EmptyState
              size="compact"
              icon={Inbox}
              title={CHANGES.EMPTY_TITLE}
              description={CHANGES.EMPTY_BODY}
              action={
                <Button
                  variant="outline"
                  size="sm"
                  data-testid="changes-empty-audit"
                  onClick={() => navigate("/admin/audit")}
                >
                  {CHANGES.EMPTY_CTA}
                </Button>
              }
            />
          </div>
        ) : (
          <Table>
            <TableHeader>
              <TableRow className="hover:bg-transparent">
                <TableHead>{CHANGES.COL_CHANGE}</TableHead>
                <TableHead>{CHANGE_REVIEW.COL_EFFECT}</TableHead>
                <TableHead>{CHANGES.COL_TARGET}</TableHead>
                <TableHead>{CHANGES.COL_BY}</TableHead>
                <TableHead>{CHANGES.COL_APPROVALS}</TableHead>
                <TableHead>{CHANGES.COL_PROPOSED}</TableHead>
                <TableHead>{CHANGES.COL_EXPIRES}</TableHead>
              </TableRow>
            </TableHeader>
            <TableBody>
              {shown.map((c) => {
                const gone = removedOrgRunner(c);
                const elevation = elevationOf(c);
                const img = imageOf(c);
                return (
                  <TableRow
                    key={c.id}
                    className="cursor-pointer"
                    tabIndex={0}
                    onClick={() => open(c)}
                    onKeyDown={(e) => e.key === "Enter" && e.target === e.currentTarget && open(c)}
                  >
                    <TableCell className="font-medium">
                      <button type="button" className="text-left hover:underline">
                        {gone
                          ? CHANGES.ORG_RUNNER_REMOVE(removalNames[c.id]?.runner ?? gone, removalNames[c.id]?.pool ?? c.target_key)
                          : changeLabel(c)}
                      </button>
                      {gone && (
                        <p className="text-xs font-normal text-muted-foreground">{CHANGES.SUPER_ADMIN_DECIDES}</p>
                      )}
                      {elevation ? (
                        <p className="text-xs font-normal text-muted-foreground">
                          {CHANGE_NOTE.ELEVATION_LINE(elevation.length, c.note?.ticket)}
                        </p>
                      ) : img ? (
                        <p className="text-xs font-normal text-muted-foreground">{IMG.CHANGE_LINE(c.note?.ticket)}</p>
                      ) : (
                        c.note && <NoteLine note={c.note} />
                      )}
                    </TableCell>
                    <TableCell>
                      {!isRequest(c) && (
                        <div className="flex flex-wrap gap-1">
                          <VerdictChip review={c.diff.review} />
                          {c.diff.review?.risk && <RiskChip risk={c.diff.review.risk} />}
                        </div>
                      )}
                    </TableCell>
                    <TableCell>
                      {elevation ? (
                        CHANGE_NOTE.ELEVATION_TARGET(elevation.person, elevation.unit)
                      ) : img ? (
                        <Mono className="text-foreground">{IMG.CHANGE_TARGET(img.person, changeTargetRef(img.ref))}</Mono>
                      ) : (
                        <Mono className="text-foreground">{changeTarget(c)}</Mono>
                      )}
                    </TableCell>
                    <TableCell>{c.proposed_by}</TableCell>
                    <TableCell className="whitespace-nowrap">
                      <ApprovalsCell change={c} standing={queueStanding(c, principal)} />
                    </TableCell>
                    <TableCell className="whitespace-nowrap text-muted-foreground" title={absoluteTime(c.proposed_at)}>
                      {relativeTime(c.proposed_at)}
                    </TableCell>
                    <TableCell className="whitespace-nowrap text-muted-foreground" title={absoluteTime(c.expires_at)}>
                      {relativeTime(c.expires_at)}
                    </TableCell>
                  </TableRow>
                );
              })}
            </TableBody>
          </Table>
        )}
      </div>
    </section>
  );
}
