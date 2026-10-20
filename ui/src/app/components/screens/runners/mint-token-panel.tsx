/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Admin -> Runners -> Add this computer -> "Managed install (MDM)" (M-ATT-4, formerly the "Create a registration
// token" dialog, M-RN-1 RA3, super admin): two steps in one panel. Step 1 names the person; step 2 shows the token
// once with the command a managed computer runs. An admin can mint but never claim, so nothing here offers a claim.
// Leaving step 2 before either copy control was used asks once (RA3b). The command's --org is the URL the server
// answered with, because wardyn-runnerd refuses an --org that does not hash to the server's; this page's own address
// is only the fallback. The start command and the proxy image are not shown: `setup` installs the service.
import * as React from "react";
import { Loader2 } from "lucide-react";
import { runners as runnersApi } from "../../../lib/api/runners";
import { appURL } from "../../../lib/base-path";
import { absoluteTime, getErrorMessage } from "../../../lib/format";
import type { RunnerRegistrationToken } from "../../../lib/types/runners";
import { ADD_COMPUTER_COMMANDS } from "../../wardyn/copy/add-computer";
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

const COPY_BUTTON = "gap-1.5 rounded-md border border-border bg-card px-2.5 py-1 text-xs text-foreground hover:bg-surface-2";

const orgOf = (token: RunnerRegistrationToken) => token.org_url || window.location.origin + appURL("");

/** The command a managed computer runs, with the token delivered as a file. */
export const managedCommand = (token: RunnerRegistrationToken) => ADD_COMPUTER_COMMANDS.mdm(orgOf(token));

export function MintTokenPanel({
  onMinted,
}: {
  /** A token was created: the unused-token list changed. */
  onMinted: () => void;
}) {
  const [person, setPerson] = React.useState("");
  const [busy, setBusy] = React.useState(false);
  const [error, setError] = React.useState("");
  const [token, setToken] = React.useState<RunnerRegistrationToken | null>(null);
  const [asking, setAsking] = React.useState(false);
  // A copy control was pressed. The token cannot be read again, so leaving without one asks first.
  const copied = React.useRef(false);
  const titleRef = React.useRef<HTMLHeadingElement | null>(null);

  React.useEffect(() => {
    if (token) titleRef.current?.focus();
  }, [token]);

  const reset = () => {
    setPerson("");
    setError("");
    setToken(null);
    setAsking(false);
    copied.current = false;
  };
  const requestDone = () => {
    if (token && !copied.current) setAsking(true);
    else reset();
  };

  const submit = async (e: React.FormEvent) => {
    e.preventDefault();
    setBusy(true);
    setError("");
    try {
      setToken(await runnersApi.mint(person.trim()));
      copied.current = false;
      onMinted();
    } catch (err) {
      setError(getErrorMessage(err));
    } finally {
      setBusy(false);
    }
  };

  const owner = token?.owner ?? person.trim();
  const command = token ? managedCommand(token) : "";
  return (
    <>
      {token ? (
        <div className="space-y-4">
          <div
            className="space-y-4"
            onClick={(e) => {
              if ((e.target as Element).closest("button")) copied.current = true;
            }}
          >
            <div className="space-y-1">
              <h3 ref={titleRef} tabIndex={-1} className="text-sm font-semibold text-foreground outline-none">
                {RUNNERS.MINT_DONE_TITLE(owner)}
              </h3>
              <p className="text-body text-muted-foreground">{RUNNERS.MINT_SHOWN_ONCE}</p>
            </div>
            <section className="space-y-1.5">
              <h4 className="label-eyebrow">{RUNNER.TOKEN_LABEL}</h4>
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
                <code className="min-w-0 break-all font-mono text-xs">{command}</code>
                <CopyButton text={command} label={RUNNER.COPY_COMMAND} className={COPY_BUTTON}>
                  {RUNNER.COPY_COMMAND}
                </CopyButton>
              </div>
            </section>
            <p className="text-body text-foreground">{RUNNER_V6.MINT_ONE_PER_COMPUTER}</p>
            <p className="text-body text-muted-foreground">{RUNNERS.MINT_NEXT(owner)}</p>
          </div>
          {/* Outside the copy-detecting wrapper: pressing Done is not a copy. */}
          <div className="flex justify-end">
            <Button type="button" variant="outline" size="sm" onClick={requestDone}>
              {RUNNER.DONE}
            </Button>
          </div>
        </div>
      ) : (
        <form onSubmit={submit} className="space-y-3">
          <p className="text-body text-muted-foreground">{RUNNERS.MINT_LEAD}</p>
          <Field label={RUNNERS.FIELD_PERSON} htmlFor="mint-runner-person" hint={RUNNERS.PERSON_HINT} required>
            <DirectoryCombobox id="mint-runner-person" kind="user" value={person} onChange={setPerson} disabled={busy} />
          </Field>
          {error && (
            <p role="alert" className="text-body text-danger">
              {error}
            </p>
          )}
          <Button type="submit" disabled={busy || person.trim() === ""}>
            {busy && <Loader2 className="size-3.5 animate-spin" />}
            {RUNNERS.MINT_SUBMIT}
          </Button>
        </form>
      )}
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
            <Button onClick={reset}>{RUNNERS.CLOSE_ANYWAY}</Button>
          </AlertDialogFooter>
        </AlertDialogContent>
      </AlertDialog>
    </>
  );
}
