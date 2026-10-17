/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { Loader2 } from "lucide-react";

// Shown while a lazy route's chunk is in flight. Deliberately the same mark +
// spinner as the console's auth probe (App.tsx), so a slow chunk reads as the console still
// connecting rather than as a broken screen.
export function RouteFallback() {
  return (
    <div
      className="flex min-h-[60vh] flex-col items-center justify-center gap-3"
      role="status"
      aria-live="polite"
    >
      <Loader2 className="size-5 animate-spin text-muted-foreground" />
      <span className="sr-only">Loading…</span>
    </div>
  );
}
