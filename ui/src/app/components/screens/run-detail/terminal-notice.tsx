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
import type { Recording, RunDetail } from "../../../lib/types";
import { AttachTerminal } from "../../attach-terminal";
import { TerminalPlayer } from "../../wardyn/terminal-player";
import { WatchDesktopButton } from "./watch-desktop";
import { Chip } from "../../wardyn/primitives";
import { useOperator, useOperatorResolved, usePrincipal } from "../../wardyn/operator-context";
import { isBackgroundRun, mayEnterRunOrUnknown, runEntryRefusalLine } from "../../../lib/run-entry";
import { RUN_COCKPIT, RUN_MODE } from "../../wardyn/copy";
import { LOCAL_RUN, runnerLabel } from "../../wardyn/copy/runners";
import { RUN_VM_PAGE } from "../../wardyn/copy/run-cockpit";
import { isMacOSVM } from "../../../lib/types/machine-kind";
import type { StartupLastStep } from "../run-status-detail";
import { StartupProgress } from "./startup-progress";
import { QueuedWait } from "./queued-wait";
import { KeptOutput } from "./output-pane";
import { useRunOutput } from "./use-run-output";
import { useRunDataOut } from "./use-run-data-out";

export function TerminalPane({
  run,
  terminal,
  recording,
  recState,
  recordingDisabled,
  onGoRecording,
  onGoOutput,
  execMode,
}: {
  run: RunDetail;
  terminal: boolean;
  recording: Recording | null;
  recState: "idle" | "loading" | "error" | "ready";
  recordingDisabled: boolean;
  onGoRecording: () => void;
  onGoOutput: () => void;
  // run.create's task_mode said "exec" — a shell command ran, no agent harness.
  execMode: boolean;
}) {
  const operator = useOperator();
  const principal = usePrincipal();
  const resolved = useOperatorResolved();
  // The same entry rule AttachTerminal gates its own connect on, and the one the
  // command bar's "attachable" chip, the SSH card and its widget gate claim —
  // all must agree or the page promises a terminal it then refuses to open
  // (#1476: the run's person, or an admin on a run no person owns).
  const canAttach = mayEnterRunOrUnknown(run, principal, operator, resolved);
  const attachable = !!run.interactive && run.state === "RUNNING" && canAttach;
  // The run's data_out rule, read only for a terminal that could open. Unknown draws nothing; a failed read turns copying off under its own line.
  const dataOut = useRunDataOut(run.id, attachable);
  // The Watch link leads to this run's recording, so it needs one: has_recording is on every single-run read.
  const watchable = !recordingDisabled && run.has_recording !== false;
  // A desktop the Mac lets be watched alone is offered here too, for whoever may enter the run. Never on a
  // background run: its watch has its own ticket and lives in the Desktop tile, and this button's attach ticket refuses it.
  const watchDesktop =
    run.state === "RUNNING" && canAttach && !isBackgroundRun(run) && run.macos?.desktop === "observe" ? <WatchDesktopButton run={run} /> : undefined;

  // A run with no recording to replay shows the bytes the Output tab reads: a macOS VM run while it lives (its
  // agent output cast is saved when the agent finishes), any other non-interactive run once the recording read
  // found none. A read that fails or is refused leaves the notices below as they were.
  const comingUp = run.state === "PENDING" || run.state === "STARTING" || run.state === "QUEUED";
  // A finished macOS VM task run can have a recording (its agent output cast): the pane stays on Output while
  // that read is still out, rather than flashing the Recording frame as the run ends, and becomes the replay
  // only once the read finds one.
  const recordingPending = recState === "idle" || recState === "loading";
  const wantsKept =
    !run.interactive && (terminal ? (recState === "ready" && !recording) || (isMacOSVM(run) && recordingPending) : isMacOSVM(run) && !comingUp);
  const kept = useRunOutput(run.id, !terminal, run.state, run.ended_at, wantsKept);
  // An answer read while the run lived is not shown once it has ended; the re-read replaces it.
  const keptFresh = kept.loaded && kept.readLive === !terminal;
  const keptOut = wantsKept && keptFresh && kept.out?.source === "stdout" ? kept.out : null;

  // 0.9 (M-RN-3 RS6): a live run on a computer that is offline cannot be reached, so the terminal and the
  // attach controls give way to a sentence naming it. The captured replay is still offered; it is a file the
  // org already holds.
  if (attachable && run.placement === "local" && run.runner_online === false) {
    return (
      <PaneFrame title="Terminal">
        <PaneNotice
          text={LOCAL_RUN.TERMINAL_OFFLINE(runnerLabel(run))}
          action={
            !watchable ? undefined : (
              <button onClick={onGoRecording} className="text-xs font-medium text-info hover:underline">
                Watch the captured session →
              </button>
            )
          }
        />
      </PaneFrame>
    );
  }

  if (attachable) {
    // fill: the pane owns the height. h-[70vh] was a guess that predates this
    // layout and stays the default for every other mount site.
    const terminal = <AttachTerminal fill barAction={watchDesktop} runId={run.id} createdBy={run.created_by} operatorOwned={run.operator_owned} policy={run.policy} keepsIn={dataOut === "unreadable" ? "unreadable" : dataOut === "review" || dataOut === "blocked"} />;
    // A macOS VM run is RUNNING while its setup runs: the terminal opens at once, so say the setup is still going.
    if (!run.macos_preparing) return terminal;
    return (
      <>
        <p role="status" className="shrink-0 rounded-lg border border-border bg-muted/40 px-2.5 py-2 text-xs leading-relaxed text-muted-foreground" data-testid="macos-setting-up">
          {RUN_VM_PAGE.settingUp}
        </p>
        {terminal}
      </>
    );
  }

  if (wantsKept && (keptOut || !keptFresh)) {
    return (
      <PaneFrame title="Output" chip={execMode ? RUN_COCKPIT.execNoHarness : RUN_COCKPIT.autonomous}>
        {keptOut ? <KeptOutput out={keptOut} onGoOutput={onGoOutput} /> : <div aria-busy="true" className="min-h-0 flex-1" />}
      </PaneFrame>
    );
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
              <button onClick={onGoRecording} className="text-xs font-medium text-info hover:underline">
                Open the Recording tab →
              </button>
            }
          />
        )}
      </PaneFrame>
    );
  }

  // Waiting for a pool slot: there is no startup to narrate yet, so the hero says why it waits and where it stands.
  // A queue the wire did not carry falls through to the plain frame below.
  if (run.state === "QUEUED" && run.queue) {
    return (
      <PaneFrame
        title={run.interactive ? "Terminal" : "Output"}
        chip={run.interactive ? undefined : execMode ? RUN_COCKPIT.execNoHarness : RUN_COCKPIT.autonomous}
      >
        <QueuedWait queue={run.queue} run={run} />
      </PaneFrame>
    );
  }

  // Coming up (PENDING/STARTING): the startup step list replaces the notice,
  // fed by the fields the page already polls. The last row names what comes
  // next; an interactive run this caller can never attach to has none, and
  // keeps the operator-only note beneath the list.
  //
  // F1-F1: The refusal line stays reserved for the caller who genuinely
  // cannot attach, at any state.
  if (run.state === "PENDING" || run.state === "STARTING") {
    // Same precedence as the pane chip below: an exec run is also non-interactive.
    const lastStep: StartupLastStep =
      run.interactive && canAttach ? "terminal" : execMode ? "command" : !run.interactive ? "task" : null;
    return (
      <PaneFrame
        title={run.interactive ? "Terminal" : "Output"}
        chip={run.interactive ? undefined : execMode ? RUN_COCKPIT.execNoHarness : RUN_COCKPIT.autonomous}
      >
        <StartupProgress run={run} lastStep={lastStep}>
          {lastStep === null && (
            <div className="flex flex-col items-center gap-2 pt-2 text-center">
              <p className="text-sm text-muted-foreground">{runEntryRefusalLine(run)}</p>
              {watchable && (
                <button onClick={onGoRecording} className="text-xs font-medium text-info hover:underline">
                  Watch the captured session →
                </button>
              )}
            </div>
          )}
        </StartupProgress>
      </PaneFrame>
    );
  }

  // Live but not drivable: an autonomous run execs the agent directly, so there
  // is no PTY to type into. (A live interactive run the caller may NOT attach to
  // lands here too — the honest thing, since the alternative is a terminal that
  // opens and immediately refuses.)
  return (
    // "Terminal" vs "Output" is not decoration — the board uses them for two
    // different situations. An interactive run HAS a PTY (you just may not
    // drive this one); an autonomous run has none to type into at all, which
    // is why that tile tails output instead of offering a prompt.
    <PaneFrame
      action={watchDesktop}
      title={run.interactive ? "Terminal" : "Output"}
      chip={run.interactive ? undefined : execMode ? RUN_COCKPIT.execNoHarness : RUN_COCKPIT.autonomous}
    >
      {run.interactive && canAttach ? (
        // Not RUNNING and not coming up (e.g. waiting for confirmation): the
        // owner can attach, so the refusal would be false, and there is no
        // startup step to narrate. An empty frame keeps the hero's shape.
        <div className="min-h-0 flex-1" />
      ) : (
        <PaneNotice
          text={run.interactive ? runEntryRefusalLine(run) : RUN_MODE.autonomous.blurb}
          action={
            !watchable ? undefined : (
              <button onClick={onGoRecording} className="text-xs font-medium text-info hover:underline">
                Watch the captured session →
              </button>
            )
          }
        />
      )}
    </PaneFrame>
  );
}

// The non-attached pane, styled as the terminal frame it stands in for so the
// hero keeps its shape across all four run situations.
function PaneFrame({
  title,
  chip,
  action,
  children,
}: {
  title: string;
  chip?: string;
  action?: React.ReactNode;
  children: React.ReactNode;
}) {
  return (
    // `dark`: the pane is always near-black, so its tokens come from the dark block in both themes (#2910).
    <div data-testid="run-pane-frame" className="dark flex min-h-0 flex-1 flex-col overflow-hidden rounded-lg border border-border bg-[#0d1117]">
      <div className="flex h-9 shrink-0 items-center gap-2 border-b border-border bg-card/60 px-3">
        <SquareTerminal className="size-3.5 text-muted-foreground" aria-hidden />
        <span className="label-eyebrow">{title}</span>
        {chip && (
          <Chip tone="neutral" className="font-mono text-meta">
            {chip}
          </Chip>
        )}
        {action && <div className="ml-auto flex min-w-0 items-center gap-2">{action}</div>}
      </div>
      {children}
    </div>
  );
}

function PaneNotice({ text, action }: { text: string; action?: React.ReactNode }) {
  return (
    <div className="scroll-thin flex min-h-0 flex-1 flex-col items-center overflow-y-auto p-6 text-center">
      <div className="my-auto flex flex-col items-center gap-2">
        <p className="max-w-md text-sm text-muted-foreground">{text}</p>
        {action}
      </div>
    </div>
  );
}
