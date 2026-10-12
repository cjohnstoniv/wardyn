/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The YAML and JSON views: the draft's real `{api_version, kind: Run, intent}`
// document, read-only, with Copy and the way into the editor.

import { Button } from "../../../ui/button";
import { NR_REVIEW } from "../../../wardyn/copy/new-run/review";
import { useCopyToClipboard } from "../../../../lib/use-copy-to-clipboard";
import type { NewRunLocation } from "../draft/new-run-url";
import type { RunIntent } from "../draft/run-intent";
import { runDocumentText, type DocumentFormat } from "../document/run-document";

export function RawView({ intent, format, go }: { intent: RunIntent; format: DocumentFormat; go: (loc: NewRunLocation) => void }) {
  const { copied, copy } = useCopyToClipboard();
  const text = runDocumentText(intent, format);
  const name = format.toUpperCase();
  return (
    <section className="flex flex-col gap-3 rounded-lg border border-border p-4" aria-label={NR_REVIEW.RAW_TITLE(name)}>
      <div className="flex items-start justify-between gap-3">
        <div>
          <h2 className="font-medium">{NR_REVIEW.RAW_TITLE(name)}</h2>
          <p className="text-sm text-muted-foreground">{NR_REVIEW.RAW_BODY}</p>
        </div>
        <Button type="button" variant="outline" onClick={() => void copy(text)}>
          {copied ? NR_REVIEW.COPIED : NR_REVIEW.COPY}
        </Button>
        <span role="status" className="sr-only">{copied ? NR_REVIEW.COPIED : ""}</span>
      </div>
      <pre className="scroll-thin overflow-x-auto rounded-lg border border-border bg-surface-2/60 p-3 text-xs leading-relaxed">
        <code className="font-mono">{text}</code>
      </pre>
      <Button type="button" variant="outline" className="w-fit" onClick={() => go({ screen: "document", format })}>
        {NR_REVIEW.EDIT_FORMAT(name)}
      </Button>
    </section>
  );
}
