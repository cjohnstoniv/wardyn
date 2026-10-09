/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// PolicyTab — the run page's Policy tab: the policy a run actually got, where it
// started from, and what launch changed (GET /runs/{id}/policy). Layout and copy
// are the owner-approved mock's (run-policy-view-packet.html); the strings live
// in policy-tab-copy.ts. Radix mounts a TabsContent only while it is open, so
// mounting IS the lazy fetch: nothing is requested until the tab is opened, and
// the read repeats only when the run's state changes while it stays open.
import * as React from "react";
import { Loader2 } from "lucide-react";
import { runs } from "../../../lib/api/runs";
import type {
  ConfinementClass,
  RunDetail,
  RunPolicyChange,
  RunPolicySource,
  RunPolicySpec,
  RunPolicyView,
} from "../../../lib/types";
import { CAPABILITY } from "../../wardyn/copy";
import { CC_META } from "../../wardyn/cc-meta";
import { usePrincipal } from "../../wardyn/operator-context";
import { PolicyDocument, type PolicyView } from "../../wardyn/policy-document/policy-document";
import { lifecycleSummary } from "../../wardyn/policy-document/policy-facts";
import {
  cpuText,
  firstUseText,
  mibText,
  type PolicyChangeMarks,
  type PolicyMark,
} from "../../wardyn/policy-document/policy-summary";
import { Chip } from "../../wardyn/primitives";
import { ErrorState } from "../../wardyn/states";
import { CHANGE_HEADING, POLICY_TAB, SUMMARY, type PlainCause } from "./policy-tab-copy";

type Load = { status: "loading" } | { status: "error" } | { status: "ready"; view: RunPolicyView };

export function PolicyTab({ run }: { run: RunDetail }) {
  const principal = usePrincipal();
  const [load, setLoad] = React.useState<Load>({ status: "loading" });
  const [attempt, setAttempt] = React.useState(0);
  const [mode, setMode] = React.useState<PolicyView>("summary");

  React.useEffect(() => {
    let alive = true;
    // A refetch on a state change keeps the last answer on screen, so the tab
    // does not flash to a spinner while a live run moves between states.
    setLoad((cur) => (cur.status === "ready" ? cur : { status: "loading" }));
    runs
      .getPolicy(run.id)
      .then((view) => alive && setLoad({ status: "ready", view }))
      .catch(() => alive && setLoad({ status: "error" }));
    return () => {
      alive = false;
    };
  }, [run.id, run.state, attempt]);

  if (load.status === "loading") {
    return (
      <div className="flex h-[240px] items-center justify-center rounded-xl border border-border bg-card">
        <Loader2 className="size-5 animate-spin text-muted-foreground" />
      </div>
    );
  }
  if (load.status === "error") {
    return (
      <div className="rounded-xl border border-border bg-card">
        <ErrorState message={POLICY_TAB.loadError} onRetry={() => setAttempt((n) => n + 1)} />
      </div>
    );
  }

  const { view } = load;
  const sourceLine = sourceText(view.source);
  const recorded = view.state === "recorded" && !!view.spec;
  // A run that never reached its sandbox has no policy, so an "unknown" source
  // would only say "Wardyn set this policy" beside a page saying none applied.
  const showSource = recorded || view.source.kind !== "unknown";
  return (
    <div className="max-w-5xl" data-testid="run-policy-tab">
      {recorded && <p className="max-w-[78ch] text-xs text-muted-foreground">{POLICY_TAB.lead}</p>}
      {showSource && <p className="mt-2 text-sm">{sourceLine}</p>}
      {view.source.preset && (
        <p className="mt-0.5 text-xs text-muted-foreground">
          {POLICY_TAB.preset(view.source.preset, view.source.preset_version ?? 0)}
        </p>
      )}
      {view.state === "not_yet" && <p className="mt-2 text-body text-muted-foreground">{POLICY_TAB.notYet}</p>}
      {view.state === "never" && <p className="mt-2 text-body text-muted-foreground">{POLICY_TAB.never}</p>}
      {recorded && (
        <RecordedPolicy
          view={view}
          spec={view.spec!}
          run={run}
          ownRun={!!principal && run.created_by === principal}
          mode={mode}
          onMode={setMode}
        />
      )}
    </div>
  );
}

function sourceText(s: RunPolicySource): string {
  switch (s.kind) {
    case "stored":
      if (s.deleted) return s.name ? POLICY_TAB.sourceStoredDeleted(s.name) : POLICY_TAB.sourceStoredDeletedNoName;
      return s.name ? POLICY_TAB.sourceStored(s.name) : POLICY_TAB.sourceUnknown;
    case "inline":
      return POLICY_TAB.sourceInline;
    case "default":
      return POLICY_TAB.sourceDefault;
    case "profile":
      return s.name ? POLICY_TAB.sourceProfile(s.name) : POLICY_TAB.sourceDefault;
    default:
      return POLICY_TAB.sourceUnknown;
  }
}

function RecordedPolicy({
  view,
  spec,
  run,
  ownRun,
  mode,
  onMode,
}: {
  view: RunPolicyView;
  spec: NonNullable<RunPolicyView["spec"]>;
  run: RunDetail;
  ownRun: boolean;
  mode: PolicyView;
  onMode: (m: PolicyView) => void;
}) {
  const now = view.stored_policy_now;
  const banner =
    now?.state === "changed" ? POLICY_TAB.changedSince : now?.state === "updated" ? POLICY_TAB.updatedSince : null;
  const groups = groupChanges(view.changes, ownRun, run.created_by, spec.first_use_hold_seconds);
  const marks = React.useMemo(() => changeMarks(view.changes), [view.changes]);
  return (
    <>
      {banner && (
        <p
          data-testid="policy-since-banner"
          className="mt-3 max-w-[88ch] rounded-lg border border-l-[3px] border-border border-l-warning bg-warning-subtle px-3 py-2 text-body"
        >
          {banner(now?.name || view.source.name || "")}
        </p>
      )}

      {/* S-13 says the run got the policy exactly as written, which a run from
          before changes were recorded cannot promise. */}
      {(groups.length > 0 || view.complete) && (
        <h3 className="label-eyebrow mb-1.5 mt-5">{POLICY_TAB.changesHeading}</h3>
      )}
      {groups.length === 0 && view.complete && <p className="text-body text-muted-foreground">{POLICY_TAB.nothingChanged}</p>}
      {groups.map((g) => (
        <div key={g.key} className="mt-2 min-w-0 rounded-lg border border-border px-3 py-2">
          <h4 className="text-body font-semibold">{g.heading}</h4>
          <ul className="mt-1 space-y-0.5">
            {g.entries.map((e, i) => (
              <li key={i} className="flex min-w-0 flex-wrap items-center gap-1.5 text-xs">
                <span className="min-w-0 break-all font-mono">{e.text}</span>
                {e.mark && <MarkChip mark={e.mark} />}
              </li>
            ))}
          </ul>
          {g.detail.map((d, i) => (
            <p key={i} className="mt-1.5 break-words font-mono text-meta text-muted-foreground">
              {d}
            </p>
          ))}
        </div>
      ))}
      {!view.complete && <p className="mt-3 text-body text-muted-foreground">{POLICY_TAB.incomplete}</p>}

      {/* The shared read-only document. This run's own facts ride with it: the
          barrier it actually used, what launch added or removed, and whether
          this reader was shown everything. */}
      <PolicyDocument
        className="mt-5"
        spec={spec}
        view={mode}
        onViewChange={onMode}
        redacted={view.redacted}
        marks={marks}
        facts={{ usedClass: run.confinement_class }}
        headingLevel={4}
      />
      <p className="mt-4 max-w-[92ch] border-t border-dashed border-border pt-2.5 text-xs text-muted-foreground">
        {POLICY_TAB.scope}
      </p>
    </>
  );
}

/* ---------- changes ---------- */

type Group = { key: string; heading: string; detail: string[]; entries: { text: string; mark?: PolicyMark }[] };

function MarkChip({ mark }: { mark: PolicyMark }) {
  return mark === "added" ? (
    <Chip tone="info">{POLICY_TAB.chipAdded}</Chip>
  ) : (
    <Chip tone="neutral">{POLICY_TAB.chipRemoved}</Chip>
  );
}

function longDate(iso: string): string {
  return new Date(iso).toLocaleDateString(undefined, { month: "short", day: "numeric", year: "numeric" });
}

// The heading a change sits under, and what separates two headings of one cause
// (a profile's name, a restart's date).
function headingFor(c: RunPolicyChange, ownRun: boolean, person: string): { key: string; heading: string } {
  switch (c.cause) {
    case "limits":
      return { key: "limits", heading: ownRun ? CHANGE_HEADING.limitsOwn : CHANGE_HEADING.limitsOther(person) };
    case "profile":
      if (c.profile) return { key: `profile:${c.profile}`, heading: CHANGE_HEADING.profile(c.profile) };
      break;
    case "restart":
      if (c.at) return { key: `restart:${c.at}`, heading: CHANGE_HEADING.restart(longDate(c.at)) };
      break;
    case "workspace":
    case "source_control":
    case "git_broker":
    case "model_access":
    case "mirror":
    case "org_disk":
      return { key: c.cause, heading: CHANGE_HEADING[c.cause as PlainCause] };
  }
  return { key: "launch", heading: CHANGE_HEADING.launch };
}

function groupChanges(changes: RunPolicyChange[], ownRun: boolean, person: string, holdSeconds?: number): Group[] {
  const groups = new Map<string, Group>();
  for (const c of changes) {
    const { key, heading } = headingFor(c, ownRun, person);
    const g = groups.get(key) ?? { key, heading, detail: [], entries: [] };
    groups.set(key, g);
    // The heading of a restart or a disk change already says what happened.
    const plain = c.cause === "restart" || c.cause === "org_disk";
    for (const text of c.added ?? []) g.entries.push({ text: entryText(c.field, text, holdSeconds), mark: plain ? undefined : "added" });
    for (const text of c.removed ?? []) g.entries.push({ text: entryText(c.field, text, holdSeconds), mark: plain ? undefined : "removed" });
    g.detail.push(...(c.detail ?? []));
  }
  return [...groups.values()];
}

// Single-value fields arrive as one-entry sets of the raw value; each reads in the
// console's own words, as the Summary does, never as a wire code like CC2.
// A first-use change names how long a held connection waits, which is the policy's own number.
function entryText(field: string, text: string, holdSeconds?: number): string {
  const n = Number(text);
  switch (field) {
    case "min_confinement_class":
      return CC_META[text as ConfinementClass]?.label ?? text;
    case "first_use_approval":
      return firstUseText(text, holdSeconds);
    case "first_use_hold_seconds":
      return SUMMARY.held(n);
    case "git_push_any_branch":
      return text === "true" ? SUMMARY.anyBranch : SUMMARY.ownBranch;
    case "allow_all_egress":
      return text === "true" ? CAPABILITY.allowAllEgress : text;
    case "auto_stop_after_sec":
      return lifecycleSummary({ auto_stop_after_sec: n } as RunPolicySpec);
    case "resources.cpu_millis":
      return cpuText(n);
    case "resources.memory_mib":
    case "resources.disk_mib":
      return mibText(n);
    default:
      return text;
  }
}

// Which spec entries a change names, so the Summary can flag them, and which
// host-list entries it took out (those are no longer in the spec to flag).
function changeMarks(changes: RunPolicyChange[]): PolicyChangeMarks {
  const marks = new Map<string, PolicyMark>();
  const removed = new Map<string, string[]>();
  for (const c of changes) {
    for (const e of c.removed ?? []) {
      marks.set(`${c.field}\0${e}`, "removed");
      // A restart's removal happened later, so "Removed at start" would be false for it.
      if (c.cause !== "restart") removed.set(c.field, [...new Set([...(removed.get(c.field) ?? []), e])]);
    }
    for (const e of c.added ?? []) marks.set(`${c.field}\0${e}`, "added");
  }
  return {
    of: (field, entry) => marks.get(`${field}\0${entry}`),
    removed: (field) => removed.get(field) ?? [],
  };
}
