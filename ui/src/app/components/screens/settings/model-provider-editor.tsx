/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The provider editor: #537 (MP-19a) built the kind step, the form, "Use
// with", and Remove for the key and endpoint kinds; #538 (MP-19b) adds Amazon
// Bedrock (E3 — the "How people sign in" SSO/Bearer choice, and, SSO only, the
// AWS IAM Identity Center block) and Claude subscription (E4 — disabled with
// its reason on the kind step until the sign-in image resolves). Drawn by
// docs/design/model-providers-mock/provider-editor.html and, for #538,
// mp-packet-B.html's own E1–E9 (owner-approved 2026-09-22); every string is
// lib/model-providers-copy.ts's. Configuration only — there is no key,
// token or sign-in field: each person adds or signs in with their own.
//
// A self-contained dialog: the host (Settings → Model providers, #536) hands
// it the GET /model-providers snapshot it rendered from, and re-reads after
// onSaved. Every write is the WHOLE document with that snapshot's If-Match.
import * as React from "react";
import { ChevronDown, ChevronRight, Loader2 } from "lucide-react";
import { toast } from "sonner";
import { HttpError } from "../../../lib/api/core";
import { modelProviders, type ModelProvidersList } from "../../../lib/api/model-providers";
import { getErrorMessage } from "../../../lib/format";
import { MODEL_PROVIDERS, PROVIDER_EDITOR, PROVIDERS } from "../../../lib/model-providers-copy";
import type { ModelProvider } from "../../../lib/types/site";
import { AGENTS, AGENTS_DRAFT } from "../../../lib/workspace-providers-copy";
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
import { Checkbox } from "../../ui/checkbox";
import { Dialog, DialogContent, DialogHeader, DialogTitle, DialogDescription } from "../../ui/dialog";
import { Input } from "../../ui/input";
import { Mono } from "../../wardyn/code-block";
import { Field } from "../../wardyn/form-primitives";
import { Chip } from "../../wardyn/primitives";
import { SavedElsewhereBanner } from "../../wardyn/saved-elsewhere-banner";
import { useRovingRadio } from "../../wardyn/use-roving-radio";
import { useDeferredBusy } from "../../../lib/use-deferred-busy";
import {
  EDITOR_KINDS,
  addressChanged,
  draftFrom,
  editorProvidesLine,
  incompatibleReason,
  isBedrock,
  isEndpoint,
  newDraft,
  providerFrom,
  vendorHost,
  type HarnessRow,
  type ProviderDraft,
} from "./model-provider-draft";

export interface ModelProviderEditorProps {
  // The GET /model-providers read the host rendered from: the document a save
  // replaces, its ETag, and each provider's connected-people count (E9).
  list: ModelProvidersList;
  // The stored provider to edit, or null to add one (starting at the kind step).
  editing: ModelProvider | null;
  // The catalog agents a provider can serve, in catalog order.
  harnesses: HarnessRow[];
  // The agents whose roster default this provider is (E8).
  defaultFor?: string[];
  // Whether the Claude sign-in image resolves right now (/setup/status's
  // claude_signin_image check) — E4's kind-step gate (QB-5). Defaults true
  // (fail open): an older host or a status read still in flight never blocks
  // the option on a false negative — the server's own E4 refusal at Save is
  // the real gate this only previews.
  subscriptionAvailable?: boolean;
  onClose: () => void;
  // A write landed (or the admin chose to reload after a 412): close and re-read.
  onSaved: () => void;
}

type Confirm = "address" | "remove" | null;

export function ModelProviderEditor({
  list,
  editing,
  harnesses,
  defaultFor = [],
  subscriptionAvailable = true,
  onClose,
  onSaved,
}: ModelProviderEditorProps) {
  const [draft, setDraft] = React.useState<ProviderDraft | null>(() => (editing ? draftFrom(editing, harnesses) : null));
  const [saving, setSaving] = React.useState(false);
  const [refused, setRefused] = React.useState<string | null>(null);
  const [stale, setStale] = React.useState<string | null>(null);
  const [confirm, setConfirm] = React.useState<Confirm>(null);
  const { disabled: busy, showSpinner } = useDeferredBusy(saving);

  const stored = list.providers.providers ?? [];
  const title = editing ? editing.name || MODEL_PROVIDERS.KIND[editing.kind] : MODEL_PROVIDERS.ADD_CTA;
  const keyKind = !isEndpoint(editing?.kind ?? draft?.kind ?? "custom_endpoint");

  const write = async (next: ModelProvider[]) => {
    const doc = { providers: next };
    setSaving(true);
    setRefused(null);
    try {
      await modelProviders.putModelProviders(doc, list.etag);
      onSaved();
      return true;
    } catch (e) {
      if (e instanceof HttpError && e.status === 400) setRefused(e.message);
      else if (e instanceof HttpError && e.status === 412) setStale(JSON.stringify(doc, null, 2));
      else toast.error(getErrorMessage(e));
      return false;
    } finally {
      setSaving(false);
    }
  };

  const candidate = () => providerFrom(draft!, editing, stored.map((p) => p.id));

  const save = async () => {
    const next = candidate();
    const rows = editing ? stored.map((p) => (p.id === editing.id ? next : p)) : [...stored, next];
    if (await write(rows)) toast.success(PROVIDER_EDITOR.SAVED_TOAST);
  };

  // How many people hold a credential for it — what rule 8 would delete (E9).
  const n = editing ? (list.connected[editing.id] ?? 0) : 0;
  const onSubmit = (e: React.FormEvent) => {
    e.preventDefault();
    // Rule 8 asks first only when someone would lose a credential (decision 2).
    if (editing && n > 0 && addressChanged(editing, candidate())) {
      setConfirm("address");
      return;
    }
    void save();
  };

  const addressBody =
    n === 1
      ? keyKind
        ? PROVIDER_EDITOR.ADDRESS_BODY_KEY_ONE
        : PROVIDER_EDITOR.ADDRESS_BODY_ONE
      : keyKind
        ? PROVIDER_EDITOR.ADDRESS_BODY_KEY(n)
        : PROVIDER_EDITOR.ADDRESS_BODY(n);
  const blockedFor = defaultFor[0] && (harnesses.find((h) => h.id === defaultFor[0])?.display ?? defaultFor[0]);

  return (
    <Dialog open onOpenChange={(open) => !open && onClose()}>
      {/* The kind step has no description line; say so rather than let Radix
          point aria-describedby at nothing. */}
      <DialogContent {...(!draft && { "aria-describedby": undefined })}>
        <DialogHeader>
          <div className="flex flex-wrap items-center gap-2">
            <DialogTitle>{title}</DialogTitle>
            {editing && <Chip tone="neutral">{MODEL_PROVIDERS.KIND[editing.kind]}</Chip>}
          </div>
          {draft && <DialogDescription>{editorProvidesLine(draft.kind)}</DialogDescription>}
        </DialogHeader>

        {!draft ? (
          <KindStep
            onPick={(kind) => setDraft(newDraft(kind, harnesses))}
            onCancel={onClose}
            subscriptionAvailable={subscriptionAvailable}
          />
        ) : (
          <form className="space-y-4" onSubmit={onSubmit}>
            <ProviderFields draft={draft} setDraft={setDraft} harnesses={harnesses} />
            {refused && (
              <div role="alert" className="rounded-lg border border-danger/30 bg-danger-subtle p-3 text-body text-danger">
                <b className="font-semibold">{PROVIDERS.SAVE_REFUSED_TITLE_ONE}</b>
                <p className="mt-0.5">{refused}</p>
              </div>
            )}
            {stale && <SavedElsewhereBanner documentText={stale} onDiscard={onSaved} />}
            <div className="flex flex-wrap items-center gap-2 pt-2">
              {editing && (
                <Button type="button" variant="outline" disabled={busy} onClick={() => setConfirm("remove")}>
                  {PROVIDER_EDITOR.REMOVE}
                </Button>
              )}
              <span className="grow" />
              <Button type="button" variant="ghost" onClick={onClose}>
                {PROVIDER_EDITOR.CANCEL}
              </Button>
              <Button type="submit" disabled={busy}>
                {showSpinner && <Loader2 className="size-4 animate-spin" aria-hidden="true" />}
                {PROVIDER_EDITOR.SAVE}
              </Button>
            </div>
          </form>
        )}

        <AlertDialog open={confirm !== null} onOpenChange={(open) => !open && setConfirm(null)}>
          <AlertDialogContent className="sm:max-w-md">
            <AlertDialogHeader>
              <AlertDialogTitle>
                {confirm === "address" ? PROVIDER_EDITOR.ADDRESS_TITLE(title) : PROVIDER_EDITOR.DELETE_TITLE(title)}
              </AlertDialogTitle>
              <AlertDialogDescription>
                {confirm === "address"
                  ? addressBody
                  : blockedFor
                    ? PROVIDER_EDITOR.DELETE_BLOCKED(title, blockedFor)
                    : keyKind
                      ? PROVIDER_EDITOR.DELETE_BODY_KEY
                      : PROVIDER_EDITOR.DELETE_BODY}
              </AlertDialogDescription>
            </AlertDialogHeader>
            <AlertDialogFooter>
              <AlertDialogCancel className={buttonVariants({ variant: "ghost" })}>{PROVIDER_EDITOR.CANCEL}</AlertDialogCancel>
              {confirm === "address" ? (
                <AlertDialogAction onClick={() => void save()}>{PROVIDER_EDITOR.SAVE}</AlertDialogAction>
              ) : (
                <AlertDialogAction
                  className={buttonVariants({ variant: "destructive" })}
                  disabled={!!blockedFor}
                  onClick={() => void write(stored.filter((p) => p.id !== editing?.id))}
                >
                  {PROVIDER_EDITOR.REMOVE}
                </AlertDialogAction>
              )}
            </AlertDialogFooter>
          </AlertDialogContent>
        </AlertDialog>
      </DialogContent>
    </Dialog>
  );
}

// The kind step: picking a kind moves on — there is no Next. Claude
// subscription is disabled with the sign-in image's own reason rather than
// hidden (QB-5) when it doesn't resolve yet.
function KindStep({
  onPick,
  onCancel,
  subscriptionAvailable,
}: {
  onPick: (kind: (typeof EDITOR_KINDS)[number]) => void;
  onCancel: () => void;
  subscriptionAvailable: boolean;
}) {
  return (
    <div className="space-y-4">
      <div role="group" aria-labelledby="mp-kind-title" className="space-y-2">
        <p id="mp-kind-title" className="text-xs font-medium text-foreground">
          {PROVIDER_EDITOR.KIND_TITLE}
        </p>
        <div className="overflow-hidden rounded-lg border border-border">
          {EDITOR_KINDS.map((kind) => {
            const disabled = kind === "anthropic_subscription" && !subscriptionAvailable;
            return (
              <button
                key={kind}
                type="button"
                disabled={disabled}
                onClick={() => onPick(kind)}
                className="block w-full border-b border-border px-3 py-2 text-left text-body last:border-b-0 hover:enabled:bg-accent disabled:cursor-not-allowed disabled:opacity-60"
              >
                {MODEL_PROVIDERS.KIND[kind]}
                {disabled && <p className="mt-0.5 text-xs font-normal text-muted-foreground">{PROVIDER_EDITOR.CLAUDE_IMAGE_MISSING}</p>}
              </button>
            );
          })}
        </div>
      </div>
      <div className="flex justify-end">
        <Button type="button" variant="ghost" onClick={onCancel}>
          {PROVIDER_EDITOR.CANCEL}
        </Button>
      </div>
    </div>
  );
}

function ProviderFields({
  draft,
  setDraft,
  harnesses,
}: {
  draft: ProviderDraft;
  setDraft: React.Dispatch<React.SetStateAction<ProviderDraft | null>>;
  harnesses: HarnessRow[];
}) {
  const set = (patch: Partial<ProviderDraft>) => setDraft((d) => d && { ...d, ...patch });
  const endpoint = isEndpoint(draft.kind);
  const bedrock = isBedrock(draft.kind);
  const sso = draft.kind === "bedrock_sso";
  const routeThrough = draft.kind === "anthropic_api_key" || draft.kind === "openai_api_key";
  const host = vendorHost(draft.kind);
  const [before, after] = PROVIDER_EDITOR.ROUTE_THROUGH_HINT(host).split(host);
  const rows = Object.keys(draft.harnesses).map((id) => harnesses.find((h) => h.id === id) ?? { id, display: id });
  // E3's "How people sign in" toggle — the ONE field that reaches bedrock_bearer
  // (never a kind-step option of its own). agents-tab.tsx's FIELD_SOURCE toggle
  // is the same segmented-radio-group idiom (role="radiogroup" Buttons over
  // useRovingRadio, not a native radiogroup — CONSOLE-RULES' precedent for a
  // two-option choice with the mock's segmented look).
  const signInGroup = useRovingRadio(2, sso ? 0 : 1, (i) => set({ kind: i === 0 ? "bedrock_sso" : "bedrock_bearer" }));

  return (
    <>
      {bedrock && (
        <Field label={PROVIDER_EDITOR.HOW_PEOPLE_SIGN_IN}>
          <div role="radiogroup" aria-label={PROVIDER_EDITOR.HOW_PEOPLE_SIGN_IN} className="flex gap-2" {...signInGroup.containerProps}>
            <Button
              type="button"
              role="radio"
              aria-checked={sso}
              size="sm"
              variant={sso ? "secondary" : "outline"}
              tabIndex={signInGroup.itemProps(0).tabIndex}
              ref={signInGroup.itemProps(0).radioRef}
              onClick={() => set({ kind: "bedrock_sso" })}
            >
              {AGENTS.MECHANISM_BEDROCK_SSO}
            </Button>
            <Button
              type="button"
              role="radio"
              aria-checked={!sso}
              size="sm"
              variant={!sso ? "secondary" : "outline"}
              tabIndex={signInGroup.itemProps(1).tabIndex}
              ref={signInGroup.itemProps(1).radioRef}
              onClick={() => set({ kind: "bedrock_bearer" })}
            >
              {AGENTS.MECHANISM_BEDROCK_BEARER}
            </Button>
          </div>
        </Field>
      )}

      <Field label={PROVIDER_EDITOR.NAME} htmlFor="mp-name" hint={PROVIDER_EDITOR.NAME_HINT}>
        <Input id="mp-name" value={draft.name} onChange={(e) => set({ name: e.target.value })} />
      </Field>

      {bedrock && (
        <>
          <p className="text-xs font-medium text-foreground">{PROVIDER_EDITOR.IDC_GROUP}</p>
          <Field label={PROVIDER_EDITOR.REGION} htmlFor="mp-region">
            <Input id="mp-region" className="font-mono" placeholder="us-east-1" value={draft.region} onChange={(e) => set({ region: e.target.value })} />
          </Field>
          {sso && (
            <>
              <Field label={AGENTS.FIELD_SSO_START_URL} htmlFor="mp-sso-start-url">
                <Input
                  id="mp-sso-start-url"
                  className="font-mono"
                  placeholder="https://"
                  value={draft.ssoStartUrl}
                  onChange={(e) => set({ ssoStartUrl: e.target.value })}
                />
              </Field>
              <Field label={AGENTS_DRAFT.FIELD_SSO_ACCOUNT_ID} htmlFor="mp-sso-account-id">
                <Input
                  id="mp-sso-account-id"
                  className="font-mono"
                  value={draft.ssoAccountId}
                  onChange={(e) => set({ ssoAccountId: e.target.value })}
                />
              </Field>
              <Field label={AGENTS_DRAFT.FIELD_SSO_ROLE_NAME} htmlFor="mp-sso-role-name" hint={PROVIDER_EDITOR.SSO_SETUP_HINT}>
                <Input
                  id="mp-sso-role-name"
                  className="font-mono"
                  value={draft.ssoRoleName}
                  onChange={(e) => set({ ssoRoleName: e.target.value })}
                />
              </Field>
            </>
          )}
        </>
      )}

      {endpoint ? (
        <>
          <Field label={PROVIDER_EDITOR.BASE_URL} htmlFor="mp-base-url" hint={PROVIDER_EDITOR.BASE_URL_HINT} required>
            <Input
              id="mp-base-url"
              required
              className="font-mono"
              placeholder="https://"
              value={draft.baseUrl}
              onChange={(e) => set({ baseUrl: e.target.value })}
            />
          </Field>
          <div className="grid gap-4 sm:grid-cols-2">
            <Field label={PROVIDER_EDITOR.AUTH_HEADER} htmlFor="mp-auth-header">
              <Input id="mp-auth-header" className="font-mono" value={draft.authHeader} onChange={(e) => set({ authHeader: e.target.value })} />
            </Field>
            <Field label={PROVIDER_EDITOR.VALUE_FORMAT} htmlFor="mp-auth-format">
              <Input id="mp-auth-format" className="font-mono" value={draft.authFormat} onChange={(e) => set({ authFormat: e.target.value })} />
            </Field>
          </div>
        </>
      ) : (
        routeThrough && (
          <Field
            label={PROVIDER_EDITOR.ROUTE_THROUGH}
            htmlFor="mp-base-url"
            hint={
              <>
                {before}
                <Mono>{host}</Mono>
                {after}
              </>
            }
          >
            <Input
              id="mp-base-url"
              className="font-mono"
              placeholder="https://"
              value={draft.baseUrl}
              onChange={(e) => set({ baseUrl: e.target.value })}
            />
          </Field>
        )
      )}

      <fieldset className="space-y-2">
        <legend className="text-sm font-medium">{PROVIDER_EDITOR.USE_WITH}</legend>
        {rows.map((h) => (
          <HarnessBlock
            key={h.id}
            harness={h}
            draft={draft}
            onChange={(patch) =>
              setDraft((d) => d && { ...d, harnesses: { ...d.harnesses, [h.id]: { ...d.harnesses[h.id], ...patch } } })
            }
          />
        ))}
      </fieldset>
    </>
  );
}

// One agent under "Use with": a checkbox, and under a tick the agent's Model and
// (endpoint kind) Path. Collapsed unless a required field is empty (QB-2); an
// agent the kind can't drive is disabled with the catalog's reason, never hidden.
function HarnessBlock({
  harness,
  draft,
  onChange,
}: {
  harness: HarnessRow;
  draft: ProviderDraft;
  onChange: (patch: Partial<ProviderDraft["harnesses"][string]>) => void;
}) {
  const row = draft.harnesses[harness.id];
  const endpoint = isEndpoint(draft.kind);
  const bedrock = isBedrock(draft.kind);
  const reason = incompatibleReason(draft.kind, harness.id);
  const needsPath = endpoint && row.ticked && !row.path.trim();
  // Model is required on a Bedrock provider (mp400ModelNeed) — E3's colfoot:
  // "Model is required for Bedrock, so that harness's settings are open until
  // it is filled" (QB-2), the same rule already applied to an endpoint's Path.
  const needsModel = bedrock && row.ticked && !row.model.trim();
  const [open, setOpen] = React.useState(needsPath || needsModel);
  React.useEffect(() => {
    if (needsPath || needsModel) setOpen(true);
  }, [needsPath, needsModel]);
  const expanded = row.ticked && open;
  const base = `mp-h-${harness.id}`;
  const pathHint = harness.id === "codex-cli" ? PROVIDER_EDITOR.PATH_HINT_CODEX : PROVIDER_EDITOR.PATH_HINT_CLAUDE;
  const modelHint = bedrock ? PROVIDER_EDITOR.MODEL_HINT_BEDROCK : PROVIDER_EDITOR.MODEL_HINT;

  return (
    <div className="rounded-lg border border-border">
      <div className="flex items-start gap-2 px-3 py-2">
        <Checkbox
          id={base}
          className="mt-0.5"
          checked={row.ticked}
          disabled={!!reason}
          onCheckedChange={(v) => onChange({ ticked: v === true })}
        />
        <div className={reason ? "min-w-0 flex-1 opacity-60" : "min-w-0 flex-1"}>
          <label htmlFor={base} className="text-body font-normal">
            {harness.display}
          </label>
          {reason && <p className="mt-0.5 text-xs text-muted-foreground">{reason}</p>}
        </div>
        {row.ticked && (
          <button
            type="button"
            aria-label={harness.display}
            aria-expanded={expanded}
            aria-controls={`${base}-body`}
            // A required, empty Path or Model stays open: collapsing would
            // unmount the input its `required` check lives on.
            disabled={needsPath || needsModel}
            onClick={() => setOpen(!expanded)}
            className="text-muted-foreground disabled:opacity-50"
          >
            {expanded ? <ChevronDown className="size-4" /> : <ChevronRight className="size-4" />}
          </button>
        )}
      </div>
      {expanded && (
        <div id={`${base}-body`} className={endpoint ? "grid gap-4 border-t border-border p-3 sm:grid-cols-2" : "border-t border-border p-3"}>
          <Field label={PROVIDER_EDITOR.MODEL} htmlFor={`${base}-model`} hint={modelHint} required={bedrock}>
            <Input
              id={`${base}-model`}
              required={bedrock}
              className="font-mono"
              value={row.model}
              onChange={(e) => onChange({ model: e.target.value })}
            />
          </Field>
          {endpoint && (
            <Field label={PROVIDER_EDITOR.PATH} htmlFor={`${base}-path`} hint={pathHint} required>
              <Input
                id={`${base}-path`}
                required
                className="font-mono"
                placeholder="/"
                value={row.path}
                onChange={(e) => onChange({ path: e.target.value })}
              />
            </Field>
          )}
        </div>
      )}
    </div>
  );
}
