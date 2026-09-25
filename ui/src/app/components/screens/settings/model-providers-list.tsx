/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Settings → Model providers (#536, packet MP-A states A1–A9): the admin's
// configuration list. Each row states org facts only — what each person
// provides, which agents use it, the per-agent default (set on the Agents tab,
// shown here read-only) and how many people have connected, never who. The
// admin's own connection lives in their own account, like everyone else's.
import * as React from "react";
import { AlertTriangle } from "lucide-react";
import { modelProviders, type ModelProvider, type ModelProvidersList as Snapshot } from "../../../lib/api/model-providers";
import { agentProviders, type AgentProvider } from "../../../lib/api/agent-providers";
import type { SetupHarnessTool } from "../../../lib/types";
import { MODEL_LEDE, MODEL_PROVIDERS as M, providesLine } from "../../../lib/model-providers-copy";
import { ACCESS_STATE } from "../../../lib/people-access-copy";
import { Button } from "../../ui/button";
import { Chip } from "../../wardyn/primitives";
import { EmptyState, TableSkeleton } from "../../wardyn/states";
import { ModelProviderEditor } from "./model-provider-editor";
import { EDITOR_KINDS } from "./model-provider-draft";

// The GET snapshot is kept whole: the editor (#537) writes the whole document
// back with its ETag.
type Loaded = { list: Snapshot; providers: ModelProvider[]; connected: Record<string, number>; roster: AgentProvider[] };

export function ModelProvidersList({ harnesses }: { harnesses?: SetupHarnessTool[] }) {
  const [data, setData] = React.useState<Loaded | "loading" | "error">("loading");
  // "Add model provider" opens the editor at its kind step; a row opens it on that provider.
  const [editing, setEditing] = React.useState<ModelProvider | "new" | null>(null);

  const load = React.useCallback(() => {
    setData("loading");
    Promise.all([modelProviders.getModelProviders(), agentProviders.getAgentProviders()])
      .then(([mp, ap]) =>
        setData({
          list: mp,
          providers: mp.providers.providers ?? [],
          connected: mp.connected,
          roster: ap.providers.agents ?? [],
        }),
      )
      .catch(() => setData("error"));
  }, []);
  React.useEffect(load, [load]);

  const label = (id: string) => harnesses?.find((h) => h.id === id)?.display ?? id;

  return (
    <section className="rounded-xl border border-border bg-card p-4" data-testid="model-providers-list">
      <div className="flex items-start justify-between gap-4">
        <div>
          <h3 className="text-sm font-medium text-foreground">{M.TITLE}</h3>
          <p className="mt-0.5 text-body leading-snug text-muted-foreground">{MODEL_LEDE}</p>
        </div>
        <Button size="sm" disabled={typeof data === "string"} onClick={() => setEditing("new")}>
          {M.ADD_CTA}
        </Button>
      </div>
      <div className="mt-3">
        {data === "loading" ? (
          <TableSkeleton rows={3} cols={3} />
        ) : data === "error" ? (
          <EmptyState
            as="h4"
            icon={AlertTriangle}
            title={M.FETCH_FAILED_TITLE}
            description={M.FETCH_FAILED_BODY}
            action={
              <Button variant="outline" size="sm" onClick={load}>
                {ACCESS_STATE.FETCH_FAILED_RETRY}
              </Button>
            }
          />
        ) : (
          <Rows data={data} label={label} harnesses={harnesses} onEdit={setEditing} />
        )}
      </div>
      {editing && typeof data !== "string" && (
        <ModelProviderEditor
          list={data.list}
          editing={editing === "new" ? null : editing}
          harnesses={(harnesses ?? []).filter((h) => !h.no_managed_auth).map((h) => ({ id: h.id, display: h.display }))}
          defaultFor={
            editing === "new" ? [] : data.roster.filter((a) => a.default_provider === editing.id).map((a) => a.id)
          }
          onClose={() => setEditing(null)}
          onSaved={() => {
            setEditing(null);
            load();
          }}
        />
      )}
    </section>
  );
}

function Rows({
  data: { providers, connected, roster },
  label,
  harnesses,
  onEdit,
}: {
  data: Loaded;
  label: (id: string) => string;
  harnesses?: SetupHarnessTool[];
  onEdit: (p: ModelProvider) => void;
}) {
  if (providers.length === 0) {
    return (
      <div className="rounded-lg border border-dashed border-border px-4 py-6 text-center">
        <h4 className="text-body font-medium text-foreground">{M.EMPTY_TITLE}</h4>
        <p className="mt-1 text-body text-muted-foreground">{M.EMPTY_BODY}</p>
      </div>
    );
  }

  const served = (h: string) => providers.filter((p) => p.harnesses?.some((x) => x.harness === h)).length;
  // A9: an agent that is turned on and takes a model provider, with none
  // serving it. `enabled` absent is an older daemon: unknown, so no notice.
  const unserved = (harnesses ?? []).filter((h) => h.enabled === true && !h.no_managed_auth && served(h.id) === 0);
  const offDefaults: string[] = [];

  const rows = providers.map((p) => {
    const kind = M.KIND[p.kind] ?? p.kind;
    const name = p.name || kind;
    const defaults = roster.filter((a) => a.default_provider === p.id).map((a) => a.id);
    // With one provider for an agent there is nothing to choose between (A2).
    const chipDefaults = defaults.filter((h) => served(h) >= 2).map(label);
    const used = (p.harnesses ?? []).map((x) => label(x.harness));
    if (p.disabled && defaults.length > 0) offDefaults.push(M.OFF_STILL_DEFAULT(defaults.map(label).join(" and ")));

    let usage: React.ReactNode;
    if (p.disabled && defaults.length === 0) usage = M.OFF_LINE;
    else if (used.length === 0) usage = <span className="text-info">{M.UNUSED}</span>;
    else usage = M.USED_BY(used);

    const facts = (
      <>
        <div className="text-body font-medium text-foreground">{name}</div>
        {name !== kind && <div className="text-meta text-muted-foreground">{kind}</div>}
        <div className="text-meta text-muted-foreground">{providesLine(p.kind)}</div>
        <div className="text-meta text-muted-foreground">{usage}</div>
      </>
    );
    return (
      <li key={p.id} className="flex items-start justify-between gap-4 px-3 py-2.5" data-testid={`model-provider-${p.id}`}>
        {/* Only the kinds the editor draws open it; Bedrock and Claude
            subscription rows stay inert until #538 builds their editor. */}
        {(EDITOR_KINDS as readonly string[]).includes(p.kind) ? (
          <button type="button" className="min-w-0 text-left" onClick={() => onEdit(p)}>
            {facts}
          </button>
        ) : (
          <div className="min-w-0">{facts}</div>
        )}
        <div className="flex shrink-0 flex-wrap justify-end gap-1.5">
          {chipDefaults.length > 0 && <Chip>{M.CHIP_DEFAULT_FOR(chipDefaults)}</Chip>}
          <Chip>{p.disabled ? M.CHIP_OFF : M.CONNECTED(connected[p.id] ?? 0)}</Chip>
        </div>
      </li>
    );
  });

  return (
    <div className="space-y-2">
      {unserved.map((h) => (
        <p key={h.id} className="rounded-lg border border-info/25 bg-info-subtle px-3 py-2 text-meta text-info">
          {M.HARNESS_UNSERVED(h.display)}
        </p>
      ))}
      <ul className="divide-y divide-border rounded-lg border border-border">{rows}</ul>
      {offDefaults.map((line) => (
        <p key={line} className="rounded-lg border border-warning/30 bg-warning-subtle px-3 py-2 text-meta text-warning">
          {line}
        </p>
      ))}
    </div>
  );
}
