/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The walls form: one field per knob a person may set, shared by Your walls and the wall presets card. It holds
// the form model (toForm and toSpec, a lossless round trip of a PersonalWallsSpec) and the controlled form itself;
// the callers own loading, saving and their own buttons.
import * as React from "react";
import { X } from "lucide-react";
import { type PersonalWallsPushRules, type PersonalWallsSpec, type WithheldGrant } from "../../../lib/api/walls";
import { CC_ORDER, type ConfinementClass, type FirstUseMode } from "../../../lib/types";
import { Checkbox } from "../../ui/checkbox";
import { Input } from "../../ui/input";
import { RadioGroup, RadioGroupItem } from "../../ui/radio-group";
import { cn } from "../../ui/utils";
import { YOUR_WALLS as T } from "../../wardyn/copy/personal-walls";
import type { FormSection } from "../../../lib/wall-preset-words";
import { WALL_PRESETS as WP, carriedKnobWords } from "../../wardyn/copy/wall-presets";
import { Switch } from "../../wardyn/form-primitives";
import { ConfinementChip, Chip } from "../../wardyn/primitives";
import { Segmented } from "../../wardyn/segmented";

export type Direction = "paste_in" | "copy_out" | "upload" | "download";
export const DIRECTIONS: { id: Direction; label: string }[] = [
  { id: "paste_in", label: T.PASTE_IN },
  { id: "copy_out", label: T.COPY_OUT },
  { id: "upload", label: T.UPLOAD },
  { id: "download", label: T.DOWNLOAD },
];
const UNLISTED: { value: FirstUseMode | ""; label: string }[] = [
  { value: "", label: T.UNLISTED_ORG },
  { value: "always_deny", label: T.UNLISTED_BLOCK },
  { value: "deny_with_review", label: T.UNLISTED_ASK },
  { value: "wait_for_review", label: T.UNLISTED_HOLD },
];

// The form, one field per knob the card shows. `allowAllOff` keeps a walls document's own
// allow_all_egress: false (written with the CLI) across a save, as the card has no control for it.
export interface Form {
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
  /** push_rules.deny_all: refuse every push. */
  denyAll: boolean;
  // The knobs the card has no control for (capability lists, tool and push rules, recording, auto-stop):
  // kept as fetched, since a save replaces the whole document.
  carried: PersonalWallsSpec;
}

export const carriedKnobs = (s: PersonalWallsSpec): PersonalWallsSpec => {
  const { azure_devops_capabilities, github_capabilities, tool_rules, recording, auto_stop_after_sec } = s;
  // deny_all has its own switch; the push paths beside it ride along as fetched.
  const paths = Object.fromEntries(Object.entries(s.push_rules ?? {}).filter(([k]) => k !== "deny_all")) as PersonalWallsPushRules;
  const push_rules = Object.keys(paths).length > 0 ? paths : undefined;
  const carried = { azure_devops_capabilities, github_capabilities, tool_rules, push_rules, recording, auto_stop_after_sec };
  return Object.fromEntries(Object.entries(carried).filter(([, v]) => v !== undefined));
};

export function toForm(s: PersonalWallsSpec): Form {
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
    denyAll: s.push_rules?.deny_all === true,
    carried: carriedKnobs(s),
  };
}

// A typed site is a bare host: lower-case, with any scheme and everything from the first "/", "?" or "#" dropped.
const siteOf = (text: string): string => text.trim().toLowerCase().replace(/^[a-z][a-z0-9+.-]*:\/\//, "").replace(/[/?#].*$/, "");

// A site typed and not yet entered still counts: Save folds it in.
export const withText = (list: string[], text: string): string[] => {
  const site = siteOf(text);
  return site && !list.includes(site) ? [...list, site] : list;
};

export function toSpec(f: Form): PersonalWallsSpec {
  const spec: PersonalWallsSpec = { ...f.carried };
  if (f.only) spec.allowed_domains = withText(f.allowed, f.allowedText);
  const denied = withText(f.denied, f.deniedText);
  if (denied.length > 0) spec.denied_domains = denied;
  if (f.allowAllOff && !f.only) spec.allow_all_egress = false;
  if (f.firstUse) spec.first_use_approval = f.firstUse;
  if (f.floor) spec.min_confinement_class = f.floor;
  const blocked = DIRECTIONS.filter((d) => f.blocked[d.id]);
  if (blocked.length > 0) spec.desktop = Object.fromEntries(blocked.map((d) => [d.id, "blocked" as const]));
  if (f.withheld.length > 0) spec.withheld_grants = f.withheld;
  if (f.denyAll) spec.push_rules = { ...spec.push_rules, deny_all: true };
  return spec;
}

/** The sites of one list: a chip for each, and the field a new one is typed into (Enter or a comma enters it). */
function SiteList({ label, sites, text, onSites, onText }: { label: string; sites: string[]; text: string; onSites: (s: string[]) => void; onText: (t: string) => void }) {
  const enter = () => {
    onSites(withText(sites, text));
    onText("");
  };
  return (
    <div className="flex flex-col gap-1.5">
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
    <p className="text-xs text-muted-foreground">{T.SITE_HINT}</p>
    </div>
  );
}

export const KIND_OPTIONS = Object.keys(T.KINDS);
export const LABEL = "text-sm font-medium text-foreground";

const SectionError = ({ text }: { text: string }) => (
  <p role="alert" className="text-xs text-danger">
    {text}
  </p>
);

/**
 * The controlled form. `onEdit` merges a patch into the caller's Form. `listCarried` adds a read-only row naming the
 * knobs the form has no control for, which a save keeps as they were fetched. `errors` puts a server sentence under the
 * section it names.
 */
export function WallsForm({ form, onEdit: edit, listCarried, errors }: { form: Form; onEdit: (patch: Partial<Form>) => void; listCarried?: boolean; errors?: Partial<Record<FormSection, string>> }) {
  // The unlisted-sites choice was filled in by picking "Only these sites", not chosen by the person.
  const [autoFirstUse, setAutoFirstUse] = React.useState(false);
  const unnamed = KIND_OPTIONS.filter((k) => !form.withheld.some((g) => g.kind === k));
  return (
    <div className="flex flex-col gap-[18px]">
      <div className="flex flex-col gap-2">
        <span className={LABEL}>{T.SITES}</span>
        {errors?.sites && <SectionError text={errors.sites} />}
        <RadioGroup
          className="gap-2"
          aria-label={T.SITES}
          value={form.only ? "only" : "any"}
          // The unlisted sites of a list are refused unless the person chose otherwise.
          onValueChange={(v) => {
            if (v === "only") {
              setAutoFirstUse(form.firstUse === undefined || autoFirstUse);
              edit({ only: true, firstUse: form.firstUse ?? "always_deny" });
            } else {
              setAutoFirstUse(false);
              edit(autoFirstUse ? { only: false, firstUse: undefined } : { only: false });
            }
          }}
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
        {errors?.never && <SectionError text={errors.never} />}
        <SiteList label={T.NEVER} sites={form.denied} text={form.deniedText} onSites={(denied) => edit({ denied })} onText={(deniedText) => edit({ deniedText })} />
      </div>
      <div className="flex flex-col gap-2">
        <span className={LABEL}>{T.UNLISTED}</span>
        {errors?.unlisted && <SectionError text={errors.unlisted} />}
        <Segmented<FirstUseMode | "">
          stackOnPhone
          value={form.firstUse ?? ""}
          options={UNLISTED}
          onChange={(v) => {
            setAutoFirstUse(false);
            edit({ firstUse: v || undefined });
          }}
        />
      </div>
      <div className="flex flex-col gap-2">
        <span className={LABEL}>{T.BARRIER}</span>
        {errors?.barrier && <SectionError text={errors.barrier} />}
        <RadioGroup className="flex flex-wrap gap-4" aria-label={T.BARRIER} value={form.floor ?? "none"} onValueChange={(v) => edit({ floor: v === "none" ? undefined : (v as ConfinementClass) })}>
          <label className="flex items-center gap-2 text-sm">
            <RadioGroupItem value="none" />
            {T.BARRIER_NONE}
          </label>
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
        <p className="text-xs text-muted-foreground">{T.CLIPBOARD_HINT}</p>
        {errors?.clipboard && <SectionError text={errors.clipboard} />}
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
        {errors?.credentials && <SectionError text={errors.credentials} />}
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
      <div className="flex flex-col gap-2">
        <span className={LABEL}>{T.PUSHES}</span>
        {errors?.pushes && <SectionError text={errors.pushes} />}
        <label className="flex items-start gap-2 text-sm">
          <Switch checked={form.denyAll} onChange={(denyAll) => edit({ denyAll })} label={T.REFUSE_PUSH} className="mt-0.5" />
          <span className="flex flex-col gap-0.5">
            <span className="font-medium text-foreground">{T.REFUSE_PUSH}</span>
            <span className="text-xs text-muted-foreground">{T.REFUSE_PUSH_HELP}</span>
          </span>
        </label>
      </div>
      {listCarried && carriedKnobWords(form.carried).length > 0 && (
        <p data-testid="walls-carried" className="rounded-md border border-dashed border-border px-3 py-2 text-xs text-muted-foreground">
          {WP.CARRIED(carriedKnobWords(form.carried).join(", "))}
        </p>
      )}
    </div>
  );
}
