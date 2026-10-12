/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Add a secret: one of the person's saved secrets, delivered as a request header
// the proxy adds for one host, or placed in the sandbox as an environment
// variable or a file. Placing it in the sandbox needs the organisation's
// resident-delivery switch on and the right to define a component.

import * as React from "react";
import type { ComponentRef } from "../../../../../lib/types/components";
import type { SecretOverride } from "../../../../../lib/types/new-run-contract";
import { Input } from "../../../../ui/input";
import { CUSTOM_COMPONENT as CC } from "../../../../wardyn/copy/components";
import { NR_TOOLS } from "../../../../wardyn/copy/new-run/tools";
import { envNameError, hostError } from "../../../new-run/custom-component-form-model";
import { residentSecretRef } from "./catalogue-edits";
import { SELECT_CLASS, ToolDialog, ToolField } from "./tool-dialog";

type Mode = "header" | "env" | "file";

export type SecretAddition = { kind: "header"; secret: SecretOverride } | { kind: "resident"; ref: ComponentRef };

export interface AddSecretDialogProps {
  /** The person's saved secret names. */
  names: readonly string[];
  /** Hosts already on the run, offered for the header's host. */
  hosts: readonly string[];
  /** False when the organisation has turned off environment-variable and file delivery. */
  residentAllowed: boolean;
  /** False when the person may not define components, which env and file delivery needs. */
  mayDefine: boolean;
  /** The sentence for why env and file are off: the organisation's switch, or the missing right. */
  residentOffReason: string;
  onAdd: (addition: SecretAddition) => void;
  onClose: () => void;
}

const FILE_NAME = /^[a-z0-9][a-z0-9_.-]{0,62}$/;

export function AddSecretDialog({ names, hosts, residentAllowed, mayDefine, residentOffReason, onAdd, onClose }: AddSecretDialogProps) {
  const base = React.useId();
  const [secret, setSecret] = React.useState(names[0] ?? "");
  const [mode, setMode] = React.useState<Mode>("header");
  const [host, setHost] = React.useState("");
  const [header, setHeader] = React.useState("");
  const [format, setFormat] = React.useState("");
  const [variable, setVariable] = React.useState("");
  const [file, setFile] = React.useState("");
  const [submitted, setSubmitted] = React.useState(false);
  const resident = residentAllowed && mayDefine;

  const hostValue = host.trim();
  const errors: Record<Mode, string> = {
    header: !hostValue ? NR_TOOLS.SECRET_ERR_HOST : (hostError(hostValue) ?? ""),
    env: !variable.trim() ? NR_TOOLS.SECRET_ERR_VARIABLE : (envNameError(variable.trim()) ?? ""),
    file: FILE_NAME.test(file.trim()) ? "" : NR_TOOLS.SECRET_ERR_FILE,
  };
  const error = submitted ? errors[mode] : "";

  const confirm = () => {
    setSubmitted(true);
    if (!secret || errors[mode]) return;
    if (mode === "header") {
      onAdd({ kind: "header", secret: { secret_name: secret, host: hostValue, ...(header.trim() && { header: header.trim() }), ...(format.trim() && { format: format.trim() }) } });
    } else if (mode === "env") {
      onAdd({ kind: "resident", ref: residentSecretRef(secret, { mode: "env", var: variable.trim() }) });
    } else {
      onAdd({ kind: "resident", ref: residentSecretRef(secret, { mode: "file", file: file.trim() }) });
    }
  };

  const delivery: { value: Mode; label: string; off: boolean }[] = [
    { value: "header", label: NR_TOOLS.DELIVERY_HEADER_OPTION, off: false },
    { value: "env", label: NR_TOOLS.DELIVERY_ENV_OPTION, off: !resident },
    { value: "file", label: NR_TOOLS.DELIVERY_FILE_OPTION, off: !resident },
  ];
  const fieldId = (name: string) => `${base}-${name}`;
  const described = (name: string) => (error ? `${fieldId(name)}-err` : undefined);

  return (
    <ToolDialog
      title={NR_TOOLS.SECRET_CARD_TITLE}
      intro={NR_TOOLS.SECRET_INTRO}
      firstField={fieldId("secret")}
      confirm={NR_TOOLS.ADD_TO_RUN}
      onConfirm={confirm}
      onClose={onClose}
    >
      <ToolField id={fieldId("secret")} label={NR_TOOLS.SECRET_LABEL}>
        <select id={fieldId("secret")} className={SELECT_CLASS} value={secret} aria-describedby={`${fieldId("secret")}-help`} onChange={(e) => setSecret(e.target.value)}>
          {names.map((n) => <option key={n} value={n}>{n}</option>)}
        </select>
        <p id={`${fieldId("secret")}-help`} className="text-sm text-muted-foreground">{NR_TOOLS.SECRET_HELP}</p>
      </ToolField>
      <fieldset className="flex flex-col gap-2">
        <legend className="text-sm font-medium">{NR_TOOLS.DELIVERY_LABEL}</legend>
        {delivery.map((d) => (
          <label key={d.value} className={d.off ? "flex items-center gap-2 text-sm opacity-50" : "flex items-center gap-2 text-sm"}>
            <input type="radio" name={`${base}-mode`} value={d.value} checked={mode === d.value} disabled={d.off} onChange={() => { setMode(d.value); setSubmitted(false); }} />
            {d.label}
          </label>
        ))}
        {!resident && <p className="text-sm text-muted-foreground">{residentOffReason}</p>}
      </fieldset>
      {mode === "header" && (
        <>
          <ToolField id={fieldId("host")} label={NR_TOOLS.SECRET_HOST_LABEL} error={error}>
            <Input id={fieldId("host")} list={`${base}-hosts`} value={host} autoComplete="off" spellCheck={false} aria-invalid={error ? true : undefined} aria-describedby={described("host")} onChange={(e) => { setHost(e.target.value); setSubmitted(false); }} />
            <datalist id={`${base}-hosts`}>{hosts.map((h) => <option key={h} value={h} />)}</datalist>
          </ToolField>
          <ToolField id={fieldId("header")} label={CC.HEADER_NAME}>
            <Input id={fieldId("header")} value={header} placeholder={CC.HEADER_NAME_PLACEHOLDER} autoComplete="off" spellCheck={false} onChange={(e) => setHeader(e.target.value)} />
          </ToolField>
          <ToolField id={fieldId("format")} label={CC.HEADER_FORMAT}>
            <Input id={fieldId("format")} value={format} placeholder={CC.HEADER_FORMAT_PLACEHOLDER} autoComplete="off" spellCheck={false} onChange={(e) => setFormat(e.target.value)} />
          </ToolField>
        </>
      )}
      {mode === "env" && (
        <ToolField id={fieldId("var")} label={NR_TOOLS.SECRET_VARIABLE_LABEL} error={error}>
          <Input id={fieldId("var")} value={variable} autoComplete="off" spellCheck={false} aria-invalid={error ? true : undefined} aria-describedby={described("var")} onChange={(e) => { setVariable(e.target.value); setSubmitted(false); }} />
        </ToolField>
      )}
      {mode === "file" && (
        <ToolField id={fieldId("file")} label={NR_TOOLS.SECRET_FILE_LABEL} error={error}>
          <Input id={fieldId("file")} value={file} autoComplete="off" spellCheck={false} aria-invalid={error ? true : undefined} aria-describedby={described("file")} onChange={(e) => { setFile(e.target.value); setSubmitted(false); }} />
          {file.trim() && <p className="font-mono text-sm text-muted-foreground">{NR_TOOLS.SECRET_FILE_PATH(file.trim())}</p>}
        </ToolField>
      )}
    </ToolDialog>
  );
}
