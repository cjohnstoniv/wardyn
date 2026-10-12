/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Your account: the invitations an admin sent this person (#1508, mock M-SVC v2). Each is a standing offer to
// create an API token that acts as them; only they, signed in, can claim it, so the card lists nothing but
// their own pending ones (GET /me/invitations) and Claim runs through the shared token dialog, which shows
// the new token once. The card renders nothing when nothing is waiting, and nothing where there is no person
// to invite (local mode, the admin token).
import * as React from "react";
import { useLocation } from "react-router-dom";
import { toast } from "sonner";
import { HttpError } from "../../../lib/api/core";
import { serviceIdentities } from "../../../lib/api/service-identities";
import { absoluteTime } from "../../../lib/format";
import type { Invitation } from "../../../lib/types/service-identities";
import { Button } from "../../ui/button";
import { CollapsibleCard } from "../../wardyn/collapsible-card";
import { INVITATIONS as T, inWords, lifetimeWords } from "../../wardyn/copy/service-tokens";
import { ErrorState, TableSkeleton } from "../../wardyn/states";
import { TokenDialog } from "./token-dialog";

// The reasons that mean the invitation is no longer the person's to claim.
const STALE_REASONS = new Set(["invitation_not_found", "invitation_not_pending"]);

type Load = "loading" | "ready" | "error" | "unavailable" | "hidden";

function loadFailure(e: unknown): Load {
  if (e instanceof HttpError) {
    if (e.status === 403 && e.reason === "api_token_no_human") return "hidden";
    if (e.reason === "people_store_unavailable") return "unavailable";
  }
  return "error";
}

export function InvitationsCard() {
  const [state, setState] = React.useState<Load>("loading");
  const [items, setItems] = React.useState<Invitation[]>([]);
  // The names of the person's own tokens, for the one an invitation replaces (it carries an id only).
  const [names, setNames] = React.useState<Record<string, string>>({});
  const [claiming, setClaiming] = React.useState<Invitation | null>(null);
  const card = React.useRef<HTMLElement>(null);
  // The console path an invitation's link names (/account?invitation=<id>) opens the card.
  const linked = new URLSearchParams(useLocation().search).get("invitation");

  const load = React.useCallback(() => {
    serviceIdentities
      .listMyInvitations()
      .then((rows) => {
        setItems(rows);
        setState("ready");
        if (rows.some((r) => r.replaces_token_id)) {
          serviceIdentities
            .listMyTokens()
            .then((toks) => setNames(Object.fromEntries(toks.map((t) => [t.id, t.name]))))
            .catch(() => {}); // the card then says "a token": the name is a courtesy
        }
      })
      .catch((e: unknown) => setState(loadFailure(e)));
  }, []);
  React.useEffect(load, [load]);

  if (state === "hidden" || (state === "ready" && items.length === 0 && !claiming)) return null;

  const restoreFocus = () => {
    const opener = card.current?.querySelector<HTMLElement>(`[data-claim="${claiming?.id}"]`);
    (opener ?? card.current?.querySelector<HTMLElement>("h3 button"))?.focus();
  };

  return (
    <CollapsibleCard
      ref={card}
      title={T.TITLE}
      summary={state === "ready" && items.length > 0 ? T.SUMMARY(items.length) : undefined}
      defaultOpen={!!linked}
      testId="invitations-card"
    >
      {state === "loading" ? (
        <div role="status" aria-label={T.LOADING}>
          <span className="sr-only">{T.LOADING}</span>
          <TableSkeleton rows={2} cols={3} />
        </div>
      ) : state === "error" ? (
        <div role="group" aria-label={T.LOAD_ERROR_LABEL}>
          <ErrorState as="h4" onRetry={() => {
            setState("loading");
            load();
          }} />
        </div>
      ) : state === "unavailable" ? (
        <div role="alert" className="rounded-md border border-border bg-muted px-3 py-2 text-sm">
          <strong className="font-medium">{T.UNAVAILABLE_TITLE}</strong>
          <br />
          {T.UNAVAILABLE_BODY}
        </div>
      ) : (
        <div className="space-y-3">
          <p className="text-xs text-muted-foreground">{T.LEDE}</p>
          <ul className="space-y-2">
            {items.map((inv) => (
              <li key={inv.id} className="flex items-start justify-between gap-3 rounded-lg border border-border px-3 py-2">
                <div className="min-w-0">
                  <p className="text-sm font-medium text-foreground">{T.FROM(inv.created_by)}</p>
                  <p className="text-xs text-muted-foreground" title={absoluteTime(inv.expires_at)}>
                    {T.EXPIRES(inWords(inv.expires_at))}
                  </p>
                  <ul className="mt-1 list-disc space-y-0.5 pl-4 text-xs text-muted-foreground">
                    {inv.token_name && (
                      <li>
                        {T.TOKEN_NAME} <span className="font-mono">{inv.token_name}</span>
                      </li>
                    )}
                    {inv.token_ttl_seconds && <li>{T.LASTS(lifetimeWords(inv.token_ttl_seconds))}</li>}
                    {inv.replaces_token_id && (
                      <li>
                        {T.REVOKES_LEAD} <span className="font-mono">{names[inv.replaces_token_id] ?? T.REPLACES_UNKNOWN}</span>
                        {T.REVOKES_TAIL}
                      </li>
                    )}
                  </ul>
                </div>
                <Button
                  type="button"
                  variant="outline"
                  size="sm"
                  data-claim={inv.id}
                  aria-label={T.CLAIM_LABEL(inv.created_by)}
                  onClick={() => setClaiming(inv)}
                >
                  {T.CLAIM}
                </Button>
              </li>
            ))}
          </ul>
        </div>
      )}

      {claiming && (
        <TokenDialog
          kind="claim"
          limitSeconds={claiming.token_ttl_seconds}
          replaces={claiming.replaces_token_id ? (names[claiming.replaces_token_id] ?? T.REPLACES_UNKNOWN) : undefined}
          initialName={claiming.token_name ?? ""}
          submit={(body) => serviceIdentities.claimInvitation(claiming.id, body)}
          onCreated={load}
          onRefused={(e) => {
            load();
            return STALE_REASONS.has(e.reason);
          }}
          onClose={(created) => {
            setClaiming(null);
            if (created) toast.success(T.CLAIMED_TOAST);
          }}
          restoreFocus={restoreFocus}
        />
      )}
    </CollapsibleCard>
  );
}
