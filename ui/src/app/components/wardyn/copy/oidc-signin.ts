/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Signing in to a model provider that signs people in at its own authority (a custom endpoint with sign_in
// "oidc" on the setup status): the strip, the connection row, the door and the window it returns to, Disconnect,
// and the Review line. A pure-data module on the shell's eager graph, so it imports nothing.
export const OIDC_SIGN_IN = {
  STRIP_NONE: (harness: string, name: string) => `${harness} runs use ${name}, and you are not signed in.`,
  STRIP_ENDED: (name: string) => `Your sign-in to ${name} has ended.`,
  BUTTON: (name: string) => `Sign in to ${name}`,
  SIGN_IN: "Sign in",
  SIGN_IN_AGAIN: "Sign in again",
  DISCONNECT: "Disconnect",
  NOT_BOUND: "Not bound to your sign-in",
  NOT_BOUND_EXPLAIN: "Uses the account you connected here.",
  REVIEW_NOT_BOUND: (name: string) => `${name} is not bound to your sign-in. The run uses the account you connected there.`,

  // A stale Add-your-token door whose provider now signs people in: the title becomes DOOR_TITLE and this is the line.
  PASTE_REFUSED: "Your admin changed this provider to use sign-in.",
  DOOR_TITLE: (name: string) => `Sign in to ${name}`,
  INTRO:
    "You'll sign in with your own account in a new window. Wardyn keeps a refresh grant and issues the run a short-lived credential when it starts. The sandbox never sees your sign-in.",
  // The setup status carries no id to start this sign-in with (a console older than its server): nothing to continue.
  NOT_READY: "Wardyn couldn't start this sign-in. Reload the page and try again.",
  CONTINUE: "Continue to sign in",
  WAITING: "Finish signing in in the new window. This page carries on when you are done.",
  BLOCKED: "Your browser blocked the sign-in window.",
  BLOCKED_LINK: (name: string) => `Open the ${name} sign-in`,
  DONE_TOAST: (name: string) => `Signed in to ${name}. Your runs can use it now.`,
  FAIL_TITLE: (name: string) => `Couldn't sign you in to ${name}`,
  RETRY: "Try again",
  // The failure sentences, by what the server's redirect named (provider_oidc_error) or the door saw itself.
  FAIL: {
    timeout: () => "The sign-in took too long, or its window was closed. Start it again.",
    refused: (name: string) => `${name} refused the sign-in. If you expected access, ask your admin.`,
    row_changed: (name: string) => `An admin changed how ${name} signs people in while you were signing in. Start again.`,
    issuer_mismatch: (name: string) =>
      `${name} signed you in through a different service from the one you use for Wardyn, so Wardyn can't tell the account is yours. Ask your admin.`,
    identity_binding: (name: string) =>
      `That account at ${name} isn't the one you use in Wardyn. Sign in with your own account.`,
    unusable_grant: (name: string) =>
      `${name} didn't give Wardyn a sign-in it can renew, so it can't keep you signed in. Ask your admin to allow offline access for the Wardyn client.`,
    store_error: () => "Wardyn couldn't save your sign-in. Try again.",
    unreachable: (name: string) => `Wardyn couldn't reach ${name}. Try again in a moment.`,
  },
  // Where the sign-in service sends the person back to a console that has no window to close. The redirect carries
  // the provider row's uid, which setup status names; a uid it does not list reads as BANNER_FAIL_NAME / BANNER_OK.
  BANNER_OK: "Signed in. Your runs can use it now.",
  BANNER_OK_NAMED: (name: string) => `Signed in to ${name}. Your runs can use it now.`,
  BANNER_FAIL_NAME: "this provider",
  // The window the sign-in service sends the person back to.
  WINDOW_OK: (name: string) => `Signed in to ${name}. You can close this window.`,
  WINDOW_FAIL: (name: string) => `Couldn't sign you in to ${name}. Close this window and try again from Wardyn.`,

  DISCONNECT_TITLE: (name: string) => `Disconnect ${name}?`,
  DISCONNECT_BODY: (name: string) =>
    `Runs you start after this are refused until you sign in again. Runs already going are held when their current token ends, and carry on once you sign in. Wardyn also asks ${name} to end your sign-in where it can.`,
  DISCONNECT_TOAST: (name: string) => `Disconnected from ${name}`,
  CANCEL: "Cancel",

  // Held runs (Approvals): provider-named, because the AWS wording would be false here.
  APPROVAL_UNNAMED: "Model provider",
  APPROVAL_LABEL: (name: string) => `${name} sign-in needed`,
  APPROVAL_HEADING: (name: string) => `${name} sign-in needed. Sign in to let this run's model calls through`,
  APPROVAL_CHIP: "Sign-in",
  APPROVAL_OTHER: (person: string) => `Only ${person} can sign in again. The run uses ${person}'s sign-in.`,
} as const;

// The query the server's callback lands on: connected, or an error reason, and the provider's uid.
export const OIDC_RETURN = {
  DONE_PARAM: "provider_oidc",
  DONE_VALUE: "connected",
  ERROR_PARAM: "provider_oidc_error",
  PROVIDER_PARAM: "provider",
  MESSAGE: "wardyn.provider_oidc",
} as const;

export type OidcFailure = keyof typeof OIDC_SIGN_IN.FAIL;

/** The sentence a callback reason reads as. consent_required and interaction_required are the authority declining;
 *  exchange_failed and any code this console has not met read as an unreachable service, the one
 *  failure a retry is the honest answer to. */
export function oidcFailureKind(reason: string): OidcFailure {
  switch (reason) {
    case "consent_required":
    case "interaction_required":
      return "refused";
    case "row_changed":
      return "row_changed";
    case "identity_issuer_mismatch":
      return "issuer_mismatch";
    case "identity_binding":
      return "identity_binding";
    case "store_error":
      return "store_error";
    case "unusable_grant":
      return "unusable_grant";
    default:
      return "unreachable";
  }
}
