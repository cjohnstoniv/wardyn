/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { useRef } from "react";
import { UNSAVED } from "../../lib/unsaved-copy";
import { Tabs, TabsList, TabsTrigger } from "../ui/tabs";
import { Chip } from "./primitives";

// The one look for a page's sections (#3062): underline tabs on the Radix Tabs, so arrow keys rove
// focus and selection. List filters are Segmented instead. The panels stay with the caller (a screen
// that keeps its own state per tab, or syncs the tab to the URL); this draws the strip only.
export function PageTabs<T extends string>({
  value,
  onValueChange,
  tabs,
  label,
  className,
}: {
  value: T;
  onValueChange: (v: T) => void;
  tabs: { value: T; label: string; dirty?: boolean }[];
  /** The strip's accessible name; omit where the page title already names it. */
  label?: string;
  className?: string;
}) {
  const byKey = useRef(false);
  return (
    // Manual activation: a tab that regains focus (a confirm dialog hands it back to the tab it was
    // opened from) must not select itself again. The arrow keys still select, below.
    <Tabs value={value} onValueChange={(v) => onValueChange(v as T)} activationMode="manual" className={className}>
      <TabsList
        variant="underline"
        aria-label={label}
        onKeyDown={(e) => {
          // Radix moves focus on the next tick; the tab that takes it by an arrow key is then selected.
          if (!["ArrowLeft", "ArrowRight", "Home", "End"].includes(e.key)) return;
          byKey.current = true;
          setTimeout(() => (byKey.current = false), 0);
        }}
        onFocus={(e) => {
          const next = e.target.getAttribute("data-page-tab");
          if (byKey.current && next !== null && next !== value) onValueChange(next as T);
        }}
      >
        {tabs.map((t) => (
          <TabsTrigger key={t.value} variant="underline" value={t.value} data-page-tab={t.value}>
            {t.label}
            {t.dirty && (
              // aria-hidden, as Segmented's: the chip echoes a fact announced elsewhere and must not
              // change the tab's accessible name.
              <span aria-hidden="true" data-testid={`tab-dirty-chip-${t.value}`}>
                <Chip tone="warning">{UNSAVED.DIRTY_CHIP}</Chip>
              </span>
            )}
          </TabsTrigger>
        ))}
      </TabsList>
    </Tabs>
  );
}
