/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// #217/#460 — the 412 "someone else saved first" banner, shared by every
// screen that PUTs a whole draft with an If-Match etag (providers-screen.tsx's
// Git/Storage tabs and agents-tab.tsx's own separate resource — it was a
// byte-for-byte copy of this same markup in both places before). One shape:
// the draft stays mounted and readable, and Copy my changes puts the WHOLE
// document on the clipboard (Q460-3, reversing #217's "changed fields only"
// rule) — never the other way around, so Discard replaces it with the
// server's version only once the admin has a copy of their own.
import * as React from "react";
import { Copy } from "lucide-react";
import { toast } from "sonner";
import { Button } from "../ui/button";
import { useCopyToClipboard } from "../../lib/use-copy-to-clipboard";
import { PROVIDERS, PROVIDERS_DRAFT } from "../../lib/workspace-providers-copy";

// Q460-3's clipboard-failure fallback: select the text so the person can copy
// it themselves with their own Ctrl/Cmd-C, rather than leaving them with
// nothing. Selection API, not `.select()` — the text lives in a <pre>, not a
// form control.
function selectText(el: HTMLElement | null): void {
  if (!el) return;
  const selection = window.getSelection();
  const range = document.createRange();
  range.selectNodeContents(el);
  selection?.removeAllRanges();
  selection?.addRange(range);
}

export function SavedElsewhereBanner({
  documentText,
  onDiscard,
}: {
  /** The WHOLE document as the editor currently holds it — what's shown here
   *  IS what Copy my changes puts on the clipboard (Q460-3), never a
   *  changed-fields-only summary. */
  documentText: string;
  onDiscard: () => void;
}) {
  const { copied, copyAsync } = useCopyToClipboard();
  const textRef = React.useRef<HTMLPreElement>(null);
  React.useEffect(() => {
    if (copied) toast.success(PROVIDERS_DRAFT.CONFLICT_COPIED_TOAST);
  }, [copied]);

  const handleCopy = async () => {
    const ok = await copyAsync(documentText);
    if (!ok) selectText(textRef.current);
  };

  return (
    <div className="space-y-3 rounded-lg border border-warning/30 bg-warning-subtle p-4">
      <p className="text-sm font-medium text-foreground">{PROVIDERS.SAVED_ELSEWHERE_TITLE}</p>
      <p className="text-body text-muted-foreground">{PROVIDERS.SAVED_ELSEWHERE_BODY}</p>
      <pre
        ref={textRef}
        className="scroll-thin max-h-36 overflow-auto rounded-md border border-border bg-surface-2 p-2.5 font-mono text-xs text-foreground"
      >
        {documentText}
      </pre>
      <div className="flex flex-wrap gap-2">
        <Button variant="outline" size="sm" onClick={handleCopy}>
          <Copy className="size-3.5" /> {PROVIDERS_DRAFT.CONFLICT_COPY}
        </Button>
        {/* #217: ghost, not outline — Copy is the one this banner wants
            pressed first, and Discard is no longer its only exit. Never a
            "save over theirs" arm — a security document is never
            last-writer-wins from this banner. */}
        <Button variant="ghost" size="sm" onClick={onDiscard}>
          {PROVIDERS_DRAFT.DISCARD_AND_RELOAD}
        </Button>
      </div>
    </div>
  );
}
