/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The strings the owner approved in mock round M-SVC v2 (#1508), character for character: each row is the
// literal the mock draws against the key that renders it. A copy change is a change to this table.
import { describe, expect, it } from "vitest";
import { DISPOSITION, MINTED } from "./credentials";
import { dayOf, inWords, INVITATIONS, lifetimeWords, ROLE_WORDS, SERVICES, TOKEN_DIALOG } from "./service-tokens";

const NOW = Date.parse("2026-10-12T09:00:00Z");
const at = (ms: number) => new Date(NOW + ms).toISOString();
const H = 3_600_000;
const D = 86_400_000;

describe("Invitations card", () => {
  it.each<[string, string]>([
    ["Invitations", INVITATIONS.TITLE],
    ["2 waiting", INVITATIONS.SUMMARY(2)],
    ["An admin invited you to create an API token. Only you can claim an invitation, signed in as yourself.", INVITATIONS.LEDE],
    ["Invitation from security-lee@example.com", INVITATIONS.FROM("security-lee@example.com")],
    ["Expires in 2 days", INVITATIONS.EXPIRES(inWords(at(2 * D), NOW))],
    ["Expires in 12 hours", INVITATIONS.EXPIRES(inWords(at(12 * H), NOW))],
    ["Token name:", INVITATIONS.TOKEN_NAME],
    ["Lasts at most 30 days", INVITATIONS.LASTS(lifetimeWords(30 * 86400))],
    ["Claiming it also revokes", INVITATIONS.REVOKES_LEAD],
    [", a token an admin created for you.", INVITATIONS.REVOKES_TAIL],
    ["Claim", INVITATIONS.CLAIM],
    ["Invitations are unavailable", INVITATIONS.UNAVAILABLE_TITLE],
    ["This server does not manage invitations yet.", INVITATIONS.UNAVAILABLE_BODY],
    ["Loading invitations", INVITATIONS.LOADING],
    ["Invitation claimed.", INVITATIONS.CLAIMED_TOAST],
  ])("%s", (mock, actual) => expect(actual).toBe(mock));
});

describe("Service identities I sponsor card", () => {
  it.each<[string, string]>([
    ["Service identities I sponsor", SERVICES.TITLE],
    ["2 active, 2 not active", SERVICES.SUMMARY(2, 4)],
    ["0 active, 1 not active", SERVICES.SUMMARY(0, 1)],
    ["1 active", SERVICES.SUMMARY(1, 1)],
    ["A service identity is automation that runs under its own name, not yours. As its sponsor you create its tokens, see its runs and can stop them. An admin creates, suspends and retires it.", SERVICES.LEDE],
    ["Active", SERVICES.STATE_ACTIVE],
    ["Suspended", SERVICES.STATE_SUSPENDED],
    ["Retired", SERVICES.STATE_RETIRED],
    ["Sponsor inactive", SERVICES.STATE_SPONSOR_INACTIVE],
    ["Suspended by an admin. Its tokens are refused until an admin resumes it.", SERVICES.HINT_SUSPENDED],
    ["Retired. Its tokens were revoked.", SERVICES.HINT_RETIRED],
    ["Your sign-in is not active as its sponsor, so its tokens are refused.", SERVICES.HINT_SPONSOR_INACTIVE],
    ["Mint a token", SERVICES.MINT],
    ["Show tokens", SERVICES.SHOW_TOKENS],
    ["Hide tokens", SERVICES.HIDE_TOKENS],
    ["Token", SERVICES.COL_TOKEN],
    ["Expires", SERVICES.COL_EXPIRES],
    ["Last used", SERVICES.COL_LAST_USED],
    ["Created 20 days ago", SERVICES.CREATED(inWords(at(-20 * D), NOW))],
    ["Created 1 minute ago", SERVICES.CREATED(inWords(at(-60_000), NOW))],
    ["Expired", SERVICES.EXPIRED],
    ["Expired 2 days ago", SERVICES.EXPIRED_AT(inWords(at(-2 * D), NOW))],
    ["Revoked", SERVICES.REVOKED],
    ["Revoked 38 days ago", SERVICES.REVOKED_AT(inWords(at(-38 * D), NOW))],
    ["Revoked just now", SERVICES.REVOKED_AT(inWords(at(-1000), NOW))],
    // FR-0126: a count that rounds up to the next unit's size steps up a unit.
    ["Created 1 hour ago", SERVICES.CREATED(inWords(at(-3_599_000), NOW))],
    ["Created 1 day ago", SERVICES.CREATED(inWords(at(-86_000_000), NOW))],
    ["Created 59 minutes ago", SERVICES.CREATED(inWords(at(-59 * 60_000), NOW))],
    ["Never", SERVICES.NEVER],
    ["No tokens yet.", SERVICES.NO_TOKENS],
    ["Mint one to let release-notes start runs.", SERVICES.NO_TOKENS_MINT("release-notes")],
    ["Revoke", SERVICES.REVOKE],
    ["Revoke nightly-runner for ci-nightly?", SERVICES.REVOKE_TITLE("nightly-runner", "ci-nightly")],
    ["It stops working now. Anything using it as ci-nightly is refused. You can mint another.", SERVICES.REVOKE_BODY("ci-nightly")],
    ["Service identities are unavailable", SERVICES.UNAVAILABLE_TITLE],
    ["This server does not manage service identities yet.", SERVICES.UNAVAILABLE_BODY],
    ["Loading service identities", SERVICES.LOADING],
    ["Loading tokens", SERVICES.LOADING_TOKENS],
    ["Token minted.", SERVICES.MINTED_TOAST],
  ])("%s", (mock, actual) => expect(actual).toBe(mock));
});

describe("Token dialog", () => {
  it.each<[string, string]>([
    ["Claim your token", TOKEN_DIALOG.CLAIM_TITLE],
    ["This invitation is for you alone. The token acts as you, with your role, and is shown once.", TOKEN_DIALOG.CLAIM_LEDE],
    ["Claiming also revokes", TOKEN_DIALOG.CLAIM_WARNING_LEAD],
    [". Anything still using it stops working now.", TOKEN_DIALOG.CLAIM_WARNING_TAIL],
    ["Mint a token for ci-nightly", TOKEN_DIALOG.MINT_TITLE("ci-nightly")],
    ["The token acts as ci-nightly, not as you. It is shown once.", TOKEN_DIALOG.MINT_LEDE("ci-nightly")],
    ["Anyone who holds the token can do what ci-nightly is allowed to do, which may be more than you can.", TOKEN_DIALOG.MINT_WARNING("ci-nightly")],
    ["Token name", TOKEN_DIALOG.NAME_LABEL],
    ["Helps you tell your tokens apart.", TOKEN_DIALOG.NAME_HINT],
    ["Name the token.", TOKEN_DIALOG.NAME_REQUIRED],
    ["Expires in", TOKEN_DIALOG.EXPIRES_LABEL],
    ["7 days", TOKEN_DIALOG.OPT_7_DAYS],
    ["30 days", TOKEN_DIALOG.OPT_30_DAYS],
    ["90 days", TOKEN_DIALOG.OPT_90_DAYS],
    ["1 year", TOKEN_DIALOG.OPT_1_YEAR],
    ["Longest the server allows", TOKEN_DIALOG.OPT_LONGEST],
    ["The invitation limits the token to 30 days.", TOKEN_DIALOG.CLAIM_HINT_LIMITED(lifetimeWords(30 * 86400))],
    ["The server may set a shorter limit.", TOKEN_DIALOG.CLAIM_HINT_OPEN],
    ["At most 90 days, or less if the server's limit is lower.", TOKEN_DIALOG.MINT_HINT],
    ["Cancel", TOKEN_DIALOG.CANCEL],
    ["Close", TOKEN_DIALOG.CLOSE],
    ["Claim token", TOKEN_DIALOG.CLAIM_SUBMIT],
    ["Claiming…", TOKEN_DIALOG.CLAIM_BUSY],
    ["Mint token", TOKEN_DIALOG.MINT_SUBMIT],
    ["Minting…", TOKEN_DIALOG.MINT_BUSY],
    ["Your new token", TOKEN_DIALOG.REVEAL_TITLE],
    ["You won't see this token again. Copy it now and keep it somewhere safe.", TOKEN_DIALOG.REVEAL_WARNING],
    ["You haven't copied it yet. Once you close this, it can't be shown again.", TOKEN_DIALOG.REVEAL_UNCOPIED],
    ["Name", TOKEN_DIALOG.DETAIL_NAME],
    ["Acts as", TOKEN_DIALOG.DETAIL_ACTS_AS],
    ["you (User)", TOKEN_DIALOG.ACTS_AS_YOU(ROLE_WORDS.user)],
    ["you (Security admin)", TOKEN_DIALOG.ACTS_AS_YOU(ROLE_WORDS.security_admin)],
    ["you (Super admin)", TOKEN_DIALOG.ACTS_AS_YOU(ROLE_WORDS.admin)],
    ["Expires", TOKEN_DIALOG.DETAIL_EXPIRES],
    ["11 Nov 2026 (in 30 days)", TOKEN_DIALOG.EXPIRY(dayOf("2026-11-11T09:00:00Z"), inWords("2026-11-11T09:00:00Z", Date.parse("2026-10-12T09:00:00Z")))],
    ["Never", TOKEN_DIALOG.NEVER],
    ["The server's limit is shorter than the one you chose, so it expires sooner.", TOKEN_DIALOG.CLAMPED],
    ["Copy token", TOKEN_DIALOG.COPY],
    ["Copied", TOKEN_DIALOG.COPIED],
    ["Token copied to the clipboard.", TOKEN_DIALOG.COPIED_STATUS],
    ["Done", TOKEN_DIALOG.DONE],
    ["Close without copying", TOKEN_DIALOG.CLOSE_UNCOPIED],
  ])("%s", (mock, actual) => expect(actual).toBe(mock));
});

describe("Admin > Credentials state column", () => {
  it.each<[string, string]>([
    ["State", MINTED.COL_STATE],
    ["No token an admin created for someone else still works.", MINTED.NONE_LIVE],
    ["Also show revoked tokens", MINTED.INCLUDE_REVOKED],
    ["Showing 3 revoked tokens as well.", MINTED.SHOWING_REVOKED(3)],
    ["Showing 1 revoked token as well.", MINTED.SHOWING_REVOKED(1)],
    ["Service identity:", MINTED.SERVICE_IDENTITY],
    ["Invite the owner with wardyn person invite, or move the token with wardyn token move. The console does neither.", MINTED.FOOTNOTE.join("")],
    ["Needs a decision", DISPOSITION.needs_decision.label],
    ["Still works as the person. Invite them, or move it to a service identity.", DISPOSITION.needs_decision.sub("")],
    ["Waiting for the owner", DISPOSITION.awaiting_owner.label],
    ["An invitation to replace it is pending.", DISPOSITION.awaiting_owner.sub("")],
    ["Linked to a service identity", DISPOSITION.linked_to_service_identity.label],
    ["Still works. Revoke it once the job uses the new token.", DISPOSITION.linked_to_service_identity.sub("")],
    ["Replaced by the owner", DISPOSITION.replaced_by_owner.label],
    ["Revoked 2 days ago, when they claimed their own.", DISPOSITION.replaced_by_owner.sub("2 days ago")],
    ["Moved to a service identity", DISPOSITION.moved_to_service_identity.label],
    ["Revoked 9 days ago, after the move.", DISPOSITION.moved_to_service_identity.sub("9 days ago")],
    ["Expired", MINTED.EXPIRED],
    ["No longer works. Nothing to decide.", MINTED.EXPIRED_SUB],
    ["Revoked", DISPOSITION.revoked.label],
    ["Revoked 30 days ago, with no replacement.", DISPOSITION.revoked.sub("30 days ago")],
  ])("%s", (mock, actual) => expect(actual).toBe(mock));

  it("covers exactly the six wire values", () => {
    expect(Object.keys(DISPOSITION).sort()).toEqual(
      ["awaiting_owner", "linked_to_service_identity", "moved_to_service_identity", "needs_decision", "replaced_by_owner", "revoked"],
    );
  });
});

describe("relative stamps", () => {
  it("reads in whole units, as the mock does", () => {
    expect(inWords(at(30_000), NOW)).toBe("just now");
    expect(inWords(at(90 * 60_000), NOW)).toBe("in 2 hours");
    expect(inWords(at(-45 * 60_000), NOW)).toBe("45 minutes ago");
    expect(inWords(at(-1 * D), NOW)).toBe("1 day ago");
    expect(lifetimeWords(3600)).toBe("1 hour");
    expect(lifetimeWords(5400)).toBe("90 minutes");
  });
});
