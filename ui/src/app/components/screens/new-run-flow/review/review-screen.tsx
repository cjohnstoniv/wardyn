/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Review: the whole run with where each value came from, the true notices, one
// ordered blocker list, and Launch only on a current, fresh, clean check.

import * as React from "react";
import { Check, CircleAlert, MoreHorizontal } from "lucide-react";
import { Button } from "../../../ui/button";
import { DropdownMenu, DropdownMenuContent, DropdownMenuItem, DropdownMenuTrigger } from "../../../ui/dropdown-menu";
import { StepSection } from "../../../wardyn/step-section";
import { Segmented } from "../../../wardyn/segmented";
import { NR_FILES } from "../../../wardyn/copy/new-run/files";
import { NR_REVIEW } from "../../../wardyn/copy/new-run/review";
import { NR_SAVE } from "../../../wardyn/copy/new-run/save";
import { NR_SHAPE } from "../../../wardyn/copy/new-run/shape";
import { NR_TEMPLATES } from "../../../wardyn/copy/new-run/templates";
import { useUIGateway } from "../../../../lib/hooks/use-ui-gateway";
import type { PreflightResult, SetupItem } from "../../../../lib/types/preflight";
import { cn } from "../../../ui/utils";
import { NR_TOOLS } from "../../../wardyn/copy/new-run/tools";
import type { Template, TemplateKind, TemplatePublish } from "../../../../lib/types/templates";
import { harnessIds } from "../lenses/lens-keys";
import { intentToWire } from "../draft/intent-to-wire";
import { launchGateOpen } from "../draft/launch-gate";
import type { NewRunLocation } from "../draft/new-run-url";
import { useDraftIdentity, useIntentChecks } from "../draft/use-intent-checks";
import { isServer } from "../lenses/apps-lens";
import { useLocalIssues, useNewRunDispatch, useNewRunDraft, type NewRunScreenProps } from "../new-run-route";
import { combinationReason, loadRoster } from "../steps/harnesses/roster";
import { macOSMinimumSentence, useMacOSOffer } from "../steps/run/macos-offer";
import { useDriveCatalogue } from "../steps/files/file-sources";
import { templateSource } from "../steps/shape/shape-sources";
import { useLoaded } from "../steps/shape/use-loaded";
import { StepShell, useAnnounce } from "../steps/step-shell";
import { BLOCKERS_ID, BlockerList } from "./blocker-list";
import { buildBlockers, groupsBlockedBy, missingSetup } from "./blockers";
import { localMcpConflicts, useHarnessNames } from "../steps/tools/local-mcp";
import { AddTokenDialog, DISABLED_FILL } from "../steps/tools/setup-needed";
import { EffectivePolicyCard } from "./effective-policy-card";
import { AccessGroup } from "./groups/access-group";
import { LimitsGroup } from "./groups/limits-group";
import { RunGroup } from "./groups/run-group";
import { ToolsGroup } from "./groups/tools-group";
import { WhereGroup } from "./groups/where-group";
import { WorkGroup } from "./groups/work-group";
import { Notices } from "./notices";
import { changedFromOrigin } from "./provenance";
import { RawView } from "./raw-view";
import { BlockedGroups } from "./review-group";
import { docOnlyRows } from "./review-model";
import type { ReviewPreflight } from "./review-seams";
import { SaveAsDialog } from "./save-as-dialog";
import { useTemplateNeeds } from "./use-template-needs";

const VIEWS = [
  { value: "pretty", label: NR_REVIEW.VIEW_PRETTY },
  { value: "yaml", label: NR_REVIEW.VIEW_YAML },
  { value: "json", label: NR_REVIEW.VIEW_JSON },
] as const;
const AUTO_CHECK = { local: true, backendArm: false, modelArm: false };
const ONLY_ME: TemplatePublish = { org: false, groups: [], groups_unverified: false };

export interface ReviewScreenProps extends NewRunScreenProps {
  /** Seam: the Name dialog (E9) opens from here. Without it Launch has nothing to open. */
  onLaunch?: () => void;
  /** Seam: DS3-S2 `GET /me/drives`. The saver's own drives, left out of a shared template. */
  ownDriveIds?: ReadonlySet<string>;
}

function setupTitle(intent: ReturnType<typeof useNewRunDraft>["intent"]): string | undefined {
  if (isServer(intent)) return NR_REVIEW.SETUP_APPLICATION_SERVER;
  if (intent.shape === "application") return NR_REVIEW.SETUP_APPLICATION;
  if (intent.shape === "environment") return NR_REVIEW.SETUP_ENVIRONMENT;
  if (intent.shape !== "agent_task") return undefined;
  return intent.experience === "background" ? NR_REVIEW.SETUP_AGENT_AUTONOMOUS : NR_REVIEW.SETUP_AGENT_INTERACTIVE;
}

const MARK_TONE = {
  danger: "border-danger/25 bg-danger-subtle text-danger",
  warning: "border-warning/30 bg-warning-subtle text-warning",
  success: "border-success/35 bg-success-subtle text-success",
} as const;

/** The status mark the group rows draw, with its word beside it: a mark, never a pill (the StepSection marker rule). */
function SetupMark({ tone }: { tone: keyof typeof MARK_TONE }) {
  const Icon = tone === "success" ? Check : CircleAlert;
  return (
    <span data-testid="review-status-mark" data-tone={tone} className="inline-flex items-center gap-1.5">
      <span aria-hidden="true" className={`grid size-[22px] place-items-center rounded-full border ${MARK_TONE[tone]}`}>
        <Icon className="size-3" />
      </span>
      {tone === "danger" ? NR_REVIEW.BADGE_ATTENTION : tone === "warning" ? NR_REVIEW.BADGE_CHECK : NR_REVIEW.BADGE_COMPLETE}
    </span>
  );
}

export function ReviewScreen({ location, go, onLaunch, ownDriveIds }: ReviewScreenProps) {
  const draft = useNewRunDraft();
  const dispatch = useNewRunDispatch();
  const announce = useAnnounce();
  const issues = useLocalIssues();
  const view = location.screen === "review" ? location.view : "pretty";

  const wire = React.useMemo(() => intentToWire(draft.intent), [draft.intent]);
  const checks = useIntentChecks({
    body: issues.length ? null : wire.body,
    selectionKey: "",
    identity: useDraftIdentity(),
    externalRevision: "",
    sourceRefreshPending: false,
    autoCheck: AUTO_CHECK,
    doorOpen: false,
    adoDoorOpen: false,
  });
  const pre = checks.preflightResult as PreflightResult | null as ReviewPreflight | null;
  const template = useTemplateNeeds(draft.origin, draft.intent);
  const harnessNames = useHarnessNames();
  const gateway = useUIGateway();
  // Harnesses no declared image carries together: the Harnesses step refuses them, and a draft that got here another way is held too.
  const roster = useLoaded(loadRoster);
  const ids = harnessIds(draft.intent);
  const macOSOffer = useMacOSOffer();
  const combination = roster.status === "ready" ? combinationReason(roster.value, ids, ids[0]) : undefined;

  const gate = (blockers: number) =>
    launchGateOpen({ blockers, preflight: checks.preflightRead, generation: checks.generation, now: Date.now() });
  // The tick is on the draft: the catalogue says whether the person is at their drive limit.
  const keepAsked = draft.intent.keep_work_folder === true;
  const keepDrives = useDriveCatalogue(pre?.runner_pool?.id ?? draft.intent.runner_pool_id, keepAsked);
  const create = keepDrives.catalogue?.create;
  const inputs = {
    issues,
    keepLimit: keepAsked && create?.reason === "limit_reached" ? { text: NR_FILES.KEEP_LIMIT(create.count, create.limit), hint: NR_FILES.KEEP_LIMIT_HINT } : undefined,
    unsupported: wire.unsupported,
    combination,
    macOSMinimum: macOSMinimumSentence(draft.intent, macOSOffer),
    templateNeeds: template.needs,
    templateUnread: template.unread,
    // A credential refusal is not a blocker: Launch's own refusal opens the provider's sign-in door.
    refusal: checks.preflightError === null || checks.preflightRefusal ? null : { text: checks.preflightError, reason: checks.preflightReason, field: checks.preflightField },
    gatewayOff: isServer(draft.intent) && gateway.on === false,
    setupItems: missingSetup(pre ?? (checks.preflightRefusal && { setup_items: checks.preflightRefusal.setupItems }), checks.preview.result),
    localMcp: localMcpConflicts(pre?.access ?? checks.preview.result?.access, draft.intent),
    harnessLabel: (id: string) => harnessNames.get(id) ?? id,
  };
  const blockers = buildBlockers({ ...inputs, stale: !gate(0) });
  const hard = blockers.filter((b) => b.kind !== "stale");
  const open = gate(hard.length);

  const asked = React.useRef(false);
  const wasOpen = React.useRef(false);
  React.useEffect(() => {
    if (open && !wasOpen.current && asked.current) announce(NR_REVIEW.CHECKED_ANNOUNCE);
    wasOpen.current = open;
  }, [open, announce]);
  const checkAgain = () => {
    asked.current = true;
    if (template.unread) template.retry();
    void checks.preflight();
    void checks.preview.retry();
    gateway.recheck();
    keepDrives.reload();
  };

  const [addingSecret, setAddingSecret] = React.useState<SetupItem | null>(null);
  const [saveKind, setSaveKind] = React.useState<TemplateKind | null>(null);
  const [publish, setPublish] = React.useState<TemplatePublish | null>(null);
  // Whether the Access group draws a walls-leave-out line; when it does not, the walls' host warning stays a notice.
  const [wallsLeftOut, setWallsLeftOut] = React.useState(false);
  const openSave = (kind: TemplateKind) => {
    setSaveKind(kind);
    templateSource.list().then((l) => setPublish(l.publish), () => setPublish(ONLY_ME));
  };
  const closeSave = () => {
    setSaveKind(null);
    setPublish(null);
  };
  const saved = (t: Template) => announce(NR_SAVE.SAVED(t.kind === "policy" ? NR_TEMPLATES.POLICY_BADGE : NR_SHAPE.TEMPLATE_LABEL));

  const group = { draft, pre, preview: checks.preview.result, go, dispatch };
  const docRows = docOnlyRows(draft.intent);
  const pool = pre?.runner_pool?.name;

  return (
    <StepShell
      eyebrow={NR_REVIEW.EYEBROW}
      title={NR_REVIEW.TITLE}
      lede={NR_REVIEW.LEDE}
      primary={{ label: NR_REVIEW.LAUNCH_RUN, onActivate: () => onLaunch?.(), disabled: !open, describedBy: BLOCKERS_ID, arrow: false, className: cn(DISABLED_FILL, "max-sm:order-3 max-sm:col-span-2 max-sm:h-11") }}
      phoneBar
      headingAside={
        <div role="group" aria-label={NR_REVIEW.VIEW_ARIA}>
          <Segmented<"pretty" | "yaml" | "json"> size="compact" value={view} options={[...VIEWS]} onChange={(v) => go({ screen: "review", view: v }, { replace: true })} />
        </div>
      }
      footerExtras={{
        start: (
          <>
            <Button type="button" variant="outline" onClick={() => openSave("template")}>{NR_REVIEW.SAVE_TEMPLATE}</Button>
            <Button type="button" variant="outline" onClick={() => openSave("policy")}>{NR_REVIEW.SAVE_POLICY}</Button>
          </>
        ),
        end: (
          <>
            {!open && !issues.length && <Button type="button" variant="outline" className="max-sm:order-1 max-sm:col-start-1" onClick={checkAgain}>{NR_REVIEW.CHECK_AGAIN}</Button>}
            {/* Below sm the two Save actions live here; from sm up they are the footer's own buttons. */}
            <DropdownMenu>
              <DropdownMenuTrigger asChild>
                <Button type="button" variant="outline" aria-label={NR_REVIEW.MORE_ARIA} className="order-2 col-start-2 sm:hidden">
                  <MoreHorizontal className="size-4" aria-hidden /> {NR_REVIEW.MORE}
                </Button>
              </DropdownMenuTrigger>
              <DropdownMenuContent align="end">
                <DropdownMenuItem onClick={() => openSave("template")}>{NR_REVIEW.SAVE_TEMPLATE}</DropdownMenuItem>
                <DropdownMenuItem onClick={() => openSave("policy")}>{NR_REVIEW.SAVE_POLICY}</DropdownMenuItem>
              </DropdownMenuContent>
            </DropdownMenu>
          </>
        ),
      }}
    >
      <Notices poolName={pool} slots={pre?.runner_pool_slots} applied={pre?.resources_applied} askedByYou={changedFromOrigin(draft, ["resources"])} warnings={pre?.warnings} warningContext={pre?.warning_context} diskCut={!!pre?.main_drive?.clamped_by && pre.main_drive.clamped_by !== "policy"} wallsLeftOut={wallsLeftOut} />
      <BlockerList blockers={blockers} go={(loc: NewRunLocation) => go(loc)} onCheckAgain={issues.length ? undefined : checkAgain} onAdd={setAddingSecret} />
      {addingSecret && (
        <AddTokenDialog
          item={addingSecret}
          onClose={() => setAddingSecret(null)}
          // As Tools does after a door: say it, then ask the server again. The blocker goes when the server reports the secret stored.
          onSaved={() => { announce(NR_TOOLS.TOKEN_SAVED); setAddingSecret(null); checkAgain(); }}
        />
      )}
      {view === "pretty" ? (
        <div className="grid items-start gap-3.5 lg:grid-cols-[minmax(0,1.7fr)_minmax(280px,0.8fr)]">
          {/* #3044: Review's groups are flat rows in the one section card the guided steps use. A blocker that names a
              group is red; a stale result alone blames none, so it is amber (#3043). */}
          <StepSection
            title={NR_REVIEW.SETUP_TITLE}
            description={setupTitle(draft.intent)}
            marker={<SetupMark tone={hard.length ? "danger" : blockers.length ? "warning" : "success"} />}
            className="overflow-hidden"
            bodyClassName="p-0"
          >
            <BlockedGroups.Provider value={groupsBlockedBy(hard)}>
              <RunGroup {...group} docRows={docRows.run} />
              <WhereGroup {...group} docRows={docRows.where} />
              <WorkGroup {...group} docRows={docRows.work} />
              <ToolsGroup {...group} docRows={docRows.tools} />
              <AccessGroup {...group} docRows={docRows.access} onLeftOut={setWallsLeftOut} />
              <LimitsGroup {...group} docRows={docRows.limits} />
            </BlockedGroups.Provider>
          </StepSection>
          <EffectivePolicyCard draft={draft} pre={pre} preview={checks.preview.result} />
        </div>
      ) : (
        <RawView intent={draft.intent} format={view} go={go} />
      )}
      {saveKind && publish && (
        <SaveAsDialog
          open
          onOpenChange={(o) => !o && closeSave()}
          kind={saveKind}
          intent={draft.intent}
          hasBlockers={hard.length > 0}
          publish={publish}
          ownDriveIds={ownDriveIds}
          onSaved={saved}
        />
      )}
    </StepShell>
  );
}
