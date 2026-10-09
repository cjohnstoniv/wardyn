/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The Permissions row for the `custom_component` feature value: whether a person
// may define their own components. The value is default ON and narrowing, so the
// switch is "no deny row": turning it off writes the one row
// {all, feature, custom_component, deny} (a deny beats every allow), and turning
// it back on deletes that row. Where the `feature` kind is enforced, a person
// also needs an allow for the value; the row says so rather than drawing "on".
import * as React from "react";
import { toast } from "sonner";
import { PendingChangeError } from "../../../lib/api/core";
import { permissions as api } from "../../../lib/api/permissions";
import { getErrorMessage } from "../../../lib/format";
import type { CapabilityGrant } from "../../../lib/types";
import { COMPONENTS_ADMIN as T } from "../../wardyn/copy/components-admin";
import { Switch } from "../../wardyn/form-primitives";
import { Chip } from "../../wardyn/primitives";

const VALUE = "custom_component";

const everyoneDeny = (g: CapabilityGrant) =>
  g.capability === "feature" && g.value === VALUE && g.subject_type === "all" && g.effect === "deny";

export function DefineOwnComponentsRow({
  grants,
  featureEnforced,
  disabled,
  onGrants,
  onSubmitted,
}: {
  grants: CapabilityGrant[];
  featureEnforced: boolean;
  disabled: boolean;
  /** The grant list after a write that landed. */
  onGrants: (next: CapabilityGrant[]) => void;
  /** The write was held for a second person (202): nothing changed yet. */
  onSubmitted: () => void;
}) {
  const [busy, setBusy] = React.useState(false);
  const deny = grants.find(everyoneDeny);
  const on = !deny;
  // Where the feature kind is enforced, only an allow admits a person, so "on" is not "everyone".
  const byAllow = on && featureEnforced;

  const toggle = async (next: boolean) => {
    setBusy(true);
    try {
      if (next && deny) {
        await api.deleteGrant(deny.id);
        onGrants(grants.filter((g) => g.id !== deny.id));
      } else if (!next) {
        const { grant } = await api.upsertGrant({
          subject_type: "all",
          subject: "",
          capability: "feature",
          value: VALUE,
          effect: "deny",
        });
        onGrants([...grants.filter((g) => g.id !== grant.id), grant]);
      }
    } catch (e) {
      if (e instanceof PendingChangeError) onSubmitted();
      else toast.error(T.DEFINE_FAILED, { description: getErrorMessage(e) });
    } finally {
      setBusy(false);
    }
  };

  return (
    <div
      className="grid grid-cols-[1fr_auto] items-start gap-4 border-t border-border px-6 py-4"
      data-testid="define-own-components-row"
    >
      <div>
        <div className="flex flex-wrap items-center gap-2">
          <h3 className="text-body font-medium text-foreground">{T.DEFINE_LABEL}</h3>
          <Chip tone={on && !byAllow ? "warning" : "neutral"} dot>
            {!on ? T.DEFINE_OFF_CHIP : byAllow ? T.DEFINE_ALLOW_CHIP : T.DEFINE_ON_CHIP}
          </Chip>
        </div>
        <p className="mt-0.5 text-xs text-muted-foreground">{T.DEFINE_BLURB}</p>
        <p className="mt-2 max-w-[76ch] text-body text-foreground">
          {!on ? T.DEFINE_OFF : byAllow ? T.DEFINE_ALLOW : T.DEFINE_ON}
        </p>
      </div>
      <Switch checked={on} disabled={disabled || busy} onChange={(v) => void toggle(v)} label={T.DEFINE_SWITCH} />
    </div>
  );
}
