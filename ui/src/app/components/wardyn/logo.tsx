/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { cn } from "../ui/utils";

export function WardynMark({ className }: { className?: string }) {
  return (
    <svg viewBox="0 0 32 32" className={cn("size-7", className)} fill="none" xmlns="http://www.w3.org/2000/svg" aria-hidden>
      <path
        d="M16 2.5 4 7v8.2c0 7.4 5 12.2 12 14.3 7-2.1 12-6.9 12-14.3V7L16 2.5Z"
        className="fill-foreground/15 stroke-foreground"
        strokeWidth="1.6"
      />
      <path d="M11 16.2l3.4 3.4L21.5 12" className="stroke-foreground" strokeWidth="2" strokeLinecap="round" strokeLinejoin="round" />
    </svg>
  );
}

export function WardynWordmark({ className, compact = false }: { className?: string; compact?: boolean }) {
  return (
    <span className={cn("inline-flex items-center gap-2 select-none", className)}>
      <WardynMark />
      {!compact && (
        <span className="text-base font-semibold tracking-tight text-foreground">
          Wardyn
        </span>
      )}
    </span>
  );
}
