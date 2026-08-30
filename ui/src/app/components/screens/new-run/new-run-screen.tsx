/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// New run — ONE page, two columns, with a live rail that answers "what can this
// run actually do?" while you build it.
//
// Derived from the deleted Figma Make design snapshot's NewRunScreen. It
// replaces a five-step modal wizard whose Review screen
// was the first place the consequences of your choices appeared — by which
// point you had made all of them blind.
//
// It is a RE-LAYOUT, not a re-model. WizardState already carries every field
// this screen edits, and buildSpec()/impliedEgressHosts() are reused verbatim,
// so the launch payload and the policy it produces are exactly what the wizard
// produced. That is deliberate: the governance contract is the tested part, and
// this change is about when the operator SEES it, not what it is.
//
// The Confinement and Network cards are GONE. `inline_policy` on POST /runs is
// the identical Go struct a saved policy stores, validated by the same
// validator — so this screen and /policies now author it through ONE component
// (wardyn/policy-panel.tsx) instead of a bespoke form that could only ever
// assemble a subset of what the server already accepts. What the JSON cannot
// know — the Workspace card's mounts/repos, the grant lanes' grants and the
// egress hosts those grants require — is unioned back in by mergeRunSelections
// and NAMED on screen, never merged behind the operator's back.
import * as React from "react";
import { useNavigate } from "react-router-dom";
import { ArrowLeft, Plus } from "lucide-react";
import { toast } from "sonner";
import type { AgentRun, ConfinementClass, PreflightResult, RunPolicySpec, Workspace } from "../../../lib/types";
import { Link } from "react-router-dom";
import type { WizardAgent } from "./wizard-types";
import { SectionCard, Seg } from "./new-run-primitives";
import { RunRail } from "./new-run-rail";
import { runs as runsApi } from "../../../lib/api/runs";
import { policies as policiesApi } from "../../../lib/api/policies";
import { health as healthApi } from "../../../lib/api/health";
import { setup as setupApi } from "../../../lib/api/setup";
import { hasLlmPath } from "../../../lib/readiness";
import { useWorkspaceList } from "../../../lib/use-workspace-list";
import { capabilityAllowed, useMyCapabilities } from "../../../lib/capabilities";
import { DENIED } from "../../../lib/permissions-copy";
import { getErrorMessage } from "../../../lib/format";
import { statusWord } from "../../../lib/workspace-status";
import { useDeferredBusy } from "../../../lib/use-deferred-busy";
import { Button } from "../../ui/button";
import { Checkbox } from "../../ui/checkbox";
import { Input } from "../../ui/input";
import { Textarea } from "../../ui/textarea";
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "../../ui/select";
import { Field } from "../../wardyn/form-primitives";
import { Mono } from "../../wardyn/code-block";
import { Chip } from "../../wardyn/primitives";
import { useOperator } from "../../wardyn/operator-context";
import { CC_META } from "../../wardyn/cc-meta";
import { RUN_MODE } from "../../wardyn/copy";
import { getDefaultCc, resolveDefaultCc } from "../../wardyn/default-confinement";
import { PolicyPanel, POLICY_TEMPLATES, parseSpec, toolRulesSummary } from "../../wardyn/policy-panel";
import { AddWorkspaceDialog } from "../add-workspace-dialog";
import { buildSpec, mergeRunSelections } from "./wizard-spec";
import { agentLabel, initialWizardState, primaryWorkspaceId, type WizardState } from "./wizard-types";
import { surfaceRunWarnings } from "./run-warnings";

const ORDERED_CLASSES: ConfinementClass[] = ["CC1", "CC2", "CC3"];

// The body a fresh Custom policy opens with: a valid, editable floor rather
// than a blank document nobody can start from.
const MINIMAL = POLICY_TEMPLATES.find((t) => t.id === "minimal")!;

// A tier is pickable only if the host can BUILD it and the policy allows it.
const rank = (c: ConfinementClass) => ORDERED_CLASSES.indexOf(c);

export function NewRunScreen() {
  const navigate = useNavigate();
  const { workspaces, reload: reloadWorkspaces } = useWorkspaceList();
  // Visibility is not capability: the workspace list is NOT narrowed by the
  // `workspace` grant (the launch gate refuses, and a hidden workspace makes
  // that refusal unexplainable and the grant undiscoverable). Ungranted rows
  // are annotated instead.
  const operator = useOperator();
  const caps = useMyCapabilities(!operator);
  // Seed with the PERSISTED default (Settings' promise); the health probe
  // below re-resolves it against what this host actually enforces. The old
  // resolveDefaultCc(…, ["CC1"]) hardcoded the availability list, so a saved
  // Wall/Vault default could never win — Settings' promise was untrue here.
  const [state, setState] = React.useState<WizardState>(() =>
    initialWizardState(getDefaultCc() ?? "CC1"),
  );
  // The policy this run ships, as the operator wrote it. `useSaved` is the mode
  // row: reuse a stored policy by REFERENCE (policy_id) or author one here.
  const [useSaved, setUseSaved] = React.useState(false);
  // The DEFAULT body floors at the operator's own default barrier (persisted
  // pick, else CC1) — NOT Minimal's authored CC2. The pre-panel screen was
  // launchable by construction (its composed floor was the selected tier); a
  // hardcoded CC2 default would open every fresh /runs/new on a Fence-only
  // host fail-closed, all tiers dead, before the operator authored anything.
  // Clicking the Minimal CHIP afterwards is an authored act and still floors
  // CC2 — that corner stays, with its reason line and preflight naming it.
  const [specText, setSpecText] = React.useState(() =>
    JSON.stringify(
      { ...MINIMAL.spec, min_confinement_class: getDefaultCc() ?? "CC1" },
      null,
      2,
    ),
  );
  // The floor the LAST SUCCESSFUL parse authored — deliberately sticky across a
  // broken edit: a half-typed document must not momentarily drop the floor and
  // re-open a barrier tier the operator's own policy forbids.
  const [parsedFloor, setParsedFloor] = React.useState<ConfinementClass | undefined>(
    () => getDefaultCc() ?? "CC1",
  );
  // The form as the MACHINE left it — what `dirty` below compares against.
  // specText's baseline is fixed, but the barrier is the one field the machine
  // writes on its own (the health probe re-resolves it, the policy floor
  // up-clamps it), so its baseline moves with those writes. Comparing it to a
  // constant would call an untouched form dirty and break Esc entirely.
  const pristineSpec = React.useRef(specText);
  const pristineCc = React.useRef(state.confinementClass);
  const [addWsOpen, setAddWsOpen] = React.useState(false);
  const [availableClasses, setAvailableClasses] = React.useState<ConfinementClass[] | null>(null);
  const [launching, setLaunching] = React.useState(false);
  // Rulebook §7: disable Launch the instant it fires, but only show the
  // spinner once the request has been running long enough to need one.
  const { disabled: launchDisabled, showSpinner: launchSpinning } = useDeferredBusy(launching);
  const [error, setError] = React.useState<string | null>(null);
  // Preflight is a dry-run of the SAME request Launch sends — see buildRunInput
  // below. Independent loading/result/error state from Launch's: the two
  // actions can be in flight or have failed independently of one another.
  const [preflighting, setPreflighting] = React.useState(false);
  const [preflightResult, setPreflightResult] = React.useState<PreflightResult | null>(null);
  const [preflightError, setPreflightError] = React.useState<string | null>(null);
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
  // The ONE validation-error state on this form. Painted only once the operator
  // has been in the field and left it empty — a red ring on an untouched form is
  // an accusation about something nobody has done yet.
  const [titleTouched, setTitleTouched] = React.useState(false);

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

  // The envelope-field detach funnel (D11/claim6) retired with the controls it
  // guarded: four of its five fields were the Network card's, and the fifth —
  // confinementClass — is now guarded by the floor-DISABLE below instead, which
  // is strictly stronger. A one-time up-clamp alone would re-open the
  // below-floor 422 (runs_create.go's floor check, on both the policy_id and
  // inline paths) the moment the operator lowered the Seg afterwards. Detach
  // now has exactly one trigger: editing the spec text (see onSpecChange).
  const patch = React.useCallback((p: Partial<WizardState>) => setState((s) => ({ ...s, ...p })), []);

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
          const resolved = resolveDefaultCc(getDefaultCc(), classes);
          pristineCc.current = resolved;
          setState((s) => ({ ...s, confinementClass: resolved }));
        }
      });
    probe();
    return () => {
      alive = false;
    };
  }, []);

  const isAgent = state.runType === "agent";
  const cc = state.confinementClass;
  const parsed = parseSpec(specText);
  // A shell command is unattended by definition — buildSpec forces batch for
  // one, so the Run mode segment is hidden rather than offering a combination
  // that would silently drop the command.
  const isInteractive = isAgent && state.mode === "interactive";
  // Shared display name (wizard-types.agentLabel) — a local re-hardcode here
  // is exactly the drift that helper's doc says it exists to prevent.
  const agentName = agentLabel(state.agent);
  const selectedPolicy =
    useSaved && state.selectedPolicyId
      ? savedPolicies.find((p) => p.id === state.selectedPolicyId)
      : undefined;
  // Whether the workspace this run is aimed at is one the caller may launch
  // against. Advisory — denyMemberRequest is the real gate.
  const pickedWorkspaceId = state.workspaces[0]?.workspaceId;
  const selectedWorkspaceUngranted =
    !!pickedWorkspaceId && !capabilityAllowed(caps, "workspace", pickedWorkspaceId);

  // The screen's ONE validation rule. Deliberately a local derivation rather
  // than a shared validator: it answers "can this button be pressed", which is
  // this screen's question, and a second general-purpose answer living
  // elsewhere is what drifts out of sync with the form it describes.
  const needsTask = !isAgent || state.mode === "batch";
  const problem = !state.title.trim()
    ? "Give this run a title."
    : needsTask && !state.task.trim()
      ? isAgent
        ? "An autonomous run needs a task to perform."
        : "Enter a command to run."
      : // A Custom policy that doesn't parse has nothing to send. The saved
        // lane launches by reference, so its body is never on the wire.
        !useSaved && !parsed.ok
        ? "The policy spec isn't valid JSON."
        : useSaved && !state.selectedPolicyId
          ? "Pick a saved policy, or write a custom one."
          : null;

  // Every successful parse re-reads the floor the document authors; a FAILED
  // parse changes nothing (parsedFloor stays whatever last parsed).
  React.useEffect(() => {
    const p = parseSpec(specText);
    if (!p.ok) return;
    const f = p.spec.min_confinement_class as ConfinementClass;
    setParsedFloor(ORDERED_CLASSES.includes(f) ? f : undefined);
  }, [specText]);

  // The ACTIVE floor: a picked saved policy's stored floor, else the last
  // successful parse's. Both paths refuse to launch below it server-side.
  const floor = useSaved ? (selectedPolicy?.spec.min_confinement_class as ConfinementClass | undefined) : parsedFloor;

  // UP-CLAMP the Barrier Seg to the active floor. `cc` is in the deps on
  // purpose: the health probe resolves ASYNCHRONOUSLY and re-seeds
  // confinementClass from the persisted default, which can land BELOW a floor
  // this already clamped to. Watching the value, not just the floor, makes
  // "never below the floor" an invariant instead of a one-shot.
  React.useEffect(() => {
    if (!floor || !ORDERED_CLASSES.includes(floor)) return;
    if (rank(floor) > rank(cc)) {
      pristineCc.current = floor;
      patch({ confinementClass: floor });
    }
  }, [floor, cc, patch]);

  // The post-parse union, computed ONCE: the same value renders the "Added for
  // this run's selections" line and goes on the wire, so the screen cannot show
  // one policy and launch another.
  const merged = React.useMemo(
    () => (parsed.ok ? mergeRunSelections(parsed.spec, state, workspaces) : null),
    // parsed is rebuilt every render; specText is what actually changes.
    // eslint-disable-next-line react-hooks/exhaustive-deps
    [specText, state, workspaces],
  );
  const added = merged?.added;
  // What happens the moment this launches, in one sentence. Derived HERE and
  // handed to the rail, so the rail cannot describe one run while Launch sends
  // another.
  const startupLine = isInteractive
    ? state.task.trim()
      ? state.interactiveStart === "agent"
        ? `Starts ${agentName} on your prompt at boot — attach to watch and take over.`
        : "Runs your startup command at boot, then a terminal is ready."
      : state.interactiveStart === "agent"
        ? `Comes up idle with the workspace ready. Attaching starts ${agentName} in it.`
        : "Comes up idle with the workspace ready. Attaching drops you into a terminal."
    : isAgent
      ? `${agentName} runs the task unattended, then the run stops.`
      : "The command runs unattended in the sandbox, then the run stops.";

  // What this run's tool_rules actually say, from the SAME spec that ships:
  // the merged document on the custom lane, the stored one on the saved lane.
  // Null when there are no rules, so a policy written before the field existed
  // grows no empty rail section.
  const toolRules = React.useMemo(() => {
    const spec = useSaved ? selectedPolicy?.spec : merged?.spec;
    return spec ? toolRulesSummary(spec) : null;
  }, [useSaved, selectedPolicy, merged]);
  const hasAdditions =
    !!added && (added.hosts.length > 0 || added.grants.length > 0 || added.mounts.length > 0 || added.repos.length > 0);

  // Editing the spec text DETACHES a picked saved policy: the body on screen is
  // no longer the stored one, and launching by reference would ship a policy
  // nobody is looking at.
  const onSpecChange = (next: string) => {
    setSpecText(next);
    if (state.selectedPolicyId) patch({ selectedPolicyId: undefined });
  };

  // Picking a policy REPLACES the body, so the textarea always shows what will
  // actually govern the run even while the reference path is what ships.
  const onPickPolicy = (id: string) => {
    const p = savedPolicies.find((x) => x.id === id);
    if (p) setSpecText(JSON.stringify(p.spec, null, 2));
    patch({ selectedPolicyId: id });
  };

  // The ONE request-payload builder — Launch and Preflight must send EXACTLY
  // the same body, since preflight's verdict is only true if it is a dry-run
  // of what Launch actually does. A second builder here is how the two drift.
  const buildRunInput = () => {
    const { run } = buildSpec(state, workspaces);
    // The MODE ROW is the discriminator: a policy id that somehow survives a
    // switch back to Custom still must not launch by reference. And the
    // workspace_id override must never OVERWRITE buildSpec's deliberate
    // ephemeral-workspace fallback with undefined — that silently launched a
    // workspace-less run.
    if (useSaved && state.selectedPolicyId) {
      return {
        ...run,
        policy_id: state.selectedPolicyId,
        workspace_id: primaryWorkspaceId(state.workspaces, workspaces) ?? run.workspace_id,
      };
    }
    // Unreachable: `problem` disables both actions while the document is
    // broken. Throwing beats substituting a composed fallback nobody wrote.
    if (!merged) throw new Error("The policy spec isn't valid JSON.");
    return { ...run, inline_policy: merged.spec };
  };

  const launch = async () => {
    setError(null);
    setLaunching(true);
    try {
      const created: AgentRun = await runsApi.createRun(buildRunInput());
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

  // A dry-run of launch's own resolution: same body, same 4xx surface, but
  // mints/dispatches nothing. Renders the member-clamp warnings, the risk
  // grade, and the confinement class the run will actually be enforced at.
  const preflight = async () => {
    // The saved lane with nothing picked has NO body to dry-run — falling
    // through would preflight the leftover Custom document this lane will
    // never launch, breaking buildRunInput's same-body invariant. (The panel
    // disables the button in this state too; this guards the race.)
    if (useSaved && !state.selectedPolicyId) return;
    setPreflightError(null);
    setPreflightResult(null);
    setPreflighting(true);
    try {
      setPreflightResult(await runsApi.preflightRun(buildRunInput()));
    } catch (e) {
      setPreflightError(getErrorMessage(e) || "Preflight failed.");
    } finally {
      setPreflighting(false);
    }
  };

  // Rulebook §8: Esc backs out quietly, with no prompt for an untouched form.
  //
  // "Untouched" is the WHOLE form, not four scalar fields: an edited policy
  // body, an attached workspace, a changed barrier or run mode are all work
  // this would throw away. One whole-state comparison rather than a field list,
  // so a control added to this screen cannot quietly fall outside it.
  //
  // ponytail: a DIRTY form ignores Esc rather than prompting to discard — the
  // explicit discard prompt the rulebook asks for needs copy M4 does not draw,
  // and the ghost "Runs" button is still one click away. Wire the prompt when
  // the mock carries its words.
  const dirty =
    useSaved ||
    specText !== pristineSpec.current ||
    JSON.stringify(state) !== JSON.stringify(initialWizardState(pristineCc.current));
  React.useEffect(() => {
    const onKey = (e: KeyboardEvent) => {
      // defaultPrevented is load-bearing: Radix's DismissableLayer preventDefaults
      // Escape on document capture and THEN dismisses, but the event still
      // reaches window — so closing a Select or the Add-workspace dialog was also
      // leaving the screen.
      if (e.key !== "Escape" || e.defaultPrevented || dirty) return;
      navigate("/runs");
    };
    window.addEventListener("keydown", onKey);
    return () => window.removeEventListener("keydown", onKey);
  }, [dirty, navigate]);

  return (
    <div className="mx-auto w-full max-w-[1200px] px-6 py-6">
      <div className="mb-6 flex items-center gap-3">
        <Button variant="ghost" size="sm" onClick={() => navigate("/runs")}>
          <ArrowLeft className="size-4" /> Runs
        </Button>
        <h1 className="text-foreground">New run</h1>
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
                  // Rulebook §8: default focus lands on the primary field, not
                  // on the back-out button or the first select.
                  autoFocus
                  aria-invalid={titleTouched && !state.title.trim() ? true : undefined}
                  onBlur={() => setTitleTouched(true)}
                  // NO Enter-to-launch here. Rulebook §8 allows it from a
                  // single-line input, but this is the input carrying the
                  // datalist below: choosing a suggestion with Enter dispatches
                  // keydown Enter on the input (Chrome), so picking a known
                  // title off the list would LAUNCH the run. Completion and
                  // commit cannot share a key. Launch is the rail's button.
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
                  is configured by a startup choice, an autonomous run by a prompt, and
                  a shell command by the command — never all three at once.
                  Hidden for Shell command, which is unattended by definition. */}
              {isAgent && (
                <Seg
                  label="Run mode"
                  value={state.mode}
                  onChange={(id) => patch({ mode: id as WizardState["mode"] })}
                  options={[
                    // Label from copy.ts's RUN_MODE canon, not spelled here: the
                    // internal id stays "batch" (it is wire-adjacent and renaming
                    // it reaches the spec builder and its tests), but the word a
                    // human reads is "Autonomous" everywhere else in the product.
                    // The two had drifted, and this radio was the last place the
                    // console still said "Batch" out loud.
                    { id: "batch", label: `${RUN_MODE.autonomous.label} — run it unattended` },
                    { id: "interactive", label: "Interactive — I drive the terminal" },
                  ]}
                />
              )}

              {isInteractive ? (
                // What an interactive run actually configures is what greets you
                // when you attach — plus, now, an OPTIONAL boot seed (Part A1):
                // the same task text a batch run would use as a prompt, fired
                // once at sandbox boot instead of discarded. Left blank, it's
                // exactly today's idle-until-attach run.
                <>
                  <Seg
                    label="Start with"
                    hint="The workspace is prepared before you land in it. Same barrier, same recording either way."
                    value={state.interactiveStart}
                    onChange={(id) => patch({ interactiveStart: id as WizardState["interactiveStart"] })}
                    options={[
                      { id: "agent", label: `${agentName} — launch it in the workspace` },
                      { id: "shell", label: "Terminal — a shell in the workspace dir" },
                    ]}
                  />
                  {state.interactiveStart === "agent" ? (
                    <Field
                      label="Initial prompt (optional)"
                      htmlFor="nr-seed"
                      hint={`Starts ${agentName} on this at boot, in the same session you attach to. Leave it blank to come up idle instead.`}
                    >
                      <Textarea
                        id="nr-seed"
                        rows={3}
                        placeholder="Start by reviewing the failing tests in payments/"
                        value={state.task}
                        onChange={(e) => patch({ task: e.target.value })}
                      />
                    </Field>
                  ) : (
                    <Field
                      label="Startup command (optional)"
                      htmlFor="nr-seed"
                      hint="Runs at boot, before you attach. Leave it blank to come up idle instead."
                    >
                      <Textarea
                        id="nr-seed"
                        rows={2}
                        className="font-mono"
                        placeholder="npm ci && npm run dev"
                        value={state.task}
                        onChange={(e) => patch({ task: e.target.value })}
                      />
                    </Field>
                  )}
                  {/* Only an agent-started seed has a tool-approval prompt to
                      auto-approve; a bare shell command has none, and an empty
                      seed has nothing to run unsupervised in the first place. */}
                  {state.interactiveStart === "agent" && state.task.trim() && (
                    <div className="space-y-2">
                      <label
                        htmlFor="nr-seed-auto-tools"
                        className="flex items-center gap-2 text-xs text-foreground"
                      >
                        <Checkbox
                          id="nr-seed-auto-tools"
                          checked={state.seedAutoTools}
                          onCheckedChange={(v) => patch({ seedAutoTools: v === true })}
                        />
                        Let it use tools before I attach
                      </label>
                      <p className="text-xs leading-snug text-muted-foreground">
                        Auto-approves the agent&apos;s own tool use until you join. The sandbox and
                        egress policy still apply.
                      </p>
                    </div>
                  )}
                </>
              ) : (
                <>
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
                  {/* Autonomous agent runs only — a shell command has no tool
                      calls to approve, and codex has no external approval
                      contract to route them through (disabled below, honestly). */}
                  {isAgent && (
                    <Seg
                      label="Tool approvals"
                      value={state.toolApprovals}
                      onChange={(id) => patch({ toolApprovals: id as WizardState["toolApprovals"] })}
                      hint={
                        state.agent === "codex-cli"
                          ? "Codex CLI has no external tool-approval contract — this run always keeps the sandbox as its only boundary."
                          : undefined
                      }
                      options={[
                        { id: "auto", label: "Auto — the sandbox is the boundary" },
                        {
                          id: "hold",
                          label: "Hold in Wardyn — every tool call parks as an approval",
                          disabled: state.agent === "codex-cli",
                        },
                      ]}
                    />
                  )}
                </>
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
                    {!capabilityAllowed(caps, "workspace", w.id) && (
                      <Chip tone="neutral">{DENIED.WORKSPACE_CHIP}</Chip>
                    )}
                  </SelectItem>
                ))}
              </SelectContent>
            </Select>
            {/* The reason rides the SELECTION, not each row: a Radix item's
                content is what the closed trigger renders, so a per-row
                paragraph would end up inside the trigger. The chip above
                annotates every ungranted row; this says what it costs. */}
            {selectedWorkspaceUngranted && (
              <p className="mt-2 text-xs text-muted-foreground">{DENIED.WORKSPACE_BODY}</p>
            )}
            <Button
              type="button"
              variant="ghost"
              size="sm"
              className="mt-2 px-1"
              onClick={() => setAddWsOpen(true)}
            >
              <Plus className="size-4" /> Add workspace
            </Button>
          </SectionCard>

          <SectionCard title="Policy">
            <div className="space-y-4">
              <PolicyPanel
                instance="run"
                value={specText}
                onChange={onSpecChange}
                onPreflight={preflight}
                preflightBusy={preflighting}
                preflightDisabled={useSaved && !state.selectedPolicyId}
                interactive={isInteractive}
                savedPolicy={{
                  active: useSaved,
                  onActiveChange: setUseSaved,
                  picker: (
                    <div className="space-y-2">
                      <Select value={state.selectedPolicyId ?? ""} onValueChange={onPickPolicy}>
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
                      {/* Rulebook §9: an empty picker carries the action that
                          fills it. With no stored policies this lane was a
                          dropdown with nothing in it and no way out. */}
                      {savedPolicies.length === 0 && (
                        <p className="text-xs text-muted-foreground">
                          No saved policies yet ·{" "}
                          <Link to="/policies" className="font-medium text-info hover:underline">
                            New policy →
                          </Link>
                        </p>
                      )}
                    </div>
                  ),
                }}
              />

              {/* What buildSpec unions in AFTER the parse, named out loud. A
                  policy the operator did not write is one they cannot be held
                  to — and these are exactly the entries the document itself
                  cannot know: the Workspace card's attachments, the grant
                  lanes' grants, and the hosts those grants must reach (an
                  api_key grant whose host is not on the allowlist authenticates
                  nothing, allow_all_egress included). Saved-policy runs launch
                  by REFERENCE, so nothing is merged into a stored spec. */}
              {!useSaved && hasAdditions && added && (
                <div
                  className="rounded-lg border border-border bg-surface-2 p-3"
                  data-testid="run-spec-additions"
                >
                  <p className="text-xs text-muted-foreground">
                    Added for this run&apos;s selections:
                  </p>
                  <div className="mt-1.5 space-y-1">
                    {added.hosts.map((h) => (
                      <div key={h} className="flex flex-wrap items-center gap-1.5">
                        <Mono className="text-xs text-foreground">{h}</Mono>
                        <Chip tone="neutral">allowed_domains</Chip>
                      </div>
                    ))}
                    {added.grants.map((g, i) => (
                      <div key={`${g.kind}-${i}`} className="flex flex-wrap items-center gap-1.5">
                        <Mono className="text-xs text-foreground">{String(g.kind)}</Mono>
                        <Chip tone="neutral">eligible_grants</Chip>
                      </div>
                    ))}
                    {added.mounts.map((m) => (
                      <div key={m.target} className="flex flex-wrap items-center gap-1.5">
                        <Mono className="text-xs text-foreground">{m.target}</Mono>
                        <Chip tone="neutral">workspace_mounts</Chip>
                      </div>
                    ))}
                    {added.repos.map((r) => (
                      <div key={r.repo} className="flex flex-wrap items-center gap-1.5">
                        <Mono className="text-xs text-foreground">{r.repo}</Mono>
                        <Chip tone="neutral">workspace_repos</Chip>
                      </div>
                    ))}
                  </div>
                </div>
              )}

              {/* The run's REQUESTED barrier — a separate wire field from the
                  spec's min_confinement_class floor, which is why it keeps its
                  own control here rather than living in the JSON. */}
              <div className="border-t border-border pt-3">
                <Seg
                  label="Barrier"
                  value={cc}
                  onChange={(id) => patch({ confinementClass: id as ConfinementClass })}
                  options={ORDERED_CLASSES.map((c) => ({
                    id: c,
                    label: CC_META[c].label,
                    // Two independent reasons, each with its own line below:
                    // the host can't build this tier, or the policy forbids it.
                    disabled:
                      (!!availableClasses && !availableClasses.includes(c)) ||
                      (!!floor && rank(c) < rank(floor)),
                  }))}
                />
                {availableClasses &&
                  ORDERED_CLASSES.filter((c) => !availableClasses.includes(c)).map((c) => (
                    <p key={c} className="mt-2 text-xs text-muted-foreground">
                      {CC_META[c].label} isn&apos;t installed on this host.
                    </p>
                  ))}
                {/* Floor-disabled tiers get their OWN reason — the line above
                    keys off availability alone, and "isn't installed" would be
                    a lie about a tier this host builds fine. Skipped for a tier
                    already named as uninstalled: one reason per tier, not two.
                    A floor above every buildable tier leaves the Seg entirely
                    disabled — fail-closed on purpose, with preflight and launch
                    naming the cause. */}
                {floor &&
                  ORDERED_CLASSES.filter(
                    (c) => rank(c) < rank(floor) && (!availableClasses || availableClasses.includes(c)),
                  ).map((c) => (
                    <p key={c} className="mt-2 text-xs text-muted-foreground">
                      {CC_META[c].label} is below the policy&apos;s floor ({CC_META[floor].label}).
                    </p>
                  ))}
                {probeSettled && !availableClasses && (
                  <p className="mt-2 text-xs text-muted-foreground">
                    Couldn&apos;t check which barriers this host has — all three stay selectable.
                  </p>
                )}
              </div>
            </div>
          </SectionCard>
        </div>

        {/* ── Right: the live rail, a fixed 320px ────────────────── */}
        <RunRail
          savedPolicy={selectedPolicy}
          cc={cc}
          showModelWarning={isAgent && llmReady === false}
          startup={startupLine}
          showHoldNote={
            !isInteractive && isAgent && state.agent === "claude-code" && state.toolApprovals === "hold"
          }
          toolRules={toolRules}
          launch={{
            onLaunch: launch,
            disabled: launchDisabled,
            spinning: launchSpinning,
            inFlight: launching,
            problem,
            error,
          }}
          preflight={{ error: preflightError, result: preflightResult }}
        />
      </div>

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
