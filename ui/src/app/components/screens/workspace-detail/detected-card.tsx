/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// "Detected, not required" — everything the scan/observed-egress noticed that
// isn't in the contract yet: leak findings (pinned top, content-free, no
// promote action), suggested egress (from file content), observed-but-denied
// egress (auto-loaded here via GET /workspaces/{id}/observed-egress — unlike
// the older WorkspaceNeedsPanel's lazy "Check run history" button, this page
// fetches it on mount), and code/CI-only secret names. Each promotable entry
// gets Add as required / Add as optional. Host-shaped entries (suggested +
// observed-denied) go through the SAME untrusted-content confirm the rest of
// the app uses for egress approval (ConfirmEgressDialog) — approving widens
// what every future run mounting this workspace can reach. A code/CI secret
// NAME isn't egress, so it promotes directly (mirrors the approved mock's
// promoteAdvisory, which never gated it behind a confirm either) — only the
// host-shaped candidates warrant the "untrusted content, reachable by every
// future run" framing that dialog states.
import * as React from "react";
import { AlertTriangle } from "lucide-react";
import { toast } from "sonner";
import { Button } from "../../ui/button";
import { Mono } from "../../wardyn/code-block";
import { ConfirmEgressDialog } from "../../wardyn/confirm-egress-dialog";
import { getErrorMessage } from "../../../lib/format";
import { workspaces as workspacesApi } from "../../../lib/api/workspaces";
import type { RequirementLevel, WorkspaceRequirementsMap } from "../../../lib/api/workspaces";
import type { Workspace, WorkspaceProfile } from "../../../lib/types";
import { C } from "../../../lib/workspace-copy";
import { SectionCard } from "./section-card";

// Boundary-local typed cast-read — same idiom requirements-card.tsx uses at
// its own boundary (see that file's comment for why this isn't shared).
function requirementsOf(ws: Workspace): WorkspaceRequirementsMap {
  return (ws as unknown as { requirements?: WorkspaceRequirementsMap }).requirements ?? {};
}

export function DetectedCard({
  ws,
  onWorkspaceUpdated,
}: {
  ws: Workspace;
  onWorkspaceUpdated: (w: Workspace) => void;
}) {
  const profile = (ws.profile ?? {}) as WorkspaceProfile;

  // Auto-loaded on mount — the task's explicit departure from the older
  // needs-panel's lazy "Check run history" button.
  const [observed, setObserved] = React.useState<{ denied: string[]; runs_examined: number } | null>(null);
  React.useEffect(() => {
    let live = true;
    workspacesApi
      .getObservedEgress(ws.id)
      .then((r) => live && setObserved(r))
      .catch(() => live && setObserved({ denied: [], runs_examined: 0 }));
    return () => {
      live = false;
    };
  }, [ws.id]);

  const reqs = requirementsOf(ws);
  const contractHosts = new Set(
    Object.keys(reqs)
      .filter((k) => k.startsWith("egress:"))
      .map((k) => k.slice("egress:".length)),
  );
  const autoAllowed = new Set(profile.egress_domains ?? []);
  const approved = new Set(ws.approved_egress ?? []);
  const inContract = (h: string) => contractHosts.has(h) || autoAllowed.has(h);

  const suggested = (profile.suggested_egress ?? []).filter((h) => !inContract(h));
  const observedDenied = (observed?.denied ?? []).filter((h) => !inContract(h) && !approved.has(h));
  const codeRefs = (profile.required_secrets ?? []).filter(
    (s) => (s.kind === "code" || s.kind === "ci") && !(`secret:${s.name}` in reqs),
  );
  const leaks = profile.leak_findings ?? [];
  const nothingPending = leaks.length === 0 && suggested.length === 0 && observedDenied.length === 0 && codeRefs.length === 0;

  const [confirmHost, setConfirmHost] = React.useState<{ host: string; level: RequirementLevel } | null>(null);
  const [busy, setBusy] = React.useState<string | null>(null);

  const promote = async (key: string, level: RequirementLevel) => {
    setBusy(key);
    try {
      const next = { ...reqs, [key]: { level, provenance: "operator_set" as const } };
      onWorkspaceUpdated(await workspacesApi.setRequirements(ws.id, next));
    } catch (e) {
      toast.error("Failed to update requirements", { description: getErrorMessage(e) });
    } finally {
      setBusy(null);
    }
  };
  const promoteHost = (host: string, level: RequirementLevel) => void promote(`egress:${host}`, level);
  const promoteSecret = (name: string, level: RequirementLevel) => void promote(`secret:${name}`, level);

  return (
    <SectionCard
      title="Detected, not required"
      subtitle="Wardyn noticed these but hasn't given them to any run. Add one only if this workspace legitimately needs it."
    >
      {leaks.length > 0 && (
        <div className="space-y-2 rounded-lg border border-danger/40 bg-danger-subtle p-3" data-testid="detected-leaks">
          <div className="flex items-center gap-2 text-danger">
            <AlertTriangle className="size-4 shrink-0" />
            <span className="text-[0.8125rem] font-semibold">
              Suspected committed secrets — rotate or remove before mounting
            </span>
          </div>
          <div className="space-y-1">
            {leaks.map((lk, i) => (
              <Mono key={`${lk.path}:${lk.line ?? ""}:${i}`} className="block text-xs text-foreground">
                {lk.path}
                {lk.line != null ? `:${lk.line}` : ""} — {lk.kind}
              </Mono>
            ))}
          </div>
          <p className="text-[0.6875rem] leading-snug text-danger/90">{C.LOCATION_ONLY}</p>
        </div>
      )}

      {suggested.length > 0 && (
        <HostGroup
          testId="detected-suggested-egress"
          title="Suggested egress"
          hint="from file content"
          hosts={suggested}
          busyHost={busy?.startsWith("egress:") ? busy.slice("egress:".length) : null}
          onPromote={(h, l) => setConfirmHost({ host: h, level: l })}
        />
      )}

      {observedDenied.length > 0 && (
        <HostGroup
          testId="detected-observed-denied"
          title="Observed but denied"
          hint={`denied for a past run using this workspace${observed?.runs_examined ? ` · ${observed.runs_examined} examined` : ""}`}
          hosts={observedDenied}
          busyHost={busy?.startsWith("egress:") ? busy.slice("egress:".length) : null}
          onPromote={(h, l) => setConfirmHost({ host: h, level: l })}
        />
      )}

      {codeRefs.length > 0 && (
        <div className="space-y-2" data-testid="detected-code-refs">
          <h4 className="text-[0.8125rem] font-semibold text-foreground">Also read in code / CI</h4>
          <div className="divide-y divide-border rounded-lg border border-border">
            {codeRefs.map((s) => (
              <div key={s.name} className="flex flex-wrap items-center gap-2 p-2.5">
                <Mono className="text-xs text-foreground">{s.name}</Mono>
                <span className="text-[0.6875rem] text-muted-foreground">advisory — referenced in code, not declared</span>
                <span className="ml-auto" />
                <Button
                  size="sm"
                  variant="outline"
                  className="h-7"
                  disabled={busy === `secret:${s.name}`}
                  onClick={() => promoteSecret(s.name, "required")}
                >
                  Add as required
                </Button>
                <Button
                  size="sm"
                  variant="ghost"
                  className="h-7"
                  disabled={busy === `secret:${s.name}`}
                  onClick={() => promoteSecret(s.name, "optional")}
                >
                  Add as optional
                </Button>
              </div>
            ))}
          </div>
        </div>
      )}

      {nothingPending && (
        <p className="text-xs text-muted-foreground" data-testid="detected-empty">
          Nothing pending — every host and secret the scan found is either required, optional, or dismissed
          by you.
        </p>
      )}

      <ConfirmEgressDialog
        hosts={confirmHost ? [confirmHost.host] : null}
        onOpenChange={(o) => !o && setConfirmHost(null)}
        onConfirm={() => {
          if (confirmHost) promoteHost(confirmHost.host, confirmHost.level);
          setConfirmHost(null);
        }}
      />
    </SectionCard>
  );
}

function HostGroup({
  testId,
  title,
  hint,
  hosts,
  busyHost,
  onPromote,
}: {
  testId: string;
  title: string;
  hint: string;
  hosts: string[];
  busyHost: string | null;
  onPromote: (host: string, level: RequirementLevel) => void;
}) {
  return (
    <div className="space-y-2" data-testid={testId}>
      <h4 className="text-[0.8125rem] font-semibold text-foreground">{title}</h4>
      <div className="divide-y divide-border rounded-lg border border-border">
        {hosts.map((h) => (
          <div key={h} className="flex flex-wrap items-center gap-2 p-2.5">
            <div className="min-w-0">
              <Mono className="text-xs text-foreground">{h}</Mono>
              <p className="text-[0.6875rem] text-muted-foreground">{hint}</p>
            </div>
            <span className="ml-auto" />
            <Button size="sm" variant="outline" className="h-7" disabled={busyHost === h} onClick={() => onPromote(h, "required")}>
              Add as required
            </Button>
            <Button size="sm" variant="ghost" className="h-7" disabled={busyHost === h} onClick={() => onPromote(h, "optional")}>
              Add as optional
            </Button>
          </div>
        ))}
      </div>
    </div>
  );
}
