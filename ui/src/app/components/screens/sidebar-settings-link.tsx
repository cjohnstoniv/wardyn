/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// #217/#460 — Settings, last under a divider in the sidebar
// (app-shell.tsx#SidebarNav), admin only (Q460-1). Split out so that growth
// stays here instead of pushing app-shell.tsx past its 1000-line size-gate
// cap (scripts/check-file-size.sh) — the file was already 971 lines before
// #217.
import type { MouseEvent } from "react";
import { NavLink } from "react-router-dom";
import { Settings } from "lucide-react";
import { cn } from "../ui/utils";
import { NAV } from "../../lib/unsaved-copy";

export function SidebarSettingsLink({
  to,
  navLinkClass,
  onClick,
}: {
  to: string;
  navLinkClass: (isActive: boolean) => string;
  onClick: (e: MouseEvent) => void;
}) {
  return (
    <>
      <div className="my-2 h-px bg-sidebar-border" />
      <NavLink to={to} onClick={onClick} className={({ isActive }) => navLinkClass(isActive)}>
        {({ isActive }) => (
          <>
            {isActive && (
              <span className="absolute -left-3 top-1/2 h-5 w-0.5 -translate-y-1/2 rounded-r bg-sidebar-primary" />
            )}
            <Settings className={cn("size-4", isActive && "text-foreground")} />
            <span className="flex-1 text-left">{NAV.SETTINGS}</span>
          </>
        )}
      </NavLink>
    </>
  );
}
