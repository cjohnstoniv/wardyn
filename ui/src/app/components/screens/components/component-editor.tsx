/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The organisation's component editor: a dialog over the catalog. An org row may
// share an operator secret (header delivery only) and may send a header over
// plain HTTP; a person's saved row may do neither, which is why this editor
// exists apart from the member's own. The server validates the whole definition
// (types.ComponentDefinition.Validate), so a refusal is shown as sent.
import * as React from "react";
import { Loader2, Plus, X } from "lucide-react";
import { components as api } from "../../../lib/api/components";
import { modelProviders } from "../../../lib/api/model-providers";
import { PROVIDER_EDITOR } from "../../../lib/model-providers-copy";
import { getErrorMessage } from "../../../lib/format";
import { useUnsavedGuard } from "../../../lib/use-unsaved-guard";
import type { Component, ComponentDeliveryMode } from "../../../lib/types";
import { Button } from "../../ui/button";
import { Checkbox } from "../../ui/checkbox";
import { CopyButton } from "../../wardyn/copy-button";
import { Chip } from "../../wardyn/primitives";
import { Dialog, DialogContent, DialogFooter, DialogHeader, DialogTitle } from "../../ui/dialog";
import { Input } from "../../ui/input";
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "../../ui/select";
import { Textarea } from "../../ui/textarea";
import { COMPONENTS_ADMIN as T } from "../../wardyn/copy/components-admin";
import { Field } from "../../wardyn/form-primitives";
import { Segmented } from "../../wardyn/segmented";
import {
  audienceEditable,
  blankSecret,
  blankSetting,
  draftOf,
  headerHostChoices,
  hostLines,
  newUuid,
  requestOf,
  withStored,
  type ComponentDraft,
  type SecretDraft,
} from "./component-editor-model";

// A save refused for the audience: a duplicate, no provider with identity on, or a value the server won't take.
const AUDIENCE_REFUSAL = /gateway[_ ]audience/i;

const MODES: { value: ComponentDeliveryMode; label: string }[] = [
  { value: "header", label: T.DEL_HEADER },
  { value: "env", label: T.DEL_ENV },
  { value: "file", label: T.DEL_FILE },
];

function SecretRow({
  n,
  secret,
  hostChoices,
  onChange,
  onRemove,
}: {
  n: number;
  secret: SecretDraft;
  hostChoices: string[];
  onChange: (s: SecretDraft) => void;
  onRemove: () => void;
}) {
  const id = `component-secret-${secret.key}`;
  const header = secret.mode === "header";
  const set = (patch: Partial<SecretDraft>) => onChange({ ...secret, ...patch });
  const host = secret.host || (hostChoices.length === 1 ? hostChoices[0] : "");
  return (
    <div className="space-y-3 rounded-lg border border-border p-3" data-testid="component-secret-row">
      <div className="flex items-end gap-2">
        <Field label={T.SECRET_NAME} htmlFor={`${id}-name`} className="flex-1">
          <Input
            id={`${id}-name`}
            value={secret.secret_name}
            onChange={(e) => set({ secret_name: e.target.value })}
            className="font-mono"
            autoComplete="off"
            spellCheck={false}
          />
        </Field>
        <Button type="button" variant="ghost" size="icon" aria-label={T.REMOVE_SECRET(n)} onClick={onRemove}>
          <X className="size-4" />
        </Button>
      </div>

      <Field label={T.DELIVERY} className="@container">
        <Segmented
          value={secret.mode}
          options={MODES}
          onChange={(mode) => set({ mode, ...(mode !== "header" && { shared: false }) })}
          stackOnPhone
        />
      </Field>
      <p className="-mt-1 text-xs text-muted-foreground">{header ? T.DEL_HEADER_HINT : T.DEL_RESIDENT_HINT}</p>

      {header && (
        <div className="space-y-3">
          <Field label={T.HEADER_HOST} htmlFor={`${id}-host`} hint={hostChoices.length > 0 ? T.HEADER_HOST_HINT : T.HEADER_HOST_NONE}>
            <Select value={host} onValueChange={(v) => set({ host: v })} disabled={hostChoices.length === 0}>
              <SelectTrigger id={`${id}-host`} aria-describedby={`${id}-host-hint`}>
                <SelectValue />
              </SelectTrigger>
              <SelectContent>
                {hostChoices.map((h) => (
                  <SelectItem key={h} value={h}>
                    {h}
                  </SelectItem>
                ))}
              </SelectContent>
            </Select>
          </Field>
          <div className="grid gap-3 sm:grid-cols-2">
            <Field label={T.HEADER_NAME} htmlFor={`${id}-header`}>
              <Input
                id={`${id}-header`}
                value={secret.header}
                placeholder={T.HEADER_NAME_PLACEHOLDER}
                onChange={(e) => set({ header: e.target.value })}
                className="font-mono"
                autoComplete="off"
              />
            </Field>
            <Field label={T.HEADER_FORMAT} htmlFor={`${id}-format`}>
              <Input
                id={`${id}-format`}
                value={secret.format}
                placeholder={T.HEADER_FORMAT_PLACEHOLDER}
                onChange={(e) => set({ format: e.target.value })}
                className="font-mono"
                autoComplete="off"
              />
            </Field>
          </div>
          <p className="-mt-1 text-xs text-muted-foreground">{T.HEADER_FORMAT_HINT}</p>
          <div className="flex items-start gap-2.5">
            <Checkbox
              id={`${id}-plain`}
              className="mt-0.5"
              checked={secret.plain_http}
              onCheckedChange={(v) => set({ plain_http: v === true })}
            />
            <label htmlFor={`${id}-plain`} className="cursor-pointer">
              <span className="block text-sm text-foreground">{T.PLAIN_HTTP}</span>
              <span className="block text-xs text-muted-foreground">{T.PLAIN_HTTP_HINT}</span>
            </label>
          </div>
        </div>
      )}

      {secret.mode === "env" && (
        <Field label={T.VAR} htmlFor={`${id}-var`} hint={T.VAR_HINT}>
          <Input id={`${id}-var`} value={secret.var} onChange={(e) => set({ var: e.target.value })} className="font-mono" autoComplete="off" />
        </Field>
      )}
      {secret.mode === "file" && (
        <Field label={T.FILE} htmlFor={`${id}-file`} hint={T.FILE_HINT}>
          <Input id={`${id}-file`} value={secret.file} onChange={(e) => set({ file: e.target.value })} className="font-mono" autoComplete="off" />
        </Field>
      )}

      <Field label={T.WHOSE} className="@container">
        <Segmented
          value={secret.shared ? "shared" : "own"}
          options={[
            { value: "own", label: T.WHOSE_OWN },
            { value: "shared", label: T.WHOSE_SHARED },
          ]}
          onChange={(v) => set({ shared: v === "shared" })}
          disabled={!header}
          stackOnPhone
        />
      </Field>
      <p className="-mt-1 text-xs text-muted-foreground">
        {!header ? T.WHOSE_SHARED_HEADER_ONLY : secret.shared ? T.WHOSE_SHARED_HINT : T.WHOSE_OWN_HINT}
      </p>
    </div>
  );
}

// What an admin copies into their gateway, shown whole beside a Copy control (as the model provider editor does).
function CopyRow({ text, label }: { text: string; label: string }) {
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

// The run identity an organisation's remote MCP server is sent (board GW-MCP): empty sends nothing. Set, it
// shows what the token carries and the issuer and key address the gateway checks it with.
function AudienceField({
  value,
  onChange,
  saved,
  refusal,
}: {
  value: string;
  onChange: (v: string) => void;
  // The stored row has an audience: a change to it reaches runs already going.
  saved: boolean;
  refusal: string | null;
}) {
  const g = PROVIDER_EDITOR.GATEWAY;
  const [gateway, setGateway] = React.useState<{ issuer?: string; jwksPath?: string }>({});
  React.useEffect(() => {
    let live = true;
    // Display only: an admin who can't read the providers is shown no issuer or key address.
    modelProviders
      .getModelProviders()
      .then((l) => live && setGateway({ issuer: l.gatewayIssuer, jwksPath: l.gatewayJwksPath }))
      .catch(() => {});
    return () => {
      live = false;
    };
  }, []);
  const on = value.trim() !== "";
  const keyAddress = gateway.jwksPath ? `${window.location.origin}${gateway.jwksPath}` : "";
  return (
    <fieldset className="space-y-2">
      <legend className="text-sm font-medium text-foreground">{T.MCP_GROUP}</legend>
      <Field label={T.AUDIENCE} htmlFor="component-gateway-audience" hint={T.AUDIENCE_HINT}>
        <Input
          id="component-gateway-audience"
          value={value}
          onChange={(e) => onChange(e.target.value)}
          className="font-mono"
          autoComplete="off"
          spellCheck={false}
          aria-invalid={!!refusal || undefined}
        />
      </Field>
      {refusal && (
        <p role="alert" className="text-xs leading-snug text-danger">
          {refusal}
        </p>
      )}
      {on && (
        <>
          <Chip tone="warning">{g.CHIP}</Chip>
          <p className="text-body">{T.AUDIENCE_SENDS}</p>
          <a href={g.CLAIMS_URL} target="_blank" rel="noreferrer" className="block text-xs text-info underline-offset-2 hover:underline">
            {g.CLAIMS_LINK}
          </a>
          {gateway.issuer && (
            <Field label={g.ISSUER} hint={g.ISSUER_HINT}>
              <CopyRow text={gateway.issuer} label={g.COPY_ISSUER} />
            </Field>
          )}
          {keyAddress && (
            <Field label={g.KEY_ADDRESS} hint={g.KEY_ADDRESS_HINT}>
              <CopyRow text={keyAddress} label={g.COPY_KEY_ADDRESS} />
            </Field>
          )}
        </>
      )}
      {saved && <p className="text-xs leading-snug text-muted-foreground">{T.AUDIENCE_SAVED_NOTE}</p>}
    </fieldset>
  );
}

export function ComponentEditor({
  component,
  onClose,
  onSaved,
}: {
  /** The row being edited, or null for a new component. */
  component: Component | null;
  onClose: () => void;
  /** The saved row's id, and whether it was created by this save. */
  onSaved: (id: string, created: boolean) => void;
}) {
  const [draft, setDraft] = React.useState<ComponentDraft>(() => draftOf(component));
  const [saving, setSaving] = React.useState(false);
  const [error, setError] = React.useState<string | null>(null);
  const savedAudience = component?.definition.mcp?.gateway_audience ?? "";
  const [audience, setAudience] = React.useState(savedAudience);
  const audienceRefused = audienceEditable(component) && !!error && AUDIENCE_REFUSAL.test(error);
  const hostChoices = headerHostChoices(hostLines(draft.hosts));
  const canSave = draft.name.trim() !== "" && !saving;

  // Leaving with edits that were not saved asks first; a save closes this dialog (onSaved). Row keys are random, so they are left out.
  const edited = JSON.stringify(
    { ...draft, secrets: draft.secrets.map(({ key: _k, ...s }) => s), settings: draft.settings.map(({ key: _k, ...s }) => s), audience },
    null,
    2,
  );
  const initial = React.useRef(edited).current;
  useUnsavedGuard("component-editor", edited !== initial, () => edited);

  const save = async () => {
    setSaving(true);
    setError(null);
    const id = component?.id ?? newUuid();
    try {
      await api.put(id, withStored(requestOf(draft), component, audience));
      onSaved(id, component === null);
    } catch (e) {
      setError(getErrorMessage(e));
    } finally {
      setSaving(false);
    }
  };

  return (
    <Dialog open onOpenChange={(o) => !o && onClose()}>
      <DialogContent className="sm:max-w-2xl">
        <DialogHeader>
          <DialogTitle>{component ? T.EDITOR_EDIT : T.EDITOR_NEW}</DialogTitle>
        </DialogHeader>

        <Field label={T.NAME} htmlFor="component-name" hint={T.NAME_HINT} required>
          <Input
            id="component-name"
            autoFocus
            required
            value={draft.name}
            onChange={(e) => setDraft({ ...draft, name: e.target.value })}
            autoComplete="off"
          />
        </Field>

        <Field label={T.DESCRIPTION} htmlFor="component-description" hint={T.DESCRIPTION_HINT}>
          <Input
            id="component-description"
            value={draft.description ?? ""}
            maxLength={500}
            onChange={(e) => setDraft({ ...draft, description: e.target.value })}
            autoComplete="off"
          />
        </Field>

        <Field label={T.HOSTS} htmlFor="component-hosts" hint={T.HOSTS_HINT}>
          <Textarea
            id="component-hosts"
            rows={3}
            value={draft.hosts}
            onChange={(e) => setDraft({ ...draft, hosts: e.target.value })}
            className="font-mono"
            spellCheck={false}
          />
        </Field>

        {audienceEditable(component) && (
          <AudienceField value={audience} onChange={setAudience} saved={savedAudience !== ""} refusal={audienceRefused ? error : null} />
        )}

        <fieldset className="min-w-0 space-y-3">
          <legend className="text-sm font-medium text-foreground">{T.SECRETS_LABEL}</legend>
          <p className="text-xs text-muted-foreground">{T.SECRETS_HINT}</p>
          {draft.secrets.map((s, i) => (
            <SecretRow
              key={s.key}
              n={i + 1}
              secret={s}
              hostChoices={hostChoices}
              onChange={(next) => setDraft({ ...draft, secrets: draft.secrets.map((x) => (x.key === s.key ? next : x)) })}
              onRemove={() => setDraft({ ...draft, secrets: draft.secrets.filter((x) => x.key !== s.key) })}
            />
          ))}
          <Button type="button" variant="outline" size="sm" onClick={() => setDraft({ ...draft, secrets: [...draft.secrets, blankSecret()] })}>
            <Plus className="size-3.5" />
            {T.ADD_SECRET}
          </Button>
        </fieldset>

        <fieldset className="space-y-3">
          <legend className="text-sm font-medium text-foreground">{T.CONFIG_LABEL}</legend>
          <p className="text-xs text-muted-foreground">{T.CONFIG_HINT}</p>
          {draft.settings.map((s, i) => (
            <div key={s.key} className="flex items-end gap-2">
              <Field label={T.SETTING_NAME} htmlFor={`component-setting-${s.key}-name`} className="flex-1">
                <Input
                  id={`component-setting-${s.key}-name`}
                  value={s.name}
                  className="font-mono"
                  autoComplete="off"
                  onChange={(e) =>
                    setDraft({ ...draft, settings: draft.settings.map((x) => (x.key === s.key ? { ...x, name: e.target.value } : x)) })
                  }
                />
              </Field>
              <Field label={T.SETTING_VALUE} htmlFor={`component-setting-${s.key}-value`} className="flex-1">
                <Input
                  id={`component-setting-${s.key}-value`}
                  value={s.value}
                  className="font-mono"
                  autoComplete="off"
                  onChange={(e) =>
                    setDraft({ ...draft, settings: draft.settings.map((x) => (x.key === s.key ? { ...x, value: e.target.value } : x)) })
                  }
                />
              </Field>
              <Button
                type="button"
                variant="ghost"
                size="icon"
                aria-label={T.REMOVE_SETTING(i + 1)}
                onClick={() => setDraft({ ...draft, settings: draft.settings.filter((x) => x.key !== s.key) })}
              >
                <X className="size-4" />
              </Button>
            </div>
          ))}
          <Button type="button" variant="outline" size="sm" onClick={() => setDraft({ ...draft, settings: [...draft.settings, blankSetting()] })}>
            <Plus className="size-3.5" />
            {T.ADD_SETTING}
          </Button>
        </fieldset>

        {error && !audienceRefused && (
          <p role="alert" className="text-body text-danger">
            {error}
          </p>
        )}
        <DialogFooter>
          <Button variant="ghost" disabled={saving} onClick={onClose}>
            {T.CANCEL}
          </Button>
          <Button disabled={!canSave} onClick={() => void save()}>
            {saving && <Loader2 className="size-3.5 animate-spin" />}
            {T.SAVE}
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}
