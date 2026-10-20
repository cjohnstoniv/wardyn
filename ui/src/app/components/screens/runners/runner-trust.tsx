/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// A runner's trust class on Admin -> Runners (0.9.3, owner mock item 3, Q5). A security admin changes a
// person's runner between Personal and Managed device, with the reason the audit keeps; an organization's
// runner is always Organization cluster and is read-only, as is the whole section for anyone who is not a
// security admin. Where a second person must approve, the change is held and the class stays as it was. The
// server refuses every other combination, so the control offers only the two classes it will take.
import * as React from "react";
import { toast } from "sonner";
import { PendingChangeError } from "../../../lib/api/core";
import { governance } from "../../../lib/api/governance";
import { runners as runnersApi } from "../../../lib/api/runners";
import { absoluteTime, getErrorMessage, relativeTime } from "../../../lib/format";
import type { GovernanceChange } from "../../../lib/types";
import type { RunnerTrustClass, RunnerView } from "../../../lib/types/runners";
import { POOL_DRAWER, RUNNER_TRUST } from "../../wardyn/copy/runner-pools";
import { Field } from "../../wardyn/form-primitives";
import { Notice } from "../../wardyn/notice";
import { Button } from "../../ui/button";
import { Input } from "../../ui/input";
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "../../ui/select";
import { Hint } from "./resource-table";

export const TRUST_LABEL: Record<RunnerTrustClass, string> = {
  personal: RUNNER_TRUST.PERSONAL,
  managed_device: RUNNER_TRUST.MANAGED,
  org_cluster: RUNNER_TRUST.ORG,
};
const TRUST_DESC: Record<RunnerTrustClass, string> = {
  personal: RUNNER_TRUST.PERSONAL_DESC,
  managed_device: RUNNER_TRUST.MANAGED_DESC,
  org_cluster: RUNNER_TRUST.ORG_DESC,
};
// How often a held proposal is checked for still waiting; a rejected or withdrawn one frees Save.
const HELD_RECHECK_MS = 15_000;
const PERSON_CLASSES: RunnerTrustClass[] = ["personal", "managed_device"];

function SetBy({ runner }: { runner: RunnerView }) {
  if (!runner.trust_set_by) return null;
  const when = runner.trust_set_at ? relativeTime(runner.trust_set_at) : "";
  return (
    <Hint>
      <span title={runner.trust_set_at ? absoluteTime(runner.trust_set_at) : undefined}>{RUNNER_TRUST.SET_BY(runner.trust_set_by, when).trim()}</span>
    </Hint>
  );
}

function TrustEditor({ runner, onChanged }: { runner: RunnerView; onChanged: () => void }) {
  const [draft, setDraft] = React.useState<RunnerTrustClass>(runner.trust_class);
  const [reason, setReason] = React.useState("");
  const [busy, setBusy] = React.useState(false);
  const [heldChange, setHeldChange] = React.useState<GovernanceChange | null>(null);
  const held = heldChange !== null;
  const [error, setError] = React.useState("");
  // A held proposal blocks another for the same runner (409), so Save waits. One that is no longer pending is dropped by
  // the recheck below, which also reloads the runner: an approval changed its class and remounts this editor.
  const heldId = heldChange?.id;
  const changedRef = React.useRef(onChanged);
  changedRef.current = onChanged;
  React.useEffect(() => {
    if (!heldId) return;
    let live = true;
    const t = setInterval(() => {
      governance
        .getChange(heldId)
        .then((change) => {
          if (!live || change.state === "pending") return;
          setHeldChange(null);
          changedRef.current();
        })
        .catch(() => {});
    }, HELD_RECHECK_MS);
    return () => {
      live = false;
      clearInterval(t);
    };
  }, [heldId]);
  const id = `rn-trust-reason-${runner.id}`;
  const save = async () => {
    setBusy(true);
    setError("");
    setHeldChange(null);
    try {
      await runnersApi.setTrust(runner.id, draft, reason.trim());
      toast.success(RUNNER_TRUST.DONE(runner.name, TRUST_LABEL[draft]));
      onChanged();
    } catch (e) {
      if (e instanceof PendingChangeError) setHeldChange(e.change);
      else setError(getErrorMessage(e));
    } finally {
      setBusy(false);
    }
  };
  return (
    <>
      {held && (
        <div className="mb-2">
          <Notice tone="success" title={POOL_DRAWER.HELD} live>
            {POOL_DRAWER.HELD_BODY}
          </Notice>
        </div>
      )}
      <Select value={draft} onValueChange={(v) => setDraft(v as RunnerTrustClass)} disabled={busy}>
        <SelectTrigger aria-label={RUNNER_TRUST.TITLE}>
          <SelectValue />
        </SelectTrigger>
        <SelectContent>
          {PERSON_CLASSES.map((c) => (
            <SelectItem key={c} value={c}>
              {TRUST_LABEL[c]}
            </SelectItem>
          ))}
        </SelectContent>
      </Select>
      <Hint>{TRUST_DESC[draft]}</Hint>
      <Field label={RUNNER_TRUST.REASON_LABEL} htmlFor={id} hint={RUNNER_TRUST.REASON_HINT} required className="mt-3">
        <Input id={id} required value={reason} maxLength={512} autoComplete="off" disabled={busy} onChange={(e) => setReason(e.target.value)} />
      </Field>
      {error && (
        <p role="alert" className="mt-1.5 text-body text-danger">
          {error}
        </p>
      )}
      <div className="mt-3">
        <Button variant="outline" size="sm" disabled={busy || held || draft === runner.trust_class || reason.trim() === ""} onClick={() => void save()}>
          {RUNNER_TRUST.SAVE}
        </Button>
      </div>
      <Hint>{RUNNER_TRUST.HINT}</Hint>
    </>
  );
}

/** The body of the drawer's Trust class section. `canEdit` is the viewer being a security admin. */
export function RunnerTrust({ runner, canEdit, onChanged }: { runner: RunnerView; canEdit: boolean; onChanged: () => void }) {
  if (runner.state === "unclaimed") return <p className="text-muted-foreground">{RUNNER_TRUST.UNCLAIMED}</p>;
  const editable = canEdit && runner.owner_kind === "person" && runner.state === "claimed";
  return (
    <>
      {editable ? (
        // Remounted when the class changes, so a saved change clears the reason.
        <TrustEditor key={`${runner.id}:${runner.trust_class}`} runner={runner} onChanged={onChanged} />
      ) : (
        <>
          <div className="font-medium">{TRUST_LABEL[runner.trust_class]}</div>
          <Hint>{TRUST_DESC[runner.trust_class]}</Hint>
        </>
      )}
      <SetBy runner={runner} />
    </>
  );
}
