/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// #217 — the 412 "someone else saved first" banner, shared by every screen
// that PUTs a whole draft with an If-Match etag (providers-screen.tsx's Git/
// Storage tabs and agents-tab.tsx's own separate resource — it was a
// byte-for-byte copy of this same markup in both places before). One shape:
// the draft stays mounted and readable, and Copy my changes reads the edits
// out as text (lib/readable-diff.ts) before Discard replaces them with the
// server's version — never the other way around.
import * as React from "react";
import { Copy } from "lucide-react";
import { toast } from "sonner";
import { Button } from "../ui/button";
import { useCopyToClipboard } from "../../lib/use-copy-to-clipboard";
import { PROVIDERS, PROVIDERS_DRAFT } from "../../lib/workspace-providers-copy";

export function SavedElsewhereBanner({
  changedLines,
  onDiscard,
}: {
  /** One "path: before → after" line per changed field (lib/readable-diff.ts).
   *  Empty when the reload that produced this 412 already matches the draft. */
  changedLines: string[];
  onDiscard: () => void;
}) {
  const { copied, copy } = useCopyToClipboard();
  React.useEffect(() => {
    if (copied) toast.success(PROVIDERS_DRAFT.CONFLICT_COPIED_TOAST);
  }, [copied]);

  return (
    <div className="space-y-3 rounded-lg border border-warning/30 bg-warning-subtle p-4">
      <p className="text-sm font-medium text-foreground">{PROVIDERS.SAVED_ELSEWHERE_TITLE}</p>
      <p className="text-body text-muted-foreground">{PROVIDERS.SAVED_ELSEWHERE_BODY}</p>
      {changedLines.length > 0 && (
        <pre className="scroll-thin max-h-36 overflow-auto rounded-md border border-border bg-surface-2 p-2.5 font-mono text-xs text-foreground">
          {changedLines.join("\n")}
        </pre>
      )}
      <div className="flex flex-wrap gap-2">
        {changedLines.length > 0 && (
          <Button variant="outline" size="sm" onClick={() => copy(changedLines.join("\n"))}>
            <Copy className="size-3.5" /> {PROVIDERS_DRAFT.CONFLICT_COPY}
          </Button>
        )}
        {/* #217: ghost, not outline — Copy is the one this banner wants
            pressed first, and Discard is no longer its only exit. */}
        <Button variant="ghost" size="sm" onClick={onDiscard}>
          {PROVIDERS_DRAFT.DISCARD_AND_RELOAD}
        </Button>
      </div>
    </div>
  );
}
