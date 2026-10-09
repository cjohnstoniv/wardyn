/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// One secret of the custom-component form: which stored secret, and how it
// reaches the run. A person's row has no "shared" choice and no plain-HTTP
// control (D6): a header secret they define is only ever sent over TLS.
import { X } from "lucide-react";
import type { ComponentDeliveryMode } from "../../../lib/types";
import { Button } from "../../ui/button";
import { Input } from "../../ui/input";
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "../../ui/select";
import { CUSTOM_COMPONENT as T } from "../../wardyn/copy/components";
import { Field } from "../../wardyn/form-primitives";
import { Segmented } from "../../wardyn/segmented";
import { secretField, type FormErrors, type SecretDraft } from "./custom-component-form-model";

const MODES: { value: ComponentDeliveryMode; label: string }[] = [
  { value: "header", label: T.DELIVERY_HEADER },
  { value: "env", label: T.DELIVERY_ENV },
  { value: "file", label: T.DELIVERY_FILE },
];
const HINT: Record<ComponentDeliveryMode, string> = {
  header: T.DELIVERY_HEADER_HINT,
  env: T.DELIVERY_ENV_HINT,
  file: T.DELIVERY_FILE_HINT,
};

/** The DOM id of a secret row's control, so a refused form can focus it. */
export const secretDomId = (s: { key: number }, field: string): string => `ccd-s-${s.key}-${field}`;

/** A field's sentence, tied to its control by id so a screen reader reads it with the field. */
export function FieldError({ id, text }: { id: string; text?: string }) {
  if (!text) return null;
  return (
    <p id={`${id}-error`} className="-mt-1 text-xs text-danger">
      {text}
    </p>
  );
}

/** What a control says about its error: `aria-invalid` and where to read it. */
export const errorProps = (id: string, text?: string) =>
  text ? ({ "aria-invalid": true, "aria-describedby": `${id}-error` } as const) : {};

export function SecretRow({
  n,
  secret,
  hostChoices,
  residentAllowed,
  errors,
  onChange,
  onRemove,
}: {
  n: number;
  secret: SecretDraft;
  hostChoices: string[];
  residentAllowed: boolean;
  errors: FormErrors;
  onChange: (s: SecretDraft) => void;
  onRemove: () => void;
}) {
  const set = (patch: Partial<SecretDraft>) => onChange({ ...secret, ...patch });
  const err = (field: string) => errors[secretField(secret, field)];
  const dom = (field: string) => secretDomId(secret, field);
  const host = secret.host || (hostChoices.length === 1 ? hostChoices[0] : "");
  // Env and file are offered only where the organisation allows them, but a row
  // already using one keeps its option so the refusal can name it.
  const options = MODES.filter((m) => residentAllowed || m.value === "header" || m.value === secret.mode);

  return (
    <div className="space-y-3 rounded-lg border border-border p-3" data-testid="custom-component-secret-row">
      <div className="flex items-end gap-2">
        <Field label={T.SECRET_NAME} htmlFor={dom("secret_name")} className="flex-1">
          <Input
            id={dom("secret_name")}
            value={secret.secret_name}
            onChange={(e) => set({ secret_name: e.target.value })}
            className="font-mono"
            autoComplete="off"
            spellCheck={false}
            {...errorProps(dom("secret_name"), err("secret_name"))}
          />
        </Field>
        <Button type="button" variant="ghost" size="icon" aria-label={T.REMOVE_SECRET(n)} onClick={onRemove}>
          <X className="size-4" />
        </Button>
      </div>
      <FieldError id={dom("secret_name")} text={err("secret_name")} />

      <Field label={T.DELIVERY}>
        <Segmented value={secret.mode} options={options} onChange={(mode) => set({ mode })} />
      </Field>
      <p className="-mt-1 text-xs text-muted-foreground">{HINT[secret.mode]}</p>
      <FieldError id={dom("mode")} text={err("mode")} />

      {secret.mode === "header" && (
        <div className="space-y-3">
          <Field label={T.HEADER_HOST} htmlFor={dom("host")} hint={hostChoices.length > 0 ? T.HEADER_HOST_HINT : T.HEADER_HOST_NONE}>
            <Select value={host} onValueChange={(v) => set({ host: v })} disabled={hostChoices.length === 0}>
              <SelectTrigger id={dom("host")} {...errorProps(dom("host"), err("host"))}>
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
          <FieldError id={dom("host")} text={err("host")} />
          <div className="grid gap-3 sm:grid-cols-2">
            <Field label={T.HEADER_NAME} htmlFor={dom("header")}>
              <Input
                id={dom("header")}
                value={secret.header}
                placeholder={T.HEADER_NAME_PLACEHOLDER}
                onChange={(e) => set({ header: e.target.value })}
                className="font-mono"
                autoComplete="off"
                {...errorProps(dom("header"), err("header"))}
              />
            </Field>
            <Field label={T.HEADER_FORMAT} htmlFor={dom("format")}>
              <Input
                id={dom("format")}
                value={secret.format}
                placeholder={T.HEADER_FORMAT_PLACEHOLDER}
                onChange={(e) => set({ format: e.target.value })}
                className="font-mono"
                autoComplete="off"
                {...errorProps(dom("format"), err("format"))}
              />
            </Field>
          </div>
          <FieldError id={dom("header")} text={err("header")} />
          <FieldError id={dom("format")} text={err("format")} />
          <p className="-mt-1 text-xs text-muted-foreground">{T.HEADER_FORMAT_HINT}</p>
        </div>
      )}

      {secret.mode === "env" && (
        <>
          <Field label={T.VAR} htmlFor={dom("var")} hint={T.VAR_HINT}>
            <Input
              id={dom("var")}
              value={secret.var}
              onChange={(e) => set({ var: e.target.value })}
              className="font-mono"
              autoComplete="off"
              {...errorProps(dom("var"), err("var"))}
            />
          </Field>
          <FieldError id={dom("var")} text={err("var")} />
        </>
      )}
      {secret.mode === "file" && (
        <>
          <Field label={T.FILE} htmlFor={dom("file")} hint={T.FILE_HINT}>
            <Input
              id={dom("file")}
              value={secret.file}
              onChange={(e) => set({ file: e.target.value })}
              className="font-mono"
              autoComplete="off"
              {...errorProps(dom("file"), err("file"))}
            />
          </Field>
          <FieldError id={dom("file")} text={err("file")} />
        </>
      )}
    </div>
  );
}
