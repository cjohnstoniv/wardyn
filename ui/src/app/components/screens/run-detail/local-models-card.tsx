/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// M9's "Local models" card on a run's Overview: the models the run launched with (run.model_allowlists,
// #3374, so a member sees them too), and for an operator the server and the caps its proxy holds each
// request to. Those come from GET /model-providers, which is admin-only and reads the provider as it is now.
import * as React from "react";
import { Server } from "lucide-react";
import { modelProviders } from "../../../lib/api/model-providers";
import { LOCAL_MODEL_CARD as T } from "../../../lib/local-model-copy";
import { LOCAL_MODEL_DEFAULTS, type ModelProvider } from "../../../lib/types/site";
import type { RunDetail } from "../../../lib/types";
import { useOperator, useOperatorResolved } from "../../wardyn/operator-context";
import { useShellSetupStatus } from "../../wardyn/model-access-context";
import { WidgetCard } from "../../wardyn/primitives";

export function LocalModelsCard({ run }: { run: RunDetail }) {
  // Resolved too: useOperator() reads true until /me answers (and when it fails), and the read below is
  // operatorOnly, so a member would be refused (and audited) for it. The card fails closed.
  const isOperator = useOperator();
  const resolved = useOperatorResolved();
  const operator = isOperator && resolved;
  const { status } = useShellSetupStatus();
  const id = run.model_provider_id;
  const models = (id && run.model_allowlists?.[id]) || [];
  const enrich = operator && models.length > 0;
  const [provider, setProvider] = React.useState<ModelProvider | null>(null);
  React.useEffect(() => {
    if (!enrich) return;
    let live = true;
    // Best-effort, like the page's other panes: a failed read leaves no card.
    modelProviders
      .getModelProviders()
      .then((l) => live && setProvider(l.providers.providers?.find((p) => p.id === id) ?? null))
      .catch(() => live && setProvider(null));
    return () => {
      live = false;
    };
  }, [enrich, id]);

  if (models.length === 0) return null;
  const detail = provider?.kind === "local_model" && provider.local ? { ...provider, local: provider.local } : null;
  const asked = (m: string) =>
    (detail?.harnesses ?? []).filter((h) => h.model === m).map((h) => status?.harnesses?.find((x) => x.id === h.harness)?.display || h.harness);
  let host = detail?.base_url ?? "";
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
        right={<span className="text-xs text-muted-foreground">{detail?.name || run.model_provider_name || id}</span>}
        bodyClassName="scroll-thin max-h-72 space-y-3 overflow-y-auto px-3 py-2.5 text-body"
      >
        <ul className="space-y-1.5">
          {models.map((m) => {
            const who = asked(m);
            return (
              <li key={m} className="flex items-center gap-2.5">
                <span className="flex-1 font-mono text-xs">{m}</span>
                {!detail ? null : who.length > 0 ? (
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
        {detail && (
          <dl className="grid grid-cols-[auto_1fr] gap-x-6 gap-y-1.5">
            <dt className="text-muted-foreground">{T.SERVER}</dt>
            <dd>
              <span className="font-mono text-xs">{host}</span>
              <span className="block text-muted-foreground">{T.SERVER_NOTE}</span>
            </dd>
            <dt className="text-muted-foreground">{T.CONTEXT}</dt>
            <dd>{T.TOKENS(detail.local.max_context || LOCAL_MODEL_DEFAULTS.max_context)}</dd>
            <dt className="text-muted-foreground">{T.REPLY}</dt>
            <dd>{T.TOKENS(detail.local.max_output || LOCAL_MODEL_DEFAULTS.max_output)}</dd>
            <dt className="text-muted-foreground">{T.KEEP_ALIVE}</dt>
            <dd>{T.KEEP_FOR(detail.local.keep_alive_seconds || LOCAL_MODEL_DEFAULTS.keep_alive_seconds)}</dd>
          </dl>
        )}
        <p className="text-muted-foreground">{T.NO_MANAGEMENT}</p>
      </WidgetCard>
    </div>
  );
}
