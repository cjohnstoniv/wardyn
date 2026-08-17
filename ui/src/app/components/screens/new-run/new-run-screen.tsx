/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// New run — ONE page, two columns, with a live rail that answers "what can this
// run actually do?" while you build it.
//
// Ported from the Figma Make canon (src/screens/NewRunScreen.tsx in the "Wardyn
// Simplified" file). It replaces a five-step modal wizard whose Review screen
// was the first place the consequences of your choices appeared — by which
// point you had made all of them blind.
//
// It is a RE-LAYOUT, not a re-model. WizardState already carries every field
// this screen edits, and buildSpec()/impliedEgressHosts() are reused verbatim,
// so the launch payload and the policy it produces are exactly what the wizard
// produced. That is deliberate: the governance contract is the tested part, and
// this change is about when the operator SEES it, not what it is.
import * as React from "react";
import { useNavigate } from "react-router-dom";
import { ArrowLeft, Loader2, Plus, TriangleAlert } from "lucide-react";
import { toast } from "sonner";
import type { AgentRun, ConfinementClass, RunPolicySpec, Workspace } from "../../../lib/types";
import type { WizardAgent } from "./wizard-types";
import { runs as runsApi } from "../../../lib/api/runs";
import { policies as policiesApi } from "../../../lib/api/policies";
import { health as healthApi } from "../../../lib/api/health";
import { setup as setupApi } from "../../../lib/api/setup";
import { hasLlmPath } from "../../../lib/readiness";
import { useWorkspaceList } from "../../../lib/use-workspace-list";
import { getErrorMessage } from "../../../lib/format";
import { statusWord } from "../../../lib/workspace-status";
import { Button } from "../../ui/button";
import { Input } from "../../ui/input";
import { Textarea } from "../../ui/textarea";
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "../../ui/select";
import { Field } from "../../wardyn/form-primitives";
import { Mono } from "../../wardyn/code-block";
import { Chip } from "../../wardyn/primitives";
import { CC_META } from "../../wardyn/cc-meta";
import { getDefaultCc, resolveDefaultCc } from "../../wardyn/default-confinement";
import { cn } from "../../ui/utils";
import { AddWorkspaceDialog } from "../add-workspace-dialog";
import { NetworkDialog, UNLISTED_RULES, type NetworkSelection } from "./network-dialog";
import { buildSpec, impliedEgressHosts } from "./wizard-spec";
import { agentLabel, initialWizardState, PRESET_DOMAINS, primaryWorkspaceId, type WizardState } from "./wizard-types";
import { surfaceRunWarnings } from "./run-warnings";

// The three Network presets. "Registries" is the whole PRESET_DOMAINS list —
// the same set the dialog groups — so the card and the dialog can never
// disagree about what "common package registries" means.
type NetworkPreset = "none" | "model" | "registries" | "everything" | "custom";

const ORDERED_CLASSES: ConfinementClass[] = ["CC1", "CC2", "CC3"];

// A run is EITHER recorded (allow everything, log everything, synthesise the
// policy afterwards) or confined. Record is Wardyn's moat, so it leads.
type ConfinementChoice = "record" | "confined" | "saved";

function SectionCard({ title, children }: { title: string; children: React.ReactNode }) {
  return (
    <section className="rounded-xl border border-border bg-surface-1">
      <div className="border-b border-border px-4 py-2.5">
        <h3 className="text-sm font-medium text-foreground">{title}</h3>
      </div>
      <div className="p-4">{children}</div>
    </section>
  );
}

function Seg({
  options,
  value,
  onChange,
  label,
}: {
  options: { id: string; label: string; disabled?: boolean }[];
  value: string;
  onChange: (id: string) => void;
  label: string;
}) {
  return (
    <div role="radiogroup" aria-label={label} className="flex flex-wrap gap-2">
      {options.map((o) => (
        <button
          key={o.id}
          type="button"
          role="radio"
          aria-checked={value === o.id}
          disabled={o.disabled}
          onClick={() => onChange(o.id)}
          className={cn(
            "rounded-lg border px-3 py-1.5 text-[0.8125rem] font-medium transition-colors",
            value === o.id
              ? "border-primary bg-primary/10 text-primary"
              : "border-border text-foreground hover:border-border-strong",
            o.disabled && "cursor-not-allowed opacity-40 hover:border-border",
          )}
        >
          {o.label}
        </button>
      ))}
    </div>
  );
}

function RadioCard({
  on,
  onSelect,
  title,
  body,
  children,
}: {
  on: boolean;
  onSelect: () => void;
  title: string;
  body: string;
  children?: React.ReactNode;
}) {
  return (
    <div className={cn("rounded-lg border transition-colors", on ? "border-primary bg-primary/5" : "border-border")}>
      <button
        type="button"
        role="radio"
        aria-checked={on}
        onClick={onSelect}
        className="flex w-full items-start gap-2.5 p-3 text-left"
      >
        <span
          aria-hidden="true"
          className={cn(
            "mt-0.5 grid size-4 shrink-0 place-items-center rounded-full border",
            on ? "border-primary" : "border-border-strong",
          )}
        >
          {on && <span className="size-2 rounded-full bg-primary" />}
        </span>
        <span>
          <span className="block text-sm font-medium text-foreground">{title}</span>
          <span className="mt-0.5 block text-[0.6875rem] leading-snug text-muted-foreground">{body}</span>
        </span>
      </button>
      {on && children && <div className="border-t border-border px-3 py-3">{children}</div>}
    </div>
  );
}

function RailSection({ title, children }: { title: string; children: React.ReactNode }) {
  return (
    <div className="border-b border-border pb-3 last:border-0">
      <p className="mb-1.5 text-[0.625rem] font-medium tracking-wide text-muted-foreground uppercase">{title}</p>
      {children}
    </div>
  );
}

export function NewRunScreen() {
  const navigate = useNavigate();
  const { workspaces, reload: reloadWorkspaces } = useWorkspaceList();
  // Seed with the PERSISTED default (Settings' promise); the health probe
  // below re-resolves it against what this host actually enforces. The old
  // resolveDefaultCc(…, ["CC1"]) hardcoded the availability list, so a saved
  // Wall/Vault default could never win — Settings' promise was untrue here.
  const [state, setState] = React.useState<WizardState>(() =>
    initialWizardState(getDefaultCc() ?? "CC1"),
  );
  const [confinement, setConfinement] = React.useState<ConfinementChoice>("confined");
  const [netOpen, setNetOpen] = React.useState(false);
  const [addWsOpen, setAddWsOpen] = React.useState(false);
  const [availableClasses, setAvailableClasses] = React.useState<ConfinementClass[] | null>(null);
  const [launching, setLaunching] = React.useState(false);
  const [error, setError] = React.useState<string | null>(null);
  const [savedPolicies, setSavedPolicies] = React.useState<
    { id: string; name: string; spec: RunPolicySpec }[]
  >([]);
  // Whether the barrier probe has SETTLED (null availableClasses after settle
  // means the check failed — unknown, never "confirmed absent").
  const [probeSettled, setProbeSettled] = React.useState(false);
  // null = not answered yet. An agent run with no model path launches and then
  // fails its first model call, so the rail must say so BEFORE launch rather
  // than promising credentials that cannot be minted.
  const [llmReady, setLlmReady] = React.useState<boolean | null>(null);
  // Existing run titles, offered as a native <datalist> under the Title input.
  // Grouping is by EXACT string, so without this the operator has to retype a
  // title character-perfect for a run to ever join its family — the feature
  // would look broken while working precisely as designed.
  const [knownTitles, setKnownTitles] = React.useState<string[]>([]);

  React.useEffect(() => {
    runsApi
      .listRuns()
      .then((rs) =>
        setKnownTitles(
          [...new Set(rs.map((r) => (r.title ?? "").trim()).filter(Boolean))].sort(),
        ),
      )
      .catch(() => {
        /* the datalist simply offers nothing — never blocks a launch */
      });
  }, []);

  React.useEffect(() => {
    setupApi
      .getSetupStatus()
      .then((st) => setLlmReady(hasLlmPath(st)))
      .catch(() => {
        /* unknown stays unknown — never claim a missing model path on a blip */
      });
  }, []);

  React.useEffect(() => {
    policiesApi
      .listPolicies()
      .then((ps) => setSavedPolicies(ps.map((p) => ({ id: p.id, name: p.name, spec: p.spec }))))
      .catch(() => {
        /* the Saved-policy lane simply offers nothing — never blocks a launch */
      });
  }, []);

  // useWorkspaceList does NOT fetch on mount — every caller loads it itself
  // (setup-screen.tsx does the same in its own mount effect). Without this the
  // Workspace select only ever offers "Ephemeral scratch", so a workspace
  // onboarded anywhere else — Getting started, the Workspaces screen — could
  // not be attached to a run at all; the only way into the list was to add one
  // from this page in this session.
  React.useEffect(() => {
    reloadWorkspaces();
    // run once on mount — reload is stable (useCallback([]))
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, []);

  const patch = React.useCallback(
    (p: Partial<WizardState>) =>
      setState((s) => {
        // D11/claim6 funnel, restored from the retired wizard: ANY edit to the
        // inline envelope detaches a picked saved policy. Without this the
        // stale id survives every visible signal of detachment, and launch
        // ships the STORED spec — its workspace mounts included — instead of
        // the inline one on screen.
        const envelope: (keyof WizardState)[] = [
          "allowAllEgress",
          "allowedDomains",
          "deniedDomains",
          "firstUseApproval",
          "confinementClass",
        ];
        const detach =
          s.selectedPolicyId && !("selectedPolicyId" in p) && envelope.some((k) => k in p);
        return { ...s, ...p, ...(detach ? { selectedPolicyId: undefined } : {}) };
      }),
    [],
  );

  // Which barriers this host can actually build. Empty means UNKNOWN, not
  // confirmed-absent (healthApi.health swallows a failure into {}), so an empty
  // result retries once before settling — the wizard learned this the hard way.
  React.useEffect(() => {
    let alive = true;
    let retried = false;
    const probe = () =>
      healthApi.health().then((h) => {
        if (!alive) return;
        const classes = ((h.confinement_classes ?? []) as ConfinementClass[]).filter(Boolean);
        if (classes.length === 0 && !retried) {
          retried = true;
          probe();
          return;
        }
        // Empty after the retry stays null: unknown, never "confirmed absent".
        // "CC1-only" was a positive claim manufactured from an absence, and it
        // disabled Wall/Vault with copy asserting they aren't installed.
        setAvailableClasses(classes.length ? classes : null);
        setProbeSettled(true);
        if (classes.length) {
          // Re-resolve the persisted default against real availability —
          // this, not the seed above, is where Settings' promise comes true.
          setState((s) => ({
            ...s,
            confinementClass: resolveDefaultCc(getDefaultCc(), classes),
          }));
        }
      });
    probe();
    return () => {
      alive = false;
    };
  }, []);

  const isAgent = state.runType === "agent";
  const isRecord = confinement === "record";
  // A shell command is unattended by definition — buildSpec forces batch for
  // one, so the Run mode segment is hidden rather than offering a combination
  // that would silently drop the command.
  const isInteractive = isAgent && state.mode === "interactive";
  // Shared display name (wizard-types.agentLabel) — a local re-hardcode here
  // is exactly the drift that helper's doc says it exists to prevent.
  const agentName = agentLabel(state.agent);
  const selectedPolicy =
    confinement === "saved" && state.selectedPolicyId
      ? savedPolicies.find((p) => p.id === state.selectedPolicyId)
      : undefined;

  // The screen's ONE validation rule. Deliberately a local derivation rather
  // than a shared validator: it answers "can this button be pressed", which is
  // this screen's question, and a second general-purpose answer living
  // elsewhere is what drifts out of sync with the form it describes.
  const needsTask = !isAgent || state.mode === "batch";
  const problem = !state.title.trim()
    ? "Give this run a title."
    : needsTask && !state.task.trim()
      ? isAgent
        ? "A batch run needs a task to perform."
        : "Enter a command to run."
      : null;

  // Which preset the current host list corresponds to — derived, never stored,
  // so an edit in the dialog is reflected here instead of silently disagreeing.
  const preset: NetworkPreset = state.allowAllEgress
    ? "everything"
    : state.allowedDomains.length === 0
      ? "none"
      : state.allowedDomains.length === 1 && state.allowedDomains[0] === "api.anthropic.com"
        ? "model"
        : state.allowedDomains.length === PRESET_DOMAINS.length &&
          PRESET_DOMAINS.every((d) => state.allowedDomains.includes(d))
        ? "registries"
        : "custom";

  const setPreset = (p: NetworkPreset) => {
    if (p === "none") patch({ allowAllEgress: false, allowedDomains: [] });
    else if (p === "model") patch({ allowAllEgress: false, allowedDomains: ["api.anthropic.com"] });
    else if (p === "registries") patch({ allowAllEgress: false, allowedDomains: [...PRESET_DOMAINS] });
    else if (p === "everything") patch({ allowAllEgress: true });
  };

  const implied = React.useMemo(
    () =>
      impliedEgressHosts(state, workspaces).filter((h) => !state.allowedDomains.includes(h.host)),
    [state, workspaces],
  );
  const ruleTitle = UNLISTED_RULES.find((r) => r.id === state.firstUseApproval)?.title ?? "";
  const cc = state.confinementClass;
  const hostCount = state.allowedDomains.length + implied.length;

  const netValue: NetworkSelection = {
    allowAllEgress: state.allowAllEgress,
    allowedDomains: state.allowedDomains,
    deniedDomains: state.deniedDomains,
    firstUseApproval: state.firstUseApproval,
  };

  const launch = async () => {
    setError(null);
    setLaunching(true);
    try {
      const { run, inline_policy } = buildSpec(state, workspaces);
      // The RADIO is the discriminator, belt to the patch-funnel's braces: a
      // policy id that somehow survives a switch back to Confined still must
      // not launch by reference. And the workspace_id override must never
      // OVERWRITE buildSpec's deliberate ephemeral-workspace fallback with
      // undefined — that silently launched a workspace-less run.
      const usePolicy = confinement === "saved" && state.selectedPolicyId;
      const created: AgentRun = await runsApi.createRun(
        usePolicy
          ? {
              ...run,
              policy_id: state.selectedPolicyId,
              workspace_id: primaryWorkspaceId(state.workspaces, workspaces) ?? run.workspace_id,
            }
          : { ...run, inline_policy },
      );
      // (A best-effort "save this as a policy" write used to live here, gated on
      // state.saveAsProfile — a flag no control on this screen has ever set. It
      // was unreachable from the moment the five-step wizard was replaced.)
      surfaceRunWarnings(created);
      navigate(`/runs/${encodeURIComponent(created.id)}`);
    } catch (e) {
      setError(getErrorMessage(e) || "Failed to launch run.");
      setLaunching(false);
    }
  };

  return (
    <div className="mx-auto w-full max-w-[1200px] px-6 py-6">
      <div className="mb-6 flex items-center gap-3">
        <Button variant="ghost" size="sm" onClick={() => navigate("/runs")}>
          <ArrowLeft className="size-4" /> Runs
        </Button>
        <h1 className="text-xl font-semibold text-foreground">New run</h1>
      </div>

      <div className="grid gap-4 lg:grid-cols-[minmax(0,1fr)_20rem]">
        {/* ── Left: the form ─────────────────────────────────────── */}
        <div className="min-w-0 space-y-4">
          {/* Identity first: the one thing that makes this run findable a week
              from now, and the only field on the page that is always required. */}
          <SectionCard title="This run">
            <div className="space-y-4">
              <Field
                label="Title"
                htmlFor="nr-title"
                required
                hint="Runs that share a title are grouped together on the Runs board."
              >
                <Input
                  id="nr-title"
                  required
                  maxLength={200}
                  // Native datalist: existing titles are offered as you type, so
                  // joining a family is a pick rather than an exact retype. No
                  // combobox library, and typing something new still just works.
                  list="nr-known-titles"
                  placeholder="Refactor the payments module"
                  value={state.title}
                  onChange={(e) => patch({ title: e.target.value })}
                />
                <datalist id="nr-known-titles">
                  {knownTitles.map((t) => (
                    <option key={t} value={t} />
                  ))}
                </datalist>
              </Field>

              <Field
                label="Description"
                htmlFor="nr-description"
                hint="Optional. Why this run exists — for whoever reads it later."
              >
                <Textarea
                  id="nr-description"
                  rows={2}
                  maxLength={2000}
                  placeholder="Ticket 4412 — the refund path double-charges on retry."
                  value={state.description}
                  onChange={(e) => patch({ description: e.target.value })}
                />
              </Field>
            </div>
          </SectionCard>

          <SectionCard title="What to run">
            <div className="space-y-4">
              {/* The choice that proves a run needn't involve AI at all. */}
              <Seg
                label="Run type"
                value={state.runType}
                onChange={(id) => patch({ runType: id as WizardState["runType"] })}
                options={[
                  { id: "agent", label: "Agent task" },
                  { id: "command", label: "Shell command" },
                ]}
              />

              {isAgent && (
                <Field label="Agent" htmlFor="nr-agent">
                  <Select value={state.agent} onValueChange={(v) => patch({ agent: v as WizardAgent })}>
                    <SelectTrigger id="nr-agent">
                      <SelectValue />
                    </SelectTrigger>
                    <SelectContent>
                      <SelectItem value="claude-code">Claude Code</SelectItem>
                      <SelectItem value="codex-cli">Codex CLI</SelectItem>
                    </SelectContent>
                  </Select>
                </Field>
              )}

              {/* The mode comes BEFORE the field it selects: an interactive run
                  is configured by a startup choice, a batch run by a prompt, and
                  a shell command by the command — never all three at once.
                  Hidden for Shell command, which is unattended by definition. */}
              {isAgent && (
                <Seg
                  label="Run mode"
                  value={state.mode}
                  onChange={(id) => patch({ mode: id as WizardState["mode"] })}
                  options={[
                    { id: "batch", label: "Batch — run it unattended" },
                    { id: "interactive", label: "Interactive — I drive the terminal" },
                  ]}
                />
              )}

              {isInteractive ? (
                // No task: the server ignores one for an interactive run, so
                // asking for a prompt nothing will read is a lie the old screen
                // told. What an interactive run actually configures is what
                // greets you when you attach.
                <Field
                  label="Start with"
                  hint="The workspace is prepared before you land in it. Same barrier, same recording either way."
                >
                  <Seg
                    label="Start with"
                    value={state.interactiveStart}
                    onChange={(id) => patch({ interactiveStart: id as WizardState["interactiveStart"] })}
                    options={[
                      { id: "agent", label: `${agentName} — launch it in the workspace` },
                      { id: "shell", label: "Terminal — a shell in the workspace dir" },
                    ]}
                  />
                </Field>
              ) : (
                <Field
                  label={isAgent ? "Task" : "Command"}
                  htmlFor="nr-task"
                  required
                  hint={
                    isAgent
                      ? "Described in plain English. The agent decides how to do it."
                      : "Run verbatim in the sandbox. No agent, no model — the same governance either way."
                  }
                >
                  <Textarea
                    id="nr-task"
                    rows={4}
                    required
                    className={isAgent ? undefined : "font-mono"}
                    placeholder={isAgent ? "Fix the flaky test in payments/refund_test.go" : "make test"}
                    value={state.task}
                    onChange={(e) => patch({ task: e.target.value })}
                  />
                </Field>
              )}
            </div>
          </SectionCard>

          <SectionCard title="Workspace">
            <Select
              value={state.workspaces[0]?.workspaceId ?? "__none__"}
              onValueChange={(v) =>
                patch({ workspaces: v === "__none__" ? [] : [{ workspaceId: v, enabledOptional: [] }] })
              }
            >
              <SelectTrigger>
                <SelectValue placeholder="Ephemeral scratch — no repo" />
              </SelectTrigger>
              <SelectContent>
                <SelectItem value="__none__">Ephemeral scratch — no repo</SelectItem>
                {workspaces.map((w: Workspace) => (
                  <SelectItem key={w.id} value={w.id}>
                    {w.name}
                    {statusWord(w.status) === "Import failed" && (
                      <span className="text-danger"> — import failed</span>
                    )}
                  </SelectItem>
                ))}
              </SelectContent>
            </Select>
            <Button variant="ghost" size="sm" className="mt-2 px-1" onClick={() => setAddWsOpen(true)}>
              <Plus className="size-4" /> Add workspace
            </Button>
          </SectionCard>

          <SectionCard title="Confinement">
            <div className="space-y-2">
              <RadioCard
                on={confinement === "record"}
                onSelect={() => {
                  setConfinement("record");
                  // Recording means allow-everything by definition; anything else
                  // would record a policy narrower than what the run really did.
                  patch({ allowAllEgress: true });
                }}
                title="Record"
                body="Allow everything. Log everything. Write the policy from what actually happened."
              />
              <RadioCard
                on={confinement === "confined"}
                onSelect={() => {
                  setConfinement("confined");
                  patch({ allowAllEgress: false });
                }}
                title="Confined"
                body="Default-deny. New hosts are held at the door for your approval."
              />
              <RadioCard
                on={confinement === "saved"}
                onSelect={() => setConfinement("saved")}
                title="Saved policy"
                body="Reuse a policy you already have."
              >
                <Select
                  value={state.selectedPolicyId ?? ""}
                  onValueChange={(v) => {
                    // Raise the barrier to the policy's floor at PICK time —
                    // the stored spec refuses to launch below it, and the old
                    // flow only learned that from a 422 after clicking Launch.
                    const floor = savedPolicies.find((p) => p.id === v)?.spec
                      ?.min_confinement_class;
                    patch({
                      selectedPolicyId: v,
                      ...(floor &&
                      ORDERED_CLASSES.indexOf(floor) >
                        ORDERED_CLASSES.indexOf(state.confinementClass)
                        ? { confinementClass: floor }
                        : {}),
                    });
                  }}
                >
                  <SelectTrigger aria-label="Saved policy">
                    <SelectValue placeholder="Pick a policy" />
                  </SelectTrigger>
                  <SelectContent>
                    {savedPolicies.map((p) => (
                      <SelectItem key={p.id} value={p.id}>
                        {p.name}
                      </SelectItem>
                    ))}
                  </SelectContent>
                </Select>
              </RadioCard>

              <div className="border-t border-border pt-3">
                <p className="mb-2 text-[0.8125rem] font-medium text-foreground">Barrier</p>
                <Seg
                  label="Barrier"
                  value={cc}
                  onChange={(id) => patch({ confinementClass: id as ConfinementClass })}
                  options={ORDERED_CLASSES.map((c) => ({
                    id: c,
                    label: CC_META[c].label,
                    disabled: !!availableClasses && !availableClasses.includes(c),
                  }))}
                />
                {availableClasses &&
                  ORDERED_CLASSES.filter((c) => !availableClasses.includes(c)).map((c) => (
                    <p key={c} className="mt-1.5 text-[0.6875rem] text-muted-foreground">
                      {CC_META[c].label} isn&apos;t installed on this host.
                    </p>
                  ))}
                {probeSettled && !availableClasses && (
                  <p className="mt-1.5 text-[0.6875rem] text-muted-foreground">
                    Couldn&apos;t check which barriers this host has — all three stay selectable.
                  </p>
                )}
              </div>
            </div>
          </SectionCard>

          <SectionCard title="Network">
            {isRecord ? (
              <p className="text-[0.8125rem] leading-relaxed text-muted-foreground italic">
                Everything is allowed. That is what recording means.
              </p>
            ) : (
              <div className="space-y-2">
                <RadioCard
                  on={preset === "none"}
                  onSelect={() => setPreset("none")}
                  title="None"
                  body="No hosts. Everything becomes an approval request."
                />
                <RadioCard
                  on={preset === "model"}
                  onSelect={() => setPreset("model")}
                  title="Just the model provider"
                  body="api.anthropic.com only — the agent reaches its model and nothing else."
                />
                <RadioCard
                  on={preset === "registries"}
                  onSelect={() => setPreset("registries")}
                  title="Common package registries"
                  body={`${PRESET_DOMAINS.length} hosts — npm, PyPI, crates.io, Go proxy, Maven Central and the rest.`}
                />
                <RadioCard
                  on={preset === "everything"}
                  onSelect={() => setPreset("everything")}
                  title="Everything"
                  body="Open egress. Nothing is blocked."
                />

                {/* Host detail lives in ONE surface, not a stack of disclosures. */}
                <div className="flex flex-wrap items-center justify-between gap-2 border-t border-border pt-3">
                  <p className="text-[0.75rem] text-muted-foreground">
                    {preset === "custom"
                      ? "Edited — this run uses your host list."
                      : "Pick individual hosts, set what happens for anything unlisted, and block hosts outright."}
                  </p>
                  <Button variant="secondary" size="sm" onClick={() => setNetOpen(true)}>
                    Edit hosts…
                  </Button>
                </div>

                {!state.allowAllEgress && (
                  <p className="text-[0.75rem] text-muted-foreground">
                    Unlisted hosts: <span className="text-foreground">{ruleTitle}</span>
                  </p>
                )}
              </div>
            )}
          </SectionCard>
        </div>

        {/* ── Right: the live rail ───────────────────────────────── */}
        <aside className="h-fit rounded-xl border border-border bg-surface-1 p-4 lg:sticky lg:top-6">
          <p className="mb-3 text-sm font-semibold text-foreground">What this run can do</p>

          <div className="space-y-3">
            {selectedPolicy && (
              <RailSection title="Policy">
                <p className="text-[0.8125rem] font-medium text-foreground">{selectedPolicy.name}</p>
                <p className="mt-0.5 text-[0.75rem] text-muted-foreground">
                  The stored spec governs this run — barrier floor{" "}
                  {CC_META[selectedPolicy.spec.min_confinement_class].label},{" "}
                  {selectedPolicy.spec.allow_all_egress
                    ? "open egress"
                    : `${(selectedPolicy.spec.allowed_domains ?? []).length} host${(selectedPolicy.spec.allowed_domains ?? []).length === 1 ? "" : "s"} allowed`}
                  . The network edits on this page do not apply to it.
                </p>
              </RailSection>
            )}
            <RailSection title="Barrier">
              <div className="mb-1 flex items-center gap-2">
                <Chip tone="neutral">{CC_META[cc].label}</Chip>
                <span className="text-[0.75rem] text-muted-foreground">· {CC_META[cc].tagline}</span>
              </div>
              <p className="text-[0.75rem] text-muted-foreground">{CC_META[cc].doesntProtect}</p>
            </RailSection>

            {!selectedPolicy && (
            <RailSection title="Network">
              {isRecord ? (
                <div className="space-y-2">
                  <p className="rounded-md border border-warning/30 bg-warning-subtle px-2 py-1.5 text-[0.75rem] text-foreground">
                    Unrestricted — every host this run reaches is logged and becomes the policy.{" "}
                    {state.deniedDomains.length > 0
                      ? `${state.deniedDomains.length} denied host${state.deniedDomains.length === 1 ? "" : "s"} stay blocked — denies always win, recording included.`
                      : "Nothing is blocked."}
                  </p>
                  {cc === "CC1" && (
                    <p className="rounded-md border border-warning/30 bg-warning-subtle px-2 py-1.5 text-[0.75rem] text-foreground">
                      On {CC_META.CC1.label}, an unrestricted run can move your data out. Use{" "}
                      {CC_META.CC2.label} or {CC_META.CC3.label} to record.
                    </p>
                  )}
                </div>
              ) : state.allowAllEgress ? (
                <p className="text-[0.75rem] text-muted-foreground">Open egress. Nothing is blocked.</p>
              ) : hostCount === 0 ? (
                <>
                  <p className="text-[0.8125rem] font-medium text-foreground">0 hosts allowed</p>
                  <p className="mt-0.5 text-[0.75rem] text-muted-foreground">{ruleTitle} for anything else.</p>
                </>
              ) : (
                <>
                  <p className="mb-1.5 text-[0.8125rem] font-medium text-foreground">
                    {hostCount} host{hostCount === 1 ? "" : "s"} allowed
                  </p>
                  <div className="space-y-1">
                    {state.allowedDomains.slice(0, 3).map((h) => (
                      <Mono key={h} className="block text-[0.75rem] text-foreground">
                        {h}
                      </Mono>
                    ))}
                    {state.allowedDomains.length > 3 && (
                      <p className="font-mono text-[0.75rem] text-muted-foreground">
                        …{state.allowedDomains.length - 3} more
                      </p>
                    )}
                    {/* Hosts the RUN implies but nobody typed — named with why,
                        so an unexplained host never appears in the allowlist. */}
                    {implied.map((h) => (
                      <div key={h.host} className="flex flex-wrap items-center gap-1.5">
                        <Mono className="text-[0.75rem] text-foreground">{h.host}</Mono>
                        <Chip tone="neutral">added by your {h.why}</Chip>
                      </div>
                    ))}
                  </div>
                  <p className="mt-1.5 text-[0.75rem] text-muted-foreground">{ruleTitle} for anything else.</p>
                </>
              )}
              {state.deniedDomains.length > 0 && (
                <p className="mt-1.5 text-[0.75rem] text-muted-foreground">
                  {state.deniedDomains.length} blocked outright.
                </p>
              )}
            </RailSection>
            )}

            <RailSection title="Credentials">
              {isAgent && llmReady === false && (
                <p className="mb-1.5 rounded-md border border-warning/30 bg-warning-subtle px-2 py-1.5 text-[0.75rem] text-foreground">
                  No model provider is connected. This run launches; its first model call fails.
                </p>
              )}
              <p className="text-[0.75rem] leading-relaxed text-muted-foreground">
                Minted at launch, injected by the proxy. Never written into the sandbox.
              </p>
            </RailSection>

            {/* What actually happens when this launches. The startup choice is
                a real fork now, and the rail is where this screen states
                consequences rather than leaving them to be discovered. */}
            <RailSection title="Startup">
              <p className="text-[0.75rem] text-muted-foreground">
                {isInteractive
                  ? state.interactiveStart === "agent"
                    ? `Comes up idle with the workspace ready. Attaching starts ${agentName} in it.`
                    : "Comes up idle with the workspace ready. Attaching drops you into a terminal."
                  : isAgent
                    ? `${agentName} runs the task unattended, then the run stops.`
                    : "The command runs unattended in the sandbox, then the run stops."}
              </p>
            </RailSection>

            <RailSection title="Recording">
              <p className="text-[0.75rem] text-muted-foreground">
                Every keystroke and every outbound connection.
              </p>
            </RailSection>
          </div>

          {error && (
            <p className="mt-3 flex items-start gap-1.5 text-[0.75rem] text-danger">
              <TriangleAlert className="mt-0.5 size-3.5 shrink-0" />
              {error}
            </p>
          )}

          <Button className="mt-4 w-full" disabled={launching || !!problem} onClick={launch}>
            {launching && <Loader2 className="size-4 animate-spin" />}
            Launch run
          </Button>
          {/* A disabled button that doesn't say why is a dead end. This screen
              had NO client-side validation at all before — an empty form
              launched, and the server's rejection arrived after the fact. */}
          {problem && !launching && (
            <p className="mt-2 text-center text-[0.75rem] text-muted-foreground">{problem}</p>
          )}
        </aside>
      </div>

      <NetworkDialog
        open={netOpen}
        onOpenChange={setNetOpen}
        value={netValue}
        onSave={(next) => {
          patch(next);
          setNetOpen(false);
          // An explicit host edit means this is no longer a recording run —
          // recording is allow-everything by definition.
          if (confinement === "record" && !next.allowAllEgress) setConfinement("confined");
        }}
      />

      {addWsOpen && (
        <AddWorkspaceDialog
          existingNames={workspaces.map((w: Workspace) => w.name)}
          onClose={() => setAddWsOpen(false)}
          onCreated={() => {
            setAddWsOpen(false);
            reloadWorkspaces();
            toast.success("Workspace added");
          }}
        />
      )}
    </div>
  );
}
