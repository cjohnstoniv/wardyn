/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import * as React from "react";
import { toast } from "sonner";
import {
  UserCog,
  Plus,
  MoreHorizontal,
  Eye,
  Pencil,
  Trash2,
  RotateCw,
  Loader2,
  ShieldCheck,
  Globe,
  Timer,
  TriangleAlert,
  ChevronRight,
} from "lucide-react";
import type { RunPolicy, RunPolicySpec } from "../../lib/types";
import { api } from "../../lib/api";
import { getErrorMessage, relativeTime } from "../../lib/format";
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
  DropdownMenuSeparator,
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
import {
  AlertDialog,
  AlertDialogAction,
  AlertDialogCancel,
  AlertDialogContent,
  AlertDialogDescription,
  AlertDialogFooter,
  AlertDialogHeader,
  AlertDialogTitle,
} from "../ui/alert-dialog";
import {
  Sheet,
  SheetContent,
  SheetHeader,
  SheetTitle,
} from "../ui/sheet";
import { ConfinementChip, Chip, SectionLabel } from "../wardyn/primitives";
import { Mono, YamlBlock } from "../wardyn/code-block";
import { EmptyState, ErrorState, TableSkeleton } from "../wardyn/states";
import { PageHeader } from "../wardyn/page-header";
import { CC_META } from "../wardyn/cc-meta";
import { RESIDUAL_PREFIX } from "../wardyn/copy";
// ComposeQuickReview is the composer wizard's CAN/CAN'T projection of a
// RunPolicySpec — reused here instead of a second prose family, so a stored
// policy reads in the exact same honest vocabulary as a proposed one.
import { ComposeQuickReview } from "./new-run/compose-quick-review";

type ChipTone = NonNullable<React.ComponentProps<typeof Chip>["tone"]>;

// A starter spec used to prefill the "create" editor. Mirrors the shipped
// default policy shape so an operator sees a valid, editable starting point.
const STARTER_SPEC: RunPolicySpec = {
  allowed_domains: ["api.anthropic.com"],
  first_use_approval: "deny_with_review",
  min_confinement_class: "CC2",
  eligible_grants: [],
};

// Compact, honest egress summary for the table row. allow_all_egress is ALWAYS
// the block-list phrasing (never "unrestricted") — see wardyn/copy.ts.
function egressSummary(spec: RunPolicySpec): { label: string; tone: ChipTone } {
  if (spec.allow_all_egress) {
    return { label: "Allow-all egress (block-list only)", tone: "info" };
  }
  const n = spec.allowed_domains?.length ?? 0;
  const denied = spec.denied_domains?.length ?? 0;
  if (n === 0) {
    return { label: denied > 0 ? `No egress, ${denied} denied` : "No egress", tone: "neutral" };
  }
  return {
    label: `${n} domain${n === 1 ? "" : "s"} allowed${denied > 0 ? `, ${denied} denied` : ""}`,
    tone: "info",
  };
}

// Honest lifecycle summary — mirrors the reaper's ACTUAL semantics
// (internal/lifecycle/lifecycle.go): auto_stop_after_sec <= 0 or unset means the
// run is exempt from idle auto-stop, not "30 minutes by default".
function lifecycleSummary(spec: RunPolicySpec): string {
  const s = spec.auto_stop_after_sec;
  if (typeof s === "number" && s > 0) return `Auto-stop: ${Math.max(1, Math.round(s / 60))} min idle`;
  return "Runs until stopped";
}

export function PoliciesScreen() {
  const [policies, setPolicies] = React.useState<RunPolicy[]>([]);
  const [status, setStatus] = React.useState<"loading" | "error" | "ready">("loading");
  const [query, setQuery] = React.useState("");
  const [selected, setSelected] = React.useState<string | null>(null);
  const [editor, setEditor] = React.useState<{ mode: "create" | "edit"; policy?: RunPolicy } | null>(null);
  const [toDelete, setToDelete] = React.useState<RunPolicy | null>(null);
  const [deleting, setDeleting] = React.useState(false);

  const load = React.useCallback(() => {
    setStatus("loading");
    api
      .listPolicies()
      .then((p) => {
        setPolicies(p);
        setStatus("ready");
      })
      .catch(() => setStatus("error"));
  }, []);
  React.useEffect(load, [load]);

  const filtered = policies.filter((p) => {
    const q = query.toLowerCase();
    return !q || p.name.toLowerCase().includes(q) || p.id.toLowerCase().includes(q);
  });

  // HONEST fix: this used to be a bare try/finally with no catch — a rejected
  // deletePolicy() (403, network drop, server error) threw as an unhandled
  // promise rejection and the operator was left believing the delete went
  // through while the policy was still there. Surface the failure as a toast
  // and keep the confirm dialog open so they can retry.
  const confirmDelete = async () => {
    if (!toDelete) return;
    setDeleting(true);
    try {
      await api.deletePolicy(toDelete.id);
      toast.success(`Policy “${toDelete.name}” deleted`);
      setToDelete(null);
      if (selected === toDelete.id) setSelected(null);
      load();
    } catch (e) {
      toast.error(`Failed to delete policy “${toDelete.name}”`, {
        description: getErrorMessage(e),
      });
    } finally {
      setDeleting(false);
    }
  };

  return (
    <div className="mx-auto max-w-[1400px] px-6 py-6">
      <PageHeader
        title="Policies"
        description="Policies set a run's barrier, egress allowlist, credential grants, and lifecycle — referenced by ID (or supplied inline) when a run is created."
        actions={
          <Button onClick={() => setEditor({ mode: "create" })}>
            <Plus className="size-4" /> New policy
          </Button>
        }
      />

      {/* Honest stand-in for the design's "built-in defaults" card: the control
          plane DOES apply a configured default policy to runs created without a
          policy_id (internal/api/server.go Config.DefaultPolicy), but no endpoint
          exposes its fields — so we say the mechanism exists without fabricating
          its contents. */}
      <p className="mb-4 text-xs text-muted-foreground">
        A run created without a policy_id falls back to the control plane's configured default —
        its exact settings aren't exposed by this API, so they aren't shown here.
      </p>

      <div className="mb-4 flex items-center gap-3">
        <Input
          placeholder="Search policies by name or id…"
          value={query}
          onChange={(e) => setQuery(e.target.value)}
          className="max-w-sm"
        />
        <span className="ml-auto text-sm text-muted-foreground">
          {status === "ready" && `${filtered.length} polic${filtered.length === 1 ? "y" : "ies"}`}
        </span>
        <Button variant="outline" size="icon" onClick={load} aria-label="Refresh">
          <RotateCw className="size-4" />
        </Button>
      </div>

      <div className="overflow-hidden rounded-xl border border-border bg-card">
        {status === "loading" ? (
          <TableSkeleton rows={5} cols={7} />
        ) : status === "error" ? (
          <ErrorState onRetry={load} />
        ) : filtered.length === 0 ? (
          <EmptyState
            icon={UserCog}
            title={query ? "No matching policies" : "No policies yet"}
            description={
              query
                ? "Try a different search term."
                : "A policy overrides the default for specific runs — tighter or looser, referenced by its ID when you create a run."
            }
            action={
              query ? (
                <Button variant="outline" onClick={() => setQuery("")}>
                  Clear filters
                </Button>
              ) : (
                <Button onClick={() => setEditor({ mode: "create" })}>
                  <Plus className="size-4" /> Create your first policy
                </Button>
              )
            }
          />
        ) : (
          <Table>
            <TableHeader>
              <TableRow className="hover:bg-transparent">
                <TableHead className="w-[180px]">Name</TableHead>
                <TableHead className="w-[120px]">ID</TableHead>
                <TableHead className="w-[90px]">Barrier</TableHead>
                <TableHead>Egress</TableHead>
                <TableHead className="w-[160px]">Attributes</TableHead>
                <TableHead className="w-[100px]">Updated</TableHead>
                <TableHead className="w-[44px]" />
              </TableRow>
            </TableHeader>
            <TableBody>
              {filtered.map((p) => {
                const egress = egressSummary(p.spec);
                const grants = p.spec.eligible_grants ?? [];
                return (
                  <TableRow key={p.id} onClick={() => setSelected(p.id)} className="cursor-pointer">
                    <TableCell>
                      <span className="font-medium text-foreground">{p.name}</span>
                    </TableCell>
                    <TableCell>
                      <Mono>{p.id}</Mono>
                    </TableCell>
                    <TableCell>
                      <ConfinementChip value={p.spec.min_confinement_class} />
                    </TableCell>
                    <TableCell>
                      <Chip tone={egress.tone} className="gap-1">
                        <Globe className="size-3" />
                        {egress.label}
                      </Chip>
                    </TableCell>
                    <TableCell>
                      <div className="flex flex-wrap gap-1">
                        {/* D2: capability GRANTS render amber — never a reassuring
                            green check. */}
                        {grants.length > 0 && (
                          <Chip
                            tone="warning"
                            className="gap-1"
                            title={grants
                              .map((g) => `${g.kind}${g.requires_approval ? " (needs approval)" : " (auto-granted)"}`)
                              .join("; ")}
                          >
                            <TriangleAlert className="size-3" />
                            {grants.length} grant{grants.length === 1 ? "" : "s"}
                          </Chip>
                        )}
                        <Chip tone="neutral" className="gap-1">
                          <Timer className="size-3" />
                          {lifecycleSummary(p.spec)}
                        </Chip>
                      </div>
                    </TableCell>
                    <TableCell>
                      <span className="text-sm text-muted-foreground" title={p.updated_at}>
                        {relativeTime(p.updated_at)}
                      </span>
                    </TableCell>
                    <TableCell onClick={(e) => e.stopPropagation()}>
                      <DropdownMenu>
                        <DropdownMenuTrigger asChild>
                          <Button variant="ghost" size="icon" className="size-8">
                            <MoreHorizontal className="size-4" />
                          </Button>
                        </DropdownMenuTrigger>
                        <DropdownMenuContent align="end">
                          <DropdownMenuItem onClick={() => setSelected(p.id)}>
                            <Eye className="size-4" /> View details
                          </DropdownMenuItem>
                          <DropdownMenuItem onClick={() => setEditor({ mode: "edit", policy: p })}>
                            <Pencil className="size-4" /> Edit
                          </DropdownMenuItem>
                          <DropdownMenuSeparator />
                          <DropdownMenuItem
                            onClick={() => setToDelete(p)}
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

      <PolicyDetail
        policy={policies.find((p) => p.id === selected) ?? null}
        onClose={() => setSelected(null)}
        onEdit={(p) => {
          setSelected(null);
          setEditor({ mode: "edit", policy: p });
        }}
      />

      <PolicyEditor
        editor={editor}
        onClose={() => setEditor(null)}
        onSaved={() => {
          setEditor(null);
          load();
        }}
      />

      <AlertDialog open={!!toDelete} onOpenChange={(o) => !o && setToDelete(null)}>
        <AlertDialogContent>
          <AlertDialogHeader>
            <AlertDialogTitle>Delete policy “{toDelete?.name}”?</AlertDialogTitle>
            <AlertDialogDescription>
              This removes the policy from the control plane. Runs already created under it are
              unaffected, but new runs can no longer reference it. This cannot be undone.
            </AlertDialogDescription>
          </AlertDialogHeader>
          <AlertDialogFooter>
            <AlertDialogCancel>Cancel</AlertDialogCancel>
            <AlertDialogAction
              onClick={(e) => {
                e.preventDefault();
                confirmDelete();
              }}
              className="bg-danger text-danger-foreground hover:bg-danger/90"
            >
              {deleting ? <Loader2 className="size-4 animate-spin" /> : <Trash2 className="size-4" />}
              Delete policy
            </AlertDialogAction>
          </AlertDialogFooter>
        </AlertDialogContent>
      </AlertDialog>
    </div>
  );
}

function PolicyDetail({
  policy,
  onClose,
  onEdit,
}: {
  policy: RunPolicy | null;
  onClose: () => void;
  onEdit: (p: RunPolicy) => void;
}) {
  const meta = policy ? CC_META[policy.spec.min_confinement_class] : undefined;

  return (
    <Sheet open={!!policy} onOpenChange={(o) => !o && onClose()}>
      <SheetContent className="scroll-thin w-full gap-0 overflow-y-auto sm:max-w-[560px]">
        <SheetHeader className="border-b border-border pb-4">
          <SheetTitle className="flex items-center gap-2">
            <ShieldCheck className="size-4 text-primary" />
            {policy?.name}
          </SheetTitle>
        </SheetHeader>
        {policy && (
          <div className="space-y-4 py-4">
            <div className="grid grid-cols-2 gap-3 text-sm">
              <Field label="Policy ID" value={<Mono>{policy.id}</Mono>} />
              <Field label="Barrier" value={<ConfinementChip value={policy.spec.min_confinement_class} />} />
              <Field label="Created" value={relativeTime(policy.created_at)} />
              <Field label="Updated" value={relativeTime(policy.updated_at)} />
            </div>

            {/* Policy bodies as humane rows (C7) — the same CAN/CAN'T projection
                the New Run wizard uses, so a stored policy and a proposed one
                read identically. */}
            <div>
              <SectionLabel>What this policy allows</SectionLabel>
              <div className="mt-2">
                <ComposeQuickReview inline_policy={policy.spec} />
              </div>
            </div>

            {meta && (
              <p className="rounded-md bg-surface-2 px-3 py-2 text-xs leading-snug text-muted-foreground">
                <span className="font-medium text-foreground/80">{RESIDUAL_PREFIX}</span> {meta.doesntProtect}
              </p>
            )}

            {(policy.spec.allowed_methods?.length ?? 0) > 0 && (
              <Field
                label="Allowed methods"
                value={<Mono>{policy.spec.allowed_methods!.join(", ")}</Mono>}
              />
            )}

            {/* Raw JSON stays one click away (C7), never the primary content. */}
            <details className="group rounded-lg border border-border">
              <summary className="flex cursor-pointer select-none items-center gap-1.5 px-3 py-2 text-xs font-medium text-muted-foreground hover:text-foreground">
                <ChevronRight className="size-3.5 transition-transform group-open:rotate-90" />
                View raw JSON
              </summary>
              <YamlBlock value={policy.spec} className="m-2 mt-0 rounded-md border-0" />
            </details>

            <div className="flex justify-end">
              <Button variant="outline" size="sm" onClick={() => onEdit(policy)}>
                <Pencil className="size-4" /> Edit policy
              </Button>
            </div>
          </div>
        )}
      </SheetContent>
    </Sheet>
  );
}

function Field({ label, value }: { label: string; value: React.ReactNode }) {
  return (
    <div>
      <div className="text-[11px] uppercase tracking-wide text-muted-foreground">{label}</div>
      <div className="mt-0.5">{value}</div>
    </div>
  );
}

// PolicyEditor is a minimal create/edit form: a name field plus a JSON textarea
// for the spec. The server is the source of truth for validation — it rejects a
// bad spec with HTTP 400, which we surface verbatim. We only do light client-side
// JSON parsing so a syntactically broken document never reaches the API.
function PolicyEditor({
  editor,
  onClose,
  onSaved,
}: {
  editor: { mode: "create" | "edit"; policy?: RunPolicy } | null;
  onClose: () => void;
  onSaved: () => void;
}) {
  const [name, setName] = React.useState("");
  const [specText, setSpecText] = React.useState("");
  const [error, setError] = React.useState<string | null>(null);
  const [saving, setSaving] = React.useState(false);

  React.useEffect(() => {
    if (!editor) return;
    setError(null);
    setSaving(false);
    if (editor.mode === "edit" && editor.policy) {
      setName(editor.policy.name);
      setSpecText(JSON.stringify(editor.policy.spec, null, 2));
    } else {
      setName("");
      setSpecText(JSON.stringify(STARTER_SPEC, null, 2));
    }
  }, [editor]);

  const save = async () => {
    setError(null);
    if (!name.trim()) {
      setError("Name is required.");
      return;
    }
    let spec: RunPolicySpec;
    try {
      spec = JSON.parse(specText) as RunPolicySpec;
    } catch (e) {
      setError(`Spec is not valid JSON: ${getErrorMessage(e)}`);
      return;
    }
    setSaving(true);
    try {
      if (editor?.mode === "edit" && editor.policy) {
        await api.updatePolicy(editor.policy.id, name.trim(), spec);
      } else {
        await api.createPolicy(name.trim(), spec);
      }
      onSaved();
    } catch (e) {
      // Surface the server's validation message (HttpError carries the body).
      setError(getErrorMessage(e) || "Failed to save policy.");
      setSaving(false);
    }
  };

  const isEdit = editor?.mode === "edit";

  return (
    <Dialog open={!!editor} onOpenChange={(o) => !o && onClose()}>
      <DialogContent className="sm:max-w-2xl">
        <DialogHeader>
          <DialogTitle>{isEdit ? "Edit policy" : "New policy"}</DialogTitle>
          <DialogDescription>
            Policies are admin-gated config. The spec is validated server-side before it is saved —
            an invalid spec is rejected and shown here.
          </DialogDescription>
        </DialogHeader>

        <div className="space-y-4 py-1">
          <div className="space-y-2">
            <Label htmlFor="policy-name">Name</Label>
            <Input
              id="policy-name"
              placeholder="payments-strict"
              value={name}
              onChange={(e) => setName(e.target.value)}
            />
          </div>
          <div className="space-y-2">
            <Label htmlFor="policy-spec">Spec (JSON)</Label>
            <Textarea
              id="policy-spec"
              value={specText}
              onChange={(e) => setSpecText(e.target.value)}
              rows={16}
              spellCheck={false}
              className="font-mono text-xs"
            />
            <p className="text-[11px] leading-snug text-muted-foreground">
              Requires <span className="font-mono">min_confinement_class</span> (CC1|CC2|CC3 = the
              Fence/Wall/Vault barrier). Optional:
              <span className="font-mono"> allowed_domains</span>, <span className="font-mono">denied_domains</span>,
              <span className="font-mono"> first_use_approval</span>, <span className="font-mono">eligible_grants</span>.
            </p>
          </div>
          {error && (
            <div className="rounded-lg border border-danger/30 bg-danger-subtle px-3 py-2 text-xs text-danger">
              {error}
            </div>
          )}
        </div>

        <DialogFooter>
          <Button variant="ghost" onClick={onClose}>
            Cancel
          </Button>
          <Button onClick={save} disabled={saving || !name.trim()}>
            {saving ? <Loader2 className="size-4 animate-spin" /> : <ShieldCheck className="size-4" />}
            {isEdit ? "Save changes" : "Create policy"}
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}
