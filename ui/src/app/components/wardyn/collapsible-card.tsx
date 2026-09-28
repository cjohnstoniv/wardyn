/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// CollapsibleCard (#1200, compact cards packet) — the shared card shell for
// Admin Settings and Your account. The owner measured both pages taller than
// the 744px viewport with every card fully open; the fix approved there is
// "compact cards": every card collapses to a one-line summary and expands on
// click, none open by default. Collapsed-by-default is load-bearing, not a
// preference — it is what keeps both pages under 744px regardless of which
// of a card's own conditional branches renders (an admin with a k8s runner,
// a member with no model block, etc.); a card that opened by default could
// not make that guarantee across every shape its body takes.
//
// A disclosure button, not a link: it changes what is already on the page
// rather than navigating (WAI-ARIA disclosure pattern). `add-workspace-
// dialog.tsx`'s Disclosure is the same idea at dialog scope ("nothing like it
// existed... to reuse") — this is the card-chrome version, reused across
// every Settings/Your account card, so this file is what the next one reuses.
//
// Controlled (open/onOpenChange) and uncontrolled (defaultOpen) both work,
// matching the Popover/Dialog convention already used across the console
// (corp-network-egress.tsx's own combobox) — a controlled caller needs it to
// force-open a card a deep link points at (ado-connection.tsx's `#azure-
// devops` hash-focus effect).
import * as React from "react";
import { ChevronDown } from "lucide-react";
import { cn } from "../ui/utils";

export interface CollapsibleCardProps
  extends Omit<React.HTMLAttributes<HTMLElement>, "title"> {
  title: React.ReactNode;
  /** The one-line state summary shown beside the title, collapsed or
   *  expanded — omit while the state that would produce it hasn't loaded yet,
   *  the same "absent, not a guess" rule every card's own summary follows. */
  summary?: React.ReactNode;
  open?: boolean;
  defaultOpen?: boolean;
  onOpenChange?: (open: boolean) => void;
  testId?: string;
  /** Applied to the title element, for a caller that needs aria-labelledby
   *  on its own content (branding-card.tsx's preview column, for example). */
  headingId?: string;
}

export const CollapsibleCard = React.forwardRef<HTMLElement, CollapsibleCardProps>(
  function CollapsibleCard(
    { title, summary, open: openProp, defaultOpen = false, onOpenChange, testId, headingId, children, className, ...rest },
    ref,
  ) {
    const [uncontrolledOpen, setUncontrolledOpen] = React.useState(defaultOpen);
    const open = openProp ?? uncontrolledOpen;
    const bodyId = React.useId();

    const toggle = () => {
      const next = !open;
      if (openProp === undefined) setUncontrolledOpen(next);
      onOpenChange?.(next);
    };

    return (
      <section
        ref={ref}
        data-testid={testId}
        className={cn("rounded-xl border border-border bg-card", className)}
        {...rest}
      >
        <button
          type="button"
          aria-expanded={open}
          aria-controls={bodyId}
          onClick={toggle}
          // p-3, not p-4: this header IS the collapsed row on Settings/Your
          // account, where seven of these plus the page header must fit
          // 744px (settings-compact-1200-packet.html §4) — p-4 measured a
          // few px over at 1280x744 in the real browser. The body keeps p-4;
          // only the always-visible collapsed row is tighter.
          className="flex w-full items-start justify-between gap-3 rounded-xl p-3 text-left outline-none focus-visible:ring-[3px] focus-visible:ring-ring"
        >
          <span className="min-w-0">
            <h3 id={headingId} className="text-sm font-medium text-foreground">
              {title}
            </h3>
            {summary && (
              <span className="mt-0.5 block truncate text-body leading-snug text-muted-foreground">
                {summary}
              </span>
            )}
          </span>
          <ChevronDown
            aria-hidden
            className={cn(
              "mt-0.5 size-4 shrink-0 text-muted-foreground transition-transform",
              open && "rotate-180",
            )}
          />
        </button>
        {open && (
          <div id={bodyId} className="border-t border-border p-4 pt-3">
            {children}
          </div>
        )}
      </section>
    );
  },
);
