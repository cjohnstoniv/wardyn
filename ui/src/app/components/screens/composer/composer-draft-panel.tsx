/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The draft beside the chat. A run draft is the person's own editor: the assistant proposes parts, and a part is in the
// draft once the person accepts it. Any other target still shows the proposal read-only, grouped under the run policy
// Summary's own section names, with what the ceiling refused listed under the parts. Every value is text.
import { useLocation } from "react-router-dom";
import { composerTarget } from "../../../lib/composer-url";
import { Chip } from "../../wardyn/primitives";
import { ASSISTANT as T } from "../../wardyn/copy/assistant";
import type { AssistantTarget } from "../../../lib/api/assistant";
import { DroppedHosts } from "./editor/dropped-hosts";
import { sectionLabel, valueLines } from "./editor/part-card";
import { RunEditor } from "./editor/run-editor";
import type { Proposal } from "./composer-stream";

export { valueLines };

/** How many lines the draft holds: what the phone's bar counts. */
export function draftLines(proposal: Proposal | null): number {
  return proposal ? proposal.parts.reduce((n, p) => n + Math.max(1, valueLines(p.value).length), 0) : 0;
}

/** `target` is what the draft is for; it is the address's own target when the screen names none. */
export function ComposerDraftPanel({ proposal, sessionId, target: named }: { proposal: Proposal | null; sessionId?: string; target?: AssistantTarget }) {
  const { search } = useLocation();
  const target = named ?? composerTarget(search);
  const runDraft = target.kind === "run";
  return (
    <section data-testid="composer-draft-panel" className="min-w-0 space-y-3">
      <p className="text-body text-muted-foreground">{T.DRAFT_LEAD}</p>
      {runDraft ? (
        <RunEditor proposal={proposal} sessionId={sessionId} target={target} />
      ) : (
        <>
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
          <DroppedHosts items={proposal?.dropped ?? []} />
        </>
      )}
    </section>
  );
}
