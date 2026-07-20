/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import * as React from "react";
import {
  Box,
  FolderGit2,
  FolderOpen,
  KeyRound,
  Loader2,
  MoreHorizontal,
  Plus,
  RotateCw,
  ScanSearch,
  Trash2,
} from "lucide-react";
import { toast } from "sonner";
import { workspaces as api } from "../../lib/api/workspaces";
import { getErrorMessage } from "../../lib/format";
import type {
  Workspace,
  WorkspaceKind,
  WorkspaceLLMCred,
} from "../../lib/types";
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
import { Chip } from "../wardyn/primitives";
import { EmptyState, ErrorState, TableSkeleton } from "../wardyn/states";
import { PageHeader } from "../wardyn/page-header";
import { WorkspaceNeedsPanel } from "./workspace-needs-panel";
import {
  LLMCredFields,
  WorkspaceLLMCredDialog,
  llmCredLabel,
  llmCredTone,
} from "./workspace-llm-cred";

// Exported so the setup wizard's Workspaces step renders the same status
// vocabulary — the two surfaces can't drift.
export const STATUS_TONE: Record<Workspace["status"], "success" | "warning" | "danger" | "info"> = {
  pending_scan: "warning",
  scanning: "info",
  scanned: "info",
  building: "info",
  build_error: "danger",
  verifying: "info",
  verify_failed: "danger",
  ready: "success",
  error: "danger",
};
export const STATUS_LABEL: Record<Workspace["status"], string> = {
  pending_scan: "Pending scan",
  scanning: "Scanning",
  scanned: "Scanned",
  building: "Building",
  build_error: "Build error",
  verifying: "Verifying",
  verify_failed: "Verify failed",
  ready: "Ready",
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

export function WorkspacesScreen() {
  const [workspaces, setWorkspaces] = React.useState<Workspace[]>([]);
  const [status, setStatus] = React.useState<"loading" | "error" | "ready">("loading");
  const [query, setQuery] = React.useState("");
  const [addOpen, setAddOpen] = React.useState(false);
  const [editTarget, setEditTarget] = React.useState<Workspace | null>(null);
  const [profileTarget, setProfileTarget] = React.useState<Workspace | null>(null);
  const [credTarget, setCredTarget] = React.useState<Workspace | null>(null);
  const [toDelete, setToDelete] = React.useState<Workspace | null>(null);
  // Rows with a scan in flight — scoped to workspace id so multiple scans (or a
  // scan alongside other list activity) never fight over one flag.
  const [scanning, setScanning] = React.useState<Set<string>>(new Set());

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

  const triggerScan = async (w: Workspace) => {
    setScanning((s) => new Set(s).add(w.id));
    try {
      const { async: isAsync } = await api.scanWorkspace(w.id);
      if (isAsync) {
        toast.info(`Scan started for "${w.name}"`, {
          description:
            "A governed scan run is analyzing the repo; the status updates when it completes (track it in Runs).",
        });
      } else {
        toast.success(`"${w.name}" scanned — ready`);
      }
    } catch (e) {
      // e.g. a local dir whose path doesn't exist on this host — the server persists
      // status=error and returns the reason (surfaced here, and shown on the row by
      // the finally re-read).
      toast.error(`Failed to scan "${w.name}"`, {
        description: getErrorMessage(e),
      });
    } finally {
      // Re-read for the authoritative status: the scan response is a profile (local)
      // or a scan-run stub (repo), never a Workspace, and a failed scan persists
      // status=error server-side — so trust the list, not the response body.
      api.listWorkspaces().then(setWorkspaces).catch(() => {});
      setScanning((s) => {
        const next = new Set(s);
        next.delete(w.id);
        return next;
      });
    }
  };

  return (
    <div className="mx-auto max-w-[1400px] px-6 py-6">
      <PageHeader
        title="Workspaces"
        description="Onboard the local directories and repos your runs may attach. Run-creation only ever offers sources onboarded here — a free-text host path is never accepted."
        actions={
          <Button onClick={() => setAddOpen(true)}>
            <Plus className="size-4" /> Add workspace
          </Button>
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

      <div className="overflow-hidden rounded-xl border border-border bg-card">
        {status === "loading" ? (
          <TableSkeleton rows={5} cols={5} />
        ) : status === "error" ? (
          <ErrorState onRetry={load} />
        ) : workspaces.length === 0 ? (
          <EmptyState
            icon={FolderOpen}
            title="No workspaces onboarded yet."
            description="Add a local directory or repo so runs can attach it. Wardyn scans it once (languages, package managers, egress) and reuses that profile for every run."
            action={
              <Button onClick={() => setAddOpen(true)}>
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
                <TableHead>Name</TableHead>
                <TableHead>Kind</TableHead>
                <TableHead>Source</TableHead>
                <TableHead>Status</TableHead>
                <TableHead>Model access</TableHead>
                <TableHead className="w-[44px]" />
              </TableRow>
            </TableHeader>
            <TableBody>
              {filtered.map((w) => {
                const kindMeta = KIND_META[w.kind] ?? KIND_META.local_dir;
                return (
                  <TableRow key={w.id}>
                    <TableCell>
                      <span className="inline-flex items-center gap-2">
                        <kindMeta.Icon className="size-3.5 text-cyan" />
                        <span className="text-foreground">{w.name}</span>
                      </span>
                    </TableCell>
                    <TableCell>
                      <Chip tone="neutral">{kindMeta.label}</Chip>
                    </TableCell>
                    <TableCell>
                      <Mono className="text-foreground" title={w.source}>
                        {w.source}
                      </Mono>
                      {w.ref && <span className="ml-1.5 text-xs text-muted-foreground">@{w.ref}</span>}
                    </TableCell>
                    <TableCell>
                      {scanning.has(w.id) ? (
                        <Chip tone="neutral" dot>
                          <Loader2 className="size-3 animate-spin" /> Scanning…
                        </Chip>
                      ) : (
                        <Chip tone={STATUS_TONE[w.status]} dot>
                          {STATUS_LABEL[w.status]}
                        </Chip>
                      )}
                    </TableCell>
                    <TableCell>
                      <Chip tone={llmCredTone(w.llm_cred?.mode)}>{llmCredLabel(w.llm_cred)}</Chip>
                    </TableCell>
                    <TableCell onClick={(e) => e.stopPropagation()}>
                      <DropdownMenu>
                        <DropdownMenuTrigger asChild>
                          <Button variant="ghost" size="icon" className="size-8" aria-label="Workspace actions">
                            <MoreHorizontal className="size-4" />
                          </Button>
                        </DropdownMenuTrigger>
                        <DropdownMenuContent align="end">
                          <DropdownMenuItem onClick={() => triggerScan(w)} disabled={scanning.has(w.id)}>
                            <ScanSearch className="size-4" /> Scan now
                          </DropdownMenuItem>
                          <DropdownMenuItem onClick={() => setEditTarget(w)}>Edit</DropdownMenuItem>
                          <DropdownMenuItem onClick={() => setCredTarget(w)}>
                            <KeyRound className="size-4" /> Model access
                          </DropdownMenuItem>
                          <DropdownMenuItem onClick={() => setProfileTarget(w)} disabled={!w.profile}>
                            View profile
                          </DropdownMenuItem>
                          <DropdownMenuItem
                            onClick={() => setToDelete(w)}
                            className="text-danger focus:text-danger"
                          >
                            <Trash2 className="size-4" /> Delete
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

      <AddWorkspaceDialog open={addOpen} onOpenChange={setAddOpen} onSaved={load} />

      <AddWorkspaceDialog
        open={!!editTarget}
        onOpenChange={(o) => !o && setEditTarget(null)}
        onSaved={() => {
          setEditTarget(null);
          load();
        }}
        initial={editTarget ?? undefined}
      />

      <WorkspaceLLMCredDialog
        workspace={credTarget}
        onOpenChange={(o) => !o && setCredTarget(null)}
        onSaved={(w) => {
          setCredTarget(null);
          setWorkspaces((list) => list.map((x) => (x.id === w.id ? w : x)));
        }}
      />

      <Dialog open={!!profileTarget} onOpenChange={(o) => !o && setProfileTarget(null)}>
        <DialogContent className="sm:max-w-lg">
          <DialogHeader>
            <DialogTitle>What this workspace needs — {profileTarget?.name}</DialogTitle>
            <DialogDescription>
              Detected by the scan from committed files — deterministic control-plane output, never
              agent-authored. Names and hosts only; values are never read.
            </DialogDescription>
          </DialogHeader>
          {profileTarget && (
            <WorkspaceNeedsPanel
              workspace={profileTarget}
              onWorkspaceUpdated={(w) => {
                setProfileTarget(w);
                setWorkspaces((list) => list.map((x) => (x.id === w.id ? w : x)));
              }}
            />
          )}
        </DialogContent>
      </Dialog>

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
// "Add workspace" inline without leaving the flow (mirrors AddSecretDialog).
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
  const [name, setName] = React.useState("");
  const [kind, setKind] = React.useState<WorkspaceKind>("local_dir");
  const [source, setSource] = React.useState("");
  const [ref, setRef] = React.useState("");
  const [defaultTarget, setDefaultTarget] = React.useState("");
  const [writable, setWritable] = React.useState(false);
  // Model/harness binding — create-only (the server ignores llm_cred on a
  // generic PUT); editing an existing workspace's binding goes through the
  // list's "Model access" action (WorkspaceLLMCredDialog) instead.
  const [llmCred, setLlmCred] = React.useState<WorkspaceLLMCred>({ mode: "" });
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
    setLlmCred({ mode: "" });
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
        llm_cred: !isEdit && llmCred.mode ? llmCred : undefined,
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
              opt-in an imported workspace can never be written, so Record/Verify
              cannot install deps, build, or let the agent edit a file — the whole
              point of the import flow. Granting it is real: changes land on the host. */}
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
                  disposable clone, not a working tree you care about.
                </p>
              )}
            </div>
          )}

          {/* Create-only: editing an existing workspace's binding uses the list's
              "Model access" action instead (the server ignores llm_cred on a
              generic PUT — see WorkspaceLLMCredDialog above). */}
          {!isEdit && <LLMCredFields value={llmCred} onChange={setLlmCred} />}

          {error && (
            <div className="rounded-lg border border-danger/30 bg-danger-subtle px-3 py-2 text-xs text-danger">
              {error}
            </div>
          )}
        </div>

        <DialogFooter>
          <Button variant="ghost" onClick={() => onOpenChange(false)}>
            Cancel
          </Button>
          <Button onClick={save} disabled={saving || !name.trim() || !source.trim()}>
            {saving && <Loader2 className="size-4 animate-spin" />}
            {isEdit ? "Save changes" : "Add workspace"}
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}
