/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The Azure Foundry sign-in's landing banner, modelled on ado-signin-banner.tsx. The callback of a provider row's
// Microsoft sign-in redirects to "/" with `azure_signin=connected`, or `azure_signin_error=<reason>`, and the
// row's uid as `azure_provider`. This is the one reader: it reads the three once on mount, clears them from the
// address bar so a refresh never replays them, and says which provider connected or failed.
import * as React from "react";
import { AlertTriangle, CheckCircle2, X } from "lucide-react";
import { apiURL } from "../../lib/base-path";
import { useShellSetupStatus } from "./model-access-context";
import { ADO_SIGNIN } from "./copy/ado-signin-error-copy";
import { AZURE_DOOR } from "./copy/door";

const DONE_PARAM = "azure_signin";
const DONE_VALUE = "connected";
const ERROR_PARAM = "azure_signin_error";
const PROVIDER_PARAM = "azure_provider";

type AzureSignInResult = { kind: "connected"; uid: string } | { kind: "error"; reason: string; uid: string };

function readAndClearAzureSignInResult(): AzureSignInResult | null {
  const url = new URL(window.location.href);
  const reason = url.searchParams.get(ERROR_PARAM);
  const done = url.searchParams.get(DONE_PARAM);
  const uid = url.searchParams.get(PROVIDER_PARAM) ?? "";
  if (!reason && done !== DONE_VALUE) return null;
  for (const param of [DONE_PARAM, ERROR_PARAM, PROVIDER_PARAM]) url.searchParams.delete(param);
  window.history.replaceState(null, "", `${url.pathname}${url.search}${url.hash}`);
  return reason ? { kind: "error", reason, uid } : { kind: "connected", uid };
}

// A reason this console does not know reads as the exchange_failed sentence, as the Azure DevOps banner reads it.
// Own keys only: a crafted reason such as __proto__ must not reach an inherited member.
const reasonSentence = (reason: string): string =>
  Object.hasOwn(ADO_SIGNIN.REASON, reason) ? (ADO_SIGNIN.REASON as Record<string, string>)[reason] : ADO_SIGNIN.REASON.exchange_failed;

export function AzureSignInBanner() {
  const [result, setResult] = React.useState<AzureSignInResult | null>(null);
  const { status } = useShellSetupStatus();
  React.useEffect(() => {
    setResult(readAndClearAzureSignInResult());
  }, []);
  if (!result) return null;

  // The row, looked up by uid among the providers this person can read: without one, nothing to name or retry.
  const provider = result.uid ? status?.model_providers?.find((p) => p.uid === result.uid) : undefined;
  if (result.kind === "connected") {
    if (!provider) return null;
    return (
      <div className="relative z-50 flex shrink-0 items-start gap-2 border-b border-border bg-success-subtle px-4 py-2 text-sm text-success">
        <CheckCircle2 className="mt-0.5 size-4 shrink-0" aria-hidden />
        <p className="min-w-0 flex-1 font-medium text-foreground">{AZURE_DOOR.CONNECTED(provider.name || provider.id)}</p>
        <button type="button" onClick={() => setResult(null)} aria-label="Dismiss" className="shrink-0 text-muted-foreground hover:text-foreground">
          <X className="size-4" />
        </button>
      </div>
    );
  }

  return (
    <div className="relative z-50 flex shrink-0 items-start gap-2 border-b border-border bg-danger-subtle px-4 py-2 text-sm text-danger">
      <AlertTriangle className="mt-0.5 size-4 shrink-0" aria-hidden />
      <p className="min-w-0 flex-1 font-medium text-foreground">{reasonSentence(result.reason)}</p>
      {provider && (
        <a href={apiURL(`/model-providers-entra/signin?uid=${encodeURIComponent(result.uid)}`)} className="shrink-0 font-medium underline underline-offset-2">
          {ADO_SIGNIN.RETRY}
        </a>
      )}
      <button type="button" onClick={() => setResult(null)} aria-label="Dismiss" className="shrink-0 text-muted-foreground hover:text-foreground">
        <X className="size-4" />
      </button>
    </div>
  );
}
