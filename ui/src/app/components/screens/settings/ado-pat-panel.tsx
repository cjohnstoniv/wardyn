/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The body of the person's Azure DevOps card on a row that creates tokens or
// takes a pasted one (#1428, #1430): the state's lines, and the one action it
// offers: connect, disconnect (with its confirmation), or add or replace an
// own token. ado-connection.tsx keeps the card's shell and its two homes; the
// states and their words are ado-pat-display.ts's and ado-pat-copy.ts's.
import * as React from "react";
import { toast } from "sonner";
import { adoPat } from "../../../lib/api/ado-pat";
import { ADO } from "../../../lib/ado-entra-copy";
import { ADO_PAT } from "../../../lib/ado-pat-copy";
import { getErrorMessage } from "../../../lib/format";
import { adoOrgLabel, adoTokensURL, type PatCardView, type PatTone } from "../../../lib/ado-pat-display";
import type { SCMAccessPAT } from "../../../lib/types/ado-pat";
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
import { AdoOwnTokenDialog } from "./ado-own-token-dialog";

export const patToneClass: Record<PatTone, string> = {
  success: "text-success",
  warning: "text-warning",
  danger: "text-danger",
  neutral: "text-muted-foreground",
};

export function AdoPatBody({
  access,
  view,
  connecting,
  onConnect,
  blockedUrl,
  onFallbackClick,
  onChanged,
}: {
  access: SCMAccessPAT;
  view: PatCardView;
  connecting: boolean;
  onConnect: () => void;
  blockedUrl: string | null;
  onFallbackClick: () => void;
  onChanged: () => void;
}) {
  const [confirmDisconnect, setConfirmDisconnect] = React.useState(false);
  const [tokenDialog, setTokenDialog] = React.useState(false);
  const [confirmRemove, setConfirmRemove] = React.useState(false);
  const [removing, setRemoving] = React.useState(false);
  // A ref as well as the state: two clicks in one tick both see removing ===
  // false until React re-renders, and the DELETE must go once.
  const removeInFlight = React.useRef(false);
  const org = adoOrgLabel(access.org ?? "");
  const tokensURL = adoTokensURL(access.org ?? "");

  const disconnect = async () => {
    setConfirmDisconnect(false);
    try {
      await adoPat.disconnect();
      onChanged();
    } catch (e) {
      toast.error(getErrorMessage(e));
    }
  };

  // Remove from Wardyn (#1488): the dialog stays open and says Removing… while
  // the DELETE runs, then closes, and the outcome is a toast. Any failure is one
  // sentence (a 404 from an older daemon included) and the card is left as it
  // was: never a "removed" that did not happen. A success reloads, which also
  // settles a stale card whose token was already gone (a 204).
  const removeToken = async () => {
    if (removeInFlight.current) return;
    removeInFlight.current = true;
    setRemoving(true);
    try {
      await adoPat.removeOwnToken(access.org ?? "");
      setConfirmRemove(false);
      toast.success(ADO_PAT.OWN_REMOVED_TOAST(org));
      onChanged();
    } catch {
      setConfirmRemove(false);
      toast.error(ADO_PAT.OWN_REMOVE_FAILED_TOAST(org));
    } finally {
      removeInFlight.current = false;
      setRemoving(false);
    }
  };

  return (
    <div className="space-y-2 text-body" data-testid="ado-pat-card">
      {view.body.map((line) => (
        <p key={line} className="text-muted-foreground">
          {line}
        </p>
      ))}
      <div className="flex flex-wrap items-center gap-2 pt-1">
        {view.action === "connect" && (
          <Button size="sm" disabled={connecting} onClick={onConnect}>
            {ADO_PAT.MEMBER_CONNECT}
          </Button>
        )}
        {view.action === "add_token" && (
          <Button size="sm" onClick={() => setTokenDialog(true)}>
            {ADO_PAT.OWN_ADD_CTA}
          </Button>
        )}
        {view.action === "replace_token" && (
          <Button size="sm" variant={view.refused ? "default" : "outline"} onClick={() => setTokenDialog(true)}>
            {ADO_PAT.OWN_REPLACE}
          </Button>
        )}
        {view.remove && (
          <Button size="sm" variant="outline" onClick={() => setConfirmRemove(true)}>
            {ADO_PAT.OWN_REMOVE}
          </Button>
        )}
        {view.disconnect && (
          <Button size="sm" variant="outline" onClick={() => setConfirmDisconnect(true)}>
            {ADO_PAT.MEMBER_DISCONNECT}
          </Button>
        )}
      </div>
      {view.action === "connect" && blockedUrl && (
        <p className="text-xs text-muted-foreground">
          {ADO.CONNECT_POPUP_BLOCKED}{" "}
          <a
            href={blockedUrl}
            target="_blank"
            rel="noopener noreferrer"
            className={buttonVariants({ variant: "outline", size: "sm" })}
            onClick={onFallbackClick}
          >
            {ADO.CONNECT_POPUP_OPEN}
          </a>
        </p>
      )}

      <AlertDialog open={confirmDisconnect} onOpenChange={setConfirmDisconnect}>
        <AlertDialogContent>
          <AlertDialogHeader>
            <AlertDialogTitle>{ADO_PAT.DISCONNECT_TITLE}</AlertDialogTitle>
            <AlertDialogDescription>{ADO_PAT.DISCONNECT_BODY}</AlertDialogDescription>
          </AlertDialogHeader>
          <AlertDialogFooter>
            <AlertDialogCancel>{ADO_PAT.DISCONNECT_CANCEL}</AlertDialogCancel>
            <AlertDialogAction className={buttonVariants({ variant: "destructive" })} onClick={() => void disconnect()}>
              {ADO_PAT.MEMBER_DISCONNECT}
            </AlertDialogAction>
          </AlertDialogFooter>
        </AlertDialogContent>
      </AlertDialog>

      <AlertDialog open={confirmRemove} onOpenChange={(o) => !removing && setConfirmRemove(o)}>
        <AlertDialogContent>
          <AlertDialogHeader>
            <AlertDialogTitle>{ADO_PAT.OWN_REMOVE_TITLE(org)}</AlertDialogTitle>
            <AlertDialogDescription>{ADO_PAT.OWN_REMOVE_BODY}</AlertDialogDescription>
          </AlertDialogHeader>
          {tokensURL && (
            <p className="text-body">
              <a href={tokensURL} target="_blank" rel="noopener noreferrer" className="font-medium text-info hover:underline">
                {ADO_PAT.OWN_OPEN_TOKENS}
              </a>
            </p>
          )}
          <p className="text-body text-muted-foreground">{ADO_PAT.OWN_REMOVE_RUNS}</p>
          <AlertDialogFooter>
            <AlertDialogCancel disabled={removing}>{ADO_PAT.OWN_REMOVE_CANCEL}</AlertDialogCancel>
            <AlertDialogAction
              className={buttonVariants({ variant: "destructive" })}
              disabled={removing}
              onClick={(e) => {
                e.preventDefault();
                void removeToken();
              }}
            >
              {removing ? ADO_PAT.OWN_REMOVE_PENDING : ADO_PAT.OWN_REMOVE}
            </AlertDialogAction>
          </AlertDialogFooter>
        </AlertDialogContent>
      </AlertDialog>

      {(view.action === "add_token" || view.action === "replace_token") && (
        <AdoOwnTokenDialog
          open={tokenDialog}
          onOpenChange={setTokenDialog}
          address={access.org ?? ""}
          days={access.max_days ?? 30}
          scopes={access.token_scopes}
          onStored={onChanged}
        />
      )}
    </div>
  );
}
