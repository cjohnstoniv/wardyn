/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// PolicyPanel — ONE spec-JSON authoring surface, two instances:
//
//   instance="policies"  the admin policy editor's body (the name field stays
//                        on the screen; the operator gate stays on Save).
//   instance="run"       the new-run screen's policy card — the same JSON, plus
//                        an optional "reuse a saved policy" mode row and an
//                        optional Preflight button.
//
// `inline_policy` on POST /runs is the IDENTICAL Go struct a stored policy
// holds (types.RunPolicySpec), validated by the same validatePolicySpec — so
// one panel serves both, and the SERVER stays the source of truth for what is
// legal. The panel only parses (so a syntactically broken document never
// reaches the API) and derives read-only summaries from what parsed. The ONE
// live call it makes is the SafetyMeter's debounced grade of the current
// document (POST /policies/grade) — advisory, and grading the DOCUMENT, which
// is a different question than the screen's preflight of the RESOLVED run.
//
// The screen owns everything that isn't the JSON: the saved-policy list, the
// preflight call (runs.preflightRun) and its result rendering, the Workspace
// card's mounts/repos, and any post-parse union it does before submit.
//
// instance="policies" is consumed by policies.tsx's PolicyEditor; the run
// instance is still unconsumed pending the /runs/new swap (plan phase 5).
import * as React from "react";
import { CircleCheck, CircleX, Globe, Plus, ShieldCheck, Timer } from "lucide-react";
import type { RunPolicySpec } from "../../lib/types";
import { getErrorMessage } from "../../lib/format";
import { Button } from "../ui/button";
import { Textarea } from "../ui/textarea";
import { cn } from "../ui/utils";
import { Chip, ConfinementChip, SectionLabel } from "./primitives";
import { SafetyMeter } from "./safety-meter";
import { Field, OptionCard } from "./form-primitives";

export type PolicyPanelInstance = "run" | "policies";

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
// claude-subscription.template.json is deliberately NOT here: its `__comment`
// key and machine-specific mounts would 400 under DisallowUnknownFields /
// validatePolicySpec the moment anyone clicked it.
//
// auto_stop_after_sec: every template EXCEPT allow-all carries 3600. Under the
// safety meter's conservative (non-interactive) frame an omitted idle cap grades
// HIGH on its own (composer/risk.go: never-reap holds minted credentials forever),
// so without it the shipped starter would read "Elevated" — contradicting the
// owner's safest axis. allow-all DELIBERATELY omits it: its two highs (allow-all
// egress + never-reap) are what make "Weakest" reachable from a single template
// click, and a 3600 there would collapse it to one high. ci/model-provider
// inherit 3600 from their source examples; minimal/registries gain it here for
// the meter. Absent still coalesces to 0 = never reaped (internal/lifecycle).
export const POLICY_TEMPLATES: readonly PolicyTemplate[] = [
  {
    id: "minimal",
    label: "Minimal",
    hint: "The model provider and nothing else; unlisted hosts raise an approval.",
    // The shipped starter (policies.tsx's STARTER_SPEC, and the run wizard's
    // fresh Custom-policy prefill via new-run-screen's MINIMAL.spec) — a valid,
    // editable floor, not a blank document. The 3600 idle cap is the meter fix:
    // it keeps the starter grading "Guarded", not "Elevated" for an omitted cap.
    spec: {
      allowed_domains: ["api.anthropic.com"],
      first_use_approval: "deny_with_review",
      min_confinement_class: "CC2",
      auto_stop_after_sec: 3600,
      eligible_grants: [],
    },
  },
  {
    id: "model-provider",
    label: "Model provider only",
    hint: "One host, one proxy-injected API key. Nothing else is reachable.",
    // examples/policies/ci-claude-llm.json, auto_stop included: this is the one
    // template carrying a minted credential, so the source's idle cap matters
    // MOST here — without it a non-interactive run holds the key forever and
    // preflight grades it RiskHigh. Idle-stop only fires after a genuinely
    // silent hour (lifecycle TouchDebounce keeps active sessions alive).
    spec: {
      allowed_domains: ["api.anthropic.com"],
      denied_domains: [],
      allow_all_egress: false,
      first_use_approval: "always_deny",
      allowed_methods: [],
      min_confinement_class: "CC1",
      auto_stop_after_sec: 3600,
      eligible_grants: [
        {
          kind: "api_key",
          scope: {
            host: "api.anthropic.com",
            header: "x-api-key",
            format: "%s",
            secret_name: "anthropic-api-key",
          },
          ttl_seconds: 3600,
          requires_approval: false,
        },
      ],
    },
  },
  {
    id: "registries",
    label: "Package registries",
    hint: "Model providers plus the language package registries — the build-and-install set.",
    // examples/policies/default.json's egress set. Its github_token grant is
    // deliberately NOT carried: any repo-covering github_token makes the run
    // brokered and UNCONDITIONALLY removes github.com and friends from egress
    // (docs/POLICIES.md "Brokered GitHub") — a surprise this template's name
    // does not promise. auto_stop_after_sec: 3600 is added here (the source has
    // no cap) for the same meter reason as minimal — see the array header.
    spec: {
      allowed_domains: [
        "api.anthropic.com",
        "api.openai.com",
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
    label: "CI baseline",
    hint: "No egress, no grants, stopped after an idle hour — the unattended default.",
    // examples/policies/ci.json verbatim. The 3600 STAYS: a non-interactive run
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
    label: "Allow-all — observe first",
    hint: "Reaches almost any site (except a block-list); watch the audit log, then tighten.",
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

export function templateText(t: PolicyTemplate): string {
  return JSON.stringify(t.spec, null, 2);
}

/* ---------- helper rail ---------- */

export interface FieldHelp {
  /** One line: what the key does. */
  what: string;
  /** Legal values / shape, in authoring terms. */
  values: string;
  /** docs/POLICIES.md heading anchor for depth. */
  doc: string;
  /** The value an "Insert" click writes under this key. */
  snippet: unknown;
}

// `satisfies Record<keyof RunPolicySpec, FieldHelp>` is the parity guard: add a
// field to RunPolicySpec (which mirrors types.RunPolicySpec) without documenting
// it here and this file stops compiling. No markdown-parsing test needed.
//
// Note what is NOT snippetable: llm_inspection.workspace_secret_values. It is
// refused on every policy write (validatePolicySpec) — dispatch resolves
// workspace_secret_names to values in memory, for the proxy sidecar only — so
// the llm_inspection snippet authors NAMES and the help text says so.
export const FIELD_HELP = {
  allowed_domains: {
    what: "The egress allowlist — the hosts the sandbox may reach.",
    values:
      "Exact hosts or \"*.\"-prefixed wildcards. Empty under default-deny means the sandbox reaches nothing.",
    doc: "top-level",
    snippet: ["api.anthropic.com"],
  },
  denied_domains: {
    what: "Always wins over allowed_domains, in both egress modes.",
    values: "Same entry shapes as allowed_domains. A deny beats allow-all too.",
    doc: "top-level",
    snippet: ["example.com"],
  },
  first_use_approval: {
    what: "What happens when the sandbox reaches a host that is not listed.",
    // The three mode bodies are docs/design/ui-batch2-mock.md's canon strings
    // (D33 for deny_with_review, D8 for wait_for_review), verbatim. They used to
    // live on the run wizard's own Confined card and NetworkDialog's
    // UNLISTED_RULES; this panel replaced both, so it inherits the copy — a hold
    // is BOUNDED (first_use_hold_seconds, 30s default), which the old
    // "until you approve or deny it" wording promised away.
    values:
      "always_deny — refused outright, no prompt, no wait. " +
      "deny_with_review — Default-deny. A new host is refused and raised for your review — approve it once and a retry gets through. " +
      "wait_for_review — The connection waits, live, for the standard 30-second window. Decide in time and it goes through; miss it and it's refused — the approval itself stays open for you to decide. " +
      "Inert under allow_all_egress.",
    doc: "first_use_approval-modes",
    snippet: "deny_with_review",
  },
  allowed_methods: {
    what: "Optional HTTP method restriction.",
    values: "e.g. [\"GET\", \"POST\"]. Empty or omitted = every method.",
    doc: "top-level",
    snippet: ["GET", "POST"],
  },
  min_confinement_class: {
    what: "The barrier floor — the run refuses to launch below it.",
    values: "CC1 (Fence) | CC2 (Wall) | CC3 (Vault). Required.",
    doc: "top-level",
    snippet: "CC2",
  },
  eligible_grants: {
    what: "The ceiling of credential scopes this run may request — eligibility, not issuance.",
    values:
      "kind: github_token | cloud_sts | api_key | git_pat | ssh_key, each with its own scope, plus ttl_seconds (1h max) and requires_approval.",
    doc: "eligible_grants--grantspec",
    snippet: [
      {
        kind: "api_key",
        scope: {
          host: "api.anthropic.com",
          header: "x-api-key",
          format: "%s",
          secret_name: "anthropic-api-key",
        },
        ttl_seconds: 3600,
        requires_approval: false,
      },
    ],
  },
  auto_stop_after_sec: {
    what: "Idle auto-stop, in seconds of wall-clock idleness (an attach or an egress call resets it).",
    values:
      "> 0 stops after that many idle seconds, plus a 30s debounce. 0 or ABSENT = never reaped. -1 = never reaped, stated explicitly — identical behavior to leaving it out, written down as intent (what an interactive run means).",
    doc: "top-level",
    snippet: 3600,
  },
  workspace_mounts: {
    what: "Operator-authored host bind mounts. Never agent-chosen.",
    values:
      "source (absolute host path) + target (under /home/agent, /work or /workspace). read_only defaults to TRUE when omitted — read-write needs an explicit false.",
    doc: "workspace_mounts--workspacemount",
    snippet: [{ source: "/srv/data", target: "/work/data" }],
  },
  workspace_repos: {
    what: "Extra git repos cloned into the run — the clone counterpart of workspace_mounts.",
    values:
      "repo (slug or URL), optional target, optional ref (branch/tag/SHA; unset clones the remote's default branch).",
    doc: "workspace_repos--workspacerepo",
    snippet: [{ repo: "owner/repo" }],
  },
  allow_all_egress: {
    what: "Switches egress to deny-list only: any non-denied PUBLIC host is allowed.",
    values:
      "true | false (default). The private-IP/SSRF guard is unaffected, and credential injection still needs an exact allowed_domains entry — allow-all never widens where a secret may go.",
    doc: "top-level",
    snippet: true,
  },
  llm_inspection: {
    what: "Outbound content inspection on the brokered LLM routes — a guardrail and visibility layer, not exfiltration prevention.",
    values:
      "mode: off (default) | alert | block, with at least one detector when not off. Author workspace_secret_names — workspace_secret_values is REFUSED on every policy write.",
    doc: "llm_inspection--llminspectionspec",
    snippet: { mode: "alert", detect_secrets: true, workspace_secret_names: [] },
  },
  resources: {
    what: "Sandbox CPU / memory / PID / disk caps.",
    values:
      "cpu_millis (2000 = 2 vCPU), memory_mib, pids_limit, disk_mib. A zero or omitted field takes the platform default — every run is capped either way.",
    doc: "resources--resourcelimits",
    snippet: { cpu_millis: 2000, memory_mib: 4096, pids_limit: 512 },
  },
  ui_apps: {
    what: "In-sandbox loopback HTTP apps the UI gateway may relay to a browser.",
    values:
      "name (lower-case slug, also the /usr/local/bin/wardyn-ui-<name> launcher), port (inside the sandbox, on 127.0.0.1), optional path. Max 8. Declaring one grants nothing: the gateway is off unless WARDYN_UI_SANDBOX_LISTEN is set, and every session still needs a single-use ticket.",
    doc: "ui_apps--uiapp",
    snippet: [{ name: "editor", port: 8080 }],
  },
} satisfies Record<keyof RunPolicySpec, FieldHelp>;

// Fields the RUN instance does not document: the Workspace card owns mounts
// there (and a member's are clamped away anyway). The help entry still exists
// above — the gate is here, at render, so the parity guard keeps its full key
// set. /policies is a stored policy: one of the two documented mount-authoring
// surfaces (types/policy.go's WorkspaceMounts doc comment).
const HIDDEN_ON_RUN: readonly (keyof RunPolicySpec)[] = ["workspace_mounts"];

/* ---------- live derivations ---------- */

export type ChipTone = NonNullable<React.ComponentProps<typeof Chip>["tone"]>;

// 4b dedup: policies.tsx's table rows import these two straight from here
// instead of keeping their own copies.

// Compact, honest egress summary. allow_all_egress is ALWAYS the block-list
// phrasing (never "unrestricted") — see wardyn/copy.ts.
export function egressSummary(spec: RunPolicySpec): { label: string; tone: ChipTone } {
  if (spec.allow_all_egress) {
    return { label: "Allow-all egress (block-list only)", tone: "info" };
  }
  const n = spec.allowed_domains?.length ?? 0;
  const denied = spec.denied_domains?.length ?? 0;
  if (n === 0) {
    return { label: denied > 0 ? `No egress, ${denied} denied` : "No egress", tone: "neutral" };
  }
  return {
    label: `${n} domain${n === 1 ? "" : "s"} allowed${denied > 0 ? `, ${denied} denied` : ""}`,
    tone: "info",
  };
}

// Honest lifecycle summary — mirrors the reaper's ACTUAL semantics
// (internal/lifecycle/lifecycle.go): auto_stop_after_sec <= 0 or unset means the
// run is exempt from idle auto-stop, not "30 minutes by default".
export function lifecycleSummary(spec: RunPolicySpec): string {
  const s = spec.auto_stop_after_sec;
  if (typeof s === "number" && s > 0) return `Auto-stop: ${Math.max(1, Math.round(s / 60))} min idle`;
  return "Runs until stopped";
}

export type ParsedSpec =
  | { ok: true; spec: RunPolicySpec }
  | { ok: false; message: string };

// Light client-side parse only: the server's validatePolicySpec decides what is
// LEGAL (and says so with a field path). This just catches a broken document —
// and a top-level array/scalar, which would otherwise spread into nonsense on
// the next snippet insert.
export function parseSpec(text: string): ParsedSpec {
  let value: unknown;
  try {
    value = JSON.parse(text) as unknown;
  } catch (e) {
    return { ok: false, message: getErrorMessage(e) };
  }
  if (value === null || typeof value !== "object" || Array.isArray(value)) {
    return { ok: false, message: "the spec must be a JSON object." };
  }
  return { ok: true, spec: value as RunPolicySpec };
}

/* ---------- the panel ---------- */

export interface PolicyPanelProps {
  instance: PolicyPanelInstance;
  /** The spec JSON text (the panel is fully controlled). */
  value: string;
  onChange: (next: string) => void;
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
  /**
   * Run instance: the "Reuse a saved policy" half of the mode row. The screen
   * owns the policy list and the selection; this is only where it renders and
   * which half of the row is lit.
   */
  savedPolicy?: {
    active: boolean;
    onActiveChange: (active: boolean) => void;
    picker: React.ReactNode;
  };
  className?: string;
}

export function PolicyPanel({
  instance,
  value,
  onChange,
  onPreflight,
  preflightBusy,
  preflightDisabled,
  interactive,
  savedPolicy,
  className,
}: PolicyPanelProps) {
  const parsed = parseSpec(value);
  const egress = parsed.ok ? egressSummary(parsed.spec) : null;
  const usingSaved = savedPolicy?.active ?? false;
  const specId = `policy-spec-${instance}`;
  const fields = (Object.keys(FIELD_HELP) as (keyof RunPolicySpec)[]).filter(
    (k) => instance === "policies" || !HIDDEN_ON_RUN.includes(k),
  );

  return (
    <div className={cn("space-y-4", className)}>
      {savedPolicy && (
        <div className="grid gap-2 sm:grid-cols-2">
          <OptionCard
            selected={usingSaved}
            onClick={() => savedPolicy.onActiveChange(true)}
            title="Reuse a saved policy"
            hint="One your operators already wrote and named."
          />
          <OptionCard
            selected={!usingSaved}
            onClick={() => savedPolicy.onActiveChange(false)}
            title="Custom policy"
            hint="Start from a template and edit the spec for this run."
          />
        </div>
      )}

      {usingSaved ? (
        savedPolicy?.picker
      ) : (
        <>
          <div>
            <SectionLabel className="mb-1.5">Start from a template</SectionLabel>
            <div className="flex flex-wrap gap-1.5">
              {POLICY_TEMPLATES.map((t) => (
                <Button
                  key={t.id}
                  type="button"
                  variant="outline"
                  size="sm"
                  title={t.hint}
                  onClick={() => onChange(templateText(t))}
                >
                  {t.label}
                </Button>
              ))}
            </div>
          </div>

          <Field label="Spec (JSON)" htmlFor={specId} required>
            <Textarea
              id={specId}
              value={value}
              onChange={(e) => onChange(e.target.value)}
              rows={16}
              spellCheck={false}
              className="font-mono text-xs"
              required
            />
          </Field>

          <div role="status" className="flex flex-wrap items-center gap-1.5">
            {parsed.ok ? (
              <>
                <Chip tone="success" className="gap-1">
                  <CircleCheck className="size-3" />
                  Valid JSON
                </Chip>
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
                {/* Hand-written JSON can omit the floor (or type it wrong) —
                    the cast through parseSpec is not a validation. */}
                {typeof parsed.spec.min_confinement_class === "string" &&
                  parsed.spec.min_confinement_class.length > 0 && (
                    <span title="Barrier floor — the run refuses to launch below it.">
                      <ConfinementChip value={parsed.spec.min_confinement_class} />
                    </span>
                  )}
              </>
            ) : (
              <Chip tone="danger" className="gap-1 whitespace-normal">
                <CircleX className="size-3 shrink-0" />
                Invalid JSON — {parsed.message}
              </Chip>
            )}
          </div>

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
              <span className="ml-auto text-[0.6875rem] text-muted-foreground">
                Full reference: docs/POLICIES.md
              </span>
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
                      <code
                        className="font-mono text-xs text-foreground"
                        title={`docs/POLICIES.md#${help.doc}`}
                      >
                        {key}
                      </code>
                      <Button
                        type="button"
                        variant="ghost"
                        size="sm"
                        className="ml-auto h-6 px-2 text-[0.6875rem]"
                        aria-label={`Insert ${key}`}
                        disabled={!parsed.ok}
                        title={parsed.ok ? undefined : "Fix the JSON above first."}
                        onClick={() =>
                          parsed.ok &&
                          onChange(JSON.stringify({ ...parsed.spec, [key]: help.snippet }, null, 2))
                        }
                      >
                        <Plus className="size-3" />
                        Insert
                      </Button>
                    </div>
                    <p className="mt-0.5 text-[0.6875rem] leading-snug text-muted-foreground">
                      {help.what}
                    </p>
                    {/* Full muted token, never a diluted one: the diluted form
                        drops this 11px text below AA (theme-contrast.test.ts). */}
                    <p className="mt-0.5 text-[0.6875rem] italic leading-snug text-muted-foreground">
                      {help.values}
                    </p>
                  </li>
                );
              })}
            </ul>
          </div>
        </>
      )}

      {onPreflight && (
        <div className="flex flex-wrap items-center gap-2">
          <Button
            type="button"
            variant="outline"
            size="sm"
            onClick={onPreflight}
            disabled={preflightBusy || preflightDisabled || (!usingSaved && !parsed.ok)}
          >
            <ShieldCheck className="size-4" />
            Preflight
          </Button>
          <span className="text-[0.6875rem] text-muted-foreground">
            Checks the spec server-side and shows what would be clamped — before you launch.
          </span>
        </div>
      )}
    </div>
  );
}
