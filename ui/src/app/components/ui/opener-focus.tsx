import * as React from "react";

// Radix hands focus back on close only to a DialogTrigger. A dialog opened from state has none, so focus
// fell to the page body. The content remembers the element that had focus when it mounted and, on close,
// puts focus back there when focus would otherwise be lost: not if the consumer handled it, the caller already
// moved focus elsewhere, the element is gone, or it was only the body.

/**
 * A dialog opened from a dropdown item has that item as the focused element, and the item unmounts with
 * its menu. The element to return to is then the menu's trigger, the one whose aria-controls is the menu's id.
 */
function openerOf(active: Element | null): Element | null {
  const menu = active?.closest('[role="menu"]');
  if (!menu?.id) return active;
  for (const el of document.querySelectorAll("[aria-controls]")) {
    if (el.getAttribute("aria-controls") === menu.id) return el;
  }
  return active;
}

/** Rendered inside the dialog content, so its first render reads the focused element before autofocus moves it. */
export function RememberOpener({ into }: { into: React.MutableRefObject<Element | null> }) {
  if (into.current === null) into.current = openerOf(document.activeElement);
  return null;
}

/**
 * The content's onCloseAutoFocus. Radix runs it after the exit animation, so a caller may already have put
 * focus somewhere on purpose (a heading, the next field): that is left alone and Radix does what it did before.
 * A consumer's own handler is passed through untouched.
 */
export function restoreOpenerFocus(
  opener: React.MutableRefObject<Element | null>,
  consumer?: (event: Event) => void,
): (event: Event) => void {
  return (event) => {
    const el = opener.current;
    opener.current = null;
    if (consumer) return consumer(event);
    const active = document.activeElement;
    const placed =
      active instanceof HTMLElement && active !== document.body && active.isConnected && !(event.currentTarget as Node | null)?.contains(active);
    if (placed) return;
    if (el instanceof HTMLElement && el !== document.body && el.isConnected) {
      event.preventDefault();
      el.focus();
    }
  };
}
