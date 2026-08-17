/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Sandbox widget — polls CPU/memory/disk/process counts read from cgroup v2 +
// procfs inside the sandbox (GET /runs/{id}/resources). EVERY field is
// optional and that is the entire point: gVisor (the Vault tier) presents a
// synthetic procfs/sysfs and may withhold the cgroup files entirely, so an
// absent metric renders RUN_COCKPIT.metricUnavailable — never 0, never a
// 0%-width bar (see RunResources's doc comment; a 0 here would tell an
// operator a governed workload is using no memory, which is a lie).
//
// Disk and process count have no denominator in this wire shape (no
// disk_limit/process_limit field exists) — unlike the design board's mockup,
// which draws a bar for all four metrics with an invented percentage, this
// renders those two value-only. Fabricating a ratio for them would be the
// exact D11 lie the memory-bar rule forbids, just for a different metric.
import * as React from "react";
import { Box } from "lucide-react";
import { runs as runsApi } from "../../../../lib/api/runs";
import { HttpError } from "../../../../lib/api/core";
import type { RunResources } from "../../../../lib/types";
import { fmtBytes } from "../../../../lib/format";
import { usePoll } from "../../../../lib/use-poll";
import { RUN_COCKPIT } from "../../../wardyn/copy";
import { WidgetCard } from "../../../wardyn/primitives";

const POLL_MS = 4000;


type SandboxState =
  | { kind: "loading" }
  | { kind: "no-sandbox" }
  | { kind: "unsupported" }
  | { kind: "error" }
  | { kind: "ready"; data: RunResources };

export function SandboxWidget({ runId, live }: { runId: string; live: boolean }) {
  const [state, setState] = React.useState<SandboxState>({ kind: "loading" });

  const load = React.useCallback(
    (foreground: boolean) => {
      runsApi
        .getResources(runId)
        .then((data) => setState({ kind: "ready", data }))
        .catch((err) => {
          if (err instanceof HttpError && err.status === 501) setState({ kind: "unsupported" });
          else if (err instanceof HttpError && err.status === 409) setState({ kind: "no-sandbox" });
          else if (foreground) setState({ kind: "error" });
          // else: transient background blip — keep whatever's already on screen.
        });
    },
    [runId],
  );

  React.useEffect(() => {
    load(true);
  }, [load]);
  usePoll(() => load(false), POLL_MS, !live);

  return <WidgetCard title="Sandbox" Icon={Box}>{renderBody(state, live)}</WidgetCard>;
}

function renderBody(state: SandboxState, live: boolean): React.ReactNode {
  switch (state.kind) {
    case "loading":
      return (
        <div className="grid grid-cols-2 gap-3">
          {Array.from({ length: 4 }).map((_, i) => (
            <div key={i} className="h-6 animate-pulse rounded bg-muted" />
          ))}
        </div>
      );
    case "no-sandbox":
      return (
        <p className="text-[0.75rem] text-muted-foreground">
          {live ? RUN_COCKPIT.noSandboxYet : RUN_COCKPIT.sandboxGone}
        </p>
      );
    case "unsupported":
      return <p className="text-[0.75rem] text-muted-foreground">{RUN_COCKPIT.execUnsupported}</p>;
    case "error":
      return <p className="text-[0.75rem] text-muted-foreground">{RUN_COCKPIT.loadError}</p>;
    case "ready":
      return <Metrics data={state.data} />;
  }
}

function Metrics({ data }: { data: RunResources }) {
  const cpu = cpuMetric(data.cpu_percent);
  const memory = memoryMetric(data.memory_used_bytes, data.memory_limit_bytes);
  return (
    <div className="grid grid-cols-2 gap-x-3 gap-y-2">
      <Metric label="CPU" value={cpu.value} barPercent={cpu.barPercent} />
      <Metric label="Memory" value={memory.value} barPercent={memory.barPercent} />
      <Metric
        label="Disk"
        value={data.disk_written_bytes !== undefined ? fmtBytes(data.disk_written_bytes) : RUN_COCKPIT.metricUnavailable}
      />
      <Metric
        label="Processes"
        value={data.process_count !== undefined ? String(data.process_count) : RUN_COCKPIT.metricUnavailable}
      />
    </div>
  );
}

function cpuMetric(pct?: number): { value: string; barPercent?: number } {
  if (pct === undefined) return { value: RUN_COCKPIT.metricUnavailable };
  const clamped = Math.max(0, Math.min(100, pct));
  return { value: `${Math.round(clamped)}%`, barPercent: clamped };
}

function memoryMetric(used?: number, limit?: number): { value: string; barPercent?: number } {
  if (used === undefined) return { value: RUN_COCKPIT.metricUnavailable };
  // limit<=0 is degenerate (no real denominator) — treat like absent rather
  // than divide toward Infinity/NaN.
  if (limit === undefined || limit <= 0) return { value: fmtBytes(used) };
  const pct = Math.max(0, Math.min(100, (used / limit) * 100));
  return { value: `${fmtBytes(used)} / ${fmtBytes(limit)}`, barPercent: pct };
}

function Metric({ label, value, barPercent }: { label: string; value: string; barPercent?: number }) {
  return (
    <div>
      <div className="flex items-baseline justify-between">
        <span className="text-[0.6875rem] text-muted-foreground">{label}</span>
        <span className="font-mono text-[0.6875rem] text-foreground">{value}</span>
      </div>
      {/* A bar needs a denominator — omitted (not a 0%-width bar) whenever
          barPercent is undefined, which covers both an unavailable metric and
          a value with no ratio to show (Disk, Processes). */}
      {barPercent !== undefined && (
        <div className="mt-1 h-1.5 overflow-hidden rounded-full bg-muted">
          <span className="block h-full rounded-full bg-primary" style={{ width: `${barPercent}%` }} />
        </div>
      )}
    </div>
  );
}
