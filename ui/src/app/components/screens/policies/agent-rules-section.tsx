/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Policies: the Agent rules section (an organisation's narrow-only walls for one agent harness, applied to every run
// that includes it). One rule set per harness: a list with Edit and Delete, an empty state that carries the Add action,
// and the Add picker for an agent that has none. Shown to the security tier only; absent where the store keeps none.
import * as React from "react";
import { Plus, ShieldCheck } from "lucide-react";
import { agentRules, agentRulesAbsent } from "../../../lib/api/agent-rules";
import { relativeTime } from "../../../lib/format";
import type { AgentRules } from "../../../lib/types/agent-rules";
import { Button } from "../../ui/button";
import { AGENT_RULES as A } from "../../wardyn/copy/agent-rules";
import { WALL_PRESETS as W } from "../../wardyn/copy/wall-presets";
import { useShellSetupStatus } from "../../wardyn/model-access-context";
import { AgentBadge, Chip } from "../../wardyn/primitives";
import { EmptyState, ErrorState, TableSkeleton, failureSentence } from "../../wardyn/states";
import { AgentRulesEditor, type RulesEdit } from "./agent-rules-editor";
import { AgentPicker, DeleteRulesDialog } from "./agent-rules-dialogs";
import { ruleChips } from "./agent-rules-words";

type State = "loading" | "absent" | "error" | "ready";
type Dialog = { kind: "pick" } | { kind: "edit"; edit: RulesEdit; nonce: number } | { kind: "delete"; rules: AgentRules; nonce: number };

export function AgentRulesSection() {
  const [state, setState] = React.useState<State>("loading");
  const [rules, setRules] = React.useState<AgentRules[]>([]);
  const [message, setMessage] = React.useState<string | undefined>();
  const [dialog, setDialog] = React.useState<Dialog | null>(null);
  const { status } = useShellSetupStatus();
  // "Your own tools" brings no agent, and the server refuses rules for it.
  const catalogue = (status?.harnesses ?? []).filter((h) => h.id !== "none");
  const nameOf = (id: string) => catalogue.find((h) => h.id === id)?.display ?? id;

  const read = React.useCallback(async (): Promise<AgentRules[] | undefined> => {
    try {
      const list = await agentRules.list();
      setRules(list);
      setState("ready");
      return list;
    } catch (e) {
      setMessage(failureSentence(e));
      setState(agentRulesAbsent(e) ? "absent" : "error");
    }
  }, []);
  React.useEffect(() => {
    void read();
  }, [read]);

  if (state === "absent") return null;
  // After a refused save or delete, the rules as stored now, reopened where the person was.
  const reload = async (harness: string, kind: "edit" | "delete") => {
    const fresh = (await read())?.find((r) => r.harness === harness);
    setDialog(fresh ? (kind === "edit" ? { kind, edit: { harness, name: nameOf(harness), rules: fresh }, nonce: Date.now() } : { kind, rules: fresh, nonce: Date.now() }) : null);
  };
  const add = (
    <Button size="sm" variant="outline" onClick={() => setDialog({ kind: "pick" })}>
      <Plus className="size-4" aria-hidden="true" /> {A.ADD}
    </Button>
  );

  return (
    <section aria-labelledby="agent-rules-title" data-testid="agent-rules-section" className="mt-6 flex flex-col gap-3">
      <div className="flex flex-wrap items-start justify-between gap-x-4 gap-y-2">
        <div className="min-w-0 flex-1 basis-80">
          <h2 id="agent-rules-title" className="text-base font-semibold">
            {A.TITLE}
          </h2>
          <p className="text-sm text-muted-foreground">{A.LEDE}</p>
        </div>
        {state === "ready" && rules.length > 0 && add}
      </div>
      <div className="overflow-hidden rounded-xl border border-border bg-card">
        {state === "loading" && <TableSkeleton rows={3} cols={4} />}
        {state === "error" && <ErrorState size="compact" title={A.LOAD_FAILED_TITLE} message={message} onRetry={() => void read()} />}
        {state === "ready" && rules.length === 0 && <EmptyState size="compact" icon={ShieldCheck} title={A.EMPTY_TITLE} description={A.EMPTY_BODY} action={add} />}
        {state === "ready" && rules.length > 0 && (
          <ul className="divide-y divide-border">
            {rules.map((r) => (
              <li key={r.harness} data-testid={`agent-rules-${r.harness}`} className="flex flex-wrap items-start justify-between gap-x-4 gap-y-2 p-3">
                <div className="flex min-w-0 flex-1 basis-60 flex-col gap-1.5">
                  <div className="flex flex-wrap items-center gap-2">
                    <AgentBadge agent={r.harness} withLabel={false} />
                    <span className="font-medium">{nameOf(r.harness)}</span>
                    <span className="font-mono text-xs text-muted-foreground">{r.harness}</span>
                  </div>
                  <div className="flex flex-wrap gap-1.5">
                    {ruleChips(r).map((c) => (
                      <Chip key={c}>{c}</Chip>
                    ))}
                  </div>
                  <p className="text-xs text-muted-foreground">{r.updated_by ? W.META_SHARED(r.version, r.updated_by, relativeTime(r.updated_at)) : W.META_PLAIN(r.version)}</p>
                </div>
                <div className="flex flex-wrap items-center gap-1">
                  <Button size="sm" variant="ghost" onClick={() => setDialog({ kind: "edit", edit: { harness: r.harness, name: nameOf(r.harness), rules: r }, nonce: 0 })}>
                    {W.EDIT}
                  </Button>
                  <Button size="sm" variant="ghost" className="text-danger" onClick={() => setDialog({ kind: "delete", rules: r, nonce: 0 })}>
                    {W.DELETE}
                  </Button>
                </div>
              </li>
            ))}
          </ul>
        )}
      </div>
      {dialog?.kind === "pick" && (
        <AgentPicker
          harnesses={catalogue}
          ruled={new Set(rules.map((r) => r.harness))}
          onCancel={() => setDialog(null)}
          onContinue={(id) => setDialog({ kind: "edit", edit: { harness: id, name: nameOf(id) }, nonce: 0 })}
        />
      )}
      {dialog?.kind === "edit" && (
        <AgentRulesEditor
          key={`${dialog.edit.harness}:${dialog.nonce}`}
          edit={dialog.edit}
          onClose={() => setDialog(null)}
          onSaved={() => (setDialog(null), void read())}
          onReload={(h) => void reload(h, "edit")}
        />
      )}
      {dialog?.kind === "delete" && (
        <DeleteRulesDialog
          key={`${dialog.rules.harness}:${dialog.nonce}`}
          rules={dialog.rules}
          name={nameOf(dialog.rules.harness)}
          onClose={() => setDialog(null)}
          onDeleted={() => (setDialog(null), void read())}
          onReload={(h) => void reload(h, "delete")}
        />
      )}
    </section>
  );
}
