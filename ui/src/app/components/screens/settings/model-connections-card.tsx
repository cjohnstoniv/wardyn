/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// "Your model connections" (#541, design §5.4, packet MP-D) — the page every
// person, admins included, connects their own model-provider credential from.
// Renders in the User view only (SettingsScreen gates it on !adminView), since
// an admin reaches it by switching to Member view (the same rule the shell
// strip and the door already follow — model-access-banner.tsx's own comment).
//
// Replaces "Your model key" (member-getting-started.tsx's retired
// your-model-key.tsx) and the AWS row it carried: those answered ONE
// hardcoded claude-code lane; this answers every provider a person may use,
// one row each, off the SAME door the strip and the Agents tab already share
// (#544) — no pane of its own.
//
// Claims the door (rule 2, model-access-context.tsx) whenever a row offers a
// button: without it, the strip's OWN per-provider line (providerAttention)
// also renders on this page in provider mode — nothing suppresses it here the
// way the legacy single-strip branch is suppressed on /account for an
// operator — so this card and the strip would show two buttons for the same
// provider.
import { Chip } from "../../wardyn/primitives";
import { Button } from "../../ui/button";
import { useClaimModelAccessDoor, useModelAccessDoor } from "../../wardyn/model-access-context";
import {
  connectionRowCopy,
  connectionRows,
  connectionsSummary,
  type ConnectionRow,
  type ConnectionRowCopy,
} from "../../../lib/model-connections";
import { CONNECTIONS } from "../../wardyn/copy/door";
import type { SetupStatus } from "../../../lib/types";

function ConnectionRowView({
  row,
  copy,
  onOpen,
}: {
  row: ConnectionRow;
  copy: ConnectionRowCopy;
  onOpen: (providerId: string) => void;
}) {
  const name = row.provider.name || row.provider.id;
  return (
    <div className="flex items-start justify-between gap-3 py-3 first:pt-0 last:pb-0">
      <div className="min-w-0">
        <p className="text-sm font-medium text-foreground">{name}</p>
        <p className="text-meta text-muted-foreground">{copy.forLine}</p>
        {copy.line && (
          <p className="text-meta text-muted-foreground" title={copy.title || undefined}>
            {copy.line}
          </p>
        )}
      </div>
      <div className="flex shrink-0 items-center gap-2">
        <Chip tone={copy.chip.tone}>{copy.chip.label}</Chip>
        {copy.button && (
          <Button size="sm" variant="outline" onClick={() => onOpen(row.provider.id)}>
            {copy.button}
          </Button>
        )}
      </div>
    </div>
  );
}

/** ModelConnectionsCard renders nothing with no provider block — the caller
 *  (SettingsScreen) gates the mount on `status.model_providers`, but this
 *  stays defensive so a direct-mount suite gets today's absence rather than
 *  an empty card shell. */
export function ModelConnectionsCard({ status, onChanged }: { status: SetupStatus; onChanged: () => void }) {
  const door = useModelAccessDoor();
  // No memo: a handful of providers, and connectionRows/connectionRowCopy are
  // cheap pure reads — not worth the dependency-array upkeep.
  const rows = connectionRows(status);
  const rowCopies = rows.map((row) => ({ row, copy: connectionRowCopy(status, row) }));
  useClaimModelAccessDoor(rowCopies.some(({ copy }) => !!copy.button));
  // #541 fix review: `model_providers == null` (not `!status.model_providers`
  // — functionally the same here since an array is always truthy, but the
  // explicit null check matches the SAME providerMode expression
  // model-access-banner.tsx and member-getting-started.tsx use) — no block at
  // all, distinct from a block granting this caller nothing (`[]`, which
  // still renders the card's own "No providers" state below).
  if (status.model_providers == null) return null;
  const summary = connectionsSummary(rows);

  return (
    <section className="rounded-xl border border-border bg-card p-4" data-testid="model-connections-card">
      <div className="flex items-center justify-between gap-2">
        <h3 className="text-sm font-medium text-foreground">{CONNECTIONS.TITLE}</h3>
        <Chip tone={summary.tone}>{summary.label}</Chip>
      </div>
      <p className="mt-0.5 text-body leading-snug text-muted-foreground">{CONNECTIONS.LEDE}</p>
      {rowCopies.length > 0 && (
        <div className="mt-3 divide-y divide-border">
          {rowCopies.map(({ row, copy }) => (
            <ConnectionRowView
              key={row.provider.id}
              row={row}
              copy={copy}
              onOpen={(providerId) => door.openDoor({ for: { provider: providerId }, onClosed: onChanged })}
            />
          ))}
        </div>
      )}
    </section>
  );
}
