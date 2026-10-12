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
 * and, when it was hover-opened, shortly after the pointer leaves. A keyboard open moves
 * focus into the content (it is named, and Esc returns focus), so its text is reachable
 * without a mouse; a pointer or hover open leaves focus where it was.
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
  const id = React.useId();
  const [open, setOpen] = React.useState(false);
  const hover = useHoverOpen(open, setOpen);
  const byKeyboard = React.useRef(false);
  return (
    <Popover
      open={open}
      onOpenChange={(next) => {
        if (!next) byKeyboard.current = false;
        hover.onOpenChange(next);
      }}
    >
      <PopoverTrigger asChild>
        <Button
          type="button"
          size="icon"
          variant="ghost"
          aria-label={label}
          aria-describedby={open ? id : undefined}
          onKeyDown={(e) => {
            if (e.key === "Enter" || e.key === " ") byKeyboard.current = true;
          }}
          onPointerDown={() => {
            byKeyboard.current = false;
          }}
          onClick={(e) => {
            // A click on a bubble the hover just opened keeps it open rather than toggling it shut.
            if (hover.pinIfHoverOpened()) e.preventDefault();
          }}
          {...hover.triggerProps}
          onPointerEnter={(e) => {
            byKeyboard.current = false;
            hover.triggerProps.onPointerEnter(e);
          }}
        >
          <Info aria-hidden="true" />
        </Button>
      </PopoverTrigger>
      <PopoverContent
        id={id}
        aria-label={label}
        side={side}
        onOpenAutoFocus={(e) => {
          if (!byKeyboard.current) e.preventDefault();
        }}
        {...hover.contentProps}
        className="text-sm"
      >
        {children}
      </PopoverContent>
    </Popover>
  );
}
