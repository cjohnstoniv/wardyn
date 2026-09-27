/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// #217 — the rail's lower slot, under a divider (app-shell.tsx#SidebarNav):
// Setup and Settings in the Admin view, Getting started and Your account in the
// User view. Split out so that growth stays here instead of pushing
// app-shell.tsx past its size-gate cap (scripts/check-file-size.sh).
import type { ElementType, MouseEvent } from "react";
import { NavLink } from "react-router-dom";
import { cn } from "../ui/utils";

export function SidebarLowerLinks({
  items,
  navLinkClass,
  onClick,
}: {
  items: { to: string; label: string; icon: ElementType }[];
  navLinkClass: (isActive: boolean) => string;
  onClick: (to: string) => (e: MouseEvent) => void;
}) {
  if (items.length === 0) return null;
  return (
    <>
      <div className="my-2 h-px bg-sidebar-border" />
      {items.map((item) => (
        <NavLink key={item.to} to={item.to} onClick={onClick(item.to)} className={({ isActive }) => navLinkClass(isActive)}>
          {({ isActive }) => (
            <>
              {isActive && (
                <span className="absolute -left-3 top-1/2 h-5 w-0.5 -translate-y-1/2 rounded-r bg-sidebar-primary" />
              )}
              <item.icon className={cn("size-4", isActive && "text-foreground")} />
              <span className="flex-1 text-left">{item.label}</span>
            </>
          )}
        </NavLink>
      ))}
    </>
  );
}
