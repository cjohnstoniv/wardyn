/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// F4-F13 (Appendix A V8): three hand-rolled `role="radiogroup"`s of
// `role="radio"` BUTTONS (never native `<input type="radio">`, which gets
// this for free from the browser via shared `name` grouping) had no roving
// tabindex or arrow keys — Tab visited every option one at a time instead of
// once per group, and Left/Right/Up/Down did nothing. This is the WAI-ARIA
// APG radio-group keyboard pattern, factored once so the three groups
// (connection-cards.tsx's ModelProviderCard, git-tab.tsx's credential lanes,
// agents-tab.tsx's credential-source toggle) can't drift into three
// half-implementations. Native radiogroups (agents-tab.tsx's mechanism
// picker) already have this and are NOT wired through here — that would be
// redundant, and a second, JS-driven arrow-key handler racing the browser's
// own is a bug generator, not a fix.
import * as React from "react";

// `radioRef`, never `ref`: these props are meant to be spread onto a plain
// function component (Lane), and React reserves the literal key `ref` for
// forwardRef — spreading one through as a prop silently drops it with a
// console warning ("Function components cannot be given refs"), and Home/End
// then have no element to focus.
export interface RovingRadioItemProps {
  tabIndex: number;
  radioRef: (el: HTMLButtonElement | null) => void;
}

/**
 * `count` radio items, `selectedIndex` the one currently checked (roving
 * tabindex follows the CHECKED item, per the APG "selection follows focus"
 * radiogroup pattern — there is exactly one Tab stop), `onSelect(i)` both
 * selects and focuses item i.
 */
export function useRovingRadio(count: number, selectedIndex: number, onSelect: (i: number) => void) {
  const refs = React.useRef<(HTMLButtonElement | null)[]>([]);

  const moveTo = React.useCallback(
    (i: number) => {
      onSelect(i);
      refs.current[i]?.focus();
    },
    [onSelect],
  );

  const onKeyDown = React.useCallback(
    (e: React.KeyboardEvent) => {
      if (count === 0) return;
      switch (e.key) {
        case "ArrowRight":
        case "ArrowDown":
          e.preventDefault();
          moveTo((selectedIndex + 1 + count) % count);
          break;
        case "ArrowLeft":
        case "ArrowUp":
          e.preventDefault();
          moveTo((selectedIndex - 1 + count) % count);
          break;
        case "Home":
          e.preventDefault();
          moveTo(0);
          break;
        case "End":
          e.preventDefault();
          moveTo(count - 1);
          break;
      }
    },
    [count, selectedIndex, moveTo],
  );

  const itemProps = React.useCallback(
    (i: number): RovingRadioItemProps => ({
      tabIndex: i === selectedIndex ? 0 : -1,
      radioRef: (el) => {
        refs.current[i] = el;
      },
    }),
    [selectedIndex],
  );

  return { containerProps: { onKeyDown }, itemProps };
}
