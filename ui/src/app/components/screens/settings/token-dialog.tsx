/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The one dialog both token cards end in (#1508, mock M-SVC v2): the form that asks for a name and a lifetime,
// then the show-once reveal of the plaintext. A claim ("Claim your token") and a mint ("Mint a token for
// {name}") differ only in their wording and in the request the card hands in.
//
// The token lives only in this component's state. The card renders the dialog while it is open and drops it
// on close, so the plaintext goes with the component: it is never in a card's state, a prop, the URL, the
// storage or a toast, and nothing can read it back. `submit` is the card's request; the dialog keeps its answer.
//
// Close rules, as drawn: Esc, Done and the X behave alike. With nothing copied the first press only warns
// and the token stays; the second closes. A click outside never closes a token. A refusal keeps the dialog
// open with the server's sentence; a stale answer leaves only Close.
import * as React from "react";
import { Button } from "../../ui/button";
import { Dialog, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle } from "../../ui/dialog";
import { Input } from "../../ui/input";
import { HttpError } from "../../../lib/api/core";
import type { ApiToken } from "../../../lib/types/service-identities";
import { useCopyToClipboard } from "../../../lib/use-copy-to-clipboard";
import { Field } from "../../wardyn/form-primitives";
import { STATES } from "../../wardyn/states";
import { dayOf, inWords, lifetimeWords, ROLE_WORDS, TOKEN_DIALOG as T } from "../../wardyn/copy/service-tokens";

const DAY = 86_400;
const DEFAULT_LIFETIME = 90 * DAY;
// The server's reason for a name it refuses; the field is marked invalid, the sentence stays the server's.
const NAME_INVALID_REASON = "api_token_name_invalid";
const SELECT_CLASS =
  "border-input bg-input-background dark:bg-input/30 focus-visible:border-ring focus-visible:ring-ring h-9 w-full rounded-md border px-3 text-base outline-none focus-visible:ring-[3px] md:text-sm";

/** The request body of a claim or a mint. `ttl_seconds` is absent for "Longest the server allows". */
export interface TokenRequest {
  name: string;
  ttl_seconds?: number;
}

export interface TokenDialogProps {
  kind: "claim" | "mint";
  /** The service identity's name (mint). */
  service?: string;
  /** The invitation's token lifetime limit in seconds (claim); absent when it sets none. */
  limitSeconds?: number;
  /** The name of the token a claim revokes (claim); absent when it replaces none. */
  replaces?: string;
  initialName: string;
  /** Runs the request. The dialog keeps the answer; the card must not. */
  submit: (body: TokenRequest) => Promise<ApiToken>;
  /** The request succeeded. Carries nothing: the card refreshes its list. */
  onCreated: () => void;
  /** A request was refused. True makes the dialog final (only Close), for an answer that will not change. */
  onRefused: (e: HttpError) => boolean | Promise<boolean>;
  /** The dialog is closed; `created` says whether a token was made. Carries nothing else. */
  onClose: (created: boolean) => void;
  /** Where focus goes when the dialog closes: the button that opened it, or the card's heading. */
  restoreFocus: () => void;
}

interface Reveal {
  token: string;
  name: string;
  expiresAt: string | null;
  /** "service:ci-nightly" for a mint; null for a claim, which acts as the claimer. */
  principal: string | null;
  role: string;
  clamped: boolean;
}

interface Refusal {
  text: string;
  nameInvalid: boolean;
  final: boolean;
}

function lifetimeOptions(kind: "claim" | "mint", limit: number | undefined): [number, string][] {
  const base: [number, string][] = [
    [7 * DAY, T.OPT_7_DAYS],
    [30 * DAY, T.OPT_30_DAYS],
    [90 * DAY, T.OPT_90_DAYS],
  ];
  if (kind === "mint") return base;
  base.push([365 * DAY, T.OPT_1_YEAR]);
  if (!limit) return [...base, [0, T.OPT_LONGEST]];
  const within = base.filter(([s]) => s <= limit);
  return within.some(([s]) => s === limit) ? within : [...within, [limit, lifetimeWords(limit)]];
}

export function TokenDialog(props: TokenDialogProps) {
  const { kind, service = "", limitSeconds, replaces, initialName, submit, onCreated, onRefused, onClose, restoreFocus } = props;
  const uid = React.useId();
  const [name, setName] = React.useState(initialName);
  const [ttl, setTtl] = React.useState(kind === "claim" && limitSeconds ? limitSeconds : DEFAULT_LIFETIME);
  const [phase, setPhase] = React.useState<"form" | "busy" | "reveal">("form");
  const [nameError, setNameError] = React.useState("");
  const [refusal, setRefusal] = React.useState<Refusal | null>(null);
  const [reveal, setReveal] = React.useState<Reveal | null>(null);
  const [warned, setWarned] = React.useState(false);
  const { copied, copy } = useCopyToClipboard(null);
  const inFlight = React.useRef(false);
  const copyRef = React.useRef<HTMLButtonElement>(null);
  const doneRef = React.useRef<HTMLButtonElement>(null);
  const submitRef = React.useRef<HTMLButtonElement>(null);
  const closeRef = React.useRef<HTMLButtonElement>(null);

  // Input does not forward a ref under React 18, so the name field is found by its id.
  const focusName = () => document.getElementById(`${uid}-name`)?.focus();
  const isClaim = kind === "claim";
  const created = reveal !== null;

  const send = async () => {
    if (inFlight.current) return;
    const trimmed = name.trim();
    if (!trimmed) {
      setNameError(T.NAME_REQUIRED);
      setRefusal(null);
      focusName();
      return;
    }
    inFlight.current = true;
    setNameError("");
    setRefusal(null);
    setPhase("busy");
    try {
      const tok = await submit(ttl > 0 ? { name: trimmed, ttl_seconds: ttl } : { name: trimmed });
      const life = tok.expires_at ? Date.parse(tok.expires_at) - Date.parse(tok.created_at) : 0;
      setReveal({
        token: tok.token ?? "",
        name: tok.name || trimmed,
        expiresAt: tok.expires_at ?? null,
        principal: isClaim ? null : tok.principal,
        role: tok.role,
        clamped: ttl > 0 && life > 0 && life < ttl * 1000 - 1000,
      });
      setPhase("reveal");
      onCreated();
    } catch (e) {
      const known = e instanceof HttpError && e.status < 500;
      let final = false;
      if (e instanceof HttpError) final = await onRefused(e);
      setRefusal({
        text: known ? e.message : STATES.ERROR_DEFAULT,
        nameInvalid: known && e.reason === NAME_INVALID_REASON,
        final,
      });
      setPhase("form");
    } finally {
      inFlight.current = false;
    }
  };

  // Focus follows the phase: the name on open, Copy token on the reveal, the submit button after a refusal.
  React.useEffect(() => {
    if (phase === "reveal") copyRef.current?.focus();
  }, [phase]);
  React.useEffect(() => {
    if (refusal) (refusal.final ? closeRef : submitRef).current?.focus();
  }, [refusal]);

  const requestClose = () => {
    if (phase === "busy") return;
    if (phase === "reveal" && !copied && !warned) {
      setWarned(true);
      doneRef.current?.focus();
      return;
    }
    onClose(created);
  };

  const hold = (e: Event) => {
    if (phase !== "form") e.preventDefault();
  };

  return (
    <Dialog open onOpenChange={(open) => !open && requestClose()}>
      <DialogContent
        onOpenAutoFocus={(e) => {
          e.preventDefault();
          focusName();
        }}
        onCloseAutoFocus={(e) => {
          e.preventDefault();
          restoreFocus();
        }}
        onPointerDownOutside={hold}
        onInteractOutside={hold}
        onEscapeKeyDown={(e) => {
          if (phase === "reveal" || phase === "busy") e.preventDefault();
          if (phase === "reveal") requestClose();
        }}
      >
        {phase === "reveal" && reveal ? (
          <>
            <DialogHeader>
              <DialogTitle>{T.REVEAL_TITLE}</DialogTitle>
              <DialogDescription role={warned && !copied ? "alert" : undefined} className="text-foreground">
                {warned && !copied ? T.REVEAL_UNCOPIED : T.REVEAL_WARNING}
              </DialogDescription>
            </DialogHeader>
            <div
              role="textbox"
              aria-readonly="true"
              aria-label={T.TOKEN_LABEL}
              tabIndex={0}
              data-testid="show-once-token"
              className="select-all break-all rounded-md border border-border bg-muted px-3 py-2 font-mono text-xs text-foreground"
            >
              {reveal.token}
            </div>
            <p role="status" className="min-h-[1.1em] text-xs text-muted-foreground">
              {copied ? T.COPIED_STATUS : ""}
            </p>
            <dl className="grid grid-cols-[auto_1fr] gap-x-4 gap-y-1 text-sm">
              <dt className="text-muted-foreground">{T.DETAIL_NAME}</dt>
              <dd className="font-mono text-xs leading-5">{reveal.name}</dd>
              <dt className="text-muted-foreground">{T.DETAIL_ACTS_AS}</dt>
              <dd>
                {reveal.principal ? (
                  <span className="font-mono text-xs leading-5">{reveal.principal}</span>
                ) : (
                  T.ACTS_AS_YOU(ROLE_WORDS[reveal.role] ?? reveal.role)
                )}
              </dd>
              <dt className="text-muted-foreground">{T.DETAIL_EXPIRES}</dt>
              <dd>{reveal.expiresAt ? T.EXPIRY(dayOf(reveal.expiresAt), inWords(reveal.expiresAt)) : T.NEVER}</dd>
            </dl>
            {reveal.clamped && <p className="text-sm text-foreground">{T.CLAMPED}</p>}
            <DialogFooter>
              <Button ref={copyRef} type="button" onClick={() => void copy(reveal.token)}>
                {copied ? T.COPIED : T.COPY}
              </Button>
              <Button ref={doneRef} type="button" variant="outline" onClick={requestClose}>
                {warned && !copied ? T.CLOSE_UNCOPIED : T.DONE}
              </Button>
            </DialogFooter>
          </>
        ) : (
          <form
            className="grid gap-4"
            noValidate
            onSubmit={(e) => {
              e.preventDefault();
              if (!refusal?.final) void send();
            }}
          >
            <DialogHeader>
              <DialogTitle>{isClaim ? T.CLAIM_TITLE : T.MINT_TITLE(service)}</DialogTitle>
              <DialogDescription>{isClaim ? T.CLAIM_LEDE : T.MINT_LEDE(service)}</DialogDescription>
            </DialogHeader>
            {!refusal?.final && isClaim && replaces && (
              <p className="rounded-md border border-warning/25 bg-warning-subtle px-3 py-2 text-sm text-foreground">
                {T.CLAIM_WARNING_LEAD} <span className="font-mono text-xs">{replaces}</span>
                {T.CLAIM_WARNING_TAIL}
              </p>
            )}
            {!refusal?.final && !isClaim && (
              <p className="rounded-md border border-warning/25 bg-warning-subtle px-3 py-2 text-sm text-foreground">
                {T.MINT_WARNING(service)}
              </p>
            )}
            {refusal && (
              <p role="alert" id={`${uid}-err`} className="rounded-md border border-danger/25 bg-danger-subtle px-3 py-2 text-sm text-danger">
                {refusal.text}
              </p>
            )}
            <Field label={T.NAME_LABEL} htmlFor={`${uid}-name`} hint={T.NAME_HINT} required>
              <Input
                id={`${uid}-name`}
                value={name}
                maxLength={200}
                autoComplete="off"
                required
                disabled={phase === "busy" || refusal?.final}
                aria-invalid={!!nameError || refusal?.nameInvalid}
                aria-describedby={refusal ? `${uid}-err` : undefined}
                onChange={(e) => setName(e.target.value)}
              />
            </Field>
            {nameError && (
              <p role="alert" className="-mt-2 text-xs text-danger">
                {nameError}
              </p>
            )}
            <Field
              label={T.EXPIRES_LABEL}
              htmlFor={`${uid}-ttl`}
              hint={isClaim ? (limitSeconds ? T.CLAIM_HINT_LIMITED(lifetimeWords(limitSeconds)) : T.CLAIM_HINT_OPEN) : T.MINT_HINT}
            >
              <select
                id={`${uid}-ttl`}
                className={SELECT_CLASS}
                value={ttl}
                disabled={phase === "busy" || refusal?.final}
                onChange={(e) => setTtl(Number(e.target.value))}
              >
                {lifetimeOptions(kind, limitSeconds).map(([seconds, label]) => (
                  <option key={seconds} value={seconds}>
                    {label}
                  </option>
                ))}
              </select>
            </Field>
            <DialogFooter>
              {refusal?.final ? (
                <Button ref={closeRef} type="button" variant="ghost" onClick={requestClose}>
                  {T.CLOSE}
                </Button>
              ) : (
                <>
                  <Button type="button" variant="ghost" disabled={phase === "busy"} onClick={requestClose}>
                    {T.CANCEL}
                  </Button>
                  <Button ref={submitRef} type="submit" disabled={phase === "busy"}>
                    {phase === "busy" ? (isClaim ? T.CLAIM_BUSY : T.MINT_BUSY) : isClaim ? T.CLAIM_SUBMIT : T.MINT_SUBMIT}
                  </Button>
                </>
              )}
            </DialogFooter>
          </form>
        )}
      </DialogContent>
    </Dialog>
  );
}
