import * as React from "react";

// Radix hands focus back on close only to a DialogTrigger. A dialog opened from state has none, so focus
// fell to the page body. The content remembers the element that had focus when it mounted and, on close,
// puts focus back there when focus would otherwise be lost: not if the consumer handled it, the caller already
// moved focus elsewhere, the element is gone, or it was only the body.
//
// When the opener is itself removed by the action the dialog confirmed (a deleted row, a decided card), the
// content also remembers where the opener sat. If focus then falls to the body, it goes to the same control in
// the row now in that slot (the next row, or the previous one for the last), else to the page heading.

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

const ROW = "tr, li, [role='row'], [data-focus-row]";
const CONTROL = "a[href], button:not([disabled]), input:not([disabled]), select:not([disabled]), textarea:not([disabled]), [tabindex]:not([tabindex='-1'])";
// How long a vanished opener is waited on, and how quiet the page must be before focus is moved: a list that
// refetches swaps its rows in a burst, and the slot is only readable once they have settled.
const WATCH_MS = 5000;
const SETTLE_MS = 100;

interface Slot {
  parent: Element;
  index: number;
  control: number;
}

const slots = new WeakMap<Element, Slot>();

const rowsOf = (parent: Element) => Array.from(parent.children).filter((c) => c.matches(ROW));
const controlsOf = (row: Element) => Array.from(row.querySelectorAll(CONTROL));

function slotOf(opener: Element): Slot | null {
  const row = opener.closest(ROW);
  const parent = row?.parentElement;
  if (!row || !parent) return null;
  return { parent, index: rowsOf(parent).indexOf(row), control: controlsOf(row).indexOf(opener) };
}

function focusEl(el: HTMLElement) {
  if (!el.hasAttribute("tabindex") && el.tabIndex < 0) el.tabIndex = -1;
  el.focus();
}

/** The control that took the opener's place, else the page heading, else the main region. */
function recoverFocus(slot: Slot | null) {
  if (slot?.parent.isConnected) {
    const rows = rowsOf(slot.parent);
    const row = rows[Math.min(slot.index, rows.length - 1)];
    const controls = row ? controlsOf(row) : [];
    const control = controls[Math.min(Math.max(slot.control, 0), controls.length - 1)];
    if (control instanceof HTMLElement) return control.focus();
  }
  const target = document.querySelector<HTMLElement>("#main-content h1") ?? document.getElementById("main-content");
  if (target) focusEl(target);
}

/** Waits for a vanished opener to leave focus on the body, then hands focus to recoverFocus. Gives up on any real focus move. */
function watchLoss(opener: Element, slot: Slot | null) {
  let timer: ReturnType<typeof setTimeout> | undefined;
  const stop = () => {
    clearTimeout(timer);
    clearTimeout(expiry);
    observer.disconnect();
    document.removeEventListener("focusin", stop, true);
  };
  const check = () => {
    const active = document.activeElement;
    const lost = !active || active === document.body || !active.isConnected;
    if (opener.isConnected) {
      if (active !== opener && !lost) stop();
      return;
    }
    if (lost) recoverFocus(slot);
    stop();
  };
  const arm = () => {
    clearTimeout(timer);
    timer = setTimeout(check, SETTLE_MS);
  };
  const observer = new MutationObserver(arm);
  const expiry = setTimeout(stop, WATCH_MS);
  observer.observe(document.body, { childList: true, subtree: true });
  document.addEventListener("focusin", stop, true);
  arm();
}

/** Rendered inside the dialog content, so its first render reads the focused element before autofocus moves it. */
export function RememberOpener({ into }: { into: React.MutableRefObject<Element | null> }) {
  if (into.current === null) {
    into.current = openerOf(document.activeElement);
    const slot = into.current && slotOf(into.current);
    if (into.current && slot) slots.set(into.current, slot);
  }
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
    if (!(el instanceof HTMLElement) || el === document.body) return;
    if (el.isConnected) {
      event.preventDefault();
      el.focus();
    }
    watchLoss(el, slots.get(el) ?? null);
  };
}
