/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The one removable-host row both egress cards (allowed, denied) render.
import { X } from "lucide-react";
import { Button } from "../../ui/button";
import { Mono } from "../../wardyn/code-block";

export interface HostRow {
  host: string;
  provenance: string;
  removable: boolean;
  // Set when the row is removable in principle but THIS caller may not remove
  // it — the sentence saying why (an existing canon reason, e.g.
  // copy.ts's OPERATOR_ONLY_REASON). A removal can need a HIGHER tier than the
  // card's own `canRemove` when it takes two writes on two different route
  // tiers (AllowedHostsCard's approved-egress + requirements pair, F030);
  // rendering a live button over a route the server refuses is the bug this
  // field exists to prevent. Distinct from `removable: false`, which means the
  // row has no remove control at all (a structural host).
  removeBlockedReason?: string;
}

export function HostList({ rows, emptyText, canRemove, removing, onRemove }: {
  rows: HostRow[];
  emptyText: string;
  canRemove: boolean;
  removing: string | null;
  onRemove: (host: string) => void;
}) {
  if (rows.length === 0) return <p className="text-xs text-muted-foreground">{emptyText}</p>;
  return (
    <ul className="space-y-2">
      {rows.map((r) => (
        <li key={r.host} className="flex items-center gap-3 rounded-lg border border-border p-2.5">
          <Mono className="flex-1 text-foreground">{r.host}</Mono>
          <span className="text-meta text-muted-foreground">{r.provenance}</span>
          {r.removable ? (
            <Button
              size="icon"
              variant="ghost"
              className="size-7 shrink-0"
              disabled={!canRemove || !!r.removeBlockedReason || removing === r.host}
              onClick={() => onRemove(r.host)}
              aria-label={`Remove ${r.host}`}
              title={r.removeBlockedReason}
            >
              <X className="size-3.5" />
            </Button>
          ) : (
            <span className="w-7 shrink-0 text-center text-muted-foreground">—</span>
          )}
        </li>
      ))}
    </ul>
  );
}
