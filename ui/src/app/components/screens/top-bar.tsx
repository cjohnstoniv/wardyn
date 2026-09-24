/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Extracted from app-shell.tsx (the seam docs/design/*.md already names) to
// keep that file under the size gate. TopBar owns the header row's identity
// chrome, theme toggle, New run button and account menu; MobileNav, ShellMeta
// and the isCustom* trust-domain/identity-provider checks stay in
// app-shell.tsx, which SidebarNav also relies on for its own trust-domain
// panel.
import { Link, useNavigate } from "react-router-dom";
import {
  ChevronsUpDown,
  Compass,
  Fingerprint,
  FlaskConical,
  KeyRound,
  LogOut,
  Moon,
  Plus,
  Settings,
  Sun,
} from "lucide-react";
import { WardynWordmark } from "../wardyn/logo";
import { Chip } from "../wardyn/primitives";
import { useTheme } from "../wardyn/theme-provider";
import { useGuardedNavClick } from "../../lib/use-unsaved-guard";
import { NAV } from "../../lib/unsaved-copy";
import { Button } from "../ui/button";
import {
  DropdownMenu,
  DropdownMenuContent,
  DropdownMenuItem,
  DropdownMenuLabel,
  DropdownMenuSeparator,
  DropdownMenuTrigger,
} from "../ui/dropdown-menu";
import { MemberModeMenuItem } from "../wardyn/member-mode-banner";
import {
  isCustomIdentityProvider,
  isCustomTrustDomain,
  MobileNav,
  type ShellMeta,
} from "./app-shell";

function initials(principal: string): string {
  const base = principal.split("@")[0] || principal;
  // Whitespace joins the separators (0.7.1): the value may now be an IdP
  // display name ("Alice Smith" → AS), not only an email local-part.
  const parts = base.split(/[\s.\-_]+/).filter(Boolean);
  const s = (parts[0]?.[0] ?? "") + (parts[1]?.[0] ?? parts[0]?.[1] ?? "");
  return (s || base.slice(0, 2)).toUpperCase();
}

// Exported (like MobileNav in app-shell.tsx) so a unit test can drive the account menu
// directly — SidebarNav's own member tests don't touch this at all.
export function TopBar({
  onSignOut,
  meta,
  pendingApprovals,
  attentionCount,
  onNewRun,
}: {
  onSignOut: () => void;
  meta: ShellMeta;
  pendingApprovals: number;
  attentionCount: number;
  onNewRun: () => void;
}) {
  // What the header calls "you": the IdP's display name, else the session
  // email, else the principal itself (an admin token or local mode has
  // neither). Display only — usePrincipal() still reads meta.principal.
  const display = meta.name || meta.email || meta.principal;
  const { theme, toggle } = useTheme();
  // #460 review — every plain <Link> in this header can navigate away from a
  // dirty editor (app-shell.tsx#SidebarNav's own guardedClick precedent);
  // this menu is the one place besides the sidebar the console offers that.
  const navigate = useNavigate();
  const guardedClick = useGuardedNavClick(navigate);
  // M-1b: /settings is deleted — Settings now lives at /admin/settings, Your
  // account at /account (both still mount the unsplit SettingsScreen until
  // M-5 splits it).
  const settingsTarget = meta.role === "user" ? "/account" : "/admin/settings";
  return (
    <header className="flex h-14 shrink-0 items-center gap-4 border-b border-border bg-card/70 px-4 backdrop-blur">
      <MobileNav
        pendingApprovals={pendingApprovals}
        attentionCount={attentionCount}
        meta={meta}
      />
      <Link
        to="/runs"
        onClick={guardedClick("/runs")}
        className="rounded-sm focus-visible:outline focus-visible:outline-2 focus-visible:outline-ring"
      >
        {/* F7-F2: icon-only below sm, so New run + the user menu stay onscreen. */}
        <WardynWordmark compact="sm" />
      </Link>

      {/* Shown ONLY when non-default. A default install is always
          wardyn.local / embedded, so these chips would be four constants nobody
          can act on, occupying the most valuable strip on every screen — while
          the footer panel still states the trust domain for anyone who wants it.
          An external SPIRE provider or a custom trust domain IS worth a reader's
          attention, and only then do they appear. */}
      {(isCustomTrustDomain(meta.trustDomain) ||
        isCustomIdentityProvider(meta.identityProvider)) && (
        <div className="ml-2 hidden items-center gap-2 lg:flex">
          {isCustomTrustDomain(meta.trustDomain) && (
            <EnvIndicator trustDomain={meta.trustDomain} />
          )}
          {isCustomIdentityProvider(meta.identityProvider) && (
            <Chip tone="neutral" className="font-mono">
              <Fingerprint className="size-3" />
              identity: {meta.identityProvider}
            </Chip>
          )}
        </div>
      )}

      {/* F7-F2: min-w-0 lets this cluster actually shrink instead of forcing
          the header wider than the viewport (no flex-wrap/height change). */}
      <div className="ml-auto flex min-w-0 items-center gap-1.5">
        <Button
          variant="ghost"
          size="icon"
          onClick={toggle}
          aria-label="Toggle theme"
        >
          {theme === "dark" ? (
            <Sun className="size-4" />
          ) : (
            <Moon className="size-4" />
          )}
        </Button>

        <Button onClick={onNewRun} size="sm" aria-label="New run">
          <Plus className="size-4" /> <span className="hidden sm:inline">New run</span>
        </Button>

        <DropdownMenu>
          <DropdownMenuTrigger asChild>
            {/* The shared Button (not a raw <button>), matching
                every sibling header control (theme toggle above, mobile nav
                trigger) — its focus-visible ring is what keyboard focus falls
                back to instead of the bare unthemed browser outline. */}
            <Button
              variant="ghost"
              className="h-auto gap-2 rounded-md px-1.5 py-1"
            >
              <span className="flex size-7 items-center justify-center rounded-full bg-secondary text-xs text-foreground">
                {initials(display)}
              </span>
              <span className="hidden max-w-48 truncate text-sm sm:block">
                {display.split("@")[0]}
              </span>
              <ChevronsUpDown className="size-3.5 text-muted-foreground" />
            </Button>
          </DropdownMenuTrigger>
          <DropdownMenuContent align="end" className="w-56">
            <DropdownMenuLabel>
              <div className="flex items-center gap-1.5">
                <span className="min-w-0 truncate text-xs" title={display}>
                  {display}
                </span>
                {/* Role is a fact, not an alert (prompt-v2): a quiet chip, no
                    banner, no callout — admin is unchanged, member just says so.
                    Gated on meta.method like its sibling line below: /me hasn't
                    resolved (or failed) while method is still "" — the fail-open
                    role default is "admin" (operator-context.tsx), which would
                    otherwise flash ADMIN next to a still-"unknown" principal. */}
                {meta.method && (
                  <Chip
                    tone="neutral"
                    className="shrink-0 uppercase tracking-wide"
                  >
                    {meta.role}
                  </Chip>
                )}
              </div>
              {/* The sign-in subject, kept where admins are told to copy it from
                  (OPERATIONS.md: paste the sign-in subject) — only when the
                  line above is not already showing it. */}
              {display !== meta.principal && (
                <div
                  className="min-w-0 truncate font-mono text-xs text-muted-foreground"
                  title={meta.principal}
                >
                  {meta.principal}
                </div>
              )}
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
                (setup-gate.ts has no hard gate any more), not the eight-item
                sidebar: this menu entry and the Runs empty state's "guided
                tour" link (runs-first-run.tsx) are the two ways in. */}
            <DropdownMenuItem asChild>
              <Link to="/setup" onClick={guardedClick("/setup")}>
                <Compass className="size-4" /> Getting started
              </Link>
            </DropdownMenuItem>
            {/* Settings is the one home for connections — Host · Model provider ·
                Providers · Your SSH keys. M-1b: /settings and /integrations are
                both deleted (clean break) — an admin tier lands on
                /admin/settings, a member on their own /account. Hidden on a
                SETTLED-but-unknown identity: this is the two-click route to the
                operator-only Model-provider Connect/Disconnect card and the
                Providers card into /admin/providers, and the shell paints no
                route at all in that state, so the link would be an invitation
                to a blank page. Sign out below stays — it is the one control
                that still works. */}
            {!(meta.resolved && !meta.identityResolved) && (
              <DropdownMenuItem asChild>
                <Link to={settingsTarget} onClick={guardedClick(settingsTarget)}>
                  <Settings className="size-4" /> {NAV.SETTINGS}
                </Link>
              </DropdownMenuItem>
            )}
            <MemberModeMenuItem meta={meta} />
            <DropdownMenuItem asChild>
              <Link to="/ssh-keys" onClick={guardedClick("/ssh-keys")}>
                <KeyRound className="size-4" /> SSH keys
              </Link>
            </DropdownMenuItem>
            {/* Demos has no server-side role gate (routes.go), so admins keep
                the same reasoning the old sidebar carried — it points into
                Getting Started's first demo step, the one demos surface now
                (/demos only redirects here). Hidden for members (Phase 5):
                /setup?step=sealed-box is meaningless on the member's own
                Getting Started (member-getting-started.tsx) — a member never
                reaches the admin welcome hero or its step query at all, and
                its own episode catalog is a single flat "Watch" list at the
                bottom of the page, not a step deep link.
                `!== "admin"`, never `=== "user"`. Only the SUPER admin's
                SetupScreen honours ?step — a security admin's /setup/status is
                redacted on the same !isOperator predicate (internal/api/setup.go)
                and App.tsx hands them the same Getting Started, so the deep link
                is exactly as dead for them. */}
            {meta.role === "admin" && (
              <DropdownMenuItem asChild>
                <Link to="/setup?step=sealed-box" onClick={guardedClick("/setup?step=sealed-box")}>
                  <FlaskConical className="size-4" /> Demos
                </Link>
              </DropdownMenuItem>
            )}
            {/* Local mode has no session to sign out of — humanOrAdminAuth
                (internal/api/http.go) bypasses auth entirely, so "Sign out" would drop
                the client to a SignIn screen whose admin-token field is unchecked
                (probeAuth trivially re-succeeds against the auth-bypassed API on
                whatever's typed). Hide the no-op action instead of offering fake auth. */}
            {meta.method !== "local" && (
              <>
                <DropdownMenuSeparator />
                <DropdownMenuItem
                  onClick={onSignOut}
                  className="text-danger focus:text-danger"
                >
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
