/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import * as React from "react";
import { useNavigate } from "react-router-dom";
import { FolderGit2, FolderOpen, Hourglass, MoreHorizontal, Plus, RotateCw, Trash2 } from "lucide-react";
import { workspaces as api } from "../../lib/api/workspaces";
import { LIST_LIMIT } from "../../lib/api/core";
import { compositionSummary } from "./new-run/wizard-types";
import type { Workspace, WorkspaceKind, WorkspaceProfile } from "../../lib/types";
import { Button } from "../ui/button";
import { Input } from "../ui/input";
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "../ui/table";
import {
  DropdownMenu,
  DropdownMenuContent,
  DropdownMenuItem,
  DropdownMenuTrigger,
} from "../ui/dropdown-menu";
import { Mono } from "../wardyn/code-block";
import { DeleteConfirmDialog } from "../wardyn/delete-confirm-dialog";
import { Chip, OperatorOnlyHint } from "../wardyn/primitives";
import { EmptyState, ErrorState, TableSkeleton, TruncatedNote } from "../wardyn/states";
import { PageHeader } from "../wardyn/page-header";
import { AddWorkspaceDialog } from "./add-workspace-dialog";
import { llmCredLabel, llmCredTone } from "./workspace-llm-cred";
import { OPERATOR_ONLY_REASON } from "../wardyn/copy";
import { useOperator } from "../wardyn/operator-context";

// Icon + label for the three onboardable kinds. "ephemeral" is scratch space
// discarded after the run, so it gets an hourglass rather than reusing the
// local-dir folder.
export const KIND_META: Record<WorkspaceKind, { Icon: React.ElementType; label: string }> = {
  local_dir: { Icon: FolderOpen, label: "local dir" },
  repo: { Icon: FolderGit2, label: "repo" },
  ephemeral: { Icon: Hourglass, label: "ephemeral" },
};

// The list's "Source" column: a multi-source composition summary
// ("2 dirs · 1 repo") or, for a single/pre-composition source, its mono path —
// exported so the detail page's header renders a consistent line off the same
// underlying data (never a second copy of this ternary).
export function sourceSubLine(ws: Workspace): string {
  const multi = compositionSummary(ws);
  if (multi) return multi;
  const single = ws.kind === "repo" && ws.ref ? `${ws.source} @${ws.ref}` : ws.source;
  // An ephemeral-only workspace carries neither Path nor Source (store.go's
  // deriveWorkspaceMirrors) — the floor scratch dir has no host-side location
  // to show.
  return single || "empty — discarded after the run";
}

export type WorkspaceImageKind = "devcontainer" | "standard" | "ref";

// The list's "Image" chip + the detail page's image row both derive from the
// SAME workspace fields — never a base-image catalog fetch (Stage 2: the UI
// stops calling GET /base-images entirely). `base_image` is the operator's
// explicit pin (registry/custom/byo, always carrying a concrete `image`);
// absent (or the implicit "recommended") falls back to whatever the scan
// profile detected, honestly degrading to "standard sandbox image" when
// nothing did or nothing has scanned yet.
export function workspaceImage(ws: Workspace): { kind: WorkspaceImageKind; label: string } {
  const b = ws.base_image;
  if (b && b.kind !== "recommended" && b.image) return { kind: "ref", label: b.image };
  const hasDevcontainer = !!(ws.profile as WorkspaceProfile | null)?.has_devcontainer;
  return hasDevcontainer
    ? { kind: "devcontainer", label: "devcontainer.json" }
    : { kind: "standard", label: "standard sandbox image" };
}

export function WorkspacesScreen() {
  const operator = useOperator();
  const navigate = useNavigate();
  const [workspaces, setWorkspaces] = React.useState<Workspace[]>([]);
  const [status, setStatus] = React.useState<"loading" | "error" | "ready">("loading");
  const [query, setQuery] = React.useState("");
  const [addOpen, setAddOpen] = React.useState(false);
  const [toDelete, setToDelete] = React.useState<Workspace | null>(null);

  const load = React.useCallback(() => {
    setStatus("loading");
    api
      .listWorkspaces()
      .then((ws) => {
        setWorkspaces(ws);
        setStatus("ready");
      })
      .catch(() => setStatus("error"));
  }, []);
  React.useEffect(load, [load]);

  const filtered = workspaces.filter(
    (w) =>
      !query ||
      w.name.toLowerCase().includes(query.toLowerCase()) ||
      w.source.toLowerCase().includes(query.toLowerCase()),
  );

  const openDetail = (id: string) => navigate(`/workspaces/${encodeURIComponent(id)}`);

  return (
    <div className="mx-auto max-w-[1400px] px-6 py-6">
      <PageHeader
        title="Workspaces"
        description="A repo or directory a run can attach. Runs can only attach what's listed here."
        actions={
          <>
            {!operator && <Chip tone="neutral">{OPERATOR_ONLY_REASON}</Chip>}
            <Button onClick={() => setAddOpen(true)} disabled={!operator}>
              <Plus className="size-4" /> Add workspace
            </Button>
          </>
        }
      />

      {status === "ready" && workspaces.length > 0 && (
        <div className="mb-4 flex items-center gap-3">
          <Input
            placeholder="Search by name or source…"
            value={query}
            onChange={(e) => setQuery(e.target.value)}
            className="max-w-sm"
          />
          <span className="ml-auto text-sm text-muted-foreground">
            {filtered.length} of {workspaces.length} workspace{workspaces.length === 1 ? "" : "s"}
          </span>
          <Button variant="outline" size="icon" onClick={load} aria-label="Refresh">
            <RotateCw className="size-4" />
          </Button>
        </div>
      )}

      {/* Past the cap the count above ("N of M") and the search below both cover
          only the fetched window, not every onboarded workspace. */}
      <TruncatedNote count={workspaces.length} cap={LIST_LIMIT} />

      <div className="overflow-hidden rounded-xl border border-border bg-card">
        {status === "loading" ? (
          <TableSkeleton rows={5} cols={4} />
        ) : status === "error" ? (
          <ErrorState onRetry={load} />
        ) : workspaces.length === 0 ? (
          <EmptyState
            icon={FolderOpen}
            title="No workspaces yet."
            description={
              operator
                ? "Add a repository, a local directory, or start empty — a run can attach it right away. Nothing is scanned or built first."
                : `Add a repository, a local directory, or start empty — a run can attach it right away. ${OPERATOR_ONLY_REASON}`
            }
            action={
              <Button onClick={() => setAddOpen(true)} disabled={!operator}>
                <Plus className="size-4" /> Add your first workspace
              </Button>
            }
          />
        ) : filtered.length === 0 ? (
          <EmptyState
            icon={FolderOpen}
            title="No workspaces match that search."
            action={
              <Button variant="outline" onClick={() => setQuery("")}>
                Clear search
              </Button>
            }
          />
        ) : (
          <Table>
            <TableHeader>
              <TableRow className="hover:bg-transparent">
                <TableHead>Workspace</TableHead>
                <TableHead>Source</TableHead>
                <TableHead>Image</TableHead>
                <TableHead>Model</TableHead>
                <TableHead className="w-[44px]" />
              </TableRow>
            </TableHeader>
            <TableBody>
              {filtered.map((w) => {
                const kindMeta = KIND_META[w.kind] ?? KIND_META.local_dir;
                const image = workspaceImage(w);
                return (
                  <TableRow
                    key={w.id}
                    // No role="button" override: a <tr> inside a real <table>
                    // already carries the implicit "row" role, which the
                    // table's own screen-reader/`getByRole("row", …)` cell
                    // associations depend on — overriding it here would trade
                    // one accessibility gap for a worse one. tabIndex + the
                    // Enter/Space handler is enough to make the row a real
                    // keyboard target without giving up its row semantics.
                    tabIndex={0}
                    onClick={() => openDetail(w.id)}
                    onKeyDown={(e) => {
                      if (e.key === "Enter" || e.key === " ") {
                        e.preventDefault();
                        openDetail(w.id);
                      }
                    }}
                    className="cursor-pointer"
                  >
                    <TableCell>
                      <span className="inline-flex items-center gap-2.5">
                        <span className="inline-flex size-7 shrink-0 items-center justify-center rounded-md border border-border text-muted-foreground">
                          <kindMeta.Icon className="size-3.5" />
                        </span>
                        <span className="text-foreground">{w.name}</span>
                      </span>
                    </TableCell>
                    <TableCell>
                      <Mono
                        className="block max-w-[260px] truncate text-[0.6875rem] text-muted-foreground"
                        title={sourceSubLine(w)}
                      >
                        {sourceSubLine(w)}
                      </Mono>
                    </TableCell>
                    <TableCell>
                      <Chip tone="neutral" mono={image.kind === "ref"} className="max-w-[220px] truncate" title={image.label}>
                        {image.label}
                      </Chip>
                    </TableCell>
                    <TableCell>
                      {w.llm_cred?.integration_ref ? (
                        <Chip tone={llmCredTone(w.llm_cred)} mono>
                          {llmCredLabel(w.llm_cred)}
                        </Chip>
                      ) : (
                        <span className="text-muted-foreground">—</span>
                      )}
                    </TableCell>
                    <TableCell onClick={(e) => e.stopPropagation()} onKeyDown={(e) => e.stopPropagation()}>
                      <DropdownMenu>
                        <DropdownMenuTrigger asChild>
                          <Button variant="ghost" size="icon" className="size-8" aria-label="Workspace actions">
                            <MoreHorizontal className="size-4" />
                          </Button>
                        </DropdownMenuTrigger>
                        <DropdownMenuContent align="end">
                          <DropdownMenuItem onClick={() => openDetail(w.id)}>Open</DropdownMenuItem>
                          <DropdownMenuItem
                            onClick={() => setToDelete(w)}
                            disabled={!operator}
                            className="text-danger focus:text-danger"
                          >
                            <Trash2 className="size-4" /> Delete…
                            {!operator && <OperatorOnlyHint />}
                          </DropdownMenuItem>
                        </DropdownMenuContent>
                      </DropdownMenu>
                    </TableCell>
                  </TableRow>
                );
              })}
            </TableBody>
          </Table>
        )}
      </div>

      {addOpen && (
        <AddWorkspaceDialog
          existingNames={workspaces.map((w) => w.name)}
          onClose={() => setAddOpen(false)}
          onCreated={(created) => {
            load();
            openDetail(created.id);
          }}
        />
      )}

      <DeleteConfirmDialog
        name={toDelete?.name ?? null}
        entity="workspace"
        description="Runs can no longer attach this source. Any run already using it is unaffected — this only gates NEW runs. This cannot be undone."
        onOpenChange={(o) => !o && setToDelete(null)}
        onDelete={() => api.deleteWorkspace(toDelete!.id)}
        onDeleted={() => {
          setToDelete(null);
          load();
        }}
      />
    </div>
  );
}
