/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// OutputTab — run-detail's Output tab (mock packets M8 and M-O). The kept bytes are
// sandbox-controlled, so they render as ONE React text node in a <pre>: never
// HTML, never a markdown component, no escape-sequence interpretation.
import * as React from "react";
import { AlertTriangle, Loader2, Logs } from "lucide-react";
import { useRecordingDisabled } from "../../../lib/hooks/use-recording-disabled";
import type { RunDetailAgent } from "../../../lib/types";
import { RUN_AGENTS, RUN_COCKPIT, RUN_OUTPUT } from "../../wardyn/copy";
import { CopyButton } from "../../wardyn/copy-button";
import { Segmented } from "../../wardyn/segmented";
import { Chip } from "../../wardyn/primitives";
import { useAgentName } from "../../wardyn/use-agent-name";
import { AgentsPanel, agentLines } from "./agents-panel";
import { useRunOutput, type Refusal } from "./use-run-output";
import { EmptyState, ErrorState, STATES } from "../../wardyn/states";

// The empty busy body stays empty this long before a loading line shows.
const LOADING_LINE_MS = 1000;

// Ended states that never went through a Wardyn stop (internal/api/run_output.go).
const NOT_STOPPED = new Set(["KILLED", "FAILED", "COMPLETED"]);

export function Notice({ id, children }: { id?: string; children: React.ReactNode }) {
  return (
    <div id={id} className="flex items-center gap-2 rounded-lg border border-warning/30 bg-warning-subtle px-3 py-2 text-xs text-warning">
      <AlertTriangle className="size-3.5 shrink-0" />
      <span>{children}</span>
    </div>
  );
}

export function OutputTab({
  runId,
  live,
  state,
  endedAt,
  agents: declared,
  runAgent,
  runTask,
  onGoRecording,
}: {
  runId: string;
  /** The run has not reached a terminal state. */
  live: boolean;
  /** The run's state: the nothing-kept sentence is only true for runs that did not end through a Wardyn stop. */
  state: string;
  endedAt?: string;
  /** Every agent of a run with several agents, with its task; the output's own agent states stand in until the run read carries them. The Agents panel and the output filter show for two or more. */
  agents?: readonly RunDetailAgent[];
  runAgent?: string;
  runTask?: string;
  onGoRecording: () => void;
}) {
  const { out, refusal, refusalText, loaded, saving, retrying, retry } = useRunOutput(runId, live, state, endedAt);
  const noticeId = React.useId();
  const [showLoading, setShowLoading] = React.useState(false);
  const recordingOff = useRecordingDisabled() === true;
  const agents: readonly RunDetailAgent[] = declared?.length ? declared : (out?.agents ?? []).map(({ agent }) => ({ agent }));
  const multi = agents.length >= 2;
  const [filter, setFilter] = React.useState("all");
  const agentName = useAgentName();
  // The Agents panel leads whatever the output has to say, a refusal included.
  const withPanel = (body: React.ReactNode) =>
    multi ? (
      <div className="space-y-4">
        <AgentsPanel agents={agents} states={out?.agents} runAgent={runAgent ?? ""} runTask={runTask ?? ""} live={live} />
        {body}
      </div>
    ) : (
      body
    );

  React.useEffect(() => {
    if (loaded) {
      setShowLoading(false);
      return;
    }
    const t = setTimeout(() => setShowLoading(true), LOADING_LINE_MS);
    return () => clearTimeout(t);
  }, [loaded]);

  if (!loaded) {
    return withPanel(
      <div className="max-w-4xl text-sm text-muted-foreground" aria-busy="true">
        {showLoading && (
          <div role="status" className="flex items-center gap-2">
            <Loader2 className="size-4 animate-spin" />
            <span>{RUN_OUTPUT.loading}</span>
          </div>
        )}
      </div>,
    );
  }

  if (refusal !== null) {
    return withPanel(
      <div
        className="max-w-4xl rounded-xl border border-border bg-card"
        data-testid="run-output-refusal"
        aria-busy={retrying}
      >
        {/* The surface stays mounted while the retry read is in flight, so keyboard focus stays on Retry. */}
        {refusalBody(refusal, refusalText, saving, recordingOff, live, state, onGoRecording, retry)}
      </div>,
    );
  }

  const o = out!;
  const pane = o.source === "pane_snapshot";
  // A recording the server could not recover from (missing, invalid or not
  // mask-covered) arrives as an empty recording-source gap row. It shows the
  // approved O4 frame: Command output and the gap notice, with no recovery claim.
  const unrecovered = o.source === "recording" && o.capture_gap && o.output === "";
  const recording = o.source === "recording" && !unrecovered;
  const sourceLabel = recording ? RUN_OUTPUT.sourceRecording : pane ? RUN_OUTPUT.sourcePane : RUN_OUTPUT.sourceStdout;
  // The warnings that limit what the bytes mean describe the region, with or without a body.
  const recoveredId = `${noticeId}-recovered`;
  const gapId = `${noticeId}-gap`;
  const describedBy = [recording && recoveredId, o.capture_gap && gapId].filter(Boolean).join(" ") || undefined;
  // Neither a recovery nor a gap establishes that the run printed nothing.
  // One agent's lines keep their prefix, so a filtered view still says who printed what.
  const shown = multi && filter !== "all" ? agentLines(o.output, filter) : o.output;
  const cleanEmpty = shown === "" && !recording && !o.capture_gap;
  return withPanel(
    <div className="max-w-4xl space-y-3 rounded-xl border border-border bg-card p-4">
      <div className="flex flex-wrap items-center gap-2">
        <span className="label-eyebrow">{RUN_OUTPUT.eyebrow}</span>
        <Chip>{sourceLabel}</Chip>
        <Chip tone={o.complete ? "neutral" : "info"}>{o.complete ? RUN_OUTPUT.final : RUN_OUTPUT.live}</Chip>
        <span className="ml-auto flex flex-wrap items-center gap-2">
          {multi && (
            <span role="group" aria-label={RUN_AGENTS.filterLabel}>
              <Segmented
                size="compact"
                value={filter}
                onChange={setFilter}
                options={[{ value: "all", label: RUN_AGENTS.filterAll }, ...agents.map((a) => ({ value: a.agent, label: agentName(a.agent) }))]}
              />
            </span>
          )}
          {o.complete && o.captured_at && (
            <span className="text-xs text-muted-foreground">{RUN_OUTPUT.capturedAt(o.captured_at)}</span>
          )}
          <CopyButton
            text={shown}
            label={RUN_OUTPUT.copyLabel}
            className="gap-1.5 rounded-md border border-border px-2 py-1 text-xs text-muted-foreground hover:text-foreground"
          >
            Copy
          </CopyButton>
        </span>
      </div>
      <div className="space-y-2">
        {recording && <Notice id={recoveredId}>{RUN_OUTPUT.recordingRecovered}</Notice>}
        {o.mask_scope === "globals_only" && <Notice>{RUN_OUTPUT.globalsOnly}</Notice>}
        {o.capture_gap && <Notice id={gapId}>{RUN_OUTPUT.captureGap}</Notice>}
        {o.incomplete && o.source !== "recording" && <Notice>{RUN_OUTPUT.incomplete}</Notice>}
        {o.truncated && <Notice>{RUN_OUTPUT.truncated}</Notice>}
        {pane && <p className="text-xs text-muted-foreground">{RUN_OUTPUT.paneCaption}</p>}
      </div>
      <pre
        data-testid="run-output-text"
        tabIndex={0}
        role="region"
        aria-label={sourceLabel}
        aria-describedby={describedBy}
        className="scroll-thin overflow-x-auto whitespace-pre-wrap break-all rounded-lg bg-[#0d1117] p-3 font-mono text-xs text-[#e6edf3]"
      >
        {cleanEmpty ? (
          <span className="text-muted-foreground">{o.complete ? RUN_OUTPUT.emptyFinal : RUN_OUTPUT.emptyLive}</span>
        ) : (
          shown
        )}
      </pre>
      <p className="text-xs text-muted-foreground">
        {RUN_OUTPUT.cliHint} <span className="font-mono">wardyn run output {runId}</span>
      </p>
    </div>,
  );
}

function refusalBody(
  r: Refusal,
  text: string,
  saving: boolean,
  recordingOff: boolean,
  live: boolean,
  state: string,
  onGoRecording: () => void,
  retry: () => void,
) {
  switch (r) {
    case "off":
      return (
        <EmptyState
          icon={Logs}
          title={RUN_OUTPUT.offTitle}
          description={
            <>
              {RUN_OUTPUT.offDescPre}
              <span className="font-mono">{RUN_OUTPUT.offEnvVar}</span>
              {RUN_OUTPUT.offDescPost}
            </>
          }
        />
      );
    case "not_kept":
      return saving ? (
        <EmptyState icon={Logs} title={RUN_OUTPUT.savingTitle} description={RUN_OUTPUT.savingDesc} />
      ) : (
        <EmptyState icon={Logs} title={RUN_OUTPUT.notKeptTitle} description={RUN_OUTPUT.notKeptDesc} />
      );
    case "expired":
      return <EmptyState icon={Logs} title={RUN_OUTPUT.expiredTitle} description={RUN_OUTPUT.expiredDesc} />;
    case "erased":
      return <EmptyState icon={Logs} title={RUN_OUTPUT.erasedTitle} description={RUN_OUTPUT.erasedDesc} />;
    case "interactive":
      if (saving) return <EmptyState icon={Logs} title={RUN_OUTPUT.savingTitle} description={RUN_OUTPUT.savingDesc} />;
      // Recording off is a known true only; unknown keeps the pointer (D).
      if (recordingOff) {
        if (live) {
          return (
            <EmptyState icon={Logs} title={RUN_OUTPUT.interactiveLiveTitle} description={RUN_OUTPUT.interactiveLiveDesc} />
          );
        }
        // Mirrors the server's interactiveNothingKept: a run Wardyn stopped is
        // told only that nothing is kept, never that it did not end through a stop.
        return NOT_STOPPED.has(state) ? (
          <EmptyState icon={Logs} title={RUN_OUTPUT.interactiveTitle} description={RUN_OUTPUT.interactiveNoneDesc} />
        ) : (
          <EmptyState icon={Logs} title={RUN_OUTPUT.interactiveTitle} description={RUN_OUTPUT.interactiveStoppedDesc} />
        );
      }
      return (
        <EmptyState
          icon={Logs}
          title={RUN_OUTPUT.interactiveTitle}
          description={RUN_OUTPUT.interactiveDesc}
          action={
            <button type="button" onClick={onGoRecording} className="text-sm text-info hover:underline">
              {RUN_OUTPUT.interactiveLink}
            </button>
          }
        />
      );
    case "not_captured":
      return (
        <EmptyState
          icon={Logs}
          title={RUN_OUTPUT.notCapturedTitle}
          description={RUN_OUTPUT.notCapturedDesc}
          action={
            <button type="button" onClick={onGoRecording} className="text-sm text-info hover:underline">
              {RUN_OUTPUT.interactiveLink}
            </button>
          }
        />
      );
    case "mask":
      return (
        <EmptyState
          icon={Logs}
          title={RUN_OUTPUT.maskTitle}
          description={RUN_OUTPUT.maskUnavailable}
          action={
            <button type="button" onClick={retry} className="text-sm text-info hover:underline">
              {STATES.RETRY}
            </button>
          }
        />
      );
    case "data_out":
      // The server's own sentence, with no action: nothing here can make the output readable.
      return <EmptyState icon={Logs} title={text} />;
    default:
      return <ErrorState message={RUN_COCKPIT.loadError} onRetry={retry} />;
  }
}
