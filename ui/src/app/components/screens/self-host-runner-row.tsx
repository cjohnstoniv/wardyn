/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The Permissions row for the `self_host_runner` feature value: whether a person may mint a registration
// token for a runner of their own. The value is restricted (migration 0178), so it is off until an allow
// names it and the switch is the one row {all, feature, self_host_runner, allow}; turning it off deletes
// that row. Allows for a person or a group are written in the grant table below and are left alone.
import * as React from "react";
import { toast } from "sonner";
import { PendingChangeError } from "../../lib/api/core";
import { permissions as api } from "../../lib/api/permissions";
import { getErrorMessage } from "../../lib/format";
import { SELF_HOST as T } from "../../lib/permissions-copy";
import type { CapabilityGrant } from "../../lib/types";
import { Switch } from "../wardyn/form-primitives";
import { Chip } from "../wardyn/primitives";

const VALUE = "self_host_runner";

const everyoneAllow = (g: CapabilityGrant) =>
  g.capability === "feature" && g.value === VALUE && g.subject_type === "all" && g.effect === "allow";

export function SelfHostRunnerRow({
  grants,
  disabled,
  onGrants,
  onSubmitted,
}: {
  grants: CapabilityGrant[];
  disabled: boolean;
  /** The grant list after a write that landed. */
  onGrants: (next: CapabilityGrant[]) => void;
  /** The write was held for a second person (202): nothing changed yet. */
  onSubmitted: () => void;
}) {
  const [busy, setBusy] = React.useState(false);
  const allow = grants.find(everyoneAllow);
  const on = !!allow;

  const toggle = async (next: boolean) => {
    setBusy(true);
    try {
      if (!next && allow) {
        await api.deleteGrant(allow.id);
        onGrants(grants.filter((g) => g.id !== allow.id));
      } else if (next) {
        const { grant } = await api.upsertGrant({
          subject_type: "all",
          subject: "",
          capability: "feature",
          value: VALUE,
          effect: "allow",
        });
        onGrants([...grants.filter((g) => g.id !== grant.id), grant]);
      }
    } catch (e) {
      if (e instanceof PendingChangeError) onSubmitted();
      else toast.error(T.FAILED, { description: getErrorMessage(e) });
    } finally {
      setBusy(false);
    }
  };

  return (
    <div className="grid grid-cols-[1fr_auto] items-start gap-4 border-t border-border px-6 py-4" data-testid="self-host-runner-row">
      <div>
        <div className="flex flex-wrap items-center gap-2">
          <h3 className="text-body font-medium text-foreground">{T.LABEL}</h3>
          <Chip tone={on ? "warning" : "neutral"} dot>
            {on ? T.EVERYONE_CHIP : T.BY_GRANT_CHIP}
          </Chip>
        </div>
        <p className="mt-0.5 text-xs text-muted-foreground">{T.BLURB}</p>
        <p className="mt-2 max-w-[76ch] text-body text-foreground">{on ? T.EVERYONE : T.BY_GRANT}</p>
      </div>
      <Switch checked={on} disabled={disabled || busy} onChange={(v) => void toggle(v)} label={T.SWITCH} />
    </div>
  );
}
