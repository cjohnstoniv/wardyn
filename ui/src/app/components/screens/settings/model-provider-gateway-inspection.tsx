/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// "Let this gateway check prompts" (gateway_identity.inspection), drawn by the GW-INSPECT board: inside the
// identity box, below Key address. On, Wardyn leaves the prompts sent to this gateway to it, except the
// run's own secret values. The switch is part of the route sealed into each run, so a change to a saved
// provider says, before Save, what it does to runs already going. A Bedrock row's calls are scanned by the
// generic forward check, which a gateway cannot take over, so it gets the reason instead of the switch.
import * as React from "react";
import { PROVIDER_EDITOR } from "../../../lib/model-providers-copy";
import { Switch } from "../../wardyn/form-primitives";
import { isBedrock, type ProviderDraft } from "./model-provider-draft";

export function GatewayInspection({
  draft,
  set,
  disabled,
}: {
  draft: ProviderDraft;
  set: (patch: Partial<ProviderDraft>) => void;
  disabled: boolean;
}) {
  const g = PROVIDER_EDITOR.GATEWAY;
  const helpId = React.useId();
  const sealedId = React.useId();
  const saved = draft.gatewaySaved;
  const changed = !!saved && saved.inspect !== draft.gatewayInspect;
  if (isBedrock(draft.kind)) {
    return (
      <div className="space-y-1.5">
        <p className="text-xs font-medium text-foreground">{g.INSPECT_HEADING}</p>
        <p className="text-xs leading-snug text-muted-foreground">{g.INSPECT_NOT_BEDROCK}</p>
      </div>
    );
  }
  return (
    <div className="space-y-1.5">
      <p className="text-xs font-medium text-foreground">{g.INSPECT_HEADING}</p>
      <div className="flex items-center gap-2.5">
        <Switch
          checked={draft.gatewayInspect}
          onChange={(gatewayInspect) => set({ gatewayInspect })}
          disabled={disabled}
          label={g.INSPECT_SWITCH}
          describedBy={changed ? `${helpId} ${sealedId}` : helpId}
        />
        <span className="text-body">{g.INSPECT_SWITCH}</span>
      </div>
      <p id={helpId} className="text-xs leading-snug text-muted-foreground">
        {draft.gatewayInspect ? g.INSPECT_ON : g.INSPECT_OFF}
      </p>
      {changed && (
        <p id={sealedId} className="text-xs leading-snug text-warning">
          {g.INSPECT_SEALED}
        </p>
      )}
    </div>
  );
}
