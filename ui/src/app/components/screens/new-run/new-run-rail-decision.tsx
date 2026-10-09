/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The rail's decision block: everything read last, directly above Launch, in
// the order it is read — what a check found and whether one is running
// (RailVerdict), then the rail's own launch-refusal alert, then the one reason
// Launch is held (RailHold). Split out of new-run-rail.tsx by seam; like the
// rail these take props and render.
import { Link } from "react-router-dom";
import { Loader2, TriangleAlert } from "lucide-react";
import { Button } from "../../ui/button";
import { ConfinementChip, RiskBadge } from "../../wardyn/primitives";
import { AGENTS } from "../../../lib/workspace-providers-copy";
import { NO_BARRIER, RAIL, RAIL_CHECK, RAIL_SETUP } from "../../wardyn/copy";
import { ISSUE_LINE_ID, type LaunchIssue } from "./new-run-launch-gates";
import type { RunRailProps } from "./new-run-rail-types";

export function RailVerdict({ preflight }: Pick<RunRailProps, "preflight">) {
  // M1 S1: only rows that need attention; `satisfied` stays hidden.
  const setupRows = (preflight.result?.setup_items ?? []).filter((i) => i.status === "missing" || i.status === "unverified");
  return (
    <>
      {/* Unframed: a bordered box inside the rail card is a card in a card
          (CONSOLE-RULES §9). A divider is what separates a section from the
          section above it. "What would be clamped" is the last thing read
          before committing, so the verdict sits directly above Launch. */}
      {preflight.result && (
        <div className="mt-3 border-t border-border pt-3" data-testid="preflight-result">
          <div className="mb-1.5 flex flex-wrap items-center gap-2">
            {preflight.result.overall_risk && <RiskBadge level={preflight.result.overall_risk} />}
            <ConfinementChip value={preflight.result.enforced_confinement_class} />
          </div>
          {setupRows.length > 0 && (
            <div className="mb-1.5">
              <p className="text-[11px] font-medium uppercase tracking-wide text-muted-foreground">{RAIL_SETUP.HEADING}</p>
              <ul className="space-y-0.5 text-xs">
                {setupRows.map((r) => (
                  <li key={r.id} className={r.kind === "backend" && r.status === "missing" ? "text-danger" : "text-warning"}>
                    {r.label}
                    {r.detail ? ` — ${r.detail}` : ""}
                  </li>
                ))}
              </ul>
            </div>
          )}
          {preflight.result.warnings && preflight.result.warnings.length > 0 ? (
            <ul className="list-disc space-y-0.5 pl-4 text-xs text-warning">
              {preflight.result.warnings.map((w, i) => (
                <li key={i}>{w}</li>
              ))}
            </ul>
          ) : (
            <p className="text-xs text-muted-foreground">{AGENTS.EFFECTIVE_NONE}</p>
          )}
        </div>
      )}

      {(preflight.checking || preflight.notChecked) && (
        <p data-testid="preflight-check-state" className="mt-2 flex items-center justify-center gap-1.5 text-xs text-muted-foreground">
          {preflight.checking ? (
            <>
              <Loader2 className="size-3 animate-spin" />
              {RAIL_CHECK.CHECKING}
            </>
          ) : (
            RAIL_CHECK.NOT_CHECKED
          )}
        </p>
      )}

      {preflight.error && (
        <p key={preflight.errorSeq} role="alert" className="mt-3 flex items-start gap-1.5 text-xs text-danger">
          <TriangleAlert className="mt-0.5 size-3.5 shrink-0" />
          <span>
            <span className="sr-only">{RAIL.PREFLIGHT_ERROR_LABEL}</span> {preflight.error}
          </span>
        </p>
      )}

    </>
  );
}

export function RailHold({
  launch,
  shown,
  onIssue,
  canSetUpBarrier,
  guardLink,
}: Pick<RunRailProps, "launch" | "guardLink"> & {
  /** The issue this block names: the first one, unless its own panel is on
   *  screen and already prints it beside its control. */
  shown: LaunchIssue | null;
  onIssue: (issue: LaunchIssue) => void;
  /** #1328 review round 2, R2-1 — who can reach the Environment step: an
   *  operator, or a super admin in the User view. Everyone else reads the
   *  reason alone; there is nothing behind that route they may open. */
  canSetUpBarrier: boolean;
}) {
  const barrierLink = canSetUpBarrier && (
    <>
      {" "}
      <Link to={NO_BARRIER.ADMIN_ROUTE} className="font-medium text-info hover:underline" onClick={guardLink?.(NO_BARRIER.ADMIN_ROUTE)}>
        {NO_BARRIER.CTA}
      </Link>
      .
    </>
  );
  return (
    <>
      {/* A disabled button that doesn't say why is a dead end. The sentence is
          a link: it shows the panel that owns the reason and puts focus on the
          control that fixes it. An issue its own panel already prints beside
          that control is not repeated here (`shown` is null then). */}
      {shown && !launch.inFlight && (
        <p id={ISSUE_LINE_ID} className="mt-2 text-center text-xs text-muted-foreground">
          <Button type="button" variant="link" size="sm" className="h-auto p-0 text-xs font-normal whitespace-normal" onClick={() => onIssue(shown)}>
            {shown.text}
          </Button>
          {/* f-f4: the same CTA, under the same rule, as the no-barrier line below. */}
          {shown.text === RAIL_SETUP.BACKEND_BLOCK && barrierLink}
          {launch.problemLink && (
            <>
              {" "}
              <Link to={launch.problemLink.to} className="font-medium text-info hover:underline" onClick={guardLink?.(launch.problemLink.to)}>
                {launch.problemLink.label}
              </Link>
            </>
          )}
        </p>
      )}
      {/* #214 — the one control that genuinely cannot work says so beside
          itself, not in a tooltip, with a route to the step that fixes it. A
          separate line from the issue above: the Barrier control's own
          TierPicker card already gives the detailed reason; this is Launch's
          own, short pointer to the fix. */}
      {launch.noBarrier && !launch.inFlight && (
        <p className="mt-2 text-center text-xs text-muted-foreground">
          {NO_BARRIER.LAUNCH_REASON}
          {barrierLink}
        </p>
      )}
    </>
  );
}
