/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The shell's half of the one door (#544), for suites that render a screen
// directly: the context, and the strip that mounts the one dialog — the two
// things app-shell.tsx and App.tsx put around every screen. An entrance's
// button opens a door only when both are above it.
import * as React from "react";
import { MemoryRouter } from "react-router-dom";

import { ModelAccessBanner } from "../app/components/wardyn/model-access-banner";
import { ModelAccessProvider } from "../app/components/wardyn/model-access-context";
import { OperatorProvider } from "../app/components/wardyn/operator-context";
import type { SetupStatus } from "../app/lib/types";

export function WithDoor({
  status,
  path = "/runs",
  operator = true,
  operatorResolved = true,
  principal = "someone@corp.example",
  onRefresh = () => {},
  children,
}: {
  status: SetupStatus | null;
  path?: string;
  operator?: boolean;
  /** false: /me has not answered yet. */
  operatorResolved?: boolean;
  principal?: string;
  onRefresh?: () => void | Promise<unknown>;
  children?: React.ReactNode;
}) {
  return (
    <MemoryRouter initialEntries={[path]}>
      <ModelAccessProvider status={status} onRefresh={onRefresh}>
        <OperatorProvider operator={operator} securityOperator={operator} principal={principal} operatorResolved={operatorResolved}>
          <div role="status">
            <ModelAccessBanner />
          </div>
          <main id="main-content" tabIndex={-1}>
            {children}
          </main>
        </OperatorProvider>
      </ModelAccessProvider>
    </MemoryRouter>
  );
}
