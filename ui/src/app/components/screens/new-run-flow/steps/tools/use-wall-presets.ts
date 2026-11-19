/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import * as React from "react";
import { wallPresets } from "../../../../../lib/api/wall-presets";
import { audienceOf } from "../../../../../lib/wall-preset-words";
import type { WallPresetList } from "../../../../../lib/types/wall-presets";

const known = new Map<string, { name: string; audience: string }>();

/** What was last read of a preset, for naming one a launch finds gone. */
export const rememberedPreset = (id: string) => known.get(id);

/**
 * The wall presets the caller may pick, undefined until read and when the read fails (a store that keeps none): the
 * choice is then not offered. Any caller who may create a run may apply a built-in or an organisation preset, so no
 * person is needed to read them.
 */
export function useWallPresets(): WallPresetList | undefined {
  const [list, setList] = React.useState<WallPresetList>();
  React.useEffect(() => {
    let alive = true;
    wallPresets.list().then((l) => {
      for (const p of l.presets) known.set(p.id, { name: p.name, audience: audienceOf(p, l.publish.group_labels) });
      if (alive) setList(l);
    }, () => undefined);
    return () => {
      alive = false;
    };
  }, []);
  return list;
}
