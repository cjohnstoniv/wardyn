/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Settings card: the organisation's assistant block (types.AssistantSettings): which purposes are on, which
// model providers on the list above may serve them, whether run history goes as host counts, and which
// record kinds are never sent. Wardyn holds no model and no credential for it; each person connects their own.
//
// Saved through PUT /assistant/settings with the GET's ETag as If-Match. A save that only narrows applies at
// once (200). One that widens anything is held for a second super admin (202): the card keeps showing what is
// stored and says the change waits, with a link to it. Super-admin only, by being mounted from
// AdminSettingsScreen alone.
import * as React from "react";
import { Loader2, X } from "lucide-react";
import { toast } from "sonner";
import { assistant, type AssistantSettingsSnapshot } from "../../../lib/api/assistant";
import { HttpError } from "../../../lib/api/core";
import { modelProviders, type ModelProvider } from "../../../lib/api/model-providers";
import { getErrorMessage } from "../../../lib/format";
import { CHANGES } from "../../../lib/governance-copy";
import { MODEL_PROVIDERS } from "../../../lib/model-providers-copy";
import type { GovernanceChange } from "../../../lib/types";
import type { AssistantProviderMark, AssistantSettings } from "../../../lib/types/site";
import { Button } from "../../ui/button";
import { Checkbox } from "../../ui/checkbox";
import { DropdownMenu, DropdownMenuContent, DropdownMenuItem, DropdownMenuTrigger } from "../../ui/dropdown-menu";
import { Input } from "../../ui/input";
import { CollapsibleCard } from "../../wardyn/collapsible-card";
import { ASSISTANT as T } from "../../wardyn/copy/assistant";
import { COMPONENTS_ADMIN } from "../../wardyn/copy/components-admin";
import { INLINE_ACTIONS } from "../../wardyn/form-actions";
import { Switch } from "../../wardyn/form-primitives";
import { Chip } from "../../wardyn/primitives";
import { SubmittedNote } from "../governance/submitted-note";
import { ErrorState, failureSentence } from "../../wardyn/states";

// The one harness that serves the assistant (types.AssistantHarness).
const ASSISTANT_HARNESS = "claude-code";

// The record kinds a person may keep from the assistant: every kind of governance change the console names.
export const KEEPABLE_KINDS = Object.keys(CHANGES.KIND);
const kindLabel = (k: string) => CHANGES.KIND[k] ?? k;

type Mark = { compose: boolean; review: boolean; models: string };
type Draft = { compose: boolean; review: boolean; runHistory: boolean; kinds: string[]; marks: Record<string, Mark> };

const modelList = (text: string) =>
  text
    .split(",")
    .map((m) => m.trim())
    .filter(Boolean);

function draftOf(s?: AssistantSettings): Draft {
  const marks: Record<string, Mark> = {};
  for (const m of s?.providers ?? []) marks[m.provider_id] = { compose: m.compose, review: m.review, models: m.models.join(", ") };
  return { compose: !!s?.compose, review: !!s?.review, runHistory: !!s?.run_history_counts, kinds: [...(s?.never_sent_kinds ?? [])], marks };
}

// The block to send: a provider with neither purpose ticked is no mark, and both lists are sorted as the server stores them.
export function settingsBody(d: Draft): AssistantSettings {
  const providers: AssistantProviderMark[] = Object.entries(d.marks)
    .filter(([, m]) => m.compose || m.review)
    .map(([id, m]) => ({ provider_id: id, compose: m.compose, review: m.review, models: modelList(m.models) }))
    .sort((a, b) => a.provider_id.localeCompare(b.provider_id));
  return { compose: d.compose, review: d.review, run_history_counts: d.runHistory, never_sent_kinds: [...d.kinds].sort(), providers };
}

const isBedrock = (p: ModelProvider) => p.kind === "bedrock_sso" || p.kind === "bedrock_bearer";

// Why a provider cannot serve the assistant, "" when it can. Bedrock is checked first because it is the more
// useful reason: the server refuses a provider without the harness first, but either way the row has no checkboxes.
function cannotServe(p: ModelProvider): string {
  if (isBedrock(p)) return T.BEDROCK;
  if (!p.harnesses?.some((h) => h.harness === ASSISTANT_HARNESS)) return T.CANNOT_SERVE;
  if (p.gateway_identity?.inspection === "gateway") return T.DEFERS_INSPECTION;
  return "";
}

const NO_MARK: Mark = { compose: false, review: false, models: "" };

function ProviderRow({ p, mark, disabled, onChange }: { p: ModelProvider; mark: Mark; disabled: boolean; onChange: (m: Mark) => void }) {
  const name = p.name || p.id;
  const reason = cannotServe(p);
  return (
    <div
      data-testid={`assistant-provider-${p.id}`}
      className="grid grid-cols-[minmax(0,1.4fr)_5rem_5rem_minmax(0,1fr)] items-center gap-2 border-t border-border py-2"
    >
      <div className="min-w-0">
        <p className="text-sm text-foreground">{name}</p>
        <p className="text-meta text-muted-foreground">{MODEL_PROVIDERS.KIND[p.kind]}</p>
      </div>
      {reason ? (
        <p className="col-span-3 text-meta text-muted-foreground">{reason}</p>
      ) : (
        <>
          <Checkbox
            aria-label={`${name}: ${T.COMPOSE}`}
            checked={mark.compose}
            disabled={disabled}
            onCheckedChange={(v) => onChange({ ...mark, compose: v === true })}
          />
          <Checkbox
            aria-label={`${name}: ${T.REVIEW}`}
            checked={mark.review}
            disabled={disabled}
            onCheckedChange={(v) => onChange({ ...mark, review: v === true })}
          />
          <Input
            aria-label={`${name}: ${T.COL_MODELS}`}
            className="h-8 font-mono text-meta"
            value={mark.models}
            disabled={disabled || (!mark.compose && !mark.review)}
            onChange={(e) => onChange({ ...mark, models: e.target.value })}
          />
        </>
      )}
    </div>
  );
}

function PurposeRow({ label, hint, checked, disabled, onChange }: { label: string; hint: string; checked: boolean; disabled: boolean; onChange: (v: boolean) => void }) {
  const hintId = React.useId();
  return (
    <div className="flex items-start gap-3 py-1.5">
      <Switch checked={checked} disabled={disabled} onChange={onChange} label={label} describedBy={hintId} />
      <div>
        <p className="text-sm font-medium text-foreground">{label}</p>
        <p id={hintId} className="text-meta text-muted-foreground">{hint}</p>
      </div>
    </div>
  );
}

function BuildFiles() {
  const groups: [string, string][] = [
    [T.GROUP_CONTAINER, T.FILES_CONTAINER],
    [T.GROUP_TOOLCHAIN, T.FILES_TOOLCHAIN],
    [T.GROUP_MANIFESTS, T.FILES_MANIFESTS],
    [T.GROUP_LOCKFILES, T.FILES_LOCKFILES],
  ];
  return (
    <section className="py-3">
      <h4 className="text-body font-medium text-foreground">{T.BUILD_FILES}</h4>
      <p className="text-meta text-muted-foreground">{T.BUILD_FILES_LEAD}</p>
      <dl className="mt-2 grid grid-cols-[9.5rem_minmax(0,1fr)] gap-x-3 gap-y-1">
        {groups.map(([group, files]) => (
          <React.Fragment key={group}>
            <dt className="text-meta text-muted-foreground">{group}</dt>
            <dd className="break-words font-mono text-meta text-foreground">{files}</dd>
          </React.Fragment>
        ))}
      </dl>
      <p className="mt-2 text-meta text-muted-foreground">{T.BUILD_PROGRAMS}</p>
      <p className="mt-1 text-meta text-muted-foreground">{T.ALSO_SENT}</p>
    </section>
  );
}

function NeverSent({ kinds, disabled, onChange }: { kinds: string[]; disabled: boolean; onChange: (k: string[]) => void }) {
  const choices = KEEPABLE_KINDS.filter((k) => !kinds.includes(k));
  return (
    <section className="py-3">
      <h4 className="text-body font-medium text-foreground">{T.NEVER_SENT}</h4>
      <div className="mt-2 flex flex-wrap items-center gap-2">
        {kinds.map((k) => (
          <Chip key={k}>
            {kindLabel(k)}
            <button type="button" aria-label={T.REMOVE_KIND(kindLabel(k))} disabled={disabled} onClick={() => onChange(kinds.filter((x) => x !== k))}>
              <X aria-hidden className="ml-1 size-3" />
            </button>
          </Chip>
        ))}
        <DropdownMenu>
          <DropdownMenuTrigger asChild>
            <Button variant="outline" size="sm" disabled={disabled || choices.length === 0}>
              {T.ADD_KIND}
            </Button>
          </DropdownMenuTrigger>
          <DropdownMenuContent align="start">
            {choices.map((k) => (
              <DropdownMenuItem key={k} onSelect={() => onChange([...kinds, k])}>
                {kindLabel(k)}
              </DropdownMenuItem>
            ))}
          </DropdownMenuContent>
        </DropdownMenu>
      </div>
    </section>
  );
}

const summaryOf = (s: AssistantSettings) => [s.compose && T.COMPOSE, s.review && T.REVIEW].filter(Boolean).join(" · ") || MODEL_PROVIDERS.CHIP_OFF;

type Loaded = { snapshot: AssistantSettingsSnapshot; providers: ModelProvider[] };

export function AssistantSettingsCard() {
  const [loaded, setLoaded] = React.useState<Loaded | null>(null);
  const [draft, setDraft] = React.useState<Draft>(draftOf());
  const [failed, setFailed] = React.useState<string | undefined | false>(false);
  const [saving, setSaving] = React.useState(false);
  const [refused, setRefused] = React.useState<string | null>(null);
  const [savedElsewhere, setSavedElsewhere] = React.useState(false);
  const [heldChange, setHeldChange] = React.useState<GovernanceChange | null>(null);

  const load = React.useCallback(
    () =>
      Promise.all([assistant.getAssistantSettings(), modelProviders.getModelProviders()])
        .then(([snapshot, list]) => {
          setLoaded({ snapshot, providers: list.providers.providers ?? [] });
          setDraft(draftOf(snapshot.settings));
          setFailed(false);
        })
        .catch((e) => setFailed(failureSentence(e))),
    [],
  );
  React.useEffect(() => {
    void load();
  }, [load]);

  const saved = loaded?.snapshot.settings;
  const dirty = !!saved && JSON.stringify(settingsBody(draft)) !== JSON.stringify(settingsBody(draftOf(saved)));

  const save = async () => {
    if (!loaded) return;
    setSaving(true);
    setRefused(null);
    setSavedElsewhere(false);
    setHeldChange(null);
    try {
      const result = await assistant.putAssistantSettings(settingsBody(draft), loaded.snapshot.etag);
      if ("held" in result) {
        // Nothing is applied: what is stored stays on screen until the change is approved.
        setDraft(draftOf(loaded.snapshot.settings));
        setHeldChange(result.held);
      } else {
        setLoaded({ ...loaded, snapshot: result.applied });
        setDraft(draftOf(result.applied.settings));
        toast.success(T.SAVED_TOAST);
      }
    } catch (e) {
      if (e instanceof HttpError && e.status === 412) {
        await load();
        setSavedElsewhere(true);
      } else if (e instanceof HttpError && e.status === 400) {
        setRefused(e.message);
      } else {
        toast.error(T.SAVE_FAILED, { description: getErrorMessage(e) });
      }
    } finally {
      setSaving(false);
    }
  };

  const setMark = (id: string, m: Mark) => setDraft((d) => ({ ...d, marks: { ...d.marks, [id]: m } }));

  return (
    <CollapsibleCard title={T.TITLE} summary={saved ? summaryOf(saved) : undefined} testId="assistant-settings-card">
      <p className="text-body leading-snug text-muted-foreground">{T.LEAD}</p>

      {!loaded && failed === false && <Loader2 role="status" aria-label="Loading" className="mt-4 size-4 animate-spin text-muted-foreground" />}
      {failed !== false && <ErrorState size="compact" title={T.LOAD_FAILED} message={failed} onRetry={() => void load()} />}

      {loaded && (
        <div className="mt-3 divide-y divide-border">
          <section className="pb-3">
            <h4 className="text-body font-medium text-foreground">{T.PURPOSES}</h4>
            <PurposeRow label={T.COMPOSE} hint={T.COMPOSE_HINT} checked={draft.compose} disabled={saving} onChange={(v) => setDraft({ ...draft, compose: v })} />
            <PurposeRow label={T.REVIEW} hint={T.REVIEW_HINT} checked={draft.review} disabled={saving} onChange={(v) => setDraft({ ...draft, review: v })} />
            <p className="mt-1 text-meta text-muted-foreground">{T.PURPOSES_OFF}</p>
          </section>

          <section className="py-3">
            <h4 className="text-body font-medium text-foreground">{T.MAY_SERVE}</h4>
            <div className="mt-2 grid grid-cols-[minmax(0,1.4fr)_5rem_5rem_minmax(0,1fr)] gap-2 text-meta text-muted-foreground">
              <span>{T.COL_PROVIDER}</span>
              <span>{T.COMPOSE}</span>
              <span>{T.REVIEW}</span>
              <span>{T.COL_MODELS}</span>
            </div>
            {loaded.providers.length === 0 && <p className="border-t border-border py-2 text-meta text-muted-foreground">{MODEL_PROVIDERS.EMPTY_TITLE}</p>}
            {loaded.providers.map((p) => (
              <ProviderRow key={p.id} p={p} mark={draft.marks[p.id] ?? NO_MARK} disabled={saving} onChange={(m) => setMark(p.id, m)} />
            ))}
            <p className="mt-2 text-meta text-muted-foreground">{T.LOCAL_NOTE}</p>
            <p className="mt-1 text-meta text-muted-foreground">{T.UNITS_DENY}</p>
          </section>

          <section className="py-3">
            <PurposeRow label={T.RUN_HISTORY} hint={T.RUN_HISTORY_HINT} checked={draft.runHistory} disabled={saving} onChange={(v) => setDraft({ ...draft, runHistory: v })} />
          </section>

          <BuildFiles />
          <NeverSent kinds={draft.kinds} disabled={saving} onChange={(kinds) => setDraft({ ...draft, kinds })} />

          <section className="py-3">
            <Chip tone="success">{T.ALWAYS_ON}</Chip>
            <p className="mt-2 text-meta text-muted-foreground">{T.REDACTION}</p>
          </section>

          {savedElsewhere && (
            <p role="status" className="my-3 rounded-md border border-warning/30 bg-warning-subtle px-3 py-2 text-xs text-warning">
              {COMPONENTS_ADMIN.SETTINGS_SAVED_ELSEWHERE}
            </p>
          )}
          {refused && (
            <p role="alert" className="py-3 text-xs text-danger">
              {refused}
            </p>
          )}
          <div className="py-3">
            <div className={INLINE_ACTIONS}>
              <Button onClick={() => void save()} disabled={saving || !dirty}>
                {saving ? <Loader2 className="size-4 animate-spin" /> : null}
                {T.SAVE}
              </Button>
            </div>
            {heldChange && (
              <div className="mt-2" data-testid="assistant-settings-held">
                <SubmittedNote change={heldChange} />
              </div>
            )}
          </div>
        </div>
      )}
    </CollapsibleCard>
  );
}
