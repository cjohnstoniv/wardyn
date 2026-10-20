/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Add an MCP server. A remote one is by URL: a streamable-HTTP server, with its token (one of the
// person's saved secrets) added by the proxy to each request. With features.mcp_local_servers on, a
// second tab starts a server as a command inside this run's sandbox. With it off the dialog is the
// remote one alone and says the other is Coming soon.

import * as React from "react";
import type { ComponentDelivery, ComponentRef } from "../../../../../lib/types/components";
import { Input } from "../../../../ui/input";
import { NR_RUN } from "../../../../wardyn/copy/new-run/run";
import { NR_SHARED } from "../../../../wardyn/copy/new-run/shared";
import { NR_TOOLS } from "../../../../wardyn/copy/new-run/tools";
import { Chip } from "../../../../wardyn/primitives";
import { Notice } from "../../../../wardyn/notice";
import { Segmented } from "../../../../wardyn/segmented";
import { envNameError, hostError } from "../../checks/custom-component-form-model";
import { FILE_NAME } from "./add-secret-dialog";
import { httpsParts, mcpLocalRef, mcpRef } from "./catalogue-edits";
import { commandProblem, defaultName } from "./mcp-local-form";
import { NamesFailed, SELECT_CLASS, ToolDialog, ToolField, type NamesFailure } from "./tool-dialog";

type Tab = "remote" | "local";
type Mode = "header" | "env" | "file";

export interface AddMcpDialogProps {
  names: readonly string[];
  namesFailure?: NamesFailure;
  /** features.mcp_local_servers: offer the server that runs in the sandbox. */
  localServers?: boolean;
  /** False when the organisation has turned off environment-variable and file delivery. */
  residentAllowed?: boolean;
  /** The sentence for why env and file are off. */
  residentOffReason?: string;
  onAdd: (ref: ComponentRef) => void;
  onClose: () => void;
}

const hostsOf = (text: string): string[] => text.split(/[\s,]+/).filter(Boolean);

export function AddMcpDialog({ names, namesFailure, localServers, residentAllowed, residentOffReason, onAdd, onClose }: AddMcpDialogProps) {
  const id = React.useId();
  const [tab, setTab] = React.useState<Tab>("remote");
  const local = !!localServers && tab === "local";
  const [name, setName] = React.useState("");
  const [url, setUrl] = React.useState("");
  const [token, setToken] = React.useState("");
  const [command, setCommand] = React.useState("");
  const [hostText, setHostText] = React.useState("");
  const [secret, setSecret] = React.useState("");
  const [mode, setMode] = React.useState<Mode>("header");
  const [headerHost, setHeaderHost] = React.useState("");
  const [variable, setVariable] = React.useState("");
  const [file, setFile] = React.useState("");
  const [submitted, setSubmitted] = React.useState(false);
  const host = httpsParts(url)?.hostname;
  const error = submitted && !host ? NR_TOOLS.MCP_ERR_URL : "";

  const problem = commandProblem(command);
  const commandError = problem === "empty" ? NR_TOOLS.MCP_ERR_COMMAND : problem === "unpinned" ? NR_TOOLS.MCP_ERR_PIN : "";
  const hosts = hostsOf(hostText);
  const hostsError = hosts.map(hostError).find((e) => e) ?? "";
  const deliveryErrors: Record<Mode, string> = {
    header: !headerHost.trim() ? NR_TOOLS.SECRET_ERR_HOST : (hostError(headerHost.trim()) ?? ""),
    env: !variable.trim() ? NR_TOOLS.SECRET_ERR_VARIABLE : (envNameError(variable.trim()) ?? ""),
    file: FILE_NAME.test(file.trim()) ? "" : NR_TOOLS.SECRET_ERR_FILE,
  };
  const deliveryError = secret ? deliveryErrors[mode] : "";
  const shown = (e: string) => (submitted ? e : "");

  const confirm = () => {
    setSubmitted(true);
    if (local) {
      if (commandError || hostsError || deliveryError) return;
      const delivery: ComponentDelivery =
        mode === "header" ? { mode, host: headerHost.trim() } : mode === "env" ? { mode, var: variable.trim() } : { mode, file: file.trim() };
      // A header goes to one of the component's hosts, so the one it names is among them.
      const reach = secret && mode === "header" && !hosts.includes(headerHost.trim()) ? [...hosts, headerHost.trim()] : hosts;
      onAdd(mcpLocalRef(name.trim() || defaultName(command), command, reach, secret ? { secret_name: secret, delivery } : undefined));
      return;
    }
    const ref = mcpRef(name.trim() || host || "", url, token || undefined);
    if (ref) onAdd(ref);
  };

  const field = (n: string) => `${id}-${n}`;
  const resident = residentAllowed !== false;
  const deliveries: { value: Mode; label: string; off: boolean }[] = [
    { value: "header", label: NR_TOOLS.DELIVERY_HEADER_OPTION, off: false },
    { value: "env", label: NR_TOOLS.DELIVERY_ENV_OPTION, off: !resident },
    { value: "file", label: NR_TOOLS.DELIVERY_FILE_OPTION, off: !resident },
  ];
  const described = (n: string, err: string) => (shown(err) ? `${field(n)}-err` : undefined);

  return (
    <ToolDialog
      title={NR_TOOLS.MCP_CARD_TITLE}
      intro={local ? undefined : NR_TOOLS.MCP_INTRO}
      header={localServers && (
        <Segmented<Tab>
          value={tab}
          onChange={(t) => { setTab(t); setSubmitted(false); }}
          options={[{ value: "remote", label: NR_TOOLS.MCP_TAB_REMOTE }, { value: "local", label: NR_TOOLS.MCP_TAB_LOCAL }]}
        />
      )}
      firstField={local ? field("where") : field("name")}
      confirm={NR_TOOLS.ADD_TO_RUN}
      onConfirm={confirm}
      onClose={onClose}
    >
      {local ? (
        <>
          <fieldset className="flex flex-col gap-2">
            <legend className="mb-2 text-sm font-medium">{NR_TOOLS.MCP_WHERE}</legend>
            <div className="flex flex-col gap-1.5 rounded-lg border border-primary bg-primary/10 px-3 py-2.5">
              <label className="flex items-center gap-2 text-sm font-medium">
                <input id={field("where")} type="radio" name={field("where")} checked readOnly aria-describedby={field("where-hint")} />
                {NR_TOOLS.MCP_WHERE_RUN}
              </label>
              <p id={field("where-hint")} className="pl-6 text-sm text-muted-foreground">{NR_TOOLS.MCP_WHERE_RUN_HINT}</p>
            </div>
            <div className="flex flex-col gap-1.5 rounded-lg border border-border bg-muted/50 px-3 py-2.5 text-muted-foreground">
              <label className="flex cursor-not-allowed items-center gap-2 text-sm font-medium">
                <input type="radio" name={field("where")} disabled aria-describedby={field("own-hint")} />
                {NR_TOOLS.MCP_WHERE_OWN} <Chip>{NR_SHARED.COMING_SOON}</Chip>
              </label>
              <p id={field("own-hint")} className="pl-6 text-sm">{NR_TOOLS.MCP_WHERE_OWN_HINT}</p>
            </div>
          </fieldset>
          <ToolField id={field("name")} label={NR_TOOLS.MCP_NAME}>
            <Input id={field("name")} value={name} placeholder={NR_TOOLS.MCP_LOCAL_NAME_PLACEHOLDER} autoComplete="off" onChange={(e) => setName(e.target.value)} />
          </ToolField>
          <div className="flex flex-col gap-2">
            <label htmlFor={field("command")} className="text-sm font-medium">{NR_RUN.COMMAND_LABEL}</label>
            <Input
              id={field("command")}
              className="font-mono"
              value={command}
              autoComplete="off"
              spellCheck={false}
              aria-invalid={shown(commandError) ? true : undefined}
              aria-describedby={`${field("command")}-help ${described("command", commandError) ?? ""}`.trim()}
              onChange={(e) => { setCommand(e.target.value); setSubmitted(false); }}
            />
            <p id={`${field("command")}-help`} className="text-sm text-muted-foreground">{NR_TOOLS.MCP_COMMAND_HELP}</p>
            {shown(commandError) && <p id={`${field("command")}-err`} role="alert" className="text-sm text-danger">{commandError}</p>}
            <p className="text-sm text-muted-foreground">{NR_TOOLS.NOTE_COMMAND}</p>
          </div>
          <ToolField id={field("hosts")} label={NR_TOOLS.CC_HOSTS} error={shown(hostsError)}>
            <Input
              id={field("hosts")}
              value={hostText}
              placeholder={NR_TOOLS.MCP_HOSTS_PLACEHOLDER}
              autoComplete="off"
              spellCheck={false}
              aria-invalid={shown(hostsError) ? true : undefined}
              aria-describedby={`${field("hosts")}-help ${described("hosts", hostsError) ?? ""}`.trim()}
              onChange={(e) => { setHostText(e.target.value); setSubmitted(false); }}
            />
            <p id={`${field("hosts")}-help`} className="text-sm text-muted-foreground">{NR_TOOLS.MCP_HOSTS_HELP}</p>
          </ToolField>
          <ToolField id={field("secret")} label={NR_TOOLS.SECRET_LABEL}>
            <select id={field("secret")} className={SELECT_CLASS} value={secret} onChange={(e) => { setSecret(e.target.value); setSubmitted(false); }}>
              <option value="">{NR_TOOLS.CC_SECRET_NONE}</option>
              {names.map((n) => <option key={n} value={n}>{n}</option>)}
            </select>
            <NamesFailed failure={namesFailure} />
          </ToolField>
          <fieldset className="flex flex-col gap-2">
            <legend className="mb-2 text-sm font-medium">{NR_TOOLS.DELIVERY_LABEL}</legend>
            {deliveries.map((d) => (
              <label key={d.value} className={d.off ? "flex items-center gap-2 text-sm opacity-50" : "flex items-center gap-2 text-sm"}>
                <input type="radio" name={field("mode")} value={d.value} checked={mode === d.value} disabled={d.off} onChange={() => { setMode(d.value); setSubmitted(false); }} />
                {d.label}
              </label>
            ))}
            <p className="text-sm text-muted-foreground">{NR_TOOLS.MCP_DELIVERY_NOTE}</p>
            {!resident && residentOffReason && <p className="text-sm text-muted-foreground">{residentOffReason}</p>}
          </fieldset>
          {mode === "env" && (
            <ToolField id={field("var")} label={NR_TOOLS.SECRET_VARIABLE_LABEL} error={shown(deliveryError)}>
              <Input id={field("var")} value={variable} autoComplete="off" spellCheck={false} aria-invalid={shown(deliveryError) ? true : undefined} aria-describedby={described("var", deliveryError)} onChange={(e) => { setVariable(e.target.value); setSubmitted(false); }} />
            </ToolField>
          )}
          {mode === "file" && (
            <ToolField id={field("file")} label={NR_TOOLS.SECRET_FILE_LABEL} error={shown(deliveryError)}>
              <Input id={field("file")} value={file} autoComplete="off" spellCheck={false} aria-invalid={shown(deliveryError) ? true : undefined} aria-describedby={described("file", deliveryError)} onChange={(e) => { setFile(e.target.value); setSubmitted(false); }} />
              {file.trim() && <p className="font-mono text-sm text-muted-foreground">{NR_TOOLS.SECRET_FILE_PATH(file.trim())}</p>}
            </ToolField>
          )}
          {mode === "header" && (
            <ToolField id={field("header-host")} label={NR_TOOLS.SECRET_HOST_LABEL} error={shown(deliveryError)}>
              <Input id={field("header-host")} value={headerHost} autoComplete="off" spellCheck={false} aria-invalid={shown(deliveryError) ? true : undefined} aria-describedby={described("header-host", deliveryError)} onChange={(e) => { setHeaderHost(e.target.value); setSubmitted(false); }} />
            </ToolField>
          )}
          <Notice tone="info">{NR_TOOLS.MCP_HARNESS_RULES}</Notice>
        </>
      ) : (
        <>
          <ToolField id={field("name")} label={NR_TOOLS.MCP_NAME}>
            <Input id={field("name")} value={name} placeholder={NR_TOOLS.MCP_NAME_PLACEHOLDER} autoComplete="off" onChange={(e) => setName(e.target.value)} />
          </ToolField>
          <ToolField id={field("url")} label={NR_TOOLS.MCP_URL} error={error}>
            <Input
              id={field("url")}
              type="url"
              value={url}
              placeholder={NR_TOOLS.MCP_URL_PLACEHOLDER}
              autoComplete="off"
              spellCheck={false}
              aria-invalid={error ? true : undefined}
              aria-describedby={error ? `${field("url")}-err` : undefined}
              onChange={(e) => { setUrl(e.target.value); setSubmitted(false); }}
            />
          </ToolField>
          <ToolField id={field("token")} label={NR_TOOLS.MCP_TOKEN}>
            <select id={field("token")} className={SELECT_CLASS} value={token} onChange={(e) => setToken(e.target.value)}>
              <option value="">{NR_TOOLS.MCP_NO_TOKEN}</option>
              {names.map((n) => <option key={n} value={n}>{n}</option>)}
            </select>
            <NamesFailed failure={namesFailure} />
          </ToolField>
          {!localServers && <p className="text-sm text-muted-foreground">{NR_TOOLS.MCP_LOCAL_SOON}</p>}
        </>
      )}
    </ToolDialog>
  );
}
