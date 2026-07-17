/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import * as React from "react";
import { Loader2 } from "lucide-react";
import { Navigate, Route, Routes, useLocation, useNavigate } from "react-router-dom";
import { Toaster } from "./components/ui/sonner";
import { ThemeProvider } from "./components/wardyn/theme-provider";
import { SignIn } from "./components/screens/sign-in";
import { AppShell } from "./components/screens/app-shell";
import { RunsScreen } from "./components/screens/runs";
// setup-gate holds only the pure auto-open decision, so the funnel itself (and
// the xterm stack it reaches through harness-login-pane) stays out of the entry
// chunk. Import these from the screen module and the split below is undone.
import { setupDismissed, shouldOpenSetup } from "./components/screens/setup/setup-gate";
import { WardynMark } from "./components/wardyn/logo";
import { onUnauthorized, probeAuth, setToken } from "./lib/api/core";
import { health } from "./lib/api/health";
import { setup as setupApi } from "./lib/api/setup";
import { approvals as approvalsApi } from "./lib/api/approvals";
import { runs as runsApi } from "./lib/api/runs";
import { usePoll } from "./lib/use-poll";
import type { AgentRun } from "./lib/types";

type AuthStatus = "checking" | "authed" | "unauthed";

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
  const location = useLocation();

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

  // Auto-open the first-run "Getting started" wizard once, right after auth
  // flips to "authed" — fire-and-forget (Runs renders first; it may flip to
  // Setup a beat later, which is an acceptable brief flash). getSetupStatus()
  // never rejects except on a 401 (routed through onUnauthorized already), so a
  // rejected promise here just means "leave the route alone".
  React.useEffect(() => {
    if (auth !== "authed") return;
    let active = true;
    setupApi
      .getSetupStatus()
      .then((status) => {
        if (active && location.pathname !== "/setup" && shouldOpenSetup(status, setupDismissed())) {
          navigate("/setup");
        }
      })
      .catch(() => {
        /* leave the route alone — never trap the operator behind a failed probe */
      });
    return () => {
      active = false;
    };
    // Only re-run when auth flips — this is a one-shot first-run check, not a
    // route-change listener (it must not re-fire on every navigation).
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [auth]);

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
          <Route
            path="/demos"
            element={
              <React.Suspense fallback={<RouteFallback />}>
                <DemoScreen />
              </React.Suspense>
            }
          />
          <Route
            path="/setup"
            element={
              <React.Suspense fallback={<RouteFallback />}>
                <GettingStarted onDone={() => navigate("/runs")} />
              </React.Suspense>
            }
          />
          {/* Fleet is retired — merged into Runs. Keep the old path working. */}
          <Route path="/fleet" element={<Navigate to="/runs" replace />} />
          <Route path="*" element={<Navigate to="/runs" replace />} />
        </Route>
      </Routes>
      <Toaster />
    </ThemeProvider>
  );
}
