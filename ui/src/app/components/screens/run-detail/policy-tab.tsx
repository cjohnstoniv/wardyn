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
import { asFirstUseMode } from "../../../lib/types";
import type { RunDetail, RunPolicyChange, RunPolicySource, RunPolicyView } from "../../../lib/types";
import { adoCapName } from "../../../lib/ado-access-copy";
import { CAPABILITY, POLICY_UI_APPS } from "../../wardyn/copy";
import { CC_META } from "../../wardyn/cc-meta";
import { CopyButton } from "../../wardyn/copy-button";
import { toYaml, YamlBlock } from "../../wardyn/code-block";
import { usePrincipal } from "../../wardyn/operator-context";
import { lifecycleSummary, toolRulesSummary } from "../../wardyn/policy-panel";
import { Chip } from "../../wardyn/primitives";
import { ErrorState } from "../../wardyn/states";
import { cn } from "../../ui/utils";
import { CHANGE_HEADING, POLICY_TAB, SUMMARY, type PlainCause } from "./policy-tab-copy";

const REDACTED = "<redacted>";

type Load = { status: "loading" } | { status: "error" } | { status: "ready"; view: RunPolicyView };

export function PolicyTab({ run }: { run: RunDetail }) {
  const principal = usePrincipal();
  const [load, setLoad] = React.useState<Load>({ status: "loading" });
  const [attempt, setAttempt] = React.useState(0);
  const [mode, setMode] = React.useState<"summary" | "yaml">("summary");

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
  mode: "summary" | "yaml";
  onMode: (m: "summary" | "yaml") => void;
}) {
  const now = view.stored_policy_now;
  const banner =
    now?.state === "changed" ? POLICY_TAB.changedSince : now?.state === "updated" ? POLICY_TAB.updatedSince : null;
  const groups = groupChanges(view.changes, ownRun, run.created_by);
  const yaml = React.useMemo(() => toYaml(spec), [spec]);
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

      <div className="mt-5 flex flex-wrap items-center gap-2.5">
        <div className="inline-flex overflow-hidden rounded-lg border border-border">
          {(["summary", "yaml"] as const).map((m) => (
            <button
              key={m}
              type="button"
              aria-pressed={mode === m}
              onClick={() => onMode(m)}
              className={cn(
                "px-3 py-1 text-xs",
                mode === m ? "bg-muted font-semibold text-foreground" : "bg-card text-muted-foreground hover:text-foreground",
              )}
            >
              {m === "summary" ? POLICY_TAB.viewSummary : POLICY_TAB.viewYaml}
            </button>
          ))}
        </div>
        <CopyButton
          text={yaml}
          label={POLICY_TAB.copyYaml}
          className="ml-auto gap-1.5 rounded-lg border border-border bg-card px-2.5 py-1 text-xs font-medium hover:bg-muted"
        >
          {POLICY_TAB.copyYaml}
        </CopyButton>
      </div>

      {mode === "summary" ? (
        <PolicySummary spec={spec} run={run} marks={changeMarks(view.changes)} />
      ) : (
        <div className="mt-2.5">
          {view.redacted && <p className="mb-2 text-xs text-muted-foreground">{POLICY_TAB.redacted}</p>}
          <YamlBlock value={spec} />
        </div>
      )}
      <p className="mt-4 max-w-[92ch] border-t border-dashed border-border pt-2.5 text-xs text-muted-foreground">
        {POLICY_TAB.scope}
      </p>
    </>
  );
}

/* ---------- changes ---------- */

type Mark = "added" | "removed";
type Group = { key: string; heading: string; detail: string[]; entries: { text: string; mark?: Mark }[] };

function MarkChip({ mark }: { mark: Mark }) {
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

function groupChanges(changes: RunPolicyChange[], ownRun: boolean, person: string): Group[] {
  const groups = new Map<string, Group>();
  for (const c of changes) {
    const { key, heading } = headingFor(c, ownRun, person);
    const g = groups.get(key) ?? { key, heading, detail: [], entries: [] };
    groups.set(key, g);
    // The heading of a restart or a disk change already says what happened.
    const plain = c.cause === "restart" || c.cause === "org_disk";
    for (const text of c.added ?? []) g.entries.push({ text: entryText(c.field, text), mark: plain ? undefined : "added" });
    for (const text of c.removed ?? []) g.entries.push({ text: entryText(c.field, text), mark: plain ? undefined : "removed" });
    g.detail.push(...(c.detail ?? []));
  }
  return [...groups.values()];
}

// A bare number under disk_mib is a size, and would read as nothing without its unit.
function entryText(field: string, text: string): string {
  return field === "disk_mib" && /^\d+$/.test(text) ? SUMMARY.mibValue(Number(text)) : text;
}

// Which spec entries a change names, so the Summary can flag them.
function changeMarks(changes: RunPolicyChange[]): (field: string, entry: string) => Mark | undefined {
  const marks = new Map<string, Mark>();
  for (const c of changes) {
    for (const e of c.removed ?? []) marks.set(`${c.field}\0${e}`, "removed");
    for (const e of c.added ?? []) marks.set(`${c.field}\0${e}`, "added");
  }
  return (field, entry) => marks.get(`${field}\0${entry}`);
}

/* ---------- summary ---------- */

type Marks = ReturnType<typeof changeMarks>;

function Section({ title, children }: { title: string; children: React.ReactNode }) {
  return (
    <section className="min-w-0 rounded-lg border border-border px-3 py-2.5">
      <h4 className="mb-1.5 text-body font-semibold">{title}</h4>
      <dl className="grid grid-cols-1 gap-x-3 gap-y-1 text-xs sm:grid-cols-[minmax(110px,max-content)_minmax(0,1fr)]">
        {children}
      </dl>
    </section>
  );
}

function Row({ label, children }: { label: string; children: React.ReactNode }) {
  return (
    <>
      <dt className="mt-1 text-muted-foreground sm:mt-0">{label}</dt>
      <dd className="min-w-0">{children}</dd>
    </>
  );
}

function Item({ children, mark }: { children: React.ReactNode; mark?: Mark }) {
  return (
    <div className="flex flex-wrap items-center gap-1.5 [&+&]:mt-0.5">
      {children}
      {mark && <MarkChip mark={mark} />}
    </div>
  );
}

const Mono = ({ children }: { children: React.ReactNode }) => <span className="break-all font-mono">{children}</span>;

function List({ items, field, marks }: { items: string[]; field: string; marks: Marks }) {
  if (items.length === 0) return <>{SUMMARY.none}</>;
  return (
    <>
      {items.map((h) => (
        <Item key={h} mark={marks(field, h)}>
          <Mono>{h}</Mono>
        </Item>
      ))}
    </>
  );
}

function otherHostText(spec: NonNullable<RunPolicyView["spec"]>): string {
  const mode = asFirstUseMode(spec.first_use_approval);
  if (mode === "wait_for_review") return SUMMARY.held(spec.first_use_hold_seconds || 30);
  return mode === "deny_with_review" ? SUMMARY.refusedThenApproval : SUMMARY.refused;
}

function grantScope(scope: Record<string, unknown> | undefined): string {
  if (Array.isArray(scope?.repos)) return (scope.repos as unknown[]).map(String).join(", ");
  return typeof scope?.host === "string" ? scope.host : "";
}

function cpuText(millis?: number): string {
  return millis ? SUMMARY.cpuValue(String(Number((millis / 1000).toFixed(2)))) : SUMMARY.standardLimit;
}

const mib = (n?: number) => (n ? SUMMARY.mibValue(n) : SUMMARY.standardLimit);

function PolicySummary({ spec, run, marks }: { spec: NonNullable<RunPolicyView["spec"]>; run: RunDetail; marks: Marks }) {
  const mounts = spec.workspace_mounts ?? [];
  const repos = spec.workspace_repos ?? [];
  const grants = spec.eligible_grants ?? [];
  const apps = spec.ui_apps ?? [];
  const deny = spec.push_rules?.deny_paths ?? [];
  const hold = spec.push_rules?.require_review_paths ?? [];
  const rules = toolRulesSummary(spec);
  const res = spec.resources;
  const ado = spec.azure_devops_capabilities ?? [];
  return (
    <div className="mt-3 grid gap-3 [grid-template-columns:repeat(auto-fit,minmax(min(100%,300px),1fr))]">
      <Section title={SUMMARY.network}>
        <Row label={SUMMARY.allowedHosts}>
          {spec.allow_all_egress ? CAPABILITY.allowAllEgress : <List items={spec.allowed_domains ?? []} field="allowed_domains" marks={marks} />}
        </Row>
        <Row label={SUMMARY.blockedHosts}>
          <List items={spec.denied_domains ?? []} field="denied_domains" marks={marks} />
        </Row>
        <Row label={SUMMARY.otherHost}>{otherHostText(spec)}</Row>
        <Row label={SUMMARY.requestTypes}>
          {spec.allowed_methods?.length ? spec.allowed_methods.join(", ") : SUMMARY.allMethods}
        </Row>
      </Section>

      <Section title={SUMMARY.barrier}>
        <Row label={SUMMARY.minimum}>{CC_META[spec.min_confinement_class]?.label ?? spec.min_confinement_class}</Row>
        <Row label={SUMMARY.used}>{CC_META[run.confinement_class]?.label ?? run.confinement_class}</Row>
      </Section>

      <Section title={SUMMARY.credentials}>
        {grants.length === 0 ? (
          <Row label={SUMMARY.credentials}>{SUMMARY.none}</Row>
        ) : (
          grants.map((g, i) => (
            <Row key={i} label={SUMMARY.grantKinds[g.kind] ?? g.kind}>
              <Item>
                <Mono>{grantScope(g.scope)}</Mono>
                {g.requires_approval && <Chip tone="warning">{SUMMARY.needsApproval}</Chip>}
              </Item>
            </Row>
          ))
        )}
      </Section>

      <Section title={SUMMARY.files}>
        <Row label={SUMMARY.folders}>
          {mounts.length === 0
            ? SUMMARY.none
            : mounts.map((m) => (
                <Item key={m.target} mark={marks("workspace_mounts", m.target)}>
                  {m.source && m.source !== REDACTED ? (
                    <Mono>{m.source}</Mono>
                  ) : (
                    <span
                      title={POLICY_TAB.hiddenTip}
                      className="cursor-help rounded bg-muted px-1.5 text-meta text-muted-foreground"
                    >
                      {POLICY_TAB.hidden}
                    </span>
                  )}
                  <span aria-hidden>→</span>
                  <Mono>{m.target}</Mono>
                  {m.read_only !== false && <Chip tone="neutral">{SUMMARY.readOnly}</Chip>}
                </Item>
              ))}
        </Row>
        <Row label={SUMMARY.repos}>
          {repos.length === 0
            ? SUMMARY.none
            : repos.map((r) => (
                <Item key={`${r.repo}${r.target}`} mark={marks("workspace_repos", r.repo)}>
                  <Mono>{`${r.repo}${r.ref ? ` at ${r.ref}` : ""}${r.target ? ` → ${r.target}` : ""}`}</Mono>
                </Item>
              ))}
        </Row>
      </Section>

      <Section title={SUMMARY.tools}>
        <Row label={SUMMARY.toolRules}>{rules ?? SUMMARY.none}</Row>
        <Row label={SUMMARY.pushes}>{spec.git_push_any_branch ? SUMMARY.anyBranch : SUMMARY.ownBranch}</Row>
        <Row label={SUMMARY.pushDeny}>
          <List items={deny} field="push_rules" marks={marks} />
        </Row>
        <Row label={SUMMARY.pushHold}>
          <List items={hold} field="push_rules" marks={marks} />
        </Row>
      </Section>

      <Section title={SUMMARY.apps}>
        <Row label={POLICY_UI_APPS.label}>
          {apps.length === 0
            ? POLICY_UI_APPS.none
            : apps.map((a) => (
                <Item key={a.name} mark={marks("ui_apps", a.name)}>
                  <Mono>{POLICY_UI_APPS.value(a.name, a.port, a.path || "/")}</Mono>
                </Item>
              ))}
        </Row>
      </Section>

      <Section title={SUMMARY.limits}>
        <Row label={SUMMARY.cpu}>{cpuText(res?.cpu_millis)}</Row>
        <Row label={SUMMARY.memory}>{mib(res?.memory_mib)}</Row>
        <Row label={SUMMARY.processes}>{res?.pids_limit ? String(res.pids_limit) : SUMMARY.standardLimit}</Row>
        <Row label={SUMMARY.disk}>{mib(res?.disk_mib)}</Row>
        <Row label={SUMMARY.idle}>{lifecycleSummary(spec)}</Row>
      </Section>

      <Section title={SUMMARY.traffic}>
        <Row label={SUMMARY.traffic}>
          {spec.llm_inspection?.mode && spec.llm_inspection.mode !== "off" ? SUMMARY.on : SUMMARY.off}
        </Row>
      </Section>

      {ado.length > 0 && (
        <section className="min-w-0 rounded-lg border border-border px-3 py-2.5">
          <h4 className="mb-1.5 text-body font-semibold">{SUMMARY.ado}</h4>
          <ul className="space-y-0.5 text-xs">
            {ado.map((c) => (
              <li key={c}>{adoCapName(c)}</li>
            ))}
          </ul>
        </section>
      )}
    </div>
  );
}
