/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// A governance change on its own page (/admin/governance/changes/:id), where the Changes tab's drawer used to be.
//
// Wide (640 px and up): the main column holds one of four tabs, Effect, Impact, Risk and Raw diff, and the right column
// holds the approvals panel above the decision footer. Narrow: no tab strip; the sections stack as verdict, impact,
// risk, then the panel, then the raw diff closed, and Reject and Approve pin to a bar at the bottom.
//
// Two rules, not styling:
//
//  1. THE DIFF IS THE SERVER'S. The sections render the server's own diff, review and impact; the console computes none.
//  2. THE SERVER DECIDES WHO MAY APPROVE. The footer follows `approvals.viewer` and shows a refusal as the server's own
//     sentence, verbatim, under the frozen heading.
//
// Every string is from CHANGES (governance-copy.ts), CHANGE_NOTE (wardyn/copy/change-note.ts) or CHANGE_REVIEW and
// CHANGE_APPROVALS (change-review-copy.ts); this file adds none.
import * as React from "react";
import { Link, useNavigate, useParams } from "react-router-dom";
import { toast } from "sonner";
import { HttpError } from "../../../lib/api/core";
import { governance as api } from "../../../lib/api/governance";
import { changeTarget } from "../../../lib/change-review";
import { CHANGE_APPROVALS, CHANGE_REVIEW } from "../../../lib/change-review-copy";
import { getErrorMessage, relativeTime } from "../../../lib/format";
import { CHANGES, GOVERNANCE } from "../../../lib/governance-copy";
import { GOVERNANCE_NAV_TITLE } from "../../../lib/nav-copy";
import type { ChangeImpact, GovernanceChange } from "../../../lib/types";
import { Table, TableBody, TableCell, TableRow } from "../../ui/table";
import { cn } from "../../ui/utils";
import { Mono } from "../../wardyn/code-block";
import { CHANGE_NOTE } from "../../wardyn/copy/change-note";
import { IMG } from "../../wardyn/copy/image-request";
import { NotFoundPage } from "../../wardyn/not-found-page";
import { usePrincipal } from "../../wardyn/operator-context";
import { ErrorState, failureSentence } from "../../wardyn/states";
import { changeTargetRef, splitImageRef } from "../elevation/image-ref";
import { ChangeApprovalsPanel } from "./change-approvals-panel";
import { ChangeDecisionFooter } from "./change-decision-footer";
import { ChangeNoteForm } from "./change-note-form";
import { ImpactSection, RawDiffSection, RiskChip, RiskSection, VerdictBlock } from "./change-review-sections";
import {
  REVIEW_ASSISTANT_COLUMN,
  ReviewAssistantColumn,
  ReviewAssistantSheetButton,
  ReviewHeaderChip,
  ReviewReplaySlot,
} from "./change-review-slots";
import { changeLabel, elevationOf, imageOf, isRequest } from "./changes";
import { Note } from "./display";

// The queue: the Changes tab of the Governance screen.
const QUEUE = "/admin/governance?tab=changes";

// The kinds whose impact the server replays, so the only ones whose impact can say the change went stale before anyone decides.
const REPLAYED_KINDS = ["governance_unit", "governance_membership", "governance_boundary"];

type TabId = "effect" | "impact" | "risk" | "raw";
const TABS: { id: TabId; label: string }[] = [
  { id: "effect", label: CHANGE_APPROVALS.TAB_EFFECT },
  { id: "impact", label: CHANGE_REVIEW.IMPACT },
  { id: "risk", label: CHANGE_REVIEW.RISK },
  { id: "raw", label: CHANGE_REVIEW.RAW_DIFF },
];

const WIDE = "(min-width: 640px)";

// Whether the page is wide enough for tabs. jsdom has no matchMedia, and an answer that cannot be asked is wide.
function useWide(): boolean {
  const [wide, setWide] = React.useState(() => window.matchMedia?.(WIDE).matches ?? true);
  React.useEffect(() => {
    const mq = window.matchMedia?.(WIDE);
    if (!mq) return;
    const on = () => setWide(mq.matches);
    on();
    mq.addEventListener?.("change", on);
    return () => mq.removeEventListener?.("change", on);
  }, []);
  return wide;
}

// The profile an assignment change assigns, from the diff's own `after.unit`.
function assignedProfile(c: GovernanceChange): Record<string, unknown> | null {
  const p = (c.diff.after as { unit?: unknown } | undefined)?.unit;
  return p && typeof p === "object" ? (p as Record<string, unknown>) : null;
}

// What approving would reach, as one line under the verdict; nothing for an impact the server did not compute.
function ImpactSummary({ impact }: { impact: ChangeImpact | undefined }) {
  if (impact?.computed !== "tree") return null;
  const people = impact.people ?? [];
  const units = new Set(people.map((p) => p.unit_id)).size;
  const runs = impact.live_runs_total ?? impact.live_runs?.length ?? 0;
  return (
    <p data-testid="change-impact-summary" className="text-body text-foreground">
      {CHANGE_APPROVALS.IMPACT_SUMMARY(units, runs)}
    </p>
  );
}

export function ChangeReviewPage() {
  const { id = "" } = useParams();
  const navigate = useNavigate();
  const principal = usePrincipal();
  const wide = useWide();

  const [change, setChange] = React.useState<GovernanceChange | null>(null);
  const [missing, setMissing] = React.useState(false);
  const [loadError, setLoadError] = React.useState<string | undefined | null>(null);
  const [impact, setImpact] = React.useState<ChangeImpact>();
  const [impactError, setImpactError] = React.useState<string | null>(null);
  const [tab, setTab] = React.useState<TabId>("effect");
  const [reason, setReason] = React.useState("");
  const [busy, setBusy] = React.useState(false);
  const [refused, setRefused] = React.useState<string | null>(null);
  const [editing, setEditing] = React.useState(false);
  // A decision answered 409 governance_change_stale: stale for every kind, whatever the impact route replays.
  const [staleSeen, setStaleSeen] = React.useState(false);

  const fail = React.useCallback((e: unknown) => {
    if (e instanceof HttpError && e.status === 404) setMissing(true);
    else setLoadError(failureSentence(e));
  }, []);

  React.useEffect(() => {
    let live = true;
    api
      .getChange(id)
      .then((c) => live && setChange(c))
      .catch((e) => live && fail(e));
    return () => {
      live = false;
    };
  }, [id, fail]);

  // The change again, in place: after an approval that did not apply it, a take-back, or a refusal that says it moved.
  const reload = React.useCallback(async () => {
    try {
      setChange(await api.getChange(id));
    } catch (e) {
      fail(e);
    }
  }, [id, fail]);

  // What approving would reach: asked once, for a pending change that is not a request.
  const reviewable = !!change && change.state === "pending" && !isRequest(change);
  React.useEffect(() => {
    if (!reviewable) return;
    let live = true;
    api
      .getChangeImpact(id)
      .then((i) => live && setImpact(i))
      .catch((e) => live && setImpactError(getErrorMessage(e)));
    return () => {
      live = false;
    };
  }, [reviewable, id]);

  if (missing) return <NotFoundPage admin />;
  if (loadError !== null) {
    return (
      <ErrorState
        message={loadError}
        onRetry={() => {
          setLoadError(null);
          void reload();
        }}
      />
    );
  }
  if (!change) return <p role="status" className="p-6 text-body text-muted-foreground">{CHANGE_APPROVALS.LOADING}</p>;

  const decide = async (approve: boolean) => {
    setBusy(true);
    setRefused(null);
    try {
      if (approve) {
        const out = await api.approveChange(change.id, reason.trim() || undefined);
        // Applied, but a token may still hold the role it took away, or a suspended service identity may keep a
        // live run: say so until dismissed.
        if ((out.tokens_revocation_failed || out.runs_end_failed) && out.detail) toast.warning(out.detail, { duration: Infinity });
        else toast.success(CHANGES.TOAST_APPROVED);
        // Still pending: more people must approve, so stay here and read where it stands. Applied: back to the queue.
        if (out.state === "pending") {
          setReason("");
          await reload();
        } else void navigate(QUEUE);
      } else {
        await api.rejectChange(change.id, reason.trim());
        toast.success(CHANGES.TOAST_REJECTED);
        void navigate(QUEUE);
      }
    } catch (e) {
      setRefused(e instanceof HttpError ? e.message : getErrorMessage(e));
      // A 409 means the change is no longer what was reviewed (stale, expired, or decided already): read it again.
      if (e instanceof HttpError && e.status === 409) {
        if (e.reason === "governance_change_stale") setStaleSeen(true);
        await reload();
      }
    } finally {
      setBusy(false);
    }
  };

  const takeBack = async () => {
    setBusy(true);
    setRefused(null);
    try {
      await api.takeBackApproval(change.id);
      toast.success(CHANGE_APPROVALS.TOAST_TAKEN_BACK);
      await reload();
    } catch (e) {
      setRefused(e instanceof HttpError ? e.message : getErrorMessage(e));
    } finally {
      setBusy(false);
    }
  };

  const request = isRequest(change);
  const elevation = elevationOf(change);
  const img = imageOf(change);
  const profile = assignedProfile(change);
  const hasNote = !!(change.note?.reason || change.note?.ticket);
  const pending = change.state === "pending";
  // Before a decision only the kinds the impact route replays can say stale from their impact; any kind can once the
  // change's own state says so or a decision was refused for it. Never inferred for another kind.
  const stale =
    change.state === "stale" ||
    staleSeen ||
    (pending && REPLAYED_KINDS.includes(change.target_kind) && impact?.computed === "none" && impact.not_computed === "stale");
  const deciding = pending || change.state === "stale";
  const pinned = !wide && deciding;
  const target = elevation
    ? CHANGE_NOTE.ELEVATION_TARGET(elevation.person, elevation.unit)
    : img
      ? IMG.CHANGE_TARGET(img.person, changeTargetRef(img.ref))
      : changeTarget(change);

  const verdict = (
    <div className="space-y-4">
      <VerdictBlock change={change} />
      <ImpactSummary impact={impact} />
      {change.diff.review?.risk && <RiskChip risk={change.diff.review.risk} />}
    </div>
  );

  // Between the section and the decider's reason: the assigned profile, the server's notes, the proposer's note.
  const aside = (
    <>
      {profile && (
        <section data-testid="change-assigned-profile">
          <h4 className="text-body font-medium text-foreground">
            {CHANGES.ASSIGNED_PROFILE}
            {typeof profile.name === "string" && <span className="ml-2 font-normal">{profile.name}</span>}
          </h4>
          <pre className="scroll-thin mt-1 max-h-72 overflow-auto rounded-lg bg-muted p-3 font-mono text-xs">
            {JSON.stringify({ ceiling: profile.ceiling, limits: profile.limits, error: profile.error }, null, 2)}
          </pre>
        </section>
      )}
      {/* The notes a direct save would have shown, for the person approving. Same amber note and heading the profile
          screen puts them under after a save. */}
      {change.diff.notes && change.diff.notes.length > 0 && (
        <Note tone="amber">
          <b className="font-semibold">{GOVERNANCE.OMISSION_TITLE}</b>
          {change.diff.notes.map((n) => (
            <Mono key={n} className="text-inherit">
              {n}
            </Mono>
          ))}
        </Note>
      )}
      {editing ? (
        <section className="space-y-2">
          <h4 className="text-body font-medium text-foreground">{CHANGE_NOTE.WHY_TITLE}</h4>
          <ChangeNoteForm
            change={change}
            onSaved={(c) => {
              setChange((cur) => (cur ? { ...cur, note: c.note } : c));
              setEditing(false);
            }}
            onCancel={() => setEditing(false)}
            onGone={(status) => {
              if (status === 404) void navigate(QUEUE);
              else void reload();
            }}
          />
        </section>
      ) : (
        hasNote && (
          <section data-testid="change-note" className="space-y-1">
            <h4 className="text-body font-medium text-foreground">{CHANGE_NOTE.WHY_TITLE}</h4>
            {change.note?.reason && <p className="whitespace-pre-wrap break-words text-body text-foreground">{change.note.reason}</p>}
            {change.note?.ticket && (
              <p className="text-xs text-muted-foreground">
                {CHANGE_NOTE.TICKET(change.note.ticket).label} <Mono className="break-all">{change.note.ticket}</Mono>
              </p>
            )}
          </section>
        )
      )}
    </>
  );

  const impactBlock = (
    <div className="space-y-4">
      <ImpactSection change={change} impact={impact} error={impactError} />
      <ReviewReplaySlot change={change} />
    </div>
  );

  // The request kinds: today's elevation or image table, then the full before and after; one person decides.
  const requestBody = (
    <div className="space-y-4">
      {elevation ? (
        <Table>
          <TableBody>
            {[
              [CHANGE_NOTE.ELEV_PERSON, elevation.person],
              [CHANGE_NOTE.ELEV_UNIT, elevation.unit],
              [CHANGE_NOTE.ELEV_LENGTH, CHANGE_NOTE.ELEV_LENGTH_VALUE(elevation.length)],
            ].map(([label, value]) => (
              <TableRow key={label}>
                <TableCell className="text-muted-foreground">{label}</TableCell>
                <TableCell>{value}</TableCell>
              </TableRow>
            ))}
          </TableBody>
        </Table>
      ) : img ? (
        <Table>
          <TableBody>
            {[
              [IMG.ROW_PERSON, img.person],
              [IMG.ROW_IMAGE, <Mono key="n" className="text-foreground">{splitImageRef(img.ref).name}</Mono>],
              [IMG.ROW_DIGEST, <Mono key="d" className="break-all text-foreground">{splitImageRef(img.ref).digest}</Mono>],
              [IMG.ROW_ASKED_UNDER, IMG.ASKED_UNDER(img.unit)],
              [IMG.ROW_WRITES, IMG.WRITES],
            ].map(([label, value]) => (
              <TableRow key={String(label)}>
                <TableCell className="align-top text-muted-foreground">{label}</TableCell>
                <TableCell>{value}</TableCell>
              </TableRow>
            ))}
          </TableBody>
        </Table>
      ) : (
        // A request this console cannot read the people out of keeps its raw diff, open.
        <RawDiffSection change={change} open />
      )}
      {(elevation || img) && (
        <details>
          <summary className="cursor-pointer text-body text-muted-foreground">{CHANGES.DIFF_FULL}</summary>
          <div className="mt-2 grid gap-2 sm:grid-cols-2">
            {[change.diff.before, change.diff.after].map((side, i) => (
              <pre key={i} className="scroll-thin max-h-72 overflow-auto rounded-lg bg-muted p-3 font-mono text-xs">
                {side === undefined ? CHANGES.DIFF_UNSET : JSON.stringify(side, null, 2)}
              </pre>
            ))}
          </div>
        </details>
      )}
      {aside}
    </div>
  );

  const decidedNote = !deciding && (CHANGE_APPROVALS.DECIDED[change.state] ?? "");
  const footer = deciding ? (
    <ChangeDecisionFooter
      change={change}
      principal={principal}
      stale={stale}
      busy={busy}
      editing={editing}
      reason={reason}
      onReason={setReason}
      refused={refused}
      pinned={pinned}
      hasNote={hasNote}
      onEditNote={() => setEditing(true)}
      onApprove={() => void decide(true)}
      onReject={() => void decide(false)}
      onTakeBack={() => void takeBack()}
    />
  ) : (
    decidedNote && <Note>{decidedNote}</Note>
  );
  const panel = deciding && <ChangeApprovalsPanel change={change} principal={principal} />;

  const header = (
    <header className="space-y-1">
      <nav>
        <Link to={QUEUE} className="text-xs text-muted-foreground hover:underline">
          {CHANGE_APPROVALS.BREADCRUMB(GOVERNANCE_NAV_TITLE, CHANGES.TAB)}
        </Link>
      </nav>
      <div className="flex flex-wrap items-center justify-between gap-2">
        <h1 className="flex min-w-0 flex-wrap items-baseline gap-x-2 break-words text-lg font-semibold text-foreground">
          <span className="min-w-0 [overflow-wrap:anywhere]">{changeLabel(change)}</span>
          <Mono className="min-w-0 text-foreground [overflow-wrap:anywhere]">{target}</Mono>
        </h1>
        <div className="flex items-center gap-2">
          <ReviewHeaderChip change={change} />
          <ReviewAssistantSheetButton change={change} />
        </div>
      </div>
      <p className="text-body text-muted-foreground">
        {CHANGES.META(change.proposed_by, relativeTime(change.proposed_at), relativeTime(change.expires_at))}
      </p>
    </header>
  );

  // The grid gains a column for the review assistant only while its mount point is turned on.
  const columns = REVIEW_ASSISTANT_COLUMN ? "grid-cols-[minmax(0,1fr)_320px_360px]" : "grid-cols-[minmax(0,1fr)_320px]";

  if (!wide) {
    return (
      <div data-testid="change-review-page" data-layout="stacked" className={cn("space-y-4 p-4", pinned && "pb-24")}>
        {header}
        {request ? (
          <>
            {requestBody}
            {panel}
            {footer}
          </>
        ) : (
          <>
            {verdict}
            {aside}
            {impactBlock}
            <RiskSection change={change} />
            {panel}
            {footer}
            <RawDiffSection change={change} />
          </>
        )}
      </div>
    );
  }

  return (
    <div data-testid="change-review-page" data-layout="tabs" className="space-y-4 p-6">
      {header}
      <div data-testid="change-review-grid" className={cn("grid items-start gap-6", columns)}>
        <div className="min-w-0 space-y-4">
          {request ? (
            requestBody
          ) : (
            <>
              <div role="tablist" aria-label={CHANGE_APPROVALS.TABS_LABEL} className="flex gap-1 border-b border-border">
                {TABS.map((t) => (
                  <button
                    key={t.id}
                    type="button"
                    role="tab"
                    id={`change-tab-${t.id}`}
                    aria-selected={tab === t.id}
                    aria-controls={`change-panel-${t.id}`}
                    onClick={() => setTab(t.id)}
                    className={cn(
                      "-mb-px border-b-2 px-3 py-2 text-body",
                      tab === t.id ? "border-primary text-foreground" : "border-transparent text-muted-foreground",
                    )}
                  >
                    {t.label}
                  </button>
                ))}
              </div>
              <div role="tabpanel" id={`change-panel-${tab}`} aria-labelledby={`change-tab-${tab}`} className="space-y-4">
                {tab === "effect" && (
                  <>
                    {verdict}
                    {aside}
                  </>
                )}
                {tab === "impact" && impactBlock}
                {tab === "risk" && <RiskSection change={change} />}
                {tab === "raw" && <RawDiffSection change={change} open />}
              </div>
            </>
          )}
        </div>
        <div className="min-w-0 space-y-4">
          {panel}
          {footer}
        </div>
        {REVIEW_ASSISTANT_COLUMN && <ReviewAssistantColumn change={change} />}
      </div>
    </div>
  );
}
