import * as React from "react";

// Radix hands focus back on close only to a DialogTrigger. A dialog opened from state has none, so focus
// fell to the page body. The content remembers the element that had focus when it mounted and, on close,
// puts focus back there unless the consumer handled it, the element is gone, or it was only the body.

/** Rendered inside the dialog content, so its first render reads the focused element before autofocus moves it. */
export function RememberOpener({ into }: { into: React.MutableRefObject<Element | null> }) {
  if (into.current === null) into.current = document.activeElement;
  return null;
}

/** The content's onCloseAutoFocus: a consumer's own handler is passed through untouched. */
export function restoreOpenerFocus(
  opener: React.MutableRefObject<Element | null>,
  consumer?: (event: Event) => void,
): (event: Event) => void {
  return (event) => {
    const el = opener.current;
    opener.current = null;
    if (consumer) return consumer(event);
    if (el instanceof HTMLElement && el !== document.body && el.isConnected) {
      event.preventDefault();
      el.focus();
    }
  };
}
