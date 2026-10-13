/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import * as React from "react";

/**
 * Escape closes the dialog this is mounted in and nothing under it. Radix hands one Escape to the nested
 * dialog and to the Add dialog beneath it in the same dispatch, and in a real browser the Add dialog can
 * close with it. Taking the key at the window, before either layer sees it, makes the top dialog the only
 * one that hears it.
 */
export function useEscapeClosesThis(close: () => void): void {
  React.useEffect(() => {
    const onKey = (e: KeyboardEvent) => {
      if (e.key !== "Escape") return;
      e.preventDefault();
      e.stopPropagation();
      close();
    };
    window.addEventListener("keydown", onKey, true);
    return () => window.removeEventListener("keydown", onKey, true);
  }, [close]);
}
