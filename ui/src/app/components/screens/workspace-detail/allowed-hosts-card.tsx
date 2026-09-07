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
import { useOperator, useSecurityOperator } from "../../wardyn/operator-context";
import { OPERATOR_ONLY_REASON } from "../../wardyn/copy";

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

// `canClearRequired` is the SECOND tier this card needs. Removing a host is
// one write (PUT .../approved-egress, securityOps — routes.go's securityOps
// block) UNLESS the host also carries an operator-authored requirements row,
// in which case it is TWO writes and the second (PUT .../requirements) is
// registered on operatorOnly. A security admin holds the first tier and not
// the second, so those rows are marked blocked here rather than offered as a
// live button the server would refuse (F030).
function hostRows(ws: Workspace, canClearRequired: boolean): HostRow[] {
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
      // Whichever provenance sentence this row ends up with, the REMOVE it
      // offers needs the requirements tier too when an operator-authored row
      // backs the host — a promoted-session host carries one just as a plain
      // "required by this workspace" one does (approveHosts writes
      // egress:<host> · required · operator_set).
      const blocked =
        reqs[`egress:${host}`]?.provenance === "operator_set" && !canClearRequired ? OPERATOR_ONLY_REASON : undefined;
      // Session attribution: a recorded session that promoted its observed
      // egress AND actually reached this host — the honest, derivable half of
      // "promoted from session X" (there is no per-host provenance field on
      // the wire, so this is inferred from the session's own capture).
      const session = Object.entries(ws.record_results ?? {}).find(
        ([, rr]) => rr.egress_promoted && (rr.observations?.domains ?? []).some((d) => d.host === host && d.allow_count > 0),
      );
      if (session) {
        const [key, rr] = session;
        return { host, provenance: `promoted from session "${rr.label || key}"`, removable: true, removeBlockedReason: blocked };
      }
      if (approved.has(host)) return { host, provenance: "approved for this workspace", removable: true, removeBlockedReason: blocked };
      const req = reqs[`egress:${host}`];
      return {
        host,
        provenance: req?.provenance === "operator_set" ? "required by this workspace" : "detected when this workspace was scanned",
        removable: true,
        removeBlockedReason: blocked,
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
  // The SECOND tier: clearing a host's operator-authored requirements row is
  // PUT /workspaces/{id}/requirements, which registers on operatorOnly — a
  // security admin is refused it. See hostRows' `canClearRequired`.
  const operator = useOperator();
  const rows = hostRows(ws, operator);
  const [removing, setRemoving] = React.useState<string | null>(null);

  const remove = async (host: string) => {
    setRemoving(host);
    try {
      const updated = await workspacesApi.setApprovedEgress(
        ws.id,
        (ws.approved_egress ?? []).filter((h) => h !== host),
      );
      // This write has LANDED on the server. Publish it BEFORE attempting the
      // second one below — the two are separate PUTs on separate route tiers
      // (approved-egress on securityOps, requirements on operatorOnly), so the
      // pair is not atomic and the second can fail on its own. Reporting that
      // as one "Failed to remove" and never calling onWorkspaceUpdated left
      // the card rendering a host the server had already dropped, with its
      // pre-write provenance (F030).
      onWorkspaceUpdated(updated);
      const reqKey = `egress:${host}`;
      // Only an operator-authored requirement row can be removed this way — a
      // scan_seeded row lives on the source's own contract and rebuilds on
      // the next scan; a full-replace PUT here must never carry it back
      // (same rule wizard-types.ts's operatorOverlay used to enforce).
      if (updated.requirements?.[reqKey]?.provenance === "operator_set") {
        const nextReqs = { ...updated.requirements };
        delete nextReqs[reqKey];
        try {
          onWorkspaceUpdated(await workspacesApi.setRequirements(ws.id, nextReqs));
        } catch (e) {
          // Half-done, said as half-done: the allowlist no longer carries the
          // host, the required-host row still does.
          toast.error(`Removed ${host} from the allowlist, but its required-host row could not be cleared`, {
            description: getErrorMessage(e),
          });
        }
      }
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
