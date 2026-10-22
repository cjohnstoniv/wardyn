/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Your account: "Signed-in CLIs" (DESIGN-ATTACH 3.9 and 11.2, boards MATT3-*). One row per computer where the
// person signed in with `wardyn login`: the store it keeps the sign-in in, when it was signed in and last used,
// when it ends and when it would end idle, and a Sign out that ends that session now. Live sessions only; the
// card stays when there are none and shows the command. It is absent where CLI sign-in is not served (the
// routes are mounted only when the operator turned it on) and where there is no person (a token, local mode).
import * as React from "react";
import { toast } from "sonner";
import { basePath } from "../../../lib/base-path";
import { HttpError } from "../../../lib/api/core";
import { cliSignIn } from "../../../lib/api/cli-sign-in";
import { getErrorMessage } from "../../../lib/format";
import type { CLISession } from "../../../lib/types/cli-sign-in";
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
import { SIGNED_IN_CLIS as T } from "../../wardyn/copy/cli-sign-in";
import { MINTED } from "../../wardyn/copy/credentials";
import { inWords, SERVICES } from "../../wardyn/copy/service-tokens";
import { Chip } from "../../wardyn/primitives";
import { ErrorState, TableSkeleton } from "../../wardyn/states";

type Load = "loading" | "ready" | "error" | "hidden";

function loadFailure(e: unknown): Load {
  if (e instanceof HttpError) {
    if (e.status === 404 || e.reason === "needs_sso" || (e.status === 403 && e.reason === "api_token_no_human")) return "hidden";
  }
  return "error";
}

// A phone gets a card per computer: the same cells, each with its column word in front (data-label).
const CELL = "max-md:flex max-md:items-baseline max-md:justify-between max-md:gap-3 max-md:px-0 max-md:py-1 max-md:before:text-xs max-md:before:text-muted-foreground max-md:before:content-[attr(data-label)]";

export function SignedInClisCard() {
  const [state, setState] = React.useState<Load>("loading");
  const [rows, setRows] = React.useState<CLISession[]>([]);
  const [leaving, setLeaving] = React.useState<CLISession | null>(null);
  const [busy, setBusy] = React.useState(false);
  const card = React.useRef<HTMLElement>(null);

  const load = React.useCallback(() => {
    cliSignIn
      .listSessions()
      .then((r) => {
        setRows(r);
        setState("ready");
      })
      .catch((e: unknown) => setState(loadFailure(e)));
  }, []);
  React.useEffect(load, [load]);

  const signOut = async () => {
    if (!leaving || busy) return;
    setBusy(true);
    try {
      await cliSignIn.signOut(leaving.id);
      toast.success(T.TOAST(leaving.machine));
    } catch (e) {
      // Ended elsewhere in the meantime: the list below says so.
      if (e instanceof HttpError && e.reason === "cli_session_not_found") toast.success(T.TOAST(leaving.machine));
      else toast.error(getErrorMessage(e));
    } finally {
      setLeaving(null);
      setBusy(false);
      load();
    }
  };

  if (state === "hidden") return null;
  const url = typeof window !== "undefined" ? window.location.origin + basePath() : "";

  return (
    <CollapsibleCard
      ref={card}
      title={T.TITLE}
      summary={state === "ready" ? T.SUMMARY(rows.length) : undefined}
      testId="signed-in-clis-card"
    >
      {state === "loading" ? (
        <div role="status" aria-label={T.LOADING}>
          <span className="sr-only">{T.LOADING}</span>
          <TableSkeleton rows={2} cols={4} />
        </div>
      ) : state === "error" ? (
        <div role="group" aria-label={T.TABLE_LABEL}>
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
          <p className="text-xs text-muted-foreground">{T.LEDE}</p>
          {rows.length === 0 ? (
            <p className="text-body">{T.EMPTY}</p>
          ) : (
            <Table aria-label={T.TABLE_LABEL} className="max-md:block">
              <TableHeader className="max-md:sr-only">
                <TableRow className="hover:bg-transparent">
                  <TableHead>{T.COL_COMPUTER}</TableHead>
                  <TableHead>{T.COL_STORE}</TableHead>
                  <TableHead>{T.COL_SIGNED_IN}</TableHead>
                  <TableHead>{SERVICES.COL_LAST_USED}</TableHead>
                  <TableHead>{T.COL_ENDS}</TableHead>
                  <TableHead>{T.COL_IDLE_END}</TableHead>
                  <TableHead />
                </TableRow>
              </TableHeader>
              <TableBody className="max-md:block">
                {rows.map((s) => (
                  <Row key={s.id} s={s} onSignOut={() => setLeaving(s)} />
                ))}
              </TableBody>
            </Table>
          )}
          <p className="text-xs text-muted-foreground">
            {T.RUN_LEAD} <span className="font-mono text-foreground">{T.RUN_COMMAND(url)}</span>
          </p>
        </div>
      )}

      <AlertDialog open={!!leaving} onOpenChange={(o) => !o && !busy && setLeaving(null)}>
        <AlertDialogContent
          onCloseAutoFocus={(e) => {
            e.preventDefault();
            card.current?.querySelector<HTMLElement>("h3 button")?.focus();
          }}
        >
          <AlertDialogHeader>
            <AlertDialogTitle>{leaving ? T.DIALOG_TITLE(leaving.machine) : ""}</AlertDialogTitle>
            <AlertDialogDescription>{leaving ? T.DIALOG_BODY(leaving.machine) : ""}</AlertDialogDescription>
          </AlertDialogHeader>
          <AlertDialogFooter>
            <AlertDialogCancel disabled={busy}>{MINTED.REVOKE_CANCEL}</AlertDialogCancel>
            <AlertDialogAction
              className={buttonVariants({ variant: "destructive" })}
              disabled={busy}
              onClick={(e) => {
                e.preventDefault();
                void signOut();
              }}
            >
              {T.SIGN_OUT}
            </AlertDialogAction>
          </AlertDialogFooter>
        </AlertDialogContent>
      </AlertDialog>
    </CollapsibleCard>
  );
}

function adminMinutes(iso: string): number {
  return Math.max(1, Math.ceil((Date.parse(iso) - Date.now()) / 60_000));
}

function Row({ s, onSignOut }: { s: CLISession; onSignOut: () => void }) {
  return (
    <TableRow className="max-md:mb-2 max-md:block max-md:rounded-lg max-md:border max-md:p-3" data-session={s.id}>
      <TableCell className="max-md:block max-md:px-0">
        <span className="font-mono text-xs">{s.machine}</span>
        <span className="mt-1 flex flex-wrap items-center gap-2">
          <Chip tone="info">{T.CHIP}</Chip>
          {s.admin_expires_at && (
            <Chip tone="warning" dot>
              {T.ADMIN_CHIP(adminMinutes(s.admin_expires_at))}
            </Chip>
          )}
        </span>
        {s.store === "file" && <span className="mt-1 block text-xs text-muted-foreground">{T.FILE_NOTE}</span>}
      </TableCell>
      <TableCell data-label={T.COL_STORE} className={`text-muted-foreground ${CELL}`}>
        {s.store === "file" ? T.STORE_FILE : T.STORE_KEYCHAIN}
      </TableCell>
      <TableCell data-label={T.COL_SIGNED_IN} className={`text-muted-foreground ${CELL}`}>
        {inWords(s.created_at)}
      </TableCell>
      <TableCell data-label={SERVICES.COL_LAST_USED} className={`text-muted-foreground ${CELL}`}>
        {inWords(s.last_refresh_at)}
      </TableCell>
      <TableCell data-label={T.COL_ENDS} className={`text-muted-foreground ${CELL}`}>
        {inWords(s.expires_at)}
      </TableCell>
      <TableCell data-label={T.COL_IDLE_END} className={`text-muted-foreground ${CELL}`}>
        {inWords(s.idle_expires_at)}
      </TableCell>
      <TableCell className="max-md:block max-md:px-0 max-md:pt-2">
        <Button type="button" size="sm" variant="outline" className="max-md:h-11" aria-label={T.SIGN_OUT_LABEL(s.machine)} onClick={onSignOut}>
          {T.SIGN_OUT}
        </Button>
      </TableCell>
    </TableRow>
  );
}
