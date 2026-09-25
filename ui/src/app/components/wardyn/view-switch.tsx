/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The Console view switch and the tab re-sync (admin-member-modes-design.md
// §2.2, §2.4; packet M-A). Kept out of app-shell.tsx, which is under the size
// gate.
import * as React from "react";
import { useLocation, useNavigate } from "react-router-dom";
import { cn } from "../ui/utils";
import { health } from "../../lib/api/health";
import { onForbidden } from "../../lib/api/core";
import { usePoll } from "../../lib/use-poll";
import { useRequestLeave } from "../../lib/use-unsaved-guard";
import {
  currentView,
  isSwitching,
  switchView,
  viewAccess,
  viewChannel,
  viewHome,
  viewOfPath,
  viewTarget,
  type ConsoleView,
  type ViewAccess,
} from "./console-view";
import { CONSOLE_VIEW } from "./copy/console-view";

interface ViewMeta {
  method: string;
  role: string;
  memberMode: boolean;
  sso: boolean;
  identityResolved: boolean;
}

/** Who this principal is to the views, and which one this page is in. */
export function useShellView(meta: ViewMeta): { access: ViewAccess; view: ConsoleView; hasSwitch: boolean } {
  const access = viewAccess(meta);
  const view = currentView(access, viewOfPath(useLocation().pathname));
  // Only a principal with both views sees the switch; nobody sees a disabled
  // segment. Nothing is drawn until /me answers, since the seed is a guess.
  const hasSwitch =
    meta.identityResolved && (access === "url" || access === "session-admin" || access === "session-user");
  return { access, view, hasSwitch };
}

const SEGMENTS: [ConsoleView, string][] = [
  ["admin", CONSOLE_VIEW.ADMIN],
  // SEAM (UT-13 / UT-7a): when /me reports more than one user type, this
  // segment becomes the type picker, the previous choice preselected. /me
  // carries no types yet, so it stays the plain two-way toggle.
  ["user", CONSOLE_VIEW.USER],
];

/** The two-segment control. No keyboard shortcut: a change of authority must
 *  never be one stray keystroke. */
export function ViewSwitch({
  access,
  view,
  className,
  onNavigate,
}: {
  access: ViewAccess;
  view: ConsoleView;
  className?: string;
  /** Called after a URL-only switch, so the mobile sheet can close itself. */
  onNavigate?: () => void;
}) {
  const requestLeave = useRequestLeave();
  const navigate = useNavigate();
  const [busy, setBusy] = React.useState(false);
  const [failed, setFailed] = React.useState(false);
  const choose = (to: ConsoleView) => {
    if (to === view || busy) return;
    requestLeave(() => {
      // A single-operator install has one authority in both views (D1).
      if (access === "url") {
        void navigate(viewHome(to));
        onNavigate?.();
        return;
      }
      setBusy(true);
      setFailed(false);
      switchView(to, viewHome(to)).catch(() => {
        setBusy(false);
        setFailed(true);
      });
    });
  };
  return (
    <div className={cn("flex min-w-0 items-center gap-2", className)}>
      <div
        role="group"
        aria-label={CONSOLE_VIEW.GROUP}
        className="inline-flex shrink-0 items-center gap-0.5 rounded-md border border-border p-0.5"
      >
        {SEGMENTS.map(([v, label]) => {
          const pressed = v === view;
          return (
            <button
              key={v}
              type="button"
              aria-pressed={pressed}
              aria-disabled={pressed || undefined}
              disabled={busy}
              onClick={() => choose(v)}
              className={cn(
                "rounded px-2.5 py-1 text-xs font-medium transition-colors outline-none focus-visible:ring-[3px] focus-visible:ring-ring disabled:opacity-50",
                pressed ? "bg-primary/10 text-foreground" : "text-muted-foreground hover:text-foreground",
              )}
            >
              {label}
            </button>
          );
        })}
      </div>
      {failed && (
        <span role="alert" className="text-xs text-danger">
          {CONSOLE_VIEW.SWITCH_FAILED}
        </span>
      )}
    </div>
  );
}

const RESYNC_MS = 60_000;

/** §2.4: an SSO tab re-reads /me on focus, every minute, at once after any 403
 *  and when another tab says it switched. When the session's view is no longer
 *  the one this page loaded in, the tab reloads into it, quietly. */
export function useViewResync(access: ViewAccess, loadedMemberMode: boolean): void {
  const active = access === "session-admin" || access === "session-user";
  const inFlight = React.useRef(false);
  const check = React.useCallback(async () => {
    if (!active || inFlight.current || isSwitching()) return;
    inFlight.current = true;
    try {
      const me = await health.whoami();
      if (!me || isSwitching() || (me.user_view ?? false) === loadedMemberMode) return;
      const { pathname, search, hash } = window.location;
      window.location.assign(viewTarget(me.user_view ? "user" : "admin", pathname, `${search}${hash}`));
    } finally {
      inFlight.current = false;
    }
  }, [active, loadedMemberMode]);
  usePoll(check, RESYNC_MS, !active);
  React.useEffect(() => {
    if (!active) return;
    const recheck = () => void check();
    const ch = viewChannel();
    window.addEventListener("focus", recheck);
    ch?.addEventListener("message", recheck);
    onForbidden(recheck);
    return () => {
      window.removeEventListener("focus", recheck);
      ch?.removeEventListener("message", recheck);
      onForbidden(null);
    };
  }, [active, check]);
}
