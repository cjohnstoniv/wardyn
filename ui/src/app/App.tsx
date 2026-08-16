/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import * as React from "react";
import { Loader2 } from "lucide-react";
import { Navigate, Route, Routes, useNavigate } from "react-router-dom";
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
import { firstRunLanding } from "./components/screens/setup/setup-gate";
import { approvals as approvalsApi } from "./lib/api/approvals";
import { runs as runsApi } from "./lib/api/runs";
import { usePoll } from "./lib/use-poll";
import type { AgentRun, SetupStatus } from "./lib/types";

type AuthStatus = "checking" | "authed" | "unauthed";

// How often the setup-status poll refreshes, so the top bar's barrier chip and
// the "control plane unreachable" banner stay fresh.
const SETUP_POLL_MS = 5000;

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
const IntegrationsScreen = React.lazy(() =>
  import("./components/screens/integrations/integrations-screen").then((m) => ({ default: m.IntegrationsScreen })),
);
const IntegrationDetailScreen = React.lazy(() =>
  import("./components/screens/integrations/integration-detail").then((m) => ({ default: m.IntegrationDetailScreen })),
);
const WorkspacesScreen = React.lazy(() =>
  import("./components/screens/workspaces").then((m) => ({ default: m.WorkspacesScreen })),
);
const WorkspaceDetailScreen = React.lazy(() =>
  import("./components/screens/workspace-detail/workspace-detail").then((m) => ({
    default: m.WorkspaceDetailScreen,
  })),
);
const AuditScreen = React.lazy(() => import("./components/screens/audit").then((m) => ({ default: m.AuditScreen })));
const RecordingScreen = React.lazy(() =>
  import("./components/screens/recording").then((m) => ({ default: m.RecordingScreen })),
);
const SSHKeysScreen = React.lazy(() =>
  import("./components/screens/ssh-keys").then((m) => ({ default: m.SSHKeysScreen })),
);
const DemoScreen = React.lazy(() =>
  import("./components/screens/demos/demo-screen").then((m) => ({ default: m.DemoScreen })),
);
// The guided Getting Started funnel — an operator-chosen route, not a gate:
// no redirect anywhere sends anyone here (see setup-gate.ts). Handles its own
// first-boot Welcome hero vs. the step funnel (onboarding-screen.tsx).
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
function FirstRunLanding({ status }: { status: SetupStatus | null }) {
  if (status === null) return <RouteFallback />;
  return <Navigate to={firstRunLanding(status)} replace />;
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

  // Setup status feeds the top bar's barrier chip (AppShell) and the "control
  // plane unreachable" banner below. getSetupStatus never rejects except on
  // 401 (routed through onUnauthorized), so a rejected probe just leaves the
  // last-known status in place.
  const [setupStatus, setSetupStatus] = React.useState<SetupStatus | null>(null);
  // When the daemon last actually answered. getSetupStatus resolves the synthetic
  // READY_FALLBACK (unreachable:true) rather than rejecting when it doesn't, so
  // this poll is also the console's ONE reachability signal — every screen's
  // background refresh swallows its own failures to keep the last-good data on
  // screen, which without this reads exactly like a healthy quiet fleet (AppShell
  // renders the banner). ponytail: a build whose /setup/status 404s also reads as
  // unreachable — the same daemon serves this console, so that means a broken build.
  const [lastOkAt, setLastOkAt] = React.useState<Date | null>(null);
  const refreshSetupStatus = React.useCallback(() => {
    setupApi
      .getSetupStatus()
      .then((s) => {
        setSetupStatus(s);
        if (!s.unreachable) setLastOkAt(new Date());
      })
      .catch(() => {
        /* leave the last-known status in place — never trap behind a failed probe */
      });
  }, []);
  React.useEffect(() => {
    if (auth === "authed") refreshSetupStatus();
  }, [auth, refreshSetupStatus]);
  usePoll(refreshSetupStatus, SETUP_POLL_MS, auth !== "authed");

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
              unreachable={!!setupStatus?.unreachable}
              lastOkAt={lastOkAt}
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
          <Route
            path="/demos"
            element={
              <React.Suspense fallback={<RouteFallback />}>
                <DemoScreen />
              </React.Suspense>
            }
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
          <Route path="/" element={<FirstRunLanding status={setupStatus} />} />
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
            path="/integrations"
            element={
              <React.Suspense fallback={<RouteFallback />}>
                <IntegrationsScreen />
              </React.Suspense>
            }
          />
          <Route
            path="/integrations/:id"
            element={
              <React.Suspense fallback={<RouteFallback />}>
                <IntegrationDetailScreen />
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
      </Routes>
      <Toaster />
    </ThemeProvider>
  );
}
