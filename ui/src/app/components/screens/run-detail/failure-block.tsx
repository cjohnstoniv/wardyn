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
import type { ReactNode } from "react";
import { ScrollText } from "lucide-react";
import type { AgentRun, AuditEvent, RunEndingKind } from "../../../lib/types";
import { runEndingFromAudit } from "../../../lib/api/audit";
import { absoluteTime } from "../../../lib/format";
import { Button } from "../../ui/button";
import { Mono } from "../../wardyn/code-block";
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
  /** Nodes, not strings, so a wire literal in a line can be <Mono> (§3). */
  todo: readonly ReactNode[];
};

const ENDING_COPY: Partial<Record<RunEndingKind, EndingCopy>> = {
  // run.build/failure is a BUILD, not a pull: a BYOI wrap, a request-level
  // devcontainer, or the workspace's own image (runs_create.go's buildFailed,
  // workspace_run_image.go's buildAudit). The server's own hint on the run is
  // "the run's sandbox image could not be built"; the builder's error rides
  // data.error, which the block renders verbatim above these lines.
  image: {
    happened: () =>
      "The sandbox image could not be built, so the run stopped before the agent started. There is no exit code because nothing ran.",
    todo: [
      "Fix the image or the devcontainer it builds from, then start a new run — the policy and workspace are unchanged.",
      "The audit row carries the builder's full output.",
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
      <>
        Raise <Mono>auto_stop_after_sec</Mono> in the policy if the window is too short, or set it
        to <Mono>-1</Mono> for a session you plan to leave sitting.
      </>,
    ],
  },
  // `unknown` is deliberately absent — see the file header.
};

// A kill whose cascade did NOT fully succeed. run.kill carries outcome
// "failure" (plus a run.revoke/failure row) when KillSandbox, the identity
// revoke or the broker revoke failed — internal/api/runs_lifecycle.go's HONEST
// OUTCOME: the run IS marked KILLED, but the sandbox may still be live and a
// minted token may still be valid until its TTL. The clean-kill copy above
// asserts the opposite of all three, so this run gets its own.
const KILL_INCOMPLETE: EndingCopy = {
  happened: (elapsed) =>
    `An operator killed this run ${elapsed} in, and a teardown step failed. The state is KILLED, but the sandbox may still be live and a credential it held may still work.`,
  todo: [
    "The audit row names the step that failed — read it before treating this run as contained.",
    "Killing it again re-runs the same teardown; the cascade is safe to repeat.",
    "Files written to a mounted workspace are still on the host; scratch is gone.",
  ],
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
  const copy =
    ending.kind === "killed" && ending.outcome === "failure" ? KILL_INCOMPLETE : ENDING_COPY[ending.kind];
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
          {/* The failing step's OWN words, mono because it is a wire value
              (§3) — the same data.error the CLI's runFailureReason prints.
              Under an unrecognised ending there is no copy block at all, so
              this never appears without a cause naming it. */}
          {ending.detail && (
            <p className="mt-1 break-words text-muted-foreground">
              <Mono>{ending.detail}</Mono>
            </p>
          )}
          <p className="label-eyebrow mt-3">{TODO}</p>
          <ul className="mt-1 space-y-0.5 text-xs leading-relaxed text-muted-foreground">
            {copy.todo.map((line, i) => (
              <li key={i} className="flex gap-2">
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
