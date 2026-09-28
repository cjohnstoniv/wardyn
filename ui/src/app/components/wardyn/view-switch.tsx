/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The Console view switch and the tab re-sync (admin-member-modes-design.md
// §2.2, §2.4; packet M-A). Kept out of app-shell.tsx, which is under the size
// gate. The User view's type picker (user-types-design.md §2.7, #912) lives
// here too: a plain toggle while the org has one type, otherwise a dropdown
// on the "User view" segment.
import * as React from "react";
import { Check, ChevronDown } from "lucide-react";
import { useLocation, useNavigate } from "react-router-dom";
import { cn } from "../ui/utils";
import { health } from "../../lib/api/health";
import { onForbidden } from "../../lib/api/core";
import { usePoll } from "../../lib/use-poll";
import { useRequestLeave } from "../../lib/use-unsaved-guard";
import { appURL, routerPath } from "../../lib/base-path";
import { useUserTypes } from "../../lib/use-user-types";
import { DropdownMenu, DropdownMenuContent, DropdownMenuItem, DropdownMenuTrigger } from "../ui/dropdown-menu";
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
import { CONSOLE_VIEW, VIEW_DROPPED } from "./copy/console-view";

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

// Only an SSO admin/security-admin tier ever resolves a type through the
// picker — url access (D1's single-operator install) has no SSO principal to
// look one up for, and a real user IS one type rather than choosing among
// them.
function typePickerEnabled(access: ViewAccess): boolean {
  return access === "session-admin" || access === "session-user";
}

const segmentClass = (pressed: boolean) =>
  cn(
    "rounded px-2.5 py-1 text-xs font-medium transition-colors outline-none focus-visible:ring-[3px] focus-visible:ring-ring disabled:opacity-50",
    pressed ? "bg-primary/10 text-foreground" : "text-muted-foreground hover:text-foreground",
  );

/** The two-segment control. No keyboard shortcut: a change of authority must
 *  never be one stray keystroke. */
export function ViewSwitch({
  access,
  view,
  className,
  onNavigate,
  currentUserType,
  preselectType,
}: {
  access: ViewAccess;
  view: ConsoleView;
  className?: string;
  /** Called after a URL-only switch, so the mobile sheet can close itself. */
  onNavigate?: () => void;
  /** The type this session is looking through, while view is "user" (§2.7). */
  currentUserType?: { id: string; name: string } | null;
  /** /me's user_view_preselect_type — the dropdown's first value before the
   *  view is entered (#912). */
  preselectType?: string;
}) {
  const requestLeave = useRequestLeave();
  const navigate = useNavigate();
  const [busy, setBusy] = React.useState(false);
  const [failed, setFailed] = React.useState(false);
  const { userTypes } = useUserTypes(typePickerEnabled(access));
  const multiType = userTypes.length > 1;
  const selected = view === "user" ? (currentUserType?.id ?? "") : (preselectType ?? "");

  const perform = (to: ConsoleView, userType?: string) => {
    requestLeave(() => {
      // A single-operator install has one authority in both views (D1).
      if (access === "url") {
        void navigate(viewHome(to));
        onNavigate?.();
        return;
      }
      setBusy(true);
      setFailed(false);
      switchView(to, viewHome(to), false, userType).catch(() => {
        setBusy(false);
        setFailed(true);
      });
    });
  };
  // The plain toggle (Admin, or User with one type): clicking the already-
  // pressed segment is a no-op, exactly as it always was.
  const switchTo = (to: ConsoleView) => {
    if (to === view || busy) return;
    perform(to);
  };
  // The dropdown's own items: a no-op ONLY while already viewing that exact
  // type — picking the PRESELECTED type from Admin view must still act, since
  // that click is how the admin confirms entering as it in the first place.
  const chooseType = (id: string) => {
    if ((view === "user" && id === selected) || busy) return;
    perform("user", id);
  };

  const userSegment =
    multiType && typePickerEnabled(access) ? (
      <DropdownMenu>
        <DropdownMenuTrigger asChild>
          <button
            type="button"
            aria-pressed={view === "user"}
            disabled={busy}
            className={cn(segmentClass(view === "user"), "inline-flex items-center gap-1")}
          >
            {CONSOLE_VIEW.USER}
            <ChevronDown className="size-3" />
          </button>
        </DropdownMenuTrigger>
        <DropdownMenuContent align="start">
          {userTypes.map((t) => (
            <DropdownMenuItem key={t.id} onSelect={() => chooseType(t.id)}>
              <Check className={cn("mr-2 size-3.5", t.id === selected ? "opacity-100" : "opacity-0")} />
              {t.name}
            </DropdownMenuItem>
          ))}
        </DropdownMenuContent>
      </DropdownMenu>
    ) : (
      <button
        type="button"
        aria-pressed={view === "user"}
        aria-disabled={view === "user" || undefined}
        disabled={busy}
        onClick={() => switchTo("user")}
        className={segmentClass(view === "user")}
      >
        {CONSOLE_VIEW.USER}
      </button>
    );

  return (
    <div className={cn("flex min-w-0 items-center gap-2", className)}>
      <div
        role="group"
        aria-label={CONSOLE_VIEW.GROUP}
        className="inline-flex shrink-0 items-center gap-0.5 rounded-md border border-border p-0.5"
      >
        <button
          type="button"
          aria-pressed={view === "admin"}
          aria-disabled={view === "admin" || undefined}
          disabled={busy}
          onClick={() => switchTo("admin")}
          className={segmentClass(view === "admin")}
        >
          {CONSOLE_VIEW.ADMIN}
        </button>
        {userSegment}
      </div>
      {failed && (
        <span role="alert" className="text-xs text-danger">
          {CONSOLE_VIEW.SWITCH_FAILED}
        </span>
      )}
    </div>
  );
}

/** The User view's own eyebrow (§2.7, #912): shown only once the org has more
 *  than one type, beside the Admin view's plain EYEBROW_ADMIN text — reopens
 *  the same picker ViewSwitch's User segment offers, so the type can change
 *  without leaving the view. */
export function UserViewEyebrow({
  access,
  currentUserType,
}: {
  access: ViewAccess;
  currentUserType?: { id: string; name: string } | null;
}) {
  const [busy, setBusy] = React.useState(false);
  const { userTypes } = useUserTypes(access === "session-user");
  if (access !== "session-user" || userTypes.length <= 1 || !currentUserType) return null;
  const choose = (id: string) => {
    if (id === currentUserType.id || busy) return;
    setBusy(true);
    switchView("user", viewHome("user"), false, id).catch(() => setBusy(false));
  };
  return (
    <DropdownMenu>
      <DropdownMenuTrigger asChild>
        <button
          type="button"
          disabled={busy}
          className="label-eyebrow mb-2 inline-flex items-center gap-1 px-2.5 disabled:opacity-50"
        >
          {CONSOLE_VIEW.EYEBROW_USER(currentUserType.name)}
          <ChevronDown className="size-3" />
        </button>
      </DropdownMenuTrigger>
      <DropdownMenuContent align="start">
        {userTypes.map((t) => (
          <DropdownMenuItem key={t.id} onSelect={() => choose(t.id)}>
            <Check className={cn("mr-2 size-3.5", t.id === currentUserType.id ? "opacity-100" : "opacity-0")} />
            {t.name}
          </DropdownMenuItem>
        ))}
      </DropdownMenuContent>
    </DropdownMenu>
  );
}

/** The deleted-type notice (UT-13, #912): the console's re-sync (below)
 *  already reloaded this admin back into the Admin view once the type they
 *  were looking through was removed — this says why, from /me's own
 *  user_view_dropped, and offers a real "Choose another type" action rather
 *  than the sentence's own prose CTA. Renders nothing once the admin picks a
 *  type (the switch's own POST clears the session state the next /me reads). */
export function UserViewDroppedNotice({
  access,
  dropped,
}: {
  access: ViewAccess;
  dropped: { user_type: string } | null;
}) {
  const [busy, setBusy] = React.useState(false);
  const [failed, setFailed] = React.useState(false);
  const { userTypes } = useUserTypes(access === "session-admin" && !!dropped);
  if (!dropped || access !== "session-admin") return null;
  const choose = (id: string) => {
    setBusy(true);
    setFailed(false);
    switchView("user", viewHome("user"), false, id).catch(() => {
      setBusy(false);
      setFailed(true);
    });
  };
  return (
    <div
      role="status"
      className="relative z-50 flex shrink-0 items-center gap-2 border-b border-border bg-warning-subtle px-4 py-2 text-sm text-warning"
    >
      <span>{VIEW_DROPPED.BODY(dropped.user_type)}</span>
      {userTypes.length > 0 && (
        <DropdownMenu>
          <DropdownMenuTrigger asChild>
            <button type="button" disabled={busy} className="font-medium underline underline-offset-2">
              {VIEW_DROPPED.CHOOSE_ANOTHER}
            </button>
          </DropdownMenuTrigger>
          <DropdownMenuContent align="start">
            {userTypes.map((t) => (
              <DropdownMenuItem key={t.id} onSelect={() => choose(t.id)}>
                {t.name}
              </DropdownMenuItem>
            ))}
          </DropdownMenuContent>
        </DropdownMenu>
      )}
      {failed && <span>{CONSOLE_VIEW.SWITCH_FAILED}</span>}
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
      const { search, hash } = window.location;
      window.location.assign(appURL(viewTarget(me.user_view ? "user" : "admin", routerPath(), `${search}${hash}`)));
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
