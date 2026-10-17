/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Add an MCP server by URL: a remote streamable-HTTP server, with its token (one
// of the person's saved secrets) added by the proxy to each request. Servers
// that run inside the sandbox are Coming soon.

import * as React from "react";
import type { ComponentRef } from "../../../../../lib/types/components";
import { Input } from "../../../../ui/input";
import { NR_TOOLS } from "../../../../wardyn/copy/new-run/tools";
import { httpsParts, mcpRef } from "./catalogue-edits";
import { NamesFailed, SELECT_CLASS, ToolDialog, ToolField, type NamesFailure } from "./tool-dialog";

export function AddMcpDialog({ names, namesFailure, onAdd, onClose }: { names: readonly string[]; namesFailure?: NamesFailure; onAdd: (ref: ComponentRef) => void; onClose: () => void }) {
  const id = React.useId();
  const [name, setName] = React.useState("");
  const [url, setUrl] = React.useState("");
  const [token, setToken] = React.useState("");
  const [submitted, setSubmitted] = React.useState(false);
  const host = httpsParts(url)?.hostname;
  const error = submitted && !host ? NR_TOOLS.MCP_ERR_URL : "";

  const confirm = () => {
    setSubmitted(true);
    const ref = mcpRef(name.trim() || host || "", url, token || undefined);
    if (ref) onAdd(ref);
  };

  return (
    <ToolDialog title={NR_TOOLS.MCP_CARD_TITLE} intro={NR_TOOLS.MCP_INTRO} firstField={`${id}-name`} confirm={NR_TOOLS.ADD_TO_RUN} onConfirm={confirm} onClose={onClose}>
      <ToolField id={`${id}-name`} label={NR_TOOLS.MCP_NAME}>
        <Input id={`${id}-name`} value={name} placeholder={NR_TOOLS.MCP_NAME_PLACEHOLDER} autoComplete="off" onChange={(e) => setName(e.target.value)} />
      </ToolField>
      <ToolField id={`${id}-url`} label={NR_TOOLS.MCP_URL} error={error}>
        <Input
          id={`${id}-url`}
          type="url"
          value={url}
          placeholder={NR_TOOLS.MCP_URL_PLACEHOLDER}
          autoComplete="off"
          spellCheck={false}
          aria-invalid={error ? true : undefined}
          aria-describedby={error ? `${id}-url-err` : undefined}
          onChange={(e) => { setUrl(e.target.value); setSubmitted(false); }}
        />
      </ToolField>
      <ToolField id={`${id}-token`} label={NR_TOOLS.MCP_TOKEN}>
        <select id={`${id}-token`} className={SELECT_CLASS} value={token} onChange={(e) => setToken(e.target.value)}>
          <option value="">{NR_TOOLS.MCP_NO_TOKEN}</option>
          {names.map((n) => <option key={n} value={n}>{n}</option>)}
        </select>
        <NamesFailed failure={namesFailure} />
      </ToolField>
      <p className="text-sm text-muted-foreground">{NR_TOOLS.MCP_LOCAL_SOON}</p>
    </ToolDialog>
  );
}
