/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Where do the Getting Started demo episodes stream from on THIS deployment?
//
// /healthz's demo_video_base_url is the honest signal (internal/api/healthz.go):
// "" (the default, unset WARDYN_DEMO_VIDEO_BASE_URL) or absent (an older
// daemon) both mean "no operator mirror configured" — episodeUrl's own default
// parameter already answers that case with the hardcoded GitHub base, so this
// hook only needs to report a value when one overrides it. Same shape as
// useRecordingDisabled: a boot-time fact, read once per mount, never polled.
import * as React from "react";
import { health } from "../api/health";

export function useDemoVideoBaseUrl(): string | undefined {
  const [base, setBase] = React.useState<string | undefined>(undefined);
  React.useEffect(() => {
    let alive = true;
    health.health().then((h) => {
      if (alive && h.demo_video_base_url) setBase(h.demo_video_base_url);
    });
    return () => {
      alive = false;
    };
  }, []);
  return base;
}
