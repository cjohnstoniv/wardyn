/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import * as React from "react";

// useCopyToClipboard — the "copy, flip a copied flag, reset it after a beat"
// affordance shared by every copy button in the console.
//
// `resetMs` controls the auto-reset timer (default 1500ms); pass `null` to
// disable it when a caller resets `copied` itself on some other event (e.g.
// SetupGuideDialog resets when the dialog's `guide` prop changes instead).
//
// Two copy variants, since call sites split on whether they need to know the
// write actually succeeded before flipping the flag:
//  - `copy`: fire-and-forget — flips `copied` immediately (matches the
//    non-secure-context reality that most of these buttons don't bother
//    checking Clipboard API availability).
//  - `copyAsync`: awaits the write and only flips `copied` (returns true) on
//    success — for callers that show a distinct success/failure toast.
export function useCopyToClipboard(resetMs: number | null = 1500) {
  const [copied, setCopied] = React.useState(false);

  const scheduleReset = () => {
    if (resetMs != null) setTimeout(() => setCopied(false), resetMs);
  };

  const copy = (text: string) => {
    navigator.clipboard?.writeText(text);
    setCopied(true);
    scheduleReset();
  };

  const copyAsync = async (text: string): Promise<boolean> => {
    try {
      await (navigator.clipboard?.writeText(text) ?? Promise.reject(new Error("no clipboard")));
      setCopied(true);
      scheduleReset();
      return true;
    } catch {
      return false;
    }
  };

  return { copied, setCopied, copy, copyAsync };
}
