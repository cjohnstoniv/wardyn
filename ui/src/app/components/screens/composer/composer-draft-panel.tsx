/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The assistant's proposal, shown read-only beside the chat: one block per part, grouped under the run policy
// Summary's own section names, with what the ceiling refused listed under the parts. Every value is text.
import { NR_TOOLS } from "../../wardyn/copy/new-run/tools";
import { Chip } from "../../wardyn/primitives";
import { ASSISTANT as T } from "../../wardyn/copy/assistant";
import { FIELD_LABEL, SUMMARY } from "../run-detail/policy-tab-copy";
import type { DroppedEntry, Proposal } from "./composer-stream";

// The part names of a draft, in the Summary's words. A name this map lacks falls through to the Summary's field labels.
const SECTION_LABEL: Record<string, string> = {
  allowed_hosts: SUMMARY.allowedHosts,
  network: SUMMARY.network,
  credentials: SUMMARY.credentials,
  tools: SUMMARY.tools,
  repositories: SUMMARY.repos,
  barrier: SUMMARY.barrier,
  image: NR_TOOLS.DETAIL_IMAGE,
};
const sectionLabel = (section: string) => SECTION_LABEL[section] ?? FIELD_LABEL[section] ?? section;

/** A part's value as the lines the panel shows. */
export function valueLines(value: unknown): string[] {
  if (value == null) return [];
  if (Array.isArray(value)) return value.flatMap(valueLines);
  if (typeof value === "object") return Object.entries(value as Record<string, unknown>).map(([k, v]) => `${k}: ${valueLines(v).join(", ")}`);
  return [String(value)];
}

/** How many lines the draft holds: what the phone's bar counts. */
export function draftLines(proposal: Proposal | null): number {
  return proposal ? proposal.parts.reduce((n, p) => n + Math.max(1, valueLines(p.value).length), 0) : 0;
}

const leftOut = (d: DroppedEntry) => T.LEFT_OUT(d.host ?? d.field ?? "", d.unit ?? "");

export function ComposerDraftPanel({ proposal }: { proposal: Proposal | null }) {
  return (
    <section data-testid="composer-draft-panel" className="min-w-0 space-y-3">
      <p className="text-body text-muted-foreground">{T.DRAFT_LEAD}</p>
      {proposal?.parts.map((part, i) => (
        <div key={`${part.section}-${i}`} className="rounded-lg border border-info/35 bg-info-subtle/40 px-2.5 py-2">
          <div className="flex items-center justify-between gap-2">
            <span className="label-eyebrow">{sectionLabel(part.section)}</span>
            <Chip tone="info">{T.PART_PROPOSED}</Chip>
          </div>
          <ul className="mt-1 flex flex-wrap gap-1.5">
            {valueLines(part.value).map((line, j) => (
              <li key={j} className="mono min-w-0 break-words rounded-md border border-border px-1.5 py-0.5 text-meta">
                {line}
              </li>
            ))}
          </ul>
        </div>
      ))}
      {proposal?.dropped.map((d, i) => (
        <p key={i} data-testid="composer-left-out" className="rounded-lg border border-warning/35 bg-warning-subtle px-2.5 py-2 text-body text-foreground">
          {leftOut(d)}
        </p>
      ))}
    </section>
  );
}
