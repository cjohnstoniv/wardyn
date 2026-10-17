/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// My runner -> Claim (M-RN-2 MR5). The claim is the security step, so it takes the whole fingerprint from
// the person's own computer (the CLI first, or a paste) and never offers one from this page: the field
// starts empty, and the runner row shows only an abbreviated fingerprint. A mismatch marks the field and
// keeps the dialog open. An optional pool (one of the person's own, or a new name) takes the runner in the
// same step, and may become their default: a claimed runner in no pool runs nothing.
import * as React from "react";
import { Loader2 } from "lucide-react";
import { toast } from "sonner";
import { HttpError } from "../../../lib/api/core";
import { runnerPools } from "../../../lib/api/runner-pools";
import { runners as runnersApi } from "../../../lib/api/runners";
import { getErrorMessage } from "../../../lib/format";
import type { RunnerPoolList } from "../../../lib/types/runner-pools";
import type { RunnerView } from "../../../lib/types/runners";
import { MY_RUNNER, RUNNER } from "../../wardyn/copy/runners";
import { makeMono } from "../../wardyn/code-block";
import { CopyButton } from "../../wardyn/copy-button";
import { Field } from "../../wardyn/form-primitives";
import { Button } from "../../ui/button";
import { Checkbox } from "../../ui/checkbox";
import { Input } from "../../ui/input";
import { Dialog, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle } from "../../ui/dialog";

const MISMATCH = "runner_claim_mismatch";
const COMMAND = "wardyn runner claim";
const withMono = makeMono([COMMAND]);

export function ClaimRunnerDialog({
  runner,
  onOpenChange,
  onClaimed,
}: {
  /** The runner being claimed; null when the dialog is closed. */
  runner: RunnerView | null;
  onOpenChange: (open: boolean) => void;
  onClaimed: () => void;
}) {
  const [fingerprint, setFingerprint] = React.useState("");
  const [busy, setBusy] = React.useState(false);
  const [mismatch, setMismatch] = React.useState(false);
  const [error, setError] = React.useState("");
  const [pool, setPool] = React.useState("");
  const [makeDefault, setMakeDefault] = React.useState(false);
  // The person's own pools, offered as the field's suggestions; a name outside them makes a new pool.
  const [own, setOwn] = React.useState<{ id: string; name: string }[]>([]);
  const id = runner?.id;
  React.useEffect(() => {
    setFingerprint("");
    setMismatch(false);
    setError("");
    setPool("");
    setMakeDefault(false);
    if (!id) return;
    let live = true;
    runnerPools
      .listMine()
      .then((l) => live && setOwn(ownPools(l)))
      .catch(() => live && setOwn([]));
    return () => {
      live = false;
    };
  }, [id]);
  // The name stays while the dialog closes, so its words do not blank during the exit.
  const last = React.useRef(runner);
  if (runner) last.current = runner;
  const shown = runner ?? last.current;
  if (!shown) return null;

  const submit = async (e: React.FormEvent) => {
    e.preventDefault();
    setBusy(true);
    setMismatch(false);
    setError("");
    try {
      await runnersApi.claim(shown.id, fingerprint.trim());
    } catch (err) {
      if (err instanceof HttpError && err.reason === MISMATCH) setMismatch(true);
      else setError(getErrorMessage(err));
      setBusy(false);
      return;
    }
    toast.success(MY_RUNNER.CLAIMED_TOAST(shown.name));
    // The claim stands from here on: a pool step that fails closes the dialog and says where to finish it.
    let joined = false;
    try {
      if (pool.trim()) await putInPool(shown.id, pool.trim(), makeDefault, () => { joined = true; });
    } catch (err) {
      // Once the runner is in the pool, a failure is the default step's: it must not say the runner is not in the pool.
      toast.error(joined ? getErrorMessage(err) : MY_RUNNER.CLAIM_POOL_FAILED(shown.name, pool.trim(), getErrorMessage(err)));
    } finally {
      onOpenChange(false);
      onClaimed();
      setBusy(false);
    }
  };

  const errId = "claim-runner-mismatch";
  const listId = "claim-runner-pools";
  return (
    <Dialog open={runner !== null} onOpenChange={(o) => !busy && onOpenChange(o)}>
      <DialogContent className="sm:max-w-lg">
        <form onSubmit={submit} className="space-y-3">
          <DialogHeader>
            <DialogTitle>{MY_RUNNER.CLAIM_TITLE(shown.name)}</DialogTitle>
            <DialogDescription>{withMono(MY_RUNNER.CLAIM_LEAD(shown.name))}</DialogDescription>
          </DialogHeader>
          <div className="flex items-center justify-between gap-2 rounded-lg border border-border bg-surface-2/60 p-3">
            <code className="font-mono text-xs">{COMMAND}</code>
            <CopyButton
              text={COMMAND}
              label={RUNNER.COPY_COMMAND}
              className="gap-1.5 rounded-md border border-border bg-card px-2.5 py-1 text-xs text-foreground hover:bg-surface-2"
            >
              {RUNNER.COPY_COMMAND}
            </CopyButton>
          </div>
          <Field label={MY_RUNNER.FIELD_FINGERPRINT} htmlFor="claim-runner-fingerprint" hint={MY_RUNNER.FINGERPRINT_HINT(shown.name)} required>
            <Input
              id="claim-runner-fingerprint"
              autoFocus
              autoComplete="off"
              spellCheck={false}
              className="font-mono"
              placeholder="SHA256:"
              value={fingerprint}
              aria-invalid={mismatch || undefined}
              aria-describedby={mismatch ? errId : undefined}
              onChange={(e) => setFingerprint(e.target.value)}
            />
          </Field>
          <Field label={MY_RUNNER.FIELD_POOL} htmlFor="claim-runner-pool" hint={MY_RUNNER.POOL_HINT}>
            <Input
              id="claim-runner-pool"
              autoComplete="off"
              list={listId}
              placeholder={MY_RUNNER.POOL_PLACEHOLDER}
              value={pool}
              onChange={(e) => setPool(e.target.value)}
            />
          </Field>
          <datalist id={listId}>
            {own.map((p) => (
              <option key={p.id} value={p.name} />
            ))}
          </datalist>
          <label className="flex items-center gap-2 text-body text-muted-foreground">
            <Checkbox checked={makeDefault && pool.trim() !== ""} disabled={pool.trim() === ""} onCheckedChange={(v) => setMakeDefault(v === true)} />
            {MY_RUNNER.POOL_DEFAULT}
          </label>
          {mismatch && (
            <p id={errId} className="text-body text-danger">
              {MY_RUNNER.CLAIM_MISMATCH(shown.name)}
            </p>
          )}
          {error && (
            <p role="alert" className="text-body text-danger">
              {error}
            </p>
          )}
          <DialogFooter>
            <Button type="button" variant="ghost" disabled={busy} onClick={() => onOpenChange(false)}>
              {RUNNER.CANCEL}
            </Button>
            <Button type="submit" disabled={busy || fingerprint.trim() === ""}>
              {busy && <Loader2 className="size-3.5 animate-spin" />}
              {MY_RUNNER.CLAIM_SUBMIT}
            </Button>
          </DialogFooter>
        </form>
      </DialogContent>
    </Dialog>
  );
}

/** The person's own user pools: in the list of pools they may use, a user-scope row is theirs. */
const ownPools = (list: RunnerPoolList) => list.pools.filter((p) => p.scope === "user");

/** Puts a runner just claimed into the person's own pool of that name, made first when they have none by it.
 * The pools are read again here, not taken from the suggestions: those may still be loading or have failed to
 * load, and a pool must not be made over one that exists. */
async function putInPool(runnerId: string, name: string, makeDefault: boolean, onJoined: () => void) {
  const poolId = ownPools(await runnerPools.listMine()).find((p) => p.name.toLowerCase() === name.toLowerCase())?.id ?? (await runnerPools.createMine({ name })).id;
  await runnerPools.addMyRunner(poolId, runnerId);
  onJoined();
  if (makeDefault) await runnerPools.setMyDefaults({ ...(await runnerPools.myDefaults()), pool_id: poolId });
}
