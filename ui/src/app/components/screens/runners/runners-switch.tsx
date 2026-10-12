/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The runners switch (M-RN-1 RA5a/RA5b/RA5i/RA5j, super admin): turning on is a confirm whose refusal (the
// server's sentence, for example a plain-HTTP control plane) stays inside it; turning off is a destructive
// confirm. Both are one site-config write the server audits.
import * as React from "react";
import { runners as runnersApi } from "../../../lib/api/runners";
import { RUNNERS } from "../../wardyn/copy/runners";
import { ConfirmDialog } from "./confirm-dialog";

export function RunnersSwitchDialog({
  mode,
  onOpenChange,
  onChanged,
}: {
  /** Which confirm is open; null when neither. */
  mode: "on" | "off" | null;
  onOpenChange: (open: boolean) => void;
  onChanged: () => void;
}) {
  // The last open mode stays while the confirm closes, so its words do not flip during the exit.
  const last = React.useRef(mode);
  if (mode) last.current = mode;
  const on = (mode ?? last.current) !== "off";
  return (
    <ConfirmDialog
      open={mode !== null}
      onOpenChange={onOpenChange}
      title={on ? RUNNERS.TURN_ON_TITLE : RUNNERS.TURN_OFF_TITLE}
      body={[on ? RUNNERS.TURN_ON_BODY : RUNNERS.TURN_OFF_BODY]}
      confirmLabel={on ? RUNNERS.TURN_ON : RUNNERS.TURN_OFF}
      destructive={!on}
      errorTitle={on ? RUNNERS.TURN_ON_REFUSED_TITLE : undefined}
      onConfirm={() => runnersApi.setEnabled(on)}
      onDone={onChanged}
    />
  );
}
