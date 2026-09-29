/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The Agents tab of /providers (§5c.4, design §5.3) — one row per harness-catalog
// id (+ any custom WARDYN_AGENT_IMAGES rows already stored, preserved unedited):
// a Switch (enabled), the row's default model provider (packet MP-C's G1–G4),
// and — on the claude-code row only (SetupStatus.model_access is scoped to it
// server-side) — the SIGNED-IN ADMIN'S OWN model-access chip (their own
// credential, C4.2): outline action, never a second teal. The mechanism radio,
// the credential toggle and the AWS start URL live on the provider now.
//
// This tab is a SEPARATE resource from the Git/Storage tabs (SiteConfig's
// agent_providers block, its own GET/PUT), so it fetches and saves itself —
// the UserDrivesCard precedent for a self-contained tab widget — rather than
// riding the parent screen's WorkspaceProviders draft/save.
//
// Every user-visible string here is workspace-providers-copy.ts's AGENTS/
// PROVIDERS (§7.7, frozen) or reused canon it names (MODEL_PROVIDERS.KIND,
// RAIL_PROVIDER.PLACEHOLDER). This file adds none.
import * as React from "react";
import { AlertTriangle } from "lucide-react";
import { toast } from "sonner";
import { HttpError } from "../../../lib/api/core";
import { agentProviders as api, type AgentProvider, type AgentProviders } from "../../../lib/api/agent-providers";
import { modelProviders as providersApi, type ModelProvider } from "../../../lib/api/model-providers";
import { IMPOSSIBLE, type AiType } from "../../../lib/integrations";
import { MODEL_PROVIDERS } from "../../../lib/model-providers-copy";
import { RAIL_PROVIDER } from "../../wardyn/copy/new-run-rail";
import type { SetupHarnessTool, SetupModelAccess } from "../../../lib/types";
import { getErrorMessage } from "../../../lib/format";
import { readableDiff } from "../../../lib/readable-diff";
import { useUnsavedGuard } from "../../../lib/use-unsaved-guard";
import { useWriteDropped } from "../../../lib/use-write-dropped";
import { REAUTH_DIALOG } from "../../../lib/reauth-copy";
import { ACCESS_STATE } from "../../../lib/people-access-copy";
import {
  AGENTS,
  MODEL_ACCESS_ACTIONABLE,
  MODEL_ACCESS_CHIP_LABEL,
  PROVIDERS,
  PROVIDERS_EXTRA,
  modelAccessActionLine,
} from "../../../lib/workspace-providers-copy";
import { Button } from "../../ui/button";
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "../../ui/select";
import { AvailabilityControl } from "../../wardyn/availability-control";
import { Field, Switch } from "../../wardyn/form-primitives";
import { Chip, OperatorOnlyHint } from "../../wardyn/primitives";
import { SavedElsewhereBanner } from "../../wardyn/saved-elsewhere-banner";
import { EmptyState, TableSkeleton } from "../../wardyn/states";
import { useModelAccessDoor } from "../../wardyn/model-access-context";

// The two catalog agents whose declared lane folds to a coarse ai-integration
// type harness.go's ProviderTypes checks against (harnessCatalog); a
// no-managed-auth row ("none"/BYOA, or any custom WARDYN_AGENT_IMAGES id) has
// no capability here — its only valid mechanism is "none" (validateAgentMechanism).
type AgentCapability = "claude_code" | "codex_cli";

// Exported ONLY for its parity gate (agents-tab.test.tsx): this hand-typed
// fold is the other half of internal/api/harness.go's catalog, and nothing but
// that test checks the two still agree — a Gateway-bearing harness added there
// with no entry here silently loses its impossible-pair reasons.
export function agentCapabilityFor(id: string): AgentCapability | undefined {
  if (id === "claude-code") return "claude_code";
  if (id === "codex-cli") return "codex_cli";
  return undefined;
}

// The mechanism a freshly enabled catalog row is seeded with: the roster still
// requires one (validateAgentMechanism's agent400NeedsLane) although the tab no
// longer offers the choice — the first lane, in this order, that the agent can
// actually speak (the catalog's impossible pairs, mirrored in IMPOSSIBLE).
const SEED_MECHANISMS: [string, AiType][] = [
  ["anthropic_subscription", "anthropic_subscription"],
  ["anthropic_api_key", "anthropic_api_key"],
  ["openai_api_key", "openai_api_key"],
  ["bedrock_bearer", "bedrock"],
];

function defaultMechanism(harness: SetupHarnessTool): string {
  if (harness.no_managed_auth) return "none";
  const cap = agentCapabilityFor(harness.id);
  return (SEED_MECHANISMS.find(([, t]) => !(cap && IMPOSSIBLE[t]?.[cap])) ?? SEED_MECHANISMS[0])[0];
}

// The row to EDIT: the stored row if one exists, else a fresh one seeded with a
// mechanism this agent can actually use — a lanes-catalog row with an empty
// mechanism 400s at save (validateAgentMechanism's agent400NeedsLane), so the
// picker never shows a blank, unlaunchable choice.
//
// Not the answer to "is this agent enabled" — see rowEnabled. A row this
// function invents carries no `disabled`, so reading `!row.disabled` off it
// would render every not-offered agent as ON.
function resolvedRow(agents: AgentProvider[], harness: SetupHarnessTool): AgentProvider {
  return agents.find((a) => a.id === harness.id) ?? { id: harness.id, mechanism: defaultMechanism(harness) };
}

// Whether this agent's Switch is ON. THE SERVER'S OWN ANSWER when there is no
// stored row to read: SetupHarnessTool.enabled is `ok && !row.Disabled`
// (setupHarnessTools, internal/api) — true for every catalog id only in legacy
// open mode, where an all-on pre-fill IS what the admin sees and saves.
//
// The stored row wins when there is one, because that is where this tab's own
// edits land (updateRow writes `disabled` onto it). agent-picker.tsx:31 already
// reads `enabled === false`; this tab did not, so an admin who had narrowed the
// roster and then edited anything here silently re-enabled every catalog agent.
function rowEnabled(agents: AgentProvider[], harness: SetupHarnessTool): boolean {
  const stored = agents.find((a) => a.id === harness.id);
  return stored ? !stored.disabled : harness.enabled !== false;
}

// per_user is captured for an AWS SSO sign-in ONLY (validateAgentCredentialSource).
// A STORED row pairing it with any other mechanism — OR pairing bedrock_sso with
// a SHARED credential_source — would be re-PUT verbatim by the next unrelated
// Save and refused 400, and this tab no longer shows those fields, so the admin
// could not clear them. All four are dropped ON LOAD whenever credential_source
// isn't actually "per_user" on a bedrock_sso row. A row that carries none of
// the four is returned UNTOUCHED, which keeps a custom WARDYN_AGENT_IMAGES row
// byte-for-byte; a valid per_user bedrock_sso row round-trips unchanged.
function normalizeAgentRow(a: AgentProvider): AgentProvider {
  if (a.mechanism === "bedrock_sso" && a.credential_source === "per_user") return a;
  if (
    a.credential_source === undefined &&
    a.sso_start_url === undefined &&
    a.sso_account_id === undefined &&
    a.sso_role_name === undefined
  )
    return a;
  return { ...a, credential_source: undefined, sso_start_url: undefined, sso_account_id: undefined, sso_role_name: undefined };
}

function normalizeAgentProviders(p: AgentProviders): AgentProviders {
  return p.agents ? { ...p, agents: p.agents.map(normalizeAgentRow) } : p;
}

const MODEL_ACCESS_TONE: Record<string, "success" | "warning" | "neutral"> = {
  live: "success",
  // #158: not_applicable is neither a success nor a warning — it is the
  // admin-token principal's own answer ("this caller is a mechanism, not a
  // person"), never a claim that something needs attention.
  not_applicable: "neutral",
};

function ModelAccessNote({ access }: { access: SetupModelAccess }) {
  // NO CHIP for a state outside the six (MODEL_ACCESS_CHIP_LABEL's own doc
  // comment): the old final `else` painted MODEL_ACCESS_NOT_CONFIGURED over
  // anything unrecognised, so a daemon reporting `expired_renewable` — a live,
  // renewable credential — told the admin they were signed out. The server's own
  // action line still renders: unknown to us is not unknown to it.
  const label = MODEL_ACCESS_CHIP_LABEL[access.state];
  return (
    <div>
      {label && <Chip tone={MODEL_ACCESS_TONE[access.state] ?? "warning"}>{label}</Chip>}
      {/* The server's own words, verbatim — never reworded client-side. The one
          exception is `expiring`'s instant, re-composed through the SAME frozen
          template on the reader's own clock (modelAccessActionLine): the server
          formats RFC3339 UTC, and this row is read by people in other
          timezones. */}
      {access.action && <p className="mt-1 text-meta text-warning">{modelAccessActionLine(access)}</p>}
    </div>
  );
}

// The SIGNED-IN ADMIN'S OWN block (C4.2): the chip, the server's action line,
// the ADMIN_OWN_CHIP_NOTE, and (for the three actionable states) the sign-in
// CTA, which opens the shell's one door (#544 — this tab mounted its own pane
// before).
function ModelAccessSignIn({ access }: { access: SetupModelAccess }) {
  const door = useModelAccessDoor();
  return (
    <>
      <ModelAccessNote access={access} />
      <p className="mt-1 text-meta text-muted-foreground">{AGENTS.ADMIN_OWN_CHIP_NOTE}</p>
      {MODEL_ACCESS_ACTIONABLE.has(access.state) && (
        // outline, never a second teal — Save is the tab's one default.
        <Button
          type="button"
          size="sm"
          variant="outline"
          className="mt-2"
          onClick={() => door.openDoor({ for: { login: "aws" } })}
        >
          {AGENTS.SIGN_IN_AWS}
        </Button>
      )}
    </>
  );
}

// The providers that can serve this agent's runs: turned on, and used by it.
// A turned-off provider is never a candidate at run create
// (chooseModelProvider, internal/api), so it is neither offered as a default
// nor counted as the only one; when every provider serving the agent is off,
// the row reads G1. Packet MP-C draws no off case — this follows the server.
// The Model providers list counts off providers as set up on purpose: it is the
// page where they are turned back on.
function servingProviders(providers: ModelProvider[], harnessId: string): ModelProvider[] {
  return providers.filter((p) => !p.disabled && p.harnesses?.some((h) => h.harness === harnessId));
}

function providerName(p: ModelProvider): string {
  return p.name || MODEL_PROVIDERS.KIND[p.kind] || p.id;
}

// Packet MP-C's G1–G4. One candidate is a static line (QC-1) and writes
// nothing; several are a select (QC-5) over the row's default_provider.
function DefaultProviderField({
  harness,
  row,
  providers,
  operator,
  onUpdate,
}: {
  harness: SetupHarnessTool;
  row: AgentProvider;
  providers: ModelProvider[];
  operator: boolean;
  onUpdate: (next: AgentProvider) => void;
}) {
  const id = `agent-${harness.id}-default-provider`;
  if (harness.no_managed_auth) {
    return (
      <Field label={AGENTS.FIELD_DEFAULT_PROVIDER}>
        <p className="text-body text-muted-foreground">{AGENTS.MECHANISM_NONE}</p>
      </Field>
    );
  }
  const serving = servingProviders(providers, harness.id);
  // A stored default that is not among the turned-on providers (turned off
  // for an incident) is never hidden behind G1 or G2: the server refuses every
  // run of this agent until another default is chosen (chooseModelProvider),
  // so the row keeps the one control that gets out of that state.
  const offDefault = !!row.default_provider && !serving.some((p) => p.id === row.default_provider);
  if (serving.length === 0) {
    return (
      <Field label={AGENTS.FIELD_DEFAULT_PROVIDER}>
        {offDefault ? (
          // Nothing is on to choose instead, so the list's own A7 line says
          // what is true here (MODEL_PROVIDERS.OFF_STILL_DEFAULT).
          <p className="text-body text-warning">{MODEL_PROVIDERS.OFF_STILL_DEFAULT(harness.display)}</p>
        ) : (
          <p className="text-body text-info">{AGENTS.NO_PROVIDER(harness.display)}</p>
        )}
      </Field>
    );
  }
  if (serving.length === 1 && !offDefault) {
    return (
      <Field label={AGENTS.FIELD_DEFAULT_PROVIDER}>
        <p className="text-body text-foreground">{AGENTS.ONLY_PROVIDER(providerName(serving[0]), harness.display)}</p>
      </Field>
    );
  }
  return (
    <Field label={AGENTS.FIELD_DEFAULT_PROVIDER} htmlFor={id} hint={AGENTS.DEFAULT_HINT}>
      {/* A stored default that is off is not an option, so the select shows
          the placeholder until a turned-on provider is chosen — with one on,
          too (the one-click switch out of an incident). */}
      <Select
        value={serving.some((p) => p.id === row.default_provider) ? row.default_provider : ""}
        disabled={!operator}
        onValueChange={(v) => onUpdate({ ...row, default_provider: v })}
      >
        <SelectTrigger id={id} aria-label={`${AGENTS.FIELD_DEFAULT_PROVIDER} — ${harness.display}`}>
          <SelectValue placeholder={RAIL_PROVIDER.PLACEHOLDER} />
        </SelectTrigger>
        <SelectContent>
          {serving.map((p) => (
            <SelectItem key={p.id} value={p.id}>
              {AGENTS.DEFAULT_OPTION(providerName(p), MODEL_PROVIDERS.KIND[p.kind] ?? p.kind)}
            </SelectItem>
          ))}
        </SelectContent>
      </Select>
    </Field>
  );
}

function Row({
  harness,
  row,
  enabled,
  providers,
  modelAccess,
  operator,
  onUpdate,
}: {
  harness: SetupHarnessTool;
  row: AgentProvider;
  /** rowEnabled's answer — the server's roster, never `!row.disabled` on a row
   *  resolvedRow invented. */
  enabled: boolean;
  providers: ModelProvider[];
  modelAccess?: SetupModelAccess;
  operator: boolean;
  onUpdate: (next: AgentProvider) => void;
}) {
  // C4.2 is claude-code only (modelAccess is scoped server-side). #158: it
  // renders for not_applicable too — MODEL_ACCESS_CHIP_LABEL carries a real,
  // neutral label for it.
  const showModelAccess = harness.id === "claude-code" && !!modelAccess;

  return (
    <div className="rounded-lg border border-border" data-testid={`agent-row-${harness.id}`}>
      <div className="flex items-center gap-3 border-b border-border p-3">
        <Switch
          checked={enabled}
          disabled={!operator}
          label={`${PROVIDERS.FIELD_ENABLED} — ${harness.display}`}
          onChange={(checked) => onUpdate({ ...row, disabled: !checked })}
        />
        <div className="min-w-0 flex-1">
          <span className="block text-sm font-medium text-foreground">{harness.display}</span>
          <span className="block font-mono text-meta text-muted-foreground">{harness.id}</span>
        </div>
        {/* Off is a fact, never red (§4's colour rule). Its OWN canon key —
            slicing AGENT_ROW_DISABLED_HINT at its colon made the chip a
            side-effect of that sentence's punctuation, so a reworded hint
            silently reworded (or emptied) the chip. */}
        <Chip tone="neutral">{enabled ? PROVIDERS.FIELD_ENABLED : AGENTS.AGENT_ROW_DISABLED_CHIP}</Chip>
      </div>

      {/* UT-7b: kind agent, value = harness.id — present whether the row is
          on or off, the git-tab.tsx precedent (a disabled agent can still
          carry a stale audience list). */}
      <div className="border-b border-border p-3">
        <AvailabilityControl kind="agent" value={harness.id} />
      </div>

      {!enabled ? (
        <div className="p-3">
          <p className="text-body text-muted-foreground">{AGENTS.AGENT_ROW_DISABLED_HINT}</p>
        </div>
      ) : (
        <div className="space-y-4 p-3">
          <DefaultProviderField harness={harness} row={row} providers={providers} operator={operator} onUpdate={onUpdate} />
          {showModelAccess && (
            <div className="border-t border-border pt-3">
              <ModelAccessSignIn access={modelAccess!} />
            </div>
          )}
        </div>
      )}
    </div>
  );
}

export function AgentsTab({
  harnesses,
  modelAccess,
  operator,
  onRetryRoster,
  onStatusRefresh,
  onDirtyChange,
}: {
  /** The harness catalog off SetupStatus.harnesses. UNDEFINED is "unknown"
   *  (an older daemon omits the field, or the status read failed) — NEVER an
   *  empty roster: save() builds its whole PUT body from this, so an absent
   *  roster read as [] PUT `{agents: []}` and disabled every catalog agent on
   *  the deployment. Unknown renders the same fetch-failed state a failed GET
   *  does: no rows, no Save, nothing to PUT. */
  harnesses?: SetupHarnessTool[];
  /** The SIGNED-IN caller's own model-access state — claude-code only. */
  modelAccess?: SetupModelAccess;
  operator: boolean;
  /** Re-fires the PARENT's WHOLE load() — /workspace-providers AND
   *  /setup/status. For the roster-unknown Retry ONLY: there is no draft on
   *  screen to lose there, since the roster came up empty in the first place.
   *  NEVER call this from save() — see onStatusRefresh below. */
  onRetryRoster: () => void;
  /** Re-fires ONLY the parent's /setup/status read (never /workspace-
   *  providers), so a successful agents Save can refresh the stale
   *  modelAccess/harnesses it just changed without touching the parent's Git/
   *  Storage draft, its pending 412 banner, or its own error state. A plain
   *  `onRetryRoster` there was the staleness fix's own regression: it is the
   *  parent's full load(), which resets `draft` (the OTHER two tabs' unsaved
   *  edits), clears `savedElsewhere`, and a transient GET failure flips the
   *  whole screen to FETCH_FAILED right after a successful agent save. */
  onStatusRefresh: () => void;
  /** #460 — this tab is its own resource with its own draft, so the parent
   *  screen has no other way to know whether IT is dirty (for the PageHeader
   *  chip and the Agents Segmented option). Fired whenever the dirty fact
   *  changes, and once more with `false` on unmount (leaving the tab clears
   *  it, same as the tab's own draft resets on remount). */
  onDirtyChange?: (dirty: boolean) => void;
}) {
  const [draft, setDraft] = React.useState<AgentProviders | null>(null);
  // GET /model-providers: what each row's default can name. Read-only here —
  // Settings → Model providers owns that document.
  const [providers, setProviders] = React.useState<ModelProvider[]>([]);
  // #217 — the snapshot `draft` started from, same role as providers-screen's
  // own `original`: what "Copy my changes" and the unsaved-navigation guard
  // both diff against. This tab is its own resource with its own Save, so it
  // keeps its own baseline rather than sharing the parent's.
  const [original, setOriginal] = React.useState<AgentProviders | null>(null);
  const [etag, setEtag] = React.useState<string | null>(null);
  const [status, setStatus] = React.useState<"loading" | "error" | "ready">("loading");
  const [saving, setSaving] = React.useState(false);
  // #483: a save of this screen's was refused when the session ended.
  const [writeDropped, clearWriteDropped] = useWriteDropped("agent-providers");
  const [saveError, setSaveError] = React.useState<string | null>(null);
  const [savedElsewhere, setSavedElsewhere] = React.useState(false);

  const load = React.useCallback(() => {
    setStatus("loading");
    setSavedElsewhere(false);
    Promise.all([api.getAgentProviders(), providersApi.getModelProviders()])
      .then(([snap, mp]) => {
        setProviders(mp.providers.providers ?? []);
        const normalized = normalizeAgentProviders(snap.providers);
        setDraft(normalized);
        setOriginal(normalized);
        setEtag(snap.etag);
        setStatus("ready");
      })
      .catch(() => setStatus("error"));
  }, []);
  React.useEffect(load, [load]);

  const agents = draft?.agents ?? [];
  // #217 — see providers-screen.tsx's own changedLines/useUnsavedGuard pair.
  const changedLines = React.useMemo(() => readableDiff(original, draft), [original, draft]);
  useUnsavedGuard("agents-tab", changedLines.length > 0, () => JSON.stringify(draft, null, 2));
  // #460 — tells the parent screen this tab's own dirty fact (its PageHeader
  // chip and Agents Segmented option); `false` on unmount, since leaving this
  // tab drops the draft too (see AgentsTab's own file-header note).
  const dirty = changedLines.length > 0;
  React.useEffect(() => {
    onDirtyChange?.(dirty);
    return () => onDirtyChange?.(false);
    // eslint-disable-next-line react-hooks/exhaustive-deps -- onDirtyChange is a parent callback; `dirty` is the only real trigger
  }, [dirty]);
  const roster = harnesses ?? [];
  const catalogIds = new Set(roster.map((h) => h.id));
  // Any row not in this build's catalog (a custom WARDYN_AGENT_IMAGES id) is
  // preserved byte-for-byte — this tab offers no UI to author one, and a
  // rewrite here would be a silent drop of an admin's own row.
  const customRows = agents.filter((a) => !catalogIds.has(a.id));

  // In place, never [...rest, next]: appending moved every edited row to the end
  // of the persisted agents[], so a one-switch change wrote an agent_provider.write
  // diff that reordered the whole roster.
  const updateRow = (harnessId: string, next: AgentProvider) => {
    setDraft((d) => {
      const rows = d?.agents ?? [];
      return {
        ...d,
        agents: rows.some((a) => a.id === harnessId) ? rows.map((a) => (a.id === harnessId ? next : a)) : [...rows, next],
      };
    });
  };

  const save = async () => {
    setSaving(true);
    setSaveError(null);
    clearWriteDropped();
    try {
      // What the admin sees is what is written, in catalog order. A switch that
      // is ON contributes its row (the stored one, or the defaults for one just
      // enabled); a switch that is OFF contributes nothing unless a stored row
      // exists for it, which is kept and pinned off. Mapping every catalog id to
      // an invented row would widen the org's roster on an unrelated edit.
      const catalogRows = roster.flatMap((h) => {
        const stored = agents.find((a) => a.id === h.id);
        if (rowEnabled(agents, h)) return [stored ?? { id: h.id, mechanism: defaultMechanism(h) }];
        return stored ? [{ ...stored, disabled: true }] : [];
      });
      const next: AgentProviders = { agents: [...catalogRows, ...customRows] };
      const result = await api.putAgentProviders(next, etag);
      const normalized = normalizeAgentProviders(result.providers);
      setDraft(normalized);
      // #217 — the new baseline: a save with nothing left unsaved must not
      // still read as dirty to the guard above.
      setOriginal(normalized);
      setEtag(result.etag);
      toast.success(PROVIDERS.SAVED_TOAST);
      // modelAccess and harnesses are the PARENT's /setup/status read, never
      // this tab's own, and a save (a switch, a default) changes what that
      // read reports. Re-fire it on every success —
      // ONLY that read (onStatusRefresh), never the parent's whole
      // onRetryRoster/load(), which would also reset the Git/Storage draft
      // sitting on the OTHER two tabs (A-01).
      onStatusRefresh();
    } catch (e) {
      // #483: a 401 is the sign-in dialog's to answer; once the person is
      // back, writeDropped says this save never went through.
      if (e instanceof HttpError && e.status === 401) return;
      if (e instanceof HttpError && e.status === 412) {
        setSavedElsewhere(true);
      } else if (e instanceof HttpError && e.status === 400) {
        // The server's own 400, rendered verbatim — never a console reword.
        setSaveError(e.message);
      } else {
        toast.error(PROVIDERS.SAVE_ERROR, { description: getErrorMessage(e) });
      }
    } finally {
      setSaving(false);
    }
  };

  // An UNKNOWN roster is the same dead end as a failed GET, and is checked
  // FIRST: without a catalog there are no rows to render and no body to PUT,
  // so a Save control here would be a button whose only possible effect is to
  // wipe the deployment's agent policy.
  if (!harnesses) {
    return (
      <EmptyState
        icon={AlertTriangle}
        title={PROVIDERS.FETCH_FAILED_TITLE}
        description={PROVIDERS.FETCH_FAILED_BODY}
        action={
          <Button variant="outline" size="sm" onClick={onRetryRoster}>
            {ACCESS_STATE.FETCH_FAILED_RETRY}
          </Button>
        }
      />
    );
  }
  if (status === "loading") {
    return (
      <div className="space-y-4">
        <p className="text-body text-muted-foreground">{AGENTS.AGENTS_LEAD}</p>
        <TableSkeleton rows={3} cols={2} />
      </div>
    );
  }
  if (status === "error") {
    return (
      <EmptyState
        icon={AlertTriangle}
        title={PROVIDERS.FETCH_FAILED_TITLE}
        description={PROVIDERS.FETCH_FAILED_BODY}
        action={
          <Button variant="outline" size="sm" onClick={load}>
            {ACCESS_STATE.FETCH_FAILED_RETRY}
          </Button>
        }
      />
    );
  }

  return (
    <div className="space-y-4">
      <p className="text-body text-muted-foreground">{AGENTS.AGENTS_LEAD}</p>

      {/* F4-F3 (Appendix A V8): keep the draft mounted — the banner sits
          above the rows rather than replacing them, so an edit typed
          moments before the 412 is still readable. #217: Copy my changes
          before Discard mine and reload — there is no "Save over theirs" arm. */}
      {savedElsewhere && <SavedElsewhereBanner documentText={JSON.stringify(draft, null, 2)} onDiscard={load} />}
      {saveError && (
        <div className="rounded-lg border border-danger/30 bg-danger-subtle p-3 text-body text-danger">
          <b className="font-semibold">{PROVIDERS.SAVE_REFUSED_TITLE}</b>
          <p className="mt-0.5">{saveError}</p>
        </div>
      )}

      <div className="space-y-3">
        {roster.map((h) => (
          <Row
            key={h.id}
            harness={h}
            row={resolvedRow(agents, h)}
            enabled={rowEnabled(agents, h)}
            providers={providers}
            modelAccess={h.id === "claude-code" ? modelAccess : undefined}
            operator={operator}
            onUpdate={(next) => updateRow(h.id, next)}
          />
        ))}
      </div>

      {/* A daemon that legitimately reports an EMPTY roster is a different
          case from an absent one — the lead still reads, and the rows are
          simply none. But Save stays withheld: with no row on screen the
          only thing it could write is `{agents: []}`, which nobody asked
          for. A control appears when there is something to save. */}
      {roster.length > 0 && (
        <div className="flex items-center justify-end gap-3 border-t border-border pt-4">
          {/* #217 — beside the control, not only in a title tooltip. */}
          {!operator && <OperatorOnlyHint />}
          {operator && changedLines.length > 0 && (
            <span data-testid="unsaved-marker" className="mr-auto text-meta text-muted-foreground">
              {PROVIDERS_EXTRA.UNSAVED_MARKER}
            </span>
          )}
          {writeDropped && (
            <span role="status" className="text-meta text-warning">
              {REAUTH_DIALOG.WRITE_DROPPED}
            </span>
          )}
          <Button disabled={!operator || saving} onClick={save}>
            {PROVIDERS.SAVE_CTA}
          </Button>
        </div>
      )}
    </div>
  );
}
