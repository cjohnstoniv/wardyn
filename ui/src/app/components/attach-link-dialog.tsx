/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The link confirm stop (M11 S6), built like attach-takeover-dialog.tsx. Every
// link it asks about was printed by the sandbox, so Cancel takes the initial
// focus (Radix's AlertDialog default) and a reflexive Enter opens nothing.
import { TriangleAlert } from "lucide-react";
import {
  AlertDialog,
  AlertDialogAction,
  AlertDialogCancel,
  AlertDialogContent,
  AlertDialogDescription,
  AlertDialogFooter,
  AlertDialogHeader,
  AlertDialogTitle,
} from "./ui/alert-dialog";
import { TERMINAL_LINK } from "./wardyn/copy";
import { openLink } from "./attach-terminal-links";

// `url.href` is the normalised form (punycode host, userinfo visible). It is
// drawn from the URL's own parts, so the highlighted run is `url.host` by
// construction and an `evil.example` inside the path can never be the bold one.
function HighlightedHref({ url }: { url: URL }) {
  const userinfo = url.username || url.password ? `${url.username}${url.password ? `:${url.password}` : ""}@` : "";
  return (
    <span className="break-all font-mono text-xs text-muted-foreground" data-testid="terminal-link-url">
      {url.protocol}//{userinfo}
      <span className="font-semibold text-foreground" data-testid="terminal-link-host">
        {url.host}
      </span>
      {url.pathname}
      {url.search}
      {url.hash}
    </span>
  );
}

export function LinkConfirmDialog({
  url,
  onClose,
  onCloseFocus,
}: {
  /** The parsed target to confirm; null = closed. */
  url: URL | null;
  onClose: () => void;
  /** Where focus lands when the dialog closes: the terminal, not the trigger. */
  onCloseFocus: () => void;
}) {
  const hasUserinfo = !!url && (url.username !== "" || url.password !== "");
  return (
    <AlertDialog open={url !== null} onOpenChange={(o) => !o && onClose()}>
      <AlertDialogContent
        data-testid="terminal-link-dialog"
        onCloseAutoFocus={(e) => {
          e.preventDefault();
          onCloseFocus();
        }}
      >
        <AlertDialogHeader>
          <AlertDialogTitle>{TERMINAL_LINK.TITLE}</AlertDialogTitle>
          <AlertDialogDescription>{TERMINAL_LINK.BODY}</AlertDialogDescription>
        </AlertDialogHeader>
        {url && (
          <div className="flex flex-col gap-2">
            <div className="rounded border border-border bg-background p-2">
              <HighlightedHref url={url} />
            </div>
            {hasUserinfo && (
              <p className="flex items-start gap-1.5 text-xs text-warning" data-testid="terminal-link-userinfo">
                <TriangleAlert className="mt-0.5 size-3.5 shrink-0" aria-hidden />
                {TERMINAL_LINK.USERINFO(url.host)}
              </p>
            )}
          </div>
        )}
        <AlertDialogFooter>
          <AlertDialogCancel>{TERMINAL_LINK.CANCEL}</AlertDialogCancel>
          <AlertDialogAction
            onClick={() => {
              if (url) openLink(url);
            }}
          >
            {TERMINAL_LINK.OPEN}
          </AlertDialogAction>
        </AlertDialogFooter>
      </AlertDialogContent>
    </AlertDialog>
  );
}
