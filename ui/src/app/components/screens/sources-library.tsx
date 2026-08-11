/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Tier 1 — the DIRECTORIES & REPOS library: a repo or directory configured
// ONCE (its own requirements contract, its own scan profile and status),
// attached to any number of workspaces. Adding an entry that already exists
// lands on the existing row (the server dedupes on canonical identity) —
// which is the reuse the library exists for. Deleting one that workspaces
// still attach is a loud refusal naming them, with an explicit
// detach-everywhere escape.
import * as React from "react";
import { FolderGit2, FolderOpen, Loader2, MoreHorizontal, Plus, RotateCw, Trash2 } from "lucide-react";
import { toast } from "sonner";
import { sourcesApi } from "../../lib/api/sources";
import { getErrorMessage } from "../../lib/format";
import { statusTone, statusWord } from "../../lib/workspace-status";
import { usePoll } from "../../lib/use-poll";
import type { Source, Workspace } from "../../lib/types";
import { Button } from "../ui/button";
import { Input } from "../ui/input";
import { Label } from "../ui/label";
import { RadioGroup, RadioGroupItem } from "../ui/radio-group";
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "../ui/table";
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "../ui/dialog";
import {
  DropdownMenu,
  DropdownMenuContent,
  DropdownMenuItem,
  DropdownMenuTrigger,
} from "../ui/dropdown-menu";
import { Mono } from "../wardyn/code-block";
import { Chip } from "../wardyn/primitives";
import { EmptyState, ErrorState, TableSkeleton } from "../wardyn/states";
import { OPERATOR_ONLY_REASON } from "../wardyn/copy";
import { useOperator } from "../wardyn/operator-context";
import { DeleteInUseDialog } from "../wardyn/delete-in-use-dialog";

// How many workspaces attach each source — counted from the workspaces the
// caller already fetched (attachments carry source ids), so the library needs
// no extra endpoint for it.
export function sourceUsage(workspaces: Workspace[]): Map<string, number> {
  const used = new Map<string, number>();
  for (const ws of workspaces) {
    for (const att of ws.attachments ?? []) {
      if (att.source_id) used.set(att.source_id, (used.get(att.source_id) ?? 0) + 1);
    }
  }
  return used;
}

// "2 required · 1 optional" — the source's OWN contract at a glance.
export function contractSummary(src: Source): string {
  const rows = Object.values(src.requirements ?? {});
  if (rows.length === 0) return "No contract yet";
  const required = rows.filter((r) => r.level === "required").length;
  const optional = rows.length - required;
  const parts: string[] = [];
  if (required) parts.push(`${required} required`);
  if (optional) parts.push(`${optional} optional`);
  return parts.join(" · ");
}

export function AddSourceDialog({
  open,
  onOpenChange,
  onSaved,
}: {
  open: boolean;
  onOpenChange: (o: boolean) => void;
  onSaved: (src: Source) => void;
}) {
  const operator = useOperator();
  const [kind, setKind] = React.useState<"local_dir" | "repo">("local_dir");
  const [locator, setLocator] = React.useState("");
  const [ref, setRef] = React.useState("");
  const [name, setName] = React.useState("");
  const [saving, setSaving] = React.useState(false);
  const [error, setError] = React.useState<string | null>(null);

  React.useEffect(() => {
    if (!open) return;
    setKind("local_dir");
    setLocator("");
    setRef("");
    setName("");
    setSaving(false);
    setError(null);
  }, [open]);

  const save = async () => {
    setSaving(true);
    setError(null);
    try {
      const src = await sourcesApi.createSource({
        kind,
        locator: locator.trim(),
        ref: kind === "repo" ? ref.trim() || undefined : undefined,
        name: name.trim() || undefined,
      });
      onSaved(src);
      onOpenChange(false);
      toast.success(`"${src.name}" is in your library`);
    } catch (e) {
      setError(getErrorMessage(e));
    } finally {
      setSaving(false);
    }
  };

  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent className="sm:max-w-lg">
        <DialogHeader>
          <DialogTitle>Add to your library</DialogTitle>
          <DialogDescription>
            Configured once, attached to any number of workspaces. Re-adding something already in the
            library lands on the existing entry — contract and all.
          </DialogDescription>
        </DialogHeader>
        <div className="space-y-3">
          <RadioGroup
            value={kind}
            onValueChange={(v) => setKind(v as "local_dir" | "repo")}
            className="flex gap-4"
          >
            <label className="flex items-center gap-1.5 text-xs">
              <RadioGroupItem value="local_dir" id="src-kind-dir" />
              <Label htmlFor="src-kind-dir" className="cursor-pointer font-normal">
                Local directory
              </Label>
            </label>
            <label className="flex items-center gap-1.5 text-xs">
              <RadioGroupItem value="repo" id="src-kind-repo" />
              <Label htmlFor="src-kind-repo" className="cursor-pointer font-normal">
                Repository
              </Label>
            </label>
          </RadioGroup>
          <div className="space-y-1.5">
            <Label htmlFor="src-locator">{kind === "local_dir" ? "Host path" : "Repo slug or clone URL"}</Label>
            <Input
              id="src-locator"
              value={locator}
              onChange={(e) => setLocator(e.target.value)}
              placeholder={kind === "local_dir" ? "/home/me/projects/payments" : "acme/payments"}
              className="font-mono"
              autoComplete="off"
            />
          </div>
          {kind === "repo" && (
            <div className="space-y-1.5">
              <Label htmlFor="src-ref">Git ref (optional)</Label>
              <Input
                id="src-ref"
                value={ref}
                onChange={(e) => setRef(e.target.value)}
                placeholder="main"
                className="font-mono"
                autoComplete="off"
              />
              <p className="text-[0.6875rem] leading-snug text-muted-foreground">
                Part of the identity — the same repo at two refs is two library entries with two
                contracts.
              </p>
            </div>
          )}
          <div className="space-y-1.5">
            <Label htmlFor="src-name">Name (optional)</Label>
            <Input
              id="src-name"
              value={name}
              onChange={(e) => setName(e.target.value)}
              placeholder="payments"
              autoComplete="off"
            />
          </div>
          {error && <p className="text-xs font-medium text-danger">{error}</p>}
          {!operator && <p className="text-xs font-medium text-warning">{OPERATOR_ONLY_REASON}</p>}
        </div>
        <DialogFooter>
          <Button variant="ghost" onClick={() => onOpenChange(false)}>
            Cancel
          </Button>
          <Button onClick={save} disabled={!operator || saving || !locator.trim()}>
            {saving && <Loader2 className="size-4 animate-spin" />}
            Add to library
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}

export function SourcesLibrary({
  workspaces,
  embedded = false,
}: {
  /** The already-fetched workspaces list — used-by counts derive from attachments. */
  workspaces: Workspace[];
  /** Compact paddings for the Getting-started embed. */
  embedded?: boolean;
}) {
  const operator = useOperator();
  const [sources, setSources] = React.useState<Source[]>([]);
  const [status, setStatus] = React.useState<"loading" | "error" | "ready">("loading");
  const [addOpen, setAddOpen] = React.useState(false);
  const [toDelete, setToDelete] = React.useState<Source | null>(null);
  const [scanning, setScanning] = React.useState<string | null>(null);

  const load = React.useCallback((quiet = false) => {
    if (!quiet) setStatus("loading");
    sourcesApi
      .listSources()
      .then((rows) => {
        setSources(rows);
        setStatus("ready");
      })
      .catch(() => {
        if (!quiet) setStatus("error");
      });
  }, []);
  React.useEffect(() => load(), [load]);

  // A repo's scan is a governed run (202) — poll quietly until no row is mid-
  // scan, so "Setting up" settles into Usable / Scan failed on its own.
  usePoll(() => load(true), 4000, !sources.some((s) => s.status === "scanning"));

  const usage = sourceUsage(workspaces);

  const scan = async (src: Source) => {
    setScanning(src.id);
    try {
      const res = (await sourcesApi.scanSource(src.id)) as { scan_run_id?: string } | null;
      if (res && typeof res === "object" && res.scan_run_id) {
        toast.info(`Scan started for "${src.name}"`, {
          description: "A governed run is analyzing the repo; the row updates when it completes.",
        });
      }
      load(true);
    } catch (e) {
      toast.error(`Scan failed for "${src.name}"`, { description: getErrorMessage(e) });
      load(true); // the row's status=error chip is the durable signal
    } finally {
      setScanning(null);
    }
  };

  return (
    <div className={embedded ? "space-y-3" : "space-y-4"}>
      <div className="flex items-center justify-between gap-3">
        <p className="text-xs leading-snug text-muted-foreground">
          A repo or directory configured once — its own requirements and scan — attached to any
          number of workspaces.
        </p>
        <Button size={embedded ? "sm" : "default"} onClick={() => setAddOpen(true)} disabled={!operator}>
          <Plus className="size-4" /> Add directory or repo
        </Button>
      </div>

      {status === "loading" && <TableSkeleton rows={3} />}
      {status === "error" && <ErrorState onRetry={() => load()} />}
      {status === "ready" && sources.length === 0 && (
        <EmptyState
          icon={FolderOpen}
          title="Nothing in your library yet"
          description="Add a directory or repo once; every workspace that needs it attaches the same entry."
        />
      )}
      {status === "ready" && sources.length > 0 && (
        <div className="overflow-x-auto rounded-xl border border-border">
          <Table>
            <TableHeader>
              <TableRow>
                <TableHead>Name</TableHead>
                <TableHead>Where</TableHead>
                <TableHead>Status</TableHead>
                <TableHead>Contract</TableHead>
                <TableHead>Used by</TableHead>
                <TableHead className="w-[44px]" />
              </TableRow>
            </TableHeader>
            <TableBody>
              {sources.map((src) => {
                const tone = statusTone(src.status);
                const usedBy = usage.get(src.id) ?? 0;
                const KindIcon = src.kind === "repo" ? FolderGit2 : FolderOpen;
                return (
                  <TableRow key={src.id}>
                    <TableCell>
                      <span className="inline-flex items-center gap-2.5">
                        <span className="inline-flex size-7 shrink-0 items-center justify-center rounded-md border border-border text-muted-foreground">
                          <KindIcon className="size-3.5" />
                        </span>
                        <span className="text-sm font-medium text-foreground">{src.name}</span>
                      </span>
                    </TableCell>
                    <TableCell>
                      <Mono className="text-xs">
                        {src.kind === "repo" ? "repo" : "dir"} · {src.locator}
                        {src.ref ? ` @${src.ref}` : ""}
                      </Mono>
                    </TableCell>
                    <TableCell>
                      <Chip tone={tone.tone} dot pulse={tone.pulse}>
                        {statusWord(src.status)}
                      </Chip>
                    </TableCell>
                    <TableCell>
                      <span className="text-xs text-muted-foreground">{contractSummary(src)}</span>
                    </TableCell>
                    <TableCell>
                      <span className="text-xs text-muted-foreground">
                        {usedBy === 0 ? "No workspaces" : `${usedBy} workspace${usedBy === 1 ? "" : "s"}`}
                      </span>
                    </TableCell>
                    <TableCell>
                      <DropdownMenu>
                        <DropdownMenuTrigger asChild>
                          <Button variant="ghost" size="icon" className="size-8" aria-label={`Actions for ${src.name}`}>
                            {scanning === src.id ? (
                              <Loader2 className="size-4 animate-spin" />
                            ) : (
                              <MoreHorizontal className="size-4" />
                            )}
                          </Button>
                        </DropdownMenuTrigger>
                        <DropdownMenuContent align="end">
                          <DropdownMenuItem disabled={!operator || scanning !== null} onClick={() => void scan(src)}>
                            <RotateCw className="size-3.5" /> Scan
                          </DropdownMenuItem>
                          <DropdownMenuItem
                            disabled={!operator}
                            onClick={() => setToDelete(src)}
                            className="text-danger focus:text-danger"
                          >
                            <Trash2 className="size-3.5" /> Delete
                          </DropdownMenuItem>
                        </DropdownMenuContent>
                      </DropdownMenu>
                    </TableCell>
                  </TableRow>
                );
              })}
            </TableBody>
          </Table>
        </div>
      )}

      <AddSourceDialog
        open={addOpen}
        onOpenChange={setAddOpen}
        onSaved={(src) => {
          load(true);
          void scan(src);
        }}
      />
      <DeleteInUseDialog
        target={toDelete}
        onOpenChange={(o) => !o && setToDelete(null)}
        onDeleted={load}
        description="Removes the library entry and its contract. Workspaces attaching it keep their own overlays; the shared configuration is what goes."
        inUseHint="Detaching un-mounts this source from those workspaces — their next runs fail loudly at the mount gate instead of silently losing code."
        onDelete={(src, force) => sourcesApi.deleteSource(src.id, force)}
        removedFrom="the library"
        errorNoun="source"
      />
    </div>
  );
}
