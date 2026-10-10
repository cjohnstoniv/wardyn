/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import type { SetupModelProvider, SetupProviderAccess } from "./types";
import { CONNECTIONS } from "../components/wardyn/copy/door";

export function modelConnectionCause(provider: SetupModelProvider, access?: SetupProviderAccess): { line: string; button?: string } | undefined {
  if (access?.state !== "not_configured") return undefined;
  const host = access.new_destination || provider.host;
  switch (access.cause) {
    case "never_connected": return { line: CONNECTIONS.NEVER_CONNECTED(host) };
    case "destination_changed": return { line: CONNECTIONS.DESTINATION_CHANGED(host), button: CONNECTIONS.REVIEW_RECONNECT };
    case "kind_changed": return { line: CONNECTIONS.KIND_CHANGED(host), button: CONNECTIONS.REVIEW_RECONNECT };
    case "store_unreadable": return { line: CONNECTIONS.STORE_UNREADABLE, button: CONNECTIONS.RECHECK };
    default: return undefined;
  }
}
