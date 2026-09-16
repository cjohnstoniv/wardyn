/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// TerminalPane — the Overview hero's four run situations (design board 2d).
// Split out of run-detail.tsx to keep that file under its line cap; nothing
// here changed shape, only address.
//
// F1-F1: the fall-through used to fold TWO different questions — "can this
// caller ever attach?" and "has the run reached RUNNING yet?" — into one
// `attachable` gate, so an interactive run still PENDING/STARTING/WAITING_FOR_
// CONFIRMATION told its OWNER the operator-only refusal ("Requires the admin
// role.") plus a dead "Watch the captured session →" link to a recording that
// cannot exist yet. The two questions are answered separately below:
// `canAttach` (who) picks the copy, `attachable` (state too) picks the pane.
import * as React from "react";
import { SquareTerminal } from "lucide-react";
import type { AgentRun, Recording } from "../../../lib/types";
import { AttachTerminal } from "../../attach-terminal";
import { TerminalPlayer } from "../../wardyn/terminal-player";
import { Chip } from "../../wardyn/primitives";
import { useOperator, usePrincipal } from "../../wardyn/operator-context";
import { OPERATOR_ONLY_REASON, RUN_COCKPIT, RUN_MODE } from "../../wardyn/copy";

export function TerminalPane({
  run,
  terminal,
  recording,
  recState,
  recordingDisabled,
  onGoRecording,
  execMode,
}: {
  run: AgentRun;
  terminal: boolean;
  recording: Recording | null;
  recState: "idle" | "loading" | "error" | "ready";
  recordingDisabled: boolean;
  onGoRecording: () => void;
  // run.create's task_mode said "exec" — a shell command ran, no agent harness.
  execMode: boolean;
}) {
  const operator = useOperator();
  const principal = usePrincipal();
  // Same owner-or-admin predicate AttachTerminal gates its own connect on, and
  // the same one the command bar's "attachable" chip claims — all three must
  // agree or the page promises a terminal it then refuses to open.
  const canAttach = operator || (!!run.created_by && run.created_by === principal);
  const attachable = !!run.interactive && run.state === "RUNNING" && canAttach;

  if (attachable) {
    // fill: the pane owns the height. h-[70vh] was a guess that predates this
    // layout and stays the default for every other mount site.
    return <AttachTerminal fill runId={run.id} createdBy={run.created_by} />;
  }

  // Finished run: the pane becomes the replay surface in place rather than a
  // dead box. The Recording TAB is unchanged and remains the full surface
  // (session picker, disabled-store explanation, retry).
  if (terminal) {
    return (
      <PaneFrame title="Recording" chip={RUN_COCKPIT.finishedReplay}>
        {recState === "ready" && recording ? (
          // The player sizes itself with fit:"width" and takes its height from
          // the cast's rows, so it cannot flex — give it its own scroll box
          // rather than letting it push the page.
          <div className="scroll-thin min-h-0 flex-1 overflow-auto p-2">
            <TerminalPlayer recording={recording} />
          </div>
        ) : (
          <PaneNotice
            // "error" is its OWN arm. Falling through to recordingMissing
            // asserted a fact about the RUN ("this run has no captured terminal
            // session") from a fetch that never established it — and on the
            // DEFAULT tab, while the Recording tab, fed by the same recState,
            // correctly admitted the failure. recordingDisabled cannot rescue
            // it either: that is a /healthz boot fact, so on a deployment where
            // recording IS enabled the false arm is the one that fires.
            text={
              recState === "loading" || recState === "idle"
                ? RUN_COCKPIT.recordingLoading
                : recState === "error"
                  ? RUN_COCKPIT.recordingError
                  : recordingDisabled
                    ? RUN_COCKPIT.recordingDisabled
                    : RUN_COCKPIT.recordingMissing
            }
            action={
              <button onClick={onGoRecording} className="text-xs font-medium text-primary hover:underline">
                Open the Recording tab →
              </button>
            }
          />
        )}
      </PaneFrame>
    );
  }

  // Live but not drivable: an autonomous run execs the agent directly, so there
  // is no PTY to type into. (A live interactive run the caller may NOT attach to
  // lands here too — the honest thing, since the alternative is a terminal that
  // opens and immediately refuses.)
  //
  // F1-F1: an interactive run this caller CAN eventually attach to, just not
  // yet (not RUNNING), gets the starting/queued notice — no link, since there
  // is no recording to watch either. OPERATOR_ONLY_REASON is now reserved for
  // the caller who genuinely cannot attach, at any state.
  return (
    // "Terminal" vs "Output" is not decoration — the board uses them for two
    // different situations. An interactive run HAS a PTY (you just may not
    // drive this one); an autonomous run has none to type into at all, which
    // is why that tile tails output instead of offering a prompt.
    <PaneFrame
      title={run.interactive ? "Terminal" : "Output"}
      chip={run.interactive ? undefined : execMode ? RUN_COCKPIT.execNoHarness : RUN_COCKPIT.autonomous}
    >
      <PaneNotice
        text={
          run.interactive
            ? canAttach
              ? RUN_COCKPIT.starting
              : OPERATOR_ONLY_REASON
            : RUN_MODE.autonomous.blurb
        }
        action={
          run.interactive && canAttach ? undefined : (
            <button onClick={onGoRecording} className="text-xs font-medium text-primary hover:underline">
              Watch the captured session →
            </button>
          )
        }
      />
    </PaneFrame>
  );
}

// The non-attached pane, styled as the terminal frame it stands in for so the
// hero keeps its shape across all four run situations.
function PaneFrame({
  title,
  chip,
  children,
}: {
  title: string;
  chip?: string;
  children: React.ReactNode;
}) {
  return (
    <div className="flex min-h-0 flex-1 flex-col overflow-hidden rounded-lg border border-border bg-[#0d1117]">
      <div className="flex h-9 shrink-0 items-center gap-2 border-b border-border bg-card/60 px-3">
        <SquareTerminal className="size-3.5 text-muted-foreground" aria-hidden />
        <span className="label-eyebrow">{title}</span>
        {chip && (
          <Chip tone="neutral" className="font-mono text-meta">
            {chip}
          </Chip>
        )}
      </div>
      {children}
    </div>
  );
}

function PaneNotice({ text, action }: { text: string; action?: React.ReactNode }) {
  return (
    <div className="flex min-h-0 flex-1 flex-col items-center justify-center gap-2 p-6 text-center">
      <p className="max-w-md text-sm text-muted-foreground">{text}</p>
      {action}
    </div>
  );
}
