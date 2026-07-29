/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import * as React from "react";
import { Check, Copy } from "lucide-react";
import { cn } from "../ui/utils";
import { useCopyToClipboard } from "../../lib/use-copy-to-clipboard";

// CopyButton — the console's one copy-to-clipboard control. The icon swap, the
// accessible name, and the polite live region that ANNOUNCES the copy all live
// here: hand-rolled copies of this button kept shipping without the live region,
// so a screen-reader user copying a setup command got no confirmation at all.
// Styling is entirely the caller's (className) — this owns behaviour, not chrome.
export function CopyButton({
  text,
  label = "Copy",
  className,
  iconClassName = "size-3.5",
  disabled,
  children,
}: {
  text: string;
  /** Accessible name — say WHAT is copied ("Copy setup command"), not just "Copy". */
  label?: string;
  className?: string;
  iconClassName?: string;
  disabled?: boolean;
  /** Visible content rendered before the icon (e.g. the command pill's text). */
  children?: React.ReactNode;
}) {
  const { copied, copy } = useCopyToClipboard();
  return (
    <button
      type="button"
      onClick={() => copy(text)}
      disabled={disabled}
      aria-label={label}
      className={cn("inline-flex items-center", className)}
    >
      {children}
      {copied ? (
        <Check className={cn(iconClassName, "text-success")} aria-hidden />
      ) : (
        <Copy className={iconClassName} aria-hidden />
      )}
      <span aria-live="polite" className="sr-only">
        {copied ? "Copied" : ""}
      </span>
    </button>
  );
}
