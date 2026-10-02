/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Admin → Credentials, under the inventory (#1477): the tokens an admin
// created for someone else. No one can create such a token any more; this is
// the read-only inventory of the ones that already exist and keep working,
// with Revoke (the existing revoke-anyone's-token power, so a button, not a
// privilege). Metadata only: a token value is never fetched or shown. Both
// admin tiers read it, the same as the page above it.
import * as React from "react";
import { toast } from "sonner";
import { KeyRound } from "lucide-react";
import { credentials as credentialsApi, type AdminMintedToken } from "../../lib/api/credentials";
import { absoluteTime, getErrorMessage, relativeTime } from "../../lib/format";
import { AlertDialog, AlertDialogAction, AlertDialogCancel, AlertDialogContent, AlertDialogDescription, AlertDialogFooter, AlertDialogHeader, AlertDialogTitle } from "../ui/alert-dialog";
import { buttonVariants, Button } from "../ui/button";
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "../ui/table";
import { Chip } from "../wardyn/primitives";
import { ErrorState, TableSkeleton } from "../wardyn/states";
import { MINTED } from "../wardyn/copy/credentials";

const personOf = (t: AdminMintedToken) => t.email || t.principal;

export function AdminMintedTokens() {
  const [tokens, setTokens] = React.useState<AdminMintedToken[] | null>(null);
  const [failed, setFailed] = React.useState(false);
  const [target, setTarget] = React.useState<AdminMintedToken | null>(null);
  const [revoking, setRevoking] = React.useState(false);
  // A ref as well as the state: two clicks in one tick must revoke once.
  const inFlight = React.useRef(false);

  const load = React.useCallback(() => {
    setFailed(false);
    credentialsApi
      .listAdminMintedTokens()
      .then(setTokens)
      .catch(() => setFailed(true));
  }, []);
  React.useEffect(load, [load]);

  const revoke = async () => {
    if (!target || inFlight.current) return;
    inFlight.current = true;
    setRevoking(true);
    try {
      await credentialsApi.revokeToken(target.id);
      setTarget(null);
      toast.success(MINTED.REVOKED_TOAST);
      load();
    } catch (e) {
      setTarget(null);
      toast.error(getErrorMessage(e));
    } finally {
      inFlight.current = false;
      setRevoking(false);
    }
  };

  return (
    <section className="mt-6 overflow-hidden rounded-xl border border-border bg-card" aria-labelledby="admin-minted-title">
      <div className="p-4">
        <div className="flex flex-wrap items-center gap-2">
          <h2 id="admin-minted-title" className="text-sm font-semibold text-foreground">
            {MINTED.TITLE}
          </h2>
          {tokens && tokens.length > 0 && <Chip tone="warning">{MINTED.CHIP(tokens.length)}</Chip>}
        </div>

        {failed ? (
          <ErrorState onRetry={load} />
        ) : !tokens ? (
          <TableSkeleton rows={2} cols={5} />
        ) : tokens.length === 0 ? (
          <div className="mt-3 space-y-1">
            <p className="flex items-center gap-2 text-sm text-foreground">
              <KeyRound className="size-4 text-muted-foreground" aria-hidden /> {MINTED.EMPTY}
            </p>
            <p className="text-xs text-muted-foreground">{MINTED.NOTE}</p>
          </div>
        ) : (
          <>
            <p className="mt-2 text-xs text-muted-foreground">{MINTED.COUNT(tokens.length)}</p>
            <p className="text-xs text-muted-foreground">{MINTED.NOTE}</p>
            <div className="mt-3 overflow-x-auto">
              <Table>
                <TableHeader>
                  <TableRow className="hover:bg-transparent">
                    <TableHead>{MINTED.COL_PERSON}</TableHead>
                    <TableHead>{MINTED.COL_TOKEN}</TableHead>
                    <TableHead>{MINTED.COL_CREATED_BY}</TableHead>
                    <TableHead>{MINTED.COL_ADDED}</TableHead>
                    <TableHead>{MINTED.COL_LAST_USED}</TableHead>
                    <TableHead />
                  </TableRow>
                </TableHeader>
                <TableBody>
                  {tokens.map((t) => (
                    <TableRow key={t.id}>
                      <TableCell className="whitespace-nowrap font-medium">{personOf(t)}</TableCell>
                      <TableCell className="font-mono text-xs">{t.name}</TableCell>
                      <TableCell className="text-muted-foreground">{t.minted_by}</TableCell>
                      <TableCell className="text-muted-foreground" title={absoluteTime(t.created_at)}>
                        {relativeTime(t.created_at)}
                      </TableCell>
                      <TableCell
                        className="text-muted-foreground"
                        title={t.last_used_at ? absoluteTime(t.last_used_at) : undefined}
                      >
                        {t.last_used_at ? relativeTime(t.last_used_at) : MINTED.NEVER}
                      </TableCell>
                      <TableCell>
                        <Button size="sm" variant="outline" onClick={() => setTarget(t)}>
                          {MINTED.REVOKE}
                        </Button>
                      </TableCell>
                    </TableRow>
                  ))}
                </TableBody>
              </Table>
            </div>
          </>
        )}
      </div>

      <AlertDialog open={!!target} onOpenChange={(o) => !o && !revoking && setTarget(null)}>
        <AlertDialogContent>
          <AlertDialogHeader>
            <AlertDialogTitle>{target ? MINTED.REVOKE_TITLE(target.name, personOf(target)) : ""}</AlertDialogTitle>
            <AlertDialogDescription>{target ? MINTED.REVOKE_BODY(personOf(target)) : ""}</AlertDialogDescription>
          </AlertDialogHeader>
          <AlertDialogFooter>
            <AlertDialogCancel disabled={revoking}>{MINTED.REVOKE_CANCEL}</AlertDialogCancel>
            <AlertDialogAction
              className={buttonVariants({ variant: "destructive" })}
              disabled={revoking}
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
    </section>
  );
}
