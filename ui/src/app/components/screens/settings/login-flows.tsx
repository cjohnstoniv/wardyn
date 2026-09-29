/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Extracted from harness-login-pane.tsx: the per-provider login
// table is pure data plus one presentational component — nothing here reads
// or writes the pane's own state. Moved out so the pane, which now carries
// three lanes' worth of logic (0.7.5's P5 wait, 0.7.6's starting-detail
// reason, Finding 7a's tab and Finding 7b's watch), stays under the size cap.
// Re-exported from the pane (`export { loginFlow, LOGIN_FLOWS }`) so this
// pane's own tests keep their import path.

// Per-provider login conventions. Adding a provider is a new row here (mirrors
// the server-side agentHarnessLogin table), not a forked component.
//
// The two flows differ in how the credential comes back:
//   · anthropic — `claude setup-token` prints the token, so we scrape it off the
//     PTY and PUT it (capture: "scrape").
//   · aws — `aws sso login` writes its token to ~/.aws/sso/cache/*.json and
//     prints only a short-lived device code + verification URL. The in-sandbox
//     `wardyn-aws-sso` helper uploads the file through the brokered internal
//     endpoint, so the pane never sees (and must never scrape) a credential —
//     it just watches for the helper's success marker (capture: "helper").
export type CaptureMode = "scrape" | "helper";
export type LoginFlow = {
  cmd: string;
  // The provider's short name in the door's progress copy (#628): "Waiting
  // for AWS", "Open Claude sign-in".
  providerName: string;
  capture: CaptureMode;
  // What the "done" phase's success line names as connected — provider-specific
  // so an AWS SSO capture never claims a Claude subscription (or vice versa).
  doneLabel: string;
  // Marker the in-sandbox helper prints on success (capture: "helper" only).
  doneMarker?: string;
  // Marker the in-sandbox helper prints on a refused capture (capture: "helper"
  // only). Unused for now — cmd/wardyn-aws-sso's TestFailMarker_UIParity reads
  // this literal by source parse, the same way TestSuccessMarker_UIParity reads
  // doneMarker above, so it stays byte-identical across the two languages.
  failMarker?: string;
};

export const LOGIN_FLOWS: Record<string, LoginFlow> = {
  anthropic: {
    cmd: "claude setup-token",
    providerName: "Claude",
    capture: "scrape",
    doneLabel: "your Claude subscription is connected",
  },
  aws: {
    // --sso-session wardyn selects the [sso-session wardyn] block the server
    // seeded into ~/.aws/config; chained so the helper uploads the moment the
    // login succeeds — the operator never has to run a second command.
    cmd: "aws sso login --sso-session wardyn --no-browser --use-device-code && wardyn-aws-sso",
    providerName: "AWS",
    doneLabel: "your AWS SSO session is connected",
    capture: "helper",
    doneMarker: "wardyn: aws sso credential captured",
    failMarker: "wardyn: aws sso credential rejected:",
  },
};

export function loginFlow(provider: string): LoginFlow {
  return LOGIN_FLOWS[provider] ?? LOGIN_FLOWS.anthropic;
}
