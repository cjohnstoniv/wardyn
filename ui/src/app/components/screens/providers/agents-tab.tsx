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
import { ACCESS_STATE } from "../../../lib/people-access-copy";
import {
  AGENTS,
  AGENTS_DRAFT,
  isPerUserSsoRow,
  MODEL_ACCESS_ACTIONABLE,
  MODEL_ACCESS_CHIP_LABEL,
  PROVIDERS,
  PROVIDERS_DRAFT,
} from "../../../lib/workspace-providers-copy";
import { modelAccessActionLine } from "../../../lib/model-access";
import { Button } from "../../ui/button";
import { Input } from "../../ui/input";
import { Field, Switch } from "../../wardyn/form-primitives";
import { Chip } from "../../wardyn/primitives";
import { EmptyState, TableSkeleton } from "../../wardyn/states";
import { HarnessLoginPane, isLikelyStartUrl } from "../settings/harness-login-pane";
import { useRovingRadio } from "../../wardyn/use-roving-radio";

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

// F4-F9 (Appendix A V8): a bedrock_sso row declared "Per person" with no
// start URL is a guaranteed 400 (agent_providers.go's
// validateAgentCredentialSource) — the Git tab already withholds Save for
// its own invalid rows (invalidGitRow, providers-screen.tsx); this is the
// same rule for this tab. NOT gated on `enabled`: the server validates every
// stored row it is handed, on or off (the git-tab precedent, display.tsx's
// own comment on gitRowInvalid).
export function agentRowInvalid(row: AgentProvider): boolean {
  return row.mechanism === "bedrock_sso" && row.credential_source === "per_user" && !isLikelyStartUrl(row.sso_start_url ?? "");
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

// per_user is captured for an AWS SSO sign-in ONLY (validateAgentCredentialSource,
// and PER_USER_UNAVAILABLE says so). A STORED row pairing it with any other
// mechanism — OR pairing bedrock_sso with a SHARED credential_source — is a
// row the admin can neither see nor clear: "Per person" painted active AND
// disabled (or simply not offered), the start-URL/pin fields hidden, and all
// four re-PUT verbatim by the next unrelated Save.
//
// So all four are dropped ON LOAD whenever credential_source isn't actually
// "per_user" on a bedrock_sso row — the SAME one clause the mechanism radio's
// own onChange and the Shared button already clear on an edit — the row
// renders `shared`, and the next Save writes what the admin was shown. A row
// that carries none of the four is returned UNTOUCHED, which is what keeps a
// custom WARDYN_AGENT_IMAGES row byte-for-byte.
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

const MODEL_ACCESS_TONE: Record<string, "success" | "warning"> = { live: "success" };

function ModelAccessNote({ access }: { access: SetupModelAccess }) {
  // NO CHIP for a state outside the five (MODEL_ACCESS_CHIP_LABEL's own doc
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
// CTA or the open login pane. Shared between the ordinary bottom placement
// and the prominent per_user banner at the top of the row (Appendix A finding
// 4) — the content is identical, only the wrapper around it differs.
function ModelAccessSignIn({
  access,
  loginOpen,
  setLoginOpen,
  startURLManaged,
}: {
  access: SetupModelAccess;
  loginOpen: boolean;
  setLoginOpen: (open: boolean) => void;
  startURLManaged: boolean;
}) {
  return (
    <>
      <ModelAccessNote access={access} />
      <p className="mt-1 text-meta text-muted-foreground">{AGENTS.ADMIN_OWN_CHIP_NOTE}</p>
      {loginOpen ? (
        <div className="mt-2">
          {/* Under a per_user row the server signs in against THAT row's
              stored sso_start_url and ignores a typed one, so the pane's
              start-URL field is suppressed for a note (the member's CTA
              does the same). A shared row has nothing stored, so the
              ordinary flow still asks. */}
          <HarnessLoginPane
            provider="aws"
            startURLManaged={startURLManaged}
            onDone={() => setLoginOpen(false)}
            onCancel={() => setLoginOpen(false)}
          />
        </div>
      ) : (
        MODEL_ACCESS_ACTIONABLE.has(access.state) && (
          // outline, never a second teal — Save is the tab's one default.
          <Button type="button" size="sm" variant="outline" className="mt-2" onClick={() => setLoginOpen(true)}>
            {AGENTS.SIGN_IN_AWS}
          </Button>
        )
      )}
    </>
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
  // U-03 (blind review lens-U): the SERVER's settled row, never the unsaved
  // draft — `harness` is this build's /setup/status read, `row` is the
  // draft being edited. A live CTA (the banner, or a suppressed start-URL
  // prompt) on a draft that hasn't been saved yet leads straight to
  // harnesscred.go's 400: it reads the STORED row's start URL, which is
  // still empty (or still someone else's) until Save + a status refresh.
  // R-01 (review): shares isPerUserSsoRow with connection-cards.tsx's
  // perUserSso — same three-part test (enabled/mechanism/credential_source),
  // one predicate instead of two that can drift.
  const perUserSaved = isPerUserSsoRow(harness);

  // F4-F13 (Appendix A V8): the credential-source group had no roving
  // tabindex or arrow keys — two role="radio" Buttons were each their own
  // Tab stop (wardyn/use-roving-radio.ts, the connection-cards.tsx/
  // git-tab.tsx precedent). This group nests no expanded body, so only the
  // roving half applies here.
  //
  // R-1 (blind review, fix pass): useRovingRadio's moveTo() calls onSelect
  // unconditionally — it has no notion of a disabled item. "Per person" is
  // disabled off a non-bedrock_sso row (perUserAvailable, below), a guard
  // the MOUSE path enforces (the Button's own `disabled`) but the keyboard
  // path bypassed: ArrowRight/End on "Shared" wrote credential_source:
  // "per_user" on e.g. a codex-cli row, which agentRowInvalid never checks
  // (bedrock_sso only) — Save stayed enabled over a guaranteed
  // agent400PerUser 400. Fix: a 1-item group off a non-bedrock_sso row, so
  // every arrow key folds to index 0 (a no-op re-select of "Shared") —
  // credentialSource can only BE "per_user" when perUserAvailable is true
  // (normalizeAgentRow's own invariant), so this never hides the checked item.
  const sourceGroup = useRovingRadio(perUserAvailable ? 2 : 1, credentialSource === "shared" ? 0 : 1, (i) =>
    i === 0
      ? onUpdate({ ...row, credential_source: undefined, sso_start_url: undefined, sso_account_id: undefined, sso_role_name: undefined })
      : onUpdate({ ...row, credential_source: "per_user" }),
  );

  // C4.2 is claude-code only (modelAccess is scoped server-side) and NEVER
  // renders for not_applicable (finding 5 — the admin-token principal's own
  // answer; an empty chip with ADMIN_OWN_CHIP_NOTE still under it would be a
  // claim with nothing behind it).
  const showModelAccess = harness.id === "claude-code" && !!modelAccess && modelAccess.state !== "not_applicable";
  // Prominence (finding 4): a per_user row with something actionable to do
  // moves this block to the TOP of the row instead of its usual spot at the
  // bottom — the legacy Settings door stops being the one an admin reaches
  // for right after declaring the lane. Gated on perUserSaved (U-03), not
  // the draft's credentialSource: prominence promises a working CTA.
  const modelAccessProminent = showModelAccess && perUserSaved && MODEL_ACCESS_ACTIONABLE.has(modelAccess!.state);

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
          {/* Appendix A finding 4: declaring the lane (this tab) and
              authenticating to it (Settings → Model provider) were on
              different screens with no link between them. Under a per_user
              row with something actionable to do, the admin's own sign-in
              moves to the TOP of the row, in a tinted panel that says WHY —
              the legacy Settings door stops being the one reached for. */}
          {modelAccessProminent && (
            <div
              className="rounded-lg border border-warning/30 bg-warning-subtle p-3"
              data-testid="per-user-sign-in-banner"
            >
              {/* role="status" covers ONLY the title/body pair — NOT
                  ModelAccessSignIn below, which can open HarnessLoginPane's
                  own multi-step device-code/poll flow. A live region around
                  that whole flow would re-announce it wholesale on every
                  poll tick. */}
              <div role="status">
                <p className="text-sm font-medium text-foreground">{AGENTS_DRAFT.PER_USER_SIGN_IN_TITLE}</p>
                <p className="mt-1 text-body text-muted-foreground">{AGENTS_DRAFT.PER_USER_SIGN_IN_BODY}</p>
              </div>
              <div className="mt-2">
                <ModelAccessSignIn
                  access={modelAccess!}
                  loginOpen={loginOpen}
                  setLoginOpen={setLoginOpen}
                  startURLManaged={perUserSaved}
                />
              </div>
            </div>
          )}

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
                            : {
                                ...row,
                                mechanism: c.value,
                                credential_source: undefined,
                                sso_start_url: undefined,
                                sso_account_id: undefined,
                                sso_role_name: undefined,
                              },
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
              {/* role=radiogroup + role=radio, not two bare buttons (V2/F3): this is
                  a one-of-two form choice exactly like the mechanism picker above
                  it, and selection was conveyed by the `variant` styling alone —
                  a screen-reader user could not tell which source was chosen.
                  Buttons keep the mock's segmented look (Segmented itself is an
                  aria-pressed control every other screen's suite asserts, so it
                  is not the primitive to re-point here); role + aria-checked is
                  what AT reads, and a button is keyboard-operable already. */}
              <div role="radiogroup" aria-label={AGENTS.FIELD_SOURCE} className="flex gap-2" {...sourceGroup.containerProps}>
                <Button
                  type="button"
                  role="radio"
                  aria-checked={credentialSource === "shared"}
                  size="sm"
                  variant={credentialSource === "shared" ? "secondary" : "outline"}
                  disabled={!operator}
                  tabIndex={sourceGroup.itemProps(0).tabIndex}
                  ref={sourceGroup.itemProps(0).radioRef}
                  onClick={() =>
                    onUpdate({
                      ...row,
                      credential_source: undefined,
                      sso_start_url: undefined,
                      sso_account_id: undefined,
                      sso_role_name: undefined,
                    })
                  }
                >
                  {AGENTS.SOURCE_SHARED}
                </Button>
                <Button
                  type="button"
                  role="radio"
                  aria-checked={credentialSource === "per_user"}
                  size="sm"
                  variant={credentialSource === "per_user" ? "secondary" : "outline"}
                  disabled={!operator || !perUserAvailable}
                  title={perUserAvailable ? undefined : AGENTS.PER_USER_UNAVAILABLE}
                  tabIndex={sourceGroup.itemProps(1).tabIndex}
                  ref={sourceGroup.itemProps(1).radioRef}
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
            <>
              <Field label={AGENTS.FIELD_SSO_START_URL} hint={AGENTS.SSO_START_URL_HINT} htmlFor={`agent-${row.id}-sso-start-url`}>
                <Input
                  id={`agent-${row.id}-sso-start-url`}
                  value={row.sso_start_url ?? ""}
                  disabled={!operator}
                  aria-invalid={agentRowInvalid(row)}
                  onChange={(e) => onUpdate({ ...row, sso_start_url: e.target.value.trim() })}
                  placeholder="https://my-org.awsapps.com/start"
                  className="font-mono"
                />
              </Field>
              {agentRowInvalid(row) && (
                <p className="-mt-2 text-xs leading-snug text-danger">{AGENTS_DRAFT.SSO_START_URL_REQUIRED}</p>
              )}
              {/* The roster pin (Appendix A finding 1, ask 1): optional, ADMIN-OWNED like the
                  start URL above it — set together, or left blank, never
                  independently (agent400SSOPinPair). */}
              <Field
                label={AGENTS_DRAFT.FIELD_SSO_ACCOUNT_ID}
                hint={AGENTS_DRAFT.SSO_ACCOUNT_ID_HINT}
                htmlFor={`agent-${row.id}-sso-account-id`}
              >
                <Input
                  id={`agent-${row.id}-sso-account-id`}
                  value={row.sso_account_id ?? ""}
                  disabled={!operator}
                  onChange={(e) => onUpdate({ ...row, sso_account_id: e.target.value.trim() })}
                  placeholder="111111111111"
                  className="font-mono"
                />
              </Field>
              <Field
                label={AGENTS_DRAFT.FIELD_SSO_ROLE_NAME}
                hint={AGENTS_DRAFT.SSO_ROLE_NAME_HINT}
                htmlFor={`agent-${row.id}-sso-role-name`}
              >
                <Input
                  id={`agent-${row.id}-sso-role-name`}
                  value={row.sso_role_name ?? ""}
                  disabled={!operator}
                  onChange={(e) => onUpdate({ ...row, sso_role_name: e.target.value.trim() })}
                  placeholder="BedrockRunner"
                  className="font-mono"
                />
              </Field>
            </>
          )}

          {/* The SIGNED-IN ADMIN'S OWN chip (C4.2), NOT prominent: an ordinary
              claude-code row (shared credential, or per_user with nothing
              actionable) keeps the block at the bottom, exactly as before. */}
          {showModelAccess && !modelAccessProminent && (
            <div className="border-t border-border pt-3">
              <ModelAccessSignIn
                access={modelAccess!}
                loginOpen={loginOpen}
                setLoginOpen={setLoginOpen}
                startURLManaged={perUserSaved}
              />
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
        setDraft(normalizeAgentProviders(snap.providers));
        setEtag(snap.etag);
        setStatus("ready");
      })
      .catch(() => setStatus("error"));
  }, []);
  React.useEffect(load, [load]);

  const agents = draft?.agents ?? [];
  // F4-F9: any stored row the server is guaranteed to 400 withholds Save —
  // the Git tab's own invalidGitRow precedent.
  const invalidAgentRow = agents.some(agentRowInvalid);
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
      setDraft(normalizeAgentProviders(result.providers));
      setEtag(result.etag);
      toast.success(PROVIDERS.SAVED_TOAST);
      // The staleness root cause (Appendix A finding 4): modelAccess is the
      // PARENT's /setup/status read, never this tab's own — right after
      // declaring per_user the admin's own door renders whatever that read
      // last saw, which can predate this save. Re-fire it on every success —
      // ONLY that read (onStatusRefresh), never the parent's whole
      // onRetryRoster/load(), which would also reset the Git/Storage draft
      // sitting on the OTHER two tabs (A-01).
      onStatusRefresh();
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

      {/* F4-F3 (Appendix A V8, corrected verdict, rule 8 — this tab's own
          412 was HELD until 0.7.3 shipped, now unblocked): keep the draft
          MOUNTED — the banner sits ABOVE the rows rather than replacing
          them, so an edit typed moments before the 412 is still readable.
          ONE control, "Discard mine and reload": the corrected verdict
          REFUSES a "Save over theirs" arm. */}
      {savedElsewhere && (
        <div className="space-y-3 rounded-lg border border-warning/30 bg-warning-subtle p-4">
          <p className="text-sm font-medium text-foreground">{PROVIDERS.SAVED_ELSEWHERE_TITLE}</p>
          <p className="text-body text-muted-foreground">{PROVIDERS.SAVED_ELSEWHERE_BODY}</p>
          <Button variant="outline" size="sm" onClick={load}>
            {PROVIDERS_DRAFT.DISCARD_AND_RELOAD}
          </Button>
        </div>
      )}
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
          <Button disabled={!operator || saving || invalidAgentRow} onClick={save}>
            {PROVIDERS.SAVE_CTA}
          </Button>
        </div>
      )}
    </div>
  );
}
