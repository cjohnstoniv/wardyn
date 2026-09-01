/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// AllowedHostsCard's mirror — the operator-owned denied_egress list
// (Workspace.DeniedEgress): hosts permanently blocked for this workspace,
// written by a `deny · always` approval decision or directly here. Unlike
// approved_egress, nothing infers a denial (no clone host, no session
// provenance), so every row is a plain operator-set block with a remove
// control. This card IS the "workspace's egress settings" that
// denyDialogCopy/egressBlastRadius's `always` cases promise as the way to
// undo one — before this existed that promise was console-wide false.
import * as React from "react";
import { toast } from "sonner";
import { getErrorMessage } from "../../../lib/format";
import { workspaces as workspacesApi } from "../../../lib/api/workspaces";
import type { Workspace } from "../../../lib/types";
import { DetailSectionCard } from "./section-card";
import { HostList } from "./host-list";
import { useSecurityOperator } from "../../wardyn/operator-context";

export function DeniedHostsCard({ ws, onWorkspaceUpdated }: { ws: Workspace; onWorkspaceUpdated: (w: Workspace) => void }) {
  // useSecurityOperator, not useOperator (0.7 §B): PUT
  // /workspaces/{id}/denied-egress registers on securityOps (routes.go:368),
  // the deny half of the same verdict AllowedHostsCard writes.
  const securityOperator = useSecurityOperator();
  const rows = React.useMemo(
    () => [...(ws.denied_egress ?? [])].sort().map((host) => ({ host, provenance: "denied for this workspace", removable: true })),
    [ws.denied_egress],
  );
  const [removing, setRemoving] = React.useState<string | null>(null);

  const remove = async (host: string) => {
    setRemoving(host);
    try {
      const updated = await workspacesApi.setDeniedEgress(
        ws.id,
        (ws.denied_egress ?? []).filter((h) => h !== host),
      );
      onWorkspaceUpdated(updated);
    } catch (e) {
      toast.error(`Failed to remove ${host}`, { description: getErrorMessage(e) });
    } finally {
      setRemoving(null);
    }
  };

  return (
    <DetailSectionCard
      title={`Denied hosts · ${rows.length}`}
      subtitle="Every run against this workspace is permanently blocked from these — deny beats any allow. Removing one only lifts the block; it doesn't approve the host."
    >
      <HostList rows={rows} emptyText="Nothing denied." canRemove={!!securityOperator} removing={removing} onRemove={(h) => void remove(h)} />
    </DetailSectionCard>
  );
}
