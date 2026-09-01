/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Card two on the workspace detail page — "Allowed hosts". Merges the
// workspace's operator-approved egress (approved_egress) with its REQUIRED
// egress: contract rows into one deduplicated list, each with honest
// provenance and a remove control — except the repo's own clone host, which
// is structural (the workspace can't function without reaching it) and
// carries neither.
import * as React from "react";
import { toast } from "sonner";
import { getErrorMessage } from "../../../lib/format";
import { workspaces as workspacesApi } from "../../../lib/api/workspaces";
import { effectiveWorkspaceRequirements, type Workspace } from "../../../lib/types";
import { DetailSectionCard } from "./section-card";
import { HostList, type HostRow } from "./host-list";
import { useSecurityOperator } from "../../wardyn/operator-context";

// Light, local parse — the same shape as the (retired) wizard's
// parseRepoSource, kept here as the one place that still needs it: the host a
// repo workspace clones from, so it can be marked structural rather than
// removable.
function cloneHostOf(ws: Workspace): string | null {
  if (ws.kind !== "repo" || !ws.source) return null;
  const s = ws.source.trim();
  const sshMatch = /^git@([^:]+):/.exec(s);
  if (sshMatch) return sshMatch[1].toLowerCase();
  const httpMatch = /^https?:\/\/([^/]+)\//.exec(s);
  if (httpMatch) return httpMatch[1].toLowerCase();
  if (/^[\w.-]+\/[\w.-]+$/.test(s)) return "github.com";
  return null;
}

function hostRows(ws: Workspace): HostRow[] {
  const clone = cloneHostOf(ws);
  const reqs = effectiveWorkspaceRequirements(ws);
  const approved = new Set(ws.approved_egress ?? []);
  const requiredHosts = new Set(
    Object.entries(reqs)
      .filter(([k, v]) => k.startsWith("egress:") && v.level === "required")
      .map(([k]) => k.slice("egress:".length)),
  );
  const all = new Set<string>([...approved, ...requiredHosts]);
  if (clone) all.add(clone);

  return Array.from(all)
    .sort()
    .map((host): HostRow => {
      if (host === clone) return { host, provenance: "clone host for this workspace", removable: false };
      // Session attribution: a recorded session that promoted its observed
      // egress AND actually reached this host — the honest, derivable half of
      // "promoted from session X" (there is no per-host provenance field on
      // the wire, so this is inferred from the session's own capture).
      const session = Object.entries(ws.record_results ?? {}).find(
        ([, rr]) => rr.egress_promoted && (rr.observations?.domains ?? []).some((d) => d.host === host && d.allow_count > 0),
      );
      if (session) {
        const [key, rr] = session;
        return { host, provenance: `promoted from session "${rr.label || key}"`, removable: true };
      }
      if (approved.has(host)) return { host, provenance: "approved for this workspace", removable: true };
      const req = reqs[`egress:${host}`];
      return {
        host,
        provenance: req?.provenance === "operator_set" ? "required by this workspace" : "detected when this workspace was scanned",
        removable: true,
      };
    });
}

export function AllowedHostsCard({ ws, onWorkspaceUpdated }: { ws: Workspace; onWorkspaceUpdated: (w: Workspace) => void }) {
  // useSecurityOperator, not useOperator (0.7 §B): PUT
  // /workspaces/{id}/approved-egress registers on securityOps (routes.go:363)
  // — deciding which hosts a workspace's runs may reach is a verdict, not a
  // deployer act. Every OTHER workspace write (llm-cred, requirements,
  // reassign, env-as-code, delete) stays on useOperator.
  const securityOperator = useSecurityOperator();
  const rows = hostRows(ws);
  const [removing, setRemoving] = React.useState<string | null>(null);

  const remove = async (host: string) => {
    setRemoving(host);
    try {
      let updated = await workspacesApi.setApprovedEgress(
        ws.id,
        (ws.approved_egress ?? []).filter((h) => h !== host),
      );
      const reqKey = `egress:${host}`;
      // Only an operator-authored requirement row can be removed this way — a
      // scan_seeded row lives on the source's own contract and rebuilds on
      // the next scan; a full-replace PUT here must never carry it back
      // (same rule wizard-types.ts's operatorOverlay used to enforce).
      if (updated.requirements?.[reqKey]?.provenance === "operator_set") {
        const nextReqs = { ...updated.requirements };
        delete nextReqs[reqKey];
        updated = await workspacesApi.setRequirements(ws.id, nextReqs);
      }
      onWorkspaceUpdated(updated);
    } catch (e) {
      toast.error(`Failed to remove ${host}`, { description: getErrorMessage(e) });
    } finally {
      setRemoving(null);
    }
  };

  return (
    <DetailSectionCard
      title={`Allowed hosts · ${rows.length}`}
      subtitle="Every run against this workspace may reach these. Nothing is here unless you approved it or a recording proved it was used."
    >
      <HostList rows={rows} emptyText="Nothing approved yet." canRemove={!!securityOperator} removing={removing} onRemove={(h) => void remove(h)} />
    </DetailSectionCard>
  );
}
