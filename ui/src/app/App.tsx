/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import * as React from "react";
import { Loader2 } from "lucide-react";
import { Navigate, Outlet, Route, Routes, useLocation, useNavigate } from "react-router-dom";
import { Toaster } from "./components/ui/sonner";
import { ThemeProvider } from "./components/wardyn/theme-provider";
import { SignIn } from "./components/screens/sign-in";
import { AppShell } from "./components/screens/app-shell";
import { RunsScreen } from "./components/screens/runs";
// setup-gate holds only the pure auto-open decision, so the funnel itself (and
// the xterm stack it reaches through harness-login-pane) stays out of the entry
// chunk. Import these from the screen module and the split below is undone.
import { setupGateActive } from "./components/screens/setup/setup-gate";
import { WardynMark } from "./components/wardyn/logo";
import { onUnauthorized, probeAuth, setToken } from "./lib/api/core";
import { health } from "./lib/api/health";
import { setup as setupApi } from "./lib/api/setup";
import { approvals as approvalsApi } from "./lib/api/approvals";
import { runs as runsApi } from "./lib/api/runs";
import { usePoll } from "./lib/use-poll";
import type { AgentRun, SetupStatus } from "./lib/types";

type AuthStatus = "checking" | "authed" | "unauthed";

// How often the first-run gate re-checks setup status, so nav unlocks promptly
// once the barrier comes up / the operator finishes (the dismiss flag is read
// live, so finishing unlocks on the next render regardless of this poll).
const SETUP_POLL_MS = 5000;

// Route guard for the mandatory first-run gate: while the gate is active, every
// app route redirects to /setup — only Getting started (/setup) and the demos
// that are part of it (/demos) stay reachable — so a fresh local operator goes
// through setup before the app opens. Nav groups are hidden in parallel
// (AppShell). Finishing the flow (dismissSetup), a first run, an onboarded
// console, SSO mode, or an unreachable daemon all clear the gate.
function RequireSetupComplete({ gated }: { gated: boolean }) {
  const loc = useLocation();
  if (gated && loc.pathname !== "/setup" && loc.pathname !== "/demos") {
    return <Navigate to="/setup" replace />;
  }
  return <Outlet />;
}

// Route-level code-splitting. Runs is the landing route (every "/" redirects
// there) so it stays eager — lazying it would only add a load waterfall to the
// first paint. Everything else is fetched on navigation, which keeps the heavy
// terminal deps out of the entry chunk: xterm rides on run-detail/demos/
// workspaces/setup, asciinema-player on run-detail/recordings. Rollup hoists
// what several lazy routes share into its own chunk automatically.
const RunDetailScreen = React.lazy(() =>
  import("./components/screens/run-detail").then((m) => ({ default: m.RunDetailScreen })),
);
const ApprovalsScreen = React.lazy(() =>
  import("./components/screens/approvals").then((m) => ({ default: m.ApprovalsScreen })),
);
const PoliciesScreen = React.lazy(() =>
  import("./components/screens/policies").then((m) => ({ default: m.PoliciesScreen })),
);
const SecretsScreen = React.lazy(() =>
  import("./components/screens/secrets").then((m) => ({ default: m.SecretsScreen })),
);
const WorkspacesScreen = React.lazy(() =>
  import("./components/screens/workspaces").then((m) => ({ default: m.WorkspacesScreen })),
);
const AuditScreen = React.lazy(() => import("./components/screens/audit").then((m) => ({ default: m.AuditScreen })));
const RecordingScreen = React.lazy(() =>
  import("./components/screens/recording").then((m) => ({ default: m.RecordingScreen })),
);
const DemoScreen = React.lazy(() =>
  import("./components/screens/demos/demo-screen").then((m) => ({ default: m.DemoScreen })),
);
const GettingStarted = React.lazy(() =>
  import("./components/screens/onboarding/onboarding-screen").then((m) => ({ default: m.GettingStarted })),
);

// Shown while a lazy route's chunk is in flight. Deliberately the same mark +
// spinner as the auth probe above, so a slow chunk reads as the console still
// connecting rather than as a broken screen.
function RouteFallback() {
  return (
    <div className="flex min-h-[60vh] flex-col items-center justify-center gap-3" role="status" aria-live="polite">
      <Loader2 className="size-5 animate-spin text-muted-foreground" />
      <span className="sr-only">Loading…</span>
    </div>
  );
}

// Run states that need an operator's attention — surfaced as the amber count
// badge on the Runs nav entry. FAILED needs eyes; WAITING_FOR_CONFIRMATION
// needs a click to unblock the agent.
const ATTENTION_STATES = new Set(["FAILED", "WAITING_FOR_CONFIRMATION"]);

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

  const refreshPending = React.useCallback(() => {
    approvalsApi
      .listApprovals()
      .then((a) => setPendingApprovals(a.filter((x) => x.state === "PENDING").length))
      .catch(() => {
        /* listApprovals already routes 401 through onUnauthorized */
      });
  }, []);

  const refreshAttention = React.useCallback(() => {
    runsApi
      .listRuns()
      .then((runs: AgentRun[]) => setAttentionCount(runs.filter((r) => ATTENTION_STATES.has(r.state as string)).length))
      .catch(() => {
        /* listRuns already routes 401 through onUnauthorized */
      });
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
    if (auth === "authed") {
      refreshPending();
      refreshAttention();
    }
  }, [auth, refreshPending, refreshAttention]);

  // Keep both nav badges live across the whole console, not just while the
  // operator is on the Runs/Approvals screen (a decision made in RunDetail must
  // still tick the pending badge down).
  const refreshBadges = React.useCallback(() => {
    refreshAttention();
    refreshPending();
  }, [refreshAttention, refreshPending]);
  usePoll(refreshBadges, ATTENTION_POLL_MS, auth !== "authed");

  // The mandatory first-run gate: fetch setup status (poll so it stays fresh as
  // the barrier comes up / setup finishes) and derive whether the app is gated.
  // Replaces the old soft auto-open with a hard redirect (RequireSetupComplete)
  // + hidden nav (AppShell). getSetupStatus never rejects except on 401 (routed
  // through onUnauthorized), so a rejected probe just leaves the gate as-is.
  const [setupStatus, setSetupStatus] = React.useState<SetupStatus | null>(null);
  const refreshSetupStatus = React.useCallback(() => {
    setupApi
      .getSetupStatus()
      .then(setSetupStatus)
      .catch(() => {
        /* leave the last-known status in place — never trap behind a failed probe */
      });
  }, []);
  React.useEffect(() => {
    if (auth === "authed") refreshSetupStatus();
  }, [auth, refreshSetupStatus]);
  usePoll(refreshSetupStatus, SETUP_POLL_MS, auth !== "authed");
  // setupGateActive reads the dismiss flag live, so finishing the funnel (or a
  // launched run) unlocks nav on the next render even before the poll refetches.
  const gated = setupStatus ? setupGateActive(setupStatus) : false;

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
              gated={gated}
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
          {/* Always reachable — Getting started and the demos that are part of it. */}
          <Route
            path="/setup"
            element={
              <React.Suspense fallback={<RouteFallback />}>
                <GettingStarted onDone={() => navigate("/runs")} />
              </React.Suspense>
            }
          />
          <Route
            path="/demos"
            element={
              <React.Suspense fallback={<RouteFallback />}>
                <DemoScreen />
              </React.Suspense>
            }
          />
          {/* Everything else waits behind the first-run gate (redirects to /setup
              while active; open once setup is finished). */}
          <Route element={<RequireSetupComplete gated={gated} />}>
            <Route path="/" element={<Navigate to="/runs" replace />} />
            <Route path="/runs" element={<RunsScreen />} />
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
                  <ApprovalsScreen onChanged={refreshPending} />
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
              path="/secrets"
              element={
                <React.Suspense fallback={<RouteFallback />}>
                  <SecretsScreen />
                </React.Suspense>
              }
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
            {/* Fleet is retired — merged into Runs. Keep the old path working. */}
            <Route path="/fleet" element={<Navigate to="/runs" replace />} />
            <Route path="*" element={<Navigate to="/runs" replace />} />
          </Route>
        </Route>
      </Routes>
      <Toaster />
    </ThemeProvider>
  );
}
