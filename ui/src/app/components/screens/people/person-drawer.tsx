/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The People page's per-person drawer (0.8.6 ppl-p2, mock M12 D3): one section per existing door, each
// destructive action behind a confirm that names the person. The actions are the doors that already
// exist: POST /sessions/revoke, DELETE /people/{p}/ssh-keys, DELETE /people/{p}/credentials, plus the
// token list. Nothing here is a new server path.
import * as React from "react";
import { Link } from "react-router-dom";
import { Loader2, RotateCw } from "lucide-react";
import { accountToast, signInGeneration } from "../../../lib/account-toasts";
import { people as peopleApi } from "../../../lib/api/people";
import { getErrorMessage, relativeTime } from "../../../lib/format";
import type { PersonSummary, PersonToken, PortalUsed } from "../../../lib/types";
import { useDeferredBusy } from "../../../lib/use-deferred-busy";
import { PEOPLE_PAGE as P } from "../../wardyn/copy/people";
import { Chip } from "../../wardyn/primitives";
import { answeredSentence, STATES } from "../../wardyn/states";
import { personState, roleLabel, STATE_LABEL, STATE_TONE } from "./people-format";
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
import { EraseDataDialog } from "../erase-data-dialog";
import { Sheet, SheetContent, SheetHeader, SheetTitle } from "../../ui/sheet";

type Action = "sign_out" | "ssh";

// The directory's API-token count (store_people_directory.go) counts the same rows: neither revoked nor expired.
const isActiveToken = (t: PersonToken) => !t.revoked_at && (!t.expires_at || Date.parse(t.expires_at) > Date.now());

function Section({ label, value, children }: { label: string; value: React.ReactNode; children?: React.ReactNode }) {
  return (
    <div className="flex items-center justify-between gap-3 border-b border-border py-3">
      <div>
        <div className="text-body font-medium text-foreground">{label}</div>
        <div className="text-meta text-muted-foreground">{value}</div>
      </div>
      {children}
    </div>
  );
}

export function PersonDrawer({
  person,
  onClose,
  onChanged,
}: {
  person: PersonSummary | null;
  onClose: () => void;
  onChanged: () => void;
}) {
  const [confirm, setConfirm] = React.useState<Action | null>(null);
  const [eraseOpen, setEraseOpen] = React.useState(false);
  const [busy, setBusy] = React.useState(false);
  const [tokens, setTokens] = React.useState<PersonToken[]>([]);
  // A swallowed rejection left the section showing the server's count with no names under it, which
  // reads as "no tokens" — so the read carries its own state and Retry refetches.
  const [tokenState, setTokenState] = React.useState<"loading" | "error" | "ready">("loading");
  const [tokenError, setTokenError] = React.useState<unknown>(null);
  // Retry bumps this rather than calling the API again: the effect below owns the fetch (and its
  // live guard), so there is one read path for the drawer.
  const [attempt, setAttempt] = React.useState(0);
  const principal = person?.principal;
  // Only a portal person has portals. The read is for this one person, so another person on the same issuer
  // never lists here.
  const portalPrincipal = person?.issuer ? principal : undefined;
  // The answer carries the person it is for: until this person's read answers, nothing of an earlier person's
  // answer (or an empty list) is shown, not even for the render before the effect starts the read.
  const [portalRead, setPortalRead] = React.useState<
    { for: string; failed: false; portals: PortalUsed[] } | { for: string; failed: true; error: unknown } | null
  >(null);
  const thisPortalRead = portalRead?.for === portalPrincipal ? portalRead : null;
  const portalState = !thisPortalRead ? "loading" : thisPortalRead.failed ? "error" : "ready";
  const portals = thisPortalRead && !thisPortalRead.failed ? thisPortalRead.portals : [];
  const portalError = thisPortalRead?.failed ? thisPortalRead.error : null;
  const [portalAttempt, setPortalAttempt] = React.useState(0);
  const { showSpinner } = useDeferredBusy(tokenState === "loading");

  React.useEffect(() => {
    setConfirm(null);
    setEraseOpen(false);
    setTokens([]);
    if (!principal) return;
    let live = true;
    setTokenState("loading");
    peopleApi
      .tokens(principal)
      .then((t) => {
        if (!live) return;
        setTokens(t.filter(isActiveToken));
        setTokenState("ready");
      })
      .catch((e: unknown) => {
        if (!live) return;
        setTokenError(e);
        setTokenState("error");
      });
    return () => {
      live = false;
    };
  }, [principal, attempt]);

  React.useEffect(() => {
    setPortalRead(null);
    if (!portalPrincipal) return;
    let live = true;
    peopleApi
      .portals(portalPrincipal)
      .then((v) => {
        if (!live) return;
        setPortalRead({ for: portalPrincipal, failed: false, portals: v.portals_used });
      })
      .catch((e: unknown) => {
        if (!live) return;
        setPortalRead({ for: portalPrincipal, failed: true, error: e });
      });
    return () => {
      live = false;
    };
  }, [portalPrincipal, portalAttempt]);

  if (!person) return null;
  const label = person.email || person.principal;
  const message: Record<Action, { title: string; confirm: string; run: () => Promise<unknown>; button: string; done: (result: unknown) => string }> = {
    sign_out: {
      title: P.SIGN_OUT_TITLE,
      confirm: P.SIGN_OUT_CONFIRM(label),
      run: () => peopleApi.signOutEverywhere(person.principal),
      button: P.SIGN_OUT,
      done: () => P.SIGN_OUT_DONE(label),
    },
    ssh: {
      title: P.SSH_REMOVE_TITLE,
      confirm: P.SSH_REMOVE_CONFIRM(label),
      run: () => peopleApi.removeSSHKeys(person.principal),
      button: P.SSH_REMOVE,
      done: (n) => P.SSH_REMOVE_DONE(Number(n), label),
    },
  };

  const run = async (action: Action) => {
    setBusy(true);
    const startedUnder = signInGeneration();
    try {
      const result = await message[action].run();
      accountToast("success", message[action].done(result), startedUnder);
      setConfirm(null);
      onChanged();
    } catch (e) {
      accountToast("error", getErrorMessage(e), startedUnder);
    } finally {
      setBusy(false);
    }
  };

  return (
    <>
      <Sheet open onOpenChange={(o) => !o && onClose()}>
        <SheetContent className="scroll-thin w-full gap-0 overflow-y-auto sm:max-w-[480px]">
          <SheetHeader className="border-b border-border pb-4">
            <SheetTitle className="break-words">
              {label}
              {person.role ? ` · ${roleLabel(person.role)}` : ""}
              {person.issuer && (
                <>
                  {" "}
                  <Chip tone="neutral">{P.PORTAL_PERSON}</Chip>
                </>
              )}
            </SheetTitle>
          </SheetHeader>
          <div className="px-4">
            <dl className="grid grid-cols-[8rem_minmax(0,1fr)] gap-x-4 border-b border-border py-3 text-body">
              <dt className="text-muted-foreground">{P.COL_STATE}</dt>
              <dd>
                <Chip tone={STATE_TONE[personState(person)]}>{STATE_LABEL[personState(person)]}</Chip>
              </dd>
            </dl>
            {person.issuer && (
              <dl className="grid grid-cols-[8rem_minmax(0,1fr)] gap-x-4 gap-y-1 border-b border-border py-3 text-body">
                <dt className="text-muted-foreground">{P.COL_ISSUER}</dt>
                <dd className="select-text break-all font-mono text-xs">{person.issuer}</dd>
                <dt className="text-muted-foreground">{P.COL_INTERNAL}</dt>
                <dd className="select-text break-all font-mono text-xs">
                  {person.internal_address || <span className="font-sans text-sm text-muted-foreground">{P.INTERNAL_UNSET}</span>}
                </dd>
              </dl>
            )}
            {person.issuer && (
              <div className="border-b border-border py-3" aria-label={P.PORTALS} role="group">
                <div className="text-body font-medium text-foreground">{P.PORTALS}</div>
                {portalState === "loading" && <Loader2 className="mt-1 size-3.5 animate-spin text-muted-foreground" aria-hidden />}
                {portalState === "error" && (
                  <div role="status" className="flex flex-wrap items-center gap-2">
                    <p className="text-meta text-muted-foreground">{answeredSentence(portalError, P.PORTALS_LOAD_FAILED) ?? STATES.ERROR_DEFAULT}</p>
                    <Button size="sm" variant="outline" onClick={() => setPortalAttempt((n) => n + 1)}>
                      <RotateCw className="size-3.5" />
                      {STATES.RETRY}
                    </Button>
                  </div>
                )}
                {portalState === "ready" && portals.length === 0 && <div className="text-meta text-muted-foreground">{P.PORTALS_NONE}</div>}
                {portalState === "ready" && portals.length > 0 && (
                  <ul className="mt-1 space-y-1">
                    {portals.map((u) => (
                      <li key={u.delegate} className="text-meta">
                        <span className={u.revoked_at ? "text-muted-foreground line-through" : "text-foreground"}>{u.name}</span>
                        <span className="text-muted-foreground">
                          {" · "}
                          {u.run_in_under ? <span className="select-text break-all font-mono">{P.PORTAL_UNIT(u.run_in_under)}</span> : P.PORTAL_NO_UNIT}
                        </span>
                        {u.revoked_at && <span className="ml-2"><Chip tone="warning">{P.PORTAL_REVOKED(relativeTime(u.revoked_at))}</Chip></span>}
                      </li>
                    ))}
                  </ul>
                )}
              </div>
            )}
            <Section label={P.SESSIONS} value={P.SESSIONS_ACTIVE(person.active_sessions)}>
              <Button size="sm" variant="outline" onClick={() => setConfirm("sign_out")}>
                {P.SIGN_OUT}
              </Button>
            </Section>
            <Section label={P.TOKENS} value={person.api_tokens}>
              {tokenState === "loading" && showSpinner && (
                <Loader2 className="size-3.5 animate-spin text-muted-foreground" aria-hidden />
              )}
              {/* The shared inline error + Retry, as the default-policy preview and the run
                  sign-in strip render it: a section of a screen, not a pane, so not ErrorState.
                  Same sentence that screen's own failed read already shows. */}
              {tokenState === "error" && (
                <div role="status" className="flex flex-wrap items-center gap-2">
                  <p className="text-meta text-muted-foreground">{answeredSentence(tokenError, P.TOKENS_LOAD_FAILED) ?? STATES.ERROR_DEFAULT}</p>
                  <Button size="sm" variant="outline" onClick={() => setAttempt((n) => n + 1)}>
                    <RotateCw className="size-3.5" />
                    {STATES.RETRY}
                  </Button>
                </div>
              )}
              {tokenState === "ready" && tokens.length > 0 && (
                <ul className="text-meta text-muted-foreground">
                  {tokens.map((t) => (
                    <li key={t.id}>
                      {t.name}
                      {t.last_used_at ? ` · ${relativeTime(t.last_used_at)}` : ""}
                    </li>
                  ))}
                </ul>
              )}
            </Section>
            {/* A portal person cannot hold a personal API token: the invitation would be refused. */}
            {!person.issuer && (
              <p className="border-b border-border py-2 text-meta text-muted-foreground">
                {P.INVITE_LEAD} <code className="font-mono text-foreground [overflow-wrap:anywhere]">{P.INVITE_CMD(person.principal)}</code>.
              </p>
            )}
            <Section label={P.SSH} value={person.ssh_keys}>
              <Button size="sm" variant="outline" disabled={person.ssh_keys === 0} onClick={() => setConfirm("ssh")}>
                {P.SSH_REMOVE}
              </Button>
            </Section>
            <Section label={P.CREDS} value={person.credentials}>
              <Button size="sm" variant="outline" disabled={person.credentials === 0} onClick={() => setEraseOpen(true)}>
                {P.CREDS_ERASE}
              </Button>
            </Section>
            <Section label={P.RUNS} value={P.RUNNING_COUNT(person.active_runs)}>
              <Link to={`/admin/runs?q=${encodeURIComponent(person.principal)}`} className="text-body text-info hover:underline">
                {P.RUNS_LINK} →
              </Link>
            </Section>
          </div>
        </SheetContent>
      </Sheet>

      <EraseDataDialog
        open={eraseOpen}
        person={{ principal: person.principal, label }}
        onOpenChange={setEraseOpen}
        onErased={onChanged}
      />

      <AlertDialog open={confirm !== null} onOpenChange={(o) => !o && !busy && setConfirm(null)}>
        <AlertDialogContent>
          {confirm && (
            <>
              <AlertDialogHeader>
                <AlertDialogTitle>{message[confirm].title}</AlertDialogTitle>
                <AlertDialogDescription>{message[confirm].confirm}</AlertDialogDescription>
              </AlertDialogHeader>
              <AlertDialogFooter>
                <AlertDialogCancel disabled={busy}>Cancel</AlertDialogCancel>
                <Button variant="destructive" disabled={busy} onClick={() => run(confirm)}>
                  {busy && <Loader2 className="size-3.5 animate-spin" />}
                  {message[confirm].button}
                </Button>
              </AlertDialogFooter>
            </>
          )}
        </AlertDialogContent>
      </AlertDialog>
    </>
  );
}
