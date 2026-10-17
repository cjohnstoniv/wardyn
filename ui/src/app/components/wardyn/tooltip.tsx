/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import * as React from "react";
import { Popover, PopoverAnchor, PopoverContent } from "../ui/popover";
import { cn } from "../ui/utils";

/** Hover opens after this long; a pointer that left has this long to reach the bubble. */
export const HOVER_OPEN_MS = 700;
export const HOVER_GRACE_MS = 150;

/**
 * Hover-intent for the Popover-based Tooltip and InfoTip: open after HOVER_OPEN_MS,
 * close HOVER_GRACE_MS after the pointer leaves both trigger and bubble (WCAG 1.4.13
 * hoverable). Only a hover-opened bubble closes on leave; one opened by focus, click
 * or tap stays until Esc, outside click or blur. Touch pointers never hover-open.
 * `openOnFocus` waits the same HOVER_OPEN_MS (its own timer, so a pointer passing over
 * does not cancel it); `onOpenChange` cancels both.
 */
export function useHoverOpen(open: boolean, setOpen: (open: boolean) => void) {
  const timer = React.useRef<ReturnType<typeof setTimeout> | undefined>(undefined);
  const focusTimer = React.useRef<ReturnType<typeof setTimeout> | undefined>(undefined);
  const hoverOpened = React.useRef(false);
  const clear = () => clearTimeout(timer.current);
  React.useEffect(
    () => () => {
      clearTimeout(timer.current);
      clearTimeout(focusTimer.current);
    },
    [],
  );
  return {
    /** Wire to Popover's onOpenChange: every non-hover close or open forgets the hover origin. */
    onOpenChange(next: boolean) {
      clear();
      clearTimeout(focusTimer.current);
      hoverOpened.current = false;
      setOpen(next);
    },
    /** Esc while a bubble is still pending (closed, so Radix is not listening): drop the pending open. */
    cancelPending() {
      clear();
      clearTimeout(focusTimer.current);
    },
    /** Keyboard focus shows the bubble after the same pause as a hover, not at once. */
    openOnFocus() {
      clearTimeout(focusTimer.current);
      if (!open) focusTimer.current = setTimeout(() => setOpen(true), HOVER_OPEN_MS);
    },
    /** True once, when a click lands on a hover-opened bubble: it pins instead of toggling. */
    pinIfHoverOpened(): boolean {
      const was = hoverOpened.current;
      hoverOpened.current = false;
      return was;
    },
    triggerProps: {
      onPointerEnter(e: React.PointerEvent) {
        if (e.pointerType === "touch") return;
        clear();
        if (open) return;
        timer.current = setTimeout(() => {
          hoverOpened.current = true;
          setOpen(true);
        }, HOVER_OPEN_MS);
      },
      onPointerLeave: leave,
    },
    contentProps: { onPointerEnter: clear, onPointerLeave: leave },
  };

  function leave() {
    clear();
    if (!hoverOpened.current) return;
    timer.current = setTimeout(() => {
      hoverOpened.current = false;
      setOpen(false);
    }, HOVER_GRACE_MS);
  }
}

type Handler<E> = ((e: E) => void) | undefined;
const both =
  <E,>(a: Handler<E>, b: Handler<E>) =>
  (e: E) => {
    a?.(e);
    b?.(e);
  };

/** Keyboard focus shows the label after the hover pause; a mouse click that focuses the trigger never does. */
function focusIsVisible(el: Element): boolean {
  try {
    return el.matches(":focus-visible");
  } catch {
    return true;
  }
}

/**
 * A label for a control that already has an accessible name; never the only home of
 * essential information (CONSOLE-RULES §8). Radix Popover supplies the layering, so no
 * extra package: the bubble overrides Popover's dialog role to tooltip and never takes focus.
 */
export function Tooltip({
  label,
  children,
  side = "top",
}: {
  label: string;
  children: React.ReactElement<React.HTMLAttributes<HTMLElement>>;
  side?: "top" | "right" | "bottom" | "left";
}) {
  const id = React.useId();
  const [open, setOpen] = React.useState(false);
  const hover = useHoverOpen(open, setOpen);
  const child = children.props;
  return (
    <Popover open={open} onOpenChange={hover.onOpenChange}>
      <PopoverAnchor asChild>
        {React.cloneElement(children, {
          // Keep the control's own description (a disabled reason): the tooltip adds to it.
          "aria-describedby": [child["aria-describedby"], open ? id : null].filter(Boolean).join(" ") || undefined,
          onPointerEnter: both(child.onPointerEnter, hover.triggerProps.onPointerEnter),
          onPointerLeave: both(child.onPointerLeave, hover.triggerProps.onPointerLeave),
          onFocus: both(child.onFocus, (e: React.FocusEvent) => {
            if (focusIsVisible(e.currentTarget)) hover.openOnFocus();
          }),
          onKeyDown: both(child.onKeyDown, (e: React.KeyboardEvent) => {
            if (e.key === "Escape") hover.cancelPending();
          }),
          onBlur: both(child.onBlur, () => hover.onOpenChange(false)),
        })}
      </PopoverAnchor>
      <PopoverContent
        id={id}
        role="tooltip"
        side={side}
        sideOffset={6}
        onOpenAutoFocus={(e) => e.preventDefault()}
        onCloseAutoFocus={(e) => e.preventDefault()}
        onPointerEnter={hover.contentProps.onPointerEnter}
        onPointerLeave={hover.contentProps.onPointerLeave}
        className={cn("w-auto max-w-xs border-0 bg-foreground px-2 py-1 text-xs text-background shadow-floating")}
      >
        {label}
      </PopoverContent>
    </Popover>
  );
}
