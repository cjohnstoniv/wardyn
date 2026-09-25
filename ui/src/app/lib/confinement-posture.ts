/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// #162's posture resolver — pure, no React, no icons, no router — deliberately
// its own module rather than living in wardyn/confinement-posture.tsx (the
// banner component). app-shell.tsx is in the EAGER entry chunk and calls this
// on every render; wardyn/confinement-posture.tsx is lazy-loaded like
// ModelAccessBanner (bundle-split.test.ts's entry-chunk guard). Importing the
// resolver FROM the lazy module would statically pull the banner's icon +
// react-router-dom import back into the entry graph and silently collapse the
// split it's named after — the exact failure mode that file's own comment
// warns about.
//
// THE PREMISE CORRECTION (mock-approval comment on issue #162): reading
// posture off `h.network_policy ?? ""` alone cannot work. internal/api/
// healthz.go omits `network_policy` in TWO situations that need OPPOSITE
// treatment — on Docker (genuinely not applicable, stay silent) and on a
// Kubernetes daemon whose Capabilities() call itself errored (could not
// confirm, must warn). One empty string cannot tell them apart, so this also
// takes `runner` (already on the /healthz wire, lib/api/health.ts's
// `runner?: string`) and resolves the five cases the mock's approval ruled on:
//
//   runner    network_policy   posture
//   docker    absent           ""             (not applicable — no banner, no ring)
//   k8s       enforced         "enforced"     (no banner, no ring)
//   k8s       acknowledged     "acknowledged" (warning strip + soft ring)
//   k8s       unenforced       "unenforced"   (danger strip + warning ring + glyph)
//   k8s       absent           "unknown"      (warning strip — could not confirm; ruling 2)
//
// A runner that is neither "docker" nor "k8s" (the pre-mount default, an
// older daemon, or any future substrate) resolves the same as Docker: silent.
// That is deliberate — a posture warning must never be INVENTED ahead of a
// real /healthz answer just because the field is momentarily empty.
import type { ConfinementPosture } from "../components/wardyn/operator-context";

export function resolveConfinementPosture(runner: string, networkPolicy: string): ConfinementPosture {
  if (runner !== "k8s") return "";
  if (networkPolicy === "enforced" || networkPolicy === "acknowledged" || networkPolicy === "unenforced") {
    return networkPolicy;
  }
  return "unknown";
}
