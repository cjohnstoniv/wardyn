/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The Templates screen: the saved starting points the caller can see, with Use, Rename and Delete where the
// server says they may write (`writable`). The same screen serves the User view (/templates) and the Admin
// view (/admin/templates); the Admin view adds Publish to organization for an organization administrator.
// Every sentence is a constant of copy/new-run/templates.ts, and the server's own sentence for a refusal.
import * as React from "react";
import { LayoutTemplate, SearchX } from "lucide-react";
import { Link } from "react-router-dom";
import { toast } from "sonner";
import { getErrorMessage } from "../../../lib/format";
import type { TemplateList, TemplateSummary } from "../../../lib/types/templates";
import {
  AlertDialog,
  AlertDialogAction,
  AlertDialogCancel,
  AlertDialogContent,
  AlertDialogDescription,
  AlertDialogFooter,
  AlertDialogHeader,
  AlertDialogTitle,
} from "../../ui/alert-dialog";
import { Button } from "../../ui/button";
import { Input } from "../../ui/input";
import { useConsoleMode } from "../../wardyn/console-view";
import { NR_SAVE } from "../../wardyn/copy/new-run/save";
import { NR_TEMPLATES as T } from "../../wardyn/copy/new-run/templates";
import { Notice } from "../../wardyn/notice";
import { Segmented } from "../../wardyn/segmented";
import { EmptyState, ErrorState, TableSkeleton } from "../../wardyn/states";
import { NEW_RUN_PATH } from "../new-run-flow/draft/new-run-url";
import { RenameTemplateDialog } from "./rename-template-dialog";
import { deleteBody } from "./template-display";
import { ROW_GRID, TemplateRow } from "./template-row";
import { templatesApi } from "./templates-api";

type ScopeFilter = "all" | "me" | "groups" | "org";
type KindFilter = "all" | "templates" | "policies";

const SCOPE_OPTIONS: { value: ScopeFilter; label: string }[] = [
  { value: "all", label: T.SCOPE_ALL },
  { value: "me", label: T.SCOPE_ME },
  { value: "groups", label: T.SCOPE_GROUPS },
  { value: "org", label: T.SCOPE_ORG },
];
const KIND_OPTIONS: { value: KindFilter; label: string }[] = [
  { value: "all", label: T.KIND_ALL },
  { value: "templates", label: T.KIND_TEMPLATES },
  { value: "policies", label: T.KIND_POLICIES },
];
const SCOPE_OF: Record<Exclude<ScopeFilter, "all">, TemplateSummary["scope"]> = { me: "person", groups: "group", org: "org" };
const KIND_OF: Record<Exclude<KindFilter, "all">, TemplateSummary["kind"]> = { templates: "template", policies: "policy" };

export function TemplatesScreen() {
  const admin = useConsoleMode() === "admin";
  const [data, setData] = React.useState<TemplateList | null>(null);
  const [loadError, setLoadError] = React.useState<string | null>(null);
  const [scope, setScope] = React.useState<ScopeFilter>("all");
  const [kind, setKind] = React.useState<KindFilter>("all");
  const [query, setQuery] = React.useState("");
  const [renaming, setRenaming] = React.useState<TemplateSummary | null>(null);
  const [deleting, setDeleting] = React.useState<TemplateSummary | null>(null);
  const [publishing, setPublishing] = React.useState<TemplateSummary | null>(null);
  const [deleteError, setDeleteError] = React.useState<string | null>(null);
  const [busyId, setBusyId] = React.useState<string | null>(null);

  // No "loading" flip here: this is also the refresh after a write, and the first read owns the skeleton.
  const load = React.useCallback(() => {
    templatesApi
      .list()
      .then((list) => {
        setData(list);
        setLoadError(null);
      })
      .catch((e) => setLoadError(getErrorMessage(e)));
  }, []);
  React.useEffect(load, [load]);

  const rows = data?.templates ?? [];
  const needle = query.trim().toLowerCase();
  const shown = rows.filter(
    (t) =>
      (scope === "all" || t.scope === SCOPE_OF[scope]) &&
      (kind === "all" || t.kind === KIND_OF[kind]) &&
      (!needle || t.name.toLowerCase().includes(needle)),
  );
  const canPublish = admin && !!data?.publish.org;

  const clearFilters = () => {
    setScope("all");
    setKind("all");
    setQuery("");
  };

  const remove = async (t: TemplateSummary) => {
    setBusyId(t.id);
    setDeleteError(null);
    try {
      await templatesApi.remove(t.id);
      setDeleting(null);
      toast.success(T.DELETED(t.name));
    } catch (e) {
      setDeleteError(getErrorMessage(e));
    } finally {
      setBusyId(null);
      load();
    }
  };

  const publish = async (t: TemplateSummary) => {
    setBusyId(t.id);
    try {
      await templatesApi.publishToOrg(t.id);
      toast.success(T.PUBLISHED(t.name));
    } catch (e) {
      toast.error(getErrorMessage(e));
    } finally {
      setBusyId(null);
      load();
    }
  };

  let body: React.ReactNode;
  if (loadError !== null) body = <ErrorState message={loadError} onRetry={load} />;
  else if (!data) body = <TableSkeleton rows={4} cols={5} />;
  else if (rows.length === 0)
    body = (
      <EmptyState
        icon={LayoutTemplate}
        title={T.EMPTY_TITLE}
        description={T.EMPTY_BODY}
        action={
          <Button asChild variant="outline">
            <Link to={NEW_RUN_PATH}>{T.EMPTY_ACTION}</Link>
          </Button>
        }
      />
    );
  else if (shown.length === 0)
    body = (
      <EmptyState
        icon={SearchX}
        title={T.NO_MATCH_TITLE}
        description={T.NO_MATCH_BODY}
        action={
          <Button variant="outline" onClick={clearFilters}>
            {T.CLEAR_FILTERS}
          </Button>
        }
      />
    );
  else
    body = (
      <>
        <div aria-hidden="true" className={`label-eyebrow hidden gap-x-4 bg-surface-2 px-4 py-2.5 md:grid ${ROW_GRID}`}>
          <span>{T.COL_NAME}</span>
          <span>{T.COL_AVAILABLE}</span>
          <span>{T.COL_INCLUDES}</span>
          <span>{T.COL_UPDATED}</span>
          <span className="sr-only">{T.COL_ACTIONS}</span>
        </div>
        <ul>
          {shown.map((t) => (
            <TemplateRow
              key={t.id}
              t={t}
              canPublish={canPublish && t.scope !== "org"}
              busy={busyId === t.id}
              onRename={() => setRenaming(t)}
              onDelete={() => {
                setDeleteError(null);
                setDeleting(t);
              }}
              onPublish={() => setPublishing(t)}
            />
          ))}
        </ul>
      </>
    );

  return (
    // The prototype's page column (proto wardyn-templates.html:56, 120): 1120 px, 24 px sides (16 px below 760 px),
    // 32 px above and 96 px below.
    <div className="mx-auto max-w-[1120px] space-y-5 px-6 pt-8 pb-24 max-[760px]:px-4 max-[760px]:pt-6">
      <div className="flex flex-wrap items-end justify-between gap-4">
        <div className="space-y-1">
          <div className="label-eyebrow">{T.EYEBROW}</div>
          <h1 className="text-[1.75rem] leading-tight font-bold text-foreground">{T.TITLE}</h1>
          <p className="max-w-[66ch] text-sm text-muted-foreground">{T.LEDE}</p>
        </div>
        <Button asChild>
          <Link to={NEW_RUN_PATH}>{T.NEW_FROM_SCRATCH}</Link>
        </Button>
      </div>

      <div className="flex flex-wrap items-center gap-3">
        <div role="group" aria-label={T.SCOPE_FILTER_ARIA}>
          <Segmented value={scope} options={SCOPE_OPTIONS} onChange={setScope} />
        </div>
        <div role="group" aria-label={T.KIND_FILTER_ARIA}>
          <Segmented value={kind} options={KIND_OPTIONS} onChange={setKind} />
        </div>
        <Input
          type="search"
          aria-label={T.SEARCH_LABEL}
          placeholder={T.SEARCH_PLACEHOLDER}
          value={query}
          onChange={(e) => setQuery(e.target.value)}
          className="min-w-[14rem] flex-1"
        />
      </div>

      <section aria-label={T.LIST_ARIA} className="overflow-hidden rounded-xl border border-border bg-card">
        {body}
      </section>

      <Notice tone="info">{T.NOTE}</Notice>

      {renaming && (
        <RenameTemplateDialog
          template={rows.find((r) => r.id === renaming.id) ?? renaming}
          rows={rows}
          onClose={() => setRenaming(null)}
          onRenamed={(name) => {
            setRenaming(null);
            toast.success(T.RENAMED(name));
            load();
          }}
          onStale={load}
        />
      )}

      <AlertDialog open={!!publishing} onOpenChange={(open) => !open && setPublishing(null)}>
        <AlertDialogContent>
          <AlertDialogHeader>
            <AlertDialogTitle>{publishing ? T.PUBLISH_TITLE(publishing.name) : ""}</AlertDialogTitle>
            <AlertDialogDescription>{NR_SAVE.SHARED_DISCLOSURE}</AlertDialogDescription>
          </AlertDialogHeader>
          <AlertDialogFooter>
            <AlertDialogCancel>{T.CANCEL}</AlertDialogCancel>
            <AlertDialogAction
              onClick={() => {
                if (publishing) void publish(publishing);
                setPublishing(null);
              }}
            >
              {T.PUBLISH}
            </AlertDialogAction>
          </AlertDialogFooter>
        </AlertDialogContent>
      </AlertDialog>

      <AlertDialog open={!!deleting} onOpenChange={(open) => !open && setDeleting(null)}>
        <AlertDialogContent>
          <AlertDialogHeader>
            <AlertDialogTitle>{deleting ? T.DELETE_TITLE(deleting.name) : ""}</AlertDialogTitle>
            <AlertDialogDescription>{deleting ? deleteBody(deleting) : ""}</AlertDialogDescription>
          </AlertDialogHeader>
          {deleteError && (
            <Notice tone="danger" live>
              {deleteError}
            </Notice>
          )}
          <AlertDialogFooter>
            <AlertDialogCancel>{T.CANCEL}</AlertDialogCancel>
            <AlertDialogAction
              className="bg-danger text-danger-foreground hover:bg-danger/90"
              disabled={busyId !== null}
              onClick={(e) => {
                e.preventDefault();
                if (deleting) void remove(deleting);
              }}
            >
              {T.DELETE_CONFIRM}
            </AlertDialogAction>
          </AlertDialogFooter>
        </AlertDialogContent>
      </AlertDialog>
    </div>
  );
}
