/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The tool trail: each call is one collapsed line with its result, open to its arguments, its dry-run mark and the
// turn's token use; and the session's audit fields, read from the server's own events. Everything is text.
import * as React from "react";
import { ChevronRight, Wrench } from "lucide-react";
import { Link } from "react-router-dom";
import { Chip } from "../../wardyn/primitives";
import { ASSISTANT as T } from "../../wardyn/copy/assistant";
import { cn } from "../../ui/utils";
import type { Suggestion, ToolItem, TurnUsage } from "./composer-stream";

/** The usage of the first turn that ended after a call: the turn the call belongs to. */
export function usageAfter(turns: readonly TurnUsage[], item: { readonly gen: number; readonly seq: number }): Readonly<Record<string, number>> | null {
  return turns.find((t) => t.gen === item.gen && t.seq > item.seq)?.usage ?? null;
}

const usageText = (usage: Readonly<Record<string, number>>) =>
  Object.entries(usage)
    .map(([k, n]) => `${k} ${n}`)
    .join(" · ");

/** One call: a collapsed line, open to what was asked and what came back. */
export function ToolLine({ item, usage }: { item: ToolItem; usage?: Readonly<Record<string, number>> | null }) {
  const [open, setOpen] = React.useState(false);
  const detail = React.useId();
  return (
    <div className="rounded-lg border border-border bg-card/40 text-meta">
      <button
        type="button"
        aria-expanded={open}
        aria-controls={detail}
        onClick={() => setOpen((o) => !o)}
        className="mono flex w-full items-center gap-1.5 px-2 py-1 text-left"
      >
        <Wrench aria-hidden="true" className="size-3 shrink-0" />
        <span className="text-foreground">{item.name}</span>
        <span aria-hidden="true">→</span>
        <span className="min-w-0 flex-1 break-words text-muted-foreground">{item.result}</span>
        <ChevronRight aria-hidden="true" className={cn("size-3 shrink-0 transition-transform", open && "rotate-90")} />
      </button>
      <div id={detail} hidden={!open} className="space-y-1 border-t border-border px-2 py-1.5">
        {item.args !== "" && (
          <p className="whitespace-pre-wrap break-words">
            <span className="text-muted-foreground">{T.ARGUMENTS}</span> <span className="mono">{item.args}</span>
          </p>
        )}
        {item.dryRun && <Chip tone="neutral">{T.DRY_RUN}</Chip>}
        {usage && Object.keys(usage).length > 0 && (
          <p>
            <span className="text-muted-foreground">{T.TOKENS_USED}</span> <span className="mono">{usageText(usage)}</span>
          </p>
        )}
      </div>
    </div>
  );
}

export interface TrailAudit {
  readonly person: string;
  readonly purpose: string;
  readonly provider: string;
  readonly model: string;
  readonly linked: string;
  readonly runHref: string | null;
}

/** The card of calls beside the draft, and the audit row of the last completed turn. */
export function ComposerTrail({ tools, suggestion, audit }: { tools: readonly ToolItem[]; suggestion: Suggestion | null; audit: TrailAudit }) {
  const row = (label: string, value: string) => (
    <>
      <dt className="text-muted-foreground">{label}</dt>
      <dd className="mono min-w-0 break-words">{value}</dd>
    </>
  );
  return (
    <section data-testid="composer-trail" className="min-w-0 space-y-3 rounded-lg border border-border bg-card p-3">
      <div className="flex items-center justify-between gap-2">
        <span className="label-eyebrow">{T.TOOL_CALLS}</span>
        <Chip tone="neutral">{T.DRY_RUN}</Chip>
      </div>
      <ul className="space-y-1">
        {tools.map((t) => (
          <li key={`${t.gen}-${t.seq}`} className="mono flex flex-wrap gap-x-2 text-meta">
            <span className="text-foreground">{t.name}</span>
            <span className="min-w-0 break-words text-muted-foreground">{t.result}</span>
          </li>
        ))}
      </ul>
      <dl className="grid grid-cols-[auto_minmax(0,1fr)] gap-x-3 gap-y-1 text-meta">
        {row(T.AUDIT_PERSON, audit.person)}
        {row(T.AUDIT_PURPOSE, audit.purpose)}
        {row(T.COL_PROVIDER, audit.provider)}
        {row(T.AUDIT_MODEL, audit.model)}
        {suggestion && row(T.AUDIT_PROMPT_DIGEST, suggestion.promptDigest)}
        {suggestion && row(T.AUDIT_OUTPUT_DIGEST, suggestion.outputDigest)}
        {suggestion && row(T.AUDIT_TOOLS, suggestion.tools.join(", "))}
        {row(T.AUDIT_LINKED, audit.linked)}
      </dl>
      <p className="text-meta text-muted-foreground">{T.TRANSCRIPT_KEPT}</p>
      {audit.runHref && (
        <Link className="text-meta text-info underline" to={audit.runHref}>
          {T.OPEN_RUN}
        </Link>
      )}
    </section>
  );
}
