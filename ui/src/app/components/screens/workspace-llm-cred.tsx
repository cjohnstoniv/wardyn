/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import * as React from "react";
import { Loader2 } from "lucide-react";
import { toast } from "sonner";
import { workspaces as api } from "../../lib/api/workspaces";
import { integrationsApi, type IntegrationRow } from "../../lib/api/integrations";
import { getErrorMessage } from "../../lib/format";
import type { Workspace, WorkspaceLLMCred } from "../../lib/types";
import { Button } from "../ui/button";
import { Label } from "../ui/label";
import { RadioGroup, RadioGroupItem } from "../ui/radio-group";
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "../ui/dialog";
import { OPERATOR_ONLY_REASON } from "../wardyn/copy";
import { useOperator } from "../wardyn/operator-context";

// Human label for the current model/harness binding (list/detail display).
// The binding names an Integration (category ai_provider) by id; absent/"" =>
// no binding, the run falls back to the global provider config. Pass the
// fetched rows when you have them to show the integration's display name;
// without them the id itself is the honest label (operator-readable by
// construction — it is what the Integrations screen shows and audit logs).
export function llmCredLabel(cred?: WorkspaceLLMCred, rows?: IntegrationRow[]): string {
  const ref = cred?.integration_ref;
  if (!ref) return "None";
  return rows?.find((r) => r.id === ref)?.name ?? ref;
}
// success = a named binding resolves model access proxy-side; neutral = no
// binding (global provider fallback).
export function llmCredTone(cred?: WorkspaceLLMCred): "neutral" | "success" {
  return cred?.integration_ref ? "success" : "neutral";
}

// The binding picker shared by the onboarding form (create) and
// WorkspaceLLMCredDialog (edit): "None" + every ai_provider Integration the
// server knows. Uncontrolled data lives in the caller — this renders `value`
// and reports edits via `onChange`. Rows are fetched once on mount; a fetch
// failure renders the None-only honest floor (the binding can still be
// cleared, never invented).
export function LLMCredFields({
  value,
  onChange,
}: {
  value: WorkspaceLLMCred;
  onChange: (next: WorkspaceLLMCred) => void;
}) {
  const [rows, setRows] = React.useState<IntegrationRow[] | null>(null);
  React.useEffect(() => {
    let live = true;
    integrationsApi
      .list()
      .then((d) => live && setRows(d.ai))
      .catch(() => live && setRows([]));
    return () => {
      live = false;
    };
  }, []);

  const ref = value.integration_ref ?? "";
  return (
    <div className="space-y-2.5 rounded-lg border border-border p-3">
      <Label>Model / harness for this environment</Label>
      <RadioGroup
        value={ref || "none"}
        onValueChange={(v) => onChange({ integration_ref: v === "none" ? "" : v })}
        className="flex flex-col gap-1.5"
      >
        <label className="flex items-center gap-1.5 text-xs">
          <RadioGroupItem value="none" id="cred-ref-none" />
          <Label htmlFor="cred-ref-none" className="cursor-pointer font-normal">
            None — use the server&apos;s global provider
          </Label>
        </label>
        {(rows ?? []).map((r) => (
          <label key={r.id} className="flex items-center gap-1.5 text-xs">
            <RadioGroupItem value={r.id} id={`cred-ref-${r.id}`} />
            <Label htmlFor={`cred-ref-${r.id}`} className="cursor-pointer font-normal">
              {r.name} <span className="font-mono text-muted-foreground">{r.typeLabel}</span>
            </Label>
          </label>
        ))}
        {/* A stored ref whose integration no longer lists: still selectable/clearable, named honestly. */}
        {ref && rows !== null && !rows.some((r) => r.id === ref) && (
          <label className="flex items-center gap-1.5 text-xs">
            <RadioGroupItem value={ref} id="cred-ref-current" />
            <Label htmlFor="cred-ref-current" className="cursor-pointer font-mono font-normal">
              {ref} (not in the Integrations list)
            </Label>
          </label>
        )}
      </RadioGroup>
      {rows === null && (
        <p className="text-[0.6875rem] leading-snug text-muted-foreground">Loading integrations…</p>
      )}
      <p className="text-[0.6875rem] leading-snug text-muted-foreground">
        A run that picks this workspace/container inherits this model access — injected proxy-side at
        launch, never resident.
      </p>
    </div>
  );
}

// Standalone editor for an EXISTING workspace's model/harness binding — the
// onboarding form's llm_cred is create-only (the server ignores it on a
// generic PUT /workspaces/{id}), so changing it post-create goes through
// api.setWorkspaceLLMCred instead. `workspace` null => closed. Exported for
// direct test coverage / reuse, the same reason other standalone dialogs in
// this codebase are (e.g. AddSecretDialog).
export function WorkspaceLLMCredDialog({
  workspace,
  onOpenChange,
  onSaved,
}: {
  workspace: Workspace | null;
  onOpenChange: (o: boolean) => void;
  onSaved: (w: Workspace) => void;
}) {
  // PUT /workspaces/{id}/llm-cred — operator-only (see http.go).
  const operator = useOperator();
  const [cred, setCred] = React.useState<WorkspaceLLMCred>({});
  const [saving, setSaving] = React.useState(false);

  React.useEffect(() => {
    if (workspace) setCred(workspace.llm_cred ?? {});
  }, [workspace]);

  const save = async () => {
    if (!workspace) return;
    setSaving(true);
    try {
      const updated = await api.setWorkspaceLLMCred(workspace.id, cred);
      onSaved(updated);
      toast.success(`Model access updated for "${workspace.name}"`);
    } catch (e) {
      toast.error("Failed to update model access", { description: getErrorMessage(e) });
    } finally {
      setSaving(false);
    }
  };

  return (
    <Dialog open={!!workspace} onOpenChange={onOpenChange}>
      <DialogContent className="sm:max-w-lg">
        <DialogHeader>
          <DialogTitle>Model access — {workspace?.name}</DialogTitle>
          <DialogDescription>
            A run that picks this workspace/container inherits this model access, injected proxy-side —
            never resident in the sandbox.
          </DialogDescription>
        </DialogHeader>
        <LLMCredFields value={cred} onChange={setCred} />
        {!operator && (
          <p id="llm-cred-operator-reason" className="text-xs font-medium text-warning">
            {OPERATOR_ONLY_REASON}
          </p>
        )}
        <DialogFooter>
          <Button variant="ghost" onClick={() => onOpenChange(false)}>
            Cancel
          </Button>
          <Button
            onClick={save}
            disabled={!operator || saving}
            aria-describedby={operator ? undefined : "llm-cred-operator-reason"}
          >
            {saving && <Loader2 className="size-4 animate-spin" />}
            Save
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}
