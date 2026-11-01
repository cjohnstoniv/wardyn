/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// KeptOutput — the Overview pane's body for a run with no recording to replay: the bytes the Output tab
// reads, read only. The sandbox controls them, so they render as ONE React text node in a <pre>: never HTML.
import * as React from "react";
import type { RunOutput } from "../../../lib/types";
import { RUN_OUTPUT } from "../../wardyn/copy";
import { Chip } from "../../wardyn/primitives";
import { Notice } from "./output-tab";

// The text follows its end while the person has not scrolled up.
const AT_END_PX = 24;

export function KeptOutput({ out, onGoOutput }: { out: RunOutput; onGoOutput: () => void }) {
  const preRef = React.useRef<HTMLPreElement>(null);
  const atEnd = React.useRef(true);
  React.useLayoutEffect(() => {
    const el = preRef.current;
    if (el && atEnd.current) el.scrollTop = el.scrollHeight;
  }, [out.output]);
  const empty = out.output === "" && !out.capture_gap;
  return (
    <>
      {(out.mask_scope === "globals_only" || out.capture_gap || out.incomplete || out.truncated) && (
        <div className="shrink-0 space-y-2 px-3 pt-2">
          {out.mask_scope === "globals_only" && <Notice>{RUN_OUTPUT.globalsOnly}</Notice>}
          {out.capture_gap && <Notice>{RUN_OUTPUT.captureGap}</Notice>}
          {out.incomplete && <Notice>{RUN_OUTPUT.incomplete}</Notice>}
          {out.truncated && <Notice>{RUN_OUTPUT.truncated}</Notice>}
        </div>
      )}
      <pre
        ref={preRef}
        data-testid="run-pane-output"
        tabIndex={0}
        role="region"
        aria-label={RUN_OUTPUT.sourceStdout}
        onScroll={(e) => {
          const el = e.currentTarget;
          atEnd.current = el.scrollHeight - el.scrollTop - el.clientHeight < AT_END_PX;
        }}
        className="scroll-thin min-h-0 flex-1 overflow-auto whitespace-pre-wrap break-all p-3 font-mono text-xs text-[#e6edf3]"
      >
        {empty ? (
          <span className="text-muted-foreground">{out.complete ? RUN_OUTPUT.emptyFinal : RUN_OUTPUT.emptyLive}</span>
        ) : (
          out.output
        )}
      </pre>
      <div className="flex shrink-0 items-center gap-2 border-t border-border bg-card/60 px-3 py-1.5">
        <Chip>{RUN_OUTPUT.sourceStdout}</Chip>
        <Chip tone={out.complete ? "neutral" : "info"}>{out.complete ? RUN_OUTPUT.final : RUN_OUTPUT.live}</Chip>
        {out.complete && out.captured_at && <span className="text-xs text-muted-foreground">{RUN_OUTPUT.capturedAt(out.captured_at)}</span>}
        <button onClick={onGoOutput} className="ml-auto text-xs font-medium text-info hover:underline">
          {RUN_OUTPUT.openTab}
        </button>
      </div>
    </>
  );
}
