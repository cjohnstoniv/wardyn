/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import * as React from "react";
import { isFixtureLeak } from "./workspace-wizard/wizard-types";
import { useNavigate } from "react-router-dom";
import {
  FolderGit2,
  FolderOpen,
  Hourglass,
  MoreHorizontal,
  Plus,
  RotateCw,
  Trash2,
} from "lucide-react";
import { workspaces as api } from "../../lib/api/workspaces";
import { secrets as secretsApi } from "../../lib/api/secrets";
import { LIST_LIMIT } from "../../lib/api/core";
import { statusTone, statusWord } from "../../lib/workspace-status";
import { compositionSummary, unstoredRequiredSecrets, workspaceRequirements } from "./new-run/wizard-types";
import type {
  Workspace,
  WorkspaceKind,
  WorkspaceProfile,
} from "../../lib/types";
import { cn } from "../ui/utils";
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
import { Tabs, TabsList, TabsTrigger } from "../ui/tabs";
import { ImageCatalog } from "./image-catalog";
import { SourcesLibrary } from "./sources-library";
import { WorkspaceWizard } from "./workspace-wizard/wizard";
import { llmCredLabel, llmCredTone } from "./workspace-llm-cred";
import { OPERATOR_ONLY_REASON } from "../wardyn/copy";
import { useOperator } from "../wardyn/operator-context";

// Icon + label for the three onboardable kinds. "ephemeral" is scratch space
// discarded after the run, so it gets an hourglass rather than reusing the
// local-dir folder (matches step-sources.tsx's own ephemeral-source icon).
export const KIND_META: Record<WorkspaceKind, { Icon: React.ElementType; label: string }> = {
  local_dir: { Icon: FolderOpen, label: "local dir" },
  repo: { Icon: FolderGit2, label: "repo" },
  ephemeral: { Icon: Hourglass, label: "ephemeral" },
};

// The list's "Workspace" column sub-line: a multi-source composition summary
// ("2 dirs · 1 repo") or, for a single/pre-composition source, its mono path —
// exported so the detail page's header renders the identical line (never a
// second copy of this ternary).
export function sourceSubLine(ws: Workspace): string {
  const multi = compositionSummary(ws);
  if (multi) return multi;
  return ws.kind === "repo" && ws.ref ? `${ws.source} @${ws.ref}` : ws.source;
}

export interface AttentionItem {
  text: string;
  tone: "warning" | "danger" | "neutral";
}

// The "Needs you" cell (list) / header attention line (detail) — mirrors the
// approved mock's attentionFor, minus its scan_failed line (the Status column/
// chip already says that). Order matches the task's own examples: unstored
// required secrets, then hosts awaiting review, then suspected leaks.
export function attentionItems(ws: Workspace, storedSecretNames: string[]): AttentionItem[] {
  const out: AttentionItem[] = [];
  const unstored = unstoredRequiredSecrets(ws, storedSecretNames);
  if (unstored.length) {
    out.push({ text: `${unstored.length} secret${unstored.length > 1 ? "s" : ""} not stored`, tone: "warning" });
  }
  const profile = (ws.profile ?? {}) as WorkspaceProfile;
  const reqs = workspaceRequirements(ws);
  const contractHosts = new Set(
    Object.keys(reqs)
      .filter((k) => k.startsWith("egress:"))
      .map((k) => k.slice("egress:".length)),
  );
  const autoAllowed = new Set(profile.egress_domains ?? []);
  const pendingHosts = (profile.suggested_egress ?? []).filter((h) => !contractHosts.has(h) && !autoAllowed.has(h));
  if (pendingHosts.length) {
    out.push({ text: `${pendingHosts.length} host${pendingHosts.length > 1 ? "s" : ""} awaiting review`, tone: "neutral" });
  }
  // Same two-tier split as the requirements step: a fixture in a test file is
  // not an emergency, and seventeen of them must never bury one real leak.
  const allLeaks = profile.leak_findings ?? [];
  const hotLeaks = allLeaks.filter((l) => !isFixtureLeak(l)).length;
  const fixtureLeaks = allLeaks.length - hotLeaks;
  if (hotLeaks) out.push({ text: `⚠ ${hotLeaks} suspected committed secret${hotLeaks > 1 ? "s" : ""}`, tone: "danger" });
  else if (fixtureLeaks)
    out.push({ text: `${fixtureLeaks} key-shaped string${fixtureLeaks > 1 ? "s" : ""} in test files`, tone: "neutral" });
  return out;
}

// The three tiers in DEPENDENCY order: directories & repos are configured
// first, base images second, and a workspace — the aggregate — composes them.
// One rail component for both surfaces (this page + the Getting-started
// Workspaces step) so the order and placement can never drift.
export type WorkspaceTier = "sources" | "images" | "workspaces";

export function TierTabRail({
  value,
  onChange,
}: {
  value: WorkspaceTier;
  onChange: (tier: WorkspaceTier) => void;
}) {
  return (
    <Tabs
      orientation="vertical"
      value={value}
      onValueChange={(v) => onChange(v as WorkspaceTier)}
      className="shrink-0"
    >
      <TabsList className="h-auto w-44 flex-col items-stretch gap-1 bg-transparent p-0">
        <TabsTrigger className="h-8 justify-start" value="sources">
          Directories &amp; repos
        </TabsTrigger>
        <TabsTrigger className="h-8 justify-start" value="images">
          Base images
        </TabsTrigger>
        <TabsTrigger className="h-8 justify-start" value="workspaces">
          Workspaces
        </TabsTrigger>
      </TabsList>
    </Tabs>
  );
}

export function WorkspacesScreen() {
  const operator = useOperator();
  const navigate = useNavigate();
  const [workspaces, setWorkspaces] = React.useState<Workspace[]>([]);
  const [secretNames, setSecretNames] = React.useState<string[]>([]);
  const [status, setStatus] = React.useState<"loading" | "error" | "ready">("loading");
  const [query, setQuery] = React.useState("");
  const [addOpen, setAddOpen] = React.useState(false);
  const [editTarget, setEditTarget] = React.useState<Workspace | null>(null);
  const [toDelete, setToDelete] = React.useState<Workspace | null>(null);
  const [tier, setTier] = React.useState<WorkspaceTier>("workspaces");

  const load = React.useCallback(() => {
    setStatus("loading");
    Promise.all([api.listWorkspaces(), secretsApi.listSecrets()])
      .then(([ws, secrets]) => {
        setWorkspaces(ws);
        setSecretNames(secrets);
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
        description="Three tiers: directories & repos configured once, shared base images, and workspaces — the aggregates runs attach. Run creation only ever offers what's here; a free-text host path is never accepted."
        actions={
          <>
            {!operator && <Chip tone="neutral">{OPERATOR_ONLY_REASON}</Chip>}
            {tier === "workspaces" && (
              <Button onClick={() => setAddOpen(true)} disabled={!operator}>
                <Plus className="size-4" /> Add workspace
              </Button>
            )}
          </>
        }
      />

      <div className="flex items-start gap-5">
        <TierTabRail value={tier} onChange={setTier} />
        <div className="min-w-0 flex-1">
          {tier === "sources" && <SourcesLibrary workspaces={workspaces} />}
          {tier === "images" && <ImageCatalog workspaces={workspaces} />}

          {tier === "workspaces" && (
            <>
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
          <TableSkeleton rows={5} cols={5} />
        ) : status === "error" ? (
          <ErrorState onRetry={load} />
        ) : workspaces.length === 0 ? (
          <EmptyState
            icon={FolderOpen}
            title="No workspaces onboarded yet."
            description={
              operator
                ? "Add a local directory, a repo, or scratch space so runs can attach it — a container image is a base image, added from the Base images tab. Wardyn scans it once (languages, package managers, egress) and reuses that profile for every run."
                : `Add a local directory, a repo, or scratch space so runs can attach it — a container image is a base image, added from the Base images tab. ${OPERATOR_ONLY_REASON}`
            }
            action={
              <Button onClick={() => setAddOpen(true)} disabled={!operator}>
                <Plus className="size-4" /> Onboard your first workspace
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
                <TableHead>Status</TableHead>
                <TableHead>Model access</TableHead>
                <TableHead>Needs you</TableHead>
                <TableHead className="w-[44px]" />
              </TableRow>
            </TableHeader>
            <TableBody>
              {filtered.map((w) => {
                const kindMeta = KIND_META[w.kind] ?? KIND_META.local_dir;
                const tone = statusTone(w.status);
                const attention = attentionItems(w, secretNames);
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
                        <span className="min-w-0">
                          <span className="block text-foreground">{w.name}</span>
                          <Mono className="block max-w-[260px] truncate text-[0.6875rem] text-muted-foreground" title={sourceSubLine(w)}>
                            {sourceSubLine(w)}
                          </Mono>
                        </span>
                      </span>
                    </TableCell>
                    <TableCell>
                      <Chip tone={tone.tone} dot pulse={tone.pulse}>
                        {statusWord(w.status)}
                      </Chip>
                    </TableCell>
                    <TableCell>
                      <span className="inline-flex items-center gap-1.5">
                        {/* Unbound is a REAL fallback, not an absence: the run still gets a
                            model, from the server's global provider (workspace-llm-cred.tsx's
                            own doc comment, and the picker's "None — use the server's global
                            provider" radio say the same thing). "None" alone read as "no model
                            access"; name the fallback and let the title spell it out. */}
                        {w.llm_cred?.integration_ref ? (
                          <Chip tone={llmCredTone(w.llm_cred)} mono>
                            {llmCredLabel(w.llm_cred)}
                          </Chip>
                        ) : (
                          <Chip tone="neutral" title="Runs fall back to the server's global provider.">
                            Not pinned
                          </Chip>
                        )}
                      </span>
                    </TableCell>
                    <TableCell>
                      {attention.length ? (
                        <span className="flex flex-col gap-0.5">
                          {attention.map((a, i) => (
                            <span
                              key={i}
                              className={cn(
                                "text-[0.6875rem] leading-snug",
                                a.tone === "danger"
                                  ? "text-danger"
                                  : a.tone === "warning"
                                    ? "text-warning"
                                    : "text-muted-foreground",
                              )}
                            >
                              {a.text}
                            </span>
                          ))}
                        </span>
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
                          <DropdownMenuItem onClick={() => setEditTarget(w)} disabled={!operator}>
                            Edit workspace…
                            {!operator && <OperatorOnlyHint />}
                          </DropdownMenuItem>
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
            </>
          )}
        </div>
      </div>

      {/* The wizard (Sources -> Base image -> Integrations -> Build -> Requirements
          -> Verify -> Done) is the ONE "Add workspace" front door AND, hydrated with
          `initial`, the only edit surface (the legacy AddWorkspaceDialog is retired —
          it rendered blank on a multi-source workspace and its save path collapsed
          sources[] back to one). Each is mounted only while open (its own Dialog is
          unconditionally open internally); a fresh `key` on the edit instance forces
          a remount — and a fresh hydration — if the operator edits a different row. */}
      {addOpen && (
        <WorkspaceWizard
          origin="library"
          onClose={() => setAddOpen(false)}
          onWorkspaceCreated={load}
          onOpenWorkspace={(id) => {
            setAddOpen(false);
            openDetail(id);
          }}
        />
      )}

      {editTarget && (
        <WorkspaceWizard
          key={editTarget.id}
          origin="library"
          initial={editTarget}
          onClose={() => {
            setEditTarget(null);
            load();
          }}
          onOpenWorkspace={(id) => {
            setEditTarget(null);
            openDetail(id);
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
