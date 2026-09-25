/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Does this deployment ever capture a session at all?
//
// /healthz's components.recording is the honest signal (cmd/wardynd's
// boot_deps.go): "none" means the recording store never came up — a stock Helm
// install leaves persistence.enabled=false, so no run on that server will ever
// produce a cast. It is a boot-time fact, not something that changes while a
// screen is open, so it is read once per mount and never polled.
//
// It is a hook because three surfaces need the same answer and two of them had
// already hand-rolled it (screens/recording.tsx, screens/run-detail.tsx). The
// third is the New Run rail, which must not promise "Every keystroke and every
// outbound connection" unconditionally — wrong out of the box on a stock
// install, and wrong in both dangerous directions: someone relying on recording
// for after-the-fact review does not have it, and a member who assumes they are
// not recorded may be.
//
// Tri-state, and that is the point. `undefined` means unknown: the fetch has not
// landed, or it failed (health.health swallows a failure into {}), or this
// daemon's /healthz carries no `recording.selected` at all. A caller that
// collapses unknown to `false` goes on asserting "every keystroke and every
// outbound connection" over a deployment that records nothing — which is the
// same shape of defect as the credentials line this lane exists to remove, one
// surface over. The two list/cockpit callers compare `=== true` (an empty state
// stays as it was until the answer arrives); the New Run rail, which makes a
// promise rather than explaining an absence, renders no sentence at all while
// this is undefined.
import * as React from "react";
import { health } from "../api/health";

export function useRecordingDisabled(): boolean | undefined {
  const [disabled, setDisabled] = React.useState<boolean | undefined>(undefined);
  React.useEffect(() => {
    let alive = true;
    void health.health().then((h) => {
      const selected = h.components?.recording?.selected;
      // An absent field is unknown, not "on": only a value we actually read
      // settles this either way.
      if (alive && selected) setDisabled(selected === "none");
    });
    return () => {
      alive = false;
    };
  }, []);
  return disabled;
}
