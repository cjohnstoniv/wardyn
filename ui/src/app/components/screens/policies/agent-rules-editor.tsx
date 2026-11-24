/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Add and Edit for one agent's rules: the walls form the Your walls card uses (without the two forge capability lists,
// which a rule may not hold) and the agent's own sandbox and approvals. A refusal shows where it belongs: a 409 asks
// for a reload, a 422 sits under the section its wire path names, and a held write (202) shows the submitted note.
import * as React from "react";
import { agentRules, AGENT_RULES_REASON } from "../../../lib/api/agent-rules";
import { HttpError, PendingChangeError } from "../../../lib/api/core";
import { getErrorMessage } from "../../../lib/format";
import type { AgentRules } from "../../../lib/types/agent-rules";
import type { GovernanceChange } from "../../../lib/types";
import { sectionOfRefusal, type FormSection } from "../../../lib/wall-preset-words";
import { Button } from "../../ui/button";
import { Dialog, DialogContent, DialogDescription, DialogFooter, DialogTitle } from "../../ui/dialog";
import { RadioGroup, RadioGroupItem } from "../../ui/radio-group";
import { AGENT_RULES as A } from "../../wardyn/copy/agent-rules";
import { ADD_ACCESS } from "../../wardyn/copy/components";
import { WALL_PRESETS as W } from "../../wardyn/copy/wall-presets";
import { SubmittedNote } from "../governance/submitted-note";
import { formBlocked, LABEL, toForm, toSpec, WallsForm, type Form, type WallsFormVoice } from "../settings/walls-form";
import { APPROVALS_CHOICES, approvalsLabel, SANDBOX_CHOICES, sandboxLabel } from "./agent-rules-words";

const VOICE: WallsFormVoice = {
  sitesAny: A.SITES_ANY,
  sitesOnlyHint: A.SITES_ONLY_HINT,
  unlistedPolicy: A.UNLISTED_POLICY,
  toolsNone: A.TOOLS_NONE,
  hideCapabilities: true,
};

/** A harness with rules to edit, or one to add (no stored rules: version 0). */
export interface RulesEdit {
  harness: string;
  name: string;
  rules?: AgentRules;
}

const NONE = "none";

export function AgentRulesEditor({ edit, onClose, onSaved, onReload }: { edit: RulesEdit; onClose: () => void; onSaved: () => void; onReload: (harness: string) => void }) {
  const stored = edit.rules;
  const [form, setForm] = React.useState<Form>(() => toForm(stored?.spec ?? {}));
  const [sandbox, setSandbox] = React.useState(stored?.inner_walls?.sandbox_mode || NONE);
  const [approvals, setApprovals] = React.useState(stored?.inner_walls?.approvals || NONE);
  const [busy, setBusy] = React.useState(false);
  const [failure, setFailure] = React.useState("");
  const [sectionErrors, setSectionErrors] = React.useState<Partial<Record<FormSection, string>>>({});
  const [conflict, setConflict] = React.useState(false);
  const [held, setHeld] = React.useState<GovernanceChange | null>(null);

  const clear = () => {
    setFailure("");
    setSectionErrors({});
    setConflict(false);
  };
  const change = (patch: Partial<Form>) => {
    setForm((f) => ({ ...f, ...patch }));
    clear();
  };
  const refuse = (e: unknown) => {
    if (e instanceof PendingChangeError) return setHeld(e.change);
    if (e instanceof HttpError && e.reason === AGENT_RULES_REASON.VERSION_CONFLICT) {
      setConflict(true);
      return setFailure(A.RELOAD_BODY);
    }
    if (e instanceof HttpError && e.reason === AGENT_RULES_REASON.INVALID) {
      const { section, text } = sectionOfRefusal(e.message);
      if (section && section !== "name" && section !== "description") return setSectionErrors({ [section]: text });
      return setFailure(text);
    }
    setFailure(getErrorMessage(e));
  };
  const submit = async () => {
    setBusy(true);
    clear();
    const inner = { ...(sandbox !== NONE && { sandbox_mode: sandbox }), ...(approvals !== NONE && { approvals }) };
    try {
      await agentRules.put(edit.harness, { spec: toSpec(form), ...(Object.keys(inner).length > 0 && { inner_walls: inner }), version: stored?.version ?? 0 });
      onSaved();
    } catch (e) {
      refuse(e);
    }
    setBusy(false);
  };

  const blocked = formBlocked(form);
  return (
    // A save in flight keeps its dialog and its form: dismissed, its answer would close a dialog opened since.
    <Dialog open onOpenChange={(o) => !o && !busy && onClose()}>
      <DialogContent className="sm:max-w-xl">
        <form
          className="grid gap-4"
          aria-busy={busy}
          onSubmit={(e) => {
            e.preventDefault();
            if (!blocked && !busy && !held) void submit();
          }}
        >
          <DialogTitle>{A.EDIT_TITLE(edit.name)}</DialogTitle>
          <DialogDescription>{A.EDIT_LEDE(edit.name)}</DialogDescription>
          <div className="space-y-1">
            <span className={LABEL}>{A.AGENT}</span>
            <p className="text-sm">{edit.name}</p>
            {stored && <p className="text-xs text-muted-foreground">{A.AGENT_FIXED}</p>}
          </div>
          <hr className="border-border" />
          <fieldset disabled={busy || !!held} className="m-0 min-w-0 border-0 p-0">
            <div className="flex flex-col gap-[18px]">
              <WallsForm form={form} onEdit={change} errors={sectionErrors} voice={VOICE} />
              <div className="flex flex-col gap-2" role="group" aria-label={A.INNER_LABEL}>
                <span className={LABEL}>{A.INNER_LABEL}</span>
                <p className="text-xs text-muted-foreground">{A.INNER_HELP}</p>
                <Choice label={A.INNER_SANDBOX} value={sandbox} onChange={(v) => (setSandbox(v), clear())} options={SANDBOX_CHOICES.map((m) => [m, sandboxLabel(m)])} />
                <Choice label={A.INNER_APPROVALS} value={approvals} onChange={(v) => (setApprovals(v), clear())} options={APPROVALS_CHOICES.map((m) => [m, approvalsLabel(m)])} />
                <p className="text-xs text-muted-foreground">{A.INNER_NEVER}</p>
              </div>
            </div>
          </fieldset>
          <p className="text-xs text-muted-foreground">{A.APPLIES_NOTE}</p>
          {stored && <p className="text-xs text-muted-foreground">{W.EDITING_VERSION(stored.version)}</p>}
          {failure && (
            <p role="alert" className="flex flex-wrap items-center gap-2 text-sm text-danger">
              {failure}
              {conflict && (
                <Button type="button" size="sm" onClick={() => onReload(edit.harness)}>
                  {W.RELOAD}
                </Button>
              )}
            </p>
          )}
          {held && <SubmittedNote change={held} />}
          <DialogFooter>
            <Button type="button" variant="outline" disabled={busy} onClick={onClose}>
              {held ? ADD_ACCESS.CLOSE : W.CANCEL}
            </Button>
            <Button type="submit" variant={conflict ? "outline" : "default"} disabled={blocked || busy || !!held}>
              {A.SAVE}
            </Button>
          </DialogFooter>
        </form>
      </DialogContent>
    </Dialog>
  );
}

/** One radio group of the agent's own walls: "No requirement" first, then each value a rule may name. */
function Choice({ label, value, onChange, options }: { label: string; value: string; onChange: (v: string) => void; options: (readonly [string, string])[] }) {
  return (
    <div className="flex flex-col gap-1.5">
      <span className="text-sm">{label}</span>
      <RadioGroup className="gap-2" aria-label={label} value={value} onValueChange={onChange}>
        {[[NONE, A.INNER_NONE] as const, ...options].map(([v, text]) => (
          <label key={v} className="flex items-center gap-2 text-sm">
            <RadioGroupItem value={v} />
            {text}
          </label>
        ))}
      </RadioGroup>
    </div>
  );
}
