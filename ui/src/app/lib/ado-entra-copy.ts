/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Azure DevOps per-person copy canon (0.7.10) — the frozen canonical-strings
// tables from docs/design/ado-entra-prompt.md §7.2-§7.8, transcribed verbatim,
// PLUS §10 — the capability request card's own addendum (plan slice S10,
// round 2/3): new rows the card needs and §7 never named (a per-capability
// noun, the ref-class field, a few mount-site strings), and three §7.6 rows
// corrected post-freeze with the owner's delegated sign-off (REQ_DENYING,
// REQ_HELD/REQ_HELD_EXPIRED, REQ_CONSENT_BODY — see §7.6's and §10.3's own
// notes in the doc for why). The namespace is ADO, exactly as the doc names
// it (§7 header).
//
// Pure TS — no React, no fetch, no DOM. Same discipline as workspace-providers-copy.ts
// and user-drives-copy.ts: the components that consume this add NO copy of their own.
//
// ado-entra-copy.test.ts PARSES §7.2-§7.8 AND §10 back out of the prompt doc
// and compares every key below against it, so a swapped hyphen, a dropped
// ellipsis or a new doc row fails a gate instead of shipping.
//
// N3 (S10 round 3): this file absorbs ui/src/app/lib/ado-capability-copy.ts,
// which carried the capability card's own subset of this same canon under a
// separate ADO_CAPABILITY namespace while this file's branch (#415) hadn't
// merged yet. That file and its test are deleted in the same change; every
// consumer now imports ADO from here.
//
// Backtick-mono rule (§7 header note): a backticked substring inside a frozen
// string (a host, a ref, a wire value, a field) is PLAIN TEXT here — the mono span
// is a DISPLAY concern the consuming component applies, never baked into the string.
//
// §7.1 is REUSED canon (PROVIDERS.*, APPROVAL_BANNER_LABEL.*, APPROVAL_SCOPE_LABEL.*,
// APPROVAL_SCOPE_HINT.*, APPROVAL.CANCELLED_BODY, CAPABILITY.*, MEMBER_GETTING_STARTED.*,
// AGENTS.MODEL_ACCESS_EXPIRING_ACTION, OPERATOR_ONLY_REASON, PEOPLE.CANCEL,
// PROVIDERS.LAUNCH_WARNING_TITLE, AGENTS.OPEN_RUN_CTA) — imported by the consuming
// screens from its own home, never re-exported or re-frozen here. §7.1's second table
// (server-composed refusals: ADO_400.*, ADO_422.*, ADO_REFUSE.*, ADO_PAT.*) is also
// deliberately absent — those are rendered from the wire, verbatim, one Go constants
// block per lane, never a second copy here.

export const ADO = {
  // ---- §7.2 `ADO` — the provider row ----
  LANE_ENTRA_LABEL: `Microsoft Entra · brokered`,
  LANE_ENTRA_TOOLTIP: `The run works as you, through a token the proxy holds — nothing is written into the sandbox.`,
  LANE_PAT_OFF_PER_USER: `Turned off while this row signs in per person. A stored token would put every run back on one account.`,
  LANE_SSH_OFF_PER_USER: `Turned off while this row signs in per person. A stored key would put every run back on one account.`,
  FIELD_CREDENTIAL_SOURCE: `Credential`,
  SOURCE_SHARED: `Shared token`,
  SOURCE_SHARED_HINT: `One token an admin stores. Every run uses it, so Azure DevOps records every commit and every API call under that one account, whoever started the run.`,
  SOURCE_PER_USER: `Each person signs in`,
  SOURCE_PER_USER_HINT: `Each person's runs act as them. Turning this on connects everyone who signs in to Wardyn — they have nothing to set up. Azure DevOps applies each person's own permissions, and removing someone from your directory ends it.`,
  FIELD_TOKEN_MODE: `Token`,
  TOKEN_BEARER: `Their sign-in`,
  TOKEN_MINTED: `One Azure DevOps mints per run`,
  TOKEN_MODE_HINT: `Their sign-in is renewed in the background and never stored anywhere a run can read. A minted token is bounded by Azure DevOps itself, and exists there until Wardyn revokes it.`,
  TOKEN_MODE_SHARED_HINT: `There is one token and it is the one you stored. This choice appears when each person signs in.`,
  FIELD_TENANT: `Directory (tenant) ID`,
  TENANT_HINT: `The Microsoft Entra directory your organisation signs in to.`,
  FIELD_CLIENT: `Application (client) ID`,
  CLIENT_HINT: `Filled from the application this console signs in with. Wardyn can only tie an Azure DevOps sign-in to the person's own Wardyn session when both use one application.`,
  CONSENT_TITLE: `Consent`,
  CONSENT_ALL_CONNECTED: `Everyone who has signed in is connected`,
  CONSENT_ALL_CONNECTED_BODY: (connected: string, total: string) => `${connected} of ${total} people who have signed in are connected to Azure DevOps, and nobody has been refused for consent.`,
  CONSENT_SOME_MISSING: (n: number) =>
    n === 1
      ? `${n} person isn't connected`
      : `${n} people aren't connected`,
  CONSENT_SOME_MISSING_BODY: (connected: string, total: string, refused: string) => `${connected} of ${total} people who have signed in are connected to Azure DevOps. ${refused} were refused by Microsoft for consent and haven't connected since.`,
  CONSENT_NOTHING_SEEN: `Nothing seen yet`,
  CONSENT_NOTHING_SEEN_BODY: `Nobody has signed in since this row was turned on, so there is nothing to report yet.`,
  CONSENT_GRANT_TITLE: `Grant it for everyone`,
  CONSENT_GRANT_BODY: (directory: string) => `In Microsoft Entra, open this application's API permissions and choose “Grant admin consent for ${directory}”. After that nobody is asked, including people who haven't signed in yet.`,
  CONSENT_HONESTY: `Wardyn can tell you who is connected, and who Microsoft refused for consent. It cannot tell you whether someone saw a consent screen and accepted it — from here, a sign-in that was consented to and one that never needed consent look identical. It does not read your directory's settings and cannot consent on your behalf.`,
  CAPS_TITLE: `What a run may do here`,
  CAPS_LEAD: `Two lists, and the difference between them matters: the ceiling is what a run may ever ask for, the defaults are what it starts with.`,
  CEILING_TITLE: `Capability ceiling`,
  CEILING_HINT: `What a run may ever ask for. Anything not ticked here is refused outright — nobody can approve it, not the person and not you.`,
  PROFILE_TITLE: `Every run starts with`,
  PROFILE_HINT: `What a run has before anyone decides anything. Everything else on the ceiling is held at the proxy until you or the person who started the run allows it.`,
  PROFILE_OFF_CEILING: `Not on the ceiling, so it can't be a default.`,
  PROFILE_READ_ONLY_NOTE: `Read-only is the honest default. A run that only reads never interrupts anyone; a run that starts able to push never asks.`,
  ALWAYS_DENIED_TITLE: `Always refused, on every Azure DevOps row`,
  ALWAYS_DENIED_BODY: `Creating or revoking tokens, service hooks, and installing extensions — and any Azure DevOps write Wardyn cannot name. None of them are on the ceiling, none can be approved, and no change here allows them.`,
  MINTED_EXPOSURE_TITLE: `A minted token is more exposed than a sign-in`,
  MINTED_EXPOSURE_BODY: `It exists at Azure DevOps under that person's name until Wardyn revokes it — at the end of the run, or on the next sweep if this Wardyn was stopped mid-run. A sign-in is never written anywhere and needs no cleanup. Choose this when you need Azure DevOps itself to bound the run.`,
  MINTED_POLICY_NOTE: `Your organisation may refuse token creation, or cap how long a token may live, by policy. If it does, runs on this row are refused with the policy named, and nothing falls back to a wider token.`,
  MINTED_REVOKE_NOTE: `Azure DevOps does not promise that revoking a token ends a connection already open with it. Revoking is what stops the next request, not necessarily the one in flight.`,
  SAVED_NARROWED_RUNS: (n: number, capability: string) =>
    n === 1
      ? `${n} run was granted ${capability} and no longer holds it — its next request for it is refused.`
      : `${n} runs were granted ${capability} and no longer hold it — their next request for it is refused.`,

  // ---- §7.3 `ADO` — the enforcement panel ----
  ENFORCEMENT_TITLE: `Where this is enforced`,
  ENFORCEMENT_LEAD: `Three different things, held by three different parties. Wardyn states all three because only two of them are Wardyn's.`,
  ENF_WHO_LABEL: `Who a run acts as`,
  ENF_WHO_PER_USER: `Azure DevOps. It is that person's own sign-in, so their own Azure DevOps permissions apply and a repository they can't read stays unreadable. Every commit and every API call is recorded under their name.`,
  ENF_WHO_SHARED: `The account your stored token belongs to — the same one for everybody. Azure DevOps applies that account's permissions, not the person's, and its audit names that account.`,
  ENF_WHAT_LABEL: `What a run may do`,
  ENF_WHAT_PER_USER: `Wardyn. Every Azure DevOps request is read at the proxy and refused if this run hasn't been granted it. This check is what bounds the run — not the token.`,
  ENF_WHAT_SHARED: `Wardyn's host and address checks only. Capabilities, and the requests they hold, need each person to sign in.`,
  ENF_TOKEN_LABEL: `The token's own reach`,
  ENF_TOKEN_BEARER: `Nobody narrows it. A Microsoft Entra sign-in token carries every Azure DevOps permission that person has consented to, and asking for less changes nothing. Switch Token to “One Azure DevOps mints per run” to have Azure DevOps bound it too.`,
  ENF_TOKEN_MINTED: `Azure DevOps. It mints this run its own token, limited to the capabilities the run has been granted and to the run's lifetime, and widens it only when you allow more.`,
  ENF_TOKEN_SHARED: `Whatever you gave the token when you created it in Azure DevOps. Wardyn can neither narrow it nor expire it.`,
  ENF_WHO_MEMBER: `Azure DevOps. It is your own sign-in, so your own permissions apply — a repository you can't read, your run can't read either.`,
  ENF_WHAT_MEMBER: `Wardyn. Every request is read before it leaves and refused unless the run was granted it. That check is what holds a run to the list above.`,
  ENF_TOKEN_MEMBER_LABEL: `The connection's own reach`,
  ENF_TOKEN_MEMBER: `It covers everything you consented to for Azure DevOps — Wardyn doesn't make it smaller. That is why the check above exists, and why every one of these is recorded.`,

  // ---- §7.4 `ADO` — capability labels ----
  CAP_READ: `Read`,
  CAP_READ_HINT: `Clone, fetch, and read work items, pipelines and policies.`,
  CAP_CODE_WRITE: `Push`,
  CAP_CODE_WRITE_HINT: `Push commits, and create or move a branch no policy protects.`,
  CAP_PR: `Pull requests`,
  CAP_PR_HINT: `Open, update, comment on and complete a pull request.`,
  CAP_POLICY_ADMIN: `Branch policies`,
  CAP_POLICY_ADMIN_HINT: `Create, change or delete a branch policy.`,
  CAP_POLICY_BYPASS: `Push past a branch policy`,
  CAP_POLICY_BYPASS_HINT: `Move a policy-protected branch, or complete a pull request with its policies bypassed.`,
  CAP_BUILD_EXECUTE: `Run pipelines`,
  CAP_BUILD_EXECUTE_HINT: `Queue a build or a release.`,
  CAP_REPO_ADMIN: `Repository settings`,
  CAP_REPO_ADMIN_HINT: `Rename, fork, or change a repository's own settings.`,
  CAP_WORK_WRITE: `Work items`,
  CAP_WORK_WRITE_HINT: `Create and edit work items.`,
  CAP_WIKI_WRITE: `Wiki`,
  CAP_WIKI_WRITE_HINT: `Create and edit wiki pages.`,
  CAP_TOKENS: `Create an Azure DevOps token`,

  // ---- §7.5 `ADO` — the member's connection and the six access states ----
  ACCESS_LIVE_ORG: `Azure DevOps · Connected through your sign-in`,
  ACCESS_LIVE_SEPARATE: `Azure DevOps · Your connection`,
  ACCESS_EXPIRING: `Azure DevOps · Expiring`,
  ACCESS_EXPIRING_ACTION: (ts: string) => `Your Azure DevOps connection ends at ${ts}. Reconnecting takes one click.`,
  ACCESS_EXPIRED: `Azure DevOps · Disconnected`,
  ACCESS_EXPIRED_ORG_ACTION: `Your Azure DevOps connection ended. Runs that clone from it are refused until you connect again — signing out of Wardyn and back in does it too.`,
  ACCESS_EXPIRED_SEPARATE_ACTION: `Your Azure DevOps connection ended. Runs that clone from it are refused until you connect again.`,
  ACCESS_NOT_CONNECTED: `Azure DevOps · Not connected`,
  ACCESS_NEEDS_CONSENT: `Azure DevOps · Needs your consent`,
  ACCESS_SHARED_LIVE: `Azure DevOps · Provided by your admin`,
  ACCESS_SHARED_EXPIRED: `Azure DevOps · Your admin's credential expired`,
  ACCESS_SHARED_EXPIRED_ACTION: `Your admin's Azure DevOps token expired — ask them to replace it. There is nothing for you to connect.`,
  CAUSE_ROW_IS_NEWER: `You signed in to Wardyn before your admin turned Azure DevOps on, so your sign-in doesn't cover it yet. Connect now, or sign out and back in — either works.`,
  CAUSE_CONSENT_NEEDED: `Microsoft needs you to allow Wardyn to reach Azure DevOps as you. It's one screen, once.`,
  CAUSE_ENDED: `Your Azure DevOps connection ended — your organisation ended your sessions, or the consent was withdrawn. Runs that clone from it are refused until you connect again.`,
  CAUSE_OTHER_DIRECTORY: `You sign in to Wardyn through a different directory from the one this Azure DevOps organisation uses, so your Wardyn sign-in can't connect you. Connect separately instead.`,
  CONNECT_ADO: `Connect Azure DevOps`,
  CONNECT_AGAIN: `Connect again`,
  CONNECT_DIALOG_TITLE: `Connect Azure DevOps`,
  CONNECT_DIALOG_BODY: (org: string) => `You'll go to your organisation's Microsoft sign-in and come straight back. After that your runs reach ${org} as you, instead of through one account shared by everyone.`,
  CONNECT_CONSENT_BODY: `Microsoft may ask you to allow it once. What you allow is what Wardyn is able to ask Azure DevOps for at all. What any one run may actually do is smaller, and Wardyn holds it there:`,
  CONNECT_APP_NOTE: (app: string) => `The application asking is ${app} — the same one you signed in to this console with. You can withdraw this at any time from your Microsoft account's My Apps page; doing so stops your runs reaching Azure DevOps.`,
  CONNECT_CTA: `Continue to Microsoft`,
  CONNECT_POPUP_BLOCKED: `Your browser blocked the popup.`,
  GROUP_STARTS_WITH: `Starts with`,
  GROUP_CAN_ASK: `Can ask you for`,
  GROUP_NEVER: `Never`,
  PANEL_CONNECTED_AS: `Connected as`,
  PANEL_CONNECTED_AS_HINT: `Your runs act as this account, and Azure DevOps records them under it.`,
  PANEL_HOW: `How`,
  PANEL_HOW_ORG: `Your Wardyn sign-in`,
  PANEL_HOW_ORG_HINT: `Your admin turned this on for the organisation, so signing in here connected you. There is nothing to set up.`,
  PANEL_HOW_SEPARATE: `You connected it`,
  PANEL_HOW_SEPARATE_HINT: `Your Wardyn sign-in doesn't cover Azure DevOps, so this is a connection you made yourself.`,
  PANEL_ORG: `Organisation`,
  PANEL_ENDS: `Connection ends`,
  PANEL_ENDS_RENEWED: `Renewed while you keep using it`,
  PANEL_ENDS_HINT: `Wardyn renews it in the background. If your organisation ends your sessions, or you're removed from the directory, it stops — and signing in to Wardyn again reconnects it.`,
  PANEL_CARD_OPEN: `What your runs may do here`,
  PANEL_CARD_SUMMARY: (start: string, n: string) => `Starts with ${start} · can ask for ${n} more`,
  DISCONNECT_CTA: `Disconnect Azure DevOps`,
  DISCONNECT_CONFIRM_TITLE: `Disconnect Azure DevOps?`,
  DISCONNECT_CONFIRM_BODY: `Runs you start after this can't reach Azure DevOps. Runs already going keep the connection they started with. Signing in to Wardyn again reconnects you — to stop that, withdraw the permission from your Microsoft account's My Apps page.`,
  ACCESS_SHARED_NOTE: `Rendered when the row uses a shared token and that token works. It is deliberately not a per-person claim: the run does not act as this person.`,

  // ---- §7.6 `ADO` — the capability request card ----
  REQ_WAITING: `Waiting for you`,
  REQ_WAITING_OTHER: (person: string) => `Waiting for ${person}`,
  REQ_COUNTDOWN: (mmss: string) => `${mmss} left`,
  REQ_SOURCE: (ts: string) => `Azure DevOps · asked by this run at ${ts}`,
  REQ_SOURCE_LIST: (run: string, person: string, ts: string) => `Azure DevOps · run “${run}” · started by ${person} · asked ${ts}`,
  REQ_FIELD_REPOSITORY: `Repository`,
  REQ_FIELD_BRANCH: `Branch`,
  REQ_FIELD_PR: `Pull request`,
  REQ_FIELD_COMMAND: `Command`,
  REQ_FIELD_CHANGE: `Change`,
  REQ_FIELD_REQUEST: `Request`,
  REQ_FIELD_ACTS_AS: `Acts as`,
  REQ_ACTS_AS_HINT: (person: string) => `Whatever you allow happens as ${person} on Azure DevOps, and Azure DevOps records it that way.`,
  REQ_UNPROTECTED_REF: `No branch policy protects this branch.`,
  REQ_PROTECTED_REF_TITLE: (ref: string) => `${ref} is protected by a branch policy`,
  REQ_PROTECTED_REF_BODY: (person: string) => `Allowing this moves it anyway. The policy that requires a reviewed pull request will not stop it, because ${person}'s Azure DevOps account is allowed to bypass it.`,
  REQ_WHAT_CODE_WRITE: (repo: string, person: string) => `the run pushes this branch to ${repo}, as ${person}.`,
  REQ_BLAST_CODE_WRITE: (repo: string) => `commits, and creating or moving branches no policy protects, anywhere in ${repo}. Not a protected branch, not completing a pull request, not changing a policy — each of those asks separately.`,
  REQ_WHAT_POLICY_BYPASS: (ref: string, person: string) => `the run moves ${ref} past the policy protecting it, as ${person}.`,
  REQ_BLAST_POLICY_BYPASS: (repo: string) => `moving any policy-protected branch in ${repo}, and completing a pull request with its policies bypassed.`,
  REQ_WHAT_POLICY_ADMIN: (ref: string, person: string) => `the run lowers the reviewer count on ${ref}, as ${person}.`,
  REQ_BLAST_POLICY_ADMIN: (repo: string) => `creating, changing and deleting branch policies anywhere in ${repo} — including the ones that would hold back its own pushes.`,
  REQ_WHAT_PR: (pr: string, person: string) => `the run completes pull request ${pr}, as ${person}.`,
  REQ_BLAST_PR: (repo: string) => `opening, updating, commenting on and completing pull requests anywhere in ${repo}. Completing one past its own policies is a separate ask.`,
  // Post-freeze correction (S10 round 2/3 — owner-delegated to the lead,
  // 2026-09-22): no expiry timestamp reaches the client for an ADO hold, so
  // "for up to four minutes" claimed a precision the card doesn't have.
  // REQ_HELD_EXPIRED (§10.3) is its honest "already lapsed" twin.
  REQ_HELD: (thing: string) => `The ${thing} may be waiting at the proxy for a short time; approving lets it through now or the next time the run asks.`,
  REQ_SCOPE_READOUT: (scope: string) => `Scope: ${scope}`,
  REQ_APPROVING_ONCE: (thing: string) => `Approving lets this one ${thing} through. The next one asks again.`,
  REQ_APPROVING_RUN: (thing: string) => `Approving lets every ${thing} from this run through until it ends. Nothing carries to the next run.`,
  // Post-freeze correction (S10 round 2/3 — owner-delegated to the lead,
  // 2026-09-22): a deny STICKS for the rest of the run since #414, UNLESS a
  // later `run`-scoped Approve of the same capability lifts it (adoStanding).
  REQ_DENYING: (thing: string) => `Denying refuses this ${thing} for the rest of the run, unless this kind of change is later allowed for the whole run.`,
  REQ_SCOPE_ONCE_HINT: (thing: string) => `This one ${thing} goes through. The next one asks again.`,
  REQ_SCOPE_RUN_HINT: (thing: string) => `Every ${thing} from this run goes through until it ends. (default)`,
  REQ_SCOPE_UNTIL_REFUSED: `Refused for Azure DevOps capabilities: a capability can't outlive the run that was granted it.`,
  REQ_SCOPE_ALWAYS_REFUSED: `Refused for Azure DevOps capabilities: nothing here is saved to the workspace.`,
  REQ_REFUSED_CHIP: `Refused`,
  REQ_CEILING_TITLE: `Above this deployment's ceiling`,
  REQ_CEILING_BODY: `Your admin's Azure DevOps row doesn't list this capability, so there is nothing to decide: it was refused and the run was told. An admin can add it to the ceiling — that changes what a future run may ask for, and does nothing for this one.`,
  REQ_ALWAYS_DENIED_TITLE: `Always refused`,
  REQ_ALWAYS_DENIED_BODY: `Creating or revoking tokens, service hooks and extensions are refused on every Azure DevOps row. This can't be approved, and no ceiling change allows it — a run that could mint its own token would be outside every check on this page.`,
  REQ_GOVERNANCE_TITLE: `Refused by your governance policy`,
  REQ_GOVERNANCE_BODY: `The policy this run launched under refuses this capability. It was refused and recorded; nobody here can allow it, and a policy edit doesn't reach a run already going.`,
  REQ_UNCLASSIFIED_HEADING: `An Azure DevOps request Wardyn doesn't recognise`,
  REQ_UNCLASSIFIED_TITLE: `Wardyn can't name what this would do`,
  REQ_UNCLASSIFIED_BODY: `An Azure DevOps write Wardyn doesn't recognise is refused rather than guessed at, because the card couldn't tell you truthfully what you'd be allowing. There is no setting that changes this — if a tool you need keeps hitting it, tell your admin what it was trying to do.`,
  REQ_CONSENT_CHIP: `Needs your Microsoft consent`,
  REQ_CONSENT_SOURCE: (ts: string) => `Azure DevOps · you allowed this at ${ts} · still held`,
  // Post-freeze correction (S10 round 2/3 — owner-delegated to the lead,
  // 2026-09-22): "You allowed it, but…" is false on the dispatch-granted
  // path (nobody approved anything; the run simply asked and Entra refused
  // the redemption). Dropped the {capability} parameter too — no capability
  // name reaches the wire scope on that path either, so this is a plain
  // string, not a function, as of round 2.
  REQ_CONSENT_BODY: `Microsoft needs your consent before Azure DevOps lets this run use this access. Reconnecting asks Microsoft for it — you'll see a consent screen, and nothing else changes. The run's request stays held meanwhile.`,
  REQ_CONSENT_CTA: `Allow and continue`,
  REQ_CONSENT_OTHER_BODY: (person: string) => `You allowed it, but only ${person} can give Microsoft the extra permission it needs — the run acts as ${person}, and consent is theirs to give. They've been shown this on their Getting started page.`,
  REQ_NOT_YOURS_CHIP: `Not yours to decide`,
  REQ_NOT_YOURS_BODY: (person: string) => `Only ${person}, who started this run, or an admin can answer this.`,
  REQ_TIMEOUT_BODY: (thing: string) => `Nobody answered within four minutes, so the ${thing} was refused and the run was told. It can ask again.`,
  REQ_REAUTH_CHIP: `Connection ended`,
  REQ_REAUTH_TITLE: `Your Azure DevOps connection ended mid-run`,
  REQ_REAUTH_BODY: `This run's next Azure DevOps request is held for up to four minutes while you reconnect. Reconnect and it goes through on its own — the run doesn't have to start over.`,
  LIST_ENDED_CHIP: `This run has ended`,
  LIST_ENDED_BODY: `There's nothing to allow — the request went away with the run. It's here so you can see what it asked for.`,
  OUTCOME_ALLOWED_ONCE: `Allowed once`,
  OUTCOME_ALLOWED_RUN: `Allowed for this run`,
  OUTCOME_DENIED: `Denied`,
  OUTCOME_BY: (person: string, ts: string) => `by ${person} at ${ts}`,
  OUTCOME_CEILING: `Above this deployment's ceiling — nobody was asked.`,
  OUTCOME_ALWAYS_DENIED: `Always refused — nobody was asked.`,
  OUTCOME_UNCLASSIFIED: `Wardyn couldn't name what it would do — nobody was asked.`,
  OUTCOME_TIMEOUT: `Nobody answered within four minutes.`,
  COL_TIME: `Time`,
  COL_ASKED: `Asked for`,
  COL_WHERE: `Where`,
  COL_OUTCOME: `Outcome`,

  // ---- §7.7 `ADO` — the launch door and the after view ----
  PREFLIGHT_LIVE: (person: string) => `Azure DevOps · connected as ${person}`,
  PREFLIGHT_MISSING: `Azure DevOps · not connected`,
  PREFLIGHT_MISSING_SUB: `You'll be asked to connect when you launch.`,
  PREFLIGHT_EXPIRING: (ts: string) => `Azure DevOps · your connection ends at ${ts}`,
  PREFLIGHT_EXPIRING_SUB: `A run going when it ends holds its next Azure DevOps request for up to four minutes while you reconnect. Reconnecting now avoids that.`,
  LAUNCH_ANYWAY: `Launch anyway`,
  LAUNCH_DIALOG_TITLE: `Connect Azure DevOps first`,
  LAUNCH_DIALOG_BODY: (org: string) => `This workspace clones from ${org}, and your runs there act as you. Connect once and this form comes back exactly as you left it.`,
  RELAUNCH_TOAST_TITLE: `Connected to Azure DevOps.`,
  RELAUNCH_TOAST_BODY: `Your run is as you left it — launch when you're ready.`,
  LAUNCH_WARNING_EXPIRING: (ts: string) => `Your Azure DevOps connection ends at ${ts}. After that this run holds its next Azure DevOps request for up to four minutes while you reconnect.`,
  AFTER_TITLE: `Azure DevOps access`,
  AFTER_LEAD: `What this run asked for, what it was given, and who decided.`,
  AFTER_ACTED_AS: `Acted as`,
  AFTER_STARTED_WITH: `Started with`,
  AFTER_ENDED_HOLDING: `Ended holding`,
  AFTER_ONCE_NOTE: (capability: string) => `${capability} was allowed once, for one request, and never joined the run's standing set.`,
  AFTER_SCOPES_TITLE: `What the connection itself covered`,
  AFTER_SCOPES_BODY: (list: string) => `Azure DevOps issued this run a token covering: ${list}.`,
  AFTER_SCOPES_HONESTY: `That is what the token carried, not what the run was allowed to do. Wardyn's own check is what held it to the decisions above; a Microsoft Entra token can't be issued narrower than what the person has consented to.`,
  AFTER_MINTED_TITLE: `What the token itself covered`,
  AFTER_MINTED_BODY: (name: string, caps: string, from: string, to: string, revoked: string) => `Azure DevOps minted ${name} for this run, limited to ${caps} and to ${from}–${to}, and Wardyn revoked it at ${revoked}. Outside those, Azure DevOps refused the request itself.`,
  AFTER_REVOKE_FAILED_TITLE: `This run's Azure DevOps token hasn't been revoked`,
  AFTER_REVOKE_FAILED_BODY: (name: string) => `Your connection had ended, so Wardyn couldn't revoke ${name}. It will be revoked the next time you sign in. You can also delete it yourself under your Azure DevOps personal access tokens.`,
  AFTER_FORGE_TITLE: `Azure DevOps has its own record`,
  AFTER_FORGE_BODY: (person: string) => `Every request that went through appears in your organisation's audit log and in the repository's push history, under ${person} — not under Wardyn, and not under a shared account.`,

  // ---- §7.8 Sentences this design falsifies — rewritten in the same change ----
  TOOL_CALL_NOTE: `An Azure DevOps capability request is held at the proxy until someone decides it — nothing reaches Azure DevOps first, and a denial is a refusal, not a note. Every other kind of tool-call approval is a record of what a run said it was about to do; nothing stops it.`,
  SECRETS_PER_USER_NOTE: `This row stores no Azure DevOps token: each person's sign-in is their own, and removing them from your directory ends it. Wardyn holds every run to its granted capabilities at the proxy.`,

  // ---- §10.1 `ADO` — the consequence sentences' {thing}, per capability ----
  CAP_THING_READ: `read`,
  CAP_THING_CODE_WRITE: `push`,
  CAP_THING_PR: `action`,
  CAP_THING_POLICY_ADMIN: `change`,
  CAP_THING_POLICY_BYPASS: `push`,
  CAP_THING_REPO_ADMIN: `change`,
  CAP_THING_BUILD_EXECUTE: `run`,
  CAP_THING_WORK_WRITE: `edit`,
  CAP_THING_WIKI_WRITE: `edit`,

  // ---- §10.2 `ADO` — the ref-class field and the consent card's heading ----
  REQ_FIELD_REF_CLASS: `Ref class`,
  REQ_REF_CLASS_PROTECTED: `Protected by a branch policy`,
  REQ_CONSENT_HEADING: `Azure DevOps needs more access`,

  // ---- §10.3 `ADO` — the hold's honest expiry ----
  REQ_HELD_EXPIRED: (thing: string) => `No longer waiting — approving lets the ${thing} through the next time the run asks.`,

  // ---- §10.5 `ADO` — the remaining mount-site strings ----
  REQ_RUN_UNAVAILABLE: `Couldn't load this run — try again.`,
  SCOPE_UNTIL_LABEL: `Until…`,
  SCOPE_ALWAYS_LABEL: `Always`,
  STRIP_HEADING_CONSENT: `Azure DevOps sign-in needed — sign in to let this run's Azure DevOps access through`,
  WAITING_ADO_MINE: `Waiting for your Azure DevOps sign-in`,
  WAITING_ADO_OWNER: `Waiting for the owner's Azure DevOps sign-in`,

  // ---- §10.6 `ADO` — the mid-run sign-in card ----
  REQ_REAUTH_HELD_BODY: `This run's Azure DevOps request is held while you sign in again. Sign in and it goes through on its own — the run doesn't have to start over. If the hold runs out first, its next request goes through once you have.`,
  REQ_REAUTH_OTHER_BODY: (person: string) => `Only ${person} can sign in again — the run acts as ${person}. Its Azure DevOps requests go through once they have.`,

} as const;
