/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Add integration — ONE dialog (B3), replacing the old AddServiceDialog ⇄
// AddIntegrationDialog double-mount. Every kind — the seven closed ones and
// any generic slug — walks the SAME ladder:
//
//   kind pick -> [flavor pick, AI kinds only] -> base form (prefilled per
//   kind/flavor: secrets w/ delivery language, egress, config, docs) -> save
//   -> Test.
//
// AI kinds are provider FLAVORS (subscription/api_key/bedrock/openai/azure)
// that only change what the base form is prefilled with — there is no second,
// richer AI-only flow any more. HarnessLoginPane survives, embedded as the
// subscription/Bedrock-SSO flavor's sign-in card. AddSecretDialog survives
// for inline secret creation — every credential field here is a NAME
// reference into the secret store, never a value typed into this dialog.
import * as React from "react";
import { ChevronDown, KeyRound, Search, ShieldCheck } from "lucide-react";
import {
  CATALOG_COPY,
  INTEGRATION_GROUPS,
  integrationGroup,
  searchIntegrationTypes,
  typesInGroup,
  type IntegrationTypeMeta,
} from "../../../lib/integration-catalog";
import { AI_KINDS, aiServerId, genericIntegrationsApi, probeChip, SECRET_DELIVERY_NOTE, type IntegrationWrite } from "../../../lib/api/integrations";
import { setup as setupApi } from "../../../lib/api/setup";
import { secrets as secretsApi } from "../../../lib/api/secrets";
import {
  BEDROCK_LANE_META,
  RESIDENCY_META,
  SUBSCRIPTION_LANE_META,
  T,
  type AiType,
  type BedrockLane,
  type SubscriptionLane,
} from "../../../lib/integrations";
import type { SetupStatus } from "../../../lib/types";
import type { WireIntegrationProbeStatus, WireIntegrationSecret } from "../../../lib/types/setup";
import { getErrorMessage, relativeTime } from "../../../lib/format";
import { Button } from "../../ui/button";
import { Checkbox } from "../../ui/checkbox";
import { Dialog, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle } from "../../ui/dialog";
import { Input } from "../../ui/input";
import { Label } from "../../ui/label";
import { Textarea } from "../../ui/textarea";
import { Field, OptionCard } from "../new-run/step-shell";
import { Mono } from "../../wardyn/code-block";
import { Chip, SectionLabel } from "../../wardyn/primitives";
import { AddSecretDialog } from "../secrets";
import { HarnessLoginPane } from "../setup/harness-login-pane";

/** One already-existing row's id + display name — enough to warn on a slug
 *  collision before Save either silently replaces it (a STORED row: PUT is
 *  create-or-REPLACE) or 409s (a DERIVED-only row: the server now refuses a
 *  PUT there outright — adoption is explicit, POST {id}/adopt only, never a
 *  side effect of this dialog). `stored` tells Save which case it's in. */
export interface ExistingIntegrationRef {
  id: string;
  name: string;
  stored: boolean;
}

/** A slug the API accepts as an integration id (secretNameRE: lowercase, dots/dashes/underscores). */
export function slugifyIntegrationId(name: string): string {
  return name
    .toLowerCase()
    .replace(/[^a-z0-9._-]+/g, "-")
    .replace(/^-+|-+$/g, "")
    .slice(0, 128);
}

/** Every operator-set value the PUT body is built from — exactly the BaseForm
 *  state, so the payload is shaped in ONE place a non-mocked test can exercise
 *  (add-integration-dialog.test.tsx's payload suite) instead of inline where
 *  only a mocked put() ever saw it. */
export interface IntegrationFormValues {
  type: IntegrationTypeMeta;
  hostCli?: boolean;
  bedrockLane?: BedrockLane;
  name: string;
  hostList: string[];
  docs: string;
  region: string;
  model: string;
  defAgent: boolean;
  defFeat: boolean;
  genericSecret: string;
  bearerSecret: string;
  awsKeyId: string;
  awsSecret: string;
  awsSession: string;
  appIdSecret: string;
  appKeySecret: string;
}

/** Build the PUT /integrations/{id} body from the form values — the single
 *  payload-shaping seam, matched to the server's write contract per kind/flavor
 *  (internal/api/integrations_write.go, runs_bedrock.go):
 *   - a Bedrock credential row carries NO delivery: its bespoke transport reads
 *     the secret by NAME (bearer ⇒ `bedrock-api-key`, static ⇒ the fixed aws-*
 *     globals), never generic proxy_header injection — exactly what
 *     foldLegacyIntegration emits for a bedrock row, and a CLOSED kind may omit
 *     delivery (validateIntegrationWrite only validates a delivery that's present);
 *   - the AWS session token is optional (STS/AssumeRole) — its row is dropped
 *     when empty rather than sent as an empty secret_name the server 400s;
 *   - an AI-provider kind gets NO auto-probe: a bare unauthenticated GET / to an
 *     AI host returns 4xx and the curl -f probe brands the healthy row "failed"
 *     (an auth-aware AI reachability probe is a documented follow-up). */
export function buildIntegrationWrite(v: IntegrationFormValues): IntegrationWrite {
  const { type } = v;
  const isSubscription = type.apiType === "anthropic_subscription";
  const isBedrock = type.apiType === "bedrock";
  const isGithubApp = type.apiType === "github_app";
  const isGitHost = type.apiType === "git_host";
  const isAi = !!type.apiType && AI_KINDS.has(type.apiType);
  const takesHeader = !!type.header;
  const bedrockLane = v.bedrockLane ?? "bearer";

  const secrets: WireIntegrationSecret[] = [];
  if (isSubscription) {
    // Managed lane's token lives in harness state, not the secret store; the
    // host-CLI lane has no stored credential at all — nothing to reference.
  } else if (isBedrock && bedrockLane === "bearer") {
    secrets.push({ role: "api_key", secret_name: v.bearerSecret.trim() });
  } else if (isBedrock && bedrockLane === "static") {
    secrets.push(
      { role: "access_key_id", secret_name: v.awsKeyId.trim() },
      { role: "secret_access_key", secret_name: v.awsSecret.trim() },
    );
    if (v.awsSession.trim()) secrets.push({ role: "session_token", secret_name: v.awsSession.trim() });
  } else if (isBedrock) {
    // sso / aws_dir: a harness session or boot-config mount, no secret row.
  } else if (isGithubApp) {
    secrets.push({ role: "app_id", secret_name: v.appIdSecret.trim() }, { role: "app_key", secret_name: v.appKeySecret.trim() });
  } else if (isGitHost) {
    // W11-S1-4: the "Git over SSH" catalog entry (id "gitssh") shares
    // apiType "git_host" with the PAT-over-HTTPS lanes, but the server's
    // capability matrix (internal/api/integrations.go) keys clone:pat vs
    // clone:ssh off the secret's ROLE — mislabeling an SSH key as "pat"
    // reports the wrong clone lane.
    secrets.push({ role: type.id === "gitssh" ? "ssh_key" : "pat", secret_name: v.genericSecret.trim() });
  } else if (takesHeader && v.genericSecret.trim()) {
    secrets.push({
      role: "api_key",
      secret_name: v.genericSecret.trim(),
      delivery: { mode: "proxy_header", header: type.header!, format: type.format },
    });
  }

  const config: Record<string, unknown> = {};
  if (isBedrock) {
    config.auth_lane = bedrockLane;
    if (v.region.trim()) config.region = v.region.trim();
    if (v.model.trim()) config.model = v.model.trim();
  }
  if (isSubscription) config.lane = v.hostCli ? "resident_host" : "managed";

  const egress = isSubscription || isBedrock ? type.hosts.slice() : v.hostList;
  // W11-S1-3: the auto-probe URL is `https://${probeHost}/` below — a wildcard
  // (or port-qualified) FIRST host there builds a malformed URL validSiteURL
  // (internal/api/site_config.go) rejects outright, 400ing Save even though
  // HOSTS_HINT ("wildcards are fine") makes no such exception. Skip to the
  // first BARE host instead — same shape the server's own bareExactHost
  // (integrations_write.go) accepts a credential header on — and drop the
  // probe entirely (never a 400) when every host is a wildcard/port entry.
  const probeHost = egress.find((h) => !h.startsWith("*.") && !h.includes(":"));

  return {
    name: v.name.trim(),
    kind: type.apiType ?? type.id,
    egress,
    ...(secrets.length ? { secrets } : {}),
    ...(Object.keys(config).length ? { config } : {}),
    ...(v.docs.trim() ? { docs: v.docs.trim() } : {}),
    ...(probeHost && !isAi ? { probe: { method: "GET", url: `https://${probeHost}/` } } : {}),
    ...(isAi && (v.defAgent || v.defFeat)
      ? { default_for: [...(v.defAgent ? ["agent_runs"] : []), ...(v.defFeat ? ["wardyn_features"] : [])] }
      : {}),
  };
}

type Step =
  | { s: "pick" }
  | { s: "flavor"; type: IntegrationTypeMeta }
  | { s: "form"; type: IntegrationTypeMeta; hostCli?: boolean; bedrockLane?: BedrockLane }
  | { s: "done"; id: string; name: string };

export function AddIntegrationDialog({
  open,
  onOpenChange,
  existingRows,
  onSaved,
}: {
  open: boolean;
  onOpenChange: (o: boolean) => void;
  /** Every currently-existing row (stored or derived) — flags a slug
   *  collision before Save either replaces a stored one or hits the
   *  server's 409-on-a-derived-id refusal. */
  existingRows: ExistingIntegrationRef[];
  /** Called once after Save+Done — the list reloads to pick up the new row. */
  onSaved: () => void;
}) {
  const [step, setStep] = React.useState<Step>({ s: "pick" });
  const [status, setStatus] = React.useState<SetupStatus | null>(null);
  const [secretNames, setSecretNames] = React.useState<string[]>([]);

  React.useEffect(() => {
    if (!open) return;
    setStep({ s: "pick" });
    Promise.all([setupApi.getSetupStatus(), secretsApi.listSecrets()]).then(([st, names]) => {
      setStatus(st);
      setSecretNames(names);
    });
  }, [open]);

  if (!open || !status) return null;

  const close = () => onOpenChange(false);
  const finish = () => {
    close();
    onSaved();
  };

  if (step.s === "pick") {
    return (
      <Dialog open onOpenChange={(o) => !o && close()}>
        <DialogContent className="max-w-[640px]">
          <DialogHeader>
            <DialogTitle>Add integration</DialogTitle>
            <DialogDescription>{CATALOG_COPY.ADD_DESC}</DialogDescription>
          </DialogHeader>
          <PickPanel
            onPick={(type) => {
              const flavored = type.apiType === "anthropic_subscription" || type.apiType === "bedrock";
              setStep(flavored ? { s: "flavor", type } : { s: "form", type });
            }}
          />
        </DialogContent>
      </Dialog>
    );
  }

  if (step.s === "flavor") {
    return (
      <Dialog open onOpenChange={(o) => !o && close()}>
        <DialogContent className="max-w-[640px]">
          <DialogHeader>
            <DialogTitle>Add integration — {step.type.label}</DialogTitle>
            <DialogDescription>One integration; the lane is switchable later.</DialogDescription>
          </DialogHeader>
          <FlavorPanel
            type={step.type}
            status={status}
            onBack={() => setStep({ s: "pick" })}
            onContinue={(hostCli, bedrockLane) => setStep({ s: "form", type: step.type, hostCli, bedrockLane })}
          />
        </DialogContent>
      </Dialog>
    );
  }

  if (step.s === "form") {
    return (
      <Dialog open onOpenChange={(o) => !o && close()}>
        <DialogContent className="scroll-thin sm:max-w-2xl" style={{ maxHeight: "calc(100vh - 96px)", overflowY: "auto" }}>
          <DialogHeader>
            <DialogTitle>Add integration — {step.type.label}</DialogTitle>
            <DialogDescription>{CATALOG_COPY.CONNECT_DESC}</DialogDescription>
          </DialogHeader>
          <BaseForm
            type={step.type}
            hostCli={step.hostCli}
            bedrockLane={step.bedrockLane}
            status={status}
            secretNames={secretNames}
            existingRows={existingRows}
            onBack={() => (step.type.apiType === "anthropic_subscription" || step.type.apiType === "bedrock" ? setStep({ s: "flavor", type: step.type }) : setStep({ s: "pick" }))}
            onSaved={(id, name) => setStep({ s: "done", id, name })}
          />
        </DialogContent>
      </Dialog>
    );
  }

  return (
    <Dialog open onOpenChange={(o) => !o && finish()}>
      <DialogContent className="max-w-md">
        <DialogHeader>
          <DialogTitle>Saved</DialogTitle>
          <DialogDescription>
            “{step.name}” is stored. {CATALOG_COPY.STORE_NOTE}
          </DialogDescription>
        </DialogHeader>
        <DonePanel id={step.id} onDone={finish} />
      </DialogContent>
    </Dialog>
  );
}

// ---- Step 1: kind pick — search-first, browse-by-section fallback ----

function PickPanel({ onPick }: { onPick: (t: IntegrationTypeMeta) => void }) {
  const [query, setQuery] = React.useState("");
  const results = searchIntegrationTypes(query);
  const browsing = query.trim() === "";

  return (
    <div className="space-y-4">
      <div className="relative">
        <Search className="pointer-events-none absolute left-2.5 top-1/2 size-4 -translate-y-1/2 text-muted-foreground" />
        <Input
          autoFocus
          className="pl-8"
          placeholder={CATALOG_COPY.SEARCH_PH}
          aria-label="Search integration types"
          value={query}
          onChange={(e) => setQuery(e.target.value)}
        />
      </div>

      {browsing ? (
        <div className="max-h-[340px] space-y-3 overflow-y-auto">
          {INTEGRATION_GROUPS.map((group) => {
            const types = typesInGroup(group.id);
            if (types.length === 0) return null;
            return (
              <div key={group.id}>
                <p className="text-xs font-medium text-foreground">{group.label}</p>
                <p className="text-[0.6875rem] leading-snug text-muted-foreground">{group.desc}</p>
                <div className="mt-1.5 flex flex-wrap gap-1.5">
                  {types.map((t) => (
                    <Button key={t.id} size="sm" variant="outline" onClick={() => onPick(t)}>
                      {t.label}
                    </Button>
                  ))}
                </div>
              </div>
            );
          })}
        </div>
      ) : results.length === 0 ? (
        <p className="text-[0.8125rem] leading-snug text-muted-foreground">{CATALOG_COPY.SEARCH_NONE}</p>
      ) : (
        <div className="space-y-1.5">
          {results.map((t) => (
            <button
              key={t.id}
              type="button"
              className="flex w-full items-center gap-2 rounded-lg border border-border px-3 py-2 text-left hover:bg-muted/40"
              onClick={() => onPick(t)}
            >
              <span className="min-w-0 flex-1">
                <span className="block text-sm text-foreground">{t.label}</span>
                <span className="block text-[0.6875rem] text-muted-foreground">
                  {integrationGroup(t.group).label}
                  {t.hosts.length > 0 ? ` · ${t.hosts[0]}` : ""}
                </span>
              </span>
              <Chip tone={RESIDENCY_META[t.delivery].tone}>{RESIDENCY_META[t.delivery].label}</Chip>
            </button>
          ))}
        </div>
      )}
    </div>
  );
}

// ---- Step 2: flavor pick — subscription (managed/host-login) or Bedrock's four lanes ----

function FlavorPanel({
  type,
  status,
  onBack,
  onContinue,
}: {
  type: IntegrationTypeMeta;
  status: SetupStatus;
  onBack: () => void;
  onContinue: (hostCli?: boolean, bedrockLane?: BedrockLane) => void;
}) {
  const [subLane, setSubLane] = React.useState<SubscriptionLane>("managed");
  const [advancedOpen, setAdvancedOpen] = React.useState(false);
  const [bedrockLane, setBedrockLane] = React.useState<BedrockLane>("bearer");
  // Sealed control plane (wardynd itself runs in a container): the host-CLI
  // subscription lane can never be satisfied here.
  const sealed = status.deployment?.host_like === false;

  if (type.apiType === "bedrock") {
    return (
      <>
        <div className="space-y-1.5">
          {(["bearer", "sso", "aws_dir", "static"] as const).map((lane) => {
            const meta = BEDROCK_LANE_META[lane];
            const res = RESIDENCY_META[meta.residency];
            return (
              <button
                key={lane}
                type="button"
                onClick={() => setBedrockLane(lane)}
                aria-pressed={bedrockLane === lane}
                className={`flex w-full items-center gap-2.5 rounded-lg border px-3 py-2 text-left transition-colors ${
                  bedrockLane === lane ? "border-primary bg-primary/10" : "border-border hover:border-border-strong"
                }`}
              >
                <span className="text-sm text-foreground">
                  {meta.title}
                  {meta.extra && <span className="text-muted-foreground"> · {meta.extra}</span>}
                </span>
                <span className="ml-auto">
                  <Chip tone={res.tone} className="text-[0.6875rem]">
                    {res.label}
                  </Chip>
                </span>
              </button>
            );
          })}
          <p className="text-[0.6875rem] text-muted-foreground">{T.LANE_SWITCH} Lanes are listed in real precedence order.</p>
        </div>
        <DialogFooter>
          <Button variant="outline" onClick={onBack}>
            Back
          </Button>
          <Button onClick={() => onContinue(undefined, bedrockLane)}>Continue</Button>
        </DialogFooter>
      </>
    );
  }

  return (
    <>
      <div className="space-y-2">
        <OptionCard
          selected={subLane === "managed"}
          onClick={() => setSubLane("managed")}
          title={
            <span className="flex items-center gap-2">
              {SUBSCRIPTION_LANE_META.managed.title}
              <Chip tone="primary" className="uppercase tracking-wide">
                Recommended
              </Chip>
            </span>
          }
          hint={
            <>
              "{T.MANAGED_LINE}" <span className="block">{T.X_SUB_DIRECT}</span>
            </>
          }
        />
        {advancedOpen ? (
          <div className="space-y-2">
            <OptionCard
              selected={subLane === "resident_host"}
              onClick={() => setSubLane("resident_host")}
              title={SUBSCRIPTION_LANE_META.resident_host.title}
              hint={
                <>
                  "{T.HOSTCLI_LINE}" <span className="block">{T.X_SUB_DIRECT}</span>
                </>
              }
            />
            {sealed && (
              <div className="rounded-lg border border-warning/30 bg-warning-subtle px-3 py-2">
                <p className="text-[0.6875rem] leading-snug text-warning">{T.SEALED_NOTE}</p>
              </div>
            )}
          </div>
        ) : (
          <button
            type="button"
            onClick={() => setAdvancedOpen(true)}
            className="flex items-center gap-1.5 text-xs text-muted-foreground hover:text-foreground"
          >
            <ChevronDown className="size-3.5" /> Advanced
          </button>
        )}
      </div>
      <DialogFooter>
        <Button variant="outline" onClick={onBack}>
          Back
        </Button>
        <Button onClick={() => onContinue(subLane === "resident_host")}>Continue</Button>
      </DialogFooter>
    </>
  );
}

// ---- Step 3: base form — secrets w/ delivery language, egress, config, docs ----

/** The credential cell for a container-login lane (Claude subscription
 *  managed, Bedrock SSO). Three states: the pane itself, captured-just-now,
 *  and already-connected-from-an-earlier-capture. */
function LoginCredentialCell({ provider, status }: { provider: "anthropic" | "aws"; status: SetupStatus }) {
  const [open, setOpen] = React.useState(true);
  const [freshCapture, setFreshCapture] = React.useState(false);
  const existing = status.harness?.find((h) => h.provider === provider && h.captured);

  if (open) {
    return (
      <HarnessLoginPane
        provider={provider}
        onDone={() => {
          setOpen(false);
          setFreshCapture(true);
        }}
        onCancel={() => setOpen(false)}
      />
    );
  }

  const line = freshCapture
    ? provider === "anthropic"
      ? "Subscription captured — stored write-only. Runs get it injected proxy-side; a run's sandbox never holds it."
      : "AWS SSO session captured — stored write-only. Bedrock runs exchange it for short-lived role credentials."
    : existing
      ? `Already connected — captured ${existing.captured_at ? relativeTime(existing.captured_at) : "earlier"}${existing.aging ? " · reconnect soon" : ""}.`
      : null;

  return (
    <div className="space-y-2">
      {line ? (
        <p className={`flex items-start gap-2 text-xs leading-snug ${freshCapture ? "text-success" : "text-muted-foreground"}`}>
          <ShieldCheck className="mt-px size-3.5 shrink-0" /> <span className="min-w-0">{line}</span>
        </p>
      ) : (
        <p className="text-xs leading-snug text-muted-foreground">Not connected yet.</p>
      )}
      <Button size="sm" variant="outline" onClick={() => setOpen(true)}>
        <KeyRound className="size-3.5" /> {line ? "Log in again" : "Log in"}
      </Button>
    </div>
  );
}

/** One "Required secret" row: an editable, prefilled secret NAME plus an
 *  "Enter value" button that opens AddSecretDialog — the base model's own
 *  "Wardyn stores this — it doesn't dial the system to check it" contract. */
function SecretRow({
  label,
  name,
  onChange,
  onEnterValue,
  deliveryNote,
}: {
  label: string;
  name: string;
  onChange: (v: string) => void;
  onEnterValue: () => void;
  /** Never-resident / resident language for THIS secret's delivery. */
  deliveryNote: string;
}) {
  // A stable id derived from the label — every SecretRow instance (bearer
  // token, AWS keys, GitHub App id/key, git PAT, generic header credential)
  // gets its own Label/Input pairing instead of the two rendering as
  // unlinked siblings (a11y-blocker: getByLabelText and every screen reader
  // need this to resolve which input a label names).
  const id = `secret-${label.toLowerCase().replace(/[^a-z0-9]+/g, "-").replace(/^-+|-+$/g, "")}`;
  return (
    <div className="space-y-1">
      <Label htmlFor={id} className="text-xs">{label}</Label>
      <div className="flex flex-wrap items-center gap-2">
        <Input id={id} value={name} onChange={(e) => onChange(e.target.value)} className="max-w-[260px]" />
        <Button size="sm" variant="outline" onClick={onEnterValue}>
          <KeyRound className="size-3.5" /> Enter value
        </Button>
      </div>
      <p className="text-[0.6875rem] leading-snug text-muted-foreground">{deliveryNote}</p>
    </div>
  );
}

function BaseForm({
  type,
  hostCli,
  bedrockLane,
  status,
  secretNames,
  existingRows,
  onBack,
  onSaved,
}: {
  type: IntegrationTypeMeta;
  hostCli?: boolean;
  bedrockLane?: BedrockLane;
  status: SetupStatus;
  secretNames: string[];
  existingRows: ExistingIntegrationRef[];
  onBack: () => void;
  onSaved: (id: string, name: string) => void;
}) {
  const isSubscription = type.apiType === "anthropic_subscription";
  const isBedrock = type.apiType === "bedrock";
  const isGithubApp = type.apiType === "github_app";
  const isGitHost = type.apiType === "git_host";
  const isAi = !!type.apiType && AI_KINDS.has(type.apiType);

  const defaultName =
    isSubscription ? `Claude subscription (${hostCli ? "host CLI" : "managed"})`
    : isBedrock ? `AWS Bedrock (${BEDROCK_LANE_META[bedrockLane ?? "bearer"].title})`
    : type.label;
  const [name, setName] = React.useState(defaultName);
  // Azure's only "host" is the "<resource>.openai.azure.com" TEMPLATE, not a
  // real value — prefilling it as the value means an un-edited save 400s on the
  // literal "<resource>". Start empty and show the template as the placeholder.
  // (ponytail: scoped to the named azure nit; jira/codeartifact carry the same
  // "<…>" template shape but are out of B3 scope — flagged as a follow-up.)
  const azureTemplatedHost = type.apiType === "azure_openai";
  const [hosts, setHosts] = React.useState(azureTemplatedHost ? "" : type.hosts.join("\n"));
  const [docs, setDocs] = React.useState("");
  const [region, setRegion] = React.useState("");
  const [model, setModel] = React.useState("");
  const [defAgent, setDefAgent] = React.useState(false);
  const [defFeat, setDefFeat] = React.useState(false);
  const [secretDialog, setSecretDialog] = React.useState<string | null>(null);
  const [saving, setSaving] = React.useState(false);
  const [error, setError] = React.useState<string | null>(null);

  // Secret name state, one row per role this kind/flavor needs.
  const [genericSecret, setGenericSecret] = React.useState(type.secret ?? "");
  // The bedrock bearer token is read by the bespoke bedrock transport SOLELY by
  // the name `bedrock-api-key` (runs_bedrock.go bedrockAPIKeySecret) — a
  // different name is a credential nothing ever reads. "Enter value" stores it
  // under this name, so it must be exactly that.
  const [bearerSecret, setBearerSecret] = React.useState("bedrock-api-key");
  const [awsKeyId, setAwsKeyId] = React.useState("aws-access-key-id");
  const [awsSecret, setAwsSecret] = React.useState("aws-secret-access-key");
  const [awsSession, setAwsSession] = React.useState("aws-session-token");
  const [appIdSecret, setAppIdSecret] = React.useState("github-app-id");
  const [appKeySecret, setAppKeySecret] = React.useState("github-app-key");

  const hostList = hosts.split("\n").map((h) => h.trim()).filter(Boolean);
  const takesHeader = !!type.header;
  const residency =
    isSubscription ? SUBSCRIPTION_LANE_META[hostCli ? "resident_host" : "managed"].residency
    : isBedrock ? BEDROCK_LANE_META[bedrockLane ?? "bearer"].residency
    : type.delivery;
  const resMeta = RESIDENCY_META[residency];

  const idForSave = () => {
    if (type.apiType === "anthropic_api_key" || type.apiType === "openai_api_key") return aiServerId(type.apiType as AiType);
    if (isSubscription) return aiServerId("anthropic_subscription", hostCli);
    if (isBedrock) return aiServerId("bedrock");
    if (isGithubApp) return "github_app";
    if (isGitHost) return `git_host:${hostList[0] ?? slugifyIntegrationId(name)}`;
    return slugifyIntegrationId(name);
  };
  const id = idForSave() ?? slugifyIntegrationId(name);
  const collision = existingRows.find((r) => r.id === id);
  // A harness-backed flavor (managed subscription, Bedrock SSO) whose credential
  // is already captured ALSO exists as a derived row the server refuses a PUT to —
  // but that row is projected from harness state, never listed in /integrations, so
  // the `collision` check above can't see it. The credential's lifecycle is the
  // Log-in cell, not a PUT here. Treat a captured harness credential as the same
  // derived-row block, so the operator gets the Adopt/refresh pointer instead of
  // filling in the form and hitting a raw 409 on Save.
  // A subscription (managed OR resident-host CLI) and a Bedrock-SSO row are
  // structurally DERIVED, never stored: you connect them by logging in via the
  // pane above and configure them with "Adopt to edit" — there is no valid PUT
  // here (the server 409s "exists only as a derived row"). Block UNCONDITIONALLY,
  // NOT gated on a captured harness in `status`: this dialog reads status once on
  // open and a login done in the pane below never refreshes it, so gating on the
  // captured flag missed exactly the just-logged-in case (the reported bug —
  // "Subscription captured" shows from the pane's own local state while the
  // dialog's status snapshot still says nothing is connected).
  const derivedOnlyLane = isSubscription || (isBedrock && (bedrockLane ?? "bearer") === "sso");
  // A PUT to an id that exists only as a DERIVATION now 409s server-side —
  // adoption is explicit, never a side effect of this dialog. Block Save
  // rather than let the operator hit that wall after filling in the form.
  const blockedByDerived = (!!collision && !collision.stored) || derivedOnlyLane;

  // Mirrors buildIntegrationWrite's own branching: the four lanes that ALWAYS
  // push a secret row regardless of whether the field is empty (bedrock
  // bearer/static, GitHub App, git host) need that field non-empty here too,
  // or Save stays clickable on a blank secret name that only a raw server 400
  // catches. The generic header lane is genuinely optional (buildIntegrationWrite
  // omits it entirely when blank) and subscription/sso/aws_dir have no secret
  // name field at all — neither belongs in this check.
  const requiredSecretsFilled =
    isBedrock && (bedrockLane ?? "bearer") === "bearer" ? bearerSecret.trim().length > 0
    : isBedrock && (bedrockLane ?? "bearer") === "static" ? awsKeyId.trim().length > 0 && awsSecret.trim().length > 0
    : isGithubApp ? appIdSecret.trim().length > 0 && appKeySecret.trim().length > 0
    : isGitHost ? genericSecret.trim().length > 0
    : true;

  const canSave =
    !blockedByDerived &&
    name.trim().length > 0 &&
    requiredSecretsFilled &&
    // A generic/header type needs its host(s) named; the closed kinds either
    // carry a fixed host (subscription/bedrock) or the operator names their own.
    (hostList.length > 0 || isSubscription || isBedrock);

  const save = async () => {
    setSaving(true);
    setError(null);
    try {
      // ONE payload seam (buildIntegrationWrite, above) — shared with the
      // non-mocked payload test so the shape a real server validates can't
      // drift from what a mocked put() lets through.
      await genericIntegrationsApi.put(
        id,
        buildIntegrationWrite({
          type, hostCli, bedrockLane, name, hostList, docs, region, model, defAgent, defFeat,
          genericSecret, bearerSecret, awsKeyId, awsSecret, awsSession, appIdSecret, appKeySecret,
        }),
      );
      onSaved(id, name.trim());
    } catch (e) {
      setError(getErrorMessage(e));
    } finally {
      setSaving(false);
    }
  };

  return (
    <div className="min-w-0 space-y-4 py-1">
      <Field label="Name" htmlFor="int-name" hint="How this row reads in lists and pickers.">
        <Input id="int-name" value={name} onChange={(e) => setName(e.target.value)} />
        <p className="mt-1 text-[0.6875rem] text-muted-foreground">
          Stored as <Mono className="text-[0.6875rem]">{id || "—"}</Mono>
        </p>
        {blockedByDerived ? (
          <p className="text-[0.6875rem] leading-snug text-warning">
            {collision
              ? `"${collision.name}" already exists at this id, derived from your current setup — close this dialog and use "Adopt to edit" on that row instead of adding it again.`
              : `Connect this by logging in above — Wardyn derives the integration from your login, it is never added here. Once you've logged in, just close this dialog; set its defaults later with "Adopt to edit" in the Integrations list.`}
          </p>
        ) : (
          collision && (
            <p className="text-[0.6875rem] leading-snug text-warning">Replaces the existing "{collision.name}" integration — same stored id.</p>
          )
        )}
      </Field>

      <div className="space-y-3 rounded-xl border border-border p-3.5">
        <SectionLabel>Required secrets</SectionLabel>
        {isSubscription && hostCli && (
          <p className="text-xs leading-snug text-muted-foreground">
            {T.HOSTCLI_LINE} Wardyn detects it automatically once you've logged in with the Claude CLI on this host — there's nothing
            to capture here.
          </p>
        )}
        {isSubscription && !hostCli && <LoginCredentialCell provider="anthropic" status={status} />}
        {isBedrock && bedrockLane === "sso" && <LoginCredentialCell provider="aws" status={status} />}
        {isBedrock && bedrockLane === "aws_dir" && (
          <p className="text-xs leading-snug text-muted-foreground">Boot-time config — the host's ~/.aws mount, not stored here.</p>
        )}
        {isBedrock && bedrockLane === "bearer" && (
          <SecretRow label="Bearer token" name={bearerSecret} onChange={setBearerSecret} onEnterValue={() => setSecretDialog(bearerSecret)} deliveryNote={SECRET_DELIVERY_NOTE.proxy_header} />
        )}
        {isBedrock && bedrockLane === "static" && (
          <>
            <SecretRow label="Access key ID" name={awsKeyId} onChange={setAwsKeyId} onEnterValue={() => setSecretDialog(awsKeyId)} deliveryNote={SECRET_DELIVERY_NOTE.resident} />
            <SecretRow label="Secret access key" name={awsSecret} onChange={setAwsSecret} onEnterValue={() => setSecretDialog(awsSecret)} deliveryNote={SECRET_DELIVERY_NOTE.resident} />
            <SecretRow label="Session token" name={awsSession} onChange={setAwsSession} onEnterValue={() => setSecretDialog(awsSession)} deliveryNote={SECRET_DELIVERY_NOTE.resident} />
          </>
        )}
        {isGithubApp && (
          <>
            <SecretRow label="App ID" name={appIdSecret} onChange={setAppIdSecret} onEnterValue={() => setSecretDialog(appIdSecret)} deliveryNote={SECRET_DELIVERY_NOTE.brokered} />
            <SecretRow label="Private key (PEM)" name={appKeySecret} onChange={setAppKeySecret} onEnterValue={() => setSecretDialog(appKeySecret)} deliveryNote={SECRET_DELIVERY_NOTE.brokered} />
          </>
        )}
        {isGitHost && (
          <SecretRow
            label="Personal access token"
            name={genericSecret}
            onChange={setGenericSecret}
            onEnterValue={() => setSecretDialog(genericSecret)}
            deliveryNote={`${SECRET_DELIVERY_NOTE.resident} The git credential helper hands it to git inside the sandbox.`}
          />
        )}
        {!isSubscription && !isBedrock && !isGithubApp && !isGitHost && takesHeader && (
          <SecretRow
            label={CATALOG_COPY.CRED_HEAD}
            name={genericSecret}
            onChange={setGenericSecret}
            onEnterValue={() => setSecretDialog(genericSecret)}
            deliveryNote={`Presented as ${type.header}. ${SECRET_DELIVERY_NOTE.proxy_header}`}
          />
        )}
        {!isSubscription && !isBedrock && !isGithubApp && !isGitHost && !takesHeader && (
          <p className="text-[0.8125rem] leading-snug text-foreground">{resMeta.tooltip}</p>
        )}
      </div>

      <div className="space-y-1.5 rounded-xl border border-border p-3.5">
        <SectionLabel>{CATALOG_COPY.HOSTS_HEAD}</SectionLabel>
        {isSubscription || isBedrock ? (
          <p className="text-[0.8125rem] leading-snug text-muted-foreground">
            <Mono className="text-[0.8125rem]">{type.hosts.join(", ")}</Mono> — fixed for this provider.
          </p>
        ) : (
          <>
            <Textarea rows={3} value={hosts} placeholder={azureTemplatedHost ? type.hosts[0] : type.hostPlaceholder} onChange={(e) => setHosts(e.target.value)} />
            <p className="text-[0.6875rem] leading-snug text-muted-foreground">{CATALOG_COPY.HOSTS_HINT}</p>
          </>
        )}
      </div>

      {isBedrock && (
        <div className="space-y-2 rounded-xl border border-border p-3.5">
          <SectionLabel>Config</SectionLabel>
          <div className="grid grid-cols-2 gap-2">
            <div>
              <Label htmlFor="int-region" className="text-xs">Region</Label>
              <Input id="int-region" value={region} onChange={(e) => setRegion(e.target.value)} placeholder="us-east-1" />
            </div>
            <div>
              <Label htmlFor="int-model" className="text-xs">Model</Label>
              <Input id="int-model" value={model} onChange={(e) => setModel(e.target.value)} placeholder="anthropic.claude-3" />
            </div>
          </div>
        </div>
      )}

      <div className="space-y-1.5 rounded-xl border border-border p-3.5">
        <Label htmlFor="int-docs" className="text-xs">
          Docs link
        </Label>
        <Input id="int-docs" value={docs} onChange={(e) => setDocs(e.target.value)} />
        <p className="text-[0.6875rem] text-muted-foreground">{CATALOG_COPY.DOCS_HINT}</p>
      </div>

      <div className="flex items-center gap-2 rounded-lg border border-border px-3 py-2.5">
        <Chip tone={resMeta.tone}>{resMeta.label}</Chip>
        <p className="min-w-0 flex-1 text-[0.6875rem] leading-snug text-muted-foreground">{CATALOG_COPY.DELIV_STATED}</p>
      </div>

      {isAi && (
        <div className="space-y-2 rounded-xl border border-border p-3.5">
          <SectionLabel>Defaults</SectionLabel>
          <div className="flex items-start gap-2.5">
            <Checkbox id="def-agent" checked={defAgent} onCheckedChange={(v) => setDefAgent(!!v)} className="mt-0.5" />
            <label htmlFor="def-agent" className="cursor-pointer text-sm text-foreground">
              Default for agent runs
            </label>
          </div>
          <div className="flex items-start gap-2.5">
            <Checkbox id="def-features" checked={defFeat} onCheckedChange={(v) => setDefFeat(!!v)} className="mt-0.5" />
            <label htmlFor="def-features" className="cursor-pointer text-sm text-foreground">
              Default for Wardyn features
            </label>
          </div>
        </div>
      )}

      <p className="text-right text-[0.6875rem] text-muted-foreground">{CATALOG_COPY.STORE_NOTE}</p>
      {error && <p className="text-[0.8125rem] leading-snug text-danger">{error}</p>}

      <DialogFooter>
        <Button variant="outline" onClick={onBack} disabled={saving}>
          Back
        </Button>
        <Button onClick={() => void save()} disabled={saving || !canSave}>
          {saving ? "Adding…" : "Add integration"}
        </Button>
      </DialogFooter>

      <AddSecretDialog
        open={!!secretDialog}
        onOpenChange={(o) => !o && setSecretDialog(null)}
        lockName
        initialName={secretDialog ?? ""}
        existingNames={secretNames}
        onSaved={() => setSecretDialog(null)}
      />
    </div>
  );
}

// ---- Step 4: post-save — Test, then done ----

function DonePanel({ id, onDone }: { id: string; onDone: () => void }) {
  const [testing, setTesting] = React.useState(false);
  const [probeStatus, setProbeStatus] = React.useState<WireIntegrationProbeStatus | undefined>(undefined);
  const [testError, setTestError] = React.useState<string | null>(null);

  // A 2xx response from POST /test means the probe RAN, not that it passed —
  // handleTestIntegration's response body IS the fresh probe_status, so read
  // the verdict straight off it rather than inferring pass/fail from the
  // request merely succeeding.
  const runTest = async () => {
    setTesting(true);
    setTestError(null);
    try {
      setProbeStatus(await genericIntegrationsApi.test(id));
    } catch (e) {
      setTestError(getErrorMessage(e));
    } finally {
      setTesting(false);
    }
  };

  const chip = probeChip(probeStatus);

  return (
    <div className="space-y-3">
      <div className="flex items-center gap-2">
        <Chip tone={chip.tone}>{chip.label}</Chip>
        {chip.detail && <span className="text-xs text-muted-foreground">{chip.detail}</span>}
      </div>
      {testError && <p className="text-[0.8125rem] leading-snug text-danger">{testError}</p>}
      <DialogFooter>
        <Button variant="outline" onClick={() => void runTest()} disabled={testing}>
          {testing ? "Testing…" : "Test connection"}
        </Button>
        <Button onClick={onDone}>Done</Button>
      </DialogFooter>
    </div>
  );
}
