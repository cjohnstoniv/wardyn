/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The Agents tab of /providers (§5c.4) — one row per harness-catalog id (+ any
// custom WARDYN_AGENT_IMAGES rows already stored, preserved unedited): a Switch
// (enabled), a mechanism radio over the model card's own lanes (reused, folded
// to the catalog's coarse values — the impossible-pair reasons are the SAME
// verbatim strings the model-provider capability table already carries,
// lib/integrations.ts's IMPOSSIBLE), a credential-source toggle (per_user
// offered only for bedrock_sso), the SSO start URL when per_user applies, and —
// on the claude-code row only (SetupStatus.model_access is scoped to it
// server-side) — the SIGNED-IN ADMIN'S OWN model-access chip (their own
// credential, C4.2): outline action, never a second teal.
//
// This tab is a SEPARATE resource from the Git/Storage tabs (SiteConfig's
// agent_providers block, its own GET/PUT), so it fetches and saves itself —
// the UserDrivesCard precedent for a self-contained tab widget — rather than
// riding the parent screen's WorkspaceProviders draft/save.
//
// Every user-visible string here is workspace-providers-copy.ts's AGENTS/
// PROVIDERS (§7.7, frozen) or reused canon it names. This file adds none.
import * as React from "react";
import { AlertTriangle } from "lucide-react";
import { toast } from "sonner";
import { HttpError } from "../../../lib/api/core";
import { agentProviders as api, type AgentProvider, type AgentProviders } from "../../../lib/api/agent-providers";
import { AI_TYPES, IMPOSSIBLE, type AiType } from "../../../lib/integrations";
import type { SetupHarnessTool, SetupModelAccess } from "../../../lib/types";
import { getErrorMessage } from "../../../lib/format";
import { AGENTS, PROVIDERS } from "../../../lib/workspace-providers-copy";
import { Button } from "../../ui/button";
import { Input } from "../../ui/input";
import { Field, Switch } from "../../wardyn/form-primitives";
import { Chip } from "../../wardyn/primitives";
import { EmptyState, TableSkeleton } from "../../wardyn/states";
import { HarnessLoginPane } from "../settings/harness-login-pane";

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

interface MechChoice {
  value: string;
  label: string;
  /** Set = disabled, and this is why — the catalog's own verbatim reason
   *  (harness.go's reasonX* constants, mirrored client-side in IMPOSSIBLE so
   *  the impossibility text can't drift from the capability table it names). */
  reason?: string;
}

// The row's candidate mechanisms, flattened rather than nested: the model
// card's three top-level lanes (subscription/api key/Bedrock) plus Bedrock's
// four sub-choices, one flat list — reusing the SAME lane titles
// (lib/integrations.ts's AI_TYPES) and the SAME bedrock labels
// (workspace-providers-copy.ts's AGENTS.MECHANISM_BEDROCK_*).
function mechanismChoices(harness: SetupHarnessTool): MechChoice[] {
  if (harness.no_managed_auth) return [{ value: "none", label: AGENTS.MECHANISM_NONE }];
  const cap = agentCapabilityFor(harness.id);
  const reasonFor = (t: AiType) => (cap ? IMPOSSIBLE[t]?.[cap] : undefined);
  return [
    { value: "anthropic_subscription", label: AI_TYPES.anthropic_subscription.title, reason: reasonFor("anthropic_subscription") },
    { value: "anthropic_api_key", label: AI_TYPES.anthropic_api_key.title, reason: reasonFor("anthropic_api_key") },
    { value: "openai_api_key", label: AI_TYPES.openai_api_key.title, reason: reasonFor("openai_api_key") },
    { value: "bedrock_bearer", label: AGENTS.MECHANISM_BEDROCK_BEARER, reason: reasonFor("bedrock") },
    { value: "bedrock_sso", label: AGENTS.MECHANISM_BEDROCK_SSO, reason: reasonFor("bedrock") },
    { value: "bedrock_env", label: AGENTS.MECHANISM_BEDROCK_ENV, reason: reasonFor("bedrock") },
    { value: "bedrock_aws_dir", label: AGENTS.MECHANISM_BEDROCK_AWS_DIR, reason: reasonFor("bedrock") },
  ];
}

function defaultMechanism(harness: SetupHarnessTool): string {
  const choices = mechanismChoices(harness);
  return (choices.find((c) => !c.reason) ?? choices[0]).value;
}

// The row to EDIT: the stored row if one exists, else a fresh one seeded with a
// mechanism this agent can actually use — a lanes-catalog row with an empty
// mechanism 400s at save (validateAgentMechanism's agent400NeedsLane), so the
// picker never shows a blank, unlaunchable choice.
//
// IT IS NOT THE ANSWER TO "IS THIS AGENT ENABLED" — see rowEnabled. A row this
// function invents carries no `disabled`, so reading `!row.disabled` off it
// rendered every not-offered agent as ON.
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

const MODEL_ACCESS_TONE: Record<string, "success" | "warning"> = { live: "success" };

function ModelAccessNote({ access }: { access: SetupModelAccess }) {
  const label =
    access.state === "live"
      ? AGENTS.MODEL_ACCESS_LIVE
      : access.state === "expiring"
        ? AGENTS.MODEL_ACCESS_EXPIRING
        : access.state === "expired_signin"
          ? AGENTS.MODEL_ACCESS_EXPIRED
          : access.state === "shared_expired"
            ? AGENTS.MODEL_ACCESS_SHARED_EXPIRED
            : AGENTS.MODEL_ACCESS_NOT_CONFIGURED;
  return (
    <div>
      <Chip tone={MODEL_ACCESS_TONE[access.state] ?? "warning"}>{label}</Chip>
      {/* The server's own words, verbatim — never reworded client-side. */}
      {access.action && <p className="mt-1 text-meta text-warning">{access.action}</p>}
    </div>
  );
}

function Row({
  harness,
  row,
  enabled,
  modelAccess,
  operator,
  onUpdate,
}: {
  harness: SetupHarnessTool;
  row: AgentProvider;
  /** rowEnabled's answer — the server's roster, never `!row.disabled` on a row
   *  resolvedRow invented. */
  enabled: boolean;
  modelAccess?: SetupModelAccess;
  operator: boolean;
  onUpdate: (next: AgentProvider) => void;
}) {
  const [loginOpen, setLoginOpen] = React.useState(false);
  const choices = mechanismChoices(harness);
  const perUserAvailable = row.mechanism === "bedrock_sso";
  const credentialSource = row.credential_source || "shared";

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

      {!enabled ? (
        <div className="p-3">
          <p className="text-body text-muted-foreground">{AGENTS.AGENT_ROW_DISABLED_HINT}</p>
        </div>
      ) : (
        <div className="space-y-4 p-3">
          {choices.length > 1 ? (
            <Field label={AGENTS.FIELD_MECHANISM} hint={AGENTS.MECHANISM_HINT}>
              <div role="radiogroup" aria-label={AGENTS.FIELD_MECHANISM} className="space-y-1.5">
                {choices.map((c) => (
                  <label key={c.value} className="flex items-start gap-2">
                    <input
                      type="radio"
                      className="mt-0.5"
                      name={`mechanism-${harness.id}`}
                      checked={row.mechanism === c.value}
                      disabled={!operator || !!c.reason}
                      onChange={() =>
                        onUpdate(
                          c.value === "bedrock_sso"
                            ? { ...row, mechanism: c.value }
                            : { ...row, mechanism: c.value, credential_source: undefined, sso_start_url: undefined },
                        )
                      }
                    />
                    <span>
                      <span className="block text-body font-medium text-foreground">{c.label}</span>
                      {c.reason && <span className="block text-meta text-muted-foreground">{c.reason}</span>}
                    </span>
                  </label>
                ))}
              </div>
            </Field>
          ) : (
            <p className="text-body text-muted-foreground">{AGENTS.MECHANISM_NONE_HINT}</p>
          )}

          {!harness.no_managed_auth && (
            <Field label={AGENTS.FIELD_SOURCE}>
              <div className="flex gap-2">
                <Button
                  type="button"
                  size="sm"
                  variant={credentialSource === "shared" ? "secondary" : "outline"}
                  disabled={!operator}
                  onClick={() => onUpdate({ ...row, credential_source: undefined, sso_start_url: undefined })}
                >
                  {AGENTS.SOURCE_SHARED}
                </Button>
                <Button
                  type="button"
                  size="sm"
                  variant={credentialSource === "per_user" ? "secondary" : "outline"}
                  disabled={!operator || !perUserAvailable}
                  title={perUserAvailable ? undefined : AGENTS.PER_USER_UNAVAILABLE}
                  onClick={() => onUpdate({ ...row, credential_source: "per_user" })}
                >
                  {AGENTS.SOURCE_PER_USER}
                </Button>
              </div>
              <p className="mt-1 text-meta text-muted-foreground">
                {credentialSource === "per_user" ? AGENTS.SOURCE_PER_USER_HINT : AGENTS.SOURCE_SHARED_HINT}
              </p>
              {!perUserAvailable && <p className="text-meta text-muted-foreground">{AGENTS.PER_USER_UNAVAILABLE}</p>}
            </Field>
          )}

          {perUserAvailable && credentialSource === "per_user" && (
            <Field label={AGENTS.FIELD_SSO_START_URL} hint={AGENTS.SSO_START_URL_HINT} htmlFor={`agent-${row.id}-sso-start-url`}>
              <Input
                id={`agent-${row.id}-sso-start-url`}
                value={row.sso_start_url ?? ""}
                disabled={!operator}
                onChange={(e) => onUpdate({ ...row, sso_start_url: e.target.value })}
                placeholder="https://my-org.awsapps.com/start"
                className="font-mono"
              />
            </Field>
          )}

          {/* The SIGNED-IN ADMIN'S OWN chip (C4.2) — SetupStatus.model_access is
              scoped to claude-code only (modelAccessAgent, internal/api). */}
          {harness.id === "claude-code" && modelAccess && (
            <div className="border-t border-border pt-3">
              <ModelAccessNote access={modelAccess} />
              <p className="mt-1 text-meta text-muted-foreground">{AGENTS.ADMIN_OWN_CHIP_NOTE}</p>
              {loginOpen ? (
                <div className="mt-2">
                  <HarnessLoginPane provider="aws" onDone={() => setLoginOpen(false)} onCancel={() => setLoginOpen(false)} />
                </div>
              ) : (
                (modelAccess.state === "not_configured" ||
                  modelAccess.state === "expired_signin" ||
                  modelAccess.state === "expiring") && (
                  // outline, never a second teal — Save is the tab's one default.
                  <Button type="button" size="sm" variant="outline" className="mt-2" onClick={() => setLoginOpen(true)}>
                    {AGENTS.SIGN_IN_AWS}
                  </Button>
                )
              )}
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
}) {
  const [draft, setDraft] = React.useState<AgentProviders | null>(null);
  const [etag, setEtag] = React.useState<string | null>(null);
  const [status, setStatus] = React.useState<"loading" | "error" | "ready">("loading");
  const [saving, setSaving] = React.useState(false);
  const [saveError, setSaveError] = React.useState<string | null>(null);
  const [savedElsewhere, setSavedElsewhere] = React.useState(false);

  const load = React.useCallback(() => {
    setStatus("loading");
    setSavedElsewhere(false);
    api
      .getAgentProviders()
      .then((snap) => {
        setDraft(snap.providers);
        setEtag(snap.etag);
        setStatus("ready");
      })
      .catch(() => setStatus("error"));
  }, []);
  React.useEffect(load, [load]);

  const agents = draft?.agents ?? [];
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
    try {
      // WHAT THE ADMIN SEES IS WHAT IS WRITTEN, in catalog order. A switch that
      // is ON contributes its row (the stored one, or the defaults for one just
      // enabled); a switch that is OFF contributes NOTHING unless a stored row
      // exists for it, which is kept and pinned off. Mapping every catalog id to
      // an invented row is what made an unrelated edit widen the org's roster.
      const catalogRows = roster.flatMap((h) => {
        const stored = agents.find((a) => a.id === h.id);
        if (rowEnabled(agents, h)) return [stored ?? { id: h.id, mechanism: defaultMechanism(h) }];
        return stored ? [{ ...stored, disabled: true }] : [];
      });
      const next: AgentProviders = { agents: [...catalogRows, ...customRows] };
      const result = await api.putAgentProviders(next, etag);
      setDraft(result.providers);
      setEtag(result.etag);
      toast.success(PROVIDERS.SAVED_TOAST);
    } catch (e) {
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
          <Button variant="outline" size="sm" onClick={load}>
            Retry
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
            Retry
          </Button>
        }
      />
    );
  }

  return (
    <div className="space-y-4">
      <p className="text-body text-muted-foreground">{AGENTS.AGENTS_LEAD}</p>

      {savedElsewhere ? (
        <div className="space-y-3 rounded-lg border border-warning/30 bg-warning-subtle p-4">
          <p className="text-sm font-medium text-foreground">{PROVIDERS.SAVED_ELSEWHERE_TITLE}</p>
          <p className="text-body text-muted-foreground">{PROVIDERS.SAVED_ELSEWHERE_BODY}</p>
          <Button variant="outline" size="sm" onClick={load}>
            Retry
          </Button>
        </div>
      ) : (
        <>
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
            <div className="flex justify-end border-t border-border pt-4">
              <Button disabled={!operator || saving} onClick={save}>
                {PROVIDERS.SAVE_CTA}
              </Button>
            </div>
          )}
        </>
      )}
    </div>
  );
}
