/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// r12 (#2957): the local model server's "Let runs on a person's own computer use that computer's model
// server" switch (local.allow_on_laptop_runners), its hint, and the disclosure of what it turns on. Only an
// admin changes it; anyone else sees its value, dimmed, with the canon reason.
import { LOCAL_MODEL_EDITOR as L } from "../../../lib/local-model-copy";
import { OPERATOR_ONLY_REASON } from "../../wardyn/copy";
import { Switch } from "../../wardyn/form-primitives";
import { useOperator } from "../../wardyn/operator-context";

export function LaptopRunnersSwitch({ on, onChange }: { on: boolean; onChange: (on: boolean) => void }) {
  const operator = useOperator();
  return (
    <div className="space-y-2">
      <div className="flex items-center gap-2.5">
        <Switch checked={on} onChange={onChange} disabled={!operator} label={L.LAPTOP_SWITCH} />
        <span className="text-body">{L.LAPTOP_SWITCH}</span>
      </div>
      <p className="text-xs leading-snug text-muted-foreground">{on ? L.LAPTOP_ON_HINT : L.LAPTOP_OFF_HINT}</p>
      {!operator && <p className="text-xs leading-snug text-muted-foreground">{OPERATOR_ONLY_REASON}</p>}
      {on && (
        <section aria-label={L.LAPTOP_DISCLOSURE} className="space-y-2 rounded-lg border border-warning/40 bg-warning-subtle px-4 py-3">
          <h4 className="text-xs font-semibold uppercase tracking-wide text-warning">{L.LAPTOP_DISCLOSURE}</h4>
          <ul className="list-disc space-y-1.5 pl-4 text-sm text-foreground">
            {L.LAPTOP_BULLETS.map((b) => (
              <li key={b}>{b}</li>
            ))}
          </ul>
          <p className="text-xs leading-snug text-muted-foreground">{L.LAPTOP_FOOT}</p>
        </section>
      )}
    </div>
  );
}
