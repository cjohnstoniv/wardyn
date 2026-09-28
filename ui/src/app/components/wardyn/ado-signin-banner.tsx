/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// #659 Q2 — the Azure DevOps sign-in home banner (owner-approved packet,
// ado-signin-error-659-packet.html, 2026-09-28, Q1 "(A) a banner on the home
// screen, reading the redirect exactly as it lands"). Both capture-callback
// redirects already land on "/" with a fixed query string
// (adoSignInDonePath/adoSignInErrorPath, internal/api/ado_entra.go) — this
// component is the ONE reader: it checks the URL once on mount, renders the
// matching sentence, and clears the query string so a refresh never replays
// it. No backend change for Q1: both redirects were already shipped strings.
//
// Q2: identity_binding means the Microsoft account that answered isn't the
// one signed in to Wardyn — a plain retry hits whatever account the browser's
// Microsoft SSO session already holds, likely the same wrong one, silently.
// Its own retry link alone adds prompt=select_account to the authorize
// request (handleADOSignIn, ado_entra.go) so Microsoft shows the account
// picker instead of skipping straight back to the wrong session.
import * as React from "react";
import { AlertTriangle, CheckCircle2, X } from "lucide-react";
import { apiURL } from "../../lib/base-path";
import { ADO_SIGNIN } from "./copy/ado-signin-error-copy";

const ERROR_PARAM = "ado_signin_error";
const DONE_PARAM = "ado_signin";
const DONE_VALUE = "connected";
// Mirrors use-ado-connect.ts's own SIGNIN_PATH — this banner's retry is a
// plain top-level navigation (the popup flow's parent window is not
// necessarily the one this banner renders in — the popup itself lands on
// this exact URL on error), never the popup/poll machinery that hook owns.
const SIGNIN_PATH = "/scm/azure-devops/signin";

type AdoSignInResult = { kind: "connected" } | { kind: "error"; reason: string };

/** Reads the redirect's own query params once, then strips them from the
 *  address bar (history.replaceState — no navigation, no reload) so back/
 *  forward and a refresh never replay a stale result. null when neither
 *  redirect's param is present. */
function readAndClearAdoSignInResult(): AdoSignInResult | null {
  const url = new URL(window.location.href);
  const reason = url.searchParams.get(ERROR_PARAM);
  const done = url.searchParams.get(DONE_PARAM);
  if (!reason && done !== DONE_VALUE) return null;
  url.searchParams.delete(ERROR_PARAM);
  url.searchParams.delete(DONE_PARAM);
  window.history.replaceState(null, "", `${url.pathname}${url.search}${url.hash}`);
  return reason ? { kind: "error", reason } : { kind: "connected" };
}

/** A reason the redirect sent that this console does not (yet) know —
 *  e.g. an older daemon's code, or a value nothing above ever emits — reads
 *  as exchange_failed's own sentence rather than nothing at all, the same
 *  "absent entry, no invented label" rule connectionRowCopy's callers get
 *  from their own tables. */
function reasonSentence(reason: string): string {
  const known = ADO_SIGNIN.REASON as Record<string, string>;
  return known[reason] ?? ADO_SIGNIN.REASON.exchange_failed;
}

export function AdoSignInBanner() {
  const [result, setResult] = React.useState<AdoSignInResult | null>(null);
  // Runs once: the redirect's query string is only ever there for the page
  // load it arrived on.
  React.useEffect(() => {
    setResult(readAndClearAdoSignInResult());
  }, []);
  if (!result) return null;

  if (result.kind === "connected") {
    return (
      <div className="relative z-50 flex shrink-0 items-start gap-2 border-b border-border bg-success-subtle px-4 py-2 text-sm text-success">
        <CheckCircle2 className="mt-0.5 size-4 shrink-0" aria-hidden />
        <p className="min-w-0 flex-1 font-medium text-foreground">{ADO_SIGNIN.CONNECTED}</p>
        <button
          type="button"
          onClick={() => setResult(null)}
          aria-label="Dismiss"
          className="shrink-0 text-muted-foreground hover:text-foreground"
        >
          <X className="size-4" />
        </button>
      </div>
    );
  }

  // Q2: identity_binding alone asks Microsoft for the account picker — the
  // other five reasons keep the plain retry (authorizeURL's own default).
  const retryHref = apiURL(SIGNIN_PATH + (result.reason === "identity_binding" ? "?prompt=select_account" : ""));
  return (
    <div className="relative z-50 flex shrink-0 items-start gap-2 border-b border-border bg-danger-subtle px-4 py-2 text-sm text-danger">
      {/* No role="alert" of its own — app-shell.tsx's shared role="status"
          region (the same one ModelAccessBanner and ConfinementPostureBanner
          rely on) already announces this text arriving. */}
      <AlertTriangle className="mt-0.5 size-4 shrink-0" aria-hidden />
      <p className="min-w-0 flex-1 font-medium text-foreground">{reasonSentence(result.reason)}</p>
      <a href={retryHref} className="shrink-0 font-medium underline underline-offset-2">
        {ADO_SIGNIN.RETRY}
      </a>
      <button
        type="button"
        onClick={() => setResult(null)}
        aria-label="Dismiss"
        className="shrink-0 text-muted-foreground hover:text-foreground"
      >
        <X className="size-4" />
      </button>
    </div>
  );
}
