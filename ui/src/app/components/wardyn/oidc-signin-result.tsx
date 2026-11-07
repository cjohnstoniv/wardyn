/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Where a provider's sign-in ends. The server's callback lands on the console root with provider_oidc=connected or
// provider_oidc_error=<reason> (internal/api/endpoint_oidc.go). In the window the door opened, this tells the door
// how it went and closes the window; anywhere else (the link a blocked browser gets opens a tab with no way back to
// the door) it is a strip, as the Azure DevOps sign-in's is. The query is read once and taken off the address.
import * as React from "react";
import { AlertTriangle, CheckCircle2, X } from "lucide-react";
import { useShellSetupStatus } from "./model-access-context";
import { OIDC_RETURN, OIDC_SIGN_IN as T, oidcFailureKind } from "./copy/oidc-signin";

type Result = ({ ok: true } | { ok: false; reason: string }) & {
  uid: string;
  /** Shown in the window the door opened, which has told the door and asked to close. */
  window?: true;
};

function readAndClearResult(): Result | null {
  const url = new URL(window.location.href);
  const reason = url.searchParams.get(OIDC_RETURN.ERROR_PARAM);
  const done = url.searchParams.get(OIDC_RETURN.DONE_PARAM);
  if (!reason && done !== OIDC_RETURN.DONE_VALUE) return null;
  const uid = url.searchParams.get(OIDC_RETURN.PROVIDER_PARAM) ?? "";
  for (const p of [OIDC_RETURN.ERROR_PARAM, OIDC_RETURN.DONE_PARAM, OIDC_RETURN.PROVIDER_PARAM]) url.searchParams.delete(p);
  window.history.replaceState(null, "", `${url.pathname}${url.search}${url.hash}`);
  return reason ? { ok: false, reason, uid } : { ok: true, uid };
}

export function OidcSignInResult() {
  const [result, setResult] = React.useState<Result | null>(null);
  React.useEffect(() => {
    const r = readAndClearResult();
    if (!r) return;
    const opener = window.opener as Window | null;
    if (opener && opener !== window) {
      try {
        opener.postMessage(
          // The window's name is the door's per-attempt name for it: echoed so the door can tell this attempt's word.
          { type: OIDC_RETURN.MESSAGE, ok: r.ok, reason: r.ok ? "" : r.reason, attempt: window.name },
          window.location.origin,
        );
        window.close();
        // A window that stays open (a browser may refuse the close) says it can be closed.
        setResult({ ...r, window: true });
        return;
      } catch {
        /* no opener to tell: show the result here */
      }
    }
    setResult(r);
  }, []);
  const { status } = useShellSetupStatus();
  if (!result) return null;
  const named = result.uid ? status?.model_providers?.find((p) => p.sign_in === "oidc" && p.uid === result.uid) : undefined;
  const name = named ? named.name || named.id : "";
  const sentence = result.window
    ? result.ok
      ? T.WINDOW_OK(name || T.BANNER_FAIL_NAME)
      : T.WINDOW_FAIL(name || T.BANNER_FAIL_NAME)
    : result.ok
    ? name
      ? T.BANNER_OK_NAMED(name)
      : T.BANNER_OK
    : T.FAIL[oidcFailureKind(result.reason)](name || T.BANNER_FAIL_NAME);
  const Icon = result.ok ? CheckCircle2 : AlertTriangle;
  return (
    <div
      role={result.ok ? "status" : "alert"}
      className={`relative z-50 flex shrink-0 items-start gap-2 border-b border-border px-4 py-2 text-sm ${result.ok ? "bg-success-subtle text-success" : "bg-danger-subtle text-danger"}`}
    >
      <Icon className="mt-0.5 size-4 shrink-0" aria-hidden />
      <p className="min-w-0 flex-1 font-medium text-foreground">{sentence}</p>
      <button type="button" onClick={() => setResult(null)} aria-label="Dismiss" className="shrink-0 text-muted-foreground hover:text-foreground">
        <X className="size-4" />
      </button>
    </div>
  );
}
