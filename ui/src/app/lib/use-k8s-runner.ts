/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import * as React from "react";
import { setup as setupApi } from "./api/setup";

// useK8sRunner — the ONE k8s-driver signal (B4), derived from
// /setup/status's runner.driver (setupRunnerInfo already pipes it), shared by
// every surface that needs to know "does this control plane run its sandboxes
// as Kubernetes pods" — today the tier-1 library add dialog
// (sources-library.tsx) and the workspace wizard's add-source step
// (step-sources.tsx), both of which must stop offering a local-directory
// source on k8s (mounts are structurally impossible there — see
// internal/runner/substrate's package doc).
//
// Self-contained: each caller mounts this hook rather than the caller's
// parent threading a prop down through wizard.tsx/workspaces.tsx/
// setup-screen.tsx, all of which mount SourcesLibrary/StepSources from more
// than one place and none of which otherwise need SetupStatus at all.
//
// Fail-open to false (the common case: docker/local — offer local
// directories) while loading or on a failed fetch: this is advisory UI only
// (the "hiding is cosmetic" rule applies here too — nothing server-side reads
// this hook), so a slow probe must never hide a legitimate option.
//
// `enabled` (default true) defers the fetch: AddSourceDialog is mounted
// (closed) for as long as its parent SourcesLibrary is on screen, not just
// while actually open — an unconditional fetch here fired one extra
// /setup/status round trip every time the Getting-started walk merely PASSED
// THROUGH the Sources step. Pass `open` (or similar) from a caller shaped
// like that; a caller that only ever mounts while genuinely relevant (e.g. a
// wizard step body) can ignore the parameter.
export function useK8sRunner(enabled = true): boolean {
  const [k8s, setK8s] = React.useState(false);
  React.useEffect(() => {
    if (!enabled) return;
    let alive = true;
    setupApi
      .getSetupStatus()
      .then((s) => {
        if (alive) setK8s(s.runner.driver === "k8s");
      })
      .catch(() => {
        /* leave the default (false) — never hide local directories on a mere fetch blip */
      });
    return () => {
      alive = false;
    };
  }, [enabled]);
  return k8s;
}
