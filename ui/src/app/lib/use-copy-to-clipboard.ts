/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import * as React from "react";
import { toast } from "sonner";

// DRAFT (M2 canon pending)
const COPY_FAILED_TOAST = "Copy failed — your browser blocked clipboard access.";

// useCopyToClipboard — the "copy, flip a copied flag, reset it after a beat"
// affordance shared by every copy button in the console.
//
// `resetMs` controls the auto-reset timer (default 1500ms); pass `null` to
// disable it when a caller resets `copied` itself on some other event.
//
// Two copy variants, since call sites split on whether they need to know the
// write actually succeeded before flipping the flag:
//  - `copy`: fire-and-forget for a plain onClick — still only flips `copied`
//    once the write actually resolves, so a caller that doesn't check the
//    return value never lies with "Copied" when navigator.clipboard is
//    unavailable (e.g. LAN HTTP / an insecure context).
//  - `copyAsync`: awaits the write and only flips `copied` (returns true) on
//    success — for callers that show a distinct success/failure toast.
//
// F6-F13: `copy` used to discard copyAsync's outcome entirely, so a plain
// onClick on an insecure context (LAN HTTP — no navigator.clipboard) told the
// caller nothing at all: no icon swap, no toast, silence. copyAsync already
// knows whether it worked; copy() now surfaces a failure toast rather than
// reinventing the write with the deprecated execCommand fallback (needs a
// focused selection, and every call site here copies an arbitrary string,
// not a DOM selection).
export function useCopyToClipboard(resetMs: number | null = 1500) {
  const [copied, setCopied] = React.useState(false);

  const scheduleReset = () => {
    if (resetMs != null) setTimeout(() => setCopied(false), resetMs);
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

  const copy = async (text: string): Promise<void> => {
    const ok = await copyAsync(text);
    if (!ok) toast.error(COPY_FAILED_TOAST);
  };

  return { copied, setCopied, copy, copyAsync };
}
