/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The two connection cards — Model provider and Git host.
//
// These replace the /integrations page and its 931-line Add dialog. The old
// surface asked an abstract question ("which of seven integration kinds?") in a
// catalog; these ask the two concrete ones an operator actually has: what runs
// my agent, and how do you clone my private repos. Each card is a radio group
// over real lanes, not a list you add rows to.
//
// ONE component, rendered in TWO places: /settings and the Getting Started
// "Connect your model" step. That is deliberate — the funnel step used to be a
// thin embed of the whole Integrations page, which is exactly how it drifted
// into showing an operator-extensibility framework during first-run setup.
// Sharing the component makes drift impossible rather than merely discouraged.
//
// Reads go through deriveIntegrations (lib/api/integrations.ts) — the SAME
// derivation lib/readiness.ts uses, so a lane that reads "Connected" here can
// never disagree with the app-shell chip or the Runs first-run checklist. The
// wire-shaped genericIntegrations model dies with the page it was built for.
//
// Writes reuse what already exists: secrets.setSecret for every key/token lane
// (a credential IS a named secret server-side), and harnessAuth for the two
// interactive logins.
import * as React from "react";
import { toast } from "sonner";
import { Check, Loader2 } from "lucide-react";
import { deriveIntegrations, type IntegrationRow } from "../../../lib/api/integrations";
import { harnessAuth } from "../../../lib/api/harness-auth";
import { secrets as secretsApi } from "../../../lib/api/secrets";
import { hostError, slugHost } from "../../../lib/scm-provider";
import { getErrorMessage } from "../../../lib/format";
import type { SetupStatus, SiteConfig } from "../../../lib/types";
import { Button } from "../../ui/button";
import { Input } from "../../ui/input";
import { Dialog, DialogContent, DialogTitle } from "../../ui/dialog";
import { Field } from "../../wardyn/form-primitives";
import { Mono } from "../../wardyn/code-block";
import { OperatorOnlyHint } from "../../wardyn/primitives";
import { useOperator } from "../../wardyn/operator-context";
import { cn } from "../../ui/utils";
import { HarnessLoginPane } from "./harness-login-pane";

// Canon strings (local/ux-0.5-mock/CANON-STRINGS.md § Settings). Kept here
// rather than in lib/integrations.ts's T, which belongs to the page being
// deleted and shrinks with it.
export const S = {
  MODEL_TITLE: "Model provider",
  MODEL_LEDE: "Agent runs need one. Governed commands don't.",
  // The old footer said "Keys never enter the sandbox" full stop, which is true
  // of the subscription lane, both api-key lanes, and Bedrock's BEARER key — the
  // proxy injects a static header on the wire for all four. It is NOT true of
  // this card's Bedrock SSO lane: AWS signs each request with SigV4 in-process,
  // so there is no header to swap and the credential is materialized inside the
  // sandbox (resolveBedrockAuth's ssoInject branch, runs_bedrock.go — and the
  // aws-access-key-id fallback below it). ARCHITECTURE.md invariant 1 names the
  // same exception. A blanket promise the card's own third lane breaks is worse
  // than a longer sentence.
  MODEL_FOOTER:
    "The egress proxy injects these on the wire, so keys never enter the sandbox — except Bedrock's SSO lane, where AWS credentials sign inside it.",
  GIT_TITLE: "Git host",
  GIT_LEDE: "How Wardyn clones your private repos.",
  // The three lanes differ in WHERE the credential goes, and the footer owns
  // that split so no lane has to overclaim: only the App lane keeps the token
  // outside the sandbox (proxy broker); a PAT or SSH key enters it for the
  // clone — helper stdout and a shredded 0400 file respectively
  // (ARCHITECTURE.md invariant 1 and the git-egress table are the source).
  GIT_FOOTER:
    "Only the GitHub App lane keeps its token outside the sandbox — a PAT or SSH key enters it for the clone, then is wiped. Public repos clone with no credential at all.",
  // Bedrock's region/model are OPERATOR BOOT-TIME config (runs_bedrock.go:116),
  // not writable over the API — so the card states where they come from instead
  // of offering an input the server would ignore. The mock implied they were
  // editable here; the runtime disagrees, and the runtime wins.
  BEDROCK_CONFIG_NOTE:
    "Region and model come from the daemon's own config (WARDYN_BEDROCK_REGION / WARDYN_BEDROCK_MODEL) — set them where wardynd starts, not here.",
  STORE_NOTE: "Wardyn stores this — it doesn't dial the provider to check it.",
} as const;

// ---------------------------------------------------------------------------
// Card shell + lane rows
// ---------------------------------------------------------------------------

function Card({
  title,
  lede,
  footer,
  children,
}: {
  title: string;
  lede: string;
  footer?: string;
  children: React.ReactNode;
}) {
  return (
    <section className="rounded-xl border border-border bg-surface-1 p-4">
      <h3 className="text-sm font-medium text-foreground">{title}</h3>
      <p className="mt-0.5 text-body leading-snug text-muted-foreground">{lede}</p>
      <div className="mt-3 space-y-2">{children}</div>
      {footer && <p className="mt-3 text-meta leading-snug text-muted-foreground">{footer}</p>}
    </section>
  );
}

// One lane. Selecting it expands its form; a connected lane shows its state and
// a Disconnect instead. Radio semantics (not aria-pressed) because these are
// mutually-exclusive choices within one group, which is what a screen reader
// needs to announce "2 of 3".
function Lane({
  id,
  title,
  hint,
  connected,
  connectedDetail,
  selected,
  onSelect,
  children,
}: {
  id: string;
  title: string;
  hint: string;
  connected: boolean;
  connectedDetail?: string;
  selected: boolean;
  onSelect: () => void;
  children?: React.ReactNode;
}) {
  return (
    <div
      className={cn(
        "rounded-lg border transition-colors",
        selected ? "border-primary bg-primary/5" : "border-border",
      )}
    >
      <button
        type="button"
        role="radio"
        aria-checked={selected}
        id={id}
        onClick={onSelect}
        className="flex w-full items-start gap-2.5 p-3 text-left"
      >
        <span
          aria-hidden="true"
          className={cn(
            "mt-0.5 grid size-4 shrink-0 place-items-center rounded-full border",
            selected ? "border-primary" : "border-border-strong",
          )}
        >
          {selected && <span className="size-2 rounded-full bg-primary" />}
        </span>
        <span className="min-w-0 flex-1">
          <span className="flex items-center gap-1.5">
            <span className="text-sm font-medium text-foreground">{title}</span>
            {connected && (
              <span className="inline-flex items-center gap-0.5 rounded-md border border-ok/25 bg-ok-subtle px-1.5 py-px text-meta font-medium text-ok">
                <Check className="size-2.5" />
                Connected
              </span>
            )}
          </span>
          <span className="mt-0.5 block text-meta leading-snug text-muted-foreground">
            {connected && connectedDetail ? connectedDetail : hint}
          </span>
        </span>
      </button>
      {selected && children && <div className="border-t border-border px-3 py-3">{children}</div>}
    </div>
  );
}

// A one-secret lane form: a single write-only value + Save, and Disconnect once
// stored. Every key/token lane in both cards is this shape.
function SecretLane({
  label,
  placeholder,
  hint,
  secretName,
  stored,
  onChanged,
  extra,
  summary,
  disabled,
}: {
  label: string;
  placeholder: string;
  hint?: React.ReactNode;
  secretName: string;
  stored: boolean;
  onChanged: () => void;
  /** Rendered above the value field while editing (e.g. the Host input). */
  extra?: React.ReactNode;
  /** Rendered INSTEAD of the form once stored — the facts worth keeping on
   *  screen (which host, which secret name) without an input to mistake for
   *  unsaved work. */
  summary?: React.ReactNode;
  disabled?: boolean;
}) {
  const [value, setValue] = React.useState("");
  const [busy, setBusy] = React.useState(false);
  // Only ever true after an explicit Replace: a stored secret is write-only, so
  // the field starts hidden rather than empty-and-ambiguous.
  const [editing, setEditing] = React.useState(false);

  const save = async () => {
    setBusy(true);
    try {
      await secretsApi.setSecret(secretName, value.trim());
      setValue("");
      setEditing(false);
      toast.success(`Saved ${secretName}`);
      onChanged();
    } catch (e) {
      toast.error(getErrorMessage(e));
    } finally {
      setBusy(false);
    }
  };

  const disconnect = async () => {
    setBusy(true);
    try {
      await secretsApi.deleteSecret(secretName);
      toast.success(`Removed ${secretName}`);
      onChanged();
    } catch (e) {
      toast.error(getErrorMessage(e));
    } finally {
      setBusy(false);
    }
  };

  // A STORED secret shows a summary, not an empty box. The box was read as
  // "your key didn't save" — the value is write-only, so there is nothing to
  // prefill it with, and an always-visible empty field next to a "Connected"
  // badge is a straight contradiction. Replace reveals it deliberately.
  if (stored && !editing) {
    return (
      <div className="space-y-2">
        {summary}
        <div className="flex items-center gap-2">
          <Button size="sm" variant="secondary" disabled={disabled} onClick={() => setEditing(true)}>
            Replace
          </Button>
          <Button size="sm" variant="ghost" disabled={disabled || busy} onClick={disconnect}>
            {busy && <Loader2 className="size-3.5 animate-spin" />}
            Disconnect
          </Button>
          <span className="text-meta text-muted-foreground">
            Stored as <Mono>{secretName}</Mono>
          </span>
        </div>
      </div>
    );
  }

  return (
    <div className="space-y-3">
      {extra}
      <Field label={label} htmlFor={`v-${secretName}`} hint={hint}>
        <Input
          id={`v-${secretName}`}
          type="password"
          autoComplete="off"
          value={value}
          placeholder={placeholder}
          disabled={disabled || busy}
          onChange={(e) => setValue(e.target.value)}
        />
      </Field>
      <div className="flex items-center gap-2">
        <Button size="sm" disabled={disabled || busy || !value.trim()} onClick={save}>
          {busy && <Loader2 className="size-3.5 animate-spin" />}
          {stored ? "Save replacement" : "Save"}
        </Button>
        {stored && (
          <Button
            size="sm"
            variant="ghost"
            disabled={busy}
            onClick={() => {
              setValue("");
              setEditing(false);
            }}
          >
            Cancel
          </Button>
        )}
        <span className="text-meta text-muted-foreground">
          {stored ? (
            <>
              Replaces <Mono>{secretName}</Mono>
            </>
          ) : (
            <>
              Stored as <Mono>{secretName}</Mono>
            </>
          )}
        </span>
      </div>
    </div>
  );
}

// The one fact a stored git credential still needs on screen: which host it
// clones. (The secret name rides on the Replace/Disconnect row below it.)
function HostSummary({ host }: { host: string }) {
  return (
    <p className="text-body text-muted-foreground">
      Clones <Mono>{host}</Mono> over an injected credential — the value itself is write-only and never read back.
    </p>
  );
}

// ---------------------------------------------------------------------------
// Model provider
// ---------------------------------------------------------------------------

type ModelLane = "subscription" | "api_key" | "bedrock";

function rowFor(rows: IntegrationRow[], match: (r: IntegrationRow) => boolean): IntegrationRow | undefined {
  return rows.find(match);
}

export function ModelProviderCard({
  status,
  siteConfig,
  onChanged,
}: {
  status: SetupStatus;
  siteConfig: SiteConfig | null;
  onChanged: () => void;
}) {
  const operator = useOperator();
  const present = status.secrets.present;
  const ai = deriveIntegrations(status, siteConfig, present).ai;

  const subRow = rowFor(ai, (r) => r.aiType === "anthropic_subscription");
  const keyRow = rowFor(ai, (r) => r.aiType === "anthropic_api_key" || r.aiType === "openai_api_key");
  const bedrockRow = rowFor(ai, (r) => r.aiType === "bedrock");

  // Open on whatever is already connected, so the card reads as a state first
  // and a form second. Falls back to the lane most operators want.
  const [lane, setLane] = React.useState<ModelLane>(
    subRow ? "subscription" : keyRow ? "api_key" : bedrockRow ? "bedrock" : "subscription",
  );
  const [loginOpen, setLoginOpen] = React.useState<"anthropic" | "aws" | null>(null);
  const [busy, setBusy] = React.useState(false);

  const disconnectHarness = async (provider: string) => {
    setBusy(true);
    try {
      await harnessAuth.harnessDisconnect(provider);
      toast.success("Disconnected");
      onChanged();
    } catch (e) {
      toast.error(getErrorMessage(e));
    } finally {
      setBusy(false);
    }
  };

  // The managed (container-login) subscription is the only lane Wardyn can
  // disconnect — a host-CLI login lives in the operator's own ~/.claude and is
  // not Wardyn's to revoke, so the lane says so rather than offering a button
  // that would silently do nothing.
  const managedSub = !!subRow && !subRow.hostCli;
  const bedrockConfigured = !!status.bedrock?.region && !!status.bedrock?.model;
  // Older daemons omit the field; absent reads as ALLOWED so this console keeps
  // working against them. The daemon is the enforcement point either way — the UI
  // only decides whether to offer a lane that would fail.
  const sharedSubBlocked = status.auth.shared_subscription_allowed === false;

  return (
    <>
      <Card title={S.MODEL_TITLE} lede={S.MODEL_LEDE} footer={S.MODEL_FOOTER}>
        {!operator && <OperatorOnlyHint />}
        <div role="radiogroup" aria-label={S.MODEL_TITLE} className="space-y-2">
          <Lane
            id="lane-subscription"
            title="Claude subscription"
            hint="Sign in through a throwaway sandbox — the token never touches disk."
            connected={!!subRow}
            connectedDetail={subRow?.name}
            selected={lane === "subscription"}
            onSelect={() => setLane("subscription")}
          >
            {subRow ? (
              <div className="flex items-center gap-2">
                <span className="text-body text-muted-foreground">
                  {managedSub
                    ? "Captured through a login sandbox and stored by Wardyn."
                    : "A login in this host's own Claude CLI — Wardyn reads it, but can't revoke it. Sign out with the CLI itself."}
                </span>
                {managedSub && (
                  <Button
                    size="sm"
                    variant="ghost"
                    disabled={!operator || busy}
                    onClick={() => disconnectHarness("anthropic")}
                  >
                    Disconnect
                  </Button>
                )}
              </div>
            ) : sharedSubBlocked ? (
              // Not merely disabled: a greyed-out button reads as "you lack
              // permission". The deployment itself cannot use this lane, so say so
              // and point at the two that work.
              <p className="text-body text-muted-foreground">
                Unavailable in this deployment — {status.auth.shared_subscription_reason}
              </p>
            ) : (
              <Button size="sm" disabled={!operator} onClick={() => setLoginOpen("anthropic")}>
                Sign in
              </Button>
            )}
          </Lane>

          <Lane
            id="lane-api-key"
            title="API key"
            hint="ANTHROPIC_API_KEY or OPENAI_API_KEY."
            connected={!!keyRow}
            connectedDetail={keyRow?.name}
            selected={lane === "api_key"}
            onSelect={() => setLane("api_key")}
          >
            <div className="space-y-4">
              <SecretLane
                label="Anthropic API key"
                placeholder="sk-ant-…"
                secretName="anthropic-api-key"
                stored={present.includes("anthropic-api-key")}
                disabled={!operator}
                onChanged={onChanged}
                hint={S.STORE_NOTE}
              />
              <SecretLane
                label="OpenAI API key"
                placeholder="sk-…"
                secretName="openai-api-key"
                stored={present.includes("openai-api-key")}
                disabled={!operator}
                onChanged={onChanged}
              />
            </div>
          </Lane>

          <Lane
            id="lane-bedrock"
            title="AWS Bedrock"
            hint="A bearer key, or an SSO device-code sign-in."
            connected={!!bedrockRow}
            connectedDetail={
              bedrockConfigured ? `${status.bedrock?.region} · ${status.bedrock?.model}` : undefined
            }
            selected={lane === "bedrock"}
            onSelect={() => setLane("bedrock")}
          >
            <div className="space-y-4">
              <p className="text-meta leading-snug text-muted-foreground">
                {bedrockConfigured ? (
                  <>
                    Region <Mono>{status.bedrock?.region}</Mono> · model <Mono>{status.bedrock?.model}</Mono>.{" "}
                    {S.BEDROCK_CONFIG_NOTE}
                  </>
                ) : (
                  S.BEDROCK_CONFIG_NOTE
                )}
              </p>
              <SecretLane
                label="Bedrock bearer key"
                placeholder="Bearer token"
                secretName="bedrock-api-key"
                stored={present.includes("bedrock-api-key")}
                disabled={!operator}
                onChanged={onChanged}
              />
              <div className="flex items-center gap-2">
                <Button size="sm" variant="secondary" disabled={!operator} onClick={() => setLoginOpen("aws")}>
                  Sign in with SSO
                </Button>
                <span className="text-meta text-muted-foreground">
                  Device-code flow in a throwaway sandbox — exchanged per run for short-lived role credentials.
                </span>
              </div>
            </div>
          </Lane>
        </div>
      </Card>

      {/* This dialog hosts a live PTY, which makes it unlike every other dialog
          in the console, in three ways worth spelling out:

          1. NO TRANSFORM. DialogContent centres itself with
             translate(-50%,-50%), and a transformed ancestor becomes the
             containing block for `position: fixed` DESCENDANTS. AttachTerminal's
             fullscreen is `fixed inset-0`, so inside the default dialog it
             rendered at the dialog's size instead of the viewport's — the
             fullscreen button silently did almost nothing. Measured: a host
             with `transform: translate(0,0)` gives a `fixed inset-0` child
             512px; `transform: none` gives it the full 2548px viewport. Note
             `transform-none`, NOT `translate-x-0` — a zeroed translate is still
             a transform and still traps `fixed`. Centring is inset-0 + m-auto
             + h-fit, which needs no transform at all.
          2. WIDER via an inline `style`, not a class. The base carries
             `w-full` and `sm:max-w-lg`; a competing `max-w-*` class is the same
             specificity, so which one wins is decided by utility order in the
             compiled stylesheet — measured, `sm:max-w-lg` won both `max-w-3xl`
             and `sm:max-w-[72rem]`. An inline style beats every class, so the
             width is a fact rather than a race.
          3. `min-w-0` on the pane. DialogContent is a GRID, and a grid item
             defaults to `min-width: auto`, so it refuses to shrink below its
             content's min-content width. The login terminal is pinned to 512
             columns (LOGIN_PTY_COLS — a wrapped OAuth URL breaks the login), so
             without this the terminal shoved the dialog past the viewport edge
             and painted over the page. */}
      <Dialog open={loginOpen !== null} onOpenChange={(o) => !o && setLoginOpen(null)}>
        <DialogContent
          className="scroll-thin inset-0 top-0 left-0 m-auto h-fit max-h-[92vh] overflow-y-auto"
          // `translate` and `transform` are SEPARATE CSS properties in Tailwind
          // v4: translate-x-[-50%] emits `translate: -50% -50%`, which
          // `transform: none` does not reset. Left applied it shifted this
          // dialog half its own size up and to the left of where margin:auto
          // had centred it — measured left 122px against a computed
          // margin-left of 698px. Cleared here, where nothing can outrank it.
          style={{
            width: "min(96vw, 72rem)",
            maxWidth: "min(96vw, 72rem)",
            translate: "none",
            transform: "none",
          }}
        >
          <DialogTitle>{loginOpen === "aws" ? "Sign in with AWS SSO" : "Sign in to Claude"}</DialogTitle>
          {loginOpen && (
            <div className="min-w-0">
              <HarnessLoginPane
                provider={loginOpen}
                onDone={() => {
                  setLoginOpen(null);
                  onChanged();
                }}
                onCancel={() => setLoginOpen(null)}
              />
            </div>
          )}
        </DialogContent>
      </Dialog>
    </>
  );
}

// ---------------------------------------------------------------------------
// Git host
// ---------------------------------------------------------------------------

type GitLane = "pat" | "ssh" | "app";

export function GitHostCard({
  status,
  siteConfig,
  onChanged,
}: {
  status: SetupStatus;
  siteConfig: SiteConfig | null;
  onChanged: () => void;
}) {
  const operator = useOperator();
  const present = status.secrets.present;
  const scm = deriveIntegrations(status, siteConfig, present).scm;

  // Most installs have exactly one git host; the field exists so the lanes can
  // name a secret per host (git-pat-<slug>) rather than pretending there is a
  // single global credential.
  const [host, setHost] = React.useState(scm[0]?.typeLabel || "github.com");
  const slug = slugHost(host || "github.com");
  // Mirrors the server's own validSiteHost rule (site_config.go) AND the secret
  // store's 128-char name limit, checked BEFORE the write. Without it a
  // shape-invalid host stores its credential and only fails later when the
  // scm_hosts write 400s — the credential already saved under a name nothing
  // will ever read.
  const hostErr = hostError(host);

  const patName = `git-pat-${slug}`;
  const sshName = `ssh-key-${slug}`;
  const appStored = status.secrets.github_app;

  const [lane, setLane] = React.useState<GitLane>(
    present.includes(sshName) ? "ssh" : appStored ? "app" : "pat",
  );

  const hostField = (
    <Field
      label="Host"
      htmlFor="git-host"
      hint={hostErr ?? "The git host these credentials are for."}
    >
      <Input
        id="git-host"
        value={host}
        placeholder="github.com"
        disabled={!operator}
        aria-invalid={!!hostErr}
        onChange={(e) => setHost(e.target.value.trim())}
      />
    </Field>
  );

  return (
    <Card title={S.GIT_TITLE} lede={S.GIT_LEDE} footer={S.GIT_FOOTER}>
      {!operator && <OperatorOnlyHint />}
      <div role="radiogroup" aria-label={S.GIT_TITLE} className="space-y-2">
        <Lane
          id="lane-pat"
          title="Personal access token"
          // Was "injected on the wire per run" — FALSE. Wire injection is the
          // GitHub App broker's mechanism; git-over-HTTPS to a PAT host is an
          // opaque CONNECT tunnel with no header to swap (ARCHITECTURE.md "Git
          // egress: two mechanisms"). A PAT is minted to wardyn-git-helper
          // INSIDE the sandbox, which hands it to git on stdout — so the value
          // does transit the sandbox during git operations, and the lane must
          // say so rather than borrowing the App lane's stronger promise.
          hint="The simplest lane — stored once; a per-run helper hands it to git inside the sandbox at clone time."
          connected={present.includes(patName)}
          connectedDetail={`${host} · stored as ${patName}`}
          selected={lane === "pat"}
          onSelect={() => setLane("pat")}
        >
          <SecretLane
            label="Access token"
            placeholder="ghp_…"
            secretName={patName}
            stored={present.includes(patName)}
            disabled={!operator || !!hostErr}
            onChanged={onChanged}
            extra={hostField}
            summary={<HostSummary host={host} />}
            hint={S.STORE_NOTE}
          />
        </Lane>

        <Lane
          id="lane-ssh"
          title="SSH key"
          hint="A per-run copy is written inside the sandbox for the clone, then shredded."
          connected={present.includes(sshName)}
          connectedDetail={`${host} · stored as ${sshName}`}
          selected={lane === "ssh"}
          onSelect={() => setLane("ssh")}
        >
          <SecretLane
            label="Private key"
            placeholder="-----BEGIN OPENSSH PRIVATE KEY-----"
            secretName={sshName}
            stored={present.includes(sshName)}
            disabled={!operator || !!hostErr}
            onChanged={onChanged}
            extra={hostField}
            summary={<HostSummary host={host} />}
          />
        </Lane>

        <Lane
          id="lane-app"
          title="GitHub App"
          hint="Repo-scoped tokens brokered at the proxy — the token never enters the sandbox."
          connected={appStored}
          connectedDetail="Installation credentials stored"
          selected={lane === "app"}
          onSelect={() => setLane("app")}
        >
          <div className="space-y-4">
            <SecretLane
              label="App ID"
              placeholder="123456"
              secretName="github-app-id"
              stored={present.includes("github-app-id")}
              disabled={!operator}
              onChanged={onChanged}
            />
            <SecretLane
              label="Private key (PEM)"
              placeholder="-----BEGIN RSA PRIVATE KEY-----"
              secretName="github-app-key"
              stored={present.includes("github-app-key")}
              disabled={!operator}
              onChanged={onChanged}
            />
          </div>
        </Lane>
      </div>
    </Card>
  );
}
