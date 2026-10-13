/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The pool details sheet (mock M-POOLS v6 drawer()): what the pool allows, its resources across its runners,
// its per-run limits, the organization default and who may use it (organization pools), its members, and the
// actions its caller may take. The sheet reads what the page already holds; only the use policy is read here.
// Writes go through the caller's own route: `mine` is the owner's, otherwise the super admin's organization
// route. The sheet's own close control is the Close (see Sheet): a second text button would be two.
import * as React from "react";
import { toast } from "sonner";
import { HttpError } from "../../../../lib/api/core";
import { runnerPools } from "../../../../lib/api/runner-pools";
import { absoluteTime, getErrorMessage, shortDate } from "../../../../lib/format";
import type { RunnerKind, RunnerPool, RunnerPoolMemberView } from "../../../../lib/types/runner-pools";
import { POOL_DIALOG, POOL_DRAWER, POOLS, RES, RUNNER_V6 } from "../../../wardyn/copy/runner-pools";
import { RUNNER } from "../../../wardyn/copy/runners";
import { Mono } from "../../../wardyn/code-block";
import { useOperator } from "../../../wardyn/operator-context";
import { Notice } from "../../../wardyn/notice";
import { Chip } from "../../../wardyn/primitives";
import { Button } from "../../../ui/button";
import { Sheet, SheetContent, SheetHeader, SheetTitle } from "../../../ui/sheet";
import { ConfirmDialog } from "../confirm-dialog";
import { Hint, KindChip, OnlineDot, ResourceTable, Sec } from "../resource-table";
import { amountsText, barriersText, cpus, gb, narrowText, poolRes, runTypesText } from "../resources";
import { DeletePoolDialog } from "./pool-dialogs";
import { PoolLimitsTable } from "./pool-limits-table";
import { PoolPolicy } from "./pool-policy";

/** The server's reason for a write that named a revision someone else has since changed. */
const STALE = "runner_pool_stale";

export const DEFAULT_CHIP = "border-info bg-info text-info-foreground";

function MachineLine({ machine }: { machine: string | undefined }) {
  if (!machine) return <>{POOL_DRAWER.MEMBER_MACHINE(RUNNER_V6.NOT_REPORTED)}</>;
  const s = POOL_DRAWER.MEMBER_MACHINE(machine);
  const i = s.indexOf(machine);
  return (
    <>
      {s.slice(0, i)}
      <Mono className="break-all">{machine}</Mono>
      {s.slice(i + machine.length)}
    </>
  );
}

function MemberCard({ m, onOpen }: { m: RunnerPoolMemberView; onOpen?: (kind: RunnerKind, id: string) => void }) {
  const l = m.limits;
  const meta = "mt-0.5 text-xs text-muted-foreground";
  return (
    <div data-member className="border-t border-border py-2.5 first:border-t-0">
      <div className="flex flex-wrap items-center gap-1.5 text-sm font-medium">
        <OnlineDot online={m.online} />
        <button type="button" className="text-info underline-offset-4 hover:underline" onClick={() => onOpen?.(m.kind, m.id)}>
          {m.name}
        </button>
        <KindChip kind={m.kind} />
        <Chip dot>{m.online ? RUNNER.ONLINE : RUNNER.OFFLINE}</Chip>
      </div>
      <div className={meta}>{m.kind === "self_hosted" ? <MachineLine machine={m.runner?.machine_name} /> : POOL_DRAWER.MEMBER_MACHINE_NA}</div>
      {l ? (
        <>
          <div className={meta}>{POOL_DRAWER.MEMBER_RUNS(l.runs_at_once, m.usage.in_use.runs)}</div>
          <div className={meta}>{POOL_DRAWER.MEMBER_PER_RUN(amountsText(l.per_run))}</div>
          <div className={meta}>
            {POOL_DRAWER.MEMBER_AVAILABLE(
              cpus(m.usage.available.cpu_millis),
              cpus(l.total.cpu_millis),
              gb(m.usage.available.memory_mib),
              gb(l.total.memory_mib),
              gb(m.usage.available.disk_mib),
              gb(l.total.disk_mib),
            )}
            {!m.online && ` ${POOL_DRAWER.MEMBER_OFFLINE}`}
          </div>
        </>
      ) : (
        <div className={meta}>{RES.LIMITS_NOT_SET}</div>
      )}
    </div>
  );
}

function Members({ pool, members, onOpen }: { pool: RunnerPool; members: RunnerPoolMemberView[]; onOpen?: (kind: RunnerKind, id: string) => void }) {
  const org = pool.scope === "organization";
  const owner = pool.owner ?? "";
  return (
    <Sec label={POOL_DRAWER.MEMBERS} meta={org ? POOL_DRAWER.MEMBERS_ORG : POOL_DRAWER.MEMBERS_USER} data-members>
      <p className="mb-1.5 text-xs text-muted-foreground">
        {org ? POOL_DRAWER.MEMBERS_ORG_BODY : POOL_DRAWER.MEMBERS_USER_BODY(owner)}
      </p>
      {org && <p className="mb-1 text-xs text-muted-foreground">{POOL_DRAWER.ORG_ONLY}</p>}
      {members.length ? members.map((m) => <MemberCard key={`${m.kind}:${m.id}`} m={m} onOpen={onOpen} />) : <p>{POOL_DRAWER.MEMBERS_NONE}</p>}
    </Sec>
  );
}

function DefaultSection({ pool, isDefault, canSet, onSet }: { pool: RunnerPool; isDefault: boolean; canSet: boolean; onSet?: (poolId: string | null) => Promise<void> }) {
  const [busy, setBusy] = React.useState(false);
  const [error, setError] = React.useState("");
  const set = async (id: string | null) => {
    if (!onSet) return;
    setBusy(true);
    setError("");
    try {
      await onSet(id);
    } catch (e) {
      setError(getErrorMessage(e));
    } finally {
      setBusy(false);
    }
  };
  const can = canSet && !!onSet;
  return (
    <Sec label={POOL_DRAWER.ORG_DEFAULT} meta={canSet ? undefined : POOL_DRAWER.SUPER_ADMIN_ONLY} data-default-section>
      {isDefault ? (
        <p className="mb-2">
          <strong className="font-semibold">{POOL_DRAWER.IS_DEFAULT}</strong> {POOL_DRAWER.IS_DEFAULT_BODY}
        </p>
      ) : pool.state !== "active" ? (
        <p>{POOL_DRAWER.OFF_NO_DEFAULT}</p>
      ) : (
        <p className="mb-2">{POOL_DRAWER.NOT_DEFAULT}</p>
      )}
      {can && isDefault && (
        <Button variant="outline" disabled={busy} onClick={() => void set(null)}>
          {POOL_DRAWER.CLEAR_DEFAULT}
        </Button>
      )}
      {can && !isDefault && pool.state === "active" && (
        <Button variant="outline" disabled={busy} onClick={() => void set(pool.id)}>
          {POOLS.MAKE_DEFAULT}
        </Button>
      )}
      {error && (
        <p role="alert" className="mt-1.5 text-xs text-danger">
          {error}
        </p>
      )}
    </Sec>
  );
}

export function PoolDrawer({
  pool,
  members,
  waiting,
  isDefault,
  mine,
  onClose,
  onChanged,
  onOpenRunner,
  onEdit,
  onSetDefault,
}: {
  pool: RunnerPool | null;
  members: RunnerPoolMemberView[];
  waiting?: number;
  isDefault: boolean;
  mine: boolean;
  onClose: () => void;
  onChanged: () => void;
  onOpenRunner?: (kind: RunnerKind, id: string) => void;
  onEdit: (pool: RunnerPool) => void;
  onSetDefault?: (poolId: string | null) => Promise<void>;
}) {
  const superAdmin = useOperator();
  const [switchingOff, setSwitchingOff] = React.useState(false);
  const [deleting, setDeleting] = React.useState(false);
  const [policySet, setPolicySet] = React.useState(false);
  const [error, setError] = React.useState("");
  const titleRef = React.useRef<HTMLSpanElement | null>(null);
  const poolId = pool?.id;
  React.useEffect(() => {
    setError("");
    setPolicySet(false);
  }, [poolId]);
  if (!pool) return null;

  const org = pool.scope === "organization";
  const active = pool.state === "active";
  const res = poolRes(pool, members);
  // A switch that lost to someone else's change is said in the sheet, in the server's words, and the pool
  // is read again so what the sheet shows is what is there now; nothing was written.
  const stale = (e: unknown) => e instanceof HttpError && e.status === 409 && e.reason === STALE;
  const switchOn = async () => {
    setError("");
    try {
      await runnerPools.update(pool.id, { revision: pool.revision, state: "active" });
      toast.success(POOL_DIALOG.ON_DONE(pool.name));
      onChanged();
    } catch (e) {
      setError(getErrorMessage(e));
      if (stale(e)) onChanged();
    }
  };
  const switchOff = () =>
    runnerPools.update(pool.id, { revision: pool.revision, state: "disabled" }).catch((e: unknown) => {
      setError(stale(e) ? getErrorMessage(e) : "");
      throw e;
    });

  const actions = mine ? (
    <div className="mt-3 flex flex-wrap gap-2">
      <Button variant="outline" onClick={() => onEdit(pool)}>
        {POOL_DRAWER.EDIT}
      </Button>
      <Button variant="outline" onClick={() => setDeleting(true)}>
        {POOL_DRAWER.DELETE}
      </Button>
    </div>
  ) : !org ? (
    <p className="mt-2 text-xs text-muted-foreground">{POOL_DRAWER.READ_ONLY(pool.owner ?? "")}</p>
  ) : superAdmin ? (
    <div className="mt-3 flex flex-wrap gap-2">
      <Button variant="outline" onClick={() => onEdit(pool)}>
        {POOL_DRAWER.EDIT}
      </Button>
      <Button variant="outline" onClick={() => (active ? setSwitchingOff(true) : void switchOn())}>
        {active ? POOL_DRAWER.SWITCH_OFF : POOL_DRAWER.SWITCH_ON}
      </Button>
      <Button variant="outline" onClick={() => setDeleting(true)}>
        {POOL_DRAWER.DELETE}
      </Button>
    </div>
  ) : (
    <p className="mt-2 text-xs text-muted-foreground">{POOL_DRAWER.SUPER_ADMIN_EDITS}</p>
  );

  return (
    <>
      <Sheet open onOpenChange={(o) => !o && onClose()}>
        <SheetContent
          className="scroll-thin w-full gap-0 overflow-y-auto sm:max-w-[640px]"
          onOpenAutoFocus={(e) => {
            e.preventDefault();
            titleRef.current?.focus();
          }}
          aria-describedby={undefined}
        >
          <SheetHeader className="pb-2">
            <div className="pr-8">
              <SheetTitle>
                <span ref={titleRef} tabIndex={-1} className="break-words outline-none">
                  {pool.name}
                </span>
              </SheetTitle>
            </div>
          </SheetHeader>
          <div className="px-4 pb-6">
            {pool.owner && (
              <p className="mb-2 text-body">
                <span className="text-muted-foreground">{POOL_DRAWER.OWNER}</span> {pool.owner}
              </p>
            )}
            <div className="mb-2 flex flex-wrap gap-2">
              <Chip>{org ? POOLS.ORG_POOL : POOLS.USER_POOL}</Chip>
              <Chip tone={active ? "success" : "neutral"} dot>
                {active ? POOLS.ACTIVE : POOLS.SWITCHED_OFF}
              </Chip>
              {isDefault && <Chip className={DEFAULT_CHIP}>{POOLS.DEFAULT_BADGE}</Chip>}
            </div>
            <Sec label={POOL_DRAWER.ALLOWS}>
              <dl className="grid grid-cols-[150px_minmax(0,1fr)] gap-x-3 gap-y-1.5 max-[480px]:grid-cols-1">
                <dt className="text-xs text-muted-foreground">{POOL_DRAWER.RUN_TYPES}</dt>
                <dd>{runTypesText(pool.limits)}</dd>
                <dt className="text-xs text-muted-foreground">{POOL_DRAWER.BARRIERS}</dt>
                <dd>{barriersText(pool.limits)}</dd>
              </dl>
            </Sec>
            <Sec label={POOL_DRAWER.RESOURCES} data-pool-res>
              {res.runners ? (
                <>
                  <ResourceTable view={res} pool />
                  {!active && <Hint data-off-note>{POOL_DRAWER.OFF_NOTE}</Hint>}
                  <Hint>{narrowText(pool, res)}</Hint>
                  {res.offline > 0 && (
                    <div className="mt-2">
                      <Notice tone="warning" live>
                        {POOL_DRAWER.OFFLINE_RUNNERS(res.offline, res.runners)}
                      </Notice>
                    </div>
                  )}
                </>
              ) : (
                <p>{POOL_DRAWER.NO_RUNNERS}</p>
              )}
            </Sec>
            <Sec label={POOL_DRAWER.PER_RUN_LIMITS}>
              <PoolLimitsTable pool={pool} members={members} />
              <Hint data-strictest>{POOL_DRAWER.STRICTEST}</Hint>
            </Sec>
            {org && <DefaultSection pool={pool} isDefault={isDefault} canSet={superAdmin} onSet={onSetDefault} />}
            {org ? (
              <PoolPolicy poolId={pool.id} poolName={pool.name} onChanged={onChanged} onPolicy={setPolicySet} />
            ) : (
              <Sec label={POOL_DRAWER.WHO}>{POOL_DRAWER.WHO_OWNER(pool.owner ?? "")}</Sec>
            )}
            <Members pool={pool} members={members} onOpen={onOpenRunner} />
            <Sec label={POOL_DRAWER.POOL}>
              <p className="text-xs text-muted-foreground" title={absoluteTime(pool.updated_at)}>
                {POOL_DRAWER.REVISION(pool.revision, shortDate(pool.updated_at))}
              </p>
              {actions}
              {error && (
                <p role="alert" className="mt-1.5 text-xs text-danger">
                  {error}
                </p>
              )}
            </Sec>
          </div>
        </SheetContent>
      </Sheet>
      <ConfirmDialog
        open={switchingOff}
        onOpenChange={setSwitchingOff}
        title={POOL_DIALOG.OFF_TITLE(pool.name)}
        body={[POOL_DIALOG.OFF_BODY]}
        confirmLabel={POOL_DRAWER.SWITCH_OFF}
        destructive={false}
        onConfirm={switchOff}
        onDone={() => {
          toast.success(POOL_DIALOG.OFF_DONE(pool.name));
          onChanged();
        }}
        onRaced={onChanged}
      />
      <DeletePoolDialog
        pool={deleting ? pool : null}
        mine={mine}
        isDefault={isDefault}
        hasPolicy={policySet}
        waiting={waiting}
        onOpenChange={setDeleting}
        onDeleted={() => {
          onChanged();
          onClose();
        }}
      />
    </>
  );
}
