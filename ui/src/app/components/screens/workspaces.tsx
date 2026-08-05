/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import * as React from "react";
import { isFixtureLeak } from "./workspace-wizard/wizard-types";
import { useNavigate } from "react-router-dom";
import {
  Box,
  FolderGit2,
  FolderOpen,
  Loader2,
  MoreHorizontal,
  Plus,
  RotateCw,
  Trash2,
} from "lucide-react";
import { workspaces as api } from "../../lib/api/workspaces";
import { secrets as secretsApi } from "../../lib/api/secrets";
import { LIST_LIMIT } from "../../lib/api/core";
import { getErrorMessage } from "../../lib/format";
import { statusTone, statusWord } from "../../lib/workspace-status";
import { compositionSummary, unstoredRequiredSecrets, workspaceRequirements } from "./new-run/wizard-types";
import type {
  Workspace,
  WorkspaceKind,
  WorkspaceLLMCred,
  WorkspaceProfile,
} from "../../lib/types";
import { cn } from "../ui/utils";
import { Button } from "../ui/button";
import { Input } from "../ui/input";
import { Label } from "../ui/label";
import { RadioGroup, RadioGroupItem } from "../ui/radio-group";
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
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "../ui/dialog";
import { Mono } from "../wardyn/code-block";
import { DeleteConfirmDialog } from "../wardyn/delete-confirm-dialog";
import { Chip, OperatorOnlyHint } from "../wardyn/primitives";
import { EmptyState, ErrorState, TableSkeleton, TruncatedNote } from "../wardyn/states";
import { PageHeader } from "../wardyn/page-header";
import { WorkspaceWizard } from "./workspace-wizard/wizard";
import { llmCredLabel, llmCredTone, LLMCredFields } from "./workspace-llm-cred";
import { OPERATOR_ONLY_REASON } from "../wardyn/copy";
import { useOperator } from "../wardyn/operator-context";

// Exported so the setup wizard's Workspaces step renders the same status
// vocabulary — the two surfaces can't drift.
export const STATUS_TONE: Record<Workspace["status"], "success" | "warning" | "danger" | "info"> = {
  pending_scan: "warning",
  scanning: "info",
  scanned: "info",
  error: "danger",
};
export const STATUS_LABEL: Record<Workspace["status"], string> = {
  pending_scan: "Pending scan",
  scanning: "Scanning",
  scanned: "Scanned",
  error: "Error",
};

// Icon + label for the three onboardable kinds. "container" has no host mount
// (source is an image ref), so it gets its own icon rather than reusing the
// local-dir folder.
export const KIND_META: Record<WorkspaceKind, { Icon: React.ElementType; label: string }> = {
  local_dir: { Icon: FolderOpen, label: "local dir" },
  repo: { Icon: FolderGit2, label: "repo" },
  container: { Icon: Box, label: "container" },
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
        description="Add the directories, repos and images your runs may attach. Run creation only ever offers what's added here — a free-text host path is never accepted."
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
          <TableSkeleton rows={5} cols={5} />
        ) : status === "error" ? (
          <ErrorState onRetry={load} />
        ) : workspaces.length === 0 ? (
          <EmptyState
            icon={FolderOpen}
            title="No workspaces onboarded yet."
            description={
              operator
                ? "Add a local directory, repo, or container image so runs can attach it. Wardyn scans it once (languages, package managers, egress) and reuses that profile for every run."
                : `Add a local directory, repo, or container image so runs can attach it. ${OPERATOR_ONLY_REASON}`
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
                  <TableRow key={w.id} className="cursor-pointer" onClick={() => openDetail(w.id)}>
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
                        <Chip tone={llmCredTone(w.llm_cred)} mono={!!w.llm_cred?.integration_ref}>
                          {llmCredLabel(w.llm_cred)}
                        </Chip>
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
                    <TableCell onClick={(e) => e.stopPropagation()}>
                      <DropdownMenu>
                        <DropdownMenuTrigger asChild>
                          <Button variant="ghost" size="icon" className="size-8" aria-label="Workspace actions">
                            <MoreHorizontal className="size-4" />
                          </Button>
                        </DropdownMenuTrigger>
                        <DropdownMenuContent align="end">
                          <DropdownMenuItem onClick={() => openDetail(w.id)}>Open</DropdownMenuItem>
                          <DropdownMenuItem onClick={() => setEditTarget(w)} disabled={!operator}>
                            Edit source…
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

      {/* The new four-step wizard (Sources -> Base image -> Requirements -> Done)
          replaces the old ImportWorkspaceDialog as the ONE "Add workspace" front
          door. Mounted only while open (its own Dialog is unconditionally open
          internally). */}
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

      <AddWorkspaceDialog
        open={!!editTarget}
        onOpenChange={(o) => !o && setEditTarget(null)}
        onSaved={() => {
          setEditTarget(null);
          load();
        }}
        initial={editTarget ?? undefined}
      />

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

// Add/Edit dialog for a single onboarded workspace. `initial` set => edit mode
// (PUT), otherwise create (POST). Exported so the New Run wizard can offer
// "Add workspace" inline without leaving the flow (mirrors AddSecretDialog),
// and so this screen's + the detail page's "Edit source…" kebab item can
// reuse the SAME edit form the wizard doesn't replace (the new wizard's
// multi-source composition has no update path yet — see workspace-wizard/
// wizard.tsx's continueFromSources comment).
export function AddWorkspaceDialog({
  open,
  onOpenChange,
  onSaved,
  initial,
}: {
  open: boolean;
  onOpenChange: (o: boolean) => void;
  onSaved?: (workspace: Workspace) => void;
  initial?: Workspace;
}) {
  const isEdit = !!initial;
  // Reused by the New Run wizard's inline "Add workspace" too — gating Save
  // here is the one chokepoint for every embedding, mirroring AddSecretDialog.
  const operator = useOperator();
  const [name, setName] = React.useState("");
  const [kind, setKind] = React.useState<WorkspaceKind>("local_dir");
  const [source, setSource] = React.useState("");
  const [ref, setRef] = React.useState("");
  const [defaultTarget, setDefaultTarget] = React.useState("");
  const [writable, setWritable] = React.useState(false);
  // Model/harness binding — create-only (the server ignores llm_cred on a
  // generic PUT); editing an existing workspace's binding goes through the
  // detail page's Requirements card (WorkspaceLLMCredDialog) instead.
  const [llmCred, setLlmCred] = React.useState<WorkspaceLLMCred>({});
  const [error, setError] = React.useState<string | null>(null);
  const [saving, setSaving] = React.useState(false);

  React.useEffect(() => {
    if (!open) return;
    setName(initial?.name ?? "");
    setKind(initial?.kind ?? "local_dir");
    setSource(initial?.source ?? "");
    setRef(initial?.ref ?? "");
    setDefaultTarget(initial?.default_target ?? "");
    setWritable(initial?.writable ?? false);
    setLlmCred({});
    setError(null);
    setSaving(false);
  }, [open, initial]);

  const save = async () => {
    setError(null);
    const n = name.trim();
    const s = source.trim();
    if (!n) {
      setError("Name is required.");
      return;
    }
    if (!s) {
      setError(
        kind === "repo" ? "Repo is required." : kind === "container" ? "Image ref is required." : "Directory path is required.",
      );
      return;
    }
    if (kind === "local_dir" && !s.startsWith("/")) {
      setError("Local directory path must be absolute (start with /).");
      return;
    }
    setSaving(true);
    try {
      const input = {
        name: n,
        kind,
        source: s,
        ref: kind === "repo" && ref.trim() ? ref.trim() : undefined,
        default_target: kind !== "container" && defaultTarget.trim() ? defaultTarget.trim() : undefined,
        writable: writable || undefined,
        llm_cred: !isEdit && llmCred.integration_ref ? llmCred : undefined,
      };
      const saved = isEdit ? await api.updateWorkspace(initial!.id, input) : await api.createWorkspace(input);
      onOpenChange(false);
      onSaved?.(saved);
    } catch (e) {
      setError(getErrorMessage(e) || "Failed to save workspace.");
    } finally {
      setSaving(false);
    }
  };

  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent className="sm:max-w-lg">
        <DialogHeader>
          <DialogTitle>{isEdit ? "Edit workspace" : "Add workspace"}</DialogTitle>
          <DialogDescription>
            {isEdit
              ? "Re-scan after changing the source so the detected profile stays accurate."
              : "Onboarding scans the source once (deterministic detection, AI fallback only when needed) and reuses that profile for every run."}
          </DialogDescription>
        </DialogHeader>

        <div className="space-y-4 py-1">
          <div className="space-y-2">
            <Label htmlFor="ws-name">Name</Label>
            <Input
              id="ws-name"
              placeholder="payments-service"
              value={name}
              onChange={(e) => setName(e.target.value)}
              autoComplete="off"
            />
          </div>

          <div className="space-y-2">
            <Label>Kind</Label>
            <RadioGroup value={kind} onValueChange={(v) => setKind(v as WorkspaceKind)} className="gap-2">
              <label className="flex items-center gap-2.5 rounded-lg border border-border p-2.5">
                <RadioGroupItem value="local_dir" id="ws-kind-local" disabled={isEdit} />
                <Label htmlFor="ws-kind-local" className="cursor-pointer font-normal">
                  Local directory
                </Label>
              </label>
              <label className="flex items-center gap-2.5 rounded-lg border border-border p-2.5">
                <RadioGroupItem value="repo" id="ws-kind-repo" disabled={isEdit} />
                <Label htmlFor="ws-kind-repo" className="cursor-pointer font-normal">
                  Repo
                </Label>
              </label>
              <label className="flex items-center gap-2.5 rounded-lg border border-border p-2.5">
                <RadioGroupItem value="container" id="ws-kind-container" disabled={isEdit} />
                <Label htmlFor="ws-kind-container" className="cursor-pointer font-normal">
                  Container image
                </Label>
              </label>
            </RadioGroup>
          </div>

          <div className="space-y-2">
            <Label htmlFor="ws-source">
              {kind === "repo" ? "Repo" : kind === "container" ? "Image ref" : "Directory path"}
            </Label>
            <Input
              id="ws-source"
              placeholder={
                kind === "repo"
                  ? "acme/payments-service"
                  : kind === "container"
                    ? "ubuntu:24.04"
                    : "/home/me/projects/payments"
              }
              value={source}
              onChange={(e) => setSource(e.target.value)}
              className="font-mono"
              autoComplete="off"
            />
            {kind === "repo" && (
              <p className="text-[0.6875rem] leading-snug text-muted-foreground">
                A private repo clones through the credential broker — it needs a{" "}
                <code className="font-mono">git-pat-&lt;host&gt;</code> (HTTPS) or{" "}
                <code className="font-mono">ssh-key-&lt;host&gt;</code> (SSH) secret, added under
                Secrets. Onboarding succeeds without one, but the clone will fail later.
              </p>
            )}
            {kind === "container" && (
              <p className="text-[0.6875rem] leading-snug text-muted-foreground">
                A tag or digest — pulled as the sandbox's base image. No host mount: nothing here needs
                a path.
              </p>
            )}
          </div>

          {kind === "repo" && (
            <div className="space-y-2">
              <Label htmlFor="ws-ref">Ref (optional)</Label>
              <Input
                id="ws-ref"
                placeholder="main"
                value={ref}
                onChange={(e) => setRef(e.target.value)}
                className="font-mono"
                autoComplete="off"
              />
            </div>
          )}

          {kind !== "container" && (
            <div className="space-y-2">
              <Label htmlFor="ws-target">Default target (optional)</Label>
              <Input
                id="ws-target"
                placeholder={kind === "repo" ? "~/work/payments-service" : "/home/agent/work"}
                value={defaultTarget}
                onChange={(e) => setDefaultTarget(e.target.value)}
                className="font-mono"
                autoComplete="off"
              />
              <p className="text-[0.6875rem] leading-snug text-muted-foreground">
                Where this attaches in the sandbox by default. A run may override it per attachment.
              </p>
            </div>
          )}

          {/* Read-only is the safe default (WorkspaceMount.ReadOnly). Without this
              opt-in an imported workspace can never be written, so an agent
              cannot install deps, build, or edit a file — the whole point of
              onboarding it. Granting it is real: changes land on the host. */}
          {kind === "local_dir" && (
            <div className="space-y-2 rounded-lg border border-border p-3">
              <label className="flex items-start gap-2.5 text-xs">
                <input
                  type="checkbox"
                  id="ws-writable"
                  checked={writable}
                  onChange={(e) => setWritable(e.target.checked)}
                  className="mt-0.5 size-3.5 shrink-0 accent-primary"
                />
                <span>
                  <span className="font-medium text-foreground">
                    Let agents write to this directory
                  </span>
                  <span className="block text-[0.6875rem] leading-snug text-muted-foreground">
                    Required to install dependencies, build, or have an agent change code. Leave
                    unticked and the workspace mounts read-only — <span className="font-mono">install</span>{" "}
                    and <span className="font-mono">build</span> steps will fail.
                  </span>
                </span>
              </label>
              {writable && (
                <p className="rounded-md bg-warning-subtle px-2 py-1.5 text-[0.6875rem] leading-snug text-warning">
                  The agent&apos;s changes persist to the host directory{" "}
                  <span className="font-mono">{source.trim() || "…"}</span>. Point this at a
                  disposable clone, not a working tree you care about. If your Docker runtime
                  runs inside a VM (Rancher Desktop / Colima / Docker Desktop on macOS), the
                  stronger barriers may still deny writes here regardless of host permissions —
                  see docs/adoption.
                </p>
              )}
            </div>
          )}

          {/* Create-only: editing an existing workspace's binding uses the detail
              page's Requirements card instead (the server ignores llm_cred on a
              generic PUT — see WorkspaceLLMCredDialog there). */}
          {!isEdit && <LLMCredFields value={llmCred} onChange={setLlmCred} />}

          {error && (
            <div className="rounded-lg border border-danger/30 bg-danger-subtle px-3 py-2 text-xs text-danger">
              {error}
            </div>
          )}
          {!operator && (
            <p id="add-workspace-operator-reason" className="text-xs font-medium text-warning">
              {OPERATOR_ONLY_REASON}
            </p>
          )}
        </div>

        <DialogFooter>
          <Button variant="ghost" onClick={() => onOpenChange(false)}>
            Cancel
          </Button>
          <Button
            onClick={save}
            disabled={!operator || saving || !name.trim() || !source.trim()}
            aria-describedby={operator ? undefined : "add-workspace-operator-reason"}
          >
            {saving && <Loader2 className="size-4 animate-spin" />}
            {isEdit ? "Save changes" : "Add workspace"}
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}
