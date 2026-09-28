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
        {/* APG accordion shape (review FINAL-PR-1329.md M3) — the heading
            wraps the button rather than nesting inside it: a `<h3>` is not
            phrasing content, so a button containing one is invalid HTML, and
            an engine/AT that honours ARIA's button-children-presentational
            rule (unlike Chromium) drops it from heading navigation. `h3`'s
            own base-layer margin reset (theme.css) already makes it a
            zero-margin block, so wrapping the (already `w-full`) button adds
            no visible box of its own.
            `aria-label` pins the heading's OWN accessible name back to the
            title alone: without it, the heading's name is its full text
            content, which is now the button's (title + summary), so every
            "Host"-only heading query would start matching "Host Runs default
            to Vault" instead. Every real caller's `title` is a plain string;
            a caller that ever passes a richer node (none does today) simply
            keeps the old, unoverridden behavior. */}
        <h3 id={headingId} aria-label={typeof title === "string" ? title : undefined}>
          <button
            type="button"
            aria-expanded={open}
            aria-controls={bodyId}
            onClick={toggle}
            // px-4 py-3, not p-3 (review L1): the body is p-4 — matching the
            // horizontal inset keeps the title flush with the body text once
            // expanded (CONSOLE-RULES §11 "one grid"). Only the vertical
            // inset is trimmed, which is what the 744px fit needed.
            className="flex w-full items-start justify-between gap-3 rounded-xl px-4 py-3 text-left outline-none focus-visible:ring-[3px] focus-visible:ring-ring"
          >
            <span className="min-w-0">
              <span className="block text-sm font-medium text-foreground">{title}</span>
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
        </h3>
        {open && (
          <div id={bodyId} className="border-t border-border p-4 pt-3">
            {children}
          </div>
        )}
      </section>
    );
  },
);
