/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import * as React from "react";
import { Loader2 } from "lucide-react";
import { Navigate, Route, Routes, useLocation, useNavigate, Outlet } from "react-router-dom";
import { toast } from "sonner";
import { Toaster } from "./components/ui/sonner";
import { SHELL } from "./components/wardyn/copy";
import { ThemeProvider } from "./components/wardyn/theme-provider";
import { SignIn } from "./components/screens/sign-in";
import { AppShell } from "./components/screens/app-shell";
import { RunsScreen } from "./components/screens/runs";
import { WardynMark } from "./components/wardyn/logo";
import { onUnauthorized, probeAuth, safeReturnPath, setToken } from "./lib/api/core";
import { health } from "./lib/api/health";
import { setup as setupApi } from "./lib/api/setup";
// From setup-gate, NOT setup-screen: the screen re-exports this, but importing it
// from there drags the whole funnel (→ integrations → harness-login → xterm) into
// the entry chunk and defeats the /setup route's code-splitting.
import {
  firstRunLanding,
  gateAlreadyFired,
  markGateFired,
  setupGateActive,
} from "./components/screens/setup/setup-gate";
import { useOperatorResolved, useRole, useRoleResolved } from "./components/wardyn/operator-context";
import { approvals as approvalsApi } from "./lib/api/approvals";
import { runs as runsApi } from "./lib/api/runs";
import { usePoll } from "./lib/use-poll";
import { AttentionPublisherProvider, type AttentionCounts } from "./lib/attention-context";
import { ModelAccessProvider } from "./components/wardyn/model-access-context";
import type {
  AgentRun,
  ApprovalRequest,
  SetupStatus,
} from "./lib/types";
import {
  approvalSignals,
  needsAttention,
} from "./components/screens/runs/board-groups";

type AuthStatus = "checking" | "authed" | "unauthed";

// How often the reachability heartbeat beats, so the "control plane
// unreachable" banner stays fresh. It hits /healthz, NOT /setup/status: the
// latter is a full ListRuns plus a host sweep that shells out (host-proxy
// detection, `git config` for SCM posture), and every authed tab used to run
// it every 5s forever. /healthz already carries the only fact the shell
// needs — liveness — so the expensive snapshot is now fetched exactly once
// per session, for the landing decision.
// Exported (X2-F15): navigation.spec.ts derives its own wait from this exact
// constant instead of a second, independently-hardcoded copy of "5000" that
// could drift from it.
export const HEALTH_POLL_MS = 5000;

// Route-level code-splitting. Runs is the landing route (every "/" redirects
// there) so it stays eager — lazying it would only add a load waterfall to the
// first paint. Everything else is fetched on navigation, which keeps the heavy
// terminal deps out of the entry chunk: xterm rides on run-detail/workspaces/
// setup (the demo steps' own lazy chunk), asciinema-player on
// run-detail/recordings. Rollup hoists
// what several lazy routes share into its own chunk automatically.
const NewRunScreen = React.lazy(() =>
  import("./components/screens/new-run/new-run-screen").then((m) => ({
    default: m.NewRunScreen,
  })),
);
const RunDetailScreen = React.lazy(() =>
  import("./components/screens/run-detail").then((m) => ({
    default: m.RunDetailScreen,
  })),
);
const ApprovalsScreen = React.lazy(() =>
  import("./components/screens/approvals").then((m) => ({
    default: m.ApprovalsScreen,
  })),
);
const PoliciesScreen = React.lazy(() =>
  import("./components/screens/policies").then((m) => ({
    default: m.PoliciesScreen,
  })),
);
const GovernanceScreen = React.lazy(() =>
  import("./components/screens/governance/governance-screen").then((m) => ({
    default: m.GovernanceScreen,
  })),
);
// The admin's drive registry. No nav item and no member path: it is reached
// from the Workspaces header's outline button, the setup Workspaces step's card
// and the Settings card, each of which renders for an operator only.
const DrivesScreen = React.lazy(() =>
  import("./components/screens/drives/drives-screen").then((m) => ({
    default: m.DrivesScreen,
  })),
);
// The org's workspace-provider policy (0.7.2) — git hosts + storage ceilings.
// No nav item and no member path: reached from the funnel's `providers` step
// card and the Settings card, each of which renders for an operator only.
const ProvidersScreen = React.lazy(() =>
  import("./components/screens/providers/providers-screen").then((m) => ({
    default: m.ProvidersScreen,
  })),
);
const PermissionsScreen = React.lazy(() =>
  import("./components/screens/permissions").then((m) => ({
    default: m.PermissionsScreen,
  })),
);
const SecretsScreen = React.lazy(() =>
  import("./components/screens/secrets").then((m) => ({
    default: m.SecretsScreen,
  })),
);
const SettingsScreen = React.lazy(() =>
  import("./components/screens/settings/settings-screen").then((m) => ({
    default: m.SettingsScreen,
  })),
);
const WorkspacesScreen = React.lazy(() =>
  import("./components/screens/workspaces").then((m) => ({
    default: m.WorkspacesScreen,
  })),
);
const WorkspaceDetailScreen = React.lazy(() =>
  import("./components/screens/workspace-detail/workspace-detail").then(
    (m) => ({
      default: m.WorkspaceDetailScreen,
    }),
  ),
);
const AuditScreen = React.lazy(() =>
  import("./components/screens/audit").then((m) => ({
    default: m.AuditScreen,
  })),
);
const RecordingScreen = React.lazy(() =>
  import("./components/screens/recording").then((m) => ({
    default: m.RecordingScreen,
  })),
);
const SSHKeysScreen = React.lazy(() =>
  import("./components/screens/ssh-keys").then((m) => ({
    default: m.SSHKeysScreen,
  })),
);
// The guided Getting Started funnel — an operator-chosen route, not a gate:
// no redirect anywhere sends anyone here (see setup-gate.ts). Handles its own
// first-boot Welcome hero vs. the step funnel (onboarding-screen.tsx).
const GettingStarted = React.lazy(() =>
  import("./components/screens/onboarding/onboarding-screen").then((m) => ({
    default: m.GettingStarted,
  })),
);

// Shown while a lazy route's chunk is in flight. Deliberately the same mark +
// spinner as the auth probe above, so a slow chunk reads as the console still
// connecting rather than as a broken screen.
function RouteFallback() {
  return (
    <div
      className="flex min-h-[60vh] flex-col items-center justify-center gap-3"
      role="status"
      aria-live="polite"
    >
      <Loader2 className="size-5 animate-spin text-muted-foreground" />
      <span className="sr-only">Loading…</span>
    </div>
  );
}

// `/setup` is a member's landing route too (setup-gate.ts's setupGateActive
// redirects a gated install here regardless of role) — reached on a COLD
// document load (bookmark, reload, the SSO callback's return) before the real
// role is known. Held here until roleResolved, the same idiom FirstRunLanding
// above uses to hold `/` open until the real role answers.
//
// Defence in depth, not the only thing standing between a cold load and the
// admin funnel: the shell already paints NO route at all while identity is
// settled-but-unknown (app-shell.tsx's `identityUnknown` arm, pinned by
// app-shell-identity-routes.test.tsx) — a FAILED `/me` never reaches here in
// the first place. This gate is what closes the remaining window: `/me`
// still IN FLIGHT, where `role` reads its fail-open "admin" default and the
// shell has not yet decided to paint nothing.
function SetupRoute({
  status,
  onDone,
}: {
  status: SetupStatus | null;
  onDone: () => void;
}) {
  const roleResolved = useRoleResolved();
  // Warms the funnel's lazy chunk while the gate above holds, so the /me
  // round trip and the dynamic import overlap instead of serializing — the
  // gate would otherwise add its wait IN FRONT OF the chunk fetch that used
  // to start immediately (both states paint the same RouteFallback, so nothing
  // here is visible either way).
  React.useEffect(() => {
    void import("./components/screens/onboarding/onboarding-screen");
  }, []);
  if (!roleResolved) return <RouteFallback />;
  return (
    <React.Suspense fallback={<RouteFallback />}>
      <GettingStarted onDone={onDone} status={status} />
    </React.Suspense>
  );
}

// Where "/" lands. A fresh install opens on the guided tour rather than an
// empty Runs board — a console whose very first screen is "No runs yet" makes
// the operator hunt for where to begin.
//
// This is NOT the old first-run gate: it redirects "/" only. Every other route
// stays directly reachable, the nav never hides, and nothing bounces you back
// into the funnel. `has_runs` is the server's own signal, so it resets with
// `make reset-all` exactly like the install it describes; `setupDismissed()` is
// the per-browser "I've done this" flag one finish-or-skip sets for good.
//
// An unreachable daemon lands on Runs: getSetupStatus resolves the synthetic
// READY_FALLBACK (has_runs:false) rather than rejecting, which would otherwise
// send a broken backend into the tour instead of showing AppShell's banner.
// Waits for the first /setup/status before deciding — redirecting on the null
// initial state would always pick Runs and the tour would never open.
//
// Phase 5: also waits for the REAL role (useRoleResolved, operator-context.tsx)
// — a member landed here before /me answers would otherwise be redirected
// under the role context's fail-open "admin" default, which reads has_runs
// against the wrong rule. Hooks are called unconditionally, before either
// early return, per the rules of hooks.
export function FirstRunLanding({ status }: { status: SetupStatus | null }) {
  const role = useRole();
  const roleResolved = useRoleResolved();
  // B1: "settled" is not "answered". roleResolved flips on a FAILED /me too (it
  // is the stop-spinning signal and must stay that), which left this gate
  // picking a landing out of the fail-open "admin" default for a human the
  // server may have refused. useOperatorResolved is the signal that says /me
  // actually answered — when it did not, redirect NOWHERE: the shell's
  // identity-unknown banner (app-shell.tsx) is the page, instead of a guess.
  const identityResolved = useOperatorResolved();
  if (roleResolved && !identityResolved) return null;
  if (status === null || !roleResolved) return <RouteFallback />;
  return <Navigate to={firstRunLanding(status, role)} replace />;
}

// The hard gate: while the daemon grades any setup check `fail` or `warn`,
// every route below redirects into the funnel. `/setup` and `/demos` sit
// OUTSIDE this wrapper, so the way to satisfy the gate is always reachable and
// this can never trap anyone (the funnel configures environment, network and
// secrets in place). Waits for the first /setup/status and the real role before
// deciding, exactly as FirstRunLanding does — redirecting on the null initial
// state would bounce every load, and the role context fails open to "admin",
// which would gate a member on checks their console cannot even see.
//
// Not the 0.5 gate this file's header warns about: that one demanded the funnel
// be FINISHED. This asks only that the install works, and `info` checks — the
// optional ones, model provider included — never hold it.
function RequireSetup({ status }: { status: SetupStatus | null }) {
  const role = useRole();
  const roleResolved = useRoleResolved();
  // B1, same reasoning as FirstRunLanding above: setupGateActive() reads the
  // role, so a /me that never answered would bounce an unknown human into the
  // ADMIN funnel on the fail-open default. Decline to gate instead — the route
  // below still enforces itself server-side, and the shell says why the console
  // looks empty.
  //
  // This <Outlet/> is NOT the hole V1-D3 closed: AppShell renders no route at all
  // while the identity is settled-but-unknown, so nothing downstream of here
  // paints. Declining to gate stays right for the reason above — bouncing an
  // unknown human into the ADMIN funnel would be a worse lie than showing them
  // the banner — and the shell, not this wrapper, is where "every route" is one
  // place.
  const identityResolved = useOperatorResolved();
  if (roleResolved && !identityResolved) return <Outlet />;
  if (status === null || !roleResolved) return <RouteFallback />;
  // Once per load: an access lands a gated install in the funnel; navigation
  // OUT of the funnel afterwards is informed wandering, not a gate escape —
  // the failing checks stay visible on every surface, and the funnel's own
  // affordances (People's "Open Permissions" et al.) must be able to leave.
  // See setup-gate.ts's gateFiredThisLoad for why this is module state.
  if (!gateAlreadyFired() && setupGateActive(status, role)) {
    markGateFired();
    return <Navigate to="/setup" replace />;
  }
  return <Outlet />;
}

// What needs an operator's attention — surfaced as the amber count badge on the
// Runs nav entry — is `needsAttention` in screens/runs/board-groups, the SAME
// predicate the board itself renders. This was a second hand-copied Set of run
// states here, which meant the badge and the board could disagree about the
// same run: neither knew about a held approval, which parks the sandbox while
// the run state stays RUNNING, so a run the board could show as blocked never
// reached the badge at all.

// The Runs attention badge and the Approvals pending badge are background
// signals visible from every screen, so both are polled — approvals can now be
// decided from RunDetail too, not only the Approvals screen, so onChanged alone
// would leave the badge stale.
const ATTENTION_POLL_MS = 5000;

// How often the model-access door re-reads the status it rides on. FIVE
// MINUTES — 12 requests an hour per tab, a sixtieth of either poller above —
// because the thing it watches moves on the scale of an SSO session, not a run:
// /setup/status is the expensive endpoint (a runner Capabilities call, a CLI
// sweep, a secret listing, a platform/SCM detect, a site-config read and an AWS
// SSO blob decrypt), and this is the poll that made it periodic at all.
//
// usePoll is what makes the cadence honest: a hidden tab skips its ticks
// entirely and returning to the tab fires one immediately, which closes the
// "left open for hours" hole without asking the daemon for a faster cadence.
//
// A named module constant on purpose: a field report moves one number here.
const MODEL_ACCESS_POLL_MS = 300_000;

// M2: can THIS role reach a captured return path? Scoped to the one place a
// wrong answer is a dead end the plan named (restoring a mid-session-401
// path after re-auth) — NOT a general client-side route guard (nav-hiding
// elsewhere is deliberately cosmetic; the server is the real gate). Mirrors
// this file's own <Route> tree tiers below: a member's REACHABLE surface is
// wider than their NAV set — Runs/Approvals/Workspaces PLUS the three
// self-service routes with no sidebar entry at all (/secrets: WRITE/DELETE
// are self-service since migration 0050, routes.go; /settings and
// /ssh-keys: the account menu renders both for every role,
// app-shell.tsx:820-831). /drives and /providers are the two SUPER-only
// routes with no nav entry for anyone, gated operatorOnly server-side —
// restorable only for an actual admin, never a security admin either.
const MEMBER_REACHABLE_PREFIXES = ["/runs", "/approvals", "/workspaces", "/secrets", "/settings", "/ssh-keys"];
const OPERATOR_ONLY_PREFIXES = ["/drives", "/providers"];
export function roleCanReach(path: string, role: string): boolean {
  const under = (prefixes: string[]) =>
    prefixes.some((p) => path === p || path.startsWith(`${p}/`));
  if (role === "member") return under(MEMBER_REACHABLE_PREFIXES);
  if (under(OPERATOR_ONLY_PREFIXES)) return role === "admin";
  return true;
}

export default function App() {
  const [auth, setAuth] = React.useState<AuthStatus>("checking");
  const [pendingApprovals, setPendingApprovals] = React.useState(0);
  const [attentionCount, setAttentionCount] = React.useState(0);
  const navigate = useNavigate();
  // X3-F7: why the gate reopened (rendered in SignIn's own alert slot) and
  // where to return once re-authenticated. The path is captured by wfetch
  // itself (lib/api/core.ts), not read here — by the time this component
  // could ask, the routed tree the SignIn branch replaces (rendered OUTSIDE
  // <Routes> below) is already gone.
  const [authReason, setAuthReason] = React.useState<string | undefined>();
  const returnPathRef = React.useRef<string | null>(null);
  // H1: onUnauthorized fires for EVERY 401, including the cold mount probe
  // (no session at all yet) — mirrored in a ref (not read from `auth` state
  // directly) because the handler below is registered once, in a mount
  // effect with an empty dep array, and closing over `auth` there would
  // freeze it at "checking" forever.
  const authRef = React.useRef<AuthStatus>(auth);
  React.useEffect(() => {
    authRef.current = auth;
  }, [auth]);
  const location = useLocation();

  // Both badges come off ONE tick, because the attention count is now a join:
  // a run is blocked when a held approval is parked on it, which lives in the
  // approvals list, not on the run. Fetching them apart would let the two
  // halves land a poll out of step and flash a wrong count. Each half fails
  // independently — a broken approvals call still leaves an honest run badge.
  // Counts, not lists, stay in state: this re-renders the whole shell, and the
  // number is the only thing it renders.
  // RETURNED for usePoll's in-flight guard (R4-F074): the badge fetch is two
  // un-scoped LIST_LIMIT reads, the most expensive tick in the shell.
  const refreshBadges = React.useCallback(() => {
    return Promise.all([
      approvalsApi.listApprovals("PENDING").catch(() => null),
      runsApi.listRuns().catch(() => null),
      /* both already route 401 through onUnauthorized */
    ]).then(
      ([approvals, runs]: [ApprovalRequest[] | null, AgentRun[] | null]) => {
        const pending = approvals?.filter((a) => a.state === "PENDING") ?? [];
        if (approvals) setPendingApprovals(pending.length);
        if (runs) {
          const signals = approvalSignals(pending);
          setAttentionCount(
            runs.filter((r) => needsAttention(r, signals)).length,
          );
        }
      },
    );
  }, []);

  // Probe auth on mount: a live OIDC session cookie or a stored admin token
  // lets us straight into the console; otherwise show the sign-in gate.
  //
  // R4/F027: the probe answers THREE ways (core.ts's AuthProbe), not two. Only
  // a real 401 means "signed out". "unreachable" — a daemon 5xx or a dead
  // network — still lands on the gate (the console has no verified session to
  // show), but it must not ALSO leave the shell claiming nothing is wrong: it
  // records the outage on the same `unreachable` flag the banner and the
  // barrier chip already read, and the health poll below now runs at the gate
  // so that flag clears itself the moment the daemon comes back.
  React.useEffect(() => {
    let active = true;
    void probeAuth().then((probe) => {
      if (!active) return;
      if (probe === "unreachable") setUnreachable(true);
      setAuth(probe === "authed" ? "authed" : "unauthed");
    });
    return () => {
      active = false;
    };
  }, []);

  // An expired session / revoked token (any HTTP 401) returns to the gate —
  // X3-F7: carrying WHY (into SignIn's alert slot) and WHERE FROM (restored
  // after re-auth below), so a mid-session expiry stops reading as a silent
  // teleport back to the gate with everything unexplained and unrecoverable.
  React.useEffect(() => {
    onUnauthorized((reason, path) => {
      // H1: this fires for EVERY 401, including the cold mount probe above
      // (no session ever established this tab) — a reason/return-path only
      // means something for a session that WAS authed and just got cut off.
      if (authRef.current === "authed") {
        returnPathRef.current = path;
        setAuthReason(reason);
      }
      setAuth("unauthed");
    });
  }, []);

  React.useEffect(() => {
    if (auth === "authed") refreshBadges();
  }, [auth, refreshBadges]);

  // Keep both nav badges live across the whole console, not just while the
  // operator is on the Runs/Approvals screen (a decision made in RunDetail must
  // still tick the pending badge down).
  // X3-F13: EXCEPT on /runs itself — the board already runs its own listRuns +
  // listApprovals poll (runs.tsx, 3s) on the same two facts, so this tick (the
  // most expensive one in the shell — two unscoped LIST_LIMIT reads, see
  // refreshBadges above) would be pure duplication while parked there. R-1:
  // that only holds because the board PUBLISHES its counts back up through
  // publishAttention below — pausing this tick with nothing feeding the
  // badges from the other side would freeze both of them for as long as the
  // operator sat on /runs.
  usePoll(refreshBadges, ATTENTION_POLL_MS, auth !== "authed" || location.pathname === "/runs");
  // R-1: the setter side of the publish — RunsScreen calls this (via
  // usePublishAttention) every time its own fetch resolves, driving the SAME
  // state the paused poll above would have updated. Stable identity so it is
  // never itself a reason for the board to re-fetch.
  const publishAttention = React.useCallback(({ pendingApprovals: p, attentionCount: a }: AttentionCounts) => {
    setPendingApprovals(p);
    setAttentionCount(a);
  }, []);

  // Setup status feeds the first-run landing decision ("/" → tour or Runs).
  // Fetched ONCE per session: it is the expensive endpoint, and nothing in the
  // shell needs it live. getSetupStatus never rejects except on 401 (routed
  // through onUnauthorized), and resolves the synthetic READY_FALLBACK rather
  // than rejecting when the daemon doesn't answer, so the landing decision
  // still gets made against a broken backend.
  const [setupStatus, setSetupStatus] = React.useState<SetupStatus | null>(
    null,
  );
  // RETURNED, like refreshHealth below (Codex #12): usePoll's in-flight guard is
  // promise-based, so a void return would let a slow /setup/status — the
  // expensive endpoint — stack a second read on top of the first every tick.
  const refreshSetupStatus = React.useCallback(() => {
    return setupApi
      .getSetupStatus()
      .then(setSetupStatus)
      .catch(() => {
        /* leave the last-known status in place — never trap behind a failed probe */
      });
  }, []);

  // The console's ONE reachability signal. Every screen's background refresh
  // swallows its own failures to keep the last-good data on screen, which
  // without this reads exactly like a healthy quiet fleet (AppShell renders
  // the banner). health() never rejects — it resolves {} on a network error
  // or any non-2xx — so a missing status:"ok" IS the unreachable verdict.
  // ponytail: a build whose /healthz doesn't answer also reads as unreachable
  // — the same daemon serves this console, so that means a broken build.
  const [unreachable, setUnreachable] = React.useState(false);
  const [lastOkAt, setLastOkAt] = React.useState<Date | null>(null);
  // R4/F066: BOTH probes, because /healthz alone cannot see the outage this
  // banner exists for. handleHealthz writes `"status": "ok"` as a literal and
  // never touches the store (internal/api/healthz.go) — deliberately, since
  // liveness must not restart a pod over a Postgres failover — so a DB-down
  // console read as a healthy quiet fleet: banner down, every screen's silent
  // `.catch` holding last-good data on screen, nothing anywhere saying why
  // nothing changes. /readyz is the probe that already Pings the store and
  // answers 503 for it. Unreachable is therefore "not live OR not ready".
  //
  // The two are asked TOGETHER (one Promise.all, one tick of the same poll):
  // sequenced, a slow store ping would delay the liveness answer it is supposed
  // to be independent of.
  const refreshHealth = React.useCallback(() => {
    // RETURNED for usePoll's in-flight guard (R4-F074): a slow probe must not
    // stack a second one on top of it.
    return Promise.all([health.health(), health.readyz()]).then(([h, r]) => {
      const live = h.status === "ok";
      const ready = r.status === "ok";
      setUnreachable(!live || !ready);
      // The daemon's own facts still land whenever IT answered — a store outage
      // does not make the trust boundary or the class list stale.
      if (!live) return;
      // …but "last data received" must not tick forward while the store is
      // down: no data IS being received, which is the whole claim of the
      // sentence this timestamp completes.
      if (ready) setLastOkAt(new Date());
    });
  }, []);
  React.useEffect(() => {
    if (auth === "authed") void refreshSetupStatus();
  }, [auth, refreshSetupStatus]);
  // …and again every five minutes, because model_access is a per-person
  // credential LIFECYCLE: read once per session, a member who signed in at 09:00
  // is told at 09:00 and never again, and the strip below would be as stale as
  // the tab is old. Paused while unauthenticated — the endpoint 401s, and the
  // landing read above is what re-arms it.
  usePoll(refreshSetupStatus, MODEL_ACCESS_POLL_MS, auth !== "authed");
  // R4/F027: reachability is NOT gated on being signed in. /healthz is the one
  // unauthenticated endpoint the console has, and the state where it matters
  // most is the one this used to skip — an outage that sent the human to the
  // gate. Polled from mount so the verdict is live in every auth state, and so
  // an outage recorded by the mount probe above clears on its own.
  React.useEffect(() => {
    void refreshHealth();
  }, [refreshHealth]);
  usePoll(refreshHealth, HEALTH_POLL_MS, false);

  if (auth === "checking") {
    return (
      <ThemeProvider>
        <div className="flex min-h-screen flex-col items-center justify-center gap-4 bg-background">
          <WardynMark className="size-10" />
          <div className="flex items-center gap-2 text-sm text-muted-foreground">
            <Loader2 className="size-4 animate-spin" />
            Connecting to Wardyn…
          </div>
        </div>
        <Toaster />
      </ThemeProvider>
    );
  }

  if (auth !== "authed") {
    return (
      <ThemeProvider>
        <SignIn
          reason={authReason}
          onSignIn={async () => {
            // X3-F7/H2: restore the path the 401 interrupted, same-origin
            // pathname only (safeReturnPath) — root/setup are landing
            // decisions, not "somewhere to return to", so those (and "no
            // path captured", the ordinary mount-probe gate) fall back to
            // Runs like every other finished flow.
            const path = safeReturnPath(returnPathRef.current);
            returnPathRef.current = null;
            // M2: nothing to check for the Runs fallback itself — every role
            // reaches it. A real captured path might belong to the caller
            // who was signed in BEFORE (an admin's /drives), not whoever
            // just signed back in on this tab — a member landing there
            // would hit a bare 403 instead of the plan's stated /runs
            // fallback, so ask who signed in before trusting it.
            //
            // L4: resolved BEFORE flipping auth, not after — the routed tree
            // only mounts once auth is "authed", so awaiting here first
            // (rather than between setAuth and navigate) means it never
            // mounts for one commit at the pre-401 URL, firing an
            // operator-only screen's own GET (and a 403 audit row) a beat
            // before the bounce.
            const me = path === "/runs" ? null : await health.whoami().catch(() => null);
            const target = me && !roleCanReach(path, me.role) ? "/runs" : path;
            setAuthReason(undefined);
            setAuth("authed");
            navigate(target, { replace: true });
          }}
        />
        <Toaster />
      </ThemeProvider>
    );
  }

  return (
    <ThemeProvider>
      <AttentionPublisherProvider value={publishAttention}>
      {/* The door: one model-access answer and one sign-in dialog for the strip
          in the shell, the New Run rail, a credential-failed run's failure
          block and a held run's approval row — none of which can be reached by
          prop-drilling through screens that are at the file-size gate. */}
      <ModelAccessProvider status={setupStatus} onRefresh={refreshSetupStatus}>
      <Routes>
        <Route
          element={
            <AppShell
              pendingApprovals={pendingApprovals}
              attentionCount={attentionCount}
              unreachable={unreachable}
              lastOkAt={lastOkAt}
              onSignOut={async () => {
                // HIGH fix (sign-out): tell the server to clear the OIDC session
                // BEFORE dropping local state. Clearing only the local admin token
                // left the HttpOnly session cookie alive, so the next auth probe
                // silently re-signed us back in. logout() is best-effort and always
                // resolves, so we then drop the local token and return to the gate.
                //
                // R4-F107: …and SAY SO when the server did not confirm it. The
                // local token is gone regardless (this tab is signed out), but
                // the OIDC session cookie may still be live, so a reload
                // re-enters the console — which, on a shared machine, is the
                // one thing the button exists to prevent. The toast outlives
                // the branch switch below: sonner's store is a module
                // singleton and the sign-in gate mounts its own <Toaster />.
                if (!(await health.logout())) {
                  toast.error(SHELL.SIGN_OUT_FAILED_TITLE, {
                    description: SHELL.SIGN_OUT_FAILED_BODY,
                  });
                }
                setToken(null);
                setAuth("unauthed");
              }}
            />
          }
        >
          {/* /demos is gone — Getting Started IS the demos surface now, one
              step per demo, so a second page listing the same catalog would be
              a page inside a page. Same redirect precedent /integrations
              carries below: bookmarks, the account menu and the Runs empty
              state all pointed here. `sealed-box` is the first demo step. */}
          <Route
            path="/demos"
            element={<Navigate to="/setup?step=sealed-box" replace />}
          />
          {/* Reachable four ways, none of them a gate: a fresh install's "/"
              (FirstRunLanding), the account menu's "Getting started" entry, the
              Runs empty state's guided-tour link, or the URL. onDone lands back
              on Runs, same as every other finished flow. */}
          <Route
            path="/setup"
            element={
              <SetupRoute
                onDone={() => navigate("/runs")}
                status={setupStatus}
              />
            }
          />
          <Route element={<RequireSetup status={setupStatus} />}>
            <Route
              path="/"
              element={<FirstRunLanding status={setupStatus} />}
            />
            <Route path="/runs" element={<RunsScreen />} />
            {/* Ahead of /runs/:id so "new" is never read as a run id. */}
            <Route
              path="/runs/new"
              element={
                <React.Suspense fallback={<RouteFallback />}>
                  <NewRunScreen />
                </React.Suspense>
              }
            />
            <Route
              path="/runs/:id"
              element={
                <React.Suspense fallback={<RouteFallback />}>
                  <RunDetailScreen />
                </React.Suspense>
              }
            />
            <Route
              path="/approvals"
              element={
                <React.Suspense fallback={<RouteFallback />}>
                  <ApprovalsScreen onChanged={refreshBadges} />
                </React.Suspense>
              }
            />
            <Route
              path="/policies"
              element={
                <React.Suspense fallback={<RouteFallback />}>
                  <PoliciesScreen />
                </React.Suspense>
              }
            />
            {/* Between /policies and /permissions, the order the sidebar
                reads (mock Q1). Gated server-side by the securityOps route
                group; the screen itself gates its writes on
                useSecurityOperator, and a member never sees the nav item. */}
            <Route
              path="/governance"
              element={
                <React.Suspense fallback={<RouteFallback />}>
                  <GovernanceScreen />
                </React.Suspense>
              }
            />
            {/* SUPER, gated server-side by the operatorOnly route group; the
                screen itself gates its writes on useOperator, and no nav entry
                or entry point exists for a member or a security admin. */}
            <Route
              path="/drives"
              element={
                <React.Suspense fallback={<RouteFallback />}>
                  <DrivesScreen />
                </React.Suspense>
              }
            />
            {/* SUPER, gated server-side by the operatorOnly route group (both
                GET/PUT /workspace-providers); the screen itself gates its
                writes on useOperator, and no nav entry or entry point exists
                for a member or a security admin (their door is two numeric
                rows on /governance instead). */}
            <Route
              path="/providers"
              element={
                <React.Suspense fallback={<RouteFallback />}>
                  <ProvidersScreen />
                </React.Suspense>
              }
            />
            <Route
              path="/permissions"
              element={
                <React.Suspense fallback={<RouteFallback />}>
                  <PermissionsScreen />
                </React.Suspense>
              }
            />
            <Route
              path="/secrets"
              element={
                <React.Suspense fallback={<RouteFallback />}>
                  <SecretsScreen />
                </React.Suspense>
              }
            />
            {/* /integrations is gone — Settings is the one home for connections
              now (Host · Model provider · Providers · Your SSH keys). The
              redirect is kept because the barrier chip, the old account menu
              and any operator bookmark pointed here. */}
            <Route
              path="/integrations"
              element={<Navigate to="/settings" replace />}
            />
            <Route
              path="/settings"
              element={
                <React.Suspense fallback={<RouteFallback />}>
                  <SettingsScreen />
                </React.Suspense>
              }
            />
            <Route
              path="/integrations/:id"
              element={<Navigate to="/settings" replace />}
            />
            <Route
              path="/workspaces"
              element={
                <React.Suspense fallback={<RouteFallback />}>
                  <WorkspacesScreen />
                </React.Suspense>
              }
            />
            <Route
              path="/workspaces/:id"
              element={
                <React.Suspense fallback={<RouteFallback />}>
                  <WorkspaceDetailScreen />
                </React.Suspense>
              }
            />
            <Route
              path="/audit"
              element={
                <React.Suspense fallback={<RouteFallback />}>
                  <AuditScreen />
                </React.Suspense>
              }
            />
            <Route
              path="/recordings"
              element={
                <React.Suspense fallback={<RouteFallback />}>
                  <RecordingScreen />
                </React.Suspense>
              }
            />
            <Route
              path="/ssh-keys"
              element={
                <React.Suspense fallback={<RouteFallback />}>
                  <SSHKeysScreen />
                </React.Suspense>
              }
            />
            <Route path="*" element={<Navigate to="/runs" replace />} />
          </Route>
        </Route>
      </Routes>
      </ModelAccessProvider>
      </AttentionPublisherProvider>
      <Toaster />
    </ThemeProvider>
  );
}
