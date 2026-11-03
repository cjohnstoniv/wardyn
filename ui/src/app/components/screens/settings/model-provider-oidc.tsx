/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// "How people connect" on your own endpoint, and the sign-in service it opens: the editor's OIDC half. The choice
// is two cards, the chosen one drawn with an accent border and the word Selected. The fields are the stored
// block's (issuer, client, secret, scopes, binding); authorize_url and token_url are not shown, and a save carries
// them through (providerFrom). The server's refusals are shown by the editor as sent; the field one names is
// outlined here.
import * as React from "react";
import { AlertTriangle, Check, Copy } from "lucide-react";
import { secrets } from "../../../lib/api/secrets";
import { apiURL } from "../../../lib/base-path";
import { PROVIDER_EDITOR } from "../../../lib/model-providers-copy";
import { cn } from "../../ui/utils";
import { Input } from "../../ui/input";
import { OPERATOR_ONLY_REASON } from "../../wardyn/copy";
import { useCopyToClipboard } from "../../../lib/use-copy-to-clipboard";
import { Field, Switch } from "../../wardyn/form-primitives";
import { useRovingRadio } from "../../wardyn/use-roving-radio";
import { ModelChips, type ChipCopy } from "./model-chips";
import type { ProviderDraft } from "./model-provider-draft";

const T = PROVIDER_EDITOR.OIDC;

// The server's `field` for a refused save: the sign-in block's own parts, "oidc" for the block as a whole.
export type OIDCRefusedField =
  | "oidc"
  | "oidc.issuer"
  | "oidc.authorize_url"
  | "oidc.token_url"
  | "oidc.client_id"
  | "oidc.client_secret_ref"
  | "oidc.scopes"
  | "oidc.identity_binding";

const SCOPE_COPY: ChipCopy = {
  none: T.SCOPES_NONE,
  placeholder: T.SCOPES_PLACEHOLDER,
  add: "Add",
  hint: T.SCOPES_HINT,
  remove: T.SCOPE_REMOVE,
};

// The address a provider registers on the client: this console's origin, its base path and the callback's route.
export const oidcCallbackURL = () => `${window.location.origin}${apiURL("/auth/provider-oidc/callback")}`;

const CHOICES = [
  { on: false, label: T.PASTE },
  { on: true, label: T.SIGN_IN },
] as const;

export function OidcChoice({
  draft,
  set,
  operator,
  note,
}: {
  draft: ProviderDraft;
  set: (patch: Partial<ProviderDraft>) => void;
  operator: boolean;
  // What saving the change deletes, when it deletes anything: shown under the choice and again in the confirmation.
  note?: string;
}) {
  const reasonId = React.useId();
  const group = useRovingRadio(2, draft.oidcOn ? 1 : 0, (i) => operator && set({ oidcOn: i === 1 }));
  return (
    <Field label={T.CHOICE}>
      <div role="radiogroup" aria-label={T.CHOICE} aria-describedby={operator ? undefined : reasonId} className="flex flex-wrap gap-2" {...group.containerProps}>
        {CHOICES.map((c, i) => {
          const selected = draft.oidcOn === c.on;
          return (
            <button
              key={c.label}
              type="button"
              role="radio"
              aria-checked={selected}
              disabled={!operator}
              tabIndex={group.itemProps(i).tabIndex}
              ref={group.itemProps(i).radioRef}
              onClick={() => set({ oidcOn: c.on })}
              className={cn(
                "inline-flex h-9 items-center gap-2 rounded-lg border px-3 text-sm font-medium outline-none transition-colors focus-visible:ring-[3px] focus-visible:ring-ring disabled:cursor-not-allowed disabled:opacity-50",
                selected ? "border-primary bg-primary/10 text-foreground" : "border-border text-muted-foreground hover:enabled:border-border-strong",
              )}
            >
              {selected && <Check className="size-4 text-primary" aria-hidden="true" />}
              {c.label}
              {selected && <span className="text-xs font-normal text-primary">{T.SELECTED}</span>}
            </button>
          );
        })}
      </div>
      {!operator && (
        <p id={reasonId} className="text-xs leading-snug text-muted-foreground">
          {OPERATOR_ONLY_REASON}
        </p>
      )}
      {draft.oidcOn && <p className="text-xs leading-snug text-muted-foreground">{T.CHOICE_HINT}</p>}
      {note && (
        <p role="status" className="rounded-lg border border-warning/30 bg-warning-subtle p-3 text-body text-warning">
          {note}
        </p>
      )}
    </Field>
  );
}

/** Copy callback URL: the button says Copied, with a check, once the address is on the clipboard. */
function CallbackCopy({ text }: { text: string }) {
  const { copied, copy } = useCopyToClipboard();
  const Icon = copied ? Check : Copy;
  return (
    <button
      type="button"
      onClick={() => copy(text)}
      aria-label={T.COPY_CALLBACK}
      className="inline-flex shrink-0 items-center gap-1.5 rounded-md border border-border px-2.5 py-1 text-xs outline-none focus-visible:ring-[3px] focus-visible:ring-ring"
    >
      {copied ? PROVIDER_EDITOR.GATEWAY.COPIED : PROVIDER_EDITOR.GATEWAY.COPY}
      <Icon className={copied ? "size-3.5 text-success" : "size-3.5"} aria-hidden="true" />
      <span aria-live="polite" className="sr-only">
        {copied ? PROVIDER_EDITOR.GATEWAY.COPIED : ""}
      </span>
    </button>
  );
}

export function OidcFields({
  draft,
  set,
  refusedAt,
}: {
  draft: ProviderDraft;
  set: (patch: Partial<ProviderDraft>) => void;
  // The field the server's last refusal named.
  refusedAt: string | null;
}) {
  const bad = (f: OIDCRefusedField) => refusedAt === f || undefined;
  // The names on the Secrets page. A read that fails leaves the list unknown, and then nothing is warned about.
  const [stored, setStored] = React.useState<string[] | null>(null);
  React.useEffect(() => {
    let live = true;
    secrets.listSecrets().then(
      (n) => live && setStored(n),
      () => live && setStored(null),
    );
    return () => {
      live = false;
    };
  }, []);
  const secret = draft.oidcSecret;
  const options = secret && !stored?.includes(secret) ? [...(stored ?? []), secret] : (stored ?? []);
  const missing = !!secret && stored !== null && !stored.includes(secret);
  const callback = oidcCallbackURL();
  const bindId = React.useId();

  return (
    <>
      <p className="text-xs font-medium text-foreground">{T.GROUP}</p>
      <Field label={T.ISSUER} htmlFor="mp-oidc-issuer" hint={T.ISSUER_HINT} required>
        <Input
          id="mp-oidc-issuer"
          required
          className="font-mono"
          placeholder="https://"
          aria-invalid={bad("oidc.issuer")}
          value={draft.oidcIssuer}
          onChange={(e) => set({ oidcIssuer: e.target.value })}
        />
      </Field>
      <Field label={T.CLIENT_ID} htmlFor="mp-oidc-client-id" hint={T.CLIENT_ID_HINT} required>
        <Input
          id="mp-oidc-client-id"
          required
          className="font-mono"
          aria-invalid={bad("oidc.client_id")}
          value={draft.oidcClientId}
          onChange={(e) => set({ oidcClientId: e.target.value })}
        />
      </Field>
      <Field
        label={T.CLIENT_SECRET}
        htmlFor="mp-oidc-secret"
        hint={
          <>
            {missing && <span className="mb-1 block text-warning">{T.CLIENT_SECRET_MISSING(secret)}</span>}
            {T.CLIENT_SECRET_HINT}
          </>
        }
      >
        <select
          id="mp-oidc-secret"
          aria-invalid={bad("oidc.client_secret_ref")}
          className="border-input bg-input-background aria-invalid:border-destructive h-9 w-full rounded-md border px-3 font-mono text-sm outline-none focus-visible:border-ring focus-visible:ring-[3px] focus-visible:ring-ring"
          value={secret}
          onChange={(e) => set({ oidcSecret: e.target.value })}
        >
          <option value="">{T.CLIENT_SECRET_NONE}</option>
          {options.map((n) => (
            <option key={n} value={n}>
              {n}
            </option>
          ))}
        </select>
      </Field>
      <Field label={T.SCOPES} htmlFor="mp-oidc-scopes" required>
        <ModelChips
          id="mp-oidc-scopes"
          value={draft.oidcScopes}
          onChange={(oidcScopes) => set({ oidcScopes })}
          isBad={() => false}
          invalid={refusedAt === "oidc.scopes"}
          copy={SCOPE_COPY}
          commas={false}
        />
      </Field>
      <Field label={T.CALLBACK} hint={T.CALLBACK_HINT}>
        <div className="flex items-center justify-between gap-2 rounded-md border border-border bg-input-background px-3 py-2">
          <code className="min-w-0 break-all font-mono text-xs">{callback}</code>
          <CallbackCopy text={callback} />
        </div>
      </Field>
      <div className="space-y-2" data-invalid={bad("oidc.identity_binding")}>
        <div className="flex items-center gap-2.5">
          <Switch checked={draft.oidcBound} onChange={(oidcBound) => set({ oidcBound })} label={T.BIND} describedBy={bindId} />
          <span className="text-body">{T.BIND}</span>
        </div>
        {draft.oidcBound ? (
          <p id={bindId} className="text-xs leading-snug text-muted-foreground">
            {T.BIND_ON}
          </p>
        ) : (
          <p id={bindId} className="flex items-start gap-1.5 text-xs leading-snug text-warning">
            <AlertTriangle className="mt-0.5 size-3.5 shrink-0" aria-hidden="true" />
            {T.BIND_OFF}
          </p>
        )}
      </div>
    </>
  );
}
