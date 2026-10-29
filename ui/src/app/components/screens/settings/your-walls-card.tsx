/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Settings: the Your walls card (T4-a, owner-approved board R15). One private document per
// person, read and written whole at /me/walls. Every knob only narrows what the organisation allows;
// none the person leaves out is a limit. The card is absent for a caller who is no person (the admin
// token, local mode) and where the store keeps no personal settings (walls.ts's wallsAbsent).
import * as React from "react";
import { Check, X } from "lucide-react";
import { HttpError } from "../../../lib/api/core";
import { walls as api, wallsAbsent, type PersonalWallsSpec, type WithheldGrant } from "../../../lib/api/walls";
import { CC_ORDER, type ConfinementClass, type FirstUseMode } from "../../../lib/types";
import { Button } from "../../ui/button";
import { Checkbox } from "../../ui/checkbox";
import { Input } from "../../ui/input";
import { RadioGroup, RadioGroupItem } from "../../ui/radio-group";
import { cn } from "../../ui/utils";
import { CollapsibleCard } from "../../wardyn/collapsible-card";
import { YOUR_WALLS as T } from "../../wardyn/copy/personal-walls";
import { ConfinementChip, Chip } from "../../wardyn/primitives";
import { Segmented } from "../../wardyn/segmented";
import { TableSkeleton } from "../../wardyn/states";

type Direction = "paste_in" | "copy_out" | "upload" | "download";
const DIRECTIONS: { id: Direction; label: string }[] = [
  { id: "paste_in", label: T.PASTE_IN },
  { id: "copy_out", label: T.COPY_OUT },
  { id: "upload", label: T.UPLOAD },
  { id: "download", label: T.DOWNLOAD },
];
const UNLISTED: { value: FirstUseMode; label: string }[] = [
  { value: "always_deny", label: T.UNLISTED_BLOCK },
  { value: "deny_with_review", label: T.UNLISTED_ASK },
  { value: "wait_for_review", label: T.UNLISTED_HOLD },
];

// The form, one field per knob the card shows. `allowAllOff` keeps a walls document's own
// allow_all_egress: false (written with the CLI) across a save, as the card has no control for it.
interface Form {
  only: boolean;
  allowed: string[];
  allowedText: string;
  denied: string[];
  deniedText: string;
  firstUse: FirstUseMode | undefined;
  floor: ConfinementClass | undefined;
  blocked: Record<Direction, boolean>;
  withheld: WithheldGrant[];
  allowAllOff: boolean;
}

function toForm(s: PersonalWallsSpec): Form {
  const d = s.desktop;
  return {
    only: s.allowed_domains !== undefined,
    allowed: s.allowed_domains ?? [],
    allowedText: "",
    denied: s.denied_domains ?? [],
    deniedText: "",
    firstUse: s.first_use_approval,
    floor: s.min_confinement_class,
    blocked: { paste_in: d?.paste_in === "blocked", copy_out: d?.copy_out === "blocked", upload: d?.upload === "blocked", download: d?.download === "blocked" },
    withheld: s.withheld_grants ?? [],
    allowAllOff: s.allow_all_egress === false,
  };
}

// A site typed and not yet entered still counts: Save folds it in.
const withText = (list: string[], text: string): string[] => {
  const site = text.trim().toLowerCase();
  return site && !list.includes(site) ? [...list, site] : list;
};

function toSpec(f: Form): PersonalWallsSpec {
  const spec: PersonalWallsSpec = {};
  if (f.only) spec.allowed_domains = withText(f.allowed, f.allowedText);
  const denied = withText(f.denied, f.deniedText);
  if (denied.length > 0) spec.denied_domains = denied;
  if (f.allowAllOff && !f.only) spec.allow_all_egress = false;
  if (f.firstUse) spec.first_use_approval = f.firstUse;
  if (f.floor) spec.min_confinement_class = f.floor;
  const blocked = DIRECTIONS.filter((d) => f.blocked[d.id]);
  if (blocked.length > 0) spec.desktop = Object.fromEntries(blocked.map((d) => [d.id, "blocked" as const]));
  if (f.withheld.length > 0) spec.withheld_grants = f.withheld;
  return spec;
}

/** The sites of one list: a chip for each, and the field a new one is typed into (Enter or a comma enters it). */
function SiteList({ label, sites, text, onSites, onText }: { label: string; sites: string[]; text: string; onSites: (s: string[]) => void; onText: (t: string) => void }) {
  const enter = () => {
    onSites(withText(sites, text));
    onText("");
  };
  return (
    <div className="flex flex-wrap items-center gap-1.5">
      {sites.map((site) => (
        <Chip key={site} mono className="gap-1">
          {site}
          <button type="button" aria-label={T.REMOVE_SITE(site)} className="text-muted-foreground hover:text-foreground" onClick={() => onSites(sites.filter((s) => s !== site))}>
            <X className="size-3" aria-hidden="true" />
          </button>
        </Chip>
      ))}
      <Input
        aria-label={label}
        value={text}
        placeholder={T.ADD_SITE}
        className="h-8 w-44 font-mono text-xs"
        onChange={(e) => (e.target.value.endsWith(",") ? (onSites(withText(sites, e.target.value.slice(0, -1))), onText("")) : onText(e.target.value))}
        onKeyDown={(e) => {
          if (e.key !== "Enter") return;
          e.preventDefault();
          enter();
        }}
      />
    </div>
  );
}

const KIND_OPTIONS = Object.keys(T.KINDS);
const LABEL = "text-sm font-medium text-foreground";

export function YourWallsCard() {
  const [state, setState] = React.useState<"loading" | "absent" | "error" | "ready">("loading");
  const [form, setForm] = React.useState<Form>(() => toForm({}));
  const [busy, setBusy] = React.useState(false);
  const [saved, setSaved] = React.useState(false);
  const [refusal, setRefusal] = React.useState<{ message: string; reason: string } | null>(null);

  const load = React.useCallback(() => {
    setState("loading");
    api
      .get()
      .then((w) => {
        setForm(toForm(w.spec));
        setState("ready");
      })
      .catch((e) => setState(wallsAbsent(e) ? "absent" : "error"));
  }, []);
  React.useEffect(load, [load]);

  const edit = (patch: Partial<Form>) => {
    setForm((f) => ({ ...f, ...patch }));
    setSaved(false);
  };
  const fail = (e: unknown) => setRefusal({ message: e instanceof HttpError ? e.message : T.SAVE_FAILED, reason: e instanceof HttpError ? e.reason : "" });
  const run = async (act: () => Promise<void>) => {
    setBusy(true);
    setRefusal(null);
    try {
      await act();
    } catch (e) {
      fail(e);
    } finally {
      setBusy(false);
    }
  };
  const save = () =>
    run(async () => {
      const w = await api.put(toSpec(form));
      setForm(toForm(w.spec));
      setSaved(true);
    });
  const removeAll = () =>
    run(async () => {
      await api.clear();
      setForm(toForm({}));
      setSaved(false);
    });

  if (state === "absent") return null;
  const unnamed = KIND_OPTIONS.filter((k) => !form.withheld.some((g) => g.kind === k));
  return (
    <CollapsibleCard title={T.TITLE} summary={T.LEDE} testId="your-walls-card">
      {state === "loading" && <TableSkeleton />}
      {state === "error" && (
        <div className="flex items-center justify-between gap-3">
          <p className="text-sm text-danger">{T.LOAD_FAILED}</p>
          <Button variant="outline" size="sm" onClick={load}>
            {T.RETRY}
          </Button>
        </div>
      )}
      {state === "ready" && (
        <div className="flex flex-col gap-[18px]">
          <div className="flex flex-col gap-2">
            <span className={LABEL}>{T.SITES}</span>
            <RadioGroup
              className="gap-2"
              aria-label={T.SITES}
              value={form.only ? "only" : "any"}
              // The unlisted sites of a list are refused unless the person chose otherwise.
              onValueChange={(v) => edit(v === "only" ? { only: true, firstUse: form.firstUse ?? "always_deny" } : { only: false })}
            >
              <label className="flex items-center gap-2 text-sm">
                <RadioGroupItem value="any" />
                {T.SITES_ANY}
              </label>
              <label className="flex items-center gap-2 text-sm">
                <RadioGroupItem value="only" />
                {T.SITES_ONLY}
              </label>
            </RadioGroup>
            {form.only && (
              <div className="flex flex-col gap-1.5 pl-6">
                <SiteList label={T.SITES_ONLY} sites={form.allowed} text={form.allowedText} onSites={(allowed) => edit({ allowed })} onText={(allowedText) => edit({ allowedText })} />
                <p className="text-xs text-muted-foreground">{T.SITES_ONLY_HINT}</p>
              </div>
            )}
          </div>
          <div className="flex flex-col gap-2">
            <span className={LABEL}>{T.NEVER}</span>
            <SiteList label={T.NEVER} sites={form.denied} text={form.deniedText} onSites={(denied) => edit({ denied })} onText={(deniedText) => edit({ deniedText })} />
          </div>
          <div className="flex flex-col gap-2">
            <span className={LABEL}>{T.UNLISTED}</span>
            <Segmented<FirstUseMode | "">
              value={form.firstUse ?? ""}
              options={UNLISTED}
              onChange={(v) => edit({ firstUse: v || undefined })}
            />
          </div>
          <div className="flex flex-col gap-2">
            <span className={LABEL}>{T.BARRIER}</span>
            <RadioGroup className="flex flex-wrap gap-4" aria-label={T.BARRIER} value={form.floor ?? ""} onValueChange={(v) => edit({ floor: v as ConfinementClass })}>
              {CC_ORDER.map((cc) => (
                <label key={cc} className="flex items-center gap-2">
                  <RadioGroupItem value={cc} />
                  <ConfinementChip value={cc} />
                </label>
              ))}
            </RadioGroup>
          </div>
          <div className="flex flex-col gap-2">
            <span className={LABEL}>{T.CLIPBOARD}</span>
            <div className="flex flex-wrap gap-x-5 gap-y-2">
              {DIRECTIONS.map((d) => (
                <label key={d.id} className="flex items-center gap-2 text-sm">
                  <Checkbox checked={form.blocked[d.id]} onCheckedChange={(on) => edit({ blocked: { ...form.blocked, [d.id]: on === true } })} />
                  {d.label}
                </label>
              ))}
            </div>
          </div>
          <div className="flex flex-col gap-2">
            <span className={LABEL}>{T.CREDENTIALS}</span>
            <div className="flex flex-wrap items-center gap-1.5">
              {form.withheld.map((g, i) => (
                <Chip key={`${g.kind}${i}`} className="gap-1">
                  {T.KINDS[g.kind] ?? g.kind}
                  <button type="button" aria-label={T.REMOVE_SITE(T.KINDS[g.kind] ?? g.kind)} className="text-muted-foreground hover:text-foreground" onClick={() => edit({ withheld: form.withheld.filter((_, j) => j !== i) })}>
                    <X className="size-3" aria-hidden="true" />
                  </button>
                </Chip>
              ))}
              {unnamed.length > 0 && (
                <select
                  aria-label={T.CREDENTIALS}
                  value=""
                  className={cn("h-8 w-44 rounded-md border border-input bg-input-background px-2 text-sm")}
                  onChange={(e) => e.target.value && edit({ withheld: [...form.withheld, { kind: e.target.value }] })}
                >
                  <option value="">{T.ADD_KIND}</option>
                  {unnamed.map((k) => (
                    <option key={k} value={k}>
                      {T.KINDS[k]}
                    </option>
                  ))}
                </select>
              )}
            </div>
          </div>
          <div className="flex flex-wrap items-center gap-2">
            <Button size="sm" disabled={busy} onClick={() => void save()}>
              {T.SAVE}
            </Button>
            <Button size="sm" variant="outline" disabled={busy} onClick={() => void removeAll()}>
              {T.REMOVE_ALL}
            </Button>
          </div>
          {saved && (
            <p role="status" className="flex items-center gap-2 rounded-md border border-success/25 bg-success-subtle px-3 py-2 text-xs text-success">
              <Check className="size-3.5" aria-hidden="true" />
              {T.SAVED}
            </p>
          )}
          {refusal && (
            <p role="alert" className="text-sm text-danger">
              {refusal.message}
              {refusal.reason && <span className="ml-2 font-mono text-[11px] text-muted-foreground">{refusal.reason}</span>}
            </p>
          )}
          <p className="text-xs text-muted-foreground">{T.RESTART_NOTE}</p>
        </div>
      )}
    </CollapsibleCard>
  );
}
