/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The no-credential preview (0.7.5): the User view plus one thing, the admin's
// own AWS sign-in reading as absent. Entered from the Permissions header, left
// through its band. The plain User view has no band: the switch is its
// indicator (packet M-A). This one keeps a band because sign-in is refused
// inside it, which is abnormal.
//
// The server does the clamping (internal/auth/oidc's contextWithPrincipal) and
// grants the posture only where the model-access roster row is per_user
// (/me's user_preview_available); nothing here gates anything.

import * as React from "react";
import { EyeOff } from "lucide-react";
import { Button } from "../ui/button";
import { switchView, viewHome } from "./console-view";
import { CONSOLE_VIEW, USER_PREVIEW } from "./copy/console-view";

// False unless the shell says so: a Permissions screen mounted without the
// shell (every existing test) offers no preview.
const PreviewAvailableContext = React.createContext(false);
export const PreviewAvailableProvider = PreviewAvailableContext.Provider;

/** The Permissions header's way in. Rendered only for an SSO admin tier in the
 *  Admin view on a deployment where the preview hides something. */
export function PreviewAsNewUser() {
  const available = React.useContext(PreviewAvailableContext);
  const [busy, setBusy] = React.useState(false);
  const [failed, setFailed] = React.useState(false);
  if (!available) return null;
  return (
    <>
      {failed && (
        <span role="alert" className="text-xs text-danger">
          {CONSOLE_VIEW.SWITCH_FAILED}
        </span>
      )}
      <Button
        variant="outline"
        size="sm"
        disabled={busy}
        onClick={() => {
          setBusy(true);
          setFailed(false);
          switchView("user", viewHome("user"), true).catch(() => {
            setBusy(false);
            setFailed(true);
          });
        }}
      >
        {USER_PREVIEW.MENU_NEW}
      </Button>
    </>
  );
}

/** The preview's band, and the way out. Renders nothing outside the preview. */
export function UserPreviewBanner({ active }: { active: boolean }) {
  const [failed, setFailed] = React.useState(false);
  if (!active) return null;
  return (
    <div
      role="status"
      className="relative z-50 flex shrink-0 items-center gap-2 border-b border-border bg-warning-subtle px-4 py-2 text-sm text-warning"
    >
      <EyeOff className="size-4 shrink-0" />
      <span>{USER_PREVIEW.BANNER}</span>
      <button
        type="button"
        onClick={() => {
          setFailed(false);
          switchView("admin", viewHome("admin")).catch(() => setFailed(true));
        }}
        className="font-medium underline underline-offset-2"
      >
        {USER_PREVIEW.EXIT}
      </button>
      {failed && <span>{CONSOLE_VIEW.SWITCH_FAILED}</span>}
    </div>
  );
}
