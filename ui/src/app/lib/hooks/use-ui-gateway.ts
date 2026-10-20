/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Is this server's web app gateway on? /healthz's ui_sandbox is the one place it says so (absent when
// WARDYN_UI_SANDBOX_LISTEN is unset), read once per mount. `undefined` means unknown: the answer has not
// landed or the daemon did not answer, and a caller must not read that as off (run-detail-ssh.tsx does the same).
import * as React from "react";
import { health } from "../api/health";

export function useUIGateway(): boolean | undefined {
  const [on, setOn] = React.useState<boolean | undefined>(undefined);
  React.useEffect(() => {
    let alive = true;
    void health.health().then((h) => {
      if (alive && h.status === "ok") setOn(!!h.ui_sandbox?.enabled);
    });
    return () => {
      alive = false;
    };
  }, []);
  return on;
}
