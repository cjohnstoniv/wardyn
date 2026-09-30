/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// "Add your personal access token" (#1430): for an organisation whose row takes
// a token each person creates in Azure DevOps and pastes here. Wardyn cannot
// read a pasted token's scopes or expiry, so the dialog lists what to create
// (in Azure DevOps' own wording) and asks for the expiry they chose. The server
// checks the token belongs to them; each refusal sits under the field it is
// about, and the message never names the other account.
import * as React from "react";
import { toast } from "sonner";
import { adoPat, OWN_TOKEN_REASON } from "../../../lib/api/ado-pat";
import { HttpError } from "../../../lib/api/core";
import { ADO_PAT } from "../../../lib/ado-pat-copy";
import { adoOrgLabel, adoTokensURL } from "../../../lib/ado-pat-display";
import { getErrorMessage } from "../../../lib/format";
import { Button } from "../../ui/button";
import { Dialog, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle } from "../../ui/dialog";
import { Input } from "../../ui/input";
import { Label } from "../../ui/label";

export function AdoOwnTokenDialog({
  open,
  onOpenChange,
  address,
  days,
  scopes,
  onStored,
}: {
  open: boolean;
  onOpenChange: (open: boolean) => void;
  /** The row's Azure DevOps address (SCMAccess.org). */
  address: string;
  days: number;
  /** What to tick on Azure DevOps' token page, in its own wording, as the server
   *  sends it (SCMAccess.token_scopes). */
  scopes: string[] | undefined;
  onStored: () => void;
}) {
  const uid = React.useId();
  const [token, setToken] = React.useState("");
  const [expires, setExpires] = React.useState("");
  const [busy, setBusy] = React.useState(false);
  const [tokenError, setTokenError] = React.useState<string | null>(null);
  const [expiresError, setExpiresError] = React.useState<string | null>(null);

  const reset = () => {
    setToken("");
    setExpires("");
    setTokenError(null);
    setExpiresError(null);
  };

  const submit = async () => {
    setBusy(true);
    setTokenError(null);
    setExpiresError(null);
    try {
      await adoPat.storeOwnToken({ org: address, token, expires_on: expires });
      reset();
      onOpenChange(false);
      onStored();
    } catch (e) {
      const reason = e instanceof HttpError ? e.reason : "";
      if (reason === OWN_TOKEN_REASON.MISMATCH) setTokenError(ADO_PAT.OWN_MISMATCH);
      else if (reason === OWN_TOKEN_REASON.REJECTED) setTokenError(ADO_PAT.OWN_REJECTED);
      else if (reason === OWN_TOKEN_REASON.TOO_LONG) setExpiresError(ADO_PAT.OWN_TOO_LONG(days));
      // Any other refusal (an empty or spaced token, a date not after today, Azure
      // DevOps not reachable) is the server's own sentence, said as it says it.
      else toast.error(getErrorMessage(e));
    } finally {
      setBusy(false);
    }
  };

  const labels = scopes ?? [];
  const tokensURL = adoTokensURL(address);
  return (
    <Dialog
      open={open}
      onOpenChange={(next) => {
        if (!next) reset();
        onOpenChange(next);
      }}
    >
      <DialogContent>
        <DialogHeader>
          <DialogTitle>{ADO_PAT.OWN_DIALOG_TITLE}</DialogTitle>
          <DialogDescription>{ADO_PAT.OWN_DIALOG_LEAD(adoOrgLabel(address), days)}</DialogDescription>
        </DialogHeader>
        {labels.length > 0 && (
          <ul className="list-disc space-y-0.5 pl-5 text-body text-foreground">
            {labels.map((l) => (
              <li key={l}>{l}</li>
            ))}
          </ul>
        )}
        {tokensURL && (
          <p className="text-body">
            <a href={tokensURL} target="_blank" rel="noopener noreferrer" className="font-medium text-info hover:underline">
              {ADO_PAT.OWN_OPEN_TOKENS}
            </a>
          </p>
        )}
        <div className="space-y-1">
          <Label htmlFor={`${uid}-token`} className="text-meta font-medium text-muted-foreground">
            {ADO_PAT.OWN_FIELD_TOKEN}
          </Label>
          <Input
            id={`${uid}-token`}
            type="password"
            autoComplete="off"
            className="font-mono"
            aria-invalid={!!tokenError}
            value={token}
            onChange={(e) => setToken(e.target.value)}
          />
          {tokenError && <p className="text-meta text-danger">{tokenError}</p>}
        </div>
        <div className="space-y-1">
          <Label htmlFor={`${uid}-expires`} className="text-meta font-medium text-muted-foreground">
            {ADO_PAT.OWN_FIELD_EXPIRES}
          </Label>
          <Input
            id={`${uid}-expires`}
            type="date"
            className="max-w-[12rem]"
            aria-invalid={!!expiresError}
            value={expires}
            onChange={(e) => setExpires(e.target.value)}
          />
          {expiresError && <p className="text-meta text-danger">{expiresError}</p>}
        </div>
        <DialogFooter>
          <Button type="button" variant="outline" onClick={() => onOpenChange(false)}>
            {ADO_PAT.OWN_DIALOG_CANCEL}
          </Button>
          <Button type="button" disabled={busy || !token.trim() || !expires} onClick={() => void submit()}>
            {ADO_PAT.OWN_DIALOG_ADD}
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}
