/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Governance › profile, the Deciders section: who besides security admins decides each kind of hold for
// runs in the profile. It saves through its own route (PUT /governance/units/{id}/deciders), so it has its
// own Save and Discard beside the profile's. Every rule the server refuses is stopped in the editor box
// (decider-rule-editor.tsx); a refusal that gets through names its rule and opens it.
import * as React from "react";
import { Layers, Lock, Plus, ScrollText, User, Users } from "lucide-react";
import { toast } from "sonner";
import { HttpError, PendingChangeError } from "../../../lib/api/core";
import { putUnitDeciders, type DeciderKind, type DeciderRule, type GovernanceDeciders } from "../../../lib/api/deciders";
import type { GovernanceUnit } from "../../../lib/api/governance";
import { formatDuration } from "../../../lib/format";
import { goDurationMs } from "../../../lib/go-duration";
import { useUnsavedGuard } from "../../../lib/use-unsaved-guard";
import { useUnitAdminOf } from "../../wardyn/operator-context";
import { Button } from "../../ui/button";
import { DECIDERS, DECIDER_KIND_LABEL } from "../../wardyn/copy/deciders";
import { Chip } from "../../wardyn/primitives";
import { EmptyState } from "../../wardyn/states";
import { DeciderRuleEditor } from "./decider-rule-editor";
import { lockedBy, ruleProblems, whoPhrase } from "./decider-rules";
import { Note } from "./display";
import { SubmittedNote } from "./submitted-note";

const KIND_ORDER = Object.keys(DECIDER_KIND_LABEL) as DeciderKind[];
const SCOPE_LABEL = { once: DECIDERS.SCOPE_ONCE, until: DECIDERS.SCOPE_UNTIL, run: DECIDERS.SCOPE_RUN } as const;

function WhoChip({ entry }: { entry: string }) {
  const icon = "size-3";
  if (entry === "owner") return <Chip><User className={icon} aria-hidden="true" />{DECIDERS.WHO_OWNER}</Chip>;
  if (entry === "unit_admins") return <Chip><Layers className={icon} aria-hidden="true" />{DECIDERS.WHO_ADMINS}</Chip>;
  if (entry.startsWith("group:")) return <Chip><Users className={icon} aria-hidden="true" />{DECIDERS.WHO_GROUP(entry.slice("group:".length))}</Chip>;
  return <Chip>{entry}</Chip>;
}

// "A second person decides. · At most: Once", the facts a rule states beyond its kinds and who.
function factLine(rule: DeciderRule): string {
  const parts: string[] = [];
  if (rule.second_human) parts.push(DECIDERS.FACT_SECOND);
  if (rule.max_scope) parts.push(DECIDERS.FACT_AT_MOST(SCOPE_LABEL[rule.max_scope]));
  const ms = rule.max_elevation ? goDurationMs(rule.max_elevation) : NaN;
  if (ms >= 0) parts.push(DECIDERS.FACT_UP_TO(formatDuration(ms, "words")));
  if (rule.eligible_requesters?.length) parts.push(DECIDERS.FACT_ASK(rule.eligible_requesters.map((g) => DECIDERS.WHO_GROUP(g.replace(/^group:/, ""))).join(", ")));
  return parts.join(" · ");
}

function RuleRow({ kinds, who, facts, met, children }: { kinds: DeciderKind[]; who: string[]; facts?: string; met?: React.ReactNode; children?: React.ReactNode }) {
  return (
    <li className="flex flex-wrap items-start justify-between gap-x-4 gap-y-2 border-b border-border px-4 py-3 last:border-b-0">
      <div className="flex min-w-0 flex-col gap-2">
        <div className="flex flex-wrap items-center gap-2">
          {kinds.map((k) => (
            <Chip key={k}>{DECIDER_KIND_LABEL[k]}</Chip>
          ))}
        </div>
        <div className="flex flex-wrap items-center gap-2 text-xs">
          <span className="font-medium text-foreground">{DECIDERS.DECIDED_BY}</span>
          {who.map((w) => (
            <WhoChip key={w} entry={w} />
          ))}
        </div>
        {facts && <p className="text-xs text-muted-foreground">{facts}</p>}
        {met}
      </div>
      {children && <div className="flex shrink-0 items-center gap-2">{children}</div>}
    </li>
  );
}

type Refusal = { body: string; mono: boolean; index: number | null };

function RefusalNote({ body, mono }: { body: string; mono: boolean }) {
  return (
    <Note tone="red" role="alert">
      <b className="font-semibold">{DECIDERS.REFUSED_TITLE}</b>
      <span className={mono ? "break-words font-mono" : undefined}>{body}</span>
    </Note>
  );
}

export function UnitDeciders({
  profile,
  profiles = [],
  deciders = [],
  supported = false,
  unitNames = {},
  disabled,
  onChanged = () => {},
}: {
  /** The profile being edited, or null for one not saved yet. */
  profile: GovernanceUnit | null;
  profiles?: GovernanceUnit[];
  deciders?: GovernanceDeciders[];
  /** False on a server that predates deciders. */
  supported?: boolean;
  unitNames?: Record<string, string>;
  disabled: boolean;
  /** The save landed or was held: the screen reloads its snapshot. */
  onChanged?: () => void;
}) {
  const entry = profile ? deciders.find((d) => d.unit_id === profile.id) : undefined;
  const stored = entry?.rules ?? [];
  const storedKey = JSON.stringify(stored);
  const [draft, setDraft] = React.useState<DeciderRule[]>(stored);
  const [editing, setEditing] = React.useState<number | null>(null);
  const [refusal, setRefusal] = React.useState<Refusal | null>(null);
  const [held, setHeld] = React.useState(false);
  const [saving, setSaving] = React.useState(false);
  // A reload that brings different stored rules starts the draft over from them.
  React.useEffect(() => {
    setDraft(JSON.parse(storedKey) as DeciderRule[]);
    setEditing(null);
    setRefusal(null);
    setHeld(false);
  }, [storedKey, profile?.id]);

  const dirty = JSON.stringify(draft) !== storedKey;
  useUnsavedGuard("unit-deciders", dirty, () => JSON.stringify(draft));

  const locker = profile ? lockedBy(profile, profiles) : null;
  const canEdit = supported && profile !== null && !disabled && !locker;
  const from = entry?.from ?? {};
  // A unit admin of this very profile sees it read only, and is told why.
  const adminsThis = useUnitAdminOf().some((u) => u.unit_id === profile?.id);

  // The kinds this profile does not name, by the rule above that decides each: one row per rule above.
  const inherited = new Map<string, { unitId: string; rule: DeciderRule | undefined; kinds: DeciderKind[] }>();
  for (const k of KIND_ORDER) {
    const unitId = from[k];
    if (!unitId || unitId === profile?.id) continue;
    const rules = deciders.find((d) => d.unit_id === unitId)?.rules ?? [];
    const at = rules.findIndex((r) => r.kinds.includes(k));
    const key = `${unitId}#${at}`;
    const row = inherited.get(key) ?? { unitId, rule: rules[at], kinds: [] };
    row.kinds.push(k);
    inherited.set(key, row);
  }
  const rows = [...inherited.values()];
  const firstSource = rows[0] ? unitNames[rows[0].unitId] : undefined;

  const save = async () => {
    if (!profile) return;
    setSaving(true);
    setRefusal(null);
    setHeld(false);
    try {
      await putUnitDeciders(profile.id, draft);
      toast.success(DECIDERS.SAVED);
      onChanged();
    } catch (e) {
      if (e instanceof PendingChangeError) {
        setHeld(true);
        onChanged();
      } else if (e instanceof HttpError) {
        const named = e.status === 409 ? null : /rules\[(\d+)\]/.exec(e.message);
        const at = named ? Number(named[1]) : null;
        const index = at !== null && at < draft.length ? at : null;
        setRefusal({ body: e.message, mono: true, index });
        if (index !== null) setEditing(index);
      } else {
        setRefusal({ body: DECIDERS.TRANSIT_BODY, mono: false, index: null });
      }
    } finally {
      setSaving(false);
    }
  };

  const add = () => {
    setEditing(draft.length);
    setDraft([...draft, { kinds: [], who: [] }]);
  };
  const discard = () => {
    setDraft(stored);
    setEditing(null);
    setRefusal(null);
    setHeld(false);
  };
  // What the rules above leave for each kind this rule names, as the server resolved it; stale once the draft differs.
  const metLines = (rule: DeciderRule): React.ReactNode => {
    if (dirty) return null;
    const lines = new Map<string, DeciderKind[]>();
    for (const k of rule.kinds) {
      if ((entry?.bounded_by?.[k]?.length ?? 0) < 2 || !entry?.effective?.[k]) continue;
      const who = entry.effective[k].who;
      const text = who.length ? DECIDERS.MET(whoPhrase(who)) : DECIDERS.MET_NONE;
      lines.set(text, [...(lines.get(text) ?? []), k]);
    }
    return [...lines].map(([text, kinds]) => (
      <p key={text} className="text-xs text-muted-foreground">
        {lines.size > 1 ? `${kinds.map((k) => DECIDER_KIND_LABEL[k]).join(", ")}: ${text}` : text}
      </p>
    ));
  };
  const invalid = draft.some((r) => r.kinds.length === 0 || ruleProblems(r).length > 0);
  const footerNote = refusal && (refusal.index === null || editing !== refusal.index) ? <RefusalNote body={refusal.body} mono={refusal.mono} /> : null;
  const showEmpty = draft.length === 0 && rows.length === 0 && stored.length === 0;

  return (
    <section id="governance-section-deciders" className="mt-6 scroll-mt-4" data-testid="governance-profile-deciders">
      <div className="flex flex-wrap items-center gap-2">
        <h4 className="text-body font-medium text-foreground">{DECIDERS.TITLE}</h4>
        {locker && (
          <Chip>
            <Lock className="size-3" aria-hidden="true" />
            {DECIDERS.LOCKED_BY(locker.name)}
          </Chip>
        )}
      </div>
      <p className="mt-0.5 max-w-[62ch] text-xs text-muted-foreground">{DECIDERS.LEAD}</p>
      <p className="mt-0.5 text-xs text-muted-foreground">{DECIDERS.HELP}</p>
      {disabled && profile && adminsThis && <p className="mt-0.5 max-w-[62ch] text-xs text-muted-foreground">{DECIDERS.OWN_UNIT(profile.name)}</p>}
      {locker && <p className="mt-2 max-w-[62ch] text-xs text-muted-foreground">{DECIDERS.LOCKED_EXPLAIN(locker.name)}</p>}

      {!supported ? (
        <p className="mt-3 text-xs text-muted-foreground">{DECIDERS.OLDER_SERVER}</p>
      ) : profile === null ? (
        <p className="mt-3 text-xs text-muted-foreground">{DECIDERS.NEW_PROFILE}</p>
      ) : (
        <>
          {showEmpty ? (
            <div className="mt-3">
              <EmptyState
                icon={ScrollText}
                size="compact"
                title={DECIDERS.EMPTY_TITLE}
                description={DECIDERS.EMPTY_BODY}
                action={
                  canEdit ? (
                    <Button type="button" variant="outline" size="sm" onClick={add}>
                      <Plus className="size-4" aria-hidden="true" />
                      {DECIDERS.ADD}
                    </Button>
                  ) : undefined
                }
              />
            </div>
          ) : (
            (draft.length > 0 || rows.length > 0) && (
              <ul className="mt-3 rounded-lg border border-border">
                {draft.map((rule, i) =>
                  canEdit && editing === i ? (
                    <li key={i} className="border-b border-border p-3 last:border-b-0">
                      <DeciderRuleEditor
                        index={i}
                        rule={rule}
                        draft={draft}
                        refusal={refusal?.index === i ? <RefusalNote body={refusal.body} mono={refusal.mono} /> : undefined}
                        onChange={(next) => setDraft(draft.map((r, j) => (j === i ? next : r)))}
                        onDone={() => setEditing(null)}
                        onRemove={() => {
                          setDraft(draft.filter((_, j) => j !== i));
                          setEditing(null);
                        }}
                      />
                    </li>
                  ) : (
                    <RuleRow key={i} kinds={rule.kinds} who={rule.who} facts={factLine(rule)} met={metLines(rule)}>
                      {canEdit && (
                        <>
                          <Button type="button" variant="outline" size="sm" onClick={() => setEditing(i)}>
                            {DECIDERS.EDIT}
                          </Button>
                          <Button type="button" variant="ghost" size="sm" onClick={() => setDraft(draft.filter((_, j) => j !== i))}>
                            {DECIDERS.REMOVE}
                          </Button>
                        </>
                      )}
                    </RuleRow>
                  ),
                )}
                {rows.map((r) => (
                  <RuleRow key={`${r.unitId}#${r.kinds[0]}`} kinds={r.kinds} who={r.rule?.who ?? []} facts={r.rule ? factLine(r.rule) : undefined}>
                    <span className="text-xs text-muted-foreground">{unitNames[r.unitId] ? DECIDERS.FROM(unitNames[r.unitId]) : DECIDERS.FROM_ABOVE}</span>
                  </RuleRow>
                ))}
              </ul>
            )
          )}

          {canEdit && draft.length === 0 && rows.length > 0 && (
            <div className="mt-3 flex flex-col items-start gap-1">
              <Button type="button" variant="outline" size="sm" onClick={add}>
                {DECIDERS.SET_HERE}
              </Button>
              {firstSource && <p className="text-xs text-muted-foreground">{DECIDERS.INHERITED_HELPER(firstSource)}</p>}
            </div>
          )}

          {canEdit && (draft.length > 0 || stored.length > 0) && (
            <div className="mt-3 flex flex-col items-start gap-2">
              {draft.length > 0 && (
                <Button type="button" variant="outline" size="sm" onClick={add}>
                  <Plus className="size-4" aria-hidden="true" />
                  {DECIDERS.ADD}
                </Button>
              )}
              <p className="text-xs text-muted-foreground">{DECIDERS.ALWAYS_NOTE}</p>
              <div className="flex items-center gap-2">
                <Button type="button" size="sm" disabled={!dirty || invalid || saving} onClick={() => void save()}>
                  {DECIDERS.SAVE}
                </Button>
                <Button type="button" variant="outline" size="sm" disabled={!dirty || saving} onClick={discard}>
                  {DECIDERS.DISCARD}
                </Button>
              </div>
            </div>
          )}
          {footerNote}
          {held && <SubmittedNote />}
        </>
      )}
    </section>
  );
}
