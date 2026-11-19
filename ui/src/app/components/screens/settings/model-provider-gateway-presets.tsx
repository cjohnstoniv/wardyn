/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { PROVIDER_EDITOR } from "../../../lib/model-providers-copy";
import { Button } from "../../ui/button";
import { Field } from "../../wardyn/form-primitives";
import { useRovingRadio } from "../../wardyn/use-roving-radio";
import { PRESET_IDS, type PresetId } from "./model-provider-presets";

// "Start from a gateway": the same segmented radio group as How people sign in. A press prefills (the editor
// applies it); the pressed chip is editor state only, never saved or sent.
export function GatewayPresets({ value, onPick }: { value: PresetId | null; onPick: (id: PresetId) => void }) {
  const group = useRovingRadio(PRESET_IDS.length, Math.max(0, value ? PRESET_IDS.indexOf(value) : 0), (i) => onPick(PRESET_IDS[i]));
  return (
    <Field label={PROVIDER_EDITOR.PRESET_LABEL} hint={PROVIDER_EDITOR.PRESET_HINT}>
      <div role="radiogroup" aria-label={PROVIDER_EDITOR.PRESET_LABEL} className="flex flex-wrap gap-2" {...group.containerProps}>
        {PRESET_IDS.map((id, i) => (
          <Button
            key={id}
            type="button"
            role="radio"
            aria-checked={value === id}
            size="sm"
            variant={value === id ? "secondary" : "outline"}
            tabIndex={group.itemProps(i).tabIndex}
            ref={group.itemProps(i).radioRef}
            onClick={() => onPick(id)}
          >
            {PROVIDER_EDITOR.PRESET[id]}
          </Button>
        ))}
      </div>
    </Field>
  );
}
