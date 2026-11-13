/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import * as React from "react";
import { useInRouterContext } from "react-router-dom";
import { Lock, Plus, MoreHorizontal, Trash2, RotateCw, Loader2, KeyRound, AlertTriangle, GitBranch, Eye, EyeOff } from "lucide-react";
import { getExpectedPrincipal, HttpError } from "../../lib/api/core";
import { secrets as secretsApi, type SecretScope } from "../../lib/api/secrets";
import { SECRET_EXISTS_REASON } from "../../lib/secret-refusals";
import { providers as providersApi } from "../../lib/api/providers";
import { getErrorMessage } from "../../lib/format";
import { useMyCapabilities } from "../../lib/capabilities";
import { DENIED } from "../../lib/permissions-copy";
import { LANE_META, laneOfName, patLaneMeta, type Lane } from "../../lib/scm-provider";
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
import { Field, fieldHintId } from "../wardyn/form-primitives";
import { Mono } from "../wardyn/code-block";
import { Chip } from "../wardyn/primitives";
import { EmptyState, ErrorState, failureSentence, TableSkeleton } from "../wardyn/states";
import { useSearchDraft, useUrlFilters } from "../../lib/use-url-filters";
import { PageHeader } from "../wardyn/page-header";
import { PageContainer } from "../wardyn/page-container";
import { DeleteConfirmDialog } from "../wardyn/delete-confirm-dialog";
import { CAPABILITY, OPERATOR_ONLY_REASON } from "../wardyn/copy";
import { OWN_SECRETS, SECRETS_PAGE } from "../wardyn/copy/components";
import { WRITE_ONLY } from "../wardyn/copy/door";
import { SECRETS_BROKER_UNCHECKED } from "../wardyn/copy/lookup-failures";
import { useOperator, useOperatorResolved } from "../wardyn/operator-context";
import { useConsoleMode } from "../wardyn/console-view";
import { useShellSetupStatus } from "../wardyn/model-access-context";

// Secret names are constrained server-side to a safe identifier set; mirror that
// here so we reject obviously-bad names before the round-trip.
const SECRET_NAME_RE = /^[a-z0-9]([a-z0-9._-]{0,126}[a-z0-9])?$/;

// Prebuilt secret-name suggestions offered on a blank-name open of AddSecretDialog
// (never on a prefilled/rotate open — the operator already knows the name then).
// Chips only prefill the Name field, never a value — the credential itself is
// always typed/pasted by the operator.
// github-pat/gitlab-pat are deliberately not suggested here — they're now
// LEGACY_NAMES (lib/scm-provider.ts): the SCM ladder writes git-pat-<slug>
// instead, and suggesting the old flat names would contradict that convention.
// Nor are the old model key names: the API refuses to store them
// (retiredModelCredentialNames), and the dialog points to Model providers instead.
const PROVIDER_NAME_CHIPS = [
  "kubeconfig",
  "npm-token",
  "pypi-token",
  "docker-registry",
] as const;

// Rungs 2/3 of the SCM safest-path ladder (ScmProviderStep) name their secrets
// with these prefixes; both are standing resident credentials auto-used by every
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
  // Every name shown, and the ones that are the caller's own (the only rows a
  // person may rotate or delete: a write or delete lands in their own namespace).
  // An administrator's list is their own namespace, so the two are one.
  const [names, setNames] = React.useState<string[]>([]);
  const [mine, setMine] = React.useState<string[]>([]);
  const [status, setStatus] = React.useState<"loading" | "error" | "ready">("loading");
  const [filters, setFilters] = useUrlFilters();
  const query = filters.get("q") ?? "";
  const [draft, setQuery] = useSearchDraft(query, (q) => setFilters({ q }));
  const [addOpen, setAddOpen] = React.useState(false);
  // The namespace the rows on screen were read from, named by the route that read them. A Rotate or Delete
  // chosen on a row keeps it, so a role change since (the same person signing in again as an admin, say) cannot
  // send the write to the other namespace's secret of the same name: the route names the namespace, not the role.
  const [listedIn, setListedIn] = React.useState<SecretScope>("shared");
  const [rotate, setRotate] = React.useState<{ name: string; scope: SecretScope } | null>(null);
  // The row whose Delete was chosen, and the person this tab had confirmed then:
  // the delete names them, so it never lands on someone who signed in since.
  const [toDelete, setToDelete] = React.useState<{ name: string; person: string; scope: SecretScope } | null>(null);
  // #381 F3: the real WARDYN_GIT_PAT_BROKER switch, read from the ONE door
  // that projects it (GET /workspace-providers, operator-only). null = not
  // known: still loading, the lookup failed, or the viewer is not an operator.
  // Unknown stays unknown: the header says nothing about where a Git token is
  // held until the switch is read, because either answer can be the true one.
  const [patBroker, setPatBroker] = React.useState<boolean | null>(null);
  const [brokerFailed, setBrokerFailed] = React.useState(false);
  const loadBroker = React.useCallback(() => {
    if (!operator) return;
    setBrokerFailed(false);
    providersApi
      .getWorkspaceProviders()
      .then((snap) => setPatBroker(snap.providers.git_pat_broker_enabled ?? true))
      .catch(() => setBrokerFailed(true));
  }, [operator]);
  React.useEffect(loadBroker, [loadBroker]);

  const [loadMessage, setLoadMessage] = React.useState<string | undefined>();
  const identityResolved = useOperatorResolved();
  const load = React.useCallback(() => {
    setStatus("loading");
    // Which namespace is this person's own is known only once /me has answered; until then `operator` is the
    // fail-open default, and a member's read of the shared list would only be refused.
    if (!identityResolved) return;
    const scope: SecretScope = operator ? "shared" : "personal";
    // An administrator's rows are the shared namespace. A member's are their own; the admin-provided names beside
    // them (no actions) still come from GET /secrets.
    (operator
      ? secretsApi.listScoped(scope).then((n) => ({ names: n, mine: n }))
      : Promise.all([secretsApi.listScoped(scope), secretsApi.listSecretsMine()]).then(([own, r]) => ({ names: r.names, mine: own })))
      .then((r) => {
        setNames(r.names);
        setMine(r.mine);
        setListedIn(scope);
        setStatus("ready");
      })
      .catch((e) => {
        setLoadMessage(failureSentence(e));
        setStatus("error");
      });
  }, [operator, identityResolved]);
  React.useEffect(load, [load]);

  // Add decides overwrite from `mine`; until a list has loaded for the person this tab has confirmed, a name
  // missing from it proves nothing.
  const inventoryKnown = status === "ready" && identityResolved;
  const ownSet = new Set(mine);
  const shown = [...mine, ...names.filter((n) => !ownSet.has(n))];
  const filtered = shown.filter((n) => !query || n.toLowerCase().includes(query.toLowerCase()));

  return (
    <PageContainer width="list">
      <PageHeader
        title="Secrets"
        // #381 F3: "Exception" only reads true when the broker is OFF (the
        // resident posture really is an exception to gitPatLine's claim); on
        // (the default), gitPatLine's own text just elaborates on the same
        // claim, so the word is dropped rather than reading as a contradiction.
        // storedSecretLine says a value "stays in Wardyn, unless a run delivers it": false for a Git token
        // with the broker off (the git helper hands over the token itself), so it waits for the switch too.
        description={`Write-only: values go in and never come out. ${
          patBroker === null
            ? SECRETS_BROKER_UNCHECKED.VALUE_DELIVERABLE
            : `${CAPABILITY.storedSecretLine} ${patBroker ? CAPABILITY.gitPatLine : `Exception: ${CAPABILITY.gitPatLineResident}`}`
        }`}
        actions={
          <>
            <Button
              onClick={() => setAddOpen(true)}
              disabled={!inventoryKnown}
              aria-describedby={status === "error" ? "secrets-add-reason" : undefined}
            >
              <Plus className="size-4" /> Add secret
            </Button>
          </>
        }
      />

      {brokerFailed && patBroker === null && (
        <p role="status" className="mb-4 flex flex-wrap items-center gap-2 rounded-lg bg-muted px-3 py-2 text-xs text-muted-foreground">
          {SECRETS_BROKER_UNCHECKED.NOTE}
          <Button variant="outline" size="sm" onClick={loadBroker}>
            {SECRETS_BROKER_UNCHECKED.RETRY}
          </Button>
        </p>
      )}

      {caps?.enforcement.secret && (
        <p className="mb-4 rounded-lg bg-muted px-3 py-2 text-xs text-muted-foreground">{DENIED.SECRETS_NARROWED}</p>
      )}

      {status === "ready" && shown.length > 0 && (
        <div className="mb-4 flex items-center gap-3">
          <Input
            placeholder="Search secrets by name…"
            aria-label="Search secrets"
            value={draft}
            onChange={(e) => setQuery(e.target.value)}
            className="min-w-0 max-w-sm"
          />
          <span className="ml-auto whitespace-nowrap text-sm text-muted-foreground">
            {filtered.length} of {shown.length} secret{shown.length === 1 ? "" : "s"}
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
          <ErrorState
            message={loadMessage}
            onRetry={load}
            action={
              <p id="secrets-add-reason" className="max-w-sm text-xs text-muted-foreground">
                {SECRETS_PAGE.ADD_WAITS_FOR_LIST}
              </p>
            }
          />
        ) : shown.length === 0 ? (
          <EmptyState
            icon={KeyRound}
            title="No secrets yet."
            description={
              operator
                ? "Add an API key or access token so runs can reference it by name — the value is stored write-only and is never returned, not even to you."
                : OWN_SECRETS.EMPTY
            }
            action={
              // This screen stores any credential, not just LLM keys — match
              // the header button's "Add secret" copy instead of narrowing a
              // first-time operator's mental model.
              <Button onClick={() => setAddOpen(true)} disabled={!inventoryKnown}>
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
                <TableHead className="w-[44px]">
                  <span className="sr-only">Actions</span>
                </TableHead>
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
                        {ownSet.has(name) ? (
                          <Chip tone="cyan" className="gap-1 [tr:hover_&]:bg-card" title={WRITE_ONLY.TOOLTIP}>
                            <Lock className="size-3" /> {WRITE_ONLY.CHIP}
                          </Chip>
                        ) : (
                          <Chip tone="neutral" title={OWN_SECRETS.FROM_ADMIN_TITLE}>
                            {OWN_SECRETS.FROM_ADMIN}
                          </Chip>
                        )}
                      </span>
                    </div>
                  </TableCell>
                  <TableCell onClick={(e) => e.stopPropagation()}>
                    {ownSet.has(name) && (
                    <DropdownMenu>
                      <DropdownMenuTrigger asChild>
                        <Button variant="ghost" size="icon" className="size-8" aria-label={`Secret actions: ${name}`}>
                          <MoreHorizontal className="size-4" />
                        </Button>
                      </DropdownMenuTrigger>
                      <DropdownMenuContent align="end">
                        <DropdownMenuItem onClick={() => setRotate({ name, scope: listedIn })}>
                          <RotateCw className="size-4" /> Rotate
                        </DropdownMenuItem>
                        <DropdownMenuItem onClick={() => setToDelete({ name, person: getExpectedPrincipal(), scope: listedIn })} className="text-danger focus:text-danger">
                          <Trash2 className="size-4" /> Delete
                        </DropdownMenuItem>
                      </DropdownMenuContent>
                    </DropdownMenu>
                    )}
                  </TableCell>
                </TableRow>
              ))}
            </TableBody>
          </Table>
        )}
      </div>

      <AddSecretDialog
        open={addOpen}
        onOpenChange={setAddOpen}
        onSaved={load}
        existingNames={mine}
        scope={listedIn}
        ownRows
        patBroker={patBroker}
      />

      <AddSecretDialog
        open={!!rotate}
        onOpenChange={(o) => !o && setRotate(null)}
        onSaved={() => {
          setRotate(null);
          load();
        }}
        existingNames={mine}
        scope={rotate?.scope}
        ownRows
        initialName={rotate?.name ?? ""}
        patBroker={patBroker}
      />

      <DeleteConfirmDialog
        name={toDelete?.name ?? null}
        entity="secret"
        // Only the caller's own rows have a menu, and a delete lands in their own namespace.
        allowed
        description="Runs that reference this secret by name will no longer be able to resolve it. This cannot be undone."
        onOpenChange={(o) => !o && setToDelete(null)}
        onDelete={() => secretsApi.deleteSecret(toDelete!.name, toDelete!.person, toDelete!.scope)}
        onDeleted={() => {
          setToDelete(null);
          load();
        }}
      />
    </PageContainer>
  );
}

// What a locked name actually means. Not "the clone lane matches this name":
// no Go code maps a host to a secret name. The only reader of the
// git-pat-/ssh-key- prefixes is scmProviderCheck (internal/api/setup.go:865),
// which grades setup posture and gates nothing. A run reaches a credential
// through a grant that names it explicitly — git_pat {host, secret_name},
// ssh_key {host, key_secret_ref}, both required
// (internal/api/runs_scm.go:223-260) — chosen by hand in the New Run wizard's
// git-credential card. The GitHub App is the one place a name is the binding:
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
  patBroker = null,
  ownRows = false,
  scope,
}: {
  open: boolean;
  onOpenChange: (o: boolean) => void;
  onSaved?: (name: string) => void;
  // The names already stored, so we can warn before silently
  // overwriting one. Optional — callers without the list (e.g. inline in a
  // wizard) simply skip the overwrite warning.
  existingNames?: string[];
  // Prefill the name field (e.g. the proxy setup's "upstream-proxy-url", or
  // this screen's "Rotate" action prefilling the secret being rotated).
  // Optional — defaults to "", preserving existing callers' blank-name behavior.
  initialName?: string;
  // Host-aware locked mode: Name becomes read-only — the
  // operator can never edit it. Optional and additive; omitted, this is
  // exactly today's editable, chip-suggested dialog.
  lockName?: boolean;
  // The host this credential is for. Rendered once, above Name, as a read-only
  // fact block (glyph + host + lane chip) — never as an input, because this
  // dialog only stores a name/value pair: nothing here binds the secret to the
  // host. That binding is per-run, made in the New Run wizard's git-credential
  // card (a git_pat/ssh_key grant naming both the host and the secret). No
  // host, no block — the generic blank-name dialog has nothing to claim.
  host?: string;
  // Which lane chip the fact block shows; defaults to laneOfName(name) so a
  // caller that already knows the locked name doesn't have to re-derive it.
  lane?: Lane;
  // #381 F3: the real WARDYN_GIT_PAT_BROKER switch, for the pat lane's chip.
  // Optional — a caller with no switch value in scope (corp-network-proxy.tsx,
  // whose secret is never a git_pat) gets the 0.7.10 default (on), the same
  // fallback every other unwired surface takes.
  /** The WARDYN_GIT_PAT_BROKER switch; null = not known (loading, failed, or a viewer who cannot read it): the dialog then claims nothing about where a value or a Git token is held. */
  patBroker?: boolean | null;
  // The caller writes into the signed-in person's OWN namespace (PUT
  // /secrets/{name} stores under the caller, never another person), which any
  // signed-in person may do. Only the Secrets screen says so; every other
  // embedding keeps the operator gate below.
  ownRows?: boolean;
  // The namespace to write into, named by the route (the Secrets page passes the one its rows came from). Omitted,
  // PUT /secrets/{name} places the write by the caller's role, as every other embedding has always done.
  scope?: SecretScope;
}) {
  // This dialog is reused everywhere a secret gets written (this screen, the
  // SCM Provider step, the New Run wizard, the setup funnel) — gating its own
  // Save is the one chokepoint that covers all of them, so none of those
  // callers need their own copy of this check.
  const mayWrite = useOperator() || ownRows;
  const [name, setName] = React.useState(initialName);
  const [value, setValue] = React.useState("");
  // Masked at entry, revealed only on request: a write-only store should not
  // put the plaintext on screen while it is typed (shoulder-surf + screen
  // shares). -webkit-text-security is the only way to mask a multiline value
  // (PEM keys need the textarea); Firefox ignores it and degrades to
  // plaintext — cosmetic masking, not a security boundary either way.
  const [reveal, setReveal] = React.useState(false);
  const [error, setError] = React.useState<string | null>(null);
  const [saving, setSaving] = React.useState(false);
  // Require an explicit confirm-overwrite click when the typed name
  // already exists, so a save never silently clobbers a secret in use by runs.
  const [confirmOverwrite, setConfirmOverwrite] = React.useState(false);
  // A name the server already held although existingNames did not have it (the list was read before someone else
  // stored it): the save asks before replacing it, exactly as for a listed name.
  const [foundExisting, setFoundExisting] = React.useState("");

  React.useEffect(() => {
    if (open) {
      setFoundExisting("");
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
  const isOverwrite = existingNames.includes(trimmed) || (!!foundExisting && foundExisting === trimmed);
  const resolvedLane: Lane = lane ?? laneOfName(trimmed);
  // A blank-name open offers suggestions; it also says where model keys go, since those names are refused here.
  const suggest = !initialName && !lockName;
  // The User view's pointer names the "Your model connections" card, which is on the page only where the
  // deployment has a provider block (an empty one included); with none there is nowhere to point.
  const adminView = useAdminView();
  const hasModelConnections = useShellSetupStatus().status?.model_providers != null;
  const modelKeysHint = adminView ? SECRETS_PAGE.MODEL_KEYS_HINT.ADMIN : hasModelConnections ? SECRETS_PAGE.MODEL_KEYS_HINT.USER : undefined;
  const nameHint = lockName ? lockedNameHint(resolvedLane, !!host) : suggest ? modelKeysHint : undefined;
  // #381 F3: pat's meta depends on the real switch; app/ssh keep LANE_META.
  // An unknown switch gets a plain "PAT" chip: brokered or in-sandbox would be a claim.
  const resolvedLaneMeta =
    resolvedLane !== "pat" ? LANE_META[resolvedLane] : patBroker === null ? { ...patLaneMeta(true), label: "PAT", tooltip: "", tone: "neutral" as const } : patLaneMeta(patBroker);
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
      // Create-only unless the person has said to replace: the list behind isOverwrite can be stale.
      await secretsApi.setSecret(n, value, { scope, createOnly: !confirmOverwrite });
      // Clear the value immediately — never retain or echo it.
      setValue("");
      onOpenChange(false);
      onSaved?.(n);
    } catch (e) {
      if (e instanceof HttpError && e.status === 409 && e.reason === SECRET_EXISTS_REASON) {
        setFoundExisting(n);
        setSaving(false);
        return;
      }
      setError(getErrorMessage(e, "Failed to store secret."));
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
              : patBroker === null
                ? "The value is stored write-only: it is never returned by the API or shown again."
                : "The value is stored write-only: it is never returned by the API or shown again. It stays in Wardyn, unless a run delivers it as a variable or a file inside its sandbox."}
            {!lockName && patBroker === false && " With the token broker off, a stored Git token is handed to git inside the sandbox."}
          </DialogDescription>
        </DialogHeader>

        <div className="space-y-4 py-1">
          {/* Read-only fact, never an input, and the one place the host appears
              on screen — the hint below deliberately says "this host" rather
              than repeating it. */}
          {host && (
            <div className="flex items-center gap-2.5">
              <div className="flex size-8 shrink-0 items-center justify-center rounded-lg border border-border text-muted-foreground">
                <GitBranch className="size-4" />
              </div>
              <Mono className="text-sm text-foreground">{host}</Mono>
              <Chip tone={resolvedLaneMeta.tone} title={resolvedLaneMeta.tooltip}>
                {resolvedLaneMeta.label}
              </Chip>
            </div>
          )}
          <Field
            label="Name"
            htmlFor="secret-name"
            required
            hint={nameHint}
          >
            {/* Suggestions only on a blank-name open (a fresh "Add secret", not a
                rotate/fix-flow or locked open that already knows what it wants) —
                prefill the name only, never a value. */}
            {suggest && (
              <ProviderNameChips onPick={setName} />
            )}
            <Input
              id="secret-name"
              placeholder="npm-token"
              // Field wires its hint only to a single child; the chips make two.
              aria-describedby={nameHint ? fieldHintId("secret-name") : undefined}
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
          {/* Warn when the name already exists so the operator
              doesn't silently overwrite a secret currently referenced by runs. */}
          {isOverwrite && !error && <SecretOverwriteWarning name={trimmed} />}
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
          {!mayWrite && (
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
            disabled={!mayWrite || saving || !name.trim() || !value}
            aria-describedby={
              [error ? "add-secret-error" : undefined, !mayWrite ? "add-secret-operator-reason" : undefined]
                .filter(Boolean)
                .join(" ") || undefined
            }
            variant={isOverwrite && confirmOverwrite ? "destructive" : "default"}
          >
            {saving ? <Loader2 className="size-4 animate-spin" /> : <Lock className="size-4" />}
            {isOverwrite ? (confirmOverwrite ? "Overwrite secret" : "Save (overwrites)") : "Save secret"}
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}

// The warning before a save replaces a stored secret's value. Exported for the proxy setup's direct save, which asks
// the same question when the server finds its secret name already taken.
export function SecretOverwriteWarning({ name }: { name: string }) {
  return (
    <div className="flex items-start gap-2 rounded-lg border border-warning/30 bg-warning-subtle px-3 py-2 text-xs text-warning">
      <AlertTriangle className="mt-0.5 size-3.5 shrink-0" />
      <span>
        A secret named <span className="font-mono">{name}</span> already exists. Saving
        will overwrite its value for every run that references it. This cannot be undone.
      </span>
    </div>
  );
}

// Where a model key goes depends on the view: an admin's Settings has Model providers, a person's has their own
// model connections. The dialog is also rendered outside a router (component tests), where no URL names a view.
function useAdminView(): boolean {
  const routed = useInRouterContext();
  // `routed` cannot change while a tree stays mounted, so the same hook runs on every render.
  // eslint-disable-next-line react-hooks/rules-of-hooks
  return routed ? useConsoleMode() === "admin" : false;
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
