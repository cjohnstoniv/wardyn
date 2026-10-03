/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

/**
 * The copy offer (see attach-terminal-clipboard.ts): the verified text the
 * user's own selection produced, shown IN FULL, with an explicit Copy. The Copy
 * button, or Cmd/Ctrl+C while the toast is focused, is the only call to
 * navigator.clipboard.writeText that terminal output can lead to.
 */
import * as React from "react";
import { Button } from "./ui/button";
import { COPY_TEXT, renderVisible, type CopyOffer } from "./attach-terminal-clipboard";

export function CopyOfferToast({ offer, onDone }: { offer: CopyOffer; onDone: () => void }) {
  const [error, setError] = React.useState("");
  const previewRef = React.useRef<HTMLPreElement>(null);

  const copy = async () => {
    try {
      await navigator.clipboard.writeText(offer.text);
      onDone();
    } catch {
      setError(COPY_TEXT.copyFailed);
    }
  };

  return (
    <div
      role="group"
      aria-label={COPY_TEXT.offerTitle}
      data-testid="terminal-copy-offer"
      tabIndex={-1}
      onKeyDown={(e) => {
        if (e.key === "Escape") {
          e.preventDefault();
          onDone();
        } else if ((e.metaKey || e.ctrlKey) && e.key.toLowerCase() === "c") {
          // A selection inside the preview keeps the browser's own copy.
          if (previewRef.current && window.getSelection()?.toString()) return;
          e.preventDefault();
          void copy();
        }
      }}
      className="absolute bottom-3 right-3 z-10 flex w-[min(28rem,calc(100%-1.5rem))] flex-col gap-2 rounded-lg border border-border bg-card p-3 text-xs shadow-lg"
    >
      <div className="flex items-center gap-2">
        <span className="font-medium text-foreground">{COPY_TEXT.offerTitle}</span>
        {offer.lineBreaks > 0 && (
          <span className="rounded border border-warning/40 bg-warning/10 px-1.5 py-0.5 font-mono text-meta text-warning">
            {COPY_TEXT.lineBreaks(offer.lineBreaks)}
          </span>
        )}
      </div>
      <pre
        ref={previewRef}
        data-testid="terminal-copy-offer-text"
        className="max-h-40 overflow-auto whitespace-pre-wrap break-all rounded border border-border bg-background p-2 font-mono text-meta text-foreground"
      >
        {renderVisible(offer.text)}
      </pre>
      {error && <p className="text-danger">{error}</p>}
      <div className="flex justify-end gap-2">
        <Button variant="ghost" size="sm" onClick={onDone}>
          {COPY_TEXT.dismiss}
        </Button>
        <Button size="sm" onClick={() => void copy()}>
          {COPY_TEXT.copy}
        </Button>
      </div>
    </div>
  );
}

export function CopyNotice({ message }: { message: string }) {
  return (
    <div
      role="status"
      data-testid="terminal-copy-notice"
      className="pointer-events-none absolute right-3 top-3 z-10 rounded-lg border border-border bg-card/90 px-2.5 py-1.5 font-mono text-meta text-muted-foreground"
    >
      {message}
    </div>
  );
}
