/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import * as React from "react";
import {
  NavLink,
  Outlet,
  useLocation,
  useNavigate,
} from "react-router-dom";
import {
  Activity,
  AlertTriangle,
  Fingerprint,
  FolderOpen,
  Lock,
  Menu,
  Play,
  Scale,
  ScrollText,
  ShieldCheck,
  UserCog,
  Users,
} from "lucide-react";
import { SHELL } from "../wardyn/copy";
import { lastCheckedLabel } from "../../lib/readiness";
// GOVERNANCE.TITLE is ONE string for two places — this nav label and the
// screen's own heading — the way every other nav entry already works. There is
// no second "Governance profiles" label (governance-prompt.md §7.2).
import { GOVERNANCE } from "../../lib/governance-copy";
import { cn } from "../ui/utils";
import { Button } from "../ui/button";
import { Sheet, SheetContent, SheetTitle, SheetTrigger } from "../ui/sheet";
import { MemberModeBanner } from "../wardyn/member-mode-banner";
import { resolveConfinementPosture } from "../../lib/confinement-posture";
import { ErrorBoundary } from "../wardyn/error-boundary";
import {
  OperatorProvider,
  RoleProvider,
  type Role,
} from "../wardyn/operator-context";
import { health as api, type MeUserDrive } from "../../lib/api/health";
import { TopBar } from "./top-bar";
// The run wizard reaches the workspaces + secrets screens and their dialogs, so
// importing it eagerly pulled all of that into the entry chunk even though the
// dialog only ever mounts on a "New run" click. Fetched on that click instead.

// useMeta fetches the real trust boundary (/healthz) + signed-in principal
// (/api/v1/me) so the shell never shows placeholder identity/tenant values.
export interface ShellMeta {
  trustDomain: string;
  identityProvider: string;
  // principal is the OWNERSHIP key (the OIDC sub, "admin-token", "local:…"):
  // it feeds PrincipalContext and every `usePrincipal() === run.created_by`
  // gate. email and name are DISPLAY ONLY — what the header shows for "you",
  // in that order of preference — and are "" outside SSO or when the IdP sent
  // none, so the header falls back to the principal there (0.7.1).
  principal: string;
  email: string;
  name: string;
  method: string;
  // True once the /me fetch has SETTLED — success or failure. This is the
  // landing gate's signal (App.tsx's FirstRunLanding), and it must not be
  // derived from `method` ("" after a failed /me) or a failed fetch strands
  // "/" on a spinner forever; the role it then reads is the fail-open admin.
  resolved: boolean;
  // True only when /me actually ANSWERED (a body came back), which `resolved`
  // above deliberately does not say — whoami() swallows every failure and
  // returns null, so `resolved` flips on a FAILED fetch too. Anything that
  // picks a lane off `operator` rather than merely offering a control needs
  // this one instead (R4-F110; see operator-context.tsx's
  // OperatorResolvedContext for the attach-WS case that named it).
  identityResolved: boolean;
  // Fail-open (see operator-context.tsx): starts true and stays true unless
  // /me resolves and explicitly says otherwise — an unresolved or failed
  // fetch must never read as "viewer".
  operator: boolean;
  // The SECOND server predicate (admin OR security_admin — /me's
  // `security_operator`), gating the security-governance surfaces. Same
  // fail-open default as `operator`, and deliberately a separate field: with
  // three role values the two booleans are not complements of each other.
  securityOperator: boolean;
  // The same B1-derived tier as `operator`, named directly (B3) — fail-open
  // "admin" for the identical three cases (unresolved /me, a failed fetch, an
  // unwrapped test). Kept alongside `operator` rather than replacing it: every
  // existing operator-only gate stays exactly as it was.
  role: Role;
  // When the SSO session dies outright (no refresh) — null for
  // local/token auth, which has no session to expire.
  sessionExpiresAt: Date | null;
  // M3 — see operator-context.tsx's MemberLocalDirRootContext. null until /me
  // resolves and stays null (fail-closed: unavailable) if it never does.
  memberLocalDirRoot: string | null;
  // 0.7 — the caller's own allocation and the profile door beside it, the same
  // /me body every other field here comes from. New Run and the member Getting
  // Started page read them off the context rather than issuing a second and a
  // third GET /me of their own; same fail-closed default as memberLocalDirRoot.
  // The trade that buys is FRESHNESS PER PAGE LOAD, not per navigation — a
  // member paused mid-session keeps the offer until they reload and learns at
  // launch, which is the direction of error this feature can afford (see
  // operator-context.tsx's UserDriveContext for the whole argument).
  userDrive: MeUserDrive | null;
  userDriveDeniedByProfile: string;
  // R4/F091 — the THIRD drive key: WHY /me could not answer, "" when it could.
  // The server always sends it on 0.7 and suppresses the allocation alongside
  // it; without it four distinguishable answers reach the member as the single
  // one whose remedy is wrong for all of them (operator-context.tsx's
  // UserDriveMeta.unavailable carries the whole argument). Same fail-closed
  // default as the two above: an unresolved or failed /me reads as "" —
  // nothing is claimed about a drive that is also null.
  userDriveUnavailable: string;
  /** 0.7.4 "view as member" — an admin whose role is paused for this session. */
  memberMode: boolean;
  /** 0.7.5 — WHICH posture of that mode: the no-credential preview, in which
   *  this admin's own model credential reads as not signed in. Implies
   *  memberMode, so only the banner's wording changes on it. */
  memberModeNoCredential: boolean;
  /** 0.7.5 — whether this deployment's roster makes the no-credential preview
   *  mean anything. False hides the second account-menu entry entirely. */
  memberPreviewAvailable: boolean;
  /** #162 — /healthz's `runner` ("docker" / "k8s" / "" on a pre-mount default
   *  or an older daemon) and `network_policy` ("enforced" / "acknowledged" /
   *  "unenforced", absent as ""). Neither is read directly by a screen — both
   *  feed resolveConfinementPosture (confinement-posture.tsx), which is the
   *  only place that may tell "not applicable" (Docker) from "could not
   *  confirm" (a k8s daemon that omitted the verdict) apart. */
  runner: string;
  networkPolicy: string;
}

/** The shell's identity, plus the retry that re-fires /me (B1's banner action). */
function useMeta(): [ShellMeta, () => void] {
  // Bumped by retry(), which is the effect's only other dependency: /me is
  // fetched once per load today, so after a failure identityResolved would stay
  // false forever and the banner below would have nothing to offer.
  const [attempt, setAttempt] = React.useState(0);
  const [meta, setMeta] = React.useState<ShellMeta>({
    trustDomain: "…",
    identityProvider: "…",
    principal: "…",
    email: "",
    name: "",
    method: "",
    resolved: false,
    identityResolved: false,
    operator: true,
    securityOperator: true,
    role: "admin",
    sessionExpiresAt: null,
    memberLocalDirRoot: null,
    userDrive: null,
    userDriveDeniedByProfile: "",
    userDriveUnavailable: "",
    memberMode: false,
    memberModeNoCredential: false,
    memberPreviewAvailable: false,
    runner: "",
    networkPolicy: "",
  });
  React.useEffect(() => {
    let alive = true;
    Promise.all([api.health(), api.whoami()])
      .then(([h, me]) => {
        if (!alive) return;
        setMeta({
          trustDomain: h.trust_domain || "unknown",
          identityProvider: h.identity_provider || "unknown",
          principal: me?.principal || "unknown",
          email: me?.email ?? "",
          // ?? "": a pre-0.7.1 daemon never sends name — absent must read as
          // "none", which falls back to the email, then the principal.
          name: me?.name ?? "",
          method: me?.method || "",
          resolved: true,
          identityResolved: me !== null,
          operator: me?.operator ?? true,
          // ?? true, not `?? me?.operator`: an older daemon that never sends
          // this field must fail OPEN like every other identity signal here.
          securityOperator: me?.security_operator ?? true,
          role: me?.role ?? "admin",
          // F3-F11: a bad string parses to an Invalid Date, not null — guard
          // NaN here so useSessionExpiry never has to.
          sessionExpiresAt: validExpiry(me?.session_expires_at),
          memberLocalDirRoot: me?.member_local_dir_root ?? null,
          userDrive: me?.user_drive ?? null,
          userDriveDeniedByProfile: me?.user_drive_denied_by_profile ?? "",
          userDriveUnavailable: me?.user_drive_unavailable ?? "",
          memberMode: me?.member_mode ?? false,
          memberModeNoCredential: me?.member_mode_no_credential ?? false,
          memberPreviewAvailable: me?.member_preview_available ?? false,
          runner: h.runner ?? "",
          networkPolicy: h.network_policy ?? "",
        });
      })
      .catch(() => {
        // health()/whoami() swallow their own errors today, so this is unreachable;
        // it exists so `resolved` never depends on two other functions keeping
        // that promise — a rejection here would strand the landing gate.
        if (alive) setMeta((m) => ({ ...m, resolved: true }));
      });
    return () => {
      alive = false;
    };
  }, [attempt]);
  return [meta, React.useCallback(() => setAttempt((n) => n + 1), [])];
}

// SESSION_WARN_MS — how far ahead of the session's real expiry to start
// warning. The session itself has no refresh; this is advance
// notice, not a renewal, so the human can save/finish before a silent 401
// wipes the console back to the sign-in gate mid-work.
const SESSION_WARN_MS = 5 * 60 * 1000;
const SESSION_CHECK_MS = 15 * 1000;

// F3-F11: the old predicate was one-sided — "expiring soon" fires at T-5min
// and never turns itself off, so a session already past its real expiry (the
// human stepped away, or the check interval landed late) still read
// "expiring soon" forever, with a re-auth link that could no longer save
// anything. Three states name the THIRD one instead of collapsing it into
// the second.
export type SessionExpiryState = "none" | "soon" | "expired";
// DRAFT (M2 canon pending) — F3-F11's two new arms.
// Exported so a suite asserting the banner stack's order reads the shipped
// sentence rather than a second, hand-copied one.
export const SESSION_EXPIRY_COPY = {
  soon: ["Your session is expiring soon.", "to avoid losing your place."],
  expired: ["Your session has expired.", "to get back in."],
} as const;

function validExpiry(iso: string | null | undefined): Date | null {
  if (!iso) return null;
  const d = new Date(iso);
  return Number.isNaN(d.getTime()) ? null : d;
}

function useSessionExpiry(expiresAt: Date | null): SessionExpiryState {
  const [state, setState] = React.useState<SessionExpiryState>("none");
  React.useEffect(() => {
    if (!expiresAt) return setState("none");
    const check = () => {
      const ms = expiresAt.getTime() - Date.now();
      setState(ms <= 0 ? "expired" : ms <= SESSION_WARN_MS ? "soon" : "none");
    };
    check();
    const id = setInterval(check, SESSION_CHECK_MS);
    return () => clearInterval(id);
  }, [expiresAt]);
  return state;
}

// Flat sidebar nav — nine items, no group headings (stage-1 redesign). Demos
// and Settings are reachable from the account menu below rather than here.
//
// Recordings is BACK (mock M6). It left the sidebar on the theory that a deep
// link from a run or a workspace was enough, which made the evidence trail
// undiscoverable: the screen and its route existed, and nothing on the console
// ever said so. It sits after Audit because the two answer the same question —
// "what happened" — one as events, one as the session itself.
interface NavItem {
  to: string;
  label: string;
  icon: React.ElementType;
  badge?: "approvals" | "attention";
}
const NAV_ITEMS: NavItem[] = [
  { to: "/runs", label: "Runs", icon: Activity, badge: "attention" },
  {
    to: "/approvals",
    label: "Approvals",
    icon: ShieldCheck,
    badge: "approvals",
  },
  { to: "/workspaces", label: "Workspaces", icon: FolderOpen },
  { to: "/policies", label: "Policies", icon: UserCog },
  // Governance (0.7) sits BETWEEN Policies and Permissions so the three read as
  // one narrowing sequence: the deployment ceiling, the ceilings assigned over
  // it, then the grants layered inside one (mock Q1). Not in MEMBER_NAV_PATHS —
  // a member never sees it, and there is no member governance route; its own
  // routes are securityOps server-side.
  { to: "/governance", label: GOVERNANCE.TITLE, icon: Scale },
  // Permissioning (0.6 pillar 2) sits beside Policies: both answer "what is
  // allowed here", one for runs and one for the humans launching them. It is
  // admin-only — deliberately NOT in MEMBER_NAV_PATHS below, and every route
  // behind it is operatorOnly server-side.
  { to: "/permissions", label: "Permissions", icon: Users },
  { to: "/secrets", label: "Secrets", icon: Lock },
  { to: "/audit", label: "Audit", icon: ScrollText },
  { to: "/recordings", label: "Recordings", icon: Play },
];

// Member console (B3): a member launches/governs only THEIR OWN runs — nav is
// Runs · Approvals · Workspaces, nothing else (no Policies/Permissions/
// Secrets/Audit/Recordings). Filtered by route path, never by re-deriving from
// a second copy of NAV_ITEMS.
//
// Workspaces joined the member set (mock M6): a member launches runs AGAINST
// workspaces and had no way to see the ones they can use — the picker in the
// New run wizard was the only place they appeared at all. The screen is
// already server-scoped like every other member surface.
//
// Hiding here is COSMETIC ONLY — every route a member can't reach still
// enforces that itself server-side (internal/api/routes.go's operatorOnly
// group and the owner-or-admin routes); this just keeps a member from
// discovering an admin-only screen as a raw 403 or an empty list instead of
// simply not offering it.
const MEMBER_NAV_PATHS = new Set(["/runs", "/approvals", "/workspaces"]);
function navItemsForRole(role: Role, identityResolved: boolean): NavItem[] {
  // B1 — the fix the field report bought: `role` is fail-open "admin" for an
  // unresolved /me AND for one that failed, so leaving this unguarded offers
  // Policies / Governance / Permissions / Secrets / Audit to a human the
  // server had correctly refused. Indistinguishable from an authz breach,
  // and it cost a customer hours of incident response. So "not known yet"
  // renders neither nav — not the admin set, not the member set — and the
  // banner below says why. `role`'s own fail-open default stays exactly as
  // it was (see
  // operator-context.tsx: never harden it), because the answer to a guess is
  // not a different guess, it is declining to draw one.
  if (!identityResolved) return [];
  // `!== "member"` and NOT `=== "admin"`, which is what makes this correct
  // unchanged under the three-tier model: a SECURITY ADMIN gets the full nav
  // (they reach approvals, audit, permissions and governance), and each of
  // those screens gates its own writes on the right predicate. Hiding is
  // cosmetic anyway — see the note above.
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
const FocusContext = React.createContext<FocusMode>({
  focus: false,
  setFocus: () => {},
});

/** The shell's focus state. Default: off, and setting it is a no-op — a screen
 *  rendered outside <AppShell> (every existing test) can call this safely. */
export function useFocusMode(): FocusMode {
  return React.useContext(FocusContext);
}

// LAZY, like every route in App.tsx and for the same dependency: the strip
// carries the AWS sign-in dialog and the whole AGENTS copy table behind it, and
// this file is in the ENTRY chunk — imported statically it put 17 kB of copy
// into the first paint of every screen (bundle-split.test.ts's entry budget).
// `fallback={null}`: the band is a notification, so a frame without it reads as
// "nothing to say", which is what it renders in the common case anyway.
const ModelAccessBanner = React.lazy(() =>
  import("../wardyn/model-access-banner").then((m) => ({ default: m.ModelAccessBanner })),
);

// #162 — same lazy rationale as ModelAccessBanner above (this file is in the
// entry chunk), and the same "renders nothing when there is nothing to say"
// shape: mounted LAST in the banner stack, after ModelAccessBanner, per the
// mock approval's third ruling.
const ConfinementPostureBanner = React.lazy(() =>
  import("../wardyn/confinement-posture").then((m) => ({ default: m.ConfinementPostureBanner })),
);

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
  const items = navItemsForRole(meta.role, meta.identityResolved);
  return (
    <>
      <nav className="space-y-0.5">
        {items.map((item) => {
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
                  <item.icon
                    className={cn("size-4", isActive && "text-foreground")}
                  />
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
        {/* Owner call (overruling the earlier keep): the default trust domain
            informs nobody anywhere — same non-default rule as the top-bar
            chips. A custom domain is the only one worth a panel. */}
        {isCustomTrustDomain(meta.trustDomain) && (
          <div className="rounded-lg border border-sidebar-border bg-card/50 p-3">
            <div className="flex items-center gap-2 text-xs text-muted-foreground">
              <Fingerprint className="size-3.5 text-muted-foreground" />
              <span className="font-mono">{meta.trustDomain}</span>
            </div>
            <p className="mt-1.5 text-meta leading-relaxed text-muted-foreground">
              All agent identities anchored to this trust domain.
            </p>
          </div>
        )}
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
        <Button
          variant="ghost"
          size="icon"
          className="md:hidden"
          aria-label="Open navigation menu"
        >
          <Menu className="size-5" />
        </Button>
      </SheetTrigger>
      <SheetContent
        side="left"
        aria-describedby={undefined}
        className="w-[248px] bg-sidebar px-3 py-4"
      >
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
}: {
  pendingApprovals: number;
  attentionCount: number;
  onSignOut: () => void;
  // The daemon didn't answer App.tsx's setup-status poll, and when it last did.
  // Every screen's background refresh keeps its last-good data on failure, so
  // this banner is the ONLY thing that tells a quiet board from a dead one.
  unreachable?: boolean;
  lastOkAt?: Date | null;
}) {
  const [meta, retryIdentity] = useMeta();
  // B1 — SETTLED and still unknown: /me answered nothing, so every tier the
  // shell holds is the fail-open seed. Distinct from "not settled yet", which is
  // an ordinary first paint and says nothing to anybody.
  const identityUnknown = meta.resolved && !meta.identityResolved;
  const sessionExpiry = useSessionExpiry(meta.sessionExpiresAt);
  // #162 — see lib/confinement-posture.ts for the runner+network_policy table.
  const confinementPosture = resolveConfinementPosture(meta.runner, meta.networkPolicy);
  const location = useLocation();
  const navigate = useNavigate();

  // See FocusContext above. Nothing here decides WHEN focus is on — the run
  // cockpit's canvas does, and it clears this on unmount.
  const [focus, setFocus] = React.useState(false);
  const focusValue = React.useMemo<FocusMode>(
    () => ({ focus, setFocus }),
    [focus],
  );

  // Wraps EVERYTHING the shell renders (nav, main/Outlet, and the New Run
  // dialog mounted below) — not just the Outlet — so every screen AND every
  // dialog reachable from here (including the wizard's inline Add secret /
  // Add workspace) sees the real role instead of silently falling back to the
  // context default.
  return (
    <OperatorProvider
      operator={meta.operator}
      operatorResolved={meta.identityResolved}
      securityOperator={meta.securityOperator}
      principal={meta.principal}
      memberLocalDirRoot={meta.memberLocalDirRoot}
      userDrive={meta.userDrive}
      userDriveDeniedByProfile={meta.userDriveDeniedByProfile}
      userDriveUnavailable={meta.userDriveUnavailable}
      confinementPosture={confinementPosture}
    >
      <RoleProvider role={meta.role} roleResolved={meta.resolved}>
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
                onNewRun={() => navigate("/runs/new")}
              />
            )}
            {/* Renders nothing when the mode is off. FIRST of the banners and not
          hidden in focus mode: it explains every refusal the other three
          might be mistaken for, and it is the only way back out. */}
            <MemberModeBanner active={meta.memberMode} noCredential={meta.memberModeNoCredential} />
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
                <span>
                  Control plane unreachable — showing the last data received.{" "}
                  {lastCheckedLabel(lastOkAt ?? null)}
                </span>
              </div>
            )}
            {/* B1 — the settled-but-unknown identity, beside the unreachable
          banner above and in the same treatment: a state the console is IN,
          stated where it cannot be missed. It replaces nothing the human could
          have acted on — with no nav and no landing redirect, this banner and
          its Retry are the page. Kept below the unreachable banner deliberately:
          a dead control plane is the better explanation of the two and should be
          read first. */}
            {identityUnknown && (
              <div
                role="status"
                className="relative z-50 flex shrink-0 items-center gap-2 border-b border-border bg-warning-subtle px-4 py-2 text-sm text-warning"
              >
                <AlertTriangle className="size-4 shrink-0" />
                <span>{SHELL.UNKNOWN_BODY}</span>
                <button
                  type="button"
                  onClick={retryIdentity}
                  className="font-medium underline underline-offset-2"
                >
                  {SHELL.UNKNOWN_ACTION}
                </button>
              </div>
            )}
            {/* The SSO session dies outright at its expiry, with no
          refresh — this is the warning that never existed, so it is not a
          silent 401 that wipes the console mid-work. Re-authenticating now
          (while the current session still works) replaces it before it dies. */}
            {/* F3-F11: three-state, so an already-past-expiry session doesn't
                read "expiring soon" forever. */}
            {!unreachable && sessionExpiry !== "none" && (
              <div
                role="status"
                className="relative z-50 flex shrink-0 items-center gap-2 border-b border-border bg-warning-subtle px-4 py-2 text-sm text-warning"
              >
                <AlertTriangle className="size-4 shrink-0" />
                <span>{SESSION_EXPIRY_COPY[sessionExpiry][0]}</span>
                <a
                  href="/auth/login"
                  className="font-medium underline underline-offset-2"
                >
                  Sign in again
                </a>
                <span>{SESSION_EXPIRY_COPY[sessionExpiry][1]}</span>
              </div>
            )}
            {/* LAST in the stack, and not hidden in focus mode: a dead control
          plane, an unknown identity and a dying session are each the better
          explanation of what you are looking at and are read first — but this
          one is the only band that carries its own repair, and 0.7.6's mid-run
          re-authentication needs exactly this surface on the cockpit. Renders
          nothing when there is nothing to say. */}
            {/* The live region is EAGER and the strip inside it is not: a
          `role="status"` region announces CHANGES to its content, so a region
          that arrives WITH its first sentence (as a lazy chunk does) announces
          nothing. The wrapper is here from the first paint; the chunk fills it. */}
            <div role="status">
              <React.Suspense fallback={null}>
                <ModelAccessBanner />
              </React.Suspense>
              {/* #162 — last in the stack (mock-approval ruling 3): the four
              bands above are each the better explanation of what you are
              looking at, or block the very thing a run needs to start, and
              this one has no per-person urgency. */}
              <React.Suspense fallback={null}>
                <ConfinementPostureBanner />
              </React.Suspense>
            </div>
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

              <main
                id="main-content"
                tabIndex={-1}
                className="scroll-thin min-w-0 flex-1 overflow-y-auto focus:outline-none"
              >
                {/* Keyed by pathname so navigating away from a screen that threw
              clears the caught error instead of wedging the console. */}
                <ErrorBoundary
                  key={location.pathname}
                  region={location.pathname}
                >
                  {/* B1 was a NAV gate only, which left every route reachable by
                URL: the account menu's Settings link and a bookmark or reload on
                /permissions, /governance, /policies, /secrets, /audit all painted
                the full operator screen off the fail-open identity seed
                (useOperator()/useSecurityOperator() answer true while /me is
                unknown, and RequireSetup answered an unknown identity with
                <Outlet/>). The gate belongs HERE, at the one shell every route
                renders under, rather than in the operator context: that context's
                defaults must keep failing open for the ordinary not-settled-yet
                paint, which is the rationale B1 recorded and this does not
                disturb. The banner above, with its Retry, is the page — as this
                file already said it was when there was no nav to reach. */}
                  {identityUnknown ? null : <Outlet />}
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

// The defaults every ordinary install reports (internal/identity/embedded's
// DefaultTrustDomain, and the identity registry's default component). A value
// equal to one of these carries no information, so the chrome stays quiet; the
// placeholder and error states ("…", "unknown") are quiet for the same reason —
// a chip that says "unknown" is worse than no chip.
const DEFAULT_TRUST_DOMAIN = "wardyn.local";
const DEFAULT_IDENTITY_PROVIDER = "embedded";

export function isCustomTrustDomain(v: string): boolean {
  return v !== "" && v !== "…" && v !== "unknown" && v !== DEFAULT_TRUST_DOMAIN;
}

export function isCustomIdentityProvider(v: string): boolean {
  return (
    v !== "" && v !== "…" && v !== "unknown" && v !== DEFAULT_IDENTITY_PROVIDER
  );
}

