/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The Tools step's Wall preset control (boards WP-2, NR-5): Open, Read-only, Sealed and No network as a segmented choice, then More
// presets for the organisation's, the groups' and the person's own. The choice narrows this run only, on top of the
// person's own walls. A preset that is no longer available stays chosen, as an error: nothing falls back to Open, and
// Launch stays held until the person picks another or picks Open.
import * as React from "react";
import { ChevronDown, CircleAlert, X } from "lucide-react";
import { audienceOf } from "../../../../../lib/wall-preset-words";
import type { WallPreset, WallPresetList } from "../../../../../lib/types/wall-presets";
import { Button } from "../../../../ui/button";
import { WALL_PRESETS as W } from "../../../../wardyn/copy/wall-presets";
import { Segmented } from "../../../../wardyn/segmented";

const BUILT_IN_IDS = ["open", "read-only", "sealed", "no-network"];

/** The preset the draft names, from the list; undefined for none chosen (Open) or one the list lacks. */
export const chosenPreset = (list: WallPresetList | undefined, id: string | undefined): WallPreset | undefined =>
  id ? list?.presets.find((p) => p.id === id) : undefined;

/** The chosen preset is one this caller can no longer pick: a launch said so, or the list no longer holds it. */
export const presetUnavailable = (list: WallPresetList | undefined, id: string | undefined, gone: { id: string; name: string } | undefined): boolean =>
  !!id && id !== "open" && (gone?.id === id || (!!list && !chosenPreset(list, id)));

export function WallPresetPicker({
  list, value, unavailable, onChange,
}: {
  list: WallPresetList;
  /** The draft's wall_preset: absent is Open. */
  value: string | undefined;
  /** Set when the chosen preset is no longer available: what is remembered of it. */
  unavailable?: { name: string; audience: string };
  onChange: (id: string) => void;
}) {
  const [more, setMore] = React.useState(false);
  // The control a pick or a clear unmounts is the one holding focus, so focus moves to one that stays.
  const moreButton = React.useRef<HTMLButtonElement>(null);
  const builtinGroup = React.useRef<HTMLDivElement>(null);
  const labels = list.publish.group_labels;
  const builtins = BUILT_IN_IDS.flatMap((id) => list.presets.filter((p) => p.id === id));
  const others = list.presets.filter((p) => p.scope !== "builtin");
  const chosen = chosenPreset(list, value);
  const custom = chosen && chosen.scope !== "builtin" ? chosen : undefined;
  const name = custom?.name || unavailable?.name || W.UNAVAILABLE_UNNAMED;
  const audience = custom ? audienceOf(custom, labels) : unavailable?.audience;
  const pill = audience ? W.PILL(name, audience) : name;
  const sections = [
    { heading: W.ORGANISATION, rows: others.filter((p) => p.scope === "org") },
    ...[...new Set(others.filter((p) => p.scope === "group").map((p) => p.group_id ?? ""))].map((g) => ({
      heading: W.GROUP_OF(labels?.[g] ?? g), rows: others.filter((p) => p.scope === "group" && (p.group_id ?? "") === g),
    })),
    { heading: W.YOURS, rows: others.filter((p) => p.scope === "person") },
  ].filter((s) => s.rows.length > 0);
  return (
    <div data-testid="wall-preset-picker" className="flex flex-col gap-2 rounded-xl border border-border bg-card p-4">
      <h3 id="wall-preset-label" className="font-medium">{W.CONTROL}</h3>
      <div className="flex flex-wrap items-center gap-2">
        <div ref={builtinGroup} role="group" aria-labelledby="wall-preset-label">
          <Segmented<string> value={unavailable || custom ? "" : (value ?? "open")} options={builtins.map((p) => ({ value: p.id, label: p.name }))} onChange={(v) => (setMore(false), onChange(v))} stackOnPhone />
        </div>
        {sections.length > 0 && (
          <Button ref={moreButton} type="button" variant="outline" aria-expanded={more} onClick={() => setMore((m) => !m)}>
            {W.MORE} <ChevronDown className="size-4" aria-hidden="true" />
          </Button>
        )}
        {(custom || unavailable) && (
          <span data-testid="wall-preset-pill" className={unavailable ? "inline-flex items-center gap-1.5 rounded-md border border-danger/40 bg-danger-subtle px-2.5 py-1 text-sm text-danger" : "inline-flex items-center gap-1.5 rounded-md border border-border bg-muted px-2.5 py-1 text-sm"}>
            {unavailable && <CircleAlert className="size-3.5" aria-hidden="true" />}
            {pill}
            <button type="button" aria-label={W.CLEAR_PRESET(name)} className="-mr-1 inline-flex size-6 items-center justify-center rounded text-muted-foreground hover:text-foreground" onClick={() => (builtinGroup.current?.querySelector("button")?.focus(), onChange("open"))}>
              <X className="size-3" aria-hidden="true" />
            </button>
          </span>
        )}
      </div>
      {more && (
        <div className="flex max-w-sm flex-col gap-2 rounded-lg border border-border p-3">
          {sections.map((s) => (
            <section key={s.heading} aria-label={s.heading} className="flex flex-col gap-1">
              <h4 className="label-eyebrow">{s.heading}</h4>
              {s.rows.map((p) => (
                <button key={p.id} type="button" aria-pressed={p.id === value} className="flex flex-col items-start gap-0.5 rounded-md px-2 py-1.5 text-left hover:bg-muted" onClick={() => (moreButton.current?.focus(), setMore(false), onChange(p.id))}>
                  <span className="text-sm font-medium">{p.name}</span>
                  {p.description && <span className="text-xs text-muted-foreground">{p.description}</span>}
                </button>
              ))}
            </section>
          ))}
        </div>
      )}
      {unavailable && (
        <p role="alert" className="text-sm text-danger">
          {W.UNAVAILABLE(name)}
        </p>
      )}
      <p className="text-sm text-muted-foreground">{W.CONTROL_HELP}</p>
    </div>
  );
}
