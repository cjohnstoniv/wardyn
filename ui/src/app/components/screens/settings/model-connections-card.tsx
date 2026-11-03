/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// "Your model connections" (#541, design §5.4, packet MP-D) — the page every
// person, admins included, connects their own model-provider credential from.
// Renders in the User view only (YourAccountScreen gates it on !adminView), since
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
//
// #1200 review R4-M1: the claim only holds while the ROW carrying the button
// is actually mounted, i.e. while the card is expanded — this is now
// CollapsibleCard's own body, collapsed by default. Claiming unconditionally
// (this card's very first shape, before the compact-cards fix) left the
// button held by a row nobody could see: the strip suppressed its own
// "Sign in to AWS" for a claim with no visible control behind it, and a
// member landing on a collapsed Your account saw zero sign-in buttons
// anywhere. ClaimWhileOpen is a child of the CollapsibleCard body for exactly
// this reason — mounted only while open, so it claims and releases with the
// same visibility the button itself has.
import * as React from "react";
import { toast } from "sonner";
import { modelProviderCredentials } from "../../../lib/api/model-provider-credentials";
import { getErrorMessage } from "../../../lib/format";
import {
  AlertDialog,
  AlertDialogAction,
  AlertDialogCancel,
  AlertDialogContent,
  AlertDialogDescription,
  AlertDialogFooter,
  AlertDialogHeader,
  AlertDialogTitle,
} from "../../ui/alert-dialog";
import { OIDC_SIGN_IN } from "../../wardyn/copy/oidc-signin";
import { Chip } from "../../wardyn/primitives";
import { Button, buttonVariants } from "../../ui/button";
import { CollapsibleCard } from "../../wardyn/collapsible-card";
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

/** Claims the model-access door only while mounted — a child of
 *  CollapsibleCard's body, so it lives and dies with the button it stands
 *  for (review R4-M1). Renders nothing itself. */
function ClaimWhileOpen({ claims }: { claims: boolean }) {
  useClaimModelAccessDoor(claims);
  return null;
}

function ConnectionRowView({
  row,
  copy,
  onOpen,
  onDisconnect,
}: {
  row: ConnectionRow;
  copy: ConnectionRowCopy;
  onOpen: (providerId: string) => void;
  onDisconnect: (row: ConnectionRow) => void;
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
        {copy.unbound && (
          <>
            <p className="text-meta text-warning">{copy.unbound.line}</p>
            <p className="text-meta text-muted-foreground">{copy.unbound.explain}</p>
          </>
        )}
        {/* #592 — Added/Last used, from THIS caller's own provider_access
            row; absent whenever nothing is stored for it (copy.meta === ""). */}
        {copy.meta && (
          <p className="text-meta text-muted-foreground" title={copy.metaTitle || undefined}>
            {copy.meta}
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
        {copy.disconnect && (
          <Button size="sm" variant="ghost" onClick={() => onDisconnect(row)}>
            {copy.disconnect}
          </Button>
        )}
      </div>
    </div>
  );
}

/** ModelConnectionsCard renders nothing with no provider block — the caller
 *  (YourAccountScreen) mounts it in the User view whatever the block, so this
 *  check is what keeps a legacy install's page free of an empty card shell. */
export function ModelConnectionsCard({ status, onChanged }: { status: SetupStatus; onChanged: () => void }) {
  const door = useModelAccessDoor();
  // The endpoint sign-in being disconnected, while the confirmation is up.
  const [leaving, setLeaving] = React.useState<ConnectionRow | null>(null);
  const [busy, setBusy] = React.useState(false);
  const leaveName = leaving ? leaving.provider.name || leaving.provider.id : "";
  const disconnect = async () => {
    if (!leaving) return;
    setBusy(true);
    try {
      await modelProviderCredentials.deleteCredential(leaving.provider.id);
      toast.success(OIDC_SIGN_IN.DISCONNECT_TOAST(leaveName));
      setLeaving(null);
      onChanged();
    } catch (e) {
      toast.error(getErrorMessage(e));
      setLeaving(null);
    } finally {
      setBusy(false);
    }
  };
  // No memo: a handful of providers, and connectionRows/connectionRowCopy are
  // cheap pure reads — not worth the dependency-array upkeep.
  const rows = connectionRows(status);
  const rowCopies = rows.map((row) => ({ row, copy: connectionRowCopy(status, row) }));
  // #541 fix review: `model_providers == null` (not `!status.model_providers`
  // — functionally the same here since an array is always truthy, but the
  // explicit null check matches the SAME providerMode expression
  // model-access-banner.tsx and member-getting-started.tsx use) — no block at
  // all, distinct from a block granting this caller nothing (`[]`, which
  // still renders the card's own "No providers" state below).
  if (status.model_providers == null) return null;
  const summary = connectionsSummary(rows);

  return (
    <CollapsibleCard
      title={CONNECTIONS.TITLE}
      summary={<Chip tone={summary.tone}>{summary.label}</Chip>}
      testId="model-connections-card"
    >
      <p className="text-body leading-snug text-muted-foreground">{CONNECTIONS.LEDE}</p>
      {rowCopies.length > 0 && (
        <>
          <ClaimWhileOpen claims={rowCopies.some(({ copy }) => !!copy.button)} />
          <div className="mt-3 divide-y divide-border">
            {rowCopies.map(({ row, copy }) => (
              <ConnectionRowView
                key={row.provider.id}
                row={row}
                copy={copy}
                onDisconnect={setLeaving}
                onOpen={(providerId) => {
                  if (row.access.state === "not_configured" && row.access.cause === "store_unreadable") { onChanged(); }
                  else door.openDoor({ for: { provider: providerId }, onClosed: onChanged });
                }}
              />
            ))}
          </div>
        </>
      )}
      <AlertDialog open={leaving !== null} onOpenChange={(open) => !open && !busy && setLeaving(null)}>
        <AlertDialogContent className="sm:max-w-md">
          <AlertDialogHeader>
            <AlertDialogTitle>{OIDC_SIGN_IN.DISCONNECT_TITLE(leaveName)}</AlertDialogTitle>
            <AlertDialogDescription>{OIDC_SIGN_IN.DISCONNECT_BODY(leaveName)}</AlertDialogDescription>
          </AlertDialogHeader>
          <AlertDialogFooter>
            <AlertDialogCancel disabled={busy}>{OIDC_SIGN_IN.CANCEL}</AlertDialogCancel>
            <AlertDialogAction
              className={buttonVariants({ variant: "destructive" })}
              disabled={busy}
              onClick={(e) => {
                e.preventDefault();
                void disconnect();
              }}
            >
              {OIDC_SIGN_IN.DISCONNECT}
            </AlertDialogAction>
          </AlertDialogFooter>
        </AlertDialogContent>
      </AlertDialog>
    </CollapsibleCard>
  );
}
