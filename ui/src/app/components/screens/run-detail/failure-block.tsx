/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// RUN FAILURE BLOCK (M7(b)) — "What happened / What to do", above the terminal,
// for a run that ended badly.
//
// The gap: a FAILED run said what state it was in and nothing about why. The
// reason was in the audit trail all along — the CLI has read it since
// runFailureReason (cmd/wardyn/commands.go) — but the console left an operator
// to page through the trail themselves, or to call the API. A governance
// product whose failure states are only readable through its own REST API has
// not reported the failure.
//
// No new endpoint: the run page already fetches the trail for the Audit tab,
// and runEndingFromAudit (lib/api/audit.ts) reads the ending out of it.
//
// The honesty rule this block is built around: a cause this build does not
// recognise renders the state and the audit link ALONE. No invented reason, and
// the "What to do" list is dropped rather than filled with generic advice — a
// wrong instruction costs an operator more than no instruction.
import { ScrollText } from "lucide-react";
import type { AgentRun, AuditEvent, RunEndingKind } from "../../../lib/types";
import { runEndingFromAudit } from "../../../lib/api/audit";
import { absoluteTime } from "../../../lib/format";
import { Button } from "../../ui/button";
import { formatElapsed } from "../run-detail-summary-header";

// COPY CHANGE (M7): the two labels, the action, and the four reason bodies.
// Local to this file rather than copy.ts on purpose — nothing else renders
// them, and copy.ts owns the vocabulary that MUST agree across surfaces
// (CONSOLE-RULES §10). Sentence case, specific nouns, no filler.
const HAPPENED = "What happened";
const TODO = "What to do";
const OPEN_AUDIT = "Open audit trail";

type EndingCopy = {
  /** `elapsed` is the run's own lifetime up to the ending event. */
  happened: (elapsed: string) => string;
  todo: readonly string[];
};

const ENDING_COPY: Partial<Record<RunEndingKind, EndingCopy>> = {
  image: {
    happened: () =>
      "The sandbox image never pulled, so the run stopped before the agent started. There is no exit code because nothing ran.",
    todo: [
      "Check the registry is reachable from the runner host.",
      "Confirm the image tag in Base images still exists.",
      "Re-run once the pull succeeds — the policy and workspace are unchanged.",
    ],
  },
  selftest: {
    happened: () =>
      "The image selftest failed, so the run was refused before any task ran. The wrapped image is missing a tool the runner needs.",
    todo: [
      "Read the selftest output in the audit trail — it names the missing tool.",
      "Rebuild the base image with that tool, or pick a different one.",
      "Nothing was mounted and no credential was minted; there is nothing to clean up.",
    ],
  },
  killed: {
    happened: (elapsed) =>
      `An operator killed this run ${elapsed} in. The sandbox was torn down and the run's identity revoked; any credential it held stopped working at that moment.`,
    todo: [
      "The audit row names who killed it and the reason they gave.",
      "Files written to a mounted workspace are still on the host; scratch is gone.",
      "Start a new run if the work still needs doing — a killed run cannot resume.",
    ],
  },
  auto_stop: {
    happened: () =>
      "Nobody attached and nothing reached out, so the run hit its idle auto-stop window and shut itself down. This is the policy working, not a fault.",
    todo: [
      "Nothing, if you were done — the recording and the audit trail are kept.",
      "Raise auto_stop_after_sec in the policy if the window is too short.",
      "Set the lifecycle to never-reap for a session you plan to leave sitting.",
    ],
  },
  // `unknown` is deliberately absent — see the file header.
};

export function RunFailureBlock({
  run,
  audit,
  onGoAudit,
}: {
  run: AgentRun;
  /** The trail the run page already fetched. */
  audit: AuditEvent[];
  /** Switch to the Audit tab — the block's one action. */
  onGoAudit: () => void;
}) {
  const ending = runEndingFromAudit(run.state, audit);
  if (!ending) return null;
  const copy = ENDING_COPY[ending.kind];
  // The ending event's own timestamp bounds the run's lifetime; without one
  // (a truncated trail) fall back to the run's last update, which is what the
  // command bar's own clock freezes at.
  const elapsed = formatElapsed(
    new Date(ending.time ?? run.updated_at).getTime() - new Date(run.created_at).getTime(),
  );

  return (
    <section
      // Hairline, not a warning tint: the state badge and the failure hint one
      // row above already carry the tone, and auto-stop is not a fault at all.
      className="shrink-0 rounded-lg border border-border bg-card p-3"
      aria-label={HAPPENED}
      data-testid="run-failure-block"
      data-ending={ending.kind}
    >
      {copy && (
        <>
          <p className="label-eyebrow">{HAPPENED}</p>
          <p className="mt-1 text-xs leading-relaxed text-foreground">{copy.happened(elapsed)}</p>
          <p className="label-eyebrow mt-3">{TODO}</p>
          <ul className="mt-1 space-y-0.5 text-xs leading-relaxed text-muted-foreground">
            {copy.todo.map((line) => (
              <li key={line} className="flex gap-2">
                <span aria-hidden="true">·</span>
                <span>{line}</span>
              </li>
            ))}
          </ul>
        </>
      )}

      <div className="mt-3 flex items-center gap-3">
        <Button variant="outline" size="sm" onClick={onGoAudit}>
          <ScrollText className="size-3.5" /> {OPEN_AUDIT}
        </Button>
        {/* Wire values, so mono (CONSOLE-RULES §3): the action that carries the
            evidence, who the row names, and when. An unrecognised ending has
            no such row and states nothing here. */}
        {ending.action && (
          <span className="min-w-0 truncate font-mono text-meta text-muted-foreground">
            {[ending.action, ending.outcome, ending.actor, ending.time && absoluteTime(ending.time)]
              .filter(Boolean)
              .join(" · ")}
          </span>
        )}
      </div>
    </section>
  );
}
