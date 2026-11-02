/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Your account: "Your API tokens" (r15 T2-d). A person mints, lists and revokes their own tokens here, through the
// shared token dialog, whose Use choice makes one for the Wardyn API or for calling inbound ports. The card
// renders nothing where there is no person (the admin token, local mode) and where the api_token feature is not
// granted: the server's wall is the same check.
import * as React from "react";
import { toast } from "sonner";
import { HttpError } from "../../../lib/api/core";
import { serviceIdentities } from "../../../lib/api/service-identities";
import { capabilityAllowed, useMyCapabilities } from "../../../lib/capabilities";
import { getErrorMessage } from "../../../lib/format";
import type { ApiToken } from "../../../lib/types/service-identities";
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
import { Button, buttonVariants } from "../../ui/button";
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "../../ui/table";
import { CollapsibleCard } from "../../wardyn/collapsible-card";
import { MINTED } from "../../wardyn/copy/credentials";
import { inWords, OWN_TOKENS as O, SERVICES as T } from "../../wardyn/copy/service-tokens";
import { useOperator } from "../../wardyn/operator-context";
import { Chip } from "../../wardyn/primitives";
import { ErrorState, TableSkeleton } from "../../wardyn/states";
import { TokenDialog } from "./token-dialog";

type Load = "loading" | "ready" | "error" | "hidden";

const isLive = (t: ApiToken) => !t.revoked_at && !(t.expires_at && Date.parse(t.expires_at) <= Date.now());

export function ApiTokensCard() {
  const [state, setState] = React.useState<Load>("loading");
  const [rows, setRows] = React.useState<ApiToken[]>([]);
  const [minting, setMinting] = React.useState(false);
  const [revoking, setRevoking] = React.useState<ApiToken | null>(null);
  const [revokeBusy, setRevokeBusy] = React.useState(false);
  const card = React.useRef<HTMLElement>(null);
  // Advisory, as the SSH keys card reads it: an admin is exempt server-side, so the set is not fetched for one.
  const operator = useOperator();
  const caps = useMyCapabilities(!operator);
  const granted = capabilityAllowed(caps, "feature", "api_token");

  const load = React.useCallback(() => {
    serviceIdentities
      .listMyTokens()
      .then((r) => {
        setRows(r);
        setState("ready");
      })
      .catch((e: unknown) => setState(e instanceof HttpError && (e.reason === "needs_sso" || e.reason === "api_token_no_human") ? "hidden" : "error"));
  }, []);
  React.useEffect(load, [load]);

  const revoke = async () => {
    if (!revoking || revokeBusy) return;
    setRevokeBusy(true);
    try {
      await serviceIdentities.revokeMyToken(revoking.id);
      toast.success(MINTED.REVOKED_TOAST);
      load();
    } catch (e) {
      toast.error(getErrorMessage(e));
    } finally {
      setRevoking(null);
      setRevokeBusy(false);
    }
  };

  if (state === "hidden" || !granted) return null;

  const focusCard = (selector: string) => {
    const el = card.current?.querySelector<HTMLElement>(selector) ?? card.current?.querySelector<HTMLElement>("h3 button");
    el?.focus();
  };

  return (
    <CollapsibleCard
      ref={card}
      title={O.TITLE}
      summary={state === "ready" ? O.SUMMARY(rows.filter(isLive).length) : undefined}
      testId="api-tokens-card"
    >
      {state === "loading" ? (
        <div role="status" aria-label={O.LOADING}>
          <span className="sr-only">{O.LOADING}</span>
          <TableSkeleton rows={2} cols={3} />
        </div>
      ) : state === "error" ? (
        <div role="group" aria-label={O.LOAD_ERROR_LABEL}>
          <ErrorState
            as="h4"
            onRetry={() => {
              setState("loading");
              load();
            }}
          />
        </div>
      ) : (
        <div className="space-y-3">
          <div className="flex justify-end">
            <Button type="button" variant="outline" size="sm" data-mint="own" onClick={() => setMinting(true)}>
              {T.MINT}
            </Button>
          </div>
          {rows.length === 0 ? (
            <p className="text-body text-muted-foreground">{T.NO_TOKENS}</p>
          ) : (
            <Table aria-label={O.TABLE_LABEL}>
              <TableHeader>
                <TableRow className="hover:bg-transparent">
                  <TableHead>{T.COL_TOKEN}</TableHead>
                  <TableHead>{T.COL_EXPIRES}</TableHead>
                  <TableHead>{T.COL_LAST_USED}</TableHead>
                  <TableHead />
                </TableRow>
              </TableHeader>
              <TableBody>
                {rows.map((t) => (
                  <TokenRow key={t.id} t={t} onRevoke={setRevoking} />
                ))}
              </TableBody>
            </Table>
          )}
        </div>
      )}

      {minting && (
        <TokenDialog
          kind="mint"
          initialName=""
          submit={(body) => serviceIdentities.mintMyToken(body)}
          onCreated={load}
          onRefused={() => false}
          onClose={(created) => {
            setMinting(false);
            if (created) toast.success(T.MINTED_TOAST);
          }}
          restoreFocus={() => focusCard('[data-mint="own"]')}
        />
      )}

      <AlertDialog open={!!revoking} onOpenChange={(o) => !o && !revokeBusy && setRevoking(null)}>
        <AlertDialogContent
          onCloseAutoFocus={(e) => {
            e.preventDefault();
            focusCard('[data-mint="own"]');
          }}
        >
          <AlertDialogHeader>
            <AlertDialogTitle>{revoking ? O.REVOKE_TITLE(revoking.name) : ""}</AlertDialogTitle>
            <AlertDialogDescription>{O.REVOKE_BODY}</AlertDialogDescription>
          </AlertDialogHeader>
          <AlertDialogFooter>
            <AlertDialogCancel disabled={revokeBusy}>{MINTED.REVOKE_CANCEL}</AlertDialogCancel>
            <AlertDialogAction
              className={buttonVariants({ variant: "destructive" })}
              disabled={revokeBusy}
              onClick={(e) => {
                e.preventDefault();
                void revoke();
              }}
            >
              {MINTED.REVOKE_CONFIRM}
            </AlertDialogAction>
          </AlertDialogFooter>
        </AlertDialogContent>
      </AlertDialog>
    </CollapsibleCard>
  );
}

function TokenRow({ t, onRevoke }: { t: ApiToken; onRevoke: (t: ApiToken) => void }) {
  const expired = !!t.expires_at && Date.parse(t.expires_at) <= Date.now();
  return (
    <TableRow>
      <TableCell>
        <span className="flex flex-wrap items-center gap-2">
          <span className="font-mono text-xs">{t.name}</span>
          {t.revoked_at ? <Chip tone="neutral" dot>{T.REVOKED}</Chip> : expired ? <Chip tone="neutral" dot>{T.EXPIRED}</Chip> : null}
          {t.inbound && <Chip tone="neutral">{T.INBOUND_ONLY}</Chip>}
        </span>
        <span className="block text-xs text-muted-foreground">{T.CREATED(inWords(t.created_at))}</span>
      </TableCell>
      <TableCell className="text-muted-foreground">
        {t.revoked_at
          ? T.REVOKED_AT(inWords(t.revoked_at))
          : t.expires_at
            ? expired
              ? T.EXPIRED_AT(inWords(t.expires_at))
              : inWords(t.expires_at)
            : T.NEVER}
      </TableCell>
      <TableCell className="text-muted-foreground">{t.last_used_at ? inWords(t.last_used_at) : T.NEVER}</TableCell>
      <TableCell>
        {isLive(t) && (
          <Button type="button" size="sm" variant="outline" aria-label={O.REVOKE_LABEL(t.name)} onClick={() => onRevoke(t)}>
            {T.REVOKE}
          </Button>
        )}
      </TableCell>
    </TableRow>
  );
}
