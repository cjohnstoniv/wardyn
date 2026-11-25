/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// A person's own list of the providers marked for the assistant: connected, not connected (with Connect,
// which opens the provider's own connect flow for that row), or denied by their unit. The credential is the
// person's, added by Wardyn's proxy to the assistant's run; nothing here implies an organisation credential.
import { ADO_PAT } from "../../lib/ado-pat-copy";
import { Button } from "../ui/button";
import { ASSISTANT as T } from "./copy/assistant";
import { DENY_LISTS } from "./copy/deny-lists";
import { RAIL_MODEL_ACCESS } from "./model-access-copy";
import { useModelAccessDoor } from "./model-access-context";
import { Chip } from "./primitives";

export type AssistantConnectionState = "connected" | "not_connected" | "denied";

export interface AssistantConnectionRow {
  providerId: string;
  /** What people see: the provider's name, with the model after a middle dot when one is named. */
  label: string;
  state: AssistantConnectionState;
}

export function AssistantConnectList({ rows, onConnect }: { rows: readonly AssistantConnectionRow[]; onConnect?: (providerId: string) => void }) {
  const door = useModelAccessDoor();
  const connect = (id: string) => (onConnect ? onConnect(id) : door.openDoor({ for: { provider: id } }));
  return (
    <section data-testid="assistant-connect-list" aria-labelledby="assistant-connect-list-title">
      <h4 id="assistant-connect-list-title" className="text-body font-medium text-foreground">
        {DENY_LISTS.STRIP_PROVIDERS}
      </h4>
      <ul className="mt-2 divide-y divide-border">
        {rows.map((row) => (
          <li key={row.providerId} className="flex flex-wrap items-center justify-between gap-2 py-2">
            <span className="min-w-0 text-sm text-foreground">{row.label}</span>
            <span className="flex flex-wrap items-center gap-2">
              <Chip>{T.MARKED}</Chip>
              {row.state === "connected" && <Chip tone="success">{ADO_PAT.CHIP_CONNECTED}</Chip>}
              {row.state === "denied" && <Chip tone="warning">{T.DENIED_BY_UNIT}</Chip>}
              {row.state === "not_connected" && (
                <>
                  <Chip tone="warning">{ADO_PAT.CHIP_NOT_CONNECTED}</Chip>
                  <Button size="sm" variant="outline" aria-label={`${RAIL_MODEL_ACCESS.NO_PROVIDER_CTA} ${row.label}`} onClick={() => connect(row.providerId)}>
                    {RAIL_MODEL_ACCESS.NO_PROVIDER_CTA}
                  </Button>
                </>
              )}
            </span>
          </li>
        ))}
      </ul>
      <p className="mt-2 text-meta text-muted-foreground">{T.CONNECT_HINT}</p>
    </section>
  );
}
