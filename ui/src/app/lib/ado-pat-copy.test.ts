/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Pins every string of the approved per-person Azure DevOps token mock to its
// exact characters (#1428, #1430). The literals below are the canon; a reworded
// sentence in ado-pat-copy.ts fails here instead of shipping. MINTED_SETUP_REDIRECT
// is the one line that is not in the mock (the plan review's finding F5).
import { describe, it, expect } from "vitest";
import { ADO_PAT, gettingStartedOwnChip } from "./ado-pat-copy";

describe("ADO_PAT copy canon", () => {
  it("plain strings, character for character", () => {
    expect(ADO_PAT.SECTION_TITLE).toBe("How people connect to Azure DevOps");
    expect(ADO_PAT.MODE_MINTED).toBe("Wardyn creates a short-lived token for each run (recommended)");
    expect(ADO_PAT.MODE_MINTED_HELP).toBe("Each person connects once. For every run, Wardyn creates a personal access token in that person's name with only the access the run was given, and revokes it when the run ends.");
    expect(ADO_PAT.MINTED_SETUP).toBe("Wardyn creates each person's tokens with the app registration your people already sign in with. In Entra, add two Azure DevOps permissions to it (vso.pats and vso.pats_manage) and grant admin consent.");
    expect(ADO_PAT.MINTED_SETUP_REDIRECT).toBe("Register the console redirect under the Web platform, then add the secret.");
    expect(ADO_PAT.TOKEN_LIFE_LABEL).toBe("Longest token life");
    expect(ADO_PAT.TOKEN_LIFE_UNIT).toBe("hours");
    expect(ADO_PAT.TOKEN_LIFE_HINT).toBe("A longer run gets a fresh token before this one expires. Keep it at or below your organisation's maximum token lifespan.");
    expect(ADO_PAT.TOKEN_LIFE_RANGE).toBe("Enter 1 to 168 hours.");
    expect(ADO_PAT.MODE_BEARER).toBe("Use each person's Entra sign-in");
    expect(ADO_PAT.MODE_BEARER_HELP).toBe("No token is created in Azure DevOps. The sign-in carries every Azure DevOps permission the person consented to; Wardyn checks each request against the run's access.");
    expect(ADO_PAT.MODE_OWN).toBe("Each person adds their own token");
    expect(ADO_PAT.MODE_OWN_HELP).toBe("For organisations that can't register an app. Wardyn checks the token is theirs and stops using it at the expiry they enter. Wardyn can't revoke it.");
    expect(ADO_PAT.OWN_EXPIRY_LABEL).toBe("Longest expiry");
    expect(ADO_PAT.OWN_EXPIRY_UNIT).toBe("days");
    expect(ADO_PAT.OWN_EXPIRY_RANGE).toBe("Enter 1 to 90 days.");
    expect(ADO_PAT.RECOMMENDED_SETTINGS).toBe("Recommended in Azure DevOps (Organization settings → Microsoft Entra): Restrict full-scoped personal access token creation — On. Enforce maximum personal access token lifespan — On.");
    expect("SIGNIN_SECTION_TITLE" in ADO_PAT).toBe(false);
    expect(ADO_PAT.CHECK_BUTTON).toBe("Check organisation settings");
    expect(ADO_PAT.CHECK_TITLE).toBe("Organisation settings");
    expect(ADO_PAT.CHECK_PERMS_OK).toBe("Your app registration has both Azure DevOps token permissions.");
    expect(ADO_PAT.CHECK_PERMS_MISSING).toBe("Your app registration doesn't have the Azure DevOps token permissions yet. Add vso.pats and vso.pats_manage and grant admin consent.");
    expect(ADO_PAT.CHECK_LIFESPAN_OFF).toBe("Maximum token lifespan is off in Azure DevOps. A stolen connection could create tokens that last up to a year. Turn it on under Organization settings → Microsoft Entra.");
    expect(ADO_PAT.NO_CLIENT_SECRET).toBe("Per-run tokens need this row to use Wardyn's own sign-in app, and that app to have a client secret. Name Wardyn's app here and set WARDYN_OIDC_CLIENT_SECRET, or choose another way to connect.");
    expect(ADO_PAT.POLICY_BANNER_BUTTON).toBe("Switch to Entra sign-in");
    expect(ADO_PAT.BEARER_WITH_TOKEN_PERMS).toBe("Your app registration holds the Azure DevOps token permissions, so Entra sign-in can't be used for runs: its tokens would let a run create tokens. Remove vso.pats and vso.pats_manage from the app first, or keep Wardyn creating a token for each run.");
    expect(ADO_PAT.MEMBER_NOT_CONNECTED).toBe("Connect once so Wardyn can create a short-lived token for each of your runs. Each token has only that run's access and is revoked when the run ends.");
    expect(ADO_PAT.MEMBER_CONNECT).toBe("Connect Azure DevOps");
    expect(ADO_PAT.MEMBER_CONNECTED).toBe("Tokens are created in your name, one per run, and revoked when it ends. Azure DevOps lists them under Personal access tokens as 'Wardyn run …'.");
    expect(ADO_PAT.MEMBER_DISCONNECT).toBe("Disconnect");
    expect(ADO_PAT.DISCONNECT_TITLE).toBe("Disconnect Azure DevOps?");
    expect(ADO_PAT.DISCONNECT_BODY).toBe("Disconnecting revokes the tokens of any of your runs in progress. They lose Azure DevOps access.");
    expect(ADO_PAT.DISCONNECT_CANCEL).toBe("Cancel");
    expect(ADO_PAT.CHIP_NOT_CONNECTED).toBe("Not connected");
    expect(ADO_PAT.CHIP_CONNECTED).toBe("Connected");
    expect(ADO_PAT.CHIP_SIGN_IN_AGAIN).toBe("Sign in again");
    expect(ADO_PAT.CHIP_BLOCKED).toBe("Blocked by your organisation");
    expect(ADO_PAT.SIGN_IN_AGAIN_BODY).toBe("Your organisation asked you to sign in again before Wardyn can create tokens.");
    expect(ADO_PAT.BLOCKED_BODY).toBe("Your organisation doesn't let you create personal access tokens. Ask an Azure DevOps administrator to add you to the allow list.");
    expect(ADO_PAT.LAUNCH_NOT_CONNECTED).toBe("Connect Azure DevOps once before launching; Wardyn creates the run's token from that connection.");
    expect(ADO_PAT.LAUNCH_POLICY_REFUSED).toBe("Azure DevOps refused to create a token for this run: your organisation restricts who can create personal access tokens. Ask an Azure DevOps administrator to add you to the allow list.");
    expect(ADO_PAT.NEWRUN_LINE_PREFIX).toBe("Azure DevOps: a token for this run with ");
    expect(ADO_PAT.NEWRUN_LINE_SUFFIX).toBe(". It's revoked when the run ends.");
    expect(ADO_PAT.RUN_TOKEN_TITLE).toBe("Azure DevOps");
    expect(ADO_PAT.RUN_PAUSED).toBe("Paused: this run's token was revoked. A new one is created when the run resumes.");
    expect(ADO_PAT.APPROVAL_WIDENS).toBe("Approving adds this access to the run's token for the rest of this run, even for a one-time approval.");
    expect(ADO_PAT.OWN_ADD_CTA).toBe("Add your personal access token");
    expect(ADO_PAT.OWN_DIALOG_TITLE).toBe("Add your personal access token");
    expect(ADO_PAT.OWN_OPEN_TOKENS).toBe("Open Azure DevOps tokens");
    expect(ADO_PAT.OWN_FIELD_TOKEN).toBe("Token");
    expect(ADO_PAT.OWN_FIELD_EXPIRES).toBe("Expires on");
    expect(ADO_PAT.OWN_DIALOG_ADD).toBe("Add token");
    expect(ADO_PAT.OWN_DIALOG_CANCEL).toBe("Cancel");
    expect(ADO_PAT.OWN_MISMATCH).toBe("This token belongs to a different Azure DevOps account than yours.");
    expect(ADO_PAT.OWN_REJECTED).toBe("Azure DevOps didn't accept this token.");
    expect(ADO_PAT.OWN_CHIP_EXPIRED).toBe("Expired");
    expect(ADO_PAT.OWN_CHIP_REFUSED).toBe("Refused");
    expect(ADO_PAT.OWN_REPLACE).toBe("Replace token");
    expect(ADO_PAT.OWN_EXPIRED_BODY).toBe("Your runs can't reach Azure DevOps until you add a new token.");
    expect(ADO_PAT.OWN_SERVER_TITLE).toBe("Azure DevOps Server");
    expect(ADO_PAT.OWN_SERVER_NOTE).toBe("Git only. Azure DevOps Server has no Entra sign-in.");
    expect(ADO_PAT.CONVERTED_CHECKLIST).toBe("Azure DevOps no longer uses one shared token. Choose how people connect.");
    expect(ADO_PAT.CONVERTED_CHOOSE).toBe("Choose");
    expect(ADO_PAT.CONVERTED_NOTE).toBe("Azure DevOps no longer uses one shared token. Choose how people connect, then turn this row on. Until then, runs can't clone from this organisation.");
    expect(ADO_PAT.CONVERTED_SAVE_ON).toBe("Save and turn on");
  });

  it("placeholder strings, character for character", () => {
    expect(ADO_PAT.CHECK_LAST("09:12")).toBe("Last checked 09:12");
    expect(ADO_PAT.CHECK_CHIP("09:12")).toBe("Checked 09:12");
    expect(ADO_PAT.CHECK_LIFESPAN_ON(8)).toBe("Maximum token lifespan is on, and tokens of 8 hours are allowed.");
    expect(ADO_PAT.LIFESPAN_REFUSAL(24)).toBe("Longest token life is above your organisation's maximum token lifespan. Lower it to 24 hours or less.");
    expect(ADO_PAT.LIFESPAN_REFUSAL(undefined)).toBe("Longest token life is above your organisation's maximum token lifespan. Lower it.");
    expect(ADO_PAT.POLICY_BANNER("Priya Shah")).toBe("Azure DevOps refused to create a token for Priya Shah: your organisation restricts who can create personal access tokens. Add the people who use Wardyn to that policy's allow list, or switch to Entra sign-in.");
    expect(ADO_PAT.CHECK_LIFESPAN_UNKNOWN("invalidValidTo")).toBe("Wardyn couldn't tell whether your organisation's maximum token lifespan is on. Azure DevOps answered: invalidValidTo.");
    expect(ADO_PAT.MEMBER_NEEDS_ADMIN).toBe("Your administrator needs to finish setting up Azure DevOps before runs can use it.");
    expect(ADO_PAT.RUN_TOKEN_LINE_NOTE("09:02", "17:02", "renewed")).toBe("Azure DevOps token: created 09:02 · expires 17:02 (renewed)");
    expect(ADO_PAT.MEMBER_LAST_TOKEN("09:02", "09:41")).toBe("Last token: created 09:02, revoked 09:41.");
    expect(ADO_PAT.RUN_TOKEN_LINE("09:02", "17:02")).toBe("Azure DevOps token: created 09:02 · expires 17:02");
    expect(ADO_PAT.RUN_TOKEN_LINE_REVOKED("09:02", "17:02", "09:41", "run ended")).toBe("Azure DevOps token: created 09:02 · expires 17:02 · revoked 09:41 (run ended)");
    expect(ADO_PAT.RUN_RENEWAL_FAILED("17:02")).toBe("Wardyn couldn't renew this run's token, so it stops working at 17:02. Sign in to Azure DevOps again to keep this run going.");
    expect(ADO_PAT.RUN_REVOKE_FAILED("17:02")).toBe("Wardyn couldn't revoke this run's token. It expires at 17:02; revoke it in Azure DevOps under Personal access tokens.");
    expect(ADO_PAT.RUN_ACCESS_ADDED("09:20", "Contribute to pull requests")).toBe("Access added 09:20: Contribute to pull requests (new token)");
    expect(ADO_PAT.OWN_DIALOG_LEAD("wardyn-live-test", 30)).toBe("In Azure DevOps, create a token for wardyn-live-test only, with these scopes and an expiry within 30 days, then paste it here.");
    expect(ADO_PAT.OWN_TOO_LONG(30)).toBe("This token expires after the 30-day limit your administrator set.");
    expect(ADO_PAT.OWN_CHIP_EXPIRING(3)).toBe("Expires in 3 days");
    expect(ADO_PAT.OWN_EXPIRING_LINE("wardyn-live-test", "27 October")).toBe("Your token for wardyn-live-test expires on 27 October.");
    expect(ADO_PAT.OWN_REFUSED_LINE("2 October", "27 October")).toBe("Azure DevOps refused this token on 2 October, before it expires on 27 October. Replace it.");
    expect(ADO_PAT.CONVERTED_CEILING("View projects & teams, Read code")).toBe("Read-only, carried over from the retired token: View projects & teams, Read code.");
  });

  it("revoke reasons: what the server writes (ado_run_pats.revoke_reason), in plain words", () => {
    expect(ADO_PAT.REVOKE_REASON).toEqual({
      run_end: "run ended",
      kill: "run stopped",
      pause: "paused",
      drift: "access changed",
      disconnect: "disconnected",
      sweep: "cleaned up after a restart",
      upstream_401: "rejected by Azure DevOps",
      offboarding: "person removed",
    });
  });

  it("superseded and expired tokens are never called revoked", () => {
    expect(ADO_PAT.RUN_TOKEN_NOTE).toEqual({ renewed: "renewed", access_added: "access added", expired: "expired" });
  });
});

// The approved Getting started own-token chip packet: Settings' chip words after
// the row's "Azure DevOps ·" prefix.
describe("Getting started's own-token chip labels", () => {
  it("the four labels, character for character", () => {
    expect(gettingStartedOwnChip("connected")).toBe("Azure DevOps · Connected");
    expect(gettingStartedOwnChip("expiring", 6)).toBe("Azure DevOps · Expires in 6 days");
    expect(gettingStartedOwnChip("refused")).toBe("Azure DevOps · Refused");
    expect(gettingStartedOwnChip("expired")).toBe("Azure DevOps · Expired");
  });
});

// #1488: the approved Remove strings, character for character. The confirm's
// second line is the owner's 2026-10-01 ruling, replacing the packet's.
describe("ADO_PAT remove canon", () => {
  it("strings", () => {
    expect(ADO_PAT.OWN_REMOVE).toBe("Remove from Wardyn");
    expect(ADO_PAT.OWN_REMOVE_TITLE("wardyn-live-test")).toBe("Remove your token for wardyn-live-test?");
    expect(ADO_PAT.OWN_REMOVE_BODY).toBe(
      "This deletes Wardyn's copy of your token. It does not revoke the token in Azure DevOps, so revoke it there too.",
    );
    expect(ADO_PAT.OWN_OPEN_TOKENS).toBe("Open Azure DevOps tokens");
    expect(ADO_PAT.OWN_REMOVE_RUNS).toBe(
      "Runs already using it keep it for up to 10 minutes; revoke it in Azure DevOps to stop them now.",
    );
    expect(ADO_PAT.OWN_REMOVE_CANCEL).toBe("Cancel");
    expect(ADO_PAT.OWN_REMOVE_PENDING).toBe("Removing…");
    expect(ADO_PAT.OWN_REMOVED_TOAST("wardyn-live-test")).toBe(
      "Your token for wardyn-live-test was removed from Wardyn. It isn't revoked in Azure DevOps.",
    );
    expect(ADO_PAT.OWN_REMOVE_FAILED_TOAST("wardyn-live-test")).toBe(
      "Couldn't remove your token for wardyn-live-test. It's still stored in Wardyn. Try again.",
    );
  });
});
