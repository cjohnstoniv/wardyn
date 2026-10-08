/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// PolicyPanel — one policy-source authoring surface, two instances:
//
//   instance="policies"  the admin policy editor's body (the name field stays
//                        on the screen; the operator gate stays on Save).
//   instance="run"       the new-run screen's policy card — the same source, plus
//                        an optional "reuse a saved policy" mode row and an
//                        optional Preflight button.
//
// `inline_policy` on POST /runs is the identical Go struct a stored policy
// holds (types.RunPolicySpec), validated by the same validatePolicySpec — so
// one panel serves both, and the server stays the source of truth for what is
// legal. The source is one string the caller holds (YAML unless JSON is chosen);
// the panel only parses it (so a broken document never reaches the API), derives
// read-only summaries from what parsed, and writes every structured edit back
// through a parse so the rest of the text, comments included, is kept. The one
// live call it makes is the SafetyMeter's debounced grade of the current
// document (POST /policies/grade) — advisory, and grading the document, which
// is a different question than the screen's preflight of the resolved run.
//
// The screen owns everything that isn't the source: the saved-policy list, the
// preflight call (runs.preflightRun) and its result rendering, the Workspace
// card's mounts/repos, and any post-parse union it does before submit.
//
// instance="policies" is consumed by policies.tsx's PolicyEditor; instance="run"
// by new-run-screen.tsx's "Policy" SectionCard.
import * as React from "react";
import { Globe, Plus, ShieldCheck, Timer } from "lucide-react";
import type { PolicySourceEditResult } from "../../lib/policy-document";
import type { ConfinementClass, RunPolicySpec, SetupModelProvider } from "../../lib/types";
import { POLICY_TEMPLATE_COPY as C } from "./copy/policy-templates";
import { templateProviders } from "./policy-template-providers";
import { Button } from "../ui/button";
import { cn } from "../ui/utils";
import { Chip, ConfinementChip, SectionLabel } from "./primitives";
import { SafetyMeter } from "./safety-meter";
import { OptionCard } from "./form-primitives";
import { FIELD_HELP } from "./policy-field-help";
import { ToolRulesSection } from "./policy-tool-rules";
import { PushRulesSection } from "./policy-push-rules";
import { ADOCapabilitiesSection } from "./policy-ado-capabilities";
import { RAIL_CHECK } from "./copy";
import { GitPATSection } from "./policy-git-pat";
import { DefaultPolicyPreview, type DefaultPolicyView } from "./policy-default-preview";
import { egressSummary, lifecycleSummary } from "./policy-document/policy-facts";
import { PolicyEditor } from "./policy-document/policy-editor";
import {
  applySpecChange,
  parseSpec,
  setSpecKey,
  specToSource,
  type ParsedSpec,
  type PolicySourceError,
  type PolicySourceFormat,
} from "./policy-document/policy-source";

export type PolicyPanelInstance = "run" | "policies";

/** The run instance's mode row: launch under the deployment default, reuse a stored policy, or author one. */
export type PolicyMode = "default" | "saved" | "custom";

/* ---------- templates ---------- */

export interface PolicyTemplate {
  id: string;
  label: string;
  /** One line on the chip's tooltip — what this policy lets a run do. */
  hint: string;
  spec: RunPolicySpec;
}

// Seeded from examples/policies/*.json (the worked set docs/POLICIES.md points
// at), as consts rather than a fetch — there is no template endpoint, and a
// deployment that wants its own gallery is the day to add one.
//
// claude-subscription.template.json is deliberately not here: its `__comment`
// key and machine-specific mounts would 400 under DisallowUnknownFields /
// validatePolicySpec the moment anyone clicked it.
//
// auto_stop_after_sec: every template except allow-all carries 3600. Under the
// safety meter's conservative (non-interactive) frame an omitted idle cap grades
// high on its own (composer/risk.go: never-reap holds minted credentials forever),
// so without it the shipped starter would read "Elevated" — contradicting the
// owner's safest axis. allow-all deliberately omits it: its two highs (allow-all
// egress + never-reap) are what make "Weakest" reachable from a single template
// click, and a 3600 there would collapse it to one high. ci/model-provider
// inherit 3600 from their source examples; minimal/registries gain it here for
// the meter. Absent still coalesces to 0 = never reaped (internal/lifecycle).
export function policyTemplates(providers?: readonly SetupModelProvider[]): readonly PolicyTemplate[] {
  const tp = templateProviders(providers);
  return [
  {
    id: "minimal",
    label: C.MINIMAL,
    hint: tp.minimalHint,
    // The shipped starter (policies.tsx's STARTER_SPEC, and the run wizard's
    // fresh Custom-policy prefill via new-run-screen's MINIMAL.spec) — a valid,
    // editable floor, not a blank document. The 3600 idle cap is the meter fix:
    // it keeps the starter grading "Guarded", not "Elevated" for an omitted cap.
    spec: {
      allowed_domains: tp.hosts,
      first_use_approval: "deny_with_review",
      min_confinement_class: "CC2",
      auto_stop_after_sec: 3600,
      eligible_grants: [],
    },
  },
  {
    id: "model-provider",
    label: C.MODEL_PROVIDER,
    hint: tp.modelProviderHint,
    // examples/policies/ci-claude-llm.json, auto_stop included: this is the one
    // template carrying a minted credential, so the source's idle cap matters
    // most here — without it a non-interactive run holds the key forever and
    // preflight grades it RiskHigh. Idle-stop only fires after a genuinely
    // silent hour (lifecycle TouchDebounce keeps active sessions alive).
    spec: {
      allowed_domains: tp.hosts,
      denied_domains: [],
      allow_all_egress: false,
      first_use_approval: "always_deny",
      allowed_methods: [],
      min_confinement_class: "CC1",
      auto_stop_after_sec: 3600,
      eligible_grants: tp.grants,
    },
  },
  {
    id: "registries",
    label: C.REGISTRIES,
    hint: C.REGISTRIES_HINT,
    // examples/policies/default.json's egress set. Its github_token grant is
    // deliberately not carried: any repo-covering github_token makes the run
    // brokered and unconditionally removes github.com and friends from egress
    // (docs/POLICIES.md "Brokered GitHub") — a surprise this template's name
    // does not promise. auto_stop_after_sec: 3600 is added here (the source has
    // no cap) for the same meter reason as minimal — see the array header.
    spec: {
      allowed_domains: [
        ...tp.registryHosts,
        "registry.npmjs.org",
        "registry.yarnpkg.com",
        "pypi.org",
        "files.pythonhosted.org",
        "proxy.golang.org",
        "sum.golang.org",
        "crates.io",
        "static.crates.io",
        "index.crates.io",
      ],
      denied_domains: [],
      first_use_approval: "deny_with_review",
      allowed_methods: [],
      min_confinement_class: "CC2",
      auto_stop_after_sec: 3600,
      eligible_grants: [],
    },
  },
  {
    id: "ci",
    label: C.CI,
    hint: C.CI_HINT,
    // examples/policies/ci.json verbatim. The 3600 stays: a non-interactive run
    // that is never reaped holds its minted credentials indefinitely, which the
    // product's own risk grade calls high (internal/composer/risk.go).
    spec: {
      allowed_domains: [],
      denied_domains: [],
      allow_all_egress: false,
      first_use_approval: "always_deny",
      allowed_methods: [],
      min_confinement_class: "CC1",
      eligible_grants: [],
      auto_stop_after_sec: 3600,
    },
  },
  {
    id: "allow-all",
    label: C.ALLOW_ALL,
    hint: C.ALLOW_ALL_HINT,
    // Authored here, not seeded: no shipped example sets allow_all_egress.
    // first_use_approval is inert under allow-all, so it is stated as the
    // honest always_deny rather than implying a review that never fires.
    spec: {
      allowed_domains: [],
      denied_domains: [],
      allow_all_egress: true,
      first_use_approval: "always_deny",
      min_confinement_class: "CC2",
      eligible_grants: [],
    },
  },
];
}

// The no-provider set: today's templates.
export const POLICY_TEMPLATES: readonly PolicyTemplate[] = policyTemplates();

// The starter spec Policies, the governance editor and a fresh Custom policy open with.
export function minimalSpec(providers?: readonly SetupModelProvider[]): RunPolicySpec {
  return policyTemplates(providers)[0].spec;
}

export function templateText(t: PolicyTemplate, format: PolicySourceFormat = "yaml"): string {
  return specToSource(t.spec, format);
}

/* ---------- helper rail ---------- */

// Fields the RUN instance does not document: the Workspace card owns mounts
// there (and a member's are clamped away anyway). The help entry still exists
// above — the gate is here, at render, so the parity guard keeps its full key
// set. /policies is a stored policy: one of the two documented mount-authoring
// surfaces (types/policy.go's WorkspaceMounts doc comment).
const HIDDEN_ON_RUN: readonly (keyof RunPolicySpec)[] = ["workspace_mounts"];

// Re-exported for the screens that read them beside the panel.
export { egressSummary, lifecycleSummary, toolRulesSummary, type ChipTone } from "./policy-document/policy-facts";
export { parseSpec, type ParsedSpec };

// C5's one real trap, named: a parse that succeeds but whose
// min_confinement_class names no real barrier class silently sets no floor —
// the caller's own barrier control is what actually launches, and nothing
// said so. Returns the unparseable value (for the caller's hint) or null —
// null for an omitted field too (a policy that authors no floor on purpose is
// not a trap).
export function unparseableFloorClass(parsed: ParsedSpec): string | null {
  if (!parsed.ok) return null;
  const raw = (parsed.spec as { min_confinement_class?: unknown }).min_confinement_class;
  const known: ConfinementClass[] = ["CC1", "CC2", "CC3"];
  return typeof raw === "string" && raw.length > 0 && !known.includes(raw as ConfinementClass) ? raw : null;
}

/* ---------- the panel ---------- */

export interface PolicyPanelProps {
  instance: PolicyPanelInstance;
  /** The policy source text, as typed (the panel is fully controlled). */
  value: string;
  onChange: (next: string) => void;
  /** The format `value` is written in. Left out, the panel keeps it itself and opens in YAML. */
  format?: PolicySourceFormat;
  onFormatChange?: (format: PolicySourceFormat) => void;
  /** Renders "Done editing" on the editor, for a surface that also shows the read view. */
  onDone?: () => void;
  /**
   * Run instance: renders a Preflight button when provided. The SCREEN owns the
   * call (runs.preflightRun) and renders its errors/warnings/risk grade — the
   * panel never touches the API.
   */
  onPreflight?: () => void;
  /** Run instance: a preflight is in flight — the button parks until it lands. */
  preflightBusy?: boolean;
  // Extra caller-known reason Preflight can't run (e.g. the run screen's saved
  // lane with no policy picked — there is no body to dry-run).
  preflightDisabled?: boolean;
  /**
   * Run instance: the screen's interactive mode, forwarded to the SafetyMeter's
   * grade hint so a never-reap policy reads its "expected for an interactive
   * run" rationale instead of the high-risk one. /policies omits it (the
   * conservative default-false frame — a stored policy has no run mode).
   */
  interactive?: boolean;
  /** The Azure DevOps row's capability_ceiling (/setup/status scm_access);
   *  capabilities off it render locked. Undefined = unknown, nothing locked. */
  adoCeiling?: readonly string[];
  /** The editor's last save refusal; one naming an axis of a git_pat grant is
   *  shown on that field (policy-git-pat.tsx). */
  serverError?: string | null;
  /** /setup/status model_providers; the template chips follow them. Absent = the no-provider set. */
  modelProviders?: readonly SetupModelProvider[];
  /**
   * Run instance: the mode row (default / saved / custom). The screen owns the
   * policy list, the selection and the default-policy read; this is only where
   * they render and which card is lit.
   */
  policyMode?: {
    mode: PolicyMode;
    onModeChange: (mode: PolicyMode) => void;
    picker: React.ReactNode;
    defaultPolicy: DefaultPolicyView;
  };
  className?: string;
}

export function PolicyPanel({
  instance,
  value,
  onChange,
  format,
  onFormatChange,
  onDone,
  onPreflight,
  preflightBusy,
  preflightDisabled,
  interactive,
  adoCeiling,
  modelProviders,
  serverError,
  policyMode,
  className,
}: PolicyPanelProps) {
  const templates = React.useMemo(() => policyTemplates(modelProviders), [modelProviders]);
  const providerNames = templateProviders(modelProviders).names;
  const [ownFormat, setOwnFormat] = React.useState<PolicySourceFormat>("yaml");
  const fmt = format ?? ownFormat;
  const parsed = React.useMemo(() => parseSpec(value, fmt), [value, fmt]);
  // A structured edit the parser refused: said beside the controls, source untouched.
  const [refused, setRefused] = React.useState<PolicySourceError | null>(null);
  const egress = parsed.ok ? egressSummary(parsed.spec) : null;
  const mode: PolicyMode = policyMode?.mode ?? "custom";
  const specId = `policy-spec-${instance}`;
  const fields = (Object.keys(FIELD_HELP) as (keyof RunPolicySpec)[]).filter(
    (k) => instance === "policies" || !HIDDEN_ON_RUN.includes(k),
  );

  const setSource = (next: string) => {
    setRefused(null);
    onChange(next);
  };
  const write = (edit: PolicySourceEditResult) => (edit.ok ? setSource(edit.source) : setRefused(edit));
  // Every structured control hands back the spec it wants; only the keys it
  // changed are rewritten, so the rest of the text keeps its comments.
  const onSpecChange = (next: RunPolicySpec) => parsed.ok && write(applySpecChange(value, fmt, parsed.spec, next));

  return (
    <div className={cn("space-y-4", className)}>
      {policyMode && (
        <div className="grid gap-2 sm:grid-cols-3">
          <OptionCard
            selected={mode === "default"}
            onClick={() => policyMode.onModeChange("default")}
            title={C.DEFAULT_TITLE}
            hint={
              policyMode.defaultPolicy.profileName
                ? C.DEFAULT_HINT_PROFILE(policyMode.defaultPolicy.profileName)
                : C.DEFAULT_HINT
            }
          />
          <OptionCard
            selected={mode === "saved"}
            onClick={() => policyMode.onModeChange("saved")}
            title={C.SAVED_TITLE}
            hint={C.SAVED_HINT}
          />
          <OptionCard
            selected={mode === "custom"}
            onClick={() => policyMode.onModeChange("custom")}
            title={C.CUSTOM_TITLE}
            hint={C.CUSTOM_HINT}
          />
        </div>
      )}

      {mode === "default" ? (
        policyMode && <DefaultPolicyPreview view={policyMode.defaultPolicy} />
      ) : mode === "saved" ? (
        policyMode?.picker
      ) : (
        <PolicyEditor
          id={specId}
          source={value}
          format={fmt}
          parsed={parsed.ok ? { ok: true, value: parsed.spec } : parsed}
          onSourceChange={setSource}
          onFormatChange={(next, source) => {
            (onFormatChange ?? setOwnFormat)(next);
            setSource(source);
          }}
          onDone={onDone}
          rows={16}
          operationError={refused}
          validExtras={
            parsed.ok && (
              <>
                {egress && (
                  <Chip tone={egress.tone} className="gap-1">
                    <Globe className="size-3" />
                    {egress.label}
                  </Chip>
                )}
                <Chip tone="neutral" className="gap-1">
                  <Timer className="size-3" />
                  {lifecycleSummary(parsed.spec)}
                </Chip>
                {/* A hand-written source can omit the floor (or type it wrong) —
                    the cast through parseSpec is not a validation. */}
                {typeof parsed.spec.min_confinement_class === "string" &&
                  parsed.spec.min_confinement_class.length > 0 && (
                    <span title="Barrier floor — the run refuses to launch below it.">
                      <ConfinementChip value={parsed.spec.min_confinement_class} />
                    </span>
                  )}
              </>
            )
          }
          structured={
            <>
              <div>
                <SectionLabel className="mb-1.5">{C.START}</SectionLabel>
                <div className="flex flex-wrap gap-1.5">
                  {templates.map((t) => (
                    <Button
                      key={t.id}
                      type="button"
                      variant="outline"
                      size="sm"
                      title={t.hint}
                      onClick={() => setSource(templateText(t, fmt))}
                    >
                      {t.label}
                    </Button>
                  ))}
                </div>
                {providerNames.length > 0 && (
                  <p className="mt-1.5 text-meta text-muted-foreground">{C.FROM_PROVIDERS(providerNames)}</p>
                )}
              </div>

              {/* The four sections below edit a LIST or a closed set that is easy
                  to get wrong by hand (a duplicate tool, a typo'd effect, a wire
                  word the server refuses). Each needs a parsed spec to edit, so
                  they render only on one — the gate the Insert buttons take. */}
              {parsed.ok && <ToolRulesSection spec={parsed.spec} onSpecChange={onSpecChange} />}
              {parsed.ok && <PushRulesSection spec={parsed.spec} onSpecChange={onSpecChange} />}
              {parsed.ok && <ADOCapabilitiesSection spec={parsed.spec} ceiling={adoCeiling} onSpecChange={onSpecChange} />}
              {/* git_pat narrowing (repos, access, api, forge): one block per
                  stored-token grant, with the honesty lines packet M7 fixes. */}
              {parsed.ok && <GitPATSection spec={parsed.spec} serverError={serverError} onSpecChange={onSpecChange} />}

              {/* The safety meter grades the DOCUMENT as typed (debounced, advisory),
                  distinct from the run instance's Preflight of the RESOLVED run — so
                  on the run instance the meter's title says the difference. A parse
                  failure passes null, which dims it. */}
              <SafetyMeter
                spec={parsed.ok ? parsed.spec : null}
                interactive={interactive}
                note={instance === "run" ? "Preflight grades the resolved run" : undefined}
              />

              <div className="rounded-lg border border-border">
                <div className="flex items-center gap-2 border-b border-border px-3 py-2">
                  <SectionLabel>Fields</SectionLabel>
                  <span className="ml-auto text-meta text-muted-foreground">Full reference: docs/POLICIES.md</span>
                </div>
                <ul className="scroll-thin max-h-72 divide-y divide-border overflow-y-auto">
                  {fields.map((key) => {
                    const help = FIELD_HELP[key];
                    return (
                      <li key={key} className="px-3 py-2">
                        <div className="flex items-center gap-2">
                          {/* The doc anchor rides the field name's tooltip: the
                              console does not serve docs/, so a real href would be
                              a dead link. */}
                          <code className="font-mono text-xs text-foreground" title={`docs/POLICIES.md#${help.doc}`}>
                            {key}
                          </code>
                          <Button
                            type="button"
                            variant="ghost"
                            size="sm"
                            className="ml-auto h-6 px-2 text-meta"
                            aria-label={`Insert ${key}`}
                            disabled={!parsed.ok}
                            onClick={() => write(setSpecKey(value, fmt, key, help.snippet))}
                          >
                            <Plus className="size-3" />
                            Insert
                          </Button>
                        </div>
                        <p className="mt-0.5 text-meta leading-snug text-muted-foreground">{help.what}</p>
                        {/* Full muted token, never a diluted one: the diluted form
                            drops this 11px text below AA (theme-contrast.test.ts). */}
                        <p className="mt-0.5 text-meta italic leading-snug text-muted-foreground">{help.values}</p>
                      </li>
                    );
                  })}
                </ul>
              </div>
            </>
          }
        />
      )}

      {onPreflight && (
        <div className="flex flex-wrap items-center gap-2">
          <Button
            type="button"
            variant="outline"
            size="sm"
            onClick={onPreflight}
            disabled={preflightBusy || preflightDisabled || (mode === "custom" && !parsed.ok)}
          >
            <ShieldCheck className="size-4" />
            {RAIL_CHECK.BUTTON}
          </Button>
          <span className="text-meta text-muted-foreground">{RAIL_CHECK.HINT}</span>
        </div>
      )}
    </div>
  );
}
