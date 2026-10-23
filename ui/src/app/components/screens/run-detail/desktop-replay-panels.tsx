/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The desktop replay's right-hand column (M7-Replay): the action log, whose rows jump the playhead,
// and the tamper-evident log's verdict on the recording's range (GET /runs/{id}/recording-chain).
import * as React from "react";
import { recordings, type AuditChainRange } from "../../../lib/api/recordings";
import { clockTime, shortTime } from "../../../lib/format";
import type { AuditEvent } from "../../../lib/types";
import { jumpTo, type ReplayAction, type ReplayGap } from "../../../lib/desktop-replay-timeline";
import { Chip } from "../../wardyn/primitives";
import { RECORDING_REPLAY as C } from "../../wardyn/copy/recording-level";

/** The most rows the log lists; the rest are in Audit. */
const MAX_ROWS = 50;

const ACTOR_TONE = { AGENT: "primary", HUMAN: "info", SYSTEM: "neutral" } as const;
const OUTCOME_CLASS = { success: "text-success", denied: "text-warning", failure: "text-danger" } as const;

export function ReplayActionLog({ actions, gaps, active, duration, onJump, onGoAudit }: {
  actions: ReplayAction[];
  gaps: ReplayGap[];
  active?: string;
  duration: number;
  onJump: (id: string, ms: number) => void;
  onGoAudit?: () => void;
}) {
  const shown = actions.slice(0, MAX_ROWS);
  return (
    <section aria-label={C.ACTION_LOG} className="rounded-xl border border-border bg-card">
      <div className="flex items-center gap-2 border-b border-border px-3.5 py-2.5">
        <span className="flex-1 text-[11px] font-semibold uppercase tracking-[.06em]">{C.ACTION_LOG}</span>
        <span className="text-meta text-muted-foreground">{C.ACTION_LOG_HINT}</span>
      </div>
      {shown.map((a) => (
        <button
          key={a.id}
          type="button"
          aria-current={active === a.id ? "true" : undefined}
          onClick={() => onJump(a.id, jumpTo(a, duration))}
          className={`flex w-full items-center gap-2.5 border-b border-border/60 px-3 py-2 text-left text-body hover:bg-muted ${
            active === a.id ? "ring-1 ring-inset ring-primary" : ""
          } ${gaps.some((g) => g.startMs <= a.ms && a.ms <= g.endMs) ? "bg-warning-subtle" : ""}`}
          data-testid="desktop-replay-action"
          data-ms={a.ms}
        >
          <span className="w-[52px] font-mono text-meta text-muted-foreground">{shortTime(a.time)}</span>
          <span className="w-[74px]">
            <Chip tone={ACTOR_TONE[a.actor]}>{a.actor}</Chip>
          </span>
          <span className="flex min-w-0 flex-1 flex-col">
            <span>{a.text}</span>
            {a.detail && <span className="truncate font-mono text-[11px] text-muted-foreground">{a.detail}</span>}
          </span>
          <span className={`text-meta font-medium ${OUTCOME_CLASS[a.outcome]}`}>{a.outcome}</span>
        </button>
      ))}
      <div className="px-3.5 py-2.5 text-meta text-muted-foreground">
        {C.SHOWING(shown.length, actions.length)}
        {onGoAudit && (
          <>
            {" · "}
            <button type="button" className="text-primary hover:underline" onClick={onGoAudit}>
              {C.OPEN_AUDIT}
            </button>
          </>
        )}
      </div>
    </section>
  );
}

export function ReplayChain({ runId, recKey, from, gaps, audit }: {
  runId: string;
  recKey: string;
  from: string;
  gaps: ReplayGap[];
  audit: AuditEvent[];
}) {
  const [chain, setChain] = React.useState<AuditChainRange | "loading" | "error">("loading");
  React.useEffect(() => {
    let alive = true;
    setChain("loading");
    recordings
      .verifyChain(runId, recKey, from)
      .then((c) => alive && setChain(c))
      .catch(() => alive && setChain("error"));
    return () => {
      alive = false;
    };
  }, [runId, recKey, from]);
  const seqOf = (id?: string) => (typeof chain === "object" ? chain.marks.find((m) => m.id === id)?.seq : undefined);
  const timeOf = (id?: string) => audit.find((e) => e.id === id)?.time;
  const entry = (id: string | undefined, fallback: string) => {
    const seq = seqOf(id);
    const t = clockTime(timeOf(id) ?? fallback);
    return seq != null ? C.ENTRY_AT(seq, t) : t;
  };
  return (
    <section aria-label={C.LOG_TITLE} className="rounded-xl border border-border bg-card" data-testid="desktop-replay-chain">
      <div className="flex items-center gap-2 border-b border-border px-3.5 py-2.5">
        <span className="flex-1 text-[11px] font-semibold uppercase tracking-[.06em]">{C.LOG_TITLE}</span>
        {typeof chain === "object" && (
          <Chip tone={chain.ok ? "success" : "danger"} dot>
            {chain.ok ? C.CHAIN_OK : C.CHAIN_BROKEN}
          </Chip>
        )}
      </div>
      <div className="flex flex-col gap-2 px-3.5 py-3 text-body">
        <span data-testid="desktop-replay-chain-verdict">
          {chain === "loading"
            ? C.CHAIN_CHECKING
            : chain === "error"
              ? C.CHAIN_UNAVAILABLE
              : chain.ok
                ? C.CHAIN_RANGE(chain.first_seq, chain.last_seq)
                : C.CHAIN_BROKEN_BODY(chain.broken_seq ?? 0, chain.reason ?? "")}
        </span>
        {gaps.map((g) => (
          <dl key={g.startMs} className="grid grid-cols-[96px_minmax(0,1fr)] gap-x-2.5 gap-y-1" data-testid="desktop-replay-log-row">
            <dt className="text-muted-foreground">{C.GAP_START}</dt>
            <dd className="font-mono text-meta">{entry(g.pauseId, new Date(Date.parse(from) + g.startMs).toISOString())}</dd>
            <dt className="text-muted-foreground">{C.GAP_END}</dt>
            <dd className="font-mono text-meta">{entry(g.resumeId, new Date(Date.parse(from) + g.endMs).toISOString())}</dd>
            <dt className="text-muted-foreground">{C.REASON}</dt>
            <dd>{g.reason.charAt(0).toUpperCase() + g.reason.slice(1)}</dd>
            <dt className="text-muted-foreground">{C.WRITTEN_BY}</dt>
            <dd>{C.WRITTEN_BY_VALUE}</dd>
          </dl>
        ))}
      </div>
    </section>
  );
}
