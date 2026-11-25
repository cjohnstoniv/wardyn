/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// What will be sent: the records the first message carries, byte for byte, the provider they go to and what is never
// sent. Nothing leaves before Start. Every section is shown as text.
import { Lock } from "lucide-react";
import { CHANGES } from "../../../lib/governance-copy";
import type { AssistantPreview } from "../../../lib/api/assistant";
import { Button } from "../../ui/button";
import { ASSISTANT as T } from "../../wardyn/copy/assistant";
import { NR_SAVE } from "../../wardyn/copy/new-run/save";
import { Chip } from "../../wardyn/primitives";

/** A record kind in the console's words. */
export const kindLabel = (kind: string): string => CHANGES.KIND[kind] ?? kind;

export interface ComposerPreviewProps {
  preview: AssistantPreview;
  /** What will be sent changed since the person last looked: say so above the preview. */
  changed: boolean;
  busy: boolean;
  /** A refusal of Start, in the server's own sentence. */
  error: string | null;
  onStart: () => void;
  onCancel: () => void;
}

export function ComposerPreview({ preview, changed, busy, error, onStart, onCancel }: ComposerPreviewProps) {
  const provider = `${preview.provider} · ${preview.model}`;
  return (
    <section data-testid="composer-preview" className="min-w-0 space-y-3 rounded-xl border border-border bg-card p-4">
      {changed && (
        <p role="status" className="rounded-lg border border-warning/35 bg-warning-subtle px-3 py-2 text-body text-foreground">
          {T.PREVIEW_CHANGED}
        </p>
      )}
      <div className="flex items-center justify-between gap-2">
        <h2 className="text-base font-medium">{T.PREVIEW_TITLE}</h2>
        <Chip tone="info">
          <Lock aria-hidden="true" className="size-3" />
          {T.ALWAYS_ON}
        </Chip>
      </div>
      <p className="text-body text-muted-foreground">{T.PREVIEW_LEAD(provider)}</p>
      <p data-testid="composer-preview-addition" className="text-body text-muted-foreground">
        {T.PREVIEW_ADDITION}
      </p>
      <div className="grid gap-4 min-[761px]:grid-cols-[minmax(0,1.6fr)_minmax(0,1fr)]">
        <div className="min-w-0 space-y-2.5">
          {preview.sections.map((s, i) => (
            <div key={`${s.label}-${i}`} className="min-w-0 rounded-lg border border-border p-2.5">
              <div className="flex items-center justify-between gap-2">
                <span className="label-eyebrow">{s.label}</span>
                {s.untrusted && <Chip tone="warning">{T.UNTRUSTED}</Chip>}
              </div>
              <pre className="mono mt-1 whitespace-pre-wrap break-words text-meta">{s.text}</pre>
            </div>
          ))}
        </div>
        <div className="min-w-0">
          <p className="label-eyebrow">{T.NEVER_SENT_HEADING}</p>
          <ul className="mt-1.5 list-disc space-y-1 pl-4.5 text-body text-foreground">
            <li>{T.NEVER_SECRETS}</li>
            <li>{T.NEVER_CREDENTIALS}</li>
            <li>{T.NEVER_SOURCE_FILES}</li>
            <li>{T.NEVER_AUDIT}</li>
            <li>{T.NEVER_MOUNTS}</li>
            <li>{T.NEVER_ROWS}</li>
            {preview.never_sent.length > 0 && <li>{T.KINDS_KEPT_AWAY(preview.never_sent.map(kindLabel).join(", "))}</li>}
          </ul>
        </div>
      </div>
      {error && (
        <p role="alert" className="text-body text-danger">
          {error}
        </p>
      )}
      <div className="flex justify-end gap-2">
        <Button type="button" variant="ghost" onClick={onCancel}>
          {NR_SAVE.CANCEL_SAVE}
        </Button>
        <Button type="button" disabled={busy} onClick={onStart}>
          {T.START}
        </Button>
      </div>
    </section>
  );
}
