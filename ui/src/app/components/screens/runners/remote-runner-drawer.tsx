/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Admin -> Runners drawer for a remote-hosted runner (mock M-POOLS v6 RN2, RN3): where it is set up, its
// resources and limits (a super admin's to set), whether people may add it to their own user pools, and
// the pools it is in. It is not someone's computer, so it has no Machine, posture, fingerprint or Revoke:
// it leaves when the server's configuration drops it. Making it available to people is a covered write,
// so where a second person must approve, the sheet says the change is held and changes nothing.
import * as React from "react";
import { toast } from "sonner";
import { PendingChangeError } from "../../../lib/api/core";
import { remoteRunners } from "../../../lib/api/runner-pools";
import { getErrorMessage } from "../../../lib/format";
import type { RemoteRunnerView } from "../../../lib/types/runner-pools";
import type { RunnerLimits } from "../../../lib/types/runners";
import { POOL_DRAWER, RES, RUNNER_KIND, RUNNER_V6 } from "../../wardyn/copy/runner-pools";
import { RUNNER } from "../../wardyn/copy/runners";
import { Notice } from "../../wardyn/notice";
import { useOperator } from "../../wardyn/operator-context";
import { Chip } from "../../wardyn/primitives";
import { Button } from "../../ui/button";
import { Checkbox } from "../../ui/checkbox";
import { Sheet, SheetContent, SheetDescription, SheetHeader, SheetTitle } from "../../ui/sheet";
import { LimitsDialog } from "./limits-dialog";
import { Hint, KindChip, OfflineNote, ResourceTable, RunsLine, Sec } from "./resource-table";
import { runnerRes } from "./resources";
import { PoolLinks } from "./runner-card";

export function RemoteRunnerDrawer({
  runner,
  onClose,
  onChanged,
  onOpenPool,
  onSaveLimits,
}: {
  runner: RemoteRunnerView | null;
  onClose: () => void;
  onChanged: () => void;
  onOpenPool: (id: string) => void;
  onSaveLimits: (runner: RemoteRunnerView, limits: RunnerLimits) => Promise<unknown>;
}) {
  const superAdmin = useOperator();
  const titleRef = React.useRef<HTMLSpanElement | null>(null);
  // The runner as the limits dialog opened on it: the save names that revision, not the one a later re-read
  // brought, so a change another admin made meanwhile is refused as stale instead of overwritten.
  const [editing, setEditing] = React.useState<RemoteRunnerView | null>(null);
  const [busy, setBusy] = React.useState(false);
  const [held, setHeld] = React.useState(false);
  const [error, setError] = React.useState("");
  const id = runner?.id;
  React.useEffect(() => {
    setHeld(false);
    setError("");
  }, [id]);
  if (!runner) return null;
  const res = runnerRes(runner.limits, runner.usage);
  const available = runner.people_pools !== "off";
  const listed = runner.people_pools === "listed";

  const setAvailable = async (on: boolean) => {
    setBusy(true);
    setError("");
    setHeld(false);
    try {
      if (on) await remoteRunners.setPeoplePolicy(runner.id, { revision: runner.revision, people_pools: "everyone" });
      else await remoteRunners.clearPeoplePolicy(runner.id);
      toast.success(on ? RUNNER_V6.AVAILABLE_ON(runner.name) : RUNNER_V6.AVAILABLE_OFF(runner.name));
      onChanged();
    } catch (e) {
      if (e instanceof PendingChangeError) setHeld(true);
      else setError(getErrorMessage(e));
    } finally {
      setBusy(false);
    }
  };

  return (
    <>
      <Sheet open onOpenChange={(o) => !o && onClose()}>
        <SheetContent
          className="scroll-thin w-full gap-0 overflow-y-auto sm:max-w-[640px]"
          onOpenAutoFocus={(e) => {
            e.preventDefault();
            titleRef.current?.focus();
          }}
        >
          <SheetHeader className="border-b border-border pb-4">
            <SheetTitle className="pr-8">
              <span ref={titleRef} tabIndex={-1} className="outline-none">
                {runner.name}
              </span>
            </SheetTitle>
            <SheetDescription className="sr-only">{RUNNER_KIND.REMOTE}</SheetDescription>
            <div className="flex flex-wrap items-center gap-2">
              <KindChip kind="remote_hosted" />
              <Chip dot>{runner.online ? RUNNER.ONLINE : RUNNER.OFFLINE}</Chip>
              {!runner.configured && <Chip tone="warning">{RUNNER_KIND.NOT_CONFIGURED}</Chip>}
            </div>
          </SheetHeader>
          <div className="px-4">
            <Sec label={RUNNER_V6.SET_UP_IN}>
              <p>{RUNNER_V6.SET_UP_IN_BODY}</p>
            </Sec>
            <Sec label={RES.RESOURCES} meta={RUNNER_V6.LIMITS_BY_ADMIN} data-resources>
              {res && runner.limits ? (
                <>
                  <RunsLine max={runner.limits.runs_at_once} active={runner.usage.in_use.runs} />
                  <ResourceTable view={res} />
                </>
              ) : (
                <p>{RES.LIMITS_NOT_SET}</p>
              )}
              {!runner.online && <OfflineNote />}
              {superAdmin ? (
                <div className="mt-3">
                  <Button variant="outline" size="sm" onClick={() => setEditing(runner)}>
                    {RUNNER_V6.EDIT_LIMITS}
                  </Button>
                </div>
              ) : (
                <Hint>{RUNNER_V6.SUPER_ADMIN_ONLY}</Hint>
              )}
            </Sec>
            <Sec label={RUNNER_V6.AVAILABLE_TO_PEOPLE} meta={RUNNER_V6.SUPER_ADMIN} data-available>
              {held && (
                <div className="mb-2">
                  <Notice tone="success" title={POOL_DRAWER.HELD} live>
                    {POOL_DRAWER.HELD_BODY}
                  </Notice>
                </div>
              )}
              <label className="inline-flex items-center gap-2 text-body font-normal">
                {/* A listed runner's people are kept on the command line: unchecking here would discard the list,
                    so the box only shows that it is available, and the line below says where the list changes. */}
                <Checkbox checked={available} disabled={!superAdmin || busy || listed} onCheckedChange={(v) => void setAvailable(v === true)} />
                {RUNNER_V6.AVAILABLE_CHECK}
              </label>
              <Hint>{RUNNER_V6.AVAILABLE_HINT}</Hint>
              {listed && <Hint>{RUNNER_V6.AVAILABLE_LISTED}</Hint>}
              {error && (
                <p role="alert" className="mt-1.5 text-body text-danger">
                  {error}
                </p>
              )}
            </Sec>
            <Sec label={RUNNER_V6.POOLS} data-pools>
              <PoolLinks pools={runner.pools} onOpenPool={onOpenPool} />
              <Hint>{RUNNER_V6.POOLS_HINT}</Hint>
            </Sec>
          </div>
        </SheetContent>
      </Sheet>
      <LimitsDialog
        target={editing ? { name: editing.name, limits: editing.limits, running: runner.usage.in_use.runs, self: false } : null}
        onOpenChange={(o) => !o && setEditing(null)}
        onSave={(limits) => onSaveLimits(editing ?? runner, limits)}
        onSaved={onChanged}
        onReload={async () => {
          const fresh = (await remoteRunners.list()).find((r) => r.id === runner.id);
          if (!fresh) return null;
          setEditing(fresh);
          onChanged();
          return { name: fresh.name, limits: fresh.limits, running: fresh.usage.in_use.runs, self: false };
        }}
      />
    </>
  );
}
