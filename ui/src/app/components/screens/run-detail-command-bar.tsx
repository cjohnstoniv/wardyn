/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// RunDetailCommandBar — the 36px tabs row directly under SummaryHeader's
// 52px command bar (design board seg2a's `h-9` row). This component owns
// only the row's layout: the four TabsTriggers on the left are Radix Tabs
// content, and Tabs.List/Tabs.Trigger must live inside the parent's own
// <Tabs> root (Radix context) — so the parent builds that markup (including
// the pending-approvals count badge on the Approvals trigger) and hands it
// in as `tabs`, rather than this component importing the Tabs primitives
// itself.
import * as React from "react";
import { cn } from "../ui/utils";

export function RunDetailCommandBar({
  tabs,
  className,
}: {
  /** The parent's <TabsList>…</TabsList>, rendered inside its own <Tabs> root. */
  tabs: React.ReactNode;
  className?: string;
}) {
  return (
    <div
      className={cn(
        "flex h-9 shrink-0 items-center gap-2 border-b border-border bg-background px-4",
        className,
      )}
    >
      <div className="flex min-w-0 items-center gap-0.5">{tabs}</div>
    </div>
  );
}
