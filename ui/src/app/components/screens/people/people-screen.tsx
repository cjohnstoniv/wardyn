/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Admin view -> People (0.8.6 ppl-p2, mock M12): everyone with access and every per-person action in
// one place. Top to bottom: who can sign in (the existing access panel, mounted unchanged and only for
// the super admin, whose route it is), the people table, and the SCIM card. A row opens the drawer.
// Both admin tiers reach it; the server (GET /people and every action) is securityOps, and hiding is
// cosmetic.
import * as React from "react";
import { Users } from "lucide-react";
import { access as accessApi } from "../../../lib/api/access";
import { HttpError } from "../../../lib/api/core";
import { people as peopleApi } from "../../../lib/api/people";
import { absoluteTime, relativeTime } from "../../../lib/format";
import type { AccessResponse, PersonSummary } from "../../../lib/types";
import { SECURITY_ONLY_REASON } from "../../wardyn/copy";
import { PEOPLE_PAGE as P } from "../../wardyn/copy/people";
import { useShellSetupStatus } from "../../wardyn/model-access-context";
import { useOperator } from "../../wardyn/operator-context";
import { Button } from "../../ui/button";
import { Input } from "../../ui/input";
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "../../ui/table";
import { PageHeader } from "../../wardyn/page-header";
import { Chip } from "../../wardyn/primitives";
import { EmptyState, ErrorState, TableSkeleton, loadFailStatus, type ScreenStatus } from "../../wardyn/states";
import { AccessPanel, type AccessLoadState } from "../setup/access-panel";
import { ScimCard } from "../settings/scim-card";
import { AddPersonDialog } from "./add-person-dialog";
import { PersonDrawer } from "./person-drawer";
import { personState, roleLabel, STATE_LABEL, type PersonState } from "./people-format";

const PAGE_SIZE = 50;
const STATE_TONE: Record<PersonState, "success" | "warning" | "neutral"> = { active: "success", deactivated: "warning", never: "neutral" };
type StateFilter = "all" | "active" | "deactivated";

// The access panel's own fetch, the same one the Getting Started People step owns. Only the super
// admin's route (GET /access is operator-only), so nobody else issues it.
function useAccess(enabled: boolean) {
  const [access, setAccess] = React.useState<AccessResponse | null>(null);
  const [state, setState] = React.useState<AccessLoadState>("loading");
  const { refresh: refreshShell } = useShellSetupStatus();
  const load = React.useCallback(() => {
    setState((s) => (s === "ready" ? s : "loading"));
    return accessApi
      .getAccess()
      .then((data) => {
        setAccess(data);
        setState("ready");
      })
      .catch((e) => {
        setAccess(null);
        setState(e instanceof HttpError && e.status === 503 ? "sso_unavailable" : "fetch_failed");
      });
  }, []);
  React.useEffect(() => {
    if (enabled) void load();
  }, [enabled, load]);
  // A role-mapping write can end the everyone-is-an-admin state, which the shell's banner reads.
  const reload = React.useCallback(() => {
    void load();
    void refreshShell();
  }, [load, refreshShell]);
  return { access, state, reload };
}

function PeopleTable({ onOpen, reloadKey }: { onOpen: (p: PersonSummary) => void; reloadKey: number }) {
  const [rows, setRows] = React.useState<PersonSummary[]>([]);
  const [cursor, setCursor] = React.useState<string | undefined>(undefined);
  const [status, setStatus] = React.useState<ScreenStatus>("loading");
  const [search, setSearch] = React.useState("");
  const [q, setQ] = React.useState("");
  const [filter, setFilter] = React.useState<StateFilter>("all");
  const seq = React.useRef(0);

  // The box debounces into q, so each keystroke is not a request.
  React.useEffect(() => {
    const id = setTimeout(() => setQ(search.trim()), 250);
    return () => clearTimeout(id);
  }, [search]);

  const load = React.useCallback(
    (after?: string) => {
      const mine = ++seq.current;
      if (!after) setStatus("loading");
      peopleApi
        .list({ limit: PAGE_SIZE, cursor: after, q: q || undefined, state: filter === "all" ? undefined : filter })
        .then((page) => {
          if (mine !== seq.current) return;
          setRows((prev) => (after ? [...prev, ...page.people] : page.people));
          setCursor(page.next_cursor || undefined);
          setStatus("ready");
        })
        .catch((e) => {
          if (mine === seq.current) setStatus(loadFailStatus(e));
        });
    },
    [q, filter],
  );
  // reloadKey re-reads the first page after a drawer action or an add.
  React.useEffect(() => load(), [load, reloadKey]);

  const filters: { id: StateFilter; label: string }[] = [
    { id: "all", label: P.FILTER_ALL },
    { id: "active", label: P.FILTER_ACTIVE },
    { id: "deactivated", label: P.FILTER_DEACTIVATED },
  ];

  return (
    <>
      <div className="flex flex-wrap items-center gap-2 border-b border-border p-3">
        <Input
          aria-label={P.SEARCH}
          placeholder={P.SEARCH}
          className="max-w-xs"
          autoComplete="off"
          value={search}
          onChange={(e) => setSearch(e.target.value)}
        />
        <div className="flex gap-1.5" role="group" aria-label={P.COL_STATE}>
          {filters.map((f) => (
            <button
              key={f.id}
              type="button"
              aria-pressed={filter === f.id}
              onClick={() => setFilter(f.id)}
              className={`rounded-full border px-2.5 py-0.5 text-xs ${filter === f.id ? "border-primary text-foreground" : "border-border text-muted-foreground"}`}
            >
              {f.label}
            </button>
          ))}
        </div>
      </div>
      {status === "loading" ? (
        <TableSkeleton rows={5} cols={5} />
      ) : status === "forbidden" ? (
        <p className="p-4 text-sm text-muted-foreground">{SECURITY_ONLY_REASON}</p>
      ) : status === "error" ? (
        <ErrorState onRetry={() => load()} />
      ) : rows.length === 0 ? (
        <EmptyState icon={Users} title={P.TABLE_TITLE} description={q || filter !== "all" ? undefined : P.EMPTY} />
      ) : (
        <div className="overflow-x-auto">
          <Table>
            <TableHeader>
              <TableRow className="hover:bg-transparent">
                <TableHead>{P.COL_PERSON}</TableHead>
                <TableHead>{P.COL_ROLE}</TableHead>
                <TableHead>{P.COL_LAST}</TableHead>
                <TableHead>{P.COL_STATE}</TableHead>
                <TableHead>{P.COL_HOLDS}</TableHead>
              </TableRow>
            </TableHeader>
            <TableBody>
              {rows.map((p) => {
                const state = personState(p);
                return (
                  <TableRow key={p.principal} className="cursor-pointer" onClick={() => onOpen(p)}>
                    <TableCell className="font-medium">
                      <button type="button" className="text-left hover:underline" onClick={(e) => { e.stopPropagation(); onOpen(p); }}>
                        {p.email || p.principal}
                      </button>
                    </TableCell>
                    <TableCell>{p.role ? roleLabel(p.role) : "—"}</TableCell>
                    <TableCell className="text-muted-foreground" title={p.last_signed_in_at ? absoluteTime(p.last_signed_in_at) : undefined}>
                      {p.last_signed_in_at ? relativeTime(p.last_signed_in_at) : "—"}
                    </TableCell>
                    <TableCell>
                      <Chip tone={STATE_TONE[state]}>{STATE_LABEL[state]}</Chip>
                    </TableCell>
                    <TableCell className="text-muted-foreground">
                      {P.HOLDS({ sessions: p.active_sessions, tokens: p.api_tokens, keys: p.ssh_keys, credentials: p.credentials, running: p.active_runs })}
                    </TableCell>
                  </TableRow>
                );
              })}
            </TableBody>
          </Table>
          {cursor && (
            <div className="border-t border-border p-3">
              <Button variant="outline" size="sm" onClick={() => load(cursor)}>
                Load more
              </Button>
            </div>
          )}
        </div>
      )}
    </>
  );
}

export function PeopleScreen() {
  const operator = useOperator();
  const { access, state, reload } = useAccess(operator);
  const [reloadKey, setReloadKey] = React.useState(0);
  const [selected, setSelected] = React.useState<PersonSummary | null>(null);
  const [adding, setAdding] = React.useState(false);
  // A drawer action or an add re-reads the first page, and the open person on their own: after Load more
  // they are not on that page, and the drawer's counts and disabled buttons follow what the action did.
  const refresh = () => {
    setReloadKey((k) => k + 1);
    const open = selected?.principal;
    if (open) {
      peopleApi
        .get(open)
        .then((p) => p && setSelected((cur) => (cur?.principal === open ? p : cur)))
        .catch(() => {});
    }
  };

  return (
    <div className="mx-auto max-w-[1200px] px-6 py-6">
      <PageHeader
        title={P.TITLE}
        description={P.LEAD}
        actions={
          <Button variant="outline" onClick={() => setAdding(true)}>
            {P.ADD}
          </Button>
        }
      />

      <section className="mt-6 space-y-3" aria-labelledby="people-access">
        <h2 id="people-access" className="label-eyebrow">
          {P.ACCESS_TITLE}
        </h2>
        {operator ? (
          <AccessPanel access={access} state={state} onReload={reload} />
        ) : (
          <p className="text-sm text-muted-foreground">{P.ACCESS_SUPER_ONLY}</p>
        )}
      </section>

      <section className="mt-8" aria-labelledby="people-table">
        <h2 id="people-table" className="label-eyebrow mb-2">
          {P.TABLE_TITLE}
        </h2>
        <div className="overflow-hidden rounded-xl border border-border bg-card">
          <PeopleTable onOpen={setSelected} reloadKey={reloadKey} />
        </div>
      </section>

      <div className="mt-8">
        <ScimCard />
      </div>

      <PersonDrawer
        person={selected}
        onClose={() => setSelected(null)}
        onChanged={refresh}
      />
      <AddPersonDialog open={adding} onOpenChange={setAdding} onAdded={refresh} />
    </div>
  );
}
