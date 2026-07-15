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
import { RunDetailScreen } from "./components/screens/run-detail";
import { ApprovalsScreen } from "./components/screens/approvals";
import { PoliciesScreen } from "./components/screens/policies";
import { SecretsScreen } from "./components/screens/secrets";
import { WorkspacesScreen } from "./components/screens/workspaces";
import { AuditScreen } from "./components/screens/audit";
import { RecordingScreen } from "./components/screens/recording";
import { DemoScreen } from "./components/screens/demos/demo-screen";
import { setupDismissed, shouldOpenSetup } from "./components/screens/setup/setup-screen";
import { GettingStarted } from "./components/screens/onboarding/onboarding-screen";
import { WardynMark } from "./components/wardyn/logo";
import { api, onUnauthorized, probeAuth, setToken } from "./lib/api";
import { usePoll } from "./lib/use-poll";
import type { AgentRun } from "./lib/types";

type AuthStatus = "checking" | "authed" | "unauthed";

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
    api
      .listApprovals()
      .then((a) => setPendingApprovals(a.filter((x) => x.state === "PENDING").length))
      .catch(() => {
        /* listApprovals already routes 401 through onUnauthorized */
      });
  }, []);

  const refreshAttention = React.useCallback(() => {
    api
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
    api
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
                await api.logout();
                setToken(null);
                setAuth("unauthed");
              }}
            />
          }
        >
          <Route path="/" element={<Navigate to="/runs" replace />} />
          <Route path="/runs" element={<RunsScreen />} />
          <Route path="/runs/:id" element={<RunDetailScreen />} />
          <Route path="/approvals" element={<ApprovalsScreen onChanged={refreshPending} />} />
          <Route path="/policies" element={<PoliciesScreen />} />
          <Route path="/secrets" element={<SecretsScreen />} />
          <Route path="/workspaces" element={<WorkspacesScreen />} />
          <Route path="/audit" element={<AuditScreen />} />
          <Route path="/recordings" element={<RecordingScreen />} />
          <Route path="/demos" element={<DemoScreen />} />
          <Route path="/setup" element={<GettingStarted onDone={() => navigate("/runs")} />} />
          {/* Fleet is retired — merged into Runs. Keep the old path working. */}
          <Route path="/fleet" element={<Navigate to="/runs" replace />} />
          <Route path="*" element={<Navigate to="/runs" replace />} />
        </Route>
      </Routes>
      <Toaster />
    </ThemeProvider>
  );
}
