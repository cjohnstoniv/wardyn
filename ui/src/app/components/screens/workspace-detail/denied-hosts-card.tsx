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
import { X } from "lucide-react";
import { toast } from "sonner";
import { Button } from "../../ui/button";
import { Mono } from "../../wardyn/code-block";
import { getErrorMessage } from "../../../lib/format";
import { workspaces as workspacesApi } from "../../../lib/api/workspaces";
import type { Workspace } from "../../../lib/types";
import { DetailSectionCard } from "./section-card";
import { useOperator } from "../../wardyn/operator-context";

export function DeniedHostsCard({ ws, onWorkspaceUpdated }: { ws: Workspace; onWorkspaceUpdated: (w: Workspace) => void }) {
  const operator = useOperator();
  const hosts = React.useMemo(() => [...(ws.denied_egress ?? [])].sort(), [ws.denied_egress]);
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
      title={`Denied hosts · ${hosts.length}`}
      subtitle="Every run against this workspace is permanently blocked from these — deny beats any allow. Removing one only lifts the block; it doesn't approve the host."
    >
      {hosts.length === 0 ? (
        <p className="text-xs text-muted-foreground">Nothing denied.</p>
      ) : (
        <ul className="space-y-2">
          {hosts.map((host) => (
            <li key={host} className="flex items-center gap-3 rounded-lg border border-border p-2.5">
              <Mono className="flex-1 text-foreground">{host}</Mono>
              <span className="text-meta text-muted-foreground">denied for this workspace</span>
              <Button
                size="icon"
                variant="ghost"
                className="size-7 shrink-0"
                disabled={!operator || removing === host}
                onClick={() => void remove(host)}
                aria-label={`Remove ${host}`}
              >
                <X className="size-3.5" />
              </Button>
            </li>
          ))}
        </ul>
      )}
    </DetailSectionCard>
  );
}
