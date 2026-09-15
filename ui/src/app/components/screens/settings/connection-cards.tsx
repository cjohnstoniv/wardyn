/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The Model provider connection card, plus the Lane/SecretLane/HostSummary
// primitives the Workspace Providers Git tab reuses for ITS credential lanes
// (0.7.2; screens/providers/git-tab.tsx).
//
// This used to also carry GitHostCard, the Git host card — retired in 0.7.2
// (workspace-providers-prompt.md §2.1, Q4): its free-text Host field could
// store a git-pat-<slug> for a host no provider admitted, a credential that
// clones nothing. The three git Lanes it rendered move INTO a provider row on
// /providers unchanged; this file keeps the shared shell (Lane/SecretLane/
// HostSummary) EXPORTED rather than re-typed there.
//
// ONE component, rendered in TWO places: /settings and the Getting Started
// "Secrets" step. That is deliberate — the funnel step used to be a thin embed
// of the whole Integrations page, which is exactly how it drifted into showing
// an operator-extensibility framework during first-run setup. Sharing the
// component makes drift impossible rather than merely discouraged.
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
import { getErrorMessage } from "../../../lib/format";
import { useDeferredBusy } from "../../../lib/use-deferred-busy";
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
  // DRAFT (M2 canon pending) — F4 (Appendix A #4), console-login lane (0.7.3).
  // A per_user Bedrock row is declared on the Agents tab, and its sign-in is
  // per person (F5 below reads the CALLER's own model_access, not a
  // deployment-wide fact) — this note says so rather than leaving the card
  // silent about where the lane actually lives.
  BEDROCK_PER_USER_NOTE:
    "This lane is per person: it is declared on the Agents tab, and each person signs in to AWS themselves. What this card reads is your own sign-in, not the deployment's.",
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
    <section className="rounded-xl border border-border bg-card p-4">
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
export function Lane({
  id,
  title,
  hint,
  connected,
  connectedDetail,
  selected,
  onSelect,
  disabled,
  children,
}: {
  id: string;
  title: string;
  hint: string;
  connected: boolean;
  connectedDetail?: string;
  selected: boolean;
  onSelect: () => void;
  /** Not selectable, and so never expandable — the Git tab passes this when the
   *  row names no host to key a credential by (git-tab.tsx's `host`). A lane
   *  that cannot be opened cannot Save, which is the point: the alternative was
   *  a Save that wrote the secret of a DIFFERENT host. */
  disabled?: boolean;
  children?: React.ReactNode;
}) {
  return (
    <div
      className={cn(
        "rounded-lg border transition-colors",
        selected ? "border-primary bg-primary/5" : "border-border",
        disabled && "opacity-60",
      )}
    >
      <button
        type="button"
        role="radio"
        aria-checked={selected}
        id={id}
        disabled={disabled}
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
      {selected && !disabled && children && <div className="border-t border-border px-3 py-3">{children}</div>}
    </div>
  );
}

// A one-secret lane form: a single write-only value + Save, and Disconnect once
// stored. Every key/token lane in both cards is this shape.
export function SecretLane({
  label,
  placeholder,
  hint,
  secretName,
  stored,
  onChanged,
  extra,
  summary,
  disabled,
  saveVariant = "default",
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
  /** Save/Save replacement's Button variant. Default "default" (teal) keeps
   *  today's ModelProviderCard/Secrets-step behaviour unchanged; the
   *  Workspace Providers Git tab passes "secondary" — that screen's one teal
   *  is its own Save providers button (CONSOLE-RULES §6), so a lane's own
   *  Save must not compete with it. */
  saveVariant?: "default" | "secondary";
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
        <Button size="sm" variant={saveVariant} disabled={disabled || busy || !value.trim()} onClick={save}>
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
export function HostSummary({ host }: { host: string }) {
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
  const { disabled: harnessBusy, showSpinner: harnessSpinning } = useDeferredBusy(busy);

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

  // F2/F4/F5 (Appendix A): the claude-code roster row is per_user Bedrock SSO
  // — declared on the Agents tab, where each person signs in for themselves.
  // Same derivation the Agents tab uses (agents-tab.tsx), read independently
  // here on purpose: that screen reads the DRAFT being edited, this card
  // reads the server's settled answer.
  const perUserSso = !!status.harnesses?.some(
    (h) => h.id === "claude-code" && h.mechanism === "bedrock_sso" && h.credential_source === "per_user",
  );
  const modelAccessState = status.model_access?.state;
  // `expiring` still counts as Connected — the session still signs, and the
  // warning rides the action line, not this badge.
  const perUserLive = modelAccessState === "live" || modelAccessState === "expiring";

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
                    disabled={!operator || harnessBusy}
                    onClick={() => disconnectHarness("anthropic")}
                  >
                    {/* Icon slot always renders — toggling `invisible` (rather
                        than mounting/unmounting the icon) keeps has-[>svg]
                        padding and the icon+gap width constant so the row
                        doesn't jump when the spinner appears. */}
                    <Loader2
                      className={cn("size-3.5 animate-spin", !harnessSpinning && "invisible")}
                    />
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
            // F5: under a per_user row, a caller WITH a per-person model_access
            // reading reports its OWN state — not_applicable (the shared
            // admin-token principal, which holds no session of its own) has no
            // per-caller answer to substitute, so it falls back to the
            // deployment-wide fact like every other row does.
            connected={
              perUserSso && status.model_access && modelAccessState !== "not_applicable"
                ? perUserLive
                : !!bedrockRow
            }
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
              {perUserSso && (
                <p className="text-meta leading-snug text-muted-foreground">{S.BEDROCK_PER_USER_NOTE}</p>
              )}
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
                startURLManaged={perUserSso}
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

// GitHostCard retired in 0.7.2 — see the file header. Its Lane/SecretLane/
// HostSummary shell lives above, exported; the row shell and the free-text
// Host field are screens/providers/git-tab.tsx's now (the row's own host,
// never a second text field).
