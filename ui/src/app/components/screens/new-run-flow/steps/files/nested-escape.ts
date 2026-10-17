/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import * as React from "react";

/**
 * Escape closes what this is mounted in and nothing under it. The Add dialog's own Escape listener sits on the
 * document in the capture phase, ahead of any handler on an element, and a nested dialog's layer and the Add
 * dialog's can both take the same key in a real browser. Taking the key at the window, before either, makes the
 * inner surface the only one that hears it. With `scope`, only a key pressed inside it is taken.
 */
export function useEscapeClosesThis(close: () => void, scope?: React.RefObject<HTMLElement | null>): void {
  React.useEffect(() => {
    const onKey = (e: KeyboardEvent) => {
      if (e.key !== "Escape" || (scope && !scope.current?.contains(e.target as Node))) return;
      e.preventDefault();
      e.stopPropagation();
      close();
    };
    window.addEventListener("keydown", onKey, true);
    return () => window.removeEventListener("keydown", onKey, true);
  }, [close, scope]);
}
