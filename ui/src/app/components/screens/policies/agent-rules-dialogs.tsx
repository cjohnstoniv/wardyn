/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The two small dialogs of the Agent rules section: choosing the agent to add rules for, and confirming a delete.
import * as React from "react";
import { agentRules, AGENT_RULES_REASON } from "../../../lib/api/agent-rules";
import { HttpError, PendingChangeError } from "../../../lib/api/core";
import { getErrorMessage } from "../../../lib/format";
import type { GovernanceChange } from "../../../lib/types";
import type { AgentRules } from "../../../lib/types/agent-rules";
import { Button } from "../../ui/button";
import { Dialog, DialogContent, DialogDescription, DialogFooter, DialogTitle } from "../../ui/dialog";
import { RadioGroup, RadioGroupItem } from "../../ui/radio-group";
import { AGENT_RULES as A } from "../../wardyn/copy/agent-rules";
import { ADD_ACCESS } from "../../wardyn/copy/components";
import { NR_SHARED } from "../../wardyn/copy/new-run/shared";
import { WALL_PRESETS as W } from "../../wardyn/copy/wall-presets";
import { SubmittedNote } from "../governance/submitted-note";
import { LABEL } from "../settings/walls-form";

/** Choose the agent to add rules for. An agent that already has rules is listed, off, with the reason. */
export function AgentPicker({ harnesses, ruled, onCancel, onContinue }: { harnesses: { id: string; display: string }[]; ruled: ReadonlySet<string>; onCancel: () => void; onContinue: (id: string) => void }) {
  const [picked, setPicked] = React.useState(() => harnesses.find((h) => !ruled.has(h.id))?.id ?? "");
  return (
    <Dialog open onOpenChange={(o) => !o && onCancel()}>
      <DialogContent className="sm:max-w-md">
        <DialogTitle>{A.ADD}</DialogTitle>
        <DialogDescription>{A.ADD_LEDE}</DialogDescription>
        <div className="flex flex-col gap-2">
          <span className={LABEL}>{A.AGENT}</span>
          <RadioGroup aria-label={A.AGENT} className="gap-2" value={picked} onValueChange={setPicked}>
            {harnesses.map((h) => (
              <label key={h.id} className={ruled.has(h.id) ? "flex flex-col gap-0.5 text-sm text-muted-foreground" : "flex flex-col gap-0.5 text-sm"}>
                <span className="flex items-center gap-2">
                  <RadioGroupItem value={h.id} disabled={ruled.has(h.id)} />
                  {h.display}
                </span>
                {ruled.has(h.id) && <span className="pl-6 text-xs">{A.ADD_REASON}</span>}
              </label>
            ))}
          </RadioGroup>
        </div>
        <DialogFooter>
          <Button type="button" variant="outline" onClick={onCancel}>
            {W.CANCEL}
          </Button>
          <Button type="button" disabled={!picked || ruled.has(picked)} onClick={() => onContinue(picked)}>
            {NR_SHARED.CONTINUE}
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}

/** Delete at the version read. A 409 asks for a reload, and a held delete (202) shows the submitted note beside Close. */
export function DeleteRulesDialog({ rules, name, onClose, onDeleted, onReload }: { rules: AgentRules; name: string; onClose: () => void; onDeleted: () => void; onReload: (harness: string) => void }) {
  const [busy, setBusy] = React.useState(false);
  const [failure, setFailure] = React.useState("");
  const [conflict, setConflict] = React.useState(false);
  const [held, setHeld] = React.useState<GovernanceChange | null>(null);
  const remove = async () => {
    setBusy(true);
    setFailure("");
    try {
      await agentRules.remove(rules.harness, rules.version);
      onDeleted();
      return;
    } catch (e) {
      if (e instanceof PendingChangeError) setHeld(e.change);
      else if (e instanceof HttpError && e.reason === AGENT_RULES_REASON.VERSION_CONFLICT) (setConflict(true), setFailure(A.DELETE_RELOAD_BODY));
      else setFailure(getErrorMessage(e));
    }
    setBusy(false);
  };
  return (
    <Dialog open onOpenChange={(o) => !o && !busy && onClose()}>
      <DialogContent className="sm:max-w-md">
        <DialogTitle>{A.DELETE_TITLE(name)}</DialogTitle>
        <DialogDescription>{A.DELETE_BODY}</DialogDescription>
        {failure && (
          <p role="alert" className="flex flex-wrap items-center gap-2 text-sm text-danger">
            {failure}
            {conflict && (
              <Button type="button" size="sm" onClick={() => onReload(rules.harness)}>
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
          {!held && (
            <Button type="button" variant="destructive" disabled={busy || conflict} onClick={() => void remove()}>
              {A.DELETE_CONFIRM}
            </Button>
          )}
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}
