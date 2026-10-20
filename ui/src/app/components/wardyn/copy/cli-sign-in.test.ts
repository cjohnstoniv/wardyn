/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The strings the owner approved in mock round M-ATT r8 (boards MATT2-Approve, MATT2-States, MATT3-SignedIn,
// MATT3-States), character for character: each row is the literal the board draws against the key that renders it.
import { describe, expect, it } from "vitest";
import { approveRefusal, SIGN_IN, SIGNED_IN_CLIS, stampOf } from "./cli-sign-in";

describe("the /cli/sign-in page", () => {
  it.each<[string, string]>([
    ["Wardyn CLI sign-in", SIGN_IN.EYEBROW],
    ["Sign in the Wardyn CLI?", SIGN_IN.TITLE],
    ["A terminal asked to sign in as you@example.com. Approve only if you just ran wardyn login yourself.", SIGN_IN.LEAD("you@example.com")],
    ["Computer", SIGN_IN.COMPUTER],
    ["Requested from", SIGN_IN.FROM],
    ["Requested", SIGN_IN.REQUESTED],
    ["Expires in 10 minutes", SIGN_IN.EXPIRES_IN(600)],
    ["19 Oct 2026, 18:42 UTC", stampOf("2026-10-19T18:42:07Z")],
    ["Code from your terminal", SIGN_IN.CODE],
    ["Approve only if your terminal printed this same code.", SIGN_IN.CODE_NOTE],
    ["What this allows", SIGN_IN.ALLOWS],
    [
      "A CLI session for you: 1-hour access that refreshes. It ends after 30 days, or after 7 days without use. It acts with your user role only.",
      SIGN_IN.GRANT(3600, 30 * 86400, 7 * 86400),
    ],
    ["Approve", SIGN_IN.APPROVE],
    ["Deny", SIGN_IN.DENY],
    ["This request came from a different address than this browser.", SIGN_IN.MISMATCH_TITLE],
    ["If you did not start this sign-in yourself, choose Deny.", SIGN_IN.MISMATCH_BODY],
    ["Not the address of this browser", SIGN_IN.NOT_THIS_BROWSER],
    ["Admin access for 1 hour", SIGN_IN.ADMIN_TITLE(3600)],
    [
      "A terminal asked to act as you with your admin role. It lasts 1 hour and does not refresh. Approve only if you just ran the command yourself.",
      SIGN_IN.ADMIN_LEAD(3600),
    ],
    [
      "Admin access for 1 hour. It does not refresh, and it ends sooner if you sign this computer out. The CLI session itself stays a user session.",
      SIGN_IN.ADMIN_GRANT(3600),
    ],
    ["Approved. Paste this code into your terminal.", SIGN_IN.PASTE_TITLE],
    ["Copy code", SIGN_IN.PASTE_COPY],
    ["Paste this only into a terminal you started yourself.", SIGN_IN.PASTE_WARN],
    ["Shown once. It works once, for 5 minutes. After that, run wardyn login again.", SIGN_IN.PASTE_ONCE(300)],
    ["Denied", SIGN_IN.DENIED_TITLE],
    ["Nothing was signed in. You can close this tab.", SIGN_IN.DENIED_BODY],
    ["This sign-in request expired", SIGN_IN.EXPIRED_TITLE],
    ["Requests last 10 minutes. Run wardyn login again in your terminal.", SIGN_IN.EXPIRED_BODY],
    ["This sign-in request was already used", SIGN_IN.USED_TITLE],
    ["Run wardyn login again if you need a new one.", SIGN_IN.USED_BODY],
    ["Exit the user view to sign in a CLI.", SIGN_IN.REFUSED_USER_VIEW],
    ["CLI sign-in isn't available to you. Ask your admin.", SIGN_IN.REFUSED_NOT_ALLOWED],
  ])("%s", (board, actual) => expect(actual).toBe(board));

  it("words the operator's lifetimes, not the defaults", () => {
    expect(SIGN_IN.GRANT(1800, 7 * 86400, 86400)).toBe(
      "A CLI session for you: 30-minute access that refreshes. It ends after 7 days, or after 1 day without use. It acts with your user role only.",
    );
  });

  // DESIGN-ATTACH 11.14 #10: the server's reason codes and texts stay; the page words these two for a sign-in.
  it("maps the two mintGuards refusals by reason and leaves every other to the server's own text", () => {
    expect(approveRefusal("api_token_member_mode_mint")).toBe("Exit the user view to sign in a CLI.");
    expect(approveRefusal("capability_feature")).toBe("CLI sign-in isn't available to you. Ask your admin.");
    expect(approveRefusal("cli_admin_needs_session")).toBe("");
    expect(approveRefusal("")).toBe("");
  });
});

describe("Signed-in CLIs card", () => {
  it.each<[string, string]>([
    ["Signed-in CLIs", SIGNED_IN_CLIS.TITLE],
    ["No computers signed in", SIGNED_IN_CLIS.SUMMARY(0)],
    ["1 computer", SIGNED_IN_CLIS.SUMMARY(1)],
    ["2 computers", SIGNED_IN_CLIS.SUMMARY(2)],
    ["Computers where you signed in with the Wardyn CLI. Signing one out ends its session now.", SIGNED_IN_CLIS.LEDE],
    ["Computer", SIGNED_IN_CLIS.COL_COMPUTER],
    ["Store", SIGNED_IN_CLIS.COL_STORE],
    ["Signed in", SIGNED_IN_CLIS.COL_SIGNED_IN],
    ["Ends", SIGNED_IN_CLIS.COL_ENDS],
    ["Idle end", SIGNED_IN_CLIS.COL_IDLE_END],
    ["CLI sign-in", SIGNED_IN_CLIS.CHIP],
    ["Keychain", SIGNED_IN_CLIS.STORE_KEYCHAIN],
    ["File, 0600", SIGNED_IN_CLIS.STORE_FILE],
    ["No keychain here: the sign-in is kept in a file only you can read, so it ends after 7 days.", SIGNED_IN_CLIS.FILE_NOTE],
    ["Admin access, ends in 42 min", SIGNED_IN_CLIS.ADMIN_CHIP(42)],
    ["Sign out", SIGNED_IN_CLIS.SIGN_OUT],
    ["Sign out mbp-dev?", SIGNED_IN_CLIS.DIALOG_TITLE("mbp-dev")],
    ["The Wardyn CLI on mbp-dev stops working now. Run wardyn login there to sign in again.", SIGNED_IN_CLIS.DIALOG_BODY("mbp-dev")],
    ["Signed out mbp-dev.", SIGNED_IN_CLIS.TOAST("mbp-dev")],
    ["No computers are signed in with the Wardyn CLI.", SIGNED_IN_CLIS.EMPTY],
    ["To sign in another computer, run", SIGNED_IN_CLIS.RUN_LEAD],
    ["wardyn login https://wardyn.example.com", SIGNED_IN_CLIS.RUN_COMMAND("https://wardyn.example.com")],
  ])("%s", (board, actual) => expect(actual).toBe(board));
});
