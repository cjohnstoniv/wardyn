/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The Agents tab of /providers (§5c.4, design §5.3) — one row per harness-catalog
// id (+ any custom WARDYN_AGENT_IMAGES rows already stored, preserved unedited):
// a Switch (enabled) and the row's default model provider (packet MP-C's
// G1–G4). The mechanism radio, the credential toggle and the AWS start URL
// live on the provider now; since 0.8 (#548) a roster row carries none of them.
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
import { MODEL_PROVIDERS } from "../../../lib/model-providers-copy";
import { RAIL_PROVIDER } from "../../wardyn/copy/new-run-rail";
import type { SetupHarnessTool } from "../../../lib/types";
import { getErrorMessage } from "../../../lib/format";
import { readableDiff } from "../../../lib/readable-diff";
import { useUnsavedGuard } from "../../../lib/use-unsaved-guard";
import { useWriteDropped } from "../../../lib/use-write-dropped";
import { REAUTH_DIALOG } from "../../../lib/reauth-copy";
import { ACCESS_STATE } from "../../../lib/people-access-copy";
import { AGENTS, PROVIDERS, PROVIDERS_EXTRA } from "../../../lib/workspace-providers-copy";
import { Button } from "../../ui/button";
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "../../ui/select";
import { AvailabilityControl } from "../../wardyn/availability-control";
import { Field, Switch } from "../../wardyn/form-primitives";
import { Chip, OperatorOnlyHint } from "../../wardyn/primitives";
import { SavedElsewhereBanner } from "../../wardyn/saved-elsewhere-banner";
import { EmptyState, TableSkeleton } from "../../wardyn/states";

// The two catalog agents whose model credential folds to a coarse
// ai-integration type harness.go's ProviderTypes checks against
// (harnessCatalog); a no-managed-auth row ("none"/BYOA, or any custom
// WARDYN_AGENT_IMAGES id) has no capability here.
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

// The row to EDIT: the stored row if one exists, else a fresh one.
//
// Not the answer to "is this agent enabled" — see rowEnabled. A row this
// function invents carries no `disabled`, so reading `!row.disabled` off it
// would render every not-offered agent as ON.
function resolvedRow(agents: AgentProvider[], harness: SetupHarnessTool): AgentProvider {
  return agents.find((a) => a.id === harness.id) ?? { id: harness.id };
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
  operator,
  onUpdate,
}: {
  harness: SetupHarnessTool;
  row: AgentProvider;
  /** rowEnabled's answer — the server's roster, never `!row.disabled` on a row
   *  resolvedRow invented. */
  enabled: boolean;
  providers: ModelProvider[];
  operator: boolean;
  onUpdate: (next: AgentProvider) => void;
}) {

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
        </div>
      )}
    </div>
  );
}

export function AgentsTab({
  harnesses,
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
  operator: boolean;
  /** Re-fires the PARENT's WHOLE load() — /workspace-providers AND
   *  /setup/status. For the roster-unknown Retry ONLY: there is no draft on
   *  screen to lose there, since the roster came up empty in the first place.
   *  NEVER call this from save() — see onStatusRefresh below. */
  onRetryRoster: () => void;
  /** Re-fires ONLY the parent's /setup/status read (never /workspace-
   *  providers), so a successful agents Save can refresh the stale
   *  harnesses it just changed without touching the parent's Git/
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
        setDraft(snap.providers);
        setOriginal(snap.providers);
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
        if (rowEnabled(agents, h)) return [stored ?? { id: h.id }];
        return stored ? [{ ...stored, disabled: true }] : [];
      });
      const next: AgentProviders = { agents: [...catalogRows, ...customRows] };
      const result = await api.putAgentProviders(next, etag);
      setDraft(result.providers);
      // #217 — the new baseline: a save with nothing left unsaved must not
      // still read as dirty to the guard above.
      setOriginal(result.providers);
      setEtag(result.etag);
      toast.success(PROVIDERS.SAVED_TOAST);
      // harnesses are the PARENT's /setup/status read, never
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
