/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// "Who may use it" on an organization pool's details (mock M-POOLS v6 policySection): no policy means everyone
// who may launch runs on organization pools; a policy lists people, groups and roles; a policy whose people have
// all left reads "Nobody can use this pool." Both admin tiers may edit it. A save or a clear that a second person
// must approve answers 202 (PendingChangeError): nothing changed yet, so the section says it is held and leaves
// edit mode. Any other refusal is the server's sentence under the editor.
import * as React from "react";
import { toast } from "sonner";
import { PendingChangeError } from "../../../../lib/api/core";
import { runnerPools } from "../../../../lib/api/runner-pools";
import { getErrorMessage } from "../../../../lib/format";
import type { RunnerPoolSubject, RunnerPoolUsePolicy } from "../../../../lib/types/runner-pools";
import { POOL_DIALOG, POOL_DRAWER } from "../../../wardyn/copy/runner-pools";
import { Mono } from "../../../wardyn/code-block";
import { Notice } from "../../../wardyn/notice";
import { Chip } from "../../../wardyn/primitives";
import { Button } from "../../../ui/button";
import { Input } from "../../../ui/input";
import { Label } from "../../../ui/label";
import { ConfirmDialog } from "../confirm-dialog";
import { Sec } from "../resource-table";

type SubjectType = RunnerPoolSubject["subject_type"];
const TYPES: SubjectType[] = ["user", "group", "user_type"];
const SELECT =
  "h-9 w-full rounded-md border border-input bg-input-background px-2 text-sm outline-none focus-visible:border-ring focus-visible:ring-[3px] focus-visible:ring-ring dark:bg-input/30";

/**
 * A pool with no policy answers with no subjects and the zero time for updated_at (the server always
 * sends the field); one set, then emptied because its people left, has a real time and no subjects.
 */
const hasPolicy = (p: RunnerPoolUsePolicy | null) => !!p?.updated_at && !p.updated_at.startsWith("0001-01-01");

function SubjectList({ subjects, onRemove }: { subjects: RunnerPoolSubject[]; onRemove?: (i: number) => void }) {
  return (
    <ul className="mb-2 space-y-1">
      {subjects.map((s, i) => (
        <li key={`${s.subject_type}:${s.subject}`} className="flex flex-wrap items-center gap-2 py-1">
          <Chip>{POOL_DRAWER.SUBJECT[s.subject_type]}</Chip>
          {s.subject_type === "user" ? <span className="break-all">{s.subject}</span> : <Mono className="break-all">{s.subject}</Mono>}
          {onRemove && (
            <Button variant="ghost" size="sm" aria-label={POOL_DRAWER.REMOVE_SUBJECT(s.subject)} onClick={() => onRemove(i)}>
              {POOL_DRAWER.REMOVE}
            </Button>
          )}
        </li>
      ))}
    </ul>
  );
}

export function PoolPolicy({
  poolId,
  poolName,
  onChanged,
  onPolicy,
}: {
  poolId: string;
  poolName: string;
  /** The policy was saved or cleared: the details around it re-read. */
  onChanged: () => void;
  /** Whether a policy is set, once it is known (the delete dialog's refusal follows it). */
  onPolicy?: (set: boolean) => void;
}) {
  const [policy, setPolicy] = React.useState<RunnerPoolUsePolicy | null>(null);
  const [editing, setEditing] = React.useState(false);
  const [draft, setDraft] = React.useState<RunnerPoolSubject[]>([]);
  const [held, setHeld] = React.useState(false);
  const [error, setError] = React.useState("");
  const [busy, setBusy] = React.useState(false);
  const [type, setType] = React.useState<SubjectType>("user");
  const [name, setName] = React.useState("");
  const [clearing, setClearing] = React.useState(false);
  const heldByClear = React.useRef(false);

  const read = React.useCallback(() => {
    runnerPools
      .usePolicy(poolId)
      .then((p) => {
        setPolicy(p);
        onPolicy?.(hasPolicy(p));
      })
      .catch((e: unknown) => setError(getErrorMessage(e)));
  }, [poolId, onPolicy]);
  React.useEffect(() => {
    setPolicy(null);
    setEditing(false);
    setHeld(false);
    setError("");
    read();
  }, [read]);

  const startEdit = () => {
    setDraft(policy?.subjects ?? []);
    setHeld(false);
    setError("");
    setName("");
    setEditing(true);
  };

  const add = () => {
    const subject = name.trim();
    if (!subject) return setError(POOL_DRAWER.ERR_SUBJECT_EMPTY);
    if (draft.some((s) => s.subject_type === type && s.subject === subject)) {
      return setError(POOL_DRAWER.ERR_SUBJECT_TWICE(POOL_DRAWER.SUBJECT[type].toLowerCase(), subject));
    }
    setDraft([...draft, { subject_type: type, subject }]);
    setName("");
    setError("");
  };

  const save = async () => {
    if (!policy) return;
    setBusy(true);
    setError("");
    try {
      await runnerPools.setUsePolicy(poolId, policy.revision, draft);
      setEditing(false);
      toast.success(POOL_DRAWER.WHO_SAVED);
      read();
      onChanged();
    } catch (e) {
      if (e instanceof PendingChangeError) {
        setHeld(true);
        setEditing(false);
      } else setError(getErrorMessage(e));
    } finally {
      setBusy(false);
    }
  };

  // A held clear resolves without an error so the dialog closes; onDone then must not claim it happened.
  const clear = async () => {
    heldByClear.current = false;
    try {
      await runnerPools.clearUsePolicy(poolId);
    } catch (e) {
      if (!(e instanceof PendingChangeError)) throw e;
      heldByClear.current = true;
    }
  };
  const cleared = () => {
    setEditing(false);
    if (heldByClear.current) return setHeld(true);
    toast.success(POOL_DIALOG.CLEAR_DONE);
    read();
    onChanged();
  };

  const subjects = editing ? draft : (policy?.subjects ?? []);
  const nobody = hasPolicy(policy) && subjects.length === 0 && !editing;
  const set = hasPolicy(policy);

  return (
    <Sec label={POOL_DRAWER.WHO} meta={POOL_DRAWER.WHO_ROLE} data-who>
      {held && (
        <div className="mb-2">
          <Notice tone="success" live title={POOL_DRAWER.HELD}>
            {POOL_DRAWER.HELD_BODY}
          </Notice>
        </div>
      )}
      {policy && !set && <p className="mb-2">{POOL_DRAWER.WHO_EVERYONE}</p>}
      {nobody && (
        <div className="mb-2">
          <Notice tone="warning" live title={POOL_DRAWER.WHO_NOBODY}>
            {POOL_DRAWER.WHO_NOBODY_BODY}
          </Notice>
        </div>
      )}
      {subjects.length > 0 && <SubjectList subjects={subjects} onRemove={editing ? (i) => setDraft(draft.filter((_, j) => j !== i)) : undefined} />}
      {policy && !editing && (
        <div className="mt-1">
          <Button variant="outline" onClick={startEdit}>
            {POOL_DRAWER.EDIT_WHO}
          </Button>
        </div>
      )}
      {editing && (
        <>
          <div className="grid grid-cols-2 items-end gap-3 max-sm:grid-cols-1">
            <div className="space-y-1.5">
              <Label htmlFor="pol-type">{POOL_DRAWER.ADD}</Label>
              <select id="pol-type" className={SELECT} value={type} onChange={(e) => setType(e.target.value as SubjectType)}>
                {TYPES.map((t) => (
                  <option key={t} value={t}>
                    {POOL_DRAWER.SUBJECT[t]}
                  </option>
                ))}
              </select>
            </div>
            <div className="space-y-1.5">
              <Label htmlFor="pol-val">{POOL_DRAWER.NAME}</Label>
              <Input
                id="pol-val"
                value={name}
                placeholder={POOL_DRAWER.NAME_PLACEHOLDER}
                autoComplete="off"
                aria-invalid={error ? true : undefined}
                aria-describedby={error ? "pol-err" : undefined}
                onChange={(e) => setName(e.target.value)}
                onKeyDown={(e) => {
                  if (e.key === "Enter") {
                    e.preventDefault();
                    add();
                  }
                }}
              />
            </div>
          </div>
          {error && (
            <p id="pol-err" role="alert" className="mt-1.5 text-xs text-danger">
              {error}
            </p>
          )}
          <div className="mt-3 flex flex-wrap gap-2">
            <Button variant="outline" size="sm" onClick={add}>
              {POOL_DRAWER.ADD_TO_LIST}
            </Button>
            <Button variant="outline" disabled={busy} onClick={() => void save()}>
              {POOL_DRAWER.SAVE_WHO}
            </Button>
            <Button variant="outline" aria-disabled={!set} onClick={() => set && setClearing(true)}>
              {POOL_DRAWER.CLEAR_POLICY}
            </Button>
            <Button variant="ghost" onClick={() => setEditing(false)}>
              {POOL_DRAWER.CANCEL}
            </Button>
          </div>
        </>
      )}
      {!editing && error && (
        <p role="alert" className="mt-1.5 text-xs text-danger">
          {error}
        </p>
      )}
      <ConfirmDialog
        open={clearing}
        onOpenChange={setClearing}
        title={POOL_DIALOG.CLEAR_TITLE(poolName)}
        body={[POOL_DIALOG.CLEAR_BODY, POOL_DIALOG.CLEAR_HELD]}
        confirmLabel={POOL_DRAWER.CLEAR_POLICY}
        destructive={false}
        onConfirm={clear}
        onDone={cleared}
      />
    </Sec>
  );
}
