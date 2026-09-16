/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// R-3/R-4: shared by focus-mode.tsx's Escape handler and attach-terminal.tsx's
// fallback-fullscreen Escape handler — both are capture-phase `document`
// listeners that must yield to a Radix dialog's OWN Escape-dismiss rather than
// fire alongside it (F1-F3). `data-radix-dialog-content` (the original guard's
// selector) exists nowhere in this codebase or in @radix-ui itself; this
// repo's dialog.tsx emits `data-slot="dialog-content"`, and both Dialog and
// AlertDialog content carry their ARIA role regardless of this repo's own
// data-slot attributes.
export function modalLayerOpen(): boolean {
  return !!document.querySelector('[data-slot="dialog-content"],[role="alertdialog"],[role="dialog"]');
}
