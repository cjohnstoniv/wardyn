/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Admin -> Runners -> "Create a registration token" (M-RN-1 RA3, super admin): two steps in one
// dialog. Step 1 names the person; step 2 shows the token once with the register command. An admin can
// mint but never claim, so nothing here offers a claim. Closing step 2 before either copy control was
// used asks once (RA3b). The command's --org is the URL the server answered with, because
// wardyn-runnerd refuses an --org that does not hash to the server's; this page's own address is only
// the fallback.
import * as React from "react";
import { Loader2 } from "lucide-react";
import { runners as runnersApi } from "../../../lib/api/runners";
import { appURL } from "../../../lib/base-path";
import { absoluteTime, getErrorMessage } from "../../../lib/format";
import type { RunnerRegistrationToken } from "../../../lib/types/runners";
import { RUNNER_V6 } from "../../wardyn/copy/runner-pools";
import { RUNNER, RUNNERS } from "../../wardyn/copy/runners";
import { CopyButton } from "../../wardyn/copy-button";
import { DirectoryCombobox } from "../../wardyn/directory-combobox";
import { Field } from "../../wardyn/form-primitives";
import { Button } from "../../ui/button";
import {
  AlertDialog,
  AlertDialogCancel,
  AlertDialogContent,
  AlertDialogDescription,
  AlertDialogFooter,
  AlertDialogHeader,
  AlertDialogTitle,
} from "../../ui/alert-dialog";
import { Dialog, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle } from "../../ui/dialog";

const COPY_BUTTON = "gap-1.5 rounded-md border border-border bg-card px-2.5 py-1 text-xs text-foreground hover:bg-surface-2";

const orgOf = (token: RunnerRegistrationToken) => token.org_url || window.location.origin + appURL("");

/** The command a person runs on their own computer. */
export function registerCommand(token: RunnerRegistrationToken, tokenFile = "/path/to/token"): string {
  return `wardyn-runnerd register --org ${orgOf(token)} --token-file ${tokenFile}`;
}

/** The command that starts the registered runner. `register` takes no proxy image, so it is `serve` that names
 * the one the server answered with; null while the server does not say, because serve refuses to start without one. */
export function serveCommand(token: RunnerRegistrationToken): string | null {
  return token.proxy_image ? `wardyn-runnerd serve --org ${orgOf(token)} --proxy-image ${token.proxy_image}` : null;
}

/** The start command, when the server named the proxy image, and the note for VM-backed Docker. */
export function StartRunnerSteps({ token }: { token: RunnerRegistrationToken }) {
  const serve = serveCommand(token);
  return (
    <>
      {serve && (
        <section className="space-y-1.5">
          <p className="text-body text-muted-foreground">{RUNNER_V6.MINT_START_LEAD}</p>
          <div className="flex items-center justify-between gap-2 rounded-lg border border-border bg-surface-2/60 p-3">
            <code className="min-w-0 break-all font-mono text-xs">{serve}</code>
            <CopyButton text={serve} label={RUNNER.COPY_COMMAND} className={COPY_BUTTON}>
              {RUNNER.COPY_COMMAND}
            </CopyButton>
          </div>
        </section>
      )}
      <p className="text-xs text-muted-foreground">{RUNNER_V6.MINT_VM_DOCKER}</p>
    </>
  );
}

export function MintTokenDialog({
  open,
  onOpenChange,
  onMinted,
}: {
  open: boolean;
  onOpenChange: (open: boolean) => void;
  /** A token was created: the unused-token list changed. */
  onMinted: () => void;
}) {
  const [person, setPerson] = React.useState("");
  const [busy, setBusy] = React.useState(false);
  const [error, setError] = React.useState("");
  const [token, setToken] = React.useState<RunnerRegistrationToken | null>(null);
  const [asking, setAsking] = React.useState(false);
  // A copy control was pressed. The token cannot be read again, so closing without one asks first.
  const copied = React.useRef(false);
  const titleRef = React.useRef<HTMLSpanElement | null>(null);

  React.useEffect(() => {
    if (open) {
      setPerson("");
      setError("");
      setToken(null);
      setAsking(false);
      copied.current = false;
    }
  }, [open]);
  React.useEffect(() => {
    if (token) titleRef.current?.focus();
  }, [token]);

  const requestClose = () => {
    if (busy) return;
    if (token && !copied.current) setAsking(true);
    else onOpenChange(false);
  };

  const submit = async (e: React.FormEvent) => {
    e.preventDefault();
    setBusy(true);
    setError("");
    try {
      setToken(await runnersApi.mint(person.trim()));
      onMinted();
    } catch (err) {
      setError(getErrorMessage(err));
    } finally {
      setBusy(false);
    }
  };

  const owner = token?.owner ?? person.trim();
  return (
    <>
      <Dialog open={open} onOpenChange={(o) => !o && requestClose()}>
        <DialogContent className="sm:max-w-lg">
          {token ? (
            <>
              <DialogHeader>
                <DialogTitle>
                  <span ref={titleRef} tabIndex={-1} className="outline-none">
                    {RUNNERS.MINT_DONE_TITLE(owner)}
                  </span>
                </DialogTitle>
                <DialogDescription>{RUNNERS.MINT_SHOWN_ONCE}</DialogDescription>
              </DialogHeader>
              <div
                className="space-y-4"
                onClick={(e) => {
                  if ((e.target as Element).closest("button")) copied.current = true;
                }}
              >
                <section className="space-y-1.5">
                  <h3 className="label-eyebrow">{RUNNER.TOKEN_LABEL}</h3>
                  <div className="flex items-center justify-between gap-2 rounded-lg border border-border bg-surface-2/60 p-3">
                    <code className="min-w-0 break-all font-mono text-xs">{token.token}</code>
                    <CopyButton text={token.token ?? ""} label={RUNNER.COPY_TOKEN} className={COPY_BUTTON}>
                      {RUNNER.COPY_TOKEN}
                    </CopyButton>
                  </div>
                  <p className="text-meta text-muted-foreground">{RUNNER.TOKEN_EXPIRES(absoluteTime(token.expires_at))}</p>
                </section>
                <section className="space-y-1.5">
                  <p className="text-body text-muted-foreground">{RUNNERS.COMMAND_LEAD}</p>
                  <div className="flex items-center justify-between gap-2 rounded-lg border border-border bg-surface-2/60 p-3">
                    <code className="min-w-0 break-all font-mono text-xs">{registerCommand(token)}</code>
                    <CopyButton text={registerCommand(token)} label={RUNNER.COPY_COMMAND} className={COPY_BUTTON}>
                      {RUNNER.COPY_COMMAND}
                    </CopyButton>
                  </div>
                </section>
                <StartRunnerSteps token={token} />
                <p className="text-body text-foreground">{RUNNER_V6.MINT_ONE_PER_COMPUTER}</p>
                <p className="text-xs text-muted-foreground">{RUNNER_V6.MINT_ASKS_LIMITS}</p>
                <p className="text-body text-muted-foreground">{RUNNERS.MINT_NEXT(owner)}</p>
              </div>
              <DialogFooter>
                <Button type="button" onClick={requestClose}>
                  {RUNNER.DONE}
                </Button>
              </DialogFooter>
            </>
          ) : (
            <form onSubmit={submit} className="space-y-3">
              <DialogHeader>
                <DialogTitle>{RUNNERS.MINT_TITLE}</DialogTitle>
                <DialogDescription>{RUNNERS.MINT_LEAD}</DialogDescription>
              </DialogHeader>
              <Field label={RUNNERS.FIELD_PERSON} htmlFor="mint-runner-person" hint={RUNNERS.PERSON_HINT} required>
                <DirectoryCombobox id="mint-runner-person" kind="user" value={person} onChange={setPerson} disabled={busy} />
              </Field>
              {error && (
                <p role="alert" className="text-body text-danger">
                  {error}
                </p>
              )}
              <DialogFooter>
                <Button type="button" variant="ghost" disabled={busy} onClick={() => onOpenChange(false)}>
                  Cancel
                </Button>
                <Button type="submit" disabled={busy || person.trim() === ""}>
                  {busy && <Loader2 className="size-3.5 animate-spin" />}
                  {RUNNERS.MINT_SUBMIT}
                </Button>
              </DialogFooter>
            </form>
          )}
        </DialogContent>
      </Dialog>
      <AlertDialog open={asking} onOpenChange={setAsking}>
        <AlertDialogContent className="sm:max-w-md">
          <AlertDialogHeader>
            <AlertDialogTitle>{RUNNERS.CLOSE_UNCOPIED_TITLE}</AlertDialogTitle>
            <AlertDialogDescription>{RUNNERS.CLOSE_UNCOPIED_BODY}</AlertDialogDescription>
          </AlertDialogHeader>
          <AlertDialogFooter>
            <AlertDialogCancel asChild>
              <Button variant="ghost">{RUNNERS.KEEP_OPEN}</Button>
            </AlertDialogCancel>
            <Button
              onClick={() => {
                setAsking(false);
                onOpenChange(false);
              }}
            >
              {RUNNERS.CLOSE_ANYWAY}
            </Button>
          </AlertDialogFooter>
        </AlertDialogContent>
      </AlertDialog>
    </>
  );
}
