/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Does this DEPLOYMENT ever capture a session at all?
//
// /healthz's components.recording is the honest signal (cmd/wardynd's
// boot_deps.go): "none" means the recording store never came up — a stock Helm
// install leaves persistence.enabled=false, so no run on that server will ever
// produce a cast. It is a BOOT-TIME fact, not something that changes while a
// screen is open, so it is read once per mount and never polled.
//
// It is a hook because three surfaces need the same answer and two of them had
// already hand-rolled it (screens/recording.tsx, screens/run-detail.tsx). The
// third is the New Run rail, which used to promise "Every keystroke and every
// outbound connection" unconditionally — wrong out of the box on a stock
// install, and wrong in both dangerous directions: someone relying on recording
// for after-the-fact review does not have it, and a member who assumes they are
// not recorded may be.
//
// FALSE WHILE UNKNOWN, deliberately and unchanged from the two reads it
// replaces: a fetch that has not landed (or failed — health.health swallows a
// failure into {}) is not evidence that recording is off, and the surfaces here
// all render their ordinary state in that case.
import * as React from "react";
import { health } from "../api/health";

export function useRecordingDisabled(): boolean {
  const [disabled, setDisabled] = React.useState(false);
  React.useEffect(() => {
    let alive = true;
    health.health().then((h) => {
      if (alive && h.components?.recording?.selected === "none") setDisabled(true);
    });
    return () => {
      alive = false;
    };
  }, []);
  return disabled;
}
