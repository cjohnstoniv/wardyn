/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Your account -> My runner: the pools of one runner, and My pools (mock M-POOLS v6 B1 to B16). A user
// pool is a person's own: only they see it here, change it, choose its runners (their own computers and
// the remote-hosted runners an admin made available) or make it their default. A runner runs nothing
// until it is in a pool, so each runner says which of the person's pools it is in and offers the rest.
import * as React from "react";
import { useNavigate } from "react-router-dom";
import { toast } from "sonner";
import { runnerPools } from "../../../lib/api/runner-pools";
import { getErrorMessage } from "../../../lib/format";
import type { RemoteRunnerView, RunnerPoolMemberView } from "../../../lib/types/runner-pools";
import type { RunnerView } from "../../../lib/types/runners";
import { MY_POOLS, POOL_DIALOG, POOLS } from "../../wardyn/copy/runner-pools";
import { Notice } from "../../wardyn/notice";
import { Chip } from "../../wardyn/primitives";
import { STATES } from "../../wardyn/states";
import { Button } from "../../ui/button";
import { ConfirmDialog } from "../runners/confirm-dialog";
import { PoolDrawer } from "../runners/pools/pool-drawer";
import { DeletePoolDialog } from "../runners/pools/pool-dialogs";
import { Hint, KindChip, OnlineDot, Sec } from "../runners/resource-table";
import { barriersText, narrowShort, poolRes, runTypesText } from "../runners/resources";
import { AddRunnerDialog } from "./add-runner-dialog";
import type { MyPool, MyPoolsData } from "./use-my-pools";

const LINK = "rounded-sm text-info underline underline-offset-2 hover:no-underline";

type Pools = MyPoolsData & { reload: () => void };
interface Removal {
  pool: MyPool;
  member: Pick<RunnerPoolMemberView, "kind" | "id" | "name">;
}

const removeMember = (r: Removal) =>
  r.member.kind === "self_hosted" ? runnerPools.removeMyRunner(r.pool.pool.id, r.member.id) : runnerPools.removeMyRemoteRunner(r.pool.pool.id, r.member.id);

function RemoveDialog({ removal, onOpenChange, onDone }: { removal: Removal | null; onOpenChange: (open: boolean) => void; onDone: () => void }) {
  const last = React.useRef(removal);
  if (removal) last.current = removal;
  const r = removal ?? last.current;
  if (!r) return null;
  return (
    <ConfirmDialog
      open={removal !== null}
      onOpenChange={onOpenChange}
      title={POOL_DIALOG.REMOVE_TITLE(r.member.name, r.pool.pool.name)}
      body={[POOL_DIALOG.REMOVE_BODY]}
      confirmLabel={POOL_DIALOG.REMOVE_CONFIRM}
      destructive={false}
      onConfirm={() => removeMember(r)}
      onDone={() => {
        toast.success(POOL_DIALOG.REMOVED(r.member.name, r.pool.pool.name));
        onDone();
      }}
    />
  );
}

const inPool = (p: MyPool, runnerId: string) => p.members.some((m) => m.kind === "self_hosted" && m.id === runnerId);

/** One runner's own "Pools" section: which of the person's pools it is in, and Add or Remove for each. */
export function RunnerPoolsSection({ runner, pools }: { runner: RunnerView; pools: Pools }) {
  const [refused, setRefused] = React.useState<{ pool: string; text: string } | null>(null);
  const [removal, setRemoval] = React.useState<Removal | null>(null);
  const [busy, setBusy] = React.useState("");
  if (runner.state === "claimed" && pools.status !== "ready") return null;
  const own = pools.pools;
  const within = own.filter((p) => inPool(p, runner.id));
  const add = async (p: MyPool) => {
    setBusy(p.pool.id);
    setRefused(null);
    try {
      await runnerPools.addMyRunner(p.pool.id, runner.id);
      toast.success(MY_POOLS.ADDED(runner.name, p.pool.name));
      pools.reload();
    } catch (e) {
      setRefused({ pool: p.pool.id, text: getErrorMessage(e) });
    } finally {
      setBusy("");
    }
  };
  let inner: React.ReactNode;
  if (runner.state !== "claimed") inner = <p>{MY_POOLS.CLAIM_FIRST}</p>;
  else if (own.length === 0) inner = <p>{MY_POOLS.NO_POOL_YET}</p>;
  else {
    inner = (
      <>
        <p className="mb-1.5">
          {MY_POOLS.IN}: <strong className="font-semibold">{within.length ? within.map((p) => p.pool.name).join(", ") : MY_POOLS.NONE}</strong>
        </p>
        {within.length === 0 && (
          <div className="mb-1.5">
            <Notice tone="warning" live>
              {MY_POOLS.IN_NO_POOL}
            </Notice>
          </div>
        )}
        <p className="mb-1 text-muted-foreground">{MY_POOLS.PUT_IN(runner.name)}</p>
        {!runner.online && <p className="mb-1 text-muted-foreground">{MY_POOLS.OFFLINE_ADD}</p>}
        <ul>
          {own.map((p) => {
            const added = inPool(p, runner.id);
            return (
              <li key={p.pool.id} className="grid grid-cols-[minmax(0,1fr)_auto] items-start gap-x-3 gap-y-1.5 border-t border-border py-2.5 first:border-t-0">
                <div className="min-w-0">
                  <div className="flex flex-wrap items-center gap-1.5 text-sm font-medium">
                    {p.pool.name}
                    {added && (
                      <Chip tone="success" dot>
                        {MY_POOLS.ADDED_CHIP}
                      </Chip>
                    )}
                  </div>
                  <div className="mt-0.5 text-xs text-muted-foreground">
                    {runTypesText(p.pool.limits)} · {barriersText(p.pool.limits)}
                  </div>
                </div>
                {added ? (
                  <Button variant="outline" size="sm" aria-label={MY_POOLS.REMOVE_FROM(runner.name, p.pool.name)} onClick={() => setRemoval({ pool: p, member: { kind: "self_hosted", id: runner.id, name: runner.name } })}>
                    {MY_POOLS.REMOVE}
                  </Button>
                ) : (
                  <Button variant="outline" size="sm" disabled={busy === p.pool.id} aria-label={MY_POOLS.ADD_TO(runner.name, p.pool.name)} onClick={() => void add(p)}>
                    {MY_POOLS.ADD}
                  </Button>
                )}
                {refused?.pool === p.pool.id && (
                  <p role="alert" className="col-span-2 rounded-lg border border-danger bg-danger-subtle p-2.5 text-body text-foreground">
                    {refused.text}
                  </p>
                )}
              </li>
            );
          })}
        </ul>
      </>
    );
  }
  return (
    <Sec label={MY_POOLS.POOLS} meta={runner.state === "claimed" && own.length > 0 ? MY_POOLS.IN_COUNT(within.length, own.length) : undefined} data-runner-pools>
      {inner}
      <RemoveDialog removal={removal} onOpenChange={(o) => !o && setRemoval(null)} onDone={pools.reload} />
    </Sec>
  );
}

function PoolRow({ p, isDefault, onOpen, onEdit, onDelete, onAddRunner, onRemove, onMakeDefault }: {
  p: MyPool; isDefault: boolean; onOpen: () => void; onEdit: () => void; onDelete: () => void; onAddRunner: () => void;
  onRemove: (m: RunnerPoolMemberView) => void; onMakeDefault: () => void;
}) {
  const { pool, members } = p;
  const narrow = narrowShort(pool, poolRes(pool, members));
  return (
    <li data-default-row={isDefault || undefined} className="grid grid-cols-1 items-start gap-x-3 gap-y-1.5 border-t border-border py-2.5 first:border-t-0 sm:grid-cols-[minmax(0,1fr)_auto]">
      <div className="min-w-0">
        <div className="flex flex-wrap items-center gap-1.5 text-sm font-medium">
          <button type="button" className={LINK} onClick={onOpen}>
            {pool.name}
          </button>
          {isDefault && (
            <span data-default-badge className="inline-flex rounded-md border border-info bg-info px-2 py-0.5 text-xs font-semibold leading-5 text-info-foreground">
              {POOLS.DEFAULT_BADGE}
            </span>
          )}
          <button type="button" className={`${LINK} text-xs font-normal`} aria-label={POOLS.DETAILS_OF(pool.name)} onClick={onOpen}>
            {POOLS.DETAILS} ›
          </button>
        </div>
        <div className="mt-0.5 text-xs text-muted-foreground">
          {runTypesText(pool.limits)} · {barriersText(pool.limits)}
          {narrow && ` · ${narrow}`}
        </div>
        <div className="mt-0.5 text-xs text-muted-foreground">
          <strong className="font-semibold">{MY_POOLS.RUNNERS_IN_IT}</strong>
          {members.length === 0 && `: ${POOLS.NONE_YET}`}
        </div>
        {members.map((m) => (
          <div key={`${m.kind}:${m.id}`} data-member className="flex max-w-[420px] flex-wrap items-center justify-between gap-2 py-0.5 text-xs">
            <span className="inline-flex flex-wrap items-center gap-1.5">
              <span>
                <OnlineDot online={m.online} />
                {m.name}
              </span>
              <KindChip kind={m.kind} />
            </span>
            <Button variant="ghost" size="sm" aria-label={MY_POOLS.REMOVE_FROM(m.name, pool.name)} onClick={() => onRemove(m)}>
              {MY_POOLS.REMOVE}
            </Button>
          </div>
        ))}
      </div>
      <div className="flex flex-wrap gap-1.5 sm:justify-end">
        <Button variant="outline" size="sm" aria-label={MY_POOLS.EDIT_POOL(pool.name)} onClick={onEdit}>
          {MY_POOLS.EDIT}
        </Button>
        <Button variant="outline" size="sm" aria-label={MY_POOLS.DELETE_POOL(pool.name)} onClick={onDelete}>
          {MY_POOLS.DELETE}
        </Button>
        <Button variant="outline" size="sm" aria-label={MY_POOLS.ADD_RUNNER_TO(pool.name)} onClick={onAddRunner}>
          {MY_POOLS.ADD_RUNNER}
        </Button>
        {!isDefault && (
          <Button variant="outline" size="sm" aria-label={MY_POOLS.MAKE_MY_DEFAULT(pool.name)} onClick={onMakeDefault}>
            {MY_POOLS.MAKE_DEFAULT}
          </Button>
        )}
      </div>
    </li>
  );
}

/** My pools: the person's own user pools, their runners, their default, and Create a pool. `bare` leaves out the heading, for My pools' own card. */
export function MyPoolsSection({ pools, runners, bare = false }: { pools: Pools; runners: RunnerView[]; bare?: boolean }) {
  const navigate = useNavigate();
  const [openId, setOpenId] = React.useState<string | null>(null);
  const [adding, setAdding] = React.useState<MyPool | null>(null);
  const [deleting, setDeleting] = React.useState<MyPool | null>(null);
  const [removal, setRemoval] = React.useState<Removal | null>(null);
  const [error, setError] = React.useState("");
  const open = pools.pools.find((p) => p.pool.id === openId) ?? null;
  const remote: RemoteRunnerView[] = pools.remote;

  const setDefault = async (p: MyPool | null) => {
    setError("");
    try {
      if (p) await runnerPools.setMyDefaults({ pool_id: p.pool.id });
      else await runnerPools.clearMyDefaults();
      toast.success(p ? MY_POOLS.DEFAULT_MADE(p.pool.name) : MY_POOLS.DEFAULT_CLEARED);
      pools.reload();
    } catch (e) {
      setError(getErrorMessage(e));
    }
  };
  const edit = (id: string) => navigate(`/account/pools/${encodeURIComponent(id)}/edit`);

  let inner: React.ReactNode;
  if (pools.status === "loading") inner = null;
  else if (pools.status === "unavailable") {
    inner = (
      <p role="alert" className="rounded-lg border border-border bg-surface-2 p-2.5 text-body">
        {MY_POOLS.UNAVAILABLE}
      </p>
    );
  } else if (pools.status === "error") {
    inner = (
      <div role="alert" className="flex flex-wrap items-center gap-2 rounded-lg border border-border bg-surface-2 p-2.5 text-body">
        <strong className="font-semibold">{POOLS.ERROR_TITLE}</strong>
        <Button variant="outline" size="sm" onClick={pools.reload}>
          {STATES.RETRY}
        </Button>
      </div>
    );
  } else {
    inner = (
      <>
        {pools.pools.length === 0 ? (
          <p className="mb-1">{MY_POOLS.EMPTY}</p>
        ) : (
          <ul>
            {pools.pools.map((p) => (
              <PoolRow
                key={p.pool.id}
                p={p}
                isDefault={pools.defaultId === p.pool.id}
                onOpen={() => setOpenId(p.pool.id)}
                onEdit={() => edit(p.pool.id)}
                onDelete={() => setDeleting(p)}
                onAddRunner={() => setAdding(p)}
                onRemove={(m) => setRemoval({ pool: p, member: m })}
                onMakeDefault={() => void setDefault(p)}
              />
            ))}
          </ul>
        )}
        <Hint>
          {MY_POOLS.DEFAULT_NOTE}
          {pools.defaultId && (
            <>
              {" "}
              <button type="button" className={LINK} onClick={() => void setDefault(null)}>
                {MY_POOLS.CLEAR_DEFAULT}
              </button>
            </>
          )}
        </Hint>
        <div className="mt-3">
          <Button variant="outline" size="sm" data-create-pool onClick={() => navigate("/account/pools/new")}>
            {MY_POOLS.CREATE}
          </Button>
        </div>
      </>
    );
  }
  const body = (
    <>
      <p className="mb-2 text-muted-foreground">{MY_POOLS.LEDE}</p>
      {pools.defaultGone && (
        <p role="alert" className="mb-2 rounded-lg border border-danger bg-danger-subtle p-2.5 text-body text-foreground">
          {MY_POOLS.DEFAULT_GONE}
        </p>
      )}
      {error && (
        <p role="alert" className="mb-2 rounded-lg border border-danger bg-danger-subtle p-2.5 text-body text-foreground">
          {error}
        </p>
      )}
      {inner}
      <PoolDrawer
        pool={open?.pool ?? null}
        members={open?.members ?? []}
        waiting={open?.entry.slots?.waiting}
        isDefault={!!open && pools.defaultId === open.pool.id}
        mine
        onClose={() => setOpenId(null)}
        onChanged={pools.reload}
        onEdit={(pool) => edit(pool.id)}
      />
      <AddRunnerDialog pool={adding} runners={runners} remote={remote} onOpenChange={(o) => !o && setAdding(null)} onAdded={pools.reload} />
      <DeletePoolDialog
        pool={deleting?.pool ?? null}
        mine
        isDefault={!!deleting && pools.defaultId === deleting.pool.id}
        waiting={deleting?.entry.slots?.waiting}
        onOpenChange={(o) => !o && setDeleting(null)}
        onDeleted={pools.reload}
      />
      <RemoveDialog removal={removal} onOpenChange={(o) => !o && setRemoval(null)} onDone={pools.reload} />
    </>
  );
  return bare ? (
    <div id="my-pools">
      <p className="mb-1 text-meta text-muted-foreground">{MY_POOLS.ONLY_YOU}</p>
      {body}
    </div>
  ) : (
    <Sec label={MY_POOLS.TITLE} meta={MY_POOLS.ONLY_YOU} id="my-pools">
      {body}
    </Sec>
  );
}
