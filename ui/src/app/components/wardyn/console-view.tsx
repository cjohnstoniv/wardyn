/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import * as React from "react";
import { Navigate, Outlet, useLocation, useNavigate } from "react-router-dom";
import { Button } from "../ui/button";
import { health } from "../../lib/api/health";
import { useRoleResolved } from "./operator-context";
import { releaseUnloadGuard } from "../../lib/use-unsaved-guard";
import {
  CONSOLE_VIEW,
  OPEN_IN_USER_VIEW,
  VIEW_ADMIN_TOKEN,
  VIEW_REFUSAL,
  VIEW_TO_ADMIN,
  VIEW_TO_USER,
} from "./copy/console-view";

// Two consoles in one shell (admin-member-modes-design.md §2): the Admin view
// is /admin/*, the User view is every other path. The URL says which view a
// page is in; who may be in which, and which one an SSO session is in, comes
// from /me.
export type ConsoleView = "admin" | "user";

export function viewOfPath(path: string): ConsoleView {
  return path === "/admin" || path.startsWith("/admin/") ? "admin" : "user";
}

export function useConsoleMode(): ConsoleView {
  return viewOfPath(useLocation().pathname);
}

// Who may be in which view (§2.1):
// - "url": both, and the URL decides. Local mode and the admin token with no
//   SSO: one person, no boundary, so the User view is presentation only (D1).
// - "user-only": a user.
// - "admin-only": the admin token on an SSO install. It is not a person.
// - "session-admin" / "session-user": an SSO admin tier; the session cookie
//   holds the view, and in the User view the server answers them as a user.
export type ViewAccess = "url" | "user-only" | "admin-only" | "session-admin" | "session-user";

export function viewAccess(me: { method: string; role: string; memberMode: boolean; sso: boolean }): ViewAccess {
  if (me.method === "local") return "url";
  if (me.method === "sso") {
    if (me.memberMode) return "session-user";
    return me.role === "user" ? "user-only" : "session-admin";
  }
  if (me.role === "user") return "user-only";
  return me.sso ? "admin-only" : "url";
}

// Default "url": the permissive answer, for the same reason every tier in
// operator-context.tsx fails open — a screen mounted with no provider above it
// (every existing test) must behave exactly as it did before the split.
const ViewAccessContext = React.createContext<ViewAccess>("url");
export const ViewAccessProvider = ViewAccessContext.Provider;

export function useViewAccess(): ViewAccess {
  return React.useContext(ViewAccessContext);
}

// The User-view pages the rules act on (§2.3). Every other user-side path was
// a pre-split route; M-1b deleted them, so it now falls to the ordinary
// catch-all like any other unmatched path.
const TWIN = /^\/(runs(\/(?!new$)[^/]+)?|approvals|workspaces(\/[^/]+)?|secrets)$/;
const USER_ONLY = /^\/(runs\/new|account|setup)$/;

export type ViewVerdict =
  | { kind: "pass" }
  | { kind: "twin"; to: string }
  | { kind: "refuse" }
  | { kind: "to-admin" }
  | { kind: "to-user" }
  | { kind: "admin-token" };

export function viewVerdict(path: string, access: ViewAccess): ViewVerdict {
  const p = path.length > 1 ? path.replace(/\/+$/, "") : path;
  if (viewOfPath(p) === "admin") {
    if (access === "user-only") return { kind: "refuse" };
    // Entering admin authority is always a click: there is no user→admin twin.
    if (access === "session-user") return { kind: "to-admin" };
    return { kind: "pass" };
  }
  const twin = TWIN.test(p);
  if (!twin && !USER_ONLY.test(p)) return { kind: "pass" };
  if (access === "admin-only") return { kind: "admin-token" };
  if (access !== "session-admin") return { kind: "pass" };
  // The same object at the same authority, so no click is asked for.
  return twin ? { kind: "twin", to: `/admin${p}` } : { kind: "to-user" };
}

// Where "/" lands, given the setup-gate's answer for the caller's role (§1).
export function viewLanding(base: "/setup" | "/runs", access: ViewAccess): string {
  if (access === "session-admin" || access === "admin-only") return `/admin${base}`;
  // D1: a single-operator install is in the Admin view until setup is done.
  if (access === "url" && base === "/setup") return "/admin/setup";
  return base;
}

// The view a page is in for this principal: an SSO session's clamp decides it,
// a single-operator install's URL does (D1), and a one-view principal has one.
export function currentView(access: ViewAccess, pathView: ConsoleView): ConsoleView {
  if (access === "url") return pathView;
  return access === "session-admin" || access === "admin-only" ? "admin" : "user";
}

export function viewHome(view: ConsoleView): string {
  return view === "admin" ? "/admin" : "/runs";
}

// A run's own detail path in the given view — the Admin monitor's link
// target everywhere a run is opened FROM the Admin view (board card, table
// row, the approvals queue's run link), so that link never doubles as an
// unannounced view switch (ViewGate's TWIN rule sends the plain /runs/:id
// path to the User view for a "url"-access install, and refuses it for an
// admin-only token — see viewVerdict). OpenInUserView is the one deliberate
// exception: it always targets the User-view path, since crossing views is
// its whole job.
export function runPath(view: ConsoleView, id: string): string {
  return view === "admin" ? `/admin/runs/${encodeURIComponent(id)}` : `/runs/${encodeURIComponent(id)}`;
}

// Where a tab lands once its session is found in the other view (§2.4): the
// same object in that view when it has a twin, else that view's home. `rest`
// is the search and hash, kept on a twin as ViewGate's own redirect keeps them.
export function viewTarget(to: ConsoleView, path: string, rest = ""): string {
  const p = screenPath(path);
  if (!TWIN.test(p)) return viewHome(to);
  return `${to === "admin" ? `/admin${p}` : p}${rest}`;
}

// The fast path for other tabs (§2.4). One instance per page, used for both
// sending and listening: a channel never delivers to the instance that posted,
// so the switching tab does not also answer its own message.
export const VIEW_CHANNEL = "wardyn-console-view";
let channel: BroadcastChannel | null = null;
export function viewChannel(): BroadcastChannel | null {
  if (!channel && typeof BroadcastChannel !== "undefined") channel = new BroadcastChannel(VIEW_CHANNEL);
  return channel;
}

// The one way a view changes on SSO: flip the session's clamp, tell the other
// tabs, then reload the whole console, because every screen on it was fetched
// under the other role. The switch, the interstitials and the preview call it;
// the unsaved guard has already been asked by then, so the reload must not ask
// again (a "Stay" there would leave the session and the page in two views).
let switching = false;
/** True while this tab is switching itself, so its own re-sync stays out of the
 *  way of the reload it is about to make. */
export function isSwitching(): boolean {
  return switching;
}

export async function switchView(to: ConsoleView, target: string, noCredential = false): Promise<void> {
  releaseUnloadGuard(true);
  switching = true;
  try {
    await health.setMemberMode(to === "user", noCredential);
  } catch (e) {
    releaseUnloadGuard(false);
    switching = false;
    throw e;
  }
  viewChannel()?.postMessage(to);
  window.location.assign(target);
}

/** M-7 (§4.6, QM-7): what the Admin view gives in place of a personal door on
 *  the admin's own run — that run, in the User view. ViewSwitch's rule: a
 *  single-operator install only navigates; an SSO session flips its clamp
 *  first, and says so when that fails. The admin token is not a person and has
 *  no User view, so it gets nothing. Stops the click, since the rows it sits in
 *  open the run in this view. */
export function OpenInUserView({ runId, className }: { runId: string; className?: string }) {
  const access = useViewAccess();
  const navigate = useNavigate();
  const [busy, setBusy] = React.useState(false);
  const [failed, setFailed] = React.useState(false);
  if (access === "admin-only") return null;
  const target = `/runs/${encodeURIComponent(runId)}`;
  const go = (e: React.MouseEvent) => {
    e.stopPropagation();
    if (access === "url") {
      void navigate(target);
      return;
    }
    setBusy(true);
    setFailed(false);
    switchView("user", target).catch(() => {
      setBusy(false);
      setFailed(true);
    });
  };
  return (
    <>
      <Button size="sm" variant="outline" className={className} disabled={busy} onClick={go}>
        {OPEN_IN_USER_VIEW}
      </Button>
      {failed && (
        <span role="alert" className="text-xs text-danger">
          {CONSOLE_VIEW.SWITCH_FAILED}
        </span>
      )}
    </>
  );
}

function ViewNotice({ title, body, children }: { title?: string; body: string; children: React.ReactNode }) {
  return (
    <div className="flex min-h-[60vh] items-center justify-center px-6 py-16">
      <section className="w-full max-w-md space-y-2 rounded-xl border border-border bg-card p-6">
        {title && <h1 className="text-foreground">{title}</h1>}
        <p className="text-sm text-muted-foreground">{body}</p>
        <div className="flex flex-wrap items-center justify-end gap-2 pt-3">{children}</div>
      </section>
    </div>
  );
}

function ViewInterstitial({ to, target }: { to: ConsoleView; target: string }) {
  const navigate = useNavigate();
  const [busy, setBusy] = React.useState(false);
  const [failed, setFailed] = React.useState(false);
  const copy = to === "admin" ? VIEW_TO_ADMIN : VIEW_TO_USER;
  const go = () => {
    setBusy(true);
    setFailed(false);
    switchView(to, target).catch(() => {
      setBusy(false);
      setFailed(true);
    });
  };
  return (
    <ViewNotice title={copy.TITLE} body={copy.BODY}>
      {failed && (
        <p role="alert" className="mr-auto text-xs text-danger">
          {CONSOLE_VIEW.SWITCH_FAILED}
        </p>
      )}
      <Button variant="ghost" onClick={() => navigate(to === "admin" ? "/runs" : "/admin/runs")}>
        {copy.STAY}
      </Button>
      <Button onClick={go} disabled={busy}>
        {copy.GO}
      </Button>
    </ViewNotice>
  );
}

// The layout route every console page renders under. It waits for the real
// role, as RequireSetup does, so no rule runs on the fail-open seed; a /me that
// never answered is already handled by the shell, which paints no route.
export function ViewGate({ fallback }: { fallback: React.ReactNode }) {
  const { pathname, search, hash } = useLocation();
  const access = useViewAccess();
  const navigate = useNavigate();
  const roleResolved = useRoleResolved();
  if (!roleResolved) return <>{fallback}</>;
  const verdict = viewVerdict(pathname, access);
  switch (verdict.kind) {
    case "pass":
      return <Outlet />;
    case "twin":
      return <Navigate to={`${verdict.to}${search}${hash}`} replace />;
    case "refuse":
      return (
        <ViewNotice title={VIEW_REFUSAL.TITLE} body={VIEW_REFUSAL.BODY}>
          <Button size="sm" onClick={() => navigate("/runs")}>
            {VIEW_REFUSAL.CTA}
          </Button>
        </ViewNotice>
      );
    case "admin-token":
      return (
        <ViewNotice body={VIEW_ADMIN_TOKEN.BODY}>
          <Button size="sm" variant="outline" onClick={() => navigate("/admin")}>
            {VIEW_ADMIN_TOKEN.CTA}
          </Button>
        </ViewNotice>
      );
    default:
      return (
        <ViewInterstitial
          key={pathname}
          to={verdict.kind === "to-admin" ? "admin" : "user"}
          target={`${pathname}${search}${hash}`}
        />
      );
  }
}

// A path with its view prefix removed, for code that asks "which screen is
// this" and must answer the same in both trees.
export function screenPath(path: string): string {
  return viewOfPath(path) === "admin" ? path.slice("/admin".length) || "/" : path;
}
