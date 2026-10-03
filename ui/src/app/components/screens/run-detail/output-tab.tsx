/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// OutputTab — run-detail's Output tab (mock packet M8). The kept bytes are
// sandbox-controlled, so they render as ONE React text node in a <pre>: never
// HTML, never a markdown component, no escape-sequence interpretation.
import * as React from "react";
import { AlertTriangle, Logs } from "lucide-react";
import { HttpError } from "../../../lib/api/core";
import { runOutput } from "../../../lib/api/run-output";
import type { RunOutput } from "../../../lib/types";
import { RUN_COCKPIT, RUN_OUTPUT } from "../../wardyn/copy";
import { CopyButton } from "../../wardyn/copy-button";
import { Chip } from "../../wardyn/primitives";
import { EmptyState, ErrorState } from "../../wardyn/states";

const POLL_MS = 4000;
// A not-kept answer this soon after the run ended is the final row still
// being written (design §3, read order 5), not a verdict.
const SAVING_WINDOW_MS = 60_000;

type Refusal = "off" | "not_kept" | "expired" | "erased" | "interactive" | "mask" | "error";

const REFUSALS: Record<string, Refusal> = {
  run_output_off: "off",
  run_output_not_kept: "not_kept",
  run_output_expired: "expired",
  run_output_erased: "erased",
  run_output_interactive: "interactive",
  mask_state_unavailable: "mask",
};

function Notice({ children }: { children: React.ReactNode }) {
  return (
    <div className="flex items-center gap-2 rounded-lg border border-warning/30 bg-warning-subtle px-3 py-2 text-xs text-warning">
      <AlertTriangle className="size-3.5 shrink-0" />
      <span>{children}</span>
    </div>
  );
}

export function OutputTab({
  runId,
  live,
  endedAt,
  onGoRecording,
}: {
  runId: string;
  /** The run has not reached a terminal state. */
  live: boolean;
  endedAt?: string;
  onGoRecording: () => void;
}) {
  const [out, setOut] = React.useState<RunOutput | null>(null);
  const [refusal, setRefusal] = React.useState<Refusal | null>(null);
  const [loaded, setLoaded] = React.useState(false);
  const [attempt, setAttempt] = React.useState(0);

  const [saving, setSaving] = React.useState(false);

  React.useEffect(() => {
    let cancelled = false;
    let timer: ReturnType<typeof setTimeout> | undefined;
    const tick = async () => {
      try {
        const o = await runOutput.get(runId);
        if (cancelled) return;
        setOut(o);
        setRefusal(null);
        setSaving(false);
        setLoaded(true);
        if (!o.complete) timer = setTimeout(tick, POLL_MS);
      } catch (e) {
        if (cancelled) return;
        setOut(null);
        const r = e instanceof HttpError ? (REFUSALS[e.reason] ?? "error") : "error";
        setRefusal(r);
        setLoaded(true);
        // Only a not-kept answer around the end of a run is worth another look.
        const again =
          r === "not_kept" && (live || (!!endedAt && Date.now() - Date.parse(endedAt) < SAVING_WINDOW_MS));
        setSaving(again);
        if (again) timer = setTimeout(tick, POLL_MS);
      }
    };
    void tick();
    return () => {
      cancelled = true;
      if (timer) clearTimeout(timer);
    };
    // endedAt/live only steer whether to poll again; a change re-reads once.
  }, [runId, live, endedAt, attempt]);

  if (!loaded) return <div className="max-w-4xl text-sm text-muted-foreground" aria-busy="true" />;

  if (refusal !== null) {
    return (
      <div className="max-w-4xl rounded-xl border border-border bg-card" data-testid="run-output-refusal">
        {refusalBody(refusal, saving, onGoRecording, () => {
          setLoaded(false);
          setAttempt((n) => n + 1);
        })}
      </div>
    );
  }

  const o = out!;
  const pane = o.source === "pane_snapshot";
  return (
    <div className="max-w-4xl space-y-3 rounded-xl border border-border bg-card p-4">
      <div className="flex flex-wrap items-center gap-2">
        <span className="label-eyebrow">{RUN_OUTPUT.eyebrow}</span>
        <Chip>{pane ? RUN_OUTPUT.sourcePane : RUN_OUTPUT.sourceStdout}</Chip>
        <Chip tone={o.complete ? "neutral" : "info"}>{o.complete ? RUN_OUTPUT.final : RUN_OUTPUT.live}</Chip>
        <span className="ml-auto flex items-center gap-2">
          {o.complete && o.captured_at && (
            <span className="text-xs text-muted-foreground">{RUN_OUTPUT.capturedAt(o.captured_at)}</span>
          )}
          <CopyButton
            text={o.output}
            label={RUN_OUTPUT.copyLabel}
            className="gap-1.5 rounded-md border border-border px-2 py-1 text-xs text-muted-foreground hover:text-foreground"
          >
            Copy
          </CopyButton>
        </span>
      </div>
      <div className="space-y-2">
        {o.mask_scope === "globals_only" && <Notice>{RUN_OUTPUT.globalsOnly}</Notice>}
        {o.capture_gap && <Notice>{RUN_OUTPUT.captureGap}</Notice>}
        {o.incomplete && <Notice>{RUN_OUTPUT.incomplete}</Notice>}
        {o.truncated && <Notice>{RUN_OUTPUT.truncated}</Notice>}
        {pane && <p className="text-xs text-muted-foreground">{RUN_OUTPUT.paneCaption}</p>}
      </div>
      <pre
        data-testid="run-output-text"
        className="scroll-thin max-h-[60vh] overflow-auto whitespace-pre-wrap break-all rounded-lg bg-[#0d1117] p-3 font-mono text-xs text-[#e6edf3]"
      >
        {o.output === "" ? (
          <span className="text-muted-foreground">{o.complete ? RUN_OUTPUT.emptyFinal : RUN_OUTPUT.emptyLive}</span>
        ) : (
          o.output
        )}
      </pre>
      <p className="text-xs text-muted-foreground">
        {RUN_OUTPUT.cliHint} <span className="font-mono">wardyn run output {runId}</span>
      </p>
    </div>
  );
}

function refusalBody(r: Refusal, saving: boolean, onGoRecording: () => void, retry: () => void) {
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
      return (
        <EmptyState
          icon={Logs}
          title={RUN_OUTPUT.interactiveTitle}
          description={RUN_OUTPUT.interactiveDesc}
          action={
            <button type="button" onClick={onGoRecording} className="text-sm text-primary hover:underline">
              {RUN_OUTPUT.interactiveLink}
            </button>
          }
        />
      );
    case "mask":
      return <EmptyState icon={Logs} title={RUN_COCKPIT.loadError} description={RUN_OUTPUT.maskUnavailable} />;
    default:
      return <ErrorState message={RUN_COCKPIT.loadError} onRetry={retry} />;
  }
}
