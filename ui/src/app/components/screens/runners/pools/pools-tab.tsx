/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The Admin -> Runners "Pools" tab (mock M-POOLS v6 poolsPage): Organization pools (with the organization
// default) and User pools (every person's own, read-only). The page header and the Runners | Pools strip are the
// caller's. Both tables are a CSS grid with table roles; below 860px each row folds to two columns with the
// column name over each cell. A pool opens in the details sheet; Edit goes to the pool form. Making a pool the
// default keeps the organization's default barrier: the write sends back the barrier it just read.
import * as React from "react";
import { Server } from "lucide-react";
import { Link, useNavigate } from "react-router-dom";
import { toast } from "sonner";
import { runnerPools } from "../../../../lib/api/runner-pools";
import { getErrorMessage } from "../../../../lib/format";
import type { RemoteRunnerView, RunnerKind, RunnerPool, RunnerPoolMemberView } from "../../../../lib/types/runner-pools";
import type { RunnerView } from "../../../../lib/types/runners";
import { POOLS, RUNNER_KIND } from "../../../wardyn/copy/runner-pools";
import { useOperator } from "../../../wardyn/operator-context";
import { CopyButton } from "../../../wardyn/copy-button";
import { Chip } from "../../../wardyn/primitives";
import { EmptyState, STATES } from "../../../wardyn/states";
import { Button } from "../../../ui/button";
import { OnlineDot } from "../resource-table";
import { barriersText, membersOf, narrowShort, poolRes, runTypesText } from "../resources";
import { DEFAULT_CHIP, PoolDrawer } from "./pool-drawer";
import { usePoolsData, type PoolEntry } from "./use-pools-data";

const GRID_ORG = "grid-cols-[minmax(150px,1.2fr)_minmax(130px,1fr)_minmax(190px,1.6fr)_minmax(120px,.9fr)_minmax(104px,.8fr)]";
const GRID_USER = "grid-cols-[minmax(120px,1fr)_minmax(150px,1.3fr)_minmax(130px,1fr)_minmax(110px,.8fr)_minmax(130px,1.2fr)]";
const ROW = "grid items-center gap-x-3.5 gap-y-1.5 px-1 text-body max-[860px]:grid-cols-2";
// The column name over a cell, only where the grid has folded (the header row is hidden there).
const CELL =
  "min-w-0 break-words max-[860px]:before:block max-[860px]:before:text-meta max-[860px]:before:font-semibold max-[860px]:before:uppercase max-[860px]:before:tracking-[0.06em] max-[860px]:before:text-muted-foreground max-[860px]:before:content-[attr(data-col)]";
const SUB = "mt-0.5 block text-xs text-muted-foreground";
const LINK = "text-info underline underline-offset-4";

const Note = ({ children, ...rest }: React.HTMLAttributes<HTMLDivElement>) => (
  <div className="mb-3 rounded-lg border border-border bg-surface-2 p-3 text-body" {...rest}>
    {children}
  </div>
);

function Card({ title, body, action }: { title: string; body: string; action?: React.ReactNode }) {
  return (
    <div role="group" aria-label={title} className="mb-4 rounded-xl border border-border bg-card p-4">
      <h3 className="mb-1 text-foreground">{title}</h3>
      <p className={`text-sm text-muted-foreground ${action ? "mb-3" : ""}`}>{body}</p>
      {action}
    </div>
  );
}

function EmptyCard({ title, body }: { title: string; body: React.ReactNode }) {
  return (
    <div role="group" aria-label={title} className="mb-4 rounded-xl border border-border bg-card">
      <EmptyState size="compact" icon={Server} title={title} description={body} />
    </div>
  );
}

function SectionHead({ id, title, lede }: { id: string; title: string; lede: string }) {
  return (
    <>
      <h2 id={id} className="mb-1 mt-5">
        {title}
      </h2>
      <p className="mb-2.5 max-w-[68ch] text-sm text-muted-foreground">{lede}</p>
    </>
  );
}

function PoolRow({
  e,
  members,
  user,
  isDefault,
  canDefault,
  onOpen,
  onMakeDefault,
}: {
  e: PoolEntry;
  members: RunnerPoolMemberView[];
  user: boolean;
  isDefault: boolean;
  canDefault: boolean;
  onOpen: (id: string) => void;
  onMakeDefault: (pool: RunnerPool) => void;
}) {
  const { pool, entry } = e;
  const res = poolRes(pool, members);
  const limits = narrowShort(pool, res);
  const active = pool.state === "active";
  return (
    <div role="row" data-default-row={isDefault ? "true" : undefined} className="border-b border-border py-3.5">
      <div role="presentation" className={`${ROW} ${user ? GRID_USER : GRID_ORG}`}>
        <div role="cell" className="min-w-0 break-words max-[860px]:col-span-2">
          <button type="button" className={`${LINK} -ml-1 rounded-md px-1 text-left text-sm font-medium hover:bg-accent`} onClick={() => onOpen(pool.id)}>
            {pool.name}
          </button>
          {isDefault && <Chip className={`ml-1 font-semibold ${DEFAULT_CHIP}`}>{POOLS.DEFAULT_BADGE}</Chip>}
          <span className={SUB}>
            {user ? POOLS.USER_POOL : POOLS.ORG_POOL} ·{" "}
            <button type="button" aria-label={POOLS.DETAILS_OF(pool.name)} className="text-xs text-info hover:underline" onClick={() => onOpen(pool.id)}>
              {POOLS.DETAILS} ›
            </button>
          </span>
        </div>
        {user && (
          <div role="cell" data-col={POOLS.COL_OWNER} className={CELL}>
            {entry.owner ?? pool.owner}
          </div>
        )}
        <div role="cell" data-col={POOLS.COL_ALLOWS} className={CELL}>
          {runTypesText(pool.limits)}
          <span className={SUB}>{barriersText(pool.limits)}</span>
          {limits && (
            <span className={SUB} data-pool-limits>
              {limits}
            </span>
          )}
        </div>
        <div role="cell" data-col={POOLS.COL_RUNNERS} className={CELL}>
          {members.length ? (
            members.map((m) => (
              <span key={`${m.kind}:${m.id}`} className="block">
                <OnlineDot online={m.online} />
                {m.name}
                {m.kind === "remote_hosted" && user && <span className="text-muted-foreground"> · {RUNNER_KIND.REMOTE_LOWER}</span>}
              </span>
            ))
          ) : (
            <span className="text-muted-foreground" data-no-runners>
              {POOLS.NO_RUNNERS}
            </span>
          )}
        </div>
        <div role="cell" data-col={POOLS.COL_STATE} className={CELL}>
          <Chip tone={active ? "success" : "neutral"} dot>
            {active ? POOLS.ACTIVE : POOLS.SWITCHED_OFF}
          </Chip>
          {entry.slots && <span className={SUB}>{POOLS.RUNNING_WAITING(entry.slots.used, entry.slots.waiting)}</span>}
        </div>
        {!user && (
          <div role="cell" data-col="" className={`${CELL} empty:hidden`}>
            {isDefault ? null : !active ? (
              <span className="text-xs text-muted-foreground">{POOLS.OFF_NO_DEFAULT}</span>
            ) : canDefault ? (
              <Button variant="outline" size="sm" aria-label={POOLS.MAKE_ORG_DEFAULT(pool.name)} onClick={() => onMakeDefault(pool)}>
                {POOLS.MAKE_DEFAULT}
              </Button>
            ) : null}
          </div>
        )}
      </div>
    </div>
  );
}

function Header({ user }: { user: boolean }) {
  const cols = user ? [POOLS.COL_POOL, POOLS.COL_OWNER, POOLS.COL_ALLOWS, POOLS.COL_RUNNERS, POOLS.COL_STATE] : [POOLS.COL_POOL, POOLS.COL_ALLOWS, POOLS.COL_RUNNERS, POOLS.COL_STATE];
  return (
    <div role="row" className={`${ROW} label-eyebrow border-b border-border py-2 max-[860px]:hidden ${user ? GRID_USER : GRID_ORG}`}>
      {cols.map((c) => (
        <span key={c} role="columnheader">
          {c}
        </span>
      ))}
      {!user && <span role="columnheader" aria-label={POOLS.COL_DEFAULT} />}
    </div>
  );
}

export function PoolsTab({
  runners,
  remote,
  openPoolId,
  onOpenPool,
  onOpenRunner,
}: {
  runners: RunnerView[];
  remote: RemoteRunnerView[];
  openPoolId: string | null;
  onOpenPool: (id: string | null) => void;
  onOpenRunner: (kind: RunnerKind, id: string) => void;
}) {
  const superAdmin = useOperator();
  const navigate = useNavigate();
  const { status, message, entries, defaultId, reload, retry } = usePoolsData(openPoolId);
  const [defaultsError, setDefaultsError] = React.useState("");

  // Both the row button and the sheet write the default. A refusal throws; each caller shows the server's sentence.
  // Every field this tab does not edit (the barrier, recording) is sent back as read.
  const setDefault = React.useCallback(
    async (poolId: string | null) => {
      const current = await runnerPools.orgDefaults();
      await runnerPools.setOrgDefaults({ ...current, pool_id: poolId ?? undefined });
      const name = entries.find((x) => x.pool.id === poolId)?.pool.name ?? "";
      toast.success(poolId ? POOLS.DEFAULT_MADE(name) : POOLS.DEFAULT_CLEARED);
      reload();
    },
    [entries, reload],
  );
  const makeDefault = (pool: RunnerPool) => {
    setDefaultsError("");
    setDefault(pool.id).catch((e: unknown) => setDefaultsError(getErrorMessage(e)));
  };

  if (status === "loading") {
    return (
      <div role="status" aria-label={POOLS.LOADING}>
        <span className="sr-only">{POOLS.LOADING}</span>
        {[0, 1, 2, 3].map((i) => (
          <div key={i} className="border-b border-border px-1 py-2.5">
            <div className="h-[34px] rounded-lg bg-muted" />
          </div>
        ))}
      </div>
    );
  }
  if (status === "unavailable") {
    return (
      <div role="alert" className="rounded-lg border border-border bg-surface-2 p-3 text-body">
        <strong className="font-semibold">{POOLS.UNAVAILABLE_TITLE}</strong>
        <br />
        {message}
      </div>
    );
  }
  if (status === "error") {
    return (
      <Card
        title={POOLS.ERROR_TITLE}
        body={POOLS.ERROR_BODY}
        action={
          <Button variant="outline" size="sm" onClick={retry}>
            {STATES.RETRY}
          </Button>
        }
      />
    );
  }

  const org = entries.filter((x) => x.entry.scope === "organization");
  const user = entries.filter((x) => x.entry.scope === "user");
  const open = entries.find((x) => x.pool.id === openPoolId);
  const membersFor = (id: string) => membersOf(id, runners, remote);
  const row = (e: PoolEntry, isUser: boolean) => (
    <PoolRow
      key={e.pool.id}
      e={e}
      members={membersFor(e.pool.id)}
      user={isUser}
      isDefault={!isUser && e.pool.id === defaultId}
      canDefault={superAdmin}
      onOpen={onOpenPool}
      onMakeDefault={makeDefault}
    />
  );

  return (
    <div>
      {org.length === 0 ? (
        <>
          <h2 id="sec-org" className="mb-2.5 mt-5">
            {POOLS.ORG_TITLE}
          </h2>
          {!superAdmin && <Note>{POOLS.ORG_SECURITY}</Note>}
          <EmptyCard
            title={POOLS.ORG_EMPTY_TITLE}
            body={
              <>
                {POOLS.ORG_CLI}
                <span className="my-2 flex items-center justify-center gap-2">
                  <span className="rounded-md border border-border bg-surface-2 px-2 py-1 font-mono text-xs break-all text-foreground">
                    {POOLS.ORG_CLI_COMMAND}
                  </span>
                  <CopyButton
                    text={POOLS.ORG_CLI_COMMAND}
                    label={POOLS.ORG_CLI_COPY}
                    className="gap-1 rounded-md border border-border px-2 py-1 text-xs text-foreground hover:bg-accent"
                  >
                    {POOLS.ORG_CLI_COPY}
                  </CopyButton>
                </span>
                {POOLS.ORG_EMPTY_THEN}
              </>
            }
          />
        </>
      ) : (
        <>
          <SectionHead id="sec-org" title={POOLS.ORG_TITLE} lede={POOLS.ORG_LEDE} />
          <Note>
            {POOLS.ORG_CLI} <span className="font-mono">{POOLS.ORG_CLI_COMMAND}</span>. {POOLS.ORG_CLI_EDIT}
          </Note>
          {!superAdmin && <Note>{POOLS.ORG_SECURITY}</Note>}
          {defaultsError && (
            <div role="alert" className="mb-3 rounded-lg border border-danger bg-danger-subtle p-3 text-body">
              {defaultsError}
            </div>
          )}
          <p className="mb-1 text-xs text-muted-foreground">{POOLS.ORG_RUNS_ON}</p>
          {defaultId ? <p className="mb-2 text-xs text-muted-foreground">{POOLS.ORG_DEFAULT_NOTE}</p> : <Note>{POOLS.ORG_NO_DEFAULT}</Note>}
          <div role="table" aria-label={POOLS.ORG_TITLE} className="border-t border-border">
            <Header user={false} />
            {org.map((e) => row(e, false))}
          </div>
        </>
      )}
      <SectionHead id="sec-user" title={POOLS.USER_TITLE} lede={POOLS.USER_LEDE} />
      <Note>
        {POOLS.USER_GOVERNANCE}{" "}
        <Link to="/admin/governance" className="text-info hover:underline">
          {POOLS.GOVERNANCE_LINK}
        </Link>
      </Note>
      {user.length === 0 ? (
        <EmptyCard title={POOLS.USER_EMPTY_TITLE} body={POOLS.USER_EMPTY_BODY} />
      ) : (
        <div role="table" aria-label={POOLS.USER_TITLE} className="border-t border-border">
          <Header user />
          {user.map((e) => row(e, true))}
        </div>
      )}
      <PoolDrawer
        pool={open?.pool ?? null}
        members={open ? membersFor(open.pool.id) : []}
        waiting={open?.entry.slots?.waiting}
        isDefault={!!open && open.entry.scope === "organization" && open.pool.id === defaultId}
        mine={false}
        onClose={() => onOpenPool(null)}
        onChanged={reload}
        onOpenRunner={onOpenRunner}
        onEdit={(p) => navigate(`/admin/runners/pools/${p.id}/edit`)}
        onSetDefault={setDefault}
      />
    </div>
  );
}
