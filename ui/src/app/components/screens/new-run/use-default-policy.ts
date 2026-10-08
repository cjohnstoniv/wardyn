/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// New Run's one read of GET /policies/default: the caller's ceiling, which is
// both what the "Use the default policy" mode previews and where the rail
// learns the governance profile and floor bounding this caller. A failed read
// leaves both unknown (the rail names no ceiling it could not confirm) and the
// preview says so; it never blocks a launch.
import * as React from "react";
import { policies as policiesApi, type DefaultPolicy } from "../../../lib/api/policies";
import type { RunPolicySpec } from "../../../lib/types";

export type DefaultPolicyRead =
  | { status: "loading" }
  | { status: "ready"; policy: DefaultPolicy }
  | { status: "error" };

// The preview shows the spec as it stands, without the response's profile-name
// annotation (which is not a policy field).
export function previewSpec(p: DefaultPolicy): RunPolicySpec {
  const { governance_profile_name: _name, ...spec } = p;
  return spec;
}

export function useDefaultPolicy(scope: string, enabled: boolean): DefaultPolicyRead {
  const [read, setRead] = React.useState<DefaultPolicyRead>({ status: "loading" });
  const [loadedScope, setLoadedScope] = React.useState(scope);
  React.useEffect(() => {
    let alive = true;
    if (!enabled) return;
    setRead({ status: "loading" });
    policiesApi
      .getDefaultPolicy()
      .then((policy) => { if (alive) { setLoadedScope(scope); setRead({ status: "ready", policy }); } })
      .catch(() => { if (alive) { setLoadedScope(scope); setRead({ status: "error" }); } });
    return () => {
      alive = false;
    };
  }, [scope, enabled]);
  return loadedScope === scope && enabled ? read : { status: "loading" };
}
