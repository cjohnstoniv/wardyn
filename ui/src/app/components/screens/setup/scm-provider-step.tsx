/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// SCM Provider step (Claude Design export, Page 5) — replaces the old fixed
// four-card ladder with a per-host provider list + a two-panel Add-provider
// dialog. A "provider row" is not a backend entity: Wardyn stores no such
// thing. Rows are a pure RESHAPING of data the console already has —
// status.secrets.present/github_app and siteConfig.scm_hosts — via
// lib/scm-provider.ts's deriveProviders. That is also why there is no
// verify/test-connection control anywhere in this file: nothing server-side
// exists to verify against.
import * as React from "react";
import {
  AlertTriangle,
  GitBranch,
  Info,
  KeyRound,
  Loader2,
  MoreHorizontal,
  Plus,
  RotateCw,
  Trash2,
} from "lucide-react";
import { toast } from "sonner";
import type { SetupStatus, SiteConfig } from "../../../lib/types";
import { secrets as secretsApi } from "../../../lib/api/secrets";
import { getErrorMessage } from "../../../lib/format";
import {
  LANE_META,
  LEGACY_NAMES,
  deriveProviders,
  hostError,
  laneOfName,
  slugHost,
  type Lane,
  type ProviderRow,
} from "../../../lib/scm-provider";
import { Button } from "../../ui/button";
import { Input } from "../../ui/input";
import { Label } from "../../ui/label";
import { cn } from "../../ui/utils";
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "../../ui/table";
import {
  DropdownMenu,
  DropdownMenuContent,
  DropdownMenuItem,
  DropdownMenuSeparator,
  DropdownMenuTrigger,
} from "../../ui/dropdown-menu";
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "../../ui/dialog";
import { RadioGroup, RadioGroupItem } from "../../ui/radio-group";
import { Field } from "../new-run/step-shell";
import { Mono } from "../../wardyn/code-block";
import { Chip, OperatorOnlyHint, SectionLabel } from "../../wardyn/primitives";
import { EmptyState } from "../../wardyn/states";
import { DeleteConfirmDialog } from "../../wardyn/delete-confirm-dialog";
import { AddSecretDialog } from "../secrets";
import { OPERATOR_ONLY_REASON } from "../../wardyn/copy";
import { useOperator } from "../../wardyn/operator-context";
import { CheckRow, RecheckButton, useSiteConfigStep } from "./step-bodies";

// Legacy pre-convention names map 1:1 onto the same four well-known SaaS hosts
// brandFor() already special-cases — GitHub/GitLab/Bitbucket are single-tenant
// in this taxonomy and Azure DevOps' cloud host is dev.azure.com (a self-hosted
// ADO Server gets its own hostname-derived name instead), so this is a fixed
// fact, not a guess. Deliberately NOT the design prototype's `n.replace(/-pat$/,
// "") + "-com"` heuristic — that mangles "ado-pat" to "ado-com" and
// "bitbucket-pat" to "bitbucket-com", neither of which is the real host.
const LEGACY_HOST: Record<string, string> = {
  "github-pat": "github.com",
  "gitlab-pat": "gitlab.com",
  "ado-pat": "dev.azure.com",
  "bitbucket-pat": "bitbucket.org",
};

export interface ProviderOption {
  id: string;
  title: string;
  hint: React.ReactNode;
  kind: "github" | "ado" | "generic";
  /** Fixed host for a well-known SaaS provider; absent means "hosted" — the
   *  operator types the hostname. */
  host?: string;
}

const PROVIDER_OPTIONS: ProviderOption[] = [
  { id: "github", title: "GitHub", hint: <><Mono>github.com</Mono> — App, fine-grained PAT, or SSH deploy key</>, kind: "github", host: "github.com" },
  { id: "ghes", title: "GHES", hint: "GitHub Enterprise Server — PAT", kind: "generic" },
  { id: "ado", title: "Azure DevOps", hint: <><Mono>dev.azure.com</Mono> — PAT or SSH</>, kind: "ado", host: "dev.azure.com" },
  { id: "ado-server", title: "ADO Server", hint: "self-hosted Azure DevOps — PAT", kind: "generic" },
  { id: "gitlab", title: "GitLab", hint: <><Mono>gitlab.com</Mono> — PAT</>, kind: "generic", host: "gitlab.com" },
  { id: "bitbucket", title: "Bitbucket", hint: <><Mono>bitbucket.org</Mono> — PAT</>, kind: "generic", host: "bitbucket.org" },
  { id: "other", title: "Other host", hint: "any git host — PAT", kind: "generic" },
];

// Verified, not under-claimed: validatePolicySpec REJECTS an ssh_key grant whose
// host isn't one of the two SSH-over-443 providers ("ssh_key host %q is not a
// supported SSH-over-443 provider", internal/api/policy.go:170, on every policy
// write path). known_hosts_secret_ref does not widen that — it only supplies
// known_hosts for those same hosts.
const SSH_LIMIT = "SSH lane covers github.com and dev.azure.com only. Use a PAT.";

type AddPanel =
  | { step: "panel1" }
  | { step: "panel2"; kind: ProviderOption["kind"]; host: string; title: string; showHostedNote: boolean };

// One confirm dialog serves every destructive action on this screen; `run` is
// the action itself so a target can delete secrets OR rewrite site-config
// without a second dialog. `run` throwing is how the dialog reports failure.
type DeleteTarget = {
  label: string;
  entity: string;
  description: React.ReactNode;
  run: () => Promise<void>;
};

export function ScmProviderStep({
  status,
  siteConfig,
  reloadSiteConfig,
  saveSiteConfig,
  onRecheck,
  rechecking,
}: {
  status: SetupStatus;
  siteConfig: SiteConfig | null;
  reloadSiteConfig: () => Promise<void>;
  saveSiteConfig: (next: SiteConfig) => Promise<void>;
  onRecheck: () => void;
  rechecking: boolean;
}) {
  const check = status.checks.find((c) => c.id === "scm_provider");
  const { saving, mutate } = useSiteConfigStep(reloadSiteConfig, saveSiteConfig);
  // Every write this step can reach — scm_hosts (site-config), the secret
  // store, and the GitHub App id/key — is operator-only. Gating "Add
  // provider" and every row action below is enough to make the whole
  // Panel1/Panel2 flow unreachable for a viewer, so its internals (Save App
  // ID, the per-rung CredCta buttons) don't need their own checks.
  const operator = useOperator();

  const addHost = (h: string) => {
    const hosts = Array.from(new Set([...(siteConfig?.scm_hosts ?? []), h]));
    return mutate({ ...(siteConfig ?? {}), scm_hosts: hosts }, "Failed to add the SCM host");
  };
  const withoutHost = (h: string): SiteConfig => ({
    ...(siteConfig ?? {}),
    scm_hosts: (siteConfig?.scm_hosts ?? []).filter((x) => x !== h),
  });

  const removeHostDirect = (h: string) => mutate(withoutHost(h), "Failed to remove the SCM host");

  // Removing a host that still has a credential does NOT remove its row — rows
  // are derived from the credentials themselves (a git-pat-/ssh-key- name via
  // deriveProviders' orphan fallback, or status.secrets.github_app), not from
  // this list. It looked like a dead button, so a credentialed row confirms
  // first and says what actually changes; a zero-credential row really does
  // disappear, so it stays one click.
  const requestRemoveHost = (row: ProviderRow) => {
    if (row.lanes.length === 0) {
      removeHostDirect(row.host);
      return;
    }
    setToDelete({
      label: row.host,
      entity: "SCM host",
      description: (
        <>
          Runs stop inheriting <Mono>{row.host}</Mono> in their egress allowlist. The stored
          credential is NOT deleted and this row stays: it is derived from the credential, not from
          this list.
        </>
      ),
      // saveSiteConfig (not mutate) so a failure REJECTS and the dialog reports
      // it, instead of mutate swallowing it into a second toast.
      run: () => saveSiteConfig(withoutHost(row.host)),
    });
  };

  // Every name is attempted even if an earlier one fails, and onRecheck ALWAYS
  // runs: a half-completed "Remove App" leaves the App ID gone and the private
  // key stored, and the row has to show that instead of a stale "App · brokered".
  const deleteSecrets = (names: string[]) => async () => {
    const results = await Promise.allSettled(names.map((n) => secretsApi.deleteSecret(n)));
    onRecheck();
    for (const r of results) if (r.status === "rejected") throw r.reason;
  };

  const rows = deriveProviders(status.secrets.present, siteConfig?.scm_hosts ?? [], status.secrets.github_app);
  const legacy = LEGACY_NAMES.filter((n) => status.secrets.present.includes(n));

  const [addPanel, setAddPanel] = React.useState<AddPanel | null>(null);
  const [secretDialog, setSecretDialog] = React.useState<{ name: string; host: string; lane: Lane } | null>(null);
  const [toDelete, setToDelete] = React.useState<DeleteTarget | null>(null);

  const openSecret = (name: string, host: string, lane: Lane) => setSecretDialog({ name, host, lane });

  const openAddForRow = (row: ProviderRow) => {
    const kind: ProviderOption["kind"] =
      row.host === "github.com" ? "github" : row.host === "dev.azure.com" ? "ado" : "generic";
    setAddPanel({ step: "panel2", kind, host: row.host, title: row.brand, showHostedNote: false });
  };

  return (
    <div className="space-y-5">
      <p className="text-sm leading-relaxed text-muted-foreground">
        One row per host. A credential is optional and never gates a launch — public repos clone
        without any credential. Storing one here doesn&apos;t attach it to a run: each run names the
        host and the stored secret in its own git_pat / ssh_key grant, in the New Run wizard&apos;s
        git-credential card.
      </p>

      {check && (
        <ul>
          <CheckRow check={check} />
        </ul>
      )}

      {status.scm?.gh_cli && (
        <p className="flex items-start gap-1.5 text-xs text-muted-foreground">
          <Info className="mt-0.5 size-3.5 shrink-0" />
          gh CLI login detected on the host — that token is broad; Wardyn never imports it. Prefer a
          fine-grained PAT.
        </p>
      )}

      {rows.length === 0 ? (
        <div className="rounded-xl border border-border">
          <EmptyState
            icon={GitBranch}
            title="No providers configured"
            description={
              operator
                ? "Public repos clone without any credential."
                : `Public repos clone without any credential. ${OPERATOR_ONLY_REASON}`
            }
            action={
              <Button onClick={() => setAddPanel({ step: "panel1" })} disabled={!operator}>
                <Plus className="size-4" /> Add provider
              </Button>
            }
          />
        </div>
      ) : (
        <>
          <div className="flex items-center gap-2">
            <SectionLabel>Providers</SectionLabel>
            <span className="flex-1" />
            {!operator && <Chip tone="neutral">{OPERATOR_ONLY_REASON}</Chip>}
            <Button size="sm" onClick={() => setAddPanel({ step: "panel1" })} disabled={!operator}>
              <Plus className="size-4" /> Add provider
            </Button>
          </div>
          <div className="overflow-hidden rounded-xl border border-border bg-card">
            <Table>
              <TableHeader>
                <TableRow className="hover:bg-transparent">
                  <TableHead>Provider</TableHead>
                  <TableHead>Credential</TableHead>
                  <TableHead className="w-[56px]" />
                </TableRow>
              </TableHeader>
              <TableBody>
                {rows.map((row) => (
                  <ProviderTableRow
                    key={row.host}
                    row={row}
                    presentSecrets={status.secrets.present}
                    onOpenSecret={openSecret}
                    onAddCredential={openAddForRow}
                    onRemoveHost={requestRemoveHost}
                    onDeleteRequest={setToDelete}
                    onDeleteSecrets={deleteSecrets}
                  />
                ))}
              </TableBody>
            </Table>
          </div>
        </>
      )}

      {legacy.length > 0 && (
        <LegacyFooter
          names={legacy}
          onReAdd={(n) => {
            const host = LEGACY_HOST[n];
            openSecret(`git-pat-${slugHost(host)}`, host, "pat");
          }}
          onDelete={(n) =>
            setToDelete({
              label: n,
              entity: "credential",
              description: (
                <>
                  <Mono>{n}</Mono> doesn&apos;t follow the <Mono>git-pat-&lt;slug&gt;</Mono>{" "}
                  convention, so this screen can&apos;t tell which host it is for — but a run&apos;s
                  git_pat grant can name any stored secret. If one names this, that run loses its
                  credential. The store is write-only: the value cannot be recovered.
                </>
              ),
              run: deleteSecrets([n]),
            })
          }
        />
      )}

      <RecheckButton onRecheck={onRecheck} rechecking={rechecking} />

      {addPanel?.step === "panel1" && (
        <AddProviderPanel1
          onCancel={() => setAddPanel(null)}
          onContinue={(opt, host) =>
            setAddPanel({ step: "panel2", kind: opt.kind, host, title: opt.title, showHostedNote: !opt.host })
          }
        />
      )}
      {addPanel?.step === "panel2" && (
        <AddProviderPanel2
          kind={addPanel.kind}
          host={addPanel.host}
          title={addPanel.title}
          showHostedNote={addPanel.showHostedNote}
          saving={saving}
          siteConfigLoaded={siteConfig !== null}
          onBack={() => setAddPanel({ step: "panel1" })}
          onClose={() => setAddPanel(null)}
          onDone={async () => {
            if (await addHost(addPanel.host)) setAddPanel(null);
          }}
          onOpenSecret={openSecret}
          onRecheck={onRecheck}
        />
      )}

      {secretDialog && (
        <AddSecretDialog
          open
          onOpenChange={(o) => !o && setSecretDialog(null)}
          initialName={secretDialog.name}
          lockName
          host={secretDialog.host}
          lane={secretDialog.lane}
          existingNames={status.secrets.present}
          onSaved={() => {
            setSecretDialog(null);
            onRecheck();
          }}
        />
      )}

      <DeleteConfirmDialog
        name={toDelete?.label ?? null}
        entity={toDelete?.entity ?? "credential"}
        description={toDelete?.description ?? ""}
        onOpenChange={(o) => !o && setToDelete(null)}
        onDelete={() => toDelete!.run()}
        onDeleted={() => setToDelete(null)}
      />
    </div>
  );
}

// One row per host: brand + host (Mono) + lane chips + a kebab whose actions
// depend on what the row already has. A row carrying an "app" lane offers only
// App actions (matches the design's fixed SCM_MENU_APP) — a PAT/SSH secret
// that happens to coexist on the same host stays reachable from the Secrets
// screen directly, not from this kebab.
function ProviderTableRow({
  row,
  presentSecrets,
  onOpenSecret,
  onAddCredential,
  onRemoveHost,
  onDeleteRequest,
  onDeleteSecrets,
}: {
  row: ProviderRow;
  presentSecrets: string[];
  onOpenSecret: (name: string, host: string, lane: Lane) => void;
  onAddCredential: (row: ProviderRow) => void;
  onRemoveHost: (row: ProviderRow) => void;
  onDeleteRequest: (target: DeleteTarget) => void;
  onDeleteSecrets: (names: string[]) => () => Promise<void>;
}) {
  const slug = slugHost(row.host);
  const isApp = row.lanes.includes("app");
  const patName = `git-pat-${slug}`;
  const sshName = `ssh-key-${slug}`;
  const credName = presentSecrets.includes(patName) ? patName : sshName;
  const operator = useOperator();

  return (
    <TableRow>
      <TableCell>
        <div className="flex flex-col gap-0.5">
          <span className="text-sm font-medium text-foreground">{row.brand}</span>
          <Mono className="text-[0.6875rem]">{row.host}</Mono>
          {/* The hostname above is a GUESS read back from the secret name (dots
              and hyphens both slug to "-", so it can differ from the real host).
              Say so, and never repeat it in destructive copy. */}
          {row.derivedFrom && (
            <span className="text-[0.6875rem] leading-snug text-muted-foreground">
              Not registered — read back from <Mono>{row.derivedFrom}</Mono>, so it may differ from
              the real hostname.
            </span>
          )}
        </div>
      </TableCell>
      <TableCell>
        <div className="flex flex-wrap gap-1.5">
          {row.lanes.length ? (
            row.lanes.map((lane, i) => (
              <Chip key={`${lane}-${i}`} tone={LANE_META[lane].tone} title={LANE_META[lane].tooltip}>
                {LANE_META[lane].label}
              </Chip>
            ))
          ) : (
            <Chip tone="neutral">No credential</Chip>
          )}
        </div>
      </TableCell>
      <TableCell className="text-right" onClick={(e) => e.stopPropagation()}>
        <DropdownMenu>
          <DropdownMenuTrigger asChild>
            <Button variant="ghost" size="icon" className="size-8" aria-label={`${row.host} actions`}>
              <MoreHorizontal className="size-4" />
            </Button>
          </DropdownMenuTrigger>
          <DropdownMenuContent align="end">
            {isApp ? (
              <>
                <DropdownMenuItem
                  onClick={() => onOpenSecret("github-app-key", row.host, "app")}
                  disabled={!operator}
                >
                  <RotateCw className="size-4" /> Rotate PEM
                  {!operator && <OperatorOnlyHint />}
                </DropdownMenuItem>
                <DropdownMenuItem
                  className="text-danger focus:text-danger"
                  disabled={!operator}
                  onClick={() =>
                    onDeleteRequest({
                      label: "GitHub App",
                      entity: "credential",
                      description: (
                        <>
                          Deletes both <Mono>github-app-id</Mono> and <Mono>github-app-key</Mono> —
                          the App&apos;s private key. Runs lose the brokered App credential; public
                          repos keep cloning, private clones fail until a credential is added. The
                          store is write-only: the key cannot be recovered.
                        </>
                      ),
                      run: onDeleteSecrets(["github-app-id", "github-app-key"]),
                    })
                  }
                >
                  <Trash2 className="size-4" /> Remove App
                  {!operator && <OperatorOnlyHint />}
                </DropdownMenuItem>
              </>
            ) : row.lanes.length > 0 ? (
              <>
                <DropdownMenuItem
                  onClick={() => onOpenSecret(credName, row.host, laneOfName(credName))}
                  disabled={!operator}
                >
                  <RotateCw className="size-4" /> Rotate credential
                  {!operator && <OperatorOnlyHint />}
                </DropdownMenuItem>
                <DropdownMenuItem
                  className="text-danger focus:text-danger"
                  disabled={!operator}
                  onClick={() =>
                    onDeleteRequest({
                      label: credName,
                      entity: "credential",
                      // Names the SECRET, never row.host — on a derived row that
                      // hostname is a guess this screen invented.
                      description: (
                        <>
                          Deletes the stored secret <Mono>{credName}</Mono>. Any run whose{" "}
                          {laneOfName(credName) === "ssh" ? "ssh_key" : "git_pat"} grant names it
                          loses that credential; public repos keep cloning, private clones fail
                          until it is replaced. The store is write-only: the value cannot be
                          recovered.
                        </>
                      ),
                      run: onDeleteSecrets([credName]),
                    })
                  }
                >
                  <Trash2 className="size-4" /> Delete credential
                  {!operator && <OperatorOnlyHint />}
                </DropdownMenuItem>
              </>
            ) : (
              <DropdownMenuItem onClick={() => onAddCredential(row)} disabled={!operator}>
                <Plus className="size-4" /> Add credential
                {!operator && <OperatorOnlyHint />}
              </DropdownMenuItem>
            )}
            <DropdownMenuSeparator />
            <DropdownMenuItem
              className="text-danger focus:text-danger"
              disabled={!operator}
              onClick={() => onRemoveHost(row)}
            >
              <Trash2 className="size-4" /> Remove host
              {!operator && <OperatorOnlyHint />}
            </DropdownMenuItem>
          </DropdownMenuContent>
        </DropdownMenu>
      </TableCell>
    </TableRow>
  );
}

function LegacyFooter({
  names,
  onReAdd,
  onDelete,
}: {
  names: string[];
  onReAdd: (name: string) => void;
  onDelete: (name: string) => void;
}) {
  const operator = useOperator();
  return (
    <div className="space-y-2.5 border-t border-border pt-4">
      <div className="flex items-center gap-2">
        <SectionLabel>Off-convention names</SectionLabel>
        {!operator && <Chip tone="neutral">{OPERATOR_ONLY_REASON}</Chip>}
      </div>
      <p className="text-[0.6875rem] leading-snug text-muted-foreground">
        Stored names that don&apos;t follow <Mono>git-pat-&lt;slug&gt;</Mono>, so this screen
        can&apos;t place them on a host. Still usable — a run&apos;s git_pat grant can name any
        stored secret. To get a row here instead, re-add the value under the conventional name (the
        store is write-only), then delete this one.
      </p>
      <div className="divide-y divide-border">
        {names.map((n) => (
          <div key={n} className="flex items-center gap-2.5 py-2">
            <Mono className="text-foreground">{n}</Mono>
            <span className="flex-1" />
            <Button variant="ghost" size="sm" onClick={() => onReAdd(n)} disabled={!operator}>
              Re-add under recognized name
            </Button>
            <Button
              variant="ghost"
              size="sm"
              className="text-danger hover:text-danger"
              onClick={() => onDelete(n)}
              disabled={!operator}
            >
              Delete
            </Button>
          </div>
        ))}
      </div>
    </div>
  );
}

// Panel 1 — pick a provider; a "hosted" pick (GHES / ADO Server / Other host)
// asks for a hostname and live-previews the git-pat-<slug> name it derives.
// Exported so the Integrations "Add integration" dialog can hand off its SCM
// category to the SAME ladder instead of forking a second copy of it (its own
// Panel 2/3 shape is AI-provider-specific and doesn't fit the App/PAT/SSH
// ladder anyway) — see integrations/add-integration-dialog.tsx.
export function AddProviderPanel1({
  onCancel,
  onContinue,
}: {
  onCancel: () => void;
  onContinue: (opt: ProviderOption, host: string) => void;
}) {
  const [selectedId, setSelectedId] = React.useState(PROVIDER_OPTIONS[0].id);
  const [hostDraft, setHostDraft] = React.useState("");
  const selected = PROVIDER_OPTIONS.find((p) => p.id === selectedId) ?? PROVIDER_OPTIONS[0];
  const hosted = !selected.host;
  const resolvedHost = selected.host ?? hostDraft.trim().toLowerCase();
  // Validated HERE, before Continue — panel 2 saves the secret first and only
  // writes scm_hosts on Done, so a host the server rejects (no dot, too long for
  // the derived name) otherwise dead-ends after the credential is already
  // stored, in a dialog whose Name field is read-only.
  const hostErr = hosted ? hostError(hostDraft) : null;

  return (
    <Dialog open onOpenChange={(o) => !o && onCancel()}>
      <DialogContent className="sm:max-w-xl">
        <DialogHeader>
          <DialogTitle>Add provider</DialogTitle>
          <DialogDescription>
            Pick where your repos live. A credential is optional — public repos clone without one.
          </DialogDescription>
        </DialogHeader>
        <div className="space-y-4 py-1">
          <RadioGroup
            value={selectedId}
            onValueChange={setSelectedId}
            aria-label="Provider"
            className="grid grid-cols-2 gap-2"
          >
            {PROVIDER_OPTIONS.map((p) => (
              // A div, not a label: the inner Label's htmlFor already associates
              // the title with the radio, and nesting a label inside a label is
              // invalid HTML that leaves the click target ambiguous.
              <div
                key={p.id}
                className={cn(
                  "flex cursor-pointer items-start gap-2.5 rounded-lg border p-2.5 transition-colors",
                  selectedId === p.id ? "border-primary bg-primary/10" : "border-border hover:border-border-strong",
                )}
              >
                <RadioGroupItem value={p.id} id={`prov-${p.id}`} className="mt-0.5" />
                <span>
                  <Label htmlFor={`prov-${p.id}`} className="cursor-pointer text-sm font-medium text-foreground">
                    {p.title}
                  </Label>
                  <span className="block text-[0.6875rem] leading-snug text-muted-foreground">{p.hint}</span>
                </span>
              </div>
            ))}
          </RadioGroup>
          {hosted && (
            <Field
              label="Host"
              htmlFor="scm-add-host"
              hint={
                hostErr ? (
                  <span className="text-danger">{hostErr}</span>
                ) : (
                  <>
                    Secret name{" "}
                    <Mono className="text-foreground">git-pat-{slugHost(hostDraft) || "<host-slug>"}</Mono> · Also
                    registers the hostname so runs can reach it.
                  </>
                )
              }
            >
              <Input
                id="scm-add-host"
                value={hostDraft}
                onChange={(e) => setHostDraft(e.target.value)}
                placeholder="ghes.example.com"
                aria-invalid={!!hostErr}
              />
            </Field>
          )}
        </div>
        <DialogFooter>
          <Button variant="outline" onClick={onCancel}>
            Cancel
          </Button>
          <Button
            onClick={() => onContinue(selected, resolvedHost)}
            disabled={hosted && (!hostDraft.trim() || !!hostErr)}
          >
            Continue
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}

// Panel 2 — the picked provider's credential rungs. GitHub's ladder is a FIXED
// order (App -> fine-grained PAT -> SSH deploy key); Azure DevOps gets PAT +
// SSH; everything else (GHES / ADO Server / GitLab / Bitbucket / Other host)
// shares one generic PAT-only rung, matching the reference prototype's actual
// branching (the static frames just showcase two illustrative instances of it).
export function AddProviderPanel2({
  kind,
  host,
  title,
  showHostedNote,
  saving,
  siteConfigLoaded,
  onBack,
  onClose,
  onDone,
  onOpenSecret,
  onRecheck,
}: {
  kind: ProviderOption["kind"];
  host: string;
  title: string;
  showHostedNote: boolean;
  saving: boolean;
  siteConfigLoaded: boolean;
  onBack: () => void;
  onClose: () => void;
  onDone: () => void;
  onOpenSecret: (name: string, host: string, lane: Lane) => void;
  onRecheck: () => void;
}) {
  const slug = slugHost(host);
  const [appId, setAppId] = React.useState("");
  const [savingAppId, setSavingAppId] = React.useState(false);

  const saveAppId = async () => {
    const id = appId.trim();
    if (!id) return;
    setSavingAppId(true);
    try {
      await secretsApi.setSecret("github-app-id", id);
      setAppId("");
      onRecheck();
    } catch (e) {
      toast.error("Failed to save App ID", { description: getErrorMessage(e) });
    } finally {
      setSavingAppId(false);
    }
  };

  return (
    <Dialog open onOpenChange={(o) => !o && onClose()}>
      <DialogContent className="sm:max-w-xl">
        <DialogHeader>
          <DialogTitle>Add provider — {title}</DialogTitle>
          <DialogDescription>
            <Mono className="text-foreground">{host}</Mono>
          </DialogDescription>
        </DialogHeader>
        <div className="space-y-3 py-1">
          {kind === "github" && (
            <>
              <Rung
                num="1"
                title="GitHub App"
                chips={
                  <Chip tone="primary" className="uppercase tracking-wide">
                    Recommended
                  </Chip>
                }
                tooltip={LANE_META.app.tooltip}
              >
                <p className="text-[0.6875rem] leading-snug text-muted-foreground">
                  The GitHub App is the only SCM credential Wardyn itself can expire: it mints a
                  brokered, ≤1h, contents-scoped token per run.
                </p>
                <Field label="App ID" htmlFor="scm-github-app-id">
                  <div className="flex flex-wrap items-center gap-2">
                    <Input
                      id="scm-github-app-id"
                      value={appId}
                      onChange={(e) => setAppId(e.target.value)}
                      placeholder="e.g. 1284951"
                      className="w-44 font-mono"
                    />
                    <Button size="sm" variant="outline" onClick={saveAppId} disabled={savingAppId || !appId.trim()}>
                      {savingAppId ? <Loader2 className="size-3.5 animate-spin" /> : "Save"}
                    </Button>
                    <span className="text-[0.6875rem] text-muted-foreground">
                      Secret name <Mono>github-app-id</Mono>
                    </span>
                  </div>
                </Field>
                <CredCta label="Add private key (PEM)" name="github-app-key" host={host} lane="app" onOpen={onOpenSecret} />
              </Rung>
              <Rung
                num="2"
                title="Fine-grained PAT"
                chips={<Chip tone={LANE_META.pat.tone}>{LANE_META.pat.label}</Chip>}
                tooltip={LANE_META.pat.tooltip}
              >
                <CredCta label="Add PAT" name={`git-pat-${slug}`} host={host} lane="pat" onOpen={onOpenSecret} />
              </Rung>
              <Rung
                num="3"
                title="SSH deploy key"
                chips={<Chip tone={LANE_META.ssh.tone}>{LANE_META.ssh.label}</Chip>}
                tooltip={LANE_META.ssh.tooltip}
              >
                <CredCta label="Add SSH key" name={`ssh-key-${slug}`} host={host} lane="ssh" onOpen={onOpenSecret} />
              </Rung>
              <div className="flex items-start gap-1.5 rounded-lg border border-warning/30 bg-warning-subtle p-2.5 text-warning">
                <AlertTriangle className="mt-0.5 size-3.5 shrink-0" />
                <p className="text-[0.6875rem] leading-snug">
                  A personal id_ed25519 or a classic PAT grants your whole account. Use the App, a
                  fine-grained PAT, or a per-repo deploy key.
                </p>
              </div>
            </>
          )}
          {kind === "ado" && (
            <>
              <Rung
                num="1"
                title="PAT"
                chips={
                  <span className="inline-flex items-center gap-1.5">
                    <Chip tone="primary" className="uppercase tracking-wide">
                      Recommended
                    </Chip>
                    <Chip tone={LANE_META.pat.tone}>{LANE_META.pat.label}</Chip>
                  </span>
                }
                tooltip={LANE_META.pat.tooltip}
              >
                <CredCta label="Add PAT" name={`git-pat-${slug}`} host={host} lane="pat" onOpen={onOpenSecret} />
              </Rung>
              <Rung
                num="2"
                title="SSH key"
                chips={<Chip tone={LANE_META.ssh.tone}>{LANE_META.ssh.label}</Chip>}
                tooltip={LANE_META.ssh.tooltip}
              >
                <CredCta label="Add SSH key" name={`ssh-key-${slug}`} host={host} lane="ssh" onOpen={onOpenSecret} />
              </Rung>
            </>
          )}
          {kind === "generic" && (
            <>
              <Rung
                num="1"
                title="PAT"
                chips={<Chip tone={LANE_META.pat.tone}>{LANE_META.pat.label}</Chip>}
                tooltip={LANE_META.pat.tooltip}
              >
                <CredCta label="Add PAT" name={`git-pat-${slug}`} host={host} lane="pat" onOpen={onOpenSecret} />
                {showHostedNote && (
                  <p className="text-[0.6875rem] text-muted-foreground">
                    Also registers the hostname so runs can reach it.
                  </p>
                )}
              </Rung>
              <p className="text-[0.6875rem] text-muted-foreground">{SSH_LIMIT}</p>
            </>
          )}
        </div>
        <DialogFooter>
          <Button variant="outline" onClick={onBack}>
            Back
          </Button>
          <Button onClick={onDone} disabled={saving || !siteConfigLoaded}>
            {saving ? <Loader2 className="size-4 animate-spin" /> : "Done"}
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}

function Rung({
  num,
  title,
  chips,
  tooltip,
  children,
}: {
  num: string;
  title: string;
  chips?: React.ReactNode;
  tooltip: string;
  children: React.ReactNode;
}) {
  return (
    <div className="space-y-2 rounded-lg border border-border p-3">
      <div className="flex flex-wrap items-center gap-2">
        <span className="font-mono text-[0.6875rem] font-semibold text-muted-foreground">{num}</span>
        <span className="text-sm font-medium text-foreground">{title}</span>
        {chips}
      </div>
      <p className="text-[0.6875rem] leading-snug text-muted-foreground">{tooltip}</p>
      {children}
    </div>
  );
}

function CredCta({
  label,
  name,
  host,
  lane,
  onOpen,
}: {
  label: string;
  name: string;
  host: string;
  lane: Lane;
  onOpen: (name: string, host: string, lane: Lane) => void;
}) {
  return (
    <div className="flex flex-wrap items-center gap-2.5">
      <Button size="sm" variant="outline" onClick={() => onOpen(name, host, lane)}>
        <KeyRound className="size-3.5" /> {label}
      </Button>
      <span className="text-[0.6875rem] text-muted-foreground">
        Secret name <Mono className="text-foreground">{name}</Mono>
      </span>
    </div>
  );
}
