/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The provider editor's door on Settings until #536's Model providers list
// replaces it: "Add model provider", and one button per stored provider that
// opens it for editing. Admin-only (GET /model-providers is operatorOnly).
// ponytail: interim host; #536 mounts ModelProviderEditor from its own list and
// deletes this file.
import * as React from "react";
import { agentProviders } from "../../../lib/api/agent-providers";
import { modelProviders, type ModelProvidersList } from "../../../lib/api/model-providers";
import { MODEL_PROVIDERS } from "../../../lib/model-providers-copy";
import type { AgentProviders, ModelProvider } from "../../../lib/types/site";
import type { SetupHarnessTool } from "../../../lib/types";
import { Button } from "../../ui/button";
import { Chip } from "../../wardyn/primitives";
import { ModelProviderEditor } from "./model-provider-editor";

export function ModelProvidersEntry({ harnesses }: { harnesses?: SetupHarnessTool[] }) {
  const [list, setList] = React.useState<ModelProvidersList | null>(null);
  const [roster, setRoster] = React.useState<AgentProviders | null>(null);
  const [editing, setEditing] = React.useState<ModelProvider | "new" | null>(null);

  // No snapshot, no door: a PUT replaces the whole document, so the editor
  // never opens without the read (and ETag) it would write over.
  const load = React.useCallback(() => {
    modelProviders.getModelProviders().then(setList, () => setList(null));
    agentProviders.getAgentProviders().then((s) => setRoster(s.providers), () => setRoster(null));
  }, []);
  React.useEffect(load, [load]);

  if (!list) return null;
  const rows = (harnesses ?? []).filter((h) => !h.no_managed_auth).map((h) => ({ id: h.id, display: h.display }));
  const current = editing === "new" ? null : editing;

  return (
    <div className="flex flex-wrap items-center gap-2">
      <Button variant="outline" onClick={() => setEditing("new")}>
        {MODEL_PROVIDERS.ADD_CTA}
      </Button>
      {(list.providers.providers ?? []).map((p) => (
        <Button key={p.id} variant="ghost" onClick={() => setEditing(p)}>
          {p.name || MODEL_PROVIDERS.KIND[p.kind]}
          <Chip tone="neutral">{MODEL_PROVIDERS.KIND[p.kind]}</Chip>
        </Button>
      ))}
      {editing && (
        <ModelProviderEditor
          list={list}
          editing={current}
          harnesses={rows}
          defaultFor={(roster?.agents ?? []).filter((a) => current && a.default_provider === current.id).map((a) => a.id)}
          onClose={() => setEditing(null)}
          onSaved={() => {
            setEditing(null);
            load();
          }}
        />
      )}
    </div>
  );
}
