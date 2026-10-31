/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import * as React from "react";
import { permissions } from "../../../../../lib/api/permissions";
import { serviceIdentities } from "../../../../../lib/api/service-identities";

/** What the console already knows about the person. Null while a list loads or when its read failed: the menu then says nothing about it. */
export interface CallerSources {
  /** The groups claim of the session (GET /me/capabilities session_groups), as sent. */
  groups: string[] | null;
  /** The names of the service identities the person sponsors that are active (GET /me/service-identities). */
  services: string[] | null;
}

/** Reads the two lists the first time the menu opens, and keeps them for the life of the picker. */
export function useCallerSources(open: boolean): CallerSources {
  const [sources, setSources] = React.useState<CallerSources>({ groups: null, services: null });
  const asked = React.useRef(false);
  React.useEffect(() => {
    if (!open || asked.current) return;
    asked.current = true;
    permissions.getMyCapabilities().then(
      (caps) => setSources((s) => ({ ...s, groups: caps.session_groups })),
      () => {},
    );
    serviceIdentities.listMine().then(
      (list) => setSources((s) => ({ ...s, services: list.filter((i) => i.state === "active").map((i) => i.name) })),
      () => {},
    );
  }, [open]);
  return sources;
}
