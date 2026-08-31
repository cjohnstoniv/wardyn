/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import * as React from "react";
import { Loader2 } from "lucide-react";
import { Navigate, Route, Routes, useNavigate, Outlet } from "react-router-dom";
import { Toaster } from "./components/ui/sonner";
import { ThemeProvider } from "./components/wardyn/theme-provider";
import { SignIn } from "./components/screens/sign-in";
import { AppShell } from "./components/screens/app-shell";
import { RunsScreen } from "./components/screens/runs";
import { WardynMark } from "./components/wardyn/logo";
import { onUnauthorized, probeAuth, setToken } from "./lib/api/core";
import { health } from "./lib/api/health";
import { setup as setupApi } from "./lib/api/setup";
// From setup-gate, NOT setup-screen: the screen re-exports this, but importing it
// from there drags the whole funnel (→ integrations → harness-login → xterm) into
// the entry chunk and defeats the /setup route's code-splitting.
import {
  firstRunLanding,
  setupGateActive,
} from "./components/screens/setup/setup-gate";
import { useRole, useRoleResolved } from "./components/wardyn/operator-context";
import { approvals as approvalsApi } from "./lib/api/approvals";
import { runs as runsApi } from "./lib/api/runs";
import { usePoll } from "./lib/use-poll";
import type {
  AgentRun,
  ApprovalRequest,
  ConfinementClass,
  SetupStatus,
} from "./lib/types";
import {
  approvalSignals,
  needsAttention,
} from "./components/screens/runs/board-groups";

type AuthStatus = "checking" | "authed" | "unauthed";

// How often the reachability heartbeat beats, so the top bar's barrier chip and
// the "control plane unreachable" banner stay fresh. It hits /healthz, NOT
// /setup/status: the latter is a full ListRuns plus a host sweep that shells
// out (host-proxy detection, `git config` for SCM posture), and every authed
// tab used to run it every 5s forever. /healthz already carries the only two
// facts the shell needs — liveness and confinement_classes — so the expensive
// snapshot is now fetched exactly once per session, for the landing decision.
const HEALTH_POLL_MS = 5000;

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
  if (status === null || !roleResolved) return <RouteFallback />;
  return setupGateActive(status, role) ? (
    <Navigate to="/setup" replace />
  ) : (
    <Outlet />
  );
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

export default function App() {
  const [auth, setAuth] = React.useState<AuthStatus>("checking");
  const [pendingApprovals, setPendingApprovals] = React.useState(0);
  const [attentionCount, setAttentionCount] = React.useState(0);
  const navigate = useNavigate();

  // Both badges come off ONE tick, because the attention count is now a join:
  // a run is blocked when a held approval is parked on it, which lives in the
  // approvals list, not on the run. Fetching them apart would let the two
  // halves land a poll out of step and flash a wrong count. Each half fails
  // independently — a broken approvals call still leaves an honest run badge.
  // Counts, not lists, stay in state: this re-renders the whole shell, and the
  // number is the only thing it renders.
  const refreshBadges = React.useCallback(() => {
    Promise.all([
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
  React.useEffect(() => {
    let active = true;
    probeAuth().then((ok) => {
      if (active) setAuth(ok ? "authed" : "unauthed");
    });
    return () => {
      active = false;
    };
  }, []);

  // An expired session / revoked token (any HTTP 401) returns to the gate.
  React.useEffect(() => {
    onUnauthorized(() => setAuth("unauthed"));
  }, []);

  React.useEffect(() => {
    if (auth === "authed") refreshBadges();
  }, [auth, refreshBadges]);

  // Keep both nav badges live across the whole console, not just while the
  // operator is on the Runs/Approvals screen (a decision made in RunDetail must
  // still tick the pending badge down).
  usePoll(refreshBadges, ATTENTION_POLL_MS, auth !== "authed");

  // Setup status feeds the first-run landing decision ("/" → tour or Runs).
  // Fetched ONCE per session: it is the expensive endpoint, and nothing in the
  // shell needs it live. getSetupStatus never rejects except on 401 (routed
  // through onUnauthorized), and resolves the synthetic READY_FALLBACK rather
  // than rejecting when the daemon doesn't answer, so the landing decision
  // still gets made against a broken backend.
  const [setupStatus, setSetupStatus] = React.useState<SetupStatus | null>(
    null,
  );
  const refreshSetupStatus = React.useCallback(() => {
    setupApi
      .getSetupStatus()
      .then(setSetupStatus)
      .catch(() => {
        /* leave the last-known status in place — never trap behind a failed probe */
      });
  }, []);

  // The console's ONE reachability signal, and the barrier chip's source.
  // Every screen's background refresh swallows its own failures to keep the
  // last-good data on screen, which without this reads exactly like a healthy
  // quiet fleet (AppShell renders the banner). health() never rejects — it
  // resolves {} on a network error or any non-2xx — so a missing status:"ok"
  // IS the unreachable verdict. ponytail: a build whose /healthz doesn't answer
  // also reads as unreachable — the same daemon serves this console, so that
  // means a broken build. Classes are left at their last-known value while
  // unreachable rather than repainted from a payload we didn't get.
  const [unreachable, setUnreachable] = React.useState(false);
  const [lastOkAt, setLastOkAt] = React.useState<Date | null>(null);
  const [confinementClasses, setConfinementClasses] = React.useState<
    ConfinementClass[] | undefined
  >(undefined);
  const refreshHealth = React.useCallback(() => {
    void health.health().then((h) => {
      const ok = h.status === "ok";
      setUnreachable(!ok);
      if (!ok) return;
      setLastOkAt(new Date());
      setConfinementClasses(
        (h.confinement_classes ?? []) as ConfinementClass[],
      );
    });
  }, []);
  React.useEffect(() => {
    if (auth === "authed") {
      refreshSetupStatus();
      refreshHealth();
    }
  }, [auth, refreshSetupStatus, refreshHealth]);
  usePoll(refreshHealth, HEALTH_POLL_MS, auth !== "authed");

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
        <SignIn onSignIn={() => setAuth("authed")} />
        <Toaster />
      </ThemeProvider>
    );
  }

  return (
    <ThemeProvider>
      <Routes>
        <Route
          element={
            <AppShell
              pendingApprovals={pendingApprovals}
              attentionCount={attentionCount}
              unreachable={unreachable}
              lastOkAt={lastOkAt}
              confinementClasses={unreachable ? undefined : confinementClasses}
              onSignOut={async () => {
                // HIGH fix (sign-out): tell the server to clear the OIDC session
                // BEFORE dropping local state. Clearing only the local admin token
                // left the HttpOnly session cookie alive, so the next auth probe
                // silently re-signed us back in. logout() is best-effort and always
                // resolves, so we then drop the local token and return to the gate.
                await health.logout();
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
              <React.Suspense fallback={<RouteFallback />}>
                <GettingStarted onDone={() => navigate("/runs")} />
              </React.Suspense>
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
              now (Host · Model provider · Git host · Your SSH keys). The
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
      <Toaster />
    </ThemeProvider>
  );
}
