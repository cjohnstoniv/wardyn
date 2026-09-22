# Azure DevOps, per person — the admin's row, the member's sign-in, and the moment a run asks for more

This is the mock round for the Azure DevOps surfaces of 0.7.10 — the design gate before any console
code (owner law: the mock is UI source of truth; canon strings are app strings). The model is decided
in `~/.claude/plans/bubbly-stargazing-duckling.md` (workstream A, §§1–10) and corrected by live
measurement against a real Microsoft Entra tenant (`FINDINGS.md`, F-LIVE-1 to F-LIVE-4). Nothing here
is open for re-design, only for drawing. **Every string in §7.2–§7.8 is a DRAFT the owner freezes at
this gate**; the drawing-level calls are Q1–Q9 in §9, with an Adjudication section at the end.

One round covers five surfaces:

1. the **administrator's** Azure DevOps provider row — the Entra lane, per-user versus shared,
   tenant and application, the **capability ceiling** over the **default profile**, and the panel
   that states which of the three enforcement claims are Wardyn's and which are not,
2. the **member's** sign-in — not signed in → the dialog → signed in, plus the six access states
   the design reuses from Model access,
3. the **capability request card** — the moment a run asks to push, edit a policy or complete a pull
   request, drawn both live and found later in a list, including the states where there is nothing
   to decide,
4. the **launch door** — a run that needs Azure DevOps when the person has not signed in, and
5. the **after view** — what was asked, what was granted, by whom, and what the forge recorded.

Static mock: `docs/design/ado-entra-mock/index.html` (ten state blocks, one page — the owner reviews
states side by side, so there is no clickable prototype); `mock.css` is the workspace-providers
round's stylesheet verbatim plus four listed idioms. Frozen strings: §7 below. No TS copy module
exists yet. The implementation stage creates `ui/src/app/lib/ado-entra-copy.ts` **from §7 verbatim**;
every product string in the mock matches §7 byte-for-byte, and §7.2 onwards is exactly two columns,
`Key` and `String`, so `parseFrozenTables()` clones over it the way `user-drives-copy.test.ts` does.

---

## 0. The one thing this round must not get wrong

Measured against a real tenant (F-LIVE-1): **a Microsoft Entra access token carries every Azure
DevOps scope the person has consented to.** A refresh grant asking for `vso.code` alone came back
carrying all fifteen consented scopes in its own `scp` claim. Consent decides a token's scope; the
request does not. Asking for less changes nothing.

Three claims follow, and the UI draws all three, always together, because only two of them are
Wardyn's:

| The claim | Who enforces it | How the UI says it |
|---|---|---|
| **Who a run acts as** | Azure DevOps | It is that person's own sign-in, so their own Azure DevOps permissions apply and a repository they cannot read stays unreadable. |
| **What a run may do** | **Wardyn** | Every request is read at the proxy and refused unless the run was granted it. This check is what bounds the run — not the token. |
| **The token's own reach** | **Nobody** | A sign-in token carries everything that person consented to. Wardyn does not make it smaller, and says so. |

The third row changes, and **only** in `minted_pat` mode, where Azure DevOps mints the run its own
token with an explicit scope list and `validTo` and enforces it itself. That is the only mode in
which the forge bounds a run, and the row that says so is drawn with success tone in exactly that
case and warning tone otherwise (mock State 2).

§5 turns this into the list of sentences the UI must never say.

## 1. What exists today (the thing being grown)

The symbols are the durable half; the line numbers are not. Re-resolve the symbol at implementation.

**Every member's run clones with the administrator's one token.** `handlePATBroker`
(`internal/api/pat_broker.go`) hands the single `git-pat-<host-slug>` PAT to every run, the only gate
before a clone being the host/org-path allowlist plus request-shape checks. Authorisation follows
the credential, not the person: the member's own Azure DevOps entitlements are never consulted,
Azure DevOps' audit sees one account (`SetBasicAuth` on the outbound leg), and revocation is
all-or-nothing. The broker made that token non-resident in 0.7; it did not make it least-privilege.

**The provider row already exists** (`SiteConfig.WorkspaceProviders.git`, `workspace-providers-prompt.md`
§2.1): `{id, kind, disabled?, base_urls[], lanes[]}`, with `lanes` a subset of `app`/`pat`/`ssh` and
**empty meaning every lane the kind supports**. This round adds a fourth lane, `entra`, and a
`credential_source` that defaults to `shared`. An upgraded install is byte-identical until an
administrator opts in — `laneAllowed` (`internal/api/workspace_admission.go`) is
`len(row.Lanes) == 0 || slices.Contains(...)`, so the empty-lanes guard is the one enforcement site
that must learn the difference between "every legacy lane" and "including the new one".

**The six-value access vocabulary exists** — `live`, `expiring`, `expired_signin`, `not_configured`,
`shared_expired`, `not_applicable` (`internal/api/modelaccess.go`), rendered on Getting started as a
chip with an action line under it (`member-getting-started.tsx`, `AGENTS.MODEL_ACCESS_*`). This round
reuses the vocabulary and the render exactly, for a second subject.

**The approval card exists, and says the opposite of what this round needs.** `approvals.tsx` renders
a title, `APPROVAL_BANNER_LABEL.what` ("What you're approving:"), `APPROVAL_BANNER_LABEL.blast`
("Blast radius:"), and an Approve button whose persistence hides behind a caret over
`APPROVAL_SCOPE_LABEL` (`Once` / `This run` / `Until…` / `Always`). For `tool_call` it says: *"Wardyn
records your decision for this tool call; the requesting process reads it and proceeds. Wardyn does
not itself run, restrict, or re-execute the command."* That sentence stops being true the moment the
control plane holds a request on one, which is exactly what this round builds — §7.8 rewrites it.
Two more shipped sentences stop being true for these rows: *"Wardyn can't expire or down-scope a
PAT"* (`approvals.tsx`, `secrets.tsx`) and the lane chip `PAT · in-sandbox`.

**The relaunch behaviour exists** — a create-time 422 carrying a `reason` opens the matching sign-in
dialog from the New Run rail and relaunches the form as it stood (the 0.7.7 `model_credential`
behaviour). This round adds one `reason` value, `git_credential`, and reuses the path.

### 1.1 What this round REUSES rather than builds

- **The provider row, its head strip, its lanes and its credential lanes** are the workspace-providers
  round's (`git-tab.tsx`, `PROVIDERS.*`): `FIELD_ENABLED`, `FIELD_BASE_URLS`, `BASE_URLS_HINT`,
  `FIELD_LANES`, `LANES_HINT`, `SAVE_CTA`, `SAVE_REFUSED_TITLE`, `ROW_DISABLED_*` — rendered
  unchanged, never re-frozen.
- **The lane chip is `LANE_META`'s shape with `CAPABILITY.*` as the tooltip**; `entra` joins the three
  with its own residency line (§7.2 `LANE_ENTRA_*`), success-toned because nothing enters the sandbox.
- **The approval banner labels are `APPROVAL_BANNER_LABEL.what` / `.blast`**, verbatim, on the
  capability card — the console already teaches people to read those two lines.
- **The decision hints are `APPROVAL_SCOPE_HINT`'s shape**, reworded for a push rather than a
  connection; `Until…` and `Always` are refused here and drawn disabled with the reason (Q2).
- **The cancelled body is `APPROVAL.CANCELLED_BODY`** — "The run ended before anyone decided this.
  Nothing was approved and nothing was denied." — rendered unchanged.
- **The access chip + action line is `member-getting-started.tsx`'s**, and the six states are
  `modelaccess.go`'s; `SIGN_IN_ADO` sits where `AGENTS.SIGN_IN_AWS` sits.
- **The sign-in pane is `HarnessLoginPane`'s shape**, with a browser redirect instead of a device code
  (F-LIVE-4: device code is blocked by Conditional Access on a real tenant).
- **The launch door is the 0.7.7 422-opens-a-dialog-and-relaunches path**, unchanged.
- **`OPERATOR_ONLY_REASON`** ("Requires the admin role.") guards the row editor, as it guards
  `/providers` today.

## 2. The model this round mocks

### 2.1 The row

`entra` is a fourth lane and `credential_source` a new field, both on the existing git provider row
(plan §1). Its `entra` block carries `tenant_id`, `client_id`, `capability_ceiling`,
`default_profile`, `token_mode` and `rest_api`. Validation: the lane is available on `dev.azure.com`
and `<org>.visualstudio.com` only (an Azure DevOps **Server** host does not take Entra tokens);
`default_profile ⊆ capability_ceiling`; `per_user` requires the `entra` lane; `client_id` must be
this deployment's own sign-in application, in the login issuer's tenant, because identity binding is
`id_token.sub == session.Sub` and Entra's `sub` is per-application (plan §2).

**The ceiling and the profile are two different questions and the surface must make that obvious.**
The ceiling is *what a run may ever ask for* — a request above it is refused outright and nobody can
approve it, not the person and not the administrator. The profile is *what a run starts with* —
everything else on the ceiling is held at the proxy until someone allows it. They are drawn side by
side, with their one-line meanings above each list, never in a collapsed "advanced" block. A
capability off the ceiling renders in the profile column disabled with its reason ("Not on the
ceiling, so it can't be a default") rather than being hidden, the workspace-providers round's Q3 rule.

**Always refused, on every row, not on the ceiling at all:** creating or revoking Azure DevOps
tokens, service hooks, and extension management. A run that could mint its own token would be outside
every check on the page. The row says so once, as a plain note under the ceiling.

**`shared` is unchanged and stays the default** — and gains the one sentence it was missing, naming
the cost: every run uses one account, so Azure DevOps records every commit under it whoever started
the run. Its enforcement panel has two warning rows and no forge row, honestly.

### 2.2 The sign-in

One browser redirect on the organisation's existing Azure SSO application (authorization code +
PKCE), scopes = the ceiling's `vso.*` plus `offline_access`, and `vso.pats` only in `minted_pat`
mode. The person is already signed in to Wardyn with Entra, so this is usually one consent click and
often none. Every scope the design uses is user-consentable — no administrator consent is required
(F-LIVE-2) — but the Azure DevOps service principal may be **absent** from a tenant until someone
creates it (F-LIVE-3); that is an adoption-doc problem, not a console state.

**The dialog before Microsoft is the honest one.** It says what consent is (what Wardyn is able to
ask Azure DevOps for *at all*) and what it is not (what any one run may do), and lists the three
groups: what a run starts with, what it can ask for, and what it can never have. It names the
application and says where the consent can be withdrawn. Microsoft's own consent screen follows, and
Wardyn does not restate or paraphrase it.

**Connected** shows the account, the organisation, the three capability groups, and how the sign-in
ends — "Renewed while you keep using it", with the two ways it stops: the organisation ending
sessions, and the person being removed from the directory. Then the same three-row panel, in the
member's words.

### 2.3 The card

The proxy classifies each Azure DevOps request into exactly one capability and asks the control
plane. The control plane answers granted (200), refused (403 — above the ceiling, always denied, or
refused by governance) or held (423), and a held request parks at the proxy for at most **four
minutes** while a human answers in the console. The card is one component in two homes: the live run,
and the approvals list.

**What the card must show, every time:** the capability in plain words as its heading; the repository
and the ref or pull request; the command or the request, verbatim; and **who the run acts as**, with
the sentence that whatever is allowed happens under that person's name at Azure DevOps. Then "What
you're approving:" (this one request) and "Blast radius:" (what the capability covers for as long as
it is held) — the shipped two-line shape.

**The protected-ref variant is a different capability, not a warning on the same one.** Any ref move
onto a policy-protected ref classifies as `policy_bypass`, on both doors (the git broker's pkt-line
commands and the REST `refs`/`pushes` bodies). The card names the policy that will not stop it.

**Four states have no decision to make, and draw no buttons.** Above the ceiling; always refused;
refused by the run's governance policy; and a write Wardyn cannot classify. Drawing a disabled
Approve button in these states would be the lie — the request was already refused and the run was
already told. Each says who, if anyone, could change it for a *future* run.

**Two states have a decision that has already been made and still cannot proceed:** the person
allowed it but Microsoft has not consented to that scope yet (they sign in again, and the held
request resumes), and the approver was an administrator but only the run's owner can give that
consent. Both keep the hold running.

Decisions are **Allow once** (exactly once, for that single request, never installed as a standing
header) and **Allow for this run** (joins the run's standing set until the run ends). `Until…` and
`Always` are refused: a capability cannot outlive the run it was granted to, and nothing here is
saved to the workspace. Deciders are the run's owner or an administrator, within the ceiling.

### 2.4 The launch door

Preflight carries a `git_credential` fact, so the rail says what it knows before anyone presses
Launch: signed in (success), not signed in (warning, plus "You'll be asked to sign in when you
launch"), or expiring (warning, with what that means for a run that outlives it). Pressing Launch
without a sign-in opens the dialog rather than refusing; signing in relaunches the form exactly as it
stood. The raw 422s exist for the API path and are rendered verbatim if they ever reach a screen.

### 2.5 The after view

A run-detail widget: who the run acted as, what it started with, what it ended holding, and one row
per request with its outcome and decider. Then the honest pair — the scope list Azure DevOps actually
returned, which is **evidence, not a bound**, with the sentence that keeps it from being read as one;
and the note that Azure DevOps has its own record of every request that went through, under that
person's name. In `minted_pat` mode the first note is replaced by the token's name, its real scope
list and lifetime, and the time Wardyn revoked it — plus the one honest failure, a revoke that could
not run because the sign-in had ended.

### 2.6 States

- **Row** — per person + sign-in token; per person + minted token; shared token; the six write
  refusals; saved-narrowed while runs are going.
- **Member sign-in** — not signed in; the dialog; signed in; the sign-out confirm.
- **Access** — `live` · `expiring` · `expired_signin` · `not_configured` · `shared_expired` ·
  `not_applicable`, plus shared-and-live (which reuses the existing "Provided by your admin" chip
  and deliberately makes no per-person claim).
- **Card, decidable** — push; push past a branch policy; change a branch policy; complete a pull
  request. Live, and in the list.
- **Card, not decidable** — above the ceiling; always refused; refused by governance; unclassifiable;
  not yours to decide; timed out; cancelled with the run.
- **Card, decided but still held** — needs the person's Microsoft consent; needs the *other* person's.
- **Mid-run** — the sign-in ended and the next request is held.
- **List** — waiting; decided rows; the run has ended.
- **Launch** — ready; not signed in; expiring; the dialog; back from Microsoft; the raw 422s; the
  launch warning.
- **After** — sign-in mode; minted mode; revoke failed.

## 3. Do not design (out of scope this round)

- **No new screen and no nav item.** Every surface is inline in one that exists.
- **No second identity provider and no second forge.** `entra` is Azure DevOps only.
- **No multi-party approval** (user + manager / infosec). Filed for 0.8 at the owner's own direction.
- **No per-capability application registration.** It would make a token narrow by construction and is
  recorded as a follow-up (F-LIVE-1, option 2), not built.
- **No device-code sign-in.** Blocked by Conditional Access on a real tenant (F-LIVE-4).
- **No change to `pat` / `ssh` rows' egress**, and no narrowing of the `*.visualstudio.com` tunnel for
  them — a behaviour change for live installs, filed for 0.8 and stated as a residual in the docs.
- **No "tell my admin" button** on the above-ceiling card. There is no backend for it, and a button
  that only appears to do something is worse than a sentence.
- **No re-record** of demo videos.

## 4. Design system

Same tokens and idioms as `docs/design/workspace-providers-mock/mock.css`, copied verbatim, plus four
listed additions built from the same tokens: `.req` (the card), `.dec` (a decision button with its
consequence under it), `.enf` (the three-row enforcement panel) and the countdown chip. No new
colours, no new sizes (`CONSOLE-RULES.md` §3).

**Teal, stated per surface.** On the provider row the one `default` button is `PROVIDERS.SAVE_CTA`, as
today. On the member's Getting started card, **Sign in to Azure DevOps** is teal when it is the first
thing not done — the page's existing budget. **On the capability card, exactly one decision is teal:
Allow once.** "Allow for this run" is `outline` and "Deny" is `outline`, except on a `policy_bypass`
card where **Deny is `destructive` and nothing is teal** — the loudest button on the scariest ask
should not be the one that says yes. Zero teal on the after view.

Colour, per rule:

- **The enforcement panel's rows are toned by who enforces them**, never by how good the news is:
  forge-enforced is success, Wardyn-enforced is plain, nobody-enforced is warning. The shared-token
  row is warning twice because two of its three answers genuinely are.
- **A capability that cannot be approved is red, and carries no buttons.** Not a disabled button.
- **Off is neutral** — a capability off the ceiling, a lane turned off, a state with nothing to do.
- **`live` is the only success tone** among the six access states; `expiring` is warning because an
  action exists; a dead sign-in is never green and never neutral.
- **Mono is for literals only:** hosts, org paths, refs, request paths, tenant and client GUIDs,
  token names, commands. A capability label and a person's name are plain.
- **A ref is always shown in full** (`refs/heads/main`, not `main`) on any card that moves one, because
  the difference between `main` and `release/main` is the whole decision.

## 5. Hard canon constraints — what the UI must NEVER claim

These are the load-bearing ones. A string that breaks one of these is a defect, not a copy preference.

1. **Never say or imply a run's token is scoped to the run** in `bearer` mode. Not "scoped", not
   "limited to", not "least privilege", not "narrowed", not "just-in-time token". The measured truth
   (F-LIVE-1) is that the token carries everything the person consented to.
2. **Never say Wardyn down-scopes, narrows or shrinks a Microsoft Entra sign-in.** It cannot.
3. **Never imply consent bounds a run.** Consent bounds what Wardyn can ask Azure DevOps for at all.
   The run's bound is Wardyn's own check. The sign-in dialog says exactly that, in those words.
4. **Never claim forge-enforced narrowing outside `minted_pat` mode.** The success-toned third
   enforcement row renders in that mode and no other.
5. **Never describe the proxy check as a second opinion, a backstop, a defence in depth or a belt and
   braces.** It is the enforcing layer for a run. Saying otherwise undersells it *and* misdescribes it.
6. **Never show a disabled decision button for a request that was already refused.** Refused states
   carry a sentence, not a greyed-out Approve.
7. **Never claim a minted token is safer than a sign-in.** It is bounded by the forge and *more*
   exposed, because it exists at Azure DevOps until revoked. Both halves are said together.
8. **Never claim revocation ends a connection already open.** Azure DevOps does not promise it.
9. **Never say "the agent"**, or any model, plan or campaign word, in a product string. It is "the
   run", "this run", "runs".
10. **No member-facing string names another person's identity, a tenant GUID, a client GUID or a
    secret name.** The card names the run's owner because the decider is deciding about them; it
    names nothing else.
11. **The shared-token row keeps its honesty.** `shared` is not deprecated, not warned about in red,
    and not silently migrated — it is described accurately and left alone.
12. **Three shipped sentences are rewritten in the same change that falsifies them** (§7.8): the
    `tool_call` "no backend component enforces this", the two "Wardyn can't expire or down-scope a
    PAT", and the `PAT · in-sandbox` chip (which reads from the broker switch rather than being
    renamed outright — "in-sandbox" is still true when the broker is off).

## 6. Where the model lives on the page

1. **`/providers`, the Git tab, the Azure DevOps row** — the existing row gains, under the existing
   lanes: `FIELD_CREDENTIAL_SOURCE` (`Segmented`), `FIELD_TOKEN_MODE` (`Segmented`, only when per
   person), `FIELD_TENANT` and `FIELD_CLIENT` (the second read-only, filled from the login
   application), then "What a run may do here" — `CEILING_TITLE` / `CEILING_HINT` beside
   `PROFILE_TITLE` / `PROFILE_HINT` as two capability lists — then `ENFORCEMENT_TITLE` and the
   three-row panel, and `ALWAYS_DENIED_*` as the plain note.
2. **Getting started, "What's set up for you"** — one more chip from the six states, its action line
   under the row, `SIGN_IN_ADO` where `SIGN_IN_AWS` sits; and, once connected, the `Azure DevOps`
   card with the identity, the three capability groups and the panel.
3. **The live run** — the capability card in `live-approvals.tsx`, above the terminal.
4. **`/approvals`** — the same card, plus `Open run`, plus the decided table and the ended-run state;
   `TOOL_CALL_NOTE` replaces the screen's `tool_call` sentence.
5. **New Run rail** — the `git_credential` preflight chip and its sub-line; the dialog on Launch;
   the relaunch toast.
6. **Run detail** — a new widget, `AFTER_TITLE` / `AFTER_LEAD`, the request table, and the two
   honest notes.

## 7. Canonical strings — DRAFT, frozen by the owner at this gate

A backticked substring inside a string (a host, a ref, a wire value, a field) renders `font-mono`; the
frozen string is plain text and the parser strips backticks. A `{placeholder}` is substituted by the
caller. Every row from §7.2 on is DRAFT — the heading says so once. The namespace is `ADO`.

### 7.1 Reused canon — referenced, never re-frozen

| Key | Lives in | String |
|---|---|---|
| `PROVIDERS.FIELD_ENABLED` / `FIELD_BASE_URLS` / `BASE_URLS_HINT` / `FIELD_LANES` / `LANES_HINT` | `workspace-providers-copy.ts` | Enabled / Allowed addresses / One per line, over HTTPS. A repository is admitted when its URL starts with one of these. / Permitted lanes / Which credential a run may use for this provider. Turning one off does not delete its stored secret. |
| `PROVIDERS.SAVE_CTA` / `SAVE_REFUSED_TITLE` / `SAVED_TOAST` / `SAVE_ERROR` | `workspace-providers-copy.ts` | Save providers / These providers can't be saved as written / Providers saved. / Couldn't save these providers. |
| `PROVIDERS.LANE_APP_UNAVAILABLE` | `workspace-providers-copy.ts` | Not available: the App broker mints repository-scoped GitHub tokens and has no Azure DevOps equivalent. |
| `PROVIDERS.KIND_AZURE_DEVOPS` | `workspace-providers-copy.ts` | Azure DevOps |
| `APPROVAL_BANNER_LABEL.what` / `.blast` | `wardyn/copy.ts` | What you're approving: / Blast radius: |
| `APPROVAL_SCOPE_LABEL.once` / `.run` / `.until` / `.always` | `wardyn/copy.ts` | Once / This run / Until… / Always |
| `APPROVAL_SCOPE_HINT.once` / `.run` | `wardyn/copy.ts` | This one connection. The next attempt asks again. / Every attempt until this run ends. (default) |
| `APPROVAL.CANCELLED_BODY` | `wardyn/copy.ts` | The run ended before anyone decided this. Nothing was approved and nothing was denied. |
| `CAPABILITY.brokerLine` / `gitPatLine` / `sshKeyLine` | `wardyn/copy.ts` | (the three residency lines, rendered as the lane chips' tooltips, unchanged) |
| `MEMBER_GETTING_STARTED.SETUP_SUMMARY_TITLE` / `_HELPER` | `wardyn/copy.ts` | What's set up for you / Your admin configured the barrier, network and shared credentials. Your runs inherit them. |
| `MEMBER_GETTING_STARTED.MODEL_ACCESS_PROVIDED_CHIP` | `wardyn/copy.ts` | Model access · Provided by your admin |
| `AGENTS.MODEL_ACCESS_EXPIRING_ACTION(ts)` | `workspace-providers-copy.ts` | Sign in again before {ts} |
| `OPERATOR_ONLY_REASON` | `wardyn/copy.ts` | Requires the admin role. |
| `PEOPLE.CANCEL` | `people-access-copy.ts` | Cancel |
| `PROVIDERS.LAUNCH_WARNING_TITLE` / `AGENTS.OPEN_RUN_CTA` | `workspace-providers-copy.ts` | Run launched with a warning / Open run |

**Composed by the server for THIS round — rendered verbatim, never keyed.** The Go side's literal is
the source; the module carries no copy of these (the drives/providers precedent). `Emitted by` names
the Go constants block.

| Source | Emitted by | String |
|---|---|---|
| Default above ceiling (400) | `ADO_400.*`, `validateProviderEntra` | entra: "{capability}" is in default_profile but not in capability_ceiling |
| Entra lane on an unsupported host (400) | `validateProviderEntra` | entra: the Microsoft Entra lane is available for dev.azure.com and <org>.visualstudio.com only — an Azure DevOps Server host signs in with a stored token |
| Foreign application (400) | `validateProviderEntra` | entra: client_id must be this deployment's sign-in application ({client_id}) — Wardyn ties an Azure DevOps sign-in to the person's own session by matching the two, and cannot do that across applications |
| Tenant shape (400) | `validateProviderEntra` | entra: tenant_id must be a GUID |
| `per_user` without the lane (400) | `validateProviderEntra` | credential_source: per_user needs the entra lane on this row |
| An always-denied capability on the ceiling (400) | `validateProviderEntra` | entra: "{capability}" can't be put on capability_ceiling — creating and revoking Azure DevOps tokens is refused on every row |
| Not signed in, at run create (422, `reason: git_credential`) | `ADO_422.*`, `runs_create_validate.go` | git_credential: you have not signed in to Azure DevOps — sign in and start the run again |
| Sign-in ended, at run create (422, `reason: git_credential`) | `runs_create_validate.go` | git_credential: your Azure DevOps sign-in ended — sign in and start the run again |
| Repository outside the row's org (403, proxy) | `ADO_REFUSE.*`, `proxy/ado_gate.go` | this run may only reach {org} on Azure DevOps |
| Token creation attempted (403, proxy) | `proxy/ado_gate.go` | creating or revoking Azure DevOps tokens is refused for every run |
| Capability refused after a decision (403, proxy) | `proxy/ado_gate.go` | {capability} was denied for this run |
| Org policy refuses a mint | `ADO_PAT.*`, `api/ado_pat.go` | your organization's policy refuses this token: {policy} |

### 7.2 `ADO` — the provider row (every row DRAFT)

| Key | String |
|---|---|
| `LANE_ENTRA_LABEL` | Microsoft Entra · brokered |
| `LANE_ENTRA_TOOLTIP` | The run works as you, through a token the proxy holds — nothing is written into the sandbox. |
| `LANE_PAT_OFF_PER_USER` | Turned off while this row signs in per person. A stored token would put every run back on one account. |
| `LANE_SSH_OFF_PER_USER` | Turned off while this row signs in per person. A stored key would put every run back on one account. |
| `FIELD_CREDENTIAL_SOURCE` | Credential |
| `SOURCE_SHARED` | Shared token |
| `SOURCE_SHARED_HINT` | One token an admin stores. Every run uses it, so Azure DevOps records every commit and every API call under that one account, whoever started the run. |
| `SOURCE_PER_USER` | Each person signs in |
| `SOURCE_PER_USER_HINT` | Each person signs in to Azure DevOps themselves. Their runs act as them, Azure DevOps applies their own permissions, and removing them from your directory ends it. |
| `FIELD_TOKEN_MODE` | Token |
| `TOKEN_BEARER` | Their sign-in |
| `TOKEN_MINTED` | One Azure DevOps mints per run |
| `TOKEN_MODE_HINT` | Their sign-in is renewed in the background and never stored anywhere a run can read. A minted token is bounded by Azure DevOps itself, and exists there until Wardyn revokes it. |
| `TOKEN_MODE_SHARED_HINT` | There is one token and it is the one you stored. This choice appears when each person signs in. |
| `FIELD_TENANT` | Directory (tenant) ID |
| `TENANT_HINT` | The Microsoft Entra directory your organisation signs in to. |
| `FIELD_CLIENT` | Application (client) ID |
| `CLIENT_HINT` | Filled from the application this console signs in with. Wardyn can only tie an Azure DevOps sign-in to the person's own Wardyn session when both use one application. |
| `CAPS_TITLE` | What a run may do here |
| `CAPS_LEAD` | Two lists, and the difference between them matters: the ceiling is what a run may ever ask for, the defaults are what it starts with. |
| `CEILING_TITLE` | Capability ceiling |
| `CEILING_HINT` | What a run may ever ask for. Anything not ticked here is refused outright — nobody can approve it, not the person and not you. |
| `PROFILE_TITLE` | Every run starts with |
| `PROFILE_HINT` | What a run has before anyone decides anything. Everything else on the ceiling is held at the proxy until you or the person who started the run allows it. |
| `PROFILE_OFF_CEILING` | Not on the ceiling, so it can't be a default. |
| `PROFILE_READ_ONLY_NOTE` | Read-only is the honest default. A run that only reads never interrupts anyone; a run that starts able to push never asks. |
| `ALWAYS_DENIED_TITLE` | Always refused, on every Azure DevOps row |
| `ALWAYS_DENIED_BODY` | Creating or revoking tokens, service hooks, and installing extensions. They are not on the ceiling, they can't be approved, and no change here allows them. |
| `MINTED_EXPOSURE_TITLE` | A minted token is more exposed than a sign-in |
| `MINTED_EXPOSURE_BODY` | It exists at Azure DevOps under that person's name until Wardyn revokes it — at the end of the run, or on the next sweep if this Wardyn was stopped mid-run. A sign-in is never written anywhere and needs no cleanup. Choose this when you need Azure DevOps itself to bound the run. |
| `MINTED_POLICY_NOTE` | Your organisation may refuse token creation, or cap how long a token may live, by policy. If it does, runs on this row are refused with the policy named, and nothing falls back to a wider token. |
| `MINTED_REVOKE_NOTE` | Azure DevOps does not promise that revoking a token ends a connection already open with it. Revoking is what stops the next request, not necessarily the one in flight. |
| `SAVED_NARROWED_RUNS(n, capability)` | {n} run was granted {capability} and no longer holds it — its next request for it is refused. / {n} runs were granted {capability} and no longer hold it — their next request for it is refused. |

### 7.3 `ADO` — the enforcement panel (every row DRAFT)

| Key | String |
|---|---|
| `ENFORCEMENT_TITLE` | Where this is enforced |
| `ENFORCEMENT_LEAD` | Three different things, held by three different parties. Wardyn states all three because only two of them are Wardyn's. |
| `ENF_WHO_LABEL` | Who a run acts as |
| `ENF_WHO_PER_USER` | Azure DevOps. It is that person's own sign-in, so their own Azure DevOps permissions apply and a repository they can't read stays unreadable. Every commit and every API call is recorded under their name. |
| `ENF_WHO_SHARED` | The account your stored token belongs to — the same one for everybody. Azure DevOps applies that account's permissions, not the person's, and its audit names that account. |
| `ENF_WHAT_LABEL` | What a run may do |
| `ENF_WHAT_PER_USER` | Wardyn. Every Azure DevOps request is read at the proxy and refused if this run hasn't been granted it. This check is what bounds the run — not the token. |
| `ENF_WHAT_SHARED` | Wardyn's host and address checks only. Capabilities, and the requests they hold, need each person to sign in. |
| `ENF_TOKEN_LABEL` | The token's own reach |
| `ENF_TOKEN_BEARER` | Nobody narrows it. A Microsoft Entra sign-in token carries every Azure DevOps permission that person has consented to, and asking for less changes nothing. Switch Token to “One Azure DevOps mints per run” to have Azure DevOps bound it too. |
| `ENF_TOKEN_MINTED` | Azure DevOps. It mints this run its own token, limited to the capabilities the run has been granted and to the run's lifetime, and widens it only when you allow more. |
| `ENF_TOKEN_SHARED` | Whatever you gave the token when you created it in Azure DevOps. Wardyn can neither narrow it nor expire it. |
| `ENF_WHO_MEMBER` | Azure DevOps. It is your own sign-in, so your own permissions apply — a repository you can't read, your run can't read either. |
| `ENF_WHAT_MEMBER` | Wardyn. Every request is read before it leaves and refused unless the run was granted it. That check is what holds a run to the list above. |
| `ENF_TOKEN_MEMBER` | It covers everything you consented to for Azure DevOps — Wardyn doesn't make it smaller. That is why the check above exists, and why every one of these is recorded. |

### 7.4 `ADO` — capability labels (every row DRAFT)

| Key | String |
|---|---|
| `CAP_READ` | Read |
| `CAP_READ_HINT` | Clone, fetch, and read work items, pipelines and policies. |
| `CAP_CODE_WRITE` | Push |
| `CAP_CODE_WRITE_HINT` | Push commits, and create or move a branch no policy protects. |
| `CAP_PR` | Pull requests |
| `CAP_PR_HINT` | Open, update, comment on and complete a pull request. |
| `CAP_POLICY_ADMIN` | Branch policies |
| `CAP_POLICY_ADMIN_HINT` | Create, change or delete a branch policy. |
| `CAP_POLICY_BYPASS` | Push past a branch policy |
| `CAP_POLICY_BYPASS_HINT` | Move a policy-protected branch, or complete a pull request with its policies bypassed. |
| `CAP_BUILD_EXECUTE` | Run pipelines |
| `CAP_BUILD_EXECUTE_HINT` | Queue a build or a release. |
| `CAP_REPO_ADMIN` | Repository settings |
| `CAP_REPO_ADMIN_HINT` | Rename, fork, or change a repository's own settings. |
| `CAP_WORK_WRITE` | Work items |
| `CAP_WORK_WRITE_HINT` | Create and edit work items. |
| `CAP_WIKI_WRITE` | Wiki |
| `CAP_WIKI_WRITE_HINT` | Create and edit wiki pages. |
| `CAP_UNCLASSIFIED` | Anything Wardyn does not recognise |
| `CAP_UNCLASSIFIED_HINT` | Off: a write Wardyn can't name is refused and can't be approved. On: it can be asked for, named by its address. Leave it off unless a tool you need is being refused. |

### 7.5 `ADO` — member sign-in and the six access states (every row DRAFT)

| Key | String |
|---|---|
| `ACCESS_LIVE` | Azure DevOps · Your sign-in |
| `ACCESS_EXPIRING` | Azure DevOps · Expiring |
| `ACCESS_EXPIRED` | Azure DevOps · Signed out |
| `ACCESS_EXPIRED_ACTION` | Your Azure DevOps sign-in ended. Runs that clone from it are refused until you sign in again. |
| `ACCESS_NOT_CONFIGURED` | Azure DevOps · Not signed in |
| `ACCESS_NOT_CONFIGURED_ACTION` | Runs that clone from Azure DevOps act as you. Sign in once — it takes a click. |
| `ACCESS_SHARED_EXPIRED` | Azure DevOps · Your admin's credential expired |
| `ACCESS_SHARED_EXPIRED_ACTION` | Your admin's Azure DevOps token expired — ask them to replace it. There is nothing for you to sign in to. |
| `SIGN_IN_ADO` | Sign in to Azure DevOps |
| `SIGN_IN_AGAIN` | Sign in again |
| `SIGNIN_TITLE` | Sign in to Azure DevOps |
| `SIGNIN_BODY(org)` | You'll go to your organisation's Microsoft sign-in and come straight back. After that your runs reach `{org}` as you, instead of through one account shared by everyone. |
| `SIGNIN_CONSENT_BODY` | Microsoft may ask you to consent once. What you consent to is what Wardyn is able to ask Azure DevOps for at all. What any one run may actually do is smaller, and Wardyn holds it there: |
| `SIGNIN_APP_NOTE(app)` | The application asking is {app} — the same one you signed in to this console with. You can withdraw the consent at any time from your Microsoft account's My Apps page; doing so stops your runs reaching Azure DevOps. |
| `SIGNIN_CTA` | Continue to Microsoft |
| `GROUP_STARTS_WITH` | Starts with |
| `GROUP_CAN_ASK` | Can ask you for |
| `GROUP_NEVER` | Never |
| `CONNECTED_SIGNED_IN_AS` | Signed in as |
| `CONNECTED_SIGNED_IN_AS_HINT` | Your runs act as this account, and Azure DevOps records them under it. |
| `CONNECTED_ORG` | Organisation |
| `CONNECTED_EXPIRY` | Sign-in ends |
| `CONNECTED_EXPIRY_RENEWED` | Renewed while you keep using it |
| `CONNECTED_EXPIRY_HINT` | Wardyn renews it in the background. If your organisation ends your sessions, or you're removed from the directory, it stops and you sign in again. |
| `SIGN_OUT_CTA` | Sign out of Azure DevOps |
| `SIGN_OUT_CONFIRM_TITLE` | Sign out of Azure DevOps? |
| `SIGN_OUT_CONFIRM_BODY` | Runs you start after this can't reach Azure DevOps until you sign in again. Runs already going keep the sign-in they started with. |
| `ACCESS_SHARED_NOTE` | Rendered when the row uses a shared token and that token works. It is deliberately not a per-person claim: the run does not act as this person. |

### 7.6 `ADO` — the capability request card (every row DRAFT)

| Key | String |
|---|---|
| `REQ_WAITING` | Waiting for you |
| `REQ_WAITING_OTHER(person)` | Waiting for {person} |
| `REQ_COUNTDOWN(mmss)` | {mmss} left |
| `REQ_SOURCE(ts)` | Azure DevOps · asked by this run at {ts} |
| `REQ_SOURCE_LIST(run, person, ts)` | Azure DevOps · run “{run}” · started by {person} · asked {ts} |
| `REQ_FIELD_REPOSITORY` | Repository |
| `REQ_FIELD_BRANCH` | Branch |
| `REQ_FIELD_PR` | Pull request |
| `REQ_FIELD_COMMAND` | Command |
| `REQ_FIELD_CHANGE` | Change |
| `REQ_FIELD_REQUEST` | Request |
| `REQ_FIELD_ACTS_AS` | Acts as |
| `REQ_ACTS_AS_HINT(person)` | Whatever you allow happens as {person} on Azure DevOps, and Azure DevOps records it that way. |
| `REQ_UNPROTECTED_REF` | No branch policy protects this branch. |
| `REQ_PROTECTED_REF_TITLE(ref)` | `{ref}` is protected by a branch policy |
| `REQ_PROTECTED_REF_BODY(person)` | Allowing this moves it anyway. The policy that requires a reviewed pull request will not stop it, because {person}'s Azure DevOps account is allowed to bypass it. |
| `REQ_HELD(thing)` | The {thing} is held at the proxy until you answer. Nothing has reached Azure DevOps. |
| `REQ_ALLOW_ONCE` | Allow once |
| `REQ_ALLOW_ONCE_HINT(thing)` | This one {thing} goes through. The next one asks again. |
| `REQ_ALLOW_RUN` | Allow for this run |
| `REQ_ALLOW_RUN_HINT(thing)` | Every {thing} from this run goes through until it ends. Nothing carries to the next run. |
| `REQ_DENY` | Deny |
| `REQ_DENY_HINT(thing)` | The {thing} is refused and the tool is told why. The run keeps going and may ask again. |
| `REQ_SCOPE_UNTIL_REFUSED` | Refused for Azure DevOps capabilities: a capability can't outlive the run that was granted it. |
| `REQ_SCOPE_ALWAYS_REFUSED` | Refused for Azure DevOps capabilities: nothing here is saved to the workspace. |
| `REQ_REFUSED_CHIP` | Refused |
| `REQ_CEILING_TITLE` | Above this deployment's ceiling |
| `REQ_CEILING_BODY` | Your admin's Azure DevOps row doesn't list this capability, so there is nothing to decide: it was refused and the run was told. An admin can add it to the ceiling — that changes what a future run may ask for, and does nothing for this one. |
| `REQ_ALWAYS_DENIED_TITLE` | Always refused |
| `REQ_ALWAYS_DENIED_BODY` | Creating or revoking tokens, service hooks and extensions are refused on every Azure DevOps row. This can't be approved, and no ceiling change allows it — a run that could mint its own token would be outside every check on this page. |
| `REQ_GOVERNANCE_TITLE` | Refused by your governance policy |
| `REQ_GOVERNANCE_BODY` | The policy this run launched under refuses this capability. It was refused and recorded; nobody here can allow it, and a policy edit doesn't reach a run already going. |
| `REQ_UNCLASSIFIED_HEADING` | An Azure DevOps request Wardyn doesn't recognise |
| `REQ_UNCLASSIFIED_TITLE` | Wardyn can't name what this would do |
| `REQ_UNCLASSIFIED_BODY` | An Azure DevOps write Wardyn doesn't recognise is refused rather than guessed at, because the card couldn't tell you truthfully what you'd be allowing. An admin can turn on “Anything Wardyn does not recognise” for this row if a tool you need keeps hitting this. |
| `REQ_CONSENT_CHIP` | Needs your Microsoft consent |
| `REQ_CONSENT_SOURCE(ts)` | Azure DevOps · you allowed this at {ts} · still held |
| `REQ_CONSENT_BODY(capability)` | You allowed it, but your Azure DevOps sign-in doesn't cover {capability} yet. Signing in again asks Microsoft for that permission — you'll see a consent screen for it, and nothing else changes. The run's request stays held meanwhile. |
| `REQ_CONSENT_CTA` | Sign in and continue |
| `REQ_CONSENT_OTHER_BODY(person)` | You allowed it, but only {person} can give Microsoft the extra permission it needs — the run acts as {person}, and consent is theirs to give. They've been shown this on their Getting started page. |
| `REQ_NOT_YOURS_CHIP` | Not yours to decide |
| `REQ_NOT_YOURS_BODY(person)` | Only {person}, who started this run, or an admin can answer this. |
| `REQ_TIMEOUT_BODY(thing)` | Nobody answered within four minutes, so the {thing} was refused and the run was told. It can ask again. |
| `REQ_REAUTH_CHIP` | Sign-in ended |
| `REQ_REAUTH_TITLE` | Your Azure DevOps sign-in ended mid-run |
| `REQ_REAUTH_BODY` | This run's next Azure DevOps request is held while you sign in again. Sign in and it goes through on its own — the run doesn't have to start over. |
| `LIST_ENDED_CHIP` | This run has ended |
| `LIST_ENDED_BODY` | There's nothing to allow — the request went away with the run. It's here so you can see what it asked for. |
| `LIST_OPEN_RUN_HINT` | Watch it live instead. |
| `OUTCOME_ALLOWED_ONCE` | Allowed once |
| `OUTCOME_ALLOWED_RUN` | Allowed for this run |
| `OUTCOME_DENIED` | Denied |
| `OUTCOME_BY(person, ts)` | by {person} at {ts} |
| `OUTCOME_CEILING` | Above this deployment's ceiling — nobody was asked. |
| `OUTCOME_ALWAYS_DENIED` | Always refused — nobody was asked. |
| `OUTCOME_UNCLASSIFIED` | Wardyn couldn't name what it would do — nobody was asked. |
| `OUTCOME_TIMEOUT` | Nobody answered within four minutes. |
| `COL_TIME` | Time |
| `COL_ASKED` | Asked for |
| `COL_WHERE` | Where |
| `COL_OUTCOME` | Outcome |

### 7.7 `ADO` — the launch door and the after view (every row DRAFT)

| Key | String |
|---|---|
| `PREFLIGHT_LIVE(person)` | Azure DevOps · signed in as {person} |
| `PREFLIGHT_MISSING` | Azure DevOps · not signed in |
| `PREFLIGHT_MISSING_SUB` | You'll be asked to sign in when you launch. |
| `PREFLIGHT_EXPIRING(ts)` | Azure DevOps · your sign-in ends at {ts} |
| `PREFLIGHT_EXPIRING_SUB` | A run going when it ends holds its next Azure DevOps request until you sign in again. Signing in now avoids that. |
| `LAUNCH_ANYWAY` | Launch anyway |
| `LAUNCH_DIALOG_TITLE` | Sign in to Azure DevOps first |
| `LAUNCH_DIALOG_BODY(org)` | This workspace clones from `{org}`, and your runs there act as you. Sign in once and this form comes back exactly as you left it. |
| `RELAUNCH_TOAST_TITLE` | Signed in to Azure DevOps. |
| `RELAUNCH_TOAST_BODY` | Your run is as you left it — launch when you're ready. |
| `LAUNCH_WARNING_EXPIRING(ts)` | Your Azure DevOps sign-in ends at {ts}. After that this run holds its next Azure DevOps request until you sign in again. |
| `AFTER_TITLE` | Azure DevOps access |
| `AFTER_LEAD` | What this run asked for, what it was given, and who decided. |
| `AFTER_ACTED_AS` | Acted as |
| `AFTER_STARTED_WITH` | Started with |
| `AFTER_ENDED_HOLDING` | Ended holding |
| `AFTER_ONCE_NOTE(capability)` | {capability} was allowed once, for one request, and never joined the run's standing set. |
| `AFTER_SCOPES_TITLE` | What the sign-in itself covered |
| `AFTER_SCOPES_BODY(list)` | Azure DevOps issued this run a token covering: {list}. |
| `AFTER_SCOPES_HONESTY` | That is what the token carried, not what the run was allowed to do. Wardyn's own check is what held it to the decisions above; a Microsoft Entra sign-in can't be issued narrower than what the person has consented to. |
| `AFTER_MINTED_TITLE` | What the token itself covered |
| `AFTER_MINTED_BODY(name, caps, from, to, revoked)` | Azure DevOps minted `{name}` for this run, limited to {caps} and to {from}–{to}, and Wardyn revoked it at {revoked}. Outside those, Azure DevOps refused the request itself. |
| `AFTER_REVOKE_FAILED_TITLE` | This run's Azure DevOps token hasn't been revoked |
| `AFTER_REVOKE_FAILED_BODY(name)` | Your sign-in had ended, so Wardyn couldn't revoke `{name}`. It will be revoked the next time you sign in. You can also delete it yourself under your Azure DevOps personal access tokens. |
| `AFTER_FORGE_TITLE` | Azure DevOps has its own record |
| `AFTER_FORGE_BODY(person)` | Every request that went through appears in your organisation's audit log and in the repository's push history, under {person} — not under Wardyn, and not under a shared account. |

### 7.8 Sentences this design falsifies — rewritten in the same change (every row DRAFT)

| Key | String |
|---|---|
| `TOOL_CALL_NOTE` | An Azure DevOps capability request is held at the proxy until someone decides it — nothing reaches Azure DevOps first, and a denial is a refusal, not a note. Every other kind of tool-call approval is a record of what a run said it was about to do; nothing stops it. |
| `SECRETS_PER_USER_NOTE` | This row stores no Azure DevOps token: each person's sign-in is their own, and removing them from your directory ends it. Wardyn holds every run to its granted capabilities at the proxy. |

The shipped sentence *"Wardyn can't expire or down-scope a PAT — it stays live until you revoke it on
{host}"* is **kept unchanged** for a row with a stored token: it is still true there. It is replaced
by `SECRETS_PER_USER_NOTE` only on a row where each person signs in. The lane chip
`PAT · in-sandbox` becomes `PAT · brokered` **read from the `WARDYN_GIT_PAT_BROKER` switch**, not
renamed outright — "in-sandbox" is the true word when the broker is off.

## 8. Where to apply (once implemented, out of scope this round)

- **The row** — `screens/providers/git-tab.tsx` + a new `entra-row.tsx`; `lib/ado-entra-copy.ts` + test.
- **Member sign-in** — `member-getting-started.tsx` (the chip, the action line, the dialog), a new
  `screens/settings/ado-connection.tsx`.
- **The card** — `wardyn/live-approvals.tsx` and `screens/approvals.tsx` from one new
  `wardyn/ado-capability-card.tsx`; `lib/types/approvals.ts` gains the scope fields.
- **The launch door** — `new-run/new-run-rail.tsx`, the `git_credential` 422 arm.
- **The after view** — `run-detail/widgets/ado-access.tsx` + one `widget-registry.ts` line.
- **Server** — the constants blocks named in §7.1's second table, one per lane.

## 9. Owner question list

**Q1 — the ceiling/profile layout.** Drawn as two capability lists side by side, each with its
meaning above it. **(a) as drawn; (b) one list with two checkboxes per row** (on the ceiling / on by
default), which halves the vertical space and makes the subset rule visually obvious, but makes each
row carry two different questions. **Recommend (a)** — the two questions are genuinely different and
a reader who conflates them mis-sets the ceiling. (b) is the stronger candidate if the capability list
grows past a dozen.

**Q2 — the decision control.** Drawn as **three explicit buttons** (Allow once · Allow for this run ·
Deny), each with its consequence under it. The shipped shape is one Approve button with the
persistence behind a caret over `APPROVAL_SCOPE_LABEL`, drawn as the losing variant. **Recommend the
three buttons** — for an egress the default scope is nearly always right, but "once" versus "for this
run" is a materially different answer for a push, and it should be readable without opening a menu.
Ruling this (a) means `APPROVAL_SCOPE_LABEL` gains no new rows and the ADO card is the one card in the
console with a different control, which is a real inconsistency worth the owner's eyes.

**Q3 — teal on the card.** Drawn as: **Allow once** teal on an ordinary card; **nothing teal and Deny
destructive** on a `policy_bypass` card. **(a) as drawn; (b) never any teal on a capability card** —
no decision is the "press this" one. **Recommend (a)**, because a card with no affirmative reads as
broken, but (b) is defensible and is the more conservative call.

**Q4 — `SOURCE_PER_USER` wording.** Drawn as "Each person signs in", mirroring the shipped
`AGENTS.SOURCE_PER_USER` = "Per person". **(a) "Each person signs in"** (says what happens);
**(b) "Per person"** (matches the Agents tab byte-for-byte). **Recommend (a)** — the Agents tab's
"Per person" describes where a credential comes from, this describes an action the member must take,
and the row's whole job is to make that consequence visible.

**Q5 — the hold's length in the copy.** The design clamps the hold at 240 seconds. The card says "four
minutes" in `REQ_TIMEOUT_BODY` and shows a live countdown. **(a) name the number in prose, as drawn;
(b) countdown only.** **Recommend (a)** — a person who walks away should know from the refusal how
long they had.

**Q6 — `CAP_UNCLASSIFIED` on the row.** Drawn as a ceiling checkbox, off, with a hint that names when
to turn it on. **(a) as drawn; (b) not offered in 0.7.10** and unclassifiable writes always refused.
**Recommend (a)** — an operator with a tool Wardyn does not know about otherwise has no path at all,
and the hint is honest about the trade. (b) is the safer ship.

**Q7 — the shared-token row's enforcement panel.** Drawn with two warning rows. **(a) as drawn — the
shared row states its own weaknesses; (b) the panel renders only on per-person rows**, so nothing
editorialises about a configuration the owner has not deprecated. **Recommend (a)**: the panel is a
statement of fact, and an administrator comparing the two modes is exactly who should see it.

**Q8 — capability vocabulary.** Drawn as plain nouns ("Push", "Branch policies", "Push past a branch
policy") rather than the wire names (`code_write`, `policy_admin`, `policy_bypass`). The wire name
appears nowhere in the UI. **(a) as drawn; (b) plain noun with the wire name in mono beneath**, for an
administrator writing `site-config.json` by hand. **Recommend (b) on the row only** and (a)
everywhere else — the administrator authoring a ceiling is the one person who needs the mapping.
Drawn as (a) throughout; the owner's call moves one line.

**Q9 — where the member's connected panel lives.** Drawn on Getting started. **(a) Getting started
only; (b) also a card on Settings**, where the AWS sign-in lives. **Recommend (b)** — a member who
signed in three months ago and wants to check what their runs can do will look in Settings. Not
drawn; it is the same card in a second home.

## Adjudication

### Owner answers

_(empty — the owner fills this at the gate)_

### Round notes (author, 2026-09-22)

0. **Every §7.2–§7.8 row is DRAFT.** The word sits in each heading rather than in each row because a
   marker in the key cell would break `parseFrozenTables`' clone.
1. **§0 is new for this round** and is the reason the round exists in this shape: the plan as approved
   promised a per-run down-scoped token, and the live tenant refused to produce one. Every surface
   was re-drawn around that, not patched.
2. **The enforcement panel is three fixed rows, always all three.** A two-row version was drafted and
   rejected: dropping the "nobody narrows it" row is exactly the omission that would let a reader
   infer the token is scoped.
3. **The not-approvable cards carry no buttons.** This follows Teleport's discipline of rejecting at
   request time what no approver could grant, rather than presenting a card that cannot be answered.
4. **The two approval-banner labels are imported, not re-frozen** — the console already teaches
   "What you're approving:" and "Blast radius:", and a second vocabulary for the same two lines
   would be the drift this round is meant to avoid.
5. **Refs are drawn in full** on every card that moves one. `main` and `release/main` differ by four
   characters and by the entire decision.
6. **Nothing on the page says "agent".** The subject is "the run", throughout, including in the
   sentences that replace shipped copy.

Not drawn, deliberately: a second forge, a second identity provider, multi-party approval, a
per-capability application registration, device code, a "tell my admin" action on the above-ceiling
card, and any change to how `pat` / `ssh` rows reach Azure DevOps today.
