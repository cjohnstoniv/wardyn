/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import * as React from "react";
import { Info } from "lucide-react";
import { Button } from "../ui/button";
import { Popover, PopoverContent, PopoverTrigger } from "../ui/popover";
import { useHoverOpen } from "./tooltip";

/**
 * The home for information a reader needs but a label cannot carry (CONSOLE-RULES §8).
 * Opens on click, tap, Enter, Space and after a hover pause; closes on Esc, outside click
 * and, when it was hover-opened, shortly after the pointer leaves. Never takes focus.
 */
export function InfoTip({
  label,
  children,
  side,
}: {
  /** The trigger's accessible name, e.g. "About Wall". */
  label: string;
  children: React.ReactNode;
  side?: "top" | "right" | "bottom" | "left";
}) {
  const [open, setOpen] = React.useState(false);
  const hover = useHoverOpen(open, setOpen);
  return (
    <Popover open={open} onOpenChange={hover.onOpenChange}>
      <PopoverTrigger asChild>
        <Button
          type="button"
          size="icon"
          variant="ghost"
          aria-label={label}
          onClick={(e) => {
            // A click on a bubble the hover just opened keeps it open rather than toggling it shut.
            if (hover.pinIfHoverOpened()) e.preventDefault();
          }}
          {...hover.triggerProps}
        >
          <Info aria-hidden="true" />
        </Button>
      </PopoverTrigger>
      <PopoverContent
        side={side}
        onOpenAutoFocus={(e) => e.preventDefault()}
        {...hover.contentProps}
        className="text-sm"
      >
        {children}
      </PopoverContent>
    </Popover>
  );
}
