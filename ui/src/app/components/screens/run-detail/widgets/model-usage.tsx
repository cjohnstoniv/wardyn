/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Model usage widget: the tokens the run proxy read off the provider's own answers
// (GET /runs/{id}/model-usage). It answers for a finished run too, so it polls only while the run is live and
// reads once more when the run ends.
//
// Input, Output and Cache read are shown as each vendor reports them and are never added together: Anthropic and
// Bedrock leave cache tokens out of Input, OpenAI counts them inside it. When the counted calls span more than one
// vendor the totals block shows Calls only, because a sum of Inputs that mean different things misleads; the
// per-model rows carry the figures.
//
// Calls is every call the proxy recorded, counted or not. A call cut off mid-answer is "partial": it is counted,
// with the tokens reported so far, and is also named in its own line. A call is "uncounted" when the answer
// carried no usable figure; it adds no tokens and its reason is named.
//
// A finished card has a Refresh button (Retry after a failed read) that keeps the numbers while it reads.
// A later failed read keeps the last numbers with a quiet cue and no Live marker. A refusal (this run is not
// readable by the caller) clears them. A server with no such route answers its own line, so the tile never leaves
// a hole in the grid.
import * as React from "react";
import { Gauge } from "lucide-react";
import { runs as runsApi } from "../../../../lib/api/runs";
import { HttpError } from "../../../../lib/api/core";
import type { ModelUsage, ModelUsageModel } from "../../../../lib/types";
import { usePoll } from "../../../../lib/use-poll";
import { MODEL_USAGE } from "../../../wardyn/copy";
import { WidgetCard } from "../../../wardyn/primitives";
import { Button } from "../../../ui/button";

const POLL_MS = 10_000;

type UsageState =
  | { kind: "loading" }
  | { kind: "ready"; data: ModelUsage; stale: boolean }
  | { kind: "unsupported" }
  | { kind: "unavailable" }
  | { kind: "error" };

const count = (n: number) => n.toLocaleString("en-US");
const countedCalls = (m: { calls: number; uncounted_calls: number }) => m.calls - m.uncounted_calls;

// A server with no model-usage route answers a bare 404 (no envelope) or 501 (no store for it); a 404 that
// carries a reason is the run being refused to this caller.
function failureKind(err: unknown): "unsupported" | "unavailable" | "transient" {
  if (!(err instanceof HttpError)) return "transient";
  if (err.status === 501 || err.status === 405 || (err.status === 404 && err.reason === "")) return "unsupported";
  if (err.status === 401 || err.status === 403 || err.status === 404) return "unavailable";
  return "transient";
}

export function ModelUsageWidget({ runId, live }: { runId: string; live: boolean }) {
  const [state, setState] = React.useState<UsageState>({ kind: "loading" });
  const [busy, setBusy] = React.useState(false);
  const seq = React.useRef(0);
  const shownRun = React.useRef(runId);

  const load = React.useCallback(() => {
    const mine = ++seq.current;
    // Returned for usePoll's in-flight guard.
    return runsApi
      .getModelUsage(runId)
      .then((data) => {
        if (mine === seq.current) setState({ kind: "ready", data, stale: false });
      })
      .catch((err) => {
        if (mine !== seq.current) return;
        const kind = failureKind(err);
        if (kind !== "transient") setState({ kind });
        else setState((prev) => (prev.kind === "ready" ? { ...prev, stale: true } : { kind: "error" }));
      });
  }, [runId]);

  // On mount, for another run, and when the run stops being live: the last reading of a run that just ended.
  React.useEffect(() => {
    if (shownRun.current !== runId) {
      shownRun.current = runId;
      setState({ kind: "loading" });
    }
    void load();
  }, [load, runId, live]);

  // A finished run stops polling, but usage a runner held back can still arrive, and a failed final read must be
  // repeatable: the person asks again here, and the last reading stays on screen while the read is out.
  const refresh = React.useCallback(() => {
    setBusy(true);
    void load().finally(() => setBusy(false));
  }, [load]);

  const stopped = state.kind === "unsupported" || state.kind === "unavailable";
  usePoll(load, POLL_MS, !live || stopped);

  const showLive = live && state.kind === "ready" && !state.stale;
  const right =
    showLive ? (
      <span className="inline-flex items-center gap-1.5 text-meta text-muted-foreground">
        <span className="size-1.5 rounded-full bg-success" aria-hidden />
        {MODEL_USAGE.live}
      </span>
    ) : !live && state.kind === "ready" ? (
      <span className="text-meta text-muted-foreground">{MODEL_USAGE.finished}</span>
    ) : undefined;

  return (
    <WidgetCard title={MODEL_USAGE.title} Icon={Gauge} right={right}>
      {renderBody(state, live, busy, refresh, () => {
        setState({ kind: "loading" });
        void load();
      })}
    </WidgetCard>
  );
}

function renderBody(state: UsageState, live: boolean, busy: boolean, refresh: () => void, retry: () => void): React.ReactNode {
  switch (state.kind) {
    case "loading":
      return (
        <div className="grid grid-cols-2 gap-3">
          {Array.from({ length: 4 }).map((_, i) => (
            <div key={i} className="h-6 animate-pulse rounded bg-muted" />
          ))}
        </div>
      );
    case "unsupported":
      return <p className="text-xs text-muted-foreground">{MODEL_USAGE.unsupported}</p>;
    case "unavailable":
      return <p className="text-xs text-muted-foreground">{MODEL_USAGE.unavailable}</p>;
    case "error":
      return (
        <div className="flex flex-col items-start gap-2">
          <p className="text-xs text-muted-foreground">{MODEL_USAGE.loadError}</p>
          {/* A live run polls again by itself; a finished one has nothing that would. */}
          {!live && (
            <Button variant="outline" size="sm" onClick={retry}>
              {MODEL_USAGE.retry}
            </Button>
          )}
        </div>
      );
    case "ready":
      return <Usage data={state.data} live={live} stale={state.stale} busy={busy} onRefresh={refresh} />;
  }
}

function Usage({ data, live, stale, busy, onRefresh }: { data: ModelUsage; live: boolean; stale: boolean; busy: boolean; onRefresh: () => void }) {
  const { totals } = data;
  const rows = data.by_model;
  const countedRows = rows.filter((m) => countedCalls(m) > 0);
  const vendors = [...new Set(countedRows.map((m) => m.vendor))];
  const counted = countedCalls(totals);
  // The same vendor and model on two endpoints reads as two identical rows; each then names its endpoint.
  const seen = new Map<string, number>();
  for (const m of rows) seen.set(`${m.vendor}|${m.model}`, (seen.get(`${m.vendor}|${m.model}`) ?? 0) + 1);

  return (
    <div>
      {totals.calls === 0 ? (
        <p className="text-xs text-muted-foreground">{live ? MODEL_USAGE.emptyLive : MODEL_USAGE.emptyFinished}</p>
      ) : counted === 0 ? (
        <p className="text-xs text-muted-foreground">{live ? MODEL_USAGE.noTokensLive : MODEL_USAGE.noTokensFinished}</p>
      ) : (
        <>
          {rows.length === 1 && (
            <p className="mb-2 break-all font-mono text-xs text-foreground">
              {MODEL_USAGE.oneModel(rows[0].model || MODEL_USAGE.noModelName, MODEL_USAGE.vendorName(rows[0].vendor), rows[0].calls)}
            </p>
          )}
          {vendors.length > 1 ? (
            <Metric label={MODEL_USAGE.labelCalls} value={count(totals.calls)} />
          ) : (
            <div className="grid grid-cols-2 gap-x-3 gap-y-2">
              <Metric label={MODEL_USAGE.labelInput} value={count(totals.input_tokens)} />
              <Metric label={MODEL_USAGE.labelOutput} value={count(totals.output_tokens)} />
              <Metric label={MODEL_USAGE.labelCacheRead} value={count(totals.cache_read_tokens)} />
              <Metric label={MODEL_USAGE.labelCalls} value={count(totals.calls)} />
            </div>
          )}
          {rows.length > 1 && (
            <ul className="mt-2.5 border-t border-border">
              {rows.map((m) => (
                <ModelRow key={`${m.vendor}|${m.host}|${m.model}`} m={m} showEndpoint={(seen.get(`${m.vendor}|${m.model}`) ?? 0) > 1} />
              ))}
            </ul>
          )}
        </>
      )}
      <Notes data={data} />
      {counted > 0 && <p className="mt-2.5 text-meta leading-snug text-muted-foreground">{MODEL_USAGE.footnote(vendors)}</p>}
      {stale && <p className="mt-2 text-meta leading-snug text-muted-foreground">{MODEL_USAGE.stale}</p>}
      {!live && (
        <Button variant="outline" size="sm" className="mt-2" disabled={busy} aria-busy={busy} onClick={onRefresh}>
          {stale ? MODEL_USAGE.retry : MODEL_USAGE.refresh}
        </Button>
      )}
    </div>
  );
}

function Notes({ data }: { data: ModelUsage }) {
  const { totals } = data;
  const reasons: Record<string, number> = {};
  for (const m of data.by_model) for (const [code, n] of Object.entries(m.uncounted_reasons ?? {})) reasons[code] = (reasons[code] ?? 0) + n;
  const why = MODEL_USAGE.uncountedWhy(reasons, totals.uncounted_calls);
  if (totals.uncounted_calls <= 0 && totals.partial_calls <= 0 && data.opaque_tunnels <= 0) return null;
  return (
    <div className="mt-2.5 flex flex-col gap-1.5 border-t border-border pt-2 text-xs leading-snug text-muted-foreground">
      {totals.uncounted_calls > 0 && (
        <p>
          <b className="font-semibold text-foreground">{MODEL_USAGE.uncountedHead(totals.uncounted_calls)}</b>
          {why && ` ${why}`}
        </p>
      )}
      {totals.partial_calls > 0 && (
        <p>
          <b className="font-semibold text-foreground">{MODEL_USAGE.cutOffHead(totals.partial_calls)}</b> {MODEL_USAGE.cutOffTail}
        </p>
      )}
      {data.opaque_tunnels > 0 && (
        <p>
          <b className="font-semibold text-foreground">{MODEL_USAGE.encryptedHead(data.opaque_tunnels)}</b>{" "}
          {MODEL_USAGE.encryptedTail(data.opaque_tunnels)}
        </p>
      )}
    </div>
  );
}

function ModelRow({ m, showEndpoint }: { m: ModelUsageModel; showEndpoint: boolean }) {
  const hasFigures = countedCalls(m) > 0;
  return (
    <li className="border-b border-border py-2 last:border-b-0 last:pb-0">
      <div className="flex items-baseline gap-2">
        {/* The full name stays on screen: it wraps rather than truncating, so a keyboard or touch reader has it too. */}
        <span className="min-w-0 break-all font-mono text-xs text-foreground">{m.model || MODEL_USAGE.noModelName}</span>
        <span className="whitespace-nowrap text-meta text-muted-foreground">{MODEL_USAGE.vendorName(m.vendor)}</span>
        <span className="ml-auto whitespace-nowrap text-meta text-muted-foreground">{MODEL_USAGE.calls(m.calls)}</span>
      </div>
      {/* Visible text, not a tooltip, so a keyboard or touch reader has the whole address. */}
      {showEndpoint && <p className="mt-0.5 break-all font-mono text-meta text-muted-foreground">{MODEL_USAGE.endpoint(m.provider, m.host)}</p>}
      {hasFigures && (
        <dl className="mt-1 grid grid-cols-3 gap-2">
          <Figure label={MODEL_USAGE.labelInput} value={count(m.input_tokens)} />
          <Figure label={MODEL_USAGE.labelOutput} value={count(m.output_tokens)} />
          <Figure label={MODEL_USAGE.labelCacheRead} value={count(m.cache_read_tokens)} />
        </dl>
      )}
    </li>
  );
}

function Figure({ label, value }: { label: string; value: string }) {
  return (
    <div>
      <dt className="text-meta text-muted-foreground">{label}</dt>
      <dd className="font-mono text-meta text-foreground">{value}</dd>
    </div>
  );
}

function Metric({ label, value }: { label: string; value: string }) {
  return (
    <div className="flex items-baseline justify-between">
      <span className="text-meta text-muted-foreground">{label}</span>
      <span className="font-mono text-meta text-foreground">{value}</span>
    </div>
  );
}
