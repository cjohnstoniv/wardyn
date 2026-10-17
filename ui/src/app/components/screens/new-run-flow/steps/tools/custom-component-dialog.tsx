/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Custom component, the short form: a name, the hosts it reaches and one saved
// secret, used for this run or saved to reuse. A header secret goes to one of the
// hosts, so a second host brings a Header host choice (DR-0050). More options
// opens the full editor the account page uses; it is imported, not copied.

import * as React from "react";
import { components as api } from "../../../../../lib/api/components";
import { getErrorMessage } from "../../../../../lib/format";
import type { ComponentRef } from "../../../../../lib/types/components";
import { Button } from "../../../../ui/button";
import { Input } from "../../../../ui/input";
import { NR_TOOLS } from "../../../../wardyn/copy/new-run/tools";
import { CustomComponentDialog as FullEditor } from "../../checks/custom-component-dialog";
import { blankSecret, headerHostChoices, requestOf, validateDraft, type ComponentDraft } from "../../checks/custom-component-form-model";
import { SELECT_CLASS, ToolDialog, ToolField } from "./tool-dialog";

export interface CustomComponentProps {
  names: readonly string[];
  residentAllowed: boolean;
  /** Where a person adds a secret of their own. */
  secretsPath: string;
  onAttach: (ref: ComponentRef) => void;
  onClose: () => void;
}

const hostsOf = (text: string): string[] => text.split(/[\s,]+/).filter(Boolean);

export function CustomComponentDialog({ names, residentAllowed, secretsPath, onAttach, onClose }: CustomComponentProps) {
  const id = React.useId();
  const [name, setName] = React.useState("");
  const [hostText, setHostText] = React.useState("");
  const [secret, setSecret] = React.useState("");
  const [headerHost, setHeaderHost] = React.useState("");
  const [save, setSave] = React.useState(false);
  const [error, setError] = React.useState("");
  const [busy, setBusy] = React.useState(false);
  const [full, setFull] = React.useState(false);

  const hosts = hostsOf(hostText);
  const choices = headerHostChoices(hosts);
  const askHost = !!secret && choices.length >= 2;
  const draftOfForm = (): ComponentDraft => ({
    name, hosts: hosts.join("\n"), settings: [],
    secrets: secret ? [{ ...blankSecret(), secret_name: secret, mode: "header", host: askHost ? headerHost : "" }] : [],
  });

  const confirm = async () => {
    const draft = draftOfForm();
    const problems = validateDraft(draft, { named: save, residentAllowed });
    const first = Object.values(problems)[0] ?? (!hosts.length || (save && !name.trim()) ? NR_TOOLS.CC_ERR : "");
    setError(first);
    if (first) return;
    const req = requestOf(draft);
    if (!save) return onAttach({ inline: req.definition, ...(req.name && { name: req.name }) });
    setBusy(true);
    try {
      onAttach({ id: (await api.saveMine(req)).id });
    } catch (e) {
      setError(getErrorMessage(e));
      setBusy(false);
    }
  };

  if (full) {
    return <FullEditor context="run" residentAllowed={residentAllowed} secretsPath={secretsPath} onClose={onClose} onAttach={onAttach} />;
  }
  return (
    <ToolDialog
      title={NR_TOOLS.CUSTOM_COMPONENT}
      intro={NR_TOOLS.CC_INTRO}
      firstField={`${id}-name`}
      confirm={NR_TOOLS.ADD_TO_RUN}
      busy={busy}
      onConfirm={() => void confirm()}
      onClose={onClose}
      extra={<Button type="button" variant="outline" disabled={busy} onClick={() => setFull(true)}>{NR_TOOLS.CC_MORE_OPTIONS}</Button>}
    >
      <ToolField id={`${id}-name`} label={NR_TOOLS.CC_NAME}>
        <Input id={`${id}-name`} value={name} placeholder={NR_TOOLS.CC_NAME_PLACEHOLDER} autoComplete="off" onChange={(e) => setName(e.target.value)} />
      </ToolField>
      <ToolField id={`${id}-hosts`} label={NR_TOOLS.CC_HOSTS}>
        <Input id={`${id}-hosts`} value={hostText} placeholder={NR_TOOLS.CC_HOSTS_PLACEHOLDER} autoComplete="off" spellCheck={false} onChange={(e) => setHostText(e.target.value)} />
      </ToolField>
      <ToolField id={`${id}-secret`} label={NR_TOOLS.CC_SECRET}>
        <select id={`${id}-secret`} className={SELECT_CLASS} value={secret} aria-describedby={`${id}-secret-help`} onChange={(e) => setSecret(e.target.value)}>
          <option value="">{NR_TOOLS.CC_SECRET_NONE}</option>
          {names.map((n) => <option key={n} value={n}>{n}</option>)}
        </select>
        <p id={`${id}-secret-help`} className="text-sm text-muted-foreground">{NR_TOOLS.CC_SECRET_HELP}</p>
      </ToolField>
      {askHost && (
        <ToolField id={`${id}-header-host`} label={NR_TOOLS.CC_HEADER_HOST}>
          <select id={`${id}-header-host`} className={SELECT_CLASS} value={headerHost} onChange={(e) => setHeaderHost(e.target.value)}>
            <option value="" />
            {choices.map((h) => <option key={h} value={h}>{h}</option>)}
          </select>
        </ToolField>
      )}
      <label className="flex items-center gap-2 text-sm">
        <input type="checkbox" checked={save} onChange={(e) => setSave(e.target.checked)} />
        {NR_TOOLS.CC_SAVE}
      </label>
      {error && <p role="alert" className="text-sm text-danger">{error}</p>}
    </ToolDialog>
  );
}
