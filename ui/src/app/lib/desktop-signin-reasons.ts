/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Every reason the desktop sign-in routes answer with (internal/api/reasons_routes.go,
// `reasonDesktopSignin*`), in console words. A reason is a wire value: the console
// matches on it and shows the sentence, never the code. desktop-signin-reasons.test.ts
// reads the Go constants and fails on a reason with no entry here.
//
// Where each one reaches a person: an API refusal body on the /runs/{id}/desktop-signin
// routes (the HttpError's `reason`), or, for desktop_signin_resume_failed, the `reason`
// on a run.kill row, which the run page's failure block shows (failure-block.tsx).

// The one reason no person ever meets: the run proxy's own check answers it (POST /internal/desktop-signin/check).
// The guard in desktop-signin-reasons.test.ts exempts it by this name alone.
export const PROXY_INTERNAL_REASONS = ["desktop_signin_credential_refused"] as const;

export const DESKTOP_SIGNIN_REASON_LINE: Readonly<Record<string, string>> = {
  desktop_signins_off: "Sign-ins in a desktop are turned off on this deployment. Ask an admin to turn them on.",
  desktop_signin_not_running: "This run isn't running, so there is no desktop to sign in to.",
  desktop_signin_no_desktop: "This run has no desktop: its policy doesn't include the desktop app.",
  desktop_signin_runner_unsupported:
    "This run started without desktop sign-ins. They work only on this deployment's own Docker runner, in the sign-in desktop image, on a run started while sign-ins were on.",
  desktop_signin_image_not_granted: "Sign-ins need the sign-in desktop image, which your access doesn't include.",
  desktop_signin_open: "A sign-in is already open on this run.",
  desktop_signin_not_open: "No sign-in is open on this run.",
  desktop_signin_not_holder: "Someone else began this sign-in, so you can't change it.",
  desktop_signin_pause_failed:
    "Wardyn couldn't confirm the agent had stopped, so the sign-in didn't start and the agent carries on. Try again in a moment.",
  desktop_signin_start_failed: "The sign-in desktop didn't start, and the agent was resumed. Try again.",
  desktop_signin_end_failed:
    "Wardyn couldn't confirm the sign-in desktop closed. The agent stays stopped, and Wardyn keeps trying to close it.",
  desktop_signin_site_invalid: "Enter the site's domain, such as example.com.",
  desktop_signin_share_failed: "That site's login couldn't be shared with the agent. Nothing was shared.",
  desktop_signin_unavailable: "Wardyn couldn't read or save this run's sign-in just now. Try again.",
  desktop_signin_resume_failed:
    "Wardyn couldn't resume the agent after a sign-in, so it ended the run rather than leave it frozen. Start the run again.",
};

/** The sentence for a sign-in reason, undefined for any other value. */
export const desktopSigninReasonLine = (reason: string | undefined): string | undefined =>
  reason !== undefined && Object.prototype.hasOwnProperty.call(DESKTOP_SIGNIN_REASON_LINE, reason) ? DESKTOP_SIGNIN_REASON_LINE[reason] : undefined;
