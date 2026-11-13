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

/**
 * Reads each list the first time the menu opens and keeps it once read. A read that failed is asked again the next
 * time the menu opens, so a passing outage does not hide the list for the life of the picker.
 */
export function useCallerSources(open: boolean): CallerSources {
  const [sources, setSources] = React.useState<CallerSources>({ groups: null, services: null });
  // Per list: not read yet (or its read failed), being read, or read.
  const state = React.useRef<Record<keyof CallerSources, "unread" | "reading" | "read">>({ groups: "unread", services: "unread" });
  React.useEffect(() => {
    if (!open) return;
    const read = (key: keyof CallerSources, get: () => Promise<string[]>) => {
      if (state.current[key] !== "unread") return;
      state.current[key] = "reading";
      get().then(
        (list) => {
          state.current[key] = "read";
          setSources((s) => ({ ...s, [key]: list }));
        },
        () => {
          state.current[key] = "unread";
        },
      );
    };
    read("groups", () => permissions.getMyCapabilities().then((caps) => caps.session_groups));
    read("services", () => serviceIdentities.listMine().then((list) => list.filter((i) => i.state === "active").map((i) => i.name)));
  }, [open]);
  return sources;
}
