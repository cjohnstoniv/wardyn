/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// SCM provider Add-flow (Claude Design export, Page 5) — the two-panel
// App/PAT/SSH ladder (AddProviderPanel1 picks the host, AddProviderPanel2
// picks the credential rung). Getting Started's own per-host provider LIST
// step (ScmProviderStep) is gone — that configuration now lives on
// /integrations, whose "Add integration" dialog hands its SCM category off to
// this SAME ladder (see integrations/add-integration-dialog.tsx) rather than
// forking a second copy of it. That is also why there is no
// verify/test-connection control anywhere in this file: nothing server-side
// exists to verify against.
import * as React from "react";
import { AlertTriangle, KeyRound, Loader2 } from "lucide-react";
import { toast } from "sonner";
import { secrets as secretsApi } from "../../../lib/api/secrets";
import { getErrorMessage } from "../../../lib/format";
import { LANE_META, hostError, slugHost, type Lane } from "../../../lib/scm-provider";
import { Button } from "../../ui/button";
import { Input } from "../../ui/input";
import { Label } from "../../ui/label";
import { cn } from "../../ui/utils";
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
import { Chip } from "../../wardyn/primitives";

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
              </Rung>
              <p className="text-[0.6875rem] text-muted-foreground">{SSH_LIMIT}</p>
            </>
          )}
        </div>
        {/* Pinned to the row that actually performs the write (Done calls
            addHost — saving a credential above does not) rather than the PAT
            button above: that claimed the registration the moment you saved a
            secret, but closing the dialog (X/Esc → back to search) right
            after leaves the promise unfulfilled — the secret is stored, the
            host never is, and the integrations list shows it under a guessed
            hostname instead. */}
        {kind === "generic" && showHostedNote && (
          <p className="text-[0.6875rem] text-muted-foreground">
            Clicking Done also registers the hostname so runs can reach it — closing this dialog first leaves the
            credential saved but the host unregistered.
          </p>
        )}
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
