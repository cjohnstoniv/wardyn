/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import * as React from "react";
import { ChevronRight } from "lucide-react";
import { cn } from "../ui/utils";

const storageKey = (id: string) => `wardyn.how.${id}`;

function readOpen(id: string): boolean {
  try {
    return window.localStorage.getItem(storageKey(id)) === "open";
  } catch {
    return false;
  }
}

/** The longer explanation behind a page's one-sentence lede (#3060): closed by default, and it remembers
 *  whether this page's block was last left open. */
export function HowItWorks({
  id,
  title,
  className,
  children,
}: {
  /** Names the remembered state; one per page. */
  id: string;
  title: string;
  className?: string;
  children: React.ReactNode;
}) {
  const [open, setOpen] = React.useState(() => readOpen(id));
  return (
    <details
      data-testid={`how-${id}`}
      open={open}
      onToggle={(e) => {
        const next = e.currentTarget.open;
        setOpen(next);
        try {
          window.localStorage.setItem(storageKey(id), next ? "open" : "closed");
        } catch {
          // Private mode: the block still opens, it just isn't remembered.
        }
      }}
      className={cn("group", className)}
    >
      <summary className="flex w-fit cursor-pointer select-none items-center gap-1.5 text-xs font-medium text-muted-foreground hover:text-foreground">
        <ChevronRight aria-hidden className="size-3.5 transition-transform group-open:rotate-90" />
        {title}
      </summary>
      <div className="mt-2 max-w-[82ch] space-y-1 text-body text-muted-foreground">{children}</div>
    </details>
  );
}
