/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import * as React from "react";
import { Lock, Plus, MoreHorizontal, Trash2, RotateCw, Loader2, KeyRound, AlertTriangle, GitBranch, Eye, EyeOff } from "lucide-react";
import { secrets as secretsApi } from "../../lib/api/secrets";
import { getErrorMessage } from "../../lib/format";
import { useMyCapabilities } from "../../lib/capabilities";
import { DENIED } from "../../lib/permissions-copy";
import { LANE_META, laneOfName, type Lane } from "../../lib/scm-provider";
import { Button } from "../ui/button";
import { Input } from "../ui/input";
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
import { Field } from "../wardyn/form-primitives";
import { Mono } from "../wardyn/code-block";
import { Chip, OperatorOnlyHint } from "../wardyn/primitives";
import { EmptyState, ErrorState, TableSkeleton } from "../wardyn/states";
import { PageHeader } from "../wardyn/page-header";
import { DeleteConfirmDialog } from "../wardyn/delete-confirm-dialog";
import { CAPABILITY, OPERATOR_ONLY_REASON } from "../wardyn/copy";
import { useOperator } from "../wardyn/operator-context";

// Secret names are constrained server-side to a safe identifier set; mirror that
// here so we reject obviously-bad names before the round-trip.
const SECRET_NAME_RE = /^[a-z0-9]([a-z0-9._-]{0,126}[a-z0-9])?$/;

// Prebuilt secret-name suggestions offered on a BLANK-name open of AddSecretDialog
// (never on a prefilled/rotate open — the operator already knows the name then).
// Chips only prefill the Name field, never a value — the credential itself is
// always typed/pasted by the operator.
// github-pat/gitlab-pat are deliberately NOT suggested here — they're now
// LEGACY_NAMES (lib/scm-provider.ts): the SCM ladder writes git-pat-<slug>
// instead, and suggesting the old flat names would contradict that convention.
const PROVIDER_NAME_CHIPS = [
  "anthropic-api-key",
  "openai-api-key",
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
  const operator = useOperator();
  // When `secret` is enforced, handleListSecrets returns only the names this
  // member holds — say so, or a short list reads as "there are only two
  // secrets here" rather than "you were shown two of them".
  const caps = useMyCapabilities(!operator);
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

  const filtered = names.filter((n) => !query || n.toLowerCase().includes(query.toLowerCase()));

  return (
    <div className="mx-auto max-w-[1400px] px-6 py-6">
      <PageHeader
        title="Secrets"
        description={`Write-only: values go in and never come out. ${CAPABILITY.brokerLine} Exception: ${CAPABILITY.gitPatLine}`}
        actions={
          <>
            {!operator && <Chip tone="neutral">{OPERATOR_ONLY_REASON}</Chip>}
            <Button onClick={() => setAddOpen(true)} disabled={!operator}>
              <Plus className="size-4" /> Add secret
            </Button>
          </>
        }
      />

      {caps?.enforcement.secret && (
        <p className="mb-4 rounded-lg bg-muted px-3 py-2 text-xs text-muted-foreground">{DENIED.SECRETS_NARROWED}</p>
      )}

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
            description={
              operator
                ? "Add an API key or access token so runs can reference it by name — the value is stored write-only and is never returned, not even to you."
                : `Add an API key or access token so runs can reference it by name. ${OPERATOR_ONLY_REASON}`
            }
            action={
              // ui-secretsPolicies-4: this screen stores any credential, not
              // just LLM keys — match the header button's "Add secret" copy
              // instead of narrowing a first-time operator's mental model.
              <Button onClick={() => setAddOpen(true)} disabled={!operator}>
                <Plus className="size-4" /> Add your first secret
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
                        <DropdownMenuItem onClick={() => setRotateName(name)} disabled={!operator}>
                          <RotateCw className="size-4" /> Rotate
                          {!operator && <OperatorOnlyHint />}
                        </DropdownMenuItem>
                        <DropdownMenuItem
                          onClick={() => setToDelete(name)}
                          disabled={!operator}
                          className="text-danger focus:text-danger"
                        >
                          <Trash2 className="size-4" /> Delete
                          {!operator && <OperatorOnlyHint />}
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

// What a locked name actually means. NOT "the clone lane matches this name":
// no Go code maps a host to a secret name. The only reader of the
// git-pat-/ssh-key- prefixes is scmProviderCheck (internal/api/setup.go:865),
// which grades setup posture and gates nothing. A run reaches a credential
// through a grant that names it explicitly — git_pat {host, secret_name},
// ssh_key {host, key_secret_ref}, both required
// (internal/api/runs_scm.go:223-260) — chosen by hand in the New Run wizard's
// git-credential card. The GitHub App is the one place a name IS the binding:
// the broker reads the fixed github-app-id / github-app-key
// (cmd/wardynd/main.go:698-699).
function lockedNameHint(lane: Lane, hasHost: boolean): string {
  if (lane === "app") return "Locked — the GitHub App broker reads this exact secret name.";
  const grant = lane === "ssh" ? "ssh_key" : "git_pat";
  const bound = `A run reaches it through a ${grant} grant that names it (New Run → Access), not through the name itself.`;
  return hasHost ? `Locked — the conventional name for this host. ${bound}` : `Locked. ${bound}`;
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
  lockName,
  host,
  lane,
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
  // Host-aware locked mode (design "Prompt D"): Name becomes read-only — the
  // operator can never edit it. Optional and additive; omitted, this is
  // exactly today's editable, chip-suggested dialog.
  lockName?: boolean;
  // The host this credential is FOR. Rendered ONCE, above Name, as a read-only
  // fact block (glyph + host + lane chip) — never as an input, because this
  // dialog only stores a name/value pair: nothing here binds the secret to the
  // host. That binding is per-run, made in the New Run wizard's git-credential
  // card (a git_pat/ssh_key grant naming both the host and the secret). No
  // host, no block — the generic blank-name dialog has nothing to claim.
  host?: string;
  // Which lane chip the fact block shows; defaults to laneOfName(name) so a
  // caller that already knows the locked name doesn't have to re-derive it.
  lane?: Lane;
}) {
  // This dialog is reused everywhere a secret gets written (this screen, the
  // SCM Provider step, the New Run wizard, the setup funnel) — gating its own
  // Save is the one chokepoint that covers all of them, so none of those
  // callers need their own copy of this check.
  const operator = useOperator();
  const [name, setName] = React.useState(initialName);
  const [value, setValue] = React.useState("");
  // Masked at entry, revealed only on request: a write-only store should not
  // put the plaintext on screen while it is typed (shoulder-surf + screen
  // shares; caught on camera by the demo series). -webkit-text-security is
  // the only way to mask a MULTILINE value (PEM keys need the textarea);
  // Firefox ignores it and degrades to plaintext — cosmetic masking, not a
  // security boundary either way.
  const [reveal, setReveal] = React.useState(false);
  const [error, setError] = React.useState<string | null>(null);
  const [saving, setSaving] = React.useState(false);
  // MEDIUM fix: require an explicit confirm-overwrite click when the typed name
  // already exists, so a save never silently clobbers a secret in use by runs.
  const [confirmOverwrite, setConfirmOverwrite] = React.useState(false);

  React.useEffect(() => {
    if (open) {
      setName(initialName);
      setValue("");
      setReveal(false);
      setError(null);
      setSaving(false);
      setConfirmOverwrite(false);
    }
  }, [open, initialName]);

  // PUT is an upsert server-side: storing a name that already exists overwrites
  // its value. Detect that case (exact match on the trimmed name) so we can warn.
  const trimmed = name.trim();
  const isOverwrite = existingNames.includes(trimmed);
  const resolvedLane: Lane = lane ?? laneOfName(trimmed);
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
            {lockName
              ? "The value is write-only — it can be replaced or removed, never read back."
              : "The value is stored write-only — it is injected proxy-side at use time and is never returned by the API or shown again."}
          </DialogDescription>
        </DialogHeader>

        <div className="space-y-4 py-1">
          {/* Read-only fact, never an input, and the ONE place the host appears
              on screen — the hint below deliberately says "this host" rather
              than repeating it. */}
          {host && (
            <div className="flex items-center gap-2.5">
              <div className="flex size-8 shrink-0 items-center justify-center rounded-lg border border-border text-muted-foreground">
                <GitBranch className="size-4" />
              </div>
              <Mono className="text-sm text-foreground">{host}</Mono>
              <Chip tone={LANE_META[resolvedLane].tone} title={LANE_META[resolvedLane].tooltip}>
                {LANE_META[resolvedLane].label}
              </Chip>
            </div>
          )}
          <Field
            label="Name"
            htmlFor="secret-name"
            required
            hint={lockName ? lockedNameHint(resolvedLane, !!host) : undefined}
          >
            {/* Suggestions only on a BLANK-name open (a fresh "Add secret", not a
                rotate/fix-flow or locked open that already knows what it wants) —
                prefill the name only, never a value. */}
            {!initialName && !lockName && (
              <ProviderNameChips onPick={setName} />
            )}
            <Input
              id="secret-name"
              placeholder="anthropic-api-key"
              value={name}
              // Guarded, not just the readOnly attribute below: readOnly stops a
              // real user's keystrokes, but a dispatched change event still
              // reaches a controlled input's onChange. Locked means locked.
              onChange={(e) => {
                if (!lockName) setName(e.target.value);
              }}
              className="font-mono"
              autoComplete="off"
              readOnly={lockName}
              aria-readonly={lockName}
              required
            />
          </Field>
          <Field
            label="Value"
            htmlFor="secret-value"
            required
            // Locked mode's DialogDescription already states the write-only fact
            // above — repeating "never displayed again" here would say it twice.
            hint={lockName ? undefined : "This field is cleared on save and the value is never displayed again."}
          >
            <div className="relative">
              <Textarea
                id="secret-value"
                placeholder="sk-…"
                value={value}
                onChange={(e) => setValue(e.target.value)}
                rows={3}
                spellCheck={false}
                autoComplete="off"
                className="font-mono text-xs pr-9"
                style={reveal ? undefined : ({ WebkitTextSecurity: "disc" } as React.CSSProperties)}
                required
              />
              <button
                type="button"
                onClick={() => setReveal((r) => !r)}
                aria-label={reveal ? "Hide value" : "Show value"}
                aria-pressed={reveal}
                className="absolute right-2 top-2 text-muted-foreground hover:text-foreground"
              >
                {reveal ? <EyeOff className="size-4" aria-hidden /> : <Eye className="size-4" aria-hidden />}
              </button>
            </div>
          </Field>
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
          {/* ui-secretsPolicies-2: role="alert" (an implicit aria-live region)
              plus wiring into the Save button's aria-describedby below —
              this fires on every rejected save, a far more common path than
              the operator-reason paragraph, but was previously visual-only. */}
          {error && (
            <div
              id="add-secret-error"
              role="alert"
              className="rounded-lg border border-danger/30 bg-danger-subtle px-3 py-2 text-xs text-danger"
            >
              {error}
            </div>
          )}
          {!operator && (
            <p id="add-secret-operator-reason" className="text-xs font-medium text-warning">
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
            disabled={!operator || saving || !name.trim() || !value}
            aria-describedby={
              [error ? "add-secret-error" : undefined, !operator ? "add-secret-operator-reason" : undefined]
                .filter(Boolean)
                .join(" ") || undefined
            }
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
// Policy panel's template chips). Clicking one sets the
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
          className="rounded-md border border-border px-2 py-1 font-mono text-meta text-muted-foreground transition-colors hover:border-border-strong hover:text-foreground"
        >
          {n}
        </button>
      ))}
      <button
        type="button"
        onClick={() => onPick("")}
        className="rounded-md border border-dashed border-border px-2 py-1 text-meta text-muted-foreground transition-colors hover:border-border-strong hover:text-foreground"
      >
        Custom…
      </button>
    </div>
  );
}
