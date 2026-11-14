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
import { Loader2 } from "lucide-react";
import { toast } from "sonner";
import { getExpectedPrincipal, HttpError } from "../../../lib/api/core";
import { modelProviders, type ModelProvidersList } from "../../../lib/api/model-providers";
import { AVAILABILITY } from "../../../lib/availability-copy";
import { getErrorMessage } from "../../../lib/format";
import { useUnsavedGuard } from "../../../lib/use-unsaved-guard";
import { LOCAL_MODEL_EDITOR } from "../../../lib/local-model-copy";
import { MODEL_PROVIDERS, PROVIDER_EDITOR, PROVIDERS } from "../../../lib/model-providers-copy";
import { LOCAL_MODEL_DEFAULTS, type ModelProvider } from "../../../lib/types/site";
import { AGENTS, AGENTS_EXTRA } from "../../../lib/workspace-providers-copy";
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
import { DANGER_OUTLINE, INLINE_ACTIONS, STICKY_FOOTER } from "../../wardyn/form-actions";
import { Dialog, DialogContent, DialogHeader, DialogTitle, DialogDescription } from "../../ui/dialog";
import { Input } from "../../ui/input";
import {
  AvailabilityControl,
  AvailabilityDraft,
  writeAvailability,
  type AvailabilityDraftValue,
} from "../../wardyn/availability-control";
import { Mono } from "../../wardyn/code-block";
import { CopyButton } from "../../wardyn/copy-button";
import { OPERATOR_ONLY_REASON } from "../../wardyn/copy";
import { Field, Switch } from "../../wardyn/form-primitives";
import { useOperator } from "../../wardyn/operator-context";
import { Chip } from "../../wardyn/primitives";
import { SavedElsewhereBanner } from "../../wardyn/saved-elsewhere-banner";
import { useDiscardReload } from "../../wardyn/use-discard-reload";
import { useRovingRadio } from "../../wardyn/use-roving-radio";
import { useDeferredBusy } from "../../../lib/use-deferred-busy";
import { ModelChips } from "./model-chips";
import { LaptopRunnersSwitch } from "./model-provider-laptop";
import { GatewayInspection } from "./model-provider-gateway-inspection";
import { ExtraHeaders } from "./model-provider-extra-headers";
import { GatewayPresets } from "./model-provider-gateway-presets";
import { HarnessBlock } from "./model-provider-harness";
import { OidcChoice, OidcFields } from "./model-provider-oidc";
import { applyPreset, type PresetId } from "./model-provider-presets";
import {
  EDITOR_KINDS,
  addressChanged,
  directHost,
  draftFrom,
  draftGatewayAddress,
  editorProvidesLine,
  gatewayKind,
  isBedrock,
  isEndpoint,
  isLocal,
  newDraft,
  oidcChange,
  providerFrom,
  refusedField,
  laptopBaseUrlRefused,
  refusedModel,
  type HarnessRow,
  type ProviderDraft,
} from "./model-provider-draft";
import { withSignerVar } from "./model-provider-signer-var";

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

const editedText = (draft: ProviderDraft, audience: AvailabilityDraftValue) => JSON.stringify({ ...draft, audience }, null, 2);

export function ModelProviderEditor({
  list: listProp,
  editing: editingProp,
  harnesses,
  defaultFor = [],
  subscriptionAvailable = true,
  onClose,
  onSaved,
}: ModelProviderEditorProps) {
  // A new provider asks who gets it (#923 decision 4): the choice is held
  // here, and Save writes it once the provider exists. If that list write is
  // refused, the editor stays open on the saved provider, with the snapshot
  // the save answered, so a further Save is an edit and not a stale create.
  const [audience, setAudience] = React.useState<AvailabilityDraftValue>({ restricted: false, audiences: [] });
  const [partial, setPartial] = React.useState<{ list: ModelProvidersList; provider: ModelProvider; message: string } | null>(
    null,
  );
  const list = partial?.list ?? listProp;
  const editing = partial?.provider ?? editingProp;
  // Once the create landed, closing must re-read even without another save.
  const close = partial ? onSaved : onClose;
  const [draft, setDraft] = React.useState<ProviderDraft | null>(() => (editing ? draftFrom(editing, harnesses) : null));
  // What the editor opened on (an existing provider), or the kind picked (a new one): the baseline for the unsaved guard.
  const [baseline, setBaseline] = React.useState<string | null>(() => (editing ? editedText(draftFrom(editing, harnesses), audience) : null));
  const [saving, setSaving] = React.useState(false);
  const [refused, setRefused] = React.useState<string | null>(null);
  // The field the server's refusal named ("oidc.issuer"): outlined beside the alert.
  const [refusedAt, setRefusedAt] = React.useState<string | null>(null);
  const [stale, setStale] = React.useState<string | null>(null);
  const discard = useDiscardReload(onSaved);
  const [confirm, setConfirm] = React.useState<Confirm>(null);
  const { disabled: busy, showSpinner } = useDeferredBusy(saving);

  const stored = list.providers.providers ?? [];
  const title = editing ? editing.name || MODEL_PROVIDERS.KIND[editing.kind] : MODEL_PROVIDERS.ADD_CTA;
  // A local model server's person adds a token (or nothing), never a key.
  const kind = editing?.kind ?? draft?.kind ?? "custom_endpoint";
  const keyKind = !(isEndpoint(kind) || isLocal(kind));

  const write = async (next: ModelProvider[]) => {
    const doc = { providers: next };
    setSaving(true);
    setRefused(null);
    setRefusedAt(null);
    try {
      return await modelProviders.putModelProviders(doc, list.etag);
    } catch (e) {
      if (e instanceof HttpError && e.status === 400) {
        setRefused(e.message);
        setRefusedAt(e.field || null);
      }
      else if (e instanceof HttpError && e.status === 412) setStale(JSON.stringify(doc, null, 2));
      else toast.error(getErrorMessage(e));
      return null;
    } finally {
      setSaving(false);
    }
  };

  // Leaving with edits that were not saved asks first. The draft holds names and settings only, never a credential.
  const edited = draft ? editedText(draft, audience) : "";
  useUnsavedGuard("model-provider-editor", draft !== null && edited !== baseline, () => edited);

  const candidate = () => providerFrom(draft!, editing, stored.map((p) => p.id));

  const save = async () => {
    const person = getExpectedPrincipal();
    const next = candidate();
    const rows = editing ? stored.map((p) => (p.id === editing.id ? next : p)) : [...stored, next];
    const snap = await write(rows);
    if (!snap) return;
    if (!editing) {
      try {
        await writeAvailability("model_provider", next.id, audience, person);
      } catch (e) {
        const provider = snap.providers.providers?.find((p) => p.id === next.id) ?? next;
        setPartial({ list: { ...list, ...snap }, provider, message: getErrorMessage(e) });
        // The row is saved now, with identity as the draft has it: later edits compare against that.
        const kept: ProviderDraft = {
          ...draft!,
          gatewaySaved: provider.gateway_identity
            ? { baseUrl: provider.base_url ?? "", audience: provider.gateway_identity.audience, inspect: provider.gateway_identity.inspection === "gateway" }
            : null,
        };
        setDraft(kept);
        setBaseline(editedText(kept, audience));
        return;
      }
    }
    toast.success(PROVIDER_EDITOR.SAVED_TOAST);
    onSaved();
  };

  const remove = async () => {
    if (await write(stored.filter((p) => p.id !== editing?.id))) onSaved();
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

  // A sign-in change on its own says what it deletes in its own words; with another address change beside it
  // the address wording stands, and either way the count and the deletion are the same.
  const signInChange = (() => {
    if (!editing || !draft) return null;
    const next = candidate();
    const change = oidcChange(editing, next);
    return change && !addressChanged({ ...editing, oidc: undefined }, { ...next, oidc: undefined }) ? change : null;
  })();
  const O = PROVIDER_EDITOR.OIDC;
  const addressBody =
    signInChange === "to_sign_in"
      ? n === 1 ? O.TO_SIGN_IN_ONE : O.TO_SIGN_IN(n)
      : signInChange === "to_paste"
        ? n === 1 ? O.TO_PASTE_ONE : O.TO_PASTE(n)
        : signInChange === "edited"
          ? n === 1 ? O.CONFIRM_BODY_ONE : O.CONFIRM_BODY(n)
          : n === 1
      ? keyKind
        ? PROVIDER_EDITOR.ADDRESS_BODY_KEY_ONE
        : PROVIDER_EDITOR.ADDRESS_BODY_ONE
      : keyKind
        ? PROVIDER_EDITOR.ADDRESS_BODY_KEY(n)
        : PROVIDER_EDITOR.ADDRESS_BODY(n);
  const blockedFor = defaultFor[0] && (harnesses.find((h) => h.id === defaultFor[0])?.display ?? defaultFor[0]);

  return (
    <Dialog open onOpenChange={(open) => !open && close()}>
      {/* The kind step has no description line; say so rather than let Radix
          point aria-describedby at nothing. */}
      <DialogContent {...(!draft && { "aria-describedby": undefined })}>
        <DialogHeader>
          <div className="flex flex-wrap items-center gap-2">
            <DialogTitle>{title}</DialogTitle>
            {editing && <Chip tone="neutral">{MODEL_PROVIDERS.KIND[editing.kind]}</Chip>}
          </div>
          {draft && <DialogDescription>{editorProvidesLine(draft.kind, draft.oidcOn)}</DialogDescription>}
        </DialogHeader>

        {!draft ? (
          <KindStep
            onPick={(kind) => {
              const d = newDraft(kind, harnesses);
              setDraft(d);
              setBaseline(editedText(d, audience));
            }}
            onCancel={onClose}
            subscriptionAvailable={subscriptionAvailable}
          />
        ) : (
          <form className="space-y-4" onSubmit={onSubmit}>
            {partial && (
              <div role="alert" className="flex flex-col gap-1 rounded-lg bg-danger-subtle p-3 text-body text-danger">
                <b className="font-semibold">{AVAILABILITY.CREATE_PARTIAL_TITLE}</b>
                <span>{partial.message}</span>
                <span>{AVAILABILITY.CREATE_PARTIAL_EVERYONE}</span>
              </div>
            )}
            <ProviderFields
              draft={draft}
              setDraft={setDraft}
              harnesses={harnesses}
              refused={refused}
              refusedAt={refusedAt}
              editing={editing}
              people={n}
              gatewayIssuer={list.gatewayIssuer}
              gatewayJwksPath={list.gatewayJwksPath}
              ssoGateway={list.gatewayIdentityBedrockSso === true}
            />
            {/* After Use with, above the footer (#923 editors.html section 2). An
                existing provider's list writes at once, never through Save. */}
            <div className="border-t border-border pt-4">
              {editing ? (
                <AvailabilityControl
                  kind="model_provider"
                  value={editing.id}
                  onlyHint={AVAILABILITY.MODEL_PROVIDER_ONLY_HINT}
                  note={AVAILABILITY.MODEL_PROVIDER_NOTE}
                />
              ) : (
                <AvailabilityDraft
                  draft={audience}
                  onChange={setAudience}
                  disabled={busy}
                  onlyHint={AVAILABILITY.MODEL_PROVIDER_ONLY_HINT}
                  note={AVAILABILITY.MODEL_PROVIDER_NOTE}
                />
              )}
            </div>
            {refused && (
              <div role="alert" className="rounded-lg border border-danger/30 bg-danger-subtle p-3 text-body text-danger">
                <b className="font-semibold">{PROVIDERS.SAVE_REFUSED_TITLE_ONE}</b>
                <p className="mt-0.5">{refused}</p>
              </div>
            )}
            {stale && <SavedElsewhereBanner documentText={stale} onDiscard={discard} />}
            {/* Sticky: on a phone the sign-in fields make this dialog scroll, and Save must stay in reach. */}
            <div data-testid="provider-editor-footer" className={`${INLINE_ACTIONS} ${STICKY_FOOTER}`}>
              {editing && (
                <Button type="button" variant="outline" className={`mr-auto ${DANGER_OUTLINE}`} disabled={busy} onClick={() => setConfirm("remove")}>
                  {PROVIDER_EDITOR.REMOVE}
                </Button>
              )}
              <Button type="button" variant="ghost" onClick={close}>
                {PROVIDER_EDITOR.CANCEL}
              </Button>
              <Button type="submit" disabled={busy || audience.pending}>
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
                {confirm === "address"
                  ? signInChange
                    ? O.CONFIRM_TITLE(title)
                    : PROVIDER_EDITOR.ADDRESS_TITLE(title)
                  : PROVIDER_EDITOR.DELETE_TITLE(title)}
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
                  onClick={() => void remove()}
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
  refused,
  refusedAt,
  editing,
  people,
  gatewayIssuer,
  gatewayJwksPath,
  ssoGateway,
}: {
  draft: ProviderDraft;
  setDraft: React.Dispatch<React.SetStateAction<ProviderDraft | null>>;
  harnesses: HarnessRow[];
  // The last refused save's sentence: the field it is about is outlined.
  refused: string | null;
  // The field the server's refusal named, when it named one.
  refusedAt: string | null;
  // The stored provider being edited, and how many people hold a credential for it.
  editing: ModelProvider | null;
  people: number;
  gatewayIssuer?: string;
  gatewayJwksPath?: string;
  // The install signs AWS requests at its proxy, so a sign-in row may send run identity.
  ssoGateway: boolean;
}) {
  const set = (patch: Partial<ProviderDraft>) => setDraft((d) => d && { ...d, ...patch });
  // The gateway preset last pressed: only decides which placeholders and hints show, and is never saved.
  const [preset, setPreset] = React.useState<PresetId | null>(null);
  // A new address needs its audience entered again (the server refuses a kept one), so an edit that
  // leaves the saved address clears it, and one that returns to it restores it.
  const setBaseUrl = (value: string) => {
    const saved = draft.gatewaySaved;
    const patch = isBedrock(draft.kind) ? { bedrockBaseUrl: value } : { baseUrl: value };
    set(saved ? { ...patch, gatewayAudience: sameAddress(value, saved.baseUrl) ? saved.audience : "" } : patch);
  };
  const endpoint = isEndpoint(draft.kind);
  const local = isLocal(draft.kind);
  const badField = refusedField(refused);
  const laptopBad = laptopBaseUrlRefused(draft);
  const bedrock = isBedrock(draft.kind);
  const sso = draft.kind === "bedrock_sso";
  const routeThrough =
    draft.kind === "anthropic_api_key" || draft.kind === "openai_api_key" || draft.kind === "bedrock_bearer" || (sso && ssoGateway);
  const host = directHost(draft);
  // A key kind's gateway address cleared while identity is still on: the server refuses it, so say so first.
  const addressCleared = routeThrough && draft.gatewayOn && !draftGatewayAddress(draft).trim();
  const [before, after] = (sso ? PROVIDER_EDITOR.ROUTE_THROUGH_HINT_SSO(host) : PROVIDER_EDITOR.ROUTE_THROUGH_HINT(host)).split(host);
  const rows = Object.keys(draft.harnesses).map((id) => harnesses.find((h) => h.id === id) ?? { id, display: id });
  // E3's "How people sign in" toggle — the ONE field that reaches bedrock_bearer
  // (never a kind-step option of its own). A segmented radio group
  // (role="radiogroup" Buttons over useRovingRadio, not a native radiogroup —
  // CONSOLE-RULES' precedent for a two-option choice with the mock's segmented
  // look).
  const operator = useOperator();
  // What saving the choice would delete, read under the choice and again in the confirmation.
  const switchNote = (() => {
    if (!editing || people === 0 || !endpoint) return undefined;
    const change = oidcChange(editing, providerFrom(draft, editing, []));
    const t = PROVIDER_EDITOR.OIDC;
    if (change === "to_sign_in") return people === 1 ? t.TO_SIGN_IN_ONE : t.TO_SIGN_IN(people);
    if (change === "to_paste") return people === 1 ? t.TO_PASTE_ONE : t.TO_PASTE(people);
    return undefined;
  })();
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

      {endpoint && !editing && (
        <GatewayPresets
          value={preset}
          onPick={(id) => {
            setPreset(id);
            setDraft((d) => d && applyPreset(d, id));
          }}
        />
      )}

      <Field label={PROVIDER_EDITOR.NAME} htmlFor="mp-name" hint={PROVIDER_EDITOR.NAME_HINT}>
        <Input id="mp-name" value={draft.name} onChange={(e) => set({ name: e.target.value })} />
      </Field>

      {bedrock && (
        <>
          {sso && <p className="text-xs font-medium text-foreground">{PROVIDER_EDITOR.IDC_GROUP}</p>}
          <Field label={sso ? PROVIDER_EDITOR.REGION : PROVIDER_EDITOR.REGION_BEARER} htmlFor="mp-region" required={bedrock}>
            <Input
              id="mp-region"
              required={bedrock}
              className="font-mono"
              placeholder="us-east-1"
              value={draft.region}
              onChange={(e) => set({ region: e.target.value })}
            />
          </Field>
          {sso && (
            <>
              <Field label={AGENTS.FIELD_SSO_START_URL} htmlFor="mp-sso-start-url" required={sso}>
                <Input
                  id="mp-sso-start-url"
                  required={sso}
                  className="font-mono"
                  placeholder="https://"
                  value={draft.ssoStartUrl}
                  onChange={(e) => set({ ssoStartUrl: e.target.value })}
                />
              </Field>
              <Field label={AGENTS_EXTRA.FIELD_SSO_ACCOUNT_ID} htmlFor="mp-sso-account-id">
                <Input
                  id="mp-sso-account-id"
                  className="font-mono"
                  value={draft.ssoAccountId}
                  onChange={(e) => set({ ssoAccountId: e.target.value })}
                />
              </Field>
              <Field label={AGENTS_EXTRA.FIELD_SSO_ROLE_NAME} htmlFor="mp-sso-role-name" hint={PROVIDER_EDITOR.SSO_SETUP_HINT}>
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

      {local && (
        <>
          <Field
            label={PROVIDER_EDITOR.BASE_URL}
            htmlFor="mp-base-url"
            hint={
              <>
                {laptopBad && <span className="mb-1 block text-danger">{LOCAL_MODEL_EDITOR.LAPTOP_BASE_URL}</span>}
                {LOCAL_MODEL_EDITOR.BASE_URL_HINT}
              </>
            }
          >
            <Input
              id="mp-base-url"
              className="font-mono"
              aria-invalid={badField === "base_url" || laptopBad || undefined}
              value={draft.baseUrl}
              onChange={(e) => set({ baseUrl: e.target.value })}
            />
          </Field>
          <div className="space-y-2">
            <div className="flex items-center gap-2.5">
              <Switch
                checked={draft.useToken}
                onChange={(useToken) => set({ useToken })}
                label={LOCAL_MODEL_EDITOR.TOKEN_SWITCH}
              />
              <span className="text-body">{LOCAL_MODEL_EDITOR.TOKEN_SWITCH}</span>
            </div>
            {draft.useToken ? (
              <>
                <div className="grid gap-4 sm:grid-cols-2">
                  <Field label={PROVIDER_EDITOR.AUTH_HEADER} htmlFor="mp-auth-header">
                    <Input id="mp-auth-header" className="font-mono" value={draft.authHeader} onChange={(e) => set({ authHeader: e.target.value })} />
                  </Field>
                  <Field label={PROVIDER_EDITOR.VALUE_FORMAT} htmlFor="mp-auth-format">
                    <Input id="mp-auth-format" className="font-mono" value={draft.authFormat} onChange={(e) => set({ authFormat: e.target.value })} />
                  </Field>
                </div>
                <p className="text-xs leading-snug text-muted-foreground">{PROVIDER_EDITOR.PROVIDES_TOKEN}</p>
              </>
            ) : (
              <p className="text-xs leading-snug text-muted-foreground">{LOCAL_MODEL_EDITOR.TOKEN_OFF_HINT}</p>
            )}
          </div>
          <LaptopRunnersSwitch on={draft.laptopRunners} onChange={(laptopRunners) => set({ laptopRunners })} />
          <Field label={LOCAL_MODEL_EDITOR.MODELS} htmlFor="mp-models">
            <ModelChips
              id="mp-models"
              value={draft.models}
              onChange={(models) => set({ models })}
              isBad={(name) => refusedModel(refused, name)}
              invalid={badField === "models"}
            />
          </Field>
          <fieldset className="space-y-2 border-t border-border pt-4">
            <legend className="text-sm font-medium">{LOCAL_MODEL_EDITOR.LIMITS}</legend>
            <div className="grid gap-3 sm:grid-cols-3">
              {(
                [
                  ["maxContext", "mp-max-context", LOCAL_MODEL_EDITOR.CONTEXT, LOCAL_MODEL_DEFAULTS.max_context, "max_context"],
                  ["maxOutput", "mp-max-output", LOCAL_MODEL_EDITOR.REPLY, LOCAL_MODEL_DEFAULTS.max_output, "max_output"],
                  ["keepAlive", "mp-keep-alive", LOCAL_MODEL_EDITOR.KEEP_ALIVE, LOCAL_MODEL_DEFAULTS.keep_alive_seconds, "keep_alive_seconds"],
                ] as const
              ).map(([key, id, label, placeholder, wire]) => (
                <Field key={id} label={label} htmlFor={id} className="space-y-1">
                  <Input
                    id={id}
                    type="number"
                    className="font-mono"
                    placeholder={String(placeholder)}
                    aria-invalid={badField === wire || undefined}
                    value={draft[key]}
                    onChange={(e) => set({ [key]: e.target.value })}
                  />
                </Field>
              ))}
            </div>
            <p className="text-xs leading-snug text-muted-foreground">{LOCAL_MODEL_EDITOR.LIMITS_HINT}</p>
          </fieldset>
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
              aria-invalid={badField === "base_url" || undefined}
              value={draft.baseUrl}
              onChange={(e) => setBaseUrl(e.target.value)}
            />
          </Field>
          <OidcChoice draft={draft} set={set} operator={operator} note={switchNote} />
          {draft.oidcOn && <OidcFields draft={draft} set={set} refusedAt={refusedAt} />}
          <div className="grid gap-4 sm:grid-cols-2">
            <Field label={PROVIDER_EDITOR.AUTH_HEADER} htmlFor="mp-auth-header">
              <Input id="mp-auth-header" className="font-mono" value={draft.authHeader} onChange={(e) => set({ authHeader: e.target.value })} />
            </Field>
            <Field label={PROVIDER_EDITOR.VALUE_FORMAT} htmlFor="mp-auth-format">
              <Input id="mp-auth-format" className="font-mono" value={draft.authFormat} onChange={(e) => set({ authFormat: e.target.value })} />
            </Field>
          </div>
          <ExtraHeaders
            rows={draft.headers}
            onChange={(headers) => set({ headers })}
            valueHint={draft.headers.some((h) => h.placeholder && !h.value) ? PROVIDER_EDITOR.PORTKEY_SLUG_HINT : undefined}
          />
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
              aria-invalid={badField === "base_url" || addressCleared || undefined}
              value={draftGatewayAddress(draft)}
              onChange={(e) => setBaseUrl(e.target.value)}
            />
          </Field>
        )
      )}

      <GatewayIdentity
        draft={draft}
        set={set}
        issuer={gatewayIssuer}
        jwksPath={gatewayJwksPath}
        ssoGateway={ssoGateway}
        addressCleared={addressCleared}
        invalid={badField === "gateway_audience"}
      />

      <fieldset className="space-y-2">
        <legend className="text-sm font-medium">{PROVIDER_EDITOR.USE_WITH}</legend>
        {rows.map((h) => (
          <HarnessBlock
            key={h.id}
            harness={h}
            draft={draft}
            refused={refused}
            modelPlaceholder={preset === "openrouter" && h.id === "codex-cli" ? PROVIDER_EDITOR.OPENROUTER_MODEL_PLACEHOLDER : undefined}
            modelFieldHint={preset === "openrouter" && h.id === "codex-cli" ? PROVIDER_EDITOR.OPENROUTER_MODEL_HINT : undefined}
            onChange={(patch) =>
              setDraft((d) => d && { ...d, harnesses: { ...d.harnesses, [h.id]: { ...d.harnesses[h.id], ...patch } } })
            }
          />
        ))}
      </fieldset>
    </>
  );
}

// The kinds that may send run identity to a gateway (gatewayKind) each need an address, or the token would go to the vendor.
const sameAddress = (a: string, b: string) => a.trim().replace(/\/$/, "") === b.trim().replace(/\/$/, "");
// The issuer an install gets when it never names its own trust domain.
const DEFAULT_ISSUER = "spiffe://wardyn.local";

// "Send the person and run to this gateway" (gateway_identity), drawn by the gateway-identity boards M1 and M2.
// Kinds that can't use it show the switch off with the reason, as Use with does for an agent.
function GatewayIdentity({
  draft,
  set,
  issuer,
  jwksPath,
  ssoGateway,
  addressCleared,
  invalid,
}: {
  draft: ProviderDraft;
  set: (patch: Partial<ProviderDraft>) => void;
  issuer?: string;
  jwksPath?: string;
  ssoGateway: boolean;
  addressCleared: boolean;
  invalid: boolean;
}) {
  const g = PROVIDER_EDITOR.GATEWAY;
  const operator = useOperator();
  const root = React.useRef<HTMLDivElement>(null);
  const noteId = React.useId();
  const reasonId = React.useId();
  const sso = draft.kind === "bedrock_sso";
  // A sign-in row is refused only where the install cannot sign AWS requests at its proxy.
  const refusedKind = !gatewayKind(draft.kind) || (sso && !ssoGateway);
  const needsAddress = !refusedKind && !draft.gatewayOn && !draftGatewayAddress(draft).trim() && draft.kind !== "custom_endpoint";
  const reason = refusedKind
    ? g.NOT_AVAILABLE[draft.kind as keyof typeof g.NOT_AVAILABLE]
    : needsAddress
      ? g.NEEDS_ADDRESS(directHost(draft))
      : undefined;
  // Turning identity off is always safe: a sign-in row already on where the install cannot sign can be switched off.
  const offOnly = sso && refusedKind && draft.gatewayOn;
  const disabled = !operator || (!!reason && !offOnly);
  // Switch takes no aria-describedby, so the sentence is attached to the control here, to be read with it.
  const describedBy = reason ? reasonId : [noteId, !operator && reasonId].filter(Boolean).join(" ");
  React.useEffect(() => {
    root.current?.querySelector('[role="switch"]')?.setAttribute("aria-describedby", describedBy);
  }, [describedBy]);
  const saved = draft.gatewaySaved;
  const moved = !!saved && !sameAddress(draftGatewayAddress(draft), saved.baseUrl);
  const keyAddress = jwksPath ? `${window.location.origin}${jwksPath}` : "";

  return (
    <div ref={root} className="space-y-2">
      <div className="flex items-center gap-2.5">
        <Switch checked={draft.gatewayOn && gatewayKind(draft.kind)} onChange={(gatewayOn) => set({ gatewayOn })} disabled={disabled} label={g.SWITCH} />
        <span className="text-body">{g.SWITCH}</span>
      </div>
      {reason ? (
        <p id={reasonId} className="text-xs leading-snug text-muted-foreground">
          {sso && refusedKind ? withSignerVar(reason ?? "", g.SIGNER_VAR) : reason}
        </p>
      ) : (
        <>
          {draft.gatewayOn && <Chip tone="warning">{g.CHIP}</Chip>}
          {addressCleared && (
            <p role="alert" className="text-xs leading-snug text-danger">
              {g.ADDRESS_CLEARED}
            </p>
          )}
          <p id={noteId} className="text-body">
            {sso ? g.DISCLOSURE_BEDROCK_SSO : g.DISCLOSURE}
          </p>
          <a href={g.CLAIMS_URL} target="_blank" rel="noreferrer" className="block text-xs text-info underline-offset-2 hover:underline">
            {g.CLAIMS_LINK}
          </a>
          {!operator && (
            <p id={reasonId} className="text-xs leading-snug text-muted-foreground">
              {OPERATOR_ONLY_REASON}
            </p>
          )}
          {!draft.gatewayOn && saved && <p className="text-xs leading-snug text-muted-foreground">{g.OFF_NOTE}</p>}
          {draft.gatewayOn && (
            <div className="space-y-4 rounded-lg border border-border p-3">
              <Field
                label={g.AUDIENCE}
                htmlFor="mp-gateway-audience"
                required
                hint={moved ? <span className="text-danger">{g.ADDRESS_CHANGED}</span> : g.AUDIENCE_HINT}
              >
                <Input
                  id="mp-gateway-audience"
                  required
                  className="font-mono"
                  aria-invalid={invalid || (moved && !draft.gatewayAudience.trim()) || undefined}
                  value={draft.gatewayAudience}
                  onChange={(e) => set({ gatewayAudience: e.target.value })}
                />
              </Field>
              {issuer && (
                <Field
                  label={g.ISSUER}
                  hint={
                    <>
                      {g.ISSUER_HINT}
                      {issuer === DEFAULT_ISSUER && <span className="mt-1 block text-warning">{g.ISSUER_DEFAULT}</span>}
                    </>
                  }
                >
                  <CopyValue text={issuer} label={g.COPY_ISSUER} />
                </Field>
              )}
              {keyAddress && (
                <Field label={g.KEY_ADDRESS} hint={`${g.KEY_ADDRESS_HINT} ${g.KEY_ADDRESS_OTHER}${saved ? "" : ` ${g.KEY_ADDRESS_UNSAVED}`}`}>
                  <CopyValue text={keyAddress} label={g.COPY_KEY_ADDRESS} />
                </Field>
              )}
              <GatewayInspection draft={draft} set={set} disabled={!operator} />
              <p className="text-xs leading-snug text-muted-foreground">{g.MAY_CHANGE}</p>
            </div>
          )}
        </>
      )}
    </div>
  );
}

// A value the admin copies into their gateway: shown whole, with a Copy control beside it.
function CopyValue({ text, label }: { text: string; label: string }) {
  return (
    <div className="flex items-center justify-between gap-2 rounded-md border border-border bg-input-background px-3 py-2">
      <code className="min-w-0 break-all font-mono text-xs">{text}</code>
      <CopyButton
        text={text}
        label={label}
        className="shrink-0 gap-1.5 rounded-md border border-border px-2.5 py-1 text-xs outline-none focus-visible:ring-[3px] focus-visible:ring-ring"
      >
        {PROVIDER_EDITOR.GATEWAY.COPY}
      </CopyButton>
    </div>
  );
}
