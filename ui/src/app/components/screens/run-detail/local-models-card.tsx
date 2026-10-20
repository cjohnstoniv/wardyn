/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// M9's "Local models" card on a run's Overview: the models the run may use, and no
// others, and the caps its proxy holds each request to. It shows only for a run on
// a local_model provider, and the allowlist is the provider's record read now.
// Only GET /model-providers carries that record, and it is admin-only, so a
// member's run page has no card until the run read carries it.
import * as React from "react";
import { Server } from "lucide-react";
import { modelProviders } from "../../../lib/api/model-providers";
import { modelProviderKindFromAudit } from "../../../lib/api/audit";
import { LOCAL_MODEL_CARD as T } from "../../../lib/local-model-copy";
import { LOCAL_MODEL_DEFAULTS, type ModelProvider } from "../../../lib/types/site";
import type { AuditEvent, RunDetail } from "../../../lib/types";
import { useOperator } from "../../wardyn/operator-context";
import { useShellSetupStatus } from "../../wardyn/model-access-context";
import { WidgetCard } from "../../wardyn/primitives";

export function LocalModelsCard({ run, audit }: { run: RunDetail; audit: AuditEvent[] }) {
  const operator = useOperator();
  const { status } = useShellSetupStatus();
  const id = run.model_provider_id;
  const local = operator && !!id && modelProviderKindFromAudit(audit) === "local_model";
  const [provider, setProvider] = React.useState<ModelProvider | null>(null);
  React.useEffect(() => {
    if (!local) return;
    let live = true;
    // Best-effort, like the page's other panes: a failed read leaves no card.
    modelProviders
      .getModelProviders()
      .then((l) => live && setProvider(l.providers.providers?.find((p) => p.id === id) ?? null))
      .catch(() => live && setProvider(null));
    return () => {
      live = false;
    };
  }, [local, id]);

  if (!local || provider?.kind !== "local_model" || !provider.local) return null;
  const { models, max_context, max_output, keep_alive_seconds } = provider.local;
  const asked = (m: string) =>
    (provider.harnesses ?? []).filter((h) => h.model === m).map((h) => status?.harnesses?.find((x) => x.id === h.harness)?.display || h.harness);
  let host = provider.base_url ?? "";
  try {
    host = new URL(host).host || host;
  } catch {
    // keep the stored text
  }

  return (
    <div className="shrink-0 px-4 pt-3">
      <WidgetCard
        title={T.TITLE}
        Icon={Server}
        right={<span className="text-xs text-muted-foreground">{provider.name || run.model_provider_name || provider.id}</span>}
        bodyClassName="scroll-thin max-h-72 space-y-3 overflow-y-auto px-3 py-2.5 text-body"
      >
        <p>{T.LEDE}</p>
        <ul className="space-y-1.5">
          {models.map((m) => {
            const who = asked(m);
            return (
              <li key={m} className="flex items-center gap-2.5">
                <span className="flex-1 font-mono text-xs">{m}</span>
                {who.length > 0 ? (
                  <span className="rounded-full border border-border-strong px-2 py-px text-meta text-muted-foreground">
                    {T.ASKED_FOR(who)}
                  </span>
                ) : (
                  <span className="text-xs text-muted-foreground">{T.NOT_ASKED_FOR}</span>
                )}
              </li>
            );
          })}
        </ul>
        <dl className="grid grid-cols-[auto_1fr] gap-x-6 gap-y-1.5">
          <dt className="text-muted-foreground">{T.SERVER}</dt>
          <dd>
            <span className="font-mono text-xs">{host}</span>
            <span className="block text-muted-foreground">{T.SERVER_NOTE}</span>
          </dd>
          <dt className="text-muted-foreground">{T.CONTEXT}</dt>
          <dd>{T.TOKENS(max_context || LOCAL_MODEL_DEFAULTS.max_context)}</dd>
          <dt className="text-muted-foreground">{T.REPLY}</dt>
          <dd>{T.TOKENS(max_output || LOCAL_MODEL_DEFAULTS.max_output)}</dd>
          <dt className="text-muted-foreground">{T.KEEP_ALIVE}</dt>
          <dd>{T.KEEP_FOR(keep_alive_seconds || LOCAL_MODEL_DEFAULTS.keep_alive_seconds)}</dd>
        </dl>
        <p className="text-muted-foreground">{T.NO_MANAGEMENT}</p>
      </WidgetCard>
    </div>
  );
}
