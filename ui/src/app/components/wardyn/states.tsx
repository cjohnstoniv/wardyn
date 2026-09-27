/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import * as React from "react";
import { AlertTriangle, RotateCw } from "lucide-react";
import { Button } from "../ui/button";
import { cn } from "../ui/utils";
import { HttpError } from "../../lib/api/core";

// #457 (docs/design/signin-first-contact-canon.md): ErrorState's default
// message was "We couldn't reach the Wardyn control plane. Please try
// again." — jargon ("control plane") plus a "Please" no other refusal in the
// console uses. Frozen; every pane that renders ErrorState with no `message`
// of its own reads this same honest default.
export const STATES = {
  ERROR_TITLE: "Something went wrong",
  ERROR_DEFAULT: "Wardyn isn't answering. Try again.",
  RETRY: "Retry",
} as const;

// X3-F5 — the third arm every gated screen needs. A 403 is not "we couldn't
// reach the control plane": the daemon answered, and answered about this
// caller's tier, so a Retry over it retries forever. Screens that already had
// this three-way (drives, providers) re-derived the predicate inline; the two
// that did not (permissions, governance) reported a tier as an outage. One
// helper, so the arms cannot drift apart again. A screen still renders its own
// forbidden branch — what the sentence says is the SCREEN's tier, not this
// helper's business.
export type ScreenStatus = "loading" | "forbidden" | "error" | "ready";

export function loadFailStatus(e: unknown): "forbidden" | "error" {
  return e instanceof HttpError && e.status === 403 ? "forbidden" : "error";
}

// F7-F10: both states used to hardcode <h3>, correct only when a caller
// happens to sit under an h1+h2 — several don't (an EmptyState/ErrorState
// that IS the whole visible content of a section, with nothing else on the
// page supplying an h2), so the DOM heading order skipped a level (WCAG
// 1.3.1/2.4.6). `as` lets a caller name the level it actually sits at;
// defaulting to h2 (rather than h3) is the safer floor — a section's OWN
// primary message, one level under the screen's PageHeader h1.
type HeadingTag = "h1" | "h2" | "h3" | "h4" | "h5" | "h6";

export function EmptyState({
  icon: Icon,
  title,
  description,
  action,
  className,
  as = "h2",
}: {
  icon: React.ElementType;
  title: string;
  // ReactNode, not string: a frozen empty-state sentence may carry an env var
  // or a wire literal that renders mono, and the mono span is applied at the
  // CALL SITE (the withMono precedent) rather than baked into the copy. A
  // `string` here was the one reason GOVERNANCE.EMPTY_BODY's
  // WARDYN_DEFAULT_POLICY could not look like every other literal on the
  // screen. Plain strings still pass unchanged — every other caller is one.
  description?: React.ReactNode;
  action?: React.ReactNode;
  className?: string;
  /** Heading level for `title` — see the module note above. Default h2. */
  as?: HeadingTag;
}) {
  const Heading = as;
  return (
    <div className={cn("flex flex-col items-center justify-center gap-3 px-6 py-16 text-center", className)}>
      <div className="flex size-12 items-center justify-center rounded-xl border border-border bg-surface-2 text-muted-foreground">
        <Icon className="size-5" />
      </div>
      <div className="space-y-1">
        <Heading className="text-foreground">{title}</Heading>
        {description && <p className="max-w-sm text-sm text-muted-foreground">{description}</p>}
      </div>
      {action}
    </div>
  );
}

export function ErrorState({
  message,
  onRetry,
  action,
  as = "h2",
}: {
  message?: string;
  onRetry?: () => void;
  // A persistent error needs something to read, retry, or act on (rulebook
  // §9). `onRetry` stays the plain-Retry shorthand; `action` is the same
  // open-ended slot EmptyState already takes, for a caller that wants the
  // retry rendered itself (or something other than retry).
  action?: React.ReactNode;
  /** Heading level for "Something went wrong" — see the module note above. Default h2. */
  as?: HeadingTag;
}) {
  const Heading = as;
  return (
    <div className="flex flex-col items-center justify-center gap-3 px-6 py-16 text-center">
      <div className="flex size-12 items-center justify-center rounded-xl border border-danger/30 bg-danger-subtle text-danger">
        <AlertTriangle className="size-5" />
      </div>
      <div className="space-y-1">
        <Heading className="text-foreground">{STATES.ERROR_TITLE}</Heading>
        <p className="max-w-sm text-sm text-muted-foreground">
          {message ?? STATES.ERROR_DEFAULT}
        </p>
      </div>
      {onRetry && (
        <Button variant="outline" size="sm" onClick={onRetry}>
          <RotateCw className="size-3.5" /> {STATES.RETRY}
        </Button>
      )}
      {action}
    </div>
  );
}

// A list that came back with exactly as many rows as the console asked for is a
// WINDOW, not the whole set — and every search/facet on these screens filters
// client-side over that window, so past the cap "no matches" can be a lie. The
// server states it exactly (X-Wardyn-Truncated), but the client decodes only the
// body; `count >= cap` is the honest stand-in, because the fetch asks for the
// server's own max (LIST_LIMIT) and gets it. Renders nothing below the cap.
export function TruncatedNote({
  count,
  cap,
  children,
}: {
  count: number;
  cap: number;
  // Screen-specific wording; omit for the plain list sentence.
  children?: React.ReactNode;
}) {
  if (count < cap) return null;
  return (
    <div className="mb-4 flex items-center gap-2 rounded-lg border border-warning/30 bg-warning-subtle px-3 py-2 text-xs text-warning">
      <AlertTriangle className="size-3.5 shrink-0" />
      <span>
        {children ?? `Showing the first ${cap} (truncated) — search and filters here cover only this window.`}
      </span>
    </div>
  );
}

export function TableSkeleton({ rows = 6, cols = 6 }: { rows?: number; cols?: number }) {
  return (
    <div className="divide-y divide-border">
      {Array.from({ length: rows }).map((_, r) => (
        <div key={r} className="flex items-center gap-4 px-4 py-3.5">
          {Array.from({ length: cols }).map((_, c) => (
            <div
              key={c}
              className="h-3.5 animate-pulse rounded bg-muted"
              style={{ width: c === 0 ? 90 : c === cols - 1 ? 40 : `${30 + ((r + c) % 4) * 14}%` }}
            />
          ))}
        </div>
      ))}
    </div>
  );
}
