/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import * as React from "react";
import { Lock, Plus, MoreHorizontal, Trash2, RotateCw, Loader2, KeyRound, AlertTriangle } from "lucide-react";
import { secrets as secretsApi } from "../../lib/api/secrets";
import { composer as composerApi } from "../../lib/api/compose";
import { getErrorMessage } from "../../lib/format";
import type { ComposerBackend } from "../../lib/types";
import { Button } from "../ui/button";
import { Input } from "../ui/input";
import { Label } from "../ui/label";
import { Textarea } from "../ui/textarea";
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
import { Chip, SectionLabel } from "../wardyn/primitives";
import { StatusChip } from "../wardyn/status-chip";
import { EmptyState, ErrorState, TableSkeleton } from "../wardyn/states";
import { PageHeader } from "../wardyn/page-header";
import { DeleteConfirmDialog } from "../wardyn/delete-confirm-dialog";
import { CAPABILITY } from "../wardyn/copy";

// Secret names are constrained server-side to a safe identifier set; mirror that
// here so we reject obviously-bad names before the round-trip.
const SECRET_NAME_RE = /^[a-z0-9]([a-z0-9._-]{0,126}[a-z0-9])?$/;

// Prebuilt secret-name suggestions offered on a BLANK-name open of AddSecretDialog
// (never on a prefilled/rotate open — the operator already knows the name then).
// Chips only prefill the Name field, never a value — the credential itself is
// always typed/pasted by the operator.
const PROVIDER_NAME_CHIPS = [
  "anthropic-api-key",
  "openai-api-key",
  "github-pat",
  "gitlab-pat",
  "kubeconfig",
  "npm-token",
  "pypi-token",
  "docker-registry",
] as const;

// Write-only chip tooltip — the one honest sentence for what "write-only" means
// here (values are never read back, not even by the operator). Verbatim, so it
// can't drift between call sites.
const WRITE_ONLY_TOOLTIP =
  "Write-only: the value can be replaced or removed, but never read back — not even by you.";

// Rungs 2/3 of the SCM safest-path ladder (ScmProviderStep) name their secrets
// with these prefixes; both are STANDING resident credentials auto-used by every
// future clone to that host, unlike a GitHub App's per-run brokered token.
const STANDING_NAME_RE = /^(ssh-key-|git-pat-)/;
const STANDING_TOOLTIP =
  "Auto-used by every future clone to this host with no per-run prompt — delete to revoke.";

export function SecretsScreen() {
  const [names, setNames] = React.useState<string[]>([]);
  const [status, setStatus] = React.useState<"loading" | "error" | "ready">("loading");
  const [query, setQuery] = React.useState("");
  const [addOpen, setAddOpen] = React.useState(false);
  const [rotateName, setRotateName] = React.useState<string | null>(null);
  const [toDelete, setToDelete] = React.useState<string | null>(null);

  const load = React.useCallback(() => {
    setStatus("loading");
    secretsApi
      .listSecrets()
      .then((n) => {
        setNames(n);
        setStatus("ready");
      })
      .catch(() => setStatus("error"));
  }, []);
  React.useEffect(load, [load]);

  // AI Run Composer backends (D13) — a secondary, advisory-only section: which
  // composer backend(s) the daemon has configured at boot. GET /composer/backends
  // only ever lists backends whose registry build already succeeded (a backend
  // whose key failed to resolve fails wardynd's boot entirely — see
  // buildComposerRegistry in cmd/wardynd), so anything returned here is genuinely
  // ready, never a hopeful "configured but maybe broken" state.
  const [backends, setBackends] = React.useState<ComposerBackend[]>([]);
  const [backendsStatus, setBackendsStatus] = React.useState<"loading" | "ready">("loading");
  React.useEffect(() => {
    let cancelled = false;
    Promise.resolve()
      .then(() => composerApi.listComposerBackends())
      // a request failure folds into the same "no backends" bucket as a
      // genuinely empty list — good enough for this advisory-only section; split
      // out a distinct error state if that ever proves confusing.
      .catch(() => [] as ComposerBackend[])
      .then((b) => {
        if (!cancelled) {
          setBackends(b);
          setBackendsStatus("ready");
        }
      });
    return () => {
      cancelled = true;
    };
  }, []);

  const filtered = names.filter((n) => !query || n.toLowerCase().includes(query.toLowerCase()));

  return (
    <div className="mx-auto max-w-[1400px] px-6 py-6">
      <PageHeader
        title="Secrets"
        description={`Write-only: values go in and never come out. ${CAPABILITY.brokerLine} Exception: ${CAPABILITY.gitPatLine}`}
        actions={
          <Button onClick={() => setAddOpen(true)}>
            <Plus className="size-4" /> Add secret
          </Button>
        }
      />

      {status === "ready" && names.length > 0 && (
        <div className="mb-4 flex items-center gap-3">
          <Input
            placeholder="Search secrets by name…"
            value={query}
            onChange={(e) => setQuery(e.target.value)}
            className="max-w-sm"
          />
          <span className="ml-auto text-sm text-muted-foreground">
            {filtered.length} of {names.length} secret{names.length === 1 ? "" : "s"}
          </span>
          <Button variant="outline" size="icon" onClick={load} aria-label="Refresh">
            <RotateCw className="size-4" />
          </Button>
        </div>
      )}

      <div className="overflow-hidden rounded-xl border border-border bg-card">
        {status === "loading" ? (
          <TableSkeleton rows={5} cols={2} />
        ) : status === "error" ? (
          <ErrorState onRetry={load} />
        ) : names.length === 0 ? (
          <EmptyState
            icon={KeyRound}
            title="No secrets yet."
            description="Add an API key or access token so runs can reference it by name — the value is stored write-only and is never returned, not even to you."
            action={
              <Button onClick={() => setAddOpen(true)}>
                <Plus className="size-4" /> Add your first LLM key
              </Button>
            }
          />
        ) : filtered.length === 0 ? (
          <EmptyState
            icon={KeyRound}
            title="No secrets match that search."
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
                <TableHead className="w-[44px]" />
              </TableRow>
            </TableHeader>
            <TableBody>
              {filtered.map((name) => (
                <TableRow key={name}>
                  <TableCell>
                    <div className="flex items-center justify-between gap-2">
                      <span className="inline-flex items-center gap-2">
                        <KeyRound className="size-3.5 text-cyan" />
                        <Mono className="text-foreground">{name}</Mono>
                      </span>
                      <span className="flex items-center gap-1.5">
                        {STANDING_NAME_RE.test(name) && (
                          <Chip tone="warning" title={STANDING_TOOLTIP}>
                            Standing
                          </Chip>
                        )}
                        <Chip tone="cyan" className="gap-1" title={WRITE_ONLY_TOOLTIP}>
                          <Lock className="size-3" /> write-only
                        </Chip>
                      </span>
                    </div>
                  </TableCell>
                  <TableCell onClick={(e) => e.stopPropagation()}>
                    <DropdownMenu>
                      <DropdownMenuTrigger asChild>
                        <Button variant="ghost" size="icon" className="size-8" aria-label="Secret actions">
                          <MoreHorizontal className="size-4" />
                        </Button>
                      </DropdownMenuTrigger>
                      <DropdownMenuContent align="end">
                        <DropdownMenuItem onClick={() => setRotateName(name)}>
                          <RotateCw className="size-4" /> Rotate
                        </DropdownMenuItem>
                        <DropdownMenuItem
                          onClick={() => setToDelete(name)}
                          className="text-danger focus:text-danger"
                        >
                          <Trash2 className="size-4" /> Delete
                        </DropdownMenuItem>
                      </DropdownMenuContent>
                    </DropdownMenu>
                  </TableCell>
                </TableRow>
              ))}
            </TableBody>
          </Table>
        )}
      </div>

      <div className="mt-8 space-y-2">
        <SectionLabel>AI Run Composer backends</SectionLabel>
        {backendsStatus === "loading" ? (
          <div className="rounded-xl border border-border bg-card p-4">
            <StatusChip status="checking" />
          </div>
        ) : backends.length > 0 ? (
          <div className="space-y-2">
            {backends.map((b) => (
              <div
                key={b.name}
                className="flex items-start justify-between gap-4 rounded-xl border border-border bg-card p-4"
              >
                <div className="space-y-1.5">
                  <div className="flex items-center gap-2">
                    <Mono className="text-foreground">{b.name}</Mono>
                    <span className="text-xs text-muted-foreground">
                      {b.provider}/{b.model}
                    </span>
                    {b.is_default && <Chip tone="neutral">default</Chip>}
                  </div>
                  <p className="max-w-xl text-xs text-muted-foreground">
                    Turns a plain-language task into a confined run setup for you to review.
                    Advisory only — it never creates a run or mints a credential.
                  </p>
                </div>
                <StatusChip status="ready" />
              </div>
            ))}
          </div>
        ) : (
          <div className="space-y-2 rounded-xl border border-border bg-card p-4">
            <div className="flex items-center justify-between gap-4">
              <p className="max-w-xl text-sm text-foreground">
                Turns a plain-language task into a confined run setup for you to review. Advisory
                only — it never creates a run or mints a credential.
              </p>
              <StatusChip status="needs-setup" />
            </div>
            <p className="text-xs text-muted-foreground">
              Optional. Set <Mono className="text-foreground">WARDYN_COMPOSER_CONFIG</Mono> (or the{" "}
              <Mono className="text-foreground">-composer-config</Mono> flag) to enable it.
            </p>
          </div>
        )}
      </div>

      <AddSecretDialog open={addOpen} onOpenChange={setAddOpen} onSaved={load} existingNames={names} />

      <AddSecretDialog
        open={!!rotateName}
        onOpenChange={(o) => !o && setRotateName(null)}
        onSaved={() => {
          setRotateName(null);
          load();
        }}
        existingNames={names}
        initialName={rotateName ?? ""}
      />

      <DeleteConfirmDialog
        name={toDelete}
        entity="secret"
        description="Runs that reference this secret by name will no longer be able to resolve it. This cannot be undone."
        onOpenChange={(o) => !o && setToDelete(null)}
        onDelete={() => secretsApi.deleteSecret(toDelete!)}
        onDeleted={() => {
          setToDelete(null);
          load();
        }}
      />
    </div>
  );
}

// A write-only Add-secret dialog: name Input + value Textarea. The value field is
// cleared after submit and its content is NEVER echoed back anywhere. Exported so
// the New Run wizard can offer "Add secret" inline, and so this screen can reuse
// it (with initialName prefilled) as the "Rotate" action on an existing secret.
export function AddSecretDialog({
  open,
  onOpenChange,
  onSaved,
  existingNames = [],
  initialName = "",
}: {
  open: boolean;
  onOpenChange: (o: boolean) => void;
  onSaved?: (name: string) => void;
  // MEDIUM fix: the names already stored, so we can warn before silently
  // overwriting one. Optional — callers without the list (e.g. inline in a
  // wizard) simply skip the overwrite warning.
  existingNames?: string[];
  // Prefill the name field (e.g. the setup screen suggesting "anthropic-api-key",
  // or this screen's "Rotate" action prefilling the secret being rotated).
  // Optional — defaults to "", preserving existing callers' blank-name behavior.
  initialName?: string;
}) {
  const [name, setName] = React.useState(initialName);
  const [value, setValue] = React.useState("");
  const [error, setError] = React.useState<string | null>(null);
  const [saving, setSaving] = React.useState(false);
  // MEDIUM fix: require an explicit confirm-overwrite click when the typed name
  // already exists, so a save never silently clobbers a secret in use by runs.
  const [confirmOverwrite, setConfirmOverwrite] = React.useState(false);

  React.useEffect(() => {
    if (open) {
      setName(initialName);
      setValue("");
      setError(null);
      setSaving(false);
      setConfirmOverwrite(false);
    }
  }, [open, initialName]);

  // PUT is an upsert server-side: storing a name that already exists overwrites
  // its value. Detect that case (exact match on the trimmed name) so we can warn.
  const trimmed = name.trim();
  const isOverwrite = existingNames.includes(trimmed);
  // Editing the name clears any prior overwrite acknowledgement.
  React.useEffect(() => {
    setConfirmOverwrite(false);
  }, [trimmed]);

  const save = async () => {
    setError(null);
    const n = name.trim();
    if (!SECRET_NAME_RE.test(n)) {
      setError("Invalid name: lowercase alphanumerics, '.', '_', '-' (cannot start/end with these).");
      return;
    }
    if (!value) {
      setError("Value is required.");
      return;
    }
    // First Save attempt on an existing name asks for explicit confirmation;
    // the button text flips to "Overwrite secret" and a second click proceeds.
    if (isOverwrite && !confirmOverwrite) {
      setConfirmOverwrite(true);
      return;
    }
    setSaving(true);
    try {
      await secretsApi.setSecret(n, value);
      // Clear the value immediately — never retain or echo it.
      setValue("");
      onOpenChange(false);
      onSaved?.(n);
    } catch (e) {
      setError(getErrorMessage(e) || "Failed to store secret.");
      setSaving(false);
    }
  };

  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent className="sm:max-w-lg">
        <DialogHeader>
          <DialogTitle>{isOverwrite ? "Rotate secret" : "Add secret"}</DialogTitle>
          <DialogDescription>
            The value is stored write-only — it is injected proxy-side at use time and is never
            returned by the API or shown again.
          </DialogDescription>
        </DialogHeader>

        <div className="space-y-4 py-1">
          <div className="space-y-2">
            <Label htmlFor="secret-name">Name</Label>
            {/* Suggestions only on a BLANK-name open (a fresh "Add secret", not a
                rotate/fix-flow open that already knows what it wants) — prefill
                the name only, never a value. */}
            {!initialName && (
              <ProviderNameChips onPick={setName} />
            )}
            <Input
              id="secret-name"
              placeholder="anthropic-api-key"
              value={name}
              onChange={(e) => setName(e.target.value)}
              className="font-mono"
              autoComplete="off"
            />
          </div>
          <div className="space-y-2">
            <Label htmlFor="secret-value">Value</Label>
            <Textarea
              id="secret-value"
              placeholder="sk-…"
              value={value}
              onChange={(e) => setValue(e.target.value)}
              rows={3}
              spellCheck={false}
              autoComplete="off"
              className="font-mono text-xs"
            />
            <p className="text-[11px] leading-snug text-muted-foreground">
              This field is cleared on save and the value is never displayed again.
            </p>
          </div>
          {/* MEDIUM fix: warn when the name already exists so the operator
              doesn't silently overwrite a secret currently referenced by runs. */}
          {isOverwrite && !error && (
            <div className="flex items-start gap-2 rounded-lg border border-warning/30 bg-warning-subtle px-3 py-2 text-xs text-warning">
              <AlertTriangle className="mt-0.5 size-3.5 shrink-0" />
              <span>
                A secret named <span className="font-mono">{trimmed}</span> already exists. Saving
                will overwrite its value for every run that references it. This cannot be undone.
              </span>
            </div>
          )}
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
          <Button
            onClick={save}
            disabled={saving || !name.trim() || !value}
            variant={isOverwrite && confirmOverwrite ? "destructive" : "info"}
          >
            {saving ? <Loader2 className="size-4 animate-spin" /> : <Lock className="size-4" />}
            {isOverwrite ? (confirmOverwrite ? "Overwrite secret" : "Save (overwrites)") : "Save secret"}
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}

// A row of common secret-name suggestions (toggle-button styling matches the
// egress preset chips, step-egress.tsx's PRESET_DOMAINS). Clicking one sets the
// Name field verbatim; "Custom…" clears it back to blank for a hand-typed name.
// Never touches the Value field.
function ProviderNameChips({ onPick }: { onPick: (name: string) => void }) {
  return (
    <div className="flex flex-wrap gap-1.5">
      {PROVIDER_NAME_CHIPS.map((n) => (
        <button
          key={n}
          type="button"
          onClick={() => onPick(n)}
          className="rounded-md border border-border px-2 py-1 font-mono text-[11px] text-muted-foreground transition-colors hover:border-border-strong hover:text-foreground"
        >
          {n}
        </button>
      ))}
      <button
        type="button"
        onClick={() => onPick("")}
        className="rounded-md border border-dashed border-border px-2 py-1 text-[11px] text-muted-foreground transition-colors hover:border-border-strong hover:text-foreground"
      >
        Custom…
      </button>
    </div>
  );
}
