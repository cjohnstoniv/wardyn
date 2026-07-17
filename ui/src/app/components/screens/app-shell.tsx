/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import * as React from "react";
import { Link, NavLink, Outlet, useLocation, useNavigate } from "react-router-dom";
import {
  Activity,
  ChevronsUpDown,
  Fingerprint,
  FolderOpen,
  Lock,
  LogOut,
  Menu,
  Moon,
  Plus,
  Rocket,
  ScrollText,
  ShieldCheck,
  SquareTerminal,
  Sun,
  UserCog,
} from "lucide-react";
import { WardynWordmark } from "../wardyn/logo";
import { Chip, SectionLabel } from "../wardyn/primitives";
import { StatusChip } from "../wardyn/status-chip";
import { useTheme } from "../wardyn/theme-provider";
import { deriveReadiness } from "./onboarding/intro";
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
import { health as api } from "../../lib/api/health";
import { setup as setupApi } from "../../lib/api/setup";
import type { StatusKind } from "../wardyn/copy";
import { usePoll } from "../../lib/use-poll";
// The run wizard reaches the workspaces + secrets screens and their dialogs, so
// importing it eagerly pulled all of that into the entry chunk even though the
// dialog only ever mounts on a "New run" click. Fetched on that click instead.
const NewRunDialog = React.lazy(() => import("./new-run/new-run-dialog").then((m) => ({ default: m.NewRunDialog })));

// useMeta fetches the real trust boundary (/healthz) + signed-in principal
// (/api/v1/me) so the shell never shows placeholder identity/tenant values.
export interface ShellMeta {
  trustDomain: string;
  identityProvider: string;
  principal: string;
  method: string;
}

function useMeta(): ShellMeta {
  const [meta, setMeta] = React.useState<ShellMeta>({
    trustDomain: "…",
    identityProvider: "…",
    principal: "…",
    method: "",
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
      });
    });
    return () => {
      alive = false;
    };
  }, []);
  return meta;
}

function initials(principal: string): string {
  const base = principal.split("@")[0] || principal;
  const parts = base.split(/[.\-_]/).filter(Boolean);
  const s = (parts[0]?.[0] ?? "") + (parts[1]?.[0] ?? parts[0]?.[1] ?? "");
  return (s || base.slice(0, 2)).toUpperCase();
}

// How often the readiness chip re-checks setup status, in ms.
const READINESS_POLL_MS = 15000;

// Grouped sidebar nav (redesign): OPERATE / CONFIGURE / FORENSICS, plus a
// pinned "Getting started" entry. Fleet is intentionally NOT here — it stays
// routable at /fleet but is retired from the nav in a later phase.
interface NavItem {
  to: string;
  label: string;
  icon: React.ElementType;
  badge?: "approvals" | "attention";
}
const NAV_GROUPS: { label: string; items: NavItem[] }[] = [
  {
    label: "Operate",
    items: [
      { to: "/runs", label: "Runs", icon: Activity, badge: "attention" },
      { to: "/approvals", label: "Approvals", icon: ShieldCheck, badge: "approvals" },
    ],
  },
  {
    label: "Configure",
    items: [
      { to: "/policies", label: "Policies", icon: UserCog },
      { to: "/secrets", label: "Secrets", icon: Lock },
      { to: "/workspaces", label: "Workspaces", icon: FolderOpen },
    ],
  },
  {
    label: "Forensics",
    items: [
      { to: "/audit", label: "Audit", icon: ScrollText },
      { to: "/recordings", label: "Recordings", icon: SquareTerminal },
    ],
  },
];

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
  readiness,
  meta,
  onNavigate,
}: {
  pendingApprovals: number;
  attentionCount: number;
  readiness: StatusKind;
  meta: ShellMeta;
  onNavigate?: () => void;
}) {
  return (
    <>
      <nav className="space-y-4">
        {NAV_GROUPS.map((group) => (
          <div key={group.label} className="space-y-0.5">
            <SectionLabel className="px-2.5 pb-1">{group.label}</SectionLabel>
            {group.items.map((item) => {
              const count =
                item.badge === "approvals"
                  ? pendingApprovals
                  : item.badge === "attention"
                    ? attentionCount
                    : 0;
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
                        <span className="inline-flex min-w-5 items-center justify-center rounded-full bg-warning-subtle px-1.5 text-[11px] font-semibold text-warning">
                          {count}
                        </span>
                      )}
                    </>
                  )}
                </NavLink>
              );
            })}
          </div>
        ))}
      </nav>

      <div className="mt-auto space-y-3">
        <NavLink to="/setup" onClick={onNavigate} className={({ isActive }) => navLinkClass(isActive)}>
          {({ isActive }) => (
            <>
              {isActive && (
                <span className="absolute -left-3 top-1/2 h-5 w-0.5 -translate-y-1/2 rounded-r bg-sidebar-primary" />
              )}
              <Rocket className={cn("size-4", isActive && "text-foreground")} />
              <span className="flex-1 text-left">Getting started</span>
              <StatusChip status={readiness} />
            </>
          )}
        </NavLink>

        <div className="rounded-lg border border-sidebar-border bg-card/50 p-3">
          <div className="flex items-center gap-2 text-xs text-muted-foreground">
            <Fingerprint className="size-3.5 text-muted-foreground" />
            <span className="font-mono">{meta.trustDomain}</span>
          </div>
          <p className="mt-1.5 text-[11px] leading-relaxed text-muted-foreground">
            All agent identities anchored to this trust domain.
          </p>
        </div>
      </div>
    </>
  );
}

// Below the md breakpoint the desktop aside is hidden; this hamburger + Sheet is
// the only nav fallback (U121). Sheet is Radix Dialog underneath, so Escape-to-
// close and focus-return-to-trigger come for free, and the trigger exposes
// aria-expanded/aria-controls automatically. md:hidden pairs it with the aside's
// md:flex so exactly one is present at any width.
export function MobileNav(props: {
  pendingApprovals: number;
  attentionCount: number;
  readiness: StatusKind;
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
}: {
  pendingApprovals: number;
  attentionCount: number;
  onSignOut: () => void;
}) {
  const meta = useMeta();
  const location = useLocation();
  const [newRunOpen, setNewRunOpen] = React.useState(false);
  // Latches true on the first "New run" click and never resets — see the mount
  // note at the dialog below.
  const [newRunMounted, setNewRunMounted] = React.useState(false);
  const navigate = useNavigate();

  const [readiness, setReadiness] = React.useState<StatusKind>("checking");
  const checkReadiness = React.useCallback(() => {
    setupApi
      .getSetupStatus()
      .then((s) => {
        // Agree with the funnel's essentials verdict (barrier AND model) —
        // backend `ready` alone is barrier-only and would overclaim here.
        const r = deriveReadiness(s);
        setReadiness(r.ready && r.llmReady ? "ready" : "needs-setup");
      })
      .catch(() => {
        /* leave the last-known readiness in place */
      });
  }, []);
  React.useEffect(checkReadiness, [checkReadiness]);
  usePoll(checkReadiness, READINESS_POLL_MS, false);

  return (
    <div className="flex h-screen flex-col bg-background text-foreground">
      {/* Skip-to-content: first focusable element, visually hidden until focused,
          so a keyboard user can jump past the nav to the main region (WCAG 2.4.1). */}
      <a
        href="#main-content"
        className="sr-only rounded-md focus:not-sr-only focus:absolute focus:left-3 focus:top-3 focus:z-50 focus:bg-primary focus:px-3 focus:py-2 focus:text-sm focus:font-medium focus:text-primary-foreground focus-visible:outline focus-visible:outline-2 focus-visible:outline-offset-2 focus-visible:outline-ring"
      >
        Skip to main content
      </a>
      <TopBar
        onSignOut={onSignOut}
        meta={meta}
        pendingApprovals={pendingApprovals}
        attentionCount={attentionCount}
        readiness={readiness}
        onNewRun={() => {
          setNewRunMounted(true);
          setNewRunOpen(true);
        }}
      />
      <div className="flex min-h-0 flex-1">
        <aside className="hidden w-[228px] shrink-0 flex-col border-r border-sidebar-border bg-sidebar px-3 py-4 md:flex">
          <SidebarNav
            pendingApprovals={pendingApprovals}
            attentionCount={attentionCount}
            readiness={readiness}
            meta={meta}
          />
        </aside>

        <main id="main-content" tabIndex={-1} className="scroll-thin min-w-0 flex-1 overflow-y-auto focus:outline-none">
          {/* Keyed by pathname so navigating away from a screen that threw
              clears the caught error instead of wedging the console. */}
          <ErrorBoundary key={location.pathname} region={location.pathname}>
            <Outlet />
          </ErrorBoundary>
        </main>
      </div>

      {/* Mounted from the first open onward — not gated on `newRunOpen` — so the
          dialog keeps its own close animation and internal state instead of
          being torn down on every dismiss. */}
      {newRunMounted && (
        <React.Suspense fallback={null}>
          <NewRunDialog open={newRunOpen} onOpenChange={setNewRunOpen} onCreated={() => navigate("/runs")} />
        </React.Suspense>
      )}
    </div>
  );
}

function TopBar({
  onSignOut,
  meta,
  pendingApprovals,
  attentionCount,
  readiness,
  onNewRun,
}: {
  onSignOut: () => void;
  meta: ShellMeta;
  pendingApprovals: number;
  attentionCount: number;
  readiness: StatusKind;
  onNewRun: () => void;
}) {
  const { theme, toggle } = useTheme();
  return (
    <header className="flex h-14 shrink-0 items-center gap-4 border-b border-border bg-card/70 px-4 backdrop-blur">
      <MobileNav
        pendingApprovals={pendingApprovals}
        attentionCount={attentionCount}
        readiness={readiness}
        meta={meta}
      />
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
        <Button onClick={onNewRun} size="sm">
          <Plus className="size-4" /> New run
        </Button>

        <Button variant="ghost" size="icon" onClick={toggle} aria-label="Toggle theme">
          {theme === "dark" ? <Sun className="size-4" /> : <Moon className="size-4" />}
        </Button>

        <DropdownMenu>
          <DropdownMenuTrigger asChild>
            <button className="flex items-center gap-2 rounded-md px-1.5 py-1 hover:bg-accent">
              <span className="flex size-7 items-center justify-center rounded-full bg-secondary text-xs text-foreground">{initials(meta.principal)}</span>
              <span className="hidden text-sm sm:block">{meta.principal.split("@")[0]}</span>
              <ChevronsUpDown className="size-3.5 text-muted-foreground" />
            </button>
          </DropdownMenuTrigger>
          <DropdownMenuContent align="end" className="w-56">
            <DropdownMenuLabel>
              <div className="font-mono text-xs text-muted-foreground">{meta.principal}</div>
              <div className="mt-0.5 text-[11px] text-muted-foreground">
                {meta.method === "sso" ? "signed in via SSO" : meta.method === "token" ? "admin token" : ""}
              </div>
            </DropdownMenuLabel>
            <DropdownMenuSeparator />
            <DropdownMenuItem onClick={onSignOut} className="text-danger focus:text-danger">
              <LogOut className="size-4" /> Sign out
            </DropdownMenuItem>
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
