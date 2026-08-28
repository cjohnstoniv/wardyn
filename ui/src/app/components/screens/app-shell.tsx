/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import * as React from "react";
import { Link, NavLink, Outlet, useLocation, useNavigate } from "react-router-dom";
import {
  Activity,
  AlertTriangle,
  ChevronsUpDown,
  Compass,
  FlaskConical,
  Fingerprint,
  FolderOpen,
  KeyRound,
  Lock,
  LogOut,
  Menu,
  Moon,
  Plus,
  ScrollText,
  Settings,
  ShieldCheck,
  Sun,
  UserCog,
  Users,
} from "lucide-react";
import { WardynWordmark } from "../wardyn/logo";
import { Chip, ConfinementChip } from "../wardyn/primitives";
import { useTheme } from "../wardyn/theme-provider";
import { strongestAvailable } from "../wardyn/default-confinement";
import { lastCheckedLabel } from "../../lib/readiness";
import { cn } from "../ui/utils";
import { Button } from "../ui/button";
import { Sheet, SheetContent, SheetTitle, SheetTrigger } from "../ui/sheet";
import {
  DropdownMenu,
  DropdownMenuContent,
  DropdownMenuItem,
  DropdownMenuLabel,
  DropdownMenuSeparator,
  DropdownMenuTrigger,
} from "../ui/dropdown-menu";
import { ErrorBoundary } from "../wardyn/error-boundary";
import { OperatorProvider, RoleProvider, type Role } from "../wardyn/operator-context";
import { health as api } from "../../lib/api/health";
import type { ConfinementClass } from "../../lib/types";
// The run wizard reaches the workspaces + secrets screens and their dialogs, so
// importing it eagerly pulled all of that into the entry chunk even though the
// dialog only ever mounts on a "New run" click. Fetched on that click instead.

// useMeta fetches the real trust boundary (/healthz) + signed-in principal
// (/api/v1/me) so the shell never shows placeholder identity/tenant values.
export interface ShellMeta {
  trustDomain: string;
  identityProvider: string;
  principal: string;
  method: string;
  // Fail-open (see operator-context.tsx): starts true and stays true unless
  // /me resolves and explicitly says otherwise — an unresolved or failed
  // fetch must never read as "viewer".
  operator: boolean;
  // The same B1-derived tier as `operator`, named directly (B3) — fail-open
  // "admin" for the identical three cases (unresolved /me, a failed fetch, an
  // unwrapped test). Kept alongside `operator` rather than replacing it: every
  // existing operator-only gate stays exactly as it was.
  role: Role;
  // W31-S1-7: when the SSO session dies outright (no refresh) — null for
  // local/token auth, which has no session to expire.
  sessionExpiresAt: Date | null;
  // M3 — see operator-context.tsx's MemberLocalDirRootContext. null until /me
  // resolves and stays null (fail-closed: unavailable) if it never does.
  memberLocalDirRoot: string | null;
}

function useMeta(): ShellMeta {
  const [meta, setMeta] = React.useState<ShellMeta>({
    trustDomain: "…",
    identityProvider: "…",
    principal: "…",
    method: "",
    operator: true,
    role: "admin",
    sessionExpiresAt: null,
    memberLocalDirRoot: null,
  });
  React.useEffect(() => {
    let alive = true;
    Promise.all([api.health(), api.whoami()]).then(([h, me]) => {
      if (!alive) return;
      setMeta({
        trustDomain: h.trust_domain || "unknown",
        identityProvider: h.identity_provider || "unknown",
        principal: me?.principal || "unknown",
        method: me?.method || "",
        operator: me?.operator ?? true,
        role: me?.role ?? "admin",
        sessionExpiresAt: me?.session_expires_at ? new Date(me.session_expires_at) : null,
        memberLocalDirRoot: me?.member_local_dir_root ?? null,
      });
    });
    return () => {
      alive = false;
    };
  }, []);
  return meta;
}

// SESSION_WARN_MS — how far ahead of the session's real expiry to start
// warning (W31-S1-7). The session itself has no refresh; this is advance
// notice, not a renewal, so the human can save/finish before a silent 401
// wipes the console back to the sign-in gate mid-work.
const SESSION_WARN_MS = 5 * 60 * 1000;
const SESSION_CHECK_MS = 15 * 1000;

function useSessionExpiringSoon(expiresAt: Date | null): boolean {
  const [soon, setSoon] = React.useState(false);
  React.useEffect(() => {
    if (!expiresAt) {
      setSoon(false);
      return;
    }
    const check = () => setSoon(expiresAt.getTime() - Date.now() <= SESSION_WARN_MS);
    check();
    const id = setInterval(check, SESSION_CHECK_MS);
    return () => clearInterval(id);
  }, [expiresAt]);
  return soon;
}

function initials(principal: string): string {
  const base = principal.split("@")[0] || principal;
  const parts = base.split(/[.\-_]/).filter(Boolean);
  const s = (parts[0]?.[0] ?? "") + (parts[1]?.[0] ?? parts[0]?.[1] ?? "");
  return (s || base.slice(0, 2)).toUpperCase();
}

// Flat sidebar nav — seven items, no group headings (stage-1 redesign). Demos,
// Recordings, and Settings all left the sidebar: Demos and Settings are
// reachable from the account menu below, Recordings stays addressable by route
// (deep link, workspace/run actions) without their own nav entry.
interface NavItem {
  to: string;
  label: string;
  icon: React.ElementType;
  badge?: "approvals" | "attention";
}
const NAV_ITEMS: NavItem[] = [
  { to: "/runs", label: "Runs", icon: Activity, badge: "attention" },
  { to: "/approvals", label: "Approvals", icon: ShieldCheck, badge: "approvals" },
  { to: "/workspaces", label: "Workspaces", icon: FolderOpen },
  { to: "/policies", label: "Policies", icon: UserCog },
  // Permissioning (0.6 pillar 2) sits beside Policies: both answer "what is
  // allowed here", one for runs and one for the humans launching them. It is
  // admin-only — deliberately NOT in MEMBER_NAV_PATHS below, and every route
  // behind it is operatorOnly server-side.
  { to: "/permissions", label: "Permissions", icon: Users },
  { to: "/secrets", label: "Secrets", icon: Lock },
  { to: "/audit", label: "Audit", icon: ScrollText },
];

// Member console (B3): a member launches/governs only THEIR OWN runs — nav is
// Runs · Approvals, nothing else (no Policies/Secrets/Workspaces/Audit).
// Filtered by route path, never by re-deriving from a second copy of NAV_ITEMS.
//
// Hiding here is COSMETIC ONLY — every route a member can't reach still
// enforces that itself server-side (internal/api/routes.go's operatorOnly
// group and the owner-or-admin routes); this just keeps a member from
// discovering an admin-only screen as a raw 403 or an empty list instead of
// simply not offering it.
const MEMBER_NAV_PATHS = new Set(["/runs", "/approvals"]);
function navItemsForRole(role: Role): NavItem[] {
  if (role !== "member") return NAV_ITEMS;
  return NAV_ITEMS.filter((i) => MEMBER_NAV_PATHS.has(i.to));
}

// FOCUS MODE (design board 2c) — one screen, the run cockpit, can ask the shell
// to get out of the way so the terminal owns the pixels. Opt-in and scoped: the
// shell renders exactly as it always did until something below it sets this,
// and the canvas clears it on unmount, so no other screen can inherit a
// chrome-less shell.
//
// A context rather than global state or a DOM query, for the same reason
// OperatorProvider/RoleProvider are: the value flows down the tree the shell
// already owns, a component with no provider above it gets the honest default
// (focus off), and a test can drive it without touching a module singleton.
interface FocusMode {
  focus: boolean;
  setFocus: (on: boolean) => void;
}
const FocusContext = React.createContext<FocusMode>({ focus: false, setFocus: () => {} });

/** The shell's focus state. Default: off, and setting it is a no-op — a screen
 *  rendered outside <AppShell> (every existing test) can call this safely. */
export function useFocusMode(): FocusMode {
  return React.useContext(FocusContext);
}

const navLinkClass = (isActive: boolean) =>
  cn(
    "relative flex w-full items-center gap-2.5 rounded-md px-2.5 py-2 text-sm transition-colors",
    isActive
      ? "bg-sidebar-accent text-foreground"
      : "text-muted-foreground hover:bg-sidebar-accent/60 hover:text-foreground",
  );

// Shared nav body — rendered both in the desktop aside and in the mobile Sheet
// (see MobileNav) so the two never drift. Relies on a flex-col parent for the
// mt-auto bottom block, which both the aside and SheetContent provide.
// `onNavigate` lets the mobile drawer close itself when a link is picked.
function SidebarNav({
  pendingApprovals,
  attentionCount,
  meta,
  onNavigate,
}: {
  pendingApprovals: number;
  attentionCount: number;
  meta: ShellMeta;
  onNavigate?: () => void;
}) {
  const items = navItemsForRole(meta.role);
  return (
    <>
      <nav className="space-y-0.5">
        {items.map((item) => {
          const count =
            item.badge === "approvals" ? pendingApprovals : item.badge === "attention" ? attentionCount : 0;
          return (
            <NavLink
              key={item.to}
              to={item.to}
              end
              onClick={onNavigate}
              className={({ isActive }) => navLinkClass(isActive)}
            >
              {({ isActive }) => (
                <>
                  {isActive && (
                    <span className="absolute -left-3 top-1/2 h-5 w-0.5 -translate-y-1/2 rounded-r bg-sidebar-primary" />
                  )}
                  <item.icon className={cn("size-4", isActive && "text-foreground")} />
                  <span className="flex-1 text-left">{item.label}</span>
                  {count > 0 && (
                    <span className="inline-flex min-w-5 items-center justify-center rounded-full bg-warning-subtle px-1.5 text-meta font-semibold text-warning">
                      {count}
                    </span>
                  )}
                </>
              )}
            </NavLink>
          );
        })}
      </nav>

      <div className="mt-auto space-y-3">
        <div className="rounded-lg border border-sidebar-border bg-card/50 p-3">
          <div className="flex items-center gap-2 text-xs text-muted-foreground">
            <Fingerprint className="size-3.5 text-muted-foreground" />
            <span className="font-mono">{meta.trustDomain}</span>
          </div>
          <p className="mt-1.5 text-meta leading-relaxed text-muted-foreground">
            All agent identities anchored to this trust domain.
          </p>
        </div>
      </div>
    </>
  );
}

// Below the md breakpoint the desktop aside is hidden; this hamburger + Sheet is
// the only nav fallback. Sheet is Radix Dialog underneath, so Escape-to-
// close and focus-return-to-trigger come for free, and the trigger exposes
// aria-expanded/aria-controls automatically. md:hidden pairs it with the aside's
// md:flex so exactly one is present at any width.
export function MobileNav(props: {
  pendingApprovals: number;
  attentionCount: number;
  meta: ShellMeta;
}) {
  const [open, setOpen] = React.useState(false);
  return (
    <Sheet open={open} onOpenChange={setOpen}>
      <SheetTrigger asChild>
        <Button variant="ghost" size="icon" className="md:hidden" aria-label="Open navigation menu">
          <Menu className="size-5" />
        </Button>
      </SheetTrigger>
      <SheetContent side="left" aria-describedby={undefined} className="w-[248px] bg-sidebar px-3 py-4">
        <SheetTitle className="sr-only">Navigation</SheetTitle>
        <SidebarNav {...props} onNavigate={() => setOpen(false)} />
      </SheetContent>
    </Sheet>
  );
}

export function AppShell({
  pendingApprovals,
  attentionCount,
  onSignOut,
  unreachable,
  lastOkAt,
  confinementClasses: confinementClassesProp,
}: {
  pendingApprovals: number;
  attentionCount: number;
  onSignOut: () => void;
  // The daemon didn't answer App.tsx's setup-status poll, and when it last did.
  // Every screen's background refresh keeps its last-good data on failure, so
  // this banner is the ONLY thing that tells a quiet board from a dead one.
  unreachable?: boolean;
  lastOkAt?: Date | null;
  // The strongest confinement tier this host can actually run, from the SAME
  // setup-status poll App.tsx already runs for the unreachable banner — not a
  // second, disagreeing poll of the same expensive endpoint. undefined (no
  // fetch yet, or the daemon was unreachable) keeps the last-known barrier
  // instead of flashing "No barrier" for a merely quiet control plane.
  confinementClasses?: ConfinementClass[];
}) {
  const meta = useMeta();
  const sessionExpiringSoon = useSessionExpiringSoon(meta.sessionExpiresAt);
  const location = useLocation();
  const navigate = useNavigate();

  // The top bar's permanent barrier chip. Empty (no confinement classes at
  // all) reads "No barrier" — the one honest state a fresh/broken host can be
  // in.
  const [confinementClasses, setConfinementClasses] = React.useState<ConfinementClass[]>([]);
  React.useEffect(() => {
    if (confinementClassesProp !== undefined) setConfinementClasses(confinementClassesProp);
  }, [confinementClassesProp]);

  // See FocusContext above. Nothing here decides WHEN focus is on — the run
  // cockpit's canvas does, and it clears this on unmount.
  const [focus, setFocus] = React.useState(false);
  const focusValue = React.useMemo<FocusMode>(() => ({ focus, setFocus }), [focus]);

  // Wraps EVERYTHING the shell renders (nav, main/Outlet, and the New Run
  // dialog mounted below) — not just the Outlet — so every screen AND every
  // dialog reachable from here (including the wizard's inline Add secret /
  // Add workspace) sees the real role instead of silently falling back to the
  // context default.
  return (
    <OperatorProvider operator={meta.operator} principal={meta.principal} memberLocalDirRoot={meta.memberLocalDirRoot}>
    <RoleProvider role={meta.role}>
    <FocusContext.Provider value={focusValue}>
    <div className="flex h-screen flex-col bg-background text-foreground">
      {/* Skip-to-content: first focusable element, visually hidden until focused,
          so a keyboard user can jump past the nav to the main region (WCAG 2.4.1). */}
      <a
        href="#main-content"
        className="sr-only rounded-md focus:not-sr-only focus:absolute focus:left-3 focus:top-3 focus:z-50 focus:bg-primary focus:px-3 focus:py-2 focus:text-sm focus:font-medium focus:text-primary-foreground focus-visible:outline focus-visible:outline-2 focus-visible:outline-offset-2 focus-visible:outline-ring"
      >
        Skip to main content
      </a>
      {/* Hidden — not merely covered — in focus mode: the cockpit's overlay is
          painted over the shell anyway, but leaving the header mounted would
          keep a dozen focusable controls ahead of the terminal in tab order
          (WCAG 2.4.3) and in the accessibility tree. */}
      {!focus && (
        <TopBar
          onSignOut={onSignOut}
          meta={meta}
          pendingApprovals={pendingApprovals}
          attentionCount={attentionCount}
          confinementClasses={confinementClasses}
          onNewRun={() => navigate("/runs/new")}
        />
      )}
      {/* NOT hidden in focus mode, and z-50 so the cockpit's overlay (z-40)
          cannot paint over it: this banner is the only thing that separates a
          quiet fleet from a dead daemon, and a full-bleed terminal is exactly
          where you would otherwise never notice. */}
      {unreachable && (
        <div
          role="status"
          className="relative z-50 flex shrink-0 items-center gap-2 border-b border-border bg-warning-subtle px-4 py-2 text-sm text-warning"
        >
          <AlertTriangle className="size-4 shrink-0" />
          <span>Control plane unreachable — showing the last data received. {lastCheckedLabel(lastOkAt ?? null)}</span>
        </div>
      )}
      {/* W31-S1-7: the SSO session dies outright at its expiry, with no
          refresh — this is the warning that never existed, so it is not a
          silent 401 that wipes the console mid-work. Re-authenticating now
          (while the current session still works) replaces it before it dies. */}
      {!unreachable && sessionExpiringSoon && (
        <div
          role="status"
          className="relative z-50 flex shrink-0 items-center gap-2 border-b border-border bg-warning-subtle px-4 py-2 text-sm text-warning"
        >
          <AlertTriangle className="size-4 shrink-0" />
          <span>Your session is expiring soon.</span>
          <a href="/auth/login" className="font-medium underline underline-offset-2">
            Sign in again
          </a>
          <span>to avoid losing your place.</span>
        </div>
      )}
      <div className="flex min-h-0 flex-1">
        {!focus && (
          <aside className="hidden w-[228px] shrink-0 flex-col border-r border-sidebar-border bg-sidebar px-3 py-4 md:flex">
            <SidebarNav
              pendingApprovals={pendingApprovals}
              attentionCount={attentionCount}
              meta={meta}
            />
          </aside>
        )}

        <main id="main-content" tabIndex={-1} className="scroll-thin min-w-0 flex-1 overflow-y-auto focus:outline-none">
          {/* Keyed by pathname so navigating away from a screen that threw
              clears the caught error instead of wedging the console. */}
          <ErrorBoundary key={location.pathname} region={location.pathname}>
            <Outlet />
          </ErrorBoundary>
        </main>
      </div>

      {/* The NewRunDialog mounted here. New run is a PAGE now (/runs/new): a
          five-step modal put the consequences of every choice on a Review
          screen you reached last, after making them all blind. The one-page
          screen shows the live policy rail beside the form the whole time. */}
    </div>
    </FocusContext.Provider>
    </RoleProvider>
    </OperatorProvider>
  );
}

function TopBar({
  onSignOut,
  meta,
  pendingApprovals,
  attentionCount,
  confinementClasses,
  onNewRun,
}: {
  onSignOut: () => void;
  meta: ShellMeta;
  pendingApprovals: number;
  attentionCount: number;
  confinementClasses: ConfinementClass[];
  onNewRun: () => void;
}) {
  const { theme, toggle } = useTheme();
  return (
    <header className="flex h-14 shrink-0 items-center gap-4 border-b border-border bg-card/70 px-4 backdrop-blur">
      <MobileNav pendingApprovals={pendingApprovals} attentionCount={attentionCount} meta={meta} />
      <Link to="/runs" className="rounded-sm focus-visible:outline focus-visible:outline-2 focus-visible:outline-ring">
        <WardynWordmark />
      </Link>

      <div className="ml-2 hidden items-center gap-2 lg:flex">
        <EnvIndicator trustDomain={meta.trustDomain} />
        <Chip tone="neutral" className="font-mono">
          <Fingerprint className="size-3" />
          identity: {meta.identityProvider}
        </Chip>
      </div>

      <div className="ml-auto flex items-center gap-1.5">
        <Button variant="ghost" size="icon" onClick={toggle} aria-label="Toggle theme">
          {theme === "dark" ? <Sun className="size-4" /> : <Moon className="size-4" />}
        </Button>

        <BarrierChip classes={confinementClasses} />

        <Button onClick={onNewRun} size="sm">
          <Plus className="size-4" /> New run
        </Button>

        <DropdownMenu>
          <DropdownMenuTrigger asChild>
            {/* ui-shellAuth-4: the shared Button (not a raw <button>), matching
                every sibling header control (theme toggle above, mobile nav
                trigger) — its focus-visible ring is what keyboard focus falls
                back to instead of the bare unthemed browser outline. */}
            <Button variant="ghost" className="h-auto gap-2 rounded-md px-1.5 py-1">
              <span className="flex size-7 items-center justify-center rounded-full bg-secondary text-xs text-foreground">{initials(meta.principal)}</span>
              <span className="hidden text-sm sm:block">{meta.principal.split("@")[0]}</span>
              <ChevronsUpDown className="size-3.5 text-muted-foreground" />
            </Button>
          </DropdownMenuTrigger>
          <DropdownMenuContent align="end" className="w-56">
            <DropdownMenuLabel>
              <div className="flex items-center gap-1.5">
                <span className="min-w-0 truncate font-mono text-xs text-muted-foreground">{meta.principal}</span>
                {/* Role is a fact, not an alert (prompt-v2): a quiet chip, no
                    banner, no callout — admin is unchanged, member just says so.
                    Gated on meta.method like its sibling line below: /me hasn't
                    resolved (or failed) while method is still "" — the fail-open
                    role default is "admin" (operator-context.tsx), which would
                    otherwise flash ADMIN next to a still-"unknown" principal. */}
                {meta.method && (
                  <Chip tone="neutral" className="shrink-0 uppercase tracking-wide">
                    {meta.role}
                  </Chip>
                )}
              </div>
              <div className="mt-0.5 text-meta text-muted-foreground">
                {meta.method === "sso"
                  ? "signed in via SSO"
                  : meta.method === "token"
                    ? "admin token"
                    : meta.method === "local"
                      ? "local mode — no login on this install"
                      : ""}
              </div>
            </DropdownMenuLabel>
            <DropdownMenuSeparator />
            {/* The guided Getting Started funnel — an operator-chosen route
                (setup-gate.ts has no hard gate any more), not the seven-item
                sidebar: this menu entry and the Runs empty state's "guided
                tour" link (runs-first-run.tsx) are the two ways in. */}
            <DropdownMenuItem asChild>
              <Link to="/setup">
                <Compass className="size-4" /> Getting started
              </Link>
            </DropdownMenuItem>
            {/* Settings is the one home for connections — Host · Model provider ·
                Git host · Your SSH keys. It replaced /integrations, which now
                redirects here, and the barrier chip above points at it too. */}
            <DropdownMenuItem asChild>
              <Link to="/settings">
                <Settings className="size-4" /> Settings
              </Link>
            </DropdownMenuItem>
            <DropdownMenuItem asChild>
              <Link to="/ssh-keys">
                <KeyRound className="size-4" /> SSH keys
              </Link>
            </DropdownMenuItem>
            {/* Demos has no server-side role gate (routes.go), so it's offered
                here for every role — same reasoning the old sidebar carried.
                It points into Getting Started's first demo step: the funnel is
                the one demos surface now, /demos only redirects here. */}
            <DropdownMenuItem asChild>
              <Link to="/setup?step=sealed-box">
                <FlaskConical className="size-4" /> Demos
              </Link>
            </DropdownMenuItem>
            {/* W31-S1-1: local mode has no session to sign out of — humanOrAdminAuth
                (internal/api/http.go) bypasses auth entirely, so "Sign out" would drop
                the client to a SignIn screen whose admin-token field is unchecked
                (probeAuth trivially re-succeeds against the auth-bypassed API on
                whatever's typed). Hide the no-op action instead of offering fake auth. */}
            {meta.method !== "local" && (
              <>
                <DropdownMenuSeparator />
                <DropdownMenuItem onClick={onSignOut} className="text-danger focus:text-danger">
                  <LogOut className="size-4" /> Sign out
                </DropdownMenuItem>
              </>
            )}
          </DropdownMenuContent>
        </DropdownMenu>
      </div>
    </header>
  );
}

function EnvIndicator({ trustDomain }: { trustDomain: string }) {
  return (
    <span className="inline-flex items-center gap-2 rounded-md border border-border bg-surface-2 px-2 py-1 text-xs">
      <span className="size-1.5 rounded-full bg-success" />
      <span className="font-mono text-muted-foreground">{trustDomain}</span>
    </span>
  );
}

// The permanent top-bar barrier chip (stage-1): the strongest confinement tier
// this host can run right now, via the same Fence/Wall/Vault metal ramp every
// other barrier chip uses (ConfinementChip) — never the teal accent, which
// means "action" elsewhere in this console. An empty confinement-class list
// (nothing installed, or the daemon hasn't answered yet) is the one state that
// gets its own honest danger chip instead of guessing a tier. Clicking either
// state opens Settings, where the barrier's own detail lives.
function BarrierChip({ classes }: { classes: ConfinementClass[] }) {
  const strongest = strongestAvailable(classes);
  return (
    <Link
      to="/settings"
      className="rounded-md focus-visible:outline focus-visible:outline-2 focus-visible:outline-offset-2 focus-visible:outline-ring"
      aria-label={strongest ? "Sandbox barrier — open Settings" : "No sandbox barrier — open Settings"}
    >
      {strongest ? (
        <ConfinementChip value={strongest} />
      ) : (
        <Chip tone="danger" dot>
          No barrier
        </Chip>
      )}
    </Link>
  );
}
