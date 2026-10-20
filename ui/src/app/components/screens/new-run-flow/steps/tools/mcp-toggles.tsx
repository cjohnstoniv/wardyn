/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// MCP servers for one harness: a toggle per server on the run, from the
// server's own facts. A harness Wardyn cannot configure MCP for shows each
// toggle off and disabled, with the reason as text.

import * as React from "react";
import { CircleAlert } from "lucide-react";
import type { AccessRow, ToolPolicyFact } from "../../../../../lib/types/new-run-access";
import { Checkbox } from "../../../../ui/checkbox";
import { NR_TOOLS } from "../../../../wardyn/copy/new-run/tools";
import { Chip } from "../../../../wardyn/primitives";
import { isLocalMcp } from "./local-mcp";
import { reasonText } from "./tool-facts";

export interface McpTogglesProps {
  fact: Pick<ToolPolicyFact, "harness" | "lane">;
  harnessName: string;
  /** The run's MCP server rows. */
  servers: readonly AccessRow[];
  /** Whether a server is on for this harness: the person's choice if made, else the server's. */
  isOn: (server: AccessRow) => boolean;
  /** `on` is the new state of the server for this harness. */
  onToggle: (server: AccessRow, on: boolean) => void;
  /** Whether this harness cannot apply the rules a local server is on under: the run would be refused. */
  refused?: (server: AccessRow) => boolean;
}

export function McpToggles({ fact, harnessName, servers, isOn, onToggle, refused }: McpTogglesProps) {
  const headingId = React.useId();
  const whyId = `${headingId}-why`;
  const unsupported = fact.lane === "unsupported";
  const why = unsupported ? reasonText("mcp_harness_unsupported", { harness: harnessName }) : undefined;
  return (
    <div role="group" aria-labelledby={headingId} className="flex flex-col gap-2">
      <div>
        <h4 id={headingId} className="text-sm font-medium">{NR_TOOLS.MCP_FOR_HARNESS}</h4>
        <p className="text-sm text-muted-foreground">{NR_TOOLS.MCP_FOLLOWS}</p>
      </div>
      {servers.length === 0 ? (
        <p className="text-sm text-muted-foreground">{NR_TOOLS.MCP_NONE}</p>
      ) : (
        <ul className="flex flex-col gap-2">
          {servers.map((s) => {
            const id = `${headingId}-${s.id}`;
            const local = isLocalMcp(s);
            const on = !unsupported && isOn(s);
            return (
              <li key={s.id} className={local ? "flex flex-col gap-1 text-sm" : "flex items-center gap-2 text-sm"}>
                <div className={local ? "flex flex-wrap items-center gap-2" : "contents"}>
                  <Checkbox
                    id={id}
                    checked={on}
                    disabled={unsupported}
                    aria-describedby={why ? whyId : undefined}
                    onCheckedChange={(v) => onToggle(s, v === true)}
                  />
                  <label htmlFor={id}>{s.name}</label>
                  {local && <Chip>{NR_TOOLS.MCP_RUNS_HERE}</Chip>}
                </div>
                {local && (on && refused?.(s) ? (
                  <p role="alert" className="flex items-start gap-1.5 pl-6 text-sm text-danger">
                    <CircleAlert aria-hidden className="mt-0.5 size-4 shrink-0" />
                    <span>{NR_TOOLS.MCP_REFUSED(harnessName, s.name)}</span>
                  </p>
                ) : (
                  <p className="pl-6 text-sm text-muted-foreground">{NR_TOOLS.MCP_HARNESS_RULES}</p>
                ))}
              </li>
            );
          })}
        </ul>
      )}
      {why && <p id={whyId} className="text-sm text-muted-foreground">{why}</p>}
    </div>
  );
}
