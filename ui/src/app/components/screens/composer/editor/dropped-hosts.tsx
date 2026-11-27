/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// What the draft left out because the person's ceiling doesn't allow it, in plain words and never as the server's
// code. For a host, "Ask an admin to allow this host" opens a note that says who can add it and how many approvals
// that admin's change would wait for. It sends nothing: there is no request for a host.
import * as React from "react";
import { Button } from "../../../ui/button";
import { ASSISTANT as T } from "../../../wardyn/copy/assistant";
import { sectionLabel } from "./part-card";

/** One left-out entry: a host or a field, the unit whose ceiling refused it, and what a change to that ceiling waits for. */
export interface DroppedItem {
  readonly host?: string;
  readonly field?: string;
  readonly unit?: string;
  readonly admin_change_approvals?: number;
  /** The server's code for why it was left out (internal/assistant/dropped.go). */
  readonly code?: string;
  /** A field the server set itself because the draft did not: information, not something the person left out. */
  readonly filled?: boolean;
}

function Dropped({ item }: { item: DroppedItem }) {
  const [open, setOpen] = React.useState(false);
  const unit = item.unit ?? "";
  const askable = !!item.host && typeof item.admin_change_approvals === "number";
  return (
    <div data-testid="composer-left-out" className="space-y-1.5 rounded-lg border border-warning/35 bg-warning-subtle px-2.5 py-2">
      <p className="text-body text-foreground">{item.filled ? T.DEFAULT_FILLED(sectionLabel(item.field ?? "")) : T.LEFT_OUT(item.host ?? item.field ?? "", unit)}</p>
      {askable && (
        <>
          <Button type="button" size="sm" variant="outline" aria-expanded={open} onClick={() => setOpen((o) => !o)}>
            {T.ASK_ADMIN}
          </Button>
          {open && (
            <p data-testid="composer-admin-note" className="text-body text-foreground">
              {T.ADMIN_ONLY(unit, item.admin_change_approvals ?? 0)}
            </p>
          )}
        </>
      )}
    </div>
  );
}

export function DroppedHosts({ items }: { items: readonly DroppedItem[] }) {
  if (items.length === 0) return null;
  return (
    <div className="space-y-2">
      {items.map((d, i) => (
        <Dropped key={`${d.host ?? d.field ?? ""}-${i}`} item={d} />
      ))}
    </div>
  );
}
