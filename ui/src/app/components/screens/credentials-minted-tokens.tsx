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
//
// #1508: each row says where its token stands (the State column, from the row's `disposition`), and a
// toggle adds the revoked rows, so a finished claim or move stays visible. The console shows the state
// and nothing more: inviting the owner and moving the token are the CLI's (`wardyn person invite`,
// `wardyn token move`).
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
import { DISPOSITION, MINTED } from "../wardyn/copy/credentials";
import { inWords } from "../wardyn/copy/service-tokens";
import { Checkbox } from "../ui/checkbox";
import { Label } from "../ui/label";

const personOf = (t: AdminMintedToken) => t.email || t.principal;

// The State cell: a chip with the disposition's label and a line under it saying what is true now. A value this
// console does not know shows as written in a neutral chip; a row with none (a daemon that predates the
// field) shows nothing.
const isExpired = (t: AdminMintedToken) => !t.revoked_at && !!t.expires_at && Date.parse(t.expires_at) <= Date.now();

function StateCell({ t }: { t: AdminMintedToken }) {
  if (!t.disposition) return null;
  // Past its expiry and never revoked: the token no longer authenticates, so "needs a decision" would be false.
  if (isExpired(t)) {
    return (
      <>
        <Chip tone="neutral" dot>{MINTED.EXPIRED}</Chip>
        <span className="mt-1 block text-xs text-muted-foreground">{MINTED.EXPIRED_SUB}</span>
      </>
    );
  }
  const d = DISPOSITION[t.disposition];
  return (
    <>
      <Chip tone={d?.tone ?? "neutral"} dot mono={!d}>
        {d?.label ?? t.disposition}
      </Chip>
      {d && <span className="mt-1 block text-xs text-muted-foreground">{d.sub(t.revoked_at ? inWords(t.revoked_at) : "")}</span>}
      {t.service_identity && (
        <span className="mt-1 block text-xs text-muted-foreground">
          {MINTED.SERVICE_IDENTITY} <span className="font-mono">{t.service_identity}</span>
        </span>
      )}
    </>
  );
}

export function AdminMintedTokens() {
  const [tokens, setTokens] = React.useState<AdminMintedToken[] | null>(null);
  const [failed, setFailed] = React.useState(false);
  const [includeRevoked, setIncludeRevoked] = React.useState(false);
  const [target, setTarget] = React.useState<AdminMintedToken | null>(null);
  const [revoking, setRevoking] = React.useState(false);
  // A ref as well as the state: two clicks in one tick must revoke once.
  const inFlight = React.useRef(false);

  // The list stays on screen while the toggle's request is out, so the toggle keeps its place and focus.
  // Only the newest read may answer: a slow earlier one must not overwrite the toggle's current choice.
  const newest = React.useRef(0);
  const load = React.useCallback(() => {
    const mine = ++newest.current;
    setFailed(false);
    credentialsApi
      .listAdminMintedTokens(includeRevoked)
      .then((rows) => { if (mine === newest.current) setTokens(rows); })
      .catch(() => { if (mine === newest.current) setFailed(true); });
  }, [includeRevoked]);
  React.useEffect(load, [load]);

  const live = tokens ? tokens.filter((t) => !t.revoked_at) : [];
  // The rows the toggle brings: the server lists revoked tokens and expired ones minted for others only when asked.
  const toggleShown = tokens ? tokens.filter((t) => t.revoked_at || isExpired(t)).length : 0;
  // "Still work" counts what authenticates: an expired token is listed, but is not one of them.
  const working = live.filter((t) => !isExpired(t)).length;
  const shown = includeRevoked ? (tokens ?? []) : live;

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

  const toggle = (
    <div className="mt-3 flex items-center gap-2">
      <Checkbox
        id="minted-include-revoked"
        checked={includeRevoked}
        onCheckedChange={(v) => setIncludeRevoked(v === true)}
      />
      <Label htmlFor="minted-include-revoked" className="text-xs font-normal">
        {MINTED.INCLUDE_REVOKED}
      </Label>
    </div>
  );

  return (
    <section className="mt-6 overflow-hidden rounded-xl border border-border bg-card" aria-labelledby="admin-minted-title">
      <div className="p-4">
        <div className="flex flex-wrap items-center gap-2">
          <h2 id="admin-minted-title" className="text-sm font-semibold text-foreground">
            {MINTED.TITLE}
          </h2>
          {working > 0 && <Chip tone="warning">{MINTED.CHIP(working)}</Chip>}
        </div>

        {failed ? (
          <ErrorState onRetry={load} />
        ) : !tokens ? (
          <TableSkeleton rows={2} cols={5} />
        ) : shown.length === 0 ? (
          <div className="mt-3 space-y-1">
            <p className="flex items-center gap-2 text-sm text-foreground">
              <KeyRound className="size-4 text-muted-foreground" aria-hidden /> {MINTED.EMPTY}
            </p>
            <p className="text-xs text-muted-foreground">{MINTED.NOTE}</p>
            {toggle}
          </div>
        ) : (
          <>
            <p className="mt-2 text-xs text-muted-foreground">{working === 0 ? MINTED.NONE_LIVE : MINTED.COUNT(working)}</p>
            <p className="text-xs text-muted-foreground">{MINTED.NOTE}</p>
            {toggle}
            {includeRevoked && toggleShown > 0 && (
              <p role="status" className="mt-1.5 text-xs text-muted-foreground">
                {MINTED.SHOWING_REVOKED(toggleShown)}
              </p>
            )}
            <div className="mt-3 overflow-x-auto scroll-thin">
              <Table>
                <TableHeader>
                  <TableRow className="hover:bg-transparent">
                    <TableHead>{MINTED.COL_PERSON}</TableHead>
                    <TableHead>{MINTED.COL_TOKEN}</TableHead>
                    <TableHead>{MINTED.COL_STATE}</TableHead>
                    <TableHead>{MINTED.COL_CREATED_BY}</TableHead>
                    <TableHead>{MINTED.COL_ADDED}</TableHead>
                    <TableHead>{MINTED.COL_LAST_USED}</TableHead>
                    <TableHead />
                  </TableRow>
                </TableHeader>
                <TableBody>
                  {shown.map((t) => (
                    <TableRow key={t.id}>
                      <TableCell className="whitespace-nowrap font-medium">{personOf(t)}</TableCell>
                      <TableCell className="font-mono text-xs">{t.name}</TableCell>
                      <TableCell>
                        <StateCell t={t} />
                      </TableCell>
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
                        {!t.revoked_at && (
                          <Button size="sm" variant="outline" onClick={() => setTarget(t)}>
                            {MINTED.REVOKE}
                          </Button>
                        )}
                      </TableCell>
                    </TableRow>
                  ))}
                </TableBody>
              </Table>
            </div>
            <p className="mt-3 text-xs text-muted-foreground">
              {MINTED.FOOTNOTE[0]}
              <span className="font-mono">{MINTED.FOOTNOTE[1]}</span>
              {MINTED.FOOTNOTE[2]}
              <span className="font-mono">{MINTED.FOOTNOTE[3]}</span>
              {MINTED.FOOTNOTE[4]}
            </p>
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
