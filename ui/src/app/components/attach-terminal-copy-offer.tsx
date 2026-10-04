/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

/**
 * The copy offer (see attach-terminal-clipboard.ts): the verified text the
 * user's own selection produced, shown IN FULL, with an explicit Copy. The Copy
 * button, or Cmd/Ctrl+C while the offer is pending, is the only call to
 * navigator.clipboard.writeText that terminal output can lead to. Wording is
 * M11 canon (docs/design/terminal-escape-canon.md).
 */
import * as React from "react";
import { CircleAlert } from "lucide-react";
import { toast } from "sonner";
import { Button } from "./ui/button";
import { isMacPlatform, OFFER_TTL_MS, visibleParts, type CopyOffer } from "./attach-terminal-clipboard";
import { TERMINAL_COPY } from "./wardyn/copy";

const chipClass =
  "rounded border border-warning/40 bg-warning/10 px-1.5 py-0.5 font-mono text-meta text-warning";

export function CopyOfferToast({
  offer,
  onDone,
  copyRef,
}: {
  offer: CopyOffer;
  onDone: () => void;
  /** Filled with this card's Copy while it is mounted: the terminal's Cmd/Ctrl+C reaches it. */
  copyRef: React.MutableRefObject<(() => void) | null>;
}) {
  const [error, setError] = React.useState("");
  const [left, setLeft] = React.useState(OFFER_TTL_MS / 1000);
  const cardRef = React.useRef<HTMLDivElement>(null);
  const previewRef = React.useRef<HTMLPreElement>(null);
  const mac = isMacPlatform();

  // Focus stays in the terminal: a person who selects and then types must not
  // have those keystrokes swallowed by the card.
  React.useEffect(() => {
    const t = setInterval(() => setLeft((s) => Math.max(0, s - 1)), 1000);
    return () => clearInterval(t);
  }, []);

  const copy = async () => {
    try {
      await navigator.clipboard.writeText(offer.text);
      toast.success(TERMINAL_COPY.COPIED);
      onDone();
    } catch {
      setError(TERMINAL_COPY.WRITE_FAILED);
    }
  };

  React.useEffect(() => {
    copyRef.current = () => void copy();
    return () => {
      copyRef.current = null;
    };
  });

  return (
    <div
      ref={cardRef}
      role="group"
      aria-label={TERMINAL_COPY.OFFER_TITLE}
      data-testid="terminal-copy-offer"
      tabIndex={-1}
      onKeyDown={(e) => {
        if (e.key === "Escape") {
          e.preventDefault();
          onDone();
        } else if ((mac ? e.metaKey && !e.ctrlKey : e.ctrlKey && !e.metaKey) && e.key.toLowerCase() === "c") {
          // A selection inside the preview keeps the browser's own copy.
          if (previewRef.current && window.getSelection()?.toString()) return;
          e.preventDefault();
          void copy();
        }
      }}
      className="absolute bottom-3 right-3 z-10 flex w-[min(28rem,calc(100%-1.5rem))] flex-col gap-2 rounded-lg border border-border bg-popover p-3 text-xs text-popover-foreground shadow-floating focus:outline-none"
    >
      <div className="flex items-baseline gap-2">
        <span className="font-medium text-foreground">{TERMINAL_COPY.OFFER_TITLE}</span>
        <span className="text-meta text-muted-foreground">{TERMINAL_COPY.OFFER_SIZE(offer.chars)}</span>
      </div>
      <pre
        ref={previewRef}
        data-testid="terminal-copy-offer-text"
        className="scroll-thin max-h-48 overflow-auto whitespace-pre-wrap break-all rounded border border-border bg-background p-2 font-mono text-meta text-foreground"
      >
        {visibleParts(offer.text).map((p, i) =>
          p.hidden ? (
            <span key={i} className="text-warning">
              {p.text}
            </span>
          ) : (
            p.text
          ),
        )}
      </pre>
      {(offer.lineBreaks > 0 || offer.invisible > 0) && (
        <div className="flex flex-wrap gap-1.5">
          {offer.lineBreaks > 0 && <span className={chipClass}>{TERMINAL_COPY.OFFER_BREAKS(offer.lineBreaks)}</span>}
          {offer.invisible > 0 && <span className={chipClass}>{TERMINAL_COPY.OFFER_INVISIBLE(offer.invisible)}</span>}
        </div>
      )}
      <p className="text-meta text-muted-foreground">
        {TERMINAL_COPY.OFFER_EXPIRES(left)} · {TERMINAL_COPY.OFFER_KEYS(mac)}
      </p>
      {error && <p className="text-danger">{error}</p>}
      <div className="flex justify-end gap-2">
        <Button variant="ghost" size="sm" onClick={onDone}>
          {TERMINAL_COPY.DISMISS}
        </Button>
        <Button size="sm" onClick={() => void copy()}>
          {TERMINAL_COPY.COPY}
        </Button>
      </div>
    </div>
  );
}

/** The copy-blocked strip, in the connection-status slot below the grid. */
export function CopyBlockedNotice({ onDismiss }: { onDismiss: () => void }) {
  const hint = TERMINAL_COPY.SELECT_HINT(TERMINAL_COPY.NATIVE_CHORD(isMacPlatform()));
  return (
    <div
      role="status"
      data-testid="terminal-copy-notice"
      className="flex flex-wrap items-center gap-3 border-t border-border bg-card/60 px-3 py-2.5"
    >
      <CircleAlert className="size-4 shrink-0 text-muted-foreground" />
      <div className="min-w-0 flex-1">
        <p className="text-sm font-medium text-foreground">{TERMINAL_COPY.BLOCKED}</p>
        <p className="mt-0.5 text-xs text-muted-foreground">{hint}.</p>
      </div>
      <Button variant="ghost" size="sm" onClick={onDismiss}>
        {TERMINAL_COPY.DISMISS}
      </Button>
    </div>
  );
}
